package origincert

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

type fakeIssuer struct{}

func (fakeIssuer) Issue(context.Context, string, []string, int, string) (IssuedCertificate, error) {
	return IssuedCertificate{}, nil
}

func TestNewCSRAndWritePair(t *testing.T) {
	csr, key, err := NewCSR([]string{"api.example.com"})
	if err != nil {
		t.Fatal(err)
	}
	if csr == "" || key == "" {
		t.Fatal("CSR or key was empty")
	}
	// A CSR is intentionally not a certificate, so use a matching self-signed
	// fixture from the standard parser's own error behavior only to prove that
	// WritePair refuses an unissued certificate before touching the filesystem.
	if _, _, err := WritePair(t.TempDir(), "frontend", csr, key); err == nil {
		t.Error("WritePair accepted a CSR as a certificate")
	}
}

func TestWritePairRejectsPathName(t *testing.T) {
	if _, _, err := WritePair(t.TempDir(), "../escape", "", ""); err == nil {
		t.Error("WritePair accepted a path-like name")
	}
}

func TestInventoryRoundTripDoesNotRequireCertificateMaterial(t *testing.T) {
	dir := t.TempDir()
	want := []InventoryEntry{{
		Name:      "managerd",
		Service:   "apiary_managerd",
		Hostnames: []string{"api.example.com"},
		ID:        "cert-1",
		ExpiresAt: time.Date(2031, 1, 2, 3, 4, 5, 0, time.UTC),
		CertPath:  filepath.Join(dir, "managerd.crt"),
		KeyPath:   filepath.Join(dir, "managerd.key"),
		UpdatedAt: time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC),
	}}
	if err := SaveInventory(dir, want); err != nil {
		t.Fatal(err)
	}
	got, err := LoadInventory(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != want[0].ID || got[0].KeyPath != want[0].KeyPath {
		t.Errorf("LoadInventory() = %+v, want %+v", got, want)
	}
}

func TestIssueRejectsIncompleteIssuerResultBeforeWriting(t *testing.T) {
	dir := t.TempDir()
	_, err := Issue(context.Background(), fakeIssuer{}, IssueRequest{
		Directory: dir, Name: "managerd", Service: "apiary_managerd",
		Hostnames: []string{"api.example.com"}, ValidityDays: 365, Token: "token",
	})
	if err == nil {
		t.Fatal("Issue accepted an incomplete issuer result")
	}
	entries, err := LoadInventory(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("inventory = %+v, want empty after failed issuance", entries)
	}
}
