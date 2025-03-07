// Copyright 2025 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package kvcoord

import (
	"context"

	"github.com/cockroachdb/cockroach/pkg/kv/kvpb"
	"github.com/cockroachdb/cockroach/pkg/kv/kvserver/concurrency/lock"
	"github.com/cockroachdb/cockroach/pkg/roachpb"
	"github.com/cockroachdb/cockroach/pkg/settings"
	"github.com/cockroachdb/cockroach/pkg/storage/enginepb"
	"github.com/cockroachdb/cockroach/pkg/storage/mvcceval"
	"github.com/cockroachdb/cockroach/pkg/util/log"
	"github.com/cockroachdb/errors"
)

// BufferedWritesEnabled is used to enable write buffering.
var BufferedWritesEnabled = settings.RegisterBoolSetting(
	settings.ApplicationLevel,
	"kv.transaction.write_buffering.enabled",
	"if enabled, transactional writes are buffered on the client",
	false,
	settings.WithPublic,
)

// txnWriteBuffer is a txnInterceptor that buffers transactional writes until
// commit time. Moreover, it also decomposes read-write KV operations (e.g.
// CPuts, InitPuts) into separate (locking) read and write operations, buffering
// the latter until commit time.
//
// Buffering writes until commit time has four main benefits:
//
// 1. It allows for more batching of writes, which can be more efficient.
// Instead of sending write batches one at a time, we can batch all write
// batches and send them in a single batch at commit time. This is a win even if
// writes would otherwise be pipelined through raft.
//
// 2. It allows for the elimination of redundant writes. If a client writes to
// the same key multiple times in a transaction, only the last write needs to be
// written to the KV layer.
//
// 3. It allows the client to serve read-your-own-writes locally, which can be
// much faster and cheaper than sending them to the leaseholder. This is
// especially true when the leaseholder isn't colocated with the client.
//
// By serving read-your-own-writes locally from the gateway, write buffering
// also avoids the problem of pipeline stalls that can occur when a client reads
// a pipelined write before it has finished replicating through raft. For
// details on pipeline stalls, see txnPipeliner.
//
// 4. It allows clients to passively hit the 1-phase commit fast path, instead
// of requiring clients to carefully construct "auto-commit" BatchRequests to
// make use of the optimization. By buffering writes on the client before
// commit, we avoid immediately disabling the fast path when the client issues
// their first write. Then, at commit time, we flush the buffer and will happen
// to hit the 1-phase commit fast path if all writes end up going to the same
// range.
//
// However, buffering writes comes with some challenges.
//
// The first challenge is that read-only requests need to be aware of any
// buffered writes, as they may need to serve some reads from the buffer
// instead of the KV layer (read-your-own-writes).
//
// Similarly, any read-write requests, such as CPuts, that we decompose into
// separate read and write operations, need to be aware of any buffered writes
// that may affect their read half. The read portion must be served from the
// buffer instead of the KV-layer if the key has already been written to
// previously. However, we aren't guaranteed to have acquired a corresponding
// lock on the key if a buffered write exists for the key -- as such, we must
// still send a locking read request to the KV layer to acquire a lock.
//
// The picture is further complicated when distributed execution is introduced
// into the mix. A read request that is distributed by the client also needs to
// be aware of the write buffer. As such, when constructing a leaf transaction
// to serve a distributed execution read, we must also ship[1] the write buffer
// along.
//
// The second challenge is around the handling of savepoints. In particular,
// when a savepoint is rolled back, we must clear out any writes that happened
// after the (now) rolled back savepoint. This means that if a key is written to
// multiple times in a transaction, we must retain all writes to the key. Note
// that this is only required when the transaction is in progress -- once the
// transaction is ready to commit, only the last value written to the key needs
// to be flushed to the KV layer.
//
// The third challenge is around memory limits and preventing OOMs. As writes
// are buffered in-memory, per-transaction, we need to be careful not to OOM
// nodes by buffering too many writes. To that end, a per-transaction memory
// limit on the write buffer must be enforced. If this limit is exceeded, no
// more writes are buffered, and the buffer (either in its entirety or
// partially[2]) must be force flushed. Force flushing entails sending all
// buffered writes to the KV layer prematurely (i.e. before commit time).
//
// [1] Instead of shipping the entire write buffer, we can constrain this to
// just the portion that overlaps with the span of the read request that's being
// distributed for evaluation by the client.
//
// [2] The decision to flush the buffer in its entirety vs. partially is a
// tradeoff. Flushing the entire buffer is simpler and frees up more memory.
// Flushing the buffer partially preserves some (all but the fourth) of the
// listed benefits of buffering writes for the unflushed portion of the buffer.
//
// TODO(arul): In various places below, there's potential to optimize things by
// batch allocating misc objects and pre-allocating some slices.
type txnWriteBuffer struct {
	enabled bool

	buffer        btree
	bufferSeek    bufferedWrite // re-use while seeking
	bufferIDAlloc uint64

	wrapped lockedSender

	// testingOverrideCPutEvalFn is used to mock the evaluation function for
	// conditional puts. Intended only for tests.
	testingOverrideCPutEvalFn func(expBytes []byte, actVal *roachpb.Value, actValPresent bool, allowNoExisting bool) *kvpb.ConditionFailedError
}

