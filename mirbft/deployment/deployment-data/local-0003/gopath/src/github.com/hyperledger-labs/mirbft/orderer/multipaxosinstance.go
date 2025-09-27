package orderer

import (
	"context"
	"crypto/sha256"
	"fmt"
	"sync"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/hyperledger-labs/mirbft/manager"
	"github.com/hyperledger-labs/mirbft/membership"
	"github.com/hyperledger-labs/mirbft/request"
	pb "github.com/hyperledger-labs/mirbft/protobufs"
	"github.com/hyperledger-labs/mirbft/tracing"
)

type instPhase int

const (
	phaseInit instPhase = iota
	phaseProposing
	phaseAcceptSent
	phaseCommitted
)

type mpxInstance struct {
	mu sync.Mutex

	parent        *MultiPaxosOrderer
	sn            int32
	maxBatchSize  int
	proposeEvery  time.Duration
	announce      AnnounceFn
	lastProposeAt time.Time
	closed        bool

	seg manager.Segment

	quorum       int32
	lastVal      *pb.MPxValue
	lastDigest   [32]byte
	acceptCount  int32
	acceptedFrom map[int32]struct{}

	lastReqBatch *request.Batch
	phase        instPhase

	acceptRtxEvery time.Duration
	lastAcceptAt   time.Time

	msgCh   chan *pb.ProtocolMessage
	stopCh  chan struct{}
	stopped bool
	wg      sync.WaitGroup
}

func newMPXInstance(parent *MultiPaxosOrderer, sn int32, announce AnnounceFn, maxBatch int, interval time.Duration) *mpxInstance {
	inst := &mpxInstance{
		parent:         parent,
		sn:             sn,
		maxBatchSize:   maxBatch,
		proposeEvery:   interval,
		announce:       announce,
		lastProposeAt:  time.Now(),
		phase:          phaseInit,
		acceptRtxEvery: interval * 2,
		msgCh:          make(chan *pb.ProtocolMessage, 4096),
		stopCh:         make(chan struct{}),
	}
	fmt.Printf("[MPX][CHK] instance init sn=%d\n", sn)
	return inst
}

func (i *mpxInstance) setSegment(seg manager.Segment) {
	i.mu.Lock()
	defer i.mu.Unlock()

	i.seg = seg
	n := int32(len(membership.AllNodeIDs()))
	if n < 1 {
		n = 1
	}
	i.quorum = n/2 + 1

	fmt.Printf("[MPX][CHK] seg bind sn=%d segID=%d firstSN=%d len=%d quorum=%d leaders=%v\n",
		i.sn, seg.SegID(), seg.FirstSN(), seg.Len(), i.quorum, seg.Leaders())
}

// ---------------- workers (serialização) ----------------

func (i *mpxInstance) startWorkers(wg *sync.WaitGroup) {
	i.wg.Add(1)
	go func() {
		defer i.wg.Done()
		for {
			select {
			case pm := <-i.msgCh:
				i.handleMPxMsg(pm, pm.GetMultipaxos())
			case <-i.stopCh:
				return
			}
		}
	}()
}
func (i *mpxInstance) stopWorkers() {
	i.mu.Lock()
	if i.stopped {
		i.mu.Unlock()
		return
	}
	i.stopped = true
	close(i.stopCh)
	close(i.msgCh)
	i.mu.Unlock()
	i.wg.Wait()
}
func (i *mpxInstance) enqueue(pm *pb.ProtocolMessage) {
	select {
	case i.msgCh <- pm:
	default:
		go func() { i.msgCh <- pm }()
	}
}
func (i *mpxInstance) isClosed() bool {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.closed
}

// ---------------- handlers ----------------

