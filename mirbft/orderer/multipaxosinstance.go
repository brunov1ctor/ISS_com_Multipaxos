// MultiPaxos Instance - Uma instância de consenso para um SN específico.
// Protocolo: PREPARE → PROMISE → ACCEPT → ACCEPTED → COMMIT
package orderer

import (
	"fmt"
	"sync"
	"time"
	"github.com/hyperledger-labs/mirbft/manager"
	"github.com/hyperledger-labs/mirbft/membership"
	"github.com/hyperledger-labs/mirbft/request"
	pb "github.com/hyperledger-labs/mirbft/protobufs"
	"github.com/hyperledger-labs/mirbft/statetransfer"
	"github.com/hyperledger-labs/mirbft/tracing"
)

type instPhase int
const (
	phaseInit instPhase = iota
	phasePrepared
	phaseAcceptSent
	phaseCommitted
)

type mpxInstance struct {
	mu               sync.Mutex
	parent           *MultiPaxosOrderer
	sn               int32
	bucketId         uint32
	groupBucketIDs   []int
	groupBucketGroup *request.BucketGroup
	proposeEvery     time.Duration
	announce         AnnounceFn
	lastProposeAt    time.Time
	closed           bool
	seg              manager.Segment
	members          []int32
	quorum           int32
	promisedBallot   uint64
	acceptedBallot   uint64
	prepared         bool
	promiseCount     int32
	promisedFrom     map[int32]struct{}
	lastReqBatch     *request.Batch
	phase            instPhase
	lastVal          *pb.MPxValue
	lastDigest       [32]byte
	acceptCount      int32
	acceptedFrom     map[int32]struct{}
	acceptRtxEvery   time.Duration
	lastAcceptAt     time.Time
	roundStartAt     time.Time
	prepSent         bool
	leader           int32
	currentBallot    int64
	inProposal       bool
	acceptTs         int64
	lastAcceptedTs   int64
	pendingCommitDigest *[32]byte
	fetchInFlight    bool
	msgCh            chan *pb.ProtocolMessage
	stopCh           chan struct{}
	wg               sync.WaitGroup
}

func newMPXInstance(parent *MultiPaxosOrderer, sn int32, announce AnnounceFn, _ int, interval time.Duration) *mpxInstance {
	now := time.Now()
	return &mpxInstance{
		parent: parent, sn: sn, proposeEvery: interval, announce: announce,
		lastProposeAt: now, phase: phaseInit, acceptRtxEvery: interval * 2,
		lastAcceptAt: now, roundStartAt: now,
		msgCh: make(chan *pb.ProtocolMessage, 8192), stopCh: make(chan struct{}),
		leader: -1,
	}
}

func (i *mpxInstance) setSegment(seg manager.Segment) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.seg = seg
	n := int32(len(i.members))
	if n == 0 { n = int32(len(membership.AllNodeIDs())) }
	if n < 1 { n = 1 }
	i.quorum = n/2 + 1
}

func (i *mpxInstance) initGroupBuckets() {
	i.mu.Lock()
	defer i.mu.Unlock()
	numBuckets := len(request.Buckets)
	numGroups := len(i.parent.am.GetDefinedGroups())
	if numGroups == 0 { numGroups = 1 }
	i.groupBucketIDs = make([]int, 0)
	for b := int(i.bucketId); b < numBuckets; b += numGroups {
		i.groupBucketIDs = append(i.groupBucketIDs, b)
	}
	i.groupBucketGroup = request.NewBucketGroup(i.groupBucketIDs)
}

func (i *mpxInstance) startWorkers(_ *sync.WaitGroup) {
	i.wg.Add(1)
	go func() {
		defer i.wg.Done()
		for {
			select {
			case pm := <-i.msgCh:
				if pm == nil { return }
				if mpx := pm.GetMultipaxos(); mpx != nil { i.handleMPxMsg(pm, mpx); continue }
				if me := pm.GetMissingEntry(); me != nil { i.onMissingEntry(me); continue }
			case <-i.stopCh:
				return
			}
		}
	}()
}

