// Copyright 2022 IBM Corp. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package messenger

import (
	"context"
	"fmt"
	"io"
	"sync"

	"github.com/rs/zerolog"
	logger "github.com/rs/zerolog/log"
	"github.com/hyperledger-labs/mirbft/config"
	"github.com/hyperledger-labs/mirbft/membership"
	pb "github.com/hyperledger-labs/mirbft/protobufs"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
)

var (
	clientConnections sync.Map

	// Synchronizes access to bucketSubscriptions and bucketAssignmentMsg.
	// A concurrent sync.Map does not do the job here, a concurrent subscription and announcement
	// could lead to a client missing an update.
	bucketAssignmentLock sync.Mutex
	bucketSubscriptions  = make(map[int32]pb.Messenger_BucketsServer)
	bucketAssignmentMsg  *pb.BucketAssignment

	ClientRequestHandler func(msg *pb.ClientRequest)
	
	// Worker pool para processar requests sem bloquear Recv()
	incomingReqCh chan *pb.ClientRequest
	workerPoolSize = 16 // Pool fixo de workers
	
	// ✅ Conexões peer-to-peer para forwarding
	peerRequestClients sync.Map // map[int32]pb.Messenger_RequestClient
)

// InitRequestWorkerPool inicializa pool de workers para processar requests
func InitRequestWorkerPool() {
	incomingReqCh = make(chan *pb.ClientRequest, 65536)
	
	for i := 0; i < workerPoolSize; i++ {
		go func(workerID int) {
			fmt.Printf("[WORKER][%d] Started, handler=%v\n", workerID, ClientRequestHandler != nil)
			for req := range incomingReqCh {
				fmt.Printf("[WORKER][%d] Processing req clId=%d clSn=%d, handler=%v\n", workerID, req.RequestId.ClientId, req.RequestId.ClientSn, ClientRequestHandler != nil)
				if ClientRequestHandler != nil {
					ClientRequestHandler(req)
				} else {
					fmt.Printf("[WORKER][%d] Handler is nil, dropping req\n", workerID)
				}
			}
		}(i)
	}
	logger.Info().Int("workers", workerPoolSize).Int("queueSize", 65536).Msg("Request worker pool initialized")
}

// Implementation of the gRPC Request service (multi-request-multi-response) used by ordering clients.
// On first client request (that it considers dummy and doesn't process normally), performs a handshake with the
// client. Submits all subsequent requests to the registered handler function.
func (ms *messengerServer) Request(srv pb.Messenger_RequestServer) error {
	defer func() {
		if r := recover(); r != nil {
			fmt.Printf("[MESSENGER][PANIC] Request handler panicked: %v\n", r)
		}
	}()

	// WARNING: If a simulate crash, the peer ignores all messages
	if Crashed {
		fmt.Printf("[MESSENGER][CRASHED] Peer is crashed, rejecting connection\n")
		return nil
	}


	// Log address of incoming connection.
	p, ok := peer.FromContext(srv.Context())
	if ok {
		logger.Info().Str("addr", p.Addr.String()).Msg("Incoming connection for requests.")
	} else {
		logger.Error().Msg("Failed to get gRPC peer info from context.")
	}

	var err error
	var req *pb.ClientRequest

	// Exchange a dummy request and a dummy response with the client.
	fmt.Printf("[MESSENGER][HANDSHAKE] Starting handshake (ClientRequestHandler=%v)\n", ClientRequestHandler != nil)
	err = ms.performClientHandshake(srv)
	if err != nil {
		fmt.Printf("[MESSENGER][HANDSHAKE][ERROR] Handshake failed: %v\n", err)
		return err
	}
	fmt.Printf("[MESSENGER][HANDSHAKE] Handshake completed, starting request loop\n")

	// Call the handler for each request received from the client
	fmt.Printf("[MESSENGER][LOOP] Starting to receive requests...\n")
	fmt.Printf("[MESSENGER][CONTEXT] Context error before loop: %v\n", srv.Context().Err())
	reqCount := 0
	for req, err = srv.Recv(); err == nil; req, err = srv.Recv() {
		reqCount++
		fmt.Printf("[MESSENGER][RECV] Received request #%d from client %d sn %d\n", reqCount, req.RequestId.ClientId, req.RequestId.ClientSn)
		logger.Trace().
			Int32("clId", req.RequestId.ClientId).
			Int32("clSn", req.RequestId.ClientSn).
			Msg("Received request.")
		if ClientRequestHandler == nil {
			fmt.Printf("[MESSENGER][ERROR] ClientRequestHandler is nil!\n")
		} else {
			// Enfileira para worker pool (não-bloqueante)
			select {
			case incomingReqCh <- req:
				// Enfileirado com sucesso
			default:
				// Fila cheia - drop com log
				fmt.Printf("[MESSENGER][DROP] Queue full, dropping request clId=%d clSn=%d\n", req.RequestId.ClientId, req.RequestId.ClientSn)
				logger.Warn().Int32("clId", req.RequestId.ClientId).Int32("clSn", req.RequestId.ClientSn).Msg("Request queue full, dropping")
			}
		}
	}

	// Log error message produced on termination of the above loop.
	fmt.Printf("[MESSENGER][LOOP-END] Request loop ended with error: %v (address=%s)\n", err, p.Addr.String())
	fmt.Printf("[MESSENGER][CONTEXT] Context error after loop: %v\n", srv.Context().Err())
	logger.Info().Err(err).Str("address", p.Addr.String()).Msg("Connection terminated.")

	// Send gRPC response message and close connection.
	return err
}

