package main

// ADR-0173: the joiner adopting a prepared certificate.
//
// What these pin: that a prepared pair for ANOTHER Comb is refused,
// that the installed pair is the CONFIGURED one (the paths this Comb
// serves from), that the write is atomic, and that the key is 0600.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/glenjbarber/apiary/internal/nodeconfig"
)

// A prepared certificate for another Comb is refused.
//
// The reservation names the FQDN, and this Comb's own configuration
// names its node ID. A certificate whose name does not contain this
// Comb's name was prepared for a DIFFERENT Comb, and installing it
// here would make this Comb serve a certificate for somebody else's
// name.
func TestCheckPreparedNameMatchesThisComb_RefusesAnotherCombsName(t *testing.T) {
	cfg := nodeconfig.Config{NodeID: "brood"}
	err := checkPreparedNameMatchesThisComb(cfg, "drone.lab2.apiary.work")
	if err == nil {
		t.Fatal("a prepared certificate for another Comb was accepted")
	}
	if !strings.Contains(err.Error(), "not this Comb's name") {
		t.Errorf("the refusal %q does not say the name is not this Comb's", err)
	}
}

// A prepared certificate for THIS Comb is accepted.
func TestCheckPreparedNameMatchesThisComb_AcceptsThisCombsName(t *testing.T) {
	cfg := nodeconfig.Config{NodeID: "brood"}
	if err := checkPreparedNameMatchesThisComb(cfg, "brood.lab2.apiary.work"); err != nil {
		t.Errorf("a prepared certificate for this Comb was refused: %v", err)
	}
}

// A Comb with no node ID cannot have its name checked, and a check
// that cannot be made is not a passing check.
func TestCheckPreparedNameMatchesThisComb_RefusesWithoutANodeID(t *testing.T) {
	cfg := nodeconfig.Config{}
	err := checkPreparedNameMatchesThisComb(cfg, "brood.lab2.apiary.work")
	if err == nil {
		t.Fatal("a prepared certificate was accepted with no node ID to check it against")
	}
	if !strings.Contains(err.Error(), "node_id") {
		t.Errorf("the refusal %q does not name the node_id", err)
	}
}

// The pair is written to the CONFIGURED paths, because that is where
// this Comb's services read their serving certificate from. A pair
// written somewhere else would be a certificate nothing serves.
func TestInstallPreparedPairAt_WritesTheConfiguredPaths(t *testing.T) {
	dir := t.TempDir()
	certPath := filepath.Join(dir, "tls", "cert.pem")
	keyPath := filepath.Join(dir, "tls", "key.pem")

	if err := installPreparedPairAt(certPath, keyPath, "THE-CERT", "THE-KEY"); err != nil {
		t.Fatalf("installPreparedPairAt: %v", err)
	}
	cert, err := os.ReadFile(certPath)
	if err != nil {
		t.Fatalf("reading the installed certificate: %v", err)
	}
	if string(cert) != "THE-CERT" {
		t.Errorf("the installed certificate is %q, want THE-CERT", string(cert))
	}
	key, err := os.ReadFile(keyPath)
	if err != nil {
		t.Fatalf("reading the installed key: %v", err)
	}
	if string(key) != "THE-KEY" {
		t.Errorf("the installed key is %q, want THE-KEY", string(key))
	}
}

// The private key is 0600 and the certificate is 0644.
//
// The key's mode is the security property: a world-readable private
// key is a private key anyone on the host can use.
func TestInstallPreparedPairAt_SetsTheModes(t *testing.T) {
	dir := t.TempDir()
	certPath := filepath.Join(dir, "cert.pem")
	keyPath := filepath.Join(dir, "key.pem")

	if err := installPreparedPairAt(certPath, keyPath, "c", "k"); err != nil {
		t.Fatalf("installPreparedPairAt: %v", err)
	}
	certInfo, err := os.Stat(certPath)
	if err != nil {
		t.Fatal(err)
	}
	if mode := certInfo.Mode().Perm(); mode != 0o644 {
		t.Errorf("the certificate's mode is %04o, want 0644", mode)
	}
	keyInfo, err := os.Stat(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	if mode := keyInfo.Mode().Perm(); mode != 0o600 {
		t.Errorf("the private key's mode is %04o, want 0600", mode)
	}
}

// The write leaves no staging file behind, and it does not damage an
// existing file when it fails.
//
// A failure partway through must leave the PREVIOUS pair serving, not
// a half-written one - that is the whole reason the write is staged
// and renamed.
func TestWriteFileAtomic_LeavesNoStagingFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cert.pem")
	if err := writeFileAtomic(path, []byte("content"), 0o644); err != nil {
		t.Fatalf("writeFileAtomic: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("the directory holds %v, want only the written file", names)
	}
}

// An existing file is REPLACED, and the replacement is complete.
//
// The rename is what makes it atomic: a reader sees either the old
// file or the new one.
func TestWriteFileAtomic_ReplacesAnExistingFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cert.pem")
	if err := os.WriteFile(path, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := writeFileAtomic(path, []byte("new"), 0o644); err != nil {
		t.Fatalf("writeFileAtomic: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "new" {
		t.Errorf("the file holds %q, want the replacement", string(data))
	}
}

// The write tightens a loose mode on an existing file.
//
// A file that was already 0644 would otherwise keep that mode, and a
// private key that was once readable would stay readable - which
// breaks the promise silently.
func TestWriteFileAtomic_TightensALooseMode(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "key.pem")
	if err := os.WriteFile(path, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := writeFileAtomic(path, []byte("new"), 0o600); err != nil {
		t.Fatalf("writeFileAtomic: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Errorf("the file's mode is %04o, want it tightened to 0600", mode)
	}
}
