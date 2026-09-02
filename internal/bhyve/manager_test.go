package bhyve

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
)

// These exercise VNC port bookkeeping (allocateVNCPort/VNCPort) directly
// against a temp RunDir, without touching real bhyve(8)/vmm(4) - unlike
// the rest of this package's tests (integration_test.go), which need a
// real FreeBSD host and are skipped everywhere else.

func TestVNCPort_NoRecordedPortReturnsNotOK(t *testing.T) {
	m := &Manager{Prefix: "test-", RunDir: t.TempDir()}

	port, ok, err := m.VNCPort("vm-1")
	if err != nil {
		t.Fatalf("VNCPort() error: %v", err)
	}
	if ok {
		t.Errorf("VNCPort() = (%d, true), want ok=false for a VM with no recorded port", port)
	}
}

func TestVNCPort_ReadsWhatWasRecorded(t *testing.T) {
	m := &Manager{Prefix: "test-", RunDir: t.TempDir()}
	if err := os.MkdirAll(m.runDir(), 0o755); err != nil {
		t.Fatalf("MkdirAll() error: %v", err)
	}
	if err := os.WriteFile(m.vncfile("test-vm-1"), []byte("5905"), 0o644); err != nil {
		t.Fatalf("WriteFile() error: %v", err)
	}

	port, ok, err := m.VNCPort("vm-1")
	if err != nil {
		t.Fatalf("VNCPort() error: %v", err)
	}
	if !ok || port != 5905 {
		t.Errorf("VNCPort() = (%d, %v), want (5905, true)", port, ok)
	}
}

func TestAllocateVNCPort_PicksLowestFree(t *testing.T) {
	m := &Manager{Prefix: "test-", RunDir: t.TempDir()}
	if err := os.MkdirAll(m.runDir(), 0o755); err != nil {
		t.Fatalf("MkdirAll() error: %v", err)
	}
	// Occupy the first two ports in the range with unrelated VM names.
	for i, used := range []int{vncBasePort, vncBasePort + 1} {
		name := filepath.Join(m.runDir(), fmt.Sprintf("other-%d.vnc", i))
		if err := os.WriteFile(name, []byte(strconv.Itoa(used)), 0o644); err != nil {
			t.Fatalf("WriteFile() error: %v", err)
		}
	}

	port, err := m.allocateVNCPort()
	if err != nil {
		t.Fatalf("allocateVNCPort() error: %v", err)
	}
	if port != vncBasePort+2 {
		t.Errorf("allocateVNCPort() = %d, want %d (lowest free)", port, vncBasePort+2)
	}
}

func TestAllocateVNCPort_ExhaustedRangeErrors(t *testing.T) {
	m := &Manager{Prefix: "test-", RunDir: t.TempDir()}
	if err := os.MkdirAll(m.runDir(), 0o755); err != nil {
		t.Fatalf("MkdirAll() error: %v", err)
	}
	for i := 0; i < vncPortRange; i++ {
		name := filepath.Join(m.runDir(), fmt.Sprintf("vm-%d.vnc", i))
		if err := os.WriteFile(name, []byte(strconv.Itoa(vncBasePort+i)), 0o644); err != nil {
			t.Fatalf("WriteFile() error: %v", err)
		}
	}

	if _, err := m.allocateVNCPort(); err == nil {
		t.Errorf("allocateVNCPort() = nil error, want an error once every port in range is used")
	}
}

// These mirror the VNC tests above exactly, for the analogous
// nmdm-unit/serial-log bookkeeping (allocateNmdm/SerialLogPath).

func TestSerialLogPath_NoRecordedLogReturnsNotOK(t *testing.T) {
	m := &Manager{Prefix: "test-", RunDir: t.TempDir()}

	path, ok, err := m.SerialLogPath("vm-1")
	if err != nil {
		t.Fatalf("SerialLogPath() error: %v", err)
	}
	if ok {
		t.Errorf("SerialLogPath() = (%q, true), want ok=false for a VM with no recorded log", path)
	}
}

func TestSerialLogPath_FoundOnceLogFileExists(t *testing.T) {
	m := &Manager{Prefix: "test-", RunDir: t.TempDir()}
	if err := os.MkdirAll(m.runDir(), 0o755); err != nil {
		t.Fatalf("MkdirAll() error: %v", err)
	}
	if err := os.WriteFile(m.seriallogfile("test-vm-1"), []byte("boot output\n"), 0o644); err != nil {
		t.Fatalf("WriteFile() error: %v", err)
	}

	path, ok, err := m.SerialLogPath("vm-1")
	if err != nil {
		t.Fatalf("SerialLogPath() error: %v", err)
	}
	if !ok || path != m.seriallogfile("test-vm-1") {
		t.Errorf("SerialLogPath() = (%q, %v), want (%q, true)", path, ok, m.seriallogfile("test-vm-1"))
	}
}

