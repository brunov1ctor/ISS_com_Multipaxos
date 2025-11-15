package multicast

import (
	"sync"

	"github.com/hyperledger-labs/mirbft/membership"
	"github.com/hyperledger-labs/mirbft/messenger"
	pb "github.com/hyperledger-labs/mirbft/protobufs"
)

// staticRouter mantém grupos em memória e usa messenger.EnqueueMsg para enviar.
type staticRouter struct {
	mu     sync.RWMutex
	groups map[GroupID][]ID
}

// NewStaticRouter cria um roteador estático com um grupo "all" (0)
// contendo todos os nós conhecidos via membership.
func NewStaticRouter() Router {
	r := &staticRouter{
		groups: make(map[GroupID][]ID),
	}
	// grupo "all" (0) = todos os nós conhecidos
	all := make([]ID, 0, len(membership.AllNodeIDs()))
	for _, nid := range membership.AllNodeIDs() {
		all = append(all, nid)
	}
	r.groups[GroupID(0)] = all
	return r
}

func (r *staticRouter) DefineGroup(g GroupID, members ...ID) {
	r.mu.Lock()
	defer r.mu.Unlock()
	cp := make([]ID, len(members))
	copy(cp, members)
	r.groups[g] = cp
}

func (r *staticRouter) SendGroup(g GroupID, builder func(dst ID) *pb.ProtocolMessage) {
	r.mu.RLock()
	members, ok := r.groups[g]
	r.mu.RUnlock()
	if !ok {
		// Se o grupo não existe, não envia nada.
		return
	}
	for _, dst := range members {
		if dst == membership.OwnID {
			// Não envia para si mesmo pelo caminho de rede.
			continue
		}
		if msg := builder(dst); msg != nil {
			messenger.EnqueueMsg(msg, dst)
		}
	}
}

