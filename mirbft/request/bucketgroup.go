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
	"sync/atomic"
	"time"

	logger "github.com/rs/zerolog/log"
	"github.com/hyperledger-labs/mirbft/config"
	"github.com/hyperledger-labs/mirbft/membership"
)

type BucketGroup struct {
	// List of buckets in the group.
	// Must be sorted by Bucket ID and, when locking buckets, locks must be acquired in list order.
	buckets []*Bucket

	// Total number of requests in the buckets.
	// Must be read / updated atomically when requests are being added to buckets and not all buckets are locked.
	totalRequests int32

	// When CutBatch() has been called but the Buckets have fewer than BatchSize requests in total, CutBatch() blocks.
	// Before blocking, it sets cutThreshold to the number of requests it is waiting for.
	// When CutBatch() is not waiting, cutThreshold is -1.
	cutThreshold int

	// When CutBatch() is waiting for more requests to be added to the Bucket, this variable holds the Timer
	// implementing a timeout, after which CutBatch() proceeds regardless of the number of requests in the Bucket.
	// When CutBatch() is not waiting, timer is nil.
	timer *time.Timer

	// When CutBatch() is waiting for more requests to be added to the Buckets, it blocks on reading from this channel.
	// A value can be pushed to this channel by
	// - a timeout
	// - the Add() method of some bucket, when enough requests have been added.
	batchTrigger chan struct{}

	// When waiting for a batch, wait at least until this time.
	// This is used to moderate the speed at which batches are cut when the bucket is full.
	// It is useful to limit the rate at which data is put on the wire, decreasing the likelihood of view changes
	// when too much data is sent out concurrently.
	nextBatchTimestamp int64
}

// Creates a new BucketGroup and returns a pointer to it.
// The buckets parameter is a list of bucket IDs. NewBucketGroup() sorts this list!
// Sorting by bucket ID is important to prevent deadlocks, as buckets are always locked in the order of this list.
// NOTE: Currently there is always only one bucket group, so these deadlocks cannot occur, but this might change.
func NewBucketGroup(bucketIDs []int) *BucketGroup {

	// Sort bucket IDs.
	sort.Ints(bucketIDs)

	// Create a new list of all the buckets based on their IDs
	bucketList := make([]*Bucket, len(bucketIDs), len(bucketIDs))
	for i, bucketID := range bucketIDs {
		bucketList[i] = Buckets[bucketID]
	}

	return &BucketGroup{
		buckets:            bucketList,
		totalRequests:      0,
		cutThreshold:       -1,
		timer:              nil,
		batchTrigger:       make(chan struct{}),
		nextBatchTimestamp: 0,
	}
}

