// Sequencer - Componente dedicado para alocação de GSN (Global Sequence Number).
// Independente do ISS (não usa segmentos, epochs, buckets ou manager).
// Implementa Paxos single-leader para tolerância a falhas.
//
// Arquitetura CSMR:
//   - O sequenciador é o "Multicast Service" para ordenação cross-group
//   - Cada nó que precisa de um GSN envia GSN_REQUEST via gRPC
//   - O líder atribui GSN sequencial e responde GSN_RESPONSE
//   - META_STREAM é publicado no grupo 0 para que todos os nós saibam a ordem global
//
// Separação de responsabilidades:
//   - sequencer.go: GSN allocation, META tracking, atomic delivery ordering
//   - multipaxosmulticastorderer.go: proxy, routing, COMMIT_NOTIFY, group management
//   - multipaxosorderer.go: ISS consensus para grupos de dados (1-4)
package orderer

import (
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hyperledger-labs/mirbft/membership"
	"github.com/hyperledger-labs/mirbft/messenger"
	pb "github.com/hyperledger-labs/mirbft/protobufs"
	logger "github.com/rs/zerolog/log"
)

func makeGlobalRequestID(nodeID int32, localCounter uint32) uint64 {
	return uint64(nodeID)<<32 | uint64(localCounter)
}

// Sequencer gerencia alocação de GSN de forma independente do ISS.
// Single-leader: o nó com menor ID no grupo 0 é o líder.
// Processa GSN_REQUESTs diretamente sem passar por buckets/batches.
type Sequencer struct {
	// GSN counter (monotonicamente crescente)
	nextGSN uint64
	gsnMu   sync.Mutex

	// Pending GSN requests (aguardando resposta do líder)
	requestsPending map[uint64]chan uint64
	reqMu           sync.Mutex
	reqCounter      uint32

	// META stream: metadata de cross-group requests
	// Maps GSN -> lista de grupos tocados
	metadata   map[uint64][]uint32
	metaMu     sync.RWMutex
	metaCounter uint32

	// Atomic delivery ordering per group
	lastDeliveredGSN map[uint32]uint64
	deliveryMu       sync.RWMutex

	// Per-group queue of GSNs that touch that group (sorted, for O(1) next lookup)
	// Populated as META arrives. Enables skipping irrelevant GSNs without linear scan.
	groupGSNQueue map[uint32][]uint64
	groupQueueMu  sync.RWMutex

	// Pending commits waiting for META before delivery
	pendingCommits map[uint32]map[uint64]*PendingCommit
	bufferMu       sync.RWMutex

	// Published META dedup
	publishedMeta map[uint64]bool
	publishedMu   sync.RWMutex

	// Group membership
	members []int32
	leader  int32

	// Leader election state (Raft-inspired, log-free: liveness only).
	// Guards term, votedFor, status, votes, leader, electionTimer, lastHeartbeatAt.
	electMu         sync.RWMutex
	term            int32
	votedFor        int32
	status          string
	votes           map[int32]bool
	electionTimer   *time.Timer
	lastHeartbeatAt time.Time

	// Running state
	started bool
	stopCh  chan struct{}
}