func (i *mpxInstance) stopWorkers() {
	i.mu.Lock()
	defer i.mu.Unlock()
	select {
	case <-i.stopCh:
	default:
		close(i.stopCh)
		close(i.msgCh)
	}
	i.wg.Wait()
}

func (i *mpxInstance) enqueue(pm *pb.ProtocolMessage) {
	select {
	case <-i.stopCh: return
	case i.msgCh <- pm:
	default:
		go func() {
			select {
			case <-i.stopCh: return
			case i.msgCh <- pm:
			}
		}()
	}
}

func (i *mpxInstance) handleMPxMsg(pm *pb.ProtocolMessage, mpx *pb.MPxMsg) {
	switch t := mpx.Type.(type) {
	case *pb.MPxMsg_Prepare:
		fmt.Printf("[MPX] sn=%d PREPARE from=%d\n", i.sn, pm.GetSenderId())
		i.onPrepare(t.Prepare)
	case *pb.MPxMsg_Promise:
		fmt.Printf("[MPX] sn=%d PROMISE from=%d\n", i.sn, pm.GetSenderId())
		i.onPromise(pm.GetSenderId(), t.Promise)
	case *pb.MPxMsg_Accept:
		fmt.Printf("[MPX] sn=%d ACCEPT from=%d\n", i.sn, pm.GetSenderId())
		i.onAccept(pm.GetSenderId(), t.Accept)
	case *pb.MPxMsg_Accepted:
		fmt.Printf("[MPX] sn=%d ACCEPTED from=%d\n", i.sn, pm.GetSenderId())
		i.onAccepted(pm, t.Accepted)
	case *pb.MPxMsg_Commit:
		fmt.Printf("[MPX] sn=%d COMMIT from=%d\n", i.sn, pm.GetSenderId())
		i.onCommit(t.Commit)
	}
}

func (i *mpxInstance) onPrepare(prepare *pb.MPxPrepare) {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.phase == phaseCommitted { return }
	ballot := uint64(prepare.GetBallot())
	i.bucketId = prepare.GetGroupId()
	if i.groupBucketGroup == nil {
		i.mu.Unlock(); i.initGroupBuckets(); i.mu.Lock()
	}
	if int64(ballot) > i.currentBallot && i.leader == membership.OwnID {
		seenCounter := ballot >> 32
		i.currentBallot = int64((seenCounter + 1) << 32 | uint64(membership.OwnID))
		i.leader = -1; i.phase = phaseInit; i.prepared = false
		i.prepSent = false; i.promiseCount = 0; i.promisedFrom = nil
	}
	if ballot < i.promisedBallot { return }
	i.promisedBallot = ballot

	members := i.members
	if len(members) == 0 { members = membership.AllNodeIDs() }
	membersUint32 := make([]uint32, len(members))
	for idx, m := range members { membersUint32[idx] = uint32(m) }

	out := &pb.ProtocolMessage{
		SenderId: membership.OwnID, Sn: i.sn,
		Msg: &pb.ProtocolMessage_Multipaxos{Multipaxos: &pb.MPxMsg{Type: &pb.MPxMsg_Promise{
			Promise: &pb.MPxPromise{
				Id: &pb.MPxInstanceId{Sn: i.sn, Lead: uint64(membership.OwnID)},
				Ballot: ballot, Ok: true, GroupId: prepare.GetGroupId(), Members: membersUint32,
			},
		}}},
	}
	if i.parent != nil && i.parent.emit != nil { i.parent.emit(out) }

	leaderID := int32(prepare.GetId().GetLead())
	if leaderID == membership.OwnID {
		i.leader = membership.OwnID
		if i.promisedFrom == nil { i.promisedFrom = make(map[int32]struct{}) }
		if _, ok := i.promisedFrom[membership.OwnID]; !ok {
			i.promisedFrom[membership.OwnID] = struct{}{}
			i.promiseCount++
			if i.promiseCount >= i.quorum && !i.prepared {
				i.prepared = true; i.phase = phasePrepared; go i.ProposeIfDue()
			}
		}
	}
}

