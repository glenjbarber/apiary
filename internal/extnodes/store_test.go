package extnodes

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// twoAuthz is a valid pair of authorizing acts: two distinct keys on two
// distinct Colony members.
func twoAuthz() []Authorization {
	return []Authorization{
		{KeyID: "key-a", NodeID: "comb-1", AtUnix: 1000},
		{KeyID: "key-b", NodeID: "comb-2", AtUnix: 1001},
	}
}

func enrollment(key Key, fingerprint string) Enrollment {
	return Enrollment{
		Key:            key,
		Fingerprint:    fingerprint,
		Status:         "online",
		EnrolledAt:     time.Unix(2000, 0).UTC(),
		Authorizations: twoAuthz(),
		EnrolledBy:     "external-discovery",
	}
}

func TestEnrollRecordsAndIsIdempotent(t *testing.T) {
	store := NewStore()
	key := Key{Provider: "proxmox", SourceID: "cluster-a", Name: "pve1"}

	record, err := store.Enroll(enrollment(key, "fp-1"))
	if err != nil {
		t.Fatalf("Enroll: err = %v, want nil", err)
	}
	if record.Key != key {
		t.Errorf("Key = %v, want %v", record.Key, key)
	}
	if got := store.Status(key); got != StatusEnrolled {
		t.Errorf("Status = %q, want %q", got, StatusEnrolled)
	}

	// Enrolling the same host again is not an error and does not make a
	// second record. This is what makes enrollment safe to retry.
	again, err := store.Enroll(enrollment(key, "fp-1"))
	if err != nil {
		t.Fatalf("second Enroll: err = %v, want nil", err)
	}
	if again.EnrolledAt != record.EnrolledAt {
		t.Errorf("EnrolledAt changed on re-enroll: %v -> %v", record.EnrolledAt, again.EnrolledAt)
	}
	if n := len(store.Enrollments()); n != 1 {
		t.Errorf("Enrollments() = %d records, want 1", n)
	}
}

func TestEnrollRefusesFingerprintChangeUnderMatchedKey(t *testing.T) {
	store := NewStore()
	key := Key{Provider: "proxmox", SourceID: "cluster-a", Name: "pve1"}

	if _, err := store.Enroll(enrollment(key, "fp-1")); err != nil {
		t.Fatalf("Enroll: err = %v, want nil", err)
	}

	// A different fingerprint under a matched key is a conflict. The
	// stored record must be left exactly as it was.
	if _, err := store.Enroll(enrollment(key, "fp-2")); err == nil {
		t.Fatalf("second Enroll: err = nil, want an error")
	}
	record, ok := store.Get(key)
	if !ok {
		t.Fatalf("Get: the record was removed")
	}
	if record.Fingerprint != "fp-1" {
		t.Errorf("Fingerprint = %q, want %q - a conflicting enrollment must not overwrite the record", record.Fingerprint, "fp-1")
	}
	if got := store.Status(key); got != StatusConflict {
		t.Errorf("Status = %q, want %q", got, StatusConflict)
	}
}

func TestEnrollRefusesOneAuthorization(t *testing.T) {
	store := NewStore()
	key := Key{Provider: "proxmox", SourceID: "cluster-a", Name: "pve1"}

	one := enrollment(key, "fp-1")
	one.Authorizations = one.Authorizations[:1]
	if _, err := store.Enroll(one); err == nil {
		t.Errorf("one authorization: err = nil, want an error")
	}
}

func TestEnrollRefusesSameKeyTwice(t *testing.T) {
	store := NewStore()
	key := Key{Provider: "proxmox", SourceID: "cluster-a", Name: "pve1"}

	same := enrollment(key, "fp-1")
	same.Authorizations = []Authorization{
		{KeyID: "key-a", NodeID: "comb-1"},
		{KeyID: "key-a", NodeID: "comb-2"},
	}
	if _, err := store.Enroll(same); err == nil {
		t.Errorf("same key twice: err = nil, want an error")
	}
}

func TestEnrollRefusesSameCombTwice(t *testing.T) {
	store := NewStore()
	key := Key{Provider: "proxmox", SourceID: "cluster-a", Name: "pve1"}

	sameComb := enrollment(key, "fp-1")
	sameComb.Authorizations = []Authorization{
		{KeyID: "key-a", NodeID: "comb-1"},
		{KeyID: "key-b", NodeID: "comb-1"},
	}
	if _, err := store.Enroll(sameComb); err == nil {
		t.Errorf("same Comb twice: err = nil, want an error")
	}
}

