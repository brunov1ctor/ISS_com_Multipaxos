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
	"github.com/hyperledger-labs/mirbft/request"
	pb "github.com/hyperledger-labs/mirbft/protobufs"
	logger "github.com/rs/zerolog/log"
)

type AnnounceFn func(sn int32, batchBytes []byte, metadata []byte)

// mpxDispatcher gerencia mapa thread-safe de instâncias Paxos
// Cada SN (sequence number) tem sua própria instância
type mpxDispatcher struct {
	mm sync.Map // map[int32]*mpxInstance
}

func (d *mpxDispatcher) load(sn int32) (*mpxInstance, bool) {
	if v, ok := d.mm.Load(sn); ok {
		return v.(*mpxInstance), true
	}
	return nil, false
}
func (d *mpxDispatcher) store(sn int32, inst *mpxInstance) { d.mm.Store(sn, inst) }
func (d *mpxDispatcher) delete(sn int32)                    { d.mm.Delete(sn) }

// mpxBacklog armazena mensagens que chegaram antes da instância ser criada
type mpxBacklog struct {
	mu sync.Mutex
	qs map[int32][]*pb.ProtocolMessage
}

func newMPXBacklog() mpxBacklog {
	return mpxBacklog{qs: make(map[int32][]*pb.ProtocolMessage)}
}

// drainTo entrega todas mensagens pendentes para uma instância
func (b *mpxBacklog) drainTo(sn int32, f func(*pb.ProtocolMessage)) {
	b.mu.Lock()
	items := b.qs[sn]
	delete(b.qs, sn)
	b.mu.Unlock()
	for _, m := range items {
		f(m)
	}
}

// MultiPaxosOrderer implementa consenso Multi-Paxos com suporte a grupos (CSMR)
//
// ARQUITETURA CSMR (Composable State Machine Replication):
// ============================================================
// 1. PARALELISMO POR GRUPOS:
//    - Cada grupo executa Paxos independentemente em paralelo
//    - Grupos não interferem entre si (exceto barreira global)
//    - Exemplo: Grupo 1 processa SNs 0,4,8... | Grupo 2 processa SNs 1,5,9...
//
// 2. BARREIRA GLOBAL (Grupo 0):
//    - Operações multi-grupo (ex: range query em 2 partições) vão para bucket 0
//    - Bucket 0 = broadcast para TODOS os nós (atomic global order)
//    - Congela propostas locais até drenagem completa (garante linearizability)
//
// 3. MULTICAST SELETIVO:
//    - Prepare/Promise/Accept: apenas membros do grupo
//    - Commit: broadcast para TODOS (meta-log para checkpoint consistente)
//    - Reduz tráfego de rede em ~70% (4 grupos = 25% do tráfego)
//
// 4. META-LOG (Checkpoint Consistente):
//    - Membros: executam batch completo
//    - Não-membros: recebem Commit, salvam digest (sem executar)
//    - Resultado: todos têm mesmo digest → checkpoint converge
//    - Log contíguo (sem buracos) → MirBFT progride corretamente
//
// 5. CRASH MODEL:
//    - Assume falhas por crash (não Byzantine)
//    - Quorum = maioria simples (n/2 + 1)
//    - Todas réplicas respondem, cliente aceita primeira resposta
type MultiPaxosOrderer struct {
	mgr         manager.Manager
	segmentChan chan manager.Segment

	dispatcher mpxDispatcher // Mapa de instâncias ativas
	backlog    mpxBacklog    // Mensagens que chegaram cedo
	last       int32         // Último SN processado

	instMu    sync.RWMutex
	startOnce sync.Once

	emit     func(pm *pb.ProtocolMessage) // Função para enviar mensagens
	announce AnnounceFn                    // Função para entregar batches

	maxBatchSize     int
	proposeEvery     time.Duration // Intervalo entre propostas
	stopWg           sync.WaitGroup
	sbNilAfter       time.Duration
	enableNilDeliver bool
	
	onInstanceCreated func(sn int32) // Callback quando instância é criada
	
	// CSMR: Gerenciamento de grupos e atomic global order
	am *AtomicMulticast // Gerenciador de grupos (members, leaders, YAML config)
	
	segmentInstances sync.Map // Rastreia instâncias por segmento
	
	// CSMR: Barreira global para atomic global order (artigo CSMR)
	// Operações multi-grupo vão para bucket 0 e congelam propostas locais
	lastGlobalSN     int32 // Último SN decidido no bucket 0 (global)
	observedGlobalSN int32 // Último SN global já entregue localmente
	globalPending    bool  // Flag: existe global pendente (freeze propostas locais)
	globalMu         sync.RWMutex
	
	// CSMR: Contador de instâncias locais em voo por grupo (para drenagem)
	// Usado para garantir que todas instâncias locais terminem antes de descongelar global
	inflightLocal map[uint32]int32 // groupId -> contador de instâncias ativas
	inflightMu    sync.RWMutex
}

