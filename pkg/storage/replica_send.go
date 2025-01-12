// Copyright 2019 The Cockroach Authors.
//
// Use of this software is governed by the Business Source License
// included in the file licenses/BSL.txt.
//
// As of the Change Date specified in that file, in accordance with
// the Business Source License, use of this software will be governed
// by the Apache License, Version 2.0, included in the file
// licenses/APL.txt.

package storage

import (
	"context"
	"reflect"

	"github.com/cockroachdb/cockroach/pkg/roachpb"
	"github.com/cockroachdb/cockroach/pkg/storage/batcheval"
	"github.com/cockroachdb/cockroach/pkg/storage/intentresolver"
	"github.com/cockroachdb/cockroach/pkg/storage/spanlatch"
	"github.com/cockroachdb/cockroach/pkg/storage/spanset"
	"github.com/cockroachdb/cockroach/pkg/storage/storagepb"
	"github.com/cockroachdb/cockroach/pkg/storage/txnwait"
	"github.com/cockroachdb/cockroach/pkg/util/hlc"
	"github.com/cockroachdb/cockroach/pkg/util/log"
	"github.com/cockroachdb/cockroach/pkg/util/tracing"
	opentracing "github.com/opentracing/opentracing-go"
	"github.com/pkg/errors"
)

// Send executes a command on this range, dispatching it to the
// read-only, read-write, or admin execution path as appropriate.
// ctx should contain the log tags from the store (and up).
func (r *Replica) Send(
	ctx context.Context, ba roachpb.BatchRequest,
) (*roachpb.BatchResponse, *roachpb.Error) {
	return r.sendWithRangeID(ctx, r.RangeID, &ba)
}

// sendWithRangeID takes an unused rangeID argument so that the range
// ID will be accessible in stack traces (both in panics and when
// sampling goroutines from a live server). This line is subject to
// the whims of the compiler and it can be difficult to find the right
// value, but as of this writing the following example shows a stack
// while processing range 21 (0x15) (the first occurrence of that
// number is the rangeID argument, the second is within the encoded
// BatchRequest, although we don't want to rely on that occurring
// within the portion printed in the stack trace):
//
// github.com/cockroachdb/cockroach/pkg/storage.(*Replica).sendWithRangeID(0xc420d1a000, 0x64bfb80, 0xc421564b10, 0x15, 0x153fd4634aeb0193, 0x0, 0x100000001, 0x1, 0x15, 0x0, ...)
func (r *Replica) sendWithRangeID(
	ctx context.Context, rangeID roachpb.RangeID, ba *roachpb.BatchRequest,
) (*roachpb.BatchResponse, *roachpb.Error) {
	var br *roachpb.BatchResponse
	if r.leaseholderStats != nil && ba.Header.GatewayNodeID != 0 {
		r.leaseholderStats.record(ba.Header.GatewayNodeID)
	}

	// Add the range log tag.
	ctx = r.AnnotateCtx(ctx)
	ctx, cleanup := tracing.EnsureContext(ctx, r.AmbientContext.Tracer, "replica send")
	defer cleanup()

	// If the internal Raft group is not initialized, create it and wake the leader.
	r.maybeInitializeRaftGroup(ctx)

	isReadOnly := ba.IsReadOnly()
	useRaft := !isReadOnly && ba.IsWrite()

	if err := r.checkBatchRequest(ba, isReadOnly); err != nil {
		return nil, roachpb.NewError(err)
	}

	if err := r.maybeBackpressureBatch(ctx, ba); err != nil {
		return nil, roachpb.NewError(err)
	}

	// NB: must be performed before collecting request spans.
	ba, err := maybeStripInFlightWrites(ba)
	if err != nil {
		return nil, roachpb.NewError(err)
	}

	if filter := r.store.cfg.TestingKnobs.TestingRequestFilter; filter != nil {
		if pErr := filter(*ba); pErr != nil {
			return nil, pErr
		}
	}

	// Differentiate between read-write, read-only, and admin.
	var pErr *roachpb.Error
	if useRaft {
		log.Event(ctx, "read-write path")
		fn := (*Replica).executeWriteBatch
		br, pErr = r.executeBatchWithConcurrencyRetries(ctx, ba, fn)
	} else if isReadOnly {
		log.Event(ctx, "read-only path")
		fn := (*Replica).executeReadOnlyBatch
		br, pErr = r.executeBatchWithConcurrencyRetries(ctx, ba, fn)
	} else if ba.IsAdmin() {
		log.Event(ctx, "admin path")
		br, pErr = r.executeAdminBatch(ctx, ba)
	} else if len(ba.Requests) == 0 {
		// empty batch; shouldn't happen (we could handle it, but it hints
		// at someone doing weird things, and once we drop the key range
		// from the header it won't be clear how to route those requests).
		log.Fatalf(ctx, "empty batch")
	} else {
		log.Fatalf(ctx, "don't know how to handle command %s", ba)
	}
	if pErr != nil {
		log.Eventf(ctx, "replica.Send got error: %s", pErr)
	} else {
		if filter := r.store.cfg.TestingKnobs.TestingResponseFilter; filter != nil {
			pErr = filter(*ba, br)
		}
	}
	return br, pErr
}

