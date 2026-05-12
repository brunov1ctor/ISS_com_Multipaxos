// MultiPaxosMulticast Orderer - Coordena múltiplos grupos MultiPaxos com atomic multicast.
// Grupo 0 = sequenciador GSN. Grupos 1+ = dados. Cross-group via GSN + META stream.
package orderer

import (
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"github.com/hyperledger-labs/mirbft/config"
	"github.com/hyperledger-labs/mirbft/crypto"
	"github.com/hyperledger-labs/mirbft/log"
	"github.com/hyperledger-labs/mirbft/manager"
	"github.com/hyperledger-labs/mirbft/membership"
	"github.com/hyperledger-labs/mirbft/messenger"
	"github.com/hyperledger-labs/mirbft/request"
	"github.com/hyperledger-labs/mirbft/tracing"
	pb "github.com/hyperledger-labs/mirbft/protobufs"
	logger "github.com/rs/zerolog/log"
)

const gsnStateFile = "/tmp/iss-Bruno/next_gsn.state"
const (
	SYSTEM_META_STREAM = "SYSTEM:META_STREAM:"
	SYSTEM_GSN_REQUEST = "SYSTEM:GSN_REQUEST:"
)

var globalMulticastOrderer *MultiPaxosMulticastOrderer

func GetGlobalMulticastOrderer() *MultiPaxosMulticastOrderer { return globalMulticastOrderer }

type MultiPaxosMulticastOrderer struct {
	groupOrderers      map[uint32]*MultiPaxosOrderer
	orderersMu         sync.RWMutex
	am                 *AtomicMulticast
	mgr                manager.Manager
	groupsFilePath     string
	nextGSN            uint64
	gsnMu              sync.Mutex
	gsnRequestsPending map[uint64]chan uint64
	gsnReqMu           sync.Mutex
	gsnSeqCounter      uint32
	metaSeqCounter     uint32
	gsnMetadata        map[uint64][]uint32
	metaMu             sync.RWMutex
	lastDeliveredGSN   map[uint32]uint64
	expectedGSNMu      sync.RWMutex
	pendingCommits     map[uint32]map[uint64]*PendingCommit
	bufferMu           sync.RWMutex
	missingRequests    map[uint64]map[uint32]time.Time
	missingMu          sync.RWMutex
	requestCache       map[uint64]*pb.ClientRequest
	cacheMu            sync.RWMutex
	publishedMeta      map[uint64]bool
	publishedMu        sync.RWMutex
	// CSMR Output Processing: proxy tracks pending requests
	proxyPending       sync.Map // key: "clientId:clientSn" -> int32 (proxyNodeID)
}

type PendingCommit struct {
	gsn      uint64
	groupID  uint32
	batch    *pb.Batch
	announce func(int32, *pb.Batch, []byte)
	sn       int32
	digest   []byte
}

func (o *MultiPaxosMulticastOrderer) Init(mngr manager.Manager) {
	globalMulticastOrderer = o
	o.mgr = mngr
	// Init components
	o.am = NewAtomicMulticast()
	fmt.Printf("[MULTICAST] NewAtomicMulticast: group0 members=%v\n", o.am.GetGroupMembers(0))
	o.groupOrderers = make(map[uint32]*MultiPaxosOrderer)
	o.lastDeliveredGSN = make(map[uint32]uint64)
	o.pendingCommits = make(map[uint32]map[uint64]*PendingCommit)
	o.gsnMetadata = make(map[uint64][]uint32)
	o.gsnRequestsPending = make(map[uint64]chan uint64)
	o.nextGSN = 1
	o.missingRequests = make(map[uint64]map[uint32]time.Time)
	o.requestCache = make(map[uint64]*pb.ClientRequest)
	o.publishedMeta = make(map[uint64]bool)
	// Load groups
	if o.groupsFilePath == "" { o.groupsFilePath = "/tmp/iss-Bruno/config/groups.yml" }
	if err := o.am.LoadGroupsFromYAML(o.groupsFilePath); err != nil {
		logger.Fatal().Err(err).Msg("Failed to load groups")
	}
	o.am.UpdateSequencerGroup()
	fmt.Printf("[MULTICAST] Groups loaded: %v group0=%v\n", o.am.GetDefinedGroups(), o.am.GetGroupMembers(0))
	o.am.SetSequencer(o)
	// Create group orderers
	for _, gid := range o.am.GetDefinedGroups() {
		ord := &MultiPaxosOrderer{am: o.am, ownedGroupID: gid, skipHandlerRegistration: true}
		ord.segmentChan = make(chan manager.Segment, 64)
		ord.Init(mngr)
		if gid == 0 { ord.proposeEvery = 1 * time.Millisecond }
		o.groupOrderers[gid] = ord
	}
	// Setup handlers
	messenger.OrdererMsgHandler = o.HandleMessage
	request.SetGSNGenerator(o.GetNextGSN)
	request.SetGroupMembersGetter(o.GetGroupMembers)
	request.SetRequestReceivedMarker(o.MarkRequestReceived)
	request.SetRequestCacher(o.CacheRequest)
	request.SetRequestPreprocessor(o.PreprocessRequest)
	o.loadNextGSN()
	go o.reforwardWatchdog()
}

