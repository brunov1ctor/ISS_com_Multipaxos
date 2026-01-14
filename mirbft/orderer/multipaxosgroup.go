package orderer

import (
	"fmt"
	"sync"
	"sync/atomic"
	"gopkg.in/yaml.v2"
	"io/ioutil"
	"github.com/hyperledger-labs/mirbft/membership"
	pb "github.com/hyperledger-labs/mirbft/protobufs"
)

// GroupID represents a multicast group identifier
// NOTA: GroupID 0 é reservado para broadcast (todos os nós)
//       Não use 0 para grupos reais
type GroupID uint32

// AtomicMulticast provides selective atomic broadcast interface
// Following the multicast(g, m)/deliver(m) pattern described in the paper
type AtomicMulticast struct {
	mu       sync.RWMutex
	groups   map[GroupID][]int32
	snCounters map[GroupID]*int32 // Contador de SN independente por grupo
}

// GroupConfig represents YAML configuration for groups
type GroupConfig struct {
	Groups map[GroupID][]int32 `yaml:"groups"`
}

// NewAtomicMulticast creates atomic multicast with selective delivery
func NewAtomicMulticast() *AtomicMulticast {
	am := &AtomicMulticast{
		groups:     make(map[GroupID][]int32),
		snCounters: make(map[GroupID]*int32),
	}
	// Group 0 is always "all nodes" for compatibility
	am.groups[0] = membership.AllNodeIDs()
	initialSN := int32(-1)
	am.snCounters[0] = &initialSN
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
	if am.snCounters[g] == nil {
		initialSN := int32(-1)
		am.snCounters[g] = &initialSN
	}
	am.mu.Unlock()
}

func (am *AtomicMulticast) GetGroupMembers(groupID uint32) []int32 {
	return am.getGroupMembers(GroupID(groupID))
}

func (am *AtomicMulticast) getGroupMembers(g GroupID) []int32 {
	am.mu.RLock()
	defer am.mu.RUnlock()
	if members, exists := am.groups[g]; exists {
		return append([]int32{}, members...)
	}
	return am.groups[0] // fallback to all
}

// LoadGroupsFromYAML loads group configuration from YAML file
func (am *AtomicMulticast) LoadGroupsFromYAML(filename string) error {
	data, err := ioutil.ReadFile(filename)
	if err != nil {
		return fmt.Errorf("failed to read config file: %v", err)
	}

	var config GroupConfig
	if err := yaml.Unmarshal(data, &config); err != nil {
		return fmt.Errorf("failed to parse YAML: %v", err)
	}

	// Define groups from config
	for gid, members := range config.Groups {
		am.DefineGroup(gid, members...)
	}

	return nil
}

// NextSN retorna o próximo SN para o grupo especificado
func (am *AtomicMulticast) NextSN(g GroupID) int32 {
	am.mu.RLock()
	counter := am.snCounters[g]
	am.mu.RUnlock()
	
	if counter == nil {
		return -1
	}
	return atomic.AddInt32(counter, 1)
}

// GetGroupLeader retorna o líder determinístico para o grupo
// DENTRO do conjunto de líderes do segmento (integração com leader_policy)
func (am *AtomicMulticast) GetGroupLeader(g GroupID, segmentLeaders []int32) int32 {
	if len(segmentLeaders) == 0 {
		return -1
	}
	
	// Líder do grupo = segmentLeaders[groupId % len(segmentLeaders)]
	// Distribui grupos entre líderes do segmento
	idx := int(g) % len(segmentLeaders)
	return segmentLeaders[idx]
}