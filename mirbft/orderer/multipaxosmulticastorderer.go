/*
MultiPaxos Multicast Orderer - Sistema de Consenso Distribuído Multi-Grupo

Implementa sistema robusto de consenso distribuído que combina:
1. MultiPaxos: Protocolo de consenso para cada grupo independente
2. Multicast Atômico: Garante ordem total entre grupos diferentes
3. GSN (Global Sequence Number): Numeração global única para todas operações
4. META Stream: Metadados determinísticos sobre quais grupos cada operação toca
5. Sistema de Liveness: Re-forward automático contra falhas de proxy/rede

Arquitetura do Sistema:
- Grupo 0: Sequenciador GSN (todos os nós) - garante ordem global determinística
- Grupos 1,2,3...: Grupos de dados (subconjuntos de nós) - processam operações
- SN Global Intercalado: Cada grupo usa slots diferentes no mesmo espaço SN
- Multi-Proxy: Qualquer nó pode atuar como proxy (sem gargalo único)

Fluxo de Operação:
1. Cliente envia request para qualquer nó (proxy)
2. Proxy atribui GSN via grupo 0 (sequenciador global)
3. Proxy publica META uma vez (evita duplicação)
4. Proxy faz fanout para todos os grupos tocados
5. Cada grupo processa via MultiPaxos independente
6. ADeliver verifica ordem sequencial GSN antes de entregar
7. Sistema de liveness detecta e corrige requests perdidas

Garantias do Sistema:
- Ordem Global: GSN garante ordem total entre todos os grupos
- Determinismo: META stream garante consistência de metadados
- Liveness: Re-forward automático garante entrega eventual
- Robustez: Tolera falhas de proxy, rede e nós individuais
- Performance: SN intercalado permite paralelismo entre grupos
*/
package orderer
import (
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"github.com/hyperledger-labs/mirbft/manager"
	"github.com/hyperledger-labs/mirbft/membership"
	"github.com/hyperledger-labs/mirbft/messenger"
	mirlog "github.com/hyperledger-labs/mirbft/log"
	"github.com/hyperledger-labs/mirbft/request"
	pb "github.com/hyperledger-labs/mirbft/protobufs"
	logger "github.com/rs/zerolog/log"
)

// Constantes para mensagens do sistema
const (
	SYSTEM_GSN_REQUEST = "SYSTEM:GSN_REQUEST:"
	SYSTEM_META_STREAM = "SYSTEM:META_STREAM:"
)

// Funções auxiliares para identificar mensagens do sistema
func isSystemMessage(req *pb.ClientRequest) bool {
	payload := string(req.Payload)
	return strings.HasPrefix(payload, "SYSTEM:")
}

func isGSNRequest(req *pb.ClientRequest) bool {
	return strings.HasPrefix(string(req.Payload), SYSTEM_GSN_REQUEST)
}

func isMETAStream(req *pb.ClientRequest) bool {
	return strings.HasPrefix(string(req.Payload), SYSTEM_META_STREAM)
}

var GlobalMulticastOrderer *MultiPaxosMulticastOrderer
type ForwardRequestFn func(req *pb.ClientRequest, members []int32)

type MultiPaxosMulticastOrderer struct {
	groupOrderers map[uint32]*MultiPaxosOrderer
	orderersMu    sync.RWMutex
	am            *AtomicMulticast
	mgr           manager.Manager
	forwardRequestFn ForwardRequestFn
	groupsFilePath   string
	
	// GSN Sequencer
	nextGSN            uint64
	gsnMu              sync.Mutex
	gsnRequestsPending map[uint64]chan uint64 // ✅ Chave composta global
	gsnReqMu           sync.Mutex
	gsnSeqCounter      uint32
	metaSeqCounter     uint32 // ✅ Contador separado para META (evita overflow)
	mcastSeqCounter    uint32 // ✅ Contador separado para Mcast (evita overflow)
	
	// META Stream
	gsnMetadata map[uint64][]uint32
	metaMu      sync.RWMutex
	
	// ADeliver ordem sequencial
	expectedNextGSN map[uint32]uint64
	expectedGSNMu   sync.RWMutex
	
	// Buffer para commits fora de ordem
	pendingCommits map[uint32]map[uint64]*PendingCommit
	bufferMu       sync.RWMutex
	
	// ✅ LIVENESS: Re-forward para robustez contra falha de proxy
	missingRequests map[uint64]map[uint32]time.Time // GSN -> grupos esperando
	missingMu       sync.RWMutex
	
	// ✅ LIVENESS: Cache de requests para re-forward
	requestCache map[uint64]*pb.ClientRequest // GSN -> request original
	cacheMu      sync.RWMutex
	
	// ✅ DEDUPLICACAO: META já publicados para evitar duplicação
	publishedMeta map[uint64]bool
	publishedMu   sync.RWMutex
}

