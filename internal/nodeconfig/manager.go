// Package nodeconfig persists a small set of node-local runtime
// settings (the uplink interfaces internal/vlan/internal/pf use and the
// DHCP resolver address - see ADR-0048 and ADR-0066) as a plain JSON file
// on disk. Physical, per-node
// data like internal/isostore/internal/hoststats - a NIC name is only
// ever meaningful to the one node that has it, so this is never
// replicated through raft. A change here takes effect the next time
// this node's managerd restarts, not live - see ADR-0049.
package nodeconfig

import (
	"encoding/json"
	"os"
)

// DefaultPath is where the settings file lives by default on a
// pkg-installed FreeBSD system, alongside internal/isostore's own
// /var/db/apiary/isos convention.
const DefaultPath = "/var/db/apiary/node-config.json"

// Config is the full set of node-local settings this package manages.
// Every field's zero value means "use the flag-provided default" -
// see cmd/managerd's own startup wiring.
type Config struct {
	// Uplink mirrors -vlan-uplink: the physical interface VLAN-tagged
	// networks attach to.
	Uplink string `json:"uplink,omitempty"`

	// NATUplink mirrors -nat-uplink: the interface a self-hosted
	// network's outbound NAT egresses through (ADR-0048).
	NATUplink string `json:"nat_uplink,omitempty"`

	// DNSServer mirrors -dhcp-dns-server: the resolver address handed to
	// DHCP clients on Apiary-managed networks. It is node-local because
	// each Hive can have a different reachable resolver.
	DNSServer string `json:"dhcp_dns_server,omitempty"`

	// JailEnabled mirrors -jail-enabled. Nil means use managerd's
	// startup flag; true/false are explicit local overrides.
	JailEnabled *bool `json:"jail_enabled,omitempty"`
}

// Manager reads/writes Config to a local file. Like internal/isostore,
// it does no validation of the values themselves (e.g. that Uplink
// names a real interface) - that's surfaced naturally the next time
// managerd starts and internal/vlan/internal/pf actually try to use it.
type Manager struct {
	// Path is where the config file is read from/written to. Defaults
	// to DefaultPath if empty.
	Path string
}

func (m *Manager) path() string {
	if m.Path == "" {
		return DefaultPath
	}
	return m.Path
}

// Load reads the current config. A missing file is not an error - it
// returns the zero Config, matching a fresh install that has never
// saved an override yet.
func (m *Manager) Load() (Config, error) {
	body, err := os.ReadFile(m.path())
	if os.IsNotExist(err) {
		return Config{}, nil
	}
	if err != nil {
		return Config{}, err
	}
	var cfg Config
	if err := json.Unmarshal(body, &cfg); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// Save writes cfg, replacing whatever was there before in full (not a
// merge) - the caller is expected to Load first if it wants to change
// only one field, the same convention internal/hast's WriteConfig and
// internal/dhcpd's RenderConfig already use for their own config files.
func (m *Manager) Save(cfg Config) error {
	body, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(m.path(), body, 0o644)
}