func (o *MultiPaxosMulticastOrderer) Start(wg *sync.WaitGroup) {
	segCh := o.mgr.SubscribeOrderer()
	go func() {
		for seg := range segCh {
			o.orderersMu.RLock()
			for _, ord := range o.groupOrderers {
				if ord != nil && ord.segmentChan != nil {
					select {
					case ord.segmentChan <- seg:
					default:
					}
				}
			}
			o.orderersMu.RUnlock()
		}
	}()
	o.orderersMu.RLock()
	defer o.orderersMu.RUnlock()
	for _, orderer := range o.groupOrderers { orderer.Start(wg) }
}

func (o *MultiPaxosMulticastOrderer) HandleMessage(pm *pb.ProtocolMessage) {
	if gsnForward := pm.GetGsnReqForward(); gsnForward != nil {
		payload := string(gsnForward.Req.Payload)
		// GSN_RESPONSE fast-path
		if strings.HasPrefix(payload, "SYSTEM:GSN_RESPONSE:") {
			var reqID, gsn uint64
			if n, _ := fmt.Sscanf(payload, "SYSTEM:GSN_RESPONSE:%d:%d", &reqID, &gsn); n == 2 {
				o.gsnReqMu.Lock()
				if ch, exists := o.gsnRequestsPending[reqID]; exists {
					select { case ch <- gsn: default: }
				}
				o.gsnReqMu.Unlock()
				return
			}
		}
		// CSMR Output Processing: COMMIT_NOTIFY from group replica back to proxy
		if strings.HasPrefix(payload, "SYSTEM:COMMIT_NOTIFY:") {
			var clientId, clientSn, orderSn int32
			if n, _ := fmt.Sscanf(payload, "SYSTEM:COMMIT_NOTIFY:%d:%d:%d", &clientId, &clientSn, &orderSn); n == 3 {
				fmt.Printf("[CSMR][COMMIT_NOTIFY-RECV] from=%d client=%d clientSn=%d orderSn=%d\n", pm.SenderId, clientId, clientSn, orderSn)
				o.handleCommitNotify(clientId, clientSn, orderSn)
			}
			return
		}
		// Inject into bucket
		req := gsnForward.Req
		if req != nil && req.GetRequestId() != nil {
			if req.GroupId == 0 || o.am.IsMember(req.GroupId, membership.OwnID) {
				if strings.HasPrefix(string(req.Payload), SYSTEM_GSN_REQUEST) || strings.HasPrefix(string(req.Payload), SYSTEM_META_STREAM) {
					request.AddSystemMessage(req)
				} else {
					request.AddReqMsg(req)
				}
			}
			if req.GSN > 0 && req.GroupId > 0 { o.MarkRequestReceived(req.GSN, req.GroupId) }
		}
		return
	}
	if me := pm.GetMissingEntry(); me != nil {
		var groupID uint32
		if me.Batch != nil && len(me.Batch.Requests) > 0 {
			groupID = me.Batch.Requests[0].GetGroupId()
		} else {
			o.orderersMu.RLock()
			for _, ord := range o.groupOrderers { ord.HandleMessage(pm) }
			o.orderersMu.RUnlock()
			return
		}
		o.orderersMu.RLock()
		if ord := o.groupOrderers[groupID]; ord != nil { ord.HandleMessage(pm) }
		o.orderersMu.RUnlock()
		return
	}
	mpx := pm.GetMultipaxos()
	if mpx == nil {
		o.orderersMu.RLock()
		for _, ord := range o.groupOrderers { ord.HandleMessage(pm) }
		o.orderersMu.RUnlock()
		return
	}
	groupID := extractGroupID(mpx)
	o.orderersMu.RLock()
	if ord := o.groupOrderers[groupID]; ord != nil { ord.HandleMessage(pm) }
	o.orderersMu.RUnlock()
}

