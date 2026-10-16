package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
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
	reqFanout = 3
)

// RunBenchmarkClients cria config.Config.ClientsPerProcess clientes, roda todos em paralelo e
// espera até todos terminarem (envio + dreno) -- extraído do antigo corpo de main() (ver
// cmd/orderingclient/main.go) pra ser reaproveitado também pelo comando "overload" (apimode.go),
// que só difere em QUANDO chama isso (sob demanda, não automaticamente no início do processo) e,
// opcionalmente, em ter setado config.Config.OverloadGroupOverride/ClientRunTime antes de chamar.
// outPrefix vai pro mesmo lugar que outFilePrefix (var de pacote) já ia: nome base dos arquivos
// de log/trace de cada cliente.
func RunBenchmarkClients(outPrefix string) {
	outFilePrefix = outPrefix

	numClients := config.Config.ClientsPerProcess
	numRequests := config.Config.RequestsPerClient

	logger.Info().
		Int("numClients", numClients).
		Int("numRequests", numRequests).
		Int("crossOpRatio", config.Config.CrossOpRatio).
		Msg("Starting clients.")

	wg := sync.WaitGroup{}

	logger.Info().Msg("Initilizing clients.")
	clients := make([]*client, numClients)
	wg.Add(numClients)
	for i := 0; i < numClients; i++ {
		go func(j int) { clients[j] = newClient(discoveryAddrForClients, numRequests); wg.Done() }(i)
	}
	wg.Wait()
	logger.Info().Msg("Clients initialized.")

	logger.Info().Msg("Launching clients.")
	wg.Add(numClients)
	for i := 0; i < numClients; i++ {
		go clients[i].Run(&wg)
	}

	logger.Info().Msg("Waiting for clients.")
	wg.Wait()

	logger.Info().Msg("Done.")
}

// discoveryAddrForClients é setado uma vez (main.go ou apimode.go) antes de chamar
// RunBenchmarkClients -- newClient precisa do endereço do discovery server, que antes vinha
// direto de uma variável local de main(); como a chamada agora pode vir de dois lugares
// diferentes, vira uma var de pacote, no mesmo espírito de outFilePrefix logo acima.
var discoveryAddrForClients string

// ===== MINIMAL SAFETY FIX =====
// safeSendReq sends to a request channel and prevents panics if the channel is closed during teardown.
// Returns false if the send failed due to a closed channel (or other panic).
func safeSendReq(ch chan *pb.ClientRequest, req *pb.ClientRequest) (ok bool) {
	defer func() {
		if r := recover(); r != nil {
			ok = false
		}
	}()
	ch <- req
	return true
}

