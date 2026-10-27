// sequencer_election.go - Failover do líder do Sequencer via um mecanismo
// inspirado no Raft (term/votedFor/status/votes/heartbeat), modelado em
// raftinstance.go mas sem replicação de log: o Sequencer não tem log a
// replicar, só um contador de GSN e a pergunta "quem está vivo agora".
//
// Bootstrap: NewSequencer() continua elegendo o nó de menor ID como líder do
// termo 0 (todos os nós chegam ao mesmo valor sem eleição real). Uma eleição
// de verdade (termo >= 1) só ocorre se o timer de eleição de algum seguidor
// expirar sem ter sido resetado por um heartbeat do líder.
package orderer

import (
	"fmt"
	"math/rand"
	"time"

	"github.com/hyperledger-labs/mirbft/config"
	"github.com/hyperledger-labs/mirbft/membership"
	"github.com/hyperledger-labs/mirbft/messenger"
	pb "github.com/hyperledger-labs/mirbft/protobufs"
	logger "github.com/rs/zerolog/log"
)

// startupElectionGrace: timeout do PRIMEIRO timer de eleição, na largada. Os primeiros
// heartbeats do líder de bootstrap se perdem enquanto as conexões sobem; com o timeout normal
// um seguidor disparava eleição sem falha nenhuma (visto no Emulab: o Node 0 se reelegia).
func startupElectionGrace() time.Duration {
	min := config.Config.SequencerElectionTimeoutMs
	if min <= 0 {
		min = 500
	}
	g := time.Duration(10*min) * time.Millisecond
	if g < 5*time.Second {
		g = 5 * time.Second
	}
	return g
}

// currentLeader retorna o líder atualmente conhecido do sequenciador (thread-safe).
func (s *Sequencer) currentLeader() int32 {
	s.electMu.RLock()
	defer s.electMu.RUnlock()
	return s.leader
}

// electionTimeoutFor sorteia um timeout em [min, 2*min], como no Raft, para
// evitar que vários seguidores disputem eleição no mesmo instante.
func electionTimeoutFor(minMs int) time.Duration {
	if minMs <= 0 {
		minMs = 500
	}
	jitter := rand.Intn(minMs)
	return time.Duration(minMs+jitter) * time.Millisecond
}

// startElectionTimer arma (ou rearma) o timer de eleição do seguidor.
func (s *Sequencer) startElectionTimer() {
	s.electMu.Lock()
	defer s.electMu.Unlock()
	s.startElectionTimerLocked()
}

// startElectionTimerLocked assume que electMu já está travado pelo chamador.
func (s *Sequencer) startElectionTimerLocked() {
	if s.electionTimer != nil {
		s.electionTimer.Stop()
	}
	s.startElectionTimerLockedFor(electionTimeoutFor(config.Config.SequencerElectionTimeoutMs))
}

// startElectionTimerLockedFor arma o timer com um timeout específico (electMu travado).
func (s *Sequencer) startElectionTimerLockedFor(timeout time.Duration) {
	if s.electionTimer != nil {
		s.electionTimer.Stop()
	}
	s.electionTimer = time.AfterFunc(timeout, s.onElectionTimeout)
}

// onElectionTimeout dispara quando o timer de eleição deste nó expira sem heartbeat do líder
// nem vitória — seguidor sem notícia do líder, ou candidato cuja própria eleição não deu quórum
// a tempo. Em ambos os casos, incrementa o termo, vira candidato, vota em si mesmo e pede votos.
func (s *Sequencer) onElectionTimeout() {
	meta := s.maxMetaSeen()
	s.gsnMu.Lock()
	accepted := s.acceptedUpTo
	s.gsnMu.Unlock()

	s.electMu.Lock()
	if s.status == leader {
		// Líder não dispara eleição contra si mesmo.
		s.electMu.Unlock()
		return
	}
	s.term++
	s.status = candidate
	s.votedFor = membership.OwnID
	s.votes = map[int32]bool{membership.OwnID: true}
	// Começa com os próprios valores; os votos recebidos podem aumentá-los (protegidos por electMu).
	s.voteMaxAccepted = accepted
	s.voteMaxMeta = meta
	term := s.term
	s.startElectionTimerLocked()
	s.electMu.Unlock()

	fmt.Printf("[SEQUENCER][ELECTION] Timeout -> candidato term=%d ownID=%d\n", term, membership.OwnID)
	logger.Info().Int32("term", term).Msg("Sequencer election timeout, becoming candidate")

	payload := fmt.Sprintf("%s%d:%d", SYSTEM_SEQ_VOTE_REQUEST, term, membership.OwnID)
	s.broadcastToMembers(payload)
}

