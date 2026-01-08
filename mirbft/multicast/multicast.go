package multicast

import (
	"fmt"
	"net"
	"sync"

	"google.golang.org/protobuf/proto"
	"github.com/hyperledger-labs/mirbft/membership"
	"github.com/hyperledger-labs/mirbft/messenger"
	pb "github.com/hyperledger-labs/mirbft/protobufs"
)

type ID = int32
type GroupID uint32

// Router define a interface básica de multicast
type Router interface {
	DefineGroup(g GroupID, members ...ID)
	SendGroup(g GroupID, builder func(dst ID) *pb.ProtocolMessage)
}

// UnicastRouter implementa multicast via loop de unicast (fallback)
type UnicastRouter struct {
	mu     sync.RWMutex
	groups map[GroupID][]ID
}

func NewUnicastRouter() Router {
	r := &UnicastRouter{
		groups: make(map[GroupID][]ID),
	}
	// Grupo "all" padrão
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

// UDPRouter implementa multicast UDP real
type UDPRouter struct {
	mu       sync.RWMutex
	groups   map[GroupID]*udpGroup
	conn     *net.UDPConn
	basePort int
	stopped  bool
}

type udpGroup struct {
	addr    *net.UDPAddr
	members []ID
}

func NewUDPRouter(baseAddr string, basePort int) (Router, error) {
	conn, err := net.ListenUDP("udp", &net.UDPAddr{Port: basePort})
	if err != nil {
		return nil, err
	}

	r := &UDPRouter{
		groups:   make(map[GroupID]*udpGroup),
		conn:     conn,
		basePort: basePort,
	}

	// Grupo "all" via broadcast
	allAddr, _ := net.ResolveUDPAddr("udp", fmt.Sprintf("255.255.255.255:%d", basePort))
	r.groups[GroupID(0)] = &udpGroup{
		addr:    allAddr,
		members: membership.AllNodeIDs(),
	}

	go r.receiveLoop()
	return r, nil
}

func (r *UDPRouter) DefineGroup(g GroupID, members ...ID) {
	r.mu.Lock()
	addr, _ := net.ResolveUDPAddr("udp", fmt.Sprintf("224.0.1.%d:%d", 
		int(g)%255, r.basePort+int(g)))
	r.groups[g] = &udpGroup{
		addr:    addr,
		members: append([]ID{}, members...),
	}
	r.mu.Unlock()
}

func (r *UDPRouter) SendGroup(g GroupID, builder func(dst ID) *pb.ProtocolMessage) {
	r.mu.RLock()
	group := r.groups[g]
	r.mu.RUnlock()

	if group == nil || len(group.members) == 0 {
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

	r.conn.WriteToUDP(data, group.addr)
}

func (r *UDPRouter) receiveLoop() {
	buffer := make([]byte, 64*1024)
	for {
		r.mu.RLock()
		stopped := r.stopped
		r.mu.RUnlock()
		if stopped {
			return
		}

		n, _, err := r.conn.ReadFromUDP(buffer)
		if err != nil {
			continue
		}

		var msg pb.ProtocolMessage
		if err := proto.Unmarshal(buffer[:n], &msg); err != nil {
			continue
		}

		if msg.SenderId != membership.OwnID {
			messenger.HandleMessage(&msg)
		}
	}
}

func (r *UDPRouter) Close() error {
	r.mu.Lock()
	r.stopped = true
	r.mu.Unlock()
	return r.conn.Close()
}

// Factory functions
func NewStaticRouter() Router {
	return NewUnicastRouter()
}

func NewUDPMulticastRouter(baseAddr string, basePort int) (Router, error) {
	return NewUDPRouter(baseAddr, basePort)
}