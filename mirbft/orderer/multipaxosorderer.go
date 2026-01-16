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
	mirlog "github.com/hyperledger-labs/mirbft/log"
	"github.com/hyperledger-labs/mirbft/manager"
	"github.com/hyperledger-labs/mirbft/membership"
	"github.com/hyperledger-labs/mirbft/messenger"
	"github.com/hyperledger-labs/mirbft/request"
	pb "github.com/hyperledger-labs/mirbft/protobufs"
	"github.com/hyperledger-labs/mirbft/tracing"
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
			fmt.Printf("[MPX][NIL] DELIVER ⊥ sn=%d\n", sn)
			tracing.MainTrace.Event(tracing.COMMIT, int64(sn), 0)
			// Cria entrada vazia no log para não travar checkpoint/GC
			emptyBatch := &pb.Batch{Requests: []*pb.ClientRequest{}}
			shouldRespond := false
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
		
		// Apenas o primeiro peer responde para evitar fan-out
		shouldRespond := (membership.OwnID == membership.AllNodeIDs()[0])
		
		entry := &mirlog.Entry{
			Sn:             sn,
			Batch:          &b,
			ShouldRespond:  &shouldRespond,
		}
		announcer.Announce(entry)

		fmt.Printf("SB-DELIVER sn=%d size=%d\n", sn, len(b.Requests))
		fmt.Printf("COMMIT sn=%d size=%d\n", sn, len(b.Requests))
		fmt.Printf("DELIVER sn=%d delivered=%d\n", sn, len(b.Requests))

		tracing.MainTrace.Event(tracing.COMMIT, int64(sn), int64(len(b.Requests)))
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
					
					// Nó não é membro: skip direto sem criar instância ativa
					if !isMember {
						// Calcula próximo globalSN deste grupo de forma determinística
						// Usa mirlog para verificar se já foi processado (não dispatcher)
						
						// Encontra o próximo globalSN não processado deste grupo
						var nextGlobalSN int32 = -1
						for candidateSN := seg.FirstSN(); candidateSN <= seg.LastSN(); candidateSN++ {
							// Verifica se este globalSN pertence a este grupo
							offset := candidateSN - seg.FirstSN()
							if offset >= 0 && offset%numGroups == groupPos[groupId] {
								// Verifica se já foi commitado no log (fonte de verdade)
								if mirlog.GetEntry(candidateSN) == nil {
									nextGlobalSN = candidateSN
									break
								}
							}
						}
						
						if nextGlobalSN >= 0 {
							// Entrega NIL diretamente (sem instância ativa)
							go func(sn int32) {
								o.announce(sn, []byte{}, []byte{})
							}(nextGlobalSN)
							fmt.Printf("[MPX][SKIP] sn=%d groupId=%d (nó não é membro)\n", nextGlobalSN, groupId)
						}
						mu.Unlock()
						continue
					}
					
					if !isGroupLeader {
						mu.Unlock()
						continue
					}
					
					groupSN := o.am.NextSN(GroupID(groupId))
					if groupSN < 0 {
						mu.Unlock()
						fmt.Printf("[MPX][SEG] groupId=%d: contador não inicializado, pulando\n", groupId)
						continue
					}
					
					// Usa índice do grupo (não groupId cru) para evitar pulos de SN
					globalSN := seg.FirstSN() + groupSN*numGroups + groupPos[groupId]
					
					if globalSN > seg.LastSN() {
						mu.Unlock()
						fmt.Printf("[MPX][SEG] sn=%d exceeds segment LastSN=%d, skipping\n", globalSN, seg.LastSN())
						continue
					}
					
					inst = o.ensureInstance(globalSN)
					inst.setSegment(seg)
					inst.bucketId = groupId
					// bucketIndex usa posição ordenada (não groupId cru)
					inst.bucketIndex = groupPos[groupId]
					
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
							Members: members,
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
				
				inst.tick(now)
				
				if isGroupLeader {
					inst.ProposeIfDue()
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

