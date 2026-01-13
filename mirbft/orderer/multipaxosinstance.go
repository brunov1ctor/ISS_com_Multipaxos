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
	"github.com/hyperledger-labs/mirbft/messenger"
	"github.com/hyperledger-labs/mirbft/request"
	pb "github.com/hyperledger-labs/mirbft/protobufs"
	"github.com/hyperledger-labs/mirbft/tracing"
)

type instPhase int

const (
	phaseInit instPhase = iota
	phasePrepared
	phaseProposing
	phaseAcceptSent
	phaseCommitted
)

type mpxInstance struct {
	mu sync.Mutex

	parent *MultiPaxosOrderer
	sn     int32
	bucketId uint32 // Bucket ID from user request

	maxBatchSize  int
	proposeEvery  time.Duration
	announce      AnnounceFn
	lastProposeAt time.Time
	closed        bool

	seg manager.Segment

	// Group-aware quorum calculation
	members      []int32 // Members of this instance's group
	quorum       int32
	lastVal      *pb.MPxValue
	lastDigest   [32]byte
	acceptCount  int32
	acceptedFrom map[int32]struct{}

	lastReqBatch *request.Batch
	phase        instPhase

	acceptRtxEvery   time.Duration
	lastAcceptAt     time.Time
	enableNilDeliver bool
	sbNilAfter       time.Duration

	prepSent bool // líder já enviou PREPARE?
	leader   int32 // líder observado para este SN (primeiro Accept recebido)

	// processamento assíncrono
	msgCh  chan *pb.ProtocolMessage
	stopCh chan struct{}
	wg     sync.WaitGroup
}

