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
	"sort"

	"github.com/hyperledger-labs/mirbft/config"
	"github.com/hyperledger-labs/mirbft/membership"
	"github.com/hyperledger-labs/mirbft/messenger"
	pb "github.com/hyperledger-labs/mirbft/protobufs"
	"github.com/hyperledger-labs/mirbft/tracing"
)

// handlerThreadIndex normaliza índice do canal para evitar panic com ClientId negativo
func handlerThreadIndex(clientID int32, threads int) int {
	if threads <= 0 {
		return 0
	}
	if clientID < 0 {
		return 0
	}
	return int(clientID) % threads
}

// ForwardRequestToNodes envia request para nós específicos (usado para cross-op)
func ForwardRequestToNodes(req *pb.ClientRequest, nodeIDs []int32) {
	for _, nodeID := range nodeIDs {
		if nodeID == membership.OwnID {
			// Local: adiciona ao bucket
			if config.Config.RequestHandlerThreads > 0 {
				idx := handlerThreadIndex(req.RequestId.ClientId, config.Config.RequestHandlerThreads)
				requestInputChannels[idx] <- req
			} else {
				AddReqMsg(req)
			}
		} else {
			// Remoto: usa ClientRequestHandler diretamente
			if messenger.ClientRequestHandler != nil {
				messenger.ClientRequestHandler(req)
			}
		}
	}
}

// TODO: It's inefficient to hash a request every time it is needed to get the request ID

// HandleRequest processa request do cliente
// Orderers específicos (como MultipaxosMulticast) podem injetar lógica customizada via callbacks
func HandleRequest(req *pb.ClientRequest) {
	
	tracing.MainTrace.Event(tracing.REQ_RECEIVE, int64(req.RequestId.ClientId), int64(req.RequestId.ClientSn))
	
	// Processa request diretamente (PBFT, ISS, etc)
	if config.Config.RequestHandlerThreads > 0 {
		idx := handlerThreadIndex(req.RequestId.ClientId, config.Config.RequestHandlerThreads)
		requestInputChannels[idx] <- req
	} else {
		AddReqMsg(req)
	}
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