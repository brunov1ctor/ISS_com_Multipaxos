package main

import (
	"fmt"
	"os"
	"sort"
	"strconv"
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
	privKey     interface{}

	// Number of requests the client tries to submit before stopping.
	// Set to 0 for no limit (and define a running time to make the client stop after a certain time).
	numRequests int

	// Set to a non-zero value to stop submitting requests.
	stop int32

	requests map[int32]*pb.ClientRequest

	reqClients map[int32]pb.Messenger_RequestClient
	reqSinks   map[int32]chan *pb.ClientRequest

	bucketClients map[int32]pb.Messenger_BucketsClient

	responses    map[int32]map[int32]bool
	submittedTo  map[int32]map[int32]bool
	sentTimestamps   map[int32]int64
	submitTimestamps map[int32]int64
	finished     map[int32]bool

	oldestClientSN int32
	watermarkWindow chan *pb.ClientRequest
	sendBufferSize  int

	epoch                   int32
	maxBucketID             int
	currentBucketAssignment map[int]int32
	bucketAssignments       map[string]*pb.BucketAssignment
	bucketAssignmentCounts  map[int32]map[string]int

	log     zerolog.Logger
	logFile *os.File

	trace tracing.Trace
}

// ===== DRAIN FIX (MINIMAL) =====
// Reads CLIENT_DRAIN_TIME_MS from env. Default 5000ms.
// 0 disables the timed drain (falls back to old "wait until submittedTo empty").
func clientDrainTime() time.Duration {
	const defMS = 5000
	v := os.Getenv("CLIENT_DRAIN_TIME_MS")
	if v == "" {
		return time.Duration(defMS) * time.Millisecond
	}
	ms, err := strconv.Atoi(v)
	if err != nil {
		return time.Duration(defMS) * time.Millisecond
	}
	if ms <= 0 {
		return 0
	}
	return time.Duration(ms) * time.Millisecond
}

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

	cl.discoverPeers(dServAddr)

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

	if config.Config.SignRequests {
		cl.loadPrivKey(config.Config.ClientPrivKeyFile)
	}

	if config.Config.PrecomputeRequests {
		cl.log.Info().Int("numRequests", numRequests).Msg("Precomputing requests.")
		for seqNr := int32(0); seqNr < int32(cl.numRequests); seqNr++ {
			cl.requests[seqNr] = cl.createRequest(seqNr)
		}
	}

	return cl
}

func (c *client) loadPrivKey(privKeyFile string) {
	var err error
	c.privKey, err = crypto.PrivateKeyFromFile(privKeyFile)
	if err != nil {
		c.log.Error().
			Err(err).
			Str("fileName", config.Config.ClientPrivKeyFile).
			Msg("Could not load client private key from file.")
	}
}

func (c *client) discoverPeers(dServAddr string) {
	var ordererIdentities []*pb.NodeIdentity
	c.ownClientID, ordererIdentities = discovery.RegisterClient(dServAddr)
	logger.Info().
		Int32("ownClientId", c.ownClientID).
		Int("numOrderers", len(ordererIdentities)).
		Msg("Registared with discovery server.")

	membershipInitializer.Do(func() {
		membership.InitNodeIdentities(ordererIdentities)
	})
}

func (c *client) createRequest(seqNr int32) *pb.ClientRequest {
	req := &pb.ClientRequest{
		RequestId: &pb.RequestID{
			ClientId: c.ownClientID,
			ClientSn: seqNr,
		},
		Payload:   randomRequestPayload,
		Signature: nil,
	}

	if config.Config.SignRequests {
		var err error
		req.Signature, err = crypto.Sign(request.Digest(req), c.privKey)
		if err != nil {
			c.log.Error().Err(err).Int32("clSn", seqNr).Msg("Failed signing request.")
		}
	}

	return req
}

