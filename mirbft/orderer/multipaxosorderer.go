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
		o.am = NewAtomicMulticast()
		// ✅ FIX: Só seta ownedGroupID=0 se ainda não foi configurado
		if o.ownedGroupID == 0 {
			o.ownedGroupID = 0 // Orderer standalone gerencia todos os grupos
		}
		fmt.Printf("[MPX] Init: created new AtomicMulticast (ownedGroupID=%d)\n", o.ownedGroupID)
	} else {
		fmt.Printf("[MPX] Init: reusing injected AtomicMulticast (groups=%v, ownedGroupID=%d)\n", o.am.GetDefinedGroups(), o.ownedGroupID)
	}
	o.emit = func(pm *pb.ProtocolMessage) {
		mpx := pm.GetMultipaxos()
		var groupID uint32
		if mpx == nil {
			// Envia para todos incluindo si mesmo
			for _, nid := range membership.AllNodeIDs() {
				messenger.EnqueueMsg(pm, nid)
			}
			return
		}
		groupID = extractGroupID(mpx)
		if groupID == 0 {
			// Grupo 0: envia para todos incluindo si mesmo
			for _, nid := range membership.AllNodeIDs() {
				messenger.EnqueueMsg(pm, nid)
			}
			return
		}
		if o.am != nil {
			members := o.am.GetGroupMembers(groupID)
			if members != nil && len(members) > 0 {
				for _, nodeID := range members {
					messenger.EnqueueMsg(pm, nodeID)  // ✅ Envia para TODOS incluindo si mesmo
				}
				return
			}
		}
		for _, nid := range membership.AllNodeIDs() {
			messenger.EnqueueMsg(pm, nid)  // ✅ Envia para TODOS incluindo si mesmo
		}
	}
	o.segmentChan = o.mgr.SubscribeOrderer()
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
		if len(b.Requests) > 0 {
			groupId := b.Requests[0].GetGroupId()
			if groupId != 0 && o.am != nil {
				shouldRespond = o.am.IsMember(groupId, membership.OwnID)
			}
		}
		entry := &mirlog.Entry{
			Sn:             sn,
			Batch:          &b,
			Digest:         digest,
			ShouldRespond:  &shouldRespond,
		}
		announcer.Announce(entry)
		
		// ✅ META stream: Processa META commitado pelo grupo 0
		if len(b.Requests) > 0 && GetGlobalMulticastOrderer() != nil {
			for _, req := range b.Requests {
				if req.GroupId == 0 && isMETAStream(req) {
					gsn := req.GSN
					touchedGroups := req.TouchedGroups
					GetGlobalMulticastOrderer().RegisterGSNMetadata(gsn, touchedGroups)
					fmt.Printf("[META-STREAM][REGISTERED] GSN %d -> groups %v\n", gsn, touchedGroups)
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
		fmt.Printf("[MPX] Start begin\n")
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
	
	// ✅ FIX: Obtém lista ordenada de grupos (sempre inclui grupo 0)
	allGroupIDs := o.am.GetDefinedGroups()
	if len(allGroupIDs) == 0 {
		allGroupIDs = []uint32{0}
	} else if allGroupIDs[0] != 0 {
		// Garante grupo 0 no início se não estiver
		allGroupIDs = append([]uint32{0}, allGroupIDs...)
	}
	
	// Identifica grupo do segmento pelo firstSN (SN intercalado)
	firstSN := seg.FirstSN()
	numGroups := int32(len(allGroupIDs))
	if numGroups == 0 {
		numGroups = 1
	}
	segmentGroupIdx := firstSN % numGroups
	if segmentGroupIdx < 0 || segmentGroupIdx >= int32(len(allGroupIDs)) {
		fmt.Printf("[MPX] Invalid segment groupIdx=%d for firstSN=%d, skipping\n", segmentGroupIdx, firstSN)
		return
	}
	segmentGroupID := allGroupIDs[segmentGroupIdx]
	fmt.Printf("[MPX] runSegment: firstSN=%d belongs to group %d (idx=%d of %d groups)\n", firstSN, segmentGroupID, segmentGroupIdx, numGroups)
	
	var groupsToProcess []uint32
	// ✅ FIX: Processa segmento se for membro do grupo (ignora ownedGroupID)
	// Cada nó pode ser membro de múltiplos grupos
	if segmentGroupID == 0 || o.am.IsMember(segmentGroupID, membership.OwnID) {
		groupsToProcess = []uint32{segmentGroupID}
		fmt.Printf("[MPX] runSegment: processing segment for group %d (member)\n", segmentGroupID)
	} else {
		fmt.Printf("[MPX] runSegment: skipping segment for group %d (not a member)\n", segmentGroupID)
		return
	}
	
	for _, groupId := range groupsToProcess {
		members := o.am.GetGroupMembers(groupId)
		if members == nil {
			fmt.Printf("[MPX][CRITICAL] Group %d has NO members, skipping\n", groupId)
			continue
		}
		fmt.Printf("[MPX][SEGMENT] Processing group %d with %d members: %v\n", groupId, len(members), members)
		if groupId != 0 && !o.am.IsMember(groupId, membership.OwnID) {
			fmt.Printf("[MPX][SEGMENT] Skipping group %d (not a member)\n", groupId)
			continue
		}
		groupLeader := o.am.GetGroupLeader(GroupID(groupId), seg.Leaders())
		fmt.Printf("[MPX][LEADER] Group %d leader=%d (ownID=%d, isLeader=%v)\n", groupId, groupLeader, membership.OwnID, groupLeader == membership.OwnID)
		
		// Apenas líder propõe (incluindo grupo 0)
		if groupLeader != membership.OwnID {
			fmt.Printf("[MPX][SKIP] Group %d: not leader (leader=%d, ownID=%d)\n", groupId, groupLeader, membership.OwnID)
			continue
		}
		var groupIdx int32 = segmentGroupIdx
		// Calculate bucketIndex: must match GroupId for correct bucket locking
		// ✅ FIX: bucketIndex = groupId (not array position)
		var bucketIdx int32 = int32(groupId)
		go func(gid uint32, gIdx int32, bIdx int32) {
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
				}
			}
		}(groupId, groupIdx, bucketIdx)
	}
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
