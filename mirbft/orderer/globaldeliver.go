// Entrega em ordem total global.
// GlobalDeliverer: um nó em vários grupos entrega as requisições em ordem de GSN.
// groupDeliverer: cada grupo entrega o seu log respeitando a ordem de GSN das cross-ops.
package orderer

import (
	"fmt"
	"sync"

	pb "github.com/hyperledger-labs/mirbft/protobufs"
)

// GlobalDelivery é uma mensagem entregue pela camada global, na ordem final.
type GlobalDelivery struct {
	GSN    uint64
	Groups []uint32
	Req    *pb.ClientRequest
}

// GlobalDeliverer junta as filas de todos os grupos de um nó numa única ordem por GSN.
type GlobalDeliverer struct {
	mu sync.Mutex

	isMine func(groupID uint32) bool // este nó pertence ao grupo?

	// GSN -> grupos; metaPrefix = maior k com META de 1..k (o META chega fora de ordem).
	meta       map[uint64][]uint32
	metaPrefix uint64

	// Requisições já decididas em algum grupo deste nó, aguardando a vez.
	committed map[uint64]*pb.ClientRequest

	lastDelivered uint64 // último GSN processado (entregue ou pulado por não ser deste nó)

	onDeliver func(GlobalDelivery)
}

func NewGlobalDeliverer(isMine func(uint32) bool, onDeliver func(GlobalDelivery)) *GlobalDeliverer {
	return &GlobalDeliverer{
		isMine:    isMine,
		meta:      make(map[uint64][]uint32),
		committed: make(map[uint64]*pb.ClientRequest),
		onDeliver: onDeliver,
	}
}

// OnMeta registra que o GSN existe e quais grupos ele toca.
func (g *GlobalDeliverer) OnMeta(gsn uint64, groups []uint32) {
	if gsn == 0 {
		return
	}
	g.mu.Lock()
	if prev, ok := g.meta[gsn]; ok && len(prev) == 0 && len(groups) > 0 {
		fmt.Printf("[GDELIVER] AVISO: META do gsn=%d chegou depois de a faixa ser declarada pulada; ignorado\n", gsn)
	}
	if _, ok := g.meta[gsn]; !ok && gsn > g.lastDelivered {
		g.meta[gsn] = append([]uint32{}, groups...)
		for g.meta[g.metaPrefix+1] != nil {
			g.metaPrefix++
		}
	}
	out := g.drainLocked()
	g.mu.Unlock()
	g.emit(out)
}

// OnSkip registra GSNs que nunca vão existir (troca de líder do sequenciador).
func (g *GlobalDeliverer) OnSkip(from, to uint64) {
	if from == 0 || to < from {
		return
	}
	g.mu.Lock()
	for gsn := from; gsn <= to; gsn++ {
		if _, ok := g.meta[gsn]; !ok && gsn > g.lastDelivered {
			g.meta[gsn] = []uint32{}
		}
	}
	for g.meta[g.metaPrefix+1] != nil {
		g.metaPrefix++
	}
	out := g.drainLocked()
	g.mu.Unlock()
	g.emit(out)
}

// OnCommit registra uma requisição decidida num grupo deste nó.
func (g *GlobalDeliverer) OnCommit(req *pb.ClientRequest) {
	if req == nil || req.GSN == 0 {
		return
	}
	g.mu.Lock()
	if req.GSN > g.lastDelivered {
		if _, ok := g.committed[req.GSN]; !ok {
			g.committed[req.GSN] = req
		}
	}
	out := g.drainLocked()
	g.mu.Unlock()
	g.emit(out)
}

func (g *GlobalDeliverer) drainLocked() []GlobalDelivery {
	var out []GlobalDelivery
	for g.lastDelivered < g.metaPrefix {
		next := g.lastDelivered + 1
		groups := g.meta[next]
		if !g.touchesMine(groups) {
			delete(g.meta, next)
			delete(g.committed, next)
			g.lastDelivered = next
			continue
		}
		req, ok := g.committed[next]
		if !ok {
			break // ainda não decidido no(s) grupo(s) deste nó
		}
		out = append(out, GlobalDelivery{GSN: next, Groups: groups, Req: req})
		delete(g.meta, next)
		delete(g.committed, next)
		g.lastDelivered = next
	}
	return out
}

func (g *GlobalDeliverer) touchesMine(groups []uint32) bool {
	for _, gid := range groups {
		if g.isMine(gid) {
			return true
		}
	}
	return false
}

func (g *GlobalDeliverer) emit(out []GlobalDelivery) {
	if g.onDeliver == nil {
		return
	}
	for _, d := range out {
		g.onDeliver(d)
	}
}

// logGlobalDelivery registra cada entrega (linha GDELIVER) no log do nó.
func logGlobalDelivery(nodeID int32) func(GlobalDelivery) {
	return func(d GlobalDelivery) {
		fmt.Printf("[GDELIVER] node=%d gsn=%d groups=%v payload=%q\n", nodeID, d.GSN, d.Groups, d.Req.Payload)
	}
}

// groupDeliverer: o lote decidido entra no log na hora (o grupo segue propondo) e a entrega
// é feita aqui, por requisição, em ordem de SN. Uma cross-op só sai depois de toda GSN menor
// que toca o grupo; se essa GSN está mais adiante no log, é antecipada. O resultado depende só
// do log e dos METAs, então toda réplica do grupo entrega a mesma sequência.

// deliveredReq é uma requisição entregue, com o SN do lote de origem.
type deliveredReq struct {
	sn     int32
	leader bool // este nó foi o líder do SN (só ele notifica o proxy no benchmark)
	req    *pb.ClientRequest
}

