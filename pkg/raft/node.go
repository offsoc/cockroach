// This code has been modified from its original form by The Cockroach Authors.
// All modifications are Copyright 2024 The Cockroach Authors.
//
// Copyright 2015 The etcd Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package raft

import (
	"fmt"
	"iter"
	"strings"

	pb "github.com/cockroachdb/cockroach/pkg/raft/raftpb"
)

type SnapshotStatus int

const (
	SnapshotFinish  SnapshotStatus = 1
	SnapshotFailure SnapshotStatus = 2
)

var emptyState = pb.HardState{}

// SoftState provides state that is useful for logging and debugging.
// The state is volatile and does not need to be persisted to the WAL.
type SoftState struct {
	RaftState pb.StateType
}

func (a *SoftState) equal(b *SoftState) bool {
	return a.RaftState == b.RaftState
}

// StorageAppend describes a storage append request. It contains the updates
// that must be written to storage atomically, such as new HardState, snapshot,
// and/or log entries. When the updates are durable on the log storage, the
// attached messages can be sent.
//
// StorageAppend must be applied to log storage atomically, and in full. All
// storage appends must be handled exactly once, in the order they are issued.
type StorageAppend struct {
	// The current HardState to be saved to stable storage. Empty if there is no
	// update to the HardState.
	pb.HardState
	// Entries contains the log entries to be appended to the log in storage.
	// Empty if there are no new entries.
	//
	// Invariant: Entries[0].Index <= LogStorage.LastIndex() + 1.
	//
	// All entries >= Entry[0].Index, if any, must be truncated from the log, and
	// Entries are written to replace them.
	Entries []pb.Entry
	// Snapshot is the snapshot to be saved to stable storage. Empty if there is
	// no snapshot.
	//
	// When installing a snapshot, the raft log must be cleared and initialized to
	// a state according to the Snapshot.Metadata.{Index,Term}.
	Snapshot *pb.Snapshot
	// LeadTerm is the term of the leader on whose behalf the storage write is
	// being made. Populated if Entries or Snapshot is not empty. A non-empty
	// LeadTerm never regresses, as well as the StorageAppend.Mark().
	LeadTerm uint64
	// Responses contains messages that should be sent AFTER the updates above
	// have been *durably* persisted in log storage. Messages addressed to the
	// local RawNode can be stepped into it directly.
	//
	// TODO(pav-kv): try to make it private. Currently, there is one use in
	// handleRaftReady that filters through these messages directly.
	Responses []pb.Message
}

// Empty returns true iff the storage append is empty.
func (m *StorageAppend) Empty() bool {
	return IsEmptyHardState(m.HardState) &&
		len(m.Entries) == 0 && m.Snapshot == nil && len(m.Responses) == 0
}

// Mark returns a non-empty log mark if the storage write has a snapshot or
// entries. Not-empty marks do not regress across consecutive storage writes.
func (m *StorageAppend) Mark() LogMark {
	if ln := len(m.Entries); ln > 0 {
		return LogMark{Term: m.LeadTerm, Index: m.Entries[ln-1].Index}
	} else if snap := m.Snapshot; snap != nil {
		return LogMark{Term: m.LeadTerm, Index: snap.Metadata.Index}
	}
	return LogMark{}
}

// MustSync returns true if this storage write must be synced.
//
// A storage write must be synced if there are durability-conditioned messages
// to be sent to the proposer (candidate or leader) after this write. Typically,
// a MsgVoteResp or MsgAppResp. The recipient of these messages can be the local
// RawNode, or a remote one.
func (m *StorageAppend) MustSync() bool {
	return len(m.Responses) != 0
}

// NeedAck returns true if the RawNode wants to be notified after the writes are
// durable on the log storage.
func (m *StorageAppend) NeedAck() bool {
	return len(m.Entries) != 0 || m.Snapshot != nil
}

// Ack returns the acknowledgement that should be used to notify
// RawNode.AckAppend after the write is durable on the log storage.
func (m *StorageAppend) Ack() StorageAppendAck {
	ack := StorageAppendAck{Mark: m.Mark(), responses: m.Responses}
	if snap := m.Snapshot; snap != nil {
		ack.SnapIndex = snap.Metadata.Index
	}
	return ack
}

// Describe returns a string representation of this storage append.
func (m *StorageAppend) Describe(f EntryFormatter) string {
	var buf strings.Builder
	if hs := m.HardState; !IsEmptyHardState(hs) {
		_, _ = fmt.Fprintf(&buf, "HardState {%s}\n", DescribeHardState(hs))
	}
	if snap := m.Snapshot; snap != nil {
		_, _ = fmt.Fprintf(&buf, "Snapshot %s\n", DescribeSnapshot(*snap))
	}
	if ln := len(m.Entries); ln == 1 {
		_, _ = fmt.Fprintf(&buf, "Entry: %s\n", DescribeEntry(m.Entries[0], f))
	} else if ln > 1 {
		_, _ = fmt.Fprintf(&buf, "Entries:\n%s", DescribeEntries(m.Entries, f))
	}
	if responses := m.Responses; len(responses) != 0 {
		buf.WriteString("OnSync:\n")
		for _, msg := range responses {
			_, _ = fmt.Fprintf(&buf, "%s\n", DescribeMessage(msg, f))
		}
	}
	return buf.String()
}

