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
	"path/filepath"

	"github.com/glenjbarber/apiary/internal/jsonstrict"
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
	if err := jsonstrict.Unmarshal(data, &cfg); err != nil {
		return Config{}, fmt.Errorf("commonconfig: parsing %s: %w", m.path(), err)
	}
	return cfg, nil
}

// Save writes cfg, replacing whatever was there before in full (not a
// merge) - the same convention nodeconfig.Manager.Save, SaveWithHistory
// and the other three config packages already establish. A caller that
// wants to change one field Loads first, exactly as the RPC handlers
// do.
//
// This existed only as a reader until ADR-0147's `apiaryctl install`,
// which writes node_id and hostname into this file rather than into
// each of the four daemon files, because ADR-0112 makes this the right
// home for values that are identical on every daemon of one Comb. The
// atomic-write-then-rename is not an embellishment added with it: the
// four sibling packages each carry their own copy of the same helper,
// and a fifth variant that used a plain os.WriteFile would leave an
// identity file world-readable for the window between the open and the
// chmod on an already-existing file, which os.WriteFile's mode argument
// does not cover because it only applies on creation.
func (m *Manager) Save(cfg Config) error {
	body, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("commonconfig: marshalling config: %w", err)
	}
	return atomicWriteFile(m.path(), body)
}

// atomicWriteFile writes body to path via a temp file in the same
// directory, explicitly chmod 0600, then renames it into place,
// mirroring internal/nodeconfig's own atomicWriteFile.
func atomicWriteFile(path string, body []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".commonconfig-*.tmp")
	if err != nil {
		return fmt.Errorf("commonconfig: creating temp file: %w", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath) // no-op once successfully renamed
	if _, err := tmp.Write(body); err != nil {
		tmp.Close()
		return fmt.Errorf("commonconfig: writing temp file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("commonconfig: syncing temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("commonconfig: closing temp file: %w", err)
	}
	if err := os.Chmod(tmpPath, 0o600); err != nil {
		return fmt.Errorf("commonconfig: setting permissions: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("commonconfig: renaming into place: %w", err)
	}
	return nil
}