func (i *mpxInstance) handleMPxMsg(pm *pb.ProtocolMessage, mpx *pb.MPxMsg) {
	switch t := mpx.Type.(type) {
	case *pb.MPxMsg_Prepare:
		fmt.Printf("[MPX][IN ] PREPARE sn=%d from=%d\n", i.sn, pm.GetSenderId())
	case *pb.MPxMsg_Promise:
		fmt.Printf("[MPX][IN ] PROMISE sn=%d from=%d\n", i.sn, pm.GetSenderId())
	case *pb.MPxMsg_Accept:
		fmt.Printf("[MPX][IN ] ACCEPT sn=%d from=%d\n", i.sn, pm.GetSenderId())
		i.onAccept(t.Accept)
	case *pb.MPxMsg_Accepted:
		fmt.Printf("[MPX][IN ] ACCEPTED sn=%d from=%d\n", i.sn, pm.GetSenderId())
		i.onAccepted(pm, t.Accepted)
	case *pb.MPxMsg_Commit:
		fmt.Printf("[MPX][IN ] COMMIT sn=%d from=%d\n", i.sn, pm.GetSenderId())
		i.onCommit(t.Commit)
	default:
		fmt.Printf("[MPX][WARN] msg MPx desconhecida sn=%d (%T)\n", i.sn, mpx.Type)
	}
}

func (i *mpxInstance) onAccept(a *pb.MPxAccept) {
	i.mu.Lock()
	defer i.mu.Unlock()

	if a.GetValue() != nil {
		if i.lastVal != nil {
			incomingDigest := sha256.Sum256(a.GetValue().GetBatch())
			if incomingDigest != i.lastDigest {
				fmt.Printf("[MPX][WARN] ACCEPT sn=%d digest mismatch (have=%x new=%x) → ignore\n",
					i.sn, i.lastDigest[:8], incomingDigest[:8])
				return
			}
		} else {
			i.lastVal = a.GetValue()
			i.lastDigest = sha256.Sum256(i.lastVal.GetBatch())
		}
	}

	if i.acceptedFrom == nil {
		i.acceptedFrom = make(map[int32]struct{})
	}
	if _, ok := i.acceptedFrom[membership.OwnID]; !ok {
		i.acceptedFrom[membership.OwnID] = struct{}{}
		i.acceptCount++
	}

	fmt.Printf("[MPX][QUORUM] ACCEPT sn=%d acceptCount=%d quorum=%d digest=%x\n",
		i.sn, i.acceptCount, i.quorum, i.lastDigest[:8])

	accepted := &pb.MPxMsg{Type: &pb.MPxMsg_Accepted{Accepted: &pb.MPxAccepted{
		Id:     &pb.MPxInstanceId{Sn: i.sn, Lead: uint64(membership.OwnID)},
		Ballot: 0,
		Ok:     true,
	}}}
	pm := &pb.ProtocolMessage{
		SenderId: membership.OwnID,
		Sn:       i.sn,
		Msg:      &pb.ProtocolMessage_Multipaxos{Multipaxos: accepted},
	}
	fmt.Printf("[MPX][NET] SEND Accepted sn=%d\n", i.sn)
	if i.parent.emit != nil {
		i.parent.emit(pm)
	} else {
		fmt.Printf("[MPX][NET][WARN] emit=nil; não enviou Accepted sn=%d\n", i.sn)
	}
}

