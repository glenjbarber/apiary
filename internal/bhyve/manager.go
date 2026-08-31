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

	// ISOPath, if set, is attached read-only as a CD-ROM (AHCI) device -
	// an installer image to boot from. See internal/isostore for how
	// uploaded images are stored and verified; this field just takes
	// whatever local path the caller resolved from there. Only for
	// genuine ISO9660 media - see InstallDiskPath for anything else.
	ISOPath string

	// InstallDiskPath, if set, attaches a second AHCI disk (distinct from
	// DiskPath, the VM's own persistent boot disk) for install media
	// that isn't a real ISO9660 filesystem - most notably a FreeBSD
	// "memstick" image, which is a raw bootable disk image, not a CD.
	// Attaching one via ISOPath instead leaves firmware with no ISO9660
	// filesystem to find, so it never boots. The caller (internal/
	// cluster's Reconciler) decides between this and ISOPath by sniffing
	// the actual file (internal/isostore.Manager.IsISO9660) - ISOPath and
	// InstallDiskPath are mutually exclusive in practice, though nothing
	// here enforces that.
	InstallDiskPath string

	// Bridge, if set, is the name of an existing bridge(4) interface
	// (e.g. "bridge0", already configured with whatever uplink member
	// the host needs - Apiary doesn't create or own the bridge itself,
	// only what's needed per VM). CreateVM creates a per-VM tap(4)
	// device, adds it to Bridge, and attaches it to the VM as a
	// virtio-net device; DestroyVM tears the tap back down. Left empty,
	// the VM boots with no NIC at all - the same opt-in pattern as
	// DiskPath.
	Bridge string

	// MACAddress, if set, is assigned to the virtio-net device (bhyve's
	// "mac=<addr>" sub-option) instead of bhyve's own default/random
	// assignment. Set by internal/cluster's Reconciler from a VM's
	// ephemeral-state-assigned MAC (internal/raft's deriveMAC) when it's
	// on a NetworkDefinition - see ADR-0022. Meaningless without Bridge
	// also set (there's no NIC to assign it to otherwise).
	MACAddress string

	// EnableVNC, if true, attaches a VNC framebuffer device (fbuf) plus a
	// USB tablet (for absolute mouse positioning - relative mouse motion
	// over VNC is nearly unusable) so the VM's graphical console can be
	// viewed remotely - see internal/frontend's noVNC-based console page
	// (ADR-0020). CreateVM allocates a free local TCP port itself
	// (there's no ephemeral-state field for this - see VNCPort's doc
	// comment for why) and persists it the same way createTap persists a
	// VM's tap device name, for a later VNCPort lookup.
	EnableVNC bool

	// EnableSerialLog, if true, attaches the VM's com1 to one end of a
	// dedicated nmdm(4) null-modem pair and starts a small detached
	// reader (via daemon(8), the same tool CreateVM itself uses) that
	// continuously appends whatever the guest writes to its serial
	// console into a plain log file - see SerialLogPath. Framebuffer
	// consoles (VNC) only show whatever's currently on screen; many
	// cloud/server images redirect their actual boot/cloud-init output
	// to the serial port instead, which a point-in-time VNC screenshot
	// can never capture. No serial port is attached at all when false.
	EnableSerialLog bool
}

// vncBasePort/vncPortRange bound the local TCP ports CreateVM will assign
// for VNC framebuffers. 100 concurrent VM consoles is far more than this
// project runs today; a real deployment needing more would need a wider
// range or a smarter allocator.
const (
	vncBasePort  = 5900
	vncPortRange = 100
)

// nmdmRange bounds the nmdm(4) unit numbers CreateVM will assign for
// serial console capture - same reasoning as vncPortRange.
const nmdmRange = 100

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

// vncfile records the local TCP port CreateVM chose for qname's VNC
// framebuffer, the same way tapfile records its tap device name - there
// is no way to recover the port from the running vmm context alone, and
// a separate process (e.g. managerd's RPC server answering
// GetVMConsole) needs to look it up without re-deriving it.
func (m *Manager) vncfile(qname string) string {
	return filepath.Join(m.runDir(), qname+".vnc")
}

