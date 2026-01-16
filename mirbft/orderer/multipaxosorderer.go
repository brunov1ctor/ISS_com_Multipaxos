package orderer

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/hyperledger-labs/mirbft/announcer"
	"github.com/hyperledger-labs/mirbft/config"
	mirlog "github.com/hyperledger-labs/mirbft/log"
	"github.com/hyperledger-labs/mirbft/manager"
	"github.com/hyperledger-labs/mirbft/membership"
	"github.com/hyperledger-labs/mirbft/messenger"
	"github.com/hyperledger-labs/mirbft/request"
	pb "github.com/hyperledger-labs/mirbft/protobufs"
	logger "github.com/rs/zerolog/log"
)

type AnnounceFn func(sn int32, batchBytes []byte, metadata []byte)

// mpxDispatcher gerencia mapa thread-safe de instâncias Paxos
// Cada SN (sequence number) tem sua própria instância
type mpxDispatcher struct {
	mm sync.Map // map[int32]*mpxInstance
}

func (d *mpxDispatcher) load(sn int32) (*mpxInstance, bool) {
	if v, ok := d.mm.Load(sn); ok {
		return v.(*mpxInstance), true
	}
	return nil, false
}
func (d *mpxDispatcher) store(sn int32, inst *mpxInstance) { d.mm.Store(sn, inst) }
func (d *mpxDispatcher) delete(sn int32)                    { d.mm.Delete(sn) }

// mpxBacklog armazena mensagens que chegaram antes da instância ser criada
type mpxBacklog struct {
	mu sync.Mutex
	qs map[int32][]*pb.ProtocolMessage
}

func newMPXBacklog() mpxBacklog {
	return mpxBacklog{qs: make(map[int32][]*pb.ProtocolMessage)}
}

// drainTo entrega todas mensagens pendentes para uma instância
func (b *mpxBacklog) drainTo(sn int32, f func(*pb.ProtocolMessage)) {
	b.mu.Lock()
	items := b.qs[sn]
	delete(b.qs, sn)
	b.mu.Unlock()
	for _, m := range items {
		f(m)
	}
}

// MultiPaxosOrderer implementa consenso Multi-Paxos com suporte a grupos
// Permite paralelismo: cada grupo executa Paxos independentemente
type MultiPaxosOrderer struct {
	mgr         manager.Manager
	segmentChan chan manager.Segment

	dispatcher mpxDispatcher // Mapa de instâncias ativas
	backlog    mpxBacklog    // Mensagens que chegaram cedo
	last       int32         // Último SN processado

	instMu    sync.RWMutex
	startOnce sync.Once

	emit     func(pm *pb.ProtocolMessage) // Função para enviar mensagens
	announce AnnounceFn                    // Função para entregar batches

	maxBatchSize     int
	proposeEvery     time.Duration // Intervalo entre propostas
	stopWg           sync.WaitGroup
	sbNilAfter       time.Duration
	enableNilDeliver bool
	
	onInstanceCreated func(sn int32) // Callback quando instância é criada
	
	am *AtomicMulticast // Gerenciador de grupos
	
	segmentInstances sync.Map // Rastreia instâncias por segmento
	
	// CSMR: Barreira global para atomic global order
	lastGlobalSN     int32 // Último SN decidido no bucket 0 (global)
	observedGlobalSN int32 // Último SN global já entregue localmente
	globalPending    bool  // Flag: existe global pendente (freeze propostas locais)
	globalMu         sync.RWMutex
	
	// CSMR: Contador de instâncias locais em voo por grupo (para drenagem)
	inflightLocal map[uint32]int32 // groupId -> contador de instâncias ativas
	inflightMu    sync.RWMutex
}

