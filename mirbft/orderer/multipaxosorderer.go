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

//
// ============================================================
// Tipos auxiliares (announce/dispatcher/backlog)
// ============================================================
//

// AnnounceFn mantém assinatura usada pela instância para anunciar DELIVER/COMMIT.
type AnnounceFn func(sn int32, batchBytes []byte, metadata []byte)

// ---- Dispatcher: mapeia SN -> instância (thread-safe, semelhante ao PBFT) ----
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

// ---- Backlog: guarda mensagens por SN até a instância existir ----
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

//
// ============================================================
// MultiPaxosOrderer
// - líder por view/segmento (compatível com pipeline do MIR/ISS)
// - cliente pode operar em broadcast (como você testou) ou via assignment
// ============================================================
//

type MultiPaxosOrderer struct {
	// Orquestração/assinaturas
	mgr         manager.Manager
	segmentChan chan manager.Segment

	// Roteamento de mensagens/instâncias
	dispatcher mpxDispatcher
	backlog    mpxBacklog
	last       int32 // maior SN já estabilizado (descarta msgs antigas)

	instances map[int32]*mpxInstance
	instMu    sync.RWMutex
	startOnce sync.Once

	// IO de protocolo e anúncio de DELIVER
	emit     func(pm *pb.ProtocolMessage)
	announce AnnounceFn

	// Parâmetros operacionais
	maxBatchSize     int
	proposeEvery     time.Duration
	view             int32 // view atual (seleciona líder do segmento)
	stopWg           sync.WaitGroup
	sbNilAfter       time.Duration // timeout para ⊥ (default: 3x BatchTimeout; pode ser ajustado pelo caller)
	enableNilDeliver bool          // liga/desliga SB-⊥
}

// ---- Util: checa se ESTE nó é o líder do segmento (não por slot) ----
func isSegmentLeader(seg manager.Segment, ownID int32, view int32) bool {
	leaders := seg.Leaders()
	if len(leaders) == 0 {
		return false
	}
	idx := int(view) % len(leaders)
	return leaders[idx] == ownID
}

//
// ============================================================
// API do Orderer (Init/Start/HandleMessage/HandleEntry/Stop)
// ============================================================
//

// Init configura callbacks, parâmetros e registra o handler com o messenger.
func (o *MultiPaxosOrderer) Init(mgr manager.Manager) {
	o.mgr = mgr
	o.instances = make(map[int32]*mpxInstance)
	o.backlog = newMPXBacklog()
	o.last = -1

	o.maxBatchSize = int(config.Config.BatchSize)
	o.proposeEvery = time.Duration(config.Config.BatchTimeout)
	o.view = 0

	// SB-⊥: por padrão 3× BatchTimeout (você pode subir p/ 6× externamente).
	o.sbNilAfter = o.proposeEvery * 3
	o.enableNilDeliver = true

	// announce: aciona Responder (e métricas) ao concluir batch (DELIVER).
	o.announce = func(sn int32, batchBytes []byte, _ []byte) {
		if len(batchBytes) == 0 {
			// NIL (⊥): avanço de SN sem entregar requests.
			fmt.Printf("[MPX][NIL] DELIVER ⊥ sn=%d\n", sn)
			tracing.MainTrace.Event(tracing.COMMIT, int64(sn), 0)
			return
		}
		var b pb.Batch
		if err := proto.Unmarshal(batchBytes, &b); err != nil {
			fmt.Printf("[MPX][ANNOUNCE][ERR] sn=%d unmarshal: %v\n", sn, err)
			return
		}
		// Anuncia para o sistema (Responder gera métricas/timeline)
		entry := &mirlog.Entry{Sn: sn, Batch: &b}
		announcer.Announce(entry)

		// Marcadores úteis p/ grep e ferramentas
		fmt.Printf("SB-DELIVER sn=%d size=%d\n", sn, len(b.Requests))
		fmt.Printf("COMMIT sn=%d size=%d\n", sn, len(b.Requests))
		fmt.Printf("DELIVER sn=%d delivered=%d\n", sn, len(b.Requests))

		tracing.MainTrace.Event(tracing.COMMIT, int64(sn), int64(len(b.Requests)))
	}

	// emit: difusão de mensagens de protocolo (para todos os outros peers).
	o.emit = func(pm *pb.ProtocolMessage) {
		for _, nid := range membership.AllNodeIDs() {
			if nid == membership.OwnID {
				continue
			}
			messenger.EnqueueMsg(pm, nid)
		}
	}

	// inscrição no canal de segmentos e registro do handler de mensagens
	o.segmentChan = o.mgr.SubscribeOrderer()
	messenger.OrdererMsgHandler = o.HandleMessage

	fmt.Printf("[MPX] Init ok; cfg: batchSize=%d batchTimeout=%s view=%d leaderPolicy=%s (leader per segment)\n",
		o.maxBatchSize, o.proposeEvery, o.view, strings.ToLower(config.Config.LeaderPolicy))
}

