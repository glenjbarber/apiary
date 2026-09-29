package assumptionregister

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestManagerSaveListDelete(t *testing.T) {
	m := &Manager{Path: filepath.Join(t.TempDir(), "register.json")}
	now := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)
	claim := Claim{ID: "peer-tls", Statement: "Peer TLS identity is valid", Owner: "ops", Scope: "apiarium to apiverse", Evidence: "2026-09-05 TLS probe", ExpiresAt: now.Add(24 * time.Hour)}
	if err := m.Save(claim, now); err != nil {
		t.Fatal(err)
	}
	claims, err := m.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(claims) != 1 || claims[0].CreatedAt != now || claims[0].UpdatedAt != now {
		t.Fatalf("claims = %#v", claims)
	}
	if err := m.Delete(claim.ID); err != nil {
		t.Fatal(err)
	}
	claims, err = m.List()
	if err != nil || len(claims) != 0 {
		t.Fatalf("claims = %#v, err = %v", claims, err)
	}
}

func TestClaimValidate(t *testing.T) {
	if err := (Claim{}).Validate(); err == nil {
		t.Fatal("Validate() = nil, want error")
	}
}

// TestStoreLoadsARegisterWrittenBeforeTheEvidenceFields exists because
// the evidence fields were added to an already-deployed on-disk format.
// A hive that has been running the register for a month holds a file
// with no evidence_status and no last_verified in it, and refusing to
// read that would turn a schema addition into data loss. Every such
// claim must come back as silence - unknown, never supported - and stay
// saveable, so an operator can add evidence to an old claim rather than
// retyping it.
func TestStoreLoadsARegisterWrittenBeforeTheEvidenceFields(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "register.json")

	// Byte-for-byte the shape written before the fields existed: no
	// evidence_status, no last_verified, no consequence_if_false.
	legacy := `[
  {
    "id": "offsite-backup",
    "statement": "An off-platform backup of the VM base images exists",
    "owner": "ops",
    "scope": "colony",
    "evidence": "2026-09-01 restic snapshot listing",
    "expires_at": "2099-01-01T00:00:00Z",
    "created_at": "2026-09-01T09:00:00Z",
    "updated_at": "2026-09-01T09:00:00Z"
  }
]
`
	if err := os.WriteFile(path, []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}

	m := &Manager{Path: path}
	claims, err := m.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(claims) != 1 {
		t.Fatalf("claims = %d, want 1", len(claims))
	}
	got := claims[0]
	if got.ID != "offsite-backup" || got.Owner != "ops" || got.Evidence == "" {
		t.Fatalf("legacy claim did not round trip its own fields: %#v", got)
	}
	if got.EvidenceStatus != "" || !got.LastVerified.IsZero() {
		t.Fatalf("a legacy claim gained evidence fields it never had: %#v", got)
	}
	if s := got.State(time.Date(2026, 9, 29, 0, 0, 0, 0, time.UTC)); s != StateUnknown {
		t.Fatalf("a claim with no evidence fields evaluates %q, want unknown", s)
	}

	// And it must still be saveable, so the operator can add evidence
	// to a claim recorded months ago without retyping the statement.
	got.EvidenceStatus = EvidenceContradicted
	got.LastVerified = time.Date(2026, 9, 20, 0, 0, 0, 0, time.UTC)
	if err := m.Save(got, time.Date(2026, 9, 29, 0, 0, 0, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}
	back, err := m.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(back) != 1 || back[0].EvidenceStatus != EvidenceContradicted || back[0].LastVerified.IsZero() {
		t.Fatalf("updated legacy claim = %#v", back)
	}
	if !back[0].CreatedAt.Equal(got.CreatedAt) {
		t.Fatalf("created_at = %s, want the original %s", back[0].CreatedAt, got.CreatedAt)
	}
}