func TestEnrollRefusesEmptyIdentity(t *testing.T) {
	store := NewStore()
	if _, err := store.Enroll(enrollment(Key{SourceID: "c", Name: "n"}, "fp")); err == nil {
		t.Errorf("empty provider: err = nil, want an error")
	}
	if _, err := store.Enroll(enrollment(Key{Provider: "p", SourceID: "c"}, "fp")); err == nil {
		t.Errorf("empty name: err = nil, want an error")
	}
}

func TestAbsentIsKeptAndRemovalIsExplicit(t *testing.T) {
	store := NewStore()
	key := Key{Provider: "proxmox", SourceID: "cluster-a", Name: "pve1"}
	if _, err := store.Enroll(enrollment(key, "fp-1")); err != nil {
		t.Fatalf("Enroll: err = %v, want nil", err)
	}

	// A host that vanished is marked absent, not removed.
	if err := store.SetStatus(key, StatusAbsent); err != nil {
		t.Fatalf("SetStatus: err = %v, want nil", err)
	}
	if _, ok := store.Get(key); !ok {
		t.Errorf("Get: the record was removed by an absence; it must be kept")
	}
	if got := store.Status(key); got != StatusAbsent {
		t.Errorf("Status = %q, want %q", got, StatusAbsent)
	}

	// Removal is an explicit operator action.
	if err := store.Remove(key); err != nil {
		t.Fatalf("Remove: err = %v, want nil", err)
	}
	if _, ok := store.Get(key); ok {
		t.Errorf("Get: the record survived an explicit removal")
	}
}

func TestSetStatusRefusesUnenrolledKey(t *testing.T) {
	store := NewStore()
	key := Key{Provider: "proxmox", SourceID: "cluster-a", Name: "pve1"}
	if err := store.SetStatus(key, StatusAbsent); err == nil {
		t.Errorf("err = nil, want an error")
	}
}

func TestSaveAndLoadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "external-nodes.json")

	store, err := Load(path)
	if err != nil {
		t.Fatalf("Load: err = %v, want nil", err)
	}
	key := Key{Provider: "proxmox", SourceID: "cluster-a", Name: "pve1"}
	if _, err := store.Enroll(enrollment(key, "fp-1")); err != nil {
		t.Fatalf("Enroll: err = %v, want nil", err)
	}
	if err := store.SetStatus(key, StatusAbsent); err != nil {
		t.Fatalf("SetStatus: err = %v, want nil", err)
	}
	if err := store.Save(); err != nil {
		t.Fatalf("Save: err = %v, want nil", err)
	}

	// The file holds which external hosts are trusted, so it must not
	// be readable by anyone else.
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("mode = %04o, want 0600", perm)
	}

	reloaded, err := Load(path)
	if err != nil {
		t.Fatalf("Load after Save: err = %v, want nil", err)
	}
	record, ok := reloaded.Get(key)
	if !ok {
		t.Fatalf("Get after reload: the record did not survive")
	}
	if record.Fingerprint != "fp-1" {
		t.Errorf("Fingerprint = %q, want %q", record.Fingerprint, "fp-1")
	}
	if len(record.Authorizations) != 2 {
		t.Errorf("Authorizations = %d, want 2", len(record.Authorizations))
	}
	if got := reloaded.Status(key); got != StatusAbsent {
		t.Errorf("Status = %q, want %q", got, StatusAbsent)
	}
}

func TestLoadMissingFileIsEmpty(t *testing.T) {
	store, err := Load(filepath.Join(t.TempDir(), "absent.json"))
	if err != nil {
		t.Fatalf("Load: err = %v, want nil", err)
	}
	if n := len(store.Enrollments()); n != 0 {
		t.Errorf("Enrollments() = %d, want 0", n)
	}
}

func TestLoadUnparseableFileIsAnError(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "external-nodes.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if _, err := Load(path); err == nil {
		t.Errorf("Load: err = nil, want an error - an unreadable store must not read as empty")
	}
}

func TestKeyRoundTrip(t *testing.T) {
	for _, key := range []Key{
		{Provider: "proxmox", SourceID: "cluster-a", Name: "pve1"},
		{Provider: "sylve", SourceID: "", Name: "sylve-1"},
	} {
		got, ok := parseKey(key.String())
		if !ok {
			t.Fatalf("parseKey(%q): ok = false", key.String())
		}
		if got != key {
			t.Errorf("parseKey(%q) = %v, want %v", key.String(), got, key)
		}
	}
}
