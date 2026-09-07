package loginconfig

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestManager_LoadMissingFileReportsNotExists(t *testing.T) {
	m := &Manager{Path: filepath.Join(t.TempDir(), "does-not-exist.json")}

	cfg, exists, err := m.Load()
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	if exists {
		t.Errorf("Load() exists = true, want false for a file that was never written")
	}
	if len(cfg.RoleMap) != 0 {
		t.Errorf("Load() RoleMap = %+v, want empty", cfg.RoleMap)
	}
}

func TestManager_SaveThenLoadRoundTrips(t *testing.T) {
	m := &Manager{Path: filepath.Join(t.TempDir(), "role-map.json")}
	want := Config{RoleMap: map[string]string{"alice": "admin", "bob": "operator"}}

	if err := m.Save(want); err != nil {
		t.Fatalf("Save() error: %v", err)
	}
	got, exists, err := m.Load()
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	if !exists {
		t.Errorf("Load() exists = false, want true after Save")
	}
	if got.RoleMap["alice"] != "admin" || got.RoleMap["bob"] != "operator" || len(got.RoleMap) != 2 {
		t.Errorf("Load() = %+v, want %+v", got.RoleMap, want.RoleMap)
	}
}

func TestManager_SaveReplacesRatherThanMerges(t *testing.T) {
	m := &Manager{Path: filepath.Join(t.TempDir(), "role-map.json")}
	if err := m.Save(Config{RoleMap: map[string]string{"alice": "admin", "bob": "operator"}}); err != nil {
		t.Fatalf("Save() error: %v", err)
	}
	if err := m.Save(Config{RoleMap: map[string]string{"carol": "viewer"}}); err != nil {
		t.Fatalf("Save() error: %v", err)
	}
	got, _, err := m.Load()
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	if len(got.RoleMap) != 1 || got.RoleMap["carol"] != "viewer" {
		t.Errorf("Load() = %+v, want only carol:viewer (Save must replace, not merge)", got.RoleMap)
	}
}

// TestManager_LoadEmptyMapAfterSaveStillReportsExists is the direct
// regression test for the "once the file exists, it wins forever" rule
// - a deliberately emptied role map (every user removed via the UI)
// must not be indistinguishable from "no file was ever written."
func TestManager_LoadEmptyMapAfterSaveStillReportsExists(t *testing.T) {
	m := &Manager{Path: filepath.Join(t.TempDir(), "role-map.json")}
	if err := m.Save(Config{RoleMap: map[string]string{}}); err != nil {
		t.Fatalf("Save() error: %v", err)
	}
	_, exists, err := m.Load()
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	if !exists {
		t.Errorf("Load() exists = false after saving an empty map, want true")
	}
}

func TestManager_SaveRejectsInvalidRole(t *testing.T) {
	m := &Manager{Path: filepath.Join(t.TempDir(), "role-map.json")}
	err := m.Save(Config{RoleMap: map[string]string{"alice": "superuser"}})
	if err == nil {
		t.Fatalf("Save() = nil error, want a rejection for an invalid role")
	}
}

func TestManager_SaveRejectsEmptyUsername(t *testing.T) {
	m := &Manager{Path: filepath.Join(t.TempDir(), "role-map.json")}
	err := m.Save(Config{RoleMap: map[string]string{"": "admin"}})
	if err == nil {
		t.Fatalf("Save() = nil error, want a rejection for an empty username")
	}
}

func TestManager_Save_FileModeIs0600(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("file mode bits are not meaningful on windows")
	}
	path := filepath.Join(t.TempDir(), "role-map.json")
	m := &Manager{Path: path}
	if err := m.Save(Config{RoleMap: map[string]string{"alice": "admin"}}); err != nil {
		t.Fatalf("Save() error: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat() error: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Errorf("file mode = %o, want 0600", got)
	}
}
