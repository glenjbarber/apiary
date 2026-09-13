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
