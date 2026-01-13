package orderer

import (
	"fmt"
	"sort"
	"sync"

	"github.com/hyperledger-labs/mirbft/manager"
	"github.com/hyperledger-labs/mirbft/membership"
	"github.com/hyperledger-labs/mirbft/messenger"
	pb "github.com/hyperledger-labs/mirbft/protobufs"
)

// MultiPaxosMulticastOrderer uses group-based communication
type MultiPaxosMulticastOrderer struct {
	*MultiPaxosOrderer
	am *AtomicMulticast
	// Guarda groupID para instâncias que ainda não existem
	pendingGroups map[int32]uint32
	mu            sync.RWMutex
}

func (o *MultiPaxosMulticastOrderer) Init(mngr manager.Manager) {
	// Inicializa campos se não foram inicializados
	if o.MultiPaxosOrderer == nil {
		o.MultiPaxosOrderer = &MultiPaxosOrderer{}
	}
	if o.am == nil {
		o.am = NewAtomicMulticast()
	}
	if o.pendingGroups == nil {
		o.pendingGroups = make(map[int32]uint32)
	}
	
	o.MultiPaxosOrderer.Init(mngr)
	
	// Configura hook para aplicar groupIDs pendentes
	o.MultiPaxosOrderer.onInstanceCreated = func(sn int32) {
		o.tryApplyPendingGroups(sn)
	}

	originalEmit := o.MultiPaxosOrderer.emit
	o.MultiPaxosOrderer.emit = func(pm *pb.ProtocolMessage) {
		mpx := pm.GetMultipaxos()
		if mpx == nil {
			originalEmit(pm)
			return
		}

		var groupID GroupID = 0
		switch msg := mpx.Type.(type) {
		case *pb.MPxMsg_Prepare:
			groupID = GroupID(msg.Prepare.GetGroupId())
			if groupID == 0 {
				groupID = o.determineGroupForSN(pm.Sn)
			}
			if groupID > 0 {
				o.SetInstanceMembers(pm.Sn, uint32(groupID))
			}
		case *pb.MPxMsg_Promise:
			groupID = GroupID(msg.Promise.GetGroupId())
			if groupID == 0 {
				groupID = o.determineGroupForSN(pm.Sn)
			}
		case *pb.MPxMsg_Accept:
			groupID = GroupID(msg.Accept.GetGroupId())
			if groupID == 0 {
				groupID = o.determineGroupForSN(pm.Sn)
			}
			if groupID > 0 {
				o.SetInstanceMembers(pm.Sn, uint32(groupID))
			}
		case *pb.MPxMsg_Commit:
			groupID = GroupID(msg.Commit.GetGroupId())
			if groupID == 0 {
				groupID = o.determineGroupForSN(pm.Sn)
			}
		}

		if groupID == 0 {
			o.am.Multicast(0, pm, o.emitToMembers)
		} else {
			o.am.Multicast(groupID, pm, o.emitToMembers)
		}
	}
}

func (o *MultiPaxosMulticastOrderer) HandleMessage(pm *pb.ProtocolMessage) {
	// Captura groupID ANTES de processar no base
	mpx := pm.GetMultipaxos()
	var groupID GroupID = 0
	if mpx != nil {
		switch msg := mpx.Type.(type) {
		case *pb.MPxMsg_Prepare:
			groupID = GroupID(msg.Prepare.GetGroupId())
			if groupID == 0 {
				groupID = o.determineGroupForSN(pm.Sn)
			}
		case *pb.MPxMsg_Promise:
			groupID = GroupID(msg.Promise.GetGroupId())
			if groupID == 0 {
				groupID = o.determineGroupForSN(pm.Sn)
			}
		case *pb.MPxMsg_Accept:
			groupID = GroupID(msg.Accept.GetGroupId())
			if groupID == 0 {
				groupID = o.determineGroupForSN(pm.Sn)
			}
		case *pb.MPxMsg_Commit:
			groupID = GroupID(msg.Commit.GetGroupId())
			if groupID == 0 {
				groupID = o.determineGroupForSN(pm.Sn)
			}
		default:
			groupID = o.determineGroupForSN(pm.Sn)
		}
	}
	
	// Processa no base (pode criar instância ou ir para backlog)
	o.MultiPaxosOrderer.HandleMessage(pm)
	
	// Aplica groupID (imediatamente ou guarda para depois)
	if groupID > 0 {
		o.applyGroupID(pm.Sn, uint32(groupID))
	}
}

// emitToMembers sends message to specific group members using existing MIR infrastructure
func (o *MultiPaxosMulticastOrderer) emitToMembers(pm *pb.ProtocolMessage, members []int32) {
	// Multicast direto para o grupo (quorum já calculado corretamente na instância)
	for _, nodeID := range members {
		if nodeID == membership.OwnID {
			continue
		}
		messenger.EnqueueMsg(pm, nodeID)
	}
}

func (o *MultiPaxosMulticastOrderer) SetInstanceMembers(sn int32, bucketID uint32) bool {
	if inst, ok := o.MultiPaxosOrderer.dispatcher.load(sn); ok && inst != nil {
		members := o.am.getGroupMembers(GroupID(bucketID))
		if len(members) > 0 {
			inst.SetMembers(members)
		} else {
			inst.SetMembers(membership.AllNodeIDs())
		}
		return true
	}
	return false
}

func (o *MultiPaxosMulticastOrderer) determineGroupForSN(sn int32) GroupID {
	groupIDs := make([]GroupID, 0)
	for gid := range o.am.groups {
		if gid > 0 {
			groupIDs = append(groupIDs, gid)
		}
	}
	if len(groupIDs) == 0 {
		return 0
	}
	sort.Slice(groupIDs, func(i, j int) bool {
		return groupIDs[i] < groupIDs[j]
	})
	return groupIDs[int(sn)%len(groupIDs)]
}

func (o *MultiPaxosMulticastOrderer) LoadGroupsFromYAML(filename string) error {
	return o.am.LoadGroupsFromYAML(filename)
}

// applyGroupID tenta aplicar groupID na instância ou guarda para quando ela existir
func (o *MultiPaxosMulticastOrderer) applyGroupID(sn int32, groupID uint32) {
	if o.SetInstanceMembers(sn, groupID) {
		return // Aplicado com sucesso
	}
	
	// Instância não existe ainda, guarda para depois
	o.mu.Lock()
	if existing, exists := o.pendingGroups[sn]; exists && existing != groupID {
		fmt.Printf("[MPX][WARN] GroupID conflict sn=%d: existing=%d new=%d (keeping first)\n", sn, existing, groupID)
		o.mu.Unlock()
		return
	}
	o.pendingGroups[sn] = groupID
	o.mu.Unlock()
}

// tryApplyPendingGroups aplica groupIDs pendentes quando instância é criada
func (o *MultiPaxosMulticastOrderer) tryApplyPendingGroups(sn int32) {
	o.mu.Lock()
	groupID, exists := o.pendingGroups[sn]
	if exists {
		delete(o.pendingGroups, sn)
	}
	o.mu.Unlock()
	
	if exists {
		o.SetInstanceMembers(sn, groupID)
	}
}