// Init inicializa o orderer Multi-Paxos
//
// CONFIGURAÇÃO:
// ===============
// 1. Parâmetros de consenso (batchSize, batchTimeout)
// 2. Função emit() - Multicast seletivo por grupo
// 3. Função announce() - Entrega de batches + barreira global
// 4. Atomic Multicast - Gerenciamento de grupos
//
// FUNÇÃO EMIT (Multicast Seletivo):
// ===================================
// - Prepare/Promise/Accept: apenas membros do grupo
// - Commit: broadcast para TODOS (meta-log)
// - Grupo 0: sempre broadcast (barreira global)
//
// FUNÇÃO ANNOUNCE (Entrega + Barreira):
// ========================================
// - Entrega batch commitado à aplicação
// - Gerencia barreira global (freeze/unfreeze)
// - Rastreia drenagem de instâncias locais
// - Crash model: todas réplicas respondem
func (o *MultiPaxosOrderer) Init(mgr manager.Manager) {
	o.mgr = mgr
	o.backlog = newMPXBacklog()
	o.last = -1

	o.maxBatchSize = int(config.Config.BatchSize)
	o.proposeEvery = time.Duration(config.Config.BatchTimeout)

	o.sbNilAfter = o.proposeEvery * 3
	o.enableNilDeliver = true
	
	// Inicializa atomic multicast
	o.am = NewAtomicMulticast()
	o.inflightLocal = make(map[uint32]int32)

	// Função emit: multicast seletivo por grupo (CSMR)
	// - Grupo 0: broadcast para todos (barreira global)
	// - Grupo N: envia apenas para membros do grupo (paralelismo)
	// - Fallback: broadcast se grupo não definido
	o.emit = func(pm *pb.ProtocolMessage) {
		mpx := pm.GetMultipaxos()
		if mpx == nil {
			// Broadcast para mensagens não-Paxos
			for _, nid := range membership.AllNodeIDs() {
				if nid != membership.OwnID {
					messenger.EnqueueMsg(pm, nid)
				}
			}
			return
		}
		
		// Extrai groupID da mensagem
		var groupID uint32
		isCommit := false
		switch msg := mpx.Type.(type) {
		case *pb.MPxMsg_Prepare:
			groupID = msg.Prepare.GetGroupId()
		case *pb.MPxMsg_Promise:
			groupID = msg.Promise.GetGroupId()
		case *pb.MPxMsg_Accept:
			groupID = msg.Accept.GetGroupId()
		case *pb.MPxMsg_Commit:
			groupID = msg.Commit.GetGroupId()
			isCommit = true // Commit vai para todos (meta-log)
		case *pb.MPxMsg_Accepted:
			if inst, ok := o.dispatcher.load(pm.Sn); ok && inst != nil {
				groupID = inst.bucketId
			}
		}
		
		// Grupo 0 OU Commit = broadcast global (meta-log)
		if groupID == 0 || isCommit {
			for _, nid := range membership.AllNodeIDs() {
				if nid != membership.OwnID {
					messenger.EnqueueMsg(pm, nid)
				}
			}
			return
		}
		
		// Multicast seletivo: envia apenas para membros do grupo
		members := o.am.GetGroupMembers(groupID)
		if members == nil || len(members) == 0 {
			// Fallback: broadcast
			for _, nid := range membership.AllNodeIDs() {
				if nid != membership.OwnID {
					messenger.EnqueueMsg(pm, nid)
				}
			}
			return
		}
		
		for _, nodeID := range members {
			if nodeID != membership.OwnID {
				messenger.EnqueueMsg(pm, nodeID)
			}
		}
	}
	o.segmentChan = o.mgr.SubscribeOrderer()
	messenger.OrdererMsgHandler = o.HandleMessage

	// Função announce: entrega batch commitado à aplicação
	// CRÍTICO: metadata contém digest do batch (para Entry.Digest)
	// CSMR: Gerencia barreira global e drenagem de instâncias locais
	// - Batch vazio: entrega NIL (crash model: todas réplicas respondem)
	// - Bucket 0: atualiza barreira global, congela propostas locais
	// - Bucket N: decrementa inflightLocal, descongela se drenagem completa
	o.announce = func(sn int32, batchBytes []byte, metadata []byte) {
		if len(batchBytes) == 0 {
			// CSMR: Batch vazio - crash model (todas réplicas respondem)
			fmt.Printf("[MPX][SKIP] sn=%d (empty batch, não entrega)\n", sn)
			emptyBatch := &pb.Batch{Requests: []*pb.ClientRequest{}}
			shouldRespond := true
			// CRÍTICO: Calcula digest mesmo para batch vazio (checkpoint precisa)
			emptyBytes, _ := proto.Marshal(emptyBatch)
			digest := crypto.Hash(emptyBytes)
			entry := &mirlog.Entry{
				Sn:            sn,
				Batch:         emptyBatch,
				Digest:        digest,
				ShouldRespond: &shouldRespond,
			}
			announcer.Announce(entry)
			return
		}
		
		var b pb.Batch
		if err := proto.Unmarshal(batchBytes, &b); err != nil {
			fmt.Printf("[MPX][ANNOUNCE][ERR] sn=%d unmarshal: %v\n", sn, err)
			return
		}
		
		// CRÍTICO: Usa digest passado via metadata, ou calcula se não fornecido
		var digest []byte
		if len(metadata) > 0 {
			digest = metadata
		} else {
			// Fallback: calcula digest localmente
			digest = crypto.Hash(batchBytes)
		}
		
		// CSMR: Atualiza barreira global se for bucket 0
		// Bucket 0 = operações multi-grupo (atomic global order)
		if len(b.Requests) > 0 && b.Requests[0].GetGroupId() == 0 {
			o.globalMu.Lock()
			if sn > o.lastGlobalSN {
				o.lastGlobalSN = sn
				o.globalPending = true // Congela propostas locais
				fmt.Printf("[CSMR][BARRIER] Global barrier updated: sn=%d (freeze local)\n", sn)
			}
			// CSMR: Só atualiza observedGlobalSN se não há locais em voo
			o.inflightMu.RLock()
			totalInflight := int32(0)
			for _, count := range o.inflightLocal {
				totalInflight += count
			}
			o.inflightMu.RUnlock()
			
			if totalInflight == 0 && sn > o.observedGlobalSN {
				o.observedGlobalSN = sn
				o.globalPending = false // Descongela
				fmt.Printf("[CSMR][BARRIER] Global observed: sn=%d (unfreeze)\n", sn)
			} else if totalInflight > 0 {
				fmt.Printf("[CSMR][DRAIN] Waiting for %d inflight local instances\n", totalInflight)
			}
			o.globalMu.Unlock()
		} else {
			// CSMR: Decrementa contador de inflight para grupos locais
			if len(b.Requests) > 0 {
				groupId := b.Requests[0].GetGroupId()
				if groupId != 0 {
					o.inflightMu.Lock()
					if o.inflightLocal[groupId] > 0 {
						o.inflightLocal[groupId]--
					}
					// CSMR: Reavaliar se pode descongelar (drain completo)
					totalInflight := int32(0)
					for _, count := range o.inflightLocal {
						totalInflight += count
					}
					o.inflightMu.Unlock()
					
					// Se drenagem completa e global pendente, descongela
					if totalInflight == 0 {
						o.globalMu.Lock()
						if o.globalPending {
							o.observedGlobalSN = o.lastGlobalSN
							o.globalPending = false
							fmt.Printf("[CSMR][UNFREEZE] Drain complete, unfreeze (observed=%d)\n", o.observedGlobalSN)
						}
						o.globalMu.Unlock()
					}
				}
			}
		}
		
		// CRASH MODEL (CSMR): Todas réplicas respondem, cliente aceita primeira
		// Artigo assume crash failures, não Byzantine (não precisa f+1)
		shouldRespond := true
		
		entry := &mirlog.Entry{
			Sn:             sn,
			Batch:          &b,
			Digest:         digest,
			ShouldRespond:  &shouldRespond,
		}
		
		announcer.Announce(entry)

		fmt.Printf("SB-DELIVER sn=%d size=%d\n", sn, len(b.Requests))
		fmt.Printf("COMMIT sn=%d size=%d\n", sn, len(b.Requests))
		fmt.Printf("DELIVER sn=%d delivered=%d\n", sn, len(b.Requests))
		// Nota: tracing.COMMIT é registrado por mirlog.CommitEntry(), não duplicar aqui
	}

	fmt.Printf("[MPX] Init ok; cfg: batchSize=%d batchTimeout=%s leaderPolicy=%s\n",
		o.maxBatchSize, o.proposeEvery, strings.ToLower(config.Config.LeaderPolicy))
}

