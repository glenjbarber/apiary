package raftdconfig

import (
	"os"
	"path/filepath"
	"testing"
)

func TestManager_LoadMissingFileReturnsDefaults(t *testing.T) {
	m := &Manager{Path: filepath.Join(t.TempDir(), "does-not-exist.json")}

	cfg, err := m.Load()
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	want := Defaults()
	if cfg != want {
		t.Errorf("Load() = %+v, want defaults %+v", cfg, want)
	}
}

func TestManager_LoadPartialFileOverlaysOnlyPresentFields(t *testing.T) {
	path := filepath.Join(t.TempDir(), "raftd.json")
	if err := os.WriteFile(path, []byte(`{"node_id": "apiverse", "internal_token": "shh"}`), 0o600); err != nil {
		t.Fatalf("writing test file: %v", err)
	}
	m := &Manager{Path: path}

	cfg, err := m.Load()
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	if cfg.NodeID != "apiverse" {
		t.Errorf("NodeID = %q, want apiverse", cfg.NodeID)
	}
	if cfg.InternalToken != "shh" {
		t.Errorf("InternalToken = %q, want shh", cfg.InternalToken)
	}
	if cfg.DataDir != Defaults().DataDir {
		t.Errorf("DataDir = %q, want the default %q since the file didn't set it", cfg.DataDir, Defaults().DataDir)
	}
	if cfg.RaftBind != Defaults().RaftBind {
		t.Errorf("RaftBind = %q, want the default %q since the file didn't set it", cfg.RaftBind, Defaults().RaftBind)
	}
}

func TestManager_LoadMalformedFileIsAnError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "raftd.json")
	if err := os.WriteFile(path, []byte(`{not valid json`), 0o600); err != nil {
		t.Fatalf("writing test file: %v", err)
	}
	m := &Manager{Path: path}

	if _, err := m.Load(); err == nil {
		t.Error("Load() error = nil, want an error for malformed JSON")
	}
}

// TestManager_SaveThenLoadRoundTripsEveryField confirms Save itself
// faithfully persists every field it's given - it has no notion of
// which fields any future caller might consider hand-edit-only
// (NodeID, DataDir, Socket, RaftBind, Join, AwaitJoin); that's a
// caller's responsibility, not Save's, and Save must never silently
// drop a field on its own.
func TestManager_SaveThenLoadRoundTripsEveryField(t *testing.T) {
	m := &Manager{Path: filepath.Join(t.TempDir(), "raftd.json")}
	want := Config{
		DataDir:       "/var/db/apiary/raftd",
		Socket:        "/var/run/apiary/raftd.sock",
		NodeID:        "apiverse",
		RaftBind:      "10.50.0.9:17701",
		Join:          "/var/run/apiary/raftd.sock",
		AwaitJoin:     true,
		InternalToken: "shh_test_token",
		RaftTLSCert:   "/usr/local/etc/apiary/tls/raft-cert.pem",
		RaftTLSKey:    "/usr/local/etc/apiary/tls/raft-key.pem",
		RaftTLSCA:     "/usr/local/etc/apiary/tls/raft-ca.pem",
	}

	if err := m.Save(want); err != nil {
		t.Fatalf("Save() error: %v", err)
	}
	got, err := m.Load()
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	if got != want {
		t.Errorf("round trip = %+v, want %+v", got, want)
	}
}

func TestManager_SaveRejectsMalformedFields(t *testing.T) {
	m := &Manager{Path: filepath.Join(t.TempDir(), "raftd.json")}
	for _, cfg := range []Config{
		{DataDir: "relative/path"},
		{Socket: "relative/path"},
		{RaftBind: "not-a-host-port"},
	} {
		if err := m.Save(cfg); err == nil {
			t.Errorf("Save(%+v) error = nil, want a validation rejection", cfg)
		}
	}
}

func TestManager_SaveRejectsNewlineInSecretOrTLSFields(t *testing.T) {
	m := &Manager{Path: filepath.Join(t.TempDir(), "raftd.json")}
	for _, cfg := range []Config{
		{InternalToken: "shh\ninjected"},
		{RaftTLSCert: "/a.pem\ninjected"},
	} {
		if err := m.Save(cfg); err == nil {
			t.Errorf("Save(%+v) error = nil, want a newline rejection", cfg)
		}
	}
}

func TestManager_SaveIsFullReplaceNotMerge(t *testing.T) {
	m := &Manager{Path: filepath.Join(t.TempDir(), "raftd.json")}
	if err := m.Save(Config{NodeID: "apiverse", InternalToken: "shh_first"}); err != nil {
		t.Fatalf("first Save() error: %v", err)
	}
	if err := m.Save(Config{NodeID: "apiverse"}); err != nil {
		t.Fatalf("second Save() error: %v", err)
	}
	got, err := m.Load()
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	if got.InternalToken != "" {
		t.Errorf("InternalToken = %q after a Save that omitted it, want empty - Save must fully replace, not merge", got.InternalToken)
	}
}

// TestManager_SaveTightensPermissionsOnExistingFile is the regression
// test for a 2026-09-15 audit finding - see the identical test in
// internal/frontendconfig for the full explanation. Especially
// important here: this file can hold InternalToken.
func TestManager_SaveTightensPermissionsOnExistingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "raftd.json")
	if err := os.WriteFile(path, []byte(`{}`), 0o644); err != nil {
		t.Fatalf("writing pre-existing file: %v", err)
	}
	m := &Manager{Path: path}

	if err := m.Save(Config{InternalToken: "shh"}); err != nil {
		t.Fatalf("Save() error: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat() error: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("permissions after Save = %o, want 0600 even though the file pre-existed at 0644", perm)
	}
}
