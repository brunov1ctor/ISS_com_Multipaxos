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
	"encoding/binary"
	"fmt"
	"strings"
	"sync"
	"time"
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
	groupBucketIDs []int            // IDs dos buckets deste grupo
	groupBucketGroup *request.BucketGroup // BucketGroup agregado para batching eficiente
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
	lastAcceptAt     time.Time      // Última vez que enviou accept (para retransmissão)
	roundStartAt     time.Time      // Início da rodada atual (para timeout de rodada)
	prepSent bool                   // Se já enviou prepare
	leader   int32                  // Líder atual desta instância
	currentBallot int64             // Ballot atual
	inProposal bool                 // Debounce: evita build batch múltiplo no mesmo tick
	
	// Canais para processamento assíncrono
	msgCh  chan *pb.ProtocolMessage // Canal de mensagens
	stopCh chan struct{}            // Canal para parar worker
	wg     sync.WaitGroup           // WaitGroup para sincronização
}
// newMPXInstance - Cria nova instância de consenso MultiPaxos
// Cada instância é responsável por um único SN e executa o protocolo completo
// Parâmetros: parent (orderer), sn (sequence number), announce (callback), interval (timeout)
func newMPXInstance(parent *MultiPaxosOrderer, sn int32, announce AnnounceFn, _ int, interval time.Duration) *mpxInstance {
	now := time.Now()
	inst := &mpxInstance{
		parent:         parent,
		sn:             sn,
		bucketId:       0,
		proposeEvery:   interval,
		announce:       announce,
		lastProposeAt:  now,
		phase:          phaseInit,
		acceptRtxEvery: interval * 2,
		lastAcceptAt:   now,
		roundStartAt:   now,
		msgCh:          make(chan *pb.ProtocolMessage, 8192),
		stopCh:         make(chan struct{}),
		promisedBallot: 0,
		acceptedBallot: 0,
		currentBallot:  int64(uint64(membership.OwnID)),
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

// initGroupBuckets - Inicializa BucketGroup agregado com todos os buckets do grupo
// Deve ser chamado após bucketId ser definido
func (i *mpxInstance) initGroupBuckets() {
	i.mu.Lock()
	defer i.mu.Unlock()
	
	numBuckets := len(request.Buckets)
	numGroups := len(i.parent.am.GetDefinedGroups())
	if numGroups == 0 {
		numGroups = 1
	}
	
	// Coleta IDs dos buckets deste grupo: bucketId, bucketId+numGroups, bucketId+2*numGroups, ...
	i.groupBucketIDs = make([]int, 0)
	for b := int(i.bucketId); b < numBuckets; b += numGroups {
		i.groupBucketIDs = append(i.groupBucketIDs, b)
	}
	
	// Cria BucketGroup agregado
	i.groupBucketGroup = request.NewBucketGroup(i.groupBucketIDs)
	fmt.Printf("[MPX][INST] sn=%d initialized group buckets: %v (total=%d)\n", i.sn, i.groupBucketIDs, len(i.groupBucketIDs))
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
	defer i.mu.Unlock()
	select {
	case <-i.stopCh:
		// Already closed
	default:
		close(i.stopCh)
		close(i.msgCh)
	}
	i.wg.Wait()
}
func (i *mpxInstance) enqueue(pm *pb.ProtocolMessage) {
	select {
	case <-i.stopCh:
		return
	case i.msgCh <- pm:
	default:
		go func() {
			select {
			case <-i.stopCh:
				return
			case i.msgCh <- pm:
			}
		}()
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
	
	// ✅ Inicializa BucketGroup agregado quando bucketId é definido pela primeira vez
	if i.groupBucketGroup == nil {
		i.mu.Unlock()
		i.initGroupBuckets()
		i.mu.Lock()
	}
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
	// ✅ FIX SIGNATURE: Envia apenas digest, não o valor completo (evita re-serialização)
	var promiseDigest []byte
	if i.acceptedValue != nil && i.acceptedValue.GetBatch() != nil {
		promiseDigest = request.BatchDigest(i.acceptedValue.GetBatch())
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
			Digest:         promiseDigest,
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
		fmt.Printf("[MPX][INST] sn=%d PREPARE from self, setting as leader\n", i.sn)
		i.leader = membership.OwnID
		if i.promisedFrom == nil {
			i.promisedFrom = make(map[int32]struct{})
		}
		if _, ok := i.promisedFrom[membership.OwnID]; !ok {
			i.promisedFrom[membership.OwnID] = struct{}{}
			i.promiseCount++
			fmt.Printf("[MPX][INST] sn=%d self-promise counted, promiseCount=%d/%d\n", i.sn, i.promiseCount, i.quorum)
			if i.promiseCount >= i.quorum && !i.prepared {
				i.prepared = true
				i.phase = phasePrepared
				fmt.Printf("[MPX][INST] sn=%d QUORUM de promises atingido, entrando em steady-state\n", i.sn)
				// ✅ Chama ProposeIfDue imediatamente ao atingir quorum
				go i.ProposeIfDue()
			}
		}
	} else {
		fmt.Printf("[MPX][INST] sn=%d sending PROMISE ballot=%d groupId=%d to leader=%d\n", i.sn, ballot, groupId, leaderID)
		if i.parent.emit != nil {
			i.parent.emit(out)
		}
	}
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
	// ✅ FIX SIGNATURE: Promise agora contém apenas digest, não valor completo
	// Líder não precisa adotar valor de promises antigas (steady-state leader)
	if len(promise.GetDigest()) > 0 {
		fmt.Printf("[MPX][INST] sn=%d promise has digest (value already accepted by follower)\n", i.sn)
	}
	fmt.Printf("[MPX][INST] sn=%d promise from=%d counted, promiseCount=%d/%d\n", i.sn, from, i.promiseCount, i.quorum)
	if i.promiseCount >= i.quorum && !i.prepared {
		i.prepared = true
		i.phase = phasePrepared
		fmt.Printf("[MPX][INST] sn=%d QUORUM de promises atingido, entrando em steady-state\n", i.sn)
		// ✅ Chama ProposeIfDue imediatamente ao atingir quorum
		go i.ProposeIfDue()
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
	}
	// ✅ FIX: Aceita apenas ballot MAIOR (não >=) para evitar split-brain
	if ballot > i.promisedBallot || i.leader == 0 {
		i.leader = from
		i.promisedBallot = ballot
		fmt.Printf("[MPX][INST] sn=%d leader set to %d (ballot=%d)\n", i.sn, from, ballot)
	} else if i.leader != from {
		fmt.Printf("[MPX][INST] sn=%d ignoring ACCEPT from=%d (leader=%d, ballot=%d < promised=%d)\n", i.sn, from, i.leader, ballot, i.promisedBallot)
		return
	}
	// ✅ FIX SIGNATURE: ACCEPT contém Batch completo (como PBFT PREPREPARE)
	batch := a.GetBatch()
	if batch == nil {
		fmt.Printf("[MPX][INST] sn=%d ACCEPT without batch (rejecting)\n", i.sn)
		return
	}
	// Armazena valor e calcula digest
	i.lastVal = &pb.MPxValue{
		Id:    &pb.MPxInstanceId{Sn: i.sn, Lead: uint64(from)},
		Batch: batch,
	}
	digestSlice := request.BatchDigest(batch)
	copy(i.lastDigest[:], digestSlice)
	fmt.Printf("[MPX][INST] sn=%d accepted batch with %d requests, digest=%x\n", i.sn, len(batch.Requests), i.lastDigest)
	// ✅ FIX: Acceptor NÃO conta votos, apenas envia ACCEPTED ao líder
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
		// ✅ FIX: Líder processa seu próprio ACCEPTED como se fosse de outro nó
		fmt.Printf("[MPX][INST] sn=%d processing self-ACCEPTED as leader\n", i.sn)
		i.mu.Unlock()
		i.onAccepted(resp, accepted.Type.(*pb.MPxMsg_Accepted).Accepted)
		i.mu.Lock()
		return
	}
	fmt.Printf("[MPX][INST] sn=%d sending ACCEPTED to leader=%d\n", i.sn, i.leader)
	if i.parent.emit != nil {
		i.parent.emit(resp)
	}
}
func (i *mpxInstance) onAccepted(pm *pb.ProtocolMessage, _ *pb.MPxAccepted) {
	i.mu.Lock()
	defer i.mu.Unlock()
	
	// ✅ FIX: pm nunca deve ser nil - sempre recebe ProtocolMessage válido
	if pm == nil {
		fmt.Printf("[MPX][INST] sn=%d ERROR: onAccepted called with nil pm\n", i.sn)
		return
	}
	
	if i.acceptedFrom == nil {
		i.acceptedFrom = make(map[int32]struct{})
	}
	
	// Verifica duplicata
	if _, ok := i.acceptedFrom[pm.SenderId]; ok {
		return
	}
	
	// Valida membership
	if len(i.members) > 0 && !i.isInGroup(pm.SenderId) {
		fmt.Printf("[MPX][INST] sn=%d vote from=%d skipped (not in group)\n", i.sn, pm.SenderId)
		return
	}
	
	// ✅ Conta voto UMA ÚNICA VEZ
	i.acceptedFrom[pm.SenderId] = struct{}{}
	i.acceptCount++
	fmt.Printf("[MPX][INST] sn=%d vote from=%d counted, acceptCount=%d/%d\n", i.sn, pm.SenderId, i.acceptCount, i.quorum)
	
	// Incorporada lógica da função duplicada ProposeIfDue() aqui
	if i.acceptCount >= i.quorum && i.lastVal != nil && i.phase != phaseCommitted {
		// ✅ FIX SIGNATURE: Envia apenas digest no COMMIT
		commit := &pb.MPxMsg{Type: &pb.MPxMsg_Commit{
			Commit: &pb.MPxCommit{
				Id:      &pb.MPxInstanceId{Sn: i.sn, Lead: uint64(membership.OwnID)},
				Digest:  i.lastDigest[:],  // Apenas digest
				GroupId: i.bucketId,
			},
		}}
		pmOut := &pb.ProtocolMessage{
			SenderId: membership.OwnID,
			Sn:       i.sn,
			Msg:      &pb.ProtocolMessage_Multipaxos{Multipaxos: commit},
		}
		if i.parent.emit != nil {
			fmt.Printf("[MPX][INST] sn=%d QUORUM reached (%d/%d), sending COMMIT (digest only)\n", i.sn, i.acceptCount, i.quorum)
			i.parent.emit(pmOut)
		}
		
		// Chama onCommit diretamente com digest
		i.mu.Unlock()
		i.onCommit(&pb.MPxCommit{Id: &pb.MPxInstanceId{Sn: i.sn}, Digest: i.lastDigest[:], GroupId: i.bucketId})
		i.mu.Lock()
	}
}
func (i *mpxInstance) onCommit(c *pb.MPxCommit) {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.phase == phaseCommitted {
		return
	}
	// ✅ FIX SIGNATURE: COMMIT agora contém apenas digest
	// Valida digest e usa Batch local
	if len(c.GetDigest()) == 0 {
		fmt.Printf("[MPX][INST] sn=%d NIL commit (no digest)\n", i.sn)
		i.phase = phaseCommitted
		i.closed = true
		tracing.MainTrace.Event(tracing.COMMIT, int64(i.sn), 0)
		return
	}
	// Valida digest recebido contra digest local
	var commitDigest [32]byte
	copy(commitDigest[:], c.GetDigest())
	if i.lastVal != nil && commitDigest != i.lastDigest {
		fmt.Printf("[MPX][INST] sn=%d commit digest mismatch: expected=%x got=%x\n", 
			i.sn, i.lastDigest, commitDigest)
		return
	}
	if i.lastVal == nil {
		// Não tem valor local - armazena digest para validação futura
		i.lastDigest = commitDigest
		fmt.Printf("[MPX][INST] sn=%d commit received but no local Batch (digest=%x)\n", i.sn, commitDigest)
	}
	i.phase = phaseCommitted
	if i.lastReqBatch != nil {
		request.RemoveBatch(i.lastReqBatch)
		i.lastReqBatch = nil
	}
	var crossOpGSN uint64
	// ✅ FIX SIGNATURE: Usa Batch local (nunca foi serializado)
	if i.lastVal == nil || i.lastVal.GetBatch() == nil {
		fmt.Printf("[MPX][INST] sn=%d onCommit no local Batch available\n", i.sn)
		return
	}
	b := i.lastVal.GetBatch()
	
	// Extrai GSN das requests se presente
	for _, req := range b.Requests {
		if len(req.TouchedGroups) > 1 && req.GSN > 0 {
			crossOpGSN = req.GSN
			fmt.Printf("[CROSS-OP] sn=%d onCommit found gsn=%d in request\n", i.sn, crossOpGSN)
			break
		}
	}
	
	// ✅ Trata NOP (batch vazio): anuncia e fecha sem processar requests
	if len(b.Requests) == 0 {
		fmt.Printf("[MPX][INST] sn=%d NOP delivered (empty batch to fill log hole)\n", i.sn)
		i.phase = phaseCommitted
		if i.announce != nil {
			i.announce(i.sn, b, i.lastDigest[:])  // ✅ Passa Batch diretamente
		}
		i.closed = true
		traceCommit(i.sn, 0)
		return
	}
	
	if i.bucketId == 0 && GetGlobalMulticastOrderer() != nil {
		for _, req := range b.Requests {
			payload := string(req.Payload)
			
			// ✅ BATCHING: Processa batch de GSN_REQUEST
			if strings.HasPrefix(payload, "SYSTEM:GSN_REQUEST:BATCH:") {
				var batchSize int
				n, _ := fmt.Sscanf(payload, "SYSTEM:GSN_REQUEST:BATCH:%d", &batchSize)
				if n < 1 || batchSize <= 0 {
					continue
				}
				
				// Extrai IDs do batch
				parts := strings.Split(payload, ":")
				if len(parts) < 4+batchSize {
					fmt.Printf("[GSN-BATCH][ERROR] Invalid batch format\n")
					continue
				}
				
				gmo := GetGlobalMulticastOrderer()
				gmo.gsnMu.Lock()
				
				// Processa cada reqID do batch
				for i := 0; i < batchSize; i++ {
					var reqID uint64
					if _, err := fmt.Sscanf(parts[4+i], "%d", &reqID); err != nil {
						continue
					}
					
					gsn := gmo.nextGSN
					gmo.nextGSN++
					
					// Extrai requester do reqID
					requester := int32(reqID >> 32)
					
					if requester == membership.OwnID {
						gmo.gsnReqMu.Lock()
						if ch, exists := gmo.gsnRequestsPending[reqID]; exists {
							ch <- gsn
							delete(gmo.gsnRequestsPending, reqID)
						}
						gmo.gsnReqMu.Unlock()
						fmt.Printf("[GSN-BATCH] GSN=%d for reqID=%d (local)\n", gsn, reqID)
					} else {
						resp := &pb.ProtocolMessage{
							SenderId: membership.OwnID,
							Sn:       -1,
							Msg: &pb.ProtocolMessage_GsnReqForward{
								GsnReqForward: &pb.GSNReqForward{
									Req: &pb.ClientRequest{
										RequestId: &pb.RequestID{
											ClientId: requester,
											ClientSn: 0,
										},
										Payload: []byte(fmt.Sprintf("SYSTEM:GSN_RESPONSE:%d:%d", reqID, gsn)),
									},
								},
							},
						}
						messenger.EnqueueMsg(resp, requester)
						fmt.Printf("[GSN-BATCH] GSN=%d for reqID=%d (remote node=%d)\n", gsn, reqID, requester)
					}
				}
				
				gmo.persistNextGSN()
				gmo.gsnMu.Unlock()
				fmt.Printf("[GSN-BATCH] Processed batch of %d GSN requests\n", batchSize)
				continue
			}
			
			// ✅ FALLBACK: Processa GSN_REQUEST individual (compatibilidade)
			if strings.HasPrefix(payload, "SYSTEM:GSN_REQUEST:") {
				var reqID uint64
				var requester int32
				n, _ := fmt.Sscanf(payload, "SYSTEM:GSN_REQUEST:%d:%d", &reqID, &requester)
				if n < 2 {
					continue
				}
				
				gmo := GetGlobalMulticastOrderer()
				gmo.gsnMu.Lock()
				gsn := gmo.nextGSN
				gmo.nextGSN++
				gmo.persistNextGSN()
				gmo.gsnMu.Unlock()
				
				if requester == membership.OwnID {
					gmo.gsnReqMu.Lock()
					if ch, exists := gmo.gsnRequestsPending[reqID]; exists {
						ch <- gsn
						delete(gmo.gsnRequestsPending, reqID)
					}
					gmo.gsnReqMu.Unlock()
					fmt.Printf("[GSN-SERVER] GSN=%d for reqID=%d (local)\n", gsn, reqID)
				} else {
					resp := &pb.ProtocolMessage{
						SenderId: membership.OwnID,
						Sn:       -1,
						Msg: &pb.ProtocolMessage_GsnReqForward{
							GsnReqForward: &pb.GSNReqForward{
								Req: &pb.ClientRequest{
									RequestId: &pb.RequestID{
										ClientId: requester,
										ClientSn: 0,
									},
									Payload: []byte(fmt.Sprintf("SYSTEM:GSN_RESPONSE:%d:%d", reqID, gsn)),
								},
							},
						},
					}
					messenger.EnqueueMsg(resp, requester)
					fmt.Printf("[GSN-SERVER] GSN=%d for reqID=%d (remote node=%d)\n", gsn, reqID, requester)
				}
			}
			
			if strings.HasPrefix(payload, "SYSTEM:META_STREAM:") {
				var gsn uint64
				n, _ := fmt.Sscanf(payload, "SYSTEM:META_STREAM:%d", &gsn)
				if n < 1 || len(req.TouchedGroups) == 0 {
					continue
				}
				GetGlobalMulticastOrderer().RegisterGSNMetadata(gsn, req.TouchedGroups)
				fmt.Printf("[META-SERVER] GSN=%d -> groups=%v\n", gsn, req.TouchedGroups)
			}
		}
		
		if i.announce != nil {
			i.announce(i.sn, b, i.lastDigest[:])  // ✅ Passa Batch diretamente
		}
		i.closed = true
		traceCommit(i.sn, len(b.Requests))
		return
	}
	
	if len(b.Requests) > 0 && len(b.Requests[0].TouchedGroups) > 0 {
		// touchedGroups = b.Requests[0].TouchedGroups // Removed unused variable
	}
	if crossOpGSN > 0 && GetGlobalMulticastOrderer() != nil {
		// Removida publicação redundante de META
		// META é publicado apenas UMA vez pelo proxy no requesthandler.go
		
		// Ponto 3: Verifica ordem sequencial antes de entregar
		// ✅ FIX SIGNATURE: Passa Batch diretamente (sem marshal) para preservar assinaturas
		if !GetGlobalMulticastOrderer().ADeliver(crossOpGSN, i.bucketId, b) {
			fmt.Printf("[GSN-ALL] sn=%d gsn=%d out of order, buffering\n", i.sn, crossOpGSN)
			// Buffer commit fora de ordem
			if i.announce != nil {
				digestBytes := i.lastDigest[:]
				i.announce(i.sn, b, digestBytes)  // ✅ Passa Batch diretamente
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
		i.announce(i.sn, b, digestBytes)  // ✅ Passa Batch diretamente
	} else {
		fmt.Printf("[MPX][INST] sn=%d announcer is nil!\n", i.sn)
	}
	i.closed = true
	traceCommit(i.sn, len(b.Requests))
}
func (i *mpxInstance) ProposeIfDue() {
	i.mu.Lock()
	defer i.mu.Unlock()
	
	if i.inProposal {
		return
	}
	i.inProposal = true
	defer func() { i.inProposal = false }()
	
	i.lastProposeAt = time.Now()
	
	if i.phase >= phaseAcceptSent && i.phase != phaseCommitted {
		return
	}
	
	if i.closed {
		return
	}
	
	// ✅ CORREÇÃO MultiPaxos "steady leader": Verifica quorum ANTES de pegar requests
	if !i.prepared {
		if i.promiseCount < i.quorum {
			fmt.Printf("[MPX][INST] sn=%d waiting quorum promises (%d/%d)\n",
				i.sn, i.promiseCount, i.quorum)
			return
		}
		i.prepared = true
		i.phase = phasePrepared
		fmt.Printf("[MPX][INST] sn=%d QUORUM atingido, entrando em steady-state\n", i.sn)
	}
	
	var val *pb.MPxValue
	reqs := 0
	
	if i.lastVal == nil {
		// ✅ FIX: Verifica se BucketGroup foi inicializado
		if i.groupBucketGroup == nil {
			fmt.Printf("[MPX][PROPOSE] sn=%d groupBucketGroup not initialized, skipping\n", i.sn)
			return
		}
		
		// ✅ NOVA LÓGICA: Usa BucketGroup agregado para cortar batch de TODOS os buckets do grupo
		rb := i.groupBucketGroup.CutBatch(i.parent.maxBatchSize, i.proposeEvery)
		
		if rb == nil || rb.Message() == nil || len(rb.Message().Requests) == 0 {
			// NOP: Preenche buraco no log sequencial
			emptyBatch := &pb.Batch{Requests: nil}
			val = &pb.MPxValue{
				Id:    &pb.MPxInstanceId{Sn: i.sn, Lead: uint64(membership.OwnID)},
				Batch: emptyBatch,  // ✅ Batch diretamente
			}
			i.lastVal = val
			// Digest calculado usando request.BatchDigest (igual ao PBFT)
			digestSlice := request.BatchDigest(emptyBatch)
			copy(i.lastDigest[:], digestSlice)
			i.lastReqBatch = nil
			reqs = 0
			fmt.Printf("[MPX][INST] sn=%d group=%d proposing NOP (no requests available)\n", i.sn, i.bucketId)
		} else {
			if !i.validateBatchHomogeneity(rb) {
				return
			}
			// ✅ FIX SIGNATURE: Chama Message() UMA ÚNICA VEZ e armazena resultado
			batchMsg := rb.Message()
			
			// Validação cross-op
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
			
			// ✅ FIX SIGNATURE: Armazena batchMsg (já criado) diretamente
			// Nunca mais chama Message() - reutiliza este Batch
			val = &pb.MPxValue{
				Id:    &pb.MPxInstanceId{Sn: i.sn, Lead: uint64(membership.OwnID)},
				Batch: batchMsg,  // ✅ Usa batchMsg criado acima
			}
			i.lastVal = val
			// Digest calculado usando request.BatchDigest (igual ao PBFT)
			digestSlice := request.BatchDigest(batchMsg)
			copy(i.lastDigest[:], digestSlice)
		}
		
		i.acceptedFrom = map[int32]struct{}{}
		i.acceptCount = 0
	} else {
		val = i.lastVal
	}
	
	// ✅ FIX SIGNATURE: Envia Batch completo no ACCEPT (como PBFT PREPREPARE)
	accept := &pb.MPxMsg{Type: &pb.MPxMsg_Accept{Accept: &pb.MPxAccept{
		Id:      &pb.MPxInstanceId{Sn: i.sn, Lead: uint64(membership.OwnID)},
		Ballot:  uint64(i.currentBallot),
		Batch:   val.GetBatch(),  // Batch completo
		GroupId: i.bucketId,
	}}}
	pm := &pb.ProtocolMessage{
		SenderId: membership.OwnID,
		Sn:       i.sn,
		Msg:      &pb.ProtocolMessage_Multipaxos{Multipaxos: accept},
	}
	if i.parent.emit != nil {
		fmt.Printf("[PROPOSE] Group %d: sn=%d ACCEPT sent with %d requests (full batch)\n", i.bucketId, i.sn, reqs)
		i.parent.emit(pm)
	}
	i.phase = phaseAcceptSent
	i.lastAcceptAt = time.Now()
	tracePropose(i.sn, reqs)
}
// tick - Gerencia timeouts e retransmissões da instância
// Implementa dois níveis de timeout:
// 1. Retransmissão de ACCEPT (acceptRtxEvery)
// 2. Nova rodada com ballot maior (acceptRtxEvery * 3 desde roundStartAt)
func (i *mpxInstance) tick(now time.Time) {
	i.mu.Lock()
	defer i.mu.Unlock()
	
	// Nível 1: Retransmissão de ACCEPT se não obteve quorum
	if i.phase == phaseAcceptSent && i.acceptCount < i.quorum && now.Sub(i.lastAcceptAt) >= i.acceptRtxEvery {
		if i.parent.emit != nil && i.lastVal != nil {
			// ✅ FIX SIGNATURE: Retransmissão envia Batch completo
			pm := &pb.ProtocolMessage{
				SenderId: membership.OwnID,
				Sn:       i.sn,
				Msg: &pb.ProtocolMessage_Multipaxos{
					Multipaxos: &pb.MPxMsg{Type: &pb.MPxMsg_Accept{
						Accept: &pb.MPxAccept{
							Id:      &pb.MPxInstanceId{Sn: i.sn, Lead: uint64(membership.OwnID)},
							Ballot:  uint64(i.currentBallot),
							Batch:   i.lastVal.GetBatch(),  // Batch completo
							GroupId: i.bucketId,
						},
					}},
				},
			}
			fmt.Printf("[MPX][INST] sn=%d resending ACCEPT (timeout, full batch)\n", i.sn)
			i.parent.emit(pm)
			i.lastAcceptAt = now // ✅ Atualiza apenas relógio de retransmissão
		}
	}
	
	// Nível 2: Nova rodada com ballot maior (usa roundStartAt, não lastAcceptAt)
	// ✅ CORREÇÃO: Usa roundStartAt para evitar que retransmissões resetem o timeout
	if i.phase == phaseAcceptSent && i.acceptCount < i.quorum && now.Sub(i.roundStartAt) >= i.acceptRtxEvery*3 {
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
		i.roundStartAt = now // ✅ Reseta início da nova rodada
		
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
	fmt.Printf("[TRACE] PROPOSE sn=%d size=%d\n", sn, size)
	tracing.MainTrace.Event(tracing.PROPOSE, int64(sn), int64(size))
}

// traceCommit - Registra evento de commit no sistema de tracing
func traceCommit(sn int32, size int) {
	fmt.Printf("[TRACE] COMMIT sn=%d size=%d\n", sn, size)
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
	if i.bucketId == 0 && firstGroupId != 0 {
		fmt.Printf("[MPX][INST] sn=%d bucket 0 rejecting groupId=%d (must be 0), returning requests\n", i.sn, firstGroupId)
		batch.Resurrect()
		return false
	}
	
	return true
}
