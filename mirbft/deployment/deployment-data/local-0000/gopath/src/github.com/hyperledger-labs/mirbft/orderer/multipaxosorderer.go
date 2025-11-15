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

// Hooks/Router (mantidos para compatibilidade e possível multicast futuro).
type OutboundHooks struct {
	OnSend func(pm *pb.ProtocolMessage) (dests []int32, err error)
}
type GroupResolver func(sn int32) uint32

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

	hooks        OutboundHooks
	resolveGroup GroupResolver

	maxBatchSize     int
	proposeEvery     time.Duration
	view             int32
	stopWg           sync.WaitGroup
	sbNilAfter       time.Duration
	enableNilDeliver bool
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

	o.hooks = OutboundHooks{}
	o.resolveGroup = func(sn int32) uint32 { return 0 } // não usado por enquanto

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

	// emit com hook (por ora, broadcast padrão; hook pode selecionar destinos)
	o.emit = func(pm *pb.ProtocolMessage) {
		o.broadcastHooked(pm, func(pm *pb.ProtocolMessage) {
			for _, nid := range membership.AllNodeIDs() {
				if nid == membership.OwnID {
					continue
				}
				messenger.EnqueueMsg(pm, nid)
			}
		})
	}

	o.segmentChan = o.mgr.SubscribeOrderer()
	messenger.OrdererMsgHandler = o.HandleMessage

	fmt.Printf("[MPX] Init ok; cfg: batchSize=%d batchTimeout=%s view=%d leaderPolicy=%s\n",
		o.maxBatchSize, o.proposeEvery, o.view, strings.ToLower(config.Config.LeaderPolicy))
}

func (o *MultiPaxosOrderer) broadcastHooked(pm *pb.ProtocolMessage, fallback func(pm *pb.ProtocolMessage)) {
	if o.hooks.OnSend != nil {
		if dests, err := o.hooks.OnSend(pm); err == nil && len(dests) > 0 {
			fmt.Printf("[MPX][HOOK] selective send sn=%d type=%T dests=%v\n", pm.Sn, pm.GetMultipaxos().GetType(), dests)
			for _, d := range dests {
				if d == membership.OwnID {
					continue
				}
				messenger.EnqueueMsg(pm, d)
			}
			return
		}
	}
	fallback(pm)
}

// envio seletivo sem passar por hooks (usado se você quiser forçar destinos)
func (o *MultiPaxosOrderer) emitTo(pm *pb.ProtocolMessage, dests []int32) {
	if len(dests) == 0 {
		o.emit(pm)
		return
	}
	fmt.Printf("[MPX][SEND] sn=%d type=%T to=%v\n", pm.Sn, pm.GetMultipaxos().GetType(), dests)
	for _, d := range dests {
		if d == membership.OwnID {
			continue
		}
		messenger.EnqueueMsg(pm, d)
	}
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
		o.backlog.add(pm)
		return
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
	for _, sn := range seg.SNs() {
		inst := o.ensureInstance(sn)
		inst.setSegment(seg)
		inst.enableNilDeliver = o.enableNilDeliver
		inst.sbNilAfter = o.sbNilAfter
		o.dispatcher.store(sn, inst)
	}
	for _, sn := range seg.SNs() {
		if inst, ok := o.dispatcher.load(sn); ok && inst != nil {
			inst.startWorkers(&o.stopWg)
			o.backlog.drainTo(sn, inst.enqueue)
		}
	}
	go func() {
		t := time.NewTicker(o.proposeEvery)
		defer t.Stop()

		nextSN := seg.FirstSN()
		for range t.C {
			now := time.Now()

			if isSegmentLeader(seg, membership.OwnID, o.view) {
				for sn := nextSN; sn <= seg.LastSN(); sn++ {
					inst, _ := o.dispatcher.load(sn)
					if inst == nil {
						continue
					}
					inst.ProposeIfDue(nil)
					inst.tick(now)
					if inst.isClosed() && sn == nextSN {
						nextSN++
					}
				}
				if nextSN > seg.LastSN() {
					return
				}
			} else {
				for sn := nextSN; sn <= seg.LastSN(); sn++ {
					inst, _ := o.dispatcher.load(sn)
					if inst != nil {
						inst.tick(now)
					}
				}
				if nextSN > seg.LastSN() {
					return
				}
			}
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

	logger.Info().Int("segID", seg.SegID()).Msg("Closing MPX instance workers.")
	for _, sn := range seg.SNs() {
		if inst, ok := o.dispatcher.load(sn); ok && inst != nil {
			inst.stopWorkers()
			o.dispatcher.delete(sn)
		}
	}
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
	}
	return inst
}

func (o *MultiPaxosOrderer) Sign(data []byte) ([]byte, error) { return nil, nil }
func (o *MultiPaxosOrderer) CheckSig(data []byte, senderID int32, signature []byte) error {
	return nil
}

