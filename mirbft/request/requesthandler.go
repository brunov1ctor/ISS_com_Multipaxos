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
	"math/big"

	"github.com/hyperledger-labs/mirbft/config"
	"github.com/hyperledger-labs/mirbft/crypto"
	pb "github.com/hyperledger-labs/mirbft/protobufs"
	"github.com/hyperledger-labs/mirbft/tracing"
	logger "github.com/rs/zerolog/log"
)

// TODO: It's inefficient to hash a request every time it is needed to get the request ID

// This function is used by the messenger as the handler function for requests (the main file performs the assignment).
// Simply adds the received request to the corresponding request buffer.
// TODO: If too many threads (64 or more in the current deployment with 32-core machines) invoke Add(),
//       the buffer locks get extremely contended.
//       Have only a fixed (configurable) number of threads invoking Add().
//       Spawn those worker threads in the Init() function and make HandleRequest (this function) only write
//       the request to a channel (do we need a big channel buffer for this?) that a worker reads.
//       It would make sense to send requests from the same client to the same worker,
//       Since the Buffer lock to be acquired by the worker is determined by the clientID.
//       The lock being acquired by the same thread is crucial for avoiding contention.
//       If this is not enough, try having the worker threads add requests to buffers in batches.
//       (Although this might be very tricky if we want to avoid verifying signatures while holding the buffer lock,
//       and at the same time avoid verifying the signature again, in case the request is already present.)
func HandleRequest(req *pb.ClientRequest) {

	// Fix: preencher TouchedGroups automaticamente se vazio
	if len(req.TouchedGroups) == 0 {
		req.TouchedGroups = []uint32{req.GroupId}
	}

	tracing.MainTrace.Event(tracing.REQ_RECEIVE, int64(req.RequestId.ClientId), int64(req.RequestId.ClientSn))

	// Log VISÍVEL de chegada da request (client → peer)
	// Usa GetBucketNr() para respeitar regra multi-grupo → bucket 0
	bucketNr := GetBucketNr(req)
	logger.Info().
		Int32("clientID", req.RequestId.ClientId).
		Int32("clientSn", req.RequestId.ClientSn).
		Int("bucketID", bucketNr).
		Uint32("groupId", req.GroupId).
		Int("numTouchedGroups", len(req.TouchedGroups)).
		Msg("[REQ] ClientRequest received -> bucket")

	if config.Config.RequestHandlerThreads > 0 {
		requestInputChannels[int(req.RequestId.ClientId)%config.Config.RequestHandlerThreads] <- req
	} else {
		AddReqMsg(req)
	}

	// Após enfileirar, logar o tamanho aproximado do bucket
	logger.Debug().
		Int("bucketID", bucketNr).
		Int("bucketLenApprox", Buckets[bucketNr].Len()).
		Msg("[REQ] Bucket size after enqueue")
}

// GetBucketByHashing retorna bucket usando apenas GroupId ou hash
// ATENÇÃO: NÃO considera TouchedGroups (multi-grupo)!
// Para roteamento real, use GetBucketNr() que implementa:
//   - Multi-grupo (len(TouchedGroups) > 1) → bucket 0 (GROUP_GLOBAL)
//   - Single-grupo → bucket baseado em GroupId ou hash
// Esta função é mantida apenas para compatibilidade/casos especiais.
func GetBucketByHashing(req *pb.ClientRequest) *Bucket {
	// Usa GroupId diretamente como bucketID
	if req.GroupId > 0 {
		bucketIndex := int(req.GroupId)
		// Garante que não excede o tamanho do array Buckets
		if bucketIndex >= len(Buckets) {
			bucketIndex = bucketIndex % len(Buckets)
		}
		return Buckets[bucketIndex]
	}
	
	// Fallback: hash-based routing para requests sem GroupId
	H := new(big.Int)
	H.SetString(crypto.Hspace, 10)

	bucketSize := new(big.Int).Div(H, big.NewInt(int64(config.Config.NumBuckets)))

	reqKey := new(big.Int)
	reqKey.SetBytes(crypto.Hash(RequestIDToBytes(req)))

	I := new(big.Int).Div(reqKey, bucketSize)
	i := I.Uint64()

	if i > uint64(config.Config.NumBuckets-1) {
		panic("Request beyond bucket limits")
	}

	return Buckets[i]
}
