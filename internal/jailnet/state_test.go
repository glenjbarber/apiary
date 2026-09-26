package jailnet

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestState_RoundTrip confirms a record written here reads back
// unchanged, including the fields Stage 1's record did not have.
func TestState_RoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "jail-epairs.json")
	state, err := LoadState(path)
	if err != nil {
		t.Fatalf("LoadState() on a missing file error = %v, want nil: a cold node is a definite state, not a failure", err)
	}
	if len(state.Epairs) != 0 {
		t.Errorf("Epairs = %v, want empty", state.Epairs)
	}
	state.Epairs["web"] = EpairRecord{HostSide: "epair0a", JailSide: "epair0b", Bridge: "bridge1", NetworkID: "net1"}
	if err := state.Save(); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	back, err := LoadState(path)
	if err != nil {
		t.Fatalf("LoadState() error = %v", err)
	}
	if got := back.Epairs["web"]; got != state.Epairs["web"] {
		t.Errorf("round trip = %+v, want %+v", got, state.Epairs["web"])
	}
}

// TestState_AdoptsAStage1Record is the upgrade path. A record written by
// internal/cluster's Stage 1 reconciler has only host_side and
// jail_side; it must load cleanly and be usable, with the absent bridge
// field reading as "not recorded" rather than as corrupt.
func TestState_AdoptsAStage1Record(t *testing.T) {
	path := filepath.Join(t.TempDir(), "jail-epairs.json")
	stage1 := `{"epairs":{"web":{"host_side":"epair0a","jail_side":"epair0b"}}}`
	if err := os.WriteFile(path, []byte(stage1), 0o644); err != nil {
		t.Fatalf("seeding: %v", err)
	}
	state, err := LoadState(path)
	if err != nil {
		t.Fatalf("LoadState() error = %v", err)
	}
	rec, present, usable := state.Lookup("web")
	if !present || !usable {
		t.Fatalf("Lookup(web) = %+v present=%v usable=%v, want a usable Stage 1 record", rec, present, usable)
	}
	if rec.Bridge != "" {
		t.Errorf("Bridge = %q, want empty: Stage 1 never recorded one", rec.Bridge)
	}
	if rec.NetworkID != "" {
		t.Errorf("NetworkID = %q, want empty", rec.NetworkID)
	}
}

// TestState_LookupSeparatesAbsentFromUnusable is the honesty
// distinction at the record level: "nothing recorded" is a cold start,
// "something recorded that cannot be trusted" is a damaged file, and a
// caller needs to be able to tell them apart to decide whether to say
// anything at all.
func TestState_LookupSeparatesAbsentFromUnusable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "jail-epairs.json")
	state, err := LoadState(path)
	if err != nil {
		t.Fatalf("LoadState() error = %v", err)
	}
	state.Epairs["web"] = EpairRecord{HostSide: "epair0a", JailSide: "epair0b"}
	state.Epairs["broken"] = EpairRecord{HostSide: "eth0", JailSide: "eth0"}
	state.Epairs["half"] = EpairRecord{HostSide: "epair1a"}
	if err := state.Save(); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	back, err := LoadState(path)
	if err != nil {
		t.Fatalf("LoadState() error = %v", err)
	}

	for _, tt := range []struct {
		id      string
		present bool
		usable  bool
	}{
		{id: "web", present: true, usable: true},
		{id: "broken", present: true, usable: false},
		{id: "half", present: true, usable: false},
		{id: "absent", present: false, usable: true},
	} {
		_, present, usable := back.Lookup(tt.id)
		if present != tt.present || usable != tt.usable {
			t.Errorf("Lookup(%q) present=%v usable=%v, want present=%v usable=%v", tt.id, present, usable, tt.present, tt.usable)
		}
	}
}

// TestState_EpairNameConsistency covers the shapes a real epair(4) pair
// can take, and the ones it cannot. A record that does not describe a
// plausible pair is worse than no record at all - it would be handed
// straight to jail(8) - so it is treated as unusable.
func TestState_EpairNameConsistency(t *testing.T) {
	tests := []struct {
		name string
		rec  EpairRecord
		want bool
	}{
		{name: "a real pair", rec: EpairRecord{HostSide: "epair0a", JailSide: "epair0b"}, want: true},
		{name: "a later pair", rec: EpairRecord{HostSide: "epair42a", JailSide: "epair42b"}, want: true},
		{name: "both ends the same", rec: EpairRecord{HostSide: "eth0", JailSide: "eth0"}, want: false},
		{name: "ends of different pairs", rec: EpairRecord{HostSide: "epair0a", JailSide: "epair1b"}, want: false},
		{name: "swapped ends", rec: EpairRecord{HostSide: "epair0b", JailSide: "epair0a"}, want: false},
		{name: "host side missing", rec: EpairRecord{JailSide: "epair0b"}, want: false},
		{name: "jail side missing", rec: EpairRecord{HostSide: "epair0a"}, want: false},
		{name: "empty", rec: EpairRecord{}, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.rec.consistent(); got != tt.want {
				t.Errorf("consistent(%+v) = %v, want %v", tt.rec, got, tt.want)
			}
		})
	}
}

// TestState_ForgetIsIdempotent confirms teardown can be retried: a jail
// with no record must not fail on the part that already succeeded.
func TestState_ForgetIsIdempotent(t *testing.T) {
	state := State{Epairs: map[string]EpairRecord{"web": {HostSide: "epair0a", JailSide: "epair0b"}}}
	state.Forget("web")
	state.Forget("web")
	state.Forget("never-existed")
	if len(state.Epairs) != 0 {
		t.Errorf("Epairs = %v, want empty", state.Epairs)
	}
}

// TestState_CorruptFileIsALoudError confirms a damaged record file is
// reported rather than silently treated as empty. Treating it as empty
// would provision a second epair pair for every jail on the node, which
// is precisely the interface leak the file exists to prevent.
func TestState_CorruptFileIsALoudError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "jail-epairs.json")
	if err := os.WriteFile(path, []byte(`{"epairs": {"web": `), 0o644); err != nil {
		t.Fatalf("seeding: %v", err)
	}
	if _, err := LoadState(path); err == nil {
		t.Fatal("LoadState() error = nil, want a loud parse failure")
	} else if !strings.Contains(err.Error(), path) {
		t.Errorf("error = %v, want it to name the file so an operator knows which one to look at", err)
	}
}

// TestState_SaveWithoutLoadIsRefused confirms the in-memory path field
// cannot be forgotten: a State built by hand and saved would otherwise
// either fail obscurely or write somewhere unexpected.
func TestState_SaveWithoutLoadIsRefused(t *testing.T) {
	state := State{Epairs: map[string]EpairRecord{}}
	if err := state.Save(); err == nil {
		t.Fatal("Save() error = nil, want a refusal for a State with no path")
	}
}

// TestState_DefaultPathIsStage1sPath is a compatibility assertion, not
// a style preference: the two implementations must agree on the file
// location or every jail on an upgraded node would orphan its pair.
func TestState_DefaultPathIsStage1sPath(t *testing.T) {
	if DefaultStatePath != clusterJailEpairStatePath {
		t.Errorf("DefaultStatePath = %q, want %q (the same file internal/cluster's Stage 1 reconciler writes)", DefaultStatePath, clusterJailEpairStatePath)
	}
}

// clusterJailEpairStatePath mirrors internal/cluster's
// DefaultJailEpairStatePath. It is duplicated as a literal rather than
// imported because importing internal/cluster from this package would
// invert the dependency this package exists to avoid.
const clusterJailEpairStatePath = "/var/db/apiary/jail-epairs.json"