// ===== MINIMAL SAFETY FIX =====
// safeCloseReqChan closes a request channel and prevents panics on double close.
func safeCloseReqChan(ch chan *pb.ClientRequest) {
	defer func() { _ = recover() }()
	close(ch)
}

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
	stop int32 // stop submitting NEW requests

	// ===== MINIMAL FIX =====
	// closing gates teardown actions (closing channels, preventing resubmits/sends during shutdown).
	// IMPORTANT: draining happens with stop=1 but closing=0, so resubmissions can still happen.
	closing int32

	// Precomputed payloads (if PrecomputeRequests=true). Signatures are NEVER precomputed.
	payloads map[int32][]byte

	// In-flight requests (created on-demand with fresh signatures).
	requests map[int32]*pb.ClientRequest

	// The gRPC client stub data structures used to send and receive requests/responses to and from replicas.
	reqClients map[int32]pb.Messenger_RequestClient

	// Channels used to send requests to orderers.
	reqSinks map[int32]chan *pb.ClientRequest

	// The gRPC client stub data structures used to receive bucket assignments from replicas.
	bucketClients map[int32]pb.Messenger_BucketsClient

	// Stores which peer responded to which request. responses[43][27] means that peer 27 responded to request 43.
	// In reality, we'd need an index for response (digest), if the contents of the response was important.
	// In case of "ACK"s, this is not necessary.
	responses map[int32]map[int32]bool

	// Stores which request has been submitted to which orderers. submittedTo[43][27] means that
	// request 43 has been submitted to orderer 27.
	submittedTo map[int32]map[int32]bool

	// For each request, stores the timestamp of request submission (in microseconds), for calculating latency.
	sentTimestamps   map[int32]int64
	submitTimestamps map[int32]int64

	// For each request, stores a flag indicating whether the request is finished.
	// Initialized to false on request submission, set to true when enoughResponses() returns true.
	finished map[int32]bool

	// The sequence number of the oldest in-flight request (i.e. submitted request to which not enough responses have been
	// received yet).
	oldestClientSN int32

	// Channel containing in-flight requests. Its capacity corresponds to the watermark window size.
	// Used to implement the client watermark window. Before submitting a request, it needs to be written
	// to this channel. Once enough responses have been received for the first request in the channel, it is removed,
	// making space for the next one.
	watermarkWindow chan *pb.ClientRequest

	// Size of the channel buffer for the requests that should be sent on the network.
	// Since no more than the watermark window of requests will ever be pending at a time, the watermark window
	// size is a good value for the buffer size too.
	// Sending to an orderer might block on this channel only if the orderer is slow and the watermarks advance
	// before the requests are even sent to this orderer.
	sendBufferSize int

	// Data structures to keep track of received bucket assignments.
	epoch                   int32
	maxBucketID             int
	currentBucketAssignment map[int]int32 // map from bucket IDs to peer IDs holding the buckets
	bucketAssignments       map[string]*pb.BucketAssignment
	// For each epoch, for each received assignment, save number of peers it was received from
	bucketAssignmentCounts map[int32]map[string]int

	// Synchronization channel: closed when first bucket assignment is installed
	bucketAssignmentReady chan struct{}

	// Logger used by this client.
	// Each client's log goes in a separate file, even if multiple clients are running from within the same process.
	log     zerolog.Logger
	logFile *os.File // Kept around only for closing.

	// Same as with logging, each client has a separate trace that is output in a separate file,
	// even if multiple clients are running in the same process.
	trace tracing.Trace
}

// Allocates and returns a pointer to a new client.
func newClient(dServAddr string, numRequests int) *client {
	cl := &client{
		ownClientID:            -1,
		numRequests:            numRequests,
		payloads:               make(map[int32][]byte),
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
		bucketAssignmentReady:  make(chan struct{}),
		// currentBucketAssignment will be initialized later (needs to be checked for nil when accessing)
		trace: &tracing.BufferedTrace{
			Sampling:              config.Config.ClientTraceSampling,
			BufferCapacity:        config.Config.EventBufferSize,
			ProtocolEventCapacity: config.Config.EventBufferSize,
			RequestEventCapacity:  config.Config.EventBufferSize,
		},
		// The reqClients field is not initialized, as it is directly assigned a map
		// that the messenger allocates when connecting to the orderers.
		// reqSinks is initialized at the same time.
	}

	// Obtain identities of all peers.
	// Must happen first, as it sets cl.ownClientID
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
	cl.logFile = logFile // Kept around only for closing.

	// Load signing key
	if config.Config.SignRequests {
		cl.loadPrivKey(config.Config.ClientPrivKeyFile)
		// Fail-fast: abort if key loading failed
		if cl.privKey == nil {
			cl.log.Fatal().Msg("SignRequests=true but privKey is nil after loading.")
		}
	}

	// Precompute ONLY payloads (never signatures)
	if config.Config.PrecomputeRequests {
		cl.log.Info().Int("numRequests", numRequests).Msg("Precomputing payloads (signatures will be created on-demand).")
		for seqNr := int32(0); seqNr < int32(cl.numRequests); seqNr++ {
			cl.payloads[seqNr] = cl.createPayload(seqNr)
		}
	}

	return cl
}

