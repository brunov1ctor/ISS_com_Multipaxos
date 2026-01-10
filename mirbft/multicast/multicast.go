package multicast

import (
	"bytes"
	"fmt"
	"net"
	"sync"

	"github.com/hyperledger-labs/mirbft/membership"
	"github.com/hyperledger-labs/mirbft/messenger"
	pb "github.com/hyperledger-labs/mirbft/protobufs"
	"golang.org/x/net/ipv4"
	"google.golang.org/protobuf/proto"
)

type ID = int32
type GroupID uint32

type Router interface {
	DefineGroup(g GroupID, members ...ID)
	SendGroup(g GroupID, builder func(dst ID) *pb.ProtocolMessage)
}

type UnicastRouter struct {
	mu     sync.RWMutex
	groups map[GroupID][]ID
}

func NewUnicastRouter() Router {
	r := &UnicastRouter{
		groups: make(map[GroupID][]ID),
	}
	r.groups[GroupID(0)] = membership.AllNodeIDs()
	return r
}

func (r *UnicastRouter) DefineGroup(g GroupID, members ...ID) {
	r.mu.Lock()
	r.groups[g] = append([]ID{}, members...)
	r.mu.Unlock()
}

func (r *UnicastRouter) SendGroup(g GroupID, builder func(dst ID) *pb.ProtocolMessage) {
	r.mu.RLock()
	members := r.groups[g]
	r.mu.RUnlock()

	for _, dst := range members {
		if dst == membership.OwnID {
			continue
		}
		if msg := builder(dst); msg != nil {
			messenger.EnqueueMsg(msg, dst)
		}
	}
}

// ----------------------------------------------------------------------------
// UDP Multicast router
// ----------------------------------------------------------------------------

type UDPRouter struct {
	mu        sync.RWMutex
	groups    map[GroupID]*udpGroup
	listeners map[GroupID]*net.UDPConn
	sendPConn *ipv4.PacketConn

	baseAddr string
	basePort int
	iface    *net.Interface

	stopped bool
}

type udpGroup struct {
	addr    *net.UDPAddr
	members []ID
	conn    *net.UDPConn
	pconn   *ipv4.PacketConn
}

func NewUDPRouter(baseAddr string, basePort int, ifaceAddr ...string) (Router, error) {
	// Detecta interface de rede adequada (configurável para Emulab)
	var iface *net.Interface

	if len(ifaceAddr) > 0 && ifaceAddr[0] != "" {
		// Usa interface especificada por IP
		if interfaces, err := net.Interfaces(); err == nil {
			for _, i := range interfaces {
				if addrs, err := i.Addrs(); err == nil {
					for _, addr := range addrs {
						if ipnet, ok := addr.(*net.IPNet); ok {
							if ipnet.IP.String() == ifaceAddr[0] {
								iface = &i
								break
							}
						}
					}
					if iface != nil {
						break
					}
				}
			}
		}
	} else {
		// Auto-detecta primeira interface IPv4 não-loopback
		if interfaces, err := net.Interfaces(); err == nil {
			for _, i := range interfaces {
				if addrs, err := i.Addrs(); err == nil {
					for _, addr := range addrs {
						if ipnet, ok := addr.(*net.IPNet); ok && !ipnet.IP.IsLoopback() {
							if ipnet.IP.To4() != nil {
								iface = &i
								break
							}
						}
					}
					if iface != nil {
						break
					}
				}
			}
		}
	}

	r := &UDPRouter{
		groups:    make(map[GroupID]*udpGroup),
		listeners: make(map[GroupID]*net.UDPConn),
		sendPConn: nil,
		baseAddr:  baseAddr,
		basePort:  basePort,
		iface:     iface,
	}

	// Grupo "all" via multicast - usa porta base
	allAddr, _ := net.ResolveUDPAddr("udp4", fmt.Sprintf("%s.0:%d", baseAddr, basePort))
	allConn, err := net.ListenMulticastUDP("udp4", iface, allAddr)
	if err != nil {
		return nil, err
	}

	// PacketConn para envio configurado (iface/TTL)
	r.sendPConn = ipv4.NewPacketConn(allConn)
	if iface != nil {
		_ = r.sendPConn.SetMulticastInterface(iface)
	}
	_ = r.sendPConn.SetMulticastTTL(2)

	r.groups[GroupID(0)] = &udpGroup{
		addr:    allAddr,
		members: membership.AllNodeIDs(),
		conn:    allConn,
		pconn:   ipv4.NewPacketConn(allConn),
	}
	r.listeners[GroupID(0)] = allConn

	// Receiver do grupo "all"
	go r.receiveLoop(GroupID(0), allConn)

	// Pré-cria grupos fixos (limitado)
	numNodes := len(membership.AllNodeIDs())
	maxGroups := numNodes
	if maxGroups > 8 {
		maxGroups = 8
	}

	for gid := 1; gid <= maxGroups; gid++ {
		port := basePort + gid
		addr, _ := net.ResolveUDPAddr("udp4", fmt.Sprintf("%s.%d:%d", baseAddr, gid, port))

		conn, err := net.ListenMulticastUDP("udp4", iface, addr)
		if err != nil {
			fmt.Printf("[UDP-MC] Failed to pre-create group %d: %v\n", gid, err)
			continue
		}

		group := &udpGroup{
			addr:    addr,
			members: []ID{},
			conn:    conn,
			pconn:   ipv4.NewPacketConn(conn),
		}

		r.groups[GroupID(gid)] = group
		r.listeners[GroupID(gid)] = conn

		go r.receiveLoop(GroupID(gid), conn)
		fmt.Printf("[UDP-MC] Pre-created group %d on %v\n", gid, addr)
	}

	fmt.Printf("[UDP-MC] Initialized with %d pre-created groups, interface: %v\n", maxGroups, iface)
	return r, nil
}