func (c *client) Run(wg *sync.WaitGroup) {
	defer wg.Done()

	var ordererIDs []int32
	if config.Config.LeaderPolicy == "SimulatedRandomFailures" {
		ordererIDs = manager.NewLeaderPolicy(config.Config.LeaderPolicy).GetLeaders(0)
	} else {
		ordererIDs = membership.AllNodeIDs()
	}

	var reqConns map[int32]*grpc.ClientConn
	c.reqClients, c.bucketClients, reqConns = messenger.ConnectToOrderers(c.ownClientID, c.log, ordererIDs)
	c.startRequestSenders()
	c.startBucketAssignmentReceivers()

	c.log.Info().Msg("Connected to orderers.")

	c.trace.Start(fmt.Sprintf("%s-%03d.trc", outFilePrefix, c.ownClientID), -1*c.ownClientID)
	defer c.trace.Stop()

	if config.Config.RequestRate == 0 {
		time.Sleep(time.Duration(config.Config.ClientRunTime) * time.Millisecond)
	} else {
		responseHandlerWG := c.startResponseHandlers()

		if config.Config.ClientRunTime != 0 {
			c.log.Info().Int("clientRunTime", config.Config.ClientRunTime).Msg("Setting up client timeout.")
			time.AfterFunc(time.Duration(config.Config.ClientRunTime)*time.Millisecond, func() {
				atomic.StoreInt32(&c.stop, 1)
				c.log.Info().Int("clientRunTime", config.Config.ClientRunTime).Msg("Stopping client on timeout.")
			})
		}

		c.log.Info().Int("numRequests", c.numRequests).Msg("Starting to submit requests.")

		timeBetweenRequests := int64(1000000 / config.Config.RequestRate)
		nextSubmitTime := time.Now().UnixNano() / 1000

		var i int32
		for i = int32(0); i < int32(c.numRequests) && atomic.LoadInt32(&c.stop) == 0; i++ {
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

			if atomic.LoadInt32(&c.stop) != 0 {
				break
			}

			c.submitRequest(i)
		}
		c.log.Info().Int32("nReq", i).Msg("Finished submitting requests.")
		atomic.StoreInt32(&c.stop, 1)

		// ===== DRAIN FIX (MINIMAL) =====
		// Instead of waiting forever for submittedTo to become empty, wait up to drain time.
		// This keeps gRPC Recv handlers alive so late replies can arrive, preventing "transport is closing".
		drain := clientDrainTime()
		if drain > 0 {
			deadline := time.Now().Add(drain)
			c.log.Info().Dur("drain", drain).Msg("Draining responses before closing connections.")
			for {
				c.Lock()
				pending := len(c.submittedTo)
				c.Unlock()

				if pending == 0 {
					break
				}
				if time.Now().After(deadline) {
					c.log.Warn().Int("pending", pending).Msg("Drain timeout reached; closing connections with pending requests.")
					break
				}
				time.Sleep(200 * time.Millisecond)
			}
		} else {
			// Old behavior: wait until everything is done (can hang forever if peers stop early).
			c.Lock()
			for len(c.submittedTo) > 0 {
				c.Unlock()
				time.Sleep(time.Second)
				c.Lock()
			}
			c.Unlock()
		}

		// Close request connections (triggers Recv() to end; handlers will exit and WG will complete).
		for peerID, conn := range reqConns {
			if err := conn.Close(); err != nil {
				c.log.Error().Err(err).Int32("ordererID", peerID).Msg("Failed to close client request connection.")
			}
		}
		responseHandlerWG.Wait()
	}

	for _, ch := range c.reqSinks {
		close(ch)
	}

	for peerID, cl := range c.bucketClients {
		if err := cl.CloseSend(); err != nil {
			logger.Error().Err(err).Int32("peerId", peerID).Msg("Falied to close bucket assignment connection.")
		}
	}

	c.logFile.Close()
}

func (c *client) startRequestSenders() {
	c.reqSinks = make(map[int32]chan *pb.ClientRequest, len(c.reqClients))
	for peerID, reqClient := range c.reqClients {
		c.reqSinks[peerID] = c.sendRequests(peerID, reqClient)
	}
}

func (c *client) sendRequests(ordererID int32, clientStub pb.Messenger_RequestClient) chan *pb.ClientRequest {
	ch := make(chan *pb.ClientRequest, c.sendBufferSize)

	go func() {
		for req := range ch {
			c.Lock()
			c.sentTimestamps[req.RequestId.ClientSn] = time.Now().UnixNano() / 1000
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

func (c *client) submitRequest(seqNr int32) {
	if atomic.LoadInt32(&c.stop) != 0 {
		return
	}

	var req *pb.ClientRequest
	if config.Config.PrecomputeRequests {
		req = c.requests[seqNr]
	} else {
		req = c.createRequest(seqNr)
	}

	c.Lock()
	c.submitTimestamps[seqNr] = time.Now().UnixNano() / 1000
	c.Unlock()

	if atomic.LoadInt32(&c.stop) != 0 {
		return
	}

	c.watermarkWindow <- req

	if atomic.LoadInt32(&c.stop) != 0 {
		return
	}

	c.Lock()

	var destIDs []int32
	if c.currentBucketAssignment != nil {
		destIDs = c.guessTargetOrderers(req)
	} else {
		destIDs = membership.AllNodeIDs()
	}

	c.requests[seqNr] = req
	c.responses[seqNr] = make(map[int32]bool)
	c.finished[seqNr] = false
	c.submittedTo[seqNr] = make(map[int32]bool)
	for _, destID := range destIDs {
		c.submittedTo[seqNr][destID] = true
	}
	c.Unlock()

	c.trace.Event(tracing.REQ_SEND, int64(seqNr), 0)

	for _, ordererID := range destIDs {
		if atomic.LoadInt32(&c.stop) != 0 {
			return
		}
		if c.reqSinks[ordererID] != nil {
			c.reqSinks[ordererID] <- req
		} else {
			c.log.Warn().Int32("ordererId", ordererID).Msg("Not sending request to orderer. No connection established.")
		}
	}

	c.log.Debug().Int32("clSeqNr", req.RequestId.ClientSn).Msg("Submitted request.")
}

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

		if atomic.LoadInt32(&c.stop) != 0 {
			return
		}
		go c.resubmitPendingRequests()
	}
}

func (c *client) resubmitPendingRequests() {
	if atomic.LoadInt32(&c.stop) != 0 {
		return
	}

	c.Lock()
	defer c.Unlock()

	if atomic.LoadInt32(&c.stop) != 0 {
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
					if atomic.LoadInt32(&c.stop) != 0 {
						return
					}
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
			result = fmt.Sprintf("%s %d", result, b)
		}
	}
	return result
}

