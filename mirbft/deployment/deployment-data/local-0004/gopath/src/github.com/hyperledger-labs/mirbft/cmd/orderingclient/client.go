package main

import (
	"fmt"
	"os"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rs/zerolog"
	logger "github.com/rs/zerolog/log"
	"github.com/hyperledger-labs/mirbft/config"
	"github.com/hyperledger-labs/mirbft/crypto"
	"github.com/hyperledger-labs/mirbft/discovery"
	"github.com/hyperledger-labs/mirbft/manager"
	"github.com/hyperledger-labs/mirbft/membership"
	"github.com/hyperledger-labs/mirbft/messenger"
	pb "github.com/hyperledger-labs/mirbft/protobufs"
	"github.com/hyperledger-labs/mirbft/request"
	"github.com/hyperledger-labs/mirbft/tracing"
	"google.golang.org/grpc"
)

const (
	// Fanout do cliente. Mantemos 1 tanto no fallback (sem assignment)
	// quanto no caminho com assignment (por bucket) para enfatizar o fluxo "cliente -> 1 nó".
	reqFanout = 1
)

type client struct {
	sync.Mutex

	ownClientID int32

	// Private key for signing requests.
	privKey interface{}

	// Number of requests the client tries to submit before stopping.
	// Set to 0 for no limit (and define a running time to make the client stop after a certain time).
	numRequests int

	// Set to a non-zero value to stop submitting requests.
	// A boolean type of this variable would better reflect its semantics,
	// but we use int32, as bool is not supported by the atomic package used to read and update the value.
	stop int32

	// All requests the client will submit (indexed by client sequence number).
	requests map[int32]*pb.ClientRequest

	// The gRPC client stub data structures used to send and receive requests/responses to and from replicas.
	reqClients map[int32]pb.Messenger_RequestClient

	// Channels used to send requests to orderers.
	reqSinks map[int32]chan *pb.ClientRequest

	// The gRPC client stub data structures used to receive bucket assignments from replicas.
	bucketClients map[int32]pb.Messenger_BucketsClient

	// Stores which peer responded to which request.
	responses map[int32]map[int32]bool

	// Stores which request has been submitted to which orderers.
	submittedTo map[int32]map[int32]bool

	// For each request, stores the timestamp of request submission (in microseconds), for calculating latency.
	sentTimestamps   map[int32]int64
	submitTimestamps map[int32]int64

	// For each request, stores a flag indicating whether the request is finished.
	finished map[int32]bool

	// The sequence number of the oldest in-flight request.
	oldestClientSN int32

	// Watermark window channel for in-flight requests.
	watermarkWindow chan *pb.ClientRequest

	// Size of the channel buffer for network sends.
	sendBufferSize int

	// Bucket assignment data.
	epoch                   int32
	maxBucketID             int
	currentBucketAssignment map[int]int32 // map from bucket IDs to peer IDs holding the buckets
	bucketAssignments       map[string]*pb.BucketAssignment
	bucketAssignmentCounts  map[int32]map[string]int

	// Logger and trace.
	log     zerolog.Logger
	logFile *os.File
	trace   tracing.Trace

	// === NOVO: fallback (sem assignment) envia para 1 nó por round-robin ===
	defaultOrderers []int32
	rrIndex         int32
}

