package orderer

import (
	"sync"

	logger "github.com/rs/zerolog/log"

	// TODO: quando for plugar verificação de assinatura real, reintroduza o crypto e use a pubkey correta.
	// "github.com/hyperledger-labs/mirbft/crypto"
	logmod "github.com/hyperledger-labs/mirbft/log"
	"github.com/hyperledger-labs/mirbft/manager"
	"github.com/hyperledger-labs/mirbft/membership"
	"github.com/hyperledger-labs/mirbft/messenger"
	pb "github.com/hyperledger-labs/mirbft/protobufs"
)

// instID identifica uma instância (sn, bucket)
type instID struct {
	sn     int32
	bucket int32
}

// MultiPaxosOrderer implementa a interface orderer.Orderer.
type MultiPaxosOrderer struct {
	mu        sync.Mutex
	mngr      manager.Manager
	instances map[instID]*multiPaxosInstance
}

// Init anexa o Manager.
func (o *MultiPaxosOrderer) Init(mngr manager.Manager) {
	o.mngr = mngr
	o.instances = make(map[instID]*multiPaxosInstance)
	logger.Info().Msg("MultiPaxosOrderer initialized")
}

// Start: cria workers por segmento conforme forem anunciados pelo Manager.
func (o *MultiPaxosOrderer) Start(wg *sync.WaitGroup) {
	defer wg.Done()
	logger.Info().Msg("MultiPaxosOrderer started")
	// TODO: iniciar rotinas de segmento (Phase1 once por líder, etc.).
}

// Stop encerra workers (se necessário).
func (o *MultiPaxosOrderer) Stop() {
	logger.Info().Msg("MultiPaxosOrderer stopped")
}

// CheckSig verifica a assinatura de uma mensagem de peer.
// Stub inicial: retorna nil para aceitar. Substitua pela verificação real,
// copiando o padrão dos outros orderers do seu repo (Pbft/Raft/HotStuff).
func (o *MultiPaxosOrderer) CheckSig(data []byte, senderID int32, signature []byte) error {
	// Exemplo (quando plugar):
	// pk := membership.<AlgumaFuncPraPegarChaveDoNo>(senderID)
	// return crypto.CheckSig(data, pk, signature)
	return nil
}

// Sign assina o blob 'data' para mensagens que exigem assinatura.
// Stub inicial: retorna nil. Substitua pela chamada ao seu keystore/crypto.
func (o *MultiPaxosOrderer) Sign(data []byte) ([]byte, error) {
	// Exemplo (quando plugar):
	// return crypto.Sign(data, membership.OwnPrivKey())
	return nil, nil
}

// HandleEntry é chamado pelo pipeline para cada entrada comprometida que deve ser aplicada/propagada.
func (o *MultiPaxosOrderer) HandleEntry(entry *logmod.Entry) {
	// Evitar usar len(entry.Batch) pois é uma mensagem protobuf, não []byte.
	logger.Debug().Int32("sn", entry.Sn).Msg("HandleEntry (MultiPaxos)")
}

// HandleMessage despacha mensagens MultiPaxos para a instância correta.
func (o *MultiPaxosOrderer) HandleMessage(msg *pb.ProtocolMessage) {
	// Ignora eco das próprias mensagens
	if msg.SenderId == membership.OwnID {
		return
	}

	mpx, ok := msg.Msg.(*pb.ProtocolMessage_Multipaxos)
	if !ok || mpx.Multipaxos == nil {
		return // outras mensagens não são do MultiPaxos
	}

	id, ok := getInstIDFromMPx(mpx.Multipaxos)
	if !ok {
		logger.Warn().Msg("MPx message without instance id")
		return
	}

	// Garante a instância
	o.mu.Lock()
	inst := o.instances[id]
	if inst == nil {
		// TODO: obter o segmento correto via Manager (por SN)
		var seg *manager.Segment // placeholder: adapte à API real
		inst = newMultiPaxosInstance(id, seg, o)
		o.instances[id] = inst
	}
	o.mu.Unlock()

	inst.handleNetMessage(msg.SenderId, mpx.Multipaxos)
}

// wrapMPx empacota MPxMsg em ProtocolMessage.
func (o *MultiPaxosOrderer) wrapMPx(id instID, m *pb.MPxMsg) *pb.ProtocolMessage {
	return &pb.ProtocolMessage{
		SenderId: membership.OwnID,
		Sn:       id.sn,
		Msg: &pb.ProtocolMessage_Multipaxos{
			Multipaxos: m,
		},
	}
}

// ===== Helpers =====

// Extrai (sn,bucket) do oneof do MPxMsg.
func getInstIDFromMPx(m *pb.MPxMsg) (instID, bool) {
	switch t := m.Type.(type) {
	case *pb.MPxMsg_Prepare:
		return instID{sn: t.Prepare.Id.Sn, bucket: t.Prepare.Id.Bucket}, true
	case *pb.MPxMsg_Promise:
		return instID{sn: t.Promise.Id.Sn, bucket: t.Promise.Id.Bucket}, true
	case *pb.MPxMsg_Accept:
		return instID{sn: t.Accept.Id.Sn, bucket: t.Accept.Id.Bucket}, true
	case *pb.MPxMsg_Accepted:
		return instID{sn: t.Accepted.Id.Sn, bucket: t.Accepted.Id.Bucket}, true
	case *pb.MPxMsg_Commit:
		return instID{sn: t.Commit.Id.Sn, bucket: t.Commit.Id.Bucket}, true
	default:
		return instID{}, false
	}
}

// majority retorna N/2+1 (crash-fault majority).
func majority() int {
	n := membership.NumNodes()
	return n/2 + 1
}

// announceCommit é um stub para integrar com seu pipeline de log/SMR.
func announceCommit(sn int32, batch []byte, digest []byte) {
	// TODO: integre com announcer/log do seu ISS (ex.: announcer.Announce(log.Entry{...}))
	_ = logmod.Entry{} // mantém import por enquanto
	logger.Info().Int32("sn", sn).Int("batchLen", len(batch)).Msg("Committed")
}

// ===== Envio de mensagens =====

func (o *MultiPaxosOrderer) send(to int32, id instID, m *pb.MPxMsg) {
	pm := o.wrapMPx(id, m)
	messenger.EnqueueMsg(pm, to)
}

func (o *MultiPaxosOrderer) bcast(id instID, m *pb.MPxMsg) {
	for _, nid := range allNodeIDs() {
		if nid != membership.OwnID {
			o.send(nid, id, m)
		}
	}
}

// allNodeIDs é um helper simples enquanto você não integra com Manager/Membership reais.
func allNodeIDs() []int32 {
	n := membership.NumNodes()
	ids := make([]int32, n)
	for i := 0; i < n; i++ {
		ids[i] = int32(i)
	}
	return ids
}

// Garante em compile-time que MultiPaxosOrderer implementa a interface.
var _ Orderer = (*MultiPaxosOrderer)(nil)

