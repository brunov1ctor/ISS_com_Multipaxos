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

// mpxDispatcher é um mapa concorrente sn -> *mpxInstance (sync.Map): dá acesso à instância
// responsável por um número de sequência sem precisar de um mutex explícito no chamador.
type mpxDispatcher struct { mm sync.Map }
func (d *mpxDispatcher) load(sn int32) (*mpxInstance, bool) {
	if v, ok := d.mm.Load(sn); ok { return v.(*mpxInstance), true }
	return nil, false
}
func (d *mpxDispatcher) store(sn int32, inst *mpxInstance) { d.mm.Store(sn, inst) }

type MultiPaxosOrderer struct {
	segmentChan    chan manager.Segment          // segmentos novos publicados pelo manager (consumido por Start)
	dispatcher     mpxDispatcher                 // sn -> instância MultiPaxos ativa para esse SN
	last           int32                         // maior SN já finalizado/estabilizado; mensagens com sn <= last são ignoradas
	instMu         sync.RWMutex                  // protege a leitura+escrita condicional de "last" em killSegment
	startOnce      sync.Once                     // garante que Start() só arma a goroutine de segmentos uma vez
	emit           func(pm *pb.ProtocolMessage)  // envia uma mensagem de protocolo aos peers do grupo (ou broadcast)
	announce       AnnounceFn                    // callback chamado quando um SN é comitado, entrega ao log local
	maxBatchSize   int                           // tamanho máximo de batch ao cortar requisições (config.Config.BatchSize)
	proposeEvery   time.Duration                 // período do ticker que dispara tick()/ProposeIfDue() por SN
	am             *AtomicMulticast              // registro de grupos/membros compartilhado (multicast atômico)
	ownedGroupID   uint32                        // grupo de dados administrado por este orderer (0 = broadcast/standalone)
	skipHandlerRegistration bool                 // true quando este orderer é filho de um MultiPaxosMulticastOrderer
	commitNotifyCh chan struct{} // shared channel from parent multicast orderer
	batchCounter   int32         // shared alternating counter for CutBatch (cross-op vs single-group)
}

func (o *MultiPaxosOrderer) Init(mgr manager.Manager) {
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
			// CSMR Output Processing: only the leader notifies proxy (avoids 3x duplicate notifications)
			if inst, ok := o.dispatcher.load(sn); ok && inst != nil && inst.leader == membership.OwnID {
				GetGlobalMulticastOrderer().NotifyProxy(b, sn)
			}
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
		inst.startWorkers()
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
	isBroadcast := !o.skipHandlerRegistration
	// Broadcast: stride = number of parallel segments (leaders)
	// Multicast: stride = number of groups
	if isBroadcast { numGroups = int32(len(membership.AllNodeIDs())) }

	groupId := o.ownedGroupID

	// Cada segmento roda em paralelo (um por líder, SNs interleaved)
	// NÃO cancela segmentos anteriores — múltiplos líderes operam simultaneamente
	var stopCh chan struct{}
	stopCh = make(chan struct{})

	members := o.am.GetGroupMembers(groupId)
	if members == nil {
		fmt.Printf("[MPX] SEGMENT SKIP group=%d members=nil (ownedGroupID=%d, definedGroups=%v)\n", groupId, o.ownedGroupID, o.am.GetDefinedGroups())
		return
	}
	groupLeader := o.am.GetGroupLeaderForSegment(groupId, seg.FirstSN(), numGroups)
	fmt.Printf("[MPX] SEGMENT group=%d firstSN=%d lastSN=%d members=%v leader=%d ownID=%d\n",
		groupId, seg.FirstSN(), seg.LastSN(), members, groupLeader, membership.OwnID)

	go func(gid uint32) {
		t := time.NewTicker(o.proposeEvery)
		defer t.Stop()
		// Start at the correct SN for this group
		// In interleaved mode: group X starts at firstSN + X (if firstSN+X <= lastSN)
		currentSN := seg.FirstSN() + int32(gid)
		if currentSN > seg.LastSN() { return }

		// commitCh: wakes up loop immediately after a commit (to advance SN and propose next)
		var commitCh <-chan struct{}
		if o.commitNotifyCh != nil {
			commitCh = o.commitNotifyCh
		}

		for {
			select {
			case <-stopCh: return
			case <-commitCh:
				if currentSN > seg.LastSN() { continue }
				if mirlog.GetEntry(currentSN) != nil { currentSN += numGroups; continue }
				inst := o.getOrCreateInstance(currentSN, gid, seg, members)
				inst.ProposeIfDue()
			case <-t.C:
				if currentSN > seg.LastSN() { continue }
				now := time.Now()
				if mirlog.GetEntry(currentSN) != nil { currentSN += numGroups; continue }
				inst := o.getOrCreateInstance(currentSN, gid, seg, members)
				inst.tick(now); inst.ProposeIfDue()
			}
		}
	}(groupId)
}

// getOrCreateInstance retorna a instância MultiPaxos já registrada para o SN informado, ou cria
// e registra uma nova (associando segmento, grupo e membros) se ainda não existir. Extraído
// porque runSegment precisa fazer exatamente esse "carrega ou cria" tanto ao acordar por commit
// quanto a cada tick do relógio.
func (o *MultiPaxosOrderer) getOrCreateInstance(sn int32, gid uint32, seg manager.Segment, members []int32) *mpxInstance {
	inst, ok := o.dispatcher.load(sn)
	if !ok || inst == nil {
		inst = o.ensureInstance(sn)
		inst.setSegment(seg); inst.bucketId = gid; inst.SetMembers(members)
		o.dispatcher.store(sn, inst)
		inst.startWorkers()
		return inst
	}
	inst.mu.Lock()
	if inst.bucketId == 0 { inst.bucketId = gid }
	inst.mu.Unlock()
	return inst
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
	return newMPXInstance(o, sn, o.announce, o.proposeEvery)
}

// RunSegmentDirect runs a segment immediately without waiting for the segment channel.
// Used for bootstrap to ensure instances are ready before client sends requests.
func (o *MultiPaxosOrderer) RunSegmentDirect(seg manager.Segment) {
	o.runSegment(seg)
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