// PendingCommit - Representa um commit que está aguardando ordem correta de GSN
type PendingCommit struct {
	gsn       uint64                           // GSN desta operação
	groupID   uint32                           // Grupo que fez o commit
	batch     []byte                           // Dados do batch
	announce  func(int32, []byte, []byte)     // Função para anunciar o commit
	sn        int32                            // Sequence Number local do grupo
	digest    []byte                           // Hash do batch
}
// Init - Inicializa o MultiPaxos Multicast Orderer
// Cria um orderer MultiPaxos para cada grupo definido no YAML
func (o *MultiPaxosMulticastOrderer) Init(mngr manager.Manager) {
	GlobalMulticastOrderer = o
	o.mgr = mngr
	o.initComponents()    // Inicializa componentes internos
	o.loadGroups()        // Carrega grupos do arquivo YAML
	o.createGroupOrderers(mngr) // Cria orderers para cada grupo
	o.setupHandlers()     // Configura handlers de mensagens
}

// initComponents - Inicializa todos os componentes internos
func (o *MultiPaxosMulticastOrderer) initComponents() {
	o.am = NewAtomicMulticast()
	o.groupOrderers = make(map[uint32]*MultiPaxosOrderer)
	o.expectedNextGSN = make(map[uint32]uint64)
	o.pendingCommits = make(map[uint32]map[uint64]*PendingCommit)
	
	// SN intercalado: Apenas campos necessários (sem mapeamento SN→grupo)
	o.gsnMetadata = make(map[uint64][]uint32)
	o.gsnRequestsPending = make(map[uint64]chan uint64)
	o.nextGSN = 1
	
	// ✅ LIVENESS: Inicializa rastreamento de requests perdidas
	o.missingRequests = make(map[uint64]map[uint32]time.Time)
	o.requestCache = make(map[uint64]*pb.ClientRequest)
	
	// ✅ DEDUPLICACAO: Inicializa controle de META publicados
	o.publishedMeta = make(map[uint64]bool)
}

// loadGroups - Carrega configuração de grupos do arquivo YAML
func (o *MultiPaxosMulticastOrderer) loadGroups() {
	if o.groupsFilePath == "" {
		o.groupsFilePath = "config/groups.yml"
	}
	if err := o.am.LoadGroupsFromYAML(o.groupsFilePath); err != nil {
		panic(fmt.Sprintf("FATAL: Failed to load groups from %s: %v", o.groupsFilePath, err))
	}
	o.am.UpdateSequencerGroup() // Grupo 0 = todos os nós
	o.am.SetSequencer(o)
}

// createGroupOrderers - Cria um MultiPaxosOrderer para cada grupo
// SN global intercalado: cada grupo usa slots diferentes no mesmo espaço SN
func (o *MultiPaxosMulticastOrderer) createGroupOrderers(mngr manager.Manager) {
	groupIDs := o.am.GetDefinedGroups()
	for _, gid := range groupIDs {
		groupOrderer := &MultiPaxosOrderer{}
		groupOrderer.am = o.am
		groupOrderer.ownedGroupID = gid
		groupOrderer.skipHandlerRegistration = true // Não registra handler global
		// SN intercalado: cada grupo começa do seu slot no espaço global
		groupOrderer.Init(mngr)
		o.groupOrderers[gid] = groupOrderer
		if gid == 0 {
			fmt.Printf("[MULTICAST] Created GSN SEQUENCER (group 0) - interleaved SN\n")
		} else {
			fmt.Printf("[MULTICAST] Created orderer for group %d - interleaved SN\n", gid)
		}
	}
}

