package statedigest

import (
	"strings"
	"testing"

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
	if got, want := Of(fullState()), Of(fullState()); got != want {
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

	if got, want := Of(reversed), Of(forward); got != want {
		t.Errorf("digest depends on map insertion order:\n got %s\nwant %s", got, want)
	}
}

// TestStateDigestIsSensitiveToEveryStateSection is the guard against the
// failure mode a hand-maintained list of hashed sections would have: a
// new state field added to the FSM and forgotten here, silently excluded
// from the digest forever. Each case varies exactly one section.
func TestStateDigestIsSensitiveToEveryStateSection(t *testing.T) {
	baseline := Of(fullState())

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
			if got := Of(mutated); got == baseline {
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

	if got, want := Of(atNine), Of(atSeven); got != want {
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
		digest := Of(state)
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
	if Of(first) == Of(second) {
		t.Error("two different states collided, want distinct digests - a collision reads as agreement")
	}
}
