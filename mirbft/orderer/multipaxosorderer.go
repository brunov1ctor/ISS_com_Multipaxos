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

	// Checkpoint por grupo (ver comentário em recordGroupCommit): como cada nó só enxerga as
	// decisões dos grupos dos quais é membro, o checkpoint global do Manager (mirlog, baseado em
	// firstEmptySN sobre o log intercalado inteiro) nunca estabiliza para a maioria dos nós.
	// Esses dois campos substituem essa dependência por um progresso contado localmente, só
	// dentro deste grupo -- sem round-trip de rede, pois o próprio COMMIT do MultiPaxos (maioria
	// de ACCEPTED) já é a prova de durabilidade que um checkpoint normalmente buscaria confirmar
	// (válido porque o MultiPaxos só tolera falhas de parada, não Byzantine).
	groupCommitCount  int32 // atomic: quantos SNs este grupo já comitou localmente, nesta réplica
	groupCheckpointSN int32 // atomic: maior SN deste grupo confirmado como ponto estável
	extendedUpTo      int32 // atomic: maior lastSN de segmento pra qual já foi criada uma extensão
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
			o.recordGroupCommit(sn)
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
		o.recordGroupCommit(sn)
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
	// MissingEntry catch-up responses are for SNs this node fell behind on -- by definition
	// often <= o.last once other segments have since progressed. Exempt them from the
	// staleness filter below, or a legitimately fetched entry would be silently dropped here
	// and the requesting node would retry forever without ever being able to apply it.
	if _, isMissingEntry := pm.Msg.(*pb.ProtocolMessage_MissingEntry); !isMissingEntry {
		if sn <= atomic.LoadInt32(&o.last) { return }
	}
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
	groupId := o.ownedGroupID
	isBroadcast := !o.skipHandlerRegistration

	// snOffset é a posição deste grupo no intercalamento global de SN (stride = numGroups).
	// Broadcast (MultiPaxos simples, sem grupos): stride = nós/líderes paralelos, offset = ID do nó.
	// Multicast: stride = só grupos de DADOS -- grupo 0 (Sequencer) nunca comita, então não pode
	// ocupar posição no stride (ver GetDataGroupIndex) ou aquele SN fica vazio pra sempre.
	var numGroups, snOffset int32
	if isBroadcast {
		numGroups = int32(len(membership.AllNodeIDs()))
		snOffset = int32(groupId)
	} else {
		idx, total := o.am.GetDataGroupIndex(groupId)
		if idx < 0 { idx, total = 0, 1 } // salvaguarda; não deveria acontecer pra um grupo de dados real
		numGroups, snOffset = total, idx
	}
	if numGroups == 0 { numGroups = 1 }

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

	go func(gid uint32, offset int32) {
		t := time.NewTicker(o.proposeEvery)
		defer t.Stop()
		// Ponto de partida deste grupo no intercalamento: firstSN + offset (offset denso, ver
		// snOffset acima -- não é mais o gid literal, senão o stride teria buraco no grupo 0).
		currentSN := seg.FirstSN() + offset
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
				if currentSN > seg.LastSN() { o.extendSegmentIfReady(seg, gid, numGroups, members); continue }
				if mirlog.GetEntry(currentSN) != nil { currentSN += numGroups; continue }
				inst := o.getOrCreateInstance(currentSN, gid, seg, members)
				inst.ProposeIfDue()
			case <-t.C:
				if currentSN > seg.LastSN() { o.extendSegmentIfReady(seg, gid, numGroups, members); continue }
				now := time.Now()
				if mirlog.GetEntry(currentSN) != nil { currentSN += numGroups; continue }
				inst := o.getOrCreateInstance(currentSN, gid, seg, members)
				inst.tick(now); inst.ProposeIfDue()
			}
		}
	}(groupId, snOffset)
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

