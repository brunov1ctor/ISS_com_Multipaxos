package orderer

import (
	"context"
	"crypto/sha256"
	"fmt"
	"sync"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/hyperledger-labs/mirbft/manager"
	"github.com/hyperledger-labs/mirbft/membership"
	"github.com/hyperledger-labs/mirbft/request"
	pb "github.com/hyperledger-labs/mirbft/protobufs"
	"github.com/hyperledger-labs/mirbft/tracing"
)

//
// ============================================================
// MultiPaxos Instance (por slot SN)
// ------------------------------------------------------------
// Cada instância coordena a decisão de um único número de
// sequência (SN) dentro de um segmento do MIR.
// ============================================================
//

// ---- Estados/ fases da instância ----

type instPhase int // estados da instância (por slot SN)

const (
	phaseInit instPhase = iota // recém-criada; ainda não propôs nada
	phaseProposing             // já cortou um batch e preparou Accept local
	phaseAcceptSent            // enviou Accept e aguarda maioria de Accepted
	phaseCommitted             // decidiu (commit real) OU entregou NIL (⊥)
)

// ---- Estrutura principal da instância ----

type mpxInstance struct {
	// Serialização interna da instância
	mu sync.Mutex

	// Ponte para o orderer (emite mensagens, anuncia DELIVER, acessa config)
	parent *MultiPaxosOrderer

	// Identidade da instância/slot
	sn int32

	// Parâmetros de proposta/ batching
	maxBatchSize  int
	proposeEvery  time.Duration // periodicidade do "tick" de proposta
	announce      AnnounceFn    // callback para anunciar DELIVER/COMMIT ao sistema
	lastProposeAt time.Time     // última tentativa de PROPOSE (limita frequência)
	closed        bool          // instância encerrada (commit ou NIL)

	// Segmento do MIR (traz líderes, buckets, lote e recorte de batch)
	seg manager.Segment

	// Controle do consenso/maioria para este SN
	quorum       int32
	lastVal      *pb.MPxValue // valor proposto/aceito (contém o batch serializado)
	lastDigest   [32]byte     // hash do batch (SHA-256) (para checagens)
	acceptCount  int32        // quantos Accepted contabilizados
	acceptedFrom map[int32]struct{}

	// Contexto de proposta/batch
	lastReqBatch *request.Batch // ponte para remover as reqs do bucket após COMMIT
	phase        instPhase      // fase atual da instância

	// Retransmissão e NIL-Deliver (SB-⊥)
	acceptRtxEvery   time.Duration // intervalo de RTX de Accept
	lastAcceptAt     time.Time     // última emissão de Accept (marca para RTX/NIL)
	enableNilDeliver bool          // habilita SB-⊥
	sbNilAfter       time.Duration // tempo máximo esperando maioria antes de ⊥

	// Filas de processamento de mensagens (serialização do handler)
	msgCh   chan *pb.ProtocolMessage
	stopCh  chan struct{}
	stopped bool
	wg      sync.WaitGroup
}

//
// ============================================================
// Construção e binding ao segmento
// ============================================================
//

// newMPXInstance cria a instância com timers padrão.
// - acceptRtxEvery = 2x BatchTimeout (retransmissão de Accept)
// - sbNilAfter     = 3x BatchTimeout (pode ser sobrescrito pelo orderer)
func newMPXInstance(parent *MultiPaxosOrderer, sn int32, announce AnnounceFn, maxBatch int, interval time.Duration) *mpxInstance {
	inst := &mpxInstance{
		parent:        parent,   	// ponte para o orderer “pai”: acesso a emit(), config e callback announce()
		sn:            sn,       	// slot (sequence number) que esta instância vai decidir dentro do segmento
		maxBatchSize:  maxBatch, 	// limite superior de reqs por batch neste SN (tipicamente = seg.BatchSize())
		proposeEvery:  interval, 	// período mínimo entre tentativas de proposta (≈ BatchTimeout)
		announce:      announce, 	// callback para anunciar COMMIT/DELIVER (aciona o Responder e métricas)
		lastProposeAt: time.Now(), 	// relógio inicial p/ rate-limit: evita propor imediatamente em sequência
		phase:         phaseInit,  	// estado inicial: ainda não cortou batch / não enviou Accept
		acceptRtxEvery: interval * 2, 	// simples: 2× BatchTimeout
		sbNilAfter:     interval * 3, 	// default; o orderer costuma sobrescrever p/ 6×
		msgCh:          make(chan *pb.ProtocolMessage, 4096),
		stopCh:         make(chan struct{}),
	}
	fmt.Printf("[MPX][CHK] instance init sn=%d\n", sn)
	return inst
}

