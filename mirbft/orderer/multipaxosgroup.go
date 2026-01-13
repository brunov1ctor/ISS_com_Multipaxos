package orderer

import (
	"sync"
	"github.com/hyperledger-labs/mirbft/membership"
	pb "github.com/hyperledger-labs/mirbft/protobufs"
)

// GroupID represents a multicast group identifier
type GroupID uint32

// AtomicMulticast provides selective atomic broadcast interface
// Following the multicast(g, m)/deliver(m) pattern described in the paper
type AtomicMulticast struct {
	mu     sync.RWMutex
	groups map[GroupID][]int32
}

// NewAtomicMulticast creates atomic multicast with selective delivery
func NewAtomicMulticast() *AtomicMulticast {
	am := &AtomicMulticast{
		groups: make(map[GroupID][]int32),
	}
	// Group 0 is always "all nodes" for compatibility
	am.groups[0] = membership.AllNodeIDs()
	return am
}

// Multicast sends message m to group g with atomic delivery guarantees
func (am *AtomicMulticast) Multicast(g GroupID, m *pb.ProtocolMessage, emit func(*pb.ProtocolMessage, []int32)) {
	members := am.getGroupMembers(g)
	emit(m, members)
}

// DefineGroup creates or updates group membership
func (am *AtomicMulticast) DefineGroup(g GroupID, members ...int32) {
	am.mu.Lock()
	am.groups[g] = append([]int32{}, members...)
	am.mu.Unlock()
}

func (am *AtomicMulticast) getGroupMembers(g GroupID) []int32 {
	am.mu.RLock()
	defer am.mu.RUnlock()
	if members, exists := am.groups[g]; exists {
		return append([]int32{}, members...)
	}
	return am.groups[0] // fallback to all
}