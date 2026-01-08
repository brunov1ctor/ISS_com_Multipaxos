package orderer

import (
	"fmt"
	"sync"
	"time"

	"google.golang.org/protobuf/proto"
	"github.com/hyperledger-labs/mirbft/manager"
	"github.com/hyperledger-labs/mirbft/membership"
	"github.com/hyperledger-labs/mirbft/multicast"
	pb "github.com/hyperledger-labs/mirbft/protobufs"
)

const (
	defaultQuorumTimeoutMs = 250
	maxGroupIDSpace  int32 = 8  // Limitado para evitar explosão de sockets
)

// MultiPaxosMulticastOrderer é um wrapper que adiciona capacidades de multicast
// ao MultiPaxosOrderer base, mantendo toda a lógica Paxos intacta
type MultiPaxosMulticastOrderer struct {
	*MultiPaxosOrderer // Composição: reutiliza toda lógica base
	mc multicast.Router // Router multicast (pode ser UDP real ou fallback)

	mu sync.RWMutex

	// Estado para otimização de grupos dinâmicos
	snMembers map[int32]map[int32]struct{} // quem prometeu por SN
	snGroup   map[int32]multicast.GroupID  // grupo definido por SN
	snGSize   map[int32]int                // tamanho do grupo por SN

	// Estado para controle de quorum no líder
	accCount    map[int32]int                        // contador de Accepted
	qTimers     map[int32]*time.Timer               // timers de fallback
	leaderForSN map[int32]int32                     // quem é líder por SN
	lastAccept  map[int32]*pb.ProtocolMessage       // última msg Accept (para reenvio)
}

// NewMultiPaxosMulticastOrderer cria um wrapper multicast sobre o orderer base
func NewMultiPaxosMulticastOrderer(useUDPMulticast bool, baseAddr string, basePort int, ifaceAddr ...string) *MultiPaxosMulticastOrderer {
	o := &MultiPaxosMulticastOrderer{
		MultiPaxosOrderer: &MultiPaxosOrderer{},
		snMembers:         make(map[int32]map[int32]struct{}),
		snGroup:           make(map[int32]multicast.GroupID),
		snGSize:           make(map[int32]int),
		accCount:          make(map[int32]int),
		qTimers:           make(map[int32]*time.Timer),
		leaderForSN:       make(map[int32]int32),
		lastAccept:        make(map[int32]*pb.ProtocolMessage),
	}

	// Escolhe implementação de multicast
	if useUDPMulticast {
		var ifaceIP string
		if len(ifaceAddr) > 0 {
			ifaceIP = ifaceAddr[0]
		}
		if mc, err := multicast.NewUDPMulticastRouter(baseAddr, basePort, ifaceIP); err == nil {
			o.mc = mc
			fmt.Printf("[MC][INIT] Using UDP multicast %s:%d (iface: %s)\n", baseAddr, basePort, ifaceIP)
		} else {
			fmt.Printf("[MC][INIT] UDP multicast failed: %v, falling back to unicast\n", err)
			o.mc = multicast.NewStaticRouter()
		}
	} else {
		o.mc = multicast.NewStaticRouter()
		fmt.Printf("[MC][INIT] Using unicast fallback\n")
	}

	return o
}

func (o *MultiPaxosMulticastOrderer) Init(mngr manager.Manager) {
	// Inicializa o orderer base
	o.MultiPaxosOrderer.Init(mngr)

	// Configura grupo "all" no router multicast
	all := membership.AllNodeIDs()
	o.mc.DefineGroup(multicast.GroupID(0), all...)
	fmt.Printf("[MC][INIT] gid=0(all) members=%v\n", all)

	// Substitui a função emit do orderer base para usar multicast
	originalEmit := o.MultiPaxosOrderer.emit
	o.MultiPaxosOrderer.emit = func(pm *pb.ProtocolMessage) {
		// Se não é mensagem Paxos, usa emit original
		mpx := pm.GetMultipaxos()
		if mpx == nil {
			originalEmit(pm)
			return
		}

		// Aplica otimizações de multicast para mensagens Paxos
		switch mpx.Type.(type) {
		case *pb.MPxMsg_Prepare:
			o.sendToAll("Prepare", pm)

		case *pb.MPxMsg_Accept:
			o.handleAcceptSend(pm)

		case *pb.MPxMsg_Commit:
			o.sendToGroupOrAll("Commit", pm)
			o.cleanupSN(pm.Sn)

		default:
			// Promise/Accepted/outros: broadcast seguro
			o.sendToAll("Other", pm)
		}
	}
}

func (o *MultiPaxosMulticastOrderer) HandleMessage(pm *pb.ProtocolMessage) {
	// Processa otimizações de multicast antes de passar para o orderer base
	if m := pm.GetMultipaxos(); m != nil {
		switch m.Type.(type) {
		case *pb.MPxMsg_Promise:
			o.onPromise(pm.SenderId, pm.Sn)
		case *pb.MPxMsg_Accept:
			o.onAccept(pm.SenderId, pm.Sn)
		case *pb.MPxMsg_Accepted:
			o.onAccepted(pm.SenderId, pm.Sn)
		case *pb.MPxMsg_Commit:
			o.cleanupSN(pm.Sn)
		}
	}
	// Delega processamento principal para o orderer base
	o.MultiPaxosOrderer.HandleMessage(pm)
}

