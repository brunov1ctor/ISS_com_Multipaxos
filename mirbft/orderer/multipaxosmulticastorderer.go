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
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"github.com/hyperledger-labs/mirbft/config"
	"github.com/hyperledger-labs/mirbft/crypto"
	"github.com/hyperledger-labs/mirbft/log"
	"github.com/hyperledger-labs/mirbft/manager"
	"github.com/hyperledger-labs/mirbft/membership"
	"github.com/hyperledger-labs/mirbft/messenger"
	mirlog "github.com/hyperledger-labs/mirbft/log"
	"github.com/hyperledger-labs/mirbft/request"
	pb "github.com/hyperledger-labs/mirbft/protobufs"
	logger "github.com/rs/zerolog/log"
)

const gsnStateFile = "/tmp/iss-Bruno/next_gsn.state"

// Constantes para mensagens do sistema
const (
	SYSTEM_META_STREAM = "SYSTEM:META_STREAM:"
	SYSTEM_GSN_REQUEST = "SYSTEM:GSN_REQUEST:"
)

// Funções auxiliares para identificar mensagens do sistema
func isMETAStream(req *pb.ClientRequest) bool {
	return strings.HasPrefix(string(req.Payload), SYSTEM_META_STREAM)
}

var globalMulticastOrderer *MultiPaxosMulticastOrderer

func GetGlobalMulticastOrderer() *MultiPaxosMulticastOrderer {
	return globalMulticastOrderer
}
type MultiPaxosMulticastOrderer struct {
	groupOrderers map[uint32]*MultiPaxosOrderer
	orderersMu    sync.RWMutex
	am            *AtomicMulticast
	mgr           manager.Manager
	groupsFilePath   string
	
	// GSN Sequencer
	nextGSN            uint64
	gsnMu              sync.Mutex
	gsnRequestsPending map[uint64]chan uint64
	gsnReqMu           sync.Mutex
	gsnSeqCounter      uint32
	metaSeqCounter     uint32
	mcastSeqCounter    uint32
	
	// Deduplicação de GSN requests
	seenGSNReq map[uint64]bool
	seenGSNMu  sync.Mutex
	
	// META Stream
	gsnMetadata map[uint64][]uint32
	metaMu      sync.RWMutex
	
	// ADeliver ordem sequencial
	lastDeliveredGSN map[uint32]uint64
	expectedGSNMu    sync.RWMutex
	
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
	globalMulticastOrderer = o
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
	o.lastDeliveredGSN = make(map[uint32]uint64)
	o.pendingCommits = make(map[uint32]map[uint64]*PendingCommit)
	
	// SN intercalado: Apenas campos necessários (sem mapeamento SN→grupo)
	o.gsnMetadata = make(map[uint64][]uint32)
	o.gsnRequestsPending = make(map[uint64]chan uint64)
	// ✅ RECUPERAÇÃO: nextGSN será reconstruído do log após Init()
	o.nextGSN = 1
	
	// ✅ LIVENESS: Inicializa rastreamento de requests perdidas
	o.missingRequests = make(map[uint64]map[uint32]time.Time)
	o.requestCache = make(map[uint64]*pb.ClientRequest)
	
	// ✅ DEDUPLICACAO: Inicializa controle de META publicados
	o.publishedMeta = make(map[uint64]bool)
	
	// Deduplicação de GSN requests no líder
	o.seenGSNReq = make(map[uint64]bool)
}

// loadGroups - Carrega configuração de grupos do arquivo YAML
func (o *MultiPaxosMulticastOrderer) loadGroups() {
	if o.groupsFilePath == "" {
		o.groupsFilePath = "/tmp/iss-Bruno/config/groups.yml"
	}
	if err := o.am.LoadGroupsFromYAML(o.groupsFilePath); err != nil {
		logger.Fatal().Err(err).Str("file", o.groupsFilePath).Msg("Failed to load groups configuration")
	}
	o.am.UpdateSequencerGroup() // Grupo 0 = todos os nós
	o.am.SetSequencer(o)
}