func (i *mpxInstance) onPromise(from int32, _ *pb.MPxPromise) {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.promisedFrom == nil { i.promisedFrom = make(map[int32]struct{}) }
	if _, ok := i.promisedFrom[from]; ok { return }
	if len(i.members) > 0 && !i.isInGroup(from) { return }
	i.promisedFrom[from] = struct{}{}
	i.promiseCount++
	if i.promiseCount >= i.quorum && !i.prepared {
		fmt.Printf("[MPX] sn=%d QUORUM promises=%d/%d\n", i.sn, i.promiseCount, i.quorum)
		i.prepared = true; i.phase = phasePrepared; go i.ProposeIfDue()
	}
}

func (i *mpxInstance) onAccept(from int32, a *pb.MPxAccept) {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.phase == phaseCommitted { return }
	ballot := uint64(a.GetBallot())
	if ballot < i.promisedBallot { return }
	if i.leader != -1 && i.leader != from && ballot == i.promisedBallot { return }
	i.promisedBallot = ballot
	i.leader = from
	if ballot >= i.acceptedBallot { i.acceptedBallot = ballot }

	batch := a.GetBatch()
	if batch == nil { return }
	i.lastVal = &pb.MPxValue{Id: &pb.MPxInstanceId{Sn: i.sn, Lead: uint64(from)}, Batch: batch}
	copy(i.lastDigest[:], request.BatchDigest(batch))
	i.acceptTs = time.Now().UnixNano()
	tracing.MainTrace.Event(tracing.PROPOSE, int64(i.sn), int64(len(batch.Requests)))

	accepted := &pb.MPxMsg{Type: &pb.MPxMsg_Accepted{Accepted: &pb.MPxAccepted{
		Id: &pb.MPxInstanceId{Sn: i.sn, Lead: uint64(membership.OwnID)},
		Ballot: ballot, Ok: true, GroupId: i.bucketId,
	}}}
	resp := &pb.ProtocolMessage{SenderId: membership.OwnID, Sn: i.sn,
		Msg: &pb.ProtocolMessage_Multipaxos{Multipaxos: accepted}}
	if i.leader == membership.OwnID {
		i.mu.Unlock()
		i.onAccepted(resp, accepted.Type.(*pb.MPxMsg_Accepted).Accepted)
		i.mu.Lock()
		return
	}
	if i.parent.emit != nil { i.parent.emit(resp) }
}

func (i *mpxInstance) onAccepted(pm *pb.ProtocolMessage, _ *pb.MPxAccepted) {
	i.mu.Lock()
	defer i.mu.Unlock()
	if pm == nil { return }
	if i.acceptedFrom == nil { i.acceptedFrom = make(map[int32]struct{}) }
	if _, ok := i.acceptedFrom[pm.SenderId]; ok { return }
	if len(i.members) > 0 && !i.isInGroup(pm.SenderId) { return }
	i.acceptedFrom[pm.SenderId] = struct{}{}
	i.acceptCount++
	i.lastAcceptedTs = time.Now().UnixNano()

	if i.acceptCount >= i.quorum && i.lastVal != nil && i.phase != phaseCommitted {
		fmt.Printf("[MPX] sn=%d QUORUM accepted=%d/%d -> COMMIT\n", i.sn, i.acceptCount, i.quorum)
		if i.parent.emit != nil {
			i.parent.emit(&pb.ProtocolMessage{SenderId: membership.OwnID, Sn: i.sn,
				Msg: &pb.ProtocolMessage_Multipaxos{Multipaxos: &pb.MPxMsg{Type: &pb.MPxMsg_Commit{
					Commit: &pb.MPxCommit{
						Id: &pb.MPxInstanceId{Sn: i.sn, Lead: uint64(membership.OwnID)},
						Digest: i.lastDigest[:], GroupId: i.bucketId,
					},
				}}}})
		}
		i.mu.Unlock()
		i.onCommit(&pb.MPxCommit{Id: &pb.MPxInstanceId{Sn: i.sn}, Digest: i.lastDigest[:], GroupId: i.bucketId})
		i.mu.Lock()
	}
}