func (twb *txnWriteBuffer) SendLocked(
	ctx context.Context, ba *kvpb.BatchRequest,
) (_ *kvpb.BatchResponse, pErr *kvpb.Error) {
	if !twb.enabled {
		return twb.wrapped.SendLocked(ctx, ba)
	}

	if _, ok := ba.GetArg(kvpb.EndTxn); ok {
		// TODO(arul): should we only flush if the transaction is being committed?
		// If the transaction is being rolled back, we shouldn't needlessly flush
		// writes.
		return twb.flushWithEndTxn(ctx, ba)
	}

	transformedBa, ts := twb.applyTransformations(ctx, ba)

	if len(transformedBa.Requests) == 0 {
		// Lower layers (the DistSender and the KVServer) do not expect/handle empty
		// batches. If all requests in the batch can be handled locally, and we're
		// left with an empty batch after applying transformations, eschew sending
		// anything to KV.
		br := ba.CreateReply()
		for i, t := range ts {
			br.Responses[i], pErr = t.toResp(twb, kvpb.ResponseUnion{}, ba.Txn)
			if pErr != nil {
				return nil, pErr
			}
		}
		return br, nil
	}

	br, pErr := twb.wrapped.SendLocked(ctx, transformedBa)
	if pErr != nil {
		return nil, twb.adjustError(ctx, transformedBa, ts, pErr)
	}

	return twb.mergeResponseWithTransformations(ctx, ts, br)
}

// adjustError adjusts the provided error based on the transformations made by
// the txnWriteBuffer to the batch request before sending it to KV.
func (twb *txnWriteBuffer) adjustError(
	ctx context.Context, ba *kvpb.BatchRequest, ts transformations, pErr *kvpb.Error,
) *kvpb.Error {
	// Fix the error index to hide the impact of any requests that were
	// transformed.
	if pErr.Index != nil {
		// We essentially want to find the number of stripped batch requests that
		// came before the request that caused the error in the original batch, and
		// therefore weren't sent to the KV layer. We can then adjust the error
		// index accordingly.
		numStripped := int32(0)
		numOriginalRequests := len(ba.Requests) + len(ts)
		baIdx := int32(0)
		for i := range numOriginalRequests {
			if len(ts) > 0 && ts[0].index == i {
				if ts[0].stripped {
					numStripped++
				} else {
					// TODO(arul): If the error index points to a request that we've
					// transformed, returning this back to the client is weird -- the
					// client doesn't know we're making transformations. We should
					// probably just log a warning and clear out the error index for such
					// cases.
					log.Fatal(ctx, "unhandled")
				}
				ts = ts[1:]
				continue
			}
			if baIdx == pErr.Index.Index {
				break
			}
			baIdx++
		}

		pErr.Index.Index += numStripped
	}

	return pErr
}

// adjustErrorUponFlush adjusts the provided error based on the number of
// pre-fixed writes due to flushing the buffer.
func (twb *txnWriteBuffer) adjustErrorUponFlush(
	ctx context.Context, numBuffered int, pErr *kvpb.Error,
) *kvpb.Error {
	if pErr.Index != nil {
		if pErr.Index.Index < int32(numBuffered) {
			// If the error belongs to a request because part of the buffer flush, nil
			// out the index.
			log.Warningf(ctx, "error index %d is part of the buffer flush", pErr.Index.Index)
			pErr.Index = nil
		} else {
			// Otherwise, adjust the error index to hide the impact of any flushed
			// write requests.
			pErr.Index.Index -= int32(numBuffered)
		}
	}
	return pErr
}

// setWrapped implements the txnInterceptor interface.
func (twb *txnWriteBuffer) setWrapped(wrapped lockedSender) {
	twb.wrapped = wrapped
}

// populateLeafInputState is part of the txnInterceptor interface.
func (twb *txnWriteBuffer) populateLeafInputState(tis *roachpb.LeafTxnInputState) {
	if !twb.enabled || twb.buffer.Len() == 0 {
		return
	}
	tis.BufferedWrites = make([]roachpb.BufferedWrite, 0, twb.buffer.Len())
	it := twb.buffer.MakeIter()
	for it.First(); it.Valid(); it.Next() {
		bw := it.Cur()
		// TODO(yuzefovich): optimize allocation of vals slices.
		vals := make([]roachpb.BufferedWrite_Val, 0, len(bw.vals))
		for _, v := range bw.vals {
			vals = append(vals, roachpb.BufferedWrite_Val{
				Val: v.val,
				Seq: v.seq,
			})
		}
		tis.BufferedWrites = append(tis.BufferedWrites, roachpb.BufferedWrite{
			ID:   bw.id,
			Key:  bw.key,
			Vals: vals,
		})
	}
}

// initializeLeaf is part of the txnInterceptor interface.
func (twb *txnWriteBuffer) initializeLeaf(tis *roachpb.LeafTxnInputState) {
	if len(tis.BufferedWrites) == 0 {
		// Regardless of whether the buffered writes are enabled on the root,
		// there are no actual buffered writes, so we can disable the
		// interceptor.
		twb.enabled = false
		return
	}
	// We have some buffered writes, so they must be enabled on the root.
	twb.enabled = true
	for _, bw := range tis.BufferedWrites {
		// TODO(yuzefovich): optimize allocation of vals slices.
		vals := make([]bufferedValue, 0, len(bw.Vals))
		for _, bv := range bw.Vals {
			vals = append(vals, bufferedValue{
				val: bv.Val,
				seq: bv.Seq,
			})
		}
		twb.buffer.Set(&bufferedWrite{
			id:   bw.ID,
			key:  bw.Key,
			vals: vals,
		})
	}
	// Note that we'll leave bufferIDAlloc field unchanged since we can't
	// perform any writes on the leaf, meaning that we won't add any new
	// buffered writes to the btree.
}

// populateLeafFinalState is part of the txnInterceptor interface.
func (twb *txnWriteBuffer) populateLeafFinalState(*roachpb.LeafTxnFinalState) {}

// importLeafFinalState is part of the txnInterceptor interface.
func (twb *txnWriteBuffer) importLeafFinalState(context.Context, *roachpb.LeafTxnFinalState) error {
	return nil
}

// epochBumpedLocked implements the txnInterceptor interface.
func (twb *txnWriteBuffer) epochBumpedLocked() {}

