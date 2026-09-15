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

func TestManager_SaveThenLoadRoundTripsEveryField(t *testing.T) {
	m := &Manager{Path: filepath.Join(t.TempDir(), "frontend.json")}
	want := Config{
		ManagerAddr:          "10.50.0.9:17700",
		HTTPAddr:             "0.0.0.0:8080",
		ManagerTLS:           true,
		ManagerTLSCA:         "/usr/local/etc/apiary/tls/ca.pem",
		ManagerTLSServerName: "apiverse.apiary.work",
		TLSCert:              "/usr/local/etc/apiary/tls/fullchain.pem",
		TLSKey:               "/usr/local/etc/apiary/tls/key.pem",
		PeerTLS:              true,
		PeerTLSCA:            "/usr/local/etc/apiary/tls/peer-ca.pem",
		PeerHostnameSuffix:   ".apiary.work",
		PeerManagerPort:      "17700",
		ManagerAPIKey:        "apk_test_value",
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
	m := &Manager{Path: filepath.Join(t.TempDir(), "frontend.json")}
	for _, cfg := range []Config{
		{ManagerAddr: "not-a-host-port"},
		{HTTPAddr: "also-not-valid"},
	} {
		if err := m.Save(cfg); err == nil {
			t.Errorf("Save(%+v) error = nil, want a validation rejection", cfg)
		}
	}
}

func TestManager_SaveRejectsNewlineInPathOrSecretFields(t *testing.T) {
	m := &Manager{Path: filepath.Join(t.TempDir(), "frontend.json")}
	for _, cfg := range []Config{
		{TLSCert: "/etc/cert.pem\ninjected"},
		{ManagerAPIKey: "apk_test\ninjected"},
	} {
		if err := m.Save(cfg); err == nil {
			t.Errorf("Save(%+v) error = nil, want a newline rejection", cfg)
		}
	}
}

func TestManager_SaveIsFullReplaceNotMerge(t *testing.T) {
	m := &Manager{Path: filepath.Join(t.TempDir(), "frontend.json")}
	if err := m.Save(Config{HTTPAddr: "0.0.0.0:8080", ManagerAPIKey: "apk_first"}); err != nil {
		t.Fatalf("first Save() error: %v", err)
	}
	if err := m.Save(Config{HTTPAddr: "0.0.0.0:9090"}); err != nil {
		t.Fatalf("second Save() error: %v", err)
	}
	got, err := m.Load()
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	if got.ManagerAPIKey != "" {
		t.Errorf("ManagerAPIKey = %q after a Save that omitted it, want empty - Save must fully replace, not merge", got.ManagerAPIKey)
	}
	if got.HTTPAddr != "0.0.0.0:9090" {
		t.Errorf("HTTPAddr = %q, want the second Save's value", got.HTTPAddr)
	}
}
