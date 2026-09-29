package joinauth

// ADR-0147 Part 3's store, tested on its own. Every case here is one
// an operator can actually hit, and the shape of each is the same: put
// the bytes on disk, ask for the answer, and assert the answer is
// refused in a way that names what to do.

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const testFingerprint = "SHA256:AB:CD:EF:01:23:45:67:89"

// writeStore writes raw bytes as the store file, bypassing Save, so a
// test can put on disk exactly what a hand edit or a truncated write
// would leave.
func writeStore(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}
}

func TestSaveWrites0600AndReloadable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "join-authorizations.json")
	store := Store{Authorizations: []Entry{{
		ID:            "auth-1",
		NodeID:        "comb-3",
		Fingerprint:   testFingerprint,
		ExpiresAtUnix: 1800000000,
	}}}
	if err := store.Save(path); err != nil {
		t.Fatalf("Save() error: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat() error: %v", err)
	}
	// The mode is the security property, not a style choice: the whole
	// reason an entry is worth anything is that an Admin of this Colony
	// cannot write the file. A 0644 store would be a store any local
	// user could add themselves to.
	if got := info.Mode().Perm(); got != 0o600 {
		t.Errorf("Save() wrote mode %04o, want 0600", got)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatalf("Load() after Save() error: %v", err)
	}
	if len(got.Authorizations) != 1 || got.Authorizations[0] != store.Authorizations[0] {
		t.Errorf("Load() = %+v, want %+v", got.Authorizations, store.Authorizations)
	}
}

func TestSaveOverTightenedModeIsTightenedAgain(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "join-authorizations.json")
	// os.WriteFile's mode argument only applies ON CREATION, so a store
	// that already exists at 0666 stays there through a plain write. A
	// Save that reuses that shape would leave the file world-writable
	// for as long as nobody noticed, which is the exact failure the
	// atomic-write-then-chmod exists to prevent.
	if err := os.WriteFile(path, []byte("{}\n"), 0o666); err != nil {
		t.Fatalf("seeding a loose file: %v", err)
	}
	if err := os.Chmod(path, 0o666); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	if err := (Store{Authorizations: []Entry{{ID: "auth-1", NodeID: "n", Fingerprint: testFingerprint, ExpiresAtUnix: 1}}}).Save(path); err != nil {
		t.Fatalf("Save() error: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat() error: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Errorf("Save() over a 0666 file left mode %04o, want 0600", got)
	}
}

func TestSaveLeavesNoTempFileBehind(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "join-authorizations.json")
	if err := (Store{Authorizations: []Entry{{ID: "auth-1", NodeID: "n", Fingerprint: testFingerprint, ExpiresAtUnix: 1}}}).Save(path); err != nil {
		t.Fatalf("Save() error: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir() error: %v", err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".join-authorizations-") {
			t.Errorf("Save() left its temp file %s behind; an interrupted Save is exactly the partial file Load has to refuse", e.Name())
		}
	}
}

func TestSaveSortsByIDSoTwoCombsProduceTheSameBytes(t *testing.T) {
	// The reason for sorting: an operator diagnosing a stale copy
	// compares two files by eye or with diff. Two Combs holding the same
	// entries written by two runs in different orders must produce
	// identical bytes, or every diff is noise.
	entries := []Entry{
		{ID: "auth-ccc", NodeID: "c", Fingerprint: testFingerprint, ExpiresAtUnix: 3},
		{ID: "auth-aaa", NodeID: "a", Fingerprint: testFingerprint, ExpiresAtUnix: 1},
		{ID: "auth-bbb", NodeID: "b", Fingerprint: testFingerprint, ExpiresAtUnix: 2},
	}
	dir := t.TempDir()
	one := filepath.Join(dir, "one.json")
	two := filepath.Join(dir, "two.json")
	if err := (Store{Authorizations: entries}).Save(one); err != nil {
		t.Fatalf("Save(one) error: %v", err)
	}
	rev := []Entry{entries[0], entries[2], entries[1]}
	if err := (Store{Authorizations: rev}).Save(two); err != nil {
		t.Fatalf("Save(two) error: %v", err)
	}
	a, _ := os.ReadFile(one)
	b, _ := os.ReadFile(two)
	if !bytes.Equal(a, b) {
		t.Errorf("two Saves of the same entries in different orders wrote different bytes:\n%s\n---\n%s", a, b)
	}
}

func TestLoadMissingFileIsAnErrorNamingThePath(t *testing.T) {
	// A missing file and an empty one are different situations, and an
	// operator needs to be told which they are in: "no file" means this
	// Comb was never installed or the file was deleted, and neither is
	// fixed by creating an entry in it.
	path := filepath.Join(t.TempDir(), "join-authorizations.json")
	_, err := Load(path)
	if err == nil {
		t.Fatal("Load() of a missing file = nil error, want an error naming the path")
	}
	if !strings.Contains(err.Error(), path) {
		t.Errorf("Load() error %q does not name the path %q", err, path)
	}
}

