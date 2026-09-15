package restshimdconfig

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
	path := filepath.Join(t.TempDir(), "restshimd.json")
	if err := os.WriteFile(path, []byte(`{"http_addr": "0.0.0.0:8081"}`), 0o600); err != nil {
		t.Fatalf("writing test file: %v", err)
	}
	m := &Manager{Path: path}

	cfg, err := m.Load()
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	if cfg.HTTPAddr != "0.0.0.0:8081" {
		t.Errorf("HTTPAddr = %q, want the file's overridden value", cfg.HTTPAddr)
	}
	if cfg.ManagerAddr != defaults().ManagerAddr {
		t.Errorf("ManagerAddr = %q, want the default %q since the file didn't set it", cfg.ManagerAddr, defaults().ManagerAddr)
	}
}

func TestManager_LoadMalformedFileIsAnError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "restshimd.json")
	if err := os.WriteFile(path, []byte(`{not valid json`), 0o600); err != nil {
		t.Fatalf("writing test file: %v", err)
	}
	m := &Manager{Path: path}

	if _, err := m.Load(); err == nil {
		t.Error("Load() error = nil, want an error for malformed JSON")
	}
}

func TestManager_SaveThenLoadRoundTripsEveryField(t *testing.T) {
	m := &Manager{Path: filepath.Join(t.TempDir(), "restshimd.json")}
	want := Config{
		ManagerAddr:          "10.50.0.9:17700",
		HTTPAddr:             "0.0.0.0:8081",
		ManagerTLS:           true,
		ManagerTLSCA:         "/usr/local/etc/apiary/tls/ca.pem",
		ManagerTLSServerName: "apiverse.apiary.work",
		TLSCert:              "/usr/local/etc/apiary/tls/fullchain.pem",
		TLSKey:               "/usr/local/etc/apiary/tls/key.pem",
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

func TestManager_SaveRejectsMalformedAddresses(t *testing.T) {
	m := &Manager{Path: filepath.Join(t.TempDir(), "restshimd.json")}
	for _, cfg := range []Config{
		{ManagerAddr: "not-a-host-port"},
		{HTTPAddr: "also-not-valid"},
	} {
		if err := m.Save(cfg); err == nil {
			t.Errorf("Save(%+v) error = nil, want a validation rejection", cfg)
		}
	}
}

func TestManager_SaveIsFullReplaceNotMerge(t *testing.T) {
	m := &Manager{Path: filepath.Join(t.TempDir(), "restshimd.json")}
	if err := m.Save(Config{HTTPAddr: "0.0.0.0:8081", TLSCert: "/a.pem"}); err != nil {
		t.Fatalf("first Save() error: %v", err)
	}
	if err := m.Save(Config{HTTPAddr: "0.0.0.0:9091"}); err != nil {
		t.Fatalf("second Save() error: %v", err)
	}
	got, err := m.Load()
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	if got.TLSCert != "" {
		t.Errorf("TLSCert = %q after a Save that omitted it, want empty - Save must fully replace, not merge", got.TLSCert)
	}
}

// TestManager_SaveTightensPermissionsOnExistingFile is the regression
// test for a 2026-09-15 audit finding - see the identical test in
// internal/frontendconfig for the full explanation.
func TestManager_SaveTightensPermissionsOnExistingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "restshimd.json")
	if err := os.WriteFile(path, []byte(`{}`), 0o644); err != nil {
		t.Fatalf("writing pre-existing file: %v", err)
	}
	m := &Manager{Path: path}

	if err := m.Save(Config{HTTPAddr: "0.0.0.0:8081"}); err != nil {
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