func (o *MultiPaxosMulticastOrderer) IsMember(groupID uint32, nodeID int32) bool {
	return o.am != nil && o.am.IsMember(groupID, nodeID)
}

func (o *MultiPaxosMulticastOrderer) GetGroupMembers(groupID uint32) []int32 {
	if o.am == nil { return nil }
	return o.am.GetGroupMembers(groupID)
}

func makeGlobalRequestID(nodeID int32, localCounter uint32) uint64 {
	return uint64(nodeID)<<32 | uint64(localCounter)
}

func (o *MultiPaxosMulticastOrderer) GetNextGSN() uint64 {
	clientSn := atomic.AddUint32(&o.gsnSeqCounter, 1)
	reqID := makeGlobalRequestID(membership.OwnID, clientSn)
	respChan := make(chan uint64, 1)
	o.gsnReqMu.Lock()
	o.gsnRequestsPending[reqID] = respChan
	o.gsnReqMu.Unlock()

	o.sendToGroup(&pb.ClientRequest{
		RequestId: &pb.RequestID{ClientId: membership.OwnID, ClientSn: int32(clientSn)},
		Payload: []byte(fmt.Sprintf("%s%d:%d", SYSTEM_GSN_REQUEST, reqID, membership.OwnID)),
		GroupId: 0, TouchedGroups: []uint32{0},
	}, 0)

	select {
	case gsn := <-respChan:
		o.gsnReqMu.Lock(); delete(o.gsnRequestsPending, reqID); o.gsnReqMu.Unlock()
		return gsn
	case <-time.After(10 * time.Second):
		o.gsnReqMu.Lock(); delete(o.gsnRequestsPending, reqID); o.gsnReqMu.Unlock()
		return 0
	}
}

func (o *MultiPaxosMulticastOrderer) ADeliver(gsn uint64, groupID uint32, _ *pb.Batch) bool {
	o.expectedGSNMu.Lock()
	defer o.expectedGSNMu.Unlock()
	lastDelivered := o.lastDeliveredGSN[groupID]
	if gsn <= lastDelivered { return true }

	nextCandidate := lastDelivered + 1
	for nextCandidate < gsn {
		o.metaMu.RLock()
		_, metaExists := o.gsnMetadata[nextCandidate]
		touches := metaExists && o.gsnTouchesGroup(nextCandidate, groupID)
		o.metaMu.RUnlock()
		if !metaExists { return false }
		if touches { return false }
		nextCandidate++
	}

	o.metaMu.RLock()
	_, metaExists := o.gsnMetadata[gsn]
	touches := metaExists && o.gsnTouchesGroup(gsn, groupID)
	o.metaMu.RUnlock()
	if !metaExists { return false }
	if !touches { return true }
	o.lastDeliveredGSN[groupID] = gsn
	return true
}

func (o *MultiPaxosMulticastOrderer) BufferCommit(gsn uint64, groupID uint32, batch *pb.Batch, announce func(int32, *pb.Batch, []byte), sn int32, digest []byte) {
	o.bufferMu.Lock()
	defer o.bufferMu.Unlock()
	if o.pendingCommits[groupID] == nil { o.pendingCommits[groupID] = make(map[uint64]*PendingCommit) }
	o.pendingCommits[groupID][gsn] = &PendingCommit{gsn: gsn, groupID: groupID, batch: batch, announce: announce, sn: sn, digest: digest}
}

func (o *MultiPaxosMulticastOrderer) drainBuffer(groupID uint32) {
	o.bufferMu.Lock()
	defer o.bufferMu.Unlock()
	if o.pendingCommits[groupID] == nil { return }
	for {
		o.expectedGSNMu.RLock()
		lastDelivered := o.lastDeliveredGSN[groupID]
		o.expectedGSNMu.RUnlock()
		nextCandidate := lastDelivered + 1
		for {
			o.metaMu.RLock()
			_, metaExists := o.gsnMetadata[nextCandidate]
			touches := metaExists && o.gsnTouchesGroup(nextCandidate, groupID)
			o.metaMu.RUnlock()
			if !metaExists { return }
			if touches { break }
			nextCandidate++
		}
		pending, exists := o.pendingCommits[groupID][nextCandidate]
		if !exists { return }
		pending.announce(pending.sn, pending.batch, pending.digest)
		o.expectedGSNMu.Lock()
		o.lastDeliveredGSN[groupID] = nextCandidate
		o.expectedGSNMu.Unlock()
		delete(o.pendingCommits[groupID], nextCandidate)
	}
}