// setSegment associa a instância a um segmento do MIR e calcula o quorum.
// Importante: quorum = floor(N/2)+1 (maioria simples).
func (i *mpxInstance) setSegment(seg manager.Segment) {
	i.mu.Lock()
	defer i.mu.Unlock()

	i.seg = seg
	n := int32(len(membership.AllNodeIDs()))
	if n < 1 {
		n = 1
	}
	i.quorum = n/2 + 1

	fmt.Printf("[MPX][CHK] seg bind sn=%d segID=%d firstSN=%d len=%d quorum=%d leaders=%v\n",
		i.sn, seg.SegID(), seg.FirstSN(), seg.Len(), i.quorum, seg.Leaders())
}

//
// ============================================================
// Workers / ciclo de vida
// ============================================================
//

// startWorkers inicia o loop que serializa o processamento de mensagens
// Multipaxos (chegam via msgCh).
func (i *mpxInstance) startWorkers(wg *sync.WaitGroup) {
	i.wg.Add(1)
	go func() {
		defer i.wg.Done()
		for {
			select {
			case pm := <-i.msgCh:
				i.handleMPxMsg(pm, pm.GetMultipaxos())
			case <-i.stopCh:
				return
			}
		}
	}()
}

// stopWorkers encerra os loops e bloqueia até a goroutine sair.
func (i *mpxInstance) stopWorkers() {
	i.mu.Lock()
	if i.stopped {
		i.mu.Unlock()
		return
	}
	i.stopped = true
	close(i.stopCh)
	close(i.msgCh)
	i.mu.Unlock()
	i.wg.Wait()
}

// enqueue enfileira uma mensagem de protocolo para processamento
// pela instância (com fallback assíncrono se a fila estiver cheia).
func (i *mpxInstance) enqueue(pm *pb.ProtocolMessage) {
	select {
	case i.msgCh <- pm:
	default:
		go func() { i.msgCh <- pm }()
	}
}

// isClosed informa se a instância já fechou (commit real ou NIL-commit).
func (i *mpxInstance) isClosed() bool {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.closed
}

//
// ============================================================
// Handlers Multipaxos
// ============================================================
//

// handleMPxMsg demultiplexa a mensagem do oneof MPxMsg.
func (i *mpxInstance) handleMPxMsg(pm *pb.ProtocolMessage, mpx *pb.MPxMsg) {
	switch t := mpx.Type.(type) {
	case *pb.MPxMsg_Prepare:
		// (Prepare/Promise não mudam estado nesta versão simplificada)
		fmt.Printf("[MPX][IN ] PREPARE sn=%d from=%d\n", i.sn, pm.GetSenderId())
	case *pb.MPxMsg_Promise:
		fmt.Printf("[MPX][IN ] PROMISE sn=%d from=%d\n", i.sn, pm.GetSenderId())
	case *pb.MPxMsg_Accept:
		fmt.Printf("[MPX][IN ] ACCEPT sn=%d from=%d\n", i.sn, pm.GetSenderId())
		i.onAccept(t.Accept)
	case *pb.MPxMsg_Accepted:
		fmt.Printf("[MPX][IN ] ACCEPTED sn=%d from=%d\n", i.sn, pm.GetSenderId())
		i.onAccepted(pm, t.Accepted)
	case *pb.MPxMsg_Commit:
		fmt.Printf("[MPX][IN ] COMMIT sn=%d from=%d\n", i.sn, pm.GetSenderId())
		i.onCommit(t.Commit)
	default:
		fmt.Printf("[MPX][WARN] msg MPx desconhecida sn=%d (%T)\n", i.sn, mpx.Type)
	}
}

