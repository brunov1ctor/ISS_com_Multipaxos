/*
MultiPaxos Orderer - Gerenciador de Consenso por Grupo

Implementa o protocolo MultiPaxos para um único grupo, gerenciando múltiplas
instâncias de consenso (uma por SN). Atua como coordenador entre o sistema
MirBFT e as instâncias individuais de consenso.

Componentes Principais:
- mpxInstance: Instância de consenso para um SN específico
- mpxDispatcher: Roteador que direciona mensagens para instâncias corretas
- mpxBacklog: Buffer para mensagens de instâncias ainda não criadas

Fluxo do Protocolo:
1. Recebe segmento do MirBFT manager com range de SNs
2. Cria instâncias sob demanda para cada SN no segmento
3. Líder propõe valores (PREPARE → PROMISE → ACCEPT → ACCEPTED → COMMIT)
4. Instância commita e anuncia resultado via callback
5. Resultado é adicionado ao log local do MirBFT

Integração com Sistema Multicast:
- Processa requests sistêmicas (GSN_REQUEST para sequenciamento global)
- Processa META_STREAM (metadados de quais grupos cada operação toca)
- TouchedGroups sempre definido para evitar erros fatais
- Suporte completo à ordem GSN global entre grupos
- Sistema de liveness com re-forward automático

Modos de Operação:
- Standalone: Gerencia todos os grupos (modo compatível)
- Multicast: Gerencia apenas um grupo específico (modo distribuído)
*/
package orderer
import (
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"google.golang.org/protobuf/proto"
	"github.com/hyperledger-labs/mirbft/announcer"
	"github.com/hyperledger-labs/mirbft/config"
	"github.com/hyperledger-labs/mirbft/crypto"
	mirlog "github.com/hyperledger-labs/mirbft/log"
	"github.com/hyperledger-labs/mirbft/manager"
	"github.com/hyperledger-labs/mirbft/membership"
	"github.com/hyperledger-labs/mirbft/messenger"
	pb "github.com/hyperledger-labs/mirbft/protobufs"
	logger "github.com/rs/zerolog/log"
)
type AnnounceFn func(sn int32, batchBytes []byte, metadata []byte)
type mpxDispatcher struct {
	mm sync.Map
}
func (d *mpxDispatcher) load(sn int32) (*mpxInstance, bool) {
	if v, ok := d.mm.Load(sn); ok {
		return v.(*mpxInstance), true
	}
	return nil, false
}
func (d *mpxDispatcher) store(sn int32, inst *mpxInstance) { d.mm.Store(sn, inst) }
func (d *mpxDispatcher) delete(sn int32)                    { d.mm.Delete(sn) }
type mpxBacklog struct {
	mu sync.Mutex
	qs map[int32][]*pb.ProtocolMessage
}
func newMPXBacklog() mpxBacklog {
	return mpxBacklog{qs: make(map[int32][]*pb.ProtocolMessage)}
}
func (b *mpxBacklog) drainTo(sn int32, f func(*pb.ProtocolMessage)) {
	b.mu.Lock()
	items := b.qs[sn]
	delete(b.qs, sn)
	b.mu.Unlock()
	for _, m := range items {
		f(m)
	}
}
// MultiPaxosOrderer - Orderer para um único grupo usando protocolo MultiPaxos
// Gerencia múltiplas instâncias de consenso (uma por SN)
type MultiPaxosOrderer struct {
	mgr         manager.Manager    // Interface com MirBFT
	segmentChan chan manager.Segment // Canal de novos segmentos
	dispatcher mpxDispatcher       // Roteador de mensagens por SN
	backlog    mpxBacklog          // Buffer para mensagens de instâncias futuras
	last       int32               // Último SN processado
	instMu    sync.RWMutex        // Protege instâncias
	startOnce sync.Once           // Garante inicialização única
	emit     func(pm *pb.ProtocolMessage) // Função para enviar mensagens
	announce AnnounceFn           // Função para anunciar commits
	maxBatchSize     int          // Tamanho máximo do batch
	proposeEvery     time.Duration // Intervalo entre propostas
	stopWg           sync.WaitGroup // WaitGroup para parada limpa
	onInstanceCreated func(sn int32) // Callback para nova instância
	am *AtomicMulticast           // Referência ao gerenciador de grupos
	ownedGroupID uint32           // ID do grupo que este orderer gerencia
	skipHandlerRegistration bool  // Se deve pular registro de handler global
	segmentInstances sync.Map     // Instâncias por segmento
	currentSegCancel func()       // Função para cancelar segmento atual
	segMu            sync.Mutex   // Protege currentSegCancel
	currentFirstSN int32          // Primeiro SN do segmento atual
	firstSNMu      sync.RWMutex   // Protege currentFirstSN
}
// inferGroupIDFromSN removed - not used (group determined by segment)
func (o *MultiPaxosOrderer) Init(mgr manager.Manager) {
	o.mgr = mgr
	o.backlog = newMPXBacklog()
	o.last = -1
	o.maxBatchSize = int(config.Config.BatchSize)
	o.proposeEvery = config.Config.BatchTimeout
	if o.am == nil {
		// ✅ FIX: Usa AtomicMulticast global compartilhado
		if GetGlobalMulticastOrderer() != nil {
			o.am = GetGlobalMulticastOrderer().am
			fmt.Printf("[MPX] Init: using shared AtomicMulticast from global orderer (groups=%v, ownedGroupID=%d)\n", o.am.GetDefinedGroups(), o.ownedGroupID)
		} else {
			o.am = NewAtomicMulticast()
			fmt.Printf("[MPX] Init: created new AtomicMulticast (ownedGroupID=%d)\n", o.ownedGroupID)
		}
	} else {
		fmt.Printf("[MPX] Init: reusing injected AtomicMulticast (groups=%v, ownedGroupID=%d)\n", o.am.GetDefinedGroups(), o.ownedGroupID)
	}
	o.emit = func(pm *pb.ProtocolMessage) {
		mpx := pm.GetMultipaxos()
		var groupID uint32
		if mpx == nil {
			for _, nid := range membership.AllNodeIDs() {
				messenger.EnqueueMsg(pm, nid)
			}
			return
		}
		groupID = extractGroupID(mpx)
		if groupID == 0 {
			for _, nid := range membership.AllNodeIDs() {
				messenger.EnqueueMsg(pm, nid)
			}
			return
		}
		if o.am != nil {
			members := o.am.GetGroupMembers(groupID)
			if members != nil && len(members) > 0 {
				for _, nodeID := range members {
					messenger.EnqueueMsg(pm, nodeID)
				}
				return
			}
		}
		for _, nid := range membership.AllNodeIDs() {
			messenger.EnqueueMsg(pm, nid)
		}
	}
	// ✅ FIX: Nenhum grupo usa segment manager (SN local com mapeamento global)
	o.segmentChan = nil
	fmt.Printf("[MPX] Using local SN loop (no segments) ownedGroupID=%d\n", o.ownedGroupID)
	if !o.skipHandlerRegistration {
		messenger.OrdererMsgHandler = o.HandleMessage
		fmt.Printf("[MPX] Registered global message handler\n")
	} else {
		fmt.Printf("[MPX] Skipped handler registration (managed by multicast orderer)\n")
	}
	o.announce = func(sn int32, batchBytes []byte, metadata []byte) {
		if len(batchBytes) == 0 {
			fmt.Printf("[MPX][SKIP] sn=%d (empty batch)\n", sn)
			emptyBatch := &pb.Batch{Requests: []*pb.ClientRequest{}}
			shouldRespond := true
			emptyBytes, _ := proto.Marshal(emptyBatch)
			digest := crypto.Hash(emptyBytes)
			entry := &mirlog.Entry{
				Sn:            sn,
				Batch:         emptyBatch,
				Digest:        digest,
				ShouldRespond: &shouldRespond,
			}
			announcer.Announce(entry)
			return
		}
		var gsn uint64
		innerBatch := batchBytes
		hasGSN := false
		if gsn, innerBatch, hasGSN = decodeGSNBatch(batchBytes); hasGSN {
			fmt.Printf("[CROSS-OP] sn=%d decoded gsn=%d from batch\n", sn, gsn)
		}
		var b pb.Batch
		if err := proto.Unmarshal(innerBatch, &b); err != nil {
			fmt.Printf("[MPX][ANNOUNCE][ERR] sn=%d unmarshal: %v\n", sn, err)
			return
		}
		if hasGSN && len(b.Requests) > 0 {
			for _, req := range b.Requests {
				req.GSN = gsn
			}
		}
		var digest []byte
		if len(metadata) > 0 {
			digest = metadata
		} else {
			digest = crypto.Hash(batchBytes)
		}
		shouldRespond := true
		if len(b.Requests) > 0 && o.am != nil {
			groupId := b.Requests[0].GetGroupId()
			// Responde apenas se for membro do grupo (incluindo grupo 0)
			shouldRespond = o.am.IsMember(groupId, membership.OwnID)
			fmt.Printf("[MPX][ANNOUNCE] sn=%d groupId=%d ownID=%d shouldRespond=%v\n", sn, groupId, membership.OwnID, shouldRespond)
		} else {
			fmt.Printf("[MPX][ANNOUNCE] sn=%d NO REQUESTS or NO AM, shouldRespond=%v\n", sn, shouldRespond)
		}
		entry := &mirlog.Entry{
			Sn:             sn,
			Batch:          &b,
			Digest:         digest,
			ShouldRespond:  &shouldRespond,
		}
		announcer.Announce(entry)
		
		// ✅ META stream: Registra operações multigrupo quando grupo 0 commita
		if len(b.Requests) > 0 && GetGlobalMulticastOrderer() != nil && o.ownedGroupID == 0 {
			// Analisa quais grupos cada requisição toca
			touchedGroupsMap := make(map[uint64][]uint32)
			for _, req := range b.Requests {
				if req.GSN > 0 {
					// Se TouchedGroups já está preenchido, usa direto
					if len(req.TouchedGroups) > 0 {
						touchedGroupsMap[req.GSN] = req.TouchedGroups
					} else if req.GroupId > 0 {
						// Operação single-group
						touchedGroupsMap[req.GSN] = []uint32{req.GroupId}
					}
				}
			}
			// Registra operações multigrupo (len > 1)
			for gsn, groups := range touchedGroupsMap {
				if len(groups) > 1 {
					GetGlobalMulticastOrderer().RegisterGSNMetadata(gsn, groups)
					fmt.Printf("[META-STREAM][REGISTERED] GSN %d -> groups %v\n", gsn, groups)
				}
			}
		}
		fmt.Printf("DELIVER sn=%d delivered=%d\n", sn, len(b.Requests))
	}
	fmt.Printf("[MPX] Init ok; cfg: batchSize=%d batchTimeout=%s leaderPolicy=%s\n",
		o.maxBatchSize, o.proposeEvery, strings.ToLower(config.Config.LeaderPolicy))
}
func (o *MultiPaxosOrderer) Start(wg *sync.WaitGroup) {
	o.startOnce.Do(func() {
		fmt.Printf("[MPX] Start begin (ownedGroupID=%d)\n", o.ownedGroupID)
		
		// ✅ FIX: Grupos de dados usam SN local, standalone/grupo0 usa segmentos
		if o.segmentChan != nil {
			// Modo segment manager (standalone ou grupo 0)
			go func() {
				for seg := range o.segmentChan {
					logger.Info().
						Int("segId", seg.SegID()).
						Int32("length", seg.Len()).
						Int32("firstSN", seg.FirstSN()).
						Int32("lastSN", seg.LastSN()).
						Int32("first leader", seg.Leaders()[0]).
						Int32("len", seg.Len()).
						Msgf("MultiPaxos received new segment: %+v", seg.SNs())
					o.runSegment(seg)
					go o.killSegment(seg)
				}
			}()
			fmt.Printf("[MPX] Started with segment manager\n")
		} else {
			// ✅ Modo SN local (grupos de dados)
			go o.runLocalSNLoop()
			fmt.Printf("[MPX] Started with local SN loop for group %d\n", o.ownedGroupID)
		}
		
		fmt.Printf("[MPX] Start done\n")
	})
}
func (o *MultiPaxosOrderer) HandleMessage(pm *pb.ProtocolMessage) {
	sn := pm.Sn
	if pm.SenderId == membership.OwnID {
		logger.Warn().Int32("sn", sn).Msg("MPX handles message from self.")
	}
	last := atomic.LoadInt32(&o.last)
	if sn <= last {
		logger.Debug().
			Int32("sn", sn).Int32("senderID", pm.SenderId).
			Msg("MPX discards message. Message belongs to an old segment.")
		return
	}
	mpx := pm.GetMultipaxos()
	var groupID uint32
	if mpx != nil {
		groupID = extractGroupID(mpx)
		if groupID != 0 && o.am != nil {
			members := o.am.GetGroupMembers(groupID)
			if members != nil {
				isMember := false
				for _, m := range members {
					if m == membership.OwnID {
						isMember = true
						break
					}
				}
				if !isMember {
					return
				}
			}
		}
	}
	inst, ok := o.dispatcher.load(sn)
	if !ok || inst == nil {
		inst = o.ensureInstance(sn)
		// Seta bucketId e bucketIndex baseado na mensagem
		if mpx != nil {
			inst.bucketId = groupID
			// ✅ FIX: bucketIndex = groupId (not array position)
			inst.bucketIndex = int32(groupID)
			fmt.Printf("[MPX][INST] sn=%d set bucketId=%d bucketIndex=%d from message\n", sn, groupID, int32(groupID))
		}
		o.dispatcher.store(sn, inst)
		inst.startWorkers(&o.stopWg)
		o.backlog.drainTo(sn, inst.enqueue)
	}
	inst.enqueue(pm)
}
func (o *MultiPaxosOrderer) HandleEntry(e *mirlog.Entry) {
	if e == nil {
		return
	}
	o.HandleMessage(&pb.ProtocolMessage{
		SenderId: -1,
		Sn:       e.Sn,
		Msg: &pb.ProtocolMessage_MissingEntry{
			MissingEntry: &pb.MissingEntry{
				Sn:      e.Sn,
				Batch:   e.Batch,
				Digest:  e.Digest,
				Aborted: e.Aborted,
				Suspect: e.Suspect,
				Proof:   "Dummy Proof.",
			},
		},
	})
}
// runLocalSNLoop - Loop de SN local para grupos de dados (sem segment manager)
// Cada grupo mantém seu próprio espaço SN local: 0,1,2,3...
// Mas mapeia para SN global único: globalSN = localSN * numGroups + groupID
// Ordem global é garantida por GSN/META
func (o *MultiPaxosOrderer) runLocalSNLoop() {
	groupId := o.ownedGroupID
	members := o.am.GetGroupMembers(groupId)
	if members == nil {
		fmt.Printf("[MPX][LOCAL-SN] Group %d has NO members, exiting\n", groupId)
		return
	}
	
	// Verifica se é membro do grupo
	if !o.am.IsMember(groupId, membership.OwnID) {
		fmt.Printf("[MPX][LOCAL-SN] Not a member of group %d, exiting\n", groupId)
		return
	}
	
	// Calcula numGroups para mapeamento SN local → global
	allGroupIDs := o.am.GetDefinedGroups()
	if len(allGroupIDs) == 0 {
		allGroupIDs = []uint32{0}
	} else if allGroupIDs[0] != 0 {
		allGroupIDs = append([]uint32{0}, allGroupIDs...)
	}
	numGroups := int32(len(allGroupIDs))
	if numGroups == 0 {
		numGroups = 1
	}
	
	fmt.Printf("[MPX][LOCAL-SN] Starting local SN loop for group %d with %d members: %v (numGroups=%d)\n", groupId, len(members), members, numGroups)
	
	// ✅ Pipeline window scheduler (correto conforme artigo)
	nextLocalSN := int32(0)
	windowW := int32(1) // Começa com 1 para debug, depois aumentar para 4-8
	inFlight := make(map[int32]*mpxInstance)
	bucketIdx := int32(groupId)
	
	// Função auxiliar: mapeia localSN → globalSN
	globalOf := func(local int32) int32 {
		return local*numGroups + int32(groupId)
	}
	
	// ✅ Avanço baseado APENAS no log (fonte de verdade do SMR)
	// Elimina "salto misterioso" - cada advance é logado explicitamente
	advance := func() {
		for {
			gsn := globalOf(nextLocalSN)
			if mirlog.GetEntry(gsn) == nil {
				return
			}
			fmt.Printf("[MPX][LOCAL-SN] Group %d: globalSN=%d already COMMITTED in log, advancing localSN=%d→%d\n",
				groupId, gsn, nextLocalSN, nextLocalSN+1)
			
			// Limpa instância antiga se existir
			if inst, ok := inFlight[gsn]; ok {
				inst.stopWorkers()
				delete(inFlight, gsn)
			}
			
			nextLocalSN++
		}
	}
	
	// Função para garantir janela de instâncias em voo
	ensureWindow := func() {
		for off := int32(0); off < windowW; off++ {
			local := nextLocalSN + off
			gsn := globalOf(local)
			
			// Se já commitou, não precisa de instância
			if mirlog.GetEntry(gsn) != nil {
				continue
			}
			
			// Se já existe, não recria
			if _, ok := inFlight[gsn]; ok {
				continue
			}
			
			// Cria nova instância
			inst := o.ensureInstance(gsn)
			inst.bucketId = groupId
			inst.bucketIndex = bucketIdx
			inst.SetMembers(members)
			o.dispatcher.store(gsn, inst)
			inst.startWorkers(&o.stopWg)
			o.backlog.drainTo(gsn, inst.enqueue)
			
			// Envia PREPARE
			prep := &pb.MPxMsg{Type: &pb.MPxMsg_Prepare{
				Prepare: &pb.MPxPrepare{
					Id:      &pb.MPxInstanceId{Sn: gsn, Lead: uint64(membership.OwnID)},
					Ballot:  uint64(inst.currentBallot),
					GroupId: groupId,
				},
			}}
			pm := &pb.ProtocolMessage{
				SenderId: membership.OwnID,
				Sn:       gsn,
				Msg:      &pb.ProtocolMessage_Multipaxos{Multipaxos: prep},
			}
			if o.emit != nil {
				inst.enqueue(pm)
				o.emit(pm)
			}
			inst.prepSent = true
			inFlight[gsn] = inst
			
			fmt.Printf("[MPX][SCHED] group=%d create inst globalSN=%d (localSN=%d)\n",
				groupId, gsn, local)
		}
	}
	
	// Função para processar instâncias da janela
	processSome := func(now time.Time) {
		processed := int32(0)
		for off := int32(0); off < windowW; off++ {
			local := nextLocalSN + off
			gsn := globalOf(local)
			
			inst := inFlight[gsn]
			if inst == nil {
				continue
			}
			
			inst.tick(now)
			inst.ProposeIfDue()
			
			processed++
			fmt.Printf("[MPX][SCHED] group=%d process globalSN=%d tick+propose\n", groupId, gsn)
			
			if processed >= windowW {
				break
			}
		}
	}
	
	// Ticker para propostas periódicas
	ticker := time.NewTicker(o.proposeEvery)
	defer ticker.Stop()
	
	// ✅ FIX: Líder = primeiro membro do grupo (determinístico)
	isLeader := (membership.OwnID == members[0])
	fmt.Printf("[MPX][LOCAL-SN] Group %d: ownID=%d leader=%d isLeader=%v\n", groupId, membership.OwnID, members[0], isLeader)
	fmt.Printf("[MPX][LOCAL-SN] Group %d: Starting ticker loop (interval=%v, isLeader=%v, windowW=%d)\n", 
		groupId, o.proposeEvery, isLeader, windowW)
	
	for {
		select {
		case <-ticker.C:
			// Não-líderes apenas processam mensagens via HandleMessage
			if !isLeader {
				continue
			}
			
			now := time.Now()
			
			// 1) Avança baseado APENAS no log (fonte de verdade do SMR)
			advance()
			
			// 2) Mantém janela de instâncias em voo
			ensureWindow()
			
			// 3) Processa instâncias da janela (controlado)
			processSome(now)
			
			// 4) Avanço final (caso commit tenha ocorrido durante processamento)
			advance()
		}
	}
}

