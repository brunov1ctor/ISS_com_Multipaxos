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
	"bytes"
	"fmt"
	"sync"
	"time"

	logger "github.com/rs/zerolog/log"

	"github.com/hyperledger-labs/mirbft/log"
	"github.com/hyperledger-labs/mirbft/messenger"
	pb "github.com/hyperledger-labs/mirbft/protobufs"
	"github.com/hyperledger-labs/mirbft/tracing"
)

// Prefixo para mensagens internas do protocolo (GSN/META)
var systemPrefix = []byte("SYSTEM:")

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
	
	// Output Processing: primeira resposta válida (como no artigo)
	respondedOps sync.Map // opID -> bool
}

// Creates a new responder.
// A responder must be created before any protocol messages can be received from the network.
// Otherwise some responses to the client could be missed (in case entries are committed to the log before
// the responder has been created).
func NewResponder() *Responder {
	printVersionBanner()
	return &Responder{
		// Usa EntriesOutOfOrder() para suportar SN intercalado
		entriesChan:  log.EntriesOutOfOrder(),
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

		// CSMR: In multicast mode, the proxy handles responses via COMMIT_NOTIFY.
		// The Responder only responds for group 0 (sequencer) system messages
		// or when isMemberFunc is nil (non-multicast mode like PBFT/Raft).
		if r.isMemberFunc != nil {
			// Multicast mode: skip responding here — NotifyProxy handles it
			fmt.Printf("[RESPONDER][CSMR-SKIP] sn=%d nreq=%d (proxy handles via COMMIT_NOTIFY)\n", e.Sn, len(e.Batch.Requests))
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
			
			// Não responda operações internas do protocolo (GSN/META) como se fossem cliente
			if bytes.HasPrefix(req.Payload, systemPrefix) {
				skipped++
				continue
			}

			cid := req.RequestId.ClientId
			csn := req.RequestId.ClientSn

			logger.Trace().
				Int32("clientId", cid).
				Int32("clientSn", csn).
				Int32("sn", e.Sn).
				Msg("Sending response to client.")

			fmt.Printf("[RESPONDER][SEND] sn=%d client=%d req=%d msg=ClientResponse{orderSn=%d,clientSn=%d}\n",
				e.Sn, cid, csn, e.Sn, csn)

			tracing.MainTrace.Event(tracing.RESP_SEND, int64(cid), int64(csn))

			messenger.RespondToClient(cid, &pb.ClientResponse{
				OrderSn:  e.Sn,
				ClientSn: csn,
			})
			sent++
		}

		if skipped > 0 {
			fmt.Printf("[RESPONDER][ENTRY][DONE] sn=%d sent=%d skipped=%d (system) total=%d\n", 
				e.Sn, sent, skipped, nReq)
		} else {
			fmt.Printf("[RESPONDER][ENTRY][DONE] sn=%d sent=%d/%d\n", e.Sn, sent, nReq)
		}
	}
}
