package orderer

import (
	"fmt"
	"sync"
	"time"

	"google.golang.org/protobuf/proto"
	"github.com/hyperledger-labs/mirbft/manager"
	"github.com/hyperledger-labs/mirbft/membership"
	"github.com/hyperledger-labs/mirbft/messenger"
	pb "github.com/hyperledger-labs/mirbft/protobufs"
)

const (
	defaultQuorumTimeoutMs = 250
)

// MultiPaxosMulticastOrderer uses group-based communication with existing MIR batches
type MultiPaxosMulticastOrderer struct {
	*MultiPaxosOrderer // Composição: reutiliza toda lógica base
	am *AtomicMulticast  // Selective atomic multicast interface
	composedWith map[string]*MultiPaxosMulticastOrderer // Connected SMRs

	mu sync.RWMutex

	// Estado para otimização de grupos dinâmicos
	snMembers map[int32]map[int32]struct{} // quem prometeu por SN
	snGroup   map[int32]GroupID            // grupo definido por SN
	snGSize   map[int32]int                // tamanho do grupo por SN

	// Estado para controle de quorum no líder
	accCount    map[int32]int                        // contador de Accepted
	qTimers     map[int32]*time.Timer               // timers de fallback
	leaderForSN map[int32]int32                     // quem é líder por SN
	lastAccept  map[int32]*pb.ProtocolMessage       // última msg Accept (para reenvio)
}

// NewMultiPaxosMulticastOrderer creates a new orderer with group-based communication
func NewMultiPaxosMulticastOrderer() *MultiPaxosMulticastOrderer {
	o := &MultiPaxosMulticastOrderer{
		MultiPaxosOrderer: &MultiPaxosOrderer{},
		am:          NewAtomicMulticast(),
		composedWith: make(map[string]*MultiPaxosMulticastOrderer),
		snMembers:         make(map[int32]map[int32]struct{}),
		snGroup:           make(map[int32]GroupID),
		snGSize:           make(map[int32]int),
		accCount:          make(map[int32]int),
		qTimers:           make(map[int32]*time.Timer),
		leaderForSN:       make(map[int32]int32),
		lastAccept:        make(map[int32]*pb.ProtocolMessage),
	}

	fmt.Printf("[MPX-MC][INIT] Using group-based communication with MIR batches\n")
	return o
}

func (o *MultiPaxosMulticastOrderer) Init(mngr manager.Manager) {
	// Inicializa o orderer base
	o.MultiPaxosOrderer.Init(mngr)

	fmt.Printf("[MPX-MC][INIT] gid=0(all) members=%v\n", membership.AllNodeIDs())

	// Substitui a função emit do orderer base para usar grupos
	originalEmit := o.MultiPaxosOrderer.emit
	o.MultiPaxosOrderer.emit = func(pm *pb.ProtocolMessage) {
		// Se não é mensagem Paxos, usa emit original
		mpx := pm.GetMultipaxos()
		if mpx == nil {
			originalEmit(pm)
			return
		}

		// Aplica otimizações de grupo para mensagens Paxos
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
	o.am.Multicast(0, pm, o.emitToMembers)
	fmt.Printf("[MPX-MC][SEND] kind=%s sn=%d gid=0 dst=ALL\n", kind, pm.Sn)
}

func (o *MultiPaxosMulticastOrderer) sendToGroupOrAll(kind string, pm *pb.ProtocolMessage) {
	o.mu.RLock()
	gid := o.snGroup[pm.Sn]
	o.mu.RUnlock()

	if gid != 0 {
		o.am.Multicast(gid, pm, o.emitToMembers)
		fmt.Printf("[MPX-MC][SEND] kind=%s sn=%d gid=%d (group)\n", kind, pm.Sn, gid)
	} else {
		o.sendToAll(kind, pm)
	}
}

// emitToMembers sends message to specific group members using existing MIR infrastructure
func (o *MultiPaxosMulticastOrderer) emitToMembers(pm *pb.ProtocolMessage, members []int32) {
	for _, nodeID := range members {
		if nodeID == membership.OwnID {
			continue
		}
		messenger.EnqueueMsg(proto.Clone(pm).(*pb.ProtocolMessage), nodeID)
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

	// Define grupo na API
	gid := GroupID(1 + (sn % 8)) // Limita a 8 grupos
	o.am.DefineGroup(gid, members...)
	o.snGroup[sn] = gid
	o.snGSize[sn] = len(members)

	fmt.Printf("[MPX-MC][GROUP] sn=%d gid=%d groupSize=%d clusterQ=%d members=%v\n",
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

		fmt.Printf("[MPX-MC][FALLBACK] quorum timeout sn=%d (acc=%d/<%d) → broadcast Accept\n",
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

// Public API for users to define groups

// DefineGroup allows users to define custom groups by node IDs
func (o *MultiPaxosMulticastOrderer) DefineGroup(gid GroupID, nodeIDs ...int32) {
	o.am.DefineGroup(gid, nodeIDs...)
}

// GetGroupMembers returns current group members
func (o *MultiPaxosMulticastOrderer) GetGroupMembers(gid GroupID) []int32 {
	return o.am.getGroupMembers(gid)
}

// ComposeWith connects this SMR with another SMR for composition
func (o *MultiPaxosMulticastOrderer) ComposeWith(name string, other *MultiPaxosMulticastOrderer) {
	o.mu.Lock()
	o.composedWith[name] = other
	o.mu.Unlock()
	fmt.Printf("[CSMR] Connected SMR component: %s\n", name)
}

// ExecuteAndForward executes locally then forwards to composed SMR
func (o *MultiPaxosMulticastOrderer) ExecuteAndForward(data []byte, targetComponent string, targetGroup GroupID) {
	// Execute locally first (existing Paxos)
	pm := &pb.ProtocolMessage{
		SenderId: membership.OwnID,
		Sn: 0, // Will be set by Paxos
		// Add data to message
	}
	o.sendToAll("Local", pm)
	
	// Forward to composed SMR
	o.mu.RLock()
	target := o.composedWith[targetComponent]
	o.mu.RUnlock()
	
	if target != nil {
		target.am.Multicast(targetGroup, pm, target.emitToMembers)
		fmt.Printf("[CSMR] Forwarded to component %s group %d\n", targetComponent, targetGroup)
	}
}