func (o *MultiPaxosMulticastOrderer) RegisterGSNMetadata(gsn uint64, touchedGroups []uint32) {
	o.metaMu.Lock()
	if len(touchedGroups) == 0 { o.metaMu.Unlock(); return }
	if _, exists := o.gsnMetadata[gsn]; exists { o.metaMu.Unlock(); return }
	o.gsnMetadata[gsn] = make([]uint32, len(touchedGroups))
	copy(o.gsnMetadata[gsn], touchedGroups)
	o.metaMu.Unlock()

	o.missingMu.Lock()
	if o.missingRequests[gsn] == nil { o.missingRequests[gsn] = make(map[uint32]time.Time) }
	for _, gid := range touchedGroups {
		if gid > 0 { o.missingRequests[gsn][gid] = time.Now() }
	}
	o.missingMu.Unlock()

	for _, gid := range touchedGroups {
		o.bufferMu.RLock()
		_, exists := o.pendingCommits[gid][gsn]
		o.bufferMu.RUnlock()
		if exists { o.drainBuffer(gid) }
	}
}

func (o *MultiPaxosMulticastOrderer) gsnTouchesGroup(gsn uint64, groupID uint32) bool {
	touched, exists := o.gsnMetadata[gsn]
	if !exists { return false }
	for _, g := range touched { if g == groupID { return true } }
	return false
}

func (o *MultiPaxosMulticastOrderer) PublishGSNMetadata(gsn uint64, touchedGroups []uint32) {
	o.publishedMu.Lock()
	if o.publishedMeta[gsn] { o.publishedMu.Unlock(); return }
	o.publishedMeta[gsn] = true
	o.publishedMu.Unlock()
	if len(touchedGroups) == 0 { return }

	metaSn := atomic.AddUint32(&o.metaSeqCounter, 1)
	o.sendToGroup(&pb.ClientRequest{
		RequestId: &pb.RequestID{ClientId: membership.OwnID, ClientSn: int32(metaSn)},
		Payload: []byte(fmt.Sprintf("%s%d", SYSTEM_META_STREAM, gsn)),
		GroupId: 0, TouchedGroups: touchedGroups, GSN: gsn,
	}, 0)
}

func (o *MultiPaxosMulticastOrderer) sendToGroup(req *pb.ClientRequest, groupID uint32) {
	members := o.am.GetGroupMembers(groupID)
	if len(members) == 0 { return }
	for _, nodeID := range members {
		if nodeID == membership.OwnID {
			if strings.HasPrefix(string(req.Payload), SYSTEM_GSN_REQUEST) || strings.HasPrefix(string(req.Payload), SYSTEM_META_STREAM) {
				request.AddSystemMessage(req)
			} else {
				request.AddReqMsg(req)
			}
			break
		}
	}
	for _, nodeID := range members {
		messenger.EnqueueMsg(&pb.ProtocolMessage{SenderId: membership.OwnID, Sn: -1,
			Msg: &pb.ProtocolMessage_GsnReqForward{GsnReqForward: &pb.GSNReqForward{Req: req}}}, nodeID)
	}
}

func (o *MultiPaxosMulticastOrderer) sendToGroupExceptSelf(req *pb.ClientRequest, groupID uint32) {
	for _, nodeID := range o.am.GetGroupMembers(groupID) {
		if nodeID == membership.OwnID { continue }
		messenger.EnqueueMsg(&pb.ProtocolMessage{SenderId: membership.OwnID, Sn: -1,
			Msg: &pb.ProtocolMessage_GsnReqForward{GsnReqForward: &pb.GSNReqForward{Req: req}}}, nodeID)
	}
}

func (o *MultiPaxosMulticastOrderer) MarkRequestReceived(gsn uint64, groupID uint32) {
	o.missingMu.Lock()
	defer o.missingMu.Unlock()
	if groups, exists := o.missingRequests[gsn]; exists {
		delete(groups, groupID)
		if len(groups) == 0 { delete(o.missingRequests, gsn) }
	}
}

func (o *MultiPaxosMulticastOrderer) CacheRequest(gsn uint64, req *pb.ClientRequest) {
	o.cacheMu.Lock()
	defer o.cacheMu.Unlock()
	if _, exists := o.requestCache[gsn]; !exists { o.requestCache[gsn] = req }
}

