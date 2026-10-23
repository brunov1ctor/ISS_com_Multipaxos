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
