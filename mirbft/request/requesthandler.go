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
	"sort"

	"github.com/hyperledger-labs/mirbft/config"
	"github.com/hyperledger-labs/mirbft/membership"
	"github.com/hyperledger-labs/mirbft/messenger"
	pb "github.com/hyperledger-labs/mirbft/protobufs"
	"github.com/hyperledger-labs/mirbft/tracing"
)

// handlerThreadIndex normaliza índice do canal para evitar panic com ClientId negativo
// Distribui por clientSn para evitar hotspot quando há poucos clientes
func handlerThreadIndex(clientID int32, clientSn int32, threads int) int {
	if threads <= 0 {
		return 0
	}
	// Hash simples: combina clientId e clientSn para distribuir melhor
	hash := uint32(clientID)*31 + uint32(clientSn)
	return int(hash % uint32(threads))
}

// ForwardRequestToNodes envia request para nós específicos (usado para cross-op)
// Usa conexões gRPC existentes (mesma infraestrutura do client)
func ForwardRequestToNodes(req *pb.ClientRequest, nodeIDs []int32) {
	// Garante RequestId nunca é nil
	rid := req.GetRequestId()
	if rid == nil {
		fmt.Printf("[FORWARD][ERROR] Request with nil RequestId, dropping\n")
		return
	}
	
	fmt.Printf("[FORWARD] Sending request clientId=%d clientSn=%d to nodes %v\n", 
		rid.GetClientId(), rid.GetClientSn(), nodeIDs)
	
	for _, nodeID := range nodeIDs {
		if nodeID == membership.OwnID {
			// Local: adiciona ao bucket
			fmt.Printf("[FORWARD] Local node %d: adding to bucket\n", nodeID)
			if config.Config.RequestHandlerThreads > 0 {
				idx := handlerThreadIndex(rid.GetClientId(), rid.GetClientSn(), config.Config.RequestHandlerThreads)
				requestInputChannels[idx] <- req
			} else {
				AddReqMsg(req)
			}
		} else {
			// ✅ CORREÇÃO: Envia via gRPC usando infraestrutura existente
			fmt.Printf("[FORWARD] Remote node %d: sending via gRPC\n", nodeID)
			pm := &pb.ProtocolMessage{
				SenderId: membership.OwnID,
				Sn:       -1,
				Msg: &pb.ProtocolMessage_GsnReqForward{
					GsnReqForward: &pb.GSNReqForward{
						Req: req,
					},
				},
			}
			messenger.EnqueueMsg(pm, nodeID)
		}
	}
}

// HandleRequest processa request do cliente
// Orderers específicos podem injetar lógica customizada via callbacks
func HandleRequest(req *pb.ClientRequest) {
	// ✅ VALIDAÇÃO: Garante RequestId nunca é nil
	rid := req.GetRequestId()
	if rid == nil {
		fmt.Printf("[HANDLE-REQ][ERROR] Request with nil RequestId, dropping\n")
		return
	}
	
	payloadPreview := req.Payload
	if len(payloadPreview) > 50 {
		payloadPreview = payloadPreview[:50]
	}
	fmt.Printf("[HANDLE-REQ][ENTRY] clientId=%d clientSn=%d groupId=%d payload=%s\n", 
		rid.GetClientId(), rid.GetClientSn(), req.GroupId, string(payloadPreview))
	
	// GSN_RESPONSE não deve entrar no fluxo normal
	// Deve ser processado diretamente pelo HandleMessage do multicast orderer
	if len(req.Payload) > 0 && string(req.Payload[:min(len(req.Payload), 20)]) == "SYSTEM:GSN_RESPONSE:" {
		fmt.Printf("[HANDLE-REQ] GSN_RESPONSE detected, should be handled by HandleMessage, dropping\n")
		return
	}
	
	tracing.MainTrace.Event(tracing.REQ_RECEIVE, int64(rid.GetClientId()), int64(rid.GetClientSn()))
	
	// Preprocessor customizado (ex: atomic multicast)
	if requestPreprocessor != nil {
		fmt.Printf("[HANDLE-REQ] Calling preprocessor...\n")
		if requestPreprocessor(req) {
			fmt.Printf("[HANDLE-REQ] Preprocessor handled request, returning\n")
			return // Preprocessor já processou
		}
		fmt.Printf("[HANDLE-REQ] Preprocessor returned false, req now: groupId=%d touchedGroups=%v\n", req.GroupId, req.TouchedGroups)
	}
		AddReqMsg(req)
}


// equalGroups verifica se dois slices de grupos são iguais como conjuntos
// Normaliza ordem e remove duplicatas antes de comparar
func equalGroups(a, b []uint32) bool {
	// Normaliza ambos os slices
	normA := normalizeGroups(a)
	normB := normalizeGroups(b)
	
	if len(normA) != len(normB) {
		return false
	}
	for i, v := range normA {
		if v != normB[i] {
			return false
		}
	}
	return true
}

// normalizeGroups ordena e remove duplicatas de um slice de grupos
func normalizeGroups(groups []uint32) []uint32 {
	if len(groups) == 0 {
		return groups
	}
	
	// Copia para não modificar o original
	normalized := make([]uint32, len(groups))
	copy(normalized, groups)
	
	// Ordena
	sort.Slice(normalized, func(i, j int) bool {
		return normalized[i] < normalized[j]
	})
	
	// Remove duplicatas
	unique := normalized[:0]
	for i, group := range normalized {
		if i == 0 || group != normalized[i-1] {
			unique = append(unique, group)
		}
	}
	
	return unique
}

// min retorna o menor de dois inteiros
func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}