// onAccept trata a chegada de um Accept:
// - Fixa (ou valida) o valor/digest.
// - Auto-contabiliza um Accepted (do próprio nó).
// - Reponde com Accepted para todos via emit.
func (i *mpxInstance) onAccept(a *pb.MPxAccept) {
	i.mu.Lock()
	defer i.mu.Unlock()

	// Fixa/valida valor e digest
	if a.GetValue() != nil {
		if i.lastVal != nil {
			incomingDigest := sha256.Sum256(a.GetValue().GetBatch()) 
			//Detecção de inconsistência: se chega um Accept com Value diferente do que a instância já fixou, compara os digests e ignora o conflitante
			if incomingDigest != i.lastDigest {
				fmt.Printf("[MPX][WARN] ACCEPT sn=%d digest mismatch (have=%x new=%x) → ignore\n",
					i.sn, i.lastDigest[:8], incomingDigest[:8])
				return
			}
		} else {
			i.lastVal = a.GetValue()
			i.lastDigest = sha256.Sum256(i.lastVal.GetBatch())
		}
	}

	// Conta o próprio Accepted
	if i.acceptedFrom == nil {
		i.acceptedFrom = make(map[int32]struct{})
	}
	if _, ok := i.acceptedFrom[membership.OwnID]; !ok {
		i.acceptedFrom[membership.OwnID] = struct{}{}
		i.acceptCount++
	}

	fmt.Printf("[MPX][QUORUM] ACCEPT sn=%d acceptCount=%d quorum=%d digest=%x\n",
		i.sn, i.acceptCount, i.quorum, i.lastDigest[:8])

	// Responde Accepted
	accepted := &pb.MPxMsg{Type: &pb.MPxMsg_Accepted{Accepted: &pb.MPxAccepted{
		Id:     &pb.MPxInstanceId{Sn: i.sn, Lead: uint64(membership.OwnID)},
		Ballot: 0,
		Ok:     true,
	}}}
	pm := &pb.ProtocolMessage{
		SenderId: membership.OwnID,
		Sn:       i.sn,
		Msg:      &pb.ProtocolMessage_Multipaxos{Multipaxos: accepted},
	}
	fmt.Printf("[MPX][NET] SEND Accepted sn=%d\n", i.sn)
	if i.parent.emit != nil {
		i.parent.emit(pm)
	} else {
		fmt.Printf("[MPX][NET][WARN] emit=nil; não enviou Accepted sn=%d\n", i.sn)
	}
}

// onAccepted contabiliza Accepted remotos e dispara COMMIT quando alcança a maioria.
// Também aciona o commit local (self-commit) imediatamente após alcançar maioria.
func (i *mpxInstance) onAccepted(pm *pb.ProtocolMessage, _ *pb.MPxAccepted) {
	i.mu.Lock()
	defer i.mu.Unlock()

	// Dedup de Accepted por remetente
	if pm != nil {
		if i.acceptedFrom == nil {
			i.acceptedFrom = make(map[int32]struct{})
		}
		if _, ok := i.acceptedFrom[pm.SenderId]; ok {
			return
		}
		i.acceptedFrom[pm.SenderId] = struct{}{}
	}
	i.acceptCount++

	fmt.Printf("[MPX][QUORUM] ACCEPTED sn=%d count=%d/%d digest=%x\n",
		i.sn, i.acceptCount, i.quorum, i.lastDigest[:8])

	// Se atingiu maioria e há valor, propaga COMMIT para o cluster
	if i.acceptCount >= i.quorum && i.lastVal != nil && i.phase != phaseCommitted {
		fmt.Printf("[MPX][QUORUM] MAJORITY REACHED sn=%d → COMMIT digest=%x\n", i.sn, i.lastDigest[:8])

		commit := &pb.MPxMsg{Type: &pb.MPxMsg_Commit{
			Commit: &pb.MPxCommit{
				Id:    &pb.MPxInstanceId{Sn: i.sn, Lead: uint64(membership.OwnID)},
				Value: i.lastVal,
			},
		}}
		pmOut := &pb.ProtocolMessage{
			SenderId: membership.OwnID,
			Sn:       i.sn,
			Msg:      &pb.ProtocolMessage_Multipaxos{Multipaxos: commit},
		}
		if i.parent.emit != nil {
			fmt.Printf("[MPX][NET] SEND Commit sn=%d digest=%x\n", i.sn, i.lastDigest[:8])
			i.parent.emit(pmOut)
		} else {
			fmt.Printf("[MPX][NET][WARN] emit=nil; não enviou Commit sn=%d\n", i.sn)
		}
	}

	// Self-commit imediato após atingir maioria
	if i.acceptCount >= i.quorum && i.lastVal != nil && i.phase != phaseCommitted {
		val := i.lastVal
		i.mu.Unlock()
		i.onCommit(&pb.MPxCommit{Id: &pb.MPxInstanceId{Sn: i.sn}, Value: val})
		i.mu.Lock()
	}
}

