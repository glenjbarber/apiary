package nodeconfig

import (
	"path/filepath"
	"testing"
)

func TestManager_LoadMissingFileReturnsZeroValueNoError(t *testing.T) {
	m := &Manager{Path: filepath.Join(t.TempDir(), "does-not-exist.json")}

	cfg, err := m.Load()
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	if cfg != (Config{}) {
		t.Errorf("Load() = %+v, want zero value", cfg)
	}
}

func TestManager_SaveThenLoadRoundTrips(t *testing.T) {
	m := &Manager{Path: filepath.Join(t.TempDir(), "node-config.json")}
	want := Config{Uplink: "re0", NATUplink: "bridge0", JailEnabled: boolPtr(true)}

	if err := m.Save(want); err != nil {
		t.Fatalf("Save() error: %v", err)
	}
	got, err := m.Load()
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	if got.Uplink != want.Uplink || got.NATUplink != want.NATUplink || got.JailEnabled == nil || !*got.JailEnabled {
		t.Errorf("Load() = %+v, want %+v", got, want)
	}
}

func TestManager_SaveReplacesRatherThanMerges(t *testing.T) {
	m := &Manager{Path: filepath.Join(t.TempDir(), "node-config.json")}
	if err := m.Save(Config{Uplink: "re0", NATUplink: "bridge0"}); err != nil {
		t.Fatalf("Save() error: %v", err)
	}

	if err := m.Save(Config{Uplink: "em0"}); err != nil {
		t.Fatalf("Save() error: %v", err)
	}
	got, err := m.Load()
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	if want := (Config{Uplink: "em0"}); got != want {
		t.Errorf("Load() = %+v, want %+v (NATUplink cleared, not merged)", got, want)
	}
}

func boolPtr(v bool) *bool {
	return &v
}
