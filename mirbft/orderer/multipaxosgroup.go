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
//
// CSMR (Composable State Machine Replication):
// =============================================
// CONCEITO:
//   - Sistema particionado em GRUPOS independentes
//   - Cada grupo executa consenso em paralelo
//   - Reduz tráfego de rede e aumenta throughput
//
// TIPOS DE OPERAÇÕES:
//   1. SINGLE-GROUP (groupId > 0):
//      - Operação afeta apenas 1 grupo (ex: PUT key1)
//      - Multicast seletivo: apenas membros do grupo
//      - Paralelismo total entre grupos
//
//   2. MULTI-GROUP (groupId = 0, len(TouchedGroups) > 1):
//      - Operação afeta múltiplos grupos (ex: range query em 2 partições)
//      - Broadcast global: TODOS os nós participam
//      - Barreira global: congela propostas locais (atomic global order)
//
// MEMBROS vs NÃO-MEMBROS:
//   - Membros: executam consenso completo + batch
//   - Não-membros: recebem apenas Commit (digest) para checkpoint
//   - Estado diverge entre nós (by design, não é bug!)
//
// DETERMINISMO CRÍTICO:
//   - groups.yml DEVE ser idêntico em todos os nós
//   - Ordem dos grupos afeta cálculo de globalSN (round-robin)
//   - Divergência = consenso quebrado (FAIL-FAST)
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

// GetGroupLeader retorna líder do grupo via round-robin ENTRE MEMBROS DO GRUPO
// CRÍTICO: Só escolhe líder que seja membro (interseção segmentLeaders ∩ members)
// Round-robin por segmento para balancear carga entre líderes elegíveis
func (am *AtomicMulticast) GetGroupLeader(g GroupID, segmentLeaders []int32) int32 {
	if len(segmentLeaders) == 0 {
		return -1
	}
	
	// Obtém membros do grupo
	members := am.GetGroupMembers(uint32(g))
	if members == nil || len(members) == 0 {
		return -1
	}
	
	// Interseção: segmentLeaders ∩ members
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
		// FAIL-FAST: configuração inválida (nenhum líder é membro)
		panic(fmt.Sprintf("FATAL: grupo %d não tem líderes elegíveis (segmentLeaders ∩ members = ∅)", g))
	}
	
	// Round-robin com groupID para balancear carga entre segmentos
	idx := int(g) % len(eligible)
	return eligible[idx]
}

// GetAvailableGroups retorna grupos não-globais para cliente distribuir requests
// FAIL-FAST: Se groups.yml não existe, retorna erro (alinhado com LoadGroupsFromYAML)
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
	if len(groups) == 0 {
		panic("FATAL: config/groups.yml não define nenhum grupo (além de 0)")
	}
	return groups
}