func (ms *messengerServer) Buckets(msgSink pb.Messenger_BucketsServer) error {

	// The client always starts by sending one subscription.
	subscription, err := msgSink.Recv()
	if err != nil {
		logger.Error().Err(err).Msg("Could not receive bucket assignment subscription.")
	}

	// Store subscription for bucket assignment updates.
	bucketAssignmentLock.Lock()
	bucketSubscriptions[subscription.ClientId] = msgSink
	bucketAssignmentLock.Unlock()

	// Directly respond with the current bucket assignment if it exists.
	if bucketAssignmentMsg != nil {
		if err := msgSink.Send(bucketAssignmentMsg); err != nil {
			logger.Error().Err(err).Int32("clId", subscription.ClientId).Msg("Could not directly respond to bucket assignment subscription.")
		}
	}

	// Wait until the client closes the connection (no other actual messages are expected).
	_, err = msgSink.Recv()
	if err != io.EOF {
		logger.Err(err).Msg("Expected EOF on Buckets RPC.")
	} else {
		logger.Info().Int32("clId", subscription.ClientId).Msg("Finished sending bucket assignments to client.")
	}

	return nil
}

func AnnounceBucketAssignment(assignment *pb.BucketAssignment) {
	bucketAssignmentLock.Lock()
	defer bucketAssignmentLock.Unlock()

	// Update current bucket assignment.
	bucketAssignmentMsg = assignment
	logger.Info().
	Int("numPeers", len(assignment.Buckets)).
	Msg("[CL] AnnounceBucketAssignment to clients")
	// Announce new assignment to all subscribers.
	for clID, msgSink := range bucketSubscriptions {
		if err := msgSink.Send(bucketAssignmentMsg); err != nil {
			logger.Warn().Err(err).Int32("clId", clID).Msg("Could not announce bucket assignment.")
		}
	}
}

// Performs an initial handshake with a connecting client.
// One dummy request message and one dummy response message are used for this.
// Those messages are not treated by the ordering protocol and only serve for synchronizing the client and the peer.
// The client is expected to send one dummy request, on the reception of which the peer initializes the required
// client-related data structures. The peer then responds with a dummy response, indicating to the client that the
// peer is ready to send actual responses to actual requests.
func (ms *messengerServer) performClientHandshake(srv pb.Messenger_RequestServer) error {

	// Receive first dummy Request from client.
	// This is not an actual request and only serves for establishing the connection.
	req, err := srv.Recv()
	if err != nil {
		logger.Error().Msg("Error receiving first (dummy) client request.")
		return err
	}
	fmt.Printf("[MESSENGER][HANDSHAKE] Received dummy request: clientId=%d clientSn=%d\n", req.RequestId.ClientId, req.RequestId.ClientSn)

	// Save the connection to the client.
	registerClientConnection(srv, req.RequestId.ClientId)

	// Send a dummy response to the client.
	// This is not an actual response, and only serves as an acknowledgment to the client that the connection has
	// been registered at the server.
	sendErr := srv.Send(&pb.ClientResponse{
		ClientSn: -1,
	})
	if sendErr != nil {
		fmt.Printf("[MESSENGER][HANDSHAKE][ERROR] Failed to send dummy response: %v\n", sendErr)
	}
	return sendErr
}

