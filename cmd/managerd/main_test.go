package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/glenjbarber/apiary/internal/hostconfig"
)

func TestRunExportHostConfig_WritesRedactedFiles(t *testing.T) {
	srcDir := t.TempDir()
	outDir := filepath.Join(t.TempDir(), "out")

	rcConfPath := filepath.Join(srcDir, "rc.conf")
	pfConfPath := filepath.Join(srcDir, "pf.conf")
	masterPasswdPath := filepath.Join(srcDir, "master.passwd")
	if err := os.WriteFile(rcConfPath, []byte(`apiary_managerd_args="-peer-api-key apk_realsecret"`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pfConfPath, []byte(`anchor "apiary/*"`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(masterPasswdPath, []byte("root:$6$realhash:0:0:Charlie &:/root:/bin/csh\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	files := hostconfig.Files{RcConfPath: rcConfPath, PfConfPath: pfConfPath, MasterPasswdPath: masterPasswdPath}
	if err := runExportHostConfig(files, outDir); err != nil {
		t.Fatalf("runExportHostConfig() error: %v", err)
	}

	rcOut, err := os.ReadFile(filepath.Join(outDir, "rc.conf.redacted"))
	if err != nil {
		t.Fatalf("reading rc.conf.redacted: %v", err)
	}
	if strings.Contains(string(rcOut), "apk_realsecret") {
		t.Errorf("exported rc.conf still contains the real secret: %s", rcOut)
	}
}

func TestRunExportHostConfig_MissingSourceIsAnError(t *testing.T) {
	files := hostconfig.Files{
		RcConfPath:       filepath.Join(t.TempDir(), "missing"),
		PfConfPath:       filepath.Join(t.TempDir(), "missing"),
		MasterPasswdPath: filepath.Join(t.TempDir(), "missing"),
	}
	if err := runExportHostConfig(files, filepath.Join(t.TempDir(), "out")); err == nil {
		t.Fatal("runExportHostConfig() with a missing source file = nil error, want one")
	}
}

func TestRunReset_WrongResetManagedPhraseDoesNothing(t *testing.T) {
	if err := runReset("not-the-phrase", "", "", "", "zroot/apiary", "apiary-", "apiary-", "/tmp/isos"); err == nil {
		t.Fatal("runReset() with wrong -reset-managed phrase = nil error, want a rejection")
	}
}

func TestRunReset_WrongFactoryResetPhraseDoesNothing(t *testing.T) {
	if err := runReset("", "not-the-phrase", "", "", "zroot/apiary", "apiary-", "apiary-", "/tmp/isos"); err == nil {
		t.Fatal("runReset() with wrong -factory-reset phrase = nil error, want a rejection")
	}
}

func TestRunReset_BothEmptyIsANoOp(t *testing.T) {
	if err := runReset("", "", "", "", "zroot/apiary", "apiary-", "apiary-", "/tmp/isos"); err != nil {
		t.Fatalf("runReset() with neither flag set = %v, want nil (this path shouldn't even be reached in main, but must be harmless)", err)
	}
}

// TestResolvePeerAPIKey_FileTakesPrecedenceAndTrimsWhitespace confirms
// -peer-api-key-file (ADR-0096) is read and trimmed correctly - the
// preferred way to configure -peer-api-key, since a value passed
// directly on the command line is visible to any local user via
// ps(1)/procstat(1) regardless of /etc/rc.conf's own permissions.
func TestResolvePeerAPIKey_FileTakesPrecedenceAndTrimsWhitespace(t *testing.T) {
	path := filepath.Join(t.TempDir(), "peer-api-key")
	if err := os.WriteFile(path, []byte("apk_realsecret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := resolvePeerAPIKey("", path)
	if err != nil {
		t.Fatalf("resolvePeerAPIKey() error: %v", err)
	}
	if got != "apk_realsecret" {
		t.Errorf("resolvePeerAPIKey() = %q, want %q (trailing newline trimmed)", got, "apk_realsecret")
	}
}

func TestResolvePeerAPIKey_NoFileReturnsFlagValue(t *testing.T) {
	got, err := resolvePeerAPIKey("apk_fromflag", "")
	if err != nil {
		t.Fatalf("resolvePeerAPIKey() error: %v", err)
	}
	if got != "apk_fromflag" {
		t.Errorf("resolvePeerAPIKey() = %q, want %q", got, "apk_fromflag")
	}
}

func TestResolvePeerAPIKey_BothSetIsAnError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "peer-api-key")
	if err := os.WriteFile(path, []byte("apk_realsecret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := resolvePeerAPIKey("apk_fromflag", path); err == nil {
		t.Fatal("resolvePeerAPIKey() with both -peer-api-key and -peer-api-key-file set = nil error, want a rejection")
	}
}

func TestResolvePeerAPIKey_MissingFileIsAnError(t *testing.T) {
	if _, err := resolvePeerAPIKey("", filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("resolvePeerAPIKey() with a missing -peer-api-key-file = nil error, want one")
	}
}

func TestSplitCommaList(t *testing.T) {
	cases := map[string][]string{
		"":         nil,
		"a":        {"a"},
		"a,b,c":    {"a", "b", "c"},
		"a, b , c": {"a", "b", "c"},
		"a,,b":     {"a", "b"},
		"  ,  ,  ": nil,
	}
	for in, want := range cases {
		got := splitCommaList(in)
		if len(got) != len(want) {
			t.Errorf("splitCommaList(%q) = %v, want %v", in, got, want)
			continue
		}
		for i := range got {
			if got[i] != want[i] {
				t.Errorf("splitCommaList(%q) = %v, want %v", in, got, want)
				break
			}
		}
	}
}