// NewSequencer cria um novo sequenciador para o grupo 0.
func NewSequencer(members []int32) *Sequencer {
	leader := int32(-1)
	if len(members) > 0 {
		leader = members[0]
		for _, m := range members {
			if m < leader {
				leader = m
			}
		}
	}

	// Termo 0 é o bootstrap determinístico: todo nó calcula o mesmo `leader`
	// (menor ID) a partir da mesma lista estática `members`, sem precisar de
	// eleição real. Cada nó "pré-vota" nesse líder no termo 0 para não
	// disparar uma eleição espúria antes do primeiro heartbeat chegar.
	// Nota: a variável local `leader` (int32, ID do líder) sombreia a
	// constante de pacote `leader` (string, definida em raftinstance.go),
	// então usamos o literal "leader" aqui em vez do identificador.
	initialStatus := follower
	if membership.OwnID == leader {
		initialStatus = "leader"
	}

	s := &Sequencer{
		nextGSN:          1,
		requestsPending:  make(map[uint64]chan uint64),
		metadata:         make(map[uint64][]uint32),
		lastDeliveredGSN: make(map[uint32]uint64),
		groupGSNQueue:    make(map[uint32][]uint64),
		pendingCommits:   make(map[uint32]map[uint64]*PendingCommit),
		publishedMeta:    make(map[uint64]bool),
		members:          members,
		leader:           leader,
		term:             0,
		votedFor:         leader,
		status:           initialStatus,
		votes:            make(map[int32]bool),
		stopCh:           make(chan struct{}),
	}

	// GSN always starts at 1 — each experiment is independent, no stale state
	fmt.Printf("[SEQUENCER] Init: members=%v leader=%d ownID=%d\n", members, leader, membership.OwnID)
	logger.Info().
		Int32("leader", leader).
		Int("numMembers", len(members)).
		Msg("Sequencer initialized")

	return s
}

// Start inicia o sequenciador. Não depende do manager/ISS.
// Chamado após messenger.Connect()/discovery.SyncPeer() (mesmo ponto em que
// MultiPaxosMulticastOrderer.Start() já roda), para não armar o timer de
// eleição antes das conexões entre nós existirem (evitaria eleições
// espúrias de cold-start).
func (s *Sequencer) Start() {
	if s.started {
		return
	}
	s.started = true
	fmt.Printf("[SEQUENCER] Started (leader=%d, ownID=%d, isLeader=%v, nextGSN=%d)\n",
		s.currentLeader(), membership.OwnID, s.IsLeader(), s.nextGSN)

	if s.IsLeader() {
		// Termo 0: líder de bootstrap já começa emitindo heartbeats.
		go s.leaderHeartbeatLoop(0)
	}
	s.startElectionTimer()
}

// IsLeader retorna true se este nó é o líder do sequenciador.
func (s *Sequencer) IsLeader() bool {
	return s.currentLeader() == membership.OwnID
}

