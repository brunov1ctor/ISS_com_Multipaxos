package orderer

import (
	"fmt"
	"sync"
	"gopkg.in/yaml.v2"
	"io/ioutil"
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

// GroupConfig represents YAML configuration for groups and compositions
type GroupConfig struct {
	Groups       map[GroupID][]int32    `yaml:"groups"`
	Compositions []CompositionConfig    `yaml:"compositions"`
}

// CompositionConfig defines how SMRs are composed
type CompositionConfig struct {
	Name  string           `yaml:"name"`
	Steps []CompositionStep `yaml:"steps"`
}

// CompositionStep defines one step in a composed operation
type CompositionStep struct {
	Component string  `yaml:"component"`
	Group     GroupID `yaml:"group"`
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
		fmt.Printf("[MPX-MC][CONFIG] Loaded group %d: %v\n", gid, members)
	}

	// Log compositions (setup would be done externally)
	for _, comp := range config.Compositions {
		fmt.Printf("[CSMR][CONFIG] Composition %s with %d steps\n", comp.Name, len(comp.Steps))
	}

	return nil
}