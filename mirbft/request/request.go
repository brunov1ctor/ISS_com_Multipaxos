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
	"encoding/binary"
	"fmt"
	"strings"
	"sync"

	logger "github.com/rs/zerolog/log"
	"github.com/hyperledger-labs/mirbft/config"
	"github.com/hyperledger-labs/mirbft/crypto"
	"github.com/hyperledger-labs/mirbft/membership"
	pb "github.com/hyperledger-labs/mirbft/protobufs"
)

var (
	// Uncommitted requests received from clients, organized in buffers indexed by client ID.
	// For each known client, this map (indexed by client ID) contains a buffer of requests from that client.
	// This data structure is necessary to filter out ClientRequests that are outside of the client watermark window.
	// It buffers up to config.Config.ClientRequestBacklogSize client requests ahead of the watermark window.
	buffers = make(map[int32]*Buffer)

	// Lock to guard the map of request Buffers.
	// For every incoming request we need to look up (by client ID) to which Buffer to put the request.
	// TODO: This lock is on the critical path all requests!
	//       Even though it is almost only RLocked, this can cause cache contention among threads that access it.
	//       With many threads, this is known to have negative performance impacts.
	//       Test if this is the case and do something about it.
	//       sync.Map could help here. For the price of quite a lot of pointer chasing (probably only in cache),
	//       we remove all contention (the way we use the map avoids contention completely).
	buffersLock sync.RWMutex

	// Holds all the buckets to which client requests are added after being added to Buffers.
	Buckets []*Bucket

	// Channels to which gRPC threads are writing incoming requests.
	// A configurable number of threads (config.Config.RequestHandlerThreads) reads these channels and puts the requests
	// in corresponding Buffers. This indirection is required to avoid cache contention on the Buffer locks when there
	// are many clients (more than the number of physical cores of the machine).
	requestInputChannels []chan *pb.ClientRequest

	// Request verifier data structures.
	verifierChan chan *Request // Channel to which requests are written for verifying

	// Function used for verifying request batches. Set during initialization.
	batchVerifierFunc func(*Batch) bool
	
	// Proxy interceptor para atribuir GSN
	proxyInterceptor func(*Request)
	
	// GSN barrier checker para atomic global order
	gsnBarrierChecker func(uint64) bool
	
	// GSN generator (injected by orderer to avoid import cycle)
	gsnGenerator func() uint64
	
	// Group members getter (injected by orderer to avoid import cycle)
	groupMembersGetter func(uint32) []int32
	
	// META publisher (injected by orderer to avoid import cycle)
	metaPublisher func(uint64, []uint32)
	
	// ✅ LIVENESS: Callback para marcar request como recebida
	requestReceivedMarker func(uint64, uint32)
	
	// ✅ LIVENESS: Callback para cache de requests
	requestCacher func(uint64, *pb.ClientRequest)
	
	// Preprocessor customizado (ex: atomic multicast)
	requestPreprocessor func(*pb.ClientRequest) bool
)

type watermarkRange struct {
	oldWM int32
	newWM int32
}