// onCommit finaliza a instância:
// - NIL (⊥): só avança o SN e gossip de Commit NIL (sem entregar requests).
// - Commit real: remove requests do bucket, anuncia DELIVER e marca closed.
func (i *mpxInstance) onCommit(c *pb.MPxCommit) {
	i.mu.Lock() 		//garante que só uma goroutine por vez mexa no estado da mpxInstance (ex.: phase, lastVal, acceptCount).
	defer i.mu.Unlock() 	//agenda o desbloqueio automático quando a função retornar (mesmo se der return no meio ou pânico)

	if i.phase == phaseCommitted {
		return
	}

	val := c.GetValue()

	// Caso NIL (⊥): avanço "vazio" do SN
	if val == nil || len(val.GetBatch()) == 0 {
		fmt.Printf("[MPX][NIL] COMMIT ⊥ sn=%d\n", i.sn)
		i.phase = phaseCommitted

		// Gossip do NIL para convergência
		nilCommit := &pb.MPxMsg{Type: &pb.MPxMsg_Commit{
			Commit: &pb.MPxCommit{
				Id:    &pb.MPxInstanceId{Sn: i.sn, Lead: uint64(membership.OwnID)},
				Value: nil, // NIL
			},
		}}
		pmOut := &pb.ProtocolMessage{
			SenderId: membership.OwnID,
			Sn:       i.sn,
			Msg:      &pb.ProtocolMessage_Multipaxos{Multipaxos: nilCommit},
		}
		if i.parent.emit != nil {
			i.parent.emit(pmOut)
		}

		// NIL não entrega requests (não anuncia)
		i.closed = true
		tracing.MainTrace.Event(tracing.COMMIT, int64(i.sn), 0)
		return
	}

	// Commit real: fixa valor/digest se ainda não tiver
	if i.lastVal == nil {
		i.lastVal = val
		i.lastDigest = sha256.Sum256(val.GetBatch())
	}

	i.phase = phaseCommitted

	// Gossip de Commit para convergência
	commit := &pb.MPxMsg{Type: &pb.MPxMsg_Commit{
		Commit: &pb.MPxCommit{
			Id:    &pb.MPxInstanceId{Sn: i.sn, Lead: uint64(membership.OwnID)},
			Value: i.lastVal,
		},
	}}
	pmOut := &pb.ProtocolMessage{
		SenderId: membership.OwnID,
		Sn:       i.sn,
		Msg:      &pb.ProtocolMessage_Multipaxos{Multipaxos: commit},
	}
	if i.parent.emit != nil {
		fmt.Printf("[MPX][NET] GOSSIP Commit sn=%d digest=%x\n", i.sn, i.lastDigest[:8])
		i.parent.emit(pmOut)
	}

	// Remove o batch dos buckets ANTES de anunciar (libera memória/pressão)
	if i.lastReqBatch != nil {
		request.RemoveBatch(i.lastReqBatch)
		i.lastReqBatch = nil
	}

	// Deserializa o batch p/ anunciar DELIVER
	var b pb.Batch
	if err := proto.Unmarshal(i.lastVal.GetBatch(), &b); err != nil {
		fmt.Printf("[MPX][ERR] COMMIT sn=%d unmarshal batch: %v\n", i.sn, err)
		return
	}
	if i.announce != nil {
		fmt.Printf("[MPX][CHK] ANNOUNCE sn=%d reqs=%d\n", i.sn, len(b.Requests))
		i.announce(i.sn, i.lastVal.GetBatch(), nil)
	} else {
		fmt.Printf("[MPX][WARN] COMMIT sn=%d announcer=nil — não vai acionar Responder!\n", i.sn)
	}

	i.closed = true
	traceCommit(i.sn, len(b.Requests))
}

