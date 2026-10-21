package main

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

	"github.com/glenjbarber/apiary/internal/colonyca"
)

// colonyCATestNow is fixed so the CA and leaf windows are predictable.
var colonyCATestNow = time.Now()

func readOrFail(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	return b
}

func modeOf(t *testing.T, path string) os.FileMode {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info.Mode().Perm()
}

func outputOf(t *testing.T, f *os.File) string {
	t.Helper()
	return string(readOrFail(t, f.Name()))
}

func TestColonyCAInit_CreatesCAAndReportsFingerprint(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "ca")
	out, errOut := tempOutputFiles(t)
	defer out.Close()
	defer errOut.Close()

	if code := colonyCAInit(dir, out, errOut); code != 0 {
		t.Fatalf("colonyCAInit() = %d, want 0; stderr: %s", code, outputOf(t, errOut))
	}
	certPEM := readOrFail(t, filepath.Join(dir, colonyca.CACertFile))
	if modeOf(t, filepath.Join(dir, colonyca.CAKeyFile)) != 0o600 {
		t.Errorf("ca.key mode = %04o, want 0600", modeOf(t, filepath.Join(dir, colonyca.CAKeyFile)))
	}
	want, err := colonyca.Fingerprint(certPEM)
	if err != nil {
		t.Fatal(err)
	}
	if got := outputOf(t, out); !strings.Contains(got, want) {
		t.Errorf("output does not carry the CA fingerprint %s:\n%s", want, got)
	}
}

// TestColonyCAInit_RefusesExistingCAAndLeavesItByteIdentical is the
// direct guard on ADR-0155's no-overwrite rule: a second init must
// fail and must not change either file.
func TestColonyCAInit_RefusesExistingCAAndLeavesItByteIdentical(t *testing.T) {
	dir := t.TempDir()
	out, errOut := tempOutputFiles(t)
	defer out.Close()
	defer errOut.Close()
	if code := colonyCAInit(dir, out, errOut); code != 0 {
		t.Fatalf("first colonyCAInit() = %d, want 0", code)
	}
	certPath := filepath.Join(dir, colonyca.CACertFile)
	keyPath := filepath.Join(dir, colonyca.CAKeyFile)
	certBefore := readOrFail(t, certPath)
	keyBefore := readOrFail(t, keyPath)

	out2, errOut2 := tempOutputFiles(t)
	defer out2.Close()
	defer errOut2.Close()
	if code := colonyCAInit(dir, out2, errOut2); code == 0 {
		t.Fatal("second colonyCAInit() = 0, want non-zero when a CA exists")
	}
	if got := readOrFail(t, certPath); !bytes.Equal(got, certBefore) {
		t.Error("ca.pem changed on refused init")
	}
	if got := readOrFail(t, keyPath); !bytes.Equal(got, keyBefore) {
		t.Error("ca.key changed on refused init")
	}
}

// TestColonyCAInit_RefusesWhenOnlyKeyExists covers the half-present
// case: a key without its certificate is still an existing CA and must
// not be replaced.
func TestColonyCAInit_RefusesWhenOnlyKeyExists(t *testing.T) {
	dir := t.TempDir()
	keyPath := filepath.Join(dir, colonyca.CAKeyFile)
	if err := os.WriteFile(keyPath, []byte("existing key\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	out, errOut := tempOutputFiles(t)
	defer out.Close()
	defer errOut.Close()
	if code := colonyCAInit(dir, out, errOut); code == 0 {
		t.Fatal("colonyCAInit() = 0, want non-zero when ca.key exists")
	}
	if got := readOrFail(t, keyPath); string(got) != "existing key\n" {
		t.Errorf("ca.key changed: %q", got)
	}
	if _, err := os.Stat(filepath.Join(dir, colonyca.CACertFile)); !os.IsNotExist(err) {
		t.Error("ca.pem was created on a refused init")
	}
}

func TestColonyCAIssueRaft_RefusesWithoutCA(t *testing.T) {
	caDir := t.TempDir() // no CA here
	outDir := t.TempDir()
	out, errOut := tempOutputFiles(t)
	defer out.Close()
	defer errOut.Close()

	code := colonyCAIssueRaft(caDir, outDir, []string{"node1"}, nil, colonyCATestNow, out, errOut)
	if code == 0 {
		t.Fatal("colonyCAIssueRaft() = 0, want non-zero without a CA")
	}
	for _, name := range []string{raftCertFile, raftKeyFile} {
		if _, err := os.Stat(filepath.Join(outDir, name)); !os.IsNotExist(err) {
			t.Errorf("%s was written without a CA", name)
		}
	}
}

// issuedRaftFixture creates a CA in caDir and a raft leaf in outDir.
func issuedRaftFixture(t *testing.T, names []string, ips []net.IP) (caDir, outDir string) {
	t.Helper()
	caDir = t.TempDir()
	outDir = t.TempDir()
	out, errOut := tempOutputFiles(t)
	defer out.Close()
	defer errOut.Close()
	if code := colonyCAInit(caDir, out, errOut); code != 0 {
		t.Fatalf("colonyCAInit() = %d", code)
	}
	out2, errOut2 := tempOutputFiles(t)
	defer out2.Close()
	defer errOut2.Close()
	if code := colonyCAIssueRaft(caDir, outDir, names, ips, colonyCATestNow, out2, errOut2); code != 0 {
		t.Fatalf("colonyCAIssueRaft() = %d; stderr: %s", code, outputOf(t, errOut2))
	}
	return caDir, outDir
}

// TestColonyCAIssueRaft_LeafVerifiesForServerAndClientAuth checks the
// ExtKeyUsage requirement: raft's mutual TLS needs both usages, from
// the file the command actually wrote.
func TestColonyCAIssueRaft_LeafVerifiesForServerAndClientAuth(t *testing.T) {
	caDir, outDir := issuedRaftFixture(t, []string{"node1.colony.example"}, []net.IP{net.ParseIP("192.0.2.10")})

	caPEM := readOrFail(t, filepath.Join(caDir, colonyca.CACertFile))
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		t.Fatal("CA not loadable into a pool")
	}
	leafPEM := readOrFail(t, filepath.Join(outDir, raftCertFile))
	block, _ := pem.Decode(leafPEM)
	if block == nil {
		t.Fatal("raft.pem has no PEM block")
	}
	leaf, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	for _, usage := range []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth} {
		if _, err := leaf.Verify(x509.VerifyOptions{Roots: pool, CurrentTime: colonyCATestNow, KeyUsages: []x509.ExtKeyUsage{usage}, DNSName: "node1.colony.example"}); err != nil {
			t.Errorf("raft.pem does not verify for usage %v: %v", usage, err)
		}
	}
	if modeOf(t, filepath.Join(outDir, raftKeyFile)) != 0o600 {
		t.Errorf("raft.key mode = %04o, want 0600", modeOf(t, filepath.Join(outDir, raftKeyFile)))
	}
	if modeOf(t, filepath.Join(outDir, raftCertFile)) != 0o644 {
		t.Errorf("raft.pem mode = %04o, want 0644", modeOf(t, filepath.Join(outDir, raftCertFile)))
	}
}