// Initialize the request package.
// Cannot be part of the init() function, as the configuration file is not yet loaded when init() is executed.
func Init() {

	// Initializes the Buckets.
	Buckets = make([]*Bucket, config.Config.NumBuckets)
	for i := range Buckets {
		Buckets[i] = NewBucket(i)
	}

	// Initialize request handler goroutines.
	// These threads are reading incoming requests from (buffered) input channels and putting them in Buffers / Buckets.
	// The gRPC threads (one per client) are writing these requests to the channels.
	// The number of request handler threads is limited on purpose to avoid cache contention when accessing the Buffers.
	// If the gRPC threads were to do this directly, cache contention could occur in deployments with many clients.
	requestInputChannels = make([]chan *pb.ClientRequest, config.Config.RequestHandlerThreads, config.Config.RequestHandlerThreads)
	for i := 0; i < config.Config.RequestHandlerThreads; i++ {

		// Create new request input channel and a thread that reads that reads requests from the channel
		// and adds those requests to the corresponding Buffers
		// TODO: Implement graceful shutdown!
		requestInputChannels[i] = make(chan *pb.ClientRequest, config.Config.RequestInputChannelBuffer)
		go func(i int) {
			for req := range requestInputChannels[i] {
				AddReqMsg(req)
			}
		}(i)
	}

	if config.Config.BatchVerifier == "sequential" {
		batchVerifierFunc = checkSignaturesSequential
	} else if config.Config.BatchVerifier == "parallel" {
		batchVerifierFunc = checkSignaturesParallel
	} else if config.Config.BatchVerifier == "external" {
		batchVerifierFunc = checkSignaturesExternal

		// Initialize request verifier goroutines.
		// These are a fixed number of threads only verifying request signatures.
		// The capacity of the verifier channel buffer is chosen such that one full batch for each verifier fits inside.
		// To stop the goroutines, close the verifier channel.
		verifierChan = make(chan *Request, config.Config.BatchSize*config.Config.RequestHandlerThreads)
		for i := 0; i < config.Config.RequestHandlerThreads; i++ {
			go func() {

				// Reads requests from the verifier channel,
				// verifies them and writes them to the channel indicated by the Request.
				for req := range verifierChan {
					if err := crypto.CheckSig(req.Digest,
						membership.ClientPubKey(req.Msg.RequestId.ClientId),
						req.Msg.Signature); err == nil {
						req.Verified = true
					}
					req.VerifiedChan <- req
				}

			}()
		}
	} else {
		logger.Fatal().Str("name", config.Config.BatchVerifier).Msg("Unknown batch verifier.")
	}

}

// Represents a client request and stores request-specific metadata.
type Request struct {

	// The request message received over the network.
	Msg *pb.ClientRequest

	// Digest of the request.
	Digest []byte

	// Pointer to the buffer the request maps to (through its client ID).
	// This, except for convenience, avoids acquiring the read lock on the Buffer map when looking up this request's Buffer.
	Buffer *Buffer

	// Pointer to the bucket the request maps to.
	// Note that this does not necessarily mean that the request is currently inserted in that bucket!
	// It only means that IF the request is in a bucket, this is the bucket.
	Bucket *Bucket

	// Flag indicating whether the request signature has been verified.
	Verified bool

	// Request is "in flight", i.e., has been added to or observed in some (protocol-specific) proposal message.
	// This flag tracks duplication.
	// A request should be marked as in flight upon being either added to or upon encountered in a proposal message.
	// If a request is already proposed it should not be found in any other proposal message.
	InFlight bool

	// Requests are stored in a doubly linked list in the bucket for constant-time (random-access) adding and removing
	// while still being able to use it as a FIFO queue.
	// We do not encapsulate the Request in a container "Element" struct to avoid allocations of those elements.
	// Both Next and Prev must be set to nil when the request is not in a bucket.
	Next *Request
	Prev *Request

	// Channel to which the verifying goroutine should write the verified request.
	// During verification of a batch, a channel is created and assigned to this field for all requests in the batch.
	// Then, the requests are written to a (different) channel for the verifier goroutines to process them.
	// When a verifier has verified the request's signature, it writes it to this channel in order to notify the
	// batch verification method that the verification of this request finished.
	VerifiedChan chan *Request

	// CROSS-OP SUPPORT (atomic multicast)
	// OpID: identificador determinístico da operação
	OpID string

	// GSN: Global Sequence Number para cross-ops
	GSN uint64
}

// SetProxyInterceptor configura interceptor do proxy
func SetProxyInterceptor(fn func(*Request)) {
	proxyInterceptor = fn
}

// SetGSNBarrierChecker configura checker de barreira GSN
func SetGSNBarrierChecker(fn func(uint64) bool) {
	gsnBarrierChecker = fn
}

// SetGSNGenerator configura gerador de GSN (injected by orderer)
func SetGSNGenerator(fn func() uint64) {
	gsnGenerator = fn
}

// SetGroupMembersGetter configura getter de membros de grupo (injected by orderer)
func SetGroupMembersGetter(fn func(uint32) []int32) {
	groupMembersGetter = fn
}

// SetMETAPublisher configura publisher de META (injected by orderer)
func SetMETAPublisher(fn func(uint64, []uint32)) {
	metaPublisher = fn
}