func TestAllocateNmdm_PicksLowestFree(t *testing.T) {
	m := &Manager{Prefix: "test-", RunDir: t.TempDir()}
	if err := os.MkdirAll(m.runDir(), 0o755); err != nil {
		t.Fatalf("MkdirAll() error: %v", err)
	}
	for i, used := range []int{0, 1} {
		name := filepath.Join(m.runDir(), fmt.Sprintf("other-%d.nmdm", i))
		if err := os.WriteFile(name, []byte(strconv.Itoa(used)), 0o644); err != nil {
			t.Fatalf("WriteFile() error: %v", err)
		}
	}

	unit, err := m.allocateNmdm()
	if err != nil {
		t.Fatalf("allocateNmdm() error: %v", err)
	}
	if unit != 2 {
		t.Errorf("allocateNmdm() = %d, want 2 (lowest free)", unit)
	}
}

func TestAllocateNmdm_ExhaustedRangeErrors(t *testing.T) {
	m := &Manager{Prefix: "test-", RunDir: t.TempDir()}
	if err := os.MkdirAll(m.runDir(), 0o755); err != nil {
		t.Fatalf("MkdirAll() error: %v", err)
	}
	for i := 0; i < nmdmRange; i++ {
		name := filepath.Join(m.runDir(), fmt.Sprintf("vm-%d.nmdm", i))
		if err := os.WriteFile(name, []byte(strconv.Itoa(i)), 0o644); err != nil {
			t.Fatalf("WriteFile() error: %v", err)
		}
	}

	if _, err := m.allocateNmdm(); err == nil {
		t.Errorf("allocateNmdm() = nil error, want an error once every unit in range is used")
	}
}

// The following exercise processAlive directly - the pure-Go half of
// the ADR-0043 VMExists fix (checking whether a pidfile names a process
// that's actually still running, not just whether the file exists).
// The other half (destroying a stale vmm(4) context via bhyvectl when
// the process is dead) still needs real hardware, covered by this
// package's own integration tests instead.

func TestProcessAlive_NoRecordedPidfileReturnsFalse(t *testing.T) {
	m := &Manager{Prefix: "test-", RunDir: t.TempDir()}

	alive, err := m.processAlive("test-vm-1")
	if err != nil {
		t.Fatalf("processAlive() error: %v", err)
	}
	if alive {
		t.Error("processAlive() = true, want false for a VM with no recorded pidfile")
	}
}

func TestProcessAlive_GenuinelyRunningProcessReturnsTrue(t *testing.T) {
	m := &Manager{Prefix: "test-", RunDir: t.TempDir()}
	if err := os.MkdirAll(m.runDir(), 0o755); err != nil {
		t.Fatalf("MkdirAll() error: %v", err)
	}
	// The test process itself is guaranteed to be alive for the
	// duration of this test.
	if err := os.WriteFile(m.pidfile("test-vm-1"), []byte(strconv.Itoa(os.Getpid())), 0o644); err != nil {
		t.Fatalf("WriteFile() error: %v", err)
	}

	alive, err := m.processAlive("test-vm-1")
	if err != nil {
		t.Fatalf("processAlive() error: %v", err)
	}
	if !alive {
		t.Error("processAlive() = false, want true for this test process's own genuinely-running pid")
	}
}

func TestProcessAlive_ExitedProcessReturnsFalse(t *testing.T) {
	m := &Manager{Prefix: "test-", RunDir: t.TempDir()}
	if err := os.MkdirAll(m.runDir(), 0o755); err != nil {
		t.Fatalf("MkdirAll() error: %v", err)
	}
	// Run a real short-lived process to completion first, so its pid is
	// guaranteed to no longer exist - this is exactly the ADR-0043
	// scenario: a recorded pid whose process has since exited.
	cmd := exec.Command("true")
	if err := cmd.Run(); err != nil {
		t.Fatalf("running throwaway process: %v", err)
	}
	if err := os.WriteFile(m.pidfile("test-vm-1"), []byte(strconv.Itoa(cmd.Process.Pid)), 0o644); err != nil {
		t.Fatalf("WriteFile() error: %v", err)
	}

	alive, err := m.processAlive("test-vm-1")
	if err != nil {
		t.Fatalf("processAlive() error: %v", err)
	}
	if alive {
		t.Error("processAlive() = true, want false for a pid whose process has already exited")
	}
}

func TestProcessAlive_GarbagePidfileContentReturnsFalse(t *testing.T) {
	m := &Manager{Prefix: "test-", RunDir: t.TempDir()}
	if err := os.MkdirAll(m.runDir(), 0o755); err != nil {
		t.Fatalf("MkdirAll() error: %v", err)
	}
	if err := os.WriteFile(m.pidfile("test-vm-1"), []byte("not-a-pid"), 0o644); err != nil {
		t.Fatalf("WriteFile() error: %v", err)
	}

	alive, err := m.processAlive("test-vm-1")
	if err != nil {
		t.Fatalf("processAlive() error: %v", err)
	}
	if alive {
		t.Error("processAlive() = true, want false for unparseable pidfile content")
	}
}
