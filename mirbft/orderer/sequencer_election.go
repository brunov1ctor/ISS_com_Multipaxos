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

// gsnRecoverySafetyMargin cobre GSNs alocados pelo líder anterior cujo META
// ainda não tenha se propagado quando este nó assume a liderança. Pode gerar
// pequenos saltos no espaço de GSN (inofensivo), nunca reuso.
const gsnRecoverySafetyMargin = 100

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
	timeout := electionTimeoutFor(config.Config.SequencerElectionTimeoutMs)
	s.electionTimer = time.AfterFunc(timeout, s.onElectionTimeout)
}

// onElectionTimeout dispara quando o timer de eleição deste nó expira sem heartbeat do líder
// nem vitória — seguidor sem notícia do líder, ou candidato cuja própria eleição não deu quórum
// a tempo. Em ambos os casos, incrementa o termo, vira candidato, vota em si mesmo e pede votos.
func (s *Sequencer) onElectionTimeout() {
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
	responsePayload := fmt.Sprintf("%s%d:%d", SYSTEM_SEQ_VOTE_RESPONSE, replyTerm, grantedInt)
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

// resumeGSNCounterOnBecomingLeader recalcula nextGSN a partir do maior GSN já
// observado via META -- que já é mantido em todo nó por RegisterMetadata,
// nunca removido -- evitando reemitir um GSN já usado pelo líder anterior.
func (s *Sequencer) resumeGSNCounterOnBecomingLeader() {
	s.metaMu.RLock()
	var maxGSN uint64
	for gsn := range s.metadata {
		if gsn > maxGSN {
			maxGSN = gsn
		}
	}
	s.metaMu.RUnlock()

	resumeFrom := maxGSN + 1 + gsnRecoverySafetyMargin

	s.gsnMu.Lock()
	if resumeFrom > s.nextGSN {
		s.nextGSN = resumeFrom
	}
	newNext := s.nextGSN
	s.gsnMu.Unlock()

	fmt.Printf("[SEQUENCER][ELECTION] nextGSN retomado em %d (maxGSN observado via META=%d)\n", newNext, maxGSN)
	logger.Info().Uint64("nextGSN", newNext).Uint64("maxObservedGSN", maxGSN).Msg("Sequencer resumed GSN counter after election")
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

		payload := fmt.Sprintf("%s%d:%d", SYSTEM_SEQ_HEARTBEAT, term, membership.OwnID)
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