// ✅ LIVENESS: SetRequestReceivedMarker configura callback para marcar requests recebidas
func SetRequestReceivedMarker(fn func(uint64, uint32)) {
	requestReceivedMarker = fn
}

// ✅ LIVENESS: SetRequestCacher configura callback para cache de requests
func SetRequestCacher(fn func(uint64, *pb.ClientRequest)) {
	requestCacher = fn
}

// SetRequestPreprocessor configura preprocessor customizado
func SetRequestPreprocessor(fn func(*pb.ClientRequest) bool) {
	requestPreprocessor = fn
}

// ReplicaMapper - Mapeia payload para grupos tocados (paper-like)
// Implementa lógica automática do proxy para decidir TouchedGroups
func ReplicaMapper(payload []byte) []uint32 {
	payloadStr := string(payload)
	
	// Requests sistêmicas sempre vão para grupo 0
	if strings.HasPrefix(payloadStr, "SYSTEM:") {
		return []uint32{0}
	}
	
	// ❌ PROBLEMA: Grupos 1,2,3,4 existem, mas lógica usa strings específicas
	// Clientes normais não enviam "CROSS", "GROUP1", etc.
	// Resultado: cai no hash-based que pode retornar grupo inexistente
	
	// Hash-based mapping para grupos de dados (1-4)
	hash := crypto.Hash(payload)
	groupCount := uint32(4) // Grupos 1, 2, 3, 4 (não conta grupo 0)
	groupID := uint32(hash[0])%groupCount + 1 // Retorna 1, 2, 3 ou 4
	
	fmt.Printf("[REPLICA-MAPPER] Mapped payload to group %d (hash-based)\n", groupID)
	return []uint32{groupID}
}

// GetGroupMembersGetter retorna getter de membros (para watchdog)
func GetGroupMembersGetter() func(uint32) []int32 {
	return groupMembersGetter
}

// Allocates a new Request object from a client request message and adds it by calling Add().
func AddReqMsg(reqMsg *pb.ClientRequest) *Request {
	if len(reqMsg.TouchedGroups) == 0 {
		logger.Fatal().
			Int32("clId", reqMsg.RequestId.ClientId).
			Int32("clSn", reqMsg.RequestId.ClientSn).
			Uint32("groupId", reqMsg.GroupId).
			Msg("[CSMR] FATAL: TouchedGroups not set!")
	}
	
	// ✅ DEBUG: Log quando request é adicionada
	payloadPreview := reqMsg.Payload
	if len(payloadPreview) > 50 {
		payloadPreview = payloadPreview[:50]
	}
	isSystem := strings.HasPrefix(string(reqMsg.Payload), "SYSTEM:")
	fmt.Printf("[ADD-REQ] clientId=%d clientSn=%d groupId=%d gsn=%d touchedGroups=%v isSystem=%v payload=%s\n",
		reqMsg.RequestId.ClientId, reqMsg.RequestId.ClientSn, reqMsg.GroupId, reqMsg.GSN, reqMsg.TouchedGroups, isSystem, string(payloadPreview))
	
	// ✅ LIVENESS: Marca request como recebida se tem GSN
	if reqMsg.GSN > 0 && reqMsg.GroupId > 0 && requestReceivedMarker != nil {
		requestReceivedMarker(reqMsg.GSN, reqMsg.GroupId)
	}
	
	opID := GenerateOpID(reqMsg)
	
	// ✅ DEBUG: Log bucket assignment
	bucketNr := GetBucketNr(reqMsg)
	fmt.Printf("[ADD-REQ] Assigning to bucket=%d (groupId=%d)\n", bucketNr, reqMsg.GroupId)
	
	req := &Request{
		Msg:      reqMsg,
		Digest:   Digest(reqMsg),
		Buffer:   getBuffer(reqMsg.RequestId.ClientId),
		Bucket:   getBucket(reqMsg),
		Verified: false,
		InFlight: false,
		Next:     nil,
		Prev:     nil,
		OpID:     opID,
		GSN:      reqMsg.GSN,
	}
	
	if proxyInterceptor != nil {
		proxyInterceptor(req)
	}
	
	addedReq := Add(req)
	if addedReq != nil {
		fmt.Printf("[ADD-REQ] Successfully added to bucket=%d\n", bucketNr)
	} else {
		fmt.Printf("[ADD-REQ][WARN] Failed to add to bucket=%d\n", bucketNr)
	}
	return addedReq
}