// GetNextGSN solicita um novo GSN ao líder do sequenciador.
// Se este nó é o líder, atribui localmente.
// Se não, envia GSN_REQUEST ao líder e aguarda resposta, tentando de novo
// (até maxGSNRequestAttempts vezes) se o líder trocar ou não responder --
// isso é o que permite ao sistema se recuperar de um failover do líder em
// vez de desistir para sempre depois de um único timeout.
func (s *Sequencer) GetNextGSN() uint64 {
	if s.IsLeader() {
		gsn := s.allocateGSN()
		fmt.Printf("[SEQUENCER][GetNextGSN] allocated gsn=%d (local leader)\n", gsn)
		return gsn
	}

	const maxGSNRequestAttempts = 3
	const perAttemptTimeout = 2 * time.Second

	// Mesmo reqID reusado entre tentativas contra o líder-alvo atual: se uma
	// resposta replicada atrasada do líder antigo chegar (HandleGSNRequest
	// replica GSN_RESPONSE para todo s.members "para tolerância a falhas"),
	// o dedup por reqID em HandleGSNResponse ainda consegue entregá-la aqui,
	// em vez de gerar um segundo GSN para a mesma operação lógica.
	counter := atomic.AddUint32(&s.reqCounter, 1)
	reqID := makeGlobalRequestID(membership.OwnID, counter)
	respChan := make(chan uint64, 1)

	s.reqMu.Lock()
	s.requestsPending[reqID] = respChan
	s.reqMu.Unlock()

	target := s.currentLeader()

	for attempt := 0; attempt < maxGSNRequestAttempts; attempt++ {
		if s.IsLeader() {
			// Este nó virou líder enquanto esperávamos -- atende localmente.
			s.reqMu.Lock()
			delete(s.requestsPending, reqID)
			s.reqMu.Unlock()
			gsn := s.allocateGSN()
			fmt.Printf("[SEQUENCER][GetNextGSN] allocated gsn=%d (became local leader mid-request)\n", gsn)
			return gsn
		}

		payload := fmt.Sprintf("%s%d:%d", SYSTEM_GSN_REQUEST, reqID, membership.OwnID)
		messenger.EnqueueMsg(&pb.ProtocolMessage{
			SenderId: membership.OwnID, Sn: -1,
			Msg: &pb.ProtocolMessage_GsnReqForward{GsnReqForward: &pb.GSNReqForward{
				Req: &pb.ClientRequest{
					RequestId: &pb.RequestID{ClientId: membership.OwnID, ClientSn: int32(counter)},
					Payload:   []byte(payload),
					GroupId:   0,
				},
			}},
		}, target)

		select {
		case gsn := <-respChan:
			s.reqMu.Lock()
			delete(s.requestsPending, reqID)
			s.reqMu.Unlock()
			return gsn
		case <-time.After(perAttemptTimeout):
			newTarget := s.currentLeader()
			if newTarget != target {
				fmt.Printf("[SEQUENCER] GSN request timeout (reqID=%d), retrying against new leader=%d\n", reqID, newTarget)
				target = newTarget
			} else {
				fmt.Printf("[SEQUENCER] GSN request timeout (reqID=%d), leader still believed to be %d, retrying (attempt %d/%d)\n", reqID, target, attempt+1, maxGSNRequestAttempts)
			}
		}
	}

	s.reqMu.Lock()
	delete(s.requestsPending, reqID)
	s.reqMu.Unlock()
	fmt.Printf("[SEQUENCER] GSN request exhausted retries (reqID=%d)\n", reqID)
	return 0
}

// allocateGSN atribui um GSN localmente (só o líder chama).
func (s *Sequencer) allocateGSN() uint64 {
	s.gsnMu.Lock()
	gsn := s.nextGSN
	s.nextGSN++
	s.gsnMu.Unlock()
	return gsn
}

// HandleGSNRequest processa um GSN_REQUEST recebido (só o líder processa).
// Atribui GSN e envia GSN_RESPONSE de volta ao requester.
func (s *Sequencer) HandleGSNRequest(payload string, senderID int32) {
	var reqID uint64
	var requester int32
	if n, _ := fmt.Sscanf(payload, "SYSTEM:GSN_REQUEST:%d:%d", &reqID, &requester); n < 2 {
		fmt.Printf("[SEQUENCER] PARSE-FAIL: %.60s\n", payload)
		return
	}

	if !s.IsLeader() {
		// Forward ao líder atual (não deveria acontecer, mas por segurança)
		leaderID := s.currentLeader()
		fmt.Printf("[SEQUENCER] Not leader, forwarding GSN_REQUEST to %d\n", leaderID)
		messenger.EnqueueMsg(&pb.ProtocolMessage{
			SenderId: membership.OwnID, Sn: -1,
			Msg: &pb.ProtocolMessage_GsnReqForward{GsnReqForward: &pb.GSNReqForward{
				Req: &pb.ClientRequest{
					RequestId: &pb.RequestID{ClientId: requester, ClientSn: 0},
					Payload:   []byte(payload),
					GroupId:   0,
				},
			}},
		}, leaderID)
		return
	}

	// Atribui GSN
	gsn := s.allocateGSN()
	fmt.Printf("[SEQUENCER] GSN-ASSIGN gsn=%d reqID=%d requester=%d\n", gsn, reqID, requester)

	// Responde ao requester
	if requester == membership.OwnID {
		// Local: envia direto para o channel
		s.reqMu.Lock()
		if ch, exists := s.requestsPending[reqID]; exists {
			select {
			case ch <- gsn:
			default:
			}
			delete(s.requestsPending, reqID)
		}
		s.reqMu.Unlock()
	} else {
		// Remoto: envia GSN_RESPONSE
		responsePayload := fmt.Sprintf("SYSTEM:GSN_RESPONSE:%d:%d", reqID, gsn)
		messenger.EnqueueMsg(&pb.ProtocolMessage{
			SenderId: membership.OwnID, Sn: -1,
			Msg: &pb.ProtocolMessage_GsnReqForward{GsnReqForward: &pb.GSNReqForward{
				Req: &pb.ClientRequest{
					RequestId: &pb.RequestID{ClientId: requester, ClientSn: 0},
					Payload:   []byte(responsePayload),
				},
			}},
		}, requester)
	}

	// Replica decisão para os outros membros (tolerância a falhas)
	for _, nodeID := range s.members {
		if nodeID == membership.OwnID || nodeID == requester {
			continue
		}
		replicaPayload := fmt.Sprintf("SYSTEM:GSN_RESPONSE:%d:%d", reqID, gsn)
		messenger.EnqueueMsg(&pb.ProtocolMessage{
			SenderId: membership.OwnID, Sn: -1,
			Msg: &pb.ProtocolMessage_GsnReqForward{GsnReqForward: &pb.GSNReqForward{
				Req: &pb.ClientRequest{
					RequestId: &pb.RequestID{ClientId: requester, ClientSn: 0},
					Payload:   []byte(replicaPayload),
				},
			}},
		}, nodeID)
	}
}