// HandleVoteRequest processa um pedido de voto vindo de um candidato.
func (s *Sequencer) HandleVoteRequest(payload string, senderID int32) {
	var term int64
	var candidateID int32
	if n, _ := fmt.Sscanf(payload, SYSTEM_SEQ_VOTE_REQUEST+"%d:%d", &term, &candidateID); n < 2 {
		return
	}

	s.electMu.Lock()
	if int32(term) > s.term {
		s.term = int32(term)
		s.votedFor = -1
		s.status = follower
	}
	granted := false
	if int32(term) == s.term && (s.votedFor == -1 || s.votedFor == candidateID) {
		s.votedFor = candidateID
		granted = true
		s.startElectionTimerLocked()
	}
	replyTerm := s.term
	s.electMu.Unlock()

	grantedInt := 0
	if granted {
		grantedInt = 1
	}
	s.gsnMu.Lock()
	accepted := s.acceptedUpTo
	s.gsnMu.Unlock()
	// Voto leva a maior reserva que este nó confirmou e o maior GSN com META que viu.
	responsePayload := fmt.Sprintf("%s%d:%d:%d:%d", SYSTEM_SEQ_VOTE_RESPONSE, replyTerm, grantedInt, accepted, s.maxMetaSeen())
	messenger.EnqueuePriorityMsg(&pb.ProtocolMessage{
		SenderId: membership.OwnID, Sn: -1,
		Msg: &pb.ProtocolMessage_GsnReqForward{GsnReqForward: &pb.GSNReqForward{
			Req: &pb.ClientRequest{Payload: []byte(responsePayload)},
		}},
	}, senderID)
}

// HandleVoteResponse processa uma resposta de voto recebida por um candidato.
func (s *Sequencer) HandleVoteResponse(payload string, senderID int32) {
	var term int64
	var grantedInt int
	if n, _ := fmt.Sscanf(payload, SYSTEM_SEQ_VOTE_RESPONSE+"%d:%d", &term, &grantedInt); n < 2 {
		return
	}

	s.electMu.Lock()
	if int32(term) > s.term {
		// Alguém já está num termo mais adiante -- desiste da candidatura.
		s.term = int32(term)
		s.status = follower
		s.votedFor = -1
		s.electMu.Unlock()
		return
	}
	if s.status != candidate || int32(term) != s.term || grantedInt == 0 {
		s.electMu.Unlock()
		return
	}
	if s.votes == nil {
		s.votes = make(map[int32]bool)
	}
	s.votes[senderID] = true
	var vAcc, vMeta uint64
	var t2 int64
	var g2 int
	if n, _ := fmt.Sscanf(payload, SYSTEM_SEQ_VOTE_RESPONSE+"%d:%d:%d:%d", &t2, &g2, &vAcc, &vMeta); n == 4 {
		if vAcc > s.voteMaxAccepted {
			s.voteMaxAccepted = vAcc
		}
		if vMeta > s.voteMaxMeta {
			s.voteMaxMeta = vMeta
		}
	}
	becameLeader := len(s.votes) > len(s.members)/2
	wonTerm := s.term
	if becameLeader {
		s.status = leader
		s.leader = membership.OwnID
		if s.electionTimer != nil {
			s.electionTimer.Stop()
		}
	}
	s.electMu.Unlock()

	if becameLeader {
		fmt.Printf("[SEQUENCER][ELECTION] Venceu eleição term=%d ownID=%d\n", wonTerm, membership.OwnID)
		logger.Info().Int32("term", wonTerm).Int32("ownID", membership.OwnID).Msg("Sequencer became leader")
		s.resumeGSNCounterOnBecomingLeader()
		go s.leaderHeartbeatLoop(wonTerm)
	}
}

