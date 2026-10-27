package colonyleafinstall

import (
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/glenjbarber/apiary/internal/colonyca"
	"github.com/glenjbarber/apiary/internal/colonyleaf"
	"github.com/glenjbarber/apiary/internal/hostcert"
	"github.com/glenjbarber/apiary/internal/raftdconfig"
)

func testCA(t *testing.T) (dir string, fingerprint string) {
	t.Helper()
	dir = t.TempDir()
	fingerprint, err := colonyca.GenerateCA(dir, time.Now())
	if err != nil {
		t.Fatalf("generating a test CA: %v", err)
	}
	return dir, fingerprint
}

func testRecipient(t *testing.T) (certPEM, keyPEM string) {
	t.Helper()
	cert, key, err := hostcert.Generate(hostcert.GenerateOptions{
		CommonName:  "comb.lab3.apiary.work",
		DNSNames:    []string{"comb.lab3.apiary.work"},
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")},
	})
	if err != nil {
		t.Fatalf("generating a test serving certificate: %v", err)
	}
	return string(cert), string(key)
}

// issueLeaf issues a leaf for the test Comb and returns it opened.
//
// The serving certificate is generated ONCE and used for both the
// seal and the open, because the seal is to that certificate: opening
// with a different one cannot work, and that is the property the
// other test checks.
func issueLeaf(t *testing.T, caDir, caFingerprint string) (*colonyleaf.Leaf, string, string) {
	t.Helper()
	recipientCertPEM, recipientKey := testRecipient(t)
	sealed, _, err := colonyleaf.Issue(colonyleaf.IssueOptions{
		CADir:                   caDir,
		NodeID:                  "comb.lab3.apiary.work",
		RaftBindAddress:         "comb.lab3.apiary.work:17600",
		RequestID:               "jreq-test",
		ExpectedCAFingerprint:   caFingerprint,
		RecipientCertificatePEM: recipientCertPEM,
		Now:                     time.Now(),
	})
	if err != nil {
		t.Fatalf("issuing a test leaf: %v", err)
	}
	leaf, err := colonyleaf.Open(sealed, []byte(recipientCertPEM), []byte(recipientKey), "jreq-test", "comb.lab3.apiary.work")
	if err != nil {
		t.Fatalf("opening the test leaf: %v", err)
	}
	return leaf, recipientCertPEM, recipientKey
}

