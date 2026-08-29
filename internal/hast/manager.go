package hast

import (
	"context"
	"fmt"
	"os"
	"strings"
)

// Role is a HAST resource's role, as passed to `hastctl role`.
type Role string

const (
	RoleInit      Role = "init"
	RolePrimary   Role = "primary"
	RoleSecondary Role = "secondary"
)

// Status is a parsed snapshot of `hastctl list <name>`'s output for one
// resource.
type Status struct {
	Role       string
	LocalPath  string
	RemoteAddr string

	// ResourceStatus is hastd's own health assessment ("complete",
	// "degraded", or "unknown"). It is only reported for a resource in
	// the primary role - a secondary's `hastctl list` output has no
	// equivalent field, so this is "" there.
	ResourceStatus string
}

// Manager renders hast.conf and drives hastctl(8) for HAST resources.
// Unlike internal/zfs and internal/jail, it does not manage the hastd(8)
// service itself - that is a system-level (rc.conf/service) concern,
// same as this project doesn't manage the zfs kernel module's load
// state.
type Manager struct {
	// ConfigPath is where hast.conf is written. Defaults to
	// DefaultConfigPath if empty.
	ConfigPath string
}

// New returns a Manager writing to DefaultConfigPath.
func New() *Manager {
	return &Manager{ConfigPath: DefaultConfigPath}
}

func (m *Manager) configPath() string {
	if m.ConfigPath == "" {
		return DefaultConfigPath
	}
	return m.ConfigPath
}

// WriteConfig renders and writes hast.conf for the given resources. The
// caller is responsible for deploying the identical file to both nodes
// and reloading/restarting hastd so it picks up the change - this
// package does not manage the hastd service (see Manager's doc comment).
func (m *Manager) WriteConfig(resources []Resource) error {
	body, err := RenderConfig(resources)
	if err != nil {
		return err
	}
	return os.WriteFile(m.configPath(), []byte(body), 0o644)
}

// CreateResource initializes local on-disk metadata for a resource
// already defined in hast.conf. It must be run on both nodes before
// either sets a role other than init.
func (m *Manager) CreateResource(ctx context.Context, name string) error {
	_, err := runCmd(ctx, "hastctl", "create", name)
	return err
}

// SetRole changes a resource's role on the local node.
func (m *Manager) SetRole(ctx context.Context, name string, role Role) error {
	_, err := runCmd(ctx, "hastctl", "role", string(role), name)
	return err
}

// Status returns the local node's current view of a resource.
func (m *Manager) Status(ctx context.Context, name string) (*Status, error) {
	out, err := runCmd(ctx, "hastctl", "list", name)
	if err != nil {
		return nil, err
	}
	return parseStatus(out)
}

// parseStatus parses `hastctl list <name>`'s output: a "<name>:" header
// line followed by indented "key: value" lines. Only the fields Status
// cares about are extracted; the rest (statistics, extentsize, etc.) are
// ignored.
func parseStatus(out string) (*Status, error) {
	s := &Status{}
	found := false

	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)

		switch key {
		case "role":
			s.Role = value
			found = true
		case "localpath":
			s.LocalPath = value
		case "remoteaddr":
			s.RemoteAddr = value
		case "status":
			s.ResourceStatus = value
		}
	}

	if !found {
		return nil, fmt.Errorf("hast: could not parse resource status from hastctl output: %q", out)
	}
	return s, nil
}