// createSavepointLocked is part of the txnInterceptor interface.
func (twb *txnWriteBuffer) createSavepointLocked(context.Context, *savepoint) {}

// rollbackToSavepointLocked is part of the txnInterceptor interface.
func (twb *txnWriteBuffer) rollbackToSavepointLocked(ctx context.Context, s savepoint) {}

// closeLocked implements the txnInterceptor interface.
func (twb *txnWriteBuffer) closeLocked() {}

// applyTransformations applies any applicable transformations to the supplied
// batch request. In doing so, a new batch request with transformations applied
// along with a list of transformations that were applied is returned. The
// caller must handle these transformations on the response path.
//
// Some examples of transformations include:
//
// 1. Blind writes (Put/Delete requests) are stripped from the batch and
// buffered locally.
// 2. Point reads (Get requests) are served from the buffer and stripped from
// the batch iff the key has seen a buffered write.
// 3. Scans are always sent to the KV layer, but if the key span being scanned
// overlaps with any buffered writes, then the response from the KV layer needs
// to be merged with buffered writes. These are collected as transformations.
// 4. ReverseScans, similar to scans, are also always sent to the KV layer and
// their response needs to be merged with any buffered writes. The only
// difference is the direction in which the buffer is iterated when doing the
// merge. As a result, they're also collected as tranformations.
// 5. Conditional Puts are decomposed into a locking Get followed by a Put. The
// Put is buffered locally if the condition evaluates successfully using the
// Get's response. Otherwise, a ConditionFailedError is returned.
//
// TODO(arul): Augment this comment as these expand.
func (twb *txnWriteBuffer) applyTransformations(
	ctx context.Context, ba *kvpb.BatchRequest,
) (*kvpb.BatchRequest, transformations) {
	baRemote := ba.ShallowCopy()
	// TODO(arul): We could improve performance here by pre-allocating
	// baRemote.Requests to the correct size by counting the number of Puts/Dels
	// in ba.Requests. The same for the transformations slice. We could also
	// allocate the right number of ResponseUnion, PutResponse, and DeleteResponse
	// objects as well.
	baRemote.Requests = nil

	var ts transformations
	for i, ru := range ba.Requests {
		req := ru.GetInner()
		switch t := req.(type) {
		case *kvpb.ConditionalPutRequest:
			ts = append(ts, transformation{
				stripped:    false,
				index:       i,
				origRequest: req,
			})
			getReq := &kvpb.GetRequest{
				RequestHeader: kvpb.RequestHeader{
					Key:      t.Key,
					Sequence: t.Sequence,
				},
				LockNonExisting:    len(t.ExpBytes) == 0 || t.AllowIfDoesNotExist,
				KeyLockingStrength: lock.Exclusive,
			}
			var getReqU kvpb.RequestUnion
			getReqU.MustSetInner(getReq)
			// Send a locking Get request to the KV layer; we'll evaluate the
			// condition locally based on the response.
			baRemote.Requests = append(baRemote.Requests, getReqU)
			// TODO(arul): We're not handling the case where this Get needs to
			// be served locally from the buffer yet.

		case *kvpb.PutRequest:
			var ru kvpb.ResponseUnion
			ru.MustSetInner(&kvpb.PutResponse{})
			// TODO(yuzefovich): if MustAcquireExclusiveLock flag is set on the
			// Put, then we need to add a locking Get to the BatchRequest,
			// including if the key doesn't exist (#139232).
			// TODO(yuzefovich): ensure that we elide the lock acquisition
			// whenever possible (e.g. blind UPSERT in an implicit txn).
			ts = append(ts, transformation{
				stripped:    true,
				index:       i,
				origRequest: req,
				resp:        ru,
			})
			twb.addToBuffer(t.Key, t.Value, t.Sequence)

		case *kvpb.DeleteRequest:
			var ru kvpb.ResponseUnion
			ru.MustSetInner(&kvpb.DeleteResponse{
				// TODO(yuzefovich): if MustAcquireExclusiveLock flag is set on
				// the Del, then we need to add a locking Get to the
				// BatchRequest, including if the key doesn't exist (#139232).
				FoundKey: false,
			})
			ts = append(ts, transformation{
				stripped:    true,
				index:       i,
				origRequest: req,
				resp:        ru,
			})
			twb.addToBuffer(t.Key, roachpb.Value{}, t.Sequence)

		case *kvpb.GetRequest:
			// If the key is in the buffer, we must serve the read from the buffer.
			val, served := twb.maybeServeRead(t.Key, t.Sequence)
			if served {
				log.VEventf(ctx, 2, "serving %s on key %s from the buffer", t.Method(), t.Key)
				var resp kvpb.ResponseUnion
				getResp := &kvpb.GetResponse{}
				if val.IsPresent() {
					getResp.Value = val
				}
				resp.MustSetInner(getResp)

				stripped := true
				if t.KeyLockingStrength != lock.None {
					// Even though the Get request must be served from the buffer, as the
					// transaction performed a previous write to the key, we still need to
					// acquire a lock at the leaseholder. As a result, we can't strip the
					// request from the batch.
					//
					// TODO(arul): we could eschew sending this request if we knew there
					// was a sufficiently strong lock already present on the key.
					stripped = false
					baRemote.Requests = append(baRemote.Requests, ru)
				}

				ts = append(ts, transformation{
					stripped:    stripped,
					index:       i,
					origRequest: req,
					resp:        resp,
				})
				// We've constructed a response that we'll stitch together with the
				// result on the response path; eschew sending the request to the KV
				// layer.
				//
				// TODO(arul): if the ReturnRawMVCCValues flag is set, we'll need to
				// flush the buffer.
				continue
			}
			// Wasn't served locally; send the request to the KV layer.
			baRemote.Requests = append(baRemote.Requests, ru)

		case *kvpb.ScanRequest:
			overlaps := twb.scanOverlaps(t.Key, t.EndKey)
			if overlaps {
				ts = append(ts, transformation{
					stripped:    false,
					index:       i,
					origRequest: req,
				})
			}
			// Regardless of whether the scan overlaps with any writes in the buffer
			// or not, we must send the request to the KV layer. We can't know for
			// sure that there's nothing else to read.
			baRemote.Requests = append(baRemote.Requests, ru)

		case *kvpb.ReverseScanRequest:
			overlaps := twb.scanOverlaps(t.Key, t.EndKey)
			if overlaps {
				ts = append(ts, transformation{
					stripped:    false,
					index:       i,
					origRequest: req,
				})
			}
			// Similar to the reasoning above, regardless of whether the reverse
			// scan overlaps with any writes in the buffer or not, we must send
			// the request to the KV layer.
			baRemote.Requests = append(baRemote.Requests, ru)

		default:
			baRemote.Requests = append(baRemote.Requests, ru)
		}
	}
	return baRemote, ts
}

