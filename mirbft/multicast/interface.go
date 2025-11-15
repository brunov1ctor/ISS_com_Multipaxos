package multicast

import (
	pb "github.com/hyperledger-labs/mirbft/protobufs"
)

// ID representa o identificador de nó destino (compatível com messenger / membership).
type ID = int32

// GroupID identifica um grupo lógico para difusão seletiva.
// Por convenção, 0 == "all" (todos os nós conhecidos).
type GroupID uint32

// Router define a API mínima de roteamento multicast usada pelo orderer.
type Router interface {
	// DefineGroup configura (ou substitui) o conjunto de membros de um grupo.
	DefineGroup(g GroupID, members ...ID)

	// SendGroup envia uma mensagem para todos os membros do grupo.
	// O builder recebe o destino e deve retornar uma *nova* mensagem (ou nil para pular).
	SendGroup(g GroupID, builder func(dst ID) *pb.ProtocolMessage)
}

