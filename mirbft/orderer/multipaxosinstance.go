package orderer

import (
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

// Fases do protocolo Paxos para uma instância
type instPhase int

const (
	phaseInit instPhase = iota       // Fase inicial, aguardando preparação
	phasePrepared                     // Fase 1 completa (quorum de promises)
	phaseAcceptSent                   // Fase 2 iniciada (accept enviado)
	phaseCommitted                    // Consenso alcançado, valor commitado
)

// mpxInstance representa uma instância do protocolo Multi-Paxos
// Cada instância é responsável por ordenar um único batch de requisições
type mpxInstance struct {
	mu sync.Mutex

	parent *MultiPaxosOrderer
	sn     int32   // Número de sequência desta instância
	bucketId uint32 // ID do grupo/bucket ao qual esta instância pertence
	bucketIndex int32

	proposeEvery  time.Duration
	announce      AnnounceFn
	lastProposeAt time.Time
	closed        bool

	seg manager.Segment

	// Estado do protocolo Paxos
	members      []int32  // Membros do grupo (para quorum)
	quorum       int32    // Número de votos necessários para maioria
	promisedBallot  uint64 // Maior ballot prometido (Fase 1)
	acceptedBallot  uint64 // Maior ballot aceito (Fase 2)
	acceptedValue   *pb.MPxValue // Valor aceito
	prepared        bool   // Se Fase 1 foi completada
	promiseCount    int32  // Contador de promises recebidos
	promisedFrom    map[int32]struct{} // Nós que enviaram promise

	lastReqBatch *request.Batch
	phase        instPhase
	lastVal      *pb.MPxValue // Último valor proposto
	lastDigest   [32]byte     // Hash do valor para validação
	acceptCount  int32        // Contador de accepts recebidos
	acceptedFrom map[int32]struct{} // Nós que enviaram accepted

	acceptRtxEvery   time.Duration // Intervalo para retransmissão
	lastAcceptAt     time.Time
	enableNilDeliver bool
	sbNilAfter       time.Duration

	prepSent bool
	leader   int32 // Líder atual desta instância
	currentBallot int64

	// Processamento assíncrono de mensagens
	msgCh  chan *pb.ProtocolMessage
	stopCh chan struct{}
	wg     sync.WaitGroup
}

func newMPXInstance(parent *MultiPaxosOrderer, sn int32, announce AnnounceFn, _ int, interval time.Duration) *mpxInstance {
	inst := &mpxInstance{
		parent:         parent,
		sn:             sn,
		bucketId:       0,
		bucketIndex:    -1,
		proposeEvery:   interval,
		announce:       announce,
		lastProposeAt:  time.Now(),
		phase:          phaseInit,
		acceptRtxEvery: interval * 2,
		sbNilAfter:     interval * 3,
		msgCh:          make(chan *pb.ProtocolMessage, 8192),
		stopCh:         make(chan struct{}),
		promisedBallot: 0,
		acceptedBallot: 0,
		currentBallot:  int64(uint64(membership.OwnID)),
	}
	fmt.Printf("[MPX][INST] sn=%d created\n", sn)
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
		fmt.Printf("[MPX][INST] sn=%d segment bound, quorum=%d (fallback all nodes)\n", i.sn, i.quorum)
	} else {
		n := int32(len(i.members))
		if n < 1 {
			n = 1
		}
		i.quorum = n/2 + 1
		fmt.Printf("[MPX][INST] sn=%d segment bound, quorum=%d (group members=%d)\n", i.sn, i.quorum, len(i.members))
	}
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
		fmt.Printf("[MPX][INST] sn=%d PREPARE from=%d\n", i.sn, pm.GetSenderId())
		i.onPrepare(t.Prepare)
	case *pb.MPxMsg_Promise:
		fmt.Printf("[MPX][INST] sn=%d PROMISE from=%d\n", i.sn, pm.GetSenderId())
		i.onPromise(pm.GetSenderId(), t.Promise)
	case *pb.MPxMsg_Accept:
		fmt.Printf("[MPX][INST] sn=%d ACCEPT from=%d\n", i.sn, pm.GetSenderId())
		i.onAccept(pm.GetSenderId(), t.Accept)
	case *pb.MPxMsg_Accepted:
		fmt.Printf("[MPX][INST] sn=%d ACCEPTED from=%d\n", i.sn, pm.GetSenderId())
		i.onAccepted(pm, t.Accepted)
	case *pb.MPxMsg_Commit:
		fmt.Printf("[MPX][INST] sn=%d COMMIT from=%d\n", i.sn, pm.GetSenderId())
		i.onCommit(t.Commit)
	default:
		fmt.Printf("[MPX][INST] sn=%d UNKNOWN msg type\n", i.sn)
	}
}

