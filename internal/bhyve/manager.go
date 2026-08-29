package bhyve

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// DefaultRunDir is where pidfiles for detached bhyve processes are kept.
const DefaultRunDir = "/var/run/apiary/bhyve"

// Config describes a VM to create. There is no disk or network
// configuration yet - v1 proves the create/destroy/list lifecycle using
// just a boot ROM, the same "prove the primitive first" scope
// internal/zfs and internal/jail used. Disk (via internal/zfs-backed
// datasets) and networking are separate future slices.
type Config struct {
	CPUs     int
	MemoryMB uint64

	// BootROM is the path to a UEFI firmware image (e.g.
	// /usr/local/share/uefi-firmware/BHYVE_UEFI.fd).
	BootROM string
}

// Manager creates, destroys, and lists bhyve VMs, all named with a
// configured Prefix - bhyve VM names have no hierarchical namespace
// (they show up flatly under /dev/vmm/<name>), the same reason
// internal/jail uses a name prefix rather than ZFS-style path scoping.
type Manager struct {
	Prefix string

	// RunDir holds pidfiles for detached bhyve processes. Defaults to
	// DefaultRunDir if empty.
	RunDir string
}

// New returns a Manager whose VMs are all named Prefix+name.
func New(prefix string) *Manager {
	return &Manager{Prefix: prefix}
}

func (m *Manager) runDir() string {
	if m.RunDir == "" {
		return DefaultRunDir
	}
	return m.RunDir
}

// qualifiedName validates name and returns the full VM name
// (Prefix+name) used with bhyve(8)/bhyvectl(8).
func (m *Manager) qualifiedName(name string) (string, error) {
	if name == "" {
		return "", fmt.Errorf("bhyve: name must not be empty")
	}
	for _, r := range name {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_') {
			return "", fmt.Errorf("bhyve: invalid name %q: only alphanumerics, '-', and '_' are allowed", name)
		}
	}
	return m.Prefix + name, nil
}

func (m *Manager) pidfile(qname string) string {
	return filepath.Join(m.runDir(), qname+".pid")
}

// CreateVM starts a new VM, detached from this process via daemon(8) so
// it keeps running independently of whatever started it (the same way a
// real hypervisor manager must not take its VMs down if it restarts).
func (m *Manager) CreateVM(ctx context.Context, name string, cfg Config) error {
	qname, err := m.qualifiedName(name)
	if err != nil {
		return err
	}
	if cfg.BootROM == "" {
		return fmt.Errorf("bhyve: Config.BootROM must be set")
	}
	if cfg.CPUs <= 0 {
		return fmt.Errorf("bhyve: Config.CPUs must be positive")
	}
	if cfg.MemoryMB == 0 {
		return fmt.Errorf("bhyve: Config.MemoryMB must be set")
	}

	if err := os.MkdirAll(m.runDir(), 0o755); err != nil {
		return fmt.Errorf("bhyve: creating run dir: %w", err)
	}

	_, err = runCmd(ctx, "daemon",
		"-f",
		"-p", m.pidfile(qname),
		"bhyve",
		"-c", strconv.Itoa(cfg.CPUs),
		"-m", fmt.Sprintf("%dM", cfg.MemoryMB),
		"-s", "0,hostbridge",
		"-s", "31,lpc",
		"-l", "bootrom,"+cfg.BootROM,
		qname,
	)
	return err
}

// DestroyVM tears down a VM's vmm context and stops its detached bhyve
// process.
func (m *Manager) DestroyVM(ctx context.Context, name string) error {
	qname, err := m.qualifiedName(name)
	if err != nil {
		return err
	}

	_, destroyErr := runCmd(ctx, "bhyvectl", "--vm="+qname, "--destroy")
	if destroyErr != nil && !strings.Contains(destroyErr.Error(), "could not be opened") {
		return destroyErr
	}

	pidPath := m.pidfile(qname)
	if data, err := os.ReadFile(pidPath); err == nil {
		if pid, err := strconv.Atoi(strings.TrimSpace(string(data))); err == nil {
			runCmd(ctx, "kill", strconv.Itoa(pid))
		}
		os.Remove(pidPath)
	}
	return nil
}

// VMExists reports whether Prefix+name currently has a live vmm context.
func (m *Manager) VMExists(ctx context.Context, name string) (bool, error) {
	qname, err := m.qualifiedName(name)
	if err != nil {
		return false, err
	}
	_, statErr := os.Stat(filepath.Join("/dev/vmm", qname))
	if statErr == nil {
		return true, nil
	}
	if os.IsNotExist(statErr) {
		return false, nil
	}
	return false, statErr
}

// ListVMs returns the names (with Prefix stripped) of all currently
// running VMs whose name starts with Prefix. VMs not created by this
// Manager (no matching prefix) are not returned.
func (m *Manager) ListVMs(ctx context.Context) ([]string, error) {
	entries, err := os.ReadDir("/dev/vmm")
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("bhyve: listing /dev/vmm: %w", err)
	}

	var names []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), m.Prefix) {
			names = append(names, strings.TrimPrefix(e.Name(), m.Prefix))
		}
	}
	return names, nil
}
