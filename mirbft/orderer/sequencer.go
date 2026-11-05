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
	"sort"
	"strings"
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
// Single-leader: no bootstrap (termo 0), o nó com menor ID no grupo 0 assume a liderança;
// depois disso, o líder pode mudar por eleição real (ver sequencer_election.go) se o timer de
// algum nó expirar sem receber heartbeat do líder atual.
// Processa GSN_REQUESTs diretamente sem passar por buckets/batches.
type Sequencer struct {
	// GSN counter (monotonicamente crescente)
	nextGSN uint64
	gsnMu   sync.Mutex
	gsnCond *sync.Cond // sinaliza reserva confirmada (allocateGSN espera nela)

	// Reserva de blocos de GSN (recuperação sem palpite): o líder só numera até confirmedUpTo,
	// uma marca que a MAIORIA do grupo 0 confirmou (SEQ_RESERVE/SEQ_RESERVE_ACK). Todo nó guarda
	// acceptedUpTo, a maior marca que já confirmou; na eleição o voto leva esse valor, e como
	// toda reserva confirmada foi vista por uma maioria, o novo líder (eleito por maioria) sabe
	// com certeza que nenhum GSN acima da maior marca recebida foi usado.
	confirmedUpTo  uint64         // líder: pode numerar até aqui
	acceptedUpTo   uint64         // todo nó: maior reserva que confirmou
	reservePending uint64         // líder: marca pedida, aguardando maioria (0 = nenhuma)
	reserveTerm    int32          // termo da reserva pendente
	reserveAcks    map[int32]bool // quem confirmou a reserva pendente
	reserveSentAt  time.Time      // para reenviar se as confirmações se perderem

	// Eleição: maiores valores informados pelos votos recebidos neste termo
	voteMaxAccepted uint64
	voteMaxMeta     uint64
	voteHoles       [][2]uint64 // GSNs sem META informados pelos votantes (faixas)

	// Pending GSN requests (aguardando resposta do líder)
	requestsPending map[uint64]chan uint64
	reqMu           sync.Mutex
	reqCounter      uint32

	// META stream: metadata de cross-group requests
	// Maps GSN -> lista de grupos tocados
	metadata   map[uint64][]uint32
	metaMu     sync.RWMutex
	metaCounter uint32

	// Published META dedup
	publishedMeta map[uint64]bool
	publishedMu   sync.RWMutex

	// Group membership
	members []int32
	leader  int32

	// Leader election state (Raft-inspired, log-free: liveness only).
	// Guards term, votedFor, status, votes, leader, electionTimer.
	electMu         sync.RWMutex
	term            int32
	votedFor        int32
	status          string
	votes           map[int32]bool
	electionTimer   *time.Timer
	// Pré-votação: quando ouviu o líder pela última vez, e os pré-votos do termo pretendido.
	lastLeaderContact time.Time
	preVoteTerm       int32
	preVotes          map[int32]bool

	// Chamado a cada META novo (entrega global por nó, ver globaldeliver.go)
	onMeta func(gsn uint64, touchedGroups []uint32)
	// Chamado quando um novo líder anuncia uma faixa de GSNs pulada [from, to]
	onSkip func(from, to uint64)
	// Faixas de GSN puladas já conhecidas (publicadas por este nó ou recebidas). O líder as
	// repete em todo heartbeat: um META_SKIP avulso pode se perder (ex.: enviado logo após a
	// eleição, antes de a conexão com algum nó estar pronta).
	skipMu     sync.Mutex
	skipRanges map[[2]uint64]bool

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

	s.gsnCond = sync.NewCond(&s.gsnMu)
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
		// Termo 0: líder de bootstrap já começa emitindo heartbeats e reserva o 1º bloco.
		go s.leaderHeartbeatLoop(0)
		go func() {
			s.gsnMu.Lock()
			s.requestReservationLocked()
			s.gsnMu.Unlock()
		}()
	}
	// Carência na largada: os primeiros heartbeats se perdem enquanto as conexões ainda estão
	// subindo; com o timer normal (0,5-1 s) um seguidor disparava eleição sem falha nenhuma.
	s.electMu.Lock()
	s.startElectionTimerLockedFor(startupElectionGrace())
	s.electMu.Unlock()
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
			if gsn == 0 {
				continue // líder sem reserva confirmada: tenta de novo
			}
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

// gsnBlockSize é o tamanho de cada reserva de GSN confirmada pela maioria.
const gsnBlockSize = 100