// onPrepare processa mensagem PREPARE (Fase 1 do Paxos)
// Líder envia PREPARE para iniciar consenso em um novo ballot
// Followers respondem com PROMISE se aceitarem o ballot
func (i *mpxInstance) onPrepare(prepare *pb.MPxPrepare) {
	i.mu.Lock()
	defer i.mu.Unlock()

	ballot := uint64(prepare.GetBallot())
	
	if int64(ballot) > i.currentBallot && i.leader == membership.OwnID {
		seenCounter := ballot >> 32
		i.currentBallot = int64((seenCounter + 1) << 32 | uint64(membership.OwnID))
		fmt.Printf("[MPX][INST] sn=%d ABORTING leadership, bumping ballot to %d\n", i.sn, i.currentBallot)
		i.leader = 0
		i.phase = phaseInit
		i.prepared = false
		i.prepSent = false
		i.promiseCount = 0
		i.promisedFrom = make(map[int32]struct{})
	}
	
	if ballot < i.promisedBallot {
		fmt.Printf("[MPX][INST] sn=%d rejecting PREPARE ballot=%d < promised=%d\n", i.sn, ballot, i.promisedBallot)
		return
	}
	
	i.promisedBallot = ballot
	
	var promiseValue *pb.MPxValue
	if i.acceptedValue != nil {
		promiseValue = i.acceptedValue
	}

	groupId := prepare.GetGroupId()
	members := make([]uint32, 0)
	
	if len(i.members) > 0 {
		for _, id := range i.members {
			members = append(members, uint32(id))
		}
	} else {
		all := membership.AllNodeIDs()
		for _, id := range all {
			members = append(members, uint32(id))
		}
	}

	promise := &pb.MPxMsg{Type: &pb.MPxMsg_Promise{
		Promise: &pb.MPxPromise{
			Id:             &pb.MPxInstanceId{Sn: i.sn, Lead: uint64(membership.OwnID)},
			Ballot:         ballot,
			Ok:             true,
			Value:          promiseValue,
			GroupId:        groupId,
			Members:        members,
		},
	}}

	out := &pb.ProtocolMessage{
		SenderId: membership.OwnID,
		Sn:       i.sn,
		Msg:      &pb.ProtocolMessage_Multipaxos{Multipaxos: promise},
	}
	if i.parent.emit != nil {
		fmt.Printf("[MPX][INST] sn=%d sending PROMISE ballot=%d groupId=%d\n", i.sn, ballot, groupId)
		i.parent.emit(out)
	}
}

// onPromise processa mensagem PROMISE (resposta da Fase 1)
// Quando quorum de promises é atingido, líder pode iniciar Fase 2
// Se algum promise contém valor aceito anteriormente, líder deve adotá-lo
func (i *mpxInstance) onPromise(from int32, promise *pb.MPxPromise) {
	i.mu.Lock()
	defer i.mu.Unlock()
	
	if i.promisedFrom == nil {
		i.promisedFrom = make(map[int32]struct{})
	}
	
	if _, ok := i.promisedFrom[from]; ok {
		return // Já contado
	}
	
	// Só conta se estiver no grupo
	if len(i.members) > 0 && !i.isInGroup(from) {
		fmt.Printf("[MPX][INST] sn=%d promise from=%d skipped (not in group)\n", i.sn, from)
		return
	}
	
	i.promisedFrom[from] = struct{}{}
	i.promiseCount++
	
	if promise.GetValue() != nil {
		promiseValue := promise.GetValue()
		i.acceptedValue = promiseValue
		i.lastVal = promiseValue
		fmt.Printf("[MPX][INST] sn=%d adopted value from promise\n", i.sn)
	}
	
	fmt.Printf("[MPX][INST] sn=%d promise from=%d counted, promiseCount=%d/%d\n", i.sn, from, i.promiseCount, i.quorum)
	
	if i.promiseCount >= i.quorum && !i.prepared {
		i.prepared = true
		i.phase = phasePrepared
		fmt.Printf("[MPX][INST] sn=%d QUORUM de promises atingido, entrando em steady-state\n", i.sn)
	}
}

