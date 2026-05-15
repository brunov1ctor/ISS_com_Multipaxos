// MultiPaxosMulticast Orderer - Coordena múltiplos grupos MultiPaxos com atomic multicast.
// Grupo 0 = sequenciador GSN. Grupos 1+ = dados. Cross-group via GSN + META stream.
package orderer

import (
	"fmt"
	"sort"
	"strings"
	"sync"
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

const (
	SYSTEM_META_STREAM = "SYSTEM:META_STREAM:"
	SYSTEM_GSN_REQUEST = "SYSTEM:GSN_REQUEST:"
)

var globalMulticastOrderer *MultiPaxosMulticastOrderer

func GetGlobalMulticastOrderer() *MultiPaxosMulticastOrderer { return globalMulticastOrderer }

type MultiPaxosMulticastOrderer struct {
	groupOrderers  map[uint32]*MultiPaxosOrderer
	orderersMu     sync.RWMutex
	am             *AtomicMulticast
	seq            *Sequencer // Sequenciador dedicado (independente do ISS)
	mgr            manager.Manager
	groupsFilePath string
	missingRequests map[uint64]map[uint32]time.Time
	missingMu       sync.RWMutex
	requestCache    map[uint64]*pb.ClientRequest
	cacheMu         sync.RWMutex
	// CSMR Output Processing: proxy tracks pending requests
	proxyPending   sync.Map // key: "clientId:clientSn" -> int32 (proxyNodeID)
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
	o.missingRequests = make(map[uint64]map[uint32]time.Time)
	o.requestCache = make(map[uint64]*pb.ClientRequest)
	// Load groups
	if o.groupsFilePath == "" { o.groupsFilePath = "/tmp/iss-Bruno/config/groups.yml" }
	if err := o.am.LoadGroupsFromYAML(o.groupsFilePath); err != nil {
		logger.Fatal().Err(err).Msg("Failed to load groups")
	}
	o.am.UpdateSequencerGroup()
	fmt.Printf("[MULTICAST] Groups loaded: %v group0=%v\n", o.am.GetDefinedGroups(), o.am.GetGroupMembers(0))
	o.am.SetSequencer(o)
	// Create standalone sequencer (independent from ISS)
	o.seq = NewSequencer(o.am.GetGroupMembers(0))
	o.seq.Start()
	// Create group orderers for DATA groups only (not group 0)
	for _, gid := range o.am.GetDefinedGroups() {
		if gid == 0 { continue } // Group 0 handled by Sequencer
		ord := &MultiPaxosOrderer{am: o.am, ownedGroupID: gid, skipHandlerRegistration: true, commitNotifyCh: make(chan struct{}, 16)}
		ord.segmentChan = make(chan manager.Segment, 64)
		ord.Init(mngr)
		o.groupOrderers[gid] = ord
	}
	// Setup handlers
	messenger.OrdererMsgHandler = o.HandleMessage
	request.SetGSNGenerator(o.seq.GetNextGSN)
	request.SetGroupMembersGetter(o.GetGroupMembers)
	request.SetRequestReceivedMarker(o.MarkRequestReceived)
	request.SetRequestCacher(o.CacheRequest)
	request.SetRequestPreprocessor(o.PreprocessRequest)
	go o.reforwardWatchdog()
}

func (o *MultiPaxosMulticastOrderer) Start(wg *sync.WaitGroup) {
	// Bootstrap: create initial instances so groups are ready before client sends requests.
	// This runs AFTER messenger.Connect() and discovery.SyncPeer(), so peer connections exist.
	numNodes := int32(len(membership.AllNodeIDs()))
	if numNodes < 1 { numNodes = 1 }
	snLength := int32(config.Config.SegmentLength) * numNodes
	initialSeg := &manager.ContiguousSegment{}
	initialSeg.SetFields(0, membership.AllNodeIDs(), membership.AllNodeIDs(), 0, snLength, -1)
	for gid, ord := range o.groupOrderers {
		members := o.am.GetGroupMembers(gid)
		if members == nil { continue }
		firstSN := int32(gid)
		inst := ord.ensureInstance(firstSN)
		inst.setSegment(initialSeg)
		inst.bucketId = gid
		inst.SetMembers(members)
		ord.dispatcher.store(firstSN, inst)
		inst.startWorkers(&ord.stopWg)
		ord.RunSegmentDirect(initialSeg)
		fmt.Printf("[MULTICAST] Bootstrap: group=%d firstSN=%d members=%v ready\n", gid, firstSN, members)
	}
	fmt.Printf("[MULTICAST] Bootstrap: %d data groups ready (SN 0-%d)\n", len(o.groupOrderers), initialSeg.LastSN())

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
		// GSN_RESPONSE -> Sequencer
		if strings.HasPrefix(payload, "SYSTEM:GSN_RESPONSE:") {
			o.seq.HandleGSNResponse(payload)
			return
		}
		// GSN_REQUEST -> Sequencer (leader processes)
		if strings.HasPrefix(payload, SYSTEM_GSN_REQUEST) {
			o.seq.HandleGSNRequest(payload, pm.SenderId)
			return
		}
		// META_STREAM -> Sequencer
		if strings.HasPrefix(payload, SYSTEM_META_STREAM) {
			req := gsnForward.Req
			o.seq.HandleMETAStream(payload, req.GetTouchedGroups(), req.GetGSN())
			return
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
		// Inject into bucket (data requests)
		req := gsnForward.Req
		if req != nil && req.GetRequestId() != nil {
			if o.am.IsMember(req.GroupId, membership.OwnID) {
				// All forwarded requests bypass Buffer/watermark
				// (watermark is managed by the proxy that received from client)
				request.AddDirectToBucket(req)
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

// Delegate to Sequencer
func (o *MultiPaxosMulticastOrderer) RegisterGSNMetadata(gsn uint64, touchedGroups []uint32) {
	o.seq.RegisterMetadata(gsn, touchedGroups)
}

func (o *MultiPaxosMulticastOrderer) ADeliver(gsn uint64, groupID uint32, _ *pb.Batch) bool {
	return o.seq.ADeliver(gsn, groupID)
}

func (o *MultiPaxosMulticastOrderer) BufferCommit(gsn uint64, groupID uint32, batch *pb.Batch, announce func(int32, *pb.Batch, []byte), sn int32, digest []byte) {
	o.seq.BufferCommit(gsn, groupID, batch, announce, sn, digest)
}

func (o *MultiPaxosMulticastOrderer) sendToGroup(req *pb.ClientRequest, groupID uint32) {
	members := o.am.GetGroupMembers(groupID)
	if len(members) == 0 { return }
	for _, nodeID := range members {
		if nodeID == membership.OwnID {
			// Bypass Buffer/watermark — inject directly into bucket
			request.AddDirectToBucket(req)
			break
		}
	}
	for _, nodeID := range members {
		if nodeID == membership.OwnID { continue }
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
	fmt.Printf("[CSMR][PREPROCESS] client=%d clientSn=%d touchedGroups=%v payload=%.30s\n",
		req.RequestId.ClientId, req.RequestId.ClientSn, req.TouchedGroups, string(req.Payload))
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
	gsn := o.seq.GetNextGSN()
	if gsn == 0 { return true }
	o.seq.PublishMETA(gsn, req.TouchedGroups)
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