// Allocates and returns a pointer to a new client.
func newClient(dServAddr string, numRequests int) *client {
	cl := &client{
		ownClientID:            -1,
		numRequests:            numRequests,
		requests:               make(map[int32]*pb.ClientRequest),
		responses:              make(map[int32]map[int32]bool, numRequests),
		submittedTo:            make(map[int32]map[int32]bool, numRequests),
		sentTimestamps:         make(map[int32]int64, numRequests),
		submitTimestamps:       make(map[int32]int64, numRequests),
		finished:               make(map[int32]bool, numRequests),
		oldestClientSN:         0,
		watermarkWindow:        make(chan *pb.ClientRequest, config.Config.ClientWatermarkWindowSize),
		sendBufferSize:         config.Config.ClientWatermarkWindowSize,
		epoch:                  -1,
		bucketAssignments:      make(map[string]*pb.BucketAssignment),
		bucketAssignmentCounts: make(map[int32]map[string]int),
		trace: &tracing.BufferedTrace{
			Sampling:              config.Config.ClientTraceSampling,
			BufferCapacity:        config.Config.EventBufferSize,
			ProtocolEventCapacity: config.Config.EventBufferSize,
			RequestEventCapacity:  config.Config.EventBufferSize,
		},
	}

	// Obtain identities of all peers (sets cl.ownClientID).
	cl.discoverPeers(dServAddr)

	// Open log file specific to this client and create a new logger.
	logFileName := fmt.Sprintf("%s-%03d.log", outFilePrefix, cl.ownClientID)
	logFile, err := os.Create(logFileName)
	if err != nil {
		logger.Fatal().
			Err(err).
			Int32("clId", cl.ownClientID).
			Str("fileName", logFileName).
			Msg("Could not create log file.")
	}
	cl.log = logger.Output(zerolog.ConsoleWriter{Out: logFile, NoColor: true, TimeFormat: "15:04:05.000"})
	cl.logFile = logFile

	// Load signing key.
	if config.Config.SignRequests {
		cl.loadPrivKey(config.Config.ClientPrivKeyFile)
	}

	// Precompute requests if configured.
	if config.Config.PrecomputeRequests {
		cl.log.Info().Int("numRequests", numRequests).Msg("Precomputing requests.")
		for seqNr := int32(0); seqNr < int32(cl.numRequests); seqNr++ {
			cl.requests[seqNr] = cl.createRequest(seqNr)
		}
	}

	return cl
}

// Loads private key for signing requests
func (c *client) loadPrivKey(privKeyFile string) {
	var err error = nil
	c.privKey, err = crypto.PrivateKeyFromFile(privKeyFile)
	if err != nil {
		c.log.Error().
			Err(err).
			Str("fileName", config.Config.ClientPrivKeyFile).
			Msg("Could not load client private key from file.")
	}
}

func (c *client) discoverPeers(dServAddr string) {
	// Get orderer identities from discovery server.
	var ordererIdentities []*pb.NodeIdentity
	c.ownClientID, ordererIdentities = discovery.RegisterClient(dServAddr)
	logger.Info().
		Int32("ownClientId", c.ownClientID).
		Int("numOrderers", len(ordererIdentities)).
		Msg("Registered with discovery server.")

	// Initialize membership only once.
	membershipInitializer.Do(func() {
		membership.InitNodeIdentities(ordererIdentities)
	})
}

func (c *client) createRequest(seqNr int32) *pb.ClientRequest {

	// Create request message.
	req := &pb.ClientRequest{
		RequestId: &pb.RequestID{
			ClientId: c.ownClientID,
			ClientSn: seqNr,
		},
		Payload:   randomRequestPayload,
		Signature: nil,
	}

	// Sign request message.
	var err error = nil
	if config.Config.SignRequests {
		req.Signature, err = crypto.Sign(request.Digest(req), c.privKey)
		if err != nil {
			c.log.Error().Err(err).Int32("clSn", seqNr).Msg("Failed signing request.")
		}
	}
	// TODO: Add public key to request or remove the Pubkey request field.

	return req
}

