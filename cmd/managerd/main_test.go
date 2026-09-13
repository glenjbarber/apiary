package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/glenjbarber/apiary/internal/hostconfig"
	"github.com/glenjbarber/apiary/internal/nodeconfig"
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

// TestApplyManagerdDefaults_FillsOnlyZeroValueFields confirms
// applyManagerdDefaults (ADR-0100) fills a default only where the
// loaded config left a field at its zero value, and leaves an
// explicitly-set field untouched - the same "config file wins, then
// hardcoded default" precedence every flag used to have.
func TestApplyManagerdDefaults_FillsOnlyZeroValueFields(t *testing.T) {
	cfg := nodeconfig.Config{ZFSBase: "custom/pool", ReconcileInterval: 5 * time.Second}
	applyManagerdDefaults(&cfg)

	if cfg.ZFSBase != "custom/pool" {
		t.Errorf("ZFSBase = %q, want the explicitly-set value preserved", cfg.ZFSBase)
	}
	if cfg.ReconcileInterval != 5*time.Second {
		t.Errorf("ReconcileInterval = %s, want the explicitly-set value preserved", cfg.ReconcileInterval)
	}
	if cfg.RPCAddr != "127.0.0.1:17700" {
		t.Errorf("RPCAddr = %q, want the default since the config didn't set it", cfg.RPCAddr)
	}
	if cfg.RaftdSocket != "/var/run/apiary/raftd.sock" {
		t.Errorf("RaftdSocket = %q, want the default since the config didn't set it", cfg.RaftdSocket)
	}
	if cfg.BhyvePrefix != "apiary-" {
		t.Errorf("BhyvePrefix = %q, want the default since the config didn't set it", cfg.BhyvePrefix)
	}
	if cfg.JailDiskSizeMB != 2048 {
		t.Errorf("JailDiskSizeMB = %d, want the default 2048 since the config didn't set it", cfg.JailDiskSizeMB)
	}
	if cfg.OriginCARenewalCheckInterval != time.Hour {
		t.Errorf("OriginCARenewalCheckInterval = %s, want the default 1h since the config didn't set it", cfg.OriginCARenewalCheckInterval)
	}
}

// TestApplyManagerdDefaults_EmptyFieldsStayEmpty confirms fields whose
// former flag default was itself empty/disabled (BhyveBootROM, Uplink,
// TLSCert, PAMService) are never given a non-empty default - an empty
// managerd.json must behave exactly like every flag being left unset.
func TestApplyManagerdDefaults_EmptyFieldsStayEmpty(t *testing.T) {
	cfg := nodeconfig.Config{}
	applyManagerdDefaults(&cfg)

	if cfg.BhyveBootROM != "" || cfg.Uplink != "" || cfg.TLSCert != "" || cfg.PAMService != "" || cfg.NodeID != "" {
		t.Errorf("applyManagerdDefaults() = %+v, want these fields to stay empty (their own former flag default was empty)", cfg)
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
