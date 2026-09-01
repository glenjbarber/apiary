package tlsdial

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

// genSelfSignedCertPEM returns a real, valid self-signed certificate in
// PEM form - generated fresh per test rather than hand-written, since a
// hand-crafted PEM/DER blob is easy to get subtly wrong in a way that
// only matters at parse time.
func genSelfSignedCertPEM(t *testing.T) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey() error: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{Organization: []string{"apiary-test"}},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageCertSign,
		IsCA:         true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("CreateCertificate() error: %v", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

func TestManagerDialOption_PlaintextIgnoresCAFile(t *testing.T) {
	// useTLS=false must never even try to read caFile - a nonexistent
	// path here would error if it were consulted.
	if _, err := ManagerDialOption(false, "/nonexistent/ca.pem", ""); err != nil {
		t.Errorf("ManagerDialOption(false, ...) error: %v, want nil (CA file should be ignored)", err)
	}
}

func TestManagerDialOption_TLSWithNoCAUsesSystemPool(t *testing.T) {
	if _, err := ManagerDialOption(true, "", ""); err != nil {
		t.Errorf("ManagerDialOption(true, \"\") error: %v, want nil", err)
	}
}

func TestManagerDialOption_ServerNameOverrideSucceeds(t *testing.T) {
	// A non-empty serverName must be accepted without error - the whole
	// point is decoupling the verified hostname from the dialed address
	// (e.g. managerd stays loopback-only but its cert names a real
	// public hostname).
	if _, err := ManagerDialOption(true, "", "apiarium.apiary.work"); err != nil {
		t.Errorf("ManagerDialOption(true, \"\", \"apiarium.apiary.work\") error: %v, want nil", err)
	}
}

func TestManagerDialOption_TLSWithMissingCAFileErrors(t *testing.T) {
	if _, err := ManagerDialOption(true, "/nonexistent/ca.pem", ""); err == nil {
		t.Error("ManagerDialOption(true, missing file) = nil error, want an error")
	}
}

func TestManagerDialOption_TLSWithMalformedCAFileErrors(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ca.pem")
	if err := os.WriteFile(path, []byte("not a real certificate"), 0o644); err != nil {
		t.Fatalf("WriteFile() error: %v", err)
	}

	if _, err := ManagerDialOption(true, path, ""); err == nil {
		t.Error("ManagerDialOption(true, malformed file) = nil error, want an error")
	}
}

func TestManagerDialOption_TLSWithValidCAFileSucceeds(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ca.pem")
	if err := os.WriteFile(path, genSelfSignedCertPEM(t), 0o644); err != nil {
		t.Fatalf("WriteFile() error: %v", err)
	}

	if _, err := ManagerDialOption(true, path, ""); err != nil {
		t.Errorf("ManagerDialOption(true, valid file) error: %v, want nil", err)
	}
}
