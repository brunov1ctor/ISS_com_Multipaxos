//go:build multipaxos_multicast_impl

package orderer

import (
	mc "github.com/hyperledger-labs/mirbft/multicast"
	mirlog "github.com/hyperledger-labs/mirbft/log"
	pb "github.com/hyperledger-labs/mirbft/protobufs"
)

// mpxmInstance: esqueleto para a versão multicast real do Multi-Paxos.
// Este arquivo só compila quando a build tag multipaxos_multicast_impl estiver ativa.
type mpxmInstance struct {
	ord *MultiPaxosMulticastOrderer
	// TODO: adicionar campos do Paxos (ballot/round, valor, quorum tracking, etc.)
}

// Propose(g, ent) — assinatura usando *log.Entry (consistente com HandleEntry).
func (i *mpxmInstance) Propose(g mc.GroupID, ent *mirlog.Entry) {
	// TODO: líder decide PREPARE/ACCEPT e usa sendPrepare/sendAccept abaixo.
	_ = g
	_ = ent
}

// Exemplo de emissor de PREPARE via multicast. Ajustar quando integrar de fato.
func (i *mpxmInstance) sendPrepare(g mc.GroupID, p *pb.MPxPrepare) {
	// Exemplo: i.ord.mc.SendGroup(g, func(dst mc.ID) *pb.ProtocolMessage { ... })
	_ = g
	_ = p
}

// Promessas recebidas → pode disparar Accept
func (i *mpxmInstance) onPromise() {
	// TODO
}

// Emissor de ACCEPT via multicast
func (i *mpxmInstance) sendAccept(g mc.GroupID, a *pb.MPxAccept) {
	_ = g
	_ = a
}

// Ao atingir quórum de Accepted → Commit
func (i *mpxmInstance) onAccepted() {
	// TODO
}

// Emissor de COMMIT via multicast
func (i *mpxmInstance) sendCommit(g mc.GroupID, c *pb.MPxCommit) {
	_ = g
	_ = c
}