// batchExecutionFn is a method on Replica that is able to execute a
// BatchRequest. It is called with the batch, along with the span bounds that
// the batch will operate over and a guard for the latches protecting the span
// bounds. The function must ensure that the latch guard is eventually released.
type batchExecutionFn func(
	*Replica, context.Context, *roachpb.BatchRequest, *spanset.SpanSet, *spanlatch.Guard,
) (*roachpb.BatchResponse, *roachpb.Error)

var _ batchExecutionFn = (*Replica).executeWriteBatch
var _ batchExecutionFn = (*Replica).executeReadOnlyBatch

// executeBatchWithConcurrencyRetries is the entry point for client (non-admin)
// requests that execute against the range's state. The method coordinates the
// execution of requests that may require multiple retries due to interactions
// with concurrent transactions.
//
// The method acquires latches for the request, which synchronizes it with
// conflicting requests. This permits the execution function to run without
// concern of coordinating with logically conflicting operations, although it
// still needs to worry about coordinating with non-conflicting operations when
// accessing shared data structures.
//
// If the execution function hits a concurrency error like a WriteIntentError or
// a TransactionPushError it will propagate the error back to this method, which
// handles the process of retrying batch execution after addressing the error.
func (r *Replica) executeBatchWithConcurrencyRetries(
	ctx context.Context, ba *roachpb.BatchRequest, fn batchExecutionFn,
) (br *roachpb.BatchResponse, pErr *roachpb.Error) {
	// Determine the maximal set of key spans that the batch will operate on.
	spans, err := r.collectSpans(ba)
	if err != nil {
		return nil, roachpb.NewError(err)
	}

	// Handle load-based splitting.
	r.recordBatchForLoadBasedSplitting(ctx, ba, spans)

	// TODO(nvanbenschoten): Clean this up once it's pulled inside the
	// concurrency manager.
	var cleanup intentresolver.CleanupFunc
	defer func() {
		if cleanup != nil {
			// This request wrote an intent only if there was no error, the
			// request is transactional, the transaction is not yet finalized,
			// and the request wasn't read-only.
			if pErr == nil && ba.Txn != nil && !br.Txn.Status.IsFinalized() && !ba.IsReadOnly() {
				cleanup(nil, &br.Txn.TxnMeta)
			} else {
				cleanup(nil, nil)
			}
		}
	}()

	// Try to execute command; exit retry loop on success.
	for {
		// Exit loop if context has been canceled or timed out.
		if err := ctx.Err(); err != nil {
			return nil, roachpb.NewError(errors.Wrap(err, "aborted during Replica.Send"))
		}

		// If necessary, the request may need to wait in the txn wait queue,
		// pending updates to the target transaction for either PushTxn or
		// QueryTxn requests.
		// TODO(nvanbenschoten): Push this into the concurrency package.
		br, pErr = r.maybeWaitForPushee(ctx, ba)
		if br != nil || pErr != nil {
			return br, pErr
		}

		// Acquire latches to prevent overlapping commands from executing until
		// this command completes.
		// TODO(nvanbenschoten): Replace this with a call into the upcoming
		// concurrency package when it is introduced.
		lg, err := r.beginCmds(ctx, ba, spans)
		if err != nil {
			return nil, roachpb.NewError(err)
		}

		br, pErr = fn(r, ctx, ba, spans, lg)
		switch t := pErr.GetDetail().(type) {
		case nil:
			// Success.
			return br, nil
		case *roachpb.WriteIntentError:
			if cleanup, pErr = r.handleWriteIntentError(ctx, ba, pErr, t, cleanup); pErr != nil {
				return nil, pErr
			}
			// Retry...
		case *roachpb.TransactionPushError:
			if pErr = r.handleTransactionPushError(ctx, ba, pErr, t); pErr != nil {
				return nil, pErr
			}
			// Retry...
		case *roachpb.IndeterminateCommitError:
			if pErr = r.handleIndeterminateCommitError(ctx, ba, pErr, t); pErr != nil {
				return nil, pErr
			}
			// Retry...
		case *roachpb.MergeInProgressError:
			if pErr = r.handleMergeInProgressError(ctx, ba, pErr, t); pErr != nil {
				return nil, pErr
			}
			// Retry...
		default:
			// Propagate error.
			return nil, pErr
		}
	}
}