func newMPXInstance(parent *MultiPaxosOrderer, sn int32, announce AnnounceFn, maxBatch int, interval time.Duration) *mpxInstance {
	inst := &mpxInstance{
		parent:         parent,
		sn:             sn,
		bucketId:       0, // Default para broadcast (será atualizado se necessário)
		maxBatchSize:   maxBatch,
		proposeEvery:   interval,
		announce:       announce,
		lastProposeAt:  time.Now(),
		phase:          phaseInit,
		acceptRtxEvery: interval * 2,
		sbNilAfter:     interval * 3,
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
	// Quorum será calculado quando SetMembers() for chamado
	// ou usa fallback para cluster inteiro se não definido
	if len(i.members) == 0 {
		n := int32(len(membership.AllNodeIDs()))
		if n < 1 {
			n = 1
		}
		i.quorum = n/2 + 1
	} else {
		n := int32(len(i.members))
		if n < 1 {
			n = 1
		}
		i.quorum = n/2 + 1
	}

	fmt.Printf("[MPX][CHK] seg bind sn=%d segID=%d firstSN=%d len=%d quorum=%d leaders=%v members=%v\n",
		i.sn, seg.SegID(), seg.FirstSN(), seg.Len(), i.quorum, seg.Leaders(), i.members)
}

func (i *mpxInstance) startWorkers(_ *sync.WaitGroup) {
	i.wg.Add(1)
	go func() {
		defer i.wg.Done()
		for {
			select {
			case pm := <-i.msgCh:
				if pm == nil {
					return
				}
				i.handleMPxMsg(pm, pm.GetMultipaxos())
			case <-i.stopCh:
				return
			}
		}
	}()
}

func (i *mpxInstance) stopWorkers() {
	i.mu.Lock()
	select {
	case <-i.stopCh:
		// já fechado
	default:
		close(i.stopCh)
		close(i.msgCh)
	}
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

// ========================== Handlers ==========================

func (i *mpxInstance) handleMPxMsg(pm *pb.ProtocolMessage, mpx *pb.MPxMsg) {
	switch t := mpx.Type.(type) {
	case *pb.MPxMsg_Prepare:
		fmt.Printf("[MPX][IN ] PREPARE sn=%d from=%d gid=%d\n", i.sn, pm.GetSenderId(), t.Prepare.GetGroupId())
		i.onPrepare(t.Prepare)
	case *pb.MPxMsg_Promise:
		fmt.Printf("[MPX][IN ] PROMISE sn=%d from=%d members=%v\n", i.sn, pm.GetSenderId(), t.Promise.GetMembers())
		i.phase = phasePrepared
	case *pb.MPxMsg_Accept:
		// registra o líder observado para este SN
		fmt.Printf("[MPX][IN ] ACCEPT sn=%d from=%d gid=%d\n", i.sn, pm.GetSenderId(), t.Accept.GetGroupId())
		i.onAccept(pm.GetSenderId(), t.Accept)
	case *pb.MPxMsg_Accepted:
		fmt.Printf("[MPX][IN ] ACCEPTED sn=%d from=%d\n", i.sn, pm.GetSenderId())
		i.onAccepted(pm, t.Accepted)
	case *pb.MPxMsg_Commit:
		fmt.Printf("[MPX][IN ] COMMIT sn=%d from=%d gid=%d\n", i.sn, pm.GetSenderId(), t.Commit.GetGroupId())
		i.onCommit(t.Commit)
	default:
		fmt.Printf("[MPX][WARN] msg MPx desconhecida sn=%d (%T)\n", i.sn, mpx.Type)
	}
}

func (i *mpxInstance) onPrepare(prepare *pb.MPxPrepare) {
	i.mu.Lock()
	defer i.mu.Unlock()

	if i.phase >= phasePrepared {
		return
	}
	i.phase = phasePrepared

	// Usa GroupId do Prepare recebido
	groupId := prepare.GetGroupId()
	members := make([]uint32, 0)
	
	if len(i.members) > 0 {
		// Usa membros do grupo já configurados
		for _, id := range i.members {
			members = append(members, uint32(id))
		}
	} else {
		// Fallback para todos os nós
		all := membership.AllNodeIDs()
		for _, id := range all {
			members = append(members, uint32(id))
		}
	}

	promise := &pb.MPxMsg{Type: &pb.MPxMsg_Promise{
		Promise: &pb.MPxPromise{
			Id:      &pb.MPxInstanceId{Sn: i.sn, Lead: uint64(membership.OwnID)},
			Ballot:  0,
			Ok:      true,
			GroupId: groupId, // Usa GroupId do Prepare
			Members: members, // Membros do grupo ou fallback
		},
	}}

	out := &pb.ProtocolMessage{
		SenderId: membership.OwnID,
		Sn:       i.sn,
		Msg:      &pb.ProtocolMessage_Multipaxos{Multipaxos: promise},
	}
	if i.parent.emit != nil {
		fmt.Printf("[MPX][NET] SEND Promise sn=%d gid=%d members=%v\n", i.sn, groupId, members)
		i.parent.emit(out)
	}
}

func (i *mpxInstance) onAccept(from int32, a *pb.MPxAccept) {
	i.mu.Lock()
	defer i.mu.Unlock()

	// Garante estabilidade do líder: aceita primeiro, ignora demais
	if i.leader == 0 {
		i.leader = from
		fmt.Printf("[MPX][LEADER] sn=%d leader set to %d\n", i.sn, from)
	} else if i.leader != from {
		fmt.Printf("[MPX][WARN] sn=%d ignoring Accept from %d (leader=%d)\n", i.sn, from, i.leader)
		return
	}

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
	
	// Só conta voto próprio se estiver no grupo
	if _, ok := i.acceptedFrom[membership.OwnID]; !ok {
		if len(i.members) == 0 || i.isInGroup(membership.OwnID) {
			i.acceptedFrom[membership.OwnID] = struct{}{}
			i.acceptCount++
			fmt.Printf("[MPX][SELF] sn=%d self-vote counted (in group)\n", i.sn)
		} else {
			fmt.Printf("[MPX][SELF] sn=%d self-vote SKIPPED (not in group)\n", i.sn)
		}
	}

	fmt.Printf("[MPX][QUORUM] ACCEPT sn=%d acceptCount=%d quorum=%d digest=%x\n",
		i.sn, i.acceptCount, i.quorum, i.lastDigest[:8])

	// Envia ACCEPTED UNICAST para o líder
	accepted := &pb.MPxMsg{Type: &pb.MPxMsg_Accepted{Accepted: &pb.MPxAccepted{
		Id:     &pb.MPxInstanceId{Sn: i.sn, Lead: uint64(membership.OwnID)},
		Ballot: 0,
		Ok:     true,
	}}}
	resp := &pb.ProtocolMessage{
		SenderId: membership.OwnID,
		Sn:       i.sn,
		Msg:      &pb.ProtocolMessage_Multipaxos{Multipaxos: accepted},
	}

	// Unicast para o líder (primeiro Accept recebido)
	if i.leader == membership.OwnID {
		fmt.Printf("[MPX][NET] SKIP Accepted sn=%d (leader=self)\n", i.sn)
		return
	}

	fmt.Printf("[MPX][NET] SEND Accepted sn=%d → leader=%d (unicast)\n", i.sn, i.leader)
	messenger.EnqueueMsg(resp, i.leader)
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
		
		// Só conta voto se sender estiver no grupo
		if len(i.members) == 0 || i.isInGroup(pm.SenderId) {
			i.acceptedFrom[pm.SenderId] = struct{}{}
			i.acceptCount++
			fmt.Printf("[MPX][VOTE] sn=%d vote from %d counted (in group)\n", i.sn, pm.SenderId)
		} else {
			fmt.Printf("[MPX][VOTE] sn=%d vote from %d SKIPPED (not in group)\n", i.sn, pm.SenderId)
			return
		}
	} else {
		i.acceptCount++
	}

	fmt.Printf("[MPX][QUORUM] ACCEPTED sn=%d count=%d/%d digest=%x\n",
		i.sn, i.acceptCount, i.quorum, i.lastDigest[:8])

	// líder decide commit quando atingir maioria
	if i.acceptCount >= i.quorum && i.lastVal != nil && i.phase != phaseCommitted {
		fmt.Printf("[MPX][QUORUM] MAJORITY REACHED sn=%d → COMMIT digest=%x\n", i.sn, i.lastDigest[:8])
		commit := &pb.MPxMsg{Type: &pb.MPxMsg_Commit{
			Commit: &pb.MPxCommit{
				Id:      &pb.MPxInstanceId{Sn: i.sn, Lead: uint64(membership.OwnID)},
				Value:   i.lastVal,
				GroupId: i.bucketId, // Usa bucket do usuário
			},
		}}
		pmOut := &pb.ProtocolMessage{
			SenderId: membership.OwnID,
			Sn:       i.sn,
			Msg:      &pb.ProtocolMessage_Multipaxos{Multipaxos: commit},
		}
		if i.parent.emit != nil {
			fmt.Printf("[MPX][NET] SEND Commit sn=%d gid=%d digest=%x\n", i.sn, i.bucketId, i.lastDigest[:8])
			i.parent.emit(pmOut) // roteador do orderer decide grupo
		}
	}

	// entrega local (idempotente)
	if i.acceptCount >= i.quorum && i.lastVal != nil && i.phase != phaseCommitted {
		val := i.lastVal
		i.mu.Unlock()
		i.onCommit(&pb.MPxCommit{Id: &pb.MPxInstanceId{Sn: i.sn}, Value: val, GroupId: i.bucketId})
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
		fmt.Printf("[MPX][NIL] COMMIT ⊥ sn=%d gid=%d\n", i.sn, c.GetGroupId())
		i.phase = phaseCommitted
		nilCommit := &pb.MPxMsg{Type: &pb.MPxMsg_Commit{
			Commit: &pb.MPxCommit{
				Id:      &pb.MPxInstanceId{Sn: i.sn, Lead: uint64(membership.OwnID)},
				Value:   nil,
				GroupId: c.GetGroupId(),
			},
		}}
		pmOut := &pb.ProtocolMessage{
			SenderId: membership.OwnID,
			Sn:       i.sn,
			Msg:      &pb.ProtocolMessage_Multipaxos{Multipaxos: nilCommit},
		}
		if i.parent.emit != nil {
			i.parent.emit(pmOut)
		}
		i.closed = true
		tracing.MainTrace.Event(tracing.COMMIT, int64(i.sn), 0)
		return
	}

	if i.lastVal == nil {
		i.lastVal = val
		i.lastDigest = sha256.Sum256(val.GetBatch())
	}

	i.phase = phaseCommitted

	commit := &pb.MPxMsg{Type: &pb.MPxMsg_Commit{
		Commit: &pb.MPxCommit{
			Id:      &pb.MPxInstanceId{Sn: i.sn, Lead: uint64(membership.OwnID)},
			Value:   i.lastVal,
			GroupId: c.GetGroupId(),
		},
	}}
	pmOut := &pb.ProtocolMessage{
		SenderId: membership.OwnID,
		Sn:       i.sn,
		Msg:      &pb.ProtocolMessage_Multipaxos{Multipaxos: commit},
	}
	if i.parent.emit != nil {
		fmt.Printf("[MPX][NET] GOSSIP Commit sn=%d gid=%d digest=%x\n", i.sn, c.GetGroupId(), i.lastDigest[:8])
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

// ==================== Propose / Tick ====================

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

	// PATCH: Descobre bucket ANTES de enviar Prepare
	if i.lastVal == nil {
		rb := i.cutReqBatch()
		if rb == nil || rb.Message() == nil || len(rb.Message().Requests) == 0 {
			return
		}
		
		if !i.validateBatchHomogeneity(rb) {
			return
		}
		
		i.lastReqBatch = rb
		batchMsg := rb.Message()
		reqs = len(batchMsg.Requests)

		// Extrai bucket do primeiro request do batch
		if len(batchMsg.Requests) > 0 {
			i.bucketId = batchMsg.Requests[0].GetGroupId()
		}

		fmt.Printf("PROPOSE sn=%d size=%d bucket=%d\n", i.sn, reqs, i.bucketId)

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
		
		if len(i.members) == 0 || i.isInGroup(membership.OwnID) {
			i.acceptedFrom[membership.OwnID] = struct{}{}
			i.acceptCount = 1
			fmt.Printf("[MPX][SELF] sn=%d leader in group, self-vote counted\n", i.sn)
		} else {
			fmt.Printf("[MPX][SELF] sn=%d leader NOT in group, no self-vote\n", i.sn)
		}

		i.phase = phaseProposing
	} else {
		val = i.lastVal
	}

	// PREPARE 1x com GroupId já conhecido
	if !i.prepSent {
		i.prepSent = true
		prep := &pb.MPxMsg{Type: &pb.MPxMsg_Prepare{
			Prepare: &pb.MPxPrepare{
				Id:      &pb.MPxInstanceId{Sn: i.sn, Lead: uint64(membership.OwnID)},
				Ballot:  0,
				GroupId: i.bucketId, // PATCH: Agora com bucket correto
			},
		}}
		pm := &pb.ProtocolMessage{
			SenderId: membership.OwnID,
			Sn:       i.sn,
			Msg:      &pb.ProtocolMessage_Multipaxos{Multipaxos: prep},
		}
		if i.parent.emit != nil {
			fmt.Printf("[MPX][NET] SEND Prepare sn=%d gid=%d\n", i.sn, i.bucketId)
			i.parent.emit(pm)
		}
		i.phase = phasePrepared
	}

	accept := &pb.MPxMsg{Type: &pb.MPxMsg_Accept{Accept: &pb.MPxAccept{
		Id:      &pb.MPxInstanceId{Sn: i.sn, Lead: uint64(membership.OwnID)},
		Ballot:  0,
		Value:   val,
		GroupId: i.bucketId,
	}}}
	pm := &pb.ProtocolMessage{
		SenderId: membership.OwnID,
		Sn:       i.sn,
		Msg:      &pb.ProtocolMessage_Multipaxos{Multipaxos: accept},
	}
	if i.parent.emit != nil {
		fmt.Printf("[MPX][NET] SEND Accept sn=%d bytes=%d gid=%d digest=%x\n", i.sn, len(val.GetBatch()), i.bucketId, i.lastDigest[:8])
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
							Id:      &pb.MPxInstanceId{Sn: i.sn, Lead: uint64(membership.OwnID)},
							Ballot:  0,
							Value:   i.lastVal,
							GroupId: i.bucketId, // Usa bucket do usuário
						},
					}},
				},
			}
			fmt.Printf("[MPX][RTX] RESEND Accept sn=%d gid=%d digest=%x\n", i.sn, i.bucketId, i.lastDigest[:8])
			i.parent.emit(pm)
			i.lastAcceptAt = now
		}
	}

	if i.enableNilDeliver &&
		i.phase == phaseAcceptSent &&
		i.acceptCount < i.quorum &&
		now.Sub(i.lastAcceptAt) >= i.sbNilAfter {

		fmt.Printf("[MPX][NIL] TIMEOUT → DELIVER ⊥ sn=%d gid=%d\n", i.sn, i.bucketId)

		i.phase = phaseCommitted
		nilCommit := &pb.MPxMsg{Type: &pb.MPxMsg_Commit{
			Commit: &pb.MPxCommit{
				Id:      &pb.MPxInstanceId{Sn: i.sn, Lead: uint64(membership.OwnID)},
				Value:   nil,
				GroupId: i.bucketId, // Usa bucket do usuário
			},
		}}
		pmOut := &pb.ProtocolMessage{
			SenderId: membership.OwnID,
			Sn:       i.sn,
			Msg:      &pb.ProtocolMessage_Multipaxos{Multipaxos: nilCommit},
		}
		if i.parent.emit != nil {
			i.parent.emit(pmOut)
		}

		i.closed = true
		tracing.MainTrace.Event(tracing.COMMIT, int64(i.sn), 0)
	}
}