func (i *mpxInstance) onAccepted(pm *pb.ProtocolMessage, _ *pb.MPxAccepted) {
	i.mu.Lock()
	defer i.mu.Unlock()

	if pm != nil {
		if i.acceptedFrom == nil {
			i.acceptedFrom = make(map[int32]struct{})
		}
		if _, ok := i.acceptedFrom[pm.SenderId]; ok {
			return
		}
		i.acceptedFrom[pm.SenderId] = struct{}{}
	}
	i.acceptCount++

	fmt.Printf("[MPX][QUORUM] ACCEPTED sn=%d count=%d/%d digest=%x\n",
		i.sn, i.acceptCount, i.quorum, i.lastDigest[:8])

	if i.acceptCount >= i.quorum && i.lastVal != nil && i.phase != phaseCommitted {
		fmt.Printf("[MPX][QUORUM] MAJORITY REACHED sn=%d → COMMIT digest=%x\n", i.sn, i.lastDigest[:8])

		commit := &pb.MPxMsg{Type: &pb.MPxMsg_Commit{
			Commit: &pb.MPxCommit{
				Id:    &pb.MPxInstanceId{Sn: i.sn, Lead: uint64(membership.OwnID)},
				Value: i.lastVal,
			},
		}}
		pmOut := &pb.ProtocolMessage{
			SenderId: membership.OwnID,
			Sn:       i.sn,
			Msg:      &pb.ProtocolMessage_Multipaxos{Multipaxos: commit},
		}
		if i.parent.emit != nil {
			fmt.Printf("[MPX][NET] SEND Commit sn=%d digest=%x\n", i.sn, i.lastDigest[:8])
			i.parent.emit(pmOut)
		} else {
			fmt.Printf("[MPX][NET][WARN] emit=nil; não enviou Commit sn=%d\n", i.sn)
		}
	}

	if i.acceptCount >= i.quorum && i.lastVal != nil && i.phase != phaseCommitted {
		val := i.lastVal
		i.mu.Unlock()
		i.onCommit(&pb.MPxCommit{Id: &pb.MPxInstanceId{Sn: i.sn}, Value: val})
		i.mu.Lock()
	}
}

func (i *mpxInstance) onCommit(c *pb.MPxCommit) {
	i.mu.Lock()
	defer i.mu.Unlock()

	if i.phase == phaseCommitted {
		return
	}
	val := c.GetValue()
	if val == nil || len(val.GetBatch()) == 0 {
		fmt.Printf("[MPX][CHK] COMMIT NIL sn=%d (⊥)\n", i.sn)
		return
	}

	if i.lastVal == nil {
		i.lastVal = val
		i.lastDigest = sha256.Sum256(val.GetBatch())
	}

	i.phase = phaseCommitted

	commit := &pb.MPxMsg{Type: &pb.MPxMsg_Commit{
		Commit: &pb.MPxCommit{
			Id:    &pb.MPxInstanceId{Sn: i.sn, Lead: uint64(membership.OwnID)},
			Value: i.lastVal,
		},
	}}
	pmOut := &pb.ProtocolMessage{
		SenderId: membership.OwnID,
		Sn:       i.sn,
		Msg:      &pb.ProtocolMessage_Multipaxos{Multipaxos: commit},
	}
	if i.parent.emit != nil {
		fmt.Printf("[MPX][NET] GOSSIP Commit sn=%d digest=%x\n", i.sn, i.lastDigest[:8])
		i.parent.emit(pmOut)
	}

	if i.lastReqBatch != nil {
		request.RemoveBatch(i.lastReqBatch)
		i.lastReqBatch = nil
	}

	var b pb.Batch
	if err := proto.Unmarshal(i.lastVal.GetBatch(), &b); err != nil {
		fmt.Printf("[MPX][ERR] COMMIT sn=%d unmarshal batch: %v\n", i.sn, err)
		return
	}
	if i.announce != nil {
		fmt.Printf("[MPX][CHK] ANNOUNCE sn=%d reqs=%d\n", i.sn, len(b.Requests))
		i.announce(i.sn, i.lastVal.GetBatch(), nil)
	} else {
		fmt.Printf("[MPX][WARN] COMMIT sn=%d announcer=nil — não vai acionar Responder!\n", i.sn)
	}

	i.closed = true
	traceCommit(i.sn, len(b.Requests))
}

// ---------------- proposição / tick ----------------