// Start inicia processamento de segmentos
// Cada segmento contém um range de SNs a serem ordenados
func (o *MultiPaxosOrderer) Start(wg *sync.WaitGroup) {
	o.startOnce.Do(func() {
		fmt.Printf("[MPX] Start begin\n")
		go func() {
			for seg := range o.segmentChan {
				logger.Info().
					Int("segId", seg.SegID()).
					Int32("length", seg.Len()).
					Int32("firstSN", seg.FirstSN()).
					Int32("lastSN", seg.LastSN()).
					Int32("first leader", seg.Leaders()[0]).
					Int32("len", seg.Len()).
					Msgf("MultiPaxos received new segment: %+v", seg.SNs())

				o.runSegment(seg)
				go o.killSegment(seg)
			}
		}()
		fmt.Printf("[MPX] Start done\n")
	})
}

// HandleMessage processa mensagens Paxos recebidas
// Cria instância on-demand se necessário (para followers)
// CSMR: Filtra mensagens de grupos
// - Grupo 0: todos processam (barreira global)
// - Grupo N: só membros processam (multicast seletivo)
func (o *MultiPaxosOrderer) HandleMessage(pm *pb.ProtocolMessage) {
	sn := pm.Sn

	if pm.SenderId == membership.OwnID {
		logger.Warn().Int32("sn", sn).Msg("MPX handles message from self.")
	}

	last := atomic.LoadInt32(&o.last)
	if sn <= last {
		logger.Debug().
			Int32("sn", sn).Int32("senderID", pm.SenderId).
			Msg("MPX discards message. Message belongs to an old segment.")
		return
	}
	
	// CSMR: Filtra mensagens de grupos para não-membros
	// EXCEÇÃO: Commit é processado por todos (não-membros aprendem digest para meta-log)
	mpx := pm.GetMultipaxos()
	if mpx != nil {
		var groupID uint32
		isCommit := false
		switch msg := mpx.Type.(type) {
		case *pb.MPxMsg_Prepare:
			groupID = msg.Prepare.GetGroupId()
		case *pb.MPxMsg_Promise:
			groupID = msg.Promise.GetGroupId()
		case *pb.MPxMsg_Accept:
			groupID = msg.Accept.GetGroupId()
		case *pb.MPxMsg_Accepted:
			if inst, ok := o.dispatcher.load(pm.Sn); ok && inst != nil {
				groupID = inst.bucketId
			}
		case *pb.MPxMsg_Commit:
			groupID = msg.Commit.GetGroupId()
			isCommit = true // Commit é processado por todos
		}
		
		// Não-membros só processam Commit (para aprender digest)
		if groupID != 0 && !isCommit {
			members := o.am.GetGroupMembers(groupID)
			if members != nil {
				isMember := false
				for _, m := range members {
					if m == membership.OwnID {
						isMember = true
						break
					}
				}
				if !isMember {
					return // Não processa (exceto Commit)
				}
			}
		}
	}

	inst, ok := o.dispatcher.load(sn)
	if !ok || inst == nil {
		// Criação on-demand: followers criam instância ao receber mensagens
		inst = o.ensureInstance(sn)
		o.dispatcher.store(sn, inst)
		inst.startWorkers(&o.stopWg)
		o.backlog.drainTo(sn, inst.enqueue)
	}
	inst.enqueue(pm)
}

