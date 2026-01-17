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

// mpxInstance representa uma instância do protocolo Multi-Paxos para um único SN
//
// PROTOCOLO PAXOS (Crash-Tolerant Consensus):
// ============================================
// FASE 1 (Preparação):
//   Líder envia PREPARE(ballot) → Followers respondem PROMISE
//   Quorum de promises → Líder pode propor valor
//
// FASE 2 (Aceitação):
//   Líder envia ACCEPT(valor) → Followers respondem ACCEPTED
//   Quorum de accepts → Valor decidido, envia COMMIT
//
// ENTREGA:
//   Todos recebem COMMIT → Entregam valor à aplicação
//
// CSMR (Composable State Machine Replication):
// =============================================
// - Cada instância pertence a um GRUPO (bucketId)
// - Apenas MEMBROS do grupo participam do consenso
// - Quorum calculado sobre MEMBROS (não sobre todos os nós)
// - Não-membros recebem apenas COMMIT (meta-log)
//
// BATCHES VAZIOS (NIL):
// =====================
// - Permite avançar SN mesmo sem requests pendentes
// - Evita travamento de segmentos esperando requests
// - Crítico para progressão do log (MirBFT requirement)
type mpxInstance struct {
	mu sync.Mutex

	parent *MultiPaxosOrderer
	sn     int32   // Número de sequência desta instância
	// CSMR: Rastreamento de grupo e paralelismo
	bucketId uint32 // ID do grupo ao qual esta instância pertence (0 = global)
	bucketIndex int32 // Índice do bucket/grupo para acessar request.Buckets[]

	proposeEvery  time.Duration
	announce      AnnounceFn
	lastProposeAt time.Time
	closed        bool
	countedInflight bool // CSMR: Evita dupla contagem em inflightLocal (líder conta ao criar, follower ao receber PREPARE)

	seg manager.Segment

	// Estado do protocolo Paxos (crash-tolerant, não Byzantine)
	members      []int32  // CSMR: Membros do grupo (subconjunto de AllNodeIDs)
	quorum       int32    // Maioria dos membros: len(members)/2 + 1
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

// onPrepare processa PREPARE (Fase 1 do Paxos)
// Líder envia PREPARE para iniciar consenso
// Followers respondem com PROMISE se aceitarem o ballot
// CSMR: Followers incrementam inflightLocal ao receber PREPARE (rastreamento de drenagem)
// Ignora PREPARE se slot já committed
func (i *mpxInstance) onPrepare(prepare *pb.MPxPrepare) {
	i.mu.Lock()
	defer i.mu.Unlock()

	// Slot já committed → ignora PREPARE
	if i.phase == phaseCommitted {
		fmt.Printf("[MPX][INST] sn=%d ignoring PREPARE (already committed)\n", i.sn)
		return
	}

	ballot := uint64(prepare.GetBallot())
	
	// GroupId sempre vem no Prepare
	i.bucketId = prepare.GetGroupId()
	
	// CSMR: NÃO incrementa inflightLocal aqui (movido para ProposeIfDue no líder)
	// Followers não precisam contar inflight (só líder propõe)
	
	if int64(ballot) > i.currentBallot && i.leader == membership.OwnID {
		seenCounter := ballot >> 32
		i.currentBallot = int64((seenCounter + 1) << 32 | uint64(membership.OwnID))
		fmt.Printf("[MPX][INST] sn=%d ABORTING leadership, bumping ballot to %d\n", i.sn, i.currentBallot)
		i.leader = 0
		i.phase = phaseInit
		i.prepared = false
		i.prepSent = false
		i.promiseCount = 0
		i.promisedFrom = nil
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
	members := make([]int32, 0)
	
	if len(i.members) > 0 {
		members = append([]int32{}, i.members...)
	} else {
		members = append([]int32{}, membership.AllNodeIDs()...)
	}
	
	// Converte int32 para uint32 para o protobuf
	membersUint32 := make([]uint32, len(members))
	for idx, m := range members {
		membersUint32[idx] = uint32(m)
	}

	promise := &pb.MPxMsg{Type: &pb.MPxMsg_Promise{
		Promise: &pb.MPxPromise{
			Id:             &pb.MPxInstanceId{Sn: i.sn, Lead: uint64(membership.OwnID)},
			Ballot:         ballot,
			Ok:             true,
			Value:          promiseValue,
			GroupId:        groupId,
			Members:        membersUint32,
		},
	}}

	out := &pb.ProtocolMessage{
		SenderId: membership.OwnID,
		Sn:       i.sn,
		Msg:      &pb.ProtocolMessage_Multipaxos{Multipaxos: promise},
	}
	
	// PROMISE unicast ao líder (não multicast)
	leaderID := int32(prepare.GetId().GetLead())
	if leaderID == membership.OwnID {
		fmt.Printf("[MPX][INST] sn=%d skip PROMISE to self\n", i.sn)
		return
	}
	
	fmt.Printf("[MPX][INST] sn=%d sending PROMISE ballot=%d groupId=%d to leader=%d\n", i.sn, ballot, groupId, leaderID)
	messenger.EnqueueMsg(out, leaderID)
}

// onPromise processa PROMISE (resposta da Fase 1)
// Quando quorum de promises é atingido, líder pode iniciar Fase 2
// Se algum promise contém valor aceito anteriormente, líder DEVE adotá-lo (safety)
// CSMR: Só conta promises de membros do grupo
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

// onAccept processa ACCEPT (Fase 2 do Paxos)
// Líder envia ACCEPT com valor proposto
// Followers aceitam e respondem com ACCEPTED
// CSMR: Só aceita de membros do grupo
// Ignora ACCEPT se slot já committed
func (i *mpxInstance) onAccept(from int32, a *pb.MPxAccept) {
	i.mu.Lock()
	defer i.mu.Unlock()

	// CORREÇÃO 1: Slot já committed → ignora ACCEPT
	if i.phase == phaseCommitted {
		fmt.Printf("[MPX][INST] sn=%d ignoring ACCEPT (already committed)\n", i.sn)
		return
	}

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

	// Digest determinístico para batches vazios
	if a.GetValue() != nil {
		incomingBatch := a.GetValue().GetBatch()
		// Normaliza batch vazio: nil ou []byte{} → []byte{} sempre
		if len(incomingBatch) == 0 {
			incomingBatch = []byte{}
		}
		incomingDigest := sha256.Sum256(incomingBatch)
		
		if i.lastVal != nil {
			// Já tem valor aceito: valida digest
			if incomingDigest != i.lastDigest {
				fmt.Printf("[MPX][INST] sn=%d digest mismatch (rejecting without state change)\n", i.sn)
				return
			}
		} else {
			// Primeiro valor: aceita e registra
			i.lastVal = a.GetValue()
			i.lastDigest = incomingDigest
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

// onAccepted processa ACCEPTED (resposta da Fase 2)
// Quando quorum de accepts é atingido, líder envia COMMIT
// Todos os nós entregam o valor commitado
// CSMR: Só conta votos de membros do grupo
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

// onCommit processa COMMIT (entrega final)
// Valor foi decidido por consenso, pode ser entregue à aplicação
// CSMR: Propaga COMMIT para garantir que todos os membros do grupo recebam
// CRÍTICO: Sempre chama announce() para criar entry no log (evita GAP)
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
		
		// CRÍTICO: Anuncia NIL para criar entry no log (evita GAP)
		if i.announce != nil {
			i.announce(i.sn, []byte{}, nil)
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
		// CRÍTICO: Passa digest via metadata (3º parâmetro)
		digestBytes := i.lastDigest[:]
		i.announce(i.sn, i.lastVal.GetBatch(), digestBytes)
	} else {
		fmt.Printf("[MPX][INST] sn=%d announcer is nil!\n", i.sn)
	}

	i.closed = true
	traceCommit(i.sn, len(b.Requests))
}

// ==================== Propose / Tick ====================

// ProposeIfDue verifica se é hora de propor um novo batch
// Líder corta batch do bucket e inicia Fase 2 (ACCEPT)
// CRÍTICO: Pode propor batch vazio (NIL) para avançar SN quando não há requests
// Isso evita que segmentos travem esperando por requests que nunca chegam
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
		
		// Tenta cortar batch do bucket
		var rb *request.Batch
		if request.Buckets[i.bucketIndex].Len() > 0 {
			rb = i.seg.Buckets().CutBatchFromBucket(int(i.bucketIndex), int(i.seg.BatchSize()), 0)
		}
		
		// Se não há requests, propõe batch vazio (NIL) para avançar SN
		if rb == nil || rb.Message() == nil || len(rb.Message().Requests) == 0 {
			// Batch vazio determinístico (sempre []byte{})
			emptyBatch := &pb.Batch{Requests: []*pb.ClientRequest{}}
			batchBytes, err := proto.Marshal(emptyBatch)
			if err != nil {
				return
			}
			// Garante que batch vazio é sempre []byte{} (não nil)
			if len(batchBytes) == 0 {
				batchBytes = []byte{}
			}
			val = &pb.MPxValue{
				Id:    &pb.MPxInstanceId{Sn: i.sn, Lead: uint64(membership.OwnID)},
				Batch: batchBytes,
			}
			i.lastVal = val
			i.lastDigest = sha256.Sum256(batchBytes)
			fmt.Printf("[MPX][INST] sn=%d proposing EMPTY batch (no requests)\n", i.sn)
		} else {
			// Batch com requests: incrementa inflightLocal AQUI (não em runSegment)
			if !i.validateBatchHomogeneity(rb) {
				return
			}
			
			// CRÍTICO: Só incrementa inflight quando batch TEM requests
			if i.bucketId != 0 && !i.countedInflight {
				i.parent.inflightMu.Lock()
				i.parent.inflightLocal[i.bucketId]++
				i.parent.inflightMu.Unlock()
				i.countedInflight = true
				fmt.Printf("[MPX][INST] sn=%d inflightLocal[%d]++ (has requests)\n", i.sn, i.bucketId)
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
		}

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
		
		// CRÍTICO: Anuncia NIL para criar entry no log (evita GAP)
		if i.announce != nil {
			i.announce(i.sn, []byte{}, nil)
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

// validateBatchHomogeneity valida consistência do batch
// 1. Todas requests devem ter o mesmo groupId (não mistura grupos)
// 2. groupId do batch deve bater com bucketId da instância
// 3. CRÍTICO: Bucket 0 (barreira global) só aceita groupId=0
// Batches inválidos são rejeitados e requests devolvidas ao bucket
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
	
	// CRÍTICO: Bucket 0 (barreira global) só aceita groupId=0
	if i.bucketIndex == 0 && firstGroupId != 0 {
		fmt.Printf("[MPX][INST] sn=%d bucket 0 rejecting groupId=%d (must be 0), returning requests\n", i.sn, firstGroupId)
		batch.Resurrect()
		return false
	}
	
	return true
}