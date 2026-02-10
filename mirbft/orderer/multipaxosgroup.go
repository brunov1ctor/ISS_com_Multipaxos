/*
MultiPaxos Group Management - Gerenciamento de Grupos para Multicast Atômico

Gerencia grupos de nós para coordenação de multicast atômico entre grupos.
Implementa a camada de abstração entre o protocolo MultiPaxos e o sistema
de grupos distribuídos.

Componentes Principais:
- AtomicMulticast: Coordenador central de operações entre grupos
- GroupConfig: Carregador de configuração determinística via YAML
- Membership Management: Controle de quais nós pertencem a cada grupo

Arquitetura de Grupos:
- Grupo 0: Sequenciador GSN (todos os nós) - garante ordem global
- Grupos 1,2,3...: Grupos de dados (subconjuntos de nós) - processam operações
- Configuração via YAML: Determinística e consistente entre todos os nós

Funcionalidades Principais:
- AMulticast: Inicia operação de multicast atômico entre grupos
- ADeliver: Entrega mensagem respeitando ordem global GSN
- Membership Validation: Verifica se nó pertence ao grupo
- Leader Selection: Seleciona líder do grupo baseado em segmento
- YAML Configuration: Carrega e valida configuração de grupos

Garantias:
- Ordem Global: GSN garante ordem consistente entre todos os grupos
- Determinismo: Configuração YAML garante consistência de membership
- Atomicidade: Operações cross-group são entregues atomicamente
- Liveness: Sistema de re-forward garante entrega eventual
*/
package orderer
import (
	"fmt"
	"path/filepath"
	"sort"
	"sync"
	"gopkg.in/yaml.v2"
	"io/ioutil"
	"github.com/hyperledger-labs/mirbft/membership"
	logger "github.com/rs/zerolog/log"
)
const (
	GROUP_GLOBAL = uint32(0)
)
type GroupID uint32
// AtomicMulticast - Coordenador de multicast atômico entre grupos
// Garante que operações cross-group sejam entregues na mesma ordem em todos os grupos
type AtomicMulticast struct {
	mu         sync.RWMutex                // Protege groups
	groups     map[GroupID][]int32         // groupID -> lista de nós membros
	sequencer  *MultiPaxosMulticastOrderer // Referência ao sequenciador GSN
}

// GroupConfig - Estrutura para carregar configuração de grupos do YAML
type GroupConfig struct {
	Groups map[GroupID][]int32 `yaml:"groups"` // Mapeamento grupo -> membros
}
func NewAtomicMulticast() *AtomicMulticast {
	am := &AtomicMulticast{
		groups: make(map[GroupID][]int32),
	}
	am.groups[0] = membership.AllNodeIDs()
	return am
}
func (am *AtomicMulticast) SetSequencer(seq *MultiPaxosMulticastOrderer) {
	am.sequencer = seq
}
// AMulticast removed - logic is in PreprocessRequest()
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
func (am *AtomicMulticast) GroupExists(groupID uint32) bool {
	am.mu.RLock()
	defer am.mu.RUnlock()
	_, exists := am.groups[GroupID(groupID)]
	return exists
}
func (am *AtomicMulticast) GetDefinedGroups() []uint32 {
	am.mu.RLock()
	defer am.mu.RUnlock()
	groups := make([]uint32, 0, len(am.groups))
	for gid := range am.groups {
		groups = append(groups, uint32(gid))
	}
	// ✅ FIX: Ordenação determinística (grupo 0 sempre primeiro)
	sort.Slice(groups, func(i, j int) bool { return groups[i] < groups[j] })
	return groups
}
func (am *AtomicMulticast) GetDataGroups() []uint32 {
	am.mu.RLock()
	defer am.mu.RUnlock()
	groups := make([]uint32, 0, len(am.groups))
	for gid := range am.groups {
		if gid != 0 {
			groups = append(groups, uint32(gid))
		}
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
	maxGroupID := uint32(0)
	for gid, members := range config.Groups {
		if len(members) == 0 {
			return fmt.Errorf("group %d has no members", gid)
		}
		if uint32(gid) > maxGroupID {
			maxGroupID = uint32(gid)
		}
		am.DefineGroup(gid, members...)
	}
	if maxGroupID > 0 {
		fmt.Printf("[GROUPS] maxGroupID=%d (NumBuckets must be >= %d)\n", maxGroupID, maxGroupID+1)
	}
	return nil
}
func (am *AtomicMulticast) GetGroupLeader(g GroupID, segmentLeaders []int32) int32 {
	if len(segmentLeaders) == 0 {
		return -1
	}
	members := am.GetGroupMembers(uint32(g))
	if members == nil || len(members) == 0 {
		return -1
	}
	memberSet := make(map[int32]struct{}, len(members))
	for _, member := range members {
		memberSet[member] = struct{}{}
	}
	eligible := make([]int32, 0)
	for _, leader := range segmentLeaders {
		if _, ok := memberSet[leader]; ok {
			eligible = append(eligible, leader)
		}
	}
	if len(eligible) == 0 {
		logger.Fatal().Uint32("groupID", uint32(g)).Msg("No eligible leaders for group")
	}
	return eligible[0]
}
func GetAvailableGroups(configPath string) []uint32 {
	if configPath == "" {
		configPath = "config/groups.yml"
	}
	data, err := ioutil.ReadFile(filepath.Clean(configPath))
	if err != nil {
		logger.Fatal().Err(err).Str("path", configPath).Msg("Failed to read groups configuration")
	}
	var config GroupConfig
	if err := yaml.Unmarshal(data, &config); err != nil {
		panic(fmt.Sprintf("FATAL: Falha ao parsear config/groups.yml: %v", err))
	}
	groups := make([]uint32, 0, len(config.Groups))
	for gid := range config.Groups {
		groups = append(groups, uint32(gid))
	}
	sort.Slice(groups, func(i, j int) bool { return groups[i] < groups[j] })
	if len(groups) == 0 {
		logger.Fatal().Str("path", configPath).Msg("No groups defined in configuration")
	}
	return groups
}
func (am *AtomicMulticast) UpdateSequencerGroup() {
	am.mu.Lock()
	allNodes := membership.AllNodeIDs()
	if len(allNodes) > 0 {
		am.groups[0] = append([]int32{}, allNodes...)
		fmt.Printf("[GROUPS] Sequencer group 0 with %d members: %v (GSN only, not for data)\n", len(allNodes), allNodes)
	} else {
		fmt.Printf("[GROUPS][WARN] AllNodeIDs() still empty, keeping group 0 as-is\n")
	}
	am.mu.Unlock()
}
func (am *AtomicMulticast) IsMember(groupID uint32, nodeID int32) bool {
	members := am.GetGroupMembers(groupID)
	if members == nil {
		return false
	}
	for _, m := range members {
		if m == nodeID {
			return true
		}
	}
	return false
}

// GetGroupLeaderForSegment - Seleciona líder determinístico do grupo baseado no segmento
// Usa mesma fórmula que SetMembers: leader = members[sn % len(members)]
// Garante consistência entre runSegment() e SetMembers()
func (am *AtomicMulticast) GetGroupLeaderForSegment(gid uint32, firstSN int32, numGroups int32) int32 {
	members := am.GetGroupMembers(gid)
	if members == nil || len(members) == 0 {
		return -1
	}
	
	idx := int(firstSN % int32(len(members)))
	return members[idx]
}