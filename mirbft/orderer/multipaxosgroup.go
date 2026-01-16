package orderer

import (
	"fmt"
	"sync"
	"sync/atomic"
	"gopkg.in/yaml.v2"
	"io/ioutil"
	"github.com/hyperledger-labs/mirbft/membership"
)

const (
	GROUP_GLOBAL = uint32(0) // Grupo 0 = broadcast global (todos os nós)
)

type GroupID uint32

// AtomicMulticast gerencia grupos de nós para comunicação seletiva.
// Implementa CSMR (Composable State Machine Replication):
//   - Nós membros executam batch real
//   - Nós não-membros entregam NIL no mesmo SN
//   - Estado diverge entre nós (by design)
type AtomicMulticast struct {
	mu         sync.RWMutex
	groups     map[GroupID][]int32  // grupo -> lista de nós membros
	snCounters map[GroupID]*int32   // contador de SN local por grupo
}

type GroupConfig struct {
	Groups map[GroupID][]int32 `yaml:"groups"`
}

func NewAtomicMulticast() *AtomicMulticast {
	am := &AtomicMulticast{
		groups:     make(map[GroupID][]int32),
		snCounters: make(map[GroupID]*int32),
	}
	// Grupo 0 sempre existe com todos os nós (usado para multi-grupo)
	am.groups[0] = membership.AllNodeIDs()
	initialSN := int32(-1)
	am.snCounters[0] = &initialSN
	return am
}

// DefineGroup cria/atualiza grupo com seus membros
func (am *AtomicMulticast) DefineGroup(g GroupID, members ...int32) {
	am.mu.Lock()
	am.groups[g] = append([]int32{}, members...)
	if am.snCounters[g] == nil {
		initialSN := int32(-1)
		am.snCounters[g] = &initialSN
	}
	am.mu.Unlock()
}

// GetGroupMembers retorna nós do grupo (nil se não existe)
func (am *AtomicMulticast) GetGroupMembers(groupID uint32) []int32 {
	am.mu.RLock()
	defer am.mu.RUnlock()
	if members, exists := am.groups[GroupID(groupID)]; exists {
		return append([]int32{}, members...)
	}
	return nil
}

func (am *AtomicMulticast) GroupExists(groupID uint32) bool {
	am.mu.RLock()
	defer am.mu.RUnlock()
	_, exists := am.groups[GroupID(groupID)]
	return exists
}

// GetDefinedGroups retorna lista ordenada de grupos (determinismo)
func (am *AtomicMulticast) GetDefinedGroups() []uint32 {
	am.mu.RLock()
	defer am.mu.RUnlock()
	groups := make([]uint32, 0, len(am.groups))
	for gid := range am.groups {
		groups = append(groups, uint32(gid))
	}
	// Ordena para garantir determinismo no cálculo de globalSN
	for i := 0; i < len(groups); i++ {
		for j := i + 1; j < len(groups); j++ {
			if groups[i] > groups[j] {
				groups[i], groups[j] = groups[j], groups[i]
			}
		}
	}
	return groups
}

// LoadGroupsFromYAML carrega grupos do arquivo YAML
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
		if len(members) == 0 {
			return fmt.Errorf("group %d has no members", gid)
		}
		am.DefineGroup(gid, members...)
	}
	return nil
}

// NextSN retorna próximo SN local do grupo (não é globalSN)
func (am *AtomicMulticast) NextSN(g GroupID) int32 {
	am.mu.RLock()
	counter := am.snCounters[g]
	if counter == nil {
		am.mu.RUnlock()
		return -1
	}
	// Mantém lock até usar counter (evita race condition)
	result := atomic.AddInt32(counter, 1)
	am.mu.RUnlock()
	return result
}

// GetGroupLeader retorna líder do grupo via round-robin
func (am *AtomicMulticast) GetGroupLeader(g GroupID, segmentLeaders []int32) int32 {
	if len(segmentLeaders) == 0 {
		return -1
	}
	idx := int(g) % len(segmentLeaders)
	return segmentLeaders[idx]
}

// GetAvailableGroups retorna grupos não-globais para cliente distribuir requests
func GetAvailableGroups() []uint32 {
	data, err := ioutil.ReadFile("config/groups.yml")
	if err != nil {
		return []uint32{1, 2, 3, 4} // fallback
	}
	var config GroupConfig
	if err := yaml.Unmarshal(data, &config); err != nil {
		return []uint32{1, 2, 3, 4}
	}
	groups := make([]uint32, 0, len(config.Groups))
	for gid := range config.Groups {
		if gid > 0 { // exclui GROUP_GLOBAL
			groups = append(groups, uint32(gid))
		}
	}
	// Ordena
	for i := 0; i < len(groups); i++ {
		for j := i + 1; j < len(groups); j++ {
			if groups[i] > groups[j] {
				groups[i], groups[j] = groups[j], groups[i]
			}
		}
	}
	return groups
}