// Runs the main client logic:
// - connects to the orderers
// - submits requests according to the configuration.
func (c *client) Run(wg *sync.WaitGroup) {
	defer wg.Done()

	// Only consider non-crashed orderers when simulating failures.
	var ordererIDs []int32
	if config.Config.LeaderPolicy == "SimulatedRandomFailures" {
		ordererIDs = manager.NewLeaderPolicy(config.Config.LeaderPolicy).GetLeaders(0)
	} else {
		ordererIDs = membership.AllNodeIDs()
	}
	// Guarda a lista padrão de orderers para o fallback sem assignment (round-robin).
	c.defaultOrderers = ordererIDs

	// Create connections to ordering servers.
	var reqConns map[int32]*grpc.ClientConn
	c.reqClients, c.bucketClients, reqConns = messenger.ConnectToOrderers(c.ownClientID, c.log, ordererIDs)
	c.startRequestSenders()
	c.startBucketAssignmentReceivers()

	c.log.Info().Msg("Connected to orderers.")

	// Initialize tracing
	// Client IDs are negative to distinguish them from peer IDs.
	c.trace.Start(fmt.Sprintf("%s-%03d.trc", outFilePrefix, c.ownClientID), -1*c.ownClientID)
	defer c.trace.Stop()

	if config.Config.RequestRate == 0 {
		time.Sleep(time.Duration(config.Config.ClientRunTime) * time.Millisecond)
	} else {
		// Create response handler threads.
		responseHandlerWG := c.startResponseHandlers()

		// Optional client timeout.
		if config.Config.ClientRunTime != 0 {
			c.log.Info().Int("clientRunTime", config.Config.ClientRunTime).Msg("Setting up client timeout.")
			time.AfterFunc(time.Duration(config.Config.ClientRunTime)*time.Millisecond, func() {
				atomic.StoreInt32(&c.stop, 1)
				c.log.Info().Int("clientRunTime", config.Config.ClientRunTime).Msg("Stopping client on timeout.")
			})
		}

		c.log.Info().Int("numRequests", c.numRequests).Msg("Starting to submit requests.")

		timeBetweenRequests := int64(1000000 / config.Config.RequestRate)
		nextSubmitTime := time.Now().UnixNano() / 1000 // us

		// Submit loop
		var i int32
		for i = int32(0); i < int32(c.numRequests) && atomic.LoadInt32(&c.stop) == 0; i++ {

			// Rate control
			if config.Config.RequestRate != -1 {
				now := time.Now().UnixNano() / 1000
				c.trace.Event(tracing.CLIENT_SLACK, int64(i), (nextSubmitTime - now))
				if now < nextSubmitTime {
					time.Sleep(time.Duration(nextSubmitTime-now) * time.Microsecond)
					nextSubmitTime += timeBetweenRequests
				} else {
					if config.Config.HardRequestRateLimit {
						nextSubmitTime = now + timeBetweenRequests
					} else {
						nextSubmitTime += timeBetweenRequests
					}
				}
			}

			// Blocks while watermark window is full
			c.submitRequest(i)
		}
		c.log.Info().Int32("nReq", i).Msg("Finished submitting requests.")
		atomic.StoreInt32(&c.stop, 1)

		// Wait until all in-flight finish (dummy loop).
		c.Lock()
		for len(c.submittedTo) > 0 {
			c.Unlock()
			time.Sleep(time.Second)
			c.Lock()
		}
		c.Unlock()

		// Close req streams and wait handlers.
		for peerID, conn := range reqConns {
			if err := conn.Close(); err != nil {
				c.log.Error().Err(err).Int32("ordererID", peerID).Msg("Failed to close client request connection.")
			}
		}
		responseHandlerWG.Wait()
	}

	// Close request channels.
	for _, ch := range c.reqSinks {
		close(ch)
	}

	// Close bucket assignment connections.
	for peerID, cl := range c.bucketClients {
		if err := cl.CloseSend(); err != nil {
			logger.Error().Err(err).Int32("peerId", peerID).Msg("Failed to close bucket assignment connection.")
		}
	}

	// Close log file.
	c.logFile.Close()
}

// Starts all request-sending threads and saves their corresponding input channels in c.reqSinks.
func (c *client) startRequestSenders() {
	c.reqSinks = make(map[int32]chan *pb.ClientRequest, len(c.reqClients))
	for peerID, reqClient := range c.reqClients {
		c.reqSinks[peerID] = c.sendRequests(peerID, reqClient)
	}
}

