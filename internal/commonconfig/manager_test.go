package commonconfig

import (
	"os"
	"path/filepath"
	"strings"
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

// TestLoadRejectsDuplicateKeys pins the incident this package shared
// with restshimdconfig and frontendconfig: a hand-edited config that
// repeats a key is not a file encoding/json can be trusted with,
// because it takes the last value of a repeated key with no error at
// all. common.json holds every daemon's shared identity, so a
// duplicated node_id would silently decide this Comb's raft identity.
func TestLoadRejectsDuplicateKeys(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "common.json")
	writeFile(t, path, `{
  "node_id": "comb-1",
  "hostname": "comb-1.example.com",
  "node_id": "comb-99"
}`)

	m := &Manager{Path: path}
	cfg, err := m.Load()
	if err == nil {
		t.Fatalf("Load() = %+v, nil; a duplicated key must not resolve to a config", cfg)
	}
	// The operator has to be able to act on this: it must name the file
	// to edit and the key to look at.
	for _, want := range []string{path, "node_id"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q; an operator cannot act on it", err, want)
		}
	}
}

// A clean common.json must decode exactly as it always did. If this
// fails, jsonstrict has changed behaviour for the common case, which
// would be a far worse regression than the duplicate it catches.
func TestLoadCleanFileUnchanged(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "common.json")
	writeFile(t, path, `{"node_id":"comb-1","hostname":"comb-1.example.com"}`)

	cfg, err := (&Manager{Path: path}).Load()
	if err != nil {
		t.Fatalf("Load() on a clean file: %v", err)
	}
	want := Config{NodeID: "comb-1", Hostname: "comb-1.example.com"}
	if cfg != want {
		t.Errorf("Load() = %+v, want %+v", cfg, want)
	}
}
