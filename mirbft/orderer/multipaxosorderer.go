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
	pb "github.com/hyperledger-labs/mirbft/protobufs"
	"github.com/hyperledger-labs/mirbft/tracing"
	logger "github.com/rs/zerolog/log"
)

type AnnounceFn func(sn int32, batchBytes []byte, metadata []byte)

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

type mpxBacklog struct {
	mu   sync.Mutex
	qs   map[int32][]*pb.ProtocolMessage
	gcCh chan int32
}

func newMPXBacklog() mpxBacklog {
	return mpxBacklog{
		qs:   make(map[int32][]*pb.ProtocolMessage),
		gcCh: make(chan int32, 1),
	}
}
func (b *mpxBacklog) add(msg *pb.ProtocolMessage) {
	b.mu.Lock()
	b.qs[msg.Sn] = append(b.qs[msg.Sn], msg)
	b.mu.Unlock()
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

type MultiPaxosOrderer struct {
	mgr         manager.Manager
	segmentChan chan manager.Segment

	dispatcher mpxDispatcher
	backlog    mpxBacklog
	last       int32

	instances map[int32]*mpxInstance
	instMu    sync.RWMutex
	startOnce sync.Once

	emit     func(pm *pb.ProtocolMessage)
	announce AnnounceFn

	maxBatchSize     int
	proposeEvery     time.Duration
	view             int32
	stopWg           sync.WaitGroup
	sbNilAfter       time.Duration
	enableNilDeliver bool
	
	onInstanceCreated func(sn int32)
	
	// === PARALELISMO POR GRUPO ===
	// Atomic multicast: cada grupo tem contador de SN independente
	am *AtomicMulticast
	
	// === PHASE 1 AMORTIZADA ===
	// REMOVIDO: cache global substituído por cache local por segmento em runSegment
	// Reseta automaticamente a cada novo segmento (troca de líder)
	// groupPrepared sync.Map // DEPRECATED
	
	// === CLEANUP CORRETO ===
	// Rastreia instâncias criadas por cada segmento para fechar corretamente
	segmentInstances sync.Map // map[int][]*mpxInstance
}

func isSegmentLeader(seg manager.Segment, ownID int32, view int32) bool {
	leaders := seg.Leaders()
	if len(leaders) == 0 {
		return false
	}
	idx := int(view) % len(leaders)
	return leaders[idx] == ownID
}

func (o *MultiPaxosOrderer) Init(mgr manager.Manager) {
	o.mgr = mgr
	o.instances = make(map[int32]*mpxInstance)
	o.backlog = newMPXBacklog()
	o.last = -1

	o.maxBatchSize = int(config.Config.BatchSize)
	o.proposeEvery = time.Duration(config.Config.BatchTimeout)
	o.view = 0

	o.sbNilAfter = o.proposeEvery * 3
	o.enableNilDeliver = true
	
	// Inicializa atomic multicast com contadores por grupo
	o.am = NewAtomicMulticast()

	// emit padrão - broadcast simples
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

	// announce padrão
	o.announce = func(sn int32, batchBytes []byte, _ []byte) {
		if len(batchBytes) == 0 {
			fmt.Printf("[MPX][NIL] DELIVER ⊥ sn=%d\n", sn)
			tracing.MainTrace.Event(tracing.COMMIT, int64(sn), 0)
			return
		}
		var b pb.Batch
		if err := proto.Unmarshal(batchBytes, &b); err != nil {
			fmt.Printf("[MPX][ANNOUNCE][ERR] sn=%d unmarshal: %v\n", sn, err)
			return
		}
		entry := &mirlog.Entry{Sn: sn, Batch: &b}
		announcer.Announce(entry)

		fmt.Printf("SB-DELIVER sn=%d size=%d\n", sn, len(b.Requests))
		fmt.Printf("COMMIT sn=%d size=%d\n", sn, len(b.Requests))
		fmt.Printf("DELIVER sn=%d delivered=%d\n", sn, len(b.Requests))

		tracing.MainTrace.Event(tracing.COMMIT, int64(sn), int64(len(b.Requests)))
	}

	fmt.Printf("[MPX] Init ok; cfg: batchSize=%d batchTimeout=%s view=%d leaderPolicy=%s\n",
		o.maxBatchSize, o.proposeEvery, o.view, strings.ToLower(config.Config.LeaderPolicy))
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

	inst, ok := o.dispatcher.load(sn)
	if !ok || inst == nil {
		// === CRIAÇÃO ON-DEMAND ===
		// Se instância não existe, tenta criar (follower sem requests locais)
		// Isso garante que followers recebam mensagens mesmo sem requests
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

func (o *MultiPaxosOrderer) runSegment(seg manager.Segment) {
	// === PARALELISMO POR GRUPO ===
	// Cada bucket (grupo) tem sua própria instância ativa
	// Permite múltiplos grupos processarem em paralelo
	activeInstances := make(map[uint32]*mpxInstance)
	var mu sync.Mutex
	
	bucketIDs := seg.Buckets().GetBucketIDs()
	numBuckets := int32(len(bucketIDs))
	if numBuckets == 0 {
		numBuckets = 1
	}
	
	go func() {
		t := time.NewTicker(o.proposeEvery)
		defer t.Stop()
		
		// Round-robin anti-starvation: garante fairness entre buckets
		lastBucketIdx := 0
		
		// Cache local de Phase 1 por segmento (reseta a cada segmento)
		localGroupPrepared := make(map[uint32]bool)
		
		for range t.C {
			now := time.Now()
			
			// Tenta propor para cada bucket, começando do último
			for offset := 0; offset < len(bucketIDs); offset++ {
				idx := (lastBucketIdx + offset) % len(bucketIDs)
				bucketID := uint32(bucketIDs[idx])
				
				// === LIDERANÇA POR GRUPO (integrada com leader_policy) ===
				segmentLeaders := seg.Leaders()
				groupLeader := o.am.GetGroupLeader(GroupID(bucketID), segmentLeaders)
				isGroupLeader := (groupLeader == membership.OwnID)
				
				mu.Lock()
				inst := activeInstances[bucketID]
				
				// === TODOS OS NÓS CRIAM INSTÂNCIA ===
				if inst == nil || inst.isClosed() {
					groupSN := o.am.NextSN(GroupID(bucketID))
					
					// === MAPEAMENTO SN GLOBAL ===
					// globalSN = base + groupSN*numBuckets + bucketIndex
					// Garante SNs únicos entre grupos sem colisão
					// Ex: 3 buckets, base=0
					//   bucket[0]: 0, 3, 6, 9...
					//   bucket[1]: 1, 4, 7, 10...
					//   bucket[2]: 2, 5, 8, 11...
					// NOTA: Segmento deve ser grande o suficiente para acomodar
					//       groupSN avançando sem ultrapassar LastSN
					globalSN := seg.FirstSN() + groupSN*numBuckets + int32(idx)
					
					// === GUARD: NÃO ULTRAPASSAR LASTSN ===
					if globalSN > seg.LastSN() {
						mu.Unlock()
						fmt.Printf("[MPX][SEG] sn=%d exceeds segment LastSN=%d, skipping\n", globalSN, seg.LastSN())
						continue
					}
					
					inst = o.ensureInstance(globalSN)
					inst.setSegment(seg)
					inst.bucketId = bucketID
					
					// === CONFIGURA MEMBROS DO GRUPO ===
					members := o.am.GetGroupMembers(bucketID)
					inst.SetMembers(members)
					
					// Log diferenciado: multicast seletivo vs broadcast
					if len(members) < len(membership.AllNodeIDs()) {
						fmt.Printf("[MPX][MULTICAST] sn=%d bucketId=%d SELECTIVE members=%v quorum=%d\n", 
							globalSN, bucketID, members, len(members)/2+1)
					} else {
						fmt.Printf("[MPX][BROADCAST] sn=%d bucketId=%d ALL_NODES members=%v quorum=%d\n", 
							globalSN, bucketID, members, len(members)/2+1)
					}
					
					// === PHASE 1 AMORTIZADA (cache local por segmento) ===
					if isGroupLeader && !localGroupPrepared[bucketID] {
						localGroupPrepared[bucketID] = true
						
						prep := &pb.MPxMsg{Type: &pb.MPxMsg_Prepare{
							Prepare: &pb.MPxPrepare{
								Id:      &pb.MPxInstanceId{Sn: globalSN, Lead: uint64(membership.OwnID)},
								Ballot:  0,
								GroupId: bucketID,
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
					}
					
					inst.enableNilDeliver = o.enableNilDeliver
					inst.sbNilAfter = o.sbNilAfter
					
					// Registra no dispatcher com SN global único
					o.dispatcher.store(globalSN, inst)
					inst.startWorkers(&o.stopWg)
					o.backlog.drainTo(globalSN, inst.enqueue)
					
					activeInstances[bucketID] = inst
					
					// Rastreia instância para cleanup correto
					if existing, loaded := o.segmentInstances.LoadOrStore(seg.SegID(), []*mpxInstance{inst}); loaded {
						instList := existing.([]*mpxInstance)
						o.segmentInstances.Store(seg.SegID(), append(instList, inst))
					}
				}
				mu.Unlock()
				
				// Tick em todos os nós
				inst.tick(now)
				
				// === APENAS LÍDER PROPÕE ===
				if isGroupLeader {
					inst.ProposeIfDue()
				}
				
				// Avança round-robin
				lastBucketIdx = (idx + 1) % len(bucketIDs)
				break // 1 bucket por tick
			}
		}
	}()
	
	// === CLEANUP CORRETO ===
	// Fecha apenas instâncias criadas por este segmento
	go func() {
		// Aguarda checkpoint do último SN do segmento
		checkpoints := mirlog.Checkpoints()
		currentCheckpoint := mirlog.GetCheckpoint()
		for currentCheckpoint == nil || currentCheckpoint.Sn < seg.LastSN() {
			currentCheckpoint = <-checkpoints
		}
		
		// Fecha instâncias criadas por este segmento
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
	// Cleanup já é feito por runSegment via segmentInstances
	// Este método mantido para compatibilidade com Manager
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
	o.instMu.RLock()
	inst := o.instances[sn]
	o.instMu.RUnlock()
	if inst != nil {
		return inst
	}

	o.instMu.Lock()
	defer o.instMu.Unlock()
	if inst = o.instances[sn]; inst == nil {
		inst = newMPXInstance(o, sn, o.announce, o.maxBatchSize, o.proposeEvery)
		o.instances[sn] = inst
		
		// Notifica callback se configurado
		if o.onInstanceCreated != nil {
			o.onInstanceCreated(sn)
		}
	}
	return inst
}

func (o *MultiPaxosOrderer) Sign(data []byte) ([]byte, error) { return nil, nil }
func (o *MultiPaxosOrderer) CheckSig(data []byte, senderID int32, signature []byte) error {
	return nil
}

