package multicast

import (
	"fmt"
	"net"
	"sync"

	"google.golang.org/protobuf/proto"
	"github.com/hyperledger-labs/mirbft/membership"
	"github.com/hyperledger-labs/mirbft/messenger"
	pb "github.com/hyperledger-labs/mirbft/protobufs"
	"golang.org/x/net/ipv4"
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

// UDPRouter implementa multicast UDP real com join correto e identificação de grupo
type UDPRouter struct {
	mu        sync.RWMutex
	groups    map[GroupID]*udpGroup
	addrToGID map[string]GroupID  // mapeia IP:porta -> GroupID
	listeners map[GroupID]*net.UDPConn  // um listener por grupo
	sendPConn *ipv4.PacketConn    // PacketConn para envio com interface/TTL
	baseAddr  string
	basePort  int
	iface     *net.Interface      // interface de rede configurada
	stopped   bool
}

type udpGroup struct {
	addr    *net.UDPAddr
	members []ID
	conn    *net.UDPConn
	pconn   *ipv4.PacketConn
}

func NewUDPMulticastRouter(baseAddr string, basePort int, ifaceAddr ...string) (Router, error) {
	// Detecta interface de rede adequada (configurável para Emulab)
	var iface *net.Interface
	var err error
	
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
							if ipnet.IP.To4() != nil { // IPv4
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
		addrToGID: make(map[string]GroupID),
		listeners: make(map[GroupID]*net.UDPConn),
		sendPConn: nil, // será criado para envio
		baseAddr:  baseAddr,
		basePort:  basePort,
		iface:     iface,
	}

	// Grupo "all" via multicast (não broadcast) - usa porta base
	allAddr, _ := net.ResolveUDPAddr("udp4", fmt.Sprintf("%s.0:%d", baseAddr, basePort))
	allConn, err := net.ListenMulticastUDP("udp4", iface, allAddr)
	if err != nil {
		return nil, err
	}

	// Cria PacketConn para envio com interface/TTL configurados
	r.sendPConn = ipv4.NewPacketConn(allConn)
	if iface != nil {
		r.sendPConn.SetMulticastInterface(iface)
	}
	r.sendPConn.SetMulticastTTL(2) // TTL baixo para experimentos locais

	r.groups[GroupID(0)] = &udpGroup{
		addr:    allAddr,
		members: membership.AllNodeIDs(),
		conn:    allConn,
		pconn:   ipv4.NewPacketConn(allConn), // grupo 0 também precisa pconn
	}
	r.listeners[GroupID(0)] = allConn
	r.addrToGID[fmt.Sprintf("%s.0:%d", baseAddr, basePort)] = GroupID(0)

	// Inicia receiver para grupo "all"
	go r.receiveLoop(GroupID(0), allConn)

	// PRÉ-CRIA grupos fixos limitados (baseado no número de nós)
	numNodes := len(membership.AllNodeIDs())
	maxGroups := numNodes // ou numNodes/2, etc.
	if maxGroups > 8 {
		maxGroups = 8 // limite absoluto para Emulab
	}

	for gid := 1; gid <= maxGroups; gid++ {
		port := basePort + gid
		addr, _ := net.ResolveUDPAddr("udp4", fmt.Sprintf("%s.%d:%d", 
			baseAddr, gid, port))

		// Usa interface específica para Emulab
		conn, err := net.ListenMulticastUDP("udp4", iface, addr)
		if err != nil {
			fmt.Printf("[UDP-MC] Failed to pre-create group %d: %v\n", gid, err)
			continue
		}

		pconn := ipv4.NewPacketConn(conn)
		group := &udpGroup{
			addr:    addr,
			members: []ID{}, // será preenchido no DefineGroup
			conn:    conn,
			pconn:   pconn,
		}

		r.groups[GroupID(gid)] = group
		r.listeners[GroupID(gid)] = conn
		r.addrToGID[addr.String()] = GroupID(gid)

		// Inicia receiver para este grupo
		go r.receiveLoop(GroupID(gid), conn)
		fmt.Printf("[UDP-MC] Pre-created group %d on %v\n", gid, addr)
	}

	fmt.Printf("[UDP-MC] Initialized with %d pre-created groups, interface: %v\n", maxGroups, iface)
	return r, nil
}

func (r *UDPRouter) DefineGroup(g GroupID, members ...ID) {
	r.mu.Lock()
	defer r.mu.Unlock()

	// Usa grupos pré-criados - mapeia gid para grupo existente
	if group, exists := r.groups[g]; exists {
		// Atualiza membros do grupo pré-existente
		group.members = append([]ID{}, members...)
		fmt.Printf("[UDP-MC] Updated group %d with %d members\n", g, len(members))
		return
	}

	// Se grupo não existe, mapeia para grupo existente (round-robin)
	numGroups := len(r.groups) - 1 // exclui grupo 0
	if numGroups > 0 {
		mappedGID := GroupID(1 + (int(g) % numGroups))
		if mappedGroup, exists := r.groups[mappedGID]; exists {
			mappedGroup.members = append([]ID{}, members...)
			fmt.Printf("[UDP-MC] Mapped group %d -> %d with %d members\n", g, mappedGID, len(members))
			return
		}
	}

	// Fallback: usa grupo 0 (multicast "all")
	if group0, exists := r.groups[GroupID(0)]; exists {
		group0.members = append([]ID{}, members...)
		fmt.Printf("[UDP-MC] Fallback group %d -> 0 (multicast all) with %d members\n", g, len(members))
	}
}

func (r *UDPRouter) SendGroup(g GroupID, builder func(dst ID) *pb.ProtocolMessage) {
	r.mu.RLock()
	group := r.groups[g]
	// Se grupo não existe, mapeia para grupo pré-criado
	if group == nil {
		numGroups := len(r.groups) - 1
		if numGroups > 0 {
			mappedGID := GroupID(1 + (int(g) % numGroups))
			group = r.groups[mappedGID]
		} else {
			group = r.groups[GroupID(0)] // fallback multicast "all"
		}
	}
	sendPConn := r.sendPConn
	r.mu.RUnlock()

	if group == nil || len(group.members) == 0 || sendPConn == nil {
		return
	}

	msg := builder(group.members[0])
	if msg == nil {
		return
	}

	data, err := proto.Marshal(msg)
	if err != nil {
		return
	}

	// Envia usando PacketConn configurado com interface/TTL
	sendPConn.WriteTo(data, nil, group.addr)
}

func (r *UDPRouter) receiveLoop(gid GroupID, conn *net.UDPConn) {
	buffer := make([]byte, 64*1024)
	for {
		r.mu.RLock()
		stopped := r.stopped
		r.mu.RUnlock()
		if stopped {
			return
		}

		n, srcAddr, err := conn.ReadFromUDP(buffer)
		if err != nil {
			continue
		}

		var msg pb.ProtocolMessage
		if err := proto.Unmarshal(buffer[:n], &msg); err != nil {
			continue
		}

		if msg.SenderId != membership.OwnID {
			// CORREÇÃO CRÍTICA: Entrega inbound via pipeline normal
			messenger.HandleMessage(&msg)
		}
	}
}

func (r *UDPRouter) Close() error {
	r.mu.Lock()
	r.stopped = true
	
	// Fecha PacketConn de envio
	if r.sendPConn != nil {
		r.sendPConn.Close()
	}
	
	// Fecha todos os listeners
	for gid, conn := range r.listeners {
		if group := r.groups[gid]; group != nil && group.pconn != nil {
			group.pconn.Close()
		}
		conn.Close()
	}
	r.mu.Unlock()
	
	return nil
}

// Factory functions
func NewStaticRouter() Router {
	return NewUnicastRouter()
}

func NewUDPMulticastRouter(baseAddr string, basePort int, ifaceAddr ...string) (Router, error) {
	return NewUDPRouter(baseAddr, basePort, ifaceAddr...)
}