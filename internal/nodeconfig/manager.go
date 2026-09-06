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
	"fmt"
	"net"
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
// it does no validation of *semantic* correctness (e.g. that Uplink
// names an interface that actually exists) - that's surfaced naturally
// the next time managerd starts and internal/vlan/internal/pf actually
// try to use it. It does validate that each value is *safe*, though
// (see Save): Uplink/NATUplink/DNSServer are rendered verbatim into
// generated dnsmasq.conf/pf rules (internal/dhcpd.RenderConfig,
// internal/pf.Manager.ApplyNAT) with no escaping of their own, so an
// unvalidated newline here previously let an Admin inject arbitrary
// dnsmasq/pf directives via UpdateNodeConfig - the same class of bug
// internal/raft.FSM's validResourceID/validInterfaceName close for
// raft-replicated VM/jail/network fields.
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
	if err := validate(cfg); err != nil {
		return err
	}
	body, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(m.path(), body, 0o644)
}

// validate rejects a value that would be unsafe to interpolate into
// generated configuration, without judging whether it's semantically
// correct (see the Manager doc comment above).
func validate(cfg Config) error {
	if cfg.Uplink != "" && !validInterfaceName(cfg.Uplink) {
		return fmt.Errorf("nodeconfig: invalid uplink %q: must be a plain interface name (alphanumerics and '-', max 15 chars)", cfg.Uplink)
	}
	if cfg.NATUplink != "" && !validInterfaceName(cfg.NATUplink) {
		return fmt.Errorf("nodeconfig: invalid nat_uplink %q: must be a plain interface name (alphanumerics and '-', max 15 chars)", cfg.NATUplink)
	}
	if cfg.DNSServer != "" && net.ParseIP(cfg.DNSServer) == nil {
		return fmt.Errorf("nodeconfig: invalid dhcp_dns_server %q: must be a plain IP address", cfg.DNSServer)
	}
	return nil
}

// validInterfaceName mirrors internal/raft.FSM's own unexported
// function of the same name and rationale (see its doc comment) -
// duplicated rather than shared across an otherwise-unrelated package
// boundary, the same "each package validates its own written format"
// convention internal/jail and internal/isostore already follow
// independently for their own name checks.
func validInterfaceName(name string) bool {
	if len(name) > 15 {
		return false
	}
	for _, r := range name {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-') {
			return false
		}
	}
	return true
}
