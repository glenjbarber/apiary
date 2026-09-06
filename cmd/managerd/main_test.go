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
