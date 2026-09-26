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

	// VNET, if true, gives this jail its own network stack (jail(8)'s
	// "vnet;" parameter) instead of ip4=inherit - see ADR-0117. Requires
	// VNETInterface to be set; CreateJail rejects VNET without it.
	VNET bool

	// VNETInterface names the epair(4) "b" end (see
	// internal/vlan.Manager.EnsureEpair) to hand to this jail as its
	// vnet interface. Ignored unless VNET is true. jail(8) moves this
	// interface into the jail's own vnet at creation time; it must not
	// already be in use by another jail.
	VNETInterface string

	// IPAddress/IPPrefixLen, if set, are assigned to VNETInterface
	// inside the jail's own network stack once it starts, via jexec(8)
	// - jail(8) itself has no ip4.addr-style parameter for a vnet
	// interface, unlike ip4=inherit's flat host-stack model, so this is
	// a separate step rather than a jail(8) creation parameter. Ignored
	// unless VNET is true.
	IPAddress   string
	IPPrefixLen int

	// Gateway, if set, becomes this jail's default route once
	// IPAddress is assigned. Ignored unless VNET and IPAddress are set.
	Gateway string
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

	// Runner overrides how this Manager executes jail(8)/jls(8)/jexec(8).
	// nil means the real shell, which is the only correct setting in
	// production; tests inject a fake so the whole package is
	// exercisable off a FreeBSD host.
	Runner CommandRunner
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

// createArgs builds the `jail -c` parameter list for cfg, given the
// already-validated, already-prefixed jail name qname - split out from
// CreateJail as a pure function so the ip4=inherit/vnet stanza choice
// can be unit-tested directly without shelling out to a real jail(8)
// (which requires root and a real FreeBSD host - see
// integration_test.go).
func createArgs(qname string, cfg Config) []string {
	args := []string{
		"name=" + qname,
		"path=" + cfg.Path,
		"host.hostname=" + cfg.Hostname,
	}
	if cfg.VNET {
		args = append(args, "vnet", "vnet.interface="+cfg.VNETInterface)
	} else {
		args = append(args, "ip4=inherit")
	}
	return append(args, "persist")
}

// CreateJail creates and starts a new persistent jail. Networking is
// ip4=inherit (sharing the host's network stack) by default - opt into
// dedicated VNET networking (ADR-0117) by setting cfg.VNET and
// cfg.VNETInterface. Existing callers that never set VNET see no
// behavior change: the ip4=inherit path below is untouched.
func (m *Manager) CreateJail(ctx context.Context, name string, cfg Config) error {
	qname, err := m.qualifiedName(name)
	if err != nil {
		return err
	}
	if cfg.Path == "" {
		return fmt.Errorf("jail: Config.Path must be set")
	}
	if cfg.VNET && cfg.VNETInterface == "" {
		return fmt.Errorf("jail: Config.VNETInterface must be set when VNET is true")
	}

	if _, err := m.run(ctx, "jail", append([]string{"-c"}, createArgs(qname, cfg)...)...); err != nil {
		return err
	}

	// A vnet jail's interface starts down with no address inside the
	// jail's own network stack - jail(8) has no ip4.addr-style
	// parameter for a vnet interface (unlike ip4=inherit's flat
	// host-stack model), so this is a separate jexec(8) step, run only
	// once, right after creation, not on every reconciler tick.
	//
	// It goes through EnsureAddressing - the exact same observe/compare/
	// repair sequence internal/jailnet's reconciler uses on every later
	// tick - so a retry after a partially-successful creation (say the
	// address landed and the route didn't) converges instead of failing
	// forever on ifconfig's "File exists", and so creation-time and
	// reconcile-time addressing can never diverge in behavior.
	if cfg.VNET && cfg.IPAddress != "" {
		addr := Address{IP: cfg.IPAddress, PrefixLen: cfg.IPPrefixLen}
		if err := m.EnsureAddressing(ctx, qname, cfg.VNETInterface, addr, cfg.Gateway); err != nil {
			return err
		}
	}
	return nil
}

// RemoveJail stops and removes a jail.
func (m *Manager) RemoveJail(ctx context.Context, name string) error {
	qname, err := m.qualifiedName(name)
	if err != nil {
		return err
	}
	_, err = m.run(ctx, "jail", "-r", qname)
	return err
}

// JailExists reports whether a jail by this name is currently running.
func (m *Manager) JailExists(ctx context.Context, name string) (bool, error) {
	qname, err := m.qualifiedName(name)
	if err != nil {
		return false, err
	}
	_, err = m.run(ctx, "jls", "-j", qname, "-n", "name")
	if err != nil {
		if notFound(err, "jls") {
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

	out, err := m.run(ctx, "jls", "-j", qname, "-n", "name", "path", "host.hostname", "jid")
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
	out, err := m.run(ctx, "jls", "-n", "name")
	if err != nil {
		return nil, err
	}

	return m.managedNames(out), nil
}

func (m *Manager) managedNames(out string) []string {
	var names []string
	for _, line := range strings.Split(out, "\n") {
		fields := parseKeyValues(strings.TrimSpace(line))
		name, ok := fields["name"]
		if !ok || !strings.HasPrefix(name, m.Prefix) {
			continue
		}
		names = append(names, strings.TrimPrefix(name, m.Prefix))
	}
	return names
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