// StorageAppendAck acknowledges that the corresponding StorageAppend is durable
// on the log storage.
//
// Acknowledgements can be delivered to the RawNode out of order, which allows
// for some concurrency in the way they are processed.
type StorageAppendAck struct {
	// Mark is the durable log mark. By the time this acknowledgement is handled,
	// the log storage can already be at a higher mark.
	Mark LogMark
	// SnapIndex is the index of the snapshot that has been applied. If there was
	// no snapshot, SnapIndex == 0.
	SnapIndex uint64
	// responses contains messages that should be sent now that the StorageAppend
	// is durable. Messages directed to the local RawNode are stepped locally.
	responses []pb.Message
}

// Send iterates through the messages that should be sent to remote peers, i.e.
// peers with ID != self.
//
// TODO(pav-kv): in a typical case, all the Responses are addressed to the
// proposer of the current Term (candidate or leader), and there is no point in
// sending responses to stale proposers. We can double-down on this, and make an
// invariant that all the Responses are addressed to the same proposer. Then it
// is either the local RawNode, or a remote one. So we can avoid scanning the
// Responses twice (in Send and step).
func (m *StorageAppendAck) Send(self pb.PeerID) iter.Seq[pb.Message] {
	return func(yield func(pb.Message) bool) {
		for _, msg := range m.responses {
			if msg.To != self && !yield(msg) {
				return
			}
		}
	}
}

// step iterates through the messages that should be stepped to the local
// RawNode when applying this acknowledgement.
func (m *StorageAppendAck) step(self pb.PeerID) iter.Seq[pb.Message] {
	return func(yield func(pb.Message) bool) {
		for _, msg := range m.responses {
			// TODO(pav-kv): remove msg.From == self after Responses no longer
			// contains the MsgStorageAppendResp.
			if msg.To == self && msg.From == self && !yield(msg) {
				return
			}
		}
	}
}

// Ready encapsulates the entries and messages that are ready to read,
// be saved to stable storage, committed or sent to other peers.
// All fields in Ready are read-only.
type Ready struct {
	// The current volatile state of a Node.
	// SoftState will be nil if there is no update.
	// It is not required to consume or store SoftState.
	*SoftState

	// StorageAppend contains a write request that the application must eventually
	// apply to the storage, and acknowledge to RawNode once it is durable.
	StorageAppend

	// Committed is the log span that has been committed and can be applied to the
	// state machine. Two subsequently accepted committed spans are contiguous,
	// except after a snapshot which can create a "gap" in this sequence.
	//
	// The caller should use RawNode.LogSnapshot() to fetch the committed entries
	// from the log and apply them to the state machine. When a batch of entries
	// has been applied, the caller should call RawNode.AckApplied, to prevent
	// these entries from showing up in subsequent Ready signals.
	//
	// The caller can also only confirm obligation to apply entries, using the
	// RawNode.AckApplying(index) call, which stops committed indices <= index
	// from causing other Ready events, but the entries are still not considered
	// applied by raft.
	//
	// Invariants:
	//	- Committed.After <= Committed.Last
	//	- Committed.After == last index previously accepted for application
	//	- Committed.After == snapshot.Index after a snapshot
	//	- Committed.Last <= committed index known to the RawNode
	//
	// Committed.Last < committed index if the latest committed entries are not
	// yet durable in the log.
	// TODO(pav-kv): reconsider if we can relax this to always == committed index.
	Committed pb.LogSpan

	// Messages specifies outbound messages.
	//
	// If async storage writes are not enabled, these messages must be sent
	// AFTER Entries are appended to stable storage.
	//
	// If async storage writes are enabled, these messages can be sent
	// immediately as the messages that have the completion of the async writes
	// as a precondition are attached to the individual MsgStorage{Append,Apply}
	// messages instead.
	//
	// If it contains a MsgSnap message, the application MUST report back to raft
	// when the snapshot has been received or has failed by calling ReportSnapshot.
	//
	// TODO(pav-kv): remove MsgStorageAppend from this slice, and allow all these
	// messages to be sent immediately.
	Messages []pb.Message
}

func isHardStateEqual(a, b pb.HardState) bool {
	return a == b
}

// IsEmptyHardState returns true if the given HardState is empty.
func IsEmptyHardState(st pb.HardState) bool {
	return isHardStateEqual(st, emptyState)
}

type Peer struct {
	ID      pb.PeerID
	Context []byte
}

func confChangeToMsg(c pb.ConfChangeI) (pb.Message, error) {
	typ, data, err := pb.MarshalConfChange(c)
	if err != nil {
		return pb.Message{}, err
	}
	return pb.Message{Type: pb.MsgProp, Entries: []pb.Entry{{Type: typ, Data: data}}}, nil
}