func (r *UDPRouter) DefineGroup(g GroupID, members ...ID) {
	r.mu.Lock()
	defer r.mu.Unlock()

	// Se grupo existe, atualiza membros
	if group, exists := r.groups[g]; exists {
		group.members = append([]ID{}, members...)
		fmt.Printf("[UDP-MC] Updated group %d with %d members\n", g, len(members))
		return
	}

	// Se não existe, mapeia para algum pré-criado (round-robin)
	numGroups := len(r.groups) - 1 // exclui grupo 0
	if numGroups > 0 {
		mappedGID := GroupID(1 + (int(g) % numGroups))
		if mappedGroup, exists := r.groups[mappedGID]; exists {
			mappedGroup.members = append([]ID{}, members...)
			fmt.Printf("[UDP-MC] Mapped group %d -> %d with %d members\n", g, mappedGID, len(members))
			return
		}
	}

	// Fallback final: grupo 0 (all)
	if group0, exists := r.groups[GroupID(0)]; exists {
		group0.members = append([]ID{}, members...)
		fmt.Printf("[UDP-MC] Fallback group %d -> 0 (all) with %d members\n", g, len(members))
	}
}

func (r *UDPRouter) SendGroup(g GroupID, builder func(dst ID) *pb.ProtocolMessage) {
	// Resolve grupo (inclui mapeamento)
	r.mu.RLock()
	group := r.groups[g]
	if group == nil {
		numGroups := len(r.groups) - 1
		if numGroups > 0 {
			mappedGID := GroupID(1 + (int(g) % numGroups))
			group = r.groups[mappedGID]
		} else {
			group = r.groups[GroupID(0)]
		}
	}
	sendPConn := r.sendPConn
	r.mu.RUnlock()

	if group == nil || sendPConn == nil || len(group.members) == 0 {
		return
	}

	// ---------------------------
	// MUDANÇA MÍNIMA #1:
	// Verifica se builder(dst) gera mensagens idênticas.
	// Se não forem idênticas, faz fallback para unicast (sem quebrar semântica).
	// ---------------------------
	var (
		refBytes []byte
		allEqual = true
	)

	// Monta bytes para o primeiro destino (que não seja eu)
	var firstDst ID = -1
	for _, dst := range group.members {
		if dst == membership.OwnID {
			continue
		}
		firstDst = dst
		break
	}
	if firstDst == -1 {
		return
	}

	refMsg := builder(firstDst)
	if refMsg == nil {
		return
	}
	b0, err := proto.Marshal(refMsg)
	if err != nil {
		return
	}
	refBytes = b0

	// Compara com os demais destinos (se houver diferenças, fallback)
	for _, dst := range group.members {
		if dst == membership.OwnID || dst == firstDst {
			continue
		}
		msg := builder(dst)
		if msg == nil {
			// Se builder decide não enviar para alguém, multicast não serve => fallback
			allEqual = false
			break
		}
		bi, err := proto.Marshal(msg)
		if err != nil {
			allEqual = false
			break
		}
		if !bytes.Equal(refBytes, bi) {
			allEqual = false
			break
		}
	}

	if !allEqual {
		// Fallback correto: envia unicast normal (mantém semântica)
		for _, dst := range group.members {
			if dst == membership.OwnID {
				continue
			}
			if msg := builder(dst); msg != nil {
				messenger.EnqueueMsg(msg, dst)
			}
		}
		return
	}

	// Multicast “real”: 1 envio para o endereço do grupo
	_, _ = sendPConn.WriteTo(refBytes, nil, group.addr)
}

func (r *UDPRouter) receiveLoop(gid GroupID, conn *net.UDPConn) {
	buffer := make([]byte, 64*1024)
	for {
		r.mu.RLock()
		stopped := r.stopped
		// snapshot do grupo para filtro rápido (mudança mínima)
		group := r.groups[gid]
		r.mu.RUnlock()

		if stopped {
			return
		}

		n, _, err := conn.ReadFromUDP(buffer)
		if err != nil {
			continue
		}

		var msg pb.ProtocolMessage
		if err := proto.Unmarshal(buffer[:n], &msg); err != nil {
			continue
		}

		// Ignora eco próprio
		if msg.SenderId == membership.OwnID {
			continue
		}

		// ---------------------------
		// MUDANÇA MÍNIMA #2:
		// Entrega só se eu pertenço ao grupo (exceto gid==0 que é "all").
		// Isso evita que todo mundo processe tudo só porque deu join no grupo.
		// ---------------------------
		if gid != GroupID(0) && group != nil {
			if !idInList(membership.OwnID, group.members) {
				continue
			}
		}

		messenger.HandleMessage(&msg)
	}
}

func idInList(id ID, members []ID) bool {
	for _, m := range members {
		if m == id {
			return true
		}
	}
	return false
}

func (r *UDPRouter) Close() error {
	r.mu.Lock()
	r.stopped = true

	if r.sendPConn != nil {
		_ = r.sendPConn.Close()
	}

	for gid, conn := range r.listeners {
		if group := r.groups[gid]; group != nil && group.pconn != nil {
			_ = group.pconn.Close()
		}
		_ = conn.Close()
	}
	r.mu.Unlock()

	return nil
}

// Factory functions
func NewStaticRouter() Router {
	return NewUnicastRouter()
}

// Mantém o nome esperado pelos chamadores
func NewUDPMulticastRouter(baseAddr string, basePort int, ifaceAddr ...string) (Router, error) {
	return NewUDPRouter(baseAddr, basePort, ifaceAddr...)
}