// Returns a channel to be used to send requests to the peer represented by clientStub.
// Starts a separate goroutine that reads requests from the channel and sends them.
// Closing the channel will stop the sender and close the send part of the connection.
func (c *client) sendRequests(ordererID int32, clientStub pb.Messenger_RequestClient) chan *pb.ClientRequest {
	ch := make(chan *pb.ClientRequest, c.sendBufferSize)

	go func() {
		for req := range ch {
			c.Lock()
			c.sentTimestamps[req.RequestId.ClientSn] = time.Now().UnixNano() / 1000 // us
			c.Unlock()
			if err := clientStub.Send(req); err != nil {
				c.log.Error().Err(err).
					Int32("ordererId", ordererID).
					Int32("clSeqNr", req.RequestId.ClientSn).
					Msg("Failed sending request to ordering peer.")
			}
		}
		if err := clientStub.CloseSend(); err != nil {
			c.log.Error().Err(err).
				Int32("ordererId", ordererID).
				Msg("Failed to close connection to ordering peer.")
		}
	}()

	return ch
}

// Submits a single client request with sequence number seqNr.
// Blocks until the request fits in the client watermark window.
func (c *client) submitRequest(seqNr int32) {
	var req *pb.ClientRequest = nil
	if config.Config.PrecomputeRequests {
		req = c.requests[seqNr]
	} else {
		req = c.createRequest(seqNr)
	}

	// For request creation, the client need not be locked.
	c.Lock()
	c.submitTimestamps[seqNr] = time.Now().UnixNano() / 1000 // us
	c.Unlock()

	// Watermark control (can block).
	c.watermarkWindow <- req

	c.Lock()

	// Decide destinos
	var destIDs []int32
	if c.currentBucketAssignment != nil {
		// Caminho com assignment (por bucket) — ainda com fanout=1
		destIDs = c.guessTargetOrderers(req)
	} else {
		// Fallback SEM assignment: escolha 1 nó por round-robin.
		destIDs = []int32{c.pickDefaultOrderer()}
	}

	// Init request state.
	c.requests[seqNr] = req
	c.responses[seqNr] = make(map[int32]bool)
	c.finished[seqNr] = false
	c.submittedTo[seqNr] = make(map[int32]bool)
	for _, destID := range destIDs {
		c.submittedTo[seqNr][destID] = true
	}
	c.Unlock()

	c.trace.Event(tracing.REQ_SEND, int64(seqNr), 0)
	c.log.Debug().Int32("clSeqNr", req.RequestId.ClientSn).Interface("dest", destIDs).Msg("Targets chosen for submission.")

	// Send to chosen orderer(s).
	for _, ordererID := range destIDs {
		if c.reqSinks[ordererID] != nil {
			c.reqSinks[ordererID] <- req
		} else {
			c.log.Warn().Int32("ordererId", ordererID).Msg("Not sending request to orderer. No connection established.")
		}
	}

	c.log.Debug().Int32("clSeqNr", req.RequestId.ClientSn).Msg("Submitted request.")
}

// Starts response handler threads, one per orderer.
func (c *client) startResponseHandlers() *sync.WaitGroup {
	wg := sync.WaitGroup{}
	wg.Add(len(c.reqClients))
	for peerID, clientStub := range c.reqClients {
		go c.handleResponses(clientStub, peerID, &wg)
	}
	return &wg
}

// Handles all responses coming from one orderer.
func (c *client) handleResponses(clientStub pb.Messenger_RequestClient, peerID int32, wg *sync.WaitGroup) {
	defer wg.Done()

	var response *pb.ClientResponse
	var err error
	for response, err = clientStub.Recv(); err == nil; response, err = clientStub.Recv() {
		c.log.Debug().Int32("clSeqNr", response.ClientSn).
			Int32("peerId", peerID).
			Msg("Received response for request.")
		c.registerResponse(response.ClientSn, peerID)
	}

	c.log.Info().Err(err).Int32("peerId", peerID).Msg("Response handler done.")
}