func (i *mpxInstance) onCommit(c *pb.MPxCommit) {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.phase == phaseCommitted { return }
	if len(c.GetDigest()) == 0 {
		i.phase = phaseCommitted; i.closed = true
		tracing.MainTrace.Event(tracing.COMMIT, int64(i.sn), 0)
		return
	}
	var cd [32]byte
	copy(cd[:], c.GetDigest())
	i.pendingCommitDigest = &cd
	if i.lastVal == nil || i.lastVal.GetBatch() == nil {
		if !i.fetchInFlight { i.fetchInFlight = true; go i.fetchCommittedValueFromGroup() }
		return
	}
	if cd != i.lastDigest {
		if !i.fetchInFlight { i.fetchInFlight = true; go i.fetchCommittedValueFromGroup() }
		return
	}
	i.pendingCommitDigest = nil; i.fetchInFlight = false
	i.deliverCommit()
}

func (i *mpxInstance) onMissingEntry(me *pb.MissingEntry) {
	i.mu.Lock()
	defer i.mu.Unlock()
	if me == nil || me.Batch == nil { return }
	i.lastVal = &pb.MPxValue{Batch: me.Batch}
	copy(i.lastDigest[:], request.BatchDigest(me.Batch))
	if i.pendingCommitDigest == nil { return }
	if *i.pendingCommitDigest != i.lastDigest { i.fetchInFlight = false; return }
	i.fetchInFlight = false; i.pendingCommitDigest = nil
	i.deliverCommit()
}

func (i *mpxInstance) fetchCommittedValueFromGroup() {
	if i.parent == nil || i.parent.am == nil { return }
	members := i.parent.am.GetGroupMembers(uint32(i.bucketId))
	sources := make([]int32, 0, len(members))
	for _, m := range members {
		if m != membership.OwnID { sources = append(sources, m) }
	}
	if len(sources) > 0 { statetransfer.FetchMissingEntry(i.sn, sources) }
}

func (i *mpxInstance) deliverCommit() {
	i.phase = phaseCommitted
	if i.lastReqBatch != nil { request.RemoveBatch(i.lastReqBatch); i.lastReqBatch = nil }

	b := i.lastVal.GetBatch()

	// NOP (batch vazio)
	if len(b.Requests) == 0 {
		if i.announce != nil { i.announce(i.sn, b, i.lastDigest[:]) }
		i.closed = true; traceCommit(i.sn, 0)
		return
	}

	// Grupo 0: mensagens de sistema são processadas pelo Sequencer standalone
	// (não chegam aqui pois grupo 0 não tem instâncias ISS)
	if i.bucketId == 0 && GetGlobalMulticastOrderer() != nil {
		if i.announce != nil { i.announce(i.sn, b, i.lastDigest[:]) }
		i.closed = true; traceCommit(i.sn, len(b.Requests))
		return
	}

	// Cross-group: verifica ordem GSN
	var crossOpGSN uint64
	for _, req := range b.Requests {
		if len(req.TouchedGroups) > 1 && req.GSN > 0 { crossOpGSN = req.GSN; break }
	}
	if crossOpGSN > 0 && GetGlobalMulticastOrderer() != nil {
		fmt.Printf("[MPX] sn=%d CROSS-OP gsn=%d group=%d\n", i.sn, crossOpGSN, i.bucketId)
		if !GetGlobalMulticastOrderer().ADeliver(crossOpGSN, i.bucketId, b) {
			// Cannot deliver yet — buffer until META arrives and ordering is satisfied
			GetGlobalMulticastOrderer().BufferCommit(crossOpGSN, i.bucketId, b, i.announce, i.sn, i.lastDigest[:])
			i.closed = true; traceCommit(i.sn, len(b.Requests))
			return
		}
	}

	// Announce commit
	if i.announce != nil {
		fmt.Printf("[MPX] sn=%d DELIVER group=%d nReq=%d\n", i.sn, i.bucketId, len(b.Requests))
		i.announce(i.sn, b, i.lastDigest[:])
	}
	i.closed = true; traceCommit(i.sn, len(b.Requests))
}