// resumeGSNCounterOnBecomingLeader: o novo líder retoma a numeração a partir da maior reserva
// informada pela maioria que o elegeu (voteMaxAccepted). Toda reserva confirmada foi vista por
// uma maioria, e duas maiorias sempre se cruzam: nenhum GSN acima dessa marca pode ter sido
// usado. A sobra (do maior META visto até a marca) é declarada pulada com META_SKIP.
func (s *Sequencer) resumeGSNCounterOnBecomingLeader() {
	ownMeta := s.maxMetaSeen()
	s.electMu.RLock()
	maxAcc := s.voteMaxAccepted
	maxMeta := s.voteMaxMeta
	s.electMu.RUnlock()
	s.gsnMu.Lock()
	if s.acceptedUpTo > maxAcc {
		maxAcc = s.acceptedUpTo
	}
	if ownMeta > maxMeta {
		maxMeta = ownMeta
	}
	oldNext := s.nextGSN
	resume := maxAcc + 1
	if oldNext > resume {
		resume = oldNext
	}
	s.nextGSN = resume
	s.confirmedUpTo = resume - 1 // só numera depois de uma nova reserva confirmada
	s.reservePending = 0
	s.requestReservationLocked()
	s.gsnMu.Unlock()

	// Sobra declarada pulada: GSNs sem META conhecido pela maioria e abaixo da marca. Começa no
	// maior entre maxMeta+1 e o que este nó já numerou (se ele foi líder antes).
	skipFrom := maxMeta + 1
	if oldNext > skipFrom {
		skipFrom = oldNext
	}
	if resume-1 >= skipFrom {
		s.PublishMETASkip(skipFrom, resume-1)
	}

	fmt.Printf("[SEQUENCER][ELECTION] nextGSN retomado em %d (maior reserva da maioria=%d, maior META=%d)\n", resume, maxAcc, maxMeta)
	logger.Info().Uint64("nextGSN", resume).Uint64("maxReserved", maxAcc).Uint64("maxMeta", maxMeta).Msg("Sequencer resumed GSN counter after election")
}

// leaderHeartbeatLoop transmite heartbeats periódicos enquanto este nó
// permanecer líder do termo `term`. Sai assim que deixa de ser líder desse termo.
func (s *Sequencer) leaderHeartbeatLoop(term int32) {
	for {
		s.electMu.RLock()
		stillLeader := s.status == leader && s.term == term
		s.electMu.RUnlock()
		if !stillLeader {
			return
		}

		payload := fmt.Sprintf("%s%d:%d", SYSTEM_SEQ_HEARTBEAT, term, membership.OwnID) + s.skipSuffix() // + faixas de GSN puladas
		s.broadcastToMembers(payload)

		time.Sleep(config.Config.SequencerHeartbeat)

		s.electMu.RLock()
		stillLeader = s.status == leader && s.term == term
		s.electMu.RUnlock()
		if !stillLeader {
			return
		}
	}
}

// HandleHeartbeat processa um heartbeat recebido do líder atual, ou de um líder
// com termo mais recente que este nó ainda não reconhecia (só líderes enviam
// heartbeat — candidatos enviam pedido de voto, tratado por HandleVoteRequest).
func (s *Sequencer) HandleHeartbeat(payload string, senderID int32) {
	var term int64
	var leaderID int32
	if n, _ := fmt.Sscanf(payload, SYSTEM_SEQ_HEARTBEAT+"%d:%d", &term, &leaderID); n < 2 {
		return
	}

	s.parseSkipSuffix(payload) // faixas de GSN puladas (valem mesmo vindas de um termo antigo)

	s.electMu.Lock()
	if int32(term) < s.term {
		// Heartbeat de um líder obsoleto (termo antigo) -- ignora, não reseta o timer.
		s.electMu.Unlock()
		return
	}
	wasLeader := s.status == leader
	s.term = int32(term)
	s.leader = leaderID
	s.votedFor = leaderID
	s.status = follower
	s.startElectionTimerLocked()
	s.electMu.Unlock()

	if wasLeader && leaderID != membership.OwnID {
		fmt.Printf("[SEQUENCER][ELECTION] Cedendo liderança: heartbeat term=%d de leader=%d\n", term, leaderID)
		logger.Info().Int32("term", int32(term)).Int32("newLeader", leaderID).Msg("Sequencer stepping down")
	}
}

// broadcastToMembers envia payload (via canal prioritário) para todos os
// membros do grupo 0, exceto o próprio nó.
func (s *Sequencer) broadcastToMembers(payload string) {
	for _, nodeID := range s.members {
		if nodeID == membership.OwnID {
			continue
		}
		messenger.EnqueuePriorityMsg(&pb.ProtocolMessage{
			SenderId: membership.OwnID, Sn: -1,
			Msg: &pb.ProtocolMessage_GsnReqForward{GsnReqForward: &pb.GSNReqForward{
				Req: &pb.ClientRequest{Payload: []byte(payload)},
			}},
		}, nodeID)
	}
}