func (o *MultiPaxosOrderer) HandleEntry(e *mirlog.Entry) {
	if e == nil {
		return
	}
	o.HandleMessage(&pb.ProtocolMessage{
		SenderId: -1,
		Sn:       e.Sn,
		Msg: &pb.ProtocolMessage_MissingEntry{
			MissingEntry: &pb.MissingEntry{
				Sn:      e.Sn,
				Batch:   e.Batch,
				Digest:  e.Digest,
				Aborted: e.Aborted,
				Suspect: e.Suspect,
				Proof:   "Dummy Proof.",
			},
		},
	})
}

// runSegment executa consenso para um segmento
//
// ARQUITETURA CSMR (Execução Paralela):
// ========================================
// 1. Cria instâncias Paxos para cada grupo em paralelo
// 2. Líder de cada grupo propõe batches independentemente
// 3. Não-membros aprendem digest via Commit (meta-log)
// 4. Bucket 0 congela propostas locais (atomic global order)
// 5. SNs intercalados entre grupos via round-robin (groupPos)
//
// MAPEAMENTO SN → GRUPO (Round-Robin):
// ======================================
// Exemplo com 4 grupos:
//   SN 0,4,8,12... → Grupo 0 (global)
//   SN 1,5,9,13... → Grupo 1
//   SN 2,6,10,14.. → Grupo 2
//   SN 3,7,11,15.. → Grupo 3
//
// BARREIRA GLOBAL (Freeze/Unfreeze):
// ===================================
// - Bucket 0 tem requests → FREEZE propostas locais
// - Aguarda drenagem de instâncias locais em voo
// - Após drenagem → UNFREEZE propostas locais
// - Garante atomic global order (linearizability)
func (o *MultiPaxosOrderer) runSegment(seg manager.Segment) {
	activeInstances := make(map[uint32]*mpxInstance)
	var mu sync.Mutex
	
	// Obtém grupos definidos (do YAML ou padrão)
	groupIDs := o.am.GetDefinedGroups()
	if len(groupIDs) == 0 {
		groupIDs = []uint32{0} // Fallback: só grupo global
	}
	
	// FAIL-FAST: valida configuração antes de iniciar segmento
	if len(groupIDs) > len(request.Buckets) {
		logger.Fatal().
			Int("numGroups", len(groupIDs)).
			Int("numBuckets", len(request.Buckets)).
			Msg("FATAL: numGroups > NumBuckets. Increase config.NumBuckets or reduce groups in groups.yml")
		return
	}
	
	numGroups := int32(len(groupIDs))
	
	// Mapa groupId -> índice na lista ordenada (para cálculo correto do globalSN)
	groupPos := make(map[uint32]int32)
	for i, gid := range groupIDs {
		groupPos[gid] = int32(i)
	}
	
	go func() {
		t := time.NewTicker(o.proposeEvery)
		defer t.Stop()
		
		lastGroupIdx := 0
		localGroupPrepared := make(map[uint32]bool)
		
		for range t.C {
			now := time.Now()
			
			// CSMR: Freeze antecipado se bucket 0 tem requests pendentes
			if len(request.Buckets) > 0 && request.Buckets[0].Len() > 0 {
				o.globalMu.Lock()
				if !o.globalPending {
					o.globalPending = true
					fmt.Printf("[CSMR][FREEZE] Bucket 0 has pending requests, freeze local proposals\n")
				}
				o.globalMu.Unlock()
			} else {
				// CSMR: Limpa freeze se bucket 0 vazio E sem inflight
				o.inflightMu.RLock()
				totalInflight := int32(0)
				for _, count := range o.inflightLocal {
					totalInflight += count
				}
				o.inflightMu.RUnlock()
				
				if totalInflight == 0 {
					o.globalMu.Lock()
					if o.globalPending {
						o.globalPending = false
						fmt.Printf("[CSMR][UNFREEZE] Bucket 0 empty and no inflight, unfreeze\n")
					}
					o.globalMu.Unlock()
				}
			}
			
			for offset := 0; offset < len(groupIDs); offset++ {
				idx := (lastGroupIdx + offset) % len(groupIDs)
				groupId := groupIDs[idx]
				
				segmentLeaders := seg.Leaders()
				groupLeader := o.am.GetGroupLeader(GroupID(groupId), segmentLeaders)
				isGroupLeader := (groupLeader == membership.OwnID)
				
				mu.Lock()
				inst := activeInstances[groupId]
				
				if inst == nil || inst.isClosed() {
					// Verifica se este nó é membro do grupo
					members := o.am.GetGroupMembers(groupId)
					if members == nil {
						mu.Unlock()
						fmt.Printf("[MPX][SEG] groupId=%d não existe, pulando\n", groupId)
						continue
					}
					
					isMember := false
					for _, m := range members {
						if m == membership.OwnID {
							isMember = true
							break
						}
					}
					
					// CSMR CORREÇÃO A: Nó não-membro aprende digest via Commit (meta-log)
					// Razão: MirBFT exige log contíguo (sem buracos) senão firstEmptySN trava
					// Solução: não-membros escutam Commit, salvam placeholder com MESMO digest
					// Resultado: checkpoint converge (todos têm mesmo digest), log sem buracos
					if !isMember {
						mu.Unlock()
						// Não-membros aguardam Commit de membros para aprender digest
						// Implementação: HandleMessage processa Commit mesmo de não-membros
						continue
					}
					
					if !isGroupLeader {
						mu.Unlock()
						continue
					}
					
					// CSMR: Não propor local se global pendente (freeze)
					if groupId != 0 {
						o.globalMu.RLock()
						pending := o.globalPending
						o.globalMu.RUnlock()
						if pending {
							mu.Unlock()
							continue // Aguarda drenagem do global
						}
					}
					
					// Calcula próximo globalSN deste grupo por varredura (sem contador global)
					// Isso garante que cada segmento começa do zero, sem pular SNs
					var nextGlobalSN int32 = -1
					for candidateSN := seg.FirstSN(); candidateSN <= seg.LastSN(); candidateSN++ {
						offset := candidateSN - seg.FirstSN()
						if offset >= 0 && offset%numGroups == groupPos[groupId] {
							if mirlog.GetEntry(candidateSN) == nil {
								nextGlobalSN = candidateSN
								break
							}
						}
					}
					
					if nextGlobalSN < 0 {
						mu.Unlock()
						fmt.Printf("[MPX][SEG] groupId=%d: todos SNs já processados\n", groupId)
						continue
					}
					
					globalSN := nextGlobalSN
					inst = o.ensureInstance(globalSN)
					inst.setSegment(seg)
					inst.bucketId = groupId
					inst.bucketIndex = groupPos[groupId]
					
					// CSMR: NÃO incrementa inflightLocal aqui (movido para ProposeIfDue)
					// Razão: batches vazios não devem contar como inflight
					
					// FAIL-FAST: configuração inválida = abort segment
					if int(inst.bucketIndex) >= len(request.Buckets) {
						mu.Unlock()
						logger.Fatal().
							Int32("bucketIndex", inst.bucketIndex).
							Int("numBuckets", len(request.Buckets)).
							Uint32("groupId", groupId).
							Int("numGroups", len(groupIDs)).
							Msg("FATAL: bucketIndex >= NumBuckets. Fix groups.yml or config.NumBuckets")
						return
					}
					
					inst.SetMembers(members)
					
					if len(members) < len(membership.AllNodeIDs()) {
						fmt.Printf("[MPX][MULTICAST] sn=%d groupId=%d SELECTIVE members=%v quorum=%d\n", 
							globalSN, groupId, members, len(members)/2+1)
					} else {
						fmt.Printf("[MPX][BROADCAST] sn=%d groupId=%d ALL_NODES members=%v quorum=%d\n", 
							globalSN, groupId, members, len(members)/2+1)
					}
					
					if !localGroupPrepared[groupId] {
						localGroupPrepared[groupId] = true
					}
					
					prep := &pb.MPxMsg{Type: &pb.MPxMsg_Prepare{
						Prepare: &pb.MPxPrepare{
							Id:      &pb.MPxInstanceId{Sn: globalSN, Lead: uint64(membership.OwnID)},
							Ballot:  uint64(inst.currentBallot),
							GroupId: groupId,
						},
					}}
					pm := &pb.ProtocolMessage{
						SenderId: membership.OwnID,
						Sn:       globalSN,
						Msg:      &pb.ProtocolMessage_Multipaxos{Multipaxos: prep},
					}
					if o.emit != nil {
						o.emit(pm)
					}
					
					inst.prepSent = true
					inst.enableNilDeliver = o.enableNilDeliver
					inst.sbNilAfter = o.sbNilAfter
					
					o.dispatcher.store(globalSN, inst)
					inst.startWorkers(&o.stopWg)
					o.backlog.drainTo(globalSN, inst.enqueue)
					
					activeInstances[groupId] = inst
					
					if existing, loaded := o.segmentInstances.LoadOrStore(seg.SegID(), []*mpxInstance{inst}); loaded {
						o.segmentInstances.Store(seg.SegID(), append(existing.([]*mpxInstance), inst))
					}
				}
				mu.Unlock()
				
				// CSMR: Verifica barreira global antes de processar grupo local
				if groupId != 0 { // Não é bucket global
					o.globalMu.RLock()
					lastGlobal := o.lastGlobalSN
					observedGlobal := o.observedGlobalSN
					o.globalMu.RUnlock()
					
					if observedGlobal < lastGlobal {
						// Espera barreira global ser observada
						fmt.Printf("[CSMR][WAIT] groupId=%d aguarda barreira global (observed=%d < last=%d)\n", 
							groupId, observedGlobal, lastGlobal)
						continue
					}
				}
				
				inst.tick(now)
				
				if isGroupLeader {
					// Líder sempre chama ProposeIfDue (pode propor NIL se bucket vazio)
					inst.ProposeIfDue()
				}
				
				lastGroupIdx = (idx + 1) % len(groupIDs)
				break
			}
		}
	}()
	
	// Cleanup: fecha instâncias quando segmento termina
	go func() {
		checkpoints := mirlog.Checkpoints()
		currentCheckpoint := mirlog.GetCheckpoint()
		for currentCheckpoint == nil || currentCheckpoint.Sn < seg.LastSN() {
			currentCheckpoint = <-checkpoints
		}
		
		if val, ok := o.segmentInstances.Load(seg.SegID()); ok {
			instList := val.([]*mpxInstance)
			for _, inst := range instList {
				if inst != nil {
					inst.stopWorkers()
					o.dispatcher.delete(inst.sn)
				}
			}
			o.segmentInstances.Delete(seg.SegID())
		}
	}()
}