// TestInstallWritesTheLeafAndItsKey is the happy path.
func TestInstallWritesTheLeafAndItsKey(t *testing.T) {
	caDir, caFingerprint := testCA(t)
	leaf, _, _ := issueLeaf(t, caDir, caFingerprint)

	dir := t.TempDir()
	certPath := filepath.Join(dir, "raft.pem")
	keyPath := filepath.Join(dir, "raft.key")
	if err := Install(InstallOptions{
		Leaf: leaf, NodeID: "comb.lab3.apiary.work",
		CertPath: certPath, KeyPath: keyPath, CADir: caDir, Now: time.Now(),
	}); err != nil {
		t.Fatalf("Install() error = %v", err)
	}
	if _, err := os.Stat(certPath); err != nil {
		t.Errorf("the leaf certificate was not written: %v", err)
	}
	info, err := os.Stat(keyPath)
	if err != nil {
		t.Fatalf("the leaf key was not written: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("the leaf key's mode is %v, want 0600", info.Mode().Perm())
	}
}

// TestInstallRefusesALeafThatDoesNotChainToTheCA is the property
// that keeps a joiner from installing a certificate no peer can
// verify.
func TestInstallRefusesALeafThatDoesNotChainToTheCA(t *testing.T) {
	caDir, caFingerprint := testCA(t)
	leaf, _, _ := issueLeaf(t, caDir, caFingerprint)

	// A DIFFERENT CA, so the leaf does not chain to this one.
	otherCADir, _ := testCA(t)
	dir := t.TempDir()
	err := Install(InstallOptions{
		Leaf: leaf, NodeID: "comb.lab3.apiary.work",
		CertPath: filepath.Join(dir, "raft.pem"), KeyPath: filepath.Join(dir, "raft.key"),
		CADir: otherCADir, Now: time.Now(),
	})
	if err == nil {
		t.Fatal("Install() accepted a leaf that does not chain to the Colony CA")
	}
	if _, statErr := os.Stat(filepath.Join(dir, "raft.pem")); statErr == nil {
		t.Error("a leaf was written even though it did not chain to the CA")
	}
}

// TestInstallRefusesALeafForAnotherComb: a joiner must not install a
// leaf issued for a different machine.
func TestInstallRefusesALeafForAnotherComb(t *testing.T) {
	caDir, caFingerprint := testCA(t)
	leaf, _, _ := issueLeaf(t, caDir, caFingerprint)

	dir := t.TempDir()
	err := Install(InstallOptions{
		Leaf: leaf, NodeID: "other.lab3.apiary.work",
		CertPath: filepath.Join(dir, "raft.pem"), KeyPath: filepath.Join(dir, "raft.key"),
		CADir: caDir, Now: time.Now(),
	})
	if err == nil {
		t.Fatal("Install() accepted a leaf issued for another Comb")
	}
	if !strings.Contains(err.Error(), "comb.lab3.apiary.work") {
		t.Errorf("refusal %q does not name the name the leaf was issued for", err)
	}
}

// TestInstallNeverReplacesAnExistingLeaf: replacing a raft
// transport certificate silently would invalidate every leaf the
// Colony holds for that Comb.
func TestInstallNeverReplacesAnExistingLeaf(t *testing.T) {
	caDir, caFingerprint := testCA(t)
	leaf, _, _ := issueLeaf(t, caDir, caFingerprint)

	dir := t.TempDir()
	certPath := filepath.Join(dir, "raft.pem")
	keyPath := filepath.Join(dir, "raft.key")
	if err := Install(InstallOptions{
		Leaf: leaf, NodeID: "comb.lab3.apiary.work",
		CertPath: certPath, KeyPath: keyPath, CADir: caDir, Now: time.Now(),
	}); err != nil {
		t.Fatalf("the first Install() error = %v", err)
	}
	original, _ := os.ReadFile(certPath)
	err := Install(InstallOptions{
		Leaf: leaf, NodeID: "comb.lab3.apiary.work",
		CertPath: certPath, KeyPath: keyPath, CADir: caDir, Now: time.Now(),
	})
	if err == nil {
		t.Fatal("Install() replaced an existing raft transport leaf")
	}
	after, _ := os.ReadFile(certPath)
	if string(after) != string(original) {
		t.Error("the existing leaf was modified despite the refusal")
	}
}

// TestPointRaftdAtTheCAConfiguresTheTransport is the step that makes
// the installed leaf actually used.
func TestPointRaftdAtTheCAConfiguresTheTransport(t *testing.T) {
	caDir, _ := testCA(t)
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "raftd.json")
	// A minimal raftd.json.
	if err := os.WriteFile(cfgPath, []byte(`{"node_id":"comb.lab3.apiary.work","raft_bind":"comb.lab3.apiary.work:17600"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	certPath := filepath.Join(dir, "raft.pem")
	keyPath := filepath.Join(dir, "raft.key")
	if err := PointRaftdAtTheCA(cfgPath, caDir, certPath, keyPath); err != nil {
		t.Fatalf("PointRaftdAtTheCA() error = %v", err)
	}
	cfg, err := (&raftdconfig.Manager{Path: cfgPath}).Load()
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.RaftTLSOn() {
		t.Error("the raft transport was not turned on")
	}
	if cfg.RaftTLSCert != certPath || cfg.RaftTLSKey != keyPath {
		t.Errorf("the leaf paths are %q and %q, want %q and %q", cfg.RaftTLSCert, cfg.RaftTLSKey, certPath, keyPath)
	}
	if cfg.RaftTLSCA != filepath.Join(caDir, colonyca.CACertFile) {
		t.Errorf("the CA path is %q, want the Colony CA", cfg.RaftTLSCA)
	}
}

// TestPointRaftdAtTheCALeavesAnExistingConfigurationAlone: a Comb
// that already has a raft transport certificate keeps it.
func TestPointRaftdAtTheCALeavesAnExistingConfigurationAlone(t *testing.T) {
	caDir, _ := testCA(t)
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "raftd.json")
	existing := `{"node_id":"comb.lab3.apiary.work","raft_bind":"comb.lab3.apiary.work:17600","raft_tls_cert":"/usr/local/etc/apiary/raft.pem","raft_tls_key":"/usr/local/etc/apiary/raft.key","raft_tls_ca":"/usr/local/etc/apiary/ca/ca.pem"}`
	if err := os.WriteFile(cfgPath, []byte(existing), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := PointRaftdAtTheCA(cfgPath, caDir, filepath.Join(dir, "raft.pem"), filepath.Join(dir, "raft.key")); err != nil {
		t.Fatalf("PointRaftdAtTheCA() error = %v", err)
	}
	cfg, err := (&raftdconfig.Manager{Path: cfgPath}).Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.RaftTLSCert != "/usr/local/etc/apiary/raft.pem" {
		t.Errorf("the existing raft transport certificate was replaced with %q", cfg.RaftTLSCert)
	}
}