// Returns a new request batch assembled from requests in the bucket group.
// Blocks until the Buckets contain at least size requests, but at most for the duration of timeout.
// On timeout, returns a batch with all requests in the Buckets, even if all the Buckets are empty.
func (bg *BucketGroup) CutBatch(size int, timeout time.Duration) *Batch {
	alreadyWaited := bg.waitMinimum()
	bg.lockBuckets()
	defer bg.unlockBuckets()

	// Create new request batch
	newBatch := Batch{Requests: make([]*Request, 0, size)}

	// If the bucket group is empty (contains no buckets), return an empty batch.
	if len(bg.buckets) == 0 {
		return &newBatch
	}

	// Grupo 0 prioriza mensagens sistêmicas (GSN_REQUEST, META_STREAM)
	// Only applies in multicast mode (groupMembersGetter configured)
	isGroup0 := false
	if groupMembersGetter != nil && len(bg.buckets) > 0 {
		numGroups := getNumGroups()
		if numGroups > 0 && bg.buckets[0].id % numGroups == 0 {
			isGroup0 = true
		}
	}
	
	// FAST-PATH: Se é Group 0 e há SYSTEM messages, NÃO espera timeout
	if isGroup0 {
		fmt.Printf("[CutBatch] Group 0 fast-path, checking %d buckets\n", len(bg.buckets))
		for _, b := range bg.buckets {
			sysReq := b.FindSystemRequest()
			fmt.Printf("[CutBatch] Bucket %d: sysReq=%v\n", b.id, sysReq != nil)
			if sysReq != nil {
				b.RemoveNoLock(sysReq)
				newBatch.Requests = append(newBatch.Requests, sysReq)
				logger.Debug().Int("bucketId", b.id).Msg("Cut batch with system request (Group 0 - fast path)")
				return &newBatch
			}
		}
		fmt.Printf("[CutBatch] Group 0: No SYSTEM messages found in any bucket\n")
	}

	// Wait for batch to fill or for the timeout to fire.
	// May release and re-acquire the bucket locks before returning.
	bg.waitForRequestsLocked(size, timeout-time.Duration(alreadyWaited)*time.Nanosecond)

	// Grupos de dados: FIFO com separação (conforme artigo CSMR)
	// Cross-ops vão sozinhas no batch. Single-group vão em batch normal.
	// Verifica se algum bucket tem cross-op como FirstRequest.
	// Se sim, propõe a de menor GSN sozinha.
	// Se não, corta batch normal de single-group (usando RemoveFirst, parando antes de cross-op).

	var initCut = 0
	if size <= int(bg.totalRequests) {
		initCut = size / len(bg.buckets)
	} else {
		initCut = int(bg.totalRequests) / len(bg.buckets)
	}

	if groupMembersGetter != nil {
		// Check if any bucket has cross-op at head
		var minCrossOp *Request
		var minCrossOpBucket *Bucket
		for _, b := range bg.buckets {
			r := b.FirstRequest
			if r != nil && len(r.Msg.TouchedGroups) > 1 && r.Msg.GSN > 0 {
				if minCrossOp == nil || r.Msg.GSN < minCrossOp.Msg.GSN {
					minCrossOp = r
					minCrossOpBucket = b
				}
			}
		}
		if minCrossOp != nil {
			minCrossOpBucket.RemoveNoLock(minCrossOp)
			newBatch.Requests = append(newBatch.Requests, minCrossOp)
			return &newBatch
		}
		// No cross-op at head: cut single-group batch (skip cross-ops in middle)
		for _, b := range bg.buckets {
			newBatch.Requests = b.RemoveFirstSingleGroup(initCut, newBatch.Requests)
		}
		for _, b := range bg.buckets {
			if len(newBatch.Requests) < size {
				newBatch.Requests = b.RemoveFirstSingleGroup(size-len(newBatch.Requests), newBatch.Requests)
			} else {
				break
			}
		}
	} else {
		// Non-multicast (PBFT/Raft/HotStuff): FIFO normal
		for _, b := range bg.buckets {
			newBatch.Requests = b.RemoveFirst(initCut, newBatch.Requests)
		}
		for _, b := range bg.buckets {
			if len(newBatch.Requests) < size {
				newBatch.Requests = b.RemoveFirst(size-len(newBatch.Requests), newBatch.Requests)
			} else {
				break
			}
		}
	}

	// TODO: Possible optimization (but probably irrelevant):
	//       We could mark requests as "in flight" here, instead of outside of this function.

	// Compute starting from when the next batch can be cut, in order to respect the throughput cap.
	// This enforces a limit on the rate batch data is being put on the wire.
	// If, for some reason, too many batches are sent concurrently (e.g. when the bucket is very full),
	// all of them will be slow, increasing the likelihood of timeouts.
	totalReq := len(newBatch.Requests) * membership.NumNodes()
	if config.Config.LeaderPolicy == "Single" {
		totalReq /= membership.NumNodes() // For Single leader policy, use the raw throughput cap, not adjusted to system size.
	}
	waitingTime := 1000000000 * int64(totalReq/config.Config.ThroughputCap) // In nanoseconds
	atomic.StoreInt64(&bg.nextBatchTimestamp, time.Now().UnixNano()+waitingTime)

	logger.Debug().
		Int("nBuckets", len(bg.buckets)).
		Int("nReq", len(newBatch.Requests)).
		Int("left", bg.CountRequests()).
		Int64("next", waitingTime/1000000). // In milliseconds
		Msg("Batch cut.")

	return &newBatch
}

// Blocks until the buckets in the BucketGroup (cumulatively) contain numRequests requests or until timeout elapses.
// When WaitForRequests returns, bg.totalRequests accurately represents the total number of requests in the BucketGroup.
func (bg *BucketGroup) WaitForRequests(numRequests int, timeout time.Duration) {
	alreadyWaited := bg.waitMinimum()
	bg.lockBuckets()
	bg.waitForRequestsLocked(numRequests, timeout-time.Duration(alreadyWaited)*time.Nanosecond)
	bg.unlockBuckets()
}

