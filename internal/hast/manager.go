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
// state. RestartService (below) is the one narrow exception: hastd must
// be restarted for a config change to take effect at all (see
// WriteConfig's own doc comment and ADR-0008's documented gotcha), so a
// caller that just wrote a changed config has no other way to make it
// live - but this package still never starts, stops, or enables the
// service, which stays a one-time host prerequisite (see ADR-0026).
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

// RestartService restarts the local hastd(8) service so a just-written
// config change takes effect - hastd does not hot-reload hast.conf (see
// WriteConfig's own doc comment). This is the one exception to this
// package not managing hastd's lifecycle: only restart, never start/
// stop/enable.
func (m *Manager) RestartService(ctx context.Context) error {
	// "onerestart" (rc.subr's own force-regardless-of-rc.conf verb,
	// confirmed live: FreeBSD's service(8) has no top-level -f flag for
	// this - a first attempt using one failed with "Illegal option -f")
	// restarts regardless of hastd_enable in rc.conf - this method only
	// ever runs against an already-running hastd (starting it in the
	// first place remains a host prerequisite, see the doc comment
	// above), so an enable-flag mismatch shouldn't silently no-op a
	// config-reload restart the reconciler is relying on.
	_, err := runCmd(ctx, "service", "hastd", "onerestart")
	return err
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
	out, err := runCmd(ctx, "hastctl", "role", string(role), name)
	if err != nil {
		return err
	}
	// hastctl can print "[ERROR] ..." (e.g. "Error 57 received from
	// hastd" - ENOTCONN, seen live when the resource's hastd worker had
	// already died trying to open a not-yet-provisioned device) while
	// still exiting 0 - confirmed live, a real hastctl quirk this
	// package can't rely on exit codes alone to catch. Treat any such
	// line in its output as a failure even though runCmd saw no error.
	if strings.Contains(out, "[ERROR]") {
		return fmt.Errorf("hastctl role %s %s: %s", role, name, out)
	}
	return nil
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