//
// ============================================================
// Proposição e Tick (retransmissões e SB-⊥)
// ============================================================
//

// ProposeIfDue tenta propor se já passou o intervalo de proposeEvery.
// - Corta batch dos buckets do segmento.
// - Envia Accept (com valor).
// - Entra em phaseAcceptSent e inicia contadores.
func (i *mpxInstance) ProposeIfDue(ctx context.Context) {
	i.mu.Lock()
	defer i.mu.Unlock()

	// Rate-limit de proposições
	if time.Since(i.lastProposeAt) < i.proposeEvery {
		return
	}
	i.lastProposeAt = time.Now()

	// Se já enviou Accept, não repropor (RTX é tratado em tick)
	if i.phase >= phaseAcceptSent {
		return
	}

	var val *pb.MPxValue
	reqs := 0

	if i.lastVal == nil {
		// 1) Corta batch do segmento (bloqueia até size ou timeout)
		rb := i.cutReqBatch()
		if rb == nil || rb.Message() == nil || len(rb.Message().Requests) == 0 {
			return
		}
		i.lastReqBatch = rb
		batchMsg := rb.Message()
		reqs = len(batchMsg.Requests)

		fmt.Printf("PROPOSE sn=%d size=%d\n", i.sn, reqs)

		// 2) Serializa e monta o valor
		batchBytes, err := proto.Marshal(batchMsg)
		if err != nil {
			fmt.Printf("[MPX][ERR] PROPOSE sn=%d marshal batch: %v\n", i.sn, err)
			return
		}

		val = &pb.MPxValue{
			Id:    &pb.MPxInstanceId{Sn: i.sn, Lead: uint64(membership.OwnID)},
			Batch: batchBytes,
		}
		i.lastVal = val
		i.lastDigest = sha256.Sum256(batchBytes)

		// 3) Inicializa contadores de Accepted
		i.acceptedFrom = map[int32]struct{}{}
		i.acceptCount = 0
		i.acceptedFrom[membership.OwnID] = struct{}{}
		i.acceptCount = 1

		i.phase = phaseProposing
	} else {
		val = i.lastVal
	}

	// 4) Emite Accept para o cluster
	accept := &pb.MPxMsg{Type: &pb.MPxMsg_Accept{Accept: &pb.MPxAccept{
		Id:     &pb.MPxInstanceId{Sn: i.sn, Lead: uint64(membership.OwnID)},
		Ballot: 0,
		Value:  val,
	}}}
	pm := &pb.ProtocolMessage{
		SenderId: membership.OwnID,
		Sn:       i.sn,
		Msg:      &pb.ProtocolMessage_Multipaxos{Multipaxos: accept},
	}
	if i.parent.emit != nil {
		fmt.Printf("[MPX][NET] SEND Accept sn=%d bytes=%d digest=%x\n", i.sn, len(val.GetBatch()), i.lastDigest[:8])
		i.parent.emit(pm)
	} else {
		fmt.Printf("[MPX][NET][WARN] emit=nil; não enviou Accept sn=%d\n", i.sn)
	}

	i.phase = phaseAcceptSent
	i.lastAcceptAt = time.Now()
	tracePropose(i.sn, reqs)
}