// Saves the client connection in clientConnections.
// Required for sending responses to the client.
func registerClientConnection(srv pb.Messenger_RequestServer, clientID int32) {

	// Assert that client is not yet registered. This should never happen
	// (For now we assume that a client never disconnects and reconnects.)
	if _, ok := clientConnections.Load(clientID); ok {
		logger.Error().
			Int32("clientID", clientID).
			Msg("Client ID already registered. Ignoring. Two clients with the same ID?")
		return
	}

	// Save the client connection.
	clientConnections.Store(clientID, srv)
}

// Sends a response to a client request.
func RespondToClient(clientID int32, response *pb.ClientResponse) {

	// Se o peer está "crashed", não envia nada.
	if Crashed {
		fmt.Printf("[MESSENGER][DROP] crashed=1 client=%d (não enviou)\n", clientID)
		return
	}

	// Sanidade do objeto de resposta.
	if response == nil {
		logger.Error().Int32("clientID", clientID).Msg("Not responding to client. Nil response.")
		fmt.Printf("[MESSENGER][ERR] client=%d nil response (drop)\n", clientID)
		return
	}

	// Extraímos os campos para logar mesmo se o envio falhar.
	orderSn := response.GetOrderSn()
	clientSn := response.GetClientSn()

	// Verifica se há conexão registrada para esse clientID.
	srv, ok := clientConnections.Load(clientID)
	if !ok || srv == nil {
		logger.Error().
			Int32("clientID", clientID).
			Int32("clientSn", clientSn).
			Int32("orderSn", orderSn).
			Msg("Not responding to client. No connection present.")
		fmt.Printf("[MESSENGER][ERR] client=%d not connected (drop clientSn=%d sn=%d)\n",
			clientID, clientSn, orderSn)
		return
	}

	// Confere o tipo da conexão (defensivo).
	rs, ok := srv.(pb.Messenger_RequestServer)
	if !ok || rs == nil {
		logger.Error().
			Int32("clientID", clientID).
			Msg("Not responding to client. Invalid connection type.")
		fmt.Printf("[MESSENGER][ERR] client=%d invalid connection type (drop clientSn=%d sn=%d)\n",
			clientID, clientSn, orderSn)
		return
	}

	// Tenta enviar a resposta.
	if err := rs.Send(response); err != nil {
		logger.Error().
			Err(err).
			Int32("clientID", clientID).
			Int32("clientSn", clientSn).
			Int32("orderSn", orderSn).
			Msg("Error responding to client.")
		fmt.Printf("[MESSENGER][ERR] send failed client=%d clientSn=%d sn=%d err=%v\n",
			clientID, clientSn, orderSn, err)
		return
	}

	// Sucesso.
	// (Este log é crucial para fechar o "fio" RESPONDER→MESSENGER→CLIENTE.)
	fmt.Printf("[MESSENGER][OK] client=%d deliver clientSn=%d sn=%d\n",
		clientID, clientSn, orderSn)
}


