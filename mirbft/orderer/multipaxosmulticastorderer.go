package orderer

import (
	"fmt"
	"sync"
	"time"

	"github.com/hyperledger-labs/mirbft/manager"
	"github.com/hyperledger-labs/mirbft/membership"
	"github.com/hyperledger-labs/mirbft/messenger"
	"github.com/hyperledger-labs/mirbft/multicast"
	pb "github.com/hyperledger-labs/mirbft/protobufs"
)

func majority(n int) int { return n/2 + 1 }

const (
	// Se você quiser manter "grupo menor", pode, mas aí o fallback TEM que usar quórum do cluster.
	// A forma mais segura é: grupo >= quórum do cluster. Aqui vamos fazer isso automaticamente.
	defaultQuorumTimeoutMs  = 250
	maxGroupIDSpace   int32 = 1024
)

type MultiPaxosMulticastOrderer struct {
	*MultiPaxosOrderer
	mc multicast.Router

	mu sync.RWMutex

	// aprendizado de grupo (via Promise)
	snMembers map[int32]map[int32]struct{}
	snGroup   map[int32]multicast.GroupID
	snGSize   map[int32]int

	// quorum Accepted (no líder)
	accCount    map[int32]int
	qTimers     map[int32]*time.Timer
	leaderForSN map[int32]int32

	// último Accept por SN (para fallback reenvio)
	lastAccept map[int32]*pb.ProtocolMessage
}

func (o *MultiPaxosMulticastOrderer) Init(mngr manager.Manager) {
	// base
	o.MultiPaxosOrderer = &MultiPaxosOrderer{}
	o.MultiPaxosOrderer.Init(mngr)

	// router
	if shim := multicast.NewMessengerShim(messenger.EnqueueMsg); shim != nil {
		o.mc = shim
	} else {
		o.mc = multicast.NewStaticRouter()
	}

	// grupo "all"
	all := membership.AllNodeIDs()
	o.mc.DefineGroup(multicast.GroupID(0), all...)
	fmt.Printf("[MC][INIT] gid=0(all) members=%v\n", all)

	// mapas
	o.snMembers = make(map[int32]map[int32]struct{})
	o.snGroup = make(map[int32]multicast.GroupID)
	o.snGSize = make(map[int32]int)
	o.accCount = make(map[int32]int)
	o.qTimers = make(map[int32]*time.Timer)
	o.leaderForSN = make(map[int32]int32)
	o.lastAccept = make(map[int32]*pb.ProtocolMessage)

	// === Substitui o emissor da base por um roteador multicast ===
	o.MultiPaxosOrderer.emit = func(pm *pb.ProtocolMessage) {
		mpx := pm.GetMultipaxos()
		if mpx == nil {
			// mensagens não-Paxos → broadcast conservador
			o.broadcast(pm)
			return
		}

		switch mpx.Type.(type) {
		case *pb.MPxMsg_Prepare:
			// bootstrap / view-change: all
			o.broadcast(pm)
			fmt.Printf("[MC][SEND] kind=Prepare sn=%d gid=0 dst=ALL\n", pm.Sn)

		case *pb.MPxMsg_Accept:
			// prepara contadores/timer para este SN (novo round de Accept)
			o.mu.Lock()
			o.lastAccept[pm.Sn] = deepCopyPM(pm)
			o.leaderForSN[pm.Sn] = membership.OwnID
			o.accCount[pm.Sn] = 0
			// se já tinha timer antigo, mata e remove (novo Accept = novo deadline)
			if t := o.qTimers[pm.Sn]; t != nil {
				t.Stop()
				delete(o.qTimers, pm.Sn)
			}
			o.mu.Unlock()

			// Accept por grupo (fallback p/ all)
			o.sendToGroupOrAll("Accept", pm)
			o.armQuorumTimer(pm.Sn)

		case *pb.MPxMsg_Commit:
			// Commit por grupo (fallback p/ all)
			o.sendToGroupOrAll("Commit", pm)

			// limpeza de estado desse SN (evita leak e timers pendurados)
			o.cleanupSN(pm.Sn)

		default:
			// Promise / Accepted / outros → broadcast por segurança
			o.broadcast(pm)
		}
	}
}

func (o *MultiPaxosMulticastOrderer) HandleMessage(pm *pb.ProtocolMessage) {
	if m := pm.GetMultipaxos(); m != nil {
		switch m.Type.(type) {
		case *pb.MPxMsg_Promise:
			o.onPromise(pm.SenderId, pm.Sn)
		case *pb.MPxMsg_Accept:
			// quem enviou Accept para este SN (réplicas registram também)
			o.onAccept(pm.SenderId, pm.Sn)
		case *pb.MPxMsg_Accepted:
			// no líder, contabiliza quorum do CLUSTER
			o.onAccepted(pm.SenderId, pm.Sn)
		case *pb.MPxMsg_Commit:
			// commit recebido também pode disparar limpeza local
			o.cleanupSN(pm.Sn)
		}
	}
	o.MultiPaxosOrderer.HandleMessage(pm)
}

