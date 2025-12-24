package main

import (
	"context"
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
	privKey     interface{}

	// Number of requests the client tries to submit before stopping.
	// Set to 0 for no limit (and define a running time to make the client stop after a certain time).
	numRequests int

	// Set to a non-zero value to stop submitting requests.
	stop int32

	requests     map[int32]*pb.ClientRequest
	reqClients    map[int32]pb.Messenger_RequestClient
	reqSinks      map[int32]chan *pb.ClientRequest
	bucketClients map[int32]pb.Messenger_BucketsClient

	responses   map[int32]map[int32]bool
	submittedTo map[int32]map[int32]bool

	sentTimestamps   map[int32]int64
	submitTimestamps map[int32]int64
	finished         map[int32]bool

	oldestClientSN   int32
	watermarkWindow  chan *pb.ClientRequest
	sendBufferSize   int
	epoch            int32
	maxBucketID      int
	currentBucketAssignment map[int]int32
	bucketAssignments       map[string]*pb.BucketAssignment
	bucketAssignmentCounts  map[int32]map[string]int

	log     zerolog.Logger
	logFile *os.File
	trace   tracing.Trace

	// fallback (sem assignment) envia para 1 nó por round-robin
	defaultOrderers []int32
	rrIndex         int32

	// Cancelamento do client
	cancel context.CancelFunc
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

	// Must happen first (sets ownClientID)
	cl.discoverPeers(dServAddr)

	// Log file
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

	// Load signing key
	if config.Config.SignRequests {
		cl.loadPrivKey(config.Config.ClientPrivKeyFile)
	}

	// Precompute requests if configured
	if config.Config.PrecomputeRequests {
		cl.log.Info().Int("numRequests", numRequests).Msg("Precomputing requests.")
		for seqNr := int32(0); seqNr < int32(cl.numRequests); seqNr++ {
			cl.requests[seqNr] = cl.createRequest(seqNr)
		}
	}

	return cl
}

func (c *client) loadPrivKey(_ string) {
	var err error
	c.privKey, err = crypto.PrivateKeyFromFile(config.Config.ClientPrivKeyFile)
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
		Msg("Registered with discovery server.")

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
		sig, err := crypto.Sign(request.Digest(req), c.privKey)
		if err != nil {
			c.log.Error().Err(err).Int32("clSn", seqNr).Msg("Failed signing request.")
		}
		req.Signature = sig
	}

	return req
}