type logItem struct {
	sn     int32
	leader bool
	req    *pb.ClientRequest
}

func isCrossOp(req *pb.ClientRequest) bool {
	return req != nil && req.GSN > 0 && len(req.TouchedGroups) > 1
}

type groupDeliverer struct {
	gid uint32

	emitMu sync.Mutex // mantém as entregas na ordem decidida
	mu     sync.Mutex

	log  []logItem // log do grupo ainda não entregue, em ordem de SN
	head int

	crossInLog map[uint64]logItem // cross-ops no log ainda não entregues
	early      map[uint64]bool    // GSNs antecipadas, puladas quando a cabeça chega nelas

	meta       map[uint64][]uint32 // GSN -> grupos
	metaPrefix uint64              // maior k com META de 1..k
	cursor     uint64              // GSNs <= cursor já resolvidas para este grupo

	onDeliver func([]deliveredReq)
}

func newGroupDeliverer(gid uint32, onDeliver func([]deliveredReq)) *groupDeliverer {
	return &groupDeliverer{
		gid:        gid,
		crossInLog: make(map[uint64]logItem),
		early:      make(map[uint64]bool),
		meta:       make(map[uint64][]uint32),
		onDeliver:  onDeliver,
	}
}

// Push recebe o próximo lote do log do grupo (chamado em ordem crescente de SN).
func (d *groupDeliverer) Push(sn int32, batch *pb.Batch, leader bool) {
	if batch == nil || len(batch.Requests) == 0 {
		return
	}
	d.emitMu.Lock()
	defer d.emitMu.Unlock()
	d.mu.Lock()
	for _, req := range batch.Requests {
		if req == nil {
			continue
		}
		it := logItem{sn: sn, leader: leader, req: req}
		if isCrossOp(req) {
			if req.GSN <= d.cursor || d.early[req.GSN] {
				continue // já entregue (cópia repetida)
			}
			if _, dup := d.crossInLog[req.GSN]; dup {
				continue
			}
			d.crossInLog[req.GSN] = it
		}
		d.log = append(d.log, it)
	}
	out := d.drainLocked()
	d.mu.Unlock()
	d.emit(out)
}

// OnMeta registra o META de uma GSN (de qualquer grupo, para o prefixo avançar).
func (d *groupDeliverer) OnMeta(gsn uint64, groups []uint32) {
	if gsn == 0 {
		return
	}
	d.emitMu.Lock()
	defer d.emitMu.Unlock()
	d.mu.Lock()
	if _, ok := d.meta[gsn]; !ok && gsn > d.cursor {
		d.meta[gsn] = append([]uint32{}, groups...)
		for d.meta[d.metaPrefix+1] != nil {
			d.metaPrefix++
		}
	}
	out := d.drainLocked()
	d.mu.Unlock()
	d.emit(out)
}

// OnSkip registra GSNs que nunca vão existir (troca de líder do sequenciador).
func (d *groupDeliverer) OnSkip(from, to uint64) {
	if from == 0 || to < from {
		return
	}
	d.emitMu.Lock()
	defer d.emitMu.Unlock()
	d.mu.Lock()
	for gsn := from; gsn <= to; gsn++ {
		if _, ok := d.meta[gsn]; !ok && gsn > d.cursor {
			d.meta[gsn] = []uint32{}
		}
	}
	for d.meta[d.metaPrefix+1] != nil {
		d.metaPrefix++
	}
	out := d.drainLocked()
	d.mu.Unlock()
	d.emit(out)
}

func (d *groupDeliverer) touches(groups []uint32) bool {
	for _, g := range groups {
		if g == d.gid {
			return true
		}
	}
	return false
}

// advanceCursor devolve a próxima GSN que o grupo precisa entregar (0 se falta META).
func (d *groupDeliverer) advanceCursor() uint64 {
	for d.cursor < d.metaPrefix {
		next := d.cursor + 1
		groups := d.meta[next]
		if len(groups) < 2 || !d.touches(groups) || d.early[next] {
			delete(d.meta, next)
			d.cursor = next
			continue
		}
		return next
	}
	return 0
}

func (d *groupDeliverer) markDelivered(gsn uint64) {
	delete(d.crossInLog, gsn)
	delete(d.meta, gsn)
	if gsn == d.cursor+1 {
		d.cursor = gsn
	}
}

func (d *groupDeliverer) drainLocked() []deliveredReq {
	var out []deliveredReq
	for d.head < len(d.log) {
		it := d.log[d.head]
		if !isCrossOp(it.req) {
			out = append(out, deliveredReq(it))
			d.head++
			continue
		}
		c := it.req.GSN
		if d.early[c] {
			delete(d.early, c)
			d.head++
			continue
		}
		next := d.advanceCursor()
		if next == 0 {
			break // falta META de alguma GSN menor: esperar
		}
		if next == c {
			out = append(out, deliveredReq(it))
			d.markDelivered(c)
			d.head++
			continue
		}
		if next > c {
			// c já não está pendente (ex.: faixa pulada): entrega na posição.
			out = append(out, deliveredReq(it))
			delete(d.crossInLog, c)
			d.head++
			continue
		}
		// Falta uma GSN menor: antecipa se já está no log, senão espera.
		ahead, ok := d.crossInLog[next]
		if !ok {
			break
		}
		out = append(out, deliveredReq(ahead))
		d.early[next] = true
		d.markDelivered(next)
	}
	if d.head > 4096 && d.head*2 > len(d.log) {
		d.log = append([]logItem(nil), d.log[d.head:]...)
		d.head = 0
	}
	return out
}

func (d *groupDeliverer) emit(out []deliveredReq) {
	if len(out) > 0 && d.onDeliver != nil {
		d.onDeliver(out)
	}
}
