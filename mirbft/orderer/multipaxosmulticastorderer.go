package orderer

import (
	"fmt"
	"os"
	"sync"

	"github.com/hyperledger-labs/mirbft/manager"
	"github.com/hyperledger-labs/mirbft/membership"
	"github.com/hyperledger-labs/mirbft/messenger"
	pb "github.com/hyperledger-labs/mirbft/protobufs"
)

// MultiPaxosMulticastOrderer estende MultiPaxosOrderer com comunicação seletiva por grupo
// Em vez de broadcast para todos os nós, envia mensagens apenas para membros do grupo
// Isso reduz tráfego de rede e permite paralelismo entre grupos
type MultiPaxosMulticastOrderer struct {
	*MultiPaxosOrderer
	pendingGroups map[int32]uint32 // Guarda groupID de instâncias ainda não criadas
	groupsFile    string           // Caminho do arquivo YAML com configuração de grupos
	mu            sync.RWMutex
}

func (o *MultiPaxosMulticastOrderer) Init(mngr manager.Manager) {
	if o.MultiPaxosOrderer == nil {
		o.MultiPaxosOrderer = &MultiPaxosOrderer{}
	}
	if o.pendingGroups == nil {
		o.pendingGroups = make(map[int32]uint32)
	}
	
	o.MultiPaxosOrderer.Init(mngr)
	
	// Carrega configuração de grupos do arquivo YAML (se existir)
	if o.groupsFile != "" {
		err := o.MultiPaxosOrderer.am.LoadGroupsFromYAML(o.groupsFile)
		if err != nil {
			if os.IsNotExist(err) {
				fmt.Printf("[MULTICAST] groups.yml não encontrado (%s), rodando em broadcast mode\n", o.groupsFile)
			} else {
				fmt.Printf("[MULTICAST][ERRO] Falha ao carregar groups.yml: %v\n", err)
				return
			}
		} else {
			fmt.Printf("[MULTICAST] groups.yml carregado com sucesso: %s\n", o.groupsFile)
		}
	}
	
	// Callback chamado quando instância é criada (aplica grupos pendentes)
	o.MultiPaxosOrderer.onInstanceCreated = func(sn int32) {
		o.tryApplyPendingGroups(sn)
	}

	// Intercepta função emit() para rotear mensagens apenas para membros do grupo
	// Em vez de broadcast, envia apenas para nós relevantes
	originalEmit := o.MultiPaxosOrderer.emit
	o.MultiPaxosOrderer.emit = func(pm *pb.ProtocolMessage) {
		mpx := pm.GetMultipaxos()
		if mpx == nil {
			originalEmit(pm)
			return
		}

		// Extrai groupID da mensagem Paxos
		var groupID uint32
		switch msg := mpx.Type.(type) {
		case *pb.MPxMsg_Prepare:
			groupID = msg.Prepare.GetGroupId()
		case *pb.MPxMsg_Promise:
			groupID = msg.Promise.GetGroupId()
		case *pb.MPxMsg_Accept:
			groupID = msg.Accept.GetGroupId()
		case *pb.MPxMsg_Accepted:
			// Accepted não tem groupID, busca de pendingGroups ou instância
			o.mu.RLock()
			groupID = o.pendingGroups[pm.Sn]
			o.mu.RUnlock()
			if groupID == 0 {
				if inst, ok := o.MultiPaxosOrderer.dispatcher.load(pm.Sn); ok && inst != nil {
					groupID = inst.bucketId
				}
			}
			if groupID > 0 {
				o.mu.Lock()
				delete(o.pendingGroups, pm.Sn)
				o.mu.Unlock()
			}
		case *pb.MPxMsg_Commit:
			groupID = msg.Commit.GetGroupId()
		}

		// Grupo 0 = broadcast para todos
		if groupID == 0 {
			originalEmit(pm)
			return
		}

		// Envia apenas para membros do grupo (multicast seletivo)
		members := o.MultiPaxosOrderer.am.GetGroupMembers(groupID)
		if len(members) == 0 {
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
	// Extrai groupID da mensagem recebida
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
	
	// Aplica membros do grupo na instância (se existir) ou guarda para depois
	if groupID > 0 {
		o.applyGroupID(pm.Sn, groupID)
	}
}

// SetInstanceMembers configura quorum baseado nos membros do grupo, não do cluster inteiro
// Isso permite que cada grupo opere independentemente com seu próprio quorum
func (o *MultiPaxosMulticastOrderer) SetInstanceMembers(sn int32, groupID uint32) bool {
	inst, ok := o.MultiPaxosOrderer.dispatcher.load(sn)
	if !ok || inst == nil {
		return false
	}
	
	members := o.MultiPaxosOrderer.am.GetGroupMembers(groupID)
	if len(members) > 0 {
		inst.SetMembers(members)
	} else {
		inst.SetMembers(membership.AllNodeIDs())
	}
	
	o.mu.Lock()
	delete(o.pendingGroups, sn)
	o.mu.Unlock()
	
	return true
}

func (o *MultiPaxosMulticastOrderer) LoadGroupsFromYAML(filename string) error {
	if o.MultiPaxosOrderer == nil || o.MultiPaxosOrderer.am == nil {
		o.groupsFile = filename
		return nil
	}
	return o.MultiPaxosOrderer.am.LoadGroupsFromYAML(filename)
}

// applyGroupID tenta aplicar membros do grupo na instância
// Se instância não existe ainda, guarda em pendingGroups para aplicar depois
func (o *MultiPaxosMulticastOrderer) applyGroupID(sn int32, groupID uint32) {
	if o.SetInstanceMembers(sn, groupID) {
		return
	}
	
	o.mu.Lock()
	if existing, exists := o.pendingGroups[sn]; !exists || existing == groupID {
		o.pendingGroups[sn] = groupID
	}
	o.mu.Unlock()
}

// tryApplyPendingGroups aplica groupID que estava pendente quando instância é criada
func (o *MultiPaxosMulticastOrderer) tryApplyPendingGroups(sn int32) {
	o.mu.RLock()
	groupID, exists := o.pendingGroups[sn]
	o.mu.RUnlock()
	
	if exists {
		o.SetInstanceMembers(sn, groupID)
	}
}