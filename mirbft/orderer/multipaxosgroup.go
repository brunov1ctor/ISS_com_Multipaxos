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

const (
	GROUP_GLOBAL = uint32(0) // Grupo 0 = broadcast para todos os nós
)

type GroupID uint32

// AtomicMulticast gerencia grupos de nós para comunicação seletiva
// Cada grupo tem seu próprio contador de sequência (SN) independente
type AtomicMulticast struct {
	mu       sync.RWMutex
	groups   map[GroupID][]int32      // Mapa de grupo -> lista de nós membros
	snCounters map[GroupID]*int32     // Contador de SN por grupo
}

type GroupConfig struct {
	Groups map[GroupID][]int32 `yaml:"groups"`
}

func NewAtomicMulticast() *AtomicMulticast {
	am := &AtomicMulticast{
		groups:     make(map[GroupID][]int32),
		snCounters: make(map[GroupID]*int32),
	}
	// Grupo 0 sempre contém todos os nós (broadcast global)
	am.groups[0] = membership.AllNodeIDs()
	initialSN := int32(-1)
	am.snCounters[0] = &initialSN
	return am
}

// DefineGroup cria ou atualiza um grupo com seus membros
func (am *AtomicMulticast) DefineGroup(g GroupID, members ...int32) {
	am.mu.Lock()
	am.groups[g] = append([]int32{}, members...)
	if am.snCounters[g] == nil {
		initialSN := int32(-1)
		am.snCounters[g] = &initialSN
	}
	am.mu.Unlock()
}

// GetGroupMembers retorna os nós que pertencem a um grupo
// Retorna nil se o grupo não existe (strict mode - sem fallback silencioso)
func (am *AtomicMulticast) GetGroupMembers(groupID uint32) []int32 {
	am.mu.RLock()
	defer am.mu.RUnlock()
	if members, exists := am.groups[GroupID(groupID)]; exists {
		return append([]int32{}, members...)
	}
	return nil // Grupo não existe - caller deve tratar
}

// GroupExists verifica se um grupo foi definido
func (am *AtomicMulticast) GroupExists(groupID uint32) bool {
	am.mu.RLock()
	defer am.mu.RUnlock()
	_, exists := am.groups[GroupID(groupID)]
	return exists
}

// GetDefinedGroups retorna lista ordenada de todos os grupos definidos
func (am *AtomicMulticast) GetDefinedGroups() []uint32 {
	am.mu.RLock()
	defer am.mu.RUnlock()
	groups := make([]uint32, 0, len(am.groups))
	for gid := range am.groups {
		groups = append(groups, uint32(gid))
	}
	// Ordena para garantir determinismo
	for i := 0; i < len(groups); i++ {
		for j := i + 1; j < len(groups); j++ {
			if groups[i] > groups[j] {
				groups[i], groups[j] = groups[j], groups[i]
			}
		}
	}
	return groups
}

// LoadGroupsFromYAML carrega configuração de grupos de um arquivo YAML
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

// NextSN retorna o próximo número de sequência para um grupo
// Cada grupo tem seu contador independente para paralelismo
func (am *AtomicMulticast) NextSN(g GroupID) int32 {
	am.mu.RLock()
	counter := am.snCounters[g]
	am.mu.RUnlock()
	if counter == nil {
		return -1
	}
	return atomic.AddInt32(counter, 1)
}

// GetGroupLeader determina qual nó é líder de um grupo
// Usa round-robin entre os líderes do segmento
func (am *AtomicMulticast) GetGroupLeader(g GroupID, segmentLeaders []int32) int32 {
	if len(segmentLeaders) == 0 {
		return -1
	}
	idx := int(g) % len(segmentLeaders)
	return segmentLeaders[idx]
}

// ValidateAndRouteRequest determina para qual grupo uma requisição deve ir
// Retorna GROUP_GLOBAL se a requisição toca múltiplos grupos ou é inválida
func ValidateAndRouteRequest(req *pb.ClientRequest) uint32 {
	if req == nil || req.GetGroupId() == GROUP_GLOBAL {
		return GROUP_GLOBAL
	}
	
	// Se a requisição especifica grupos tocados, valida se é apenas um
	if len(req.TouchedGroups) > 0 {
		var g uint32
		seen := make(map[uint32]struct{})
		for _, x := range req.TouchedGroups {
			seen[x] = struct{}{}
			g = x
			if len(seen) > 1 {
				return GROUP_GLOBAL // Toca múltiplos grupos -> broadcast
			}
		}
		return g // Toca apenas um grupo
	}
	
	// Usa o groupId especificado na requisição (sem validação)
	return req.GetGroupId()
}