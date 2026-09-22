package commonconfig

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadMissingFile(t *testing.T) {
	m := &Manager{Path: filepath.Join(t.TempDir(), "common.json")}
	cfg, err := m.Load()
	if err != nil {
		t.Fatalf("Load: unexpected error: %v", err)
	}
	if cfg != (Config{}) {
		t.Fatalf("Load: expected zero-value Config for missing file, got %+v", cfg)
	}
}

func TestLoadPopulatedFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "common.json")
	writeFile(t, path, `{"node_id":"comb-1","hostname":"comb-1.example.com"}`)

	m := &Manager{Path: path}
	cfg, err := m.Load()
	if err != nil {
		t.Fatalf("Load: unexpected error: %v", err)
	}
	if cfg.NodeID != "comb-1" {
		t.Errorf("NodeID = %q, want %q", cfg.NodeID, "comb-1")
	}
	if cfg.Hostname != "comb-1.example.com" {
		t.Errorf("Hostname = %q, want %q", cfg.Hostname, "comb-1.example.com")
	}
}

func TestLoadMalformedFileIsError(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "common.json")
	writeFile(t, path, `{not valid json`)

	m := &Manager{Path: path}
	if _, err := m.Load(); err == nil {
		t.Fatal("Load: expected error for malformed JSON, got nil")
	}
}

func writeFile(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}
}
