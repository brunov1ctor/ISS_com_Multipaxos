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
	reqFanout = 3
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

	// --- MINIMAL FIX: shutdown signal to prevent sending while stopping ---
	done     chan struct{}
	doneOnce sync.Once
	// --------------------------------------------------------------------

	// All requests the client will submit (indexed by client sequence number).
	// All clients running on the same machine first create (ans sigh) all requests
	// and only then start submitting them.
	// This is to remove the CPU bottleneck of running many clients on the same machine.
	// Filled with data on initialization.
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

	// Logger used by this client.
	// Each client's log goes in a separate file, even if multiple clients are running from within the same process.
	log     zerolog.Logger
	logFile *os.File // Kept around only for closing.

	// Same as with logging, each client has a separate trace that is output in a separate file,
	// even if multiple clients are running in the same process.
	trace tracing.Trace
}

func (c *client) shutdown() {
	c.doneOnce.Do(func() { close(c.done) })
}

// Allocates and returns a pointer to a new client.
func newClient(dServAddr string, numRequests int) *client {
	cl := &client{
		ownClientID:            -1,
		numRequests:            numRequests,
		stop:                   0,
		done:                   make(chan struct{}), // MINIMAL FIX
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
		// currentBucketAssignment will be initialized later (needs to be checked for nil when accessing)
		trace: &tracing.BufferedTrace{
			Sampling:              config.Config.ClientTraceSampling,
			BufferCapacity:        config.Config.EventBufferSize,
			ProtocolEventCapacity: config.Config.EventBufferSize,
			RequestEventCapacity:  config.Config.EventBufferSize,
		},
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
	}

	// Generate all request messages if configured to do so
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
		Msg("Registared with discovery server.")

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

	return req
}

// Runs the main client logic that
// - connects to the orderers and
// - submits requests according to the configuration read from the config file.
func (c *client) Run(wg *sync.WaitGroup) {
	defer wg.Done()
	defer c.shutdown() // MINIMAL FIX: stop background goroutines cleanly no matter what

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

	if config.Config.RequestRate == 0 {
		time.Sleep(time.Duration(config.Config.ClientRunTime) * time.Millisecond)
	} else {
		// Create response handler threads.
		responseHandlerWG := c.startResponseHandlers()

		// Make client stop after a predefined time, if configured
		if config.Config.ClientRunTime != 0 {
			c.log.Info().Int("clientRunTime", config.Config.ClientRunTime).Msg("Setting up client timeout.")
			time.AfterFunc(time.Duration(config.Config.ClientRunTime)*time.Millisecond, func() {
				atomic.StoreInt32(&c.stop, 1)
				c.shutdown() // MINIMAL FIX
				c.log.Info().Int("clientRunTime", config.Config.ClientRunTime).Msg("Stopping client on timeout.")
			})
		}

		c.log.Info().Int("numRequests", c.numRequests).Msg("Starting to submit requests.")

		timeBetweenRequests := int64(1000000 / config.Config.RequestRate)
		nextSubmitTime := time.Now().UnixNano() / 1000 // Submit first request immediately

		// Submit requests
		var i int32
		for i = int32(0); i < int32(c.numRequests) && atomic.LoadInt32(&c.stop) == 0; i++ {

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

			// blocks while watermark window is full (but will stop promptly on done)
			c.submitRequest(i)
		}
		c.log.Info().Int32("nReq", i).Msg("Finished submitting requests.")
		atomic.StoreInt32(&c.stop, 1)
		c.shutdown() // MINIMAL FIX

		// Wait for enough responses for all requests
		c.Lock()
		for len(c.submittedTo) > 0 {
			c.Unlock()
			time.Sleep(time.Second)
			c.Lock()
		}
		c.Unlock()

		// Close connections; this will cause response handlers to exit.
		for peerID, conn := range reqConns {
			if err := conn.Close(); err != nil {
				c.log.Error().Err(err).Int32("ordererID", peerID).Msg("Failed to close client request connection.")
			}
		}
		responseHandlerWG.Wait()
	}

	// Close bucket assignment connections
	for peerID, cl := range c.bucketClients {
		if err := cl.CloseSend(); err != nil {
			logger.Error().Err(err).Int32("peerId", peerID).Msg("Falied to close bucket assignment connection.")
		}
	}

	// Close log file.
	_ = c.logFile.Close()
}

// Starts all request-sending threads and saves their corresponding input channels in c.reqSinks.
func (c *client) startRequestSenders() {
	c.reqSinks = make(map[int32]chan *pb.ClientRequest, len(c.reqClients))

	for peerID, reqClient := range c.reqClients {
		c.reqSinks[peerID] = c.sendRequests(peerID, reqClient)
	}
}