func (o *MultiPaxosMulticastOrderer) handleAcceptSend(pm *pb.ProtocolMessage) {
	o.mu.Lock()
	o.lastAccept[pm.Sn] = proto.Clone(pm).(*pb.ProtocolMessage)
	o.leaderForSN[pm.Sn] = membership.OwnID
	o.accCount[pm.Sn] = 0
	// Limpa timer antigo se existir
	if t := o.qTimers[pm.Sn]; t != nil {
		t.Stop()
		delete(o.qTimers, pm.Sn)
	}
	o.mu.Unlock()

	o.sendToGroupOrAll("Accept", pm)
	o.armQuorumTimer(pm.Sn)
}

func (o *MultiPaxosMulticastOrderer) sendToAll(kind string, pm *pb.ProtocolMessage) {
	o.mc.SendGroup(multicast.GroupID(0), func(dst multicast.ID) *pb.ProtocolMessage {
		return proto.Clone(pm).(*pb.ProtocolMessage)
	})
	fmt.Printf("[MC][SEND] kind=%s sn=%d gid=0 dst=ALL\n", kind, pm.Sn)
}

func (o *MultiPaxosMulticastOrderer) sendToGroupOrAll(kind string, pm *pb.ProtocolMessage) {
	o.mu.RLock()
	gid := o.snGroup[pm.Sn]
	o.mu.RUnlock()

	if gid != 0 {
		o.mc.SendGroup(gid, func(dst multicast.ID) *pb.ProtocolMessage {
			return proto.Clone(pm).(*pb.ProtocolMessage)
		})
		fmt.Printf("[MC][SEND] kind=%s sn=%d gid=%d (group)\n", kind, pm.Sn, gid)
	} else {
		o.sendToAll(kind, pm)
	}
}

func majority(n int) int { return n/2 + 1 }

// onPromise aprende grupos dinamicamente baseado em quem envia Promise
func (o *MultiPaxosMulticastOrderer) onPromise(from, sn int32) {
	o.mu.Lock()
	defer o.mu.Unlock()

	if o.snMembers[sn] == nil {
		o.snMembers[sn] = make(map[int32]struct{})
	}
	o.snMembers[sn][from] = struct{}{}

	// Já existe grupo para este SN
	if o.snGroup[sn] != 0 {
		return
	}

	all := membership.AllNodeIDs()
	clusterQ := majority(len(all))

	// Só define grupo quando temos promessas suficientes
	if len(o.snMembers[sn]) < clusterQ {
		return
	}

	// Cria grupo com tamanho = quórum do cluster (segurança)
	groupSize := clusterQ
	members := make([]int32, 0, groupSize)

	// Escolhe membros que prometeram primeiro
	for id := range o.snMembers[sn] {
		members = append(members, id)
		if len(members) == groupSize {
			break
		}
	}

	// Completa com outros nós se necessário
	if len(members) < groupSize {
		seenMap := make(map[int32]struct{}, len(members))
		for _, m := range members {
			seenMap[m] = struct{}{}
		}
		for _, id := range all {
			if _, seen := seenMap[id]; !seen {
				members = append(members, id)
				if len(members) == groupSize {
					break
				}
			}
		}
	}

	// Fallback: se ainda não tem membros suficientes, usa todos
	if len(members) == 0 {
		members = append(members, all...)
	}

	// Define grupo no router multicast
	gid := multicast.GroupID(1 + (sn % maxGroupIDSpace))
	o.mc.DefineGroup(gid, members...)
	o.snGroup[sn] = gid
	o.snGSize[sn] = len(members)

	fmt.Printf("[MC][GROUP] sn=%d gid=%d groupSize=%d clusterQ=%d members=%v\n",
		sn, gid, len(members), clusterQ, members)
}

func (o *MultiPaxosMulticastOrderer) onAccept(from, sn int32) {
	o.mu.Lock()
	o.leaderForSN[sn] = from
	o.mu.Unlock()
}

func (o *MultiPaxosMulticastOrderer) onAccepted(_from, sn int32) {
	o.mu.RLock()
	isLeader := (o.leaderForSN[sn] == membership.OwnID)
	o.mu.RUnlock()
	if !isLeader {
		return
	}

	o.mu.Lock()
	o.accCount[sn]++
	o.mu.Unlock()
}

func (o *MultiPaxosMulticastOrderer) armQuorumTimer(sn int32) {
	o.mu.Lock()
	if o.qTimers[sn] != nil {
		o.mu.Unlock()
		return
	}

	t := time.AfterFunc(time.Duration(defaultQuorumTimeoutMs)*time.Millisecond, func() {
		all := membership.AllNodeIDs()
		clusterQ := majority(len(all))

		o.mu.RLock()
		acc := o.accCount[sn]
		accept := o.lastAccept[sn]
		o.mu.RUnlock()

		if accept == nil || acc >= clusterQ {
			return
		}

		fmt.Printf("[MC][FALLBACK] quorum timeout sn=%d (acc=%d/<%d) → broadcast Accept\n",
			sn, acc, clusterQ)

		o.sendToAll("Accept-Fallback", accept)
	})

	o.qTimers[sn] = t
	o.mu.Unlock()
}

func (o *MultiPaxosMulticastOrderer) cleanupSN(sn int32) {
	o.mu.Lock()
	if t := o.qTimers[sn]; t != nil {
		t.Stop()
		delete(o.qTimers, sn)
	}
	delete(o.accCount, sn)
	delete(o.lastAccept, sn)
	delete(o.leaderForSN, sn)
	// Limpa mapas de grupo para evitar vazamento de memória
	delete(o.snMembers, sn)
	delete(o.snGroup, sn)
	delete(o.snGSize, sn)
	o.mu.Unlock()
}