func (i *mpxInstance) ProposeIfDue() {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.leader != membership.OwnID || i.inProposal || i.closed { return }
	if i.phase >= phaseAcceptSent && i.phase != phaseCommitted { return }
	i.inProposal = true
	defer func() { i.inProposal = false }()
	i.lastProposeAt = time.Now()

	if !i.prepared {
		if i.promiseCount < i.quorum { return }
		i.prepared = true; i.phase = phasePrepared
	}

	var val *pb.MPxValue
	reqs := 0
	if i.lastVal == nil {
		if i.groupBucketGroup == nil {
			if i.bucketId == 0 {
				fmt.Printf("[MPX] sn=%d PROPOSE-SKIP group=0 reason=groupBucketGroup nil\n", i.sn)
			}
			return
		}
		rb := i.groupBucketGroup.CutBatch(i.parent.maxBatchSize, i.proposeEvery)
		if rb == nil || rb.Message() == nil || len(rb.Message().Requests) == 0 {
			emptyBatch := &pb.Batch{Requests: nil}
			val = &pb.MPxValue{Id: &pb.MPxInstanceId{Sn: i.sn, Lead: uint64(membership.OwnID)}, Batch: emptyBatch}
			i.lastVal = val
			copy(i.lastDigest[:], request.BatchDigest(emptyBatch))
			i.lastReqBatch = nil
		} else {
			if !i.validateBatchHomogeneity(rb) { return }
			batchMsg := rb.Message()
			// Cross-op validation
			if len(batchMsg.Requests) > 0 && len(batchMsg.Requests[0].TouchedGroups) > 1 && len(batchMsg.Requests) != 1 {
				rb.Resurrect(); return
			}
			for _, req := range rb.Requests {
				if len(req.Msg.TouchedGroups) > 1 && req.Msg.GSN == 0 { rb.Resurrect(); return }
			}
			// Mark requests as in-flight to prevent duplicate proposals
			rb.MarkInFlight()
			i.lastReqBatch = rb
			reqs = len(batchMsg.Requests)
			val = &pb.MPxValue{Id: &pb.MPxInstanceId{Sn: i.sn, Lead: uint64(membership.OwnID)}, Batch: batchMsg}
			i.lastVal = val
			copy(i.lastDigest[:], request.BatchDigest(batchMsg))
		}
		i.acceptedFrom = map[int32]struct{}{}; i.acceptCount = 0
	} else {
		val = i.lastVal
	}

	if i.parent.emit != nil {
		fmt.Printf("[MPX] sn=%d PROPOSE group=%d nReq=%d\n", i.sn, i.bucketId, reqs)
		i.parent.emit(&pb.ProtocolMessage{SenderId: membership.OwnID, Sn: i.sn,
			Msg: &pb.ProtocolMessage_Multipaxos{Multipaxos: &pb.MPxMsg{Type: &pb.MPxMsg_Accept{
				Accept: &pb.MPxAccept{
					Id: &pb.MPxInstanceId{Sn: i.sn, Lead: uint64(membership.OwnID)},
					Ballot: uint64(i.currentBallot), Batch: val.GetBatch(), GroupId: i.bucketId,
				},
			}}}})
	}
	i.phase = phaseAcceptSent; i.lastAcceptAt = time.Now()
	tracing.MainTrace.Event(tracing.PROPOSE, int64(i.sn), int64(reqs))
}

