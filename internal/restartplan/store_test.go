package restartplan

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/glenjbarber/apiary/internal/manager"
)

// TestPendingStoreMatchesManagerRestartConfirmStore is the check that
// makes having two writers to the pending-restart file safe rather than
// merely convenient.
//
// internal/manager.RestartConfirmStore is the writer in production today;
// PendingStore is the reader cmd/raftd uses, and the writer Engine uses.
// If those two ever disagree about the file name, the JSON field names or
// the encoding, the restarted raftd silently finds nothing to confirm and
// a raft-replicated lease is stranded forever with no TTL to release it -
// a failure that no unit test inside either package could catch, because
// each would be testing only its own half. So this test writes through one
// type and reads through the other, both ways, and compares the bytes on
// disk as well as the decoded values.
func TestPendingStoreMatchesManagerRestartConfirmStore(t *testing.T) {
	dir := t.TempDir()
	mine := NewPendingStore(dir)
	theirs := manager.NewRestartConfirmStore(dir)
	want := PendingRestart{Service: DefaultService, NodeID: "comb-a", LeaseID: 4242}

	t.Run("what I write, managerd reads", func(t *testing.T) {
		if err := mine.Save(want); err != nil {
			t.Fatalf("PendingStore.Save: %v", err)
		}
		got, found, err := theirs.Load(DefaultService)
		if err != nil {
			t.Fatalf("manager.RestartConfirmStore.Load: %v", err)
		}
		if !found {
			t.Fatalf("manager.RestartConfirmStore.Load found nothing; the two stores disagree about the path")
		}
		if got.Service != want.Service || got.NodeID != want.NodeID || got.LeaseID != want.LeaseID {
			t.Errorf("managerd read %+v, want %+v", got, want)
		}
	})

	t.Run("what managerd writes, I read", func(t *testing.T) {
		if err := os.RemoveAll(dir); err != nil {
			t.Fatal(err)
		}
		theirRec := manager.PendingRestart{Service: DefaultService, NodeID: "comb-b", LeaseID: 99}
		if err := theirs.Save(theirRec); err != nil {
			t.Fatalf("manager.RestartConfirmStore.Save: %v", err)
		}
		got, found, err := mine.Load(DefaultService)
		if err != nil {
			t.Fatalf("PendingStore.Load: %v", err)
		}
		if !found {
			t.Fatalf("PendingStore.Load found nothing; the two stores disagree about the path")
		}
		if got.Service != theirRec.Service || got.NodeID != theirRec.NodeID || got.LeaseID != theirRec.LeaseID {
			t.Errorf("I read %+v, want %+v", got, theirRec)
		}
	})

	t.Run("the on-disk encoding is byte-for-byte identical", func(t *testing.T) {
		if err := os.RemoveAll(dir); err != nil {
			t.Fatal(err)
		}
		if err := mine.Save(want); err != nil {
			t.Fatal(err)
		}
		mineBytes, err := os.ReadFile(filepath.Join(dir, "pending-restart-"+DefaultService+".json"))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.RemoveAll(dir); err != nil {
			t.Fatal(err)
		}
		if err := theirs.Save(manager.PendingRestart(want)); err != nil {
			t.Fatal(err)
		}
		theirBytes, err := os.ReadFile(filepath.Join(dir, "pending-restart-"+DefaultService+".json"))
		if err != nil {
			t.Fatal(err)
		}
		if string(mineBytes) != string(theirBytes) {
			t.Errorf("encodings differ:\n  restartplan: %s\n  manager:    %s", mineBytes, theirBytes)
		}
		// A var (not :=) so this stays true even if the encoding above
		// somehow became identical for a different reason.
		var decodedA, decodedB map[string]any
		if err := json.Unmarshal(mineBytes, &decodedA); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(theirBytes, &decodedB); err != nil {
			t.Fatal(err)
		}
		for _, key := range []string{"service", "node_id", "lease_id"} {
			if _, ok := decodedA[key]; !ok {
				t.Errorf("key %q missing; the on-disk contract cmd/raftd depends on has drifted", key)
			}
		}
	})

	t.Run("clear is symmetric too", func(t *testing.T) {
		if err := os.RemoveAll(dir); err != nil {
			t.Fatal(err)
		}
		if err := mine.Save(want); err != nil {
			t.Fatal(err)
		}
		if err := theirs.Clear(DefaultService); err != nil {
			t.Fatalf("manager RestartConfirmStore.Clear: %v", err)
		}
		if _, found, err := mine.Load(DefaultService); err != nil || found {
			t.Errorf("after managerd cleared it, Load = (found=%v, err=%v), want found=false", found, err)
		}
	})
}