func (r *Replica) handleWriteIntentError(
	ctx context.Context,
	ba *roachpb.BatchRequest,
	pErr *roachpb.Error,
	t *roachpb.WriteIntentError,
	cleanup intentresolver.CleanupFunc,
) (intentresolver.CleanupFunc, *roachpb.Error) {
	if r.store.cfg.TestingKnobs.DontPushOnWriteIntentError {
		return cleanup, pErr
	}

	// Process and resolve write intent error.
	var pushType roachpb.PushTxnType
	if ba.IsWrite() {
		pushType = roachpb.PUSH_ABORT
	} else {
		pushType = roachpb.PUSH_TIMESTAMP
	}

	index := pErr.Index
	args := ba.Requests[index.Index].GetInner()
	// Make a copy of the header for the upcoming push; we will update the
	// timestamp.
	h := ba.Header
	if h.Txn != nil {
		// We must push at least to h.Timestamp, but in fact we want to
		// go all the way up to a timestamp which was taken off the HLC
		// after our operation started. This allows us to not have to
		// restart for uncertainty as we come back and read.
		obsTS, ok := h.Txn.GetObservedTimestamp(ba.Replica.NodeID)
		if !ok {
			// This was set earlier in this method, so it's
			// completely unexpected to not be found now.
			log.Fatalf(ctx, "missing observed timestamp: %+v", h.Txn)
		}
		h.Timestamp.Forward(obsTS)
		// We are going to hand the header (and thus the transaction proto)
		// to the RPC framework, after which it must not be changed (since
		// that could race). Since the subsequent execution of the original
		// request might mutate the transaction, make a copy here.
		//
		// See #9130.
		h.Txn = h.Txn.Clone()
	}

	// Handle the case where we get more than one write intent error;
	// we need to cleanup the previous attempt to handle it to allow
	// any other pusher queued up behind this RPC to proceed.
	if cleanup != nil {
		cleanup(t, nil)
	}
	cleanup, pErr = r.store.intentResolver.ProcessWriteIntentError(ctx, pErr, args, h, pushType)
	if pErr != nil {
		// Do not propagate ambiguous results; assume success and retry original op.
		if _, ok := pErr.GetDetail().(*roachpb.AmbiguousResultError); ok {
			return cleanup, nil
		}
		// Propagate new error. Preserve the error index.
		pErr.Index = index
		return cleanup, pErr
	}
	// We've resolved the write intent; retry command.
	return cleanup, nil
}

func (r *Replica) handleTransactionPushError(
	ctx context.Context,
	ba *roachpb.BatchRequest,
	pErr *roachpb.Error,
	t *roachpb.TransactionPushError,
) *roachpb.Error {
	// On a transaction push error, retry immediately if doing so will enqueue
	// into the txnWaitQueue in order to await further updates to the unpushed
	// txn's status. We check ShouldPushImmediately to avoid retrying
	// non-queueable PushTxnRequests (see #18191).
	dontRetry := r.store.cfg.TestingKnobs.DontRetryPushTxnFailures
	if !dontRetry && ba.IsSinglePushTxnRequest() {
		pushReq := ba.Requests[0].GetInner().(*roachpb.PushTxnRequest)
		dontRetry = txnwait.ShouldPushImmediately(pushReq)
	}
	if dontRetry {
		return pErr
	}
	// Enqueue unsuccessfully pushed transaction on the txnWaitQueue and retry.
	r.txnWaitQueue.Enqueue(&t.PusheeTxn)
	return nil
}