// GenerateOpID cria identificador determinístico para operação
func GenerateOpID(req *pb.ClientRequest) string {
	buffer := make([]byte, 0, 8+len(req.Payload))
	// ClientID (4 bytes) + ClientSn (4 bytes)
	id := RequestIDToBytes(req)
	buffer = append(buffer, id...)
	buffer = append(buffer, req.Payload...)
	hash := crypto.Hash(buffer)
	return fmt.Sprintf("%x", hash[:16]) // 16 bytes = 32 hex chars
}

// Adds a request received as a protobuf message to the appropriate buffer and bucket.
func Add(req *Request) *Request {

	// The buffer needs to be Rlocked not only while checking watermarks, but also while adding the request to the Bucket!
	req.Buffer.RLock()
	defer req.Buffer.RUnlock()

	// Check request's watermarks and backlog it if necessary (by adding it to the Buffer)
	// Return if Request is not suited for processing right now (due to being outside of the watermark window).
	// The request might be backlogged though by the Buffer
	// (Note that the implementation of backlogging does not require a write lock on the buffer.).
	if !req.Buffer.Add(req) {
		return nil
	}

	// If Request is within the client watermark window, try looking it up in its bucket.
	// If an identical request has already been stored in the bucket, return that request.
	storedReq, retry := req.Bucket.AddRequest(req)

	// If retrying is not necessary (request added) or meaningful (request cannot be added anyway),
	// Return the result of the first optimistic attempt.
	if retry {
		// Check client signature.
		// Not checking whether signature checking is enabled,
		// since no retrying would be requested if signature checking was disabled.
		if err := crypto.CheckSig(req.Digest, membership.ClientPubKey(req.Msg.RequestId.ClientId), req.Msg.Signature); err == nil {
			req.Verified = true
		} else {
			logger.Warn().
				Err(err).
				Int32("clSn", req.Msg.RequestId.ClientSn).
				Int32("clId", req.Msg.RequestId.ClientId).
				Msg("Invalid request signature.")

			return nil
		}

		// Add verified request.
		// If request cannot be added this time, it is not because of an unverified signature.
		storedReq, _ = req.Bucket.AddRequest(req)
	}

	// Return the request object that ended up being stored, eithher on the first, or on the second attempt.
	// storedReq is nil if request is invalid (outside of the watermark window.)
	return storedReq
}

// Removes all requests in batch from their respective Buckets.
// The buffer is not affected by this function.
func RemoveBatch(batch *Batch) {

	// Do nothing if batch is empty.
	if len(batch.Requests) == 0 {
		return
	}

	// As this usually touches many Buckets and contends with the critical path of requests,
	// we first prepare lists of requests to be removed from each bucket and remove all at once,
	// to avoid frequent locking and unlocking of the buckets.
	toRemove := make(map[int][]*Request) // Maps bucket IDs to the lists of the corresponding clients' requests
	for _, req := range batch.Requests {
		bucketID := req.Bucket.GetId()
		reqs, ok := toRemove[bucketID]
		if !ok {
			reqs = make([]*Request, 0)
			toRemove[bucketID] = reqs
		}
		toRemove[bucketID] = append(reqs, req)
	}

	// For each bucket represented in this batch, remove the all the corresponding requests at once.
	// By construction, no list of requests is empty and all requests in each list are in the same bucket,
	// so taking the first reuest's bucket is safe.
	for _, reqs := range toRemove {
		reqs[0].Bucket.Remove(reqs)
	}
}

