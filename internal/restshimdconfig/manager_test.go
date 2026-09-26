package restshimdconfig

import (
	"os"
	"path/filepath"
	"strings"
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
	// Every Save carries a manager_addr: Config.Validate now requires one
	// (a config without it cannot be dialed at all), so these cases about
	// full-replace semantics have to stay valid configs themselves.
	if err := m.Save(Config{ManagerAddr: "127.0.0.1:17700", HTTPAddr: "0.0.0.0:8081", TLSCert: "/a.pem", TLSKey: "/a.key"}); err != nil {
		t.Fatalf("first Save() error: %v", err)
	}
	if err := m.Save(Config{ManagerAddr: "127.0.0.1:17700", HTTPAddr: "0.0.0.0:9091"}); err != nil {
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

	if err := m.Save(Config{ManagerAddr: "127.0.0.1:17700", HTTPAddr: "0.0.0.0:8081"}); err != nil {
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

// TestManager_LoadRejectsSelfContradictoryTLSFields is the config half of
// the live failure: managerd was switched to TLS, this file kept a CA (or
// a server name) from before while still saying manager_tls=false, and
// the two lines contradicted each other. It is now refused where the
// fields can still be named, rather than surfacing as a 502 later.
func TestManager_LoadRejectsSelfContradictoryTLSFields(t *testing.T) {
	for _, tc := range []struct {
		name string
		json string
		want string
	}{
		{
			name: "ca set while tls is off",
			json: `{"manager_tls": false, "manager_tls_ca": "/usr/local/etc/apiary/managerd-ca.pem"}`,
			want: "manager_tls_ca is set",
		},
		{
			name: "server name set while tls is off",
			json: `{"manager_tls": false, "manager_tls_server_name": "apiarium.apiary.work"}`,
			want: "manager_tls_server_name is set",
		},
		{
			name: "tls on with both trust settings is fine",
			json: `{"manager_tls": true, "manager_tls_ca": "/usr/local/etc/apiary/managerd-ca.pem", "manager_tls_server_name": "apiarium.apiary.work"}`,
			want: "",
		},
		{
			name: "tls off with no trust settings is the default deployment",
			json: `{"manager_tls": false}`,
			want: "",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "restshimd.json")
			if err := os.WriteFile(path, []byte(tc.json), 0o600); err != nil {
				t.Fatalf("writing test file: %v", err)
			}
			cfg, err := (&Manager{Path: path}).Load()
			switch {
			case tc.want == "" && err != nil:
				t.Fatalf("Load() error = %v, want the config accepted", err)
			case tc.want != "" && err == nil:
				t.Fatalf("Load() = %+v, want an error naming %q", cfg, tc.want)
			case tc.want != "" && !strings.Contains(err.Error(), tc.want):
				t.Errorf("Load() error = %v, want it to name %q", err, tc.want)
			}
		})
	}
}

func TestValidate_RequiresAnUnambiguousManagerAddr(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  Config
	}{
		{"empty", Config{HTTPAddr: "127.0.0.1:8081"}},
		{"no port", Config{ManagerAddr: "127.0.0.1"}},
		{"no host", Config{ManagerAddr: ":17700"}},
		{"not host:port", Config{ManagerAddr: "managerd"}},
	} {
		if err := tc.cfg.Validate(); err == nil {
			t.Errorf("Validate(%s) = nil, want an error: %q cannot be dialed", tc.name, tc.cfg.ManagerAddr)
		}
	}
}

func TestValidate_AcceptsTheDefaults(t *testing.T) {
	if err := defaults().Validate(); err != nil {
		t.Errorf("Validate(defaults()) = %v, want nil - the shipped defaults must be loadable", err)
	}
}

func TestValidate_RejectsAHalfConfiguredServingTLSPair(t *testing.T) {
	// Already refused at startup by cmd/restshimd, but a config like this
	// should not be writable in the first place - by hand or through
	// managerd's UpdateRestshimdConfig.
	if err := (Config{ManagerAddr: "127.0.0.1:17700", HTTPAddr: "127.0.0.1:8081", TLSCert: "/path/cert.pem"}).Validate(); err == nil {
		t.Error("Validate() = nil for tls_cert without tls_key, want an error")
	}
	if err := (Config{ManagerAddr: "127.0.0.1:17700", HTTPAddr: "127.0.0.1:8081", TLSKey: "/path/key.pem"}).Validate(); err == nil {
		t.Error("Validate() = nil for tls_key without tls_cert, want an error")
	}
}

func TestManager_SaveRejectsContradictoryTLSFields(t *testing.T) {
	// The web UI's write path has to refuse the same thing Load does, or
	// it would let an operator save a config that cannot start.
	m := &Manager{Path: filepath.Join(t.TempDir(), "restshimd.json")}
	cfg := Config{
		ManagerAddr:  "127.0.0.1:17700",
		HTTPAddr:     "127.0.0.1:8081",
		ManagerTLS:   false,
		ManagerTLSCA: "/usr/local/etc/apiary/managerd-ca.pem",
	}
	if err := m.Save(cfg); err == nil {
		t.Error("Save() = nil for manager_tls=false with a CA set, want a validation rejection")
	}
}