func (r *Replica) handleIndeterminateCommitError(
	ctx context.Context,
	ba *roachpb.BatchRequest,
	pErr *roachpb.Error,
	t *roachpb.IndeterminateCommitError,
) *roachpb.Error {
	if r.store.cfg.TestingKnobs.DontRecoverIndeterminateCommits {
		return pErr
	}
	// On an indeterminate commit error, attempt to recover and finalize the
	// stuck transaction. Retry immediately if successful.
	if _, err := r.store.recoveryMgr.ResolveIndeterminateCommit(ctx, t); err != nil {
		// Do not propagate ambiguous results; assume success and retry original op.
		if _, ok := err.(*roachpb.AmbiguousResultError); ok {
			return nil
		}
		// Propagate new error. Preserve the error index.
		newPErr := roachpb.NewError(err)
		newPErr.Index = pErr.Index
		return newPErr
	}
	// We've recovered the transaction that blocked the push; retry command.
	return nil
}

func (r *Replica) handleMergeInProgressError(
	ctx context.Context,
	ba *roachpb.BatchRequest,
	pErr *roachpb.Error,
	t *roachpb.MergeInProgressError,
) *roachpb.Error {
	// A merge was in progress. We need to retry the command after the merge
	// completes, as signaled by the closing of the replica's mergeComplete
	// channel. Note that the merge may have already completed, in which case
	// its mergeComplete channel will be nil.
	mergeCompleteCh := r.getMergeCompleteCh()
	if mergeCompleteCh == nil {
		// Merge no longer in progress. Retry the command.
		return nil
	}
	log.Event(ctx, "waiting on in-progress merge")
	select {
	case <-mergeCompleteCh:
		// Merge complete. Retry the command.
		return nil
	case <-ctx.Done():
		return roachpb.NewError(errors.Wrap(ctx.Err(), "aborted during merge"))
	case <-r.store.stopper.ShouldQuiesce():
		return roachpb.NewError(&roachpb.NodeUnavailableError{})
	}
}

