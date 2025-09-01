package orderer

import (
	"sync"
	"time"

	logger "github.com/rs/zerolog/log"

	"github.com/hyperledger-labs/mirbft/manager"
	pb "github.com/hyperledger-labs/mirbft/protobufs"
)

// multiPaxosInstance representa uma decisão (sn,bucket).
type multiPaxosInstance struct {
	mu   sync.Mutex
	id   instID
	seg  *manager.Segment // TODO: adapte ao tipo real de Segment no seu projeto
	ord  *MultiPaxosOrderer

	// Paxos state
	segmentPrepared bool
	promisedBallot  uint64
	acceptedBallot  uint64
	acceptedValue   *pb.MPxValue

	// Líder
	ballot uint64
	// Quórum tracking
	promiseCount map[uint64]int // por ballot
	acceptCount  map[uint64]int // por ballot
}

func newMultiPaxosInstance(id instID, seg *manager.Segment, ord *MultiPaxosOrderer) *multiPaxosInstance {
	return &multiPaxosInstance{
		id:           id,
		seg:          seg,
		ord:          ord,
		promiseCount: make(map[uint64]int),
		acceptCount:  make(map[uint64]int),
	}
}

func (i *multiPaxosInstance) pbID() *pb.MPxInstanceId {
	return &pb.MPxInstanceId{Sn: i.id.sn, Bucket: i.id.bucket}
}

// ===== Net handling =====

func (i *multiPaxosInstance) handleNetMessage(from int32, m *pb.MPxMsg) {
	switch t := m.Type.(type) {
	case *pb.MPxMsg_Prepare:
		i.onPrepare(from, t.Prepare)
	case *pb.MPxMsg_Promise:
		i.onPromise(from, t.Promise)
	case *pb.MPxMsg_Accept:
		i.onAccept(from, t.Accept)
	case *pb.MPxMsg_Accepted:
		i.onAccepted(from, t.Accepted)
	case *pb.MPxMsg_Commit:
		i.onCommit(from, t.Commit)
	default:
		logger.Warn().Msg("unknown MPx message")
	}
}

// ===== Fase 1 =====

func (i *multiPaxosInstance) maybePhase1() {
	i.mu.Lock()
	if i.segmentPrepared {
		i.mu.Unlock()
		return
	}
	// Ballot único por segmento: (segID<<32)|OwnID – aqui usamos timestamp como placeholder
	i.ballot = uint64(time.Now().UnixNano())
	prep := &pb.MPxPrepare{Id: i.pbID(), Ballot: i.ballot}
	i.mu.Unlock()

	i.ord.bcast(i.id, &pb.MPxMsg{Type: &pb.MPxMsg_Prepare{Prepare: prep}})

	i.mu.Lock()
	i.segmentPrepared = true
	i.mu.Unlock()
}

func (i *multiPaxosInstance) onPrepare(from int32, p *pb.MPxPrepare) {
	i.mu.Lock()
	defer i.mu.Unlock()

	if p.Ballot >= i.promisedBallot {
		i.promisedBallot = p.Ballot
		// Responde Promise
		resp := &pb.MPxPromise{
			Id:          i.pbID(),
			Ballot:      p.Ballot,
			HasAccepted: i.acceptedValue != nil,
			AccBallot:   i.acceptedBallot,
			AccValue:    i.acceptedValue, // <-- era *i.acceptedValue (errado)
		}
		go i.ord.send(from, i.id, &pb.MPxMsg{Type: &pb.MPxMsg_Promise{Promise: resp}})
	}
}

// ===== Fase 2 =====

func (i *multiPaxosInstance) onPromise(from int32, pr *pb.MPxPromise) {
	i.mu.Lock()
	defer i.mu.Unlock()

	if pr.Ballot != i.ballot {
		return // promessa para outro ballot
	}

	i.promiseCount[i.ballot]++

	var chosen *pb.MPxValue
	if pr.HasAccepted && pr.AccValue != nil {
		// Preferir o maior acc_ballot
		if chosen == nil || pr.AccBallot >= i.acceptedBallot {
			chosen = pr.AccValue // <-- era &v (ponteiro de ponteiro)
		}
	}

	// Quorum atingido?
	if i.promiseCount[i.ballot] >= majority() {
		if chosen == nil {
			// TODO: cortar batch do buffer do segmento
			chosen = &pb.MPxValue{
				Id:     i.pbID(), // <-- era *i.pbID()
				Batch:  []byte{},
				Digest: []byte{},
			}
		}
		acc := &pb.MPxAccept{Id: i.pbID(), Ballot: i.ballot, Value: chosen} // Value é *pb.MPxValue
		go i.broadcastAccept(acc)
	}
}

func (i *multiPaxosInstance) broadcastAccept(a *pb.MPxAccept) {
	i.ord.bcast(i.id, &pb.MPxMsg{Type: &pb.MPxMsg_Accept{Accept: a}})
}

func (i *multiPaxosInstance) onAccept(from int32, a *pb.MPxAccept) {
	i.mu.Lock()
	defer i.mu.Unlock()

	if a.Ballot >= i.promisedBallot {
		i.promisedBallot = a.Ballot
		i.acceptedBallot = a.Ballot
		i.acceptedValue = a.Value // <-- era &val
		go i.ord.bcast(i.id, &pb.MPxMsg{Type: &pb.MPxMsg_Accepted{Accepted: &pb.MPxAccepted{Id: i.pbID(), Ballot: a.Ballot}}})
	}
}

func (i *multiPaxosInstance) onAccepted(from int32, ac *pb.MPxAccepted) {
	i.mu.Lock()
	defer i.mu.Unlock()

	i.acceptCount[ac.Ballot]++
	if i.acceptCount[ac.Ballot] >= majority() && i.acceptedValue != nil {
		val := i.acceptedValue
		go i.commit(val)
		go i.ord.bcast(i.id, &pb.MPxMsg{Type: &pb.MPxMsg_Commit{Commit: &pb.MPxCommit{Id: i.pbID(), Value: val}}}) // Value é *pb.MPxValue
	}
}

func (i *multiPaxosInstance) onCommit(from int32, c *pb.MPxCommit) {
	i.commit(c.Value) // <-- era &c.Value
}

// ===== Commit =====

func (i *multiPaxosInstance) commit(val *pb.MPxValue) {
	// TODO: remover batch do buffer (se aplicável) e anunciar ao SMR
	announceCommit(i.id.sn, val.Batch, val.Digest)
}