// Advances the client watermarks of all Buffers.
// This allows new Requests to be added to the Buffers.
func AdvanceWatermarks(entries []interface{}) { //expected type is []*log.Entry
	logger.Info().Int("numEntries", len(entries)).Msg("Advancing watermarks.")

	// We only acquire a read lock on the buffers map, since the map itself is not modified.
	// What is modified is the Buffers the map entries point to, but those have their own locks.
	buffersLock.RLock()
	defer buffersLock.RUnlock()

	// This map stores, for each client, its old and its new watermark.
	watermarks := &sync.Map{}

	// All buffers advance their watermarks in parallel and synchronize over this wait group.
	wg := sync.WaitGroup{}
	wg.Add(len(buffers))

	// Advance watermarks for each buffer in parallel.
	for _, buf := range buffers {
		go func(b *Buffer) {
			watermarks.Store(b.ClientID, b.AdvanceWatermarks(entries))
			wg.Done()
		}(buf)
	}
	wg.Wait()

	// Once watermarks have been advanced (and only then),
	// Prune the index of all the buckets in parallel.
	// Count the number of requests left in buckets for analytical purposes.
	wg.Add(len(Buckets))
	for _, bucket := range Buckets {
		go func(b *Bucket) {
			b.PruneIndex(watermarks)
			wg.Done()
		}(bucket)
	}
	wg.Wait()
}

// Returns a bucket to which the request message belongs.
func getBucket(req *pb.ClientRequest) *Bucket {
	return Buckets[GetBucketNr(req)]
}

func GetBucketNr(req *pb.ClientRequest) int {
	// ATOMIC MULTICAST: cada grupo tem seu bucket (não usa bucket 0 global)
	if len(req.TouchedGroups) == 1 {
		groupId := int(req.TouchedGroups[0])
		if groupId >= 0 && groupId < len(Buckets) {
			return groupId
		}
		return 0
	}
	
	// Cross-op: usa GroupId do clone (cada clone vai para bucket do seu grupo)
	groupId := int(req.GetGroupId())
	if groupId > 0 && groupId < len(Buckets) {
		return groupId
	}
	return int((req.RequestId.ClientId + req.RequestId.ClientSn) % int32(len(Buckets)))
}

// IsMultiGroupRequest verifica se request toca múltiplos grupos (precisa atomic global order)
func IsMultiGroupRequest(req *pb.ClientRequest) bool {
	return len(req.TouchedGroups) > 1
}

// Returns the request buffer associated with a client ID.
// If the request buffer does not exist, allocates a new one.
func getBuffer(clientID int32) *Buffer {

	// First, check if buffer is present only using a read lock.
	// This check only fails for the very first request from a client (unless we implement client GC later).
	// If we find the buffer, no modification to the buffers map is made and a read lock suffices.
	buffersLock.RLock()
	if buf, ok := buffers[clientID]; ok {
		buffersLock.RUnlock()
		return buf
	}
	buffersLock.RUnlock()

	// Otherwise (i.e., for the first request from a client),
	// acquire a write lock and add buffer for new client.
	buffersLock.Lock()
	defer buffersLock.Unlock()

	// The new check is required in case some other thread adds the buffer for this clientID in the meantime.
	// (Even if currently this should not happen anyway, as only one thread is dealing with a client connection.)
	if buf, ok := buffers[clientID]; ok {
		return buf
	} else {
		newBuf := NewBuffer(clientID)
		buffers[clientID] = newBuf
		return newBuf
	}
}

// This ugly ugly function is only a nasty workaround for the DummyOrderer
// to circumvent properly adding received requests to their buffers.
// Should never be used anywhere outside the DummyOrderer.
func UglyUglyDummyRegisterRequest(reqMsg *pb.ClientRequest) *Request {
	return &Request{
		Msg:      reqMsg,
		Bucket:   getBucket(reqMsg),
		InFlight: true,
	}
}

// Return the hash of a protobuf client request message.
func Digest(req *pb.ClientRequest) []byte {
	buffer := make([]byte, 0, 4+4+len(req.Payload)+len(req.Pubkey))
	id := RequestIDToBytes(req)
	buffer = append(buffer, id...)
	buffer = append(buffer, req.Payload...)
	buffer = append(buffer, req.Pubkey...)
	return crypto.Hash(buffer)
}

func RequestIDToBytes(req *pb.ClientRequest) []byte {
	buffer := make([]byte, 0, 0)
	sn := make([]byte, 4)
	binary.LittleEndian.PutUint32(sn, uint32(req.RequestId.ClientSn))
	buffer = append(buffer, sn...)
	id := make([]byte, 4)
	binary.LittleEndian.PutUint32(id, uint32(req.RequestId.ClientId))
	buffer = append(buffer, id...)
	return buffer
}
