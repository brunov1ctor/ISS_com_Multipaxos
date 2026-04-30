// MultiPaxos Group Management - Gerenciamento de grupos para multicast atômico.
// Grupo 0: Sequenciador GSN (todos os nós). Grupos 1+: Dados (subconjuntos).
package orderer

import (
	"fmt"
	"io/ioutil"
	"path/filepath"
	"sort"
	"sync"
	"gopkg.in/yaml.v2"
	"github.com/hyperledger-labs/mirbft/membership"
)

type GroupID uint32

type AtomicMulticast struct {
	mu        sync.RWMutex
	groups    map[GroupID][]int32
	sequencer *MultiPaxosMulticastOrderer
}

type GroupConfig struct {
	Groups map[GroupID][]int32 `yaml:"groups"`
}

func NewAtomicMulticast() *AtomicMulticast {
	am := &AtomicMulticast{groups: make(map[GroupID][]int32)}
	am.groups[0] = membership.AllNodeIDs()
	return am
}

func (am *AtomicMulticast) SetSequencer(seq *MultiPaxosMulticastOrderer) {
	am.sequencer = seq
}

func (am *AtomicMulticast) DefineGroup(g GroupID, members ...int32) {
	am.mu.Lock()
	am.groups[g] = append([]int32{}, members...)
	am.mu.Unlock()
}

func (am *AtomicMulticast) GetGroupMembers(groupID uint32) []int32 {
	am.mu.RLock()
	defer am.mu.RUnlock()
	if members, exists := am.groups[GroupID(groupID)]; exists {
		return append([]int32{}, members...)
	}
	return nil
}

func (am *AtomicMulticast) GetDefinedGroups() []uint32 {
	am.mu.RLock()
	defer am.mu.RUnlock()
	groups := make([]uint32, 0, len(am.groups))
	for gid := range am.groups {
		groups = append(groups, uint32(gid))
	}
	sort.Slice(groups, func(i, j int) bool { return groups[i] < groups[j] })
	return groups
}

func (am *AtomicMulticast) LoadGroupsFromYAML(filename string) error {
	if filename == "" {
		return fmt.Errorf("filename cannot be empty")
	}
	data, err := ioutil.ReadFile(filepath.Clean(filename))
	if err != nil {
		return fmt.Errorf("failed to read file: %w", err)
	}
	var config GroupConfig
	if err := yaml.Unmarshal(data, &config); err != nil {
		return fmt.Errorf("failed to parse YAML: %v", err)
	}
	for gid, members := range config.Groups {
		if len(members) == 0 {
			return fmt.Errorf("group %d has no members", gid)
		}
		am.DefineGroup(gid, members...)
	}
	return nil
}

func (am *AtomicMulticast) UpdateSequencerGroup() {
	am.mu.Lock()
	defer am.mu.Unlock()
	allNodes := membership.AllNodeIDs()
	if len(allNodes) > 0 {
		am.groups[0] = append([]int32{}, allNodes...)
	}
}

func (am *AtomicMulticast) IsMember(groupID uint32, nodeID int32) bool {
	for _, m := range am.GetGroupMembers(groupID) {
		if m == nodeID { return true }
	}
	return false
}

func (am *AtomicMulticast) GetGroupLeaderForSegment(gid uint32, firstSN int32, _ int32) int32 {
	members := am.GetGroupMembers(gid)
	if len(members) == 0 { return -1 }
	return members[int(firstSN)%len(members)]
}
