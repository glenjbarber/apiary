package tlscache

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// generatePair returns a PEM-encoded self-signed certificate/key for
// commonName, distinct on each call (a fresh key and serial), so a test
// can tell two generated pairs apart.
func generatePair(t *testing.T, commonName string) (certPEM, keyPEM []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generating key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: commonName},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("creating certificate: %v", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatalf("marshaling key: %v", err)
	}
	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	return certPEM, keyPEM
}

func writePair(t *testing.T, certPath, keyPath string, certPEM, keyPEM []byte) {
	t.Helper()
	if err := os.WriteFile(certPath, certPEM, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, keyPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	// Force a distinct mtime from whatever was there before - some
	// filesystems have coarse mtime resolution, and the test needs two
	// successive writes to look different to a stat-based cache.
	future := time.Now().Add(time.Hour)
	os.Chtimes(certPath, future, future)
	os.Chtimes(keyPath, future, future)
}

func TestGetCertificate_ReloadsWhenFilesChange(t *testing.T) {
	dir := t.TempDir()
	certPath, keyPath := filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
	cert1, key1 := generatePair(t, "first")
	writePair(t, certPath, keyPath, cert1, key1)

	var logged []string
	cache, err := New(certPath, keyPath, func(format string, args ...any) {
		logged = append(logged, format)
	})
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}

	got, err := cache.GetCertificate(nil)
	if err != nil {
		t.Fatalf("GetCertificate() error: %v", err)
	}
	firstLeaf, err := x509.ParseCertificate(got.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	if firstLeaf.Subject.CommonName != "first" {
		t.Fatalf("CommonName = %q, want %q", firstLeaf.Subject.CommonName, "first")
	}

	// Replace with a different pair - a renewal landing on disk.
	cert2, key2 := generatePair(t, "second")
	// Advance further still so the new mtime is unambiguously later.
	time.Sleep(10 * time.Millisecond)
	certPath2, keyPath2 := certPath, keyPath
	if err := os.WriteFile(certPath2, cert2, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath2, key2, 0o600); err != nil {
		t.Fatal(err)
	}
	future := time.Now().Add(2 * time.Hour)
	os.Chtimes(certPath, future, future)
	os.Chtimes(keyPath, future, future)

	got, err = cache.GetCertificate(nil)
	if err != nil {
		t.Fatalf("GetCertificate() after replace error: %v", err)
	}
	secondLeaf, err := x509.ParseCertificate(got.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	if secondLeaf.Subject.CommonName != "second" {
		t.Errorf("CommonName after replace = %q, want %q - reload did not pick up the new pair", secondLeaf.Subject.CommonName, "second")
	}
}

func TestGetCertificate_UnchangedFilesServeCachedCertWithNoDiskRead(t *testing.T) {
	dir := t.TempDir()
	certPath, keyPath := filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
	certPEM, keyPEM := generatePair(t, "stable")
	writePair(t, certPath, keyPath, certPEM, keyPEM)

	cache, err := New(certPath, keyPath, nil)
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}

	first, err := cache.GetCertificate(nil)
	if err != nil {
		t.Fatal(err)
	}

	// Remove the files entirely. If GetCertificate kept returning the
	// cached certificate without re-reading (because mtimes/sizes are
	// unchanged from its view), this has no effect; if it blindly
	// reloaded every call, this would fail it.
	if err := os.Remove(certPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(keyPath); err != nil {
		t.Fatal(err)
	}

	second, err := cache.GetCertificate(nil)
	if err != nil {
		t.Fatalf("GetCertificate() error = %v, want nil - deleting the files after a successful load should not surface through a handshake", err)
	}
	if string(second.Certificate[0]) != string(first.Certificate[0]) {
		t.Error("certificate changed even though the on-disk files never changed before being removed")
	}
}

func TestGetCertificate_CorruptReplacementKeepsServingPreviousPair(t *testing.T) {
	dir := t.TempDir()
	certPath, keyPath := filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
	goodCert, goodKey := generatePair(t, "good")
	writePair(t, certPath, keyPath, goodCert, goodKey)

	var logged []string
	cache, err := New(certPath, keyPath, func(format string, args ...any) {
		logged = append(logged, format)
	})
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}

	before, err := cache.GetCertificate(nil)
	if err != nil {
		t.Fatal(err)
	}

	// Simulate a corrupt/mismatched replacement: a new key paired with
	// the OLD certificate, which tls.LoadX509KeyPair rejects outright.
	_, mismatchedKey := generatePair(t, "mismatched")
	if err := os.WriteFile(keyPath, mismatchedKey, 0o600); err != nil {
		t.Fatal(err)
	}
	future := time.Now().Add(time.Hour)
	os.Chtimes(keyPath, future, future)

	after, err := cache.GetCertificate(nil)
	if err != nil {
		t.Fatalf("GetCertificate() error = %v, want nil - a bad replacement must not fail the handshake", err)
	}
	if string(after.Certificate[0]) != string(before.Certificate[0]) {
		t.Error("certificate changed despite the replacement pair failing to load - should keep serving the previous one")
	}
	if len(logged) == 0 {
		t.Error("no log line recorded for the failed reload")
	}
}

func TestNew_FailsFastOnBadInitialPair(t *testing.T) {
	dir := t.TempDir()
	certPath, keyPath := filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
	if _, err := New(certPath, keyPath, nil); err == nil {
		t.Fatal("New() error = nil, want non-nil when the files do not exist at all")
	}
}
