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
	"sort"
	"strings"
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

// minElectionTimeout é o prazo mínimo sem notícia do líder para um nó aceitar trocá-lo.
func minElectionTimeout() time.Duration {
	ms := config.Config.SequencerElectionTimeoutMs
	if ms <= 0 {
		ms = 500
	}
	return time.Duration(ms) * time.Millisecond
}

// onElectionTimeout dispara quando o timer de eleição expira sem notícia do líder. Antes de
// subir o termo, pergunta aos outros se aceitariam trocar de líder (pré-votação): sob carga, um
// heartbeat atrasado num só nó não derruba um líder que a maioria ainda ouve.
func (s *Sequencer) onElectionTimeout() {
	s.electMu.Lock()
	if s.status == leader {
		s.electMu.Unlock()
		return
	}
	s.preVoteTerm = s.term + 1
	s.preVotes = map[int32]bool{membership.OwnID: true}
	next := s.preVoteTerm
	won := len(s.preVotes) > len(s.members)/2
	s.startElectionTimerLocked()
	s.electMu.Unlock()
	if won {
		s.startElection()
		return
	}
	s.broadcastToMembers(fmt.Sprintf("%s%d:%d", SYSTEM_SEQ_PREVOTE_REQUEST, next, membership.OwnID))
}

// HandlePreVoteRequest concede o pré-voto só se este nó também não ouve o líder há um prazo
// mínimo de eleição. Não muda termo nem voto.
func (s *Sequencer) HandlePreVoteRequest(payload string, senderID int32) {
	var term int64
	var candidateID int32
	if n, _ := fmt.Sscanf(payload, SYSTEM_SEQ_PREVOTE_REQUEST+"%d:%d", &term, &candidateID); n < 2 {
		return
	}
	s.electMu.RLock()
	granted := int32(term) > s.term && s.status != leader && time.Since(s.lastLeaderContact) >= minElectionTimeout()
	s.electMu.RUnlock()
	g := 0
	if granted {
		g = 1
	}
	messenger.EnqueuePriorityMsg(&pb.ProtocolMessage{
		SenderId: membership.OwnID, Sn: -1,
		Msg: &pb.ProtocolMessage_GsnReqForward{GsnReqForward: &pb.GSNReqForward{
			Req: &pb.ClientRequest{Payload: []byte(fmt.Sprintf("%s%d:%d", SYSTEM_SEQ_PREVOTE_RESPONSE, term, g))},
		}},
	}, senderID)
}

// HandlePreVoteResponse: com a maioria dos pré-votos, começa a eleição de fato.
func (s *Sequencer) HandlePreVoteResponse(payload string, senderID int32) {
	var term int64
	var g int
	if n, _ := fmt.Sscanf(payload, SYSTEM_SEQ_PREVOTE_RESPONSE+"%d:%d", &term, &g); n < 2 || g == 0 {
		return
	}
	s.electMu.Lock()
	if s.status == leader || int32(term) != s.preVoteTerm || s.term+1 != int32(term) || s.preVotes == nil {
		s.electMu.Unlock()
		return
	}
	s.preVotes[senderID] = true
	won := len(s.preVotes) > len(s.members)/2
	if won {
		s.preVotes = nil
	}
	s.electMu.Unlock()
	if won {
		s.startElection()
	}
}