// executeAdminBatch executes the command directly. There is no interaction
// with the spanlatch manager or the timestamp cache, as admin commands
// are not meant to consistently access or modify the underlying data.
// Admin commands must run on the lease holder replica. Batch support here is
// limited to single-element batches; everything else catches an error.
func (r *Replica) executeAdminBatch(
	ctx context.Context, ba *roachpb.BatchRequest,
) (*roachpb.BatchResponse, *roachpb.Error) {
	if len(ba.Requests) != 1 {
		return nil, roachpb.NewErrorf("only single-element admin batches allowed")
	}

	args := ba.Requests[0].GetInner()
	if sp := opentracing.SpanFromContext(ctx); sp != nil {
		sp.SetOperationName(reflect.TypeOf(args).String())
	}

	// Admin commands always require the range lease.
	status, pErr := r.redirectOnOrAcquireLease(ctx)
	if pErr != nil {
		return nil, pErr
	}
	// Note there is no need to limit transaction max timestamp on admin requests.

	// Verify that the batch can be executed.
	// NB: we pass nil for the spanlatch guard because we haven't acquired
	// latches yet. This is ok because each individual request that the admin
	// request sends will acquire latches.
	if err := r.checkExecutionCanProceed(ba, nil /* lg */, &status); err != nil {
		return nil, roachpb.NewError(err)
	}

	var resp roachpb.Response
	switch tArgs := args.(type) {
	case *roachpb.AdminSplitRequest:
		var reply roachpb.AdminSplitResponse
		reply, pErr = r.AdminSplit(ctx, *tArgs, "manual")
		resp = &reply

	case *roachpb.AdminUnsplitRequest:
		var reply roachpb.AdminUnsplitResponse
		reply, pErr = r.AdminUnsplit(ctx, *tArgs, "manual")
		resp = &reply

	case *roachpb.AdminMergeRequest:
		var reply roachpb.AdminMergeResponse
		reply, pErr = r.AdminMerge(ctx, *tArgs, "manual")
		resp = &reply

	case *roachpb.AdminTransferLeaseRequest:
		pErr = roachpb.NewError(r.AdminTransferLease(ctx, tArgs.Target))
		resp = &roachpb.AdminTransferLeaseResponse{}

	case *roachpb.AdminChangeReplicasRequest:
		chgs := tArgs.Changes()
		desc, err := r.ChangeReplicas(ctx, &tArgs.ExpDesc, SnapshotRequest_REBALANCE, storagepb.ReasonAdminRequest, "", chgs)
		pErr = roachpb.NewError(err)
		if pErr != nil {
			resp = &roachpb.AdminChangeReplicasResponse{}
		} else {
			resp = &roachpb.AdminChangeReplicasResponse{
				Desc: *desc,
			}
		}

	case *roachpb.AdminRelocateRangeRequest:
		err := r.store.AdminRelocateRange(ctx, *r.Desc(), tArgs.Targets)
		pErr = roachpb.NewError(err)
		resp = &roachpb.AdminRelocateRangeResponse{}

	case *roachpb.CheckConsistencyRequest:
		var reply roachpb.CheckConsistencyResponse
		reply, pErr = r.CheckConsistency(ctx, *tArgs)
		resp = &reply

	case *roachpb.ImportRequest:
		cArgs := batcheval.CommandArgs{
			EvalCtx: NewReplicaEvalContext(r, todoSpanSet),
			Header:  ba.Header,
			Args:    args,
		}
		var err error
		resp, err = importCmdFn(ctx, cArgs)
		pErr = roachpb.NewError(err)

	case *roachpb.AdminScatterRequest:
		reply, err := r.adminScatter(ctx, *tArgs)
		pErr = roachpb.NewError(err)
		resp = &reply

	default:
		return nil, roachpb.NewErrorf("unrecognized admin command: %T", args)
	}

	if pErr != nil {
		return nil, pErr
	}

	if ba.Header.ReturnRangeInfo {
		returnRangeInfo(resp, r)
	}

	br := &roachpb.BatchResponse{}
	br.Add(resp)
	br.Txn = resp.Header().Txn
	return br, nil
}

// checkBatchRequest verifies BatchRequest validity requirements. In particular,
// the batch must have an assigned timestamp, and either all requests must be
// read-only, or none.
//
// TODO(tschottdorf): should check that request is contained in range
// and that EndTransaction only occurs at the very end.
func (r *Replica) checkBatchRequest(ba *roachpb.BatchRequest, isReadOnly bool) error {
	if ba.Timestamp == (hlc.Timestamp{}) {
		// For transactional requests, Store.Send sets the timestamp. For non-
		// transactional requests, the client sets the timestamp. Either way, we
		// need to have a timestamp at this point.
		return errors.New("Replica.checkBatchRequest: batch does not have timestamp assigned")
	}
	consistent := ba.ReadConsistency == roachpb.CONSISTENT
	if isReadOnly {
		if !consistent && ba.Txn != nil {
			// Disallow any inconsistent reads within txns.
			return errors.Errorf("cannot allow %v reads within a transaction", ba.ReadConsistency)
		}
	} else if !consistent {
		return errors.Errorf("%v mode is only available to reads", ba.ReadConsistency)
	}

	return nil
}