// createGroupOrderers - Cria um MultiPaxosOrderer para cada grupo
// SN global intercalado: cada grupo usa slots diferentes no mesmo espaço SN
// IMPORTANTE: Grupo 0 é apenas para GSN/META, não processa requisições de dados
func (o *MultiPaxosMulticastOrderer) createGroupOrderers(mngr manager.Manager) {
	groupIDs := o.am.GetDefinedGroups()
	for _, gid := range groupIDs {
		groupOrderer := &MultiPaxosOrderer{}
		groupOrderer.am = o.am
		groupOrderer.ownedGroupID = gid // ✅ CRITICAL FIX: Each orderer owns its own group
		groupOrderer.skipHandlerRegistration = true // Não registra handler global
		// SN intercalado: cada grupo começa do seu slot no espaço global
		groupOrderer.Init(mngr)
		o.groupOrderers[gid] = groupOrderer
		if gid == 0 {
			fmt.Printf("[MULTICAST] Created GSN SEQUENCER (group 0) - GSN/META only\n")
		} else {
			fmt.Printf("[MULTICAST] Created orderer for group %d - interleaved SN\n", gid)
		}
	}
}

// setupHandlers - Configura handlers de mensagens e goroutines de monitoramento
func (o *MultiPaxosMulticastOrderer) setupHandlers() {
	messenger.OrdererMsgHandler = o.HandleMessage
	
	// Registra callbacks do request package para GSN/atomic multicast
	request.SetGSNGenerator(o.GetNextGSN)
	request.SetGroupMembersGetter(o.GetGroupMembers)
	request.SetRequestReceivedMarker(o.MarkRequestReceived)
	request.SetRequestCacher(o.CacheRequest)
	request.SetRequestPreprocessor(o.PreprocessRequest)
	request.SetNumGroupsGetter(o.GetNumGroups) // ✅ Injeta getter de numGroups
	
	logger.Info().Msg("[MULTICAST] Registered GSN/atomic multicast callbacks")
	
	// ✅ PERSISTÊNCIA: Carrega nextGSN do disco
	o.loadNextGSN()
	
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
	// ✅ CORREÇÃO #2: Parser de SYSTEM:GSN_RESPONSE (fast-path)
	if gsnForward := pm.GetGsnReqForward(); gsnForward != nil {
		payload := string(gsnForward.Req.Payload)
		
		// ✅ Fast-path: Se é resposta de GSN, completa pending e retorna
		if strings.HasPrefix(payload, "SYSTEM:GSN_RESPONSE:") {
			var reqID uint64
			var gsn uint64
			n, _ := fmt.Sscanf(payload, "SYSTEM:GSN_RESPONSE:%d:%d", &reqID, &gsn)
			if n == 2 {
				o.gsnReqMu.Lock()
				if ch, exists := o.gsnRequestsPending[reqID]; exists {
					ch <- gsn
					delete(o.gsnRequestsPending, reqID)
					fmt.Printf("[GSN-CLIENT] Received GSN=%d for reqID=%d (via GsnReqForward)\n", gsn, reqID)
				}
				o.gsnReqMu.Unlock()
				return // ✅ Não adiciona ao bucket
			}
		}
		
		// Forwarded request: NÃO adicionar ao bucket (já foi adicionado no proxy)
		// Apenas marca como recebida para liveness
		if gsnForward.Req.GSN > 0 && gsnForward.Req.GroupId > 0 {
			o.MarkRequestReceived(gsnForward.Req.GSN, gsnForward.Req.GroupId)
		}
		return
	}
	
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

// GetNumGroups - Retorna número de grupos definidos
// ✅ DYNAMIC: Usado por GetBucketNr() para cálculo dinâmico
func (o *MultiPaxosMulticastOrderer) GetNumGroups() int {
	if o.am == nil {
		return 5 // Fallback
	}
	groups := o.am.GetDefinedGroups()
	return len(groups)
}

// GetNextGSN - Obtém próximo GSN via grupo 0 (sequenciador global)
func (o *MultiPaxosMulticastOrderer) GetNextGSN() uint64 {
	clientSn := atomic.AddUint32(&o.gsnSeqCounter, 1)
	reqID := makeGlobalRequestID(membership.OwnID, clientSn)
	respChan := make(chan uint64, 1)
	
	fmt.Printf("[GSN-REQ][START] nodeId=%d clientSn=%d reqID=%d\n", membership.OwnID, clientSn, reqID)
	
	o.gsnReqMu.Lock()
	o.gsnRequestsPending[reqID] = respChan
	fmt.Printf("[GSN-REQ][PENDING] reqID=%d registered, total pending=%d\n", reqID, len(o.gsnRequestsPending))
	o.gsnReqMu.Unlock()
	
	gsnReq := &pb.ClientRequest{
		RequestId: &pb.RequestID{
			ClientId: membership.OwnID,
			ClientSn: int32(clientSn),
		},
		Payload:       []byte(fmt.Sprintf("%s%d:%d", SYSTEM_GSN_REQUEST, reqID, membership.OwnID)),
		GroupId:       0,
		TouchedGroups: []uint32{0},
	}
	
	// ✅ FIX: Envia para líder do grupo 0 (ou todos membros)
	o.sendToGroup(gsnReq, 0)
	fmt.Printf("[GSN-REQ][SENT] reqID=%d sent to group 0\n", reqID)
	
	select {
	case gsn := <-respChan:
		fmt.Printf("[GSN-REQ][SUCCESS] Received GSN=%d for reqID=%d\n", gsn, reqID)
		o.gsnReqMu.Lock()
		delete(o.gsnRequestsPending, reqID)
		o.gsnReqMu.Unlock()
		return gsn
	case <-time.After(10 * time.Second):
		fmt.Printf("[GSN-REQ][ERROR] Timeout waiting for GSN reqID=%d after 10s\n", reqID)
		o.gsnReqMu.Lock()
		delete(o.gsnRequestsPending, reqID)
		fmt.Printf("[GSN-REQ][ERROR] Still pending: %d requests\n", len(o.gsnRequestsPending))
		o.gsnReqMu.Unlock()
		return 0
	}
}

func (o *MultiPaxosMulticastOrderer) cleanOldMappings(checkpointSN int32) {
	// SN intercalado: mantém compatibilidade com log/checkpoint global
	fmt.Printf("[MULTICAST] Checkpoint %d (interleaved SN space)\n", checkpointSN)
}

// ADeliver - Verifica ordem determinística baseada em META antes de entregar
// ✅ FIX: Só entrega quando gsn == nextTouchingGSN (determinístico via META)
func (o *MultiPaxosMulticastOrderer) ADeliver(gsn uint64, groupID uint32, batch []byte) bool {
	o.expectedGSNMu.Lock()
	defer o.expectedGSNMu.Unlock()
	
	lastDelivered := o.lastDeliveredGSN[groupID]
	
	// Duplicata
	if gsn <= lastDelivered {
		fmt.Printf("[ATOMIC-ORDER] Group %d: GSN %d <= lastDelivered %d, skipping\n", groupID, gsn, lastDelivered)
		return true
	}
	
	// Avança cursor pulando GSNs que não tocam o grupo (via META)
	nextCandidate := lastDelivered + 1
	for nextCandidate < gsn {
		o.metaMu.RLock()
		_, metaExists := o.gsnMetadata[nextCandidate]
		touches := false
		if metaExists {
			touches = o.gsnTouchesGroup(nextCandidate, groupID)
		}
		o.metaMu.RUnlock()
		
		if !metaExists {
			// META não existe - não pode pular
			fmt.Printf("[ATOMIC-ORDER] Group %d: GSN %d blocked (waiting for META of %d)\n", groupID, gsn, nextCandidate)
			return false
		}
		
		if touches {
			// GSN menor toca grupo - precisa esperar
			fmt.Printf("[ATOMIC-ORDER] Group %d: GSN %d blocked (waiting for %d)\n", groupID, gsn, nextCandidate)
			return false
		}
		
		// Não toca - pode pular
		nextCandidate++
	}
	
	// Verifica se este GSN toca o grupo
	o.metaMu.RLock()
	_, metaExists := o.gsnMetadata[gsn]
	if !metaExists {
		o.metaMu.RUnlock()
		fmt.Printf("[ATOMIC-ORDER] Group %d: GSN %d waiting for META\n", groupID, gsn)
		return false
	}
	touches := o.gsnTouchesGroup(gsn, groupID)
	o.metaMu.RUnlock()
	
	if !touches {
		fmt.Printf("[ATOMIC-ORDER] Group %d: GSN %d does not touch group\n", groupID, gsn)
		return true
	}
	
	// gsn == nextCandidate e toca o grupo - pode entregar
	fmt.Printf("[ATOMIC-ORDER] Group %d: Delivering GSN %d (lastDelivered=%d)\n", groupID, gsn, lastDelivered)
	o.lastDeliveredGSN[groupID] = gsn
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

// drainBuffer - Processa commits buffered em ordem determinística baseada em META
// ✅ FIX: Usa mesma lógica do ADeliver - só entrega nextTouchingGSN
func (o *MultiPaxosMulticastOrderer) drainBuffer(groupID uint32) {
	o.bufferMu.Lock()
	defer o.bufferMu.Unlock()
	
	if o.pendingCommits[groupID] == nil {
		return
	}
	
	// Processa em ordem determinística (não apenas "menor buffered")
	for {
		o.expectedGSNMu.RLock()
		lastDelivered := o.lastDeliveredGSN[groupID]
		o.expectedGSNMu.RUnlock()
		
		// Avança cursor pulando GSNs que não tocam o grupo (via META)
		nextCandidate := lastDelivered + 1
		for {
			o.metaMu.RLock()
			_, metaExists := o.gsnMetadata[nextCandidate]
			touches := false
			if metaExists {
				touches = o.gsnTouchesGroup(nextCandidate, groupID)
			}
			o.metaMu.RUnlock()
			
			if !metaExists {
				// META não existe - para (não pode pular)
				return
			}
			
			if touches {
				// Este GSN toca o grupo - é o próximo candidato
				break
			}
			
			// Não toca - pula para próximo
			nextCandidate++
		}
		
		// Verifica se commit do nextCandidate está buffered
		pending, exists := o.pendingCommits[groupID][nextCandidate]
		if !exists {
			// Commit ainda não chegou - para
			return
		}
		
		// Entrega nextCandidate
		fmt.Printf("[BUFFER] Group %d: Draining GSN %d (sn=%d)\n", groupID, nextCandidate, pending.sn)
		pending.announce(pending.sn, pending.batch, pending.digest)
		
		o.expectedGSNMu.Lock()
		o.lastDeliveredGSN[groupID] = nextCandidate
		o.expectedGSNMu.Unlock()
		
		delete(o.pendingCommits[groupID], nextCandidate)
	}
}

// RegisterGSNMetadata - META stream determinístico via grupo 0
// ✅ FIX: Tenta liberar commits buffered após registrar META
func (o *MultiPaxosMulticastOrderer) RegisterGSNMetadata(gsn uint64, touchedGroups []uint32) {
	o.metaMu.Lock()
	
	// ✅ VALIDAÇÃO: Rejeita touchedGroups vazio (bug do chamador)
	if len(touchedGroups) == 0 {
		o.metaMu.Unlock()
		logger.Error().
			Uint64("gsn", gsn).
			Msg("[META-STREAM][ERROR] Empty TouchedGroups; refusing to register META")
		return
	}
	
	// ✅ DEDUPLICACAO: Verifica se META já existe para este GSN
	if _, exists := o.gsnMetadata[gsn]; exists {
		o.metaMu.Unlock()
		fmt.Printf("[META-STREAM][WARN] GSN %d metadata already exists, skipping\n", gsn)
		return
	}
	
	o.gsnMetadata[gsn] = make([]uint32, len(touchedGroups))
	copy(o.gsnMetadata[gsn], touchedGroups)
	o.metaMu.Unlock()
	
	fmt.Printf("[ATOMIC-ORDER] Registered META GSN %d -> groups %v\n", gsn, touchedGroups)
	
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
	
	// ✅ FIX: Tenta liberar commits buffered deste GSN
	for _, groupID := range touchedGroups {
		o.tryDeliverAfterMeta(gsn, groupID)
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

// tryDeliverAfterMeta - Tenta entregar commit buffered após META chegar
// ✅ FIX: Chama drainBuffer que processa em ordem sem deadlock
func (o *MultiPaxosMulticastOrderer) tryDeliverAfterMeta(gsn uint64, groupID uint32) {
	o.bufferMu.RLock()
	_, exists := o.pendingCommits[groupID][gsn]
	o.bufferMu.RUnlock()
	
	if !exists {
		return // Não há commit buffered para este GSN
	}
	
	fmt.Printf("[META-ARRIVED] Group %d: Trying to drain buffer after GSN %d META\n", groupID, gsn)
	o.drainBuffer(groupID)
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
	
	// ✅ VALIDAÇÃO: Rejeita touchedGroups vazio (bug do chamador)
	if len(touchedGroups) == 0 {
		logger.Error().
			Uint64("gsn", gsn).
			Msg("[META-STREAM][ERROR] Empty TouchedGroups; refusing to publish META")
		return
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
	
	fmt.Printf("[META-STREAM] Publishing GSN %d -> groups %v to group 0 (proxy %d)\n", gsn, touchedGroups, membership.OwnID)
	
	// ✅ FIX: Envia para líder do grupo 0 (ou todos membros)
	o.sendToGroup(metaReq, 0)
}

// Mcast - API black-box para multicast atômico
// Esconde toda a complexidade de GSN/META do usuário
// Parâmetros:
//   groups: lista de grupos que devem receber a mensagem
//   msg: dados da mensagem a ser enviada
// Retorna: erro se houver falha na submissão
func (o *MultiPaxosMulticastOrderer) Mcast(groups []uint32, msg []byte) error {
	// ✅ 1) Valida grupos ANTES de qualquer operação
	if len(groups) == 0 {
		return fmt.Errorf("groups cannot be empty")
	}
	
	// ✅ 2) Saneia: remove grupo 0, dedup e ordena
	groups = sanitizeGroups(groups)
	if len(groups) == 0 {
		return fmt.Errorf("no valid data groups (group 0 is not allowed)")
	}
	
	// ✅ 3) Obtém GSN e valida (timeout = 0)
	gsn := o.GetNextGSN()
	if gsn == 0 {
		return fmt.Errorf("failed to get GSN (timeout)")
	}
	
	// ✅ 4) Publica META apenas após validações
	o.PublishGSNMetadata(gsn, groups)
	
	// ✅ 5) Submete request para cada grupo tocado
	for _, groupID := range groups {
		mcastSn := atomic.AddUint32(&o.mcastSeqCounter, 1)
		req := &pb.ClientRequest{
			RequestId: &pb.RequestID{
				ClientId: membership.OwnID,
				ClientSn: int32(mcastSn),
			},
			Payload:       msg,
			TouchedGroups: groups,
			GSN:           gsn,
			GroupId:       groupID,
		}
		o.sendToGroup(req, groupID)
	}
	return nil
}

// sanitizeGroups - Remove grupo 0, duplicatas e ordena
func sanitizeGroups(in []uint32) []uint32 {
	seen := make(map[uint32]struct{}, len(in))
	out := make([]uint32, 0, len(in))
	for _, g := range in {
		if g == 0 {
			continue // Grupo 0 é sequenciador, não grupo de dados
		}
		if _, ok := seen[g]; ok {
			continue // Duplicata
		}
		seen[g] = struct{}{}
		out = append(out, g)
	}
	// Ordena para determinismo
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// sendToGroup - Envia request para o grupo via rede (messenger)
// ✅ FIX: Se proxy não é membro, encaminha para membros do grupo
// Garante que o líder do grupo SEMPRE receba a request (liveness)
func (o *MultiPaxosMulticastOrderer) sendToGroup(req *pb.ClientRequest, groupID uint32) {
	members := o.am.GetGroupMembers(groupID)
	if members == nil || len(members) == 0 {
		fmt.Printf("[SEND] Group %d has no members, dropping\n", groupID)
		return
	}

	// ✅ FIX: Verifica se proxy é membro do grupo
	isMember := false
	for _, nodeID := range members {
		if nodeID == membership.OwnID {
			isMember = true
			break
		}
	}

	if isMember {
		// ✅ ATALHO: Proxy é membro, adiciona ao bucket local
		fmt.Printf("[SEND] Adding to local bucket for group %d (I am member)\n", groupID)
		request.AddReqMsg(req)
	} else {
		// ✅ PROXY: Não é membro, encaminha para membros do grupo via rede
		fmt.Printf("[SEND] Forwarding to group %d members (I am NOT member)\n", groupID)
	}

	// ✅ REDE: Envia para TODOS os membros do grupo (garante liveness)
	for _, nodeID := range members {
		if nodeID == membership.OwnID {
			continue // Já adicionou localmente acima (se for membro)
		}
		
		// Envia para outros membros via rede
		pm := &pb.ProtocolMessage{
			SenderId: membership.OwnID,
			Sn:       -1,
			Msg: &pb.ProtocolMessage_GsnReqForward{
				GsnReqForward: &pb.GSNReqForward{
					Req: req,
				},
			},
		}
		messenger.EnqueueMsg(pm, nodeID)
		fmt.Printf("[SEND] Forwarded to node %d (group %d member)\n", nodeID, groupID)
	}
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

// MarkRequestReceived - Marca request como recebida (para watchdog)
// ✅ DEDUPLICAÇÃO: Evita processar mesma (GSN, GroupId) múltiplas vezes
func (o *MultiPaxosMulticastOrderer) MarkRequestReceived(gsn uint64, groupID uint32) {
	o.missingMu.Lock()
	defer o.missingMu.Unlock()
	
	if groups, exists := o.missingRequests[gsn]; exists {
		if _, waiting := groups[groupID]; waiting {
			delete(groups, groupID)
			fmt.Printf("[LIVENESS] GSN %d received by group %d (stopped waiting)\n", gsn, groupID)
			if len(groups) == 0 {
				delete(o.missingRequests, gsn)
				fmt.Printf("[LIVENESS] GSN %d received by all groups (cleanup)\n", gsn)
			}
		}
	}
}

// CacheRequest - Cache request para re-forward
// ✅ DEDUPLICAÇÃO: Armazena apenas uma vez por GSN
func (o *MultiPaxosMulticastOrderer) CacheRequest(gsn uint64, req *pb.ClientRequest) {
	o.cacheMu.Lock()
	defer o.cacheMu.Unlock()
	
	if _, exists := o.requestCache[gsn]; !exists {
		o.requestCache[gsn] = req
		fmt.Printf("[LIVENESS] Cached request GSN %d for re-forward\n", gsn)
	}
}

// ✅ LIVENESS: Watchdog para detectar e re-forward requests perdidas
func (o *MultiPaxosMulticastOrderer) reforwardWatchdog() {
	ticker := time.NewTicker(30 * time.Second) // Verifica a cada 30s
	defer ticker.Stop()
	
	for range ticker.C {
		o.missingMu.RLock()
		now := time.Now()
		toReforward := make(map[uint64][]uint32)
		
		for gsn, groups := range o.missingRequests {
			for groupID, timestamp := range groups {
				if now.Sub(timestamp) > 60*time.Second { // Timeout de 60s
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
	o.cacheMu.RLock()
	req, exists := o.requestCache[gsn]
	o.cacheMu.RUnlock()
	
	if !exists {
		fmt.Printf("[LIVENESS][ERROR] GSN %d not in cache, cannot reforward\n", gsn)
		return
	}
	
	// Reenvia para cada grupo faltante
	for _, groupID := range groups {
		clone := &pb.ClientRequest{
			RequestId:     req.RequestId,
			Payload:       req.Payload,
			Signature:     req.Signature,
			Pubkey:        req.Pubkey,
			GroupId:       groupID,
			TouchedGroups: req.TouchedGroups,
			GSN:           gsn,
		}
		fmt.Printf("[LIVENESS] Re-forwarding GSN %d to group %d\n", gsn, groupID)
		o.sendToGroup(clone, groupID)
		
		// Atualiza timestamp
		o.missingMu.Lock()
		if o.missingRequests[gsn] != nil {
			o.missingRequests[gsn][groupID] = time.Now()
		}
		o.missingMu.Unlock()
	}
}

// Sign - Assina dados com chave privada do orderer (implementa interface Orderer)
func (o *MultiPaxosMulticastOrderer) Sign(data []byte) ([]byte, error) {
	if membership.OwnPrivKey == nil {
		return nil, fmt.Errorf("private key not initialized")
	}
	return crypto.Sign(data, membership.OwnPrivKey)
}

// loadNextGSN - Carrega nextGSN persistido do disco
func (o *MultiPaxosMulticastOrderer) loadNextGSN() {
	b, err := os.ReadFile(gsnStateFile)
	if err == nil {
		if v, err2 := strconv.ParseUint(strings.TrimSpace(string(b)), 10, 64); err2 == nil && v > 0 {
			o.nextGSN = v
			fmt.Printf("[RECOVERY] Loaded nextGSN=%d from disk\n", v)
			return
		}
	}
	fmt.Printf("[RECOVERY] Starting with nextGSN=1 (no persisted state)\n")
}

// persistNextGSN - Persiste nextGSN no disco
func (o *MultiPaxosMulticastOrderer) persistNextGSN() {
	_ = os.WriteFile(gsnStateFile, []byte(fmt.Sprintf("%d\n", o.nextGSN)), 0644)
}

// CheckSig - Verifica assinatura de dados (implementa interface Orderer)
func (o *MultiPaxosMulticastOrderer) CheckSig(data []byte, senderID int32, signature []byte) error {
	if !config.Config.SignRequests {
		return nil
	}
	nodeIdentity := membership.NodeIdentity(senderID)
	if nodeIdentity == nil || nodeIdentity.PubKey == nil {
		return fmt.Errorf("public key not found for node %d", senderID)
	}
	pubKey, err := crypto.PublicKeyFromBytes(nodeIdentity.PubKey)
	if err != nil {
		return fmt.Errorf("failed to decode public key for node %d: %w", senderID, err)
	}
	return crypto.CheckSig(data, pubKey, signature)
}

// HandleEntry - Processa entrada do log (implementa interface Orderer)
func (o *MultiPaxosMulticastOrderer) HandleEntry(entry *log.Entry) {
	// Delega para orderers de grupo apropriados
	if entry == nil {
		return
	}
	
	// Determina grupo baseado no SN ou conteúdo da entrada
	var groupID uint32 = 0
	if entry.Batch != nil && len(entry.Batch.Requests) > 0 {
		groupID = entry.Batch.Requests[0].GetGroupId()
	}
	
	o.orderersMu.RLock()
	orderer := o.groupOrderers[groupID]
	o.orderersMu.RUnlock()
	
	if orderer != nil {
		orderer.HandleEntry(entry)
	}
}

// NotifyNewRequest - Notifica líder quando request chega no bucket
// ✅ LIVENESS: Acorda líder imediatamente sem depender de polling
func (o *MultiPaxosMulticastOrderer) NotifyNewRequest(groupID uint32) {
	// Obtém orderer do grupo
	o.orderersMu.RLock()
	groupOrderer := o.groupOrderers[groupID]
	o.orderersMu.RUnlock()
	
	if groupOrderer == nil {
		return
	}
	
	// Verifica se este nó é líder do grupo
	// Para grupo 0, todos os nós são membros, então verifica liderança diretamente
	// Para outros grupos, verifica se é membro E líder
	if groupID != 0 && !o.am.IsMember(groupID, membership.OwnID) {
		return // Não é membro do grupo
	}
	
	// Obtém instância ativa do grupo
	inst := groupOrderer.GetActiveInstance(groupID)
	if inst != nil {
		// Verifica se é líder da instância
		inst.mu.Lock()
		isLeader := (inst.leader == membership.OwnID)
		inst.mu.Unlock()
		
		if isLeader {
			fmt.Printf("[NOTIFY] Group %d: Waking leader for new request\n", groupID)
			inst.ProposeIfDue()
		}
	}
}

// PreprocessRequest - Preprocessa request para atomic multicast
// Retorna true se já processou (não precisa processamento padrão)
func (o *MultiPaxosMulticastOrderer) PreprocessRequest(req *pb.ClientRequest) bool {
	payloadPreview := req.Payload
	if len(payloadPreview) > 50 {
		payloadPreview = payloadPreview[:50]
	}
	fmt.Printf("[PREPROCESS] Called for clientId=%d clientSn=%d groupId=%d touchedGroups=%v payload=%s\n", 
		req.RequestId.ClientId, req.RequestId.ClientSn, req.GroupId, req.TouchedGroups, string(payloadPreview))
	
	// ✅ Se já tem GSN, foi preprocessado (forwarded de outro nó ou clone interno)
	if req.GSN > 0 {
		fmt.Printf("[PREPROCESS] Already has GSN=%d (forwarded), skipping\n", req.GSN)
		return false
	}
	
	// ✅ FILTRO GERAL: Bloqueia TODAS mensagens SYSTEM:* de virarem carga de aplicação
	// Apenas GSN_REQUEST e META_STREAM devem entrar no consenso (grupo 0)
	if strings.HasPrefix(string(req.Payload), "SYSTEM:") {
		// Permite apenas mensagens sistêmicas conhecidas para grupo 0
		if strings.HasPrefix(string(req.Payload), SYSTEM_GSN_REQUEST) || 
		   strings.HasPrefix(string(req.Payload), SYSTEM_META_STREAM) {
			fmt.Printf("[PREPROCESS] System request for group 0, allowing\n")
			return false // Deixa sistema processar normalmente
		}
		// Bloqueia outras mensagens SYSTEM:* (ex: SYSTEM:GSN_RESPONSE já tratada no HandleMessage)
		fmt.Printf("[PREPROCESS] Blocking unknown SYSTEM:* message (not for consensus)\n")
		return true // Bloqueia
	}
	
	// ✅ Mapeia requisição para grupos usando ReplicaMapper
	fmt.Printf("[PREPROCESS] Calling ReplicaMapper...\n")
	req.TouchedGroups = request.ReplicaMapper(req.Payload)
	
	// ✅ DETERMINISMO: Ordena TouchedGroups (segunda camada de segurança)
	sort.Slice(req.TouchedGroups, func(i, j int) bool { return req.TouchedGroups[i] < req.TouchedGroups[j] })
	fmt.Printf("[PREPROCESS] ReplicaMapper returned groups=%v (sorted)\n", req.TouchedGroups)
	
	// ✅ VALIDAÇÃO: Remove grupo 0 se ReplicaMapper retornou (não deve acontecer)
	for i := 0; i < len(req.TouchedGroups); i++ {
		if req.TouchedGroups[i] == 0 {
			fmt.Printf("[PREPROCESS][WARN] Removing group 0 from TouchedGroups\n")
			req.TouchedGroups = append(req.TouchedGroups[:i], req.TouchedGroups[i+1:]...)
			i--
		}
	}
	
	// ✅ VALIDAÇÃO: Deve ter pelo menos um grupo de dados
	if len(req.TouchedGroups) == 0 {
		fmt.Printf("[PREPROCESS][ERROR] No valid data groups, rejecting request\n")
		return true // Bloqueia requisição inválida
	}
	
	// ✅ Single-group: Marca como preprocessado e deixa AddReqMsg processar
	if len(req.TouchedGroups) == 1 {
		req.GroupId = req.TouchedGroups[0]
		req.GSN = 1 // Marca como preprocessado
		fmt.Printf("[PREPROCESS] Single-group: group=%d (no GSN/META needed)\n", req.GroupId)
		return false // Deixa AddReqMsg processar normalmente
	}
	
	// ✅ Cross-group: Obtém GSN e publica META
	fmt.Printf("[PREPROCESS] Cross-group: getting GSN...\n")
	gsn := o.GetNextGSN()
	if gsn == 0 {
		fmt.Printf("[PREPROCESS][ERROR] Failed to get GSN (timeout), rejecting request\n")
		return true // Bloqueia requisição (GSN inválido)
	}
	fmt.Printf("[PREPROCESS] Got GSN=%d, publishing META...\n", gsn)
	o.PublishGSNMetadata(gsn, req.TouchedGroups)
	
	// ✅ 2) LIVENESS: Cache template ANTES do fanout (para reforward)
	templateReq := &pb.ClientRequest{
		RequestId:     req.RequestId,
		Payload:       req.Payload,
		Signature:     req.Signature,
		Pubkey:        req.Pubkey,
		TouchedGroups: req.TouchedGroups,
		GSN:           gsn,
	}
	o.CacheRequest(gsn, templateReq)
	
	fmt.Printf("[PREPROCESS] Cross-group: mapped to groups=%v gsn=%d\n", req.TouchedGroups, gsn)
	
	// ✅ Cross-op: clona para cada grupo
	fmt.Printf("[PREPROCESS] Cross-op: cloning for %d groups\n", len(req.TouchedGroups))
	for _, groupID := range req.TouchedGroups {
		clone := &pb.ClientRequest{
			RequestId:     req.RequestId,
			Payload:       req.Payload,
			Signature:     req.Signature,
			Pubkey:        req.Pubkey,
			GroupId:       groupID,
			TouchedGroups: req.TouchedGroups,
			GSN:           gsn,
		}
		o.sendToGroup(clone, groupID)
	}
	
	// Já adicionou aos buckets, não precisa processar de novo
	fmt.Printf("[PREPROCESS] Processing complete, returning true\n")
	return true
}