// nmdmfile records the nmdm(4) unit number CreateVM allocated for
// qname's serial console, the same way vncfile records a VNC port.
func (m *Manager) nmdmfile(qname string) string {
	return filepath.Join(m.runDir(), qname+".nmdm")
}

// serialpidfile records the pid of the detached reader process draining
// qname's nmdm pair into its log file, so DestroyVM can stop it - it's a
// separate process from bhyve itself, with its own lifecycle.
func (m *Manager) serialpidfile(qname string) string {
	return filepath.Join(m.runDir(), qname+".serialpid")
}

// seriallogfile is where qname's serial console output is continuously
// appended - see SerialLogPath.
func (m *Manager) seriallogfile(qname string) string {
	return filepath.Join(m.runDir(), qname+".serial.log")
}

// allocateNmdm picks the lowest nmdm(4) unit number in [0, nmdmRange) not
// already recorded in a *.nmdm file under RunDir - mirrors
// allocateVNCPort exactly.
func (m *Manager) allocateNmdm() (int, error) {
	used := make(map[int]bool)
	entries, err := os.ReadDir(m.runDir())
	if err != nil && !os.IsNotExist(err) {
		return 0, fmt.Errorf("bhyve: scanning run dir for nmdm units: %w", err)
	}
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".nmdm") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(m.runDir(), e.Name()))
		if err != nil {
			continue
		}
		if unit, err := strconv.Atoi(strings.TrimSpace(string(data))); err == nil {
			used[unit] = true
		}
	}
	for unit := 0; unit < nmdmRange; unit++ {
		if !used[unit] {
			return unit, nil
		}
	}
	return 0, fmt.Errorf("bhyve: no free nmdm unit in [0, %d)", nmdmRange)
}

// SerialLogPath returns the local path name's serial console output is
// being continuously appended to, as recorded by CreateVM. ok is false
// (with err nil) if name has no recorded serial log - not created with
// EnableSerialLog, or not created at all.
func (m *Manager) SerialLogPath(name string) (path string, ok bool, err error) {
	qname, err := m.qualifiedName(name)
	if err != nil {
		return "", false, err
	}
	logPath := m.seriallogfile(qname)
	if _, err := os.Stat(logPath); os.IsNotExist(err) {
		return "", false, nil
	} else if err != nil {
		return "", false, err
	}
	return logPath, true, nil
}

// allocateVNCPort picks the lowest port in [vncBasePort, vncBasePort+
// vncPortRange) not already recorded in a *.vnc file under RunDir.
func (m *Manager) allocateVNCPort() (int, error) {
	used := make(map[int]bool)
	entries, err := os.ReadDir(m.runDir())
	if err != nil && !os.IsNotExist(err) {
		return 0, fmt.Errorf("bhyve: scanning run dir for VNC ports: %w", err)
	}
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".vnc") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(m.runDir(), e.Name()))
		if err != nil {
			continue
		}
		if port, err := strconv.Atoi(strings.TrimSpace(string(data))); err == nil {
			used[port] = true
		}
	}
	for port := vncBasePort; port < vncBasePort+vncPortRange; port++ {
		if !used[port] {
			return port, nil
		}
	}
	return 0, fmt.Errorf("bhyve: no free VNC port in [%d, %d)", vncBasePort, vncBasePort+vncPortRange)
}