// Creates connections to all the orderers and returns them as a slice of gRPC client stubs.
// This function is used by the client.
func ConnectToOrderers(ownClientID int32, clientLog zerolog.Logger, ordererIDs []int32) (map[int32]pb.Messenger_RequestClient, map[int32]pb.Messenger_BucketsClient, map[int32]*grpc.ClientConn) {

	var mapLock sync.Mutex
	var wg sync.WaitGroup
	reqClients := make(map[int32]pb.Messenger_RequestClient, len(ordererIDs))
	bucketClients := make(map[int32]pb.Messenger_BucketsClient, len(ordererIDs))
	reqConns := make(map[int32]*grpc.ClientConn, len(ordererIDs))

	wg.Add(len(ordererIDs))
	// For each known orderer identity
	for _, id := range ordererIDs {

		// In parallel
		go func(peerID int32) {
			defer wg.Done()

			// Create a connection to orderer (represented by a gRPC client stub).
			reqClient, bucketClient, reqConn := connectToOrderer(peerID, ownClientID, clientLog)

			// Save client stub in clientStubs (or log an error on failure).
			if reqClient != nil && bucketClient != nil {
				mapLock.Lock()
				reqClients[peerID] = reqClient
				bucketClients[peerID] = bucketClient
				reqConns[peerID] = reqConn
				mapLock.Unlock()
			} else {
				clientLog.Error().Int32("ordererId", peerID).Msg("Could not connect to orderer.")
			}
		}(id)
	}
	wg.Wait()

	return reqClients, bucketClients, reqConns
}

// Connects to a single orderer node and returns a message sink (gRPC stub),
// through which messages destined to the orderer node can be sent.
// This function is used by connectToOrderers when the client is connecting to the system.
func connectToOrderer(ordererID int32, ownClientID int32, clientLog zerolog.Logger) (pb.Messenger_RequestClient, pb.Messenger_BucketsClient, *grpc.ClientConn) {

	// Get network address of orderer.
	// The client uses the public address of the orderer
	// (while the peers communicate through their private addresses).
	identity := membership.NodeIdentity(ordererID)
	addrString := fmt.Sprintf("%s:%d", identity.PublicAddr, identity.Port)

	clientLog.Info().Int32("peerId", ordererID).Str("addrStr", addrString).Msg("Connecting to orderer.")

	// Create connection for requests
	reqConn, err := newGRPCClientConnection(addrString)
	if err != nil {
		clientLog.Error().Str("addrStr", addrString).Msg("Couldn't connect to orderer")
		return nil, nil, nil
	}

	// Create connection for bucket assignment updates
	bucketConn, err := newGRPCClientConnection(addrString)
	if err != nil {
		clientLog.Error().Str("addrStr", addrString).Msg("Couldn't connect to orderer")
		return nil, nil, nil
	}

	// Create client stubs.
	reqStub := pb.NewMessengerClient(reqConn)
	bucketStub := pb.NewMessengerClient(bucketConn)

	// Remotely invoke the Request function on the orderer's gRPC server.
	// As this is "stream of requests"-type RPC, it returns a message sink.
	reqClient, err := reqStub.Request(context.Background())
	if err != nil {
		clientLog.Error().Str("addrStr", addrString).Err(err).Msg("Could not invoke Request RPC.")
		if err := reqConn.Close(); err != nil {
			clientLog.Error().Str("addrStr", addrString).Err(err).Msg("Error closing connection to orderer.")
		}
		return nil, nil, nil
	}

	// Remotely invoke the Request function on the orderer's gRPC server.
	// As this is "stream of requests"-type RPC, it returns a message sink.
	bucketClient, err := bucketStub.Buckets(context.Background())
	if err != nil {
		clientLog.Error().Str("addrStr", addrString).Err(err).Msg("Could not invoke Request RPC.")
		if err := bucketConn.Close(); err != nil {
			clientLog.Error().Str("addrStr", addrString).Err(err).Msg("Error closing connection to orderer.")
		}
		return nil, nil, nil
	}
	if err = bucketClient.Send(&pb.BucketSubscription{ClientId: ownClientID}); err != nil {
		clientLog.Error().Str("addrStr", addrString).Err(err).Msg("Could not send bucket assignment subscription.")
	}

	// Perform an initial handshake with the server, exchanging one dummy request and one dummy response.
	// TODO: Is this still necessary when using secure connections? (Probably yes.)
	performServerHandshake(reqClient, ownClientID)
	logger.Info().Msg("[CL] Client handshake OK (dummy request/response)")
	clientLog.Info().Int32("id", identity.NodeId).Str("addrStr", addrString).Msg("Connected to orderer.")

	// Return gRPC message sinks and connections.
	return reqClient, bucketClient, reqConn
}

