package jail

import (
	"context"
	"fmt"
	"strconv"
	"strings"
)

// Config describes a jail to create.
type Config struct {
	// Path is the jail's root directory. Unlike internal/zfs, this
	// package does not create or manage that directory itself - callers
	// are expected to provide an already-populated root (e.g. a ZFS
	// dataset from internal/zfs, mounted at Path).
	Path string

	// Hostname is the jail's host.hostname.
	Hostname string
}

// Info is a snapshot of a running jail's state, as reported by jls(8).
type Info struct {
	Name     string
	JID      int
	Path     string
	Hostname string
}

// Manager creates, removes, lists, and inspects jails, all named with a
// configured Prefix so Apiary never touches a jail it didn't create -
// jail(8) has no ZFS-style delegated/scoped namespace, so a name prefix
// is the only scoping mechanism available.
type Manager struct {
	Prefix string
}

// New returns a Manager whose jails are all named Prefix+name (e.g.
// Prefix "apiary-" for a jail named "web-1" creates "apiary-web-1").
func New(prefix string) *Manager {
	return &Manager{Prefix: prefix}
}

// qualifiedName validates name and returns the full jail name
// (Prefix+name) used with jail(8)/jls(8).
func (m *Manager) qualifiedName(name string) (string, error) {
	if name == "" {
		return "", fmt.Errorf("jail: name must not be empty")
	}
	for _, r := range name {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_') {
			return "", fmt.Errorf("jail: invalid name %q: only alphanumerics, '-', and '_' are allowed", name)
		}
	}
	return m.Prefix + name, nil
}

// CreateJail creates and starts a new persistent jail. Networking is
// ip4=inherit (sharing the host's network stack) for v1 - VNET/dedicated
// IP allocation is a separate concern for later.
func (m *Manager) CreateJail(ctx context.Context, name string, cfg Config) error {
	qname, err := m.qualifiedName(name)
	if err != nil {
		return err
	}
	if cfg.Path == "" {
		return fmt.Errorf("jail: Config.Path must be set")
	}

	_, err = runCmd(ctx, "jail", "-c",
		"name="+qname,
		"path="+cfg.Path,
		"host.hostname="+cfg.Hostname,
		"ip4=inherit",
		"persist",
	)
	return err
}

// RemoveJail stops and removes a jail.
func (m *Manager) RemoveJail(ctx context.Context, name string) error {
	qname, err := m.qualifiedName(name)
	if err != nil {
		return err
	}
	_, err = runCmd(ctx, "jail", "-r", qname)
	return err
}

// JailExists reports whether a jail by this name is currently running.
func (m *Manager) JailExists(ctx context.Context, name string) (bool, error) {
	qname, err := m.qualifiedName(name)
	if err != nil {
		return false, err
	}
	_, err = runCmd(ctx, "jls", "-j", qname, "-n", "name")
	if err != nil {
		if strings.Contains(err.Error(), "not found") {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// JailInfo returns the current state of a running jail.
func (m *Manager) JailInfo(ctx context.Context, name string) (*Info, error) {
	qname, err := m.qualifiedName(name)
	if err != nil {
		return nil, err
	}

	out, err := runCmd(ctx, "jls", "-j", qname, "-n", "name", "path", "host.hostname", "jid")
	if err != nil {
		return nil, err
	}

	fields := parseKeyValues(out)
	jid, err := strconv.Atoi(fields["jid"])
	if err != nil {
		return nil, fmt.Errorf("jail: parsing jid from jls output %q: %w", out, err)
	}

	return &Info{
		Name:     strings.TrimPrefix(fields["name"], m.Prefix),
		JID:      jid,
		Path:     fields["path"],
		Hostname: fields["host.hostname"],
	}, nil
}

// ListJails returns the names (with Prefix stripped) of all currently
// running jails whose name starts with Prefix. Jails not created by this
// Manager (no matching prefix) are not returned.
func (m *Manager) ListJails(ctx context.Context) ([]string, error) {
	out, err := runCmd(ctx, "jls", "-n", "name")
	if err != nil {
		return nil, err
	}

	var names []string
	for _, line := range strings.Split(out, "\n") {
		fields := parseKeyValues(strings.TrimSpace(line))
		name, ok := fields["name"]
		if !ok || !strings.HasPrefix(name, m.Prefix) {
			continue
		}
		names = append(names, strings.TrimPrefix(name, m.Prefix))
	}
	return names, nil
}

// parseKeyValues parses jls(8)'s `-n` output: space-separated key=value
// pairs on one line. This is only used with a fixed, known set of
// requested parameters (name, path, host.hostname, jid) whose values
// never contain spaces in this package's own usage, so a plain
// space-split is sufficient - it does not attempt to handle jls's
// quoted-string escaping for arbitrary parameters.
func parseKeyValues(line string) map[string]string {
	fields := make(map[string]string)
	for _, tok := range strings.Fields(line) {
		key, value, found := strings.Cut(tok, "=")
		if !found {
			continue
		}
		fields[key] = strings.Trim(value, `"`)
	}
	return fields
}
