// MultiPaxos Instance - Uma instância de consenso para um SN específico.
// Protocolo: PREPARE → PROMISE → ACCEPT → ACCEPTED → COMMIT
package orderer

import (
	"fmt"
	"sync"
	"sync/atomic"
	"time"
	"github.com/hyperledger-labs/mirbft/config"
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

// mpxInstance é uma rodada de consenso Paxos completa para UM número de sequência (SN): decide
// qual batch (ou ⊥/NOP) ocupa essa posição do log. Cada SN do segmento tem a sua própria
// mpxInstance, todas rodando em paralelo (ver runSegment em multipaxosorderer.go).
type mpxInstance struct {
	mu               sync.Mutex          // protege todos os campos abaixo (acessados por múltiplas goroutines: worker, ticker, ProposeIfDue)
	parent           *MultiPaxosOrderer  // orderer dono desta instância (dá acesso a emit/am/maxBatchSize/etc)
	sn               int32               // número de sequência que esta instância decide
	bucketId         uint32              // grupo/bucket de dados ao qual este SN pertence
	groupBucketIDs   []int               // buckets de requisição atribuídos a este grupo (round-robin sobre todos os buckets)
	groupBucketGroup *request.BucketGroup // fila de requisições pendentes desses buckets, de onde os batches são cortados
	proposeEvery     time.Duration       // intervalo do batch timeout, herdado do orderer pai
	announce         AnnounceFn          // callback de commit, herdado do orderer pai
	lastProposeAt    time.Time           // timestamp da última tentativa de proposta (ProposeIfDue)
	closed           bool                // true depois que este SN já comitou (evita reprocessar)
	seg              manager.Segment     // segmento ISS ao qual este SN pertence
	members          []int32             // membros do grupo participando do consenso deste SN
	quorum           int32               // maioria simples necessária para decidir (ver comentário em setSegment)
	promisedBallot   uint64              // maior ballot já prometido por este nó (regra de promessa do Paxos)
	prepared         bool                // true depois de reunir promessas suficientes (fase PREPARE concluída)
	promiseCount     int32               // quantas PROMISE já recebemos na rodada do ballot atual
	promisedFrom     map[int32]struct{}  // de quais nós já recebemos PROMISE (evita contar duplicata)
	lastReqBatch     *request.Batch      // batch real cortado do bucket, se este nó é quem propôs (nil se seguiu ACCEPT de outro líder)
	phase            instPhase           // fase atual do protocolo para este SN (Init/Prepared/AcceptSent/Committed)
	lastVal          *pb.MPxValue        // valor (batch) sendo proposto/aceito na rodada atual
	lastDigest       [32]byte            // digest de lastVal, comparado com o digest recebido no COMMIT
	acceptCount      int32               // quantos ACCEPTED já recebemos na rodada do ballot atual
	acceptedFrom     map[int32]struct{}  // de quais nós já recebemos ACCEPTED (evita contar duplicata)
	acceptRtxEvery   time.Duration       // intervalo entre retransmissões de ACCEPT (2x proposeEvery)
	lastAcceptAt     time.Time           // timestamp do último ACCEPT enviado; tick() usa para decidir se retransmite
	roundStartAt     time.Time           // quando a rodada do ballot atual começou; tick() usa para decidir se troca de ballot
	stuckSince       time.Time           // primeiro tick observado sem quorum (ver bloco GIVE-UP em tick())
	prepSent         bool                // true depois que este nó, como líder inicial, já mandou seu primeiro PREPARE
	leader           int32               // nó que este SN reconhece como líder atual (-1 = ainda não definido)
	currentBallot    int64               // ballot que este nó está usando/reconhecendo agora (ver nextBallotAfter)
	inProposal       bool                // trava reentrância de ProposeIfDue (evita duas propostas concorrentes)
	acceptTs         int64               // timestamp de quando este nó aceitou o valor (métricas de latência)
	lastAcceptedTs   int64               // timestamp do último ACCEPTED recebido (métricas de latência)
	pendingCommitDigest *[32]byte        // digest de um COMMIT recebido mas ainda não entregável (esperando state transfer)
	fetchInFlight    bool                // true enquanto uma busca de state transfer para este SN está em andamento
	msgCh            chan *pb.ProtocolMessage // fila de mensagens de protocolo recebidas para esta instância
	stopCh           chan struct{}       // fechado para sinalizar à goroutine worker que pare
	wg               sync.WaitGroup      // usado por stopWorkers para esperar a goroutine worker terminar
}

func newMPXInstance(parent *MultiPaxosOrderer, sn int32, announce AnnounceFn, interval time.Duration) *mpxInstance {
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
	// Quorum = maioria simples (n/2 + 1), não 2n/3+1 como em protocolos tolerantes a Bizantinos:
	// MultiPaxos só tolera falhas por parada (crash), e duas maiorias de tamanho n/2+1 sempre
	// se sobrepõem em pelo menos um nó — é isso que impede duas maiorias de decidirem valores
	// diferentes para o mesmo número de sequência.
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

func (i *mpxInstance) startWorkers() {
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

// nextBallotAfter calcula um ballot estritamente maior que "seen". Um ballot é codificado em
// 64 bits: os 32 bits mais altos são um contador de rodada (só cresce) e os 32 bits mais baixos
// são o ID do nó proponente. Isso garante duas coisas: (1) ballots ficam totalmente ordenados
// entre todos os nós do sistema, e (2) dois nós nunca produzem o mesmo ballot, porque o ID do
// nó desempata rodadas iguais.
func nextBallotAfter(seen uint64) int64 {
	seenCounter := seen >> 32
	return int64((seenCounter+1)<<32 | uint64(membership.OwnID))
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
	// Alguém propôs um ballot mais novo que o nosso enquanto ainda nos achávamos líder:
	// abandonamos a rodada atual e recomeçamos com um ballot ainda maior (ver nextBallotAfter).
	if int64(ballot) > i.currentBallot && i.leader == membership.OwnID {
		i.currentBallot = nextBallotAfter(ballot)
		i.leader = -1; i.phase = phaseInit; i.prepared = false
		i.prepSent = false; i.promiseCount = 0; i.promisedFrom = nil
	}
	// Regra de "promessa" do Paxos: um acceptor nunca aceita/promete um ballot menor que o
	// maior que já prometeu. É isso que impede um líder antigo/lento de sobrescrever uma
	// decisão já tomada por um líder mais novo.
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
	// O ballot já codifica o ID de quem propôs (ver nextBallotAfter), então dois remetentes
	// diferentes nunca deveriam apresentar o mesmo ballot — se isso acontece aqui, é uma
	// mensagem duplicada/atrasada de uma rodada já conhecida, e a descartamos.
	if i.leader != -1 && i.leader != from && ballot == i.promisedBallot { return }
	i.promisedBallot = ballot
	i.leader = from

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
		i.callUnlocked(func() { i.onAccepted(resp, accepted.Type.(*pb.MPxMsg_Accepted).Accepted) })
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
		i.callUnlocked(func() {
			i.onCommit(&pb.MPxCommit{Id: &pb.MPxInstanceId{Sn: i.sn}, Digest: i.lastDigest[:], GroupId: i.bucketId})
		})
	}
}

// callUnlocked chama f() depois de soltar o mutex desta instância e o retrava em seguida.
// Usado quando este nó precisa processar sua própria mensagem localmente (ex: ele é o líder,
// então não passa pela rede) — como f() reentra em métodos que também travam i.mu, soltar o
// lock antes de chamar evita deadlock.
func (i *mpxInstance) callUnlocked(f func()) {
	i.mu.Unlock()
	f()
	i.mu.Lock()
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
	// Este nó pode não ter o valor que realmente atingiu quorum: ou porque ainda não recebeu
	// nenhum ACCEPT para este SN, ou porque o valor que ele próprio aceitou (i.lastVal) é
	// diferente do que venceu em outro lugar (ex: perdeu uma corrida de ballot). Nos dois
	// casos, busca o valor comitado de verdade via state transfer em vez de confiar no local.
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
	// Notify parent orderer to advance SN immediately after this function completes
	defer func() {
		if i.parent != nil && i.parent.commitNotifyCh != nil {
			select {
			case i.parent.commitNotifyCh <- struct{}{}:
			default:
			}
		}
	}()
	if i.lastReqBatch != nil {
		request.RemoveBatch(i.lastReqBatch); i.lastReqBatch = nil
	} else {
		// Follower path: we received the batch via ACCEPT (not CutBatch).
		// Remove committed requests from local buckets to prevent re-proposal
		// when this node becomes leader of a future instance.
		if i.lastVal != nil && i.lastVal.GetBatch() != nil {
			request.RemoveCommittedFromBuckets(i.lastVal.GetBatch())
		}
	}

	b := i.lastVal.GetBatch()

	// NOP (batch vazio): um valor ⊥ é um resultado legítimo e deliberado em Paxos, não um erro.
	// Acontece quando não havia requisições pendentes na hora de propor, ou quando esta
	// instância desistiu de esperar quorum (ver o bloco GIVE-UP em tick()). De qualquer forma,
	// o número de sequência precisa ser preenchido para o log poder avançar.
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

	// Cross-group: verifica ordem GSN para cada cross-op no batch
	var crossOps []*pb.ClientRequest
	for _, req := range b.Requests {
		if len(req.TouchedGroups) > 1 && req.GSN > 0 {
			crossOps = append(crossOps, req)
		}
	}
	if len(crossOps) > 0 && GetGlobalMulticastOrderer() != nil {
		fmt.Printf("[MPX] sn=%d CROSS-OP-BATCH count=%d gsn_range=[%d..%d] group=%d\n",
			i.sn, len(crossOps), crossOps[0].GSN, crossOps[len(crossOps)-1].GSN, i.bucketId)
		// Try ADeliver for each cross-op GSN in order.
		// If any GSN blocks, buffer the entire batch at that GSN.
		for _, cop := range crossOps {
			if !GetGlobalMulticastOrderer().ADeliver(cop.GSN, i.bucketId, b) {
				GetGlobalMulticastOrderer().BufferCommit(cop.GSN, i.bucketId, b, i.announce, i.sn, i.lastDigest[:])
				i.closed = true; traceCommit(i.sn, len(b.Requests))
				return
			}
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
	// Já existe uma rodada de ACCEPT em aberto para este SN (enviamos um ACCEPT mas ainda não
	// comitou) — não propõe de novo por cima, só espera essa rodada terminar ou o tick()
	// decidir tentar de novo com um ballot maior.
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
		rb := i.groupBucketGroup.CutBatchWithMode(i.parent.maxBatchSize, i.proposeEvery,
			(atomic.AddInt32(&i.parent.batchCounter, 1)%2) == 0)
		if rb == nil || rb.Message() == nil || len(rb.Message().Requests) == 0 {
			emptyBatch := &pb.Batch{Requests: nil}
			val = &pb.MPxValue{Id: &pb.MPxInstanceId{Sn: i.sn, Lead: uint64(membership.OwnID)}, Batch: emptyBatch}
			i.lastVal = val
			copy(i.lastDigest[:], request.BatchDigest(emptyBatch))
			i.lastReqBatch = nil
		} else {
			if !i.validateBatchHomogeneity(rb) { return }
			batchMsg := rb.Message()
			// Cross-op validation: all cross-ops must have GSN assigned.
			// A batch can contain multiple cross-ops (batched by GSN order)
			// or only single-group requests, but not a mix.
			hasCrossOp := false
			hasSingleGroup := false
			for _, req := range rb.Requests {
				if len(req.Msg.TouchedGroups) > 1 {
					if req.Msg.GSN == 0 { rb.Resurrect(); return }
					hasCrossOp = true
				} else {
					hasSingleGroup = true
				}
			}
			if hasCrossOp && hasSingleGroup {
				rb.Resurrect(); return
			}
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
		i.sendAccept(val.GetBatch())
	}
	i.phase = phaseAcceptSent; i.lastAcceptAt = time.Now()
	tracing.MainTrace.Event(tracing.PROPOSE, int64(i.sn), int64(reqs))
}

// sendAccept envia (ou reenvia) uma mensagem ACCEPT com o ballot atual desta instância para o
// batch informado — usado tanto na primeira proposta (ProposeIfDue) quanto nas retransmissões
// disparadas por tick(). O chamador já garante que i.parent.emit não é nil.
func (i *mpxInstance) sendAccept(batch *pb.Batch) {
	i.parent.emit(&pb.ProtocolMessage{SenderId: membership.OwnID, Sn: i.sn,
		Msg: &pb.ProtocolMessage_Multipaxos{Multipaxos: &pb.MPxMsg{Type: &pb.MPxMsg_Accept{
			Accept: &pb.MPxAccept{
				Id: &pb.MPxInstanceId{Sn: i.sn, Lead: uint64(membership.OwnID)},
				Ballot: uint64(i.currentBallot), Batch: batch, GroupId: i.bucketId,
			},
		}}}})
}

func (i *mpxInstance) tick(now time.Time) {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.phase != phaseAcceptSent || i.acceptCount >= i.quorum { return }

	// SB4 (Eventual Progress, ISS/EuroSys'22 §2.2): se este SN não consegue quorum por
	// tempo suficiente (líder suspeito/peers indisponíveis), desiste e commita ⊥ (NOP) em
	// vez de retransmitir/trocar de ballot para sempre. Sem isso, um peer que nunca mais
	// alcança quorum (porque peers suficientes saíram do grupo) trava o segmento inteiro
	// e, por causa de WaitForCheckpoints, o resto do sweep de experimentos.
	if i.stuckSince.IsZero() {
		i.stuckSince = now // marca o primeiro tick sem quorum observado, não o último
	} else if now.Sub(i.stuckSince) >= config.Config.ViewChangeTimeout {
		fmt.Printf("[MPX] sn=%d GIVE-UP after %s sem quorum (%d/%d) — commitando NOP\n",
			i.sn, config.Config.ViewChangeTimeout, i.acceptCount, i.quorum)
		if i.lastReqBatch != nil {
			// Devolve as requisições cortadas para o bucket em vez de perdê-las: como esta
			// instância vai comitar ⊥, elas serão propostas de novo numa instância futura.
			i.lastReqBatch.Resurrect()
			i.lastReqBatch = nil
		}
		emptyBatch := &pb.Batch{Requests: nil}
		i.lastVal = &pb.MPxValue{Id: &pb.MPxInstanceId{Sn: i.sn, Lead: uint64(membership.OwnID)}, Batch: emptyBatch}
		copy(i.lastDigest[:], request.BatchDigest(emptyBatch))
		i.deliverCommit()
		return
	}

	// Retransmissão de ACCEPT
	if now.Sub(i.lastAcceptAt) >= i.acceptRtxEvery && i.parent.emit != nil && i.lastVal != nil {
		i.sendAccept(i.lastVal.GetBatch())
		i.lastAcceptAt = now
	}

	// Nova rodada com ballot maior: depois de retransmitir sem sucesso por um tempo, em vez de
	// insistir no mesmo ballot para sempre, inicia uma troca de líder informal — volta para o
	// início do protocolo (PREPARE) com um ballot mais alto. Isso é diferente do GIVE-UP acima:
	// aqui ainda tentamos progredir dentro do protocolo normal; o GIVE-UP é o último recurso.
	if now.Sub(i.roundStartAt) >= i.acceptRtxEvery*3 {
		i.currentBallot = nextBallotAfter(uint64(i.currentBallot))
		i.phase = phaseInit; i.prepared = false
		i.promiseCount = 0; i.promisedFrom = nil
		i.acceptCount = 0; i.acceptedFrom = nil; i.roundStartAt = now
		if i.parent.emit != nil { i.sendPrepare() }
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
	i.quorum = n/2 + 1 // maioria simples — ver comentário em setSegment()

	// Initialize groupBucketGroup eagerly (don't wait for onPrepare)
	if i.groupBucketGroup == nil && i.bucketId < uint32(len(request.Buckets)) {
		fmt.Printf("[MPX] sn=%d INIT-BUCKETS-EAGER group=%d (SetMembers)\n", i.sn, i.bucketId)
		i.mu.Unlock()
		i.initGroupBuckets()
		i.mu.Lock()
	}

	if i.leader == -1 {
		// Líder deste SN é escolhido por round-robin determinístico entre os membros do grupo:
		// todo nó calcula o mesmo dono só a partir do número de sequência, sem troca de
		// mensagens. Por isso SNs diferentes do mesmo segmento normalmente têm líderes
		// diferentes, mesmo sem nenhuma eleição explícita ter acontecido.
		i.leader = i.members[i.sn%n]
		fmt.Printf("[MPX] sn=%d SetMembers members=%v quorum=%d leader=%d\n", i.sn, members, i.quorum, i.leader)
		i.currentBallot = int64(uint64(0)<<32 | uint64(i.leader))
		if i.leader == membership.OwnID && !i.prepSent {
			i.prepSent = true
			if i.parent.emit != nil { i.sendPrepare() }
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

// sendPrepare envia uma mensagem PREPARE com o ballot atual desta instância — usado tanto ao
// assumir a liderança pela primeira vez (SetMembers) quanto ao iniciar uma nova rodada (tick).
// O chamador já garante que i.parent.emit não é nil.
func (i *mpxInstance) sendPrepare() {
	i.parent.emit(&pb.ProtocolMessage{SenderId: membership.OwnID, Sn: i.sn,
		Msg: &pb.ProtocolMessage_Multipaxos{Multipaxos: &pb.MPxMsg{Type: &pb.MPxMsg_Prepare{
			Prepare: &pb.MPxPrepare{
				Id: &pb.MPxInstanceId{Sn: i.sn, Lead: uint64(membership.OwnID)},
				Ballot: uint64(i.currentBallot), GroupId: i.bucketId,
			},
		}}}})
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