func newGRPCClientConnection(addrString string) (*grpc.ClientConn, error) {

	// Set general gRPC dial options.
	dialOpts := []grpc.DialOption{
		grpc.WithBlock(),
		grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(maxMessageSize), grpc.MaxCallSendMsgSize(maxMessageSize)),
	}

	// Add TLS-specific gRPC dial options, depending on configuration
	if config.Config.UseTLS {
		tlsConfig := ConfigureTLS(config.Config.CertFile, config.Config.KeyFile)
		dialOpts = append(dialOpts, grpc.WithTransportCredentials(credentials.NewTLS(tlsConfig)))
	} else {
		dialOpts = append(dialOpts, grpc.WithInsecure())
	}

	// Set up a gRPC connection.
	return grpc.Dial(addrString, dialOpts...)
}

// Performs an initial application-level handshake with the server.
// Sends a dummy request and waits for a dummy response.
// This ensures that the server knows about this client and is ready for sending actual responses to actual requests.
// TODO: Is this still necessary when using secure connections? (Probably yes.)
func performServerHandshake(cl pb.Messenger_RequestClient, ownClientID int32) {

	// Send dummy request.
	err := cl.Send(&pb.ClientRequest{
		RequestId: &pb.RequestID{
			ClientId: ownClientID,
			ClientSn: -1,
		},
		Payload:   nil,
		Signature: nil,
	})
	if err != nil {
		logger.Error().Msg("Failed to send dummy request to server during handshake.")
	}

	// Wait for dummy response.
	_, err = cl.Recv()
	if err != nil {
		logger.Error().Msg("Failed to receive dummy response from server during handshake.")
	}
}

// SendRequestToPeer - Envia ClientRequest para outro peer (reutiliza infraestrutura existente)
// ✅ CORREÇÃO: Usa mesma lógica de ConnectToOrderers para peer-to-peer forwarding
func SendRequestToPeer(req *pb.ClientRequest, destNodeID int32) error {
	if destNodeID == membership.OwnID {
		// Local: chama handler diretamente
		if ClientRequestHandler != nil {
			ClientRequestHandler(req)
		}
		return nil
	}

	// Remoto: usa conexão gRPC existente ou cria nova
	clientVal, exists := peerRequestClients.Load(destNodeID)
	if !exists {
		// Lazy connection: conecta sob demanda
		client, err := connectToPeerForRequests(destNodeID)
		if err != nil {
			return fmt.Errorf("failed to connect to peer %d: %w", destNodeID, err)
		}
		peerRequestClients.Store(destNodeID, client)
		clientVal = client
	}

	client := clientVal.(pb.Messenger_RequestClient)
	if err := client.Send(req); err != nil {
		logger.Error().Err(err).Int32("destNodeID", destNodeID).Msg("Failed to send request to peer")
		return err
	}

	return nil
}

// connectToPeerForRequests - Conecta a outro peer para forwarding (reutiliza lógica existente)
func connectToPeerForRequests(peerID int32) (pb.Messenger_RequestClient, error) {
	identity := membership.NodeIdentity(peerID)
	addrString := fmt.Sprintf("%s:%d", identity.PrivateAddr, identity.Port)

	logger.Info().Int32("peerId", peerID).Str("addr", addrString).Msg("Connecting to peer for forwarding")

	// Reutiliza newGRPCClientConnection existente
	conn, err := newGRPCClientConnection(addrString)
	if err != nil {
		return nil, fmt.Errorf("failed to dial peer: %w", err)
	}

	stub := pb.NewMessengerClient(conn)
	client, err := stub.Request(context.Background())
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("failed to create Request stream: %w", err)
	}

	// Handshake (mesmo protocolo do client)
	performServerHandshake(client, membership.OwnID)

	// Start response receiver (discard responses)
	go func() {
		for {
			_, err := client.Recv()
			if err != nil {
				logger.Debug().Err(err).Int32("peerID", peerID).Msg("Peer forwarding stream closed")
				return
			}
		}
	}()

	return client, nil
}