// ==================== Utilidades ====================

func (i *mpxInstance) cutReqBatch() *request.Batch {
	if i.seg == nil {
		return nil
	}
	
	// Escolhe bucket com requests (round-robin simples)
	bucketIDs := i.seg.Buckets().GetBucketIDs()
	for _, bucketID := range bucketIDs {
		if request.Buckets[bucketID].Len() > 0 {
			// Corta batch apenas deste bucket
			size := i.seg.BatchSize()
			timeout := i.proposeEvery
			batch := i.seg.Buckets().CutBatchFromBucket(bucketID, int(size), timeout)
			if len(batch.Requests) > 0 {
				return batch
			}
		}
	}
	return nil
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

// SetMembers configures the group members for this instance and recalculates quorum
func (i *mpxInstance) SetMembers(members []int32) {
	i.mu.Lock()
	defer i.mu.Unlock()
	
	i.members = make([]int32, len(members))
	copy(i.members, members)
	
	// Recalcula quorum baseado no grupo
	n := int32(len(i.members))
	if n < 1 {
		n = 1
	}
	i.quorum = n/2 + 1
	
	// CRÍTICO: Reajusta acceptCount para só contar membros do grupo
	if i.acceptedFrom != nil {
		newAcceptedFrom := make(map[int32]struct{})
		newCount := int32(0)
		
		// Só mantém votos de membros do grupo
		for nodeID := range i.acceptedFrom {
			if i.isInGroup(nodeID) {
				newAcceptedFrom[nodeID] = struct{}{}
				newCount++
			}
		}
		
		i.acceptedFrom = newAcceptedFrom
		i.acceptCount = newCount
		fmt.Printf("[MPX][RECOUNT] sn=%d adjusted acceptCount=%d/%d after SetMembers\n", i.sn, i.acceptCount, i.quorum)
	}
	
	fmt.Printf("[MPX][CHK] instance sn=%d members=%v quorum=%d\n", i.sn, i.members, i.quorum)
}

// isInGroup verifica se um nó está nos membros do grupo desta instância
func (i *mpxInstance) isInGroup(nodeID int32) bool {
	for _, member := range i.members {
		if member == nodeID {
			return true
		}
	}
	return false
}

func tracePropose(sn int32, size int) {
	tracing.MainTrace.Event(tracing.PROPOSE, int64(sn), int64(size))
	fmt.Printf("[MPX] PROPOSE sn=%d size=%d\n", sn, size)
}
func traceCommit(sn int32, size int) {
	tracing.MainTrace.Event(tracing.COMMIT, int64(sn), int64(size))
	fmt.Printf("[MPX] COMMIT  sn=%d size=%d\n", sn, size)
}

// validateBatchHomogeneity checks if all requests in batch have same GroupId
func (i *mpxInstance) validateBatchHomogeneity(batch *request.Batch) bool {
	reqs := batch.Message().Requests
	if len(reqs) == 0 {
		return true
	}
	
	firstGroupId := reqs[0].GetGroupId()
	for _, req := range reqs {
		if req.GetGroupId() != firstGroupId {
			fmt.Printf("[MPX][WARN] Heterogeneous batch sn=%d: first=%d current=%d → returning requests\n", 
				i.sn, firstGroupId, req.GetGroupId())
			// Retorna requests para seus buckets corretos
			batch.Resurrect()
			return false
		}
	}
	return true
}

