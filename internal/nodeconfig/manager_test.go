package nodeconfig

import (
	"os"
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

// TestManager_SaveRejectsUnsafeNewFields is a broader regression test
// covering every new field validate() gained for ADR-0070 (the "system
// settings" expansion) - each of these is validated even though most
// of them flow into a separate argv/API argument rather than a
// generated text file, as defense in depth (see validate's own doc
// comment).
func TestManager_SaveRejectsUnsafeNewFields(t *testing.T) {
	m := &Manager{Path: filepath.Join(t.TempDir(), "node-config.json")}

	cases := map[string]Config{
		"bhyve_bridge too long":       {BhyveBridge: "toolonginterfacename01"},
		"bhyve_bootrom newline":       {BhyveBootROM: "/path\ninjected"},
		"iso_dir newline":             {ISODir: "/path\ninjected"},
		"jail_mount_base newline":     {JailMountBase: "/path\ninjected"},
		"tls_cert newline":            {TLSCert: "/path\ninjected"},
		"tls_key newline":             {TLSKey: "/path\ninjected"},
		"cloudflare_token_file nl":    {CloudflareTokenFile: "/path\ninjected"},
		"cloudflare_creds_file nl":    {CloudflareTunnelCredentialsFile: "/path\ninjected"},
		"zfs_base bad char":           {ZFSBase: "zroot/apiary; rm -rf /"},
		"bhyve_prefix bad char":       {BhyvePrefix: "apiary-\ninjected"},
		"jail_prefix bad char":        {JailPrefix: "apiary $(whoami)"},
		"cloudflare_zone_id bad char": {CloudflareZoneID: "zone;drop"},
		"cloudflare_tunnel_id bad":    {CloudflareTunnelID: "tunnel|pipe"},
		"peer_managerd_port non-num":  {PeerManagerdPort: "not-a-port"},
		"peer_managerd_port too big":  {PeerManagerdPort: "99999"},
		"peer_managerd_port zero":     {PeerManagerdPort: "0"},
		"hostname map no equals":      {PeerTLSHostnameMap: "10.50.0.9"},
		"hostname map bad ip":         {PeerTLSHostnameMap: "not-an-ip=host.example.com"},
		"negative reconcile interval": {ReconcileInterval: -1},
		"negative history limit":      {AssumptionHistoryLimit: -1},
	}
	for name, cfg := range cases {
		t.Run(name, func(t *testing.T) {
			if err := m.Save(cfg); err == nil {
				t.Errorf("Save(%+v): error = nil, want a rejection", cfg)
			}
		})
	}

	// Valid values across the same set of fields must still be accepted.
	valid := Config{
		BhyveBridge:                     "bridge0",
		BhyveBootROM:                    "/usr/local/share/uefi-firmware/BHYVE_UEFI.fd",
		ISODir:                          "/var/db/apiary/isos",
		JailMountBase:                   "/apiary-jails",
		TLSCert:                         "/home/claude/apiary-tls/fullchain.pem",
		TLSKey:                          "/home/claude/apiary-tls/key.pem",
		CloudflareTokenFile:             "/home/claude/cf-token",
		CloudflareTunnelCredentialsFile: "/home/claude/cf-creds.json",
		ZFSBase:                         "zroot/apiary",
		BhyvePrefix:                     "apiary-",
		JailPrefix:                      "apiary-",
		CloudflareZoneID:                "abc123",
		CloudflareTunnelID:              "def-456",
		PeerManagerdPort:                "17700",
		PeerTLSHostnameMap:              "10.50.0.9=apiverse.apiary.work,10.50.0.14=apiarium.apiary.work",
		ReconcileInterval:               30_000_000_000, // 30s in ns
		AssumptionHistoryLimit:          200,
	}
	if err := m.Save(valid); err != nil {
		t.Errorf("Save() with valid values returned error: %v", err)
	}
}

// TestManager_SaveEnforcesScopePathWriteOnce is the regression test for
// the write-once resource-scope-path rule (ADR-0070): once a value is
// saved, a *different*, non-empty value for the same field must be
// rejected, since changing it later would silently orphan real
// resources rather than move them.
func TestManager_SaveEnforcesScopePathWriteOnce(t *testing.T) {
	m := &Manager{Path: filepath.Join(t.TempDir(), "node-config.json")}

	if err := m.Save(Config{ZFSBase: "zroot/apiary"}); err != nil {
		t.Fatalf("first Save() error: %v", err)
	}

	// Re-saving the identical value must succeed (not a real change).
	if err := m.Save(Config{ZFSBase: "zroot/apiary", Uplink: "re0"}); err != nil {
		t.Errorf("re-saving the same zfs_base value should succeed, got: %v", err)
	}

	// A genuinely different value must be rejected.
	if err := m.Save(Config{ZFSBase: "zroot/other"}); err == nil {
		t.Error("Save() with a changed zfs_base = nil error, want a rejection")
	}

	// The rejected save must not have overwritten the file.
	got, err := m.Load()
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	if got.ZFSBase != "zroot/apiary" {
		t.Errorf("ZFSBase = %q after a rejected change, want it unchanged at %q", got.ZFSBase, "zroot/apiary")
	}

	// Leaving the field empty (not touching it) while changing something
	// else must succeed and must not clear the saved value.
	if err := m.Save(Config{Uplink: "em0"}); err != nil {
		t.Fatalf("Save() with zfs_base left empty error: %v", err)
	}
	got, err = m.Load()
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	if got.ZFSBase != "" {
		t.Errorf("ZFSBase = %q after saving with it empty, want it cleared (Save replaces, not merges - see TestManager_SaveReplacesRatherThanMerges)", got.ZFSBase)
	}
}

// TestManager_SaveEnforcesScopePathWriteOnce_AllFields confirms every
// one of the five write-once fields is actually covered, not just
// zfs_base.
func TestManager_SaveEnforcesScopePathWriteOnce_AllFields(t *testing.T) {
	cases := []struct {
		name  string
		first Config
		next  Config
	}{
		{"bhyve_prefix", Config{BhyvePrefix: "apiary-"}, Config{BhyvePrefix: "other-"}},
		{"iso_dir", Config{ISODir: "/var/db/apiary/isos"}, Config{ISODir: "/other/isos"}},
		{"jail_prefix", Config{JailPrefix: "apiary-"}, Config{JailPrefix: "other-"}},
		{"jail_mount_base", Config{JailMountBase: "/apiary-jails"}, Config{JailMountBase: "/other-jails"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m := &Manager{Path: filepath.Join(t.TempDir(), "node-config.json")}
			if err := m.Save(c.first); err != nil {
				t.Fatalf("first Save() error: %v", err)
			}
			if err := m.Save(c.next); err == nil {
				t.Errorf("Save() with a changed %s = nil error, want a rejection", c.name)
			}
		})
	}
}

// TestManager_Save_FileModeIs0600 is the regression test for the file
// permission tightening this same change made: node-config.json can
// now hold PeerAPIKey/RaftdToken, live credentials, so it must never be
// group/world-readable the way it was (0644) before those fields
// existed.
func TestManager_Save_FileModeIs0600(t *testing.T) {
	path := filepath.Join(t.TempDir(), "node-config.json")
	m := &Manager{Path: path}
	if err := m.Save(Config{PeerAPIKey: "apk_test"}); err != nil {
		t.Fatalf("Save() error: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat() error: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("file mode = %v, want 0600", info.Mode().Perm())
	}
}

func boolPtr(v bool) *bool {
	return &v
}