func (o *MultiPaxosOrderer) runSegment(seg manager.Segment) {
	o.firstSNMu.Lock()
	o.currentFirstSN = seg.FirstSN()
	o.firstSNMu.Unlock()
	o.segMu.Lock()
	if o.currentSegCancel != nil {
		o.currentSegCancel()
	}
	stopCh := make(chan struct{})
	o.currentSegCancel = func() { close(stopCh) }
	o.segMu.Unlock()
	
	// ✅ FIX: SN intercalado - cada orderer processa apenas SEU grupo
	// Calcula qual grupo este segmento pertence
	firstSN := seg.FirstSN()
	allGroupIDs := o.am.GetDefinedGroups()
	if len(allGroupIDs) == 0 {
		allGroupIDs = []uint32{0}
	} else if allGroupIDs[0] != 0 {
		allGroupIDs = append([]uint32{0}, allGroupIDs...)
	}
	numGroups := int32(len(allGroupIDs))
	if numGroups == 0 {
		numGroups = 1
	}
	
	// ✅ FIX CRÍTICO: Grupo = firstSN % numGroups
	segmentGroupID := uint32(firstSN % numGroups)
	
	// ✅ FIX: Processa apenas se este orderer gerencia este grupo
	if o.ownedGroupID != segmentGroupID {
		fmt.Printf("[MPX] runSegment: firstSN=%d belongs to group %d, skipping (ownedGroupID=%d)\n", firstSN, segmentGroupID, o.ownedGroupID)
		return
	}
	
	groupId := o.ownedGroupID
	fmt.Printf("[MPX] runSegment: firstSN=%d processing for group %d\n", firstSN, groupId)
	
	// ✅ Verifica se é membro do grupo antes de processar
	if groupId != 0 && !o.am.IsMember(groupId, membership.OwnID) {
		fmt.Printf("[MPX][SEGMENT] Skipping group %d (not a member)\n", groupId)
		return
	}
	
	members := o.am.GetGroupMembers(groupId)
	if members == nil {
		fmt.Printf("[MPX][CRITICAL] Group %d has NO members, skipping\n", groupId)
		return
	}
	fmt.Printf("[MPX][SEGMENT] Processing group %d with %d members: %v\n", groupId, len(members), members)
	
	groupLeader := o.am.GetGroupLeader(GroupID(groupId), seg.Leaders())
	fmt.Printf("[MPX][LEADER] Group %d leader=%d (ownID=%d, isLeader=%v)\n", groupId, groupLeader, membership.OwnID, groupLeader == membership.OwnID)
	
	// Apenas líder propõe (incluindo grupo 0)
	if groupLeader != membership.OwnID {
		fmt.Printf("[MPX][SKIP] Group %d: not leader (leader=%d, ownID=%d)\n", groupId, groupLeader, membership.OwnID)
		return
	}
	
	// Calculate bucketIndex: must match GroupId for correct bucket locking
	// ✅ FIX: bucketIndex = groupId (not array position)
	var bucketIdx int32 = int32(groupId)
	go func(gid uint32, bIdx int32) {
			t := time.NewTicker(o.proposeEvery)
			defer t.Stop()
			// ✅ FIX: currentSN já está correto no seg.FirstSN() (SN intercalado)
			// Cada segmento já começa no SN correto para seu grupo
			currentSN := seg.FirstSN()
			fmt.Printf("[MPX][TICKER] Started ticker for group %d, firstSN=%d, currentSN=%d, interval=%v\n", gid, seg.FirstSN(), currentSN, o.proposeEvery)
			for {
				select {
				case <-stopCh:
					return
				case <-t.C:
					if gid == 0 {
						fmt.Printf("[MPX][TICK-G0] sn=%d tick received for group 0\n", currentSN)
					}
					if currentSN > seg.LastSN() {
						continue
					}
					now := time.Now()
					if mirlog.GetEntry(currentSN) != nil {
						currentSN += numGroups
						continue
					}
					inst, ok := o.dispatcher.load(currentSN)
					if !ok || inst == nil {
						inst = o.ensureInstance(currentSN)
						inst.setSegment(seg)
						inst.bucketId = gid
						inst.bucketIndex = bIdx
						inst.SetMembers(members)
						o.dispatcher.store(currentSN, inst)
						inst.startWorkers(&o.stopWg)
						o.backlog.drainTo(currentSN, inst.enqueue)
						prep := &pb.MPxMsg{Type: &pb.MPxMsg_Prepare{
							Prepare: &pb.MPxPrepare{
								Id:      &pb.MPxInstanceId{Sn: currentSN, Lead: uint64(membership.OwnID)},
								Ballot:  uint64(inst.currentBallot),
								GroupId: gid,
							},
						}}
						pm := &pb.ProtocolMessage{
							SenderId: membership.OwnID,
							Sn:       currentSN,
							Msg:      &pb.ProtocolMessage_Multipaxos{Multipaxos: prep},
						}
						if o.emit != nil {
							// Líder processa seu próprio PREPARE localmente primeiro
							inst.enqueue(pm)
							// Depois envia para os outros membros do grupo
							o.emit(pm)
						}
						inst.prepSent = true
					} else {
						// Instância já existe, atualiza bucketIndex e bucketId se necessário
						inst.mu.Lock()
						if inst.bucketIndex < 0 {
							// ✅ FIX: bucketIndex = groupId (not array position)
							inst.bucketIndex = int32(gid)
							inst.bucketId = gid
							fmt.Printf("[MPX][INST] sn=%d updated bucketIndex=%d bucketId=%d\n", currentSN, int32(gid), gid)
						}
						inst.mu.Unlock()
					}
					inst.tick(now)
					inst.ProposeIfDue()
					fmt.Printf("[PROPOSE] Group %d: sn=%d tick+propose executed\n", gid, currentSN)
				}
			}
		}(groupId, bucketIdx)
}
func (o *MultiPaxosOrderer) killSegment(seg manager.Segment) {
	groupIDs := o.am.GetDefinedGroups()
	if len(groupIDs) == 0 {
		groupIDs = []uint32{0}
	}
	numGroups := int32(len(groupIDs))
	if numGroups == 0 {
		numGroups = 1
	}
	
	// ✅ FIX: killSegment deve processar apenas o grupo deste segmento
	// Cada segmento é específico para um grupo (SN intercalado)
	// Não deve iterar sobre todos os grupos
	
	// Aguarda até que o último SN do segmento seja commitado
	lastSN := seg.LastSN()
	timeout := time.After(30 * time.Second)
	for mirlog.GetEntry(lastSN) == nil {
		select {
		case <-timeout:
			logger.Warn().Int32("lastSN", lastSN).Msg("Timeout waiting for segment completion")
			return
		default:
			time.Sleep(10 * time.Millisecond)
		}
	}
	fmt.Printf("[MPX] Segment completed (lastSN=%d)\n", lastSN)
	
	// Aguarda checkpoint se necessário
	checkpoints := mirlog.Checkpoints()
	currentCheckpoint := mirlog.GetCheckpoint()
	checkpointTimeout := time.After(60 * time.Second)
	for currentCheckpoint == nil || currentCheckpoint.Sn < seg.LastSN() {
		select {
		case currentCheckpoint = <-checkpoints:
		case <-checkpointTimeout:
			logger.Warn().Int32("lastSN", seg.LastSN()).Msg("Timeout waiting for checkpoint")
			return
		}
	}
	
	o.instMu.Lock()
	if seg.LastSN() > o.last {
		atomic.StoreInt32(&o.last, seg.LastSN())
	}
	o.instMu.Unlock()
	logger.Info().Int("segID", seg.SegID()).Msg("Segment finished.")
}
func (o *MultiPaxosOrderer) ensureInstance(sn int32) *mpxInstance {
	inst := newMPXInstance(o, sn, o.announce, o.maxBatchSize, o.proposeEvery)
	if o.onInstanceCreated != nil {
		o.onInstanceCreated(sn)
	}
	return inst
}
func (o *MultiPaxosOrderer) LoadGroupsFromYAML(filename string) error {
	if o.am == nil {
		o.am = NewAtomicMulticast()
	}
	err := o.am.LoadGroupsFromYAML(filename)
	if err != nil {
		logger.Fatal().
			Err(err).
			Str("file", filename).
			Msg("FATAL: groups.yml é obrigatório para modo multicast. Determinismo quebrado sem ele.")
	}
	return err
}
func (o *MultiPaxosOrderer) Sign(data []byte) ([]byte, error) { return nil, nil }
func (o *MultiPaxosOrderer) CheckSig(data []byte, senderID int32, signature []byte) error {
	return nil
}
func extractGroupID(mpx *pb.MPxMsg) uint32 {
	switch msg := mpx.Type.(type) {
	case *pb.MPxMsg_Prepare:
		return msg.Prepare.GetGroupId()
	case *pb.MPxMsg_Promise:
		return msg.Promise.GetGroupId()
	case *pb.MPxMsg_Accept:
		return msg.Accept.GetGroupId()
	case *pb.MPxMsg_Accepted:
		return msg.Accepted.GetGroupId()
	case *pb.MPxMsg_Commit:
		return msg.Commit.GetGroupId()
	}
	return 0
}

// GetActiveInstance - Retorna instância ativa para o grupo
// ✅ LIVENESS: Usado para acordar líder quando request chega
func (o *MultiPaxosOrderer) GetActiveInstance(groupID uint32) *mpxInstance {
	o.firstSNMu.RLock()
	currentSN := o.currentFirstSN
	o.firstSNMu.RUnlock()
	
	if currentSN <= 0 {
		return nil // Nenhum segmento ativo
	}
	
	// Retorna instância do SN atual do grupo
	inst, ok := o.dispatcher.load(currentSN)
	if !ok || inst == nil {
		return nil
	}
	
	return inst
}