// maybeServeRead serves the supplied read request from the buffer if a write or
// deletion tombstone on the key is present in the buffer. Additionally, a
// boolean indicating whether the read request was served or not is also
// returned.
func (twb *txnWriteBuffer) maybeServeRead(
	key roachpb.Key, seq enginepb.TxnSeq,
) (*roachpb.Value, bool) {
	it := twb.buffer.MakeIter()
	seek := &twb.bufferSeek
	seek.key = key

	it.FirstOverlap(seek)
	if it.Valid() {
		bufferedVals := it.Cur().vals
		// In the common case, we're reading the most recently buffered write. That
		// is, the sequence number we're reading at is greater than or equal to the
		// sequence number of the last write that was buffered. The list of buffered
		// values is stored in ascending order; we can therefore start iterating
		// from the end of the slice. In the common case, there won't be much
		// "iteration" happening here.
		//
		// TODO(arul): explore adding special treatment for the common case and
		// using a binary search here instead.
		for i := len(bufferedVals) - 1; i >= 0; i-- {
			if seq >= bufferedVals[i].seq {
				return bufferedVals[i].valPtr(), true
			}
		}
		// We've iterated through the buffer, but it seems like our sequence number
		// is smaller than any buffered write performed by our transaction. We can't
		// serve the read locally.
		return nil, false
	}
	return nil, false
}

// scanOverlaps returns whether the given key range overlaps with any buffered
// write.
func (twb *txnWriteBuffer) scanOverlaps(key roachpb.Key, endKey roachpb.Key) bool {
	it := twb.buffer.MakeIter()
	seek := &twb.bufferSeek
	seek.key = key
	seek.endKey = endKey

	it.FirstOverlap(seek)
	return it.Valid()
}

// mergeWithScanResp takes a ScanRequest, that was sent to the KV layer, and the
// response returned by the KV layer, and merges it with any writes that were
// buffered by the transaction to correctly uphold read-your-own-write
// semantics.
func (twb *txnWriteBuffer) mergeWithScanResp(
	req *kvpb.ScanRequest, resp *kvpb.ScanResponse,
) (*kvpb.ScanResponse, error) {
	if req.ScanFormat == kvpb.COL_BATCH_RESPONSE {
		return nil, errors.AssertionFailedf("unexpectedly called mergeWithScanResp on a ScanRequest " +
			"with COL_BATCH_RESPONSE scan format")
	}
	if req.ScanFormat == kvpb.BATCH_RESPONSE {
		// TODO(arul): See pebbleResults.put for how this should be done.
		return nil, errors.AssertionFailedf("unimplemented")
	}

	respIter := newScanRespIter(req, resp)
	// First, calculate the size of the merged response. This then allows us to
	// exactly pre-allocate the response slice when constructing the
	// respMerger.
	respSize := 0
	twb.mergeBufferAndResp(
		respIter,
		func(roachpb.Key, *roachpb.Value) { respSize++ },
		func() { respSize++ },
		false, /* reverse */
	)

	respIter.reset()
	rm := makeRespMerger(respIter, respSize)
	twb.mergeBufferAndResp(respIter, rm.acceptKV, rm.acceptServerResp, false /* reverse */)
	return rm.toScanResp(), nil
}

// mergeWithReverseScanResp takes a ReverseScanRequest, that was sent to the KV
// layer, and the response returned by the KV layer, and merges it with any
// writes that were buffered by the transaction to correctly uphold
// read-your-own-write semantics.
func (twb *txnWriteBuffer) mergeWithReverseScanResp(
	req *kvpb.ReverseScanRequest, resp *kvpb.ReverseScanResponse,
) (*kvpb.ReverseScanResponse, error) {
	if req.ScanFormat == kvpb.COL_BATCH_RESPONSE {
		return nil, errors.AssertionFailedf("unexpectedly called mergeWithReverseScanResp on a " +
			"ReverseScanRequest with COL_BATCH_RESPONSE scan format")
	}
	if req.ScanFormat == kvpb.BATCH_RESPONSE {
		// TODO(arul): See pebbleResults.put for how this should be done.
		return nil, errors.AssertionFailedf("unimplemented")
	}

	respIter := newReverseScanRespIter(req, resp)
	// First, calculate the size of the merged response. This then allows us to
	// exactly pre-allocate the response slice when constructing the
	// respMerger.
	respSize := 0
	twb.mergeBufferAndResp(
		respIter,
		func(roachpb.Key, *roachpb.Value) { respSize++ },
		func() { respSize++ },
		true, /* reverse */
	)

	respIter.reset()
	rm := makeRespMerger(respIter, respSize)
	twb.mergeBufferAndResp(respIter, rm.acceptKV, rm.acceptServerResp, true /* reverse */)
	return rm.toReverseScanResp(), nil
}

