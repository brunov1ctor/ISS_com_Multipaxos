package orderer

import (
	mirlog "github.com/hyperledger-labs/mirbft/log"
	pb "github.com/hyperledger-labs/mirbft/protobufs"
)

// mpxmInstance: simplified instance for group-based Multi-Paxos
type mpxmInstance struct {
	ord *MultiPaxosMulticastOrderer
	// TODO: adicionar campos do Paxos (ballot/round, valor, quorum tracking, etc.)
}

// Propose(g, ent) — assinatura usando *log.Entry (consistente com HandleEntry).
func (i *mpxmInstance) Propose(g GroupID, ent *mirlog.Entry) {
	// TODO: líder decide PREPARE/ACCEPT e usa sendPrepare/sendAccept abaixo.
	_ = g
	_ = ent
}

// Exemplo de emissor de PREPARE via grupo. Ajustar quando integrar de fato.
func (i *mpxmInstance) sendPrepare(g GroupID, p *pb.MPxPrepare) {
	// TODO: usar i.ord.groupAPI.SendToGroup(g, msg, i.ord.emitToMembers)
	_ = g
	_ = p
}

// Promessas recebidas → pode disparar Accept
func (i *mpxmInstance) onPromise() {
	// TODO
}

// Emissor de ACCEPT via grupo
func (i *mpxmInstance) sendAccept(g GroupID, a *pb.MPxAccept) {
	_ = g
	_ = a
}

// Ao atingir quórum de Accepted → Commit
func (i *mpxmInstance) onAccepted() {
	// TODO
}

// Emissor de COMMIT via grupo
func (i *mpxmInstance) sendCommit(g GroupID, c *pb.MPxCommit) {
	_ = g
	_ = c
}