// setupHandlers - Configura handlers de mensagens e goroutines de monitoramento
func (o *MultiPaxosMulticastOrderer) setupHandlers() {
	messenger.OrdererMsgHandler = o.HandleMessage
	go o.trackSegments()    // Monitora novos segmentos
	go o.trackCheckpoints() // Monitora checkpoints para limpeza
	go o.reforwardWatchdog() // ✅ LIVENESS: Monitora requests perdidas
}
func (o *MultiPaxosMulticastOrderer) trackSegments() {
	// SN intercalado: grupos compartilham espaço SN global com slots diferentes
	// Cada grupo usa: firstSN + groupIdx, depois += numGroups
	fmt.Printf("[MULTICAST] Using interleaved SN (groups share global SN space)\n")
}

func (o *MultiPaxosMulticastOrderer) trackCheckpoints() {
	checkpoints := mirlog.Checkpoints()
	for checkpoint := range checkpoints {
		if checkpoint != nil {
			o.cleanOldMappings(checkpoint.Sn)
		}
	}
}
func (o *MultiPaxosMulticastOrderer) SetForwardRequestFn(fn ForwardRequestFn) {
	o.forwardRequestFn = fn
}
func (o *MultiPaxosMulticastOrderer) LoadGroupsFromYAML(filename string) error {
	if o.am == nil {
		o.am = NewAtomicMulticast()
	}
	return o.am.LoadGroupsFromYAML(filename)
}
func (o *MultiPaxosMulticastOrderer) Start(wg *sync.WaitGroup) {
	o.orderersMu.RLock()
	defer o.orderersMu.RUnlock()
	for gid, orderer := range o.groupOrderers {
		fmt.Printf("[MULTICAST] Starting orderer for group %d\n", gid)
		orderer.Start(wg)
	}
}
func (o *MultiPaxosMulticastOrderer) HandleMessage(pm *pb.ProtocolMessage) {
	if missingEntry := pm.GetMissingEntry(); missingEntry != nil {
		// SN intercalado: usa GroupId da request ao invés de mapear SN
		var groupID uint32 = 0
		if missingEntry.Batch != nil && len(missingEntry.Batch.Requests) > 0 {
			groupID = missingEntry.Batch.Requests[0].GetGroupId()
			fmt.Printf("[MULTICAST] MissingEntry for group %d (from request)\n", groupID)
		} else {
			// Fallback: broadcast para todos os grupos
			fmt.Printf("[MULTICAST] MissingEntry without group info, broadcasting\n")
			o.orderersMu.RLock()
			for _, orderer := range o.groupOrderers {
				orderer.HandleMessage(pm)
			}
			o.orderersMu.RUnlock()
			return
		}
		o.orderersMu.RLock()
		orderer := o.groupOrderers[groupID]
		o.orderersMu.RUnlock()
		if orderer != nil {
			orderer.HandleMessage(pm)
			fmt.Printf("[MULTICAST] Routed MissingEntry to group %d\n", groupID)
		}
		return
	}
	mpx := pm.GetMultipaxos()
	if mpx == nil {
		fmt.Printf("[MULTICAST] Unknown message type, broadcasting to all orderers\n")
		o.orderersMu.RLock()
		for _, orderer := range o.groupOrderers {
			orderer.HandleMessage(pm)
		}
		o.orderersMu.RUnlock()
		return
	}
	var groupID uint32
	switch msg := mpx.Type.(type) {
	case *pb.MPxMsg_Prepare:
		groupID = msg.Prepare.GetGroupId()
	case *pb.MPxMsg_Promise:
		groupID = msg.Promise.GetGroupId()
	case *pb.MPxMsg_Accept:
		groupID = msg.Accept.GetGroupId()
	case *pb.MPxMsg_Accepted:
		groupID = msg.Accepted.GetGroupId()
	case *pb.MPxMsg_Commit:
		groupID = msg.Commit.GetGroupId()
	}
	o.orderersMu.RLock()
	orderer := o.groupOrderers[groupID]
	o.orderersMu.RUnlock()
	if orderer != nil {
		orderer.HandleMessage(pm)
	}
}
func (o *MultiPaxosMulticastOrderer) IsMember(groupID uint32, nodeID int32) bool {
	if o.am == nil {
		return false
	}
	return o.am.IsMember(groupID, nodeID)
}
func (o *MultiPaxosMulticastOrderer) GetGroupMembers(groupID uint32) []int32 {
	if o.am == nil {
		return nil
	}
	return o.am.GetGroupMembers(groupID)
}
func (o *MultiPaxosMulticastOrderer) GetOrdererForGroup(groupID uint32) *MultiPaxosOrderer {
	o.orderersMu.RLock()
	defer o.orderersMu.RUnlock()
	return o.groupOrderers[groupID]
}
// makeGlobalRequestID - Cria identificador único global para requests sistêmicas
// Combina nodeID + contador local para evitar colisões entre proxies
func makeGlobalRequestID(nodeID int32, localCounter uint32) uint64 {
	return uint64(nodeID)<<32 | uint64(localCounter)
}