// mergeBufferAndScanResp merges (think the merge step from merge sort) the
// buffer and the server's response to a Scan or ReverseScan request. It does so
// by iterating over both, in-order[1], and calling the appropriate accept
// function, based on which KV pair should be preferred[2] by the combined
// response.
//
// [1] Forward or reverse order, depending on the direction of the scan.
// [2] See inline comments for more details on what "preferred" means.
func (twb *txnWriteBuffer) mergeBufferAndResp(
	respIter *respIter,
	acceptBuffer func(roachpb.Key, *roachpb.Value),
	acceptResp func(),
	reverse bool,
) {
	it := twb.buffer.MakeIter()
	seek := &twb.bufferSeek
	seek.key = respIter.startKey()
	seek.endKey = respIter.endKey()

	if reverse {
		it.LastOverlap(seek)
	} else {
		it.FirstOverlap(seek)
	}
	bufferNext := func() {
		if reverse {
			it.PrevOverlap(seek)
		} else {
			it.NextOverlap(seek)
		}
	}

	for respIter.valid() && it.Valid() {
		k := respIter.peekKey()
		cmp := it.Cur().key.Compare(k)
		if reverse {
			// The comparison between keys in the buffer and the response is
			// inverted when scanning in reverse order, as we want to prefer the
			// larger of the two keys.
			cmp = cmp * -1
		}
		switch cmp {
		case -1:
			// The key in the buffer is less than the next key in the server's
			// response, so we prefer it.
			val, served := twb.maybeServeRead(it.Cur().key, respIter.seq())
			if served && val.IsPresent() {
				// NB: Only include a buffered value in the response if it hasn't been
				// deleted by the transaction previously. This matches the behaviour
				// of MVCCScan, which configures Pebble to not return deletion
				// tombstones. See pebbleMVCCScanner.add().
				acceptBuffer(it.Cur().key, val)
			}
			bufferNext()

		case 0:
			// The key exists in the buffer. We must serve the read from the buffer,
			// assuming it is visible to the sequence number of the request.
			val, served := twb.maybeServeRead(it.Cur().key, respIter.seq())
			if served {
				if val.IsPresent() {
					// NB: Only include a buffered value in the response if it hasn't been
					// deleted by the transaction previously. This matches the behaviour
					// of MVCCScan, which configures Pebble to not return deletion
					// tombstones. See pebbleMVCCScanner.add().
					acceptBuffer(it.Cur().key, val)
				}
			} else {
				// Even though the key was in the buffer, its sequence number was higher
				// than the request's. Accept the response from server.
				acceptResp()
			}
			// Move on to the next key, both in the buffer and the response.
			respIter.next()
			bufferNext()

		case 1:
			// The key in the buffer is greater than the current key in the
			// server's response, so we prefer the row from the server's
			// response.
			acceptResp()
			respIter.next()
		}
	}

	for respIter.valid() {
		acceptResp()
		respIter.next()
	}
	for it.Valid() {
		val, served := twb.maybeServeRead(it.Cur().key, respIter.seq())
		if served && val.IsPresent() {
			// Like above, we'll only include the value in the response if the Scan's
			// sequence number requires us to see it and it isn't a deletion
			// tombstone.
			acceptBuffer(it.Cur().key, val)
		}
		bufferNext()
	}
}

// mergeResponsesWithTransformations merges responses from the KV layer with the
// transformations that were applied by the txnWriteBuffer before sending the
// batch request. As a result, interceptors above the txnWriteBuffer remain
// oblivious to its decision to buffer any writes.
func (twb *txnWriteBuffer) mergeResponseWithTransformations(
	ctx context.Context, ts transformations, br *kvpb.BatchResponse,
) (_ *kvpb.BatchResponse, pErr *kvpb.Error) {
	if ts.Empty() && br == nil {
		log.Fatal(ctx, "unexpectedly found no transformations and no batch response")
	} else if ts.Empty() {
		return br, nil
	}

	// Figure out the length of the merged responses slice.
	mergedRespsLen := len(br.Responses)
	for _, t := range ts {
		if t.stripped {
			mergedRespsLen++
		}
	}
	mergedResps := make([]kvpb.ResponseUnion, mergedRespsLen)
	for i := range mergedResps {
		if len(ts) > 0 && ts[0].index == i {
			if !ts[0].stripped {
				// If the transformation wasn't stripped from the batch we sent to KV,
				// we received a response for it, which then needs to be combined with
				// what's in the write buffer.
				resp := br.Responses[0]
				mergedResps[i], pErr = ts[0].toResp(twb, resp, br.Txn)
				if pErr != nil {
					return nil, pErr
				}
				br.Responses = br.Responses[1:]
			} else {
				mergedResps[i], pErr = ts[0].toResp(twb, kvpb.ResponseUnion{}, br.Txn)
				if pErr != nil {
					return nil, pErr
				}
			}

			ts = ts[1:]
			continue
		}

		// No transformation applies at this index. Copy over the response as is.
		mergedResps[i] = br.Responses[0]
		br.Responses = br.Responses[1:]
	}
	br.Responses = mergedResps
	return br, nil
}

// transformation is a modification applied by the txnWriteBuffer on a batch
// request that needs to be accounted for when returning the response.
type transformation struct {
	// stripped, if true, indicates that the request was stripped from the batch
	// and never sent to the KV layer.
	stripped bool
	// index of the request in the original batch to which the transformation
	// applies.
	index int
	// origRequest is the original request that was transformed.
	origRequest kvpb.Request
	// resp is locally produced response that needs to be merged with any
	// responses returned by the KV layer. This is set for requests that can be
	// evaluated locally (e.g. blind writes, reads that can be served entirely
	// from the buffer). Must be set if stripped is true, but the converse doesn't
	// hold.
	resp kvpb.ResponseUnion
}

