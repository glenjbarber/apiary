package colonyca

import (
	"bytes"
	"crypto/x509"
	"encoding/pem"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// knownCAFingerprint pins the SHA-256 of testdata/known-ca.pem. It was
// computed independently with openssl (x509 -fingerprint -sha256 over
// the DER), so a change to Fingerprint's output format fails here.
const knownCAFingerprint = "2a3f3150cc026680d08a5e62484d04c033d92918bd3d7181f7fe3824617c0f27"

var testNow = time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)

func mustGenerateCA(t *testing.T, now time.Time) (dir string, certPEM, keyPEM []byte) {
	t.Helper()
	dir = t.TempDir()
	if _, err := GenerateCA(dir, now); err != nil {
		t.Fatalf("GenerateCA: %v", err)
	}
	certPEM = mustRead(t, filepath.Join(dir, CACertFile))
	keyPEM = mustRead(t, filepath.Join(dir, CAKeyFile))
	return dir, certPEM, keyPEM
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	return b
}

func parseCert(t *testing.T, certPEM []byte) *x509.Certificate {
	t.Helper()
	block, _ := pem.Decode(certPEM)
	if block == nil {
		t.Fatal("no PEM block")
	}
	c, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatalf("ParseCertificate: %v", err)
	}
	return c
}

func caPool(t *testing.T, caPEM []byte) *x509.CertPool {
	t.Helper()
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		t.Fatal("could not add CA to pool")
	}
	return pool
}

func TestLeafVerifiesForServerAuthAndClientAuth(t *testing.T) {
	_, caCert, caKey := mustGenerateCA(t, testNow)
	leafCert, _, err := IssueLeaf(caCert, caKey, LeafRequest{
		DNSNames: []string{"node1.colony.example"},
		IPs:      []net.IP{net.ParseIP("192.0.2.10")},
	}, testNow)
	if err != nil {
		t.Fatalf("IssueLeaf: %v", err)
	}
	leaf := parseCert(t, leafCert)
	for _, usage := range []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth} {
		_, err := leaf.Verify(x509.VerifyOptions{
			Roots:       caPool(t, caCert),
			CurrentTime: testNow,
			KeyUsages:   []x509.ExtKeyUsage{usage},
		})
		if err != nil {
			t.Errorf("leaf does not verify for usage %v: %v", usage, err)
		}
	}
	if _, err := leaf.Verify(x509.VerifyOptions{
		Roots:       caPool(t, caCert),
		CurrentTime: testNow,
		DNSName:     "node1.colony.example",
		KeyUsages:   []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}); err != nil {
		t.Errorf("leaf does not verify for its DNS name: %v", err)
	}
}

func TestLeafFromAnotherCADoesNotVerify(t *testing.T) {
	_, caCert, _ := mustGenerateCA(t, testNow)
	_, otherCert, otherKey := mustGenerateCA(t, testNow)
	leafCert, _, err := IssueLeaf(otherCert, otherKey, LeafRequest{DNSNames: []string{"node2"}}, testNow)
	if err != nil {
		t.Fatalf("IssueLeaf: %v", err)
	}
	leaf := parseCert(t, leafCert)
	for _, usage := range []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth} {
		if _, err := leaf.Verify(x509.VerifyOptions{
			Roots:       caPool(t, caCert),
			CurrentTime: testNow,
			KeyUsages:   []x509.ExtKeyUsage{usage},
		}); err == nil {
			t.Errorf("leaf from another CA verified against the Colony CA for usage %v", usage)
		}
	}
}

func TestGenerateCARefusesExistingFiles(t *testing.T) {
	cases := map[string][]string{
		"cert only": {CACertFile},
		"key only":  {CAKeyFile},
		"both":      {CACertFile, CAKeyFile},
	}
	for name, existing := range cases {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			contents := map[string][]byte{}
			for _, f := range existing {
				p := filepath.Join(dir, f)
				body := []byte("preexisting " + f + "\n")
				if err := os.WriteFile(p, body, 0o600); err != nil {
					t.Fatal(err)
				}
				contents[f] = body
			}
			_, err := GenerateCA(dir, testNow)
			if err == nil {
				t.Fatal("GenerateCA overwrote existing CA files")
			}
			for _, f := range existing {
				if !strings.Contains(err.Error(), filepath.Join(dir, f)) {
					t.Errorf("error %q does not name %s", err, filepath.Join(dir, f))
				}
			}
			for f, want := range contents {
				if got := mustRead(t, filepath.Join(dir, f)); !bytes.Equal(got, want) {
					t.Errorf("%s changed: got %q want %q", f, got, want)
				}
			}
			entries, err := os.ReadDir(dir)
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != len(existing) {
				t.Errorf("refusal left extra files in dir: %d entries, want %d", len(entries), len(existing))
			}
		})
	}
}

func TestGenerateCARefusesWhenBothFilesExistAndLeavesThemIdentical(t *testing.T) {
	dir, certBefore, keyBefore := mustGenerateCA(t, testNow)
	if _, err := GenerateCA(dir, testNow); err == nil {
		t.Fatal("second GenerateCA succeeded over an existing CA")
	}
	if got := mustRead(t, filepath.Join(dir, CACertFile)); !bytes.Equal(got, certBefore) {
		t.Error("ca.pem changed on refused GenerateCA")
	}
	if got := mustRead(t, filepath.Join(dir, CAKeyFile)); !bytes.Equal(got, keyBefore) {
		t.Error("ca.key changed on refused GenerateCA")
	}
}