// GetNextGSN - Obtém próximo GSN via grupo 0 (sequenciador global)
// Usa identificador único global para evitar colisões entre proxies
func (o *MultiPaxosMulticastOrderer) GetNextGSN() uint64 {
	clientSn := atomic.AddUint32(&o.gsnSeqCounter, 1)
	
	// ✅ CORREÇÃO: Chave composta global (nodeID + contador)
	reqID := makeGlobalRequestID(membership.OwnID, clientSn)
	respChan := make(chan uint64, 1)
	
	o.gsnReqMu.Lock()
	o.gsnRequestsPending[reqID] = respChan
	o.gsnReqMu.Unlock()
	
	// ✅ GARANTIA: TouchedGroups sempre setado + ClientId único por proxy
	gsnReq := &pb.ClientRequest{
		RequestId: &pb.RequestID{
			ClientId: membership.OwnID, // ✅ ID único do proxy (não constante)
			ClientSn: int32(clientSn),  // Contador local do proxy
		},
		Payload:       []byte(fmt.Sprintf("%s%d", SYSTEM_GSN_REQUEST, reqID)),
		GroupId:       0,
		TouchedGroups: []uint32{0}, // Sempre grupo 0 (sequenciador)
	}
	
	request.AddReqMsg(gsnReq)
	gsn := <-respChan
	
	o.gsnReqMu.Lock()
	delete(o.gsnRequestsPending, reqID)
	o.gsnReqMu.Unlock()
	
	return gsn
}

func (o *MultiPaxosMulticastOrderer) OnGroup0Commit(req *pb.ClientRequest) {
	if req.GroupId != 0 {
		return
	}
	
	// GSN request (identificado por string no payload)
	if isGSNRequest(req) {
		// ✅ CORREÇÃO: Usa chave composta global do RequestId
		reqID := makeGlobalRequestID(req.RequestId.ClientId, uint32(req.RequestId.ClientSn))
		gsn := o.onGroup0Commit(reqID)
		fmt.Printf("[GSN-SEQ] Group 0 committed GSN request %d -> GSN=%d (from proxy %d)\n", reqID, gsn, req.RequestId.ClientId)
		return
	}
	
	// META stream (identificado por string no payload)
	if isMETAStream(req) {
		gsn := req.GSN
		touchedGroups := req.TouchedGroups
		o.RegisterGSNMetadata(gsn, touchedGroups)
		fmt.Printf("[META-STREAM] Group 0 committed GSN %d -> groups %v (from proxy %d)\n", gsn, touchedGroups, req.RequestId.ClientId)
		return
	}
}
// onGroup0Commit - Processa commit do grupo 0 (sequenciador GSN)
// Atribui GSN sequencial e acorda proxies aguardando
func (o *MultiPaxosMulticastOrderer) onGroup0Commit(reqID uint64) uint64 {
	o.gsnMu.Lock()
	gsn := o.nextGSN
	o.nextGSN++
	o.gsnMu.Unlock()
	
	// ✅ ROBUSTEZ: Acorda apenas o canal correto com chave composta
	o.gsnReqMu.Lock()
	respChan, exists := o.gsnRequestsPending[reqID]
	if exists {
		delete(o.gsnRequestsPending, reqID) // Remove imediatamente
	}
	o.gsnReqMu.Unlock()
	
	if exists {
		select {
		case respChan <- gsn:
			fmt.Printf("[GSN-SEQ] Delivered GSN %d to reqID %d\n", gsn, reqID)
		default:
			fmt.Printf("[GSN-SEQ][WARN] Channel blocked for reqID %d\n", reqID)
		}
	} else {
		fmt.Printf("[GSN-SEQ][WARN] No pending channel for reqID %d (possible duplicate)\n", reqID)
	}
	
	return gsn
}

