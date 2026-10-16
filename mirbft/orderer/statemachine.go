// Máquina de estados KV por grupo, usada pela API de cliente (clientlib). Mantém um mapa
// chave->valor por grupo de dados, aplicado deterministicamente em toda réplica desse grupo,
// na mesma ordem (ver applyEntryInOrder e o comentário em runSegment, multipaxosorderer.go,
// sobre por que o gancho tem que ser exatamente ali). Completamente isolada do caminho de
// benchmark existente: só processa requisições cujo payload decodifica como um dos verbos de
// request.DecodeAPICommand ("APIPUT"/"APIGET"/"APIDELETE"/"APITRANSFER"); qualquer outro payload (todo o
// tráfego de benchmark, "GET K00000005" etc.) é ignorado aqui, igual antes desta feature existir.
package orderer

import (
	"encoding/binary"
	"sync"

	pb "github.com/hyperledger-labs/mirbft/protobufs"
	"github.com/hyperledger-labs/mirbft/request"
)

// GroupState é o KV store de um grupo de dados: um mapa chave->valor, replicado
// implicitamente (cada réplica do grupo mantém sua própria cópia, aplicando os mesmos
// comandos na mesma ordem).
type GroupState struct {
	mu   sync.RWMutex
	data map[string][]byte
}

var groupStates sync.Map // uint32 groupID -> *GroupState

func getGroupState(groupID uint32) *GroupState {
	if v, ok := groupStates.Load(groupID); ok {
		return v.(*GroupState)
	}
	gs := &GroupState{data: make(map[string][]byte)}
	actual, _ := groupStates.LoadOrStore(groupID, gs)
	return actual.(*GroupState)
}

func decodeInt64(b []byte) int64 {
	if len(b) != 8 {
		return 0
	}
	return int64(binary.BigEndian.Uint64(b))
}

func encodeInt64(v int64) []byte {
	b := make([]byte, 8)
	binary.BigEndian.PutUint64(b, uint64(v))
	return b
}

func (gs *GroupState) put(key string, value []byte) []byte {
	gs.mu.Lock()
	defer gs.mu.Unlock()
	gs.data[key] = append([]byte{}, value...)
	return value
}

func (gs *GroupState) get(key string) ([]byte, bool) {
	gs.mu.RLock()
	defer gs.mu.RUnlock()
	v, ok := gs.data[key]
	return v, ok
}

// delete remove key e devolve se ela existia antes (idempotente, igual delete() de map do Go
// -- chamar de novo numa chave já ausente não é erro, só devolve false).
func (gs *GroupState) delete(key string) bool {
	gs.mu.Lock()
	defer gs.mu.Unlock()
	_, existed := gs.data[key]
	delete(gs.data, key)
	return existed
}

// addBalance soma delta ao int64 (big-endian) armazenado em key e devolve o novo valor.
// Sem atomicidade/rollback entre grupos -- ver limitação documentada no plano: débito e
// crédito de um Transfer aplicam incondicionalmente, cada um na sua réplica, só com ordem
// relativa consistente garantida pelo GSN/ADeliver, não uma transação 2PC.
func (gs *GroupState) addBalance(key string, delta int64) []byte {
	gs.mu.Lock()
	defer gs.mu.Unlock()
	next := decodeInt64(gs.data[key]) + delta
	gs.data[key] = encodeInt64(next)
	return gs.data[key]
}

// reqKey combina clientId+clientSn num único int64, usado como chave dos mapas de resultado.
func reqKey(clientID, clientSn int32) int64 {
	return int64(clientID)<<32 | int64(uint32(clientSn))
}

// ApplyBatch decodifica e aplica toda requisição da API de cliente no batch cujo lado
// (fromGroup/toGroup, ou grupo único de Put/Get) é groupID, na ordem em que aparecem no
// batch (ordem já consistente entre réplicas -- é literalmente o valor sobre o qual o
// MultiPaxos já decidiu). Requisições que não decodificam como um comando da API (todo o
// tráfego de benchmark existente) são puladas silenciosamente, preservando exatamente o
// comportamento de antes desta feature.
func ApplyBatch(groupID uint32, sn int32, batch *pb.Batch) map[int64]*pb.ClientResponse {
	results := make(map[int64]*pb.ClientResponse)
	if batch == nil {
		return results
	}
	gs := getGroupState(groupID)
	for _, req := range batch.Requests {
		if req == nil || req.RequestId == nil {
			continue
		}
		cmd, ok := request.DecodeAPICommand(req.Payload)
		if !ok {
			continue
		}
		resp := &pb.ClientResponse{OrderSn: sn, ClientSn: req.RequestId.ClientSn}
		switch cmd.Verb {
		case request.VerbPut:
			resp.Value, resp.Found = gs.put(cmd.Key, cmd.Value), true
		case request.VerbGet:
			resp.Value, resp.Found = gs.get(cmd.Key)
		case request.VerbDelete:
			resp.Found = gs.delete(cmd.Key) // Found aqui = "existia antes de apagar"
		case request.VerbTransfer:
			switch groupID {
			case cmd.FromGroup:
				resp.Value, resp.Found = gs.addBalance(cmd.Key, -cmd.Amount), true
			case cmd.ToGroup:
				resp.Value, resp.Found = gs.addBalance(cmd.ToKey, cmd.Amount), true
			default:
				continue // este grupo não é nenhum dos dois lados -- não deveria acontecer
			}
		default:
			continue
		}
		results[reqKey(req.RequestId.ClientId, req.RequestId.ClientSn)] = resp
	}
	return results
}

var lastAppliedMu sync.Mutex
var lastApplied = map[uint32]int32{} // groupID -> maior SN já aplicado

// applyEntryInOrder é chamada de dentro do laço runSegment de cada MultiPaxosOrderer (o
// único ponto que já visita os SNs de um grupo estritamente em ordem crescente, em toda
// réplica -- ver orderer/multipaxosorderer.go). Idempotente contra o laço revisitar o mesmo
// SN (ramos <-commitCh e <-t.C podem ambos acabar chamando pro mesmo SN).
func (o *MultiPaxosOrderer) applyEntryInOrder(groupID uint32, sn int32, batch *pb.Batch) {
	lastAppliedMu.Lock()
	if sn <= lastApplied[groupID] {
		lastAppliedMu.Unlock()
		return
	}
	lastApplied[groupID] = sn
	lastAppliedMu.Unlock()

	if batch == nil || len(batch.Requests) == 0 {
		return
	}
	results := ApplyBatch(groupID, sn, batch)
	if len(results) > 0 && GetGlobalMulticastOrderer() != nil {
		GetGlobalMulticastOrderer().NotifyProxy(batch, sn, results)
	}
}