// allocateGSN atribui um GSN localmente (só o líder chama). Só numera dentro da reserva
// confirmada pela maioria; se ela acabou, pede a próxima e espera (até 3 s). Devolve 0 se não
// conseguiu (sem maioria) -- quem chamou trata como falha e o pedido é refeito por timeout.
func (s *Sequencer) allocateGSN() uint64 {
	deadline := time.Now().Add(3 * time.Second)
	s.gsnMu.Lock()
	defer s.gsnMu.Unlock()
	for s.nextGSN > s.confirmedUpTo {
		s.requestReservationLocked()
		if time.Now().After(deadline) {
			fmt.Printf("[SEQUENCER][RESERVE] sem reserva confirmada (next=%d confirmed=%d)\n", s.nextGSN, s.confirmedUpTo)
			return 0
		}
		t := time.AfterFunc(200*time.Millisecond, func() {
			s.gsnMu.Lock()
			s.gsnCond.Broadcast()
			s.gsnMu.Unlock()
		})
		s.gsnCond.Wait()
		t.Stop()
	}
	gsn := s.nextGSN
	s.nextGSN++
	// Pede o próximo bloco antes de acabar, para a rodada não ficar no caminho das mensagens.
	if s.confirmedUpTo-gsn < gsnBlockSize/2 {
		s.requestReservationLocked()
	}
	return gsn
}

// requestReservationLocked pede à maioria a próxima marca (gsnMu travado). Não faz nada se não
// é líder ou se já há um pedido recente pendente; reenvia se as confirmações demoraram.
func (s *Sequencer) requestReservationLocked() {
	s.electMu.RLock()
	isLeader := s.status == "leader" && s.leader == membership.OwnID
	term := s.term
	s.electMu.RUnlock()
	if !isLeader {
		return
	}
	if s.reservePending > 0 && s.reserveTerm == term && time.Since(s.reserveSentAt) < time.Second {
		return
	}
	if s.reservePending == 0 || s.reserveTerm != term {
		base := s.confirmedUpTo
		if s.nextGSN-1 > base {
			base = s.nextGSN - 1
		}
		s.reservePending = base + gsnBlockSize
		s.reserveTerm = term
		s.reserveAcks = map[int32]bool{membership.OwnID: true}
		if s.reservePending > s.acceptedUpTo {
			s.acceptedUpTo = s.reservePending
		}
	}
	s.reserveSentAt = time.Now()
	upto := s.reservePending
	fmt.Printf("[SEQUENCER][RESERVE] term=%d pedindo reserva ate %d\n", term, upto)
	if len(s.reserveAcks) > len(s.members)/2 { // grupo 0 de 1 nó
		s.confirmReservationLocked()
		return
	}
	go s.broadcastToMembers(fmt.Sprintf("%s%d:%d", SYSTEM_SEQ_RESERVE, term, upto))
}

func (s *Sequencer) confirmReservationLocked() {
	s.confirmedUpTo = s.reservePending
	fmt.Printf("[SEQUENCER][RESERVE] term=%d reserva CONFIRMADA ate %d (%d/%d)\n",
		s.reserveTerm, s.confirmedUpTo, len(s.reserveAcks), len(s.members))
	s.reservePending = 0
	s.gsnCond.Broadcast()
}

// HandleReserve: um nó confirma a reserva do líder (se o termo não for antigo) e guarda a marca.
func (s *Sequencer) HandleReserve(payload string, senderID int32) {
	var term int64
	var upto uint64
	if n, _ := fmt.Sscanf(payload, SYSTEM_SEQ_RESERVE+"%d:%d", &term, &upto); n < 2 {
		return
	}
	s.electMu.Lock()
	stale := int32(term) < s.term
	if !stale && senderID == s.leader && s.status != "leader" {
		// Reserva do líder também é sinal de vida (o heartbeat pode atrasar sob carga).
		s.lastLeaderContact = time.Now()
		s.startElectionTimerLocked()
	}
	s.electMu.Unlock()
	if stale {
		return
	}
	s.gsnMu.Lock()
	if upto > s.acceptedUpTo {
		s.acceptedUpTo = upto
	}
	s.gsnMu.Unlock()
	messenger.EnqueuePriorityMsg(&pb.ProtocolMessage{
		SenderId: membership.OwnID, Sn: -1,
		Msg: &pb.ProtocolMessage_GsnReqForward{GsnReqForward: &pb.GSNReqForward{
			Req: &pb.ClientRequest{Payload: []byte(fmt.Sprintf("%s%d:%d", SYSTEM_SEQ_RESERVE_ACK, term, upto))},
		}},
	}, senderID)
}

// HandleReserveAck: o líder conta confirmações; com a maioria, libera a numeração até a marca.
func (s *Sequencer) HandleReserveAck(payload string, senderID int32) {
	var term int64
	var upto uint64
	if n, _ := fmt.Sscanf(payload, SYSTEM_SEQ_RESERVE_ACK+"%d:%d", &term, &upto); n < 2 {
		return
	}
	s.gsnMu.Lock()
	defer s.gsnMu.Unlock()
	if s.reservePending != upto || s.reserveTerm != int32(term) {
		return
	}
	s.reserveAcks[senderID] = true
	if len(s.reserveAcks) > len(s.members)/2 {
		s.confirmReservationLocked()
	}
}