// toResp returns the response that should be added to the batch response as
// a result of applying the transformation.
func (t transformation) toResp(
	twb *txnWriteBuffer, br kvpb.ResponseUnion, txn *roachpb.Transaction,
) (kvpb.ResponseUnion, *kvpb.Error) {
	if t.stripped {
		return t.resp, nil
	}

	var ru kvpb.ResponseUnion
	switch t.origRequest.(type) {
	case *kvpb.ConditionalPutRequest:
		// Evaluate the condition.
		evalFn := mvcceval.MaybeConditionFailedError
		if twb.testingOverrideCPutEvalFn != nil {
			evalFn = twb.testingOverrideCPutEvalFn
		}
		cputReq := t.origRequest.(*kvpb.ConditionalPutRequest)
		getResp := br.GetInner().(*kvpb.GetResponse)
		condFailedErr := evalFn(
			cputReq.ExpBytes,
			getResp.Value,
			getResp.Value.IsPresent(),
			cputReq.AllowIfDoesNotExist,
		)
		if condFailedErr != nil {
			pErr := kvpb.NewErrorWithTxn(condFailedErr, txn)
			pErr.SetErrorIndex(int32(t.index))
			return kvpb.ResponseUnion{}, pErr
		}
		// The condition was satisfied; buffer a Put, and return a synthesized
		// response.
		twb.addToBuffer(cputReq.Key, cputReq.Value, cputReq.Sequence)
		ru.MustSetInner(&kvpb.ConditionalPutResponse{})

	case *kvpb.PutRequest:
		ru = t.resp

	case *kvpb.DeleteRequest:
		ru = t.resp

	case *kvpb.GetRequest:
		// Get requests must be served from the local buffer if a transaction
		// performed a previous write to the key being read. However, Get requests
		// must be sent to the KV layer (i.e. not be stripped) iff they are locking
		// in nature.
		req := t.origRequest.(*kvpb.GetRequest)
		assertTrue(t.stripped == (req.KeyLockingStrength == lock.None),
			"Get requests should either be stripped or be locking")
		ru = t.resp

	case *kvpb.ScanRequest:
		scanResp, err := twb.mergeWithScanResp(
			t.origRequest.(*kvpb.ScanRequest), br.GetInner().(*kvpb.ScanResponse),
		)
		if err != nil {
			return kvpb.ResponseUnion{}, kvpb.NewError(err)
		}
		ru.MustSetInner(scanResp)

	case *kvpb.ReverseScanRequest:
		reverseScanResp, err := twb.mergeWithReverseScanResp(
			t.origRequest.(*kvpb.ReverseScanRequest), br.GetInner().(*kvpb.ReverseScanResponse),
		)
		if err != nil {
			return kvpb.ResponseUnion{}, kvpb.NewError(err)
		}
		ru.MustSetInner(reverseScanResp)

	default:
		// This is only possible once we start decomposing read-write requests into
		// separate bits.
		panic("unimplemented")
	}

	// TODO(arul): in the future, when we'll evaluate CPuts locally, we'll have
	// this function take in the result of the KVGet, save the CPut function
	// locally on the transformation, and use these two things to evaluate the
	// condition here, on the client. We'll then construct and return the
	// appropriate response.

	return ru, nil
}

// transformations is a list of transformations applied by the txnWriteBuffer.
type transformations []transformation

func (t transformations) Empty() bool {
	return len(t) == 0
}

// addToBuffer adds a write to the given key to the buffer.
func (twb *txnWriteBuffer) addToBuffer(key roachpb.Key, val roachpb.Value, seq enginepb.TxnSeq) {
	it := twb.buffer.MakeIter()
	seek := &twb.bufferSeek
	seek.key = key

	it.FirstOverlap(seek)
	if it.Valid() {
		// We've already seen a write for this key.
		bw := it.Cur()
		bw.vals = append(bw.vals, bufferedValue{val: val, seq: seq})
	} else {
		twb.bufferIDAlloc++
		twb.buffer.Set(&bufferedWrite{
			id:   twb.bufferIDAlloc,
			key:  key,
			vals: []bufferedValue{{val: val, seq: seq}},
		})
	}
}

// flushWithEndTxn flushes all buffered writes to the KV layer along with the
// EndTxn request. Responses from the flushing are stripped before returning.
func (twb *txnWriteBuffer) flushWithEndTxn(
	ctx context.Context, ba *kvpb.BatchRequest,
) (*kvpb.BatchResponse, *kvpb.Error) {
	numBuffered := twb.buffer.Len()
	if numBuffered == 0 {
		return twb.wrapped.SendLocked(ctx, ba) // nothing to flush
	}
	// Iterate over the buffered writes and flush all buffered writes to the KV
	// layer by adding them to the batch.
	//
	// TODO(arul): If the batch request with the EndTxn request also contains an
	// overlapping write to a key that's already in the buffer, we could exclude
	// that write from the buffer.
	reqs := make([]kvpb.RequestUnion, 0, numBuffered+len(ba.Requests))
	it := twb.buffer.MakeIter()
	for it.First(); it.Valid(); it.Next() {
		reqs = append(reqs, it.Cur().toRequest())
	}

	ba = ba.ShallowCopy()
	reqs = append(reqs, ba.Requests...)
	ba.Requests = reqs

	br, pErr := twb.wrapped.SendLocked(ctx, ba)
	if pErr != nil {
		return nil, twb.adjustErrorUponFlush(ctx, numBuffered, pErr)
	}
	// Strip out responses for all the flushed buffered writes.
	br.Responses = br.Responses[numBuffered:]
	return br, nil
}

// hasBufferedWrites returns whether the interceptor has buffered any writes
// locally.
func (twb *txnWriteBuffer) hasBufferedWrites() bool {
	return twb.buffer.Len() > 0
}