// CSMR Output Processing: handle COMMIT_NOTIFY from group replicas
func (o *MultiPaxosMulticastOrderer) handleCommitNotify(clientId, clientSn, orderSn int32) {
	key := fmt.Sprintf("%d:%d", clientId, clientSn)
	if _, loaded := o.proxyPending.LoadAndDelete(key); loaded {
		fmt.Printf("[CSMR][PROXY-RESPOND] client=%d clientSn=%d orderSn=%d\n", clientId, clientSn, orderSn)
		messenger.RespondToClient(clientId, &pb.ClientResponse{
			OrderSn:  orderSn,
			ClientSn: clientSn,
		})
		tracing.MainTrace.Event(tracing.RESP_SEND, int64(clientId), int64(clientSn))
	} else {
		fmt.Printf("[CSMR][COMMIT_NOTIFY-IGNORE] key=%s not in proxyPending (already responded or not proxy)\n", key)
	}
}

// CSMR Output Processing: called by group orderer after commit to notify proxy
func (o *MultiPaxosMulticastOrderer) NotifyProxy(batch *pb.Batch, sn int32) {
	if batch == nil { return }
	for _, req := range batch.Requests {
		if req == nil || req.RequestId == nil { continue }
		// Skip system messages
		if strings.HasPrefix(string(req.Payload), "SYSTEM:") { continue }
		clientId := req.RequestId.ClientId
		clientSn := req.RequestId.ClientSn
		// Check if WE are the proxy for this request
		key := fmt.Sprintf("%d:%d", clientId, clientSn)
		if _, isProxy := o.proxyPending.Load(key); isProxy {
			// We are the proxy — respond directly
			fmt.Printf("[CSMR][NOTIFY-LOCAL] sn=%d client=%d clientSn=%d (I am proxy)\n", sn, clientId, clientSn)
			o.handleCommitNotify(clientId, clientSn, sn)
		} else {
			// We are NOT the proxy — send COMMIT_NOTIFY to all peers
			fmt.Printf("[CSMR][NOTIFY-REMOTE] sn=%d client=%d clientSn=%d (sending to peers)\n", sn, clientId, clientSn)
			notifyPayload := fmt.Sprintf("SYSTEM:COMMIT_NOTIFY:%d:%d:%d", clientId, clientSn, sn)
			for _, nodeID := range membership.AllNodeIDs() {
				if nodeID == membership.OwnID { continue }
				messenger.EnqueueMsg(&pb.ProtocolMessage{
					SenderId: membership.OwnID, Sn: -1,
					Msg: &pb.ProtocolMessage_GsnReqForward{GsnReqForward: &pb.GSNReqForward{
						Req: &pb.ClientRequest{
							RequestId: &pb.RequestID{ClientId: clientId, ClientSn: clientSn},
							Payload:   []byte(notifyPayload),
						},
					}},
				}, nodeID)
			}
		}
	}
}

func (o *MultiPaxosMulticastOrderer) reforwardWatchdog() {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for range ticker.C {
		o.missingMu.RLock()
		now := time.Now()
		toReforward := make(map[uint64][]uint32)
		for gsn, groups := range o.missingRequests {
			for gid, ts := range groups {
				if now.Sub(ts) > 60*time.Second { toReforward[gsn] = append(toReforward[gsn], gid) }
			}
		}
		o.missingMu.RUnlock()
		for gsn, groups := range toReforward {
			o.cacheMu.RLock()
			req, exists := o.requestCache[gsn]
			o.cacheMu.RUnlock()
			if !exists { continue }
			for _, gid := range groups {
				o.sendToGroup(&pb.ClientRequest{
					RequestId: req.RequestId, Payload: req.Payload, Signature: req.Signature,
					Pubkey: req.Pubkey, GroupId: gid, TouchedGroups: req.TouchedGroups, GSN: gsn,
				}, gid)
				o.missingMu.Lock()
				if o.missingRequests[gsn] != nil { o.missingRequests[gsn][gid] = time.Now() }
				o.missingMu.Unlock()
			}
		}
	}
}

func (o *MultiPaxosMulticastOrderer) Sign(data []byte) ([]byte, error) {
	if membership.OwnPrivKey == nil { return nil, fmt.Errorf("private key not initialized") }
	return crypto.Sign(data, membership.OwnPrivKey)
}