func TestLoadMalformedFileIsAnErrorNamingThePath(t *testing.T) {
	for _, tc := range []struct{ name, body string }{
		{"truncated", `{"authorizations": [{"id": "auth-1"`},
		{"not an object", `[]`},
		{"wrong field type", `{"authorizations": {"id": "auth-1"}}`},
		{"trailing garbage", `{"authorizations": []} oops`},
		{"empty file", ``},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "join-authorizations.json")
			writeStore(t, path, tc.body)
			_, err := Load(path)
			if err == nil {
				t.Fatalf("Load(%q) = nil error, want an error naming the path", tc.body)
			}
			if !strings.Contains(err.Error(), path) {
				t.Errorf("Load() error %q does not name the path %q", err, path)
			}
		})
	}
}

func TestLoadRefusesUnknownFields(t *testing.T) {
	// An unknown field is a hand edit this binary does not understand.
	// Ignoring it would mean the next Save silently deletes whatever the
	// operator was trying to say - the same reason internal/hostinstall
	// refuses unknown fields in every config file it reads.
	path := filepath.Join(t.TempDir(), "join-authorizations.json")
	writeStore(t, path, `{"authorizations": [], "consumed": [{"id": "x"}]}`)
	_, err := Load(path)
	if err == nil {
		t.Fatal("Load() of a store carrying an unknown field = nil error, want a refusal")
	}
	if !strings.Contains(err.Error(), path) {
		t.Errorf("Load() error %q does not name the path", err)
	}
}

func TestLoadAcceptsAHandEditedEntry(t *testing.T) {
	// The refusal above is not a refusal to let a human write this file.
	// The ADR says hand-editing is honored, and the only way that stays
	// true is if a hand-edited file that IS well-formed loads.
	path := filepath.Join(t.TempDir(), "join-authorizations.json")
	writeStore(t, path, `{"authorizations":[{"id":"auth-byhand","node_id":"comb-9","fingerprint":"`+testFingerprint+`","expires_at_unix":1900000000}]}`)
	got, err := Load(path)
	if err != nil {
		t.Fatalf("Load() of a hand-edited store: %v", err)
	}
	if len(got.Authorizations) != 1 || got.Authorizations[0].ID != "auth-byhand" {
		t.Errorf("Load() = %+v, want the hand-written entry", got.Authorizations)
	}
}

func TestFindMatchesOnlyTheExactNodeAndFingerprint(t *testing.T) {
	now := time.Unix(1700000000, 0)
	store := Store{Authorizations: []Entry{{
		ID: "auth-1", NodeID: "comb-3", Fingerprint: testFingerprint, ExpiresAtUnix: now.Add(time.Hour).Unix(),
	}}}

	t.Run("exact match", func(t *testing.T) {
		entry, refusal := store.Find("comb-3", testFingerprint, now)
		if refusal != nil {
			t.Fatalf("Find() = refusal %q, want the entry", refusal)
		}
		if entry.ID != "auth-1" {
			t.Errorf("Find() = %q, want auth-1", entry.ID)
		}
	})

	t.Run("different node_id", func(t *testing.T) {
		_, refusal := store.Find("comb-4", testFingerprint, now)
		if refusal == nil || refusal.Reason != "no-entry" {
			t.Fatalf("Find() refusal = %+v, want reason no-entry", refusal)
		}
		if !strings.Contains(refusal.Detail, "comb-4") {
			t.Errorf("refusal %q does not name the node it could not find", refusal.Detail)
		}
	})

	t.Run("fingerprint is compared as a certificate, not as typed", func(t *testing.T) {
		// The fingerprint reaches this comparison through a human
		// retyping it off a web page, so case and the "sha256:" prefix
		// are typography rather than identity.
		_, refusal := store.Find("comb-3", "sha256:ab:cd:ef:01:23:45:67:89", now)
		if refusal != nil {
			t.Errorf("Find() with a lower-case, sha256:-prefixed fingerprint = refusal %q, want a match: the same certificate must compare equal however it was typed", refusal)
		}
	})

	t.Run("right node, wrong fingerprint names both", func(t *testing.T) {
		// An operator has to be able to see WHAT differs, not only that
		// something does: a stale entry from an earlier attempt and a
		// different Comb claiming the same identity both land here, and
		// they are not the same problem.
		_, refusal := store.Find("comb-3", "SHA256:00:00:00:00:00:00:00:00:00", now)
		if refusal == nil || refusal.Reason != "fingerprint-mismatch" {
			t.Fatalf("Find() refusal = %+v, want reason fingerprint-mismatch", refusal)
		}
		if !strings.Contains(refusal.Detail, "SHA256:00:00:00:00:00:00:00:00:00") {
			t.Errorf("refusal %q does not name the fingerprint that was presented", refusal.Detail)
		}
		if !strings.Contains(refusal.Detail, testFingerprint) {
			t.Errorf("refusal %q does not name the fingerprint the entry carries", refusal.Detail)
		}
	})
}

