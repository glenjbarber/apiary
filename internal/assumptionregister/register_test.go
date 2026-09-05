package assumptionregister

import (
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