// Registers response to request with clientSN from replica peerID.
func (c *client) registerResponse(clientSN int32, peerID int32) {
	c.Lock()
	defer c.Unlock()

	c.trace.Event(tracing.RESP_RECEIVE, int64(clientSN), time.Now().UnixNano()/1000-c.sentTimestamps[clientSN])

	clientWatermarkWindowSize := int32(config.Config.ClientWatermarkWindowSize)

	// Ignore responses outside of the client watermark window.
	if clientSN >= c.oldestClientSN && clientSN < c.oldestClientSN+clientWatermarkWindowSize {

		// Note received response
		c.responses[clientSN][peerID] = true

		// Mark finished if enough responses were received.
		if enoughResponses(len(c.responses[clientSN])) && !c.finished[clientSN] {
			now := time.Now().UnixNano() / 1000
			c.trace.Event(tracing.ENOUGH_RESP, int64(clientSN), now-c.sentTimestamps[clientSN])
			c.trace.Event(tracing.REQ_FINISHED, int64(clientSN), now-c.submitTimestamps[clientSN])
			c.finished[clientSN] = true
			delete(c.submittedTo, clientSN)
			c.requests[clientSN] = nil
			c.log.Info().Int32("clSeqNr", clientSN).Msg("Request finished (out of order).")
		}

	} else if clientSN >= c.oldestClientSN+clientWatermarkWindowSize {
		c.log.Error().
			Int32("clSeqNr", clientSN).
			Int32("maxExpected", c.oldestClientSN+clientWatermarkWindowSize-1).
			Msg("Received response for unsubmitted request!")
	}

	// If this was the last response required for the oldest in-flight request
	if clientSN == c.oldestClientSN {
		for c.finished[c.oldestClientSN] {
			c.trace.Event(tracing.REQ_DELIVERED, int64(clientSN), time.Now().UnixNano()/1000-c.submitTimestamps[clientSN])
			c.log.Info().Int32("clSeqNr", c.oldestClientSN).Msg("Request delivered (in order).")
			select {
			case <-c.watermarkWindow:
			default:
				panic("Watermark window underflow!")
			}
			delete(c.responses, c.oldestClientSN)
			c.oldestClientSN++
		}
	}
}

func (c *client) startBucketAssignmentReceivers() {
	for peerID, cl := range c.bucketClients {
		go c.receiveBucketAssignments(peerID, cl)
	}
}

func (c *client) receiveBucketAssignments(_ int32, cl pb.Messenger_BucketsClient) {
	var err error
	var assignment *pb.BucketAssignment
	for assignment, err = cl.Recv(); err == nil; assignment, err = cl.Recv() {
		c.registerBucketAssignment(assignment)
	}
	c.log.Info().Err(err).Msg("Bucket assignment stream ended.")
}

func (c *client) registerBucketAssignment(assignment *pb.BucketAssignment) {
	c.Lock()
	defer c.Unlock()

	// Ignore old late messages
	if assignment.Epoch <= c.epoch {
		return
	}

	// Register received assignment message
	strKey := bucketAssignmentToString(assignment)
	c.bucketAssignments[strKey] = assignment
	if msgCounts, ok := c.bucketAssignmentCounts[assignment.Epoch]; ok {
		msgCounts[strKey]++
	} else {
		c.bucketAssignmentCounts[assignment.Epoch] = make(map[string]int)
		c.bucketAssignmentCounts[assignment.Epoch][strKey] = 1
	}

	// Update bucket assignment if enough messages have been received.
	if newAssignment := c.newBucketsReady(assignment.Epoch); newAssignment != nil {
		c.log.Info().Int32("epoch", assignment.Epoch).Msg("Updating bucket assignment.")
		c.currentBucketAssignment = make(map[int]int32)
		c.maxBucketID = 0
		for peerID, bucketList := range newAssignment.Buckets {
			c.log.Debug().Int32("orderer", peerID).Interface("buckets", bucketList.Vals).Msg("New bucket assignment.")
			for _, b := range bucketList.Vals {
				c.currentBucketAssignment[int(b)] = peerID
				if int(b) > c.maxBucketID {
					c.maxBucketID = int(b)
				}
			}
		}
		c.epoch = newAssignment.Epoch
		go c.resubmitPendingRequests()
	}
}

