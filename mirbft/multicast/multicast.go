package multicast

import (
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

// ===== MINIMAL FIX: increase UDP socket buffers to avoid kernel drops =====
const udpBufBytes = 32 * 1024 * 1024 // 32MB

func tuneUDPConn(conn *net.UDPConn) {
	if conn == nil {
		return
	}
	_ = conn.SetReadBuffer(udpBufBytes)
	_ = conn.SetWriteBuffer(udpBufBytes)
}

// UDPRouter implementa multicast UDP real com join correto e identificação de grupo
type UDPRouter struct {
	mu        sync.RWMutex
	groups    map[GroupID]*udpGroup
	addrToGID map[string]GroupID       // mapeia IP:porta -> GroupID
	listeners map[GroupID]*net.UDPConn // um listener por grupo
	sendPConn *ipv4.PacketConn         // PacketConn para envio com interface/TTL
	baseAddr  string
	basePort  int
	iface     *net.Interface // interface de rede configurada
	stopped   bool
}

type udpGroup struct {
	addr    *net.UDPAddr
	members []ID
	conn    *net.UDPConn
	pconn   *ipv4.PacketConn
}

// NewUDPMulticastRouter é a implementação principal (única) do roteador UDP multicast.
func NewUDPMulticastRouter(baseAddr string, basePort int, ifaceAddr ...string) (Router, error) {
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
								ii := i // evita capturar o range var
								iface = &ii
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
								ii := i // evita capturar o range var
								iface = &ii
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
	// MINIMAL FIX: tune socket buffers
	tuneUDPConn(allConn)

	// Cria PacketConn para envio com interface/TTL configurados
	r.sendPConn = ipv4.NewPacketConn(allConn)
	if iface != nil {
		_ = r.sendPConn.SetMulticastInterface(iface)
	}
	_ = r.sendPConn.SetMulticastTTL(2) // TTL baixo para experimentos locais

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
	maxGroups := numNodes
	if maxGroups > 8 {
		maxGroups = 8 // limite absoluto para Emulab
	}

	for gid := 1; gid <= maxGroups; gid++ {
		port := basePort + gid
		addr, _ := net.ResolveUDPAddr("udp4", fmt.Sprintf("%s.%d:%d", baseAddr, gid, port))

		conn, err := net.ListenMulticastUDP("udp4", iface, addr)
		if err != nil {
			fmt.Printf("[UDP-MC] Failed to pre-create group %d: %v\n", gid, err)
			continue
		}
		// MINIMAL FIX: tune socket buffers
		tuneUDPConn(conn)

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

		go r.receiveLoop(GroupID(gid), conn)
		fmt.Printf("[UDP-MC] Pre-created group %d on %v\n", gid, addr)
	}

	fmt.Printf("[UDP-MC] Initialized with %d pre-created groups, interface: %v\n", maxGroups, iface)
	return r, nil
}

// NewUDPRouter: alias de compatibilidade (se algum lugar ainda chama esse nome).
func NewUDPRouter(baseAddr string, basePort int, ifaceAddr ...string) (Router, error) {
	return NewUDPMulticastRouter(baseAddr, basePort, ifaceAddr...)
}

func (r *UDPRouter) DefineGroup(g GroupID, members ...ID) {
	r.mu.Lock()
	defer r.mu.Unlock()

	// Usa grupos pré-criados - mapeia gid para grupo existente
	if group, exists := r.groups[g]; exists {
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
	_, _ = sendPConn.WriteTo(data, nil, group.addr)
}

func (r *UDPRouter) receiveLoop(gid GroupID, conn *net.UDPConn) {
	_ = gid // se quiser usar no log depois; por ora evita “unused” se você remover prints
	buffer := make([]byte, 64*1024)

	for {
		r.mu.RLock()
		stopped := r.stopped
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

		if msg.SenderId != membership.OwnID {
			// Entrega inbound via pipeline normal
			messenger.HandleMessage(&msg)
		}
	}
}

func (r *UDPRouter) Close() error {
	r.mu.Lock()
	r.stopped = true

	// Fecha PacketConn de envio
	if r.sendPConn != nil {
		_ = r.sendPConn.Close()
	}

	// Fecha todos os listeners
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