func (r *Replica) collectSpans(ba *roachpb.BatchRequest) (*spanset.SpanSet, error) {
	spans := &spanset.SpanSet{}
	// TODO(bdarnell): need to make this less global when local
	// latches are used more heavily. For example, a split will
	// have a large read-only span but also a write (see #10084).
	// Currently local spans are the exception, so preallocate for the
	// common case in which all are global. We rarely mix read and
	// write commands, so preallocate for writes if there are any
	// writes present in the batch.
	//
	// TODO(bdarnell): revisit as the local portion gets its appropriate
	// use.
	if ba.IsReadOnly() {
		spans.Reserve(spanset.SpanReadOnly, spanset.SpanGlobal, len(ba.Requests))
	} else {
		guess := len(ba.Requests)
		if et, ok := ba.GetArg(roachpb.EndTransaction); ok {
			// EndTransaction declares a global write for each of its intent spans.
			guess += len(et.(*roachpb.EndTransactionRequest).IntentSpans) - 1
		}
		spans.Reserve(spanset.SpanReadWrite, spanset.SpanGlobal, guess)
	}

	// For non-local, MVCC spans we annotate them with the request timestamp
	// during declaration. This is the timestamp used during latch acquisitions.
	// For read requests this works as expected, reads are performed at the same
	// timestamp. During writes however, we may encounter a versioned value newer
	// than the request timestamp, and may have to retry at a higher timestamp.
	// This is still safe as we're only ever writing at timestamps higher than the
	// timestamp any write latch would be declared at.
	desc := r.Desc()
	batcheval.DeclareKeysForBatch(desc, ba.Header, spans)
	for _, union := range ba.Requests {
		inner := union.GetInner()
		if cmd, ok := batcheval.LookupCommand(inner.Method()); ok {
			cmd.DeclareKeys(desc, ba.Header, inner, spans)
		} else {
			return nil, errors.Errorf("unrecognized command %s", inner.Method())
		}
	}

	// Commands may create a large number of duplicate spans. De-duplicate
	// them to reduce the number of spans we pass to the spanlatch manager.
	spans.SortAndDedup()

	// If any command gave us spans that are invalid, bail out early
	// (before passing them to the spanlatch manager, which may panic).
	if err := spans.Validate(); err != nil {
		return nil, err
	}
	return spans, nil
}

// limitTxnMaxTimestamp limits the batch transaction's max timestamp
// so that it respects any timestamp already observed on this node.
// This prevents unnecessary uncertainty interval restarts caused by
// reading a value written at a timestamp between txn.Timestamp and
// txn.MaxTimestamp. The replica lease's start time is also taken into
// consideration to ensure that a lease transfer does not result in
// the observed timestamp for this node being inapplicable to data
// previously written by the former leaseholder. To wit:
//
// 1. put(k on leaseholder n1), gateway chooses t=1.0
// 2. begin; read(unrelated key on n2); gateway chooses t=0.98
// 3. pick up observed timestamp for n2 of t=0.99
// 4. n1 transfers lease for range with k to n2 @ t=1.1
// 5. read(k) on leaseholder n2 at ReadTimestamp=0.98 should get
//    ReadWithinUncertaintyInterval because of the write in step 1, so
//    even though we observed n2's timestamp in step 3 we must expand
//    the uncertainty interval to the lease's start time, which is
//    guaranteed to be greater than any write which occurred under
//    the previous leaseholder.
func (r *Replica) limitTxnMaxTimestamp(
	ctx context.Context, ba *roachpb.BatchRequest, status storagepb.LeaseStatus,
) {
	if ba.Txn == nil {
		return
	}
	// For calls that read data within a txn, we keep track of timestamps
	// observed from the various participating nodes' HLC clocks. If we have
	// a timestamp on file for this Node which is smaller than MaxTimestamp,
	// we can lower MaxTimestamp accordingly. If MaxTimestamp drops below
	// ReadTimestamp, we effectively can't see uncertainty restarts anymore.
	// TODO(nvanbenschoten): This should use the lease's node id.
	obsTS, ok := ba.Txn.GetObservedTimestamp(ba.Replica.NodeID)
	if !ok {
		return
	}
	// If the lease is valid, we use the greater of the observed
	// timestamp and the lease start time, up to the max timestamp. This
	// ensures we avoid incorrect assumptions about when data was
	// written, in absolute time on a different node, which held the
	// lease before this replica acquired it.
	// TODO(nvanbenschoten): Do we ever need to call this when
	//   status.State != VALID?
	if status.State == storagepb.LeaseState_VALID {
		obsTS.Forward(status.Lease.Start)
	}
	if obsTS.Less(ba.Txn.MaxTimestamp) {
		// Copy-on-write to protect others we might be sharing the Txn with.
		txnClone := ba.Txn.Clone()
		// The uncertainty window is [ReadTimestamp, maxTS), so if that window
		// is empty, there won't be any uncertainty restarts.
		if !ba.Txn.ReadTimestamp.Less(obsTS) {
			log.Event(ctx, "read has no clock uncertainty")
		}
		txnClone.MaxTimestamp.Backward(obsTS)
		ba.Txn = txnClone
	}
}