// TestPendingStoreLifecycle covers the read/clear contract cmd/raftd's
// startup hook depends on.
func TestPendingStoreLifecycle(t *testing.T) {
	dir := t.TempDir()
	s := NewPendingStore(dir)

	t.Run("a missing record is not an error", func(t *testing.T) {
		got, found, err := s.Load(DefaultService)
		if err != nil {
			t.Errorf("Load on a fresh directory: %v, want no error", err)
		}
		if found {
			t.Errorf("Load found %+v on a fresh directory", got)
		}
	})

	t.Run("save then load round-trips", func(t *testing.T) {
		want := PendingRestart{Service: DefaultService, NodeID: "comb-a", LeaseID: 7}
		if err := s.Save(want); err != nil {
			t.Fatal(err)
		}
		got, found, err := s.Load(DefaultService)
		if err != nil || !found {
			t.Fatalf("Load = (found=%v, err=%v)", found, err)
		}
		if got != want {
			t.Errorf("Load = %+v, want %+v", got, want)
		}
	})

	t.Run("the file is not world-readable", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("POSIX permissions are not meaningful here")
		}
		info, err := os.Stat(filepath.Join(dir, "pending-restart-"+DefaultService+".json"))
		if err != nil {
			t.Fatal(err)
		}
		if perm := info.Mode().Perm(); perm != 0o600 {
			t.Errorf("pending record mode = %o, want 600", perm)
		}
	})

	t.Run("records are per service", func(t *testing.T) {
		if err := s.Save(PendingRestart{Service: ManagerService, NodeID: "comb-a", LeaseID: 8}); err != nil {
			t.Fatal(err)
		}
		if _, found, _ := s.Load(DefaultService); !found {
			t.Errorf("the apiary_raftd record disappeared when the apiary_managerd one was written")
		}
		if _, found, _ := s.Load(ManagerService); !found {
			t.Errorf("the apiary_managerd record was not written")
		}
	})

	t.Run("clear removes it, and clearing twice is not an error", func(t *testing.T) {
		if err := s.Clear(DefaultService); err != nil {
			t.Errorf("first Clear: %v", err)
		}
		if err := s.Clear(DefaultService); err != nil {
			t.Errorf("second Clear: %v, want no error - Clear is called on paths where the file may already be gone", err)
		}
		if _, found, _ := s.Load(DefaultService); found {
			t.Errorf("Load found a record after Clear")
		}
		if _, found, _ := s.Load(ManagerService); !found {
			t.Errorf("Clear on apiary_raftd removed the apiary_managerd record")
		}
	})

	t.Run("a corrupt record is an error, not a silent miss", func(t *testing.T) {
		if err := s.Save(PendingRestart{Service: DefaultService, NodeID: "comb-a", LeaseID: 1}); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "pending-restart-"+DefaultService+".json"), []byte("{not json"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, found, err := s.Load(DefaultService); err == nil || found {
			t.Errorf("Load on a corrupt record = (found=%v, err=%v), want (false, error) - a corrupt record may be a held lease", found, err)
		}
	})
}

func TestNewPendingStoreDefaultsToTheAgreedDirectory(t *testing.T) {
	if got := NewPendingStore("").Dir; got != DefaultStateDir {
		t.Errorf("NewPendingStore(\"\").Dir = %q, want %q", got, DefaultStateDir)
	}
	if got := NewResultStore("").Dir; got != DefaultResultDir {
		t.Errorf("NewResultStore(\"\").Dir = %q, want %q", got, DefaultResultDir)
	}
}