// onAccept processa mensagem ACCEPT (Fase 2 do Paxos)
// Líder envia ACCEPT com valor proposto
// Followers aceitam e respondem com ACCEPTED
func (i *mpxInstance) onAccept(from int32, a *pb.MPxAccept) {
	i.mu.Lock()
	defer i.mu.Unlock()

	ballot := uint64(a.GetBallot())
	
	if ballot < i.promisedBallot {
		fmt.Printf("[MPX][INST] sn=%d rejecting ACCEPT ballot=%d < promised=%d\n", i.sn, ballot, i.promisedBallot)
		return
	}
	
	if ballot >= i.acceptedBallot {
		i.acceptedBallot = ballot
		if a.GetValue() != nil {
			i.acceptedValue = a.GetValue()
		}
	}

	if ballot >= i.promisedBallot || i.leader == 0 {
		i.leader = from
		i.promisedBallot = ballot
		fmt.Printf("[MPX][INST] sn=%d leader set to %d (ballot=%d)\n", i.sn, from, ballot)
	} else if i.leader != from {
		fmt.Printf("[MPX][INST] sn=%d ignoring ACCEPT from=%d (leader=%d)\n", i.sn, from, i.leader)
		return
	}

	if a.GetValue() != nil {
		if i.lastVal != nil {
			incomingDigest := sha256.Sum256(a.GetValue().GetBatch())
			if incomingDigest != i.lastDigest {
			fmt.Printf("[MPX][INST] sn=%d digest mismatch\n", i.sn)
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
	
	if len(i.members) == 0 || i.isInGroup(membership.OwnID) {
		i.acceptedFrom[membership.OwnID] = struct{}{}
		i.acceptCount++
		fmt.Printf("[MPX][INST] sn=%d self-vote counted, acceptCount=%d/%d\n", i.sn, i.acceptCount, i.quorum)
	} else {
		fmt.Printf("[MPX][INST] sn=%d self-vote skipped (not in group)\n", i.sn)
	}

	// Envia ACCEPTED para o líder
	accepted := &pb.MPxMsg{Type: &pb.MPxMsg_Accepted{Accepted: &pb.MPxAccepted{
		Id:     &pb.MPxInstanceId{Sn: i.sn, Lead: uint64(membership.OwnID)},
		Ballot: ballot,
		Ok:     true,
	}}}
	resp := &pb.ProtocolMessage{
		SenderId: membership.OwnID,
		Sn:       i.sn,
		Msg:      &pb.ProtocolMessage_Multipaxos{Multipaxos: accepted},
	}

	if i.leader == membership.OwnID {
		fmt.Printf("[MPX][INST] sn=%d skip ACCEPTED to self\n", i.sn)
		return
	}

	fmt.Printf("[MPX][INST] sn=%d sending ACCEPTED to leader=%d\n", i.sn, i.leader)
	messenger.EnqueueMsg(resp, i.leader)
}

// onAccepted processa mensagem ACCEPTED (resposta da Fase 2)
// Quando quorum de accepts é atingido, líder envia COMMIT
// Todos os nós entregam o valor commitado
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
		
		if len(i.members) == 0 || i.isInGroup(pm.SenderId) {
			i.acceptedFrom[pm.SenderId] = struct{}{}
			i.acceptCount++
			fmt.Printf("[MPX][INST] sn=%d vote from=%d counted, acceptCount=%d/%d\n", i.sn, pm.SenderId, i.acceptCount, i.quorum)
		} else {
			fmt.Printf("[MPX][INST] sn=%d vote from=%d skipped (not in group)\n", i.sn, pm.SenderId)
			return
		}
	} else {
		i.acceptCount++
	}

	if i.acceptCount >= i.quorum && i.lastVal != nil && i.phase != phaseCommitted {
		commit := &pb.MPxMsg{Type: &pb.MPxMsg_Commit{
			Commit: &pb.MPxCommit{
				Id:      &pb.MPxInstanceId{Sn: i.sn, Lead: uint64(membership.OwnID)},
				Value:   i.lastVal,
				GroupId: i.bucketId,
			},
		}}
		pmOut := &pb.ProtocolMessage{
			SenderId: membership.OwnID,
			Sn:       i.sn,
			Msg:      &pb.ProtocolMessage_Multipaxos{Multipaxos: commit},
		}
		if i.parent.emit != nil {
			fmt.Printf("[MPX][INST] sn=%d QUORUM reached (%d/%d), sending COMMIT\n", i.sn, i.acceptCount, i.quorum)
			i.parent.emit(pmOut)
		}
	}

	if i.acceptCount >= i.quorum && i.lastVal != nil && i.phase != phaseCommitted {
		val := i.lastVal
		i.mu.Unlock()
		i.onCommit(&pb.MPxCommit{Id: &pb.MPxInstanceId{Sn: i.sn}, Value: val, GroupId: i.bucketId})
		i.mu.Lock()
	}
}

// onCommit processa mensagem COMMIT (entrega final)
// Valor foi decidido por consenso, pode ser entregue à aplicação
func (i *mpxInstance) onCommit(c *pb.MPxCommit) {
	i.mu.Lock()
	defer i.mu.Unlock()

	if i.phase == phaseCommitted {
		return
	}

	val := c.GetValue()
	if val == nil || len(val.GetBatch()) == 0 {
		fmt.Printf("[MPX][INST] sn=%d NIL commit\n", i.sn)
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
		i.parent.emit(pmOut)
	}

	if i.lastReqBatch != nil {
		request.RemoveBatch(i.lastReqBatch)
		i.lastReqBatch = nil
	}

	var b pb.Batch
	if err := proto.Unmarshal(i.lastVal.GetBatch(), &b); err != nil {
		return
	}
	if i.announce != nil {
		fmt.Printf("[MPX][INST] sn=%d announcing commit, size=%d\n", i.sn, len(b.Requests))
		i.announce(i.sn, i.lastVal.GetBatch(), nil)
	} else {
		fmt.Printf("[MPX][INST] sn=%d announcer is nil!\n", i.sn)
	}

	i.closed = true
	traceCommit(i.sn, len(b.Requests))
}

// ==================== Propose / Tick ====================

// ProposeIfDue verifica se é hora de propor um novo batch
// Líder corta batch do bucket e inicia Fase 2 (ACCEPT)
func (i *mpxInstance) ProposeIfDue() {
	i.mu.Lock()
	defer i.mu.Unlock()

	minInterval := i.proposeEvery / 5
	if time.Since(i.lastProposeAt) < minInterval {
		return
	}
	i.lastProposeAt = time.Now()

	if i.phase >= phaseAcceptSent {
		return
	}

	var val *pb.MPxValue
	reqs := 0

	if i.lastVal == nil {
		if i.seg == nil || i.bucketIndex < 0 {
			return
		}
		
		if request.Buckets[i.bucketIndex].Len() == 0 {
			return
		}
		
		rb := i.seg.Buckets().CutBatchFromBucket(int(i.bucketIndex), int(i.seg.BatchSize()), 0)
		if rb == nil || rb.Message() == nil || len(rb.Message().Requests) == 0 {
			return
		}
		
		if !i.validateBatchHomogeneity(rb) {
			return
		}
		
		i.lastReqBatch = rb
		batchMsg := rb.Message()
		reqs = len(batchMsg.Requests)

		batchBytes, err := proto.Marshal(batchMsg)
		if err != nil {
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
		}
	} else {
		val = i.lastVal
	}

	if !i.prepared {
		if i.promiseCount < i.quorum {
			fmt.Printf("[MPX][INST] sn=%d aguardando quorum de promises (%d/%d)\n", i.sn, i.promiseCount, i.quorum)
			return
		}
		i.prepared = true
		i.phase = phasePrepared
	}

	accept := &pb.MPxMsg{Type: &pb.MPxMsg_Accept{Accept: &pb.MPxAccept{
		Id:      &pb.MPxInstanceId{Sn: i.sn, Lead: uint64(membership.OwnID)},
		Ballot:  uint64(i.currentBallot),
		Value:   val,
		GroupId: i.bucketId,
	}}}
	pm := &pb.ProtocolMessage{
		SenderId: membership.OwnID,
		Sn:       i.sn,
		Msg:      &pb.ProtocolMessage_Multipaxos{Multipaxos: accept},
	}
	if i.parent.emit != nil {
		fmt.Printf("[MPX][INST] sn=%d sending ACCEPT groupId=%d reqs=%d\n", i.sn, i.bucketId, reqs)
		i.parent.emit(pm)
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
							Ballot:  uint64(i.currentBallot),
							Value:   i.lastVal,
							GroupId: i.bucketId,
						},
					}},
				},
			}
			fmt.Printf("[MPX][INST] sn=%d resending ACCEPT (timeout)\n", i.sn)
			i.parent.emit(pm)
			i.lastAcceptAt = now
		}
	}

	if i.enableNilDeliver &&
		i.phase == phaseAcceptSent &&
		i.acceptCount < i.quorum &&
		now.Sub(i.lastAcceptAt) >= i.sbNilAfter {

		fmt.Printf("[MPX][INST] sn=%d NIL timeout, delivering empty\n", i.sn)
		i.phase = phaseCommitted
		nilCommit := &pb.MPxMsg{Type: &pb.MPxMsg_Commit{
			Commit: &pb.MPxCommit{
				Id:      &pb.MPxInstanceId{Sn: i.sn, Lead: uint64(membership.OwnID)},
				Value:   nil,
				GroupId: i.bucketId,
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

// SetMembers configura membros do grupo e recalcula quorum
// Quorum = maioria dos membros do grupo (n/2 + 1)
// Reajusta contadores de votos para considerar apenas membros do grupo
func (i *mpxInstance) SetMembers(members []int32) {
	i.mu.Lock()
	defer i.mu.Unlock()
	
	i.members = make([]int32, len(members))
	copy(i.members, members)
	
	n := int32(len(i.members))
	if n < 1 {
		n = 1
	}
	i.quorum = n/2 + 1
	fmt.Printf("[MPX][INST] sn=%d SetMembers: members=%v quorum=%d\n", i.sn, members, i.quorum)
	
	if i.acceptedFrom != nil {
		newAcceptedFrom := make(map[int32]struct{})
		newCount := int32(0)
		
		for nodeID := range i.acceptedFrom {
			if i.isInGroup(nodeID) {
				newAcceptedFrom[nodeID] = struct{}{}
				newCount++
			}
		}
		
		i.acceptedFrom = newAcceptedFrom
		i.acceptCount = newCount
		fmt.Printf("[MPX][INST] sn=%d recount after SetMembers: acceptCount=%d/%d\n", i.sn, newCount, i.quorum)
	}
}

// isInGroup verifica se um nó pertence ao grupo desta instância
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
}
func traceCommit(sn int32, size int) {
	tracing.MainTrace.Event(tracing.COMMIT, int64(sn), int64(size))
}

// validateBatchHomogeneity valida se todas requisições pertencem ao mesmo grupo
// Batches heterogêneos (múltiplos grupos) são rejeitados
func (i *mpxInstance) validateBatchHomogeneity(batch *request.Batch) bool {
	reqs := batch.Message().Requests
	if len(reqs) == 0 {
		return true
	}
	
	firstGroupId := reqs[0].GetGroupId()
	for _, req := range reqs {
		if req.GetGroupId() != firstGroupId {
			fmt.Printf("[MPX][INST] sn=%d heterogeneous batch detected, returning requests\n", i.sn)
			batch.Resurrect()
			return false
		}
	}
	
	if firstGroupId != i.bucketId {
		fmt.Printf("[MPX][INST] sn=%d groupId mismatch: batch=%d expected=%d, returning requests\n", i.sn, firstGroupId, i.bucketId)
		batch.Resurrect()
		return false
	}
	
	return true
}