// Init inicializa o orderer Multi-Paxos
// Configura parâmetros, funções de comunicação e gerenciamento de grupos
func (o *MultiPaxosOrderer) Init(mgr manager.Manager) {
	o.mgr = mgr
	o.backlog = newMPXBacklog()
	o.last = -1

	o.maxBatchSize = int(config.Config.BatchSize)
	o.proposeEvery = time.Duration(config.Config.BatchTimeout)

	o.sbNilAfter = o.proposeEvery * 3
	o.enableNilDeliver = true
	
	// Inicializa atomic multicast
	o.am = NewAtomicMulticast()
	o.inflightLocal = make(map[uint32]int32)

	// Função emit padrão: broadcast para todos os nós
	o.emit = func(pm *pb.ProtocolMessage) {
		for _, nid := range membership.AllNodeIDs() {
			if nid == membership.OwnID {
				continue
			}
			messenger.EnqueueMsg(pm, nid)
		}
	}
	o.segmentChan = o.mgr.SubscribeOrderer()
	messenger.OrdererMsgHandler = o.HandleMessage

	// Função announce padrão: entrega batch commitado à aplicação
	o.announce = func(sn int32, batchBytes []byte, _ []byte) {
		if len(batchBytes) == 0 {
			// CSMR: NÃO entrega NIL para nós não-membros
			// Apenas cria entrada vazia no log para checkpoint/GC
			fmt.Printf("[MPX][SKIP] sn=%d (empty batch, não entrega)\n", sn)
			emptyBatch := &pb.Batch{Requests: []*pb.ClientRequest{}}
			allNodes := append([]int32(nil), membership.AllNodeIDs()...)
			sort.Slice(allNodes, func(i, j int) bool { return allNodes[i] < allNodes[j] })
			n := len(allNodes)
			f := (n - 1) / 3
			q := f + 1
			if q < 1 {
				q = 1
			}
			if q > n {
				q = n
			}
			shouldRespond := false
			for i := 0; i < q; i++ {
				if membership.OwnID == allNodes[i] {
					shouldRespond = true
					break
				}
			}
			entry := &mirlog.Entry{
				Sn:            sn,
				Batch:         emptyBatch,
				ShouldRespond: &shouldRespond,
			}
			announcer.Announce(entry)
			return
		}
		
		var b pb.Batch
		if err := proto.Unmarshal(batchBytes, &b); err != nil {
			fmt.Printf("[MPX][ANNOUNCE][ERR] sn=%d unmarshal: %v\n", sn, err)
			return
		}
		
		// CSMR: Atualiza barreira global se for bucket 0
		// Bucket 0 = operações multi-grupo (atomic global order)
		if len(b.Requests) > 0 && b.Requests[0].GetGroupId() == 0 {
			o.globalMu.Lock()
			if sn > o.lastGlobalSN {
				o.lastGlobalSN = sn
				o.globalPending = true // Congela propostas locais
				fmt.Printf("[CSMR][BARRIER] Global barrier updated: sn=%d (freeze local)\n", sn)
			}
			// CSMR: Só atualiza observedGlobalSN se não há locais em voo
			o.inflightMu.RLock()
			totalInflight := int32(0)
			for _, count := range o.inflightLocal {
				totalInflight += count
			}
			o.inflightMu.RUnlock()
			
			if totalInflight == 0 && sn > o.observedGlobalSN {
				o.observedGlobalSN = sn
				o.globalPending = false // Descongela
				fmt.Printf("[CSMR][BARRIER] Global observed: sn=%d (unfreeze)\n", sn)
			} else if totalInflight > 0 {
				fmt.Printf("[CSMR][DRAIN] Waiting for %d inflight local instances\n", totalInflight)
			}
			o.globalMu.Unlock()
		} else {
			// CSMR: Decrementa contador de inflight para grupos locais
			if len(b.Requests) > 0 {
				groupId := b.Requests[0].GetGroupId()
				if groupId != 0 {
					o.inflightMu.Lock()
					if o.inflightLocal[groupId] > 0 {
						o.inflightLocal[groupId]--
					}
					// CSMR: Reavaliar se pode descongelar (drain completo)
					totalInflight := int32(0)
					for _, count := range o.inflightLocal {
						totalInflight += count
					}
					o.inflightMu.Unlock()
					
					// Se drenagem completa e global pendente, descongela
					if totalInflight == 0 {
						o.globalMu.Lock()
						if o.globalPending {
							o.observedGlobalSN = o.lastGlobalSN
							o.globalPending = false
							fmt.Printf("[CSMR][UNFREEZE] Drain complete, unfreeze (observed=%d)\n", o.observedGlobalSN)
						}
						o.globalMu.Unlock()
					}
				}
			}
		}
		
		// f+1 peers respondem para garantir quorum no cliente (>= f+1 respostas)
		// Ordenação determinística para garantir mesmo conjunto em todos os nós
		allNodes := append([]int32(nil), membership.AllNodeIDs()...)
		sort.Slice(allNodes, func(i, j int) bool { return allNodes[i] < allNodes[j] })
		n := len(allNodes)
		f := (n - 1) / 3
		q := f + 1
		if q < 1 {
			q = 1
		}
		if q > n {
			q = n
		}
		// Seleção determinística: primeiros q NodeIDs ordenados
		shouldRespond := false
		for i := 0; i < q; i++ {
			if membership.OwnID == allNodes[i] {
				shouldRespond = true
				break
			}
		}
		
		entry := &mirlog.Entry{
			Sn:             sn,
			Batch:          &b,
			ShouldRespond:  &shouldRespond,
		}
		
		announcer.Announce(entry)

		fmt.Printf("SB-DELIVER sn=%d size=%d\n", sn, len(b.Requests))
		fmt.Printf("COMMIT sn=%d size=%d\n", sn, len(b.Requests))
		fmt.Printf("DELIVER sn=%d delivered=%d\n", sn, len(b.Requests))
		// Nota: tracing.COMMIT é registrado por mirlog.CommitEntry(), não duplicar aqui
	}

	fmt.Printf("[MPX] Init ok; cfg: batchSize=%d batchTimeout=%s leaderPolicy=%s\n",
		o.maxBatchSize, o.proposeEvery, strings.ToLower(config.Config.LeaderPolicy))
}