func (o *MultiPaxosMulticastOrderer) CheckSig(data []byte, senderID int32, signature []byte) error {
	if !config.Config.SignRequests { return nil }
	ni := membership.NodeIdentity(senderID)
	if ni == nil || ni.PubKey == nil { return fmt.Errorf("public key not found for node %d", senderID) }
	pubKey, err := crypto.PublicKeyFromBytes(ni.PubKey)
	if err != nil { return err }
	return crypto.CheckSig(data, pubKey, signature)
}

func (o *MultiPaxosMulticastOrderer) HandleEntry(entry *log.Entry) {
	if entry == nil { return }
	var groupID uint32
	if entry.Batch != nil && len(entry.Batch.Requests) > 0 { groupID = entry.Batch.Requests[0].GetGroupId() }
	o.orderersMu.RLock()
	if ord := o.groupOrderers[groupID]; ord != nil { ord.HandleEntry(entry) }
	o.orderersMu.RUnlock()
}

func (o *MultiPaxosMulticastOrderer) loadNextGSN() {
	b, err := os.ReadFile(gsnStateFile)
	if err == nil {
		if v, err2 := strconv.ParseUint(strings.TrimSpace(string(b)), 10, 64); err2 == nil && v > 0 {
			o.nextGSN = v; return
		}
	}
}

func (o *MultiPaxosMulticastOrderer) persistNextGSN() {
	_ = os.WriteFile(gsnStateFile, []byte(fmt.Sprintf("%d\n", o.nextGSN)), 0644)
}

func (o *MultiPaxosMulticastOrderer) PreprocessRequest(req *pb.ClientRequest) bool {
	// System messages bypass preprocessor
	if strings.HasPrefix(string(req.Payload), SYSTEM_GSN_REQUEST) || strings.HasPrefix(string(req.Payload), SYSTEM_META_STREAM) {
		return request.AddSystemMessage(req)
	}
	// COMMIT_NOTIFY bypass
	if strings.HasPrefix(string(req.Payload), "SYSTEM:COMMIT_NOTIFY:") { return true }
	if req.GetRequestId() == nil { return true }

	// Already preprocessed
	if req.GroupId > 0 && req.GSN > 0 { return false }
	if req.GroupId > 0 && len(req.TouchedGroups) == 1 { return false }
	if req.GroupId > 0 && len(req.TouchedGroups) > 1 && req.GSN == 0 { return false }

	// Block other SYSTEM messages
	if strings.HasPrefix(string(req.Payload), "SYSTEM:") { return true }

	// CSMR: Register this node as proxy for this request (Output Processing)
	key := fmt.Sprintf("%d:%d", req.RequestId.ClientId, req.RequestId.ClientSn)
	o.proxyPending.Store(key, membership.OwnID)
	fmt.Printf("[CSMR][PROXY-REG] client=%d clientSn=%d registered as proxy\n", req.RequestId.ClientId, req.RequestId.ClientSn)

	// Map to groups via ReplicaMapper
	if len(req.TouchedGroups) == 0 {
		req.TouchedGroups = request.ReplicaMapper(req.Payload)
		sort.Slice(req.TouchedGroups, func(i, j int) bool { return req.TouchedGroups[i] < req.TouchedGroups[j] })
	}
	// Remove group 0
	filtered := req.TouchedGroups[:0]
	for _, g := range req.TouchedGroups { if g != 0 { filtered = append(filtered, g) } }
	req.TouchedGroups = filtered
	if len(req.TouchedGroups) == 0 { return true }

	// Single-group
	if len(req.TouchedGroups) == 1 {
		req.GroupId = req.TouchedGroups[0]
		if o.am.IsMember(req.GroupId, membership.OwnID) {
			o.sendToGroupExceptSelf(req, req.GroupId)
			return false
		}
		o.sendToGroup(req, req.GroupId)
		return true
	}

	// Cross-group: get GSN + publish META + fanout
	gsn := o.GetNextGSN()
	if gsn == 0 { return true }
	o.PublishGSNMetadata(gsn, req.TouchedGroups)
	o.CacheRequest(gsn, &pb.ClientRequest{
		RequestId: req.RequestId, Payload: req.Payload, Signature: req.Signature,
		Pubkey: req.Pubkey, TouchedGroups: req.TouchedGroups, GSN: gsn,
	})
	for _, groupID := range req.TouchedGroups {
		o.sendToGroup(&pb.ClientRequest{
			RequestId: req.RequestId, Payload: req.Payload, Signature: req.Signature,
			Pubkey: req.Pubkey, GroupId: groupID, TouchedGroups: req.TouchedGroups, GSN: gsn,
		}, groupID)
	}
	return true
}
