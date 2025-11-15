package multicast

import (
	pb "github.com/hyperledger-labs/mirbft/protobufs"
)

// messengerShim expõe a interface Router usando uma função de envio fornecida.
type messengerShim struct {
	send   func(*pb.ProtocolMessage, int32)
	groups map[GroupID][]ID
}

// NewMessengerShim cria um roteador multicast genérico em cima de uma função de envio.
// Ex.: NewMessengerShim(messenger.EnqueueMsg)
func NewMessengerShim(send func(*pb.ProtocolMessage, int32)) Router {
	if send == nil {
		return nil
	}
	return &messengerShim{
		send:   send,
		groups: make(map[GroupID][]ID),
	}
}

func (m *messengerShim) DefineGroup(g GroupID, members ...ID) {
	cp := make([]ID, len(members))
	copy(cp, members)
	m.groups[g] = cp
}

func (m *messengerShim) SendGroup(g GroupID, builder func(dst ID) *pb.ProtocolMessage) {
	members, ok := m.groups[g]
	if !ok {
		return
	}
	for _, dst := range members {
		// Evite eco para si próprio pelo caminho de rede
		msg := builder(dst)
		if msg != nil {
			m.send(msg, int32(dst))
		}
	}
}