// Returns a channel to be used to send requests to the peer represented by clientStub.
func (c *client) sendRequests(ordererID int32, clientStub pb.Messenger_RequestClient) chan *pb.ClientRequest {
	ch := make(chan *pb.ClientRequest, c.sendBufferSize)

	go func() {
		defer func() {
			// Close connection when sender goroutine ends.
			if err := clientStub.CloseSend(); err != nil {
				c.log.Error().Err(err).
					Int32("ordererId", ordererID).
					Msg("Failed to close connection to ordering peer.")
			}
		}()

		for {
			select {
			case <-c.done:
				return
			case req := <-ch:
				if req == nil {
					// Shouldn't happen (we never send nil), but don't crash.
					continue
				}

				c.Lock()
				c.sentTimestamps[req.RequestId.ClientSn] = time.Now().UnixNano() / 1000 // In us
				c.Unlock()

				if err := clientStub.Send(req); err != nil {
					c.log.Error().Err(err).
						Int32("ordererId", ordererID).
						Int32("clSeqNr", req.RequestId.ClientSn).
						Msg("Failed sending request to ordering peer.")
					// "antigo-like": não fechamos ch; o conn.Close() no Run derruba tudo.
				}
			}
		}
	}()

	return ch
}

// Submits a single client request with sequence number seqNr.
func (c *client) submitRequest(seqNr int32) {

	var req *pb.ClientRequest = nil
	if config.Config.PrecomputeRequests {
		req = c.requests[seqNr]
	} else {
		req = c.createRequest(seqNr)
	}

	c.Lock()
	c.submitTimestamps[seqNr] = time.Now().UnixNano() / 1000 // In us
	c.Unlock()

	// If shutting down, don't block forever trying to enqueue.
	select {
	case <-c.done:
		return
	case c.watermarkWindow <- req:
	}

	c.Lock()

	// Find out to which orderers to send the request.
	var destIDs []int32
	if c.currentBucketAssignment != nil {
		destIDs = c.guessTargetOrderers(req)
	} else {
		destIDs = membership.AllNodeIDs()
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

	// Send message to all orderers.
	for _, ordererID := range destIDs {
		if c.reqSinks[ordererID] != nil {
			select {
			case <-c.done:
				return
			case c.reqSinks[ordererID] <- req:
			}
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

	strKey := bucketAssignmentToString(assignment)
	c.bucketAssignments[strKey] = assignment
	if msgCounts, ok := c.bucketAssignmentCounts[assignment.Epoch]; ok {
		msgCounts[strKey]++
	} else {
		c.bucketAssignmentCounts[assignment.Epoch] = make(map[string]int)
		c.bucketAssignmentCounts[assignment.Epoch][strKey] = 1
	}

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

		// MINIMAL FIX: don't spawn resubmitter if we're shutting down
		select {
		case <-c.done:
			return
		default:
			go c.resubmitPendingRequests()
		}
	}
}

func (c *client) resubmitPendingRequests() {
	// MINIMAL FIX: don't try to send while shutting down; also avoid holding the lock while sending.
	c.Lock()

	// Quick exit if already stopping.
	select {
	case <-c.done:
		c.Unlock()
		return
	default:
	}

	// Build list of sends to perform outside the lock.
	type sendItem struct {
		destID int32
		req    *pb.ClientRequest
		seqNr  int32
	}
	var sends []sendItem
	resubmitted := 0

	for seqNr, submitted := range c.submittedTo {
		if c.finished[seqNr] {
			continue
		}

		req := c.requests[seqNr]
		if req == nil || c.currentBucketAssignment == nil {
			continue
		}

		destIDs := c.guessTargetOrderers(req)

		for _, destID := range destIDs {
			if !submitted[destID] {
				// mark as submitted while locked
				submitted[destID] = true
				resubmitted++
				sends = append(sends, sendItem{destID: destID, req: req, seqNr: seqNr})
			}
		}
	}

	epoch := c.epoch
	c.Unlock()

	// Perform sends without holding the mutex; stop cleanly on shutdown.
	for _, it := range sends {
		ch := c.reqSinks[it.destID]
		if ch == nil {
			continue
		}
		select {
		case <-c.done:
			return
		case ch <- it.req:
		}
	}

	c.log.Info().
		Int("n", resubmitted).
		Int32("epoch", epoch).
		Msg("Resubmitted Requests.")
}

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
			result = fmt.Sprintf("%s %d", result, b) // FIX: include result
		}
	}

	return result
}

