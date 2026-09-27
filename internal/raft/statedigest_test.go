package raft

import (
	"bytes"
	"io"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/hashicorp/raft"
	"google.golang.org/protobuf/proto"

	internalpb "github.com/glenjbarber/apiary/api/internalpb"
)

// These cover ADR-0143's canonical state digest. The properties that
// matter are not "the hash is stable" - any hash is - but the four the
// cross-voter comparison is only sound if it has: identical state must
// digest identically no matter what order it was built in, every piece of
// state must move the digest, nothing but state may move it, and the
// digest must never be absent for a live node.

// fullState returns an FSMSnapshotState with every map the FSM holds
// populated, so a digest sensitivity test can vary one section at a time
// and know the rest is identical.
func fullState() *internalpb.FSMSnapshotState {
	return &internalpb.FSMSnapshotState{
		LastIndex:           7,
		Vms:                 map[string]*internalpb.VMDefinition{"vm-1": {Id: "vm-1", Name: "web-1"}},
		Networks:            map[string]*internalpb.NetworkDefinition{"net-1": {Id: "net-1", Name: "lan"}},
		ApiKeys:             map[string]*internalpb.ApiKey{"key-1": {Id: "key-1", Name: "ci"}},
		Jails:               map[string]*internalpb.JailDefinition{"jail-1": {Id: "jail-1", Name: "dns"}},
		PendingJoinRequests: map[string]*internalpb.PendingJoinRequest{"req-1": {RequestId: "req-1", NodeId: "new-comb"}},
		RestartLeases:       map[string]*internalpb.RestartLease{"apiary_raftd": {LeaseId: 4, HolderNodeId: "brood"}},
		RestartRecords:      map[string]*internalpb.RestartRecord{"apiary_managerd": {Service: "apiary_managerd", NodeId: "brood"}},
		AuthEnabled:         true,
	}
}

func TestStateDigestIsStableAcrossIdenticalState(t *testing.T) {
	if got, want := stateDigestOf(fullState()), stateDigestOf(fullState()); got != want {
		t.Errorf("digest of two independently built identical states differ:\n got %s\nwant %s", got, want)
	}
}

// TestStateDigestIgnoresMapInsertionOrder is the property a
// proto.Marshal-based digest would fail. Go randomizes map iteration per
// process and per range, so building the same state in a different
// insertion order is the cheapest available proof that the encoding
// sorts rather than trusting iteration order.
func TestStateDigestIgnoresMapInsertionOrder(t *testing.T) {
	forward := &internalpb.FSMSnapshotState{
		Vms:                 map[string]*internalpb.VMDefinition{},
		Jails:               map[string]*internalpb.JailDefinition{},
		PendingJoinRequests: map[string]*internalpb.PendingJoinRequest{},
	}
	for i := 0; i < 12; i++ {
		id := string(rune('a' + i))
		forward.Vms[id] = &internalpb.VMDefinition{Id: id, Name: "n" + id}
		forward.Jails[id] = &internalpb.JailDefinition{Id: id, Name: "j" + id}
		forward.PendingJoinRequests[id] = &internalpb.PendingJoinRequest{RequestId: id, NodeId: id}
	}
	reversed := &internalpb.FSMSnapshotState{
		Vms:                 map[string]*internalpb.VMDefinition{},
		Jails:               map[string]*internalpb.JailDefinition{},
		PendingJoinRequests: map[string]*internalpb.PendingJoinRequest{},
	}
	for i := 11; i >= 0; i-- {
		id := string(rune('a' + i))
		reversed.Vms[id] = &internalpb.VMDefinition{Id: id, Name: "n" + id}
		reversed.Jails[id] = &internalpb.JailDefinition{Id: id, Name: "j" + id}
		reversed.PendingJoinRequests[id] = &internalpb.PendingJoinRequest{RequestId: id, NodeId: id}
	}

	if got, want := stateDigestOf(reversed), stateDigestOf(forward); got != want {
		t.Errorf("digest depends on map insertion order:\n got %s\nwant %s", got, want)
	}
}