func TestFindRefusesAnExpiredEntryAndNamesItsDeadline(t *testing.T) {
	now := time.Unix(1700000000, 0)
	store := Store{Authorizations: []Entry{{
		ID: "auth-old", NodeID: "comb-3", Fingerprint: testFingerprint, ExpiresAtUnix: now.Add(-time.Second).Unix(),
	}}}
	_, refusal := store.Find("comb-3", testFingerprint, now)
	if refusal == nil || refusal.Reason != "expired" {
		t.Fatalf("Find() refusal = %+v, want reason expired", refusal)
	}
	if !strings.Contains(refusal.Detail, "auth-old") {
		t.Errorf("refusal %q does not name the entry that expired, so an operator cannot tell WHICH entry to recreate", refusal.Detail)
	}
}

func TestFindRefusesAtTheDeadlineNotAfterIt(t *testing.T) {
	// expires_at_unix is an absolute second. A request arriving in that
	// same second is not inside the authorization, and treating the
	// boundary as inclusive would make a one-second-wide authorization
	// valid that nobody could use and nobody could notice.
	deadline := time.Unix(1700000000, 0)
	store := Store{Authorizations: []Entry{{
		ID: "auth-edge", NodeID: "comb-3", Fingerprint: testFingerprint, ExpiresAtUnix: deadline.Unix(),
	}}}
	if _, refusal := store.Find("comb-3", testFingerprint, deadline); refusal == nil {
		t.Error("Find() exactly at expires_at_unix = nil refusal, want expired")
	}
	if _, refusal := store.Find("comb-3", testFingerprint, deadline.Add(-time.Second)); refusal != nil {
		t.Errorf("Find() one second before expires_at_unix = refusal %q, want the entry", refusal)
	}
}

func TestNormalizeFingerprint(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"SHA256:AB:CD", "SHA256:AB:CD"},
		{"sha256:ab:cd", "SHA256:AB:CD"},
		{"  SHA256:ab:cd  ", "SHA256:AB:CD"},
		// A value with no recognized prefix is uppercased rather than
		// rejected: NormalizeFingerprint normalizes, and whether a
		// fingerprint is well-formed is Load's and RequestJoinColony's
		// separate question, not this function's to answer.
		{"ab:cd", "AB:CD"},
	} {
		if got := NormalizeFingerprint(tc.in); got != tc.want {
			t.Errorf("NormalizeFingerprint(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestNewEntryIDIsDistinctAndPrefixed(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 64; i++ {
		id, err := NewEntryID()
		if err != nil {
			t.Fatalf("NewEntryID() error: %v", err)
		}
		if !strings.HasPrefix(id, "auth-") {
			t.Fatalf("NewEntryID() = %q, want an auth- prefix", id)
		}
		if seen[id] {
			t.Fatalf("NewEntryID() repeated %q within 64 calls", id)
		}
		seen[id] = true
	}
}

func TestDefaultTTLIsTwentyFourHours(t *testing.T) {
	// The owner answered ADR-0147's own question 1 with 24 hours. This
	// test exists so the answer cannot be quietly changed later without
	// somebody noticing that a constant moved.
	if DefaultTTL != 24*time.Hour {
		t.Errorf("DefaultTTL = %s, want 24h", DefaultTTL)
	}
}

func TestDefaultPathIsNotADaemonConfigFile(t *testing.T) {
	// The mechanism depends on the store NOT being one of the config
	// files the three Update*Config RPCs can write. A path under the
	// same directory is fine; the same NAME as a config file is not.
	if got := filepath.Base(DefaultPath); got != "join-authorizations.json" {
		t.Errorf("DefaultPath base = %q, want join-authorizations.json", got)
	}
	for _, configName := range []string{"common.json", "managerd.json", "raftd.json", "frontend.json", "restshimd.json"} {
		if filepath.Base(DefaultPath) == configName {
			t.Fatalf("DefaultPath is %q, which is a config file an Admin-tier caller can write", configName)
		}
	}
}

func TestStoreRoundTripsThroughJSONWithoutLoss(t *testing.T) {
	// A test that the struct is JSON-shaped, so a hand edit and a Save
	// cannot disagree about what a field is called. The store's whole
	// value is that a human can read and write it.
	path := filepath.Join(t.TempDir(), "join-authorizations.json")
	body := `{"authorizations":[{"id":"auth-1","node_id":"comb-3","fingerprint":"` + testFingerprint + `","expires_at_unix":1800000000}]}`
	writeStore(t, path, body)
	store, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	if err := store.Save(path); err != nil {
		t.Fatalf("Save() error: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile() error: %v", err)
	}
	var probe struct {
		Authorizations []map[string]any `json:"authorizations"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		t.Fatalf("re-reading the saved file: %v", err)
	}
	if len(probe.Authorizations) != 1 {
		t.Fatalf("saved file has %d entries, want 1: %s", len(probe.Authorizations), raw)
	}
	for _, key := range []string{"id", "node_id", "fingerprint", "expires_at_unix"} {
		if _, ok := probe.Authorizations[0][key]; !ok {
			t.Errorf("saved entry is missing %q, so the file on disk does not use the field name the ADR documents: %s", key, raw)
		}
	}
}