// Blocks until the buckets in the BucketGroup (cumulatively) contain numRequests requests or until timeout elapses.
// When WaitForRequests returns, bg.totalRequests accurately represents the total number of requests in the BucketGroup.
// ATTENTION: All Buckets must be LOCKED when calling this method.
//            May release and re-acquire the bucket locks before returning.
func (bg *BucketGroup) waitForRequestsLocked(numRequests int, timeout time.Duration) {

	// Count all requests in all buckets in the group.
	// (A normal assignment suffices, as all buckets are locked.)
	bg.totalRequests = int32(bg.CountRequests())

	// If there are enough requests in the bucket, return immediately.
	if int(bg.totalRequests) >= numRequests || (timeout == 0) {
		return
	}

	// Initialize data structures for waiting for requests.
	bg.cutThreshold = numRequests
	bg.timer = time.AfterFunc(timeout, func() { bg.batchTrigger <- struct{}{} })

	// Register this group with all buckets (for notifications).
	for _, b := range bg.buckets {
		b.Group = bg
	}

	// Release locks (so that Add() can add requests to the buckets) and wait for the trigger
	// (released by the timeout or by bg.RequestAdded() called by a bucket's Add() method).
	bg.unlockBuckets()
	<-bg.batchTrigger
	bg.lockBuckets()

	// Clean up.
	for _, b := range bg.buckets {
		b.Group = nil
	}
	bg.timer = nil
	bg.cutThreshold = -1
}

func (bg *BucketGroup) waitMinimum() int64 {
	// Note that if nextBatchTimestamp is 0, this function returns immediately as expected.
	dur := atomic.LoadInt64(&bg.nextBatchTimestamp) - time.Now().UnixNano()
	if dur > 0 {
		time.Sleep(time.Duration(dur) * time.Nanosecond)
	} else {
		dur = 0
	}

	return dur // Return the number of nanoseconds waited.
}

// Notifies the BucketGroup that is waiting to cut a batch about a request being added in one of its buckets.
// Can be called concurrently from many Bucket.Add() methods (while the Bucket is locked).
func (bg *BucketGroup) RequestAdded() {

	// Atomically increment and fetch the number of requests in the BucketGroup.
	totalRequests := atomic.AddInt32(&bg.totalRequests, 1)

	// If CutBatch() is waiting and the required threshold of requests has been reached
	// It is important to use == (and not >= ) when comparing the request count, s.t. the body of the condition
	// is only executed once.
	if bg.timer != nil && int(totalRequests) == bg.cutThreshold {

		// Stop the timer
		if bg.timer.Stop() {

			// And release WaitForReuests().
			// Note that stopping the timer might fail if the timeout triggers concurrently with RequestAdded().
			// In such a case, this line is not executed, as the timeout does the job.
			bg.batchTrigger <- struct{}{}
		}
	}
}

// Counts all requests in all buckets.
// Only makes sense if the buckets are locked.
func (bg *BucketGroup) CountRequests() int {
	n := 0
	for _, b := range bg.buckets {
		n += b.Len()
	}
	return n
}

// Returns a list with the bucket IDs  in the Bucket Group.
func (bg *BucketGroup) GetBucketIDs() []int {
	ids := make([]int, len(bg.buckets))
	for i, b := range bg.buckets {
		ids[i] = b.id
	}
	return ids
}

// Locks all buckets in the group.
func (bg *BucketGroup) lockBuckets() {
	for _, b := range bg.buckets {
		b.Lock()
	}
}

// Unlocks all buckets in the group.
func (bg *BucketGroup) unlockBuckets() {
	for _, b := range bg.buckets {
		b.Unlock()
	}
}

// CutBatchFromBucket cuts batch from a specific bucket only
func (bg *BucketGroup) CutBatchFromBucket(bucketID int, size int, timeout time.Duration) *Batch {
	// Find the specific bucket
	var targetBucket *Bucket
	for _, b := range bg.buckets {
		if b.id == bucketID {
			targetBucket = b
			break
		}
	}
	
	if targetBucket == nil {
		return &Batch{Requests: make([]*Request, 0)} // Empty batch
	}
	
	// Lock only the target bucket
	targetBucket.Lock()
	
	// Wait for requests in this bucket only
	if targetBucket.Len() < size && timeout > 0 {
		// Simple wait - could be optimized with proper signaling
		targetBucket.Unlock()
		time.Sleep(timeout)
		targetBucket.Lock()
	}
	
	// Cut batch from this bucket only (single-group only, leave cross-ops for separate handling)
	newBatch := Batch{Requests: make([]*Request, 0, size)}
	newBatch.Requests = targetBucket.RemoveFirstSingleGroup(size, newBatch.Requests)
	
	targetBucket.Unlock()
	return &newBatch
}
