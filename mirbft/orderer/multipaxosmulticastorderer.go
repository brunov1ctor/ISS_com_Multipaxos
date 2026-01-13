package orderer

import (
	"fmt"
	"sync"

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
	am                *AtomicMulticast // Selective atomic multicast interface

	composedMu   sync.RWMutex
	composedWith map[string]*MultiPaxosMulticastOrderer // Connected SMRs
}

// NewMultiPaxosMulticastOrderer creates a new orderer with group-based communication
func NewMultiPaxosMulticastOrderer() *MultiPaxosMulticastOrderer {
	o := &MultiPaxosMulticastOrderer{
		MultiPaxosOrderer: &MultiPaxosOrderer{},
		am:                NewAtomicMulticast(),
		composedWith:      make(map[string]*MultiPaxosMulticastOrderer),
	}

	fmt.Printf("[MPX-MC][INIT] Using group-based communication with MIR batches\n")
	return o
}

func (o *MultiPaxosMulticastOrderer) Init(mngr manager.Manager) {
	// Inicializa o orderer base
	o.MultiPaxosOrderer.Init(mngr)

	fmt.Printf("[MPX-MC][INIT] Using bucket-based routing with groups\n")

	// Substitui a função emit do orderer base para usar grupos por bucket
	originalEmit := o.MultiPaxosOrderer.emit
	o.MultiPaxosOrderer.emit = func(pm *pb.ProtocolMessage) {
		// Se não é mensagem Paxos, usa emit original
		mpx := pm.GetMultipaxos()
		if mpx == nil {
			originalEmit(pm)
			return
		}

		// Usa group_id da mensagem Paxos se disponível
		var groupID GroupID = 0
		switch msg := mpx.Type.(type) {
		case *pb.MPxMsg_Accept:
			groupID = GroupID(msg.Accept.GetGroupId())
		case *pb.MPxMsg_Commit:
			groupID = GroupID(msg.Commit.GetGroupId())
		}

		if groupID == 0 {
			// Broadcast para todos se não há grupo específico
			o.am.Multicast(0, pm, o.emitToMembers)
		} else {
			// Usa grupo específico
			o.am.Multicast(groupID, pm, o.emitToMembers)
		}
	}
}

func (o *MultiPaxosMulticastOrderer) HandleMessage(pm *pb.ProtocolMessage) {
	// Delega processamento para o orderer base
	o.MultiPaxosOrderer.HandleMessage(pm)
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

// Public API for users to define groups

// DefineGroup allows users to define custom groups by node IDs
func (o *MultiPaxosMulticastOrderer) DefineGroup(gid GroupID, nodeIDs ...int32) {
	o.am.DefineGroup(gid, nodeIDs...)
}

// GetGroupMembers returns current group members
func (o *MultiPaxosMulticastOrderer) GetGroupMembers(gid GroupID) []int32 {
	return o.am.getGroupMembers(gid)
}

// LoadGroupsFromYAML loads group configuration from YAML file
func (o *MultiPaxosMulticastOrderer) LoadGroupsFromYAML(filename string) error {
	return o.am.LoadGroupsFromYAML(filename)
}

// ComposeWith connects this SMR with another SMR for composition
func (o *MultiPaxosMulticastOrderer) ComposeWith(name string, other *MultiPaxosMulticastOrderer) {
	o.composedMu.Lock()
	o.composedWith[name] = other
	o.composedMu.Unlock()

	fmt.Printf("[CSMR] Connected SMR component: %s\n", name)
}

// ExecuteAndForward executes locally then forwards to composed SMR
func (o *MultiPaxosMulticastOrderer) ExecuteAndForward(data []byte, targetComponent string, targetGroup GroupID) {
	// Execute locally first (existing Paxos)
	pm := &pb.ProtocolMessage{
		SenderId: membership.OwnID,
		Sn:       0, // Will be set by Paxos
		// TODO: se você tiver um campo no proto pra payload (ex.: Request/Batch/Data),
		// coloque o "data" nele aqui.
	}

	o.am.Multicast(0, pm, o.emitToMembers)

	// Forward to composed SMR (com leitura protegida)
	o.composedMu.RLock()
	target := o.composedWith[targetComponent]
	o.composedMu.RUnlock()

	if target != nil {
		target.am.Multicast(targetGroup, pm, target.emitToMembers)
		fmt.Printf("[CSMR] Forwarded to component %s group %d\n", targetComponent, targetGroup)
	}
}

