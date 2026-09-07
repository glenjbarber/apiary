package raft

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// testCA is a self-signed CA, generated fresh per test, used to sign leaf
// certificates for each simulated raft node - mirroring
// internal/tlsdial's own genSelfSignedCertPEM precedent, extended to
// actually sign leaves rather than being used bare.
type testCA struct {
	cert    *x509.Certificate
	certPEM []byte
	key     *ecdsa.PrivateKey
}

func newTestCA(t *testing.T) *testCA {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{Organization: []string{"apiary-raft-test-ca"}},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return &testCA{cert: cert, certPEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), key: key}
}

// issueLeaf signs a leaf certificate valid for "localhost"/127.0.0.1, and
// writes the CA file plus the leaf's own cert/key files into t.TempDir(),
// returning the three paths newTLSConfig/Config.TLSCert et al. expect.
func (ca *testCA) issueLeaf(t *testing.T) (certPath, keyPath, caPath string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{Organization: []string{"apiary-raft-test-node"}},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
		DNSNames:     []string{"localhost"},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	certPath = filepath.Join(dir, "node.crt")
	keyPath = filepath.Join(dir, "node.key")
	caPath = filepath.Join(dir, "ca.crt")
	if err := os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(caPath, ca.certPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	return certPath, keyPath, caPath
}

func TestConfig_WithDefaults_RejectsPartialTLSFields(t *testing.T) {
	cfg := Config{DataDir: t.TempDir(), BindAddr: freeLoopbackAddr(t), TLSCert: "/some/cert"}
	if _, err := cfg.withDefaults(); err == nil {
		t.Fatal("withDefaults() accepted a partial TLS config (cert set, key/CA empty)")
	}
}

func TestConfig_WithDefaults_AllowsAllEmptyOrAllSetTLSFields(t *testing.T) {
	plain := Config{DataDir: t.TempDir(), BindAddr: freeLoopbackAddr(t)}
	if _, err := plain.withDefaults(); err != nil {
		t.Errorf("withDefaults() with no TLS fields set: %v, want nil", err)
	}
	full := Config{DataDir: t.TempDir(), BindAddr: freeLoopbackAddr(t), TLSCert: "a", TLSKey: "b", TLSCA: "c"}
	if _, err := full.withDefaults(); err != nil {
		t.Errorf("withDefaults() with all three TLS fields set: %v, want nil", err)
	}
}

func TestNewTLSConfig_MissingCertFileErrors(t *testing.T) {
	ca := newTestCA(t)
	_, _, caPath := ca.issueLeaf(t)
	if _, err := newTLSConfig("/nonexistent/cert.pem", "/nonexistent/key.pem", caPath); err == nil {
		t.Fatal("newTLSConfig() accepted a nonexistent certificate/key path")
	}
}

func TestNewTLSConfig_InvalidCAFileErrors(t *testing.T) {
	ca := newTestCA(t)
	certPath, keyPath, _ := ca.issueLeaf(t)
	badCA := filepath.Join(t.TempDir(), "bad-ca.pem")
	if err := os.WriteFile(badCA, []byte("not a certificate"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := newTLSConfig(certPath, keyPath, badCA); err == nil {
		t.Fatal("newTLSConfig() accepted a CA file with no valid certificates")
	}
}

// TestTwoNodeClusterReplicatesOverMutualTLS is this codebase's first
// automated multi-node raft test - every prior multi-node claim was
// verified live against the real apiarium/apiverse cluster instead,
// since raft.NewTCPTransport alone is straightforward to run twice in
// one process on different loopback ports. It proves two things at
// once: that AddVoter/replication genuinely works end to end, and that
// it works over the new TLS transport specifically, not just that TLS
// handshakes succeed in isolation.
func TestTwoNodeClusterReplicatesOverMutualTLS(t *testing.T) {
	ca := newTestCA(t)

	cert1, key1, ca1 := ca.issueLeaf(t)
	cfg1 := Config{NodeID: "node-1", DataDir: t.TempDir(), BindAddr: freeLoopbackAddr(t), TLSCert: cert1, TLSKey: key1, TLSCA: ca1}
	node1, err := New(cfg1)
	if err != nil {
		t.Fatalf("New(node-1) error: %v", err)
	}
	t.Cleanup(func() { node1.Shutdown() })

	cert2, key2, ca2 := ca.issueLeaf(t)
	cfg2 := Config{NodeID: "node-2", DataDir: t.TempDir(), BindAddr: freeLoopbackAddr(t), TLSCert: cert2, TLSKey: key2, TLSCA: ca2}
	node2, err := New(cfg2)
	if err != nil {
		t.Fatalf("New(node-2) error: %v", err)
	}
	t.Cleanup(func() { node2.Shutdown() })

	if err := node1.Bootstrap(); err != nil {
		t.Fatalf("Bootstrap() error: %v", err)
	}
	eventually(t, 5*time.Second, func() bool { return node1.Status().IsLeader })

	if err := node1.AddVoter(cfg2.NodeID, cfg2.BindAddr, 0, 5*time.Second); err != nil {
		t.Fatalf("AddVoter() error: %v", err)
	}
	eventually(t, 5*time.Second, func() bool {
		return len(node1.Status().Servers) == 2
	})

	if _, err := node1.Apply(mustMarshalCommand(t, createVMCmd("vm-tls-1", "web-tls")), 5*time.Second); err != nil {
		t.Fatalf("Apply() error: %v", err)
	}

	eventually(t, 5*time.Second, func() bool {
		for _, vm := range node2.ListVMsLocal() {
			if vm.GetId() == "vm-tls-1" {
				return true
			}
		}
		return false
	})
}