// startElection incrementa o termo, vira candidato, vota em si mesmo e pede votos.
func (s *Sequencer) startElection() {
	meta := s.maxMetaSeen()
	holes := s.metaHoles(meta)
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
	s.voteHoles = holes
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
	// Voto leva a maior reserva que este nó confirmou, o maior GSN com META que viu e os GSNs
	// abaixo dele que ficaram sem META.
	meta := s.maxMetaSeen()
	responsePayload := fmt.Sprintf("%s%d:%d:%d:%d:%s", SYSTEM_SEQ_VOTE_RESPONSE, replyTerm, grantedInt, accepted, meta,
		encodeRanges(s.metaHoles(meta)))
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
	if f := strings.Split(strings.TrimPrefix(payload, SYSTEM_SEQ_VOTE_RESPONSE), ":"); len(f) >= 5 {
		s.voteHoles = append(s.voteHoles, decodeRanges(f[4])...)
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
	holes := append([][2]uint64{}, s.voteHoles...)
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
	// GSNs abaixo do maior META que ficaram sem META (o proxy parou antes de publicar):
	// sem isso a entrega de todos os grupos espera por eles para sempre.
	holes = append(holes, s.metaHoles(maxMeta)...)
	if len(holes) > 0 {
		go s.recoverMetaHoles(holes)
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
	s.lastLeaderContact = time.Now()
	s.startElectionTimerLocked()
	s.electMu.Unlock()

	if wasLeader && leaderID != membership.OwnID {
		fmt.Printf("[SEQUENCER][ELECTION] Cedendo liderança: heartbeat term=%d de leader=%d\n", term, leaderID)
		logger.Info().Int32("term", int32(term)).Int32("newLeader", leaderID).Msg("Sequencer stepping down")
	}
}

// maxHoleScan limita quantos GSNs abaixo do maior META são examinados atrás de buracos.
const maxHoleScan = 200000

// metaQueryWait é quanto o novo líder espera os METAs republicados antes de pular o que faltar.
const metaQueryWait = 2 * time.Second

// metaHoles devolve, em faixas, os GSNs até `upto` sem META conhecido e fora das faixas puladas.
func (s *Sequencer) metaHoles(upto uint64) [][2]uint64 {
	if upto == 0 {
		return nil
	}
	from := uint64(1)
	if upto > maxHoleScan {
		from = upto - maxHoleScan + 1
	}
	s.skipMu.Lock()
	skips := make([][2]uint64, 0, len(s.skipRanges))
	for r := range s.skipRanges {
		skips = append(skips, r)
	}
	s.skipMu.Unlock()
	s.metaMu.RLock()
	defer s.metaMu.RUnlock()
	var out [][2]uint64
	for g := from; g <= upto; g++ {
		if _, ok := s.metadata[g]; ok || inRanges(skips, g) {
			continue
		}
		out = addToRanges(out, g)
	}
	return out
}

func inRanges(rs [][2]uint64, g uint64) bool {
	for _, r := range rs {
		if g >= r[0] && g <= r[1] {
			return true
		}
	}
	return false
}

// addToRanges acrescenta g (em ordem crescente) à lista de faixas.
func addToRanges(rs [][2]uint64, g uint64) [][2]uint64 {
	if n := len(rs); n > 0 && rs[n-1][1]+1 == g {
		rs[n-1][1] = g
		return rs
	}
	return append(rs, [2]uint64{g, g})
}

// encodeRanges escreve as faixas como "a-b,c-d" (até 512 faixas, para o voto não crescer demais).
func encodeRanges(rs [][2]uint64) string {
	if len(rs) > 512 {
		rs = rs[len(rs)-512:]
	}
	parts := make([]string, len(rs))
	for i, r := range rs {
		parts[i] = fmt.Sprintf("%d-%d", r[0], r[1])
	}
	return strings.Join(parts, ",")
}

func decodeRanges(s string) [][2]uint64 {
	var out [][2]uint64
	for _, p := range strings.Split(s, ",") {
		var a, b uint64
		if n, _ := fmt.Sscanf(p, "%d-%d", &a, &b); n == 2 && a > 0 && b >= a {
			out = append(out, [2]uint64{a, b})
		}
	}
	return out
}

// metaOf devolve os grupos do META de um GSN, ou nil se ele não é conhecido aqui.
func (s *Sequencer) metaOf(gsn uint64) []uint32 {
	s.metaMu.RLock()
	defer s.metaMu.RUnlock()
	if g, ok := s.metadata[gsn]; ok {
		return append([]uint32{}, g...)
	}
	return nil
}

func (s *Sequencer) isSkipped(gsn uint64) bool {
	s.skipMu.Lock()
	defer s.skipMu.Unlock()
	for r := range s.skipRanges {
		if gsn >= r[0] && gsn <= r[1] {
			return true
		}
	}
	return false
}

// recoverMetaHoles (novo líder): republica os METAs que tem, pede a todos os que não tem e,
// depois de metaQueryWait, declara pulados os que ninguém republicou.
func (s *Sequencer) recoverMetaHoles(holes [][2]uint64) {
	set := make(map[uint64]bool)
	for _, r := range holes {
		for g := r[0]; g <= r[1] && len(set) < maxHoleScan; g++ {
			set[g] = true
		}
	}
	gsns := make([]uint64, 0, len(set))
	for g := range set {
		gsns = append(gsns, g)
	}
	sort.Slice(gsns, func(i, j int) bool { return gsns[i] < gsns[j] })

	var missing [][2]uint64
	republished := 0
	for _, g := range gsns {
		if groups := s.metaOf(g); groups != nil {
			s.sendMETA(g, groups)
			republished++
		} else if !s.isSkipped(g) {
			missing = addToRanges(missing, g)
		}
	}
	fmt.Printf("[SEQUENCER][META-HOLES] %d GSNs sem META em algum votante: %d republicados, faltam %s\n",
		len(gsns), republished, encodeRanges(missing))
	if len(missing) == 0 {
		return
	}
	s.broadcastToMembers(SYSTEM_META_QUERY + encodeRanges(missing))
	time.Sleep(metaQueryWait)
	if !s.IsLeader() {
		return
	}
	var still [][2]uint64
	for _, r := range missing {
		for g := r[0]; g <= r[1]; g++ {
			if s.metaOf(g) == nil && !s.isSkipped(g) {
				still = addToRanges(still, g)
			}
		}
	}
	for _, r := range still {
		s.PublishMETASkip(r[0], r[1])
	}
}

// HandleMETAQuery: republica os METAs pedidos pelo novo líder que este nó conhece.
func (s *Sequencer) HandleMETAQuery(payload string) {
	for _, r := range decodeRanges(strings.TrimPrefix(payload, SYSTEM_META_QUERY)) {
		for g := r[0]; g <= r[1]; g++ {
			if groups := s.metaOf(g); groups != nil {
				s.sendMETA(g, groups)
			}
		}
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
