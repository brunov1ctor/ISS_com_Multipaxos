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

// ===== TUNING: increase UDP socket buffers to avoid kernel drops =====
const udpBufBytes = 32 * 1024 * 1024 // 32MB

func tuneUDPConn(conn *net.UDPConn) {
	if conn == nil {
		return
	}
	_ = conn.SetReadBuffer(udpBufBytes)
	_ = conn.SetWriteBuffer(udpBufBytes)
}

// ===== Option A: create enough UDP multicast groups (match gid space) =====
// IMPORTANT: Keep this aligned with the orderer (e.g., MultiPaxosMulticast uses up to 8).
const maxGroupIDSpace = 8

// UDPRouter implementa multicast UDP real com join correto e identificação de grupo
type UDPRouter struct {
	mu sync.RWMutex

	// logical group membership (what the protocol wants per gid)
	logicalMembers map[GroupID][]ID

	// physical UDP groups (what we actually joined)
	udpGroups map[GroupID]*udpGroup

	listeners map[GroupID]*net.UDPConn
	sendPConn  *ipv4.PacketConn // PacketConn para envio (iface/TTL)

	baseAddr string
	basePort int
	iface    *net.Interface

	stopped bool
}

type udpGroup struct {
	addr  *net.UDPAddr
	conn  *net.UDPConn
	pconn *ipv4.PacketConn
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
								ii := i
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
							if ipnet.IP.To4() != nil {
								ii := i
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
		logicalMembers: make(map[GroupID][]ID),
		udpGroups:      make(map[GroupID]*udpGroup),
		listeners:      make(map[GroupID]*net.UDPConn),
		sendPConn:      nil,
		baseAddr:       baseAddr,
		basePort:       basePort,
		iface:          iface,
	}

	// Grupo "all" via multicast (não broadcast) - usa porta base
	allAddr, _ := net.ResolveUDPAddr("udp4", fmt.Sprintf("%s.0:%d", baseAddr, basePort))
	allConn, err := net.ListenMulticastUDP("udp4", iface, allAddr)
	if err != nil {
		return nil, err
	}
	tuneUDPConn(allConn)

	// PacketConn para envio (com interface/TTL)
	r.sendPConn = ipv4.NewPacketConn(allConn)
	if iface != nil {
		_ = r.sendPConn.SetMulticastInterface(iface)
	}
	_ = r.sendPConn.SetMulticastTTL(2)

	r.udpGroups[GroupID(0)] = &udpGroup{
		addr:  allAddr,
		conn:  allConn,
		pconn: ipv4.NewPacketConn(allConn),
	}
	r.listeners[GroupID(0)] = allConn

	// logical "all"
	r.logicalMembers[GroupID(0)] = membership.AllNodeIDs()

	go r.receiveLoop(GroupID(0), allConn)

	// === Opção A: pré-criar SEMPRE 1..maxGroupIDSpace (8) ===
	for gid := 1; gid <= maxGroupIDSpace; gid++ {
		port := basePort + gid
		addr, _ := net.ResolveUDPAddr("udp4", fmt.Sprintf("%s.%d:%d", baseAddr, gid, port))

		conn, err := net.ListenMulticastUDP("udp4", iface, addr)
		if err != nil {
			fmt.Printf("[UDP-MC] Failed to pre-create group %d: %v\n", gid, err)
			continue
		}
		tuneUDPConn(conn)

		pconn := ipv4.NewPacketConn(conn)
		r.udpGroups[GroupID(gid)] = &udpGroup{
			addr:  addr,
			conn:  conn,
			pconn: pconn,
		}
		r.listeners[GroupID(gid)] = conn

		go r.receiveLoop(GroupID(gid), conn)
		fmt.Printf("[UDP-MC] Pre-created group %d on %v\n", gid, addr)
	}

	fmt.Printf("[UDP-MC] Initialized with %d pre-created groups, interface: %v\n", maxGroupIDSpace, iface)
	return r, nil
}

// NewUDPRouter: alias de compatibilidade (se algum lugar ainda chama esse nome).
func NewUDPRouter(baseAddr string, basePort int, ifaceAddr ...string) (Router, error) {
	return NewUDPMulticastRouter(baseAddr, basePort, ifaceAddr...)
}

// mapLogicalToPhysical maps any logical gid to an existing UDP group.
// If gid is within 1..maxGroupIDSpace, it's a 1:1 mapping.
// If gid is outside, we wrap into 1..maxGroupIDSpace (never to 0).
func mapLogicalToPhysical(g GroupID) GroupID {
	if g == 0 {
		return 0
	}
	if g <= GroupID(maxGroupIDSpace) {
		return g
	}
	// wrap into [1..maxGroupIDSpace]
	return GroupID(1 + (uint32(g-1) % uint32(maxGroupIDSpace)))
}

func (r *UDPRouter) DefineGroup(g GroupID, members ...ID) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.logicalMembers[g] = append([]ID{}, members...)

	pg := mapLogicalToPhysical(g)
	if pg == 0 {
		// "all" is fixed
		return
	}

	if _, ok := r.udpGroups[pg]; ok {
		fmt.Printf("[UDP-MC] DefineGroup logical=%d -> physical=%d members=%d\n", g, pg, len(members))
		return
	}

	// If somehow physical group is missing, fallback to 0 (should not happen with precreate 1..8)
	fmt.Printf("[UDP-MC] DefineGroup logical=%d -> physical missing, fallback to 0 members=%d\n", g, len(members))
}

func (r *UDPRouter) SendGroup(g GroupID, builder func(dst ID) *pb.ProtocolMessage) {
	r.mu.RLock()
	sendPConn := r.sendPConn
	members := r.logicalMembers[g]
	pg := mapLogicalToPhysical(g)
	group := r.udpGroups[pg]
	r.mu.RUnlock()

	if sendPConn == nil || group == nil || len(members) == 0 {
		return
	}

	// Build a representative message (dst not used for multicast)
	msg := builder(members[0])
	if msg == nil {
		return
	}

	data, err := proto.Marshal(msg)
	if err != nil {
		return
	}

	_, _ = sendPConn.WriteTo(data, nil, group.addr)
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

		n, _, err := conn.ReadFromUDP(buffer)
		if err != nil {
			continue
		}

		var msg pb.ProtocolMessage
		if err := proto.Unmarshal(buffer[:n], &msg); err != nil {
			continue
		}

		// Avoid processing our own multicast sends
		if msg.SenderId != membership.OwnID {
			messenger.HandleMessage(&msg)
		}
	}
}

func (r *UDPRouter) Close() error {
	r.mu.Lock()
	r.stopped = true

	if r.sendPConn != nil {
		_ = r.sendPConn.Close()
	}

	for gid, conn := range r.listeners {
		if group := r.udpGroups[gid]; group != nil && group.pconn != nil {
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

