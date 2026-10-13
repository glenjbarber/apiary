package manager

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

// These tests are about one decision: whether a member may skip pinning
// because its own serving certificate is publicly trusted. The safe
// answer to anything that cannot be established is "not trusted", which
// keeps today's pinning behavior, so most of these cases assert false.

// trustTestCert issues one certificate. A nil parent makes it
// self-signed. It returns the parsed certificate, its key and its PEM.
func trustTestCert(t *testing.T, name string, serial int64, isCA bool, parent *x509.Certificate, parentKey *ecdsa.PrivateKey) (*x509.Certificate, *ecdsa.PrivateKey, []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generating key for %s: %v", name, err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(serial),
		Subject:      pkix.Name{CommonName: name},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{name},
	}
	if isCA {
		tmpl.IsCA = true
		tmpl.BasicConstraintsValid = true
		tmpl.KeyUsage = x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature
		tmpl.ExtKeyUsage = nil
		tmpl.DNSNames = nil
	}
	signer, signerKey := tmpl, key
	if parent != nil {
		signer, signerKey = parent, parentKey
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, signer, &key.PublicKey, signerKey)
	if err != nil {
		t.Fatalf("creating certificate %s: %v", name, err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parsing certificate %s: %v", name, err)
	}
	return cert, key, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

func writeTrustTestFile(t *testing.T, dir, name string, data []byte) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("writing %s: %v", name, err)
	}
	return path
}

func TestServingCertificateChainsToRoots(t *testing.T) {
	root, rootKey, _ := trustTestCert(t, "test-root", 1, true, nil, nil)
	inter, interKey, interPEM := trustTestCert(t, "test-intermediate", 2, true, root, rootKey)
	_, _, leafPEM := trustTestCert(t, "brood.example.test", 3, false, inter, interKey)
	_, _, selfPEM := trustTestCert(t, "self.example.test", 4, false, nil, nil)

	roots := x509.NewCertPool()
	roots.AddCert(root)

	dir := t.TempDir()
	fullchain := writeTrustTestFile(t, dir, "fullchain.pem", append(append([]byte{}, leafPEM...), interPEM...))
	leafOnly := writeTrustTestFile(t, dir, "leaf.pem", leafPEM)
	selfSigned := writeTrustTestFile(t, dir, "self.pem", selfPEM)
	notACert := writeTrustTestFile(t, dir, "junk.pem", []byte("not a certificate\n"))

	cases := []struct {
		name  string
		path  string
		roots *x509.CertPool
		want  bool
	}{
		{"leaf plus intermediate chains to a trusted root", fullchain, roots, true},
		{"leaf alone is missing its intermediate", leafOnly, roots, false},
		{"a root that does not issue the chain is not trust", fullchain, x509.NewCertPool(), false},
		{"a self-signed certificate is not publicly trusted", selfSigned, roots, false},
		{"the system pool does not trust a test CA", fullchain, nil, false},
		{"a missing file is not trusted", filepath.Join(dir, "absent.pem"), roots, false},
		{"a file with no certificate is not trusted", notACert, roots, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := servingCertificateChainsToRoots(tc.path, tc.roots); got != tc.want {
				t.Errorf("servingCertificateChainsToRoots = %v, want %v", got, tc.want)
			}
		})
	}
}