// TestResultStoreRoundTrip covers the durable outcome record, including
// the validation that makes an unbacked verdict unwritable.
func TestResultStoreRoundTrip(t *testing.T) {
	dir := t.TempDir()
	s := NewResultStore(dir)

	t.Run("an unknown outcome round-trips as unknown", func(t *testing.T) {
		want := Result{
			Service: DefaultService, NodeID: "comb-a", LeaseID: 11, Attempt: 2,
			Outcome:        OutcomeUnobserved,
			Detail:         "the restart command succeeded but nothing confirmed it",
			Evidence:       []string{"attempt 1/5: connection refused", "attempt 2/5: connection refused"},
			StartedAtUnix:  1758800000,
			FinishedAtUnix: 1758800015,
		}
		if err := s.Save(want); err != nil {
			t.Fatalf("Save: %v", err)
		}
		got, found, err := s.Load("comb-a", DefaultService)
		if err != nil || !found {
			t.Fatalf("Load = (found=%v, err=%v)", found, err)
		}
		if got.Outcome != OutcomeUnobserved {
			t.Errorf("Outcome = %q, want %q - a round trip must not turn unknown into anything else", got.Outcome, OutcomeUnobserved)
		}
		if len(got.Evidence) != 2 || got.Evidence[0] != want.Evidence[0] {
			t.Errorf("Evidence = %v, want %v", got.Evidence, want.Evidence)
		}
		if got.Attempt != 2 || got.LeaseID != 11 {
			t.Errorf("Load = %+v, want lease 11 attempt 2", got)
		}
	})

	t.Run("records are per node and per service", func(t *testing.T) {
		base := Result{Service: DefaultService, NodeID: "comb-a", Outcome: OutcomeConfirmed,
			Detail: "confirmed", Evidence: []string{"lease released"}, LeaseID: 1}
		if err := s.Save(base); err != nil {
			t.Fatal(err)
		}
		other := base
		other.NodeID = "comb-b"
		other.LeaseID = 2
		if err := s.Save(other); err != nil {
			t.Fatal(err)
		}
		gotA, _, _ := s.Load("comb-a", DefaultService)
		gotB, _, _ := s.Load("comb-b", DefaultService)
		if gotA.LeaseID != 1 || gotB.LeaseID != 2 {
			t.Errorf("per-node records overwrote each other: comb-a=%d comb-b=%d", gotA.LeaseID, gotB.LeaseID)
		}
		if _, found, _ := s.Load("comb-c", DefaultService); found {
			t.Errorf("Load for a node that never attempted anything reported found=true")
		}
	})

	t.Run("a node or service name cannot escape the store directory", func(t *testing.T) {
		hostile := Result{Service: "../../etc", NodeID: "../../../tmp/pwned", Outcome: OutcomeBlocked,
			Detail: "blocked by guardrail", LeaseID: 0}
		if err := s.Save(hostile); err != nil {
			t.Fatal(err)
		}
		// The file must have landed inside dir, sanitized, not up in
		// /tmp. Only the separators matter here: ".." survives as an
		// inert substring of a filename, which is harmless; a "/" would
		// not be.
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		sawHostile := false
		for _, e := range entries {
			if strings.ContainsAny(e.Name(), `/\`) {
				t.Errorf("store produced an entry with a path separator: %q", e.Name())
			}
			if strings.HasPrefix(e.Name(), "restart-result-") {
				sawHostile = true
			}
		}
		if !sawHostile {
			t.Errorf("the hostile record was not written into %s at all", dir)
		}
		if _, err := os.Stat("/tmp/pwned.json"); err == nil {
			t.Errorf("a hostile node id escaped the store directory")
		}
		got, found, err := s.Load(hostile.NodeID, hostile.Service)
		if err != nil {
			t.Errorf("Load of the hostile key: %v", err)
		}
		if !found || got.Outcome != OutcomeBlocked {
			t.Errorf("hostile key did not round-trip: found=%v got=%+v", found, got)
		}
	})

	t.Run("a leading dot cannot produce a hidden file", func(t *testing.T) {
		hidden := Result{Service: DefaultService, NodeID: ".ssh", Outcome: OutcomeBlocked, Detail: "blocked"}
		if err := s.Save(hidden); err != nil {
			t.Fatal(err)
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range entries {
			if strings.HasPrefix(e.Name(), ".") {
				t.Errorf("store produced a hidden entry %q", e.Name())
			}
		}
	})
}

func TestSafeFileComponent(t *testing.T) {
	// Expectations are the literal sanitized filenames, chosen so a
	// reader can see that no separator, no leading dot and no empty
	// component survives.
	cases := map[string]string{
		"comb-a":            "comb-a",
		"apiary_raftd":      "apiary_raftd",
		"":                  "_",
		".":                 "_",
		"..":                "_.",
		"a/b":               "a_b",
		"../../etc/passwd":  "_._.._etc_passwd",
		"node with spaces":  "node_with_spaces",
		"apiary_raftd.json": "apiary_raftd.json",
		"\u00fcn\u00efcode": "_n_code",
	}
	for in, want := range cases {
		if got := safeFileComponent(in); got != want {
			t.Errorf("safeFileComponent(%q) = %q, want %q", in, got, want)
		}
	}
}
