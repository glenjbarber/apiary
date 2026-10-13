package acme

import (
	"crypto"
	"os"
	"path/filepath"
	"testing"

	"github.com/glenjbarber/apiary/internal/origincert"
)

type equalPublicKey interface {
	Equal(crypto.PublicKey) bool
}

func TestLoadOrCreateAccountKey_GeneratesAndPersists(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "account.key")

	key1, err := loadOrCreateAccountKey(path)
	if err != nil {
		t.Fatalf("loadOrCreateAccountKey() error: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat account key: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("account key mode = %o, want 0600", info.Mode().Perm())
	}

	key2, err := loadOrCreateAccountKey(path)
	if err != nil {
		t.Fatalf("loadOrCreateAccountKey() second call error: %v", err)
	}
	pub1, ok := key1.Public().(equalPublicKey)
	if !ok {
		t.Fatalf("key1.Public() does not implement Equal")
	}
	if !pub1.Equal(key2.Public()) {
		t.Error("second loadOrCreateAccountKey() call generated a different key instead of reloading the persisted one")
	}
}

func TestLoadOrCreateAccountKey_RejectsCorruptFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "account.key")
	if err := os.WriteFile(path, []byte("not a key"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadOrCreateAccountKey(path); err == nil {
		t.Fatal("loadOrCreateAccountKey() error = nil, want non-nil for a corrupt file")
	}
}

func TestCSRDERFromPEM(t *testing.T) {
	_, _, err := origincert.NewCSR([]string{"example.com"})
	if err != nil {
		t.Fatalf("NewCSR() error: %v", err)
	}
	csrPEM, _, err := origincert.NewCSR([]string{"example.com"})
	if err != nil {
		t.Fatalf("NewCSR() error: %v", err)
	}
	der, err := csrDERFromPEM(csrPEM)
	if err != nil {
		t.Fatalf("csrDERFromPEM() error: %v", err)
	}
	if len(der) == 0 {
		t.Error("csrDERFromPEM() returned empty DER")
	}
}

func TestCSRDERFromPEM_RejectsNonCSR(t *testing.T) {
	if _, err := csrDERFromPEM("not pem at all"); err == nil {
		t.Fatal("csrDERFromPEM() error = nil, want non-nil for invalid input")
	}
}