func (c *client) resubmitPendingRequests() {
	c.Lock()
	defer c.Unlock()

	resubmitted := 0
	for seqNr, submitted := range c.submittedTo {
		if !c.finished[seqNr] {

			// Get request itself and new destinations.
			req := c.requests[seqNr]
			destIDs := c.guessTargetOrderers(req)
			c.log.Trace().
				Int32("clSeqNr", req.RequestId.ClientSn).
				Interface("dest", destIDs).
				Msg("Resubmitting Request.")

			// Resubmit request.
			for _, destID := range destIDs {
				if !submitted[destID] {
					resubmitted++
					c.reqSinks[destID] <- req
					submitted[destID] = true
				}
			}
		}
	}
	c.log.Info().
		Int("n", resubmitted).
		Int32("epoch", c.epoch).
		Msg("Resubmitted Requests.")
}

// Returns a bucket assignment for an epoch if it is ready, nil otherwise.
// The client must be locked when calling this function.
func (c *client) newBucketsReady(epoch int32) *pb.BucketAssignment {
	assignments := c.bucketAssignmentCounts[epoch]
	if assignments == nil {
		return nil
	}
	for strKey, cnt := range assignments {
		if enoughResponses(cnt) {
			return c.bucketAssignments[strKey]
		}
	}
	return nil
}

// Client must be locked when calling this function.
func (c *client) guessTargetOrderers(req *pb.ClientRequest) []int32 {
	guess := make([]int32, reqFanout, reqFanout)
	b := request.GetBucketNr(req.RequestId.ClientId, req.RequestId.ClientSn)
	for i := 0; i < reqFanout; i++ {
		guess[i] = c.currentBucketAssignment[b]
		if b > 0 {
			b--
		} else {
			b = c.maxBucketID
		}
	}
	return guess
}

// === NOVO: escolha 1 nó padrão por round-robin quando não houver assignment ===
func (c *client) pickDefaultOrderer() int32 {
	list := c.defaultOrderers
	if len(list) == 0 {
		// fallback extremo — deve existir pelo menos 1 conexão
		all := membership.AllNodeIDs()
		if len(all) == 0 {
			return 0
		}
		return all[0]
	}
	idx := int(atomic.AddInt32(&c.rrIndex, 1)-1) % len(list)
	return list[idx]
}

// Creates a string representation of a bucket assignment for the purpose of using it as a map key.
// There must be a bijection between assignments and their string representation (no need for human readability though).
func bucketAssignmentToString(assignment *pb.BucketAssignment) string {
	// Get leader IDs (map keys) and sort them (for a deterministic representation)
	leaderIDs := make([]int, 0)
	for leaderID := range assignment.Buckets {
		leaderIDs = append(leaderIDs, int(leaderID))
	}
	sort.Ints(leaderIDs)

	// Start with epoch number
	result := fmt.Sprintf("%d:", assignment.Epoch)
	for _, leaderID := range leaderIDs {
		// Append leader ID
		result = fmt.Sprintf("%s(%d)", result, leaderID)

		// Sort buckets assigned to leader
		buckets := assignment.Buckets[int32(leaderID)].Vals
		sort.Slice(buckets, func(i int, j int) bool { return buckets[i] < buckets[j] })

		// Append all bucket IDs to the string
		for _, b := range buckets {
			result = fmt.Sprintf("%s %d", result, b)
		}
	}

	return result
}