// testingBufferedWritesAsSlice returns all buffered writes, in key order, as a
// slice.
func (twb *txnWriteBuffer) testingBufferedWritesAsSlice() []bufferedWrite {
	var writes []bufferedWrite
	it := twb.buffer.MakeIter()
	for it.First(); it.Valid(); it.Next() {
		bw := *it.Cur()
		// Scrub the id/endKey for the benefit of tests.
		bw.id = 0
		bw.endKey = nil
		writes = append(writes, bw)
	}
	return writes
}

// bufferedWrite is a buffered write operation to a given key. It maps a key to
// possibly multiple values[1], each with an associated sequence number.
//
// [1] A transaction is allowed to write to a single key multiple times. Of
// this, only the final write needs to be flushed to the KV layer. However, we
// track intermediate values in the buffer to support read-your-own-writes and
// savepoint rollbacks.
type bufferedWrite struct {
	id  uint64
	key roachpb.Key
	// TODO(arul): explore the possibility of using a b-tree which doesn't use an
	// endKey as a comparator. We could then remove this unnecessary field here,
	// and also in the keyLocks struct.
	endKey roachpb.Key // used in btree iteration
	// TODO(arul): instead of this slice, consider adding a small (fixed size,
	// maybe 1) array instead.
	vals []bufferedValue // sorted in increasing sequence number order
}

// bufferedValue is a value written to a key at a given sequence number.
type bufferedValue struct {
	val roachpb.Value
	seq enginepb.TxnSeq
}

// valPtr returns a pointer to the buffered value.
func (bv *bufferedValue) valPtr() *roachpb.Value {
	// TODO(arul): add a knob to return a pointer into the buffer instead of
	// creating a copy. As long as the caller doesn't modify the value we should
	// be fine; just have them opt into it.
	valCpy := bv.val
	return &valCpy
}

//go:generate ../../../util/interval/generic/gen.sh *bufferedWrite kvcoord

// Methods required by util/interval/generic type contract.

func (bw *bufferedWrite) ID() uint64          { return bw.id }
func (bw *bufferedWrite) Key() []byte         { return bw.key }
func (bw *bufferedWrite) EndKey() []byte      { return bw.endKey }
func (bw *bufferedWrite) String() string      { return bw.key.String() }
func (bw *bufferedWrite) New() *bufferedWrite { return new(bufferedWrite) }
func (bw *bufferedWrite) SetID(v uint64)      { bw.id = v }
func (bw *bufferedWrite) SetKey(v []byte)     { bw.key = v }
func (bw *bufferedWrite) SetEndKey(v []byte)  { bw.endKey = v }

func (bw *bufferedWrite) toRequest() kvpb.RequestUnion {
	var ru kvpb.RequestUnion
	// A key may be written to multiple times during the course of a transaction.
	// However, when flushing to KV, we only need to flush the most recent write
	// (read: the one with the highest sequence number). As we store values in
	// increasing sequence number order, this should be the last value in the
	// slice.
	val := bw.vals[len(bw.vals)-1]
	if val.val.IsPresent() {
		// TODO(arul): we could allocate PutRequest objects all at once when we're
		// about to flush the buffer. We'll probably want to keep track of the
		// number of each request type in the btree to avoid iterating and counting
		// each request type.
		//
		// TODO(arul): should we use a sync.Pool here?
		putAlloc := new(struct {
			put   kvpb.PutRequest
			union kvpb.RequestUnion_Put
		})
		putAlloc.put.Key = bw.key
		putAlloc.put.Value = val.val
		putAlloc.put.Sequence = val.seq
		putAlloc.union.Put = &putAlloc.put
		ru.Value = &putAlloc.union
	} else {
		delAlloc := new(struct {
			del   kvpb.DeleteRequest
			union kvpb.RequestUnion_Delete
		})
		delAlloc.del.Key = bw.key
		delAlloc.del.Sequence = val.seq
		delAlloc.union.Delete = &delAlloc.del
		ru.Value = &delAlloc.union
	}
	return ru
}

// respIter is an iterator over a scan or reverse scan response returned by
// the KV layer.
type respIter struct {
	// One and only one of scanReq/reverseScanReq (and by extension,
	// scanResp/reverseScanResp) should ever be set.
	scanReq         *kvpb.ScanRequest
	scanResp        *kvpb.ScanResponse
	reverseScanReq  *kvpb.ReverseScanRequest
	reverseScanResp *kvpb.ReverseScanResponse
	// scanFormat indicates the ScanFormat of the request. Only KEY_VALUES is
	// supported right now.
	scanFormat kvpb.ScanFormat

	rowsIndex int
}

// newScanRespIter constructs and returns a new iterator to iterate over a
// ScanRequest/Response.
func newScanRespIter(req *kvpb.ScanRequest, resp *kvpb.ScanResponse) *respIter {
	if req.ScanFormat != kvpb.KEY_VALUES {
		panic("unexpected")
	}
	return &respIter{
		scanReq:    req,
		scanResp:   resp,
		scanFormat: req.ScanFormat,
	}
}

// newReverseScanRespIter constructs and returns a new iterator to iterate over
// a ReverseScanRequest/Response.
func newReverseScanRespIter(
	req *kvpb.ReverseScanRequest, resp *kvpb.ReverseScanResponse,
) *respIter {
	if req.ScanFormat != kvpb.KEY_VALUES {
		panic("unexpected")
	}
	return &respIter{
		reverseScanReq:  req,
		reverseScanResp: resp,
		scanFormat:      req.ScanFormat,
	}
}

// peekKey returns the key at the current iterator position.
func (s *respIter) peekKey() roachpb.Key {
	if s.scanFormat == kvpb.KEY_VALUES {
		if s.scanReq != nil {
			return s.scanResp.Rows[s.rowsIndex].Key
		}
		return s.reverseScanResp.Rows[s.rowsIndex].Key
	}
	panic("unexpected")
}

