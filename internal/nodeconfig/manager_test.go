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
	want := Config{Uplink: "re0", NATUplink: "bridge0", DNSServer: "10.62.0.1", JailEnabled: boolPtr(true)}

	if err := m.Save(want); err != nil {
		t.Fatalf("Save() error: %v", err)
	}
	got, err := m.Load()
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	if got.Uplink != want.Uplink || got.NATUplink != want.NATUplink || got.DNSServer != want.DNSServer || got.JailEnabled == nil || !*got.JailEnabled {
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

// TestManager_SaveRejectsUnsafeValues is the regression test for a
// 2026-09-06 security-audit finding: Uplink/NATUplink/DNSServer were
// entirely unvalidated, but are interpolated verbatim into generated
// dnsmasq.conf (internal/dhcpd.RenderConfig) and pf rules
// (internal/pf.Manager.ApplyNAT) - a newline in any of them let an
// Admin (the role UpdateNodeConfig requires) inject an arbitrary
// dnsmasq/pf directive via what looked like an ordinary Machine
// Configuration save.
func TestManager_SaveRejectsUnsafeValues(t *testing.T) {
	m := &Manager{Path: filepath.Join(t.TempDir(), "node-config.json")}

	cases := []Config{
		{Uplink: "re0\ndhcp-script=/tmp/pwn.sh"},
		{NATUplink: "bridge0\ndhcp-script=/tmp/pwn.sh"},
		{DNSServer: "10.62.0.1\ndhcp-script=/tmp/pwn.sh"},
		{DNSServer: "not-an-ip"},
		{Uplink: "toolonginterfacename01"}, // over FreeBSD's 15-char limit
	}
	for _, cfg := range cases {
		if err := m.Save(cfg); err == nil {
			t.Errorf("Save(%+v): error = nil, want a rejection", cfg)
		}
	}

	// A valid config must still be accepted.
	if err := m.Save(Config{Uplink: "re0", NATUplink: "bridge0", DNSServer: "10.62.0.1"}); err != nil {
		t.Errorf("Save() with valid values returned error: %v", err)
	}
}

func boolPtr(v bool) *bool {
	return &v
}