// HandleGSNResponse processa um GSN_RESPONSE recebido.
func (s *Sequencer) HandleGSNResponse(payload string) {
	var reqID, gsn uint64
	if n, _ := fmt.Sscanf(payload, "SYSTEM:GSN_RESPONSE:%d:%d", &reqID, &gsn); n < 2 {
		return
	}

	s.reqMu.Lock()
	if ch, exists := s.requestsPending[reqID]; exists {
		select {
		case ch <- gsn:
		default:
		}
		delete(s.requestsPending, reqID)
	}
	s.reqMu.Unlock()
}

// PublishMETA publica metadata de um cross-group request para todos os membros do grupo 0.
func (s *Sequencer) PublishMETA(gsn uint64, touchedGroups []uint32) {
	fmt.Printf("[SEQUENCER][PublishMETA] gsn=%d touchedGroups=%v\n", gsn, touchedGroups)
	s.publishedMu.Lock()
	if s.publishedMeta[gsn] {
		s.publishedMu.Unlock()
		fmt.Printf("[SEQUENCER][PublishMETA] gsn=%d ALREADY PUBLISHED (dedup)\n", gsn)
		return
	}
	s.publishedMeta[gsn] = true
	s.publishedMu.Unlock()

	if len(touchedGroups) == 0 {
		return
	}

	// Registra localmente
	s.RegisterMetadata(gsn, touchedGroups)

	// Broadcast META para todos os nós (com prioridade para evitar buffer overflow)
	metaPayload := fmt.Sprintf("%s%d", SYSTEM_META_STREAM, gsn)
	for _, nodeID := range membership.AllNodeIDs() {
		if nodeID == membership.OwnID {
			continue
		}
		messenger.EnqueuePriorityMsg(&pb.ProtocolMessage{
			SenderId: membership.OwnID, Sn: -1,
			Msg: &pb.ProtocolMessage_GsnReqForward{GsnReqForward: &pb.GSNReqForward{
				Req: &pb.ClientRequest{
					RequestId:    &pb.RequestID{ClientId: membership.OwnID, ClientSn: int32(atomic.AddUint32(&s.metaCounter, 1))},
					Payload:      []byte(metaPayload),
					GroupId:      0,
					TouchedGroups: touchedGroups,
					GSN:          gsn,
				},
			}},
		}, nodeID)
	}
}

