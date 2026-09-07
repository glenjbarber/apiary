// Package loginconfig persists cmd/frontend's own local, per-node
// override of its login role map (ADR-0030) - physical, per-node data,
// mirroring internal/nodeconfig's role for managerd's local settings
// exactly, but scoped to just the -role-map flag's live-editable form.
// Never routed through raft: the set of who may log in to a given
// Hive's web UI is that Hive's own concern, not cluster state.
package loginconfig

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// DefaultPath is where the persisted role-map override lives by
// default on a pkg-installed FreeBSD system, mirroring
// nodeconfig.DefaultPath's own convention.
const DefaultPath = "/var/db/apiary/frontend-role-map.json"

// Config is the persisted shape: username -> role name ("admin",
// "operator", or "viewer"). Stored as plain strings rather than
// manager.Role so this package has no reason to import internal/manager
// for a single type alias.
type Config struct {
	RoleMap map[string]string `json:"role_map"`
}

// Manager loads and saves the role-map override file. The zero value
// is ready to use (Path empty means DefaultPath).
type Manager struct {
	Path string
}

func (m *Manager) path() string {
	if m.Path == "" {
		return DefaultPath
	}
	return m.Path
}

// Load reads the persisted config. exists is false (with a zero-value
// Config, no error) when the file has never been written - the
// caller's own -role-map flag value remains authoritative in that
// case, matching nodeconfig's own "no file yet, flags are the
// default" posture. Once the file exists at all - even holding an
// empty map, because every entry was deliberately removed through the
// UI - it is authoritative forever; a caller must not fall back to
// the flag just because the current map happens to be empty.
func (m *Manager) Load() (cfg Config, exists bool, err error) {
	data, err := os.ReadFile(m.path())
	if err != nil {
		if os.IsNotExist(err) {
			return Config{}, false, nil
		}
		return Config{}, false, err
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return Config{}, false, fmt.Errorf("loginconfig: parsing %s: %w", m.path(), err)
	}
	if cfg.RoleMap == nil {
		cfg.RoleMap = map[string]string{}
	}
	return cfg, true, nil
}

// Save atomically replaces the persisted config - the whole file, not
// merged, mirroring nodeconfig.Manager.Save's own "replace, never
// merge" contract. Validates every entry before writing anything, the
// same "validate at the point first accepted" discipline this
// project's own ADR-0067 established.
func (m *Manager) Save(cfg Config) error {
	if cfg.RoleMap == nil {
		cfg.RoleMap = map[string]string{}
	}
	for user, role := range cfg.RoleMap {
		if user == "" {
			return fmt.Errorf("loginconfig: username must not be empty")
		}
		switch role {
		case "admin", "operator", "viewer":
		default:
			return fmt.Errorf("loginconfig: role %q for user %q must be admin, operator, or viewer", role, user)
		}
	}
	body, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(m.path()), 0o755); err != nil {
		return err
	}
	return os.WriteFile(m.path(), body, 0o600)
}
