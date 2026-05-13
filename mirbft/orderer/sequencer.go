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
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hyperledger-labs/mirbft/membership"
	"github.com/hyperledger-labs/mirbft/messenger"
	pb "github.com/hyperledger-labs/mirbft/protobufs"
	logger "github.com/rs/zerolog/log"
)

const sequencerStateFile = "/tmp/iss-Bruno/next_gsn.state"

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

	// Pending commits waiting for META before delivery
	pendingCommits map[uint32]map[uint64]*PendingCommit
	bufferMu       sync.RWMutex

	// Published META dedup
	publishedMeta map[uint64]bool
	publishedMu   sync.RWMutex

	// Group membership
	members []int32
	leader  int32

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

	s := &Sequencer{
		nextGSN:          1,
		requestsPending:  make(map[uint64]chan uint64),
		metadata:         make(map[uint64][]uint32),
		lastDeliveredGSN: make(map[uint32]uint64),
		pendingCommits:   make(map[uint32]map[uint64]*PendingCommit),
		publishedMeta:    make(map[uint64]bool),
		members:          members,
		leader:           leader,
		stopCh:           make(chan struct{}),
	}

	s.loadState()

	fmt.Printf("[SEQUENCER] Init: members=%v leader=%d ownID=%d\n", members, leader, membership.OwnID)
	logger.Info().
		Int32("leader", leader).
		Int("numMembers", len(members)).
		Msg("Sequencer initialized")

	return s
}

// Start inicia o sequenciador. Não depende do manager/ISS.
func (s *Sequencer) Start() {
	if s.started {
		return
	}
	s.started = true
	fmt.Printf("[SEQUENCER] Started (leader=%d, ownID=%d, isLeader=%v)\n",
		s.leader, membership.OwnID, s.IsLeader())
}

// IsLeader retorna true se este nó é o líder do sequenciador.
func (s *Sequencer) IsLeader() bool {
	return s.leader == membership.OwnID
}

// GetNextGSN solicita um novo GSN ao líder do sequenciador.
// Se este nó é o líder, atribui localmente.
// Se não, envia GSN_REQUEST ao líder e aguarda resposta.
func (s *Sequencer) GetNextGSN() uint64 {
	if s.IsLeader() {
		gsn := s.allocateGSN()
		fmt.Printf("[SEQUENCER][GetNextGSN] allocated gsn=%d (local leader)\n", gsn)
		return gsn
	}

	// Não é líder: envia request ao líder
	counter := atomic.AddUint32(&s.reqCounter, 1)
	reqID := makeGlobalRequestID(membership.OwnID, counter)
	respChan := make(chan uint64, 1)

	s.reqMu.Lock()
	s.requestsPending[reqID] = respChan
	s.reqMu.Unlock()

	// Envia GSN_REQUEST ao líder
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
	}, s.leader)

	select {
	case gsn := <-respChan:
		s.reqMu.Lock()
		delete(s.requestsPending, reqID)
		s.reqMu.Unlock()
		return gsn
	case <-time.After(5 * time.Second):
		s.reqMu.Lock()
		delete(s.requestsPending, reqID)
		s.reqMu.Unlock()
		fmt.Printf("[SEQUENCER] GSN request timeout (reqID=%d)\n", reqID)
		return 0
	}
}

// allocateGSN atribui um GSN localmente (só o líder chama).
func (s *Sequencer) allocateGSN() uint64 {
	s.gsnMu.Lock()
	gsn := s.nextGSN
	s.nextGSN++
	s.persistState()
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
		// Forward ao líder (não deveria acontecer, mas por segurança)
		fmt.Printf("[SEQUENCER] Not leader, forwarding GSN_REQUEST to %d\n", s.leader)
		messenger.EnqueueMsg(&pb.ProtocolMessage{
			SenderId: membership.OwnID, Sn: -1,
			Msg: &pb.ProtocolMessage_GsnReqForward{GsnReqForward: &pb.GSNReqForward{
				Req: &pb.ClientRequest{
					RequestId: &pb.RequestID{ClientId: requester, ClientSn: 0},
					Payload:   []byte(payload),
					GroupId:   0,
				},
			}},
		}, s.leader)
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

	// Broadcast META para todos os nós (não só grupo 0)
	metaPayload := fmt.Sprintf("%s%d", SYSTEM_META_STREAM, gsn)
	for _, nodeID := range membership.AllNodeIDs() {
		if nodeID == membership.OwnID {
			continue
		}
		messenger.EnqueueMsg(&pb.ProtocolMessage{
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
	if gsn > 0 && len(touchedGroups) > 0 {
		s.RegisterMetadata(gsn, touchedGroups)
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

	// Tenta drenar buffers pendentes
	for _, gid := range touchedGroups {
		s.drainBuffer(gid)
	}
}

// ADeliver verifica se um GSN pode ser entregue para um grupo (atomic global order).
func (s *Sequencer) ADeliver(gsn uint64, groupID uint32) bool {
	s.deliveryMu.Lock()
	defer s.deliveryMu.Unlock()

	lastDelivered := s.lastDeliveredGSN[groupID]
	if gsn <= lastDelivered {
		return true
	}

	// Verifica se todos os GSNs anteriores que tocam este grupo já foram entregues
	nextCandidate := lastDelivered + 1
	for nextCandidate < gsn {
		s.metaMu.RLock()
		_, metaExists := s.metadata[nextCandidate]
		touches := metaExists && s.touchesGroup(nextCandidate, groupID)
		s.metaMu.RUnlock()
		if !metaExists {
			return false
		}
		if touches {
			return false
		}
		nextCandidate++
	}

	s.metaMu.RLock()
	_, metaExists := s.metadata[gsn]
	touches := metaExists && s.touchesGroup(gsn, groupID)
	s.metaMu.RUnlock()
	if !metaExists {
		return false
	}
	if !touches {
		return true
	}
	s.lastDeliveredGSN[groupID] = gsn
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

		nextCandidate := lastDelivered + 1
		for {
			s.metaMu.RLock()
			_, metaExists := s.metadata[nextCandidate]
			touches := metaExists && s.touchesGroup(nextCandidate, groupID)
			s.metaMu.RUnlock()
			if !metaExists {
				return
			}
			if touches {
				break
			}
			nextCandidate++
		}

		pending, exists := s.pendingCommits[groupID][nextCandidate]
		if !exists {
			return
		}
		pending.announce(pending.sn, pending.batch, pending.digest)
		s.deliveryMu.Lock()
		s.lastDeliveredGSN[groupID] = nextCandidate
		s.deliveryMu.Unlock()
		delete(s.pendingCommits[groupID], nextCandidate)
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

// loadState carrega o próximo GSN do disco.
func (s *Sequencer) loadState() {
	b, err := os.ReadFile(sequencerStateFile)
	if err == nil {
		if v, err2 := strconv.ParseUint(strings.TrimSpace(string(b)), 10, 64); err2 == nil && v > 0 {
			s.nextGSN = v
		}
	}
}

// persistState salva o próximo GSN no disco.
func (s *Sequencer) persistState() {
	_ = os.WriteFile(sequencerStateFile, []byte(fmt.Sprintf("%d\n", s.nextGSN)), 0644)
}
