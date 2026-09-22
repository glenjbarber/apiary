// Package commonconfig persists the small set of settings that are
// genuinely identical across every daemon running on the same
// physical Comb (ADR-0111): NodeID and Hostname. It follows the same
// Manager/Load conventions as internal/nodeconfig,
// internal/frontendconfig, internal/restshimdconfig, and
// internal/raftdconfig, but on its own file - it is not itself a
// daemon's config, just a lower-precedence source each of those four
// packages consults for fields it doesn't set itself.
//
// This package only loads and parses common.json - it does not decide
// which of a service's own fields fall back to it. That merge is each
// service's own Manager.Load, since which fields are shared differs
// per daemon (see ADR-0111 for why TLS CA paths are excluded for now).
package commonconfig

import (
	"encoding/json"
	"fmt"
	"os"
)

// DefaultPath is where this file lives by default on a pkg-installed
// FreeBSD system, alongside every daemon's own config file under
// /usr/local/etc/apiary (ADR-0100, ADR-0111).
const DefaultPath = "/usr/local/etc/apiary/common.json"

// Config holds settings shared across every daemon on the same Comb.
// Every field's zero value means "not set here" - a service consulting
// this file falls back to its own default in that case, exactly as if
// common.json did not exist at all.
type Config struct {
	// NodeID is this Comb's raft/cluster identity, shared by managerd
	// (internal/nodeconfig) and raftd (internal/raftdconfig) - the two
	// daemons that each have their own node_id field today.
	NodeID string `json:"node_id,omitempty"`

	// Hostname is this Comb's hostname, for daemons that want it
	// without separately hardcoding or re-deriving it. Empty means
	// "use os.Hostname()", the same convention nodeconfig's own NodeID
	// field already uses.
	Hostname string `json:"hostname,omitempty"`
}

// Manager loads this file. The zero value is ready to use (Path empty
// means DefaultPath).
type Manager struct {
	Path string
}

func (m *Manager) path() string {
	if m.Path == "" {
		return DefaultPath
	}
	return m.Path
}

// Load reads the persisted common config. A missing file is not an
// error - most hosts will never have one, and that must behave
// identically to every field simply being unset. A malformed file is
// a real error: unlike a missing file, there's no reasonable fallback
// value for JSON a caller believed was valid.
func (m *Manager) Load() (Config, error) {
	data, err := os.ReadFile(m.path())
	if err != nil {
		if os.IsNotExist(err) {
			return Config{}, nil
		}
		return Config{}, err
	}
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return Config{}, fmt.Errorf("commonconfig: parsing %s: %w", m.path(), err)
	}
	return cfg, nil
}
