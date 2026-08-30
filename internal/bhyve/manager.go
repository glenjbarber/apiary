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

// Config describes a VM to create. Per-VM IP allocation is still not
// implemented (that's ephemeral-state-shaped work, not purely local
// jail/VM config, per ADR-0010's consequences) - what Bridge adds here
// is only network *connectivity*, matching how bhyve/the guest OS
// itself would still need DHCP or static config inside the VM.
type Config struct {
	CPUs     int
	MemoryMB uint64

	// BootROM is the path to a UEFI firmware image (e.g.
	// /usr/local/share/uefi-firmware/BHYVE_UEFI.fd).
	BootROM string

	// DiskPath, if set, is a raw disk image or block device attached as
	// the VM's boot disk (AHCI). Left empty, the VM boots with no disk
	// at all - useful for lifecycle testing, not for running anything.
	DiskPath string

	// Bridge, if set, is the name of an existing bridge(4) interface
	// (e.g. "bridge0", already configured with whatever uplink member
	// the host needs - Apiary doesn't create or own the bridge itself,
	// only what's needed per VM). CreateVM creates a per-VM tap(4)
	// device, adds it to Bridge, and attaches it to the VM as a
	// virtio-net device; DestroyVM tears the tap back down. Left empty,
	// the VM boots with no NIC at all - the same opt-in pattern as
	// DiskPath.
	Bridge string
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

// tapfile records the tap(4) device name CreateVM created for qname, so
// a later DestroyVM - potentially in a different process, since bhyve
// itself runs detached - can find and tear it down without needing to
// re-derive or guess a name. There is no way to recover the tap name
// from the running vmm context alone.
func (m *Manager) tapfile(qname string) string {
	return filepath.Join(m.runDir(), qname+".tap")
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

	var tapName string
	if cfg.Bridge != "" {
		tapName, err = m.createTap(ctx, qname, cfg.Bridge)
		if err != nil {
			return err
		}
	}

	args := []string{
		"-f",
		"-p", m.pidfile(qname),
		"bhyve",
		"-c", strconv.Itoa(cfg.CPUs),
		"-m", fmt.Sprintf("%dM", cfg.MemoryMB),
		"-s", "0,hostbridge",
	}
	if cfg.DiskPath != "" {
		args = append(args, "-s", "4,ahci-hd,"+cfg.DiskPath)
	}
	if tapName != "" {
		args = append(args, "-s", "5,virtio-net,"+tapName)
	}
	args = append(args,
		"-s", "31,lpc",
		"-l", "bootrom,"+cfg.BootROM,
		qname,
	)

	if _, err := runCmd(ctx, "daemon", args...); err != nil {
		if tapName != "" {
			m.destroyTap(ctx, qname, tapName)
		}
		return err
	}
	return nil
}

// createTap clone-creates a new tap(4) device, adds it to bridge, and
// records its name via tapfile so destroyTap can find it again later
// (including from a different process, since bhyve itself runs
// detached). If adding to the bridge fails, the tap it just created is
// torn back down rather than left as an orphaned, unbridged interface.
func (m *Manager) createTap(ctx context.Context, qname, bridge string) (string, error) {
	tapName, err := runCmd(ctx, "ifconfig", "tap", "create")
	if err != nil {
		return "", fmt.Errorf("bhyve: creating tap device: %w", err)
	}
	if _, err := runCmd(ctx, "ifconfig", bridge, "addm", tapName); err != nil {
		runCmd(ctx, "ifconfig", tapName, "destroy")
		return "", fmt.Errorf("bhyve: adding %s to bridge %s: %w", tapName, bridge, err)
	}
	if err := os.WriteFile(m.tapfile(qname), []byte(tapName), 0o644); err != nil {
		runCmd(ctx, "ifconfig", tapName, "destroy")
		return "", fmt.Errorf("bhyve: recording tap device: %w", err)
	}
	return tapName, nil
}

// destroyTap tears down qname's tap device (destroying a tap interface
// also removes it from whatever bridge it was a member of, so there's
// no separate "deletem" step needed) and removes its tapfile record.
// tapName is passed in when the caller already has it (avoiding a
// redundant read); pass "" to have it read from tapfile instead.
func (m *Manager) destroyTap(ctx context.Context, qname, tapName string) {
	path := m.tapfile(qname)
	if tapName == "" {
		data, err := os.ReadFile(path)
		if err != nil {
			return
		}
		tapName = strings.TrimSpace(string(data))
	}
	if tapName != "" {
		runCmd(ctx, "ifconfig", tapName, "destroy")
	}
	os.Remove(path)
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

	m.destroyTap(ctx, qname, "")
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
