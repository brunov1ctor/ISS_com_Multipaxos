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
	"sort"
	"sync"
	"gopkg.in/yaml.v2"
	"io/ioutil"
	"github.com/hyperledger-labs/mirbft/membership"
	pb "github.com/hyperledger-labs/mirbft/protobufs"
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
	delivered  map[uint32]map[uint64]bool  // grupo -> GSN -> já entregue?
	deliveryMu sync.RWMutex                // Protege delivered
}

// GroupConfig - Estrutura para carregar configuração de grupos do YAML
type GroupConfig struct {
	Groups map[GroupID][]int32 `yaml:"groups"` // Mapeamento grupo -> membros
}
func NewAtomicMulticast() *AtomicMulticast {
	am := &AtomicMulticast{
		groups:     make(map[GroupID][]int32),
		delivered:  make(map[uint32]map[uint64]bool),
	}
	am.groups[0] = membership.AllNodeIDs()
	return am
}
func (am *AtomicMulticast) SetSequencer(seq *MultiPaxosMulticastOrderer) {
	am.sequencer = seq
}
func (am *AtomicMulticast) AMulticast(req *pb.ClientRequest) error {
	if len(req.TouchedGroups) == 0 {
		return fmt.Errorf("request has no touched groups")
	}
	if len(req.TouchedGroups) == 1 {
		return nil
	}
	if am.sequencer == nil {
		return fmt.Errorf("sequencer not set")
	}
	if req.GSN == 0 {
		req.GSN = am.sequencer.GetNextGSN()
	}
	fmt.Printf("[AMCAST] Multicasting req gsn=%d to groups=%v\n", req.GSN, req.TouchedGroups)
	return nil
}
func (am *AtomicMulticast) ADeliver(groupID uint32, req *pb.ClientRequest) bool {
	// TODAS as requests têm GSN (ordem global)
	gsn := req.GSN
	if gsn == 0 {
		fmt.Printf("[AMCAST][WARN] Request without GSN, rejecting\n")
		return false
	}
	
	// Verifica se este grupo é tocado pela request
	touchesThisGroup := false
	for _, gid := range req.TouchedGroups {
		if gid == groupID {
			touchesThisGroup = true
			break
		}
	}
	if !touchesThisGroup {
		// Pula GSNs que não tocam este grupo (ordem global preservada)
		fmt.Printf("[AMCAST] Skipping gsn=%d (doesn't touch group %d)\n", gsn, groupID)
		return false
	}
	
	// Verifica se já foi entregue neste grupo
	am.deliveryMu.Lock()
	if am.delivered[groupID] == nil {
		am.delivered[groupID] = make(map[uint64]bool)
	}
	if am.delivered[groupID][gsn] {
		am.deliveryMu.Unlock()
		fmt.Printf("[AMCAST] Already delivered gsn=%d group=%d\n", gsn, groupID)
		return false
	}
	am.delivered[groupID][gsn] = true
	am.deliveryMu.Unlock()
	
	fmt.Printf("[AMCAST] Delivering gsn=%d to group=%d (global order)\n", gsn, groupID)
	return true
}
func (am *AtomicMulticast) TryDeliverPending(groupID uint32) []*pb.ClientRequest {
	// Sistema simplificado - sem pending complexo, apenas ordem GSN
	return nil
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
	data, err := ioutil.ReadFile(filename)
	if err != nil {
		return err
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
	eligible := make([]int32, 0)
	for _, leader := range segmentLeaders {
		for _, member := range members {
			if leader == member {
				eligible = append(eligible, leader)
				break
			}
		}
	}
	if len(eligible) == 0 {
		panic(fmt.Sprintf("FATAL: grupo %d não tem líderes elegíveis (segmentLeaders ∩ members = ∅)", g))
	}
	return eligible[0]
}
func GetAvailableGroups() []uint32 {
	data, err := ioutil.ReadFile("config/groups.yml")
	if err != nil {
		panic(fmt.Sprintf("FATAL: config/groups.yml é obrigatório. Erro: %v", err))
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
		panic("FATAL: config/groups.yml não define nenhum grupo")
	}
	return groups
}
func (am *AtomicMulticast) UpdateSequencerGroup() {
	am.mu.Lock()
	allNodes := membership.AllNodeIDs()
	if len(allNodes) > 0 {
		am.groups[0] = append([]int32{}, allNodes...)
		fmt.Printf("[GROUPS] Updated sequencer group 0 with %d members: %v\n", len(allNodes), allNodes)
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