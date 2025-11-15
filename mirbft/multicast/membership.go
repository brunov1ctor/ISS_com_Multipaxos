package multicast

import (
	"fmt"
	"sort"
	"sync"
)

// StaticMembership mantém, em memória, o mapeamento de grupos -> membros.
// É thread-safe e mantém ordem determinística dos membros.
type StaticMembership struct {
	mu sync.RWMutex

	self ID

	// grupos existentes (ordenado)
	groups []GroupID
	// membros por grupo (cada slice ordenado)
	members map[GroupID][]ID
}

// NewStaticMembership cria uma membership vazia com self definido.
func NewStaticMembership(initial map[GroupID][]ID, self ID) *StaticMembership {
	sm := &StaticMembership{
		self:    self,
		members: make(map[GroupID][]ID),
	}
	if initial != nil {
		for g, ids := range initial {
			cp := append([]ID(nil), ids...)
			sort.Slice(cp, func(i, j int) bool { return cp[i] < cp[j] })
			sm.members[g] = cp
			sm.groups = insertGroupIDSorted(sm.groups, g)
		}
	}
	return sm
}

// Self retorna o ID próprio.
func (m *StaticMembership) Self() ID {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.self
}

// SetSelf altera o ID próprio.
func (m *StaticMembership) SetSelf(id ID) {
	m.mu.Lock()
	m.self = id
	m.mu.Unlock()
}

// AddGroup cria/atualiza um grupo com a lista de membros.
// A lista é copiada e ordenada.
func (m *StaticMembership) AddGroup(g GroupID, ids []ID) {
	m.mu.Lock()
	defer m.mu.Unlock()

	cp := append([]ID(nil), ids...)
	sort.Slice(cp, func(i, j int) bool { return cp[i] < cp[j] })
	if _, ok := m.members[g]; !ok {
		m.groups = insertGroupIDSorted(m.groups, g)
	}
	m.members[g] = cp
}

// RemoveGroup remove um grupo.
func (m *StaticMembership) RemoveGroup(g GroupID) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if _, ok := m.members[g]; !ok {
		return
	}
	delete(m.members, g)
	m.groups = removeGroupID(m.groups, g)
}

// Members retorna uma cópia dos membros do grupo (ordenado).
func (m *StaticMembership) Members(g GroupID) []ID {
	m.mu.RLock()
	defer m.mu.RUnlock()

	ids, ok := m.members[g]
	if !ok {
		return nil
	}
	cp := append([]ID(nil), ids...)
	return cp
}

// Groups retorna a lista de grupos (ordenada).
func (m *StaticMembership) Groups() []GroupID {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return append([]GroupID(nil), m.groups...)
}

// MajoritySize retorna o tamanho de maioria (⌊n/2⌋+1) do grupo.
func (m *StaticMembership) MajoritySize(g GroupID) (int, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	ids, ok := m.members[g]
	if !ok || len(ids) == 0 {
		return 0, fmt.Errorf("group %d has no members", g)
	}
	n := len(ids)
	return (n / 2) + 1, nil
}

// Helpers (manter lista ordenada de groups)
func insertGroupIDSorted(list []GroupID, g GroupID) []GroupID {
	i := sort.Search(len(list), func(i int) bool { return list[i] >= g })
	if i < len(list) && list[i] == g {
		return list
	}
	list = append(list, 0)
	copy(list[i+1:], list[i:])
	list[i] = g
	return list
}

func removeGroupID(list []GroupID, g GroupID) []GroupID {
	i := sort.Search(len(list), func(i int) bool { return list[i] >= g })
	if i >= len(list) || list[i] != g {
		return list
	}
	return append(list[:i], list[i+1:]...)
}

