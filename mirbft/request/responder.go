// Copyright 2022 IBM Corp. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package request

import (
	"fmt"
	"sync"
	"time"

	logger "github.com/rs/zerolog/log"

	"github.com/hyperledger-labs/mirbft/log"
	"github.com/hyperledger-labs/mirbft/membership"
	"github.com/hyperledger-labs/mirbft/messenger"
	pb "github.com/hyperledger-labs/mirbft/protobufs"
	"github.com/hyperledger-labs/mirbft/tracing"
)

// ========================== Instrumentação extra ============================

// ResponderBuildTag ajuda a identificar nos logs se a versão nova está rodando.
const ResponderBuildTag = "OUTPUT-PROCESSING-v2.0-ATOMIC-COMMIT"

// Banner de versão impresso apenas uma vez.
var printVersionOnce sync.Once

func printVersionBanner() {
	printVersionOnce.Do(func() {
		fmt.Printf("[RESPONDER][VER] %s started at %s\n",
			ResponderBuildTag, time.Now().Format(time.RFC3339Nano))
	})
}

// ============================= Tipo principal ===============================

// Responder implementa Output Processing do CSMR 
// Coleta outputs das réplicas e aplica função de seleção 
type Responder struct {
	entriesChan chan *log.Entry
	
	// CSMR Replica Mapper : mapeia operações → réplicas
	isMemberFunc func(groupID uint32, nodeID int32) bool
	getGroupMembers func(groupID uint32) []int32
	
	// Deduplicação de respostas para cross-ops
	respondedOps   sync.Map // opID -> time.Time
	respondedOpsMu sync.Mutex
}

// Creates a new responder.
// A responder must be created before any protocol messages can be received from the network.
// Otherwise some responses to the client could be missed (in case entries are committed to the log before
// the responder has been created).
func NewResponder() *Responder {
	printVersionBanner()
	return &Responder{
		entriesChan:  log.Entries(),
	}
}

// SetIsMemberFunc configura Replica Mapper (CSMR Definition 7)
func (r *Responder) SetIsMemberFunc(f func(groupID uint32, nodeID int32) bool) {
	r.isMemberFunc = f
}

// SetGroupMembersFunc configura função R(x) - replication set (Definition 7)
func (r *Responder) SetGroupMembersFunc(f func(groupID uint32) []int32) {
	r.getGroupMembers = f
}

// Observes the log and responds to clients in commit order.
// Meant to be run as a separate goroutine.
// Decrements the provided wait group when done.
func (r *Responder) Start(wg *sync.WaitGroup) {
	defer wg.Done()
	printVersionBanner()
	
	// Cleanup periódico de respondedOps
	// Timeout conservador: 5 minutos para tolerar atrasos
	go func() {
		ticker := time.NewTicker(2 * time.Minute)
		defer ticker.Stop()
		for range ticker.C {
			r.cleanupRespondedOps(5 * time.Minute)
		}
	}()

	// Read log entries (containing ordered batches) from
	// the entries channel until the channel is closed.
	for e := <-r.entriesChan; e != nil; e = <-r.entriesChan {

		// Sanidade do entry/batch
		if e.Batch == nil || len(e.Batch.Requests) == 0 {
			fmt.Printf("[RESPONDER][WARN] sn=%d batch vazio ou nil — nada a enviar\n", e.Sn)
			continue
		}

		// Verifica se este peer deve responder
		if e.ShouldRespond != nil && !*e.ShouldRespond {
			fmt.Printf("[RESPONDER][SKIP] sn=%d shouldRespond=false — pulando respostas\n", e.Sn)
			continue
		}

		nReq := len(e.Batch.Requests)
		fmt.Printf("[RESPONDER][ENTRY] sn=%d nreq=%d\n", e.Sn, nReq)

		// For each ClientRequest in the ordered batch
		sent := 0
		skipped := 0
		for _, req := range e.Batch.Requests {
			if req == nil || req.RequestId == nil {
				fmt.Printf("[RESPONDER][WARN] sn=%d request sem RequestId — ignorando\n", e.Sn)
				continue
			}

			cid := req.RequestId.ClientId
			csn := req.RequestId.ClientSn
			
			// Cross-op: DEDUPLICAÇÃO apenas
			// Atomic order já garantido pelo orderer (expectedGSN)
			if len(req.TouchedGroups) > 1 {
				opID := GenerateOpID(req)
				
				// DEDUPLICAÇÃO: responde apenas uma vez por opID
				if _, loaded := r.respondedOps.LoadOrStore(opID, time.Now()); loaded {
					fmt.Printf("[OUTPUT-PROC][DEDUP] sn=%d opid=%s already responded\n", e.Sn, opID)
					skipped++
					continue
				}
				
				fmt.Printf("[OUTPUT-PROC][CROSS-OP] sn=%d opid=%s group=%d gsn=%d (atomic order by orderer)\n", 
					e.Sn, opID, req.GetGroupId(), req.GSN)
			}
			
			// CSMR Output Processing: filtra por membership
			// Crash model: primeira resposta válida é suficiente
			if r.isMemberFunc != nil && req.GroupId != 0 {
				if !r.isMemberFunc(req.GroupId, membership.OwnID) {
					fmt.Printf("[OUTPUT-PROC][SKIP] sn=%d req=%d group=%d (not in R(x))\n",
						e.Sn, csn, req.GroupId)
					skipped++
					continue
				}
			}

			logger.Trace().
				Int32("clientId", cid).
				Int32("clientSn", csn).
				Int32("sn", e.Sn).
				Msg("Sending response to client.")

			// >>> LOG explícito (envio pelo responder)
			// Mostramos também o "formato" da mensagem que será enviada:
			// ClientResponse{ orderSn=<e.Sn>, clientSn=<csn> }
			fmt.Printf("[RESPONDER][SEND] sn=%d client=%d req=%d msg=ClientResponse{orderSn=%d,clientSn=%d}\n",
				e.Sn, cid, csn, e.Sn, csn)

			// Respond to the corresponding client.
			tracing.MainTrace.Event(tracing.RESP_SEND, int64(cid), int64(csn))

			// A API atual não retorna erro; se existir no seu fork, capture e logue aqui.
			messenger.RespondToClient(cid, &pb.ClientResponse{
				OrderSn:  e.Sn,
				ClientSn: csn,
			})
			sent++
		}

		if skipped > 0 {
			fmt.Printf("[RESPONDER][ENTRY][DONE] sn=%d sent=%d skipped=%d (not member) total=%d\n", 
				e.Sn, sent, skipped, nReq)
		} else {
			fmt.Printf("[RESPONDER][ENTRY][DONE] sn=%d sent=%d/%d\n", e.Sn, sent, nReq)
		}
	}
}



// cleanupRespondedOps remove entradas antigas para evitar vazamento de memória
func (r *Responder) cleanupRespondedOps(timeout time.Duration) {
	now := time.Now()
	cleaned := 0
	
	r.respondedOps.Range(func(key, value interface{}) bool {
		if ts, ok := value.(time.Time); ok {
			if now.Sub(ts) > timeout {
				r.respondedOps.Delete(key)
				cleaned++
			}
		}
		return true
	})
	
	if cleaned > 0 {
		fmt.Printf("[RESPONDER-CLEANUP] removed %d stale opIDs\n", cleaned)
	}
}
