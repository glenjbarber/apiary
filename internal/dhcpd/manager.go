package dhcpd

import (
	"context"
	"os"
)

// Manager renders dnsmasq.conf and drives the dnsmasq service to pick
// it up. Like internal/hast, it does not install or otherwise manage
// the dnsmasq package itself - `pkg install dnsmasq` is a one-time host
// prerequisite (see ADR-0022), not something Apiary does.
type Manager struct {
	// ConfigPath is where dnsmasq.conf is written. Defaults to
	// DefaultConfigPath if empty.
	ConfigPath string
}

func (m *Manager) configPath() string {
	if m.ConfigPath == "" {
		return DefaultConfigPath
	}
	return m.ConfigPath
}

// WriteAndReload renders scopes and writes them to dnsmasq.conf, then
// restarts the dnsmasq service so it picks up the change. A restart,
// not a reload signal, because dnsmasq's SIGHUP handling of new
// dhcp-range/interface stanzas is not reliable across versions - the
// same "no hot reload" caution already documented for hastd
// (internal/hast's package doc comment). This means a brief lease
// refresh blip for already-connected VMs on every network/lease change,
// not a correctness issue.
func (m *Manager) WriteAndReload(ctx context.Context, scopes []NetworkScope) error {
	body, err := RenderConfig(scopes)
	if err != nil {
		return err
	}
	if err := os.WriteFile(m.configPath(), []byte(body), 0o644); err != nil {
		return err
	}
	_, err = runCmd(ctx, "service", "dnsmasq", "restart")
	return err
}
