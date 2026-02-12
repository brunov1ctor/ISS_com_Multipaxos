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
type AnnounceFn func(sn int32, batch *pb.Batch, metadata []byte)  // ✅ Changed from batchBytes []byte to batch *pb.Batch
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
	
	// ✅ CRITICAL: segmentChan já foi setado pelo multicast orderer (fan-out)
	// Não precisa chamar SubscribeOrderer() novamente
	if !o.skipHandlerRegistration {
		// Standalone: usa segment manager diretamente
		o.segmentChan = mgr.SubscribeOrderer()
		messenger.OrdererMsgHandler = o.HandleMessage
		fmt.Printf("[MPX] Standalone: subscribed to segments and registered handler\n")
	} else {
		// Multicast child: segmentChan já foi setado pelo parent
		fmt.Printf("[MPX] Multicast child: using parent's segment channel (ownedGroupID=%d)\n", o.ownedGroupID)
	}
	
	o.announce = func(sn int32, b *pb.Batch, metadata []byte) {
		if b == nil {
			fmt.Printf("[MPX][ANNOUNCE][ERR] sn=%d batch is nil\n", sn)
			return
		}
		if len(b.Requests) == 0 {
			fmt.Printf("[MPX][ANNOUNCE] sn=%d NOP (empty requests, announcing to log)\n", sn)
			shouldRespond := true
			batchBytes, _ := proto.Marshal(b)
			digest := crypto.Hash(batchBytes)
			now := time.Now().UnixNano()
			entry := &mirlog.Entry{
				Sn:            sn,
				Batch:         b,
				Digest:        digest,
				ShouldRespond: &shouldRespond,
				ProposeTs:     now,
				CommitTs:      now,
			}
			announcer.Announce(entry)
			return
		}
		var digest []byte
		if len(metadata) > 0 {
			digest = metadata
		} else {
			batchBytes, _ := proto.Marshal(b)
			digest = crypto.Hash(batchBytes)
		}
		shouldRespond := true
		if len(b.Requests) > 0 && o.am != nil {
			groupId := b.Requests[0].GetGroupId()
			shouldRespond = o.am.IsMember(groupId, membership.OwnID)
			fmt.Printf("[MPX][ANNOUNCE] sn=%d groupId=%d ownID=%d shouldRespond=%v\n", sn, groupId, membership.OwnID, shouldRespond)
		} else {
			fmt.Printf("[MPX][ANNOUNCE] sn=%d NO REQUESTS or NO AM, shouldRespond=%v\n", sn, shouldRespond)
		}
		now := time.Now().UnixNano()
		entry := &mirlog.Entry{
			Sn:             sn,
			Batch:          b,
			Digest:         digest,
			ShouldRespond:  &shouldRespond,
			ProposeTs:      now,
			CommitTs:       now,
		}
		announcer.Announce(entry)
		if len(b.Requests) > 0 && GetGlobalMulticastOrderer() != nil {
			if len(b.Requests[0].TouchedGroups) > 1 && b.Requests[0].GSN > 0 {
				gsn := b.Requests[0].GSN
				GetGlobalMulticastOrderer().RegisterGSNMetadata(gsn, b.Requests[0].TouchedGroups)
				fmt.Printf("[META-STREAM][REGISTERED] sn=%d GSN %d -> groups %v\n", sn, gsn, b.Requests[0].TouchedGroups)
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
		
		// ✅ CRITICAL: Usa segment manager como PBFT
		if o.segmentChan != nil {
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
			fmt.Printf("[MPX] No segment channel, orderer will not process segments\n")
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
		// ✅ FIX: Valida se REMETENTE é membro do grupo (não o receptor)
		// Isso permite que líderes recebam mensagens de seguidores de outros grupos
		if groupID != 0 && o.am != nil {
			members := o.am.GetGroupMembers(groupID)
			if members != nil {
				isSenderMember := false
				for _, m := range members {
					if m == pm.SenderId {
						isSenderMember = true
						break
					}
				}
				if !isSenderMember {
					fmt.Printf("[MPX][FILTER] sn=%d sender=%d not member of group=%d, dropping\n", sn, pm.SenderId, groupID)
					return
				}
			}
		}
	}
	inst, ok := o.dispatcher.load(sn)
	if !ok || inst == nil {
		inst = o.ensureInstance(sn)
		// Seta bucketId baseado na mensagem
		if mpx != nil {
			inst.bucketId = groupID
			fmt.Printf("[MPX][INST] sn=%d set bucketId=%d from message\n", sn, groupID)
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
	// ✅ FIX CRÍTICO: Verifica grupo ANTES de cancelar segmento anterior
	// Evita race condition onde segmento de outro grupo mata ticker do grupo correto
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
	
	// ✅ FIX CRÍTICO: Com interleaved SN, TODOS os grupos processam TODOS os segmentos
	// Cada grupo pega seus SNs específicos (grupo 1 pega SNs 1,6,11... grupo 2 pega 2,7,12...)
	// A verificação de "qual grupo processa qual segmento" estava ERRADA e causava deadlock
	fmt.Printf("[MPX] runSegment: firstSN=%d processing for group %d (interleaved SN mode)\n", firstSN, o.ownedGroupID)
	
	// ✅ Agora sim: cancela segmento anterior (apenas do MESMO grupo)
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
	
	groupId := o.ownedGroupID
	fmt.Printf("[MPX] runSegment: firstSN=%d processing for group %d\n", firstSN, groupId)
	
	members := o.am.GetGroupMembers(groupId)
	if members == nil {
		fmt.Printf("[MPX][CRITICAL] Group %d has NO members, skipping\n", groupId)
		return
	}
	fmt.Printf("[MPX][SEGMENT] Processing group %d with %d members: %v\n", groupId, len(members), members)
	
	// ✅ FIX: Líder determinístico do grupo (não depende de seg.Leaders())
	// Usa round-robin dentro do grupo: round = firstSN / numGroups
	groupLeader := o.am.GetGroupLeaderForSegment(groupId, seg.FirstSN(), numGroups)
	fmt.Printf("[MPX][LEADER] Group %d leader=%d (ownID=%d, isLeader=%v)\n", 
		groupId, groupLeader, membership.OwnID, groupLeader == membership.OwnID)
	
	// Don't skip non-leaders - all members must process segments
	// Only the leader will propose (check happens inside ProposeIfDue)
	// Non-leaders still need to receive messages, vote, and commit
	
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
						fmt.Printf("[MPX][TICK] sn=%d already in log, skipping (advancing by %d)\n", currentSN, numGroups)
						currentSN += numGroups
						continue
					}
					inst, ok := o.dispatcher.load(currentSN)
					if !ok || inst == nil {
						inst = o.ensureInstance(currentSN)
						inst.setSegment(seg)
						inst.bucketId = gid
						inst.SetMembers(members)
						o.dispatcher.store(currentSN, inst)
						inst.startWorkers(&o.stopWg)
						o.backlog.drainTo(currentSN, inst.enqueue)
					fmt.Printf("[MPX][INST] sn=%d created for group %d, leader=%d\n", currentSN, gid, inst.leader)
				} else {
						// Instância já existe, atualiza bucketIndex e bucketId se necessário
						inst.mu.Lock()
						if inst.bucketId == 0 {
							inst.bucketId = gid
							fmt.Printf("[MPX][INST] sn=%d updated bucketId=%d\n", currentSN, gid)
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