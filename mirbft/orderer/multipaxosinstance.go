/*
MultiPaxos Instance - Instância de Consenso Individual

Implementa uma única instância do protocolo MultiPaxos para um SN específico.
Cada instância executa o protocolo completo de consenso distribuído:

Fases do Protocolo:
1. PREPARE: Líder solicita permissão para propor (Phase 1a)
2. PROMISE: Seguidores prometem não aceitar ballots menores (Phase 1b)
3. ACCEPT: Líder propõe valor após quorum de promises (Phase 2a)
4. ACCEPTED: Seguidores aceitam o valor proposto (Phase 2b)
5. COMMIT: Líder commita após quorum de accepts e anuncia resultado

Características Avançadas:
- Thread-safe: Cada instância tem worker próprio e locks independentes
- Timeouts e Retransmissões: Detecta falhas e reelege líderes automaticamente
- Integração GSN: Suporta Global Sequence Numbers para ordem entre grupos
- Validação de Membership: Apenas membros do grupo podem votar
- Otimizações de Performance:
  * Grupo 0 prioriza requests sistêmicas (GSN_REQUEST, META_STREAM)
  * Propõe apenas GSN esperado (evita buffering excessivo)
  * Encoding/decoding de GSN em batches para cross-group operations

Arquitetura:
- Cada SN tem uma instância independente
- Instâncias são criadas sob demanda pelo MultiPaxosOrderer
- Workers assíncronos processam mensagens sem bloquear
- Integração com sistema de liveness para robustez
*/
package orderer
import (
	"crypto/sha256"
	"encoding/binary"
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
// GSN_MAGIC - Identificador mágico para batches com GSN embarcado
// Usado para distinguir batches normais de batches com Global Sequence Number
const GSN_MAGIC = uint32(0x47534E00) // "GSN\0"

// encodeGSNBatch - Embarca GSN no início do batch para cross-group operations
// Formato: [MAGIC(4) + GSN(8) + BATCH_ORIGINAL]
// Permite que o GSN seja preservado através do protocolo MultiPaxos
func encodeGSNBatch(gsn uint64, batchBytes []byte) []byte {
	result := make([]byte, 12+len(batchBytes))
	binary.LittleEndian.PutUint32(result[0:4], GSN_MAGIC)
	binary.LittleEndian.PutUint64(result[4:12], gsn)
	copy(result[12:], batchBytes)
	return result
}

// decodeGSNBatch - Extrai GSN do batch se presente
// Retorna: GSN, batch limpo, se tinha GSN
// Usado no commit para recuperar GSN de cross-group operations
func decodeGSNBatch(data []byte) (uint64, []byte, bool) {
	if len(data) < 12 {
		return 0, data, false
	}
	magic := binary.LittleEndian.Uint32(data[0:4])
	if magic != GSN_MAGIC {
		return 0, data, false
	}
	gsn := binary.LittleEndian.Uint64(data[4:12])
	return gsn, data[12:], true
}
// Fases do protocolo MultiPaxos
// Cada instância progride através destas fases durante o consenso
type instPhase int
const (
	phaseInit instPhase = iota      // Fase inicial - aguardando prepare
	phasePrepared                   // Quorum de promises obtido - pode propor
	phaseAcceptSent                 // Accept enviado - aguardando accepted
	phaseCommitted                  // Consenso alcançado - valor commitado
)
// mpxInstance - Uma instância de consenso MultiPaxos para um SN específico
// Cada instância executa o protocolo completo de forma independente
type mpxInstance struct {
	mu sync.Mutex                    // Protege estado da instância
	parent *MultiPaxosOrderer       // Referência ao orderer pai
	sn     int32                     // Sequence Number desta instância
	bucketId uint32                 // ID do grupo (bucket)
	bucketIndex int32               // Índice do bucket para requests
	proposeEvery  time.Duration     // Intervalo entre propostas
	announce      AnnounceFn        // Função para anunciar commits
	lastProposeAt time.Time         // Última vez que propôs
	closed        bool              // Se a instância foi fechada
	countedInflight bool            // Se está contada como em voo
	seg manager.Segment             // Segmento ao qual pertence
	members      []int32            // Membros do grupo
	quorum       int32              // Tamanho do quorum necessário
	
	// Estado do protocolo MultiPaxos
	promisedBallot  uint64          // Último ballot prometido
	acceptedBallot  uint64          // Último ballot aceito
	acceptedValue   *pb.MPxValue    // Último valor aceito
	prepared        bool            // Se já tem quorum de promises
	promiseCount    int32           // Contador de promises recebidas
	promisedFrom    map[int32]struct{} // Nós que enviaram promise
	lastReqBatch *request.Batch     // Último batch de requests
	phase        instPhase          // Fase atual do protocolo
	lastVal      *pb.MPxValue       // Último valor proposto
	lastDigest   [32]byte           // Hash do último valor
	acceptCount  int32              // Contador de accepts recebidos
	acceptedFrom map[int32]struct{} // Nós que enviaram accepted
	acceptRtxEvery   time.Duration  // Intervalo para retransmissão
	lastAcceptAt     time.Time      // Última vez que enviou accept
	prepSent bool                   // Se já enviou prepare
	leader   int32                  // Líder atual desta instância
	currentBallot int64             // Ballot atual
	
	// Canais para processamento assíncrono
	msgCh  chan *pb.ProtocolMessage // Canal de mensagens
	stopCh chan struct{}            // Canal para parar worker
	wg     sync.WaitGroup           // WaitGroup para sincronização
}
// newMPXInstance - Cria nova instância de consenso MultiPaxos
// Cada instância é responsável por um único SN e executa o protocolo completo
// Parâmetros: parent (orderer), sn (sequence number), announce (callback), interval (timeout)
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
		msgCh:          make(chan *pb.ProtocolMessage, 8192),
		stopCh:         make(chan struct{}),
		promisedBallot: 0,
		acceptedBallot: 0,
		currentBallot:  int64(uint64(membership.OwnID)), // Ballot inicial baseado no ID do nó
	}
	fmt.Printf("[MPX][INST] sn=%d created\n", sn)
	return inst
}
// setSegment - Vincula instância a um segmento e calcula quorum
// O quorum é calculado como maioria simples (n/2 + 1) dos membros do grupo
func (i *mpxInstance) setSegment(seg manager.Segment) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.seg = seg
	if len(i.members) == 0 {
		// Fallback: usa todos os nós se grupo não definido
		n := int32(len(membership.AllNodeIDs()))
		if n < 1 {
			n = 1
		}
		i.quorum = n/2 + 1
		fmt.Printf("[MPX][INST] sn=%d segment bound, quorum=%d (fallback all nodes)\n", i.sn, i.quorum)
	} else {
		// Usa membros do grupo específico
		n := int32(len(i.members))
		if n < 1 {
			n = 1
		}
		i.quorum = n/2 + 1
		fmt.Printf("[MPX][INST] sn=%d segment bound, quorum=%d (group members=%d)\n", i.sn, i.quorum, len(i.members))
	}
}
// startWorkers - Inicia worker assíncrono para processar mensagens
// Cada instância tem seu próprio worker para evitar bloqueios entre instâncias
func (i *mpxInstance) startWorkers(_ *sync.WaitGroup) {
	i.wg.Add(1)
	go func() {
		defer i.wg.Done()
		for {
			select {
			case pm := <-i.msgCh:
				if pm == nil {
					return // Canal fechado
				}
				i.handleMPxMsg(pm, pm.GetMultipaxos())
			case <-i.stopCh:
				return // Sinal de parada
			}
		}
	}()
}
func (i *mpxInstance) stopWorkers() {
	i.mu.Lock()
	select {
	case <-i.stopCh:
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
// handleMPxMsg - Roteador principal de mensagens do protocolo MultiPaxos
// Direciona cada tipo de mensagem para o handler apropriado
func (i *mpxInstance) handleMPxMsg(pm *pb.ProtocolMessage, mpx *pb.MPxMsg) {
	switch t := mpx.Type.(type) {
	case *pb.MPxMsg_Prepare:
		fmt.Printf("[MPX][INST] sn=%d PREPARE from=%d\n", i.sn, pm.GetSenderId())
		i.onPrepare(t.Prepare) // Phase 1a: Líder solicita permissão
	case *pb.MPxMsg_Promise:
		fmt.Printf("[MPX][INST] sn=%d PROMISE from=%d\n", i.sn, pm.GetSenderId())
		i.onPromise(pm.GetSenderId(), t.Promise) // Phase 1b: Seguidor promete
	case *pb.MPxMsg_Accept:
		fmt.Printf("[MPX][INST] sn=%d ACCEPT from=%d\n", i.sn, pm.GetSenderId())
		i.onAccept(pm.GetSenderId(), t.Accept) // Phase 2a: Líder propõe valor
	case *pb.MPxMsg_Accepted:
		fmt.Printf("[MPX][INST] sn=%d ACCEPTED from=%d\n", i.sn, pm.GetSenderId())
		i.onAccepted(pm, t.Accepted) // Phase 2b: Seguidor aceita valor
	case *pb.MPxMsg_Commit:
		fmt.Printf("[MPX][INST] sn=%d COMMIT from=%d\n", i.sn, pm.GetSenderId())
		i.onCommit(t.Commit) // Commit: Líder anuncia consenso
	default:
		fmt.Printf("[MPX][INST] sn=%d UNKNOWN msg type\n", i.sn)
	}
}
func (i *mpxInstance) onPrepare(prepare *pb.MPxPrepare) {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.phase == phaseCommitted {
		fmt.Printf("[MPX][INST] sn=%d ignoring PREPARE (already committed)\n", i.sn)
		return
	}
	ballot := uint64(prepare.GetBallot())
	i.bucketId = prepare.GetGroupId()
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
	leaderID := int32(prepare.GetId().GetLead())
	if leaderID == membership.OwnID {
		fmt.Printf("[MPX][INST] sn=%d skip PROMISE to self\n", i.sn)
		return
	}
	fmt.Printf("[MPX][INST] sn=%d sending PROMISE ballot=%d groupId=%d to leader=%d\n", i.sn, ballot, groupId, leaderID)
	messenger.EnqueueMsg(out, leaderID)
}
func (i *mpxInstance) onPromise(from int32, promise *pb.MPxPromise) {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.promisedFrom == nil {
		i.promisedFrom = make(map[int32]struct{})
	}
	if _, ok := i.promisedFrom[from]; ok {
		return
	}
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
func (i *mpxInstance) onAccept(from int32, a *pb.MPxAccept) {
	i.mu.Lock()
	defer i.mu.Unlock()
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
	if a.GetValue() != nil {
		incomingBatch := a.GetValue().GetBatch()
		if len(incomingBatch) == 0 {
			incomingBatch = []byte{}
		}
		incomingDigest := sha256.Sum256(incomingBatch)
		if i.lastVal != nil {
			if incomingDigest != i.lastDigest {
				fmt.Printf("[MPX][INST] sn=%d digest mismatch (rejecting without state change)\n", i.sn)
				return
			}
		} else {
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
	accepted := &pb.MPxMsg{Type: &pb.MPxMsg_Accepted{Accepted: &pb.MPxAccepted{
		Id:      &pb.MPxInstanceId{Sn: i.sn, Lead: uint64(membership.OwnID)},
		Ballot:  ballot,
		Ok:      true,
		GroupId: i.bucketId,
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
	
	// Incorporada lógica da função duplicada ProposeIfDue() aqui
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
		
		// Chama onCommit diretamente
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
		fmt.Printf("[MPX][INST] sn=%d NIL commit\n", i.sn)
		i.phase = phaseCommitted
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
	if i.lastReqBatch != nil {
		request.RemoveBatch(i.lastReqBatch)
		i.lastReqBatch = nil
	}
	var crossOpGSN uint64
	var batchBytes []byte
	batchBytes = i.lastVal.GetBatch()
	innerBatch := batchBytes
	if gsn, inner, hasGSN := decodeGSNBatch(batchBytes); hasGSN {
		crossOpGSN = gsn
		innerBatch = inner // Usa batch limpo sem MAGIC
		fmt.Printf("[CROSS-OP] sn=%d onCommit decoded gsn=%d\n", i.sn, gsn)
	}
	var b pb.Batch
	if err := proto.Unmarshal(innerBatch, &b); err != nil {
		fmt.Printf("[MPX][INST] sn=%d onCommit unmarshal error: %v\n", i.sn, err)
		return
	}
	if len(b.Requests) > 0 && len(b.Requests[0].TouchedGroups) > 0 {
		// touchedGroups = b.Requests[0].TouchedGroups // Removed unused variable
	}
	if crossOpGSN > 0 && GlobalMulticastOrderer != nil {
		// Removida publicação redundante de META
		// META é publicado apenas UMA vez pelo proxy no requesthandler.go
		
		// Ponto 3: Verifica ordem sequencial antes de entregar
		if !GlobalMulticastOrderer.ADeliver(crossOpGSN, i.bucketId, i.lastVal.GetBatch()) {
			fmt.Printf("[GSN-ALL] sn=%d gsn=%d out of order, buffering\n", i.sn, crossOpGSN)
			// Buffer commit fora de ordem
			if i.announce != nil {
				digestBytes := i.lastDigest[:]
				GlobalMulticastOrderer.BufferCommit(crossOpGSN, i.bucketId, i.lastVal.GetBatch(), i.announce, i.sn, digestBytes)
			}
			i.closed = true
			traceCommit(i.sn, len(b.Requests))
			return
		}
		
		fmt.Printf("[GSN-ALL] sn=%d committed gsn=%d (sequential order verified)\n", i.sn, crossOpGSN)
	}
	if i.announce != nil {
		fmt.Printf("[MPX][INST] sn=%d announcing commit, size=%d\n", i.sn, len(b.Requests))
		digestBytes := i.lastDigest[:]
		i.announce(i.sn, i.lastVal.GetBatch(), digestBytes)
	} else {
		fmt.Printf("[MPX][INST] sn=%d announcer is nil!\n", i.sn)
	}
	i.closed = true
	traceCommit(i.sn, len(b.Requests))
}
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
		var rb *request.Batch
		if i.seg == nil || i.bucketIndex < 0 {
			return
		}
		// ✅ CORREÇÃO: Propõe apenas GSN esperado para evitar buffering excessivo
		// Obtém expectedNextGSN para este grupo
		var expectedGSN uint64
		if GlobalMulticastOrderer != nil {
			GlobalMulticastOrderer.expectedGSNMu.RLock()
			expected, exists := GlobalMulticastOrderer.expectedNextGSN[i.bucketId]
			if !exists {
				expected = 1
			}
			expectedGSN = expected
			GlobalMulticastOrderer.expectedGSNMu.RUnlock()
		} else {
			expectedGSN = 1 // Fallback se não há multicast orderer
		}
		
		request.Buckets[i.bucketIndex].Lock()
		
		// Grupo 0 prioriza requests sistêmicas (GSN_REQUEST, META_STREAM)
		var selectedReq *request.Request
		if i.bucketId == 0 {
			// Grupo 0: busca primeiro request sistêmico
			systemReq := request.Buckets[i.bucketIndex].FindSystemRequest()
			if systemReq != nil {
				selectedReq = systemReq
				fmt.Printf("[SYSTEM-PRIORITY] sn=%d group=0 prioritizing system request\n", i.sn)
			} else {
				// Se não há system, busca GSN exato esperado
				selectedReq = request.Buckets[i.bucketIndex].FindRequestWithGSN(expectedGSN)
			}
		} else {
			// Outros grupos: busca GSN exato esperado
			selectedReq = request.Buckets[i.bucketIndex].FindRequestWithGSN(expectedGSN)
		}
		
		if selectedReq != nil {
			// Remove request selecionado
			request.Buckets[i.bucketIndex].Remove([]*request.Request{selectedReq})
			request.Buckets[i.bucketIndex].Unlock()
			batchMsg := &pb.Batch{Requests: []*pb.ClientRequest{selectedReq.Msg}}
			rb = &request.Batch{Requests: []*request.Request{selectedReq}}
			if i.bucketId == 0 {
				fmt.Printf("[SYSTEM-PRIORITY] sn=%d group=0 proposing system/expected request\n", i.sn)
			} else {
				fmt.Printf("[GSN-EXACT] sn=%d group=%d proposing expected gsn=%d (no buffering)\n", 
					i.sn, i.bucketId, expectedGSN)
			}
		} else {
			// Sem request adequado: propõe batch vazio
			request.Buckets[i.bucketIndex].Unlock()
			if i.bucketId == 0 {
				fmt.Printf("[SYSTEM-PRIORITY] sn=%d group=0 no system requests, will propose empty\n", i.sn)
			} else {
				fmt.Printf("[GSN-EXACT] sn=%d group=%d no request with expected gsn=%d, will propose empty\n", 
					i.sn, i.bucketId, expectedGSN)
			}
		}
		if rb == nil || rb.Message() == nil || len(rb.Message().Requests) == 0 {
			emptyBatch := &pb.Batch{Requests: []*pb.ClientRequest{}}
			batchBytes, err := proto.Marshal(emptyBatch)
			if err != nil {
				return
			}
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
			if !i.validateBatchHomogeneity(rb) {
				return
			}
			batchMsg := rb.Message()
			if len(batchMsg.Requests) > 0 && len(batchMsg.Requests[0].TouchedGroups) > 1 {
				if len(batchMsg.Requests) != 1 {
					fmt.Printf("[CROSS-OP][ERROR] sn=%d batch has %d requests, cross-op must be alone\n", 
						i.sn, len(batchMsg.Requests))
					rb.Resurrect()
					return
				}
			}
			var crossOpGSN uint64
			for _, req := range rb.Requests {
				if len(req.Msg.TouchedGroups) > 1 {
					crossOpGSN = req.Msg.GSN
					if crossOpGSN == 0 {
						fmt.Printf("[CROSS-OP][ERROR] sn=%d opid=%s missing GSN from proxy\n", i.sn, req.OpID)
						rb.Resurrect()
						return
					}
					fmt.Printf("[CROSS-OP] sn=%d opid=%s gsn=%d groups=%v (from proxy)\n", 
						i.sn, req.OpID, crossOpGSN, req.Msg.TouchedGroups)
					break
				}
			}
			i.lastReqBatch = rb
			reqs = len(batchMsg.Requests)
			batchBytes, err := proto.Marshal(batchMsg)
			if err != nil {
				return
			}
			// GSN para todas: sempre embute GSN se presente
			var gsnToEncode uint64
			if crossOpGSN > 0 {
				gsnToEncode = crossOpGSN
			} else if len(batchMsg.Requests) > 0 && batchMsg.Requests[0].GSN > 0 {
				gsnToEncode = batchMsg.Requests[0].GSN
			}
			if gsnToEncode > 0 {
				batchBytes = encodeGSNBatch(gsnToEncode, batchBytes)
				fmt.Printf("[GSN-ALL] sn=%d encoded gsn=%d into batch (cross-op=%t)\n", i.sn, gsnToEncode, crossOpGSN > 0)
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
// tick - Gerencia timeouts e retransmissões da instância
// Implementa dois níveis de timeout:
// 1. Retransmissão de ACCEPT (acceptRtxEvery)
// 2. Nova rodada com ballot maior (acceptRtxEvery * 3)
func (i *mpxInstance) tick(now time.Time) {
	i.mu.Lock()
	defer i.mu.Unlock()
	
	// Nível 1: Retransmissão de ACCEPT se não obteve quorum
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
	
	// Nível 2: Nova rodada com ballot maior (reeleição de líder)
	if i.phase == phaseAcceptSent && i.acceptCount < i.quorum && now.Sub(i.lastAcceptAt) >= i.acceptRtxEvery*3 {
		// Incrementa contador de rodadas no ballot
		seenCounter := uint64(i.currentBallot) >> 32
		i.currentBallot = int64((seenCounter + 1) << 32 | uint64(membership.OwnID))
		
		// Reset completo da instância para nova rodada
		i.phase = phaseInit
		i.prepared = false
		i.promiseCount = 0
		i.promisedFrom = nil
		i.acceptCount = 0
		i.acceptedFrom = nil
		
		fmt.Printf("[MPX][INST] sn=%d TIMEOUT: starting new round, ballot=%d\n", i.sn, i.currentBallot)
		
		// Inicia nova rodada com PREPARE
		prep := &pb.MPxMsg{Type: &pb.MPxMsg_Prepare{
			Prepare: &pb.MPxPrepare{
				Id:      &pb.MPxInstanceId{Sn: i.sn, Lead: uint64(membership.OwnID)},
				Ballot:  uint64(i.currentBallot),
				GroupId: i.bucketId,
			},
		}}
		pm := &pb.ProtocolMessage{
			SenderId: membership.OwnID,
			Sn:       i.sn,
			Msg:      &pb.ProtocolMessage_Multipaxos{Multipaxos: prep},
		}
		if i.parent.emit != nil {
			i.parent.emit(pm)
		}
		i.lastAcceptAt = now
	}
}
// SetMembers - Define membros do grupo e recalcula quorum
// Atualiza contadores de votos para refletir nova membership
func (i *mpxInstance) SetMembers(members []int32) {
	i.mu.Lock()
	defer i.mu.Unlock()
	
	// Copia lista de membros
	i.members = make([]int32, len(members))
	copy(i.members, members)
	
	// Calcula novo quorum (maioria simples)
	n := int32(len(i.members))
	if n < 1 {
		n = 1
	}
	i.quorum = n/2 + 1
	fmt.Printf("[MPX][INST] sn=%d SetMembers: members=%v quorum=%d\n", i.sn, members, i.quorum)
	
	// Recontagem de votos accepted para nova membership
	if i.acceptedFrom != nil {
		newAcceptedFrom := make(map[int32]struct{})
		newCount := int32(0)
		
		// Mantém apenas votos de membros atuais do grupo
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
// isInGroup - Verifica se um nó é membro do grupo desta instância
// Usado para validar votos e membership durante o consenso
func (i *mpxInstance) isInGroup(nodeID int32) bool {
	for _, member := range i.members {
		if member == nodeID {
			return true
		}
	}
	return false
}

// tracePropose - Registra evento de proposta no sistema de tracing
func tracePropose(sn int32, size int) {
	tracing.MainTrace.Event(tracing.PROPOSE, int64(sn), int64(size))
}

// traceCommit - Registra evento de commit no sistema de tracing
func traceCommit(sn int32, size int) {
	tracing.MainTrace.Event(tracing.COMMIT, int64(sn), int64(size))
}
// validateBatchHomogeneity - Valida que todas as requests no batch pertencem ao mesmo grupo
// Garante que instâncias não processem requests de grupos diferentes
// Retorna false e ressuscita requests se validação falhar
func (i *mpxInstance) validateBatchHomogeneity(batch *request.Batch) bool {
	reqs := batch.Message().Requests
	if len(reqs) == 0 {
		return true // Batch vazio é sempre válido
	}
	
	// Verifica se todas as requests têm o mesmo GroupId
	firstGroupId := reqs[0].GetGroupId()
	for _, req := range reqs {
		if req.GetGroupId() != firstGroupId {
			fmt.Printf("[MPX][INST] sn=%d heterogeneous batch detected, returning requests\n", i.sn)
			batch.Resurrect() // Retorna requests para fila
			return false
		}
	}
	
	// Verifica se GroupId do batch corresponde ao grupo desta instância
	if firstGroupId != i.bucketId {
		fmt.Printf("[MPX][INST] sn=%d groupId mismatch: batch=%d expected=%d, returning requests\n", i.sn, firstGroupId, i.bucketId)
		batch.Resurrect()
		return false
	}
	
	// Validação especial para bucket 0 (sequenciador GSN)
	if i.bucketIndex == 0 && firstGroupId != 0 {
		fmt.Printf("[MPX][INST] sn=%d bucket 0 rejecting groupId=%d (must be 0), returning requests\n", i.sn, firstGroupId)
		batch.Resurrect()
		return false
	}
	
	return true
}
