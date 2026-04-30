// MultiPaxos Orderer - Gerencia consenso MultiPaxos para um grupo.
// Standalone (broadcast) ou Multicast child (um grupo específico).
package orderer

import (
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"google.golang.org/protobuf/proto"
	"github.com/hyperledger-labs/mirbft/announcer"
	"github.com/hyperledger-labs/mirbft/config"
	"github.com/hyperledger-labs/mirbft/crypto"
	mirlog "github.com/hyperledger-labs/mirbft/log"
	"github.com/hyperledger-labs/mirbft/manager"
	"github.com/hyperledger-labs/mirbft/membership"
	"github.com/hyperledger-labs/mirbft/messenger"
	pb "github.com/hyperledger-labs/mirbft/protobufs"
	logger "github.com/rs/zerolog/log"
)

type AnnounceFn func(sn int32, batch *pb.Batch, metadata []byte)

type mpxDispatcher struct { mm sync.Map }
func (d *mpxDispatcher) load(sn int32) (*mpxInstance, bool) {
	if v, ok := d.mm.Load(sn); ok { return v.(*mpxInstance), true }
	return nil, false
}
func (d *mpxDispatcher) store(sn int32, inst *mpxInstance) { d.mm.Store(sn, inst) }

type mpxBacklog struct {
	mu sync.Mutex
	qs map[int32][]*pb.ProtocolMessage
}
func newMPXBacklog() mpxBacklog { return mpxBacklog{qs: make(map[int32][]*pb.ProtocolMessage)} }
func (b *mpxBacklog) drainTo(sn int32, f func(*pb.ProtocolMessage)) {
	b.mu.Lock()
	items := b.qs[sn]; delete(b.qs, sn)
	b.mu.Unlock()
	for _, m := range items { f(m) }
}

type MultiPaxosOrderer struct {
	mgr            manager.Manager
	segmentChan    chan manager.Segment
	dispatcher     mpxDispatcher
	backlog        mpxBacklog
	last           int32
	instMu         sync.RWMutex
	startOnce      sync.Once
	emit           func(pm *pb.ProtocolMessage)
	announce       AnnounceFn
	maxBatchSize   int
	proposeEvery   time.Duration
	stopWg         sync.WaitGroup
	am             *AtomicMulticast
	ownedGroupID   uint32
	skipHandlerRegistration bool
	currentSegCancel func()
	segMu          sync.Mutex
	currentFirstSN int32
	firstSNMu      sync.RWMutex
}

func (o *MultiPaxosOrderer) Init(mgr manager.Manager) {
	o.mgr = mgr
	o.backlog = newMPXBacklog()
	o.last = -1
	o.maxBatchSize = int(config.Config.BatchSize)
	o.proposeEvery = config.Config.BatchTimeout
	if o.am == nil {
		if GetGlobalMulticastOrderer() != nil {
			o.am = GetGlobalMulticastOrderer().am
		} else {
			o.am = NewAtomicMulticast()
		}
	}
	o.emit = func(pm *pb.ProtocolMessage) {
		mpx := pm.GetMultipaxos()
		if mpx == nil {
			for _, nid := range membership.AllNodeIDs() { messenger.EnqueueMsg(pm, nid) }
			return
		}
		groupID := extractGroupID(mpx)
		if groupID == 0 {
			for _, nid := range membership.AllNodeIDs() { messenger.EnqueueMsg(pm, nid) }
			return
		}
		if o.am != nil {
			if members := o.am.GetGroupMembers(groupID); len(members) > 0 {
				for _, nid := range members { messenger.EnqueueMsg(pm, nid) }
				return
			}
		}
		for _, nid := range membership.AllNodeIDs() { messenger.EnqueueMsg(pm, nid) }
	}
	if !o.skipHandlerRegistration {
		o.segmentChan = mgr.SubscribeOrderer()
		messenger.OrdererMsgHandler = o.HandleMessage
	}
	o.announce = func(sn int32, b *pb.Batch, metadata []byte) {
		if b == nil { return }
		if len(b.Requests) == 0 {
			shouldRespond := true
			batchBytes, _ := proto.Marshal(b)
			now := time.Now().UnixNano()
			announcer.Announce(&mirlog.Entry{Sn: sn, Batch: b, Digest: crypto.Hash(batchBytes),
				ShouldRespond: &shouldRespond, ProposeTs: now, CommitTs: now})
			return
		}
		var digest []byte
		if len(metadata) > 0 { digest = metadata } else {
			batchBytes, _ := proto.Marshal(b); digest = crypto.Hash(batchBytes)
		}
		shouldRespond := true
		if len(b.Requests) > 0 && o.am != nil {
			shouldRespond = o.am.IsMember(b.Requests[0].GetGroupId(), membership.OwnID)
		}
		var proposeTs, commitTs int64
		if inst, ok := o.dispatcher.load(sn); ok && inst != nil {
			proposeTs = inst.acceptTs; commitTs = inst.lastAcceptedTs
		}
		now := time.Now().UnixNano()
		if proposeTs == 0 { proposeTs = now }
		if commitTs == 0 { commitTs = now }
		announcer.Announce(&mirlog.Entry{Sn: sn, Batch: b, Digest: digest,
			ShouldRespond: &shouldRespond, ProposeTs: proposeTs, CommitTs: commitTs})
		if len(b.Requests) > 0 && GetGlobalMulticastOrderer() != nil {
			if len(b.Requests[0].TouchedGroups) > 1 && b.Requests[0].GSN > 0 {
				GetGlobalMulticastOrderer().RegisterGSNMetadata(b.Requests[0].GSN, b.Requests[0].TouchedGroups)
			}
		}
	}
	fmt.Printf("[MPX] Init ok; cfg: batchSize=%d batchTimeout=%s leaderPolicy=%s\n",
		o.maxBatchSize, o.proposeEvery, strings.ToLower(config.Config.LeaderPolicy))
}