// Run: connect, submit, wait responses, exit safely on timeout/cancel.
func (c *client) Run(wg *sync.WaitGroup) {
	defer wg.Done()

	// Select orderers
	var ordererIDs []int32
	if config.Config.LeaderPolicy == "SimulatedRandomFailures" {
		ordererIDs = manager.NewLeaderPolicy(config.Config.LeaderPolicy).GetLeaders(0)
	} else {
		ordererIDs = membership.AllNodeIDs()
	}
	c.defaultOrderers = ordererIDs

	// Client context
	var (
		ctx    context.Context
		cancel context.CancelFunc
	)
	if config.Config.ClientRunTime != 0 {
		ctx, cancel = context.WithTimeout(context.Background(), time.Duration(config.Config.ClientRunTime)*time.Millisecond)
		c.log.Info().Int("clientRunTime", config.Config.ClientRunTime).Msg("Client timeout enabled (context).")
	} else {
		ctx, cancel = context.WithCancel(context.Background())
		c.log.Info().Msg("Client timeout disabled (context).")
	}
	c.cancel = cancel
	defer cancel()

	// Connect
	var reqConns map[int32]*grpc.ClientConn
	c.reqClients, c.bucketClients, reqConns = messenger.ConnectToOrderers(c.ownClientID, c.log, ordererIDs)

	// Close everything on exit (and on ctx.Done)
	closeAllOnce := sync.Once{}
	closeAll := func(reason string) {
		closeAllOnce.Do(func() {
			c.log.Info().Str("reason", reason).Msg("Closing client connections and channels.")

			// Close request channels (stops send goroutines)
			for _, ch := range c.reqSinks {
				// reqSinks might not be initialized yet; guard below
				if ch != nil {
					close(ch)
				}
			}

			// Close bucket assignment streams
			for peerID, cl := range c.bucketClients {
				if err := cl.CloseSend(); err != nil {
					logger.Error().Err(err).Int32("peerId", peerID).Msg("Failed to close bucket assignment connection.")
				}
			}

			// Close grpc conns to break Recv()
			for peerID, conn := range reqConns {
				if err := conn.Close(); err != nil {
					c.log.Error().Err(err).Int32("ordererID", peerID).Msg("Failed to close client request connection.")
				}
			}
		})
	}
	defer closeAll("defer-exit")
	go func() {
		<-ctx.Done()
		atomic.StoreInt32(&c.stop, 1)
		c.log.Info().Err(ctx.Err()).Msg("Client context done; stopping.")
		closeAll("ctx-done")
	}()

	// Start senders/receivers
	c.startRequestSenders(ctx)
	c.startBucketAssignmentReceivers(ctx)

	c.log.Info().Msg("Connected to orderers.")

	// Tracing
	c.trace.Start(fmt.Sprintf("%s-%03d.trc", outFilePrefix, c.ownClientID), -1*c.ownClientID)
	defer c.trace.Stop()

	// Special case: RequestRate==0 means "sleep"
	if config.Config.RequestRate == 0 {
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Duration(config.Config.ClientRunTime) * time.Millisecond):
			return
		}
	}

	// Responses handlers
	responseHandlerWG := c.startResponseHandlers(ctx)

	// Submit loop
	c.log.Info().Int("numRequests", c.numRequests).Msg("Starting to submit requests.")

	timeBetweenRequests := int64(1000000 / config.Config.RequestRate)
	nextSubmitTime := time.Now().UnixNano() / 1000 // us

	var i int32
	for i = 0; i < int32(c.numRequests) && atomic.LoadInt32(&c.stop) == 0; i++ {
		select {
		case <-ctx.Done():
			c.log.Info().Int32("nReq", i).Msg("Stopping submit loop (ctx done).")
			atomic.StoreInt32(&c.stop, 1)
			break
		default:
		}

		// Rate control
		if config.Config.RequestRate != -1 {
			now := time.Now().UnixNano() / 1000
			c.trace.Event(tracing.CLIENT_SLACK, int64(i), (nextSubmitTime - now))
			if now < nextSubmitTime {
				sleepDur := time.Duration(nextSubmitTime-now) * time.Microsecond
				select {
				case <-ctx.Done():
					atomic.StoreInt32(&c.stop, 1)
					break
				case <-time.After(sleepDur):
				}
				nextSubmitTime += timeBetweenRequests
			} else {
				if config.Config.HardRequestRateLimit {
					nextSubmitTime = now + timeBetweenRequests
				} else {
					nextSubmitTime += timeBetweenRequests
				}
			}
		}

		// Blocks while watermark window is full - BUT cancellable.
		if ok := c.submitRequest(ctx, i); !ok {
			break
		}
	}

	c.log.Info().Int32("nReq", i).Msg("Finished submitting requests.")
	atomic.StoreInt32(&c.stop, 1)

	// Wait for in-flight to finish, but stop on ctx.Done()
	for {
		c.Lock()
		pending := len(c.submittedTo)
		c.Unlock()

		if pending == 0 {
			break
		}

		select {
		case <-ctx.Done():
			c.log.Warn().Int("pending", pending).Msg("Exiting with pending in-flight requests (ctx done).")
			break
		case <-time.After(250 * time.Millisecond):
		}
	}

	// Stop response handlers by closing conns (already in closeAll on ctx.Done or defer)
	closeAll("normal-finish")
	responseHandlerWG.Wait()

	// Close log file.
	_ = c.logFile.Close()
}

func (c *client) startRequestSenders(ctx context.Context) {
	c.reqSinks = make(map[int32]chan *pb.ClientRequest, len(c.reqClients))
	for peerID, reqClient := range c.reqClients {
		c.reqSinks[peerID] = c.sendRequests(ctx, peerID, reqClient)
	}
}

