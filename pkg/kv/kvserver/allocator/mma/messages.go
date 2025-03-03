// Copyright 2023 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package mma

import (
	"time"

	"github.com/cockroachdb/cockroach/pkg/roachpb"
)

// Incoming messages for updating cluster state.
//
// This is a stop-gap and we will substitute these with protos.
//
// TODO(sumeer): add corresponding protos.

// storeLoadMsg is periodically sent by each store.
type storeLoadMsg struct {
	roachpb.StoreID

	load          loadVector
	capacity      loadVector
	secondaryLoad secondaryLoadVector
}

// nodeLoadMsg provides all the load information for a node and its
// constituent stores.
type nodeLoadMsg struct {
	nodeLoad
	stores   []storeLoadMsg
	loadTime time.Time
}

// storeLeaseholderMsg is sent by a local store and includes information about
// all ranges for which this store is the leaseholder. The range information
// includes other replica stores. This is a local message and will be sent
// before every allocator pass, so that the allocator has the latest state to
// make decisions.
type storeLeaseholderMsg struct {
	roachpb.StoreID

	// ranges provides authoritative information from the leaseholder.
	ranges []rangeMsg
}

// rangeMsg is generated by the leaseholder store (and part of
// storeLeaseholderMsg). If there is any change for that range, the full
// information for that range is provided. This is also the case for a new
// leaseholder since it does not know whether something has changed since the
// last leaseholder informed the allocator. A tiny change to the rangeLoad
// (decided by the caller) will not cause a rangeMsg.
//
// Also used to tell the allocator about ranges that no longer exist.
//
// TODO(sumeeer): these diff semantics are ok for now, but we may decide to
// incorporate the diffing logic into the allocator after the first code
// iteration.
type rangeMsg struct {
	roachpb.RangeID
	replicas  []storeIDAndReplicaState
	conf      roachpb.SpanConfig
	rangeLoad rangeLoad
}

func (rm *rangeMsg) isDeletedRange() bool {
	return len(rm.replicas) == 0
}

// Avoid unused lint errors.

var _ = (&rangeMsg{}).isDeletedRange
var _ = storeLoadMsg{}.StoreID
var _ = storeLoadMsg{}.load
var _ = storeLoadMsg{}.capacity
var _ = storeLoadMsg{}.secondaryLoad
var _ = storeLeaseholderMsg{}.StoreID
var _ = storeLeaseholderMsg{}.ranges
var _ = rangeMsg{}.RangeID
var _ = rangeMsg{}.replicas
var _ = rangeMsg{}.conf
var _ = rangeMsg{}.rangeLoad
var _ = nodeLoadMsg{}.nodeLoad
var _ = nodeLoadMsg{}.stores
var _ = nodeLoadMsg{}.loadTime