func (o *MultiPaxosOrderer) Start(wg *sync.WaitGroup) {
	o.startOnce.Do(func() {
		if o.segmentChan != nil {
			go func() {
				for seg := range o.segmentChan {
					logger.Info().Int32("firstSN", seg.FirstSN()).Int32("lastSN", seg.LastSN()).Msg("MultiPaxos new segment")
					o.runSegment(seg)
					go o.killSegment(seg)
				}
			}()
		}
	})
}

func (o *MultiPaxosOrderer) HandleMessage(pm *pb.ProtocolMessage) {
	sn := pm.Sn
	if sn <= atomic.LoadInt32(&o.last) { return }
	mpx := pm.GetMultipaxos()
	if mpx != nil {
		groupID := extractGroupID(mpx)
		if groupID != 0 && o.am != nil {
			members := o.am.GetGroupMembers(groupID)
			if members != nil {
				found := false
				for _, m := range members { if m == pm.SenderId { found = true; break } }
				if !found { return }
			}
		}
	}
	inst, ok := o.dispatcher.load(sn)
	if !ok || inst == nil {
		inst = o.ensureInstance(sn)
		if mpx != nil { inst.bucketId = extractGroupID(mpx) }
		o.dispatcher.store(sn, inst)
		inst.startWorkers(&o.stopWg)
		o.backlog.drainTo(sn, inst.enqueue)
	}
	inst.enqueue(pm)
}

func (o *MultiPaxosOrderer) HandleEntry(e *mirlog.Entry) {
	if e == nil { return }
	o.HandleMessage(&pb.ProtocolMessage{SenderId: -1, Sn: e.Sn,
		Msg: &pb.ProtocolMessage_MissingEntry{MissingEntry: &pb.MissingEntry{
			Sn: e.Sn, Batch: e.Batch, Digest: e.Digest, Aborted: e.Aborted, Suspect: e.Suspect, Proof: "Dummy Proof.",
		}}})
}

func (o *MultiPaxosOrderer) runSegment(seg manager.Segment) {
	allGroupIDs := o.am.GetDefinedGroups()
	if len(allGroupIDs) == 0 { allGroupIDs = []uint32{0} }
	numGroups := int32(len(allGroupIDs))
	if numGroups == 0 { numGroups = 1 }

	o.firstSNMu.Lock(); o.currentFirstSN = seg.FirstSN(); o.firstSNMu.Unlock()
	o.segMu.Lock()
	if o.currentSegCancel != nil { o.currentSegCancel() }
	stopCh := make(chan struct{})
	o.currentSegCancel = func() { close(stopCh) }
	o.segMu.Unlock()

	groupId := o.ownedGroupID
	members := o.am.GetGroupMembers(groupId)
	if members == nil { return }

	go func(gid uint32) {
		t := time.NewTicker(o.proposeEvery)
		defer t.Stop()
		currentSN := seg.FirstSN()
		for {
			select {
			case <-stopCh: return
			case <-t.C:
				if currentSN > seg.LastSN() { continue }
				now := time.Now()
				if mirlog.GetEntry(currentSN) != nil { currentSN += numGroups; continue }
				inst, ok := o.dispatcher.load(currentSN)
				if !ok || inst == nil {
					inst = o.ensureInstance(currentSN)
					inst.setSegment(seg); inst.bucketId = gid; inst.SetMembers(members)
					o.dispatcher.store(currentSN, inst)
					inst.startWorkers(&o.stopWg)
					o.backlog.drainTo(currentSN, inst.enqueue)
				} else {
					inst.mu.Lock()
					if inst.bucketId == 0 { inst.bucketId = gid }
					inst.mu.Unlock()
				}
				inst.tick(now); inst.ProposeIfDue()
			}
		}
	}(groupId)
}

func (o *MultiPaxosOrderer) killSegment(seg manager.Segment) {
	lastSN := seg.LastSN()
	timeout := time.After(30 * time.Second)
	for mirlog.GetEntry(lastSN) == nil {
		select {
		case <-timeout: return
		default: time.Sleep(10 * time.Millisecond)
		}
	}
	checkpoints := mirlog.Checkpoints()
	cp := mirlog.GetCheckpoint()
	cpTimeout := time.After(60 * time.Second)
	for cp == nil || cp.Sn < seg.LastSN() {
		select {
		case cp = <-checkpoints:
		case <-cpTimeout: return
		}
	}
	o.instMu.Lock()
	if seg.LastSN() > o.last { atomic.StoreInt32(&o.last, seg.LastSN()) }
	o.instMu.Unlock()
}

func (o *MultiPaxosOrderer) ensureInstance(sn int32) *mpxInstance {
	return newMPXInstance(o, sn, o.announce, o.maxBatchSize, o.proposeEvery)
}

func (o *MultiPaxosOrderer) Sign(data []byte) ([]byte, error) { return nil, nil }
func (o *MultiPaxosOrderer) CheckSig(data []byte, senderID int32, signature []byte) error { return nil }

func extractGroupID(mpx *pb.MPxMsg) uint32 {
	switch msg := mpx.Type.(type) {
	case *pb.MPxMsg_Prepare:  return msg.Prepare.GetGroupId()
	case *pb.MPxMsg_Promise:  return msg.Promise.GetGroupId()
	case *pb.MPxMsg_Accept:   return msg.Accept.GetGroupId()
	case *pb.MPxMsg_Accepted: return msg.Accepted.GetGroupId()
	case *pb.MPxMsg_Commit:   return msg.Commit.GetGroupId()
	}
	return 0
}
