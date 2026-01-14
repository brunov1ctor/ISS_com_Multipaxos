package orderer

import (
	"sync"

	"github.com/hyperledger-labs/mirbft/manager"
	"github.com/hyperledger-labs/mirbft/membership"
	"github.com/hyperledger-labs/mirbft/messenger"
	pb "github.com/hyperledger-labs/mirbft/protobufs"
)

// === MULTIPAXOS COM MULTICAST SELETIVO ===
// Extensão do MultiPaxosOrderer que adiciona:
// 1. Comunicação por grupo (multicast seletivo)
// 2. Quorum baseado em membros do grupo, não cluster inteiro
// 3. Roteamento de mensagens apenas para nós do grupo

type MultiPaxosMulticastOrderer struct {
	*MultiPaxosOrderer
	
	// === PENDING GROUPS ===
	// Guarda groupID para instâncias que ainda não existem
	// Quando instância é criada, aplica membros do grupo
	pendingGroups map[int32]uint32
	mu            sync.RWMutex
}

func (o *MultiPaxosMulticastOrderer) Init(mngr manager.Manager) {
	if o.MultiPaxosOrderer == nil {
		o.MultiPaxosOrderer = &MultiPaxosOrderer{}
	}
	if o.pendingGroups == nil {
		o.pendingGroups = make(map[int32]uint32)
	}
	
	// Inicializa orderer base (cria o.MultiPaxosOrderer.am)
	o.MultiPaxosOrderer.Init(mngr)
	
	// === CALLBACK: APLICA GRUPOS PENDENTES ===
	// Quando instância é criada, verifica se há groupID pendente
	o.MultiPaxosOrderer.onInstanceCreated = func(sn int32) {
		o.tryApplyPendingGroups(sn)
	}

	// === ROTEAMENTO POR GRUPO ===
	// Intercepta emit() para enviar apenas para membros do grupo
	// Extrai groupID da mensagem e roteia seletivamente
	originalEmit := o.MultiPaxosOrderer.emit
	o.MultiPaxosOrderer.emit = func(pm *pb.ProtocolMessage) {
		mpx := pm.GetMultipaxos()
		if mpx == nil {
			// Não é mensagem MultiPaxos, usa broadcast padrão
			originalEmit(pm)
			return
		}

		// Extrai groupID da mensagem
		var groupID uint32
		switch msg := mpx.Type.(type) {
		case *pb.MPxMsg_Prepare:
			groupID = msg.Prepare.GetGroupId()
		case *pb.MPxMsg_Promise:
			groupID = msg.Promise.GetGroupId()
		case *pb.MPxMsg_Accept:
			groupID = msg.Accept.GetGroupId()
		case *pb.MPxMsg_Accepted:
			// Accepted não tem GroupId, busca via SN
			o.mu.RLock()
			groupID = o.pendingGroups[pm.Sn]
			o.mu.RUnlock()
		case *pb.MPxMsg_Commit:
			groupID = msg.Commit.GetGroupId()
		}

		if groupID == 0 {
			// Grupo 0 = broadcast para todos
			originalEmit(pm)
			return
		}

		// === MULTICAST SELETIVO ===
		// Envia apenas para membros do grupo
		members := o.MultiPaxosOrderer.am.getGroupMembers(GroupID(groupID))
		if len(members) == 0 {
			// Grupo não definido, fallback para broadcast
			originalEmit(pm)
		} else {
			for _, nodeID := range members {
				if nodeID != membership.OwnID {
					messenger.EnqueueMsg(pm, nodeID)
				}
			}
		}
	}
}

func (o *MultiPaxosMulticastOrderer) HandleMessage(pm *pb.ProtocolMessage) {
	// === EXTRAI GROUPID DA MENSAGEM ===
	// Identifica qual grupo esta mensagem pertence
	mpx := pm.GetMultipaxos()
	var groupID uint32
	if mpx != nil {
		switch msg := mpx.Type.(type) {
		case *pb.MPxMsg_Prepare:
			groupID = msg.Prepare.GetGroupId()
		case *pb.MPxMsg_Promise:
			groupID = msg.Promise.GetGroupId()
		case *pb.MPxMsg_Accept:
			groupID = msg.Accept.GetGroupId()
		case *pb.MPxMsg_Commit:
			groupID = msg.Commit.GetGroupId()
		}
	}
	
	// Processa mensagem normalmente
	o.MultiPaxosOrderer.HandleMessage(pm)
	
	// === APLICA MEMBROS DO GRUPO NA INSTÂNCIA ===
	// Se instância existe, configura quorum baseado no grupo
	// Se não existe, guarda em pendingGroups
	if groupID > 0 {
		o.applyGroupID(pm.Sn, groupID)
	}
}

// === CONFIGURA MEMBROS DO GRUPO NA INSTÂNCIA ===
// Define quorum baseado nos membros do grupo, não do cluster inteiro
func (o *MultiPaxosMulticastOrderer) SetInstanceMembers(sn int32, groupID uint32) bool {
	inst, ok := o.MultiPaxosOrderer.dispatcher.load(sn)
	if !ok || inst == nil {
		// Instância ainda não existe
		return false
	}
	
	members := o.MultiPaxosOrderer.am.getGroupMembers(GroupID(groupID))
	if len(members) > 0 {
		// Usa membros do grupo
		inst.SetMembers(members)
	} else {
		// Fallback: usa todos os nós
		inst.SetMembers(membership.AllNodeIDs())
	}
	return true
}

// === CARREGA GRUPOS DO YAML ===
// Define quais nós pertencem a cada grupo
func (o *MultiPaxosMulticastOrderer) LoadGroupsFromYAML(filename string) error {
	return o.MultiPaxosOrderer.am.LoadGroupsFromYAML(filename)
}

// === APLICA GROUPID (IMEDIATO OU PENDENTE) ===
// Tenta aplicar membros do grupo na instância
// Se instância não existe, guarda em pendingGroups
func (o *MultiPaxosMulticastOrderer) applyGroupID(sn int32, groupID uint32) {
	if o.SetInstanceMembers(sn, groupID) {
		// Instância existe, membros aplicados
		return
	}
	
	// Instância não existe, guarda para depois
	o.mu.Lock()
	if existing, exists := o.pendingGroups[sn]; !exists || existing == groupID {
		o.pendingGroups[sn] = groupID
	}
	o.mu.Unlock()
}

// === APLICA GRUPOS PENDENTES ===
// Chamado quando instância é criada (via onInstanceCreated callback)
// Aplica groupID que estava pendente
func (o *MultiPaxosMulticastOrderer) tryApplyPendingGroups(sn int32) {
	o.mu.Lock()
	groupID, exists := o.pendingGroups[sn]
	if exists {
		delete(o.pendingGroups, sn)
	}
	o.mu.Unlock()
	
	if exists {
		o.SetInstanceMembers(sn, groupID)
	}
}