// TestStateDigestIsSensitiveToEveryStateSection is the guard against the
// failure mode a hand-maintained list of hashed sections would have: a
// new state field added to the FSM and forgotten here, silently excluded
// from the digest forever. Each case varies exactly one section.
func TestStateDigestIsSensitiveToEveryStateSection(t *testing.T) {
	baseline := stateDigestOf(fullState())

	cases := []struct {
		name   string
		mutate func(*internalpb.FSMSnapshotState)
	}{
		{"vms value changed", func(s *internalpb.FSMSnapshotState) { s.Vms["vm-1"].Name = "renamed" }},
		{"vm added", func(s *internalpb.FSMSnapshotState) { s.Vms["vm-2"] = &internalpb.VMDefinition{Id: "vm-2"} }},
		{"vm removed", func(s *internalpb.FSMSnapshotState) { delete(s.Vms, "vm-1") }},
		{"networks value changed", func(s *internalpb.FSMSnapshotState) { s.Networks["net-1"].Name = "wan" }},
		{"network added", func(s *internalpb.FSMSnapshotState) { s.Networks["net-2"] = &internalpb.NetworkDefinition{Id: "net-2"} }},
		{"api keys value changed", func(s *internalpb.FSMSnapshotState) { s.ApiKeys["key-1"].Name = "other" }},
		{"api key added", func(s *internalpb.FSMSnapshotState) { s.ApiKeys["key-2"] = &internalpb.ApiKey{Id: "key-2"} }},
		{"jails value changed", func(s *internalpb.FSMSnapshotState) { s.Jails["jail-1"].Name = "other" }},
		{"jail added", func(s *internalpb.FSMSnapshotState) { s.Jails["jail-2"] = &internalpb.JailDefinition{Id: "jail-2"} }},
		{"pending join request changed", func(s *internalpb.FSMSnapshotState) { s.PendingJoinRequests["req-1"].NodeId = "other-comb" }},
		{"pending join request added", func(s *internalpb.FSMSnapshotState) {
			s.PendingJoinRequests["req-2"] = &internalpb.PendingJoinRequest{RequestId: "req-2"}
		}},
		{"restart lease changed", func(s *internalpb.FSMSnapshotState) { s.RestartLeases["apiary_raftd"].HolderNodeId = "drone" }},
		{"restart lease added", func(s *internalpb.FSMSnapshotState) {
			s.RestartLeases["apiary_managerd"] = &internalpb.RestartLease{LeaseId: 9}
		}},
		{"restart record changed", func(s *internalpb.FSMSnapshotState) { s.RestartRecords["apiary_managerd"].NodeId = "sting" }},
		{"restart record added", func(s *internalpb.FSMSnapshotState) {
			s.RestartRecords["apiary_raftd"] = &internalpb.RestartRecord{Service: "apiary_raftd"}
		}},
		{"auth enabled flipped", func(s *internalpb.FSMSnapshotState) { s.AuthEnabled = false }},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mutated := fullState()
			tc.mutate(mutated)
			if got := stateDigestOf(mutated); got == baseline {
				t.Errorf("digest = %s, unchanged after %s - that state difference is invisible to the comparison", got, tc.name)
			}
		})
	}
}

// TestStateDigestExcludesLastIndex pins the one deliberate omission. If
// last_index ever leaked into the encoding, every voter would disagree
// with every other voter the instant the colony applied anything, and the
// badge would be permanently amber for a perfectly healthy cluster.
func TestStateDigestExcludesLastIndex(t *testing.T) {
	atSeven := fullState()
	atNine := fullState()
	atNine.LastIndex = 9

	if got, want := stateDigestOf(atNine), stateDigestOf(atSeven); got != want {
		t.Errorf("digest changed with last_index alone (%s vs %s), want it excluded", got, want)
	}
}

// TestStateDigestIsNeverEmpty is what lets raftd's StatusResponse treat
// an empty state_digest as "not observed" with no extra presence flag. If
// the empty state ever digested to "", that inference would invert: every
// unreadable node would become a node in agreement with every other.
func TestStateDigestIsNeverEmpty(t *testing.T) {
	for name, state := range map[string]*internalpb.FSMSnapshotState{
		"nil state":                nil,
		"empty state":              {},
		"empty maps":               {Vms: map[string]*internalpb.VMDefinition{}, Jails: map[string]*internalpb.JailDefinition{}},
		"state with only an index": {LastIndex: 42},
	} {
		digest := stateDigestOf(state)
		if digest == "" {
			t.Errorf("%s: digest = empty, want a real digest so an empty value can mean only 'not observed'", name)
		}
		if len(digest) != 64 {
			t.Errorf("%s: len(digest) = %d, want 64 hex characters of SHA-256", name, len(digest))
		}
		if strings.ToLower(digest) != digest {
			t.Errorf("%s: digest = %q, want lowercase hex", name, digest)
		}
	}
}

// TestStateDigestLengthPrefixPreventsCollisions is a sanity check on the
// encoding's injectivity: two states proto considers different must not
// collide here, because a collision is a false "match" - the one error
// this whole mechanism cannot afford.
func TestStateDigestLengthPrefixPreventsCollisions(t *testing.T) {
	// Two different key/value splits that a naive concatenation would
	// render identically: keys "ab"+"c" and "a"+"bc".
	first := &internalpb.FSMSnapshotState{Vms: map[string]*internalpb.VMDefinition{
		"ab": {Id: "ab", Name: "c"},
		"a":  {Id: "a", Name: "bc"},
	}}
	second := &internalpb.FSMSnapshotState{Vms: map[string]*internalpb.VMDefinition{
		"ab": {Id: "ab", Name: "cc"},
		"a":  {Id: "a", Name: "bc"},
	}}
	if stateDigestOf(first) == stateDigestOf(second) {
		t.Error("two different states collided, want distinct digests - a collision reads as agreement")
	}
}

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

	if got, want := fsm.StateDigest(), stateDigestOf(fsm.SnapshotState()); got != want {
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

	if digest := fsm.StateDigest(); digest != stateDigestOf(fsm.SnapshotState()) {
		t.Errorf("final StateDigest = %s, want it to match SnapshotState after the applies settled", digest)
	}
}