// Loads private key for signing requests
func (c *client) loadPrivKey(privKeyFile string) {
	var err error
	c.privKey, err = crypto.PrivateKeyFromFile(filepath.Join("experiment-output", privKeyFile))
	if err != nil {
		c.log.Fatal().
			Err(err).
			Str("fileName", privKeyFile).
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
		Msg("Registared with discovery server.")

	// Initialize membership only once.
	membershipInitializer.Do(func() {
		membership.InitNodeIdentities(ordererIdentities)
	})
}

// createPayload generates only the payload bytes (no signature).
// Deterministically picks a cross-group (TX) or single-group (GET) operation based on
// seqNr and the configured CrossOpRatio (percentage of requests that are cross-group).
func (c *client) createPayload(seqNr int32) []byte {
	var basePayload string
	if config.Config.ApiWorkingSetSize > 0 {
		basePayload = c.createAPIPayload(seqNr)
	} else if int(seqNr)%100 < config.Config.CrossOpRatio {
		numGroups := config.PickCrossOpGroupCount(seqNr)
		keys := make([]string, numGroups)
		for i := 0; i < numGroups; i++ {
			keys[i] = fmt.Sprintf("K%08d", seqNr+int32(i)*1000)
		}
		basePayload = "TX " + strings.Join(keys, ",")
	} else {
		basePayload = fmt.Sprintf("GET K%08d", seqNr)
	}

	// Add padding to reach configured payload size
	paddingSize := config.Config.RequestPayloadSize - len(basePayload)
	if paddingSize < 0 {
		paddingSize = 0
	}
	return []byte(basePayload + " " + strings.Repeat("X", paddingSize))
}

// createAPIPayload gera o payload REAL da API (APIPUT/APIGET/APIDELETE), usado só quando
// config.Config.ApiWorkingSetSize > 0 ("overload ... --api"). As primeiras ApiWorkingSetSize
// requisições (seqNr < ApiWorkingSetSize, enviadas por seedAPIWorkingSet ANTES do laço de envio
// normal começar -- ver Run()) são a FASE DE SEMEADURA: um APIPUT sequencial, uma chave nova
// por seqNr. Depois disso (fase medida, i começa em ApiWorkingSetSize no laço principal -- ver
// Run()), cada requisição escolhe uma chave DENTRE AS JÁ SEMEADAS (seqNr % ApiWorkingSetSize) e
// decide PUT (sobrescreve a mesma chave, nunca cria chave nova -- não mistura "custo de
// operação" com "custo do dataset crescer") / DELETE / GET, pela mesma lógica determinística
// por seqNr que CrossOpRatio já usa (sem aleatoriedade real). found=true garantido de verdade
// pros GETs da fase medida, porque toda chave consultada foi realmente escrita na semeadura.
func (c *client) createAPIPayload(seqNr int32) string {
	n := int32(config.Config.ApiWorkingSetSize)
	if seqNr < n {
		key := fmt.Sprintf("K%08d", seqNr)
		return string(request.EncodePut(key, []byte(fmt.Sprintf("seed-%08d", seqNr))))
	}

	key := fmt.Sprintf("K%08d", seqNr%n)
	pct := int(seqNr) % 100
	switch {
	case pct < config.Config.ApiWriteRatio:
		return string(request.EncodePut(key, []byte(fmt.Sprintf("w-%08d", seqNr))))
	case pct < config.Config.ApiWriteRatio+config.Config.ApiDeleteRatio:
		return string(request.EncodeDelete(key))
	default:
		return string(request.EncodeGet(key))
	}
}

// createRequest creates a request with FRESH signature (never reuses precomputed signatures)
func (c *client) createRequest(seqNr int32) *pb.ClientRequest {
	var payload []byte
	if config.Config.PrecomputeRequests {
		payload = c.payloads[seqNr]
	} else {
		payload = c.createPayload(seqNr)
	}

	// Cliente NÃO deve setar GroupId/TouchedGroups - deixa o preprocessor do servidor fazer isso
	// Servidor vai calcular via ReplicaMapper para garantir determinismo. ÚNICA exceção: o
	// comando "overload <grupo> <duração>" (apimode.go) seta OverloadGroupOverride em memória
	// pra pinar toda a carga gerada nesta invocação num grupo só -- fora isso, fica nil/0 como
	// sempre foi.
	req := &pb.ClientRequest{
		RequestId: &pb.RequestID{
			ClientId: c.ownClientID,
			ClientSn: seqNr,
		},
		Payload:   payload,
		Signature: nil,
	}
	if ov := config.Config.OverloadGroupOverride; ov != nil {
		req.GroupId = *ov
		req.TouchedGroups = []uint32{*ov}
	}

	// ALWAYS sign fresh (never reuse precomputed signatures)
	if config.Config.SignRequests {
		if c.privKey == nil {
			c.log.Fatal().Int32("clSn", seqNr).Msg("Cannot sign: privKey is nil")
		}
		var err error
		req.Signature, err = crypto.Sign(request.Digest(req), c.privKey)
		if err != nil {
			c.log.Fatal().Err(err).Int32("clSn", seqNr).Msg("Failed signing request.")
		}
	}

	return req
}

// Runs the main client logic that
// - connects to the orderers and
// - submits requests according to the configuration read from the config file.
func (c *client) Run(wg *sync.WaitGroup) {
	defer wg.Done()

	// Only consider non-crashed orderers when simulating failures.
	var ordererIDs []int32
	if config.Config.LeaderPolicy == "SimulatedRandomFailures" {
		ordererIDs = manager.NewLeaderPolicy(config.Config.LeaderPolicy).GetLeaders(0)
	} else {
		ordererIDs = membership.AllNodeIDs()
	}

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

	var responseHandlerWG *sync.WaitGroup

	if config.Config.RequestRate == 0 {
		time.Sleep(time.Duration(config.Config.ClientRunTime) * time.Millisecond)
	} else {
		// Create response handler threads.
		// Returns a wait group waiting for all the handlers to finish.
		responseHandlerWG = c.startResponseHandlers()

		// Wait for first bucket assignment before submitting -- movido pra ANTES do timer de
		// ClientRunTime ser armado (ver abaixo), porque a fase de semeadura do modo
		// "overload --api" (seedAPIWorkingSet) precisa rodar antes do timer começar a contar,
		// senão consumiria parte da duração pedida pelo usuário em vez de só a fase medida.
		<-c.bucketAssignmentReady
		c.log.Info().Msg("Bucket assignment ready.")

		// Fase de semeadura do modo "overload --api" (ver createAPIPayload) -- sem efeito
		// (startSeqNr fica 0, igual sempre foi) quando ApiWorkingSetSize não está configurado.
		startSeqNr := int32(0)
		if config.Config.ApiWorkingSetSize > 0 {
			startSeqNr = c.seedAPIWorkingSet()
		}

		// Make client stop after a predefined time, if configured
		if config.Config.ClientRunTime != 0 {
			c.log.Info().Int("clientRunTime", config.Config.ClientRunTime).Msg("Setting up client timeout.")
			time.AfterFunc(time.Duration(config.Config.ClientRunTime)*time.Millisecond, func() {
				atomic.StoreInt32(&c.stop, 1) // halt NEW submissions
				c.log.Info().Int("clientRunTime", config.Config.ClientRunTime).Msg("Stopping client submissions on timeout.")
			})
		}

		c.log.Info().Int("numRequests", c.numRequests).Msg("Starting to submit requests.")

		timeBetweenRequests := int64(1000000 / config.Config.RequestRate)
		nextSubmitTime := time.Now().UnixNano() / 1000 // Submit first request immediately

		// Submit requests
		// numRequests == 0 means unlimited: only the ClientRunTime timer stops the client.
		// i começa em startSeqNr (não 0) pra continuar o ClientSn de onde a semeadura parou,
		// sem reenviar os mesmos números de sequência (e sem createAPIPayload reinterpretar
		// essas requisições como semeadura de novo -- ela só semeia enquanto seqNr < N).
		var i int32
		for i = startSeqNr; (c.numRequests == 0 || i < startSeqNr+int32(c.numRequests)) && atomic.LoadInt32(&c.stop) == 0; i++ {

			if config.Config.RequestRate != -1 {
				now := time.Now().UnixNano() / 1000

				// Log client slack. Watch out, units are microseconds!
				c.trace.Event(tracing.CLIENT_SLACK, int64(i), (nextSubmitTime - now))

				// Wait for next submit time if necessary.
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

			// Important: re-check stop after sleeping / rate-limit logic.
			if atomic.LoadInt32(&c.stop) != 0 {
				break
			}

			// blocks while watermark window is full
			c.submitRequest(i)
		}
		c.log.Info().Int32("nReq", i).Msg("Finished submitting requests.")
		atomic.StoreInt32(&c.stop, 1) // halt NEW submissions

		// ===== MINIMAL DRAIN FIX =====
		// During drain we keep the client "alive" and ALLOW resubmissions (closing=0),
		// so pending requests can still complete.
		drainMs := config.Config.ClientDrainTime
		if drainMs < 0 {
			drainMs = 0
		}
		if drainMs > 0 {
			c.log.Info().Int("clientDrainTimeMs", drainMs).Msg("Draining client responses before shutdown.")
		}

		deadline := time.Now().Add(time.Duration(drainMs) * time.Millisecond)
		for {
			c.Lock()
			pending := len(c.submittedTo) // unfinished requests
			c.Unlock()

			if pending == 0 {
				c.log.Info().Msg("Drain complete (pending=0).")
				break
			}
			if drainMs == 0 || time.Now().After(deadline) {
				c.log.Warn().Int("pending", pending).Msg("Drain window ended with pending requests.")
				break
			}
			time.Sleep(100 * time.Millisecond)
		}
	}

	// ===== BEGIN TEARDOWN =====
	atomic.StoreInt32(&c.closing, 1)

	// Close request connections (ONLY here, after submit loop/drain is done)
	for _, ch := range c.reqSinks {
		safeCloseReqChan(ch)
	}

	// Close bucket assignment connections
	for peerID, cl := range c.bucketClients {
		if err := cl.CloseSend(); err != nil {
			logger.Error().Err(err).Int32("peerId", peerID).Msg("Falied to close bucket assignment connection.")
		}
	}

	// Close gRPC connections (after draining and after senders closed their streams)
	for peerID, conn := range reqConns {
		if err := conn.Close(); err != nil {
			c.log.Error().Err(err).Int32("ordererID", peerID).Msg("Failed to close client request connection.")
		}
	}

	// Stop response handlers and wait for them (if any were started).
	if responseHandlerWG != nil {
		responseHandlerWG.Wait()
	}

	// Close log file.
	_ = c.logFile.Close()
}

// seedAPIWorkingSet roda a fase de semeadura do modo "overload --api" (ver createAPIPayload):
// manda ApiWorkingSetSize requisições APIPUT (ClientSn 0..N-1) o mais rápido possível -- sem
// limite de rate, só o watermark window que submitRequest já respeita sozinho (canal
// bufferizado, bloqueia quando cheio) -- e espera todas serem confirmadas antes de devolver.
// Roda ANTES do timer de ClientRunTime ser armado (ver Run()), então não consome a duração
// pedida pelo usuário -- só a fase medida (que começa logo depois, com ClientSn continuando
// de N em diante) conta pro throughput/latência reportados. Devolve N, pro chamador saber de
// onde continuar o ClientSn.
func (c *client) seedAPIWorkingSet() int32 {
	n := int32(config.Config.ApiWorkingSetSize)
	c.log.Info().Int32("workingSetSize", n).Msg("Seeding API working set before measured phase.")
	for i := int32(0); i < n; i++ {
		c.submitRequest(i)
	}
	// Espera a semeadura comitar de verdade antes da fase medida começar -- sem isso, GETs no
	// início da fase medida podiam cair em chaves ainda não aplicadas (found=false por atraso
	// de rede/consenso, não por nunca terem sido escritas).
	deadline := time.Now().Add(30 * time.Second)
	for {
		c.Lock()
		pending := len(c.submittedTo)
		c.Unlock()
		if pending == 0 {
			break
		}
		if time.Now().After(deadline) {
			c.log.Warn().Int("pending", pending).Msg("Seeding deadline reached with pending requests.")
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	c.log.Info().Int32("workingSetSize", n).Msg("Seeding complete.")
	return n
}

// Starts all request-sending threads and saves their corresponding input channels in c.reqSinks.
func (c *client) startRequestSenders() {
	c.reqSinks = make(map[int32]chan *pb.ClientRequest, len(c.reqClients))
	for peerID, reqClient := range c.reqClients {
		c.reqSinks[peerID] = c.sendRequests(peerID, reqClient)
	}
}

// Returns a channel to be used to send requests to the peer represented by by clientStub.
// Starts a separate thread that reads requests from the channel and sends them over the network.
// This is mostly for synchronizing access to the clientStub's Send() function.
// Closing the channel will stop the sender and close the send part of the connection.
func (c *client) sendRequests(ordererID int32, clientStub pb.Messenger_RequestClient) chan *pb.ClientRequest {
	ch := make(chan *pb.ClientRequest, c.sendBufferSize)

	go func() {
		c.log.Info().Int32("ordererID", ordererID).Msg("[CLIENT] sendRequests goroutine started")
		// Send requests as long as there are any.
		for req := range ch {
			c.Lock()
			c.sentTimestamps[req.RequestId.ClientSn] = time.Now().UnixNano() / 1000 // In us
			c.Unlock()
			c.log.Info().Int32("clSn", req.RequestId.ClientSn).Int32("ordererID", ordererID).Msg("[CLIENT] Sending request")
			if err := clientStub.Send(req); err != nil {
				c.log.Error().Err(err).
					Int32("ordererId", ordererID).
					Int32("clSeqNr", req.RequestId.ClientSn).
					Msg("Failed sending request to ordering peer.")
			} else {
				c.log.Info().Int32("clSn", req.RequestId.ClientSn).Int32("ordererID", ordererID).Msg("[CLIENT] Sent request OK")
			}
		}

		// Close connection when channel is closed.
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
	// If stopping submissions, do not enqueue / send anything anymore.
	if atomic.LoadInt32(&c.stop) != 0 {
		return
	}

	// ALWAYS create request fresh (with fresh signature)
	req := c.createRequest(seqNr)

	// For request creation, the client need not be locked.
	c.Lock()
	c.submitTimestamps[seqNr] = time.Now().UnixNano() / 1000 // In us
	c.Unlock()

	// If stop flips, abort before blocking/enqueueing.
	if atomic.LoadInt32(&c.stop) != 0 {
		return
	}

	c.watermarkWindow <- req

	// If stop flips right after enqueue, avoid sending further (drain will handle in-flight).
	if atomic.LoadInt32(&c.stop) != 0 {
		return
	}

	c.Lock()

	// Find out to which orderers to send the request.
	var destIDs []int32
	if c.currentBucketAssignment != nil {
		destIDs = c.guessTargetOrderers(req)
		c.log.Info().Int32("clSn", seqNr).Interface("destIDs", destIDs).Msg("[CLIENT] Using bucket assignment")
	} else {
		destIDs = membership.AllNodeIDs()
		c.log.Info().Int32("clSn", seqNr).Interface("destIDs", destIDs).Msg("[CLIENT] NO bucket assignment, sending to all")
	}

	// Initialize request-related data structures.
	c.requests[seqNr] = req
	c.responses[seqNr] = make(map[int32]bool)
	c.finished[seqNr] = false
	c.submittedTo[seqNr] = make(map[int32]bool)
	for _, destID := range destIDs {
		c.submittedTo[seqNr][destID] = true
	}
	c.Unlock()

	c.trace.Event(tracing.REQ_SEND, int64(seqNr), 0)

	c.log.Info().Int32("clSn", seqNr).Int("numDest", len(destIDs)).Msg("[CLIENT] Sending to orderers")
	// Send message to all orderers.
	for _, ordererID := range destIDs {
		if atomic.LoadInt32(&c.stop) != 0 {
			return
		}
		if c.reqSinks[ordererID] != nil {
			c.log.Info().Int32("clSn", seqNr).Int32("ordererID", ordererID).Msg("[CLIENT] Enqueueing to orderer")
			_ = safeSendReq(c.reqSinks[ordererID], req)
		} else {
			c.log.Warn().Int32("clSn", seqNr).Int32("ordererID", ordererID).Msg("[CLIENT] reqSink is nil!")
		}
	}

	c.log.Debug().Int32("clSeqNr", req.RequestId.ClientSn).Msg("Submitted request.")
}

// Starts response handler threads, one per orderer.
// Each response handler thread receives and processes response messages from the corresponding orderer.
// Returns a wait group waiting for all the handlers to fihish.
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

	if clientSN >= c.oldestClientSN && clientSN < c.oldestClientSN+clientWatermarkWindowSize {
		c.responses[clientSN][peerID] = true

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

		// Signal that first bucket assignment is ready
		select {
		case <-c.bucketAssignmentReady:
			// Already closed
		default:
			close(c.bucketAssignmentReady)
		}

		// ===== MINIMAL FIX =====
		// Allow resubmissions during drain (stop=1), only block them during teardown (closing=1).
		if atomic.LoadInt32(&c.closing) != 0 {
			return
		}
		go c.resubmitPendingRequests()
	}
}

func (c *client) resubmitPendingRequests() {
	// ===== MINIMAL FIX =====
	// Only block resubmissions during teardown (closing), not during drain.
	if atomic.LoadInt32(&c.closing) != 0 {
		return
	}

	c.Lock()
	defer c.Unlock()

	if atomic.LoadInt32(&c.closing) != 0 {
		return
	}

	resubmitted := 0
	for seqNr, submitted := range c.submittedTo {
		if !c.finished[seqNr] {
			req := c.requests[seqNr]
			destIDs := c.guessTargetOrderers(req)
			c.log.Trace().
				Int32("clSeqNr", req.RequestId.ClientSn).
				Interface("dest", destIDs).
				Msg("Resubmitting Request.")

			for _, destID := range destIDs {
				if !submitted[destID] {
					if atomic.LoadInt32(&c.closing) != 0 {
						return
					}
					resubmitted++
					if c.reqSinks[destID] != nil {
						_ = safeSendReq(c.reqSinks[destID], req)
					}
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

func (c *client) guessTargetOrderers(req *pb.ClientRequest) []int32 {
	// If request has no TouchedGroups, send to proxy for preprocessing
	// Cliente SEMPRE envia para proxy para preprocessamento
	// Servidor vai calcular TouchedGroups e fazer roteamento correto
	if config.Config.CrossOpProxyNodeID >= 0 {
		c.log.Trace().Int32("clSn", req.RequestId.ClientSn).Int32("proxy", config.Config.CrossOpProxyNodeID).Msg("Sending to proxy for preprocessing")
		return []int32{config.Config.CrossOpProxyNodeID}
	}
	// No fixed proxy: round-robin by ClientSn instead of always node 0.
	nodeIDs := membership.AllNodeIDs()
	proxyID := nodeIDs[int(req.RequestId.ClientSn)%len(nodeIDs)]
	c.log.Trace().Int32("clSn", req.RequestId.ClientSn).Int32("proxy", proxyID).Msg("Sending to round-robin proxy for preprocessing")
	return []int32{proxyID}
}

// Creates a string representation of a bucket assignment for the purpose of using it as a map key.
func bucketAssignmentToString(assignment *pb.BucketAssignment) string {
	leaderIDs := make([]int, 0)
	for leaderID := range assignment.Buckets {
		leaderIDs = append(leaderIDs, int(leaderID))
	}
	sort.Ints(leaderIDs)

	result := fmt.Sprintf("%d:", assignment.Epoch)
	for _, leaderID := range leaderIDs {
		result = fmt.Sprintf("%s(%d)", result, leaderID)

		buckets := assignment.Buckets[int32(leaderID)].Vals
		sort.Slice(buckets, func(i int, j int) bool { return buckets[i] < buckets[j] })

		for _, b := range buckets {
			result = fmt.Sprintf("%s %d", result, b)
		}
	}
	return result
}


