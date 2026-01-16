package orderer

import (
	"fmt"
	"sync"
	"sync/atomic"
	"gopkg.in/yaml.v2"
	"io/ioutil"
	"github.com/hyperledger-labs/mirbft/config"
	"github.com/hyperledger-labs/mirbft/membership"
	pb "github.com/hyperledger-labs/mirbft/protobufs"
)

const (
	GROUP_GLOBAL = uint32(0)
)

type GroupID uint32

type AtomicMulticast struct {
	mu       sync.RWMutex
	groups   map[GroupID][]int32
	snCounters map[GroupID]*int32
}

type GroupConfig struct {
	Groups map[GroupID][]int32 `yaml:"groups"`
}

func NewAtomicMulticast() *AtomicMulticast {
	am := &AtomicMulticast{
		groups:     make(map[GroupID][]int32),
		snCounters: make(map[GroupID]*int32),
	}
	am.groups[0] = membership.AllNodeIDs()
	initialSN := int32(-1)
	am.snCounters[0] = &initialSN
	return am
}

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
	am.mu.RLock()
	defer am.mu.RUnlock()
	if members, exists := am.groups[GroupID(groupID)]; exists {
		return append([]int32{}, members...)
	}
	return am.groups[0]
}

func (am *AtomicMulticast) LoadGroupsFromYAML(filename string) error {
	data, err := ioutil.ReadFile(filename)
	if err != nil {
		return err
	}
	var config GroupConfig
	if err := yaml.Unmarshal(data, &config); err != nil {
		return fmt.Errorf("failed to parse YAML: %v", err)
	}
	for gid, members := range config.Groups {
		am.DefineGroup(gid, members...)
	}
	return nil
}

func (am *AtomicMulticast) NextSN(g GroupID) int32 {
	am.mu.RLock()
	counter := am.snCounters[g]
	am.mu.RUnlock()
	if counter == nil {
		return -1
	}
	return atomic.AddInt32(counter, 1)
}

func (am *AtomicMulticast) GetGroupLeader(g GroupID, segmentLeaders []int32) int32 {
	if len(segmentLeaders) == 0 {
		return -1
	}
	idx := int(g) % len(segmentLeaders)
	return segmentLeaders[idx]
}

func ValidateAndRouteRequest(req *pb.ClientRequest) uint32 {
	if req == nil {
		return GROUP_GLOBAL
	}
	
	if req.GetGroupId() == GROUP_GLOBAL {
		return GROUP_GLOBAL
	}
	
	tg := req.GetTouchedGroups()
	if len(tg) > 0 {
		var g uint32
		seen := make(map[uint32]struct{})
		for _, x := range tg {
			if x >= uint32(config.Config.NumBuckets) {
				return GROUP_GLOBAL
			}
			seen[x] = struct{}{}
			g = x
			if len(seen) > 1 {
				return GROUP_GLOBAL
			}
		}
		return g
	}
	
	groupId := req.GetGroupId()
	if groupId >= uint32(config.Config.NumBuckets) {
		return GROUP_GLOBAL
	}
	return groupId
}