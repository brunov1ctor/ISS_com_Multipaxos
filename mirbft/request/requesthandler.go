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
	logger "github.com/rs/zerolog/log"
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

// HandleRequest processa request do cliente e implementa atomic multicast:
// - Qualquer nó pode atuar como proxy (não há gargalo de proxy único)
// - GSN sequencer (grupo 0) garante ordem global determinística
// - META publicado apenas uma vez por proxy para evitar duplicação
func HandleRequest(req *pb.ClientRequest) {

	if len(req.TouchedGroups) == 0 {
		// ✅ REPLICA MAPPER: Proxy decide TouchedGroups automaticamente
		req.TouchedGroups = ReplicaMapper(req.Payload)
		logger.Info().
			Interface("touchedGroups", req.TouchedGroups).
			Str("payload", string(req.Payload)[:min(50, len(req.Payload))]).
			Msg("[REPLICA-MAPPER] Proxy mapped payload to groups")
	} else {
		// TouchedGroups já definido - valida consistência
		mappedGroups := ReplicaMapper(req.Payload)
		if !equalGroups(req.TouchedGroups, mappedGroups) {
			logger.Warn().
				Interface("provided", req.TouchedGroups).
				Interface("mapped", mappedGroups).
				Msg("[REPLICA-MAPPER] TouchedGroups mismatch - using provided")
		}
	}
	
	// ✅ NORMALIZAÇÃO: Sempre normaliza TouchedGroups (ordena e remove duplicatas)
	req.TouchedGroups = normalizeGroups(req.TouchedGroups)
	
	tracing.MainTrace.Event(tracing.REQ_RECEIVE, int64(req.RequestId.ClientId), int64(req.RequestId.ClientSn))
	
	// TODAS as requests precisam de GSN (conforme artigo)
	if req.GSN == 0 {
		// GSN não atribuído: qualquer nó pode atuar como proxy
		// Obtém GSN via sequenciador global (grupo 0)
		if gsnGenerator == nil {
			logger.Error().
				Int32("clientID", req.RequestId.ClientId).
				Int32("clientSn", req.RequestId.ClientSn).
				Msg("[ATOMIC-MCAST] GSN generator not set - skipping request")
			return
		}
		req.GSN = gsnGenerator()
		
		// ✅ CORREÇÃO: Publica META apenas UMA vez por proxy (evita duplicação)
		if metaPublisher != nil {
			metaPublisher(req.GSN, req.TouchedGroups)
			logger.Info().
				Uint64("gsn", req.GSN).
				Interface("touchedGroups", req.TouchedGroups).
				Int32("proxyNode", membership.OwnID).
				Msg("[MULTI-PROXY] Published META once (no duplication)")
		}
		
		// ✅ LIVENESS: Cache request para re-forward
		if requestCacher != nil {
			requestCacher(req.GSN, req)
		}
		
		opID := GenerateOpID(req)
		logger.Info().
			Int32("clientID", req.RequestId.ClientId).
			Int32("clientSn", req.RequestId.ClientSn).
			Str("opID", opID).
			Uint64("gsn", req.GSN).
			Int32("proxyNode", membership.OwnID).
			Int("numGroups", len(req.TouchedGroups)).
			Msg("[MULTI-PROXY] Node acting as proxy, assigned GSN")
	} else {
		// GSN já atribuído (forwarded): apenas enfileirar localmente
		logger.Info().
			Int32("clientID", req.RequestId.ClientId).
			Uint64("gsn", req.GSN).
			Uint32("groupId", req.GroupId).
			Msg("[ATOMIC-MCAST] Request received with GSN, enqueuing locally")
		
		if config.Config.RequestHandlerThreads > 0 {
			idx := handlerThreadIndex(req.RequestId.ClientId, config.Config.RequestHandlerThreads)
			requestInputChannels[idx] <- req
		} else {
			AddReqMsg(req)
		}
		return
	}
	
	gsn := req.GSN
	
	// Fanout para todos os grupos tocados (single ou multi-group)
	for _, groupID := range req.TouchedGroups {
		clone := &pb.ClientRequest{
			RequestId:     req.RequestId,
			Payload:       req.Payload,
			Signature:     req.Signature,
			Pubkey:        req.Pubkey,
			GroupId:       groupID,
			TouchedGroups: req.TouchedGroups,
			GSN:           gsn,
		}
		
		logger.Info().
			Int32("clientID", clone.RequestId.ClientId).
			Int32("clientSn", clone.RequestId.ClientSn).
			Uint32("groupId", groupID).
			Uint64("gsn", gsn).
			Msg("[ATOMIC-MCAST] Forwarding to group members (global order)")
		
		// Forward para todos os membros do grupo via callback
		if groupMembersGetter != nil {
			members := groupMembersGetter(groupID)
			if members != nil && len(members) > 0 {
				ForwardRequestToNodes(clone, members)
			}
		} else {
			panic("[ATOMIC-MCAST] Group members getter not set (orderer must inject it)")
		}
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