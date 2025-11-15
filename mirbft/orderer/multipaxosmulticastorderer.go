package orderer

import (
	"fmt"
	"sync"
	"time"

	"github.com/hyperledger-labs/mirbft/manager"
	"github.com/hyperledger-labs/mirbft/membership"
	"github.com/hyperledger-labs/mirbft/messenger"
	"github.com/hyperledger-labs/mirbft/multicast"
	pb "github.com/hyperledger-labs/mirbft/protobufs"
)

func majority(n int) int { return n/2 + 1 }

const (
	defaultGroupSize        = 3
	defaultQuorumTimeoutMs  = 250
	maxGroupIDSpace   int32 = 1024
)

type MultiPaxosMulticastOrderer struct {
	*MultiPaxosOrderer
	mc multicast.Router

	mu sync.RWMutex

	// aprendizado de grupo (via Promise)
	snMembers map[int32]map[int32]struct{}
	snGroup   map[int32]multicast.GroupID
	snGSize   map[int32]int

	// quorum Accepted (no líder)
	accCount    map[int32]int
	qTimers     map[int32]*time.Timer
	leaderForSN map[int32]int32

	// último Accept por SN (para fallback reenvio)
	lastAccept map[int32]*pb.ProtocolMessage
}

func (o *MultiPaxosMulticastOrderer) Init(mngr manager.Manager) {
	// base
	o.MultiPaxosOrderer = &MultiPaxosOrderer{}
	o.MultiPaxosOrderer.Init(mngr)

	// router
	if shim := multicast.NewMessengerShim(messenger.EnqueueMsg); shim != nil {
		o.mc = shim
	} else {
		o.mc = multicast.NewStaticRouter()
	}

	// grupo "all"
	all := membership.AllNodeIDs()
	o.mc.DefineGroup(multicast.GroupID(0), all...)
	fmt.Printf("[MC][INIT] gid=0(all) members=%v\n", all)

	// mapas
	o.snMembers = make(map[int32]map[int32]struct{})
	o.snGroup = make(map[int32]multicast.GroupID)
	o.snGSize = make(map[int32]int)
	o.accCount = make(map[int32]int)
	o.qTimers = make(map[int32]*time.Timer)
	o.leaderForSN = make(map[int32]int32)
	o.lastAccept = make(map[int32]*pb.ProtocolMessage)

	// === Substitui o emissor da base por um roteador multicast ===
	o.MultiPaxosOrderer.emit = func(pm *pb.ProtocolMessage) {
		mpx := pm.GetMultipaxos()
		if mpx == nil {
			// mensagens não-Paxos → broadcast conservador
			o.broadcast(pm)
			return
		}
		switch mpx.Type.(type) {
		case *pb.MPxMsg_Prepare:
			// bootstrap / view-change: all
			o.broadcast(pm)
			fmt.Printf("[MC][SEND] kind=Prepare sn=%d gid=0 dst=ALL\n", pm.Sn)

		case *pb.MPxMsg_Accept:
			// guarda último Accept do sn e líder local do sn
			o.mu.Lock()
			o.lastAccept[pm.Sn] = deepCopyPM(pm)
			o.leaderForSN[pm.Sn] = membership.OwnID
			o.mu.Unlock()

			// Accept por grupo (fallback p/ all)
			if o.sendToGroupOrAll("Accept", pm) {
				o.armQuorumTimer(pm.Sn)
			}

		case *pb.MPxMsg_Commit:
			// Commit por grupo (fallback p/ all)
			o.sendToGroupOrAll("Commit", pm)

		default:
			// Promise / Accepted / outros → broadcast por segurança
			o.broadcast(pm)
		}
	}
}

func (o *MultiPaxosMulticastOrderer) HandleMessage(pm *pb.ProtocolMessage) {
	if m := pm.GetMultipaxos(); m != nil {
		switch m.Type.(type) {
		case *pb.MPxMsg_Promise:
			o.onPromise(pm.SenderId, pm.Sn)
		case *pb.MPxMsg_Accept:
			// quem enviou Accept para este SN (réplicas registram também)
			o.onAccept(pm.SenderId, pm.Sn)
		case *pb.MPxMsg_Accepted:
			// no líder, contabiliza quorum do grupo
			o.onAccepted(pm.SenderId, pm.Sn)
		}
	}
	o.MultiPaxosOrderer.HandleMessage(pm)
}

