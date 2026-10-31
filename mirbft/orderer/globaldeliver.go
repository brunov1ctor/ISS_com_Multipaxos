// Entrega em ordem total GLOBAL (Défago, Schiper & Urbán 2004, Seção 3.2.3; definição de
// Hadzilacos & Toueg): a relação "m entregue antes de m′" não pode ter ciclo considerando
// todos os grupos.
//
// O consenso de cada grupo (MultiPaxos) só garante ordem LOCAL: mensagens do mesmo grupo na
// mesma ordem. Um nó que pertence a vários grupos recebe uma fila por grupo e, sem esta camada,
// intercala as filas na ordem em que chegam -- dois nós em G1∩G2 podem ver intercalações
// diferentes.
//
// Esta camada fica ACIMA do consenso: toda requisição de modo global tem um GSN (número de
// sequência global, dado pelo sequenciador) e o nó só a entrega quando todo GSN menor que
// toca algum grupo deste nó já foi entregue. Como todos os nós usam a mesma numeração, a
// ordem de entrega é a mesma em todo nó (restrita às mensagens que cada um recebe).
//
// Trabalha por REQUISIÇÃO, não por lote: segurar lotes inteiros entre grupos trava (lote de G1
// com GSNs {5,8} espera G2 por causa do 8; lote de G2 com {6,7} espera G1 por causa do 5).
package orderer

import (
	"fmt"
	"sort"
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

	// META recebido: GSN -> grupos tocados. metaPrefix = maior k tal que os GSNs 1..k já
	// têm META; só dá pra afirmar "não vem GSN menor" até esse ponto (o META é enviado
	// ponto a ponto por proxies diferentes e pode chegar fora de ordem).
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

// OnSkip registra que os GSNs [from, to] nunca vão existir (novo líder do sequenciador pulou a
// faixa). Ficam como "não tocam nenhum grupo deste nó". Se o META real de um desses GSNs chegar
// depois (o líder antigo chegou a atribuí-lo), ele é ignorado e avisado no log -- limitação
// conhecida do failover sem log replicado.
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

// OnCommit registra que a requisição foi decidida pelo consenso de um grupo deste nó.
// Pode ser chamada uma vez por grupo; a entrega acontece uma vez só.
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
			// GSN de grupos dos quais este nó não participa: nada a entregar aqui.
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

// Pending devolve os GSNs decididos que ainda esperam a vez (para depuração/testes).
func (g *GlobalDeliverer) Pending() []uint64 {
	g.mu.Lock()
	defer g.mu.Unlock()
	keys := make([]uint64, 0, len(g.committed))
	for k := range g.committed {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
	return keys
}

// logGlobalDelivery é o onDeliver padrão do nó: a linha GDELIVER é o que comprova a ordem
// global ao comparar os logs de nós que estão na interseção de grupos.
func logGlobalDelivery(nodeID int32) func(GlobalDelivery) {
	return func(d GlobalDelivery) {
		fmt.Printf("[GDELIVER] node=%d gsn=%d groups=%v payload=%q\n", nodeID, d.GSN, d.Groups, d.Req.Payload)
	}
}

// ---------------------------------------------------------------------------------------------
// Entrega em ordem por grupo, separada da decisão do consenso.
//
// Antes, o lote decidido só entrava no log do grupo depois que todas as suas cross-ops podiam
// ser entregues na ordem de GSN. Se o lote tinha a cross-op 9480 mas não a 9475 (que ainda não
// tinha chegado ao líder quando ele cortou o lote), o lote ficava retido; o laço do grupo só
// propõe o próximo SN depois que o atual entra no log, então a 9475 nunca era proposta e o grupo
// travava. Com lotes de cross-ops grandes (CrossOpBatchMaxBytes de 1 MB) isso acontecia em
// segundos.
//
// Agora o consenso e a entrega são independentes, como no ISS (instâncias decidem em paralelo, o
// log é entregue em ordem): todo lote decidido entra no log na hora e o grupo continua propondo.
// Este entregador percorre o log do grupo em ordem de SN e entrega requisição por requisição:
//   - operação de um grupo só: entregue na posição em que aparece no log;
//   - cross-op: entregue só quando toda GSN menor que toca este grupo já foi entregue. Para saber
//     isso sem depender de quando as mensagens chegam, espera o META de todas as GSNs menores
//     (mesma regra de prefixo sem buracos do GlobalDeliverer). Enquanto a cabeça do log espera
//     uma GSN menor que já está mais adiante no log, essa GSN é antecipada.
//
// A sequência entregue depende só do conteúdo do log do grupo e dos METAs, não da ordem de
// chegada das mensagens; por isso toda réplica do grupo entrega a mesma sequência. Cada grupo
// entrega as cross-ops em ordem crescente de GSN, o que dá a ordem global entre grupos (Défago et
// al. 2004, Seção 3.2.3).

// deliveredReq é uma requisição entregue pelo groupDeliverer, com o SN do lote de origem.
type deliveredReq struct {
	sn     int32
	leader bool // este nó era o líder do SN de origem (só o líder notifica o proxy no benchmark)
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

	emitMu sync.Mutex // serializa as entregas (ordem de emissão = ordem decidida)
	mu     sync.Mutex

	// Log do grupo ainda não entregue, em ordem de SN.
	log  []logItem
	head int

	// Cross-ops presentes no log ainda não entregues: GSN -> item (para antecipar).
	crossInLog map[uint64]logItem
	// GSNs já entregues fora da posição (antecipados); pulados quando a cabeça chega neles.
	early map[uint64]bool

	// META (só de cross-ops): GSN -> grupos. metaPrefix = maior k com META de 1..k.
	meta       map[uint64][]uint32
	metaPrefix uint64
	// Toda GSN <= cursor já foi resolvida para este grupo (entregue ou não toca o grupo).
	cursor uint64

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

// OnMeta registra o META de uma GSN (todas as GSNs, não só as deste grupo: as que não tocam o
// grupo também precisam ser conhecidas para o prefixo avançar).
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

// advanceCursor pula as GSNs já resolvidas e devolve a próxima GSN que este grupo precisa
// entregar (0 se ainda não dá para saber, por falta de META).
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
			// c não aparece mais como pendente (META diz que não toca este grupo, ou faixa
			// pulada). Não deveria acontecer para uma cross-op no log deste grupo; entrega na
			// posição para não travar.
			out = append(out, deliveredReq(it))
			delete(d.crossInLog, c)
			d.head++
			continue
		}
		// next < c: a cabeça espera uma GSN menor. Se ela já está mais adiante no log,
		// antecipa; senão espera ela ser decidida.
		ahead, ok := d.crossInLog[next]
		if !ok {
			break
		}
		out = append(out, deliveredReq(ahead))
		d.early[next] = true
		d.markDelivered(next)
	}
	// Compacta o log de tempos em tempos.
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

// Pending devolve quantas requisições do log ainda esperam entrega (depuração/testes).
func (d *groupDeliverer) Pending() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.log) - d.head
}