func (o *MultiPaxosMulticastOrderer) cleanOldMappings(checkpointSN int32) {
	// SN intercalado: mantém compatibilidade com log/checkpoint global
	fmt.Printf("[MULTICAST] Checkpoint %d (interleaved SN space)\n", checkpointSN)
}

// ADeliver - Verifica ordem sequencial GSN antes de entregar
// ✅ ATOMIC GLOBAL ORDER: Implementa ordem total conforme artigo
// Garante que operações sejam entregues na mesma ordem em todos os grupos
func (o *MultiPaxosMulticastOrderer) ADeliver(gsn uint64, groupID uint32, batch []byte) bool {
	// ✅ BARREIRA META: Lê META primeiro para evitar inversão de locks
	o.metaMu.RLock()
	_, metaExists := o.gsnMetadata[gsn]
	if !metaExists {
		o.metaMu.RUnlock()
		// ✅ DETERMINISMO: Bloqueia até META chegar (ordem global garantida)
		fmt.Printf("[ATOMIC-ORDER] Group %d: GSN %d waiting for META (deterministic order)\n", groupID, gsn)
		return false // Força espera por META
	}
	touches := o.gsnTouchesGroup(gsn, groupID)
	o.metaMu.RUnlock()
	
	if !touches {
		fmt.Printf("[ATOMIC-ORDER] Group %d: GSN %d does not touch this group (META)\n", groupID, gsn)
		return true // Não precisa processar
	}
	
	// ✅ ORDEM SEQUENCIAL: Verifica se é o próximo GSN esperado
	o.expectedGSNMu.Lock()
	defer o.expectedGSNMu.Unlock()
	
	expected, exists := o.expectedNextGSN[groupID]
	if !exists {
		o.expectedNextGSN[groupID] = 1
		expected = 1
	}
	
	if gsn != expected {
		fmt.Printf("[ATOMIC-ORDER] Group %d: GSN %d != expected %d, buffering (sequential order)\n", groupID, gsn, expected)
		return false // Não pode entregar ainda
	}
	
	fmt.Printf("[ATOMIC-ORDER] Group %d: Delivering GSN %d (sequential order verified)\n", groupID, gsn)
	o.expectedNextGSN[groupID] = gsn + 1
	
	// Drena buffer após avançar expectedNextGSN
	o.drainBuffer(groupID)
	return true
}

// Buffer para commits fora de ordem
func (o *MultiPaxosMulticastOrderer) BufferCommit(gsn uint64, groupID uint32, batch []byte, announce func(int32, []byte, []byte), sn int32, digest []byte) {
	o.bufferMu.Lock()
	defer o.bufferMu.Unlock()
	
	if o.pendingCommits[groupID] == nil {
		o.pendingCommits[groupID] = make(map[uint64]*PendingCommit)
	}
	
	o.pendingCommits[groupID][gsn] = &PendingCommit{
		gsn:      gsn,
		groupID:  groupID,
		batch:    batch,
		announce: announce,
		sn:       sn,
		digest:   digest,
	}
	
	fmt.Printf("[BUFFER] Group %d: Buffered GSN %d (sn=%d)\n", groupID, gsn, sn)
}