// define groupSize = quórum do CLUSTER (seguro)
// e monta grupo com IDs que prometeram (e completa se faltar).
func (o *MultiPaxosMulticastOrderer) onPromise(from, sn int32) {
	o.mu.Lock()
	defer o.mu.Unlock()

	if o.snMembers[sn] == nil {
		o.snMembers[sn] = make(map[int32]struct{})
	}
	o.snMembers[sn][from] = struct{}{}

	// já existe grupo pro SN
	if o.snGroup[sn] != 0 {
		return
	}

	all := membership.AllNodeIDs()
	clusterQ := majority(len(all))

	// só define grupo quando já vimos "promessas suficientes" pra ter estabilidade
	// (pode ser clusterQ, pode ser menor; manter clusterQ é ok e simples)
	if len(o.snMembers[sn]) < clusterQ {
		return
	}

	// groupSize = quórum do cluster (mínimo necessário para finalizar Paxos com majority(N))
	groupSize := clusterQ

	// escolhe até groupSize membros dentre quem prometeu; completa se precisar
	members := make([]int32, 0, groupSize)
	for id := range o.snMembers[sn] {
		members = append(members, id)
		if len(members) == groupSize {
			break
		}
	}
	if len(members) < groupSize {
		seenMap := make(map[int32]struct{}, len(members))
		for _, m := range members {
			seenMap[m] = struct{}{}
		}
		for _, id := range all {
			if _, seen := seenMap[id]; seen {
				continue
			}
			members = append(members, id)
			if len(members) == groupSize {
				break
			}
		}
	}
	if len(members) == 0 {
		members = append(members, all...)
	}

	gid := multicast.GroupID(1 + (sn % maxGroupIDSpace))
	o.mc.DefineGroup(gid, members...)
	o.snGroup[sn] = gid
	o.snGSize[sn] = len(members)

	fmt.Printf("[MC][GROUP] sn=%d gid=%d groupSize=%d clusterQ=%d members=%v\n",
		sn, gid, len(members), clusterQ, members)
}

func (o *MultiPaxosMulticastOrderer) onAccept(from, sn int32) {
	o.mu.Lock()
	o.leaderForSN[sn] = from
	o.mu.Unlock()
}

func (o *MultiPaxosMulticastOrderer) onAccepted(_from, sn int32) {
	o.mu.RLock()
	isLeader := (o.leaderForSN[sn] == membership.OwnID)
	o.mu.RUnlock()
	if !isLeader {
		return
	}

	o.mu.Lock()
	o.accCount[sn]++
	o.mu.Unlock()
}

func (o *MultiPaxosMulticastOrderer) sendToGroupOrAll(kind string, pm *pb.ProtocolMessage) {
	o.mu.RLock()
	gid := o.snGroup[pm.Sn]
	o.mu.RUnlock()

	if gid != 0 {
		o.sendGroup(gid, pm)
		fmt.Printf("[MC][SEND] kind=%s sn=%d gid=%d (group)\n", kind, pm.Sn, gid)
		return
	}

	o.broadcast(pm)
	fmt.Printf("[MC][SEND] kind=%s sn=%d gid=0 dst=ALL (fallback)\n", kind, pm.Sn)
}

func (o *MultiPaxosMulticastOrderer) broadcast(pm *pb.ProtocolMessage) {
	for _, dst := range membership.AllNodeIDs() {
		messenger.EnqueueMsg(deepCopyPM(pm), dst)
	}
}

func (o *MultiPaxosMulticastOrderer) sendGroup(gid multicast.GroupID, pm *pb.ProtocolMessage) {
	o.mc.SendGroup(gid, func(dst multicast.ID) *pb.ProtocolMessage {
		return deepCopyPM(pm)
	})
}

func deepCopyPM(pm *pb.ProtocolMessage) *pb.ProtocolMessage {
	// OBS: cópia rasa. Se você suspeitar que mensagens internas são mutáveis/reusadas,
	// troque por proto.Clone(pm).(*pb.ProtocolMessage)
	cp := *pm
	return &cp
}

// Timer: se não fechar maioria do CLUSTER em T ms, reenvia Accept para ALL (somente esse sn).
func (o *MultiPaxosMulticastOrderer) armQuorumTimer(sn int32) {
	o.mu.Lock()
	// se já existe (por alguma corrida), não cria outro
	if o.qTimers[sn] != nil {
		o.mu.Unlock()
		return
	}

	t := time.AfterFunc(time.Duration(defaultQuorumTimeoutMs)*time.Millisecond, func() {
		all := membership.AllNodeIDs()
		clusterQ := majority(len(all))

		o.mu.RLock()
		acc := o.accCount[sn]
		accept := o.lastAccept[sn]
		o.mu.RUnlock()

		if accept == nil {
			return
		}
		// já fechou quórum do CLUSTER → nada a fazer
		if acc >= clusterQ {
			return
		}

		fmt.Printf("[MC][FALLBACK] quorum timeout sn=%d (acc=%d/<%d>) → broadcast Accept\n",
			sn, acc, clusterQ)

		o.broadcast(accept)
	})

	o.qTimers[sn] = t
	o.mu.Unlock()
}

func (o *MultiPaxosMulticastOrderer) cleanupSN(sn int32) {
	o.mu.Lock()
	if t := o.qTimers[sn]; t != nil {
		t.Stop()
		delete(o.qTimers, sn)
	}
	delete(o.accCount, sn)
	delete(o.lastAccept, sn)
	delete(o.leaderForSN, sn)
	// opcional: se quiser liberar também info de grupo/aprendizado por sn:
	// delete(o.snMembers, sn)
	// delete(o.snGroup, sn)
	// delete(o.snGSize, sn)
	o.mu.Unlock()
}