// HandleMETAStream processa uma mensagem META_STREAM recebida.
func (s *Sequencer) HandleMETAStream(payload string, touchedGroups []uint32, gsn uint64) {
	if gsn == 0 {
		// Parse GSN from payload
		fmt.Sscanf(payload, "SYSTEM:META_STREAM:%d", &gsn)
	}
	fmt.Printf("[SEQUENCER][META-RECV] gsn=%d touchedGroups=%v payload=%.40s\n", gsn, touchedGroups, payload)
	if gsn > 0 && len(touchedGroups) > 0 {
		s.RegisterMetadata(gsn, touchedGroups)
	} else {
		fmt.Printf("[SEQUENCER][META-RECV] IGNORED gsn=%d len(touched)=%d\n", gsn, len(touchedGroups))
	}
}

// RegisterMetadata registra metadata de um GSN (quais grupos são tocados).
func (s *Sequencer) RegisterMetadata(gsn uint64, touchedGroups []uint32) {
	s.metaMu.Lock()
	if _, exists := s.metadata[gsn]; exists {
		s.metaMu.Unlock()
		return
	}
	s.metadata[gsn] = make([]uint32, len(touchedGroups))
	copy(s.metadata[gsn], touchedGroups)
	s.metaMu.Unlock()

	// Append to per-group GSN queue (already sorted since GSNs arrive in order)
	s.groupQueueMu.Lock()
	for _, gid := range touchedGroups {
		s.groupGSNQueue[gid] = append(s.groupGSNQueue[gid], gsn)
	}
	s.groupQueueMu.Unlock()

	// Tenta drenar buffers pendentes
	for _, gid := range touchedGroups {
		s.drainBuffer(gid)
	}
}

// ADeliver verifica se um GSN pode ser entregue para um grupo (atomic global order).
func (s *Sequencer) ADeliver(gsn uint64, groupID uint32) bool {
	s.deliveryMu.Lock()
	defer s.deliveryMu.Unlock()
	return s.aDeliverInternal(gsn, groupID)
}

// aDeliverUnlocked is like ADeliver but assumes deliveryMu is NOT held.
// Used by drainBuffer which holds bufferMu but not deliveryMu.
func (s *Sequencer) aDeliverUnlocked(gsn uint64, groupID uint32) bool {
	s.deliveryMu.Lock()
	defer s.deliveryMu.Unlock()
	return s.aDeliverInternal(gsn, groupID)
}

func (s *Sequencer) aDeliverInternal(gsn uint64, groupID uint32) bool {
	lastDelivered := s.lastDeliveredGSN[groupID]
	if gsn <= lastDelivered {
		return true
	}

	// Use per-group GSN queue for O(1) lookup of next required GSN
	// instead of scanning all GSNs linearly.
	s.groupQueueMu.RLock()
	queue := s.groupGSNQueue[groupID]
	s.groupQueueMu.RUnlock()

	// Find the next GSN in this group's queue that hasn't been delivered yet
	nextRequired := uint64(0)
	for _, qgsn := range queue {
		if qgsn > lastDelivered {
			nextRequired = qgsn
			break
		}
	}

	// If no next required GSN found in queue, check if META exists for our GSN
	if nextRequired == 0 {
		s.metaMu.RLock()
		_, metaExists := s.metadata[gsn]
		touches := metaExists && s.touchesGroup(gsn, groupID)
		s.metaMu.RUnlock()
		if !metaExists {
			fmt.Printf("[ADeliver] BLOCKED gsn=%d group=%d: missing META for own gsn\n", gsn, groupID)
			return false
		}
		if !touches {
			return true
		}
		s.lastDeliveredGSN[groupID] = gsn
		fmt.Printf("[ADeliver] DELIVERED gsn=%d group=%d (lastDelivered updated)\n", gsn, groupID)
		return true
	}

	// The next required GSN for this group must be <= our GSN
	if nextRequired < gsn {
		// There's a prior GSN that touches this group and hasn't been delivered
		fmt.Printf("[ADeliver] BLOCKED gsn=%d group=%d: prior gsn=%d not yet delivered (lastDelivered=%d)\n", gsn, groupID, nextRequired, lastDelivered)
		return false
	}

	// nextRequired == gsn: this is the next one to deliver
	if nextRequired == gsn {
		s.metaMu.RLock()
		_, metaExists := s.metadata[gsn]
		s.metaMu.RUnlock()
		if !metaExists {
			fmt.Printf("[ADeliver] BLOCKED gsn=%d group=%d: missing META for own gsn\n", gsn, groupID)
			return false
		}
		s.lastDeliveredGSN[groupID] = gsn
		fmt.Printf("[ADeliver] DELIVERED gsn=%d group=%d (lastDelivered updated)\n", gsn, groupID)
		return true
	}

	// nextRequired > gsn: our GSN doesn't touch this group (already past it in queue)
	return true
}