// next moves the iterator forward.
func (s *respIter) next() {
	s.rowsIndex++
}

// valid returns whether the iterator is (still) positioned to a valid index.
func (s *respIter) valid() bool {
	if s.scanFormat == kvpb.KEY_VALUES {
		if s.scanReq != nil {
			return s.rowsIndex < len(s.scanResp.Rows)
		}
		return s.rowsIndex < len(s.reverseScanResp.Rows)
	}
	panic("unexpected")
}

// reset re-positions the iterator to the beginning of the response.
func (s *respIter) reset() {
	s.rowsIndex = 0
}

// startKey returns the start key of the request in response to which the
// iterator was created.
func (s *respIter) startKey() roachpb.Key {
	if s.scanReq != nil {
		return s.scanReq.Key
	}
	return s.reverseScanReq.Key
}

// endKey returns the end key of the request in response to which the iterator
// was created.
func (s *respIter) endKey() roachpb.Key {
	if s.scanReq != nil {
		return s.scanReq.EndKey
	}
	return s.reverseScanReq.EndKey
}

// seq returns the sequence number of the request in response to which the
// iterator was created.
func (s *respIter) seq() enginepb.TxnSeq {
	if s.scanReq != nil {
		return s.scanReq.Sequence
	}
	return s.reverseScanReq.Sequence
}

// respMerger encapsulates state to combine a {,Reverse}ScanResponse, returned
// by the KV layer, with any overlapping buffered writes to correctly uphold
// read-your-own-write semantics. It can be used to accumulate a response when
// merging a {,Reverse}ScanResponse with buffered writes.
type respMerger struct {
	serverRespIter *respIter
	// We should only ever be accumulating either one of scanResp or
	// reverseScanResp; the other field should be nil.
	scanResp        *kvpb.ScanResponse
	reverseScanResp *kvpb.ReverseScanResponse

	// rowsIdx tracks the position within Rows slice of the response to be
	// populated next.
	rowsIdx int
}

// makeRespMerger constructs and returns a new respMerger.
func makeRespMerger(serverSideRespIter *respIter, size int) respMerger {
	m := respMerger{
		serverRespIter: serverSideRespIter,
	}
	if serverSideRespIter.scanReq != nil {
		resp := serverSideRespIter.scanResp.ShallowCopy().(*kvpb.ScanResponse)
		if serverSideRespIter.scanFormat == kvpb.KEY_VALUES {
			resp.Rows = make([]roachpb.KeyValue, size)
		} else {
			panic("unexpected")
		}
		m.scanResp = resp
		return m
	}
	resp := serverSideRespIter.reverseScanResp.ShallowCopy().(*kvpb.ReverseScanResponse)
	if serverSideRespIter.scanFormat == kvpb.KEY_VALUES {
		resp.Rows = make([]roachpb.KeyValue, size)
	} else {
		panic("unexpected")
	}
	m.reverseScanResp = resp
	return m
}

// acceptKV takes a key and a value (presumably from the write buffer) and adds
// it to the result set.
func (m *respMerger) acceptKV(key roachpb.Key, value *roachpb.Value) {
	if m.serverRespIter.scanFormat == kvpb.KEY_VALUES {
		if m.serverRespIter.scanReq != nil {
			m.scanResp.Rows[m.rowsIdx] = roachpb.KeyValue{
				Key:   key,
				Value: *value,
			}
		} else {
			m.reverseScanResp.Rows[m.rowsIdx] = roachpb.KeyValue{
				Key:   key,
				Value: *value,
			}
		}
		m.rowsIdx++
		return
	}
	panic("unexpected")
}

// acceptServerResp accepts the current server response and adds it to the
// result set.
//
// Note that the iterator is not moved forward after accepting the response; the
// responsibility of doing so, if desired, is the caller's.
func (m *respMerger) acceptServerResp() {
	if m.serverRespIter.scanFormat == kvpb.KEY_VALUES {
		if m.serverRespIter.scanReq != nil {
			m.scanResp.Rows[m.rowsIdx] = m.serverRespIter.scanResp.Rows[m.serverRespIter.rowsIndex]
		} else {
			m.reverseScanResp.Rows[m.rowsIdx] = m.serverRespIter.reverseScanResp.Rows[m.serverRespIter.rowsIndex]
		}
		m.rowsIdx++
		return
	}
	panic("unexpected")
}

// toScanResp returns the final merged ScanResponse.
func (m *respMerger) toScanResp() *kvpb.ScanResponse {
	assertTrue(m.serverRespIter.scanReq != nil, "weren't accumulating a scan resp")
	// If we've done everything correctly, resIdx == len(response rows).
	if m.serverRespIter.scanFormat == kvpb.KEY_VALUES {
		assertTrue(m.rowsIdx == len(m.scanResp.Rows), "did not fill in all rows; did we miscount?")
		return m.scanResp
	}
	panic("unexpected")
}

// toReverseScanResp returns the final merged ReverseScanResponse.
func (m *respMerger) toReverseScanResp() *kvpb.ReverseScanResponse {
	assertTrue(m.serverRespIter.scanReq == nil, "weren't accumulating a reverse scan resp")
	// If we've done everything correctly, resIdx == len(response rows).
	if m.serverRespIter.scanFormat == kvpb.KEY_VALUES {
		assertTrue(m.rowsIdx == len(m.reverseScanResp.Rows), "did not fill in all rows; did we miscount?")
		return m.reverseScanResp
	}
	panic("unexpected")

}

// assertTrue panics with a message if the supplied condition isn't true.
func assertTrue(cond bool, msg string) {
	if !cond {
		panic(msg)
	}
}
