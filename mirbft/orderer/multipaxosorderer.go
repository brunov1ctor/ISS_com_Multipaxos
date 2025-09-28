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

type MultiPaxosOrderer struct {
	mgr manager.Manager

	dispatcher mpxDispatcher
	backlog    mpxBacklog
	last       int32 // SN máximo aceito; descartamos msg/sn <= last

	instances map[int32]*mpxInstance
	instMu    sync.RWMutex
	startOnce sync.Once

	emit     func(pm *pb.ProtocolMessage)
	announce AnnounceFn

	maxBatchSize     int
	proposeEvery     time.Duration
	view             int32 // view atual
	stopWg           sync.WaitGroup
	sbNilAfter       time.Duration // timeout para ⊥ (default: 6x BatchTimeout)
	enableNilDeliver bool          // liga/desliga SB-⊥

	segmentChan chan manager.Segment
}

// líder do SEGMENTO (não por slot)
func isSegmentLeader(seg manager.Segment, ownID int32, view int32) bool {
	leaders := seg.Leaders()
	if len(leaders) == 0 {
		return false
	}
	idx := int(view) % len(leaders)
	return leaders[idx] == ownID
}

type mpxDispatcher struct {
	mm sync.Map // SN -> *mpxInstance
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
		gcCh: make(chan int32, 1024),
	}
}

func (b *mpxBacklog) enqueue(pm *pb.ProtocolMessage) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.qs[pm.Sn] = append(b.qs[pm.Sn], pm)
}

func (b *mpxBacklog) drainTo(sn int32, into func(pm *pb.ProtocolMessage)) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if q := b.qs[sn]; len(q) > 0 {
		for _, pm := range q {
			into(pm)
		}
		delete(b.qs, sn)
	}
}

func (b *mpxBacklog) gcUntil(sn int32) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for key := range b.qs {
		if key <= sn {
			delete(b.qs, key)
		}
	}
}

// =========================
// API
// =========================

func (o *MultiPaxosOrderer) Init(mgr manager.Manager) {
	o.mgr = mgr
	o.instances = make(map[int32]*mpxInstance)
	o.backlog = newMPXBacklog()
	o.last = -1

	o.maxBatchSize = int(config.Config.BatchSize)
	o.proposeEvery = time.Duration(config.Config.BatchTimeout)
	o.view = 0

	// SB-⊥: multiplicador para ⊥ (aqui ajustado para 6× o BatchTimeout).
	o.sbNilAfter = o.proposeEvery * 6
	o.enableNilDeliver = true

	logger.Info().
		Int("batchSize", o.maxBatchSize).
		Dur("batchTimeout", o.proposeEvery).
		Dur("sbNilAfter", o.sbNilAfter).
		Bool("enableNilDeliver", o.enableNilDeliver).
		Msg("[MPX] Init parameters")

	// Announce: aciona Responder e instrumenta traces do pipeline
	o.announce = func(sn int32, batchBytes []byte, _ []byte) {
		var b pb.Batch
		if err := proto.Unmarshal(batchBytes, &b); err != nil {
			logger.Warn().Err(err).Msg("[MPX] announce: unmarshal batch")
			return
		}
		entry := &mirlog.Entry{Sn: sn, Batch: &b}
		announcer.Announce(entry)

		// marcadores úteis p/ métricas/grep
		fmt.Printf("SB-DELIVER sn=%d size=%d\n", sn, len(b.Requests))
		fmt.Printf("COMMIT sn=%d size=%d\n", sn, len(b.Requests))
		fmt.Printf("DELIVER sn=%d delivered=%d\n", sn, len(b.Requests))

		tracing.MainTrace.Event(tracing.COMMIT, int64(sn), int64(len(b.Requests)))
	}

	// broadcast de protocolo para os demais peers (cliente continua broadcastando requests)
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

	fmt.Printf("[MPX] Init ok; cfg: batchSize=%d batchTimeout=%s view=%d leaderPolicy=%s (leader per segment)\n",
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
					Int32("firstLeader", seg.Leaders()[0]).
					Msg("[MPX] New segment received")

				// IMPORTANTE: sem anúncio de buckets aqui (voltamos ao broadcast do cliente)
				// Manager (se configurado) pode anunciar; este orderer não interfere.

				o.runSegment(seg)
				go o.killSegment(seg)
			}
		}()
	})
}