func TestFileModes(t *testing.T) {
	dir, _, _ := mustGenerateCA(t, testNow)
	for file, want := range map[string]os.FileMode{CAKeyFile: 0o600, CACertFile: 0o644} {
		info, err := os.Stat(filepath.Join(dir, file))
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != want {
			t.Errorf("%s mode = %o, want %o", file, got, want)
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Errorf("temporary file left behind: %s", e.Name())
		}
	}
}

func TestCABasicConstraints(t *testing.T) {
	_, caCert, _ := mustGenerateCA(t, testNow)
	ca := parseCert(t, caCert)
	if !ca.BasicConstraintsValid || !ca.IsCA {
		t.Error("CA certificate is not CA:true")
	}
	if !ca.MaxPathLenZero || ca.MaxPathLen != 0 {
		t.Errorf("CA MaxPathLen = %d (zero flag %v), want 0", ca.MaxPathLen, ca.MaxPathLenZero)
	}
	if ca.KeyUsage != x509.KeyUsageCertSign|x509.KeyUsageCRLSign {
		t.Errorf("CA KeyUsage = %v, want CertSign|CRLSign", ca.KeyUsage)
	}
	if got := ca.NotAfter.Sub(testNow); got < 10*365*24*time.Hour-time.Hour || got > 10*365*24*time.Hour+time.Hour {
		t.Errorf("CA lifetime from now = %s, want about 10 years", got)
	}
}

func TestIssueLeafRefusesEmptyRequest(t *testing.T) {
	_, caCert, caKey := mustGenerateCA(t, testNow)
	for _, req := range []LeafRequest{{}, {DNSNames: []string{}, IPs: []net.IP{}}} {
		if _, _, err := IssueLeaf(caCert, caKey, req, testNow); err == nil {
			t.Errorf("IssueLeaf accepted empty request %+v", req)
		}
	}
}

func TestIssueLeafRefusesExpiredCA(t *testing.T) {
	// A CA generated 20 years ago expired 10 years ago.
	_, caCert, caKey := mustGenerateCA(t, testNow.AddDate(-20, 0, 0))
	_, _, err := IssueLeaf(caCert, caKey, LeafRequest{DNSNames: []string{"node1"}}, testNow)
	if err == nil || !strings.Contains(err.Error(), "expired") {
		t.Fatalf("IssueLeaf with expired CA: err = %v, want an expiry refusal", err)
	}
}

func TestIssueLeafRefusesNonCA(t *testing.T) {
	_, caCert, caKey := mustGenerateCA(t, testNow)
	leafCert, leafKey, err := IssueLeaf(caCert, caKey, LeafRequest{DNSNames: []string{"node1"}}, testNow)
	if err != nil {
		t.Fatalf("IssueLeaf: %v", err)
	}
	if _, _, err := IssueLeaf(leafCert, leafKey, LeafRequest{DNSNames: []string{"node2"}}, testNow); err == nil {
		t.Error("IssueLeaf accepted a leaf certificate as the CA")
	}
}

func TestIssueLeafRefusesLifetimeBeyondCA(t *testing.T) {
	_, caCert, caKey := mustGenerateCA(t, testNow)
	_, _, err := IssueLeaf(caCert, caKey, LeafRequest{DNSNames: []string{"node1"}, Lifetime: 11 * 365 * 24 * time.Hour}, testNow)
	if err == nil {
		t.Error("IssueLeaf issued a leaf that outlives the CA")
	}
}

func TestFingerprintPinnedValue(t *testing.T) {
	fixture := mustRead(t, filepath.Join("testdata", "known-ca.pem"))
	got, err := Fingerprint(fixture)
	if err != nil {
		t.Fatalf("Fingerprint: %v", err)
	}
	if got != knownCAFingerprint {
		t.Errorf("Fingerprint = %s, want %s", got, knownCAFingerprint)
	}
	again, err := Fingerprint(fixture)
	if err != nil || again != got {
		t.Errorf("Fingerprint not stable across calls: %s then %s (%v)", got, again, err)
	}
}

func TestFingerprintChangesWhenCAChanges(t *testing.T) {
	_, caCert, _ := mustGenerateCA(t, testNow)
	_, otherCert, _ := mustGenerateCA(t, testNow)
	a, err := Fingerprint(caCert)
	if err != nil {
		t.Fatal(err)
	}
	b, err := Fingerprint(otherCert)
	if err != nil {
		t.Fatal(err)
	}
	if a == b {
		t.Error("two different CAs have the same fingerprint")
	}
	if len(a) != 64 || strings.ContainsAny(a, ":ABCDEF") {
		t.Errorf("fingerprint %q is not 64 lowercase hex characters", a)
	}
}

func TestGenerateCAReturnsFingerprintOfWrittenCert(t *testing.T) {
	dir := t.TempDir()
	fp, err := GenerateCA(dir, testNow)
	if err != nil {
		t.Fatal(err)
	}
	want, err := Fingerprint(mustRead(t, filepath.Join(dir, CACertFile)))
	if err != nil {
		t.Fatal(err)
	}
	if fp != want {
		t.Errorf("GenerateCA fingerprint %s, want %s", fp, want)
	}
}