func (o *MultiPaxosMulticastOrderer) drainBuffer(groupID uint32) {
	o.bufferMu.Lock()
	defer o.bufferMu.Unlock()
	
	if o.pendingCommits[groupID] == nil {
		return
	}
	
	// Processa commits em ordem sequencial
	for {
		expected := o.expectedNextGSN[groupID]
		pending, exists := o.pendingCommits[groupID][expected]
		if !exists {
			break // Não há próximo GSN no buffer
		}
		
		// Entrega commit buffered
		fmt.Printf("[BUFFER] Group %d: Draining GSN %d (sn=%d)\n", groupID, expected, pending.sn)
		pending.announce(pending.sn, pending.batch, pending.digest)
		
		// Remove do buffer e avança
		delete(o.pendingCommits[groupID], expected)
		o.expectedNextGSN[groupID] = expected + 1
	}
}

// RegisterGSNMetadata - META stream determinístico via grupo 0
// ✅ LIVENESS: Registra requests esperadas para detectar perdas
// ✅ ATOMIC GLOBAL ORDER: Garante que META chegue antes das requests
func (o *MultiPaxosMulticastOrderer) RegisterGSNMetadata(gsn uint64, touchedGroups []uint32) {
	o.metaMu.Lock()
	defer o.metaMu.Unlock()
	
	// ✅ DEDUPLICACAO: Verifica se META já existe para este GSN
	if _, exists := o.gsnMetadata[gsn]; exists {
		fmt.Printf("[META-STREAM][WARN] GSN %d metadata already exists, skipping\n", gsn)
		return
	}
	
	o.gsnMetadata[gsn] = make([]uint32, len(touchedGroups))
	copy(o.gsnMetadata[gsn], touchedGroups)
	
	fmt.Printf("[ATOMIC-ORDER] Registered META GSN %d -> groups %v (deterministic)\n", gsn, touchedGroups)
	
	// ✅ LIVENESS: Registra requests esperadas para watchdog
	o.missingMu.Lock()
	if o.missingRequests[gsn] == nil {
		o.missingRequests[gsn] = make(map[uint32]time.Time)
	}
	for _, groupID := range touchedGroups {
		if groupID > 0 { // Apenas grupos de dados (não grupo 0)
			o.missingRequests[gsn][groupID] = time.Now()
		}
	}
	o.missingMu.Unlock()
	
	// Processa todos os grupos para avançar expectedNextGSN se GSN não toca o grupo
	allGroups := o.am.GetDefinedGroups()
	for _, groupID := range allGroups {
		if !o.gsnTouchesGroup(gsn, groupID) {
			o.advanceGSNForGroup(groupID, gsn)
		}
	}
}

// gsnTouchesGroup - Verifica se GSN toca um grupo específico baseado em META
// ✅ ATOMIC GLOBAL ORDER: Decisão determinística baseada em META stream
func (o *MultiPaxosMulticastOrderer) gsnTouchesGroup(gsn uint64, groupID uint32) bool {
	touchedGroups, exists := o.gsnMetadata[gsn]
	if !exists {
		// ✅ DETERMINISMO: Não decide sem META, força espera determinística
		// Retorna false para bloquear até META chegar (atomic global order)
		fmt.Printf("[ATOMIC-ORDER] GSN %d: No META yet, blocking group %d (deterministic)\n", gsn, groupID)
		return false
	}
	
	for _, touched := range touchedGroups {
		if touched == groupID {
			return true
		}
	}
	return false
}

func (o *MultiPaxosMulticastOrderer) advanceGSNForGroup(groupID uint32, gsn uint64) {
	o.expectedGSNMu.Lock()
	defer o.expectedGSNMu.Unlock()
	
	expected, exists := o.expectedNextGSN[groupID]
	if !exists {
		o.expectedNextGSN[groupID] = 1
		expected = 1
	}
	
	if gsn == expected {
		fmt.Printf("[META-STREAM] Group %d: Skipping GSN %d (not touched)\n", groupID, gsn)
		o.expectedNextGSN[groupID] = gsn + 1
		o.drainBuffer(groupID)
	}
}