func (o *MultiPaxosOrderer) Stop() {
	close(o.backlog.gcCh)
	o.stopWg.Wait()
}

func (o *MultiPaxosOrderer) killSegment(seg manager.Segment) {
	// encerra instâncias e libera dispatcher após término do segmento
	timer := time.NewTimer(time.Duration(seg.Len()) * o.proposeEvery * 2)
	defer timer.Stop()
	<-timer.C

	for _, sn := range seg.SNs() {
		if inst, ok := o.dispatcher.load(sn); ok && inst != nil {
			inst.stopWorkers()
			o.dispatcher.delete(sn)
		}
	}
}

func (o *MultiPaxosOrderer) HandleMessage(pm *pb.ProtocolMessage) {
	// descarta msgs antigas
	last := atomic.LoadInt32(&o.last)
	if pm.Sn <= last {
		return
	}

	// log de chegada (tipo/seq/remetente) — sem depender de nomes de oneof específicos
	kind := fmt.Sprintf("%T", pm.Msg)
	logger.Debug().
		Int32("sn", pm.Sn).
		Int32("from", pm.SenderId).
		Str("kind", kind).
		Msg("[MPX] HandleMessage")

	// pega/instancia e entrega ao SN correto
	inst, ok := o.dispatcher.load(pm.Sn)
	if !ok || inst == nil {
		// backlog até runSegment instalar as instâncias
		o.backlog.enqueue(pm)
		return
	}
	inst.enqueue(pm)
}

// HandleEntry: chamado p/ inserção fora do orderer (ex.: state transfer)
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
	// cria/instala instâncias e workers
	for _, sn := range seg.SNs() {
		inst := o.ensureInstance(sn)
		inst.setSegment(seg)
		// herda parâmetros de drenagem/⊥ do orderer
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

	ownID := membership.OwnID
	nextSN := seg.FirstSN()

	go func() {
		t := time.NewTicker(o.proposeEvery / 2)
		defer t.Stop()
		for range t.C {
			now := time.Now()

			if isSegmentLeader(seg, ownID, o.view) {
				logger.Trace().Int32("nextSN", nextSN).Msg("[MPX] Leader tick")
				// líder: propõe e faz tick
				for sn := nextSN; sn <= seg.LastSN(); sn++ {
					inst, _ := o.dispatcher.load(sn)
					if inst == nil {
						continue
					}
					logger.Trace().Int32("sn", sn).Msg("[MPX] ProposeIfDue")
					inst.ProposeIfDue(nil) // 1a proposta ou idempotente
					inst.tick(now)         // RTX e (opcional) ⊥
					if inst.isClosed() && sn == nextSN {
						nextSN++
					}
				}
				if nextSN > seg.LastSN() {
					logger.Info().Int32("lastSN", seg.LastSN()).Msg("[MPX] Segment done (leader)")
					return
				}
			} else {
				// seguidores também fazem tick (para SB-⊥ local)
				for sn := nextSN; sn <= seg.LastSN(); sn++ {
					inst, _ := o.dispatcher.load(sn)
					if inst != nil {
						inst.tick(now)
					}
				}
				if nextSN > seg.LastSN() {
					logger.Info().Int32("lastSN", seg.LastSN()).Msg("[MPX] Segment done (follower)")
					return
				}
			}
		}
	}()
}

func (o *MultiPaxosOrderer) ensureInstance(sn int32) *mpxInstance {
	o.instMu.RLock()
	if inst := o.instances[sn]; inst != nil {
		o.instMu.RUnlock()
		return inst
	}
	o.instMu.RUnlock()

	o.instMu.Lock()
	defer o.instMu.Unlock()
	if inst := o.instances[sn]; inst == nil {
		inst = newMPXInstance(o, sn, o.announce, o.maxBatchSize, o.proposeEvery)
		o.instances[sn] = inst
		return inst
	} else {
		return inst
	}
}

// Satisfaz a interface orderer.Orderer
func (o *MultiPaxosOrderer) Sign(data []byte) ([]byte, error) { return nil, nil }
func (o *MultiPaxosOrderer) CheckSig(data []byte, senderID int32, signature []byte) error {
	return nil
}