// tick executa:
// - RTX de Accept se não alcançou maioria em acceptRtxEvery.
// - SB-⊥ (entrega NIL) se exceder sbNilAfter sem maioria.
func (i *mpxInstance) tick(now time.Time) {
	i.mu.Lock()
	defer i.mu.Unlock()

	// 1) Retransmissão de Accept
	if i.phase == phaseAcceptSent && i.acceptCount < i.quorum && now.Sub(i.lastAcceptAt) >= i.acceptRtxEvery {
		if i.parent.emit != nil && i.lastVal != nil {
			pm := &pb.ProtocolMessage{
				SenderId: membership.OwnID,
				Sn:       i.sn,
				Msg: &pb.ProtocolMessage_Multipaxos{
					Multipaxos: &pb.MPxMsg{Type: &pb.MPxMsg_Accept{
						Accept: &pb.MPxAccept{
							Id:     &pb.MPxInstanceId{Sn: i.sn, Lead: uint64(membership.OwnID)},
							Ballot: 0,
							Value:  i.lastVal,
						},
					}},
				},
			}
			fmt.Printf("[MPX][RTX] RESEND Accept sn=%d digest=%x\n", i.sn, i.lastDigest[:8])
			i.parent.emit(pm)
			i.lastAcceptAt = now
		}
	}

	// 2) SB-⊥ (NIL-Deliver) — segurança para não travar o segmento
	if i.enableNilDeliver &&
		i.phase == phaseAcceptSent &&
		i.acceptCount < i.quorum &&
		now.Sub(i.lastAcceptAt) >= i.sbNilAfter {

		fmt.Printf("[MPX][NIL] TIMEOUT → DELIVER ⊥ sn=%d\n", i.sn)

		i.phase = phaseCommitted

		// Gossip do NIL Commit
		nilCommit := &pb.MPxMsg{Type: &pb.MPxMsg_Commit{
			Commit: &pb.MPxCommit{
				Id:    &pb.MPxInstanceId{Sn: i.sn, Lead: uint64(membership.OwnID)},
				Value: nil, // NIL
			},
		}}
		pmOut := &pb.ProtocolMessage{
			SenderId: membership.OwnID,
			Sn:       i.sn,
			Msg:      &pb.ProtocolMessage_Multipaxos{Multipaxos: nilCommit},
		}
		if i.parent.emit != nil {
			i.parent.emit(pmOut)
		}

		// NIL não remove batch nem anuncia
		i.closed = true
		tracing.MainTrace.Event(tracing.COMMIT, int64(i.sn), 0)
	}
}

//
// ============================================================
// Utilidades (corte de batch, fechamento, tracing)
// ============================================================
//

// cutReqBatch bloqueia até haver requests suficientes (size) OU até timeout,
// e retorna o batch cortado dos buckets do segmento.
func (i *mpxInstance) cutReqBatch() *request.Batch {
	if i.seg == nil {
		return nil
	}
	size := i.seg.BatchSize()
	timeout := i.proposeEvery
	i.seg.Buckets().WaitForRequests(int(size), timeout)
	return i.seg.Buckets().CutBatch(int(size), timeout)
}

// Close marca a instância como encerrada (usado em shutdown/GC).
func (i *mpxInstance) Close() {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.closed {
		return
	}
	i.closed = true
	fmt.Printf("[MPX][CHK] instance close sn=%d\n", i.sn)
}

// Traces mínimos (integram com o tracing do MIR e deixam marcas no stdout)
func tracePropose(sn int32, size int) {
	tracing.MainTrace.Event(tracing.PROPOSE, int64(sn), int64(size))
	fmt.Printf("[MPX] PROPOSE sn=%d size=%d\n", sn, size)
}
func traceCommit(sn int32, size int) {
	tracing.MainTrace.Event(tracing.COMMIT, int64(sn), int64(size))
	fmt.Printf("[MPX] COMMIT  sn=%d size=%d\n", sn, size)
}