func TestColonyCAIssueRaft_ReportsFingerprintAndExpiry(t *testing.T) {
	caDir := t.TempDir()
	out0, errOut0 := tempOutputFiles(t)
	defer out0.Close()
	defer errOut0.Close()
	if code := colonyCAInit(caDir, out0, errOut0); code != 0 {
		t.Fatalf("colonyCAInit() = %d", code)
	}
	outDir := t.TempDir()
	out, errOut := tempOutputFiles(t)
	defer out.Close()
	defer errOut.Close()
	if code := colonyCAIssueRaft(caDir, outDir, []string{"node1"}, nil, colonyCATestNow, out, errOut); code != 0 {
		t.Fatalf("colonyCAIssueRaft() = %d", code)
	}
	fp, err := colonyca.Fingerprint(readOrFail(t, filepath.Join(outDir, raftCertFile)))
	if err != nil {
		t.Fatal(err)
	}
	got := outputOf(t, out)
	if !strings.Contains(got, fp) {
		t.Errorf("output does not carry the leaf fingerprint %s:\n%s", fp, got)
	}
	if !strings.Contains(got, "not after:") {
		t.Errorf("output does not report the leaf expiry:\n%s", got)
	}
}

// TestColonyCAIssueRaft_RefusesToOverwrite covers both halves: an
// existing raft.pem and an existing raft.key each stop the command, and
// neither file is changed or joined by a new one.
func TestColonyCAIssueRaft_RefusesToOverwrite(t *testing.T) {
	for _, name := range []string{raftCertFile, raftKeyFile} {
		t.Run(name, func(t *testing.T) {
			caDir := t.TempDir()
			out0, errOut0 := tempOutputFiles(t)
			defer out0.Close()
			defer errOut0.Close()
			if code := colonyCAInit(caDir, out0, errOut0); code != 0 {
				t.Fatalf("colonyCAInit() = %d", code)
			}
			outDir := t.TempDir()
			existing := filepath.Join(outDir, name)
			if err := os.WriteFile(existing, []byte("keep me\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			out, errOut := tempOutputFiles(t)
			defer out.Close()
			defer errOut.Close()
			if code := colonyCAIssueRaft(caDir, outDir, []string{"node1"}, nil, colonyCATestNow, out, errOut); code == 0 {
				t.Fatal("colonyCAIssueRaft() = 0, want non-zero when a file already exists")
			}
			if got := readOrFail(t, existing); string(got) != "keep me\n" {
				t.Errorf("%s changed: %q", name, got)
			}
			other := raftKeyFile
			if name == raftKeyFile {
				other = raftCertFile
			}
			if _, err := os.Stat(filepath.Join(outDir, other)); !os.IsNotExist(err) {
				t.Errorf("%s was written on a refused issue", other)
			}
		})
	}
}

func TestColonyCAIssueRaft_RejectsBadIPBeforeRootCheck(t *testing.T) {
	if code := runColonyCAIssueRaft([]string{"--name", "node1", "--ip", "not-an-ip"}); code != 2 {
		t.Errorf("runColonyCAIssueRaft(bad ip) = %d, want 2", code)
	}
}

func TestColonyCAIssueRaft_RequiresAName(t *testing.T) {
	if code := runColonyCAIssueRaft(nil); code != 2 {
		t.Errorf("runColonyCAIssueRaft(no names) = %d, want 2", code)
	}
}

func TestColonyCAUnknownSubcommand(t *testing.T) {
	if code := runColonyCA([]string{"rotate"}); code != 2 {
		t.Errorf("runColonyCA(rotate) = %d, want 2", code)
	}
}