func (i *mpxInstance) ProposeIfDue(ctx context.Context) {
	i.mu.Lock()
	defer i.mu.Unlock()

	if time.Since(i.lastProposeAt) < i.proposeEvery {
		return
	}
	i.lastProposeAt = time.Now()

	if i.phase >= phaseAcceptSent {
		return
	}

	var val *pb.MPxValue
	reqs := 0

	if i.lastVal == nil {
		rb := i.cutReqBatch()
		if rb == nil || rb.Message() == nil || len(rb.Message().Requests) == 0 {
			return
		}
		i.lastReqBatch = rb
		batchMsg := rb.Message()
		reqs = len(batchMsg.Requests)

		fmt.Printf("PROPOSE sn=%d size=%d\n", i.sn, reqs)
		batchBytes, err := proto.Marshal(batchMsg)
		if err != nil {
			fmt.Printf("[MPX][ERR] PROPOSE sn=%d marshal batch: %v\n", i.sn, err)
			return
		}

		val = &pb.MPxValue{
			Id:    &pb.MPxInstanceId{Sn: i.sn, Lead: uint64(membership.OwnID)},
			Batch: batchBytes,
		}
		i.lastVal = val
		i.lastDigest = sha256.Sum256(batchBytes)
		i.acceptedFrom = map[int32]struct{}{}
		i.acceptCount = 0

		i.acceptedFrom[membership.OwnID] = struct{}{}
		i.acceptCount = 1
		i.phase = phaseProposing
	} else {
		val = i.lastVal
	}

	accept := &pb.MPxMsg{Type: &pb.MPxMsg_Accept{Accept: &pb.MPxAccept{
		Id:     &pb.MPxInstanceId{Sn: i.sn, Lead: uint64(membership.OwnID)},
		Ballot: 0,
		Value:  val,
	}}}
	pm := &pb.ProtocolMessage{
		SenderId: membership.OwnID,
		Sn:       i.sn,
		Msg:      &pb.ProtocolMessage_Multipaxos{Multipaxos: accept},
	}
	if i.parent.emit != nil {
		fmt.Printf("[MPX][NET] SEND Accept sn=%d bytes=%d digest=%x\n", i.sn, len(val.GetBatch()), i.lastDigest[:8])
		i.parent.emit(pm)
	} else {
		fmt.Printf("[MPX][NET][WARN] emit=nil; não enviou Accept sn=%d\n", i.sn)
	}

	i.phase = phaseAcceptSent
	i.lastAcceptAt = time.Now()
	tracePropose(i.sn, reqs)
}

func (i *mpxInstance) tick(now time.Time) {
	i.mu.Lock()
	defer i.mu.Unlock()

	if i.phase == phaseAcceptSent && i.acceptCount < i.quorum && now.Sub(i.lastAcceptAt) >= i.acceptRtxEvery {
		if i.parent.emit != nil && i.lastVal != nil {
			pm := &pb.ProtocolMessage{
				SenderId: membership.OwnID,
				Sn:       i.sn,
				Msg: &pb.ProtocolMessage_Multipaxos{
					Multipaxos: &pb.MPxMsg{Type: &pb.MPxMsg_Accept{
						Accept: &pb.MPxAccept{
							Id:     &pb.MPxInstanceId{Sn: i.sn, Lead: uint64(membership.OwnID)},
							Ballot: 0,
							Value:  i.lastVal,
						},
					}},
				},
			}
			fmt.Printf("[MPX][RTX] RESEND Accept sn=%d digest=%x\n", i.sn, i.lastDigest[:8])
			i.parent.emit(pm)
			i.lastAcceptAt = now
		}
	}
}

// --------------- util ---------------

func (i *mpxInstance) cutReqBatch() *request.Batch {
	if i.seg == nil {
		return nil
	}
	size := i.seg.BatchSize()
	timeout := i.proposeEvery
	i.seg.Buckets().WaitForRequests(int(size), timeout)
	return i.seg.Buckets().CutBatch(int(size), timeout)
}

func (i *mpxInstance) Close() {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.closed {
		return
	}
	i.closed = true
	fmt.Printf("[MPX][CHK] instance close sn=%d\n", i.sn)
}

// Traces mínimos (mantidos)
func tracePropose(sn int32, size int) {
	tracing.MainTrace.Event(tracing.PROPOSE, int64(sn), int64(size))
	fmt.Printf("[MPX] PROPOSE sn=%d size=%d\n", sn, size)
}
func traceCommit(sn int32, size int) {
	tracing.MainTrace.Event(tracing.COMMIT, int64(sn), int64(size))
	fmt.Printf("[MPX] COMMIT  sn=%d size=%d\n", sn, size)
}

