package frontendconfig

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
	want := defaults()
	if cfg != want {
		t.Errorf("Load() = %+v, want defaults %+v", cfg, want)
	}
}

func TestManager_LoadPartialFileOverlaysOnlyPresentFields(t *testing.T) {
	path := filepath.Join(t.TempDir(), "frontend.json")
	if err := os.WriteFile(path, []byte(`{"http_addr": "0.0.0.0:8080", "manager_api_key": "apk_test"}`), 0o600); err != nil {
		t.Fatalf("writing test file: %v", err)
	}
	m := &Manager{Path: path}

	cfg, err := m.Load()
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	if cfg.HTTPAddr != "0.0.0.0:8080" {
		t.Errorf("HTTPAddr = %q, want the file's overridden value", cfg.HTTPAddr)
	}
	if cfg.ManagerAPIKey != "apk_test" {
		t.Errorf("ManagerAPIKey = %q, want apk_test", cfg.ManagerAPIKey)
	}
	if cfg.ManagerAddr != defaults().ManagerAddr {
		t.Errorf("ManagerAddr = %q, want the default %q since the file didn't set it", cfg.ManagerAddr, defaults().ManagerAddr)
	}
	if cfg.PeerManagerPort != defaults().PeerManagerPort {
		t.Errorf("PeerManagerPort = %q, want the default %q since the file didn't set it", cfg.PeerManagerPort, defaults().PeerManagerPort)
	}
}

func TestManager_LoadMalformedFileIsAnError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "frontend.json")
	if err := os.WriteFile(path, []byte(`{not valid json`), 0o600); err != nil {
		t.Fatalf("writing test file: %v", err)
	}
	m := &Manager{Path: path}

	if _, err := m.Load(); err == nil {
		t.Error("Load() error = nil, want an error for malformed JSON")
	}
}
