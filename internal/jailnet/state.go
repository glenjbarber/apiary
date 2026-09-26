package jailnet

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// DefaultStatePath is where the epair bookkeeping lives by default. It
// is deliberately the same path internal/cluster's Stage 1 reconciler
// wrote (see cluster.DefaultJailEpairStatePath) and uses the same
// on-disk shape, so a node upgraded from Stage 1 adopts its existing
// records instead of orphaning every pair it had already created - and
// orphaning them would mean a fresh pair per jail per tick, with the old
// ones left attached to bridges until someone noticed.
//
// The file is node-local and unreplicated, which is correct for the
// same reason a VM's tap is: an epair is a fact about one host's
// kernel, not about the Cell. A jail that genuinely moved to another
// node needs a pair provisioned there, exactly as a VM does.
const DefaultStatePath = "/var/db/apiary/jail-epairs.json"

// EpairRecord is one jail's recorded epair(4) pair.
//
// Both ends are kept, not just the host side, for the reason
// internal/cluster's Stage 1 record already gives: a tick that creates
// the pair but fails before jail(8) starts must be able to hand
// jail(8) the identical jail-side name next tick rather than leaking a
// fresh pair on every retry.
type EpairRecord struct {
	HostSide string `json:"host_side"`
	JailSide string `json:"jail_side"`

	// Bridge records which bridge the host side was last joined to.
	// Stage 1's record had no such field, so it reads back as "" for a
	// node upgrading from it, which this package treats as "unknown,
	// go and look" rather than "wrong" - see reconcileHost.
	Bridge string `json:"bridge,omitempty"`

	// NetworkID is which NetworkDefinition this pair was provisioned
	// for. Recorded so that a jail moved to a different network is
	// recognized as a move rather than silently kept on the old
	// bridge. Empty for Stage 1 records, and treated the same way as an
	// empty Bridge.
	NetworkID string `json:"network_id,omitempty"`
}

// consistent reports whether the record names a plausible epair(4)
// pair. FreeBSD's epair names are always "<base>a" and "<base>b" from
// one `ifconfig epair create`, so a record whose two ends do not share
// a base and differ only in that final letter is not something this
// package wrote, or was truncated into, and trusting it would hand
// jail(8) an interface name that can never exist. A corrupt record is
// treated as no record at all: the pair is re-provisioned and the
// broken entry replaced.
func (r EpairRecord) consistent() bool {
	if r.HostSide == "" || r.JailSide == "" {
		return false
	}
	if !strings.HasSuffix(r.HostSide, "a") || !strings.HasSuffix(r.JailSide, "b") {
		return false
	}
	return strings.TrimSuffix(r.HostSide, "a") == strings.TrimSuffix(r.JailSide, "b")
}

// stateFile is the on-disk shape. It is a named wrapper rather than
// State itself so that the path this package carries around in memory
// is never something JSON can see, and so the envelope is stated once,
// here, instead of being assembled by hand at the write site.
type stateFile struct {
	Epairs map[string]EpairRecord `json:"epairs"`
}

// State is the whole node-local file: jail id to epair record.
type State struct {
	Epairs map[string]EpairRecord

	// path is the file this State was loaded from, carried so callers
	// never have to thread it back through Save.
	path string
}

// LoadState reads the epair record file at path. A file that does not
// exist is not an error and not a failure to observe: it is the
// correct, definite state of a node that has never provisioned a VNET
// jail, and it must come back as an empty, writable state rather than
// an error, or the very first reconcile of the very first VNET jail
// could never bootstrap itself.
func LoadState(path string) (State, error) {
	if path == "" {
		path = DefaultStatePath
	}
	body, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return State{Epairs: make(map[string]EpairRecord), path: path}, nil
	}
	if err != nil {
		return State{}, fmt.Errorf("reading jail epair state from %s: %w", path, err)
	}
	var file stateFile
	if err := json.Unmarshal(body, &file); err != nil {
		return State{}, fmt.Errorf("parsing jail epair state from %s: %w", path, err)
	}
	if file.Epairs == nil {
		file.Epairs = make(map[string]EpairRecord)
	}
	return State{Epairs: file.Epairs, path: path}, nil
}

// Save writes the state back, creating the containing directory if it
// does not exist. It is deliberately whole-file and not atomic via
// rename: the only writer is this reconciler, single-threaded per node,
// and a torn write here is recoverable (the next load reports a parse
// error, which is a loud failure, not a silently wrong state) - the
// same trade internal/cluster's own loadJailEpairState/saveJailEpairState
// already makes. The comment is here because "add a rename" is the
// obvious next thing someone will suggest.
func (s State) Save() error {
	if s.path == "" {
		return fmt.Errorf("saving jail epair state: no path was set (State was not loaded with LoadState)")
	}
	if s.Epairs == nil {
		s.Epairs = make(map[string]EpairRecord)
	}
	if dir := filepath.Dir(s.path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("creating %s for jail epair state: %w", dir, err)
		}
	}
	body, err := json.MarshalIndent(stateFile{Epairs: s.Epairs}, "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling jail epair state: %w", err)
	}
	if err := os.WriteFile(s.path, body, 0o644); err != nil {
		return fmt.Errorf("writing jail epair state to %s: %w", s.path, err)
	}
	return nil
}

// Forget removes jailID's record. It is what teardown calls once the
// pair is confirmed destroyed, and what a drift repair calls when it
// decides the recorded pair is gone and a new one is on its way.
//
// Forgetting a jail that has no record is a no-op, not an error, so
// teardown stays idempotent: a retry after a partial teardown must not
// fail on the part that already succeeded.
func (s State) Forget(jailID string) {
	delete(s.Epairs, jailID)
}

// Lookup returns jailID's record and whether a usable one was found.
// A record present but structurally inconsistent is reported the same
// way as absent (ok=false), with its existence reported separately, so
// a caller can tell "nothing recorded" from "something recorded that
// cannot be trusted" - the latter means a corrupt file worth telling an
// operator about, not just a cold start.
func (s State) Lookup(jailID string) (EpairRecord, bool, bool) {
	rec, ok := s.Epairs[jailID]
	if !ok {
		return EpairRecord{}, false, true
	}
	if !rec.consistent() {
		return EpairRecord{}, true, false
	}
	return rec, true, true
}