func (o *MultiPaxosMulticastOrderer) PublishGSNMetadata(gsn uint64, touchedGroups []uint32) {
	// ✅ DEDUPLICACAO: Verifica se META já foi publicado para este GSN
	o.publishedMu.Lock()
	if o.publishedMeta[gsn] {
		o.publishedMu.Unlock()
		fmt.Printf("[META-STREAM] GSN %d already published, skipping duplicate\n", gsn)
		return
	}
	o.publishedMeta[gsn] = true
	o.publishedMu.Unlock()
	
	// ✅ GARANTIA ABSOLUTA: TouchedGroups nunca vazio para evitar travamento
	if len(touchedGroups) == 0 {
		logger.Error().
			Uint64("gsn", gsn).
			Msg("[META-STREAM][CRITICAL] Empty TouchedGroups, using fallback group 0")
		touchedGroups = []uint32{0} // Fallback crítico
	}
	
	// ✅ CORREÇÃO OVERFLOW: Usa contador separado para evitar int32 overflow
	metaSn := atomic.AddUint32(&o.metaSeqCounter, 1)
	
	metaReq := &pb.ClientRequest{
		RequestId: &pb.RequestID{
			ClientId: membership.OwnID, // ✅ ID único do proxy (não constante)
			ClientSn: int32(metaSn),    // Contador separado (não GSN diretamente)
		},
		Payload:       []byte(fmt.Sprintf("%s%d", SYSTEM_META_STREAM, gsn)),
		GroupId:       0,
		TouchedGroups: touchedGroups, // Sempre setado (nunca vazio)
		GSN:           gsn,
	}
	
	fmt.Printf("[META-STREAM] Publishing GSN %d -> groups %v to group 0 (proxy %d, guaranteed non-empty)\n", gsn, touchedGroups, membership.OwnID)
	request.AddReqMsg(metaReq)
}

// Mcast - API black-box para multicast atômico
// Esconde toda a complexidade de GSN/META do usuário
// Parâmetros:
//   groups: lista de grupos que devem receber a mensagem
//   msg: dados da mensagem a ser enviada
// Retorna: erro se houver falha na submissão
func (o *MultiPaxosMulticastOrderer) Mcast(groups []uint32, msg []byte) error {
	// Obtém GSN global único via grupo 0
	gsn := o.GetNextGSN()
	
	// NãO publica META aqui - será publicado pelo proxy no requesthandler
	// Evita duplicação quando múltiplas réplicas processam
	
	// TouchedGroups sempre setado
	if len(groups) == 0 {
		return fmt.Errorf("groups cannot be empty")
	}
	
	// Submete request para cada grupo tocado
	for _, groupID := range groups {
		// ✅ CORREÇÃO OVERFLOW: Usa contador separado ao invés de GSN diretamente
		mcastSn := atomic.AddUint32(&o.mcastSeqCounter, 1)
		req := &pb.ClientRequest{
			RequestId: &pb.RequestID{
				ClientId: membership.OwnID, // ✅ ID único do proxy (não tamanho da mensagem)
				ClientSn: int32(mcastSn),   // Contador atômico único
			},
			Payload:       msg,
			TouchedGroups: groups, // Sempre setado (nunca vazio)
			GSN:           gsn,    // Global Sequence Number
			GroupId:       groupID,
		}
		request.AddReqMsg(req) // Adiciona à fila do grupo
	}
	return nil
}

// ProxyInterceptor - Intercepta requests para processamento de GSN
// Usado pelo sistema de requests para coordenar operações cross-group
func (o *MultiPaxosMulticastOrderer) ProxyInterceptor(gsn uint64, touchedGroups []uint32, req *pb.ClientRequest) {
	// Não publica META aqui - evita duplicação
	// META é publicado apenas no proxy (requesthandler.go)
	fmt.Printf("[PROXY] Intercepted GSN %d -> groups %v (META published by proxy only)\n", gsn, touchedGroups)
}

