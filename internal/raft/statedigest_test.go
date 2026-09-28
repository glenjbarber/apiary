package raft

// These are the FSM-side half of ADR-0143's digest, kept here rather than
// moved with the canonical encoding: what they pin is that the raftd FSM
// reports the digest Of computes over the state raftd itself holds. The
// encoding's own properties - determinism, sensitivity, the last_index
// exclusion - are tested in internal/statedigest, next to the code that
// implements them.

import (
	"bytes"
	"io"
	"strconv"
	"sync"
	"testing"

	"github.com/hashicorp/raft"
	"google.golang.org/protobuf/proto"

	"github.com/glenjbarber/apiary/internal/statedigest"
)

func TestFSMStateDigestTracksApply(t *testing.T) {
	fsm := NewFSM()
	empty := fsm.StateDigest()
	if empty == "" {
		t.Fatal("StateDigest on a fresh FSM = empty, want the digest of the empty state")
	}

	fsm.Apply(&raft.Log{Index: 1, Data: mustMarshalCommand(t, createVMCmd("vm-1", "web-1"))})
	afterCreate := fsm.StateDigest()
	if afterCreate == empty {
		t.Error("StateDigest unchanged after a VM was created, want it to move with state")
	}

	// A rejected command changes no state, so it must change no digest -
	// even though the applied index does advance. This is the property
	// that makes a digest comparable across voters at all.
	fsm.Apply(&raft.Log{Index: 2, Data: mustMarshalCommand(t, createVMCmd("vm-1", "duplicate"))})
	if got := fsm.StateDigest(); got != afterCreate {
		t.Errorf("StateDigest = %s after a rejected command, want it unchanged at %s", got, afterCreate)
	}

	fsm.Apply(&raft.Log{Index: 3, Data: mustMarshalCommand(t, createJailCmd("jail-1", "dns"))})
	if got := fsm.StateDigest(); got == afterCreate {
		t.Error("StateDigest unchanged after a jail was created, want it to move with state")
	}
}

// TestFSMStateDigestEqualsSnapshotStateDigest ties the cached value to the
// message it claims to describe, so the two cannot drift apart.
func TestFSMStateDigestEqualsSnapshotStateDigest(t *testing.T) {
	fsm := NewFSM()
	fsm.Apply(&raft.Log{Index: 1, Data: mustMarshalCommand(t, createVMCmd("vm-1", "web-1"))})
	fsm.Apply(&raft.Log{Index: 2, Data: mustMarshalCommand(t, createNetworkCmd("net-1", "lan", "10.0.0.0/24"))})
	fsm.Apply(&raft.Log{Index: 3, Data: mustMarshalCommand(t, createAPIKeyCmd("key-1", "ci", "hash"))})
	fsm.Apply(&raft.Log{Index: 4, Data: mustMarshalCommand(t, createPendingJoinRequestCmd("req-1", "new-comb", "10.0.0.9:17701", "code", 1<<40))})
	fsm.Apply(&raft.Log{Index: 5, Data: mustMarshalCommand(t, acquireRestartLeaseCmd("apiary_raftd", "brood", 1000, 600, []string{"brood"}, true))})
	fsm.Apply(&raft.Log{Index: 6, Data: mustMarshalCommand(t, recordRestartCompletedCmd("apiary_raftd", "brood", 1010, 7))})

	if got, want := fsm.StateDigest(), statedigest.Of(fsm.SnapshotState()); got != want {
		t.Errorf("StateDigest = %s, want the digest of SnapshotState %s", got, want)
	}
}

// TestFSMStateDigestSurvivesRestore covers the path that matters most in
// practice: a voter restarted from a raft snapshot. Its digest must
// describe the restored state, not the empty state it started from.
func TestFSMStateDigestSurvivesRestore(t *testing.T) {
	source := NewFSM()
	source.Apply(&raft.Log{Index: 1, Data: mustMarshalCommand(t, createVMCmd("vm-1", "web-1"))})
	source.Apply(&raft.Log{Index: 2, Data: mustMarshalCommand(t, createJailCmd("jail-1", "dns"))})
	want := source.StateDigest()

	encoded, err := proto.Marshal(source.SnapshotState())
	if err != nil {
		t.Fatalf("marshalling snapshot state: %v", err)
	}

	restored := NewFSM()
	if err := restored.Restore(io.NopCloser(bytes.NewReader(encoded))); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if got := restored.StateDigest(); got != want {
		t.Errorf("restored StateDigest = %s, want %s - a digest left over from the pre-restore state would report divergence that is not there", got, want)
	}
}

// TestFSMStateDigestCoversExpiredPendingJoinRequests is ADR-0143's own
// recorded hazard, pinned as a test. ListPendingJoinRequests filters
// expired records at read time, so two voters with byte-identical state
// legitimately return different lists. A digest computed over that
// filtered view would report divergence on a healthy colony.
func TestFSMStateDigestCoversExpiredPendingJoinRequests(t *testing.T) {
	// expires_at_unix 1 is in the distant past for any wall clock.
	expiring := NewFSM()
	expiring.Apply(&raft.Log{Index: 1, Data: mustMarshalCommand(t,
		createPendingJoinRequestCmd("req-1", "new-comb", "10.0.0.9:17701", "code", 1))})
	if got := len(expiring.ListPendingJoinRequests()); got != 0 {
		t.Fatalf("ListPendingJoinRequests = %d, want 0 - the record is expired at read time", got)
	}
	withExpired := expiring.StateDigest()

	fresh := NewFSM()
	fresh.Apply(&raft.Log{Index: 1, Data: mustMarshalCommand(t,
		createPendingJoinRequestCmd("req-1", "new-comb", "10.0.0.9:17701", "code", 1<<40))})

	if withExpired == fresh.StateDigest() {
		t.Error("an expired pending join request digests the same as a live one, want the stored record covered " +
			"regardless of read-time expiry filtering")
	}
}

// TestFSMStateDigestIsNeverEmptyUnderConcurrentApply runs the digest read
// against concurrent applies. It is a race test in substance even without
// -race: an unsynchronised cache would show up here as an empty or
// torn value, which is what a consumer would then report as a node in
// agreement with nothing.
func TestFSMStateDigestIsNeverEmptyUnderConcurrentApply(t *testing.T) {
	fsm := NewFSM()
	var wg sync.WaitGroup
	stop := make(chan struct{})

	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			fsm.Apply(&raft.Log{Index: uint64(i + 1), Data: mustMarshalCommand(t, createVMCmd("vm-"+strconv.Itoa(i), "n"))})
		}
	}()

	for i := 0; i < 200; i++ {
		if digest := fsm.StateDigest(); digest == "" {
			t.Fatal("StateDigest = empty while applies were running, want a digest on every read")
		}
	}
	close(stop)
	wg.Wait()

	if digest := fsm.StateDigest(); digest != statedigest.Of(fsm.SnapshotState()) {
		t.Errorf("final StateDigest = %s, want it to match SnapshotState after the applies settled", digest)
	}
}