func (c *client) sendRequests(ctx context.Context, ordererID int32, clientStub pb.Messenger_RequestClient) chan *pb.ClientRequest {
	ch := make(chan *pb.ClientRequest, c.sendBufferSize)

	go func() {
		defer func() {
			_ = clientStub.CloseSend()
		}()

		for {
			select {
			case <-ctx.Done():
				return
			case req, ok := <-ch:
				if !ok {
					return
				}
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
		}
	}()

	return ch
}

// submitRequest returns false if it aborted due to ctx.Done()
func (c *client) submitRequest(ctx context.Context, seqNr int32) bool {
	var req *pb.ClientRequest
	if config.Config.PrecomputeRequests {
		req = c.requests[seqNr]
	} else {
		req = c.createRequest(seqNr)
	}

	c.Lock()
	c.submitTimestamps[seqNr] = time.Now().UnixNano() / 1000 // us
	c.Unlock()

	// Watermark control, cancellable
	select {
	case <-ctx.Done():
		return false
	case c.watermarkWindow <- req:
	}

	c.Lock()

	// Choose destinations
	var destIDs []int32
	if c.currentBucketAssignment != nil {
		destIDs = c.guessTargetOrderers(req)
	} else {
		destIDs = []int32{c.pickDefaultOrderer()}
	}

	// Init request state
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

	// Send
	for _, ordererID := range destIDs {
		ch := c.reqSinks[ordererID]
		if ch == nil {
			c.log.Warn().Int32("ordererId", ordererID).Msg("Not sending request to orderer. No connection established.")
			continue
		}
		select {
		case <-ctx.Done():
			return false
		case ch <- req:
		}
	}

	c.log.Debug().Int32("clSeqNr", req.RequestId.ClientSn).Msg("Submitted request.")
	return true
}

func (c *client) startResponseHandlers(ctx context.Context) *sync.WaitGroup {
	wg := sync.WaitGroup{}
	wg.Add(len(c.reqClients))
	for peerID, clientStub := range c.reqClients {
		go c.handleResponses(ctx, clientStub, peerID, &wg)
	}
	return &wg
}

func (c *client) handleResponses(ctx context.Context, clientStub pb.Messenger_RequestClient, peerID int32, wg *sync.WaitGroup) {
	defer wg.Done()

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		response, err := clientStub.Recv()
		if err != nil {
			c.log.Info().Err(err).Int32("peerId", peerID).Msg("Response handler done.")
			return
		}

		c.log.Debug().Int32("clSeqNr", response.ClientSn).
			Int32("peerId", peerID).
			Msg("Received response for request.")
		c.registerResponse(response.ClientSn, peerID)
	}
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

func (c *client) startBucketAssignmentReceivers(ctx context.Context) {
	for peerID, cl := range c.bucketClients {
		go c.receiveBucketAssignments(ctx, peerID, cl)
	}
}

func (c *client) receiveBucketAssignments(ctx context.Context, _ int32, cl pb.Messenger_BucketsClient) {
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		assignment, err := cl.Recv()
		if err != nil {
			c.log.Info().Err(err).Msg("Bucket assignment stream ended.")
			return
		}
		c.registerBucketAssignment(ctx, assignment)
	}
}

func (c *client) registerBucketAssignment(ctx context.Context, assignment *pb.BucketAssignment) {
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
		go c.resubmitPendingRequests(ctx)
	}
}

func (c *client) resubmitPendingRequests(ctx context.Context) {
	c.Lock()
	defer c.Unlock()

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
		c.log.Trace().
			Int32("clSeqNr", req.RequestId.ClientSn).
			Interface("dest", destIDs).
			Msg("Resubmitting Request.")

		for _, destID := range destIDs {
			if submitted[destID] {
				continue
			}

			ch := c.reqSinks[destID]
			if ch == nil {
				continue
			}

			select {
			case <-ctx.Done():
				return
			case ch <- req:
				resubmitted++
				submitted[destID] = true
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

func (c *client) pickDefaultOrderer() int32 {
	list := c.defaultOrderers
	if len(list) == 0 {
		all := membership.AllNodeIDs()
		if len(all) == 0 {
			return 0
		}
		return all[0]
	}
	idx := int(atomic.AddInt32(&c.rrIndex, 1)-1) % len(list)
	return list[idx]
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