func (o *MultiPaxosOrderer) killSegment(seg manager.Segment) {
	checkpoints := mirlog.Checkpoints()
	currentCheckpoint := mirlog.GetCheckpoint()
	for currentCheckpoint == nil || currentCheckpoint.Sn < seg.LastSN() {
		currentCheckpoint = <-checkpoints
	}
	mirlog.WaitForEntry(seg.LastSN())

	o.instMu.Lock()
	if seg.LastSN() > o.last {
		atomic.StoreInt32(&o.last, seg.LastSN())
	}
	o.instMu.Unlock()

	logger.Info().Int("segID", seg.SegID()).Msg("Segment finished.")
}

func (o *MultiPaxosOrderer) ensureInstance(sn int32) *mpxInstance {
	inst := newMPXInstance(o, sn, o.announce, o.maxBatchSize, o.proposeEvery)
	if o.onInstanceCreated != nil {
		o.onInstanceCreated(sn)
	}
	return inst
}

// LoadGroupsFromYAML carrega configuração de grupos do arquivo YAML
// FAIL-FAST: Aborta se arquivo não existir
// Razão: Determinismo crítico - todos nós devem ter mesma configuração de grupos
// Senão: groupPos diverge, SNs mapeiam para grupos diferentes, consenso quebra
func (o *MultiPaxosOrderer) LoadGroupsFromYAML(filename string) error {
	err := o.am.LoadGroupsFromYAML(filename)
	if err != nil {
		logger.Fatal().
			Err(err).
			Str("file", filename).
			Msg("FATAL: groups.yml é obrigatório para modo multicast. Determinismo quebrado sem ele.")
	}
	return err
}

func (o *MultiPaxosOrderer) Sign(data []byte) ([]byte, error) { return nil, nil }
func (o *MultiPaxosOrderer) CheckSig(data []byte, senderID int32, signature []byte) error {
	return nil
}