// maxMetaSeen devolve o maior GSN com META conhecido neste nó.
func (s *Sequencer) maxMetaSeen() uint64 {
	s.metaMu.RLock()
	defer s.metaMu.RUnlock()
	var m uint64
	for gsn := range s.metadata {
		if gsn > m {
			m = gsn
		}
	}
	return m
}

// HandleGSNRequest processa um GSN_REQUEST recebido. Se este nó não for o líder atual, apenas
// encaminha a requisição pro líder; só o líder de fato atribui o GSN e responde ao requester.
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

	// Atribui GSN (dentro da reserva confirmada pela maioria)
	gsn := s.allocateGSN()
	if gsn == 0 {
		return // sem reserva confirmada: o requester tenta de novo por timeout
	}
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
	s.sendMETA(gsn, touchedGroups)
}

// sendMETA envia o META de um GSN a todos os outros nós (com prioridade para evitar buffer overflow).
func (s *Sequencer) sendMETA(gsn uint64, touchedGroups []uint32) {
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

// PublishMETASkip avisa todos os nós (e este) que os GSNs [from, to] nunca serão atribuídos.
// Usado pelo novo líder ao retomar o contador com folga (gsnRecoverySafetyMargin): sem esse
// aviso, a entrega em ordem global (globaldeliver.go) esperaria para sempre pelo META deles.
func (s *Sequencer) PublishMETASkip(from, to uint64) {
	if from == 0 || to < from {
		return
	}
	fmt.Printf("[SEQUENCER][META-SKIP] publicando faixa pulada [%d..%d]\n", from, to)
	s.recordSkip(from, to)
	payload := fmt.Sprintf("%s%d:%d", SYSTEM_META_SKIP, from, to)
	for _, nodeID := range membership.AllNodeIDs() {
		if nodeID == membership.OwnID {
			continue
		}
		messenger.EnqueuePriorityMsg(&pb.ProtocolMessage{
			SenderId: membership.OwnID, Sn: -1,
			Msg: &pb.ProtocolMessage_GsnReqForward{GsnReqForward: &pb.GSNReqForward{
				Req: &pb.ClientRequest{
					RequestId: &pb.RequestID{ClientId: membership.OwnID, ClientSn: int32(atomic.AddUint32(&s.metaCounter, 1))},
					Payload:   []byte(payload),
				},
			}},
		}, nodeID)
	}
}

// HandleMETASkip processa um META_SKIP recebido de outro nó.
func (s *Sequencer) HandleMETASkip(payload string) {
	var from, to uint64
	if n, _ := fmt.Sscanf(payload, SYSTEM_META_SKIP+"%d:%d", &from, &to); n < 2 {
		return
	}
	s.recordSkip(from, to)
}

// recordSkip registra uma faixa pulada (idempotente) e repassa ao GlobalDeliverer.
func (s *Sequencer) recordSkip(from, to uint64) {
	if from == 0 || to < from {
		return
	}
	s.skipMu.Lock()
	if s.skipRanges == nil {
		s.skipRanges = make(map[[2]uint64]bool)
	}
	isNew := !s.skipRanges[[2]uint64{from, to}]
	s.skipRanges[[2]uint64{from, to}] = true
	s.skipMu.Unlock()
	if !isNew {
		return
	}
	fmt.Printf("[SEQUENCER][META-SKIP-RECV] [%d..%d]\n", from, to)
	if s.onSkip != nil {
		s.onSkip(from, to)
	}
}

// skipSuffix devolve as faixas puladas conhecidas no formato ":a-b,c-d" (vazio se nenhuma),
// anexado ao payload do heartbeat.
func (s *Sequencer) skipSuffix() string {
	s.skipMu.Lock()
	defer s.skipMu.Unlock()
	if len(s.skipRanges) == 0 {
		return ""
	}
	parts := make([]string, 0, len(s.skipRanges))
	for r := range s.skipRanges {
		parts = append(parts, fmt.Sprintf("%d-%d", r[0], r[1]))
	}
	sort.Strings(parts)
	return ":" + strings.Join(parts, ",")
}

// parseSkipSuffix lê as faixas anexadas ao heartbeat (terceiro campo em diante).
func (s *Sequencer) parseSkipSuffix(payload string) {
	fields := strings.Split(strings.TrimPrefix(payload, SYSTEM_SEQ_HEARTBEAT), ":")
	if len(fields) < 3 || fields[2] == "" {
		return
	}
	for _, r := range strings.Split(fields[2], ",") {
		var from, to uint64
		if n, _ := fmt.Sscanf(r, "%d-%d", &from, &to); n == 2 {
			s.recordSkip(from, to)
		}
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

	if s.onMeta != nil {
		s.onMeta(gsn, touchedGroups)
	}
}