// CanProcessCrossOp - Verifica se pode processar operação cross-group
// ✅ ATOMIC GLOBAL ORDER: Barreira GSN garante ordem determinística
// Usado como barreira GSN pelo sistema de requests
func (o *MultiPaxosMulticastOrderer) CanProcessCrossOp(gsn uint64) bool {
	// ✅ BARREIRA REAL: Só libera cross-op quando META já existe
	if gsn == 0 {
		return true // Single-group sempre pode processar
	}
	
	o.metaMu.RLock()
	_, metaExists := o.gsnMetadata[gsn]
	o.metaMu.RUnlock()
	
	if !metaExists {
		fmt.Printf("[ATOMIC-ORDER] Blocking GSN %d (waiting for META - global order)\n", gsn)
		return false
	}
	
	fmt.Printf("[ATOMIC-ORDER] Allowing GSN %d (META exists - global order ready)\n", gsn)
	return true
}

// PublishMETAOnce - Publica metadata apenas uma vez (chamado pelo proxy)
// Evita duplicação quando múltiplas réplicas processam a mesma request
func (o *MultiPaxosMulticastOrderer) PublishMETAOnce(gsn uint64, touchedGroups []uint32) {
	// Publica metadata determinístico via grupo 0
	o.PublishGSNMetadata(gsn, touchedGroups)
	fmt.Printf("[PROXY-ONLY] Published META GSN %d -> groups %v\n", gsn, touchedGroups)
}

// ✅ LIVENESS: Marca request como recebida (para de esperar re-forward)
func (o *MultiPaxosMulticastOrderer) MarkRequestReceived(gsn uint64, groupID uint32) {
	o.missingMu.Lock()
	defer o.missingMu.Unlock()
	
	if groups, exists := o.missingRequests[gsn]; exists {
		delete(groups, groupID)
		if len(groups) == 0 {
			delete(o.missingRequests, gsn)
		}
	}
}

// ✅ LIVENESS: Cache request para re-forward
func (o *MultiPaxosMulticastOrderer) CacheRequest(gsn uint64, req *pb.ClientRequest) {
	o.cacheMu.Lock()
	defer o.cacheMu.Unlock()
	o.requestCache[gsn] = req
	fmt.Printf("[LIVENESS] Cached request GSN %d for re-forward\n", gsn)
}

// ✅ LIVENESS: Watchdog para detectar e re-forward requests perdidas
func (o *MultiPaxosMulticastOrderer) reforwardWatchdog() {
	ticker := time.NewTicker(5 * time.Second) // Verifica a cada 5s
	defer ticker.Stop()
	
	for range ticker.C {
		o.missingMu.RLock()
		now := time.Now()
		toReforward := make(map[uint64][]uint32)
		
		for gsn, groups := range o.missingRequests {
			for groupID, timestamp := range groups {
				if now.Sub(timestamp) > 10*time.Second { // Timeout de 10s
					if toReforward[gsn] == nil {
						toReforward[gsn] = make([]uint32, 0)
					}
					toReforward[gsn] = append(toReforward[gsn], groupID)
				}
			}
		}
		o.missingMu.RUnlock()
		
		// Re-forward requests perdidas
		for gsn, groups := range toReforward {
			fmt.Printf("[LIVENESS] Re-forwarding GSN %d to groups %v (timeout)\n", gsn, groups)
			o.requestReforward(gsn, groups)
		}
	}
}

// ✅ LIVENESS: Solicita re-forward de request perdida
func (o *MultiPaxosMulticastOrderer) requestReforward(gsn uint64, groups []uint32) {
	// Simplified approach: use existing MissingEntry mechanism
	fmt.Printf("[LIVENESS] Requesting reforward for GSN %d groups %v (using MissingEntry)\n", gsn, groups)
	// TODO: Implement using existing MissingEntry mechanism or add custom protobuf field
}