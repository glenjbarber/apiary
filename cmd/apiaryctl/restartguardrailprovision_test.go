package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestRestartGuardrailProvision_GeneratesWhenAbsent is the normal case
// issue #98's requirement 4 exists for: a Colony with no token anywhere
// gets one on the Comb this command is run on.
func TestRestartGuardrailProvision_GeneratesWhenAbsent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "restart-guardrail-token")

	outFile, errFile := tempOutputFiles(t)
	defer outFile.Close()
	defer errFile.Close()

	code := restartGuardrailProvision(path, outFile, errFile)
	if code != 0 {
		t.Fatalf("restartGuardrailProvision() = %d, want 0", code)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading generated token: %v", err)
	}
	if strings.TrimSpace(string(data)) == "" {
		t.Error("generated token file is empty")
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("generated token mode = %04o, want 0600", info.Mode().Perm())
	}
}

// TestRestartGuardrailProvision_RefusesWhenAlreadyPresent is the direct
// regression test for this command's one hard rule: it must never
// overwrite a credential that is already there, exactly like
// planRestartGuardrailToken's own ActionKeep case.
func TestRestartGuardrailProvision_RefusesWhenAlreadyPresent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "restart-guardrail-token")
	if err := os.WriteFile(path, []byte("already-here\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	outFile, errFile := tempOutputFiles(t)
	defer outFile.Close()
	defer errFile.Close()

	code := restartGuardrailProvision(path, outFile, errFile)
	if code == 0 {
		t.Fatal("restartGuardrailProvision() = 0, want non-zero when a token already exists")
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "already-here\n" {
		t.Errorf("existing token was changed: %q", string(data))
	}
}

// tempOutputFiles gives restartGuardrailProvision real *os.File values
// to write to (its signature takes *os.File, matching os.Stdout/
// os.Stderr in production) backed by temp files a test can read back.
func tempOutputFiles(t *testing.T) (out, errOut *os.File) {
	t.Helper()
	dir := t.TempDir()
	out, err := os.Create(filepath.Join(dir, "out"))
	if err != nil {
		t.Fatal(err)
	}
	errOut, err = os.Create(filepath.Join(dir, "err"))
	if err != nil {
		t.Fatal(err)
	}
	return out, errOut
}