// recordGroupCommit atualiza o checkpoint LOCAL deste grupo após um commit bem-sucedido (chamado
// de dentro de o.announce, uma vez por SN comitado). Só se aplica ao modo multicast; no broadcast
// (sem grupos) todo nó já vê o log inteiro e o checkpoint global do Manager funciona normalmente.
// Sem round-trip de rede: o próprio COMMIT do MultiPaxos (maioria de ACCEPTED) já é a prova de
// durabilidade que um checkpoint buscaria confirmar -- válido porque MultiPaxos só tolera falhas
// de parada, não Byzantine. O intervalo é dividido por numGroups pra manter a mesma frequência
// relativa de checkpoints que CheckpointInterval teria num log não particionado em grupos.
func (o *MultiPaxosOrderer) recordGroupCommit(sn int32) {
	if !o.skipHandlerRegistration || o.am == nil { return }
	_, numGroups := o.am.GetDataGroupIndex(o.ownedGroupID)
	if numGroups < 1 { numGroups = 1 }
	interval := int32(config.Config.CheckpointInterval) / numGroups
	if interval < 1 { interval = 1 }
	if n := atomic.AddInt32(&o.groupCommitCount, 1); n%interval == 0 {
		atomic.StoreInt32(&o.groupCheckpointSN, sn)
		logger.Info().Uint32("group", o.ownedGroupID).Int32("sn", sn).Msg("New stable checkpoint (per-group).")
	}
}

// GroupCheckpointSN retorna o maior SN deste grupo já confirmado como ponto de checkpoint
// estável nesta réplica. Usado por killSegment no lugar do checkpoint global do Manager.
func (o *MultiPaxosOrderer) GroupCheckpointSN() int32 {
	return atomic.LoadInt32(&o.groupCheckpointSN)
}

// extendSegmentIfReady cria e lança o próximo segmento deste grupo quando o atual se esgota
// (currentSN > seg.LastSN()), seguindo o mesmo padrão que o Manager usa pros outros orderers
// (checkpoint confirma -> emite segmento novo), só que local a este grupo e disparado pelo
// checkpoint por grupo (groupCheckpointSN), não pelo checkpoint global do cluster -- que nunca
// estabiliza pra um nó que não é membro de todos os grupos (ver recordGroupCommit). Sem isso, um
// grupo que esgota seu segmento simplesmente para de propor pra sempre, silenciosamente.
// Só se aplica ao modo multicast; no broadcast o Manager compartilhado já cuida disso sozinho.
func (o *MultiPaxosOrderer) extendSegmentIfReady(seg manager.Segment, groupId uint32, numGroups int32, members []int32) {
	if !o.skipHandlerRegistration { return }
	lastSN := seg.LastSN()
	if atomic.LoadInt32(&o.groupCheckpointSN) < lastSN { return } // segmento ainda não confirmado
	for {
		cur := atomic.LoadInt32(&o.extendedUpTo)
		if cur >= lastSN { return } // outra goroutine já criou (ou está criando) esta extensão
		if atomic.CompareAndSwapInt32(&o.extendedUpTo, cur, lastSN) { break }
	}
	next := &manager.ContiguousSegment{}
	newFirst := lastSN + 1
	next.SetFields(0, membership.AllNodeIDs(), membership.AllNodeIDs(), newFirst, seg.Len(), lastSN)
	fmt.Printf("[MPX] group=%d EXTEND segment firstSN=%d lastSN=%d (checkpoint do grupo confirmou o fim do anterior)\n",
		groupId, next.FirstSN(), next.LastSN())
	o.runSegment(next)
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
	cpTimeout := time.After(60 * time.Second)
	if o.skipHandlerRegistration {
		// Multicast: checkpoint por grupo (ver recordGroupCommit), não o global do Manager, que
		// nunca estabiliza pra um nó que não é membro de todos os grupos de dados.
		for atomic.LoadInt32(&o.groupCheckpointSN) < seg.LastSN() {
			select {
			case <-time.After(50 * time.Millisecond):
			case <-cpTimeout: return
			}
		}
	} else {
		// Broadcast (MultiPaxos sem grupos): todo nó vê o log inteiro, o checkpoint global do
		// Manager funciona normalmente aqui.
		checkpoints := mirlog.Checkpoints()
		cp := mirlog.GetCheckpoint()
		for cp == nil || cp.Sn < seg.LastSN() {
			select {
			case cp = <-checkpoints:
			case <-cpTimeout: return
			}
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