func (o *MultiPaxosMulticastOrderer) onPromise(from, sn int32) {
	o.mu.Lock()
	defer o.mu.Unlock()

	if o.snMembers[sn] == nil {
		o.snMembers[sn] = make(map[int32]struct{})
	}
	o.snMembers[sn][from] = struct{}{}

	if o.snGroup[sn] != 0 {
		return
	}

	if len(o.snMembers[sn]) >= majority(len(membership.AllNodeIDs())) {
		// escolhe até 3 membros dentre quem prometeu; completa se precisar
		members := make([]int32, 0, defaultGroupSize)
		for id := range o.snMembers[sn] {
			members = append(members, id)
			if len(members) == defaultGroupSize {
				break
			}
		}
		if len(members) < defaultGroupSize {
			for _, id := range membership.AllNodeIDs() {
				seen := false
				for _, m := range members {
					if m == id {
						seen = true
						break
					}
				}
				if !seen {
					members = append(members, id)
					if len(members) == defaultGroupSize {
						break
					}
				}
			}
		}
		if len(members) == 0 {
			members = append(members, membership.AllNodeIDs()...)
		}
		gid := multicast.GroupID(1 + (sn % maxGroupIDSpace))
		o.mc.DefineGroup(gid, members...)
		o.snGroup[sn] = gid
		o.snGSize[sn] = len(members)
		fmt.Printf("[MC][GROUP] sn=%d gid=%d members=%v\n", sn, gid, members)
	}
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

func (o *MultiPaxosMulticastOrderer) sendToGroupOrAll(kind string, pm *pb.ProtocolMessage) bool {
	o.mu.RLock()
	gid := o.snGroup[pm.Sn]
	o.mu.RUnlock()

	if gid != 0 {
		o.sendGroup(gid, pm)
		fmt.Printf("[MC][SEND] kind=%s sn=%d gid=%d (group)\n", kind, pm.Sn, gid)
		return true
	}
	o.broadcast(pm)
	fmt.Printf("[MC][SEND] kind=%s sn=%d gid=0 dst=ALL (fallback)\n", kind, pm.Sn)
	return true
}

func (o *MultiPaxosMulticastOrderer) broadcast(pm *pb.ProtocolMessage) {
	for _, dst := range membership.AllNodeIDs() {
		messenger.EnqueueMsg(deepCopyPM(pm), dst)
	}
}

func (o *MultiPaxosMulticastOrderer) sendGroup(gid multicast.GroupID, pm *pb.ProtocolMessage) {
	o.mc.SendGroup(gid, func(dst multicast.ID) *pb.ProtocolMessage {
		return deepCopyPM(pm)
	})
}

func deepCopyPM(pm *pb.ProtocolMessage) *pb.ProtocolMessage {
	cp := *pm
	return &cp
}

// Timer: se não fechar maioria do GRUPO em T ms, reenvia Accept para ALL (somente esse sn).
func (o *MultiPaxosMulticastOrderer) armQuorumTimer(sn int32) {
	o.mu.Lock()
	if o.qTimers[sn] != nil {
		o.mu.Unlock()
		return
	}
	t := time.AfterFunc(time.Duration(defaultQuorumTimeoutMs)*time.Millisecond, func() {
		o.mu.RLock()
		gsz := o.snGSize[sn]
		acc := o.accCount[sn]
		accept := o.lastAccept[sn]
		o.mu.RUnlock()

		if gsz == 0 || accept == nil {
			return
		}
		if acc >= majority(gsz) {
			return
		}

		fmt.Printf("[MC][FALLBACK] quorum timeout sn=%d (acc=%d/<%d>) → broadcast Accept\n",
			sn, acc, majority(gsz))
		o.broadcast(accept)
	})
	o.qTimers[sn] = t
	o.mu.Unlock()
}

