package peerca

import (
	"crypto/x509"
	"encoding/pem"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	internalpb "github.com/glenjbarber/apiary/api/internalpb"
	"github.com/glenjbarber/apiary/internal/hostcert"
)

func testCert(t *testing.T, name string) *internalpb.TrustedPeer {
	t.Helper()
	now := time.Now().Truncate(time.Second)
	pem, _, err := hostcert.Generate(hostcert.GenerateOptions{
		CommonName:  name + ".lab3.home.arpa",
		DNSNames:    []string{name, name + ".lab3.home.arpa"},
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")},
		Now:         now,
	})
	if err != nil {
		t.Fatal(err)
	}
	info, err := hostcert.Evaluate(pem, hostcert.EvaluateOptions{Now: now, RequireDNSSAN: true, RequireLoopbackSAN: true})
	if err != nil {
		t.Fatal(err)
	}
	return &internalpb.TrustedPeer{
		NodeId: name, CombName: name, Fingerprint: info.Fingerprint, CertPem: string(pem),
		NotAfterUnix: info.NotAfter.Unix(), PinnedAtUnix: now.Unix(), PinnedBy: "brood", IsVoter: true,
	}
}

// The file's whole purpose is to be the same bytes on every Comb, so
// an operator can diff two hosts. Input order must therefore not
// matter - the writer sorts rather than trusting its caller to have.
func TestRenderIsIndependentOfInputOrder(t *testing.T) {
	brood, drone, sting := testCert(t, "brood"), testCert(t, "drone"), testCert(t, "sting")

	forward, err := Render([]*internalpb.TrustedPeer{brood, drone, sting})
	if err != nil {
		t.Fatal(err)
	}
	reverse, err := Render([]*internalpb.TrustedPeer{sting, drone, brood})
	if err != nil {
		t.Fatal(err)
	}
	if string(forward) != string(reverse) {
		t.Errorf("Render output depends on input order:\n%s\n---\n%s", forward, reverse)
	}

	// And the order is by node ID, so the bytes are inspectable rather
	// than merely stable. The names are inside the DER, not the PEM
	// armor, so this reads the subjects back out rather than searching
	// the text.
	got := commonNamesInOrder(t, forward)
	want := []string{"brood.lab3.home.arpa", "drone.lab3.home.arpa", "sting.lab3.home.arpa"}
	if len(got) != len(want) {
		t.Fatalf("the file holds %d certificates, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("certificate %d has CN %q, want %q - the file is not in node ID order", i, got[i], want[i])
		}
	}
}

// commonNamesInOrder parses a rendered bundle and returns each
// certificate's CN in file order.
func commonNamesInOrder(t *testing.T, body []byte) []string {
	t.Helper()
	var names []string
	rest := body
	for {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			return names
		}
		if block.Type != "CERTIFICATE" {
			continue
		}
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			t.Fatalf("the rendered file holds a block that does not parse as a certificate: %v", err)
		}
		names = append(names, cert.Subject.CommonName)
	}
}

// The file is a trust anchor, so it has to actually parse into a pool
// of the certificates it claims. A file that exists and is unusable is
// worse than a missing one, because LoadPeerCAPool's failure names a
// parse problem rather than a missing trust store.
func TestRenderedFileLoadsAsACertPoolOfThePinnedCertificates(t *testing.T) {
	brood, drone := testCert(t, "brood"), testCert(t, "drone")
	body, err := Render([]*internalpb.TrustedPeer{brood, drone})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "peer-ca.pem")
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(body) {
		t.Fatal("the rendered file did not parse as certificates")
	}
	for _, want := range []*internalpb.TrustedPeer{brood, drone} {
		if !strings.Contains(string(body), want.GetCertPem()) {
			t.Errorf("the file does not carry the certificate pinned for %s", want.GetNodeId())
		}
	}
}

// An entry with no certificate is a refusal, not a skip. Skipping it
// would write a file that silently omits a pin the operator believes is
// in the store, and the failure would surface later as a handshake
// against a host nobody can explain.
func TestRenderRefusesAPinWithNoCertificate(t *testing.T) {
	_, err := Render([]*internalpb.TrustedPeer{{NodeId: "brood"}})
	if err == nil {
		t.Fatal("Render accepted a pin carrying no certificate")
	}
	if !strings.Contains(err.Error(), "carries no certificate") || !strings.Contains(err.Error(), "brood") {
		t.Errorf("refusal %q does not say which pin is empty", err)
	}
}

// An empty store renders an empty file rather than an error. A Colony
// that has pinned nothing is a real state - a fresh install, or one
// whose members predate the store - and it is not an error to record.
func TestRenderOfAnEmptyStoreIsAnEmptyFile(t *testing.T) {
	body, err := Render(nil)
	if err != nil {
		t.Fatalf("Render(nil) error: %v", err)
	}
	if len(body) != 0 {
		t.Errorf("Render(nil) = %q, want an empty file", body)
	}
}

// 0600, and root-owned. This file is not a public certificate: it is
// the list of which peers this Colony has decided to trust, every entry
// of which an operator compared by hand.
func TestWriteIsAtomicAndPrivate(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("file modes are not the subject here")
	}
	dir := filepath.Join(t.TempDir(), "etc", "apiary")
	path := filepath.Join(dir, "peer-ca.pem")

	if err := Write(path, []*internalpb.TrustedPeer{testCert(t, "brood")}); err != nil {
		t.Fatalf("Write: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v, want 0600", info.Mode().Perm())
	}
	di, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if di.Mode().Perm() != 0o700 {
		t.Errorf("directory mode = %v, want 0700", di.Mode().Perm())
	}

	// No temp file left behind: a derived file that is rewritten on
	// every trust-store change would otherwise accumulate .peer-ca-*
	// files for as long as the Combs runs.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "peer-ca.pem" {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("directory holds %v, want only peer-ca.pem", names)
	}
}

// The file is a derived cache, so a rewrite that produces the same
// bytes is the normal case - managerd runs on a tick, and the trust
// store does not change between most ticks. Rewriting identical content
// must not fail, and must not leave the file absent.
func TestWriteOfUnchangedContentIsNotAFailure(t *testing.T) {
	path := filepath.Join(t.TempDir(), "peer-ca.pem")
	peers := []*internalpb.TrustedPeer{testCert(t, "brood")}
	for i := 0; i < 3; i++ {
		if err := Write(path, peers); err != nil {
			t.Fatalf("Write #%d: %v", i+1, err)
		}
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(body) == 0 {
		t.Error("the file is empty after repeated writes of a non-empty store")
	}
}
