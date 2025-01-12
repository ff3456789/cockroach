// Copyright 2019 The Cockroach Authors.
//
// Use of this software is governed by the Business Source License
// included in the file licenses/BSL.txt.
//
// As of the Change Date specified in that file, in accordance with
// the Business Source License, use of this software will be governed
// by the Apache License, Version 2.0, included in the file
// licenses/APL.txt.

// Package protectedts exports the interface to the protected timestamp
// subsystem which allows clients to prevent GC of expired data.
package protectedts

import (
	"context"
	"errors"

	"github.com/cockroachdb/cockroach/pkg/internal/client"
	"github.com/cockroachdb/cockroach/pkg/roachpb"
	"github.com/cockroachdb/cockroach/pkg/storage/protectedts/ptpb"
	"github.com/cockroachdb/cockroach/pkg/util/hlc"
	"github.com/cockroachdb/cockroach/pkg/util/stop"
	"github.com/cockroachdb/cockroach/pkg/util/uuid"
)

// ErrNotExists is returned from Get or Release if the record does
// not exist.
var ErrNotExists = errors.New("protected timestamp record does not exist")

// ErrExists returned from Protect when trying to protect a record
// with an ID which already exists.
var ErrExists = errors.New("protected timestamp record already exists")

// Provider is the central coordinator for the protectedts subsystem.
// It exists to abstract interaction with subsystem.
type Provider interface {
	Storage
	Tracker
	Verifier

	Start(context.Context, *stop.Stopper) error
}

// Storage provides clients with a mechanism to transactionally protect and
// release protected timestamps for a set of spans.
//
// Clients may provide a txn object which will allow them to write the id
// of this new protection transactionally into their own state.
// It is the caller's responsibility to ensure that a timestamp is ultimately
// released.
type Storage interface {

	// Protect will durably create a protected timestamp, if no error is returned
	// then no data in the specified spans which are live at the specified
	// timestamp can be garbage collected until this Record is released.
	//
	// Protect may succeed and yet data may be or already have been garbage
	// collected in the spans specified by the Record. However, the protected
	// timestamp subsystem guarantees that if all possible zone configs which
	// could have applied have GC TTLs which would not have allowed data at
	// the timestamp which the passed Txn commits to be GC'd then that
	// data will not be GC'd until this *Record is released.
	//
	// An error will be returned if the ID of the provided record already exists
	// so callers should be sure to generate new IDs when creating records.
	Protect(context.Context, *client.Txn, *ptpb.Record) error

	// GetRecord retreives the record with the specified UUID as well as the MVCC
	// timestamp at which it was written. If no corresponding record exists
	// ErrNotExists is returned.
	//
	// GetRecord exists to work in coordination with verification. In order
	// to use Verifier.Verify a client must provide both the timestamp which
	// should be protected as well as the timestamp at which the Record providing
	// that protection is known to be alive. The ReadTimestamp of the Txn used in
	// this method can be used to provide such a timestamp.
	GetRecord(context.Context, *client.Txn, uuid.UUID) (*ptpb.Record, error)

	// MarkVerified will mark a protected timestamp as verified.
	//
	// This method is generally used by an implementation of Verifier.
	MarkVerified(context.Context, *client.Txn, uuid.UUID) error

	// Release allows spans which were previously protected to now be garbage
	// collected.
	//
	// If the specified UUID does not exist ErrNotFound is returned but the
	// passed txn remains safe for future use.
	Release(context.Context, *client.Txn, uuid.UUID) error

	// GetMetadata retreives the metadata with the provided Txn.
	GetMetadata(context.Context, *client.Txn) (ptpb.Metadata, error)

	// GetState retreives the entire state of protectedts.Storage with the
	// provided Txn.
	GetState(context.Context, *client.Txn) (ptpb.State, error)
}

// Tracker is used in the storage package to determine a safe timestamp for
// garbage collection of expired data. A storage.Replica can remove data when
// it has a proof from the Tracker that there was no Record providing
// protection. For example, a Replica which determines that it is not protected
// by any Records at a given asOf can move its GC threshold up to that
// timestamp less its GC TTL.
type Tracker interface {

	// ProtectedBy calls the passed function for each record which overlaps the
	// provided Span. The return value is the MVCC timestamp at which this set of
	// records is known to be valid.
	ProtectedBy(context.Context, roachpb.Span, func(*ptpb.Record)) (asOf hlc.Timestamp)
}

// Verifier provides a mechanism to verify that a created Record will certainly
// apply.
type Verifier interface {

	// Verify returns an error if the record of the provided ID cannot be
	// verified. If nil is returned then the record has been proven to apply
	// until it is removed.
	Verify(context.Context, uuid.UUID) error
}

// ClockTracker returns a tracker which always returns the current time and no
// records. This is often useful in testing where you want a tracker which
// protects nothing and is always up-to-date.
func ClockTracker(c *hlc.Clock) Tracker {
	return (*clockTracker)(c)
}

type clockTracker hlc.Clock

func (t *clockTracker) ProtectedBy(
	context.Context, roachpb.Span, func(*ptpb.Record),
) (asOf hlc.Timestamp) {
	return (*hlc.Clock)(t).Now()
}