func (i *mpxInstance) tick(now time.Time) {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.phase != phaseAcceptSent || i.acceptCount >= i.quorum { return }

	// Retransmissão de ACCEPT
	if now.Sub(i.lastAcceptAt) >= i.acceptRtxEvery && i.parent.emit != nil && i.lastVal != nil {
		i.parent.emit(&pb.ProtocolMessage{SenderId: membership.OwnID, Sn: i.sn,
			Msg: &pb.ProtocolMessage_Multipaxos{Multipaxos: &pb.MPxMsg{Type: &pb.MPxMsg_Accept{
				Accept: &pb.MPxAccept{
					Id: &pb.MPxInstanceId{Sn: i.sn, Lead: uint64(membership.OwnID)},
					Ballot: uint64(i.currentBallot), Batch: i.lastVal.GetBatch(), GroupId: i.bucketId,
				},
			}}}})
		i.lastAcceptAt = now
	}

	// Nova rodada com ballot maior
	if now.Sub(i.roundStartAt) >= i.acceptRtxEvery*3 {
		seenCounter := uint64(i.currentBallot) >> 32
		i.currentBallot = int64((seenCounter + 1) << 32 | uint64(membership.OwnID))
		i.phase = phaseInit; i.prepared = false
		i.promiseCount = 0; i.promisedFrom = nil
		i.acceptCount = 0; i.acceptedFrom = nil; i.roundStartAt = now
		if i.parent.emit != nil {
			i.parent.emit(&pb.ProtocolMessage{SenderId: membership.OwnID, Sn: i.sn,
				Msg: &pb.ProtocolMessage_Multipaxos{Multipaxos: &pb.MPxMsg{Type: &pb.MPxMsg_Prepare{
					Prepare: &pb.MPxPrepare{
						Id: &pb.MPxInstanceId{Sn: i.sn, Lead: uint64(membership.OwnID)},
						Ballot: uint64(i.currentBallot), GroupId: i.bucketId,
					},
				}}}})
		}
		i.lastAcceptAt = now
	}
}

func (i *mpxInstance) SetMembers(members []int32) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.members = make([]int32, len(members))
	copy(i.members, members)
	n := int32(len(i.members))
	if n < 1 { n = 1 }
	i.quorum = n/2 + 1

	// Initialize groupBucketGroup eagerly (don't wait for onPrepare)
	if i.groupBucketGroup == nil && i.bucketId < uint32(len(request.Buckets)) {
		fmt.Printf("[MPX] sn=%d INIT-BUCKETS-EAGER group=%d (SetMembers)\n", i.sn, i.bucketId)
		i.mu.Unlock()
		i.initGroupBuckets()
		i.mu.Lock()
	}

	if i.leader == -1 {
		i.leader = i.members[i.sn%n]
		fmt.Printf("[MPX] sn=%d SetMembers members=%v quorum=%d leader=%d\n", i.sn, members, i.quorum, i.leader)
		i.currentBallot = int64(uint64(0)<<32 | uint64(i.leader))
		if i.leader == membership.OwnID && !i.prepSent {
			i.prepSent = true
			if i.parent.emit != nil {
				i.parent.emit(&pb.ProtocolMessage{SenderId: membership.OwnID, Sn: i.sn,
					Msg: &pb.ProtocolMessage_Multipaxos{Multipaxos: &pb.MPxMsg{Type: &pb.MPxMsg_Prepare{
						Prepare: &pb.MPxPrepare{
							Id: &pb.MPxInstanceId{Sn: i.sn, Lead: uint64(membership.OwnID)},
							Ballot: uint64(i.currentBallot), GroupId: i.bucketId,
						},
					}}}})
			}
		}
	}

	if i.acceptedFrom != nil {
		newFrom := make(map[int32]struct{})
		cnt := int32(0)
		for id := range i.acceptedFrom {
			if i.isInGroup(id) { newFrom[id] = struct{}{}; cnt++ }
		}
		i.acceptedFrom = newFrom; i.acceptCount = cnt
	}
}

func (i *mpxInstance) isInGroup(nodeID int32) bool {
	for _, m := range i.members { if m == nodeID { return true } }
	return false
}

func traceCommit(sn int32, size int) {
	tracing.MainTrace.Event(tracing.COMMIT, int64(sn), int64(size))
}

func (i *mpxInstance) validateBatchHomogeneity(batch *request.Batch) bool {
	reqs := batch.Message().Requests
	if len(reqs) == 0 { return true }
	firstGid := reqs[0].GetGroupId()
	for _, req := range reqs {
		if req.GetGroupId() != firstGid { batch.Resurrect(); return false }
	}
	if firstGid != i.bucketId { batch.Resurrect(); return false }
	if i.bucketId == 0 && firstGid != 0 { batch.Resurrect(); return false }
	return true
}