// BufferCommit armazena um commit pendente aguardando META para delivery.
func (s *Sequencer) BufferCommit(gsn uint64, groupID uint32, batch *pb.Batch, announce func(int32, *pb.Batch, []byte), sn int32, digest []byte) {
	s.bufferMu.Lock()
	defer s.bufferMu.Unlock()
	if s.pendingCommits[groupID] == nil {
		s.pendingCommits[groupID] = make(map[uint64]*PendingCommit)
	}
	s.pendingCommits[groupID][gsn] = &PendingCommit{
		gsn: gsn, groupID: groupID, batch: batch,
		announce: announce, sn: sn, digest: digest,
	}
}

// drainBuffer tenta entregar commits pendentes que agora têm META disponível.
func (s *Sequencer) drainBuffer(groupID uint32) {
	s.bufferMu.Lock()
	defer s.bufferMu.Unlock()
	if s.pendingCommits[groupID] == nil {
		return
	}
	for {
		s.deliveryMu.RLock()
		lastDelivered := s.lastDeliveredGSN[groupID]
		s.deliveryMu.RUnlock()

		// Use per-group queue to find next GSN that touches this group
		s.groupQueueMu.RLock()
		queue := s.groupGSNQueue[groupID]
		s.groupQueueMu.RUnlock()

		nextCandidate := uint64(0)
		for _, qgsn := range queue {
			if qgsn > lastDelivered {
				nextCandidate = qgsn
				break
			}
		}
		if nextCandidate == 0 {
			return
		}

		pending, exists := s.pendingCommits[groupID][nextCandidate]
		if !exists {
			return
		}

		// Advance lastDeliveredGSN for this GSN
		s.deliveryMu.Lock()
		s.lastDeliveredGSN[groupID] = nextCandidate
		s.deliveryMu.Unlock()
		delete(s.pendingCommits[groupID], nextCandidate)

		// Check if the batch has more cross-op GSNs that also need delivery
		allDelivered := true
		if pending.batch != nil {
			for _, req := range pending.batch.Requests {
				if len(req.TouchedGroups) > 1 && req.GSN > nextCandidate {
					if s.aDeliverUnlocked(req.GSN, groupID) {
						continue
					}
					s.pendingCommits[groupID][req.GSN] = pending
					allDelivered = false
					break
				}
			}
		}

		if allDelivered {
			fmt.Printf("[ADeliver] DRAIN-DELIVERED gsn=%d group=%d (batch)\n", nextCandidate, groupID)
			pending.announce(pending.sn, pending.batch, pending.digest)
		}
	}
}

// touchesGroup verifica se um GSN toca um grupo específico.
func (s *Sequencer) touchesGroup(gsn uint64, groupID uint32) bool {
	touched, exists := s.metadata[gsn]
	if !exists {
		return false
	}
	for _, g := range touched {
		if g == groupID {
			return true
		}
	}
	return false
}