// Start consome segmentos do manager e dispara execução por segmento.
// (não anuncia buckets aqui — manager continua responsável por assignment)
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

				o.runSegment(seg)   // instala instâncias + pipeline
				go o.killSegment(seg) // GC após estabilidade
			}
		}()
		fmt.Printf("[MPX] Start done\n")
	})
}

// HandleMessage: entrada de mensagens de protocolo.
// - descarta mensagens antigas (<= o.last)
// - backlog se instância ainda não está criada
// - entrega na fila da instância quando disponível
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

// HandleEntry: caminho auxiliar para injetar entradas (ex.: state transfer).
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

//
// ============================================================
// Execução por segmento (runSegment/killSegment)
// ============================================================
//

// runSegment cria as instâncias de SN, liga workers e executa o “tick”:
// - O líder do segmento chama ProposeIfDue + tick (RTX/⊥).
// - Seguidores também chamam tick (para SB-⊥ local).
func (o *MultiPaxosOrderer) runSegment(seg manager.Segment) {
	// 1) Criar/instalar instâncias e herdar parâmetros de NIL
	for _, sn := range seg.SNs() {
		inst := o.ensureInstance(sn)
		inst.setSegment(seg)
		inst.enableNilDeliver = o.enableNilDeliver
		inst.sbNilAfter = o.sbNilAfter
		o.dispatcher.store(sn, inst)
	}
	// 2) Ligar workers e drenar backlog por SN
	for _, sn := range seg.SNs() {
		if inst, ok := o.dispatcher.load(sn); ok && inst != nil {
			inst.startWorkers(&o.stopWg)
			o.backlog.drainTo(sn, inst.enqueue)
		}
	}

	// 3) Loop do segmento: um líder dirige o pipeline,
	//    mas todos “ticam” para permitir SB-⊥.
	go func() {
		t := time.NewTicker(o.proposeEvery)
		defer t.Stop()

		nextSN := seg.FirstSN()
		for range t.C {
			now := time.Now()

			if isSegmentLeader(seg, membership.OwnID, o.view) {
				// Líder → propõe e avança SNs
				for sn := nextSN; sn <= seg.LastSN(); sn++ {
					inst, _ := o.dispatcher.load(sn)
					if inst == nil {
						continue
					}
					inst.ProposeIfDue(nil) // primeira proposta (ou idempotente)
					inst.tick(now)         // RTX / NIL
					if inst.isClosed() && sn == nextSN {
						nextSN++
					}
				}
				if nextSN > seg.LastSN() {
					return
				}
			} else {
				// Seguidores → apenas tick (para convergência/NIL)
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

// killSegment aguarda estabilidade (checkpoint + commit do lastSN),
// avança janela (o.last) e derruba os workers/instâncias desse segmento.
func (o *MultiPaxosOrderer) killSegment(seg manager.Segment) {
	// Espera checkpoint estável alcançar lastSN do segmento
	checkpoints := mirlog.Checkpoints()
	currentCheckpoint := mirlog.GetCheckpoint()
	for currentCheckpoint == nil || currentCheckpoint.Sn < seg.LastSN() {
		currentCheckpoint = <-checkpoints
	}
	mirlog.WaitForEntry(seg.LastSN())

	// Avança a janela de mensagens válidas
	o.instMu.Lock()
	if seg.LastSN() > o.last {
		atomic.StoreInt32(&o.last, seg.LastSN())
	}
	o.instMu.Unlock()

	// Encerra workers e remove instâncias
	logger.Info().Int("segID", seg.SegID()).Msg("Closing MPX instance workers.")
	for _, sn := range seg.SNs() {
		if inst, ok := o.dispatcher.load(sn); ok && inst != nil {
			inst.stopWorkers()
			o.dispatcher.delete(sn)
		}
	}
}

//
// ============================================================
// Utilidades e compatibilidade com a interface orderer.Orderer
// ============================================================
//

// ensureInstance cria (uma única vez) a instância para um SN.
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

// Satisfaz a interface orderer.Orderer (assinaturas opcionais não usadas aqui)
func (o *MultiPaxosOrderer) Sign(data []byte) ([]byte, error) { return nil, nil }
func (o *MultiPaxosOrderer) CheckSig(data []byte, senderID int32, signature []byte) error {
	return nil
}