// Start inicia processamento de segmentos
// Cada segmento contém um range de SNs a serem ordenados
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

// HandleMessage processa mensagens Paxos recebidas
// Cria instância on-demand se necessário (para followers)
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

	inst, ok := o.dispatcher.load(sn)
	if !ok || inst == nil {
		// Criação on-demand: followers criam instância ao receber mensagens
		inst = o.ensureInstance(sn)
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

// runSegment executa consenso para um segmento
// Cria instâncias Paxos para cada grupo/bucket em paralelo
// Líder de cada grupo propõe batches independentemente
func (o *MultiPaxosOrderer) runSegment(seg manager.Segment) {
	activeInstances := make(map[uint32]*mpxInstance)
	var mu sync.Mutex
	
	// Obtém grupos definidos (do YAML ou padrão)
	groupIDs := o.am.GetDefinedGroups()
	if len(groupIDs) == 0 {
		groupIDs = []uint32{0} // Fallback: só grupo global
	}
	
	// FAIL-FAST: valida configuração antes de iniciar segmento
	if len(groupIDs) > len(request.Buckets) {
		logger.Fatal().
			Int("numGroups", len(groupIDs)).
			Int("numBuckets", len(request.Buckets)).
			Msg("FATAL: numGroups > NumBuckets. Increase config.NumBuckets or reduce groups in groups.yml")
		return
	}
	
	numGroups := int32(len(groupIDs))
	
	// Mapa groupId -> índice na lista ordenada (para cálculo correto do globalSN)
	groupPos := make(map[uint32]int32)
	for i, gid := range groupIDs {
		groupPos[gid] = int32(i)
	}
	
	go func() {
		t := time.NewTicker(o.proposeEvery)
		defer t.Stop()
		
		lastGroupIdx := 0
		localGroupPrepared := make(map[uint32]bool)
		
		for range t.C {
			now := time.Now()
			
			// CSMR: Freeze antecipado se bucket 0 tem requests pendentes
			if len(request.Buckets) > 0 && request.Buckets[0].Len() > 0 {
				o.globalMu.Lock()
				if !o.globalPending {
					o.globalPending = true
					fmt.Printf("[CSMR][FREEZE] Bucket 0 has pending requests, freeze local proposals\n")
				}
				o.globalMu.Unlock()
			}
			
			for offset := 0; offset < len(groupIDs); offset++ {
				idx := (lastGroupIdx + offset) % len(groupIDs)
				groupId := groupIDs[idx]
				
				segmentLeaders := seg.Leaders()
				groupLeader := o.am.GetGroupLeader(GroupID(groupId), segmentLeaders)
				isGroupLeader := (groupLeader == membership.OwnID)
				
				mu.Lock()
				inst := activeInstances[groupId]
				
				if inst == nil || inst.isClosed() {
					// Verifica se este nó é membro do grupo
					members := o.am.GetGroupMembers(groupId)
					if members == nil {
						mu.Unlock()
						fmt.Printf("[MPX][SEG] groupId=%d não existe, pulando\n", groupId)
						continue
					}
					
					isMember := false
					for _, m := range members {
						if m == membership.OwnID {
							isMember = true
							break
						}
					}
					
					// CSMR: Nó não é membro - NÃO entrega NIL
					// Apenas skip (não participa, não entrega)
					if !isMember {
						mu.Unlock()
						fmt.Printf("[CSMR][SKIP] groupId=%d: nó não é membro, não entrega\n", groupId)
						continue
					}
					
					if !isGroupLeader {
						mu.Unlock()
						continue
					}
					
					// CSMR: Não propor local se global pendente (freeze)
					if groupId != 0 {
						o.globalMu.RLock()
						pending := o.globalPending
						o.globalMu.RUnlock()
						if pending {
							mu.Unlock()
							continue // Aguarda drenagem do global
						}
					}
					
					// Calcula próximo globalSN deste grupo por varredura (sem contador global)
					// Isso garante que cada segmento começa do zero, sem pular SNs
					var nextGlobalSN int32 = -1
					for candidateSN := seg.FirstSN(); candidateSN <= seg.LastSN(); candidateSN++ {
						offset := candidateSN - seg.FirstSN()
						if offset >= 0 && offset%numGroups == groupPos[groupId] {
							if mirlog.GetEntry(candidateSN) == nil {
								nextGlobalSN = candidateSN
								break
							}
						}
					}
					
					if nextGlobalSN < 0 {
						mu.Unlock()
						fmt.Printf("[MPX][SEG] groupId=%d: todos SNs já processados\n", groupId)
						continue
					}
					
					globalSN := nextGlobalSN
					inst = o.ensureInstance(globalSN)
					inst.setSegment(seg)
					inst.bucketId = groupId
					inst.bucketIndex = groupPos[groupId]
					
					// CSMR: Incrementa contador de inflight para grupos locais
					if groupId != 0 {
						o.inflightMu.Lock()
						o.inflightLocal[groupId]++
						o.inflightMu.Unlock()
						inst.countedInflight = true // Marca para evitar dupla contagem
					}
					
					// FAIL-FAST: configuração inválida = abort segment
					if int(inst.bucketIndex) >= len(request.Buckets) {
						mu.Unlock()
						logger.Fatal().
							Int32("bucketIndex", inst.bucketIndex).
							Int("numBuckets", len(request.Buckets)).
							Uint32("groupId", groupId).
							Int("numGroups", len(groupIDs)).
							Msg("FATAL: bucketIndex >= NumBuckets. Fix groups.yml or config.NumBuckets")
						return
					}
					
					inst.SetMembers(members)
					
					if len(members) < len(membership.AllNodeIDs()) {
						fmt.Printf("[MPX][MULTICAST] sn=%d groupId=%d SELECTIVE members=%v quorum=%d\n", 
							globalSN, groupId, members, len(members)/2+1)
					} else {
						fmt.Printf("[MPX][BROADCAST] sn=%d groupId=%d ALL_NODES members=%v quorum=%d\n", 
							globalSN, groupId, members, len(members)/2+1)
					}
					
					if !localGroupPrepared[groupId] {
						localGroupPrepared[groupId] = true
					}
					
					prep := &pb.MPxMsg{Type: &pb.MPxMsg_Prepare{
						Prepare: &pb.MPxPrepare{
							Id:      &pb.MPxInstanceId{Sn: globalSN, Lead: uint64(membership.OwnID)},
							Ballot:  uint64(inst.currentBallot),
							GroupId: groupId,
						},
					}}
					pm := &pb.ProtocolMessage{
						SenderId: membership.OwnID,
						Sn:       globalSN,
						Msg:      &pb.ProtocolMessage_Multipaxos{Multipaxos: prep},
					}
					if o.emit != nil {
						o.emit(pm)
					}
					
					inst.prepSent = true
					inst.enableNilDeliver = o.enableNilDeliver
					inst.sbNilAfter = o.sbNilAfter
					
					o.dispatcher.store(globalSN, inst)
					inst.startWorkers(&o.stopWg)
					o.backlog.drainTo(globalSN, inst.enqueue)
					
					activeInstances[groupId] = inst
					
					if existing, loaded := o.segmentInstances.LoadOrStore(seg.SegID(), []*mpxInstance{inst}); loaded {
						o.segmentInstances.Store(seg.SegID(), append(existing.([]*mpxInstance), inst))
					}
				}
				mu.Unlock()
				
				// CSMR: Verifica barreira global antes de processar grupo local
				if groupId != 0 { // Não é bucket global
					o.globalMu.RLock()
					lastGlobal := o.lastGlobalSN
					observedGlobal := o.observedGlobalSN
					o.globalMu.RUnlock()
					
					if observedGlobal < lastGlobal {
						// Espera barreira global ser observada
						fmt.Printf("[CSMR][WAIT] groupId=%d aguarda barreira global (observed=%d < last=%d)\n", 
							groupId, observedGlobal, lastGlobal)
						continue
					}
				}
				
				inst.tick(now)
				
				if isGroupLeader {
					// Só propõe se houver requests pendentes no bucket
					if request.Buckets[inst.bucketIndex].Len() > 0 {
						inst.ProposeIfDue()
					}
				}
				
				lastGroupIdx = (idx + 1) % len(groupIDs)
				break
			}
		}
	}()
	
	// Cleanup: fecha instâncias quando segmento termina
	go func() {
		checkpoints := mirlog.Checkpoints()
		currentCheckpoint := mirlog.GetCheckpoint()
		for currentCheckpoint == nil || currentCheckpoint.Sn < seg.LastSN() {
			currentCheckpoint = <-checkpoints
		}
		
		if val, ok := o.segmentInstances.Load(seg.SegID()); ok {
			instList := val.([]*mpxInstance)
			for _, inst := range instList {
				if inst != nil {
					inst.stopWorkers()
					o.dispatcher.delete(inst.sn)
				}
			}
			o.segmentInstances.Delete(seg.SegID())
		}
	}()
}

func (o *MultiPaxosOrderer) killSegment(seg manager.Segment) {
	checkpoints := mirlog.Checkpoints()
	currentCheckpoint := mirlog.GetCheckpoint()
	for currentCheckpoint == nil || currentCheckpoint.Sn < seg.LastSN() {
		currentCheckpoint = <-checkpoints
	}
	mirlog.WaitForEntry(seg.LastSN())

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

func (o *MultiPaxosOrderer) Sign(data []byte) ([]byte, error) { return nil, nil }
func (o *MultiPaxosOrderer) CheckSig(data []byte, senderID int32, signature []byte) error {
	return nil
}