// VNCPort returns the local TCP port name's VNC framebuffer is listening
// on, as recorded by CreateVM. ok is false (with err nil) if name has no
// recorded VNC port - not created with EnableVNC, or not created at all.
func (m *Manager) VNCPort(name string) (port int, ok bool, err error) {
	qname, err := m.qualifiedName(name)
	if err != nil {
		return 0, false, err
	}
	data, err := os.ReadFile(m.vncfile(qname))
	if os.IsNotExist(err) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	port, err = strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		return 0, false, fmt.Errorf("bhyve: parsing recorded VNC port for %s: %w", qname, err)
	}
	return port, true, nil
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

	var vncPort int
	if cfg.EnableVNC {
		vncPort, err = m.allocateVNCPort()
		if err != nil {
			if tapName != "" {
				m.destroyTap(ctx, qname, tapName)
			}
			return err
		}
	}

	var nmdmUnit int
	haveNmdm := false
	if cfg.EnableSerialLog {
		nmdmUnit, err = m.allocateNmdm()
		if err != nil {
			if tapName != "" {
				m.destroyTap(ctx, qname, tapName)
			}
			return err
		}
		haveNmdm = true
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
		netArg := "5,virtio-net," + tapName
		if cfg.MACAddress != "" {
			netArg += ",mac=" + cfg.MACAddress
		}
		args = append(args, "-s", netArg)
	}
	if cfg.ISOPath != "" {
		args = append(args, "-s", "6,ahci-cd,"+cfg.ISOPath)
	}
	if cfg.InstallDiskPath != "" {
		args = append(args, "-s", "7,ahci-hd,"+cfg.InstallDiskPath)
	}
	if cfg.EnableVNC {
		args = append(args,
			"-s", fmt.Sprintf("29,fbuf,tcp=0.0.0.0:%d,w=1024,h=768", vncPort),
			"-s", "30,xhci,tablet",
		)
	}
	args = append(args,
		"-s", "31,lpc",
		"-l", "bootrom,"+cfg.BootROM,
	)
	if haveNmdm {
		// The guest gets the "B" end; a separate reader (started below,
		// once bhyve itself is up) drains the "A" end into a log file.
		// nmdm(4) device nodes are created on first open, no explicit
		// setup step needed.
		args = append(args, "-l", fmt.Sprintf("com1,/dev/nmdm%dB", nmdmUnit))
	}
	args = append(args, qname)

	if _, err := runCmd(ctx, "daemon", args...); err != nil {
		if tapName != "" {
			m.destroyTap(ctx, qname, tapName)
		}
		return err
	}

	if cfg.EnableVNC {
		if err := os.WriteFile(m.vncfile(qname), []byte(strconv.Itoa(vncPort)), 0o644); err != nil {
			// The VM is already running at this point - a failure to
			// record its VNC port just means the console won't be
			// discoverable until this is fixed, not a reason to tear the
			// VM back down.
			return fmt.Errorf("bhyve: VM created but recording VNC port failed: %w", err)
		}
	}

	if haveNmdm {
		if err := m.startSerialLogger(ctx, qname, nmdmUnit); err != nil {
			// Same reasoning as the VNC-port-recording failure above -
			// the VM is already running; a missing serial log just means
			// diagnostics are unavailable until this is fixed.
			return fmt.Errorf("bhyve: VM created but starting serial console logger failed: %w", err)
		}
	}
	return nil
}

// startSerialLogger starts a small detached reader (via daemon(8), same
// as bhyve itself) that opens the "A" end of qname's nmdm pair and
// appends everything it reads to qname's serial log file. Recorded via
// nmdmfile/serialpidfile so DestroyVM can find and stop it later,
// mirroring how tapfile/vncfile track bhyve's own per-VM state.
func (m *Manager) startSerialLogger(ctx context.Context, qname string, nmdmUnit int) error {
	if err := os.WriteFile(m.nmdmfile(qname), []byte(strconv.Itoa(nmdmUnit)), 0o644); err != nil {
		return fmt.Errorf("recording nmdm unit: %w", err)
	}
	if _, err := runCmd(ctx, "daemon",
		"-f",
		"-p", m.serialpidfile(qname),
		"-o", m.seriallogfile(qname),
		"cat", fmt.Sprintf("/dev/nmdm%dA", nmdmUnit),
	); err != nil {
		os.Remove(m.nmdmfile(qname))
		return fmt.Errorf("starting reader: %w", err)
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
	os.Remove(m.vncfile(qname))

	if data, err := os.ReadFile(m.serialpidfile(qname)); err == nil {
		if pid, err := strconv.Atoi(strings.TrimSpace(string(data))); err == nil {
			runCmd(ctx, "kill", strconv.Itoa(pid))
		}
	}
	os.Remove(m.serialpidfile(qname))
	// nmdmfile is removed (it only tracks a live allocation, freeing the
	// unit for reuse) but seriallogfile deliberately is not - it's the
	// one place a failed VM's boot/console output survives after
	// teardown, exactly when a diagnosis is most likely to be needed.
	os.Remove(m.nmdmfile(qname))
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
