package bhyve

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// These tests exercise real bhyve(8)/bhyvectl(8) and require a host with
// hardware-assisted virtualization (VT-x+EPT on Intel, AMD-V+RVI on AMD)
// and the vmm(4) kernel module loaded, plus root (vmm access, like
// jail(8), is a superuser-only operation with no delegation mechanism).
// Cross-compile (GOOS=freebsd GOARCH=amd64 go test -c ./internal/bhyve)
// and run the resulting binary as root on such a host. Set
// APIARY_BHYVE_TEST_BOOTROM to the UEFI firmware path if it isn't at the
// default location.
func testManager(t *testing.T) (*Manager, Config) {
	t.Helper()
	if _, err := exec.LookPath("bhyve"); err != nil {
		t.Skip("bhyve not available on this host; see package doc comment for how to run these tests")
	}
	// /dev/vmm itself is created lazily on first VM, not at module load,
	// so check kldstat rather than the directory's existence.
	if out, err := exec.Command("kldstat").CombinedOutput(); err != nil || !strings.Contains(string(out), "vmm.ko") {
		t.Skip("vmm(4) not loaded (kldload vmm)")
	}

	bootrom := os.Getenv("APIARY_BHYVE_TEST_BOOTROM")
	if bootrom == "" {
		bootrom = "/usr/local/share/uefi-firmware/BHYVE_UEFI.fd"
	}
	if _, err := os.Stat(bootrom); err != nil {
		t.Skipf("boot ROM %s not present; see package doc comment", bootrom)
	}

	prefix := fmt.Sprintf("apiary-it-%d-", time.Now().UnixNano())
	m := New(prefix)
	m.RunDir = t.TempDir()
	return m, Config{CPUs: 1, MemoryMB: 256, BootROM: bootrom}
}

func TestIntegration_VMLifecycle(t *testing.T) {
	m, cfg := testManager(t)
	ctx := context.Background()

	exists, err := m.VMExists(ctx, "vm-1")
	if err != nil {
		t.Fatalf("VMExists() error: %v", err)
	}
	if exists {
		t.Fatalf("VMExists() = true before creation")
	}

	if err := m.CreateVM(ctx, "vm-1", cfg); err != nil {
		t.Fatalf("CreateVM() error: %v", err)
	}
	t.Cleanup(func() { m.DestroyVM(context.Background(), "vm-1") })

	// The daemonized bhyve process needs a moment to actually open the
	// vmm device after daemon(8) forks it.
	var exists2 bool
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		exists2, err = m.VMExists(ctx, "vm-1")
		if err != nil {
			t.Fatalf("VMExists() error: %v", err)
		}
		if exists2 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !exists2 {
		t.Fatalf("VMExists() = false after creation (timed out waiting)")
	}

	names, err := m.ListVMs(ctx)
	if err != nil {
		t.Fatalf("ListVMs() error: %v", err)
	}
	if len(names) != 1 || names[0] != "vm-1" {
		t.Errorf("ListVMs() = %v, want [vm-1]", names)
	}

	if err := m.DestroyVM(ctx, "vm-1"); err != nil {
		t.Fatalf("DestroyVM() error: %v", err)
	}

	exists, err = m.VMExists(ctx, "vm-1")
	if err != nil {
		t.Fatalf("VMExists() error: %v", err)
	}
	if exists {
		t.Fatalf("VMExists() = true after destroy")
	}
}

func TestIntegration_ListVMsOnlyReturnsOwnPrefix(t *testing.T) {
	m, cfg := testManager(t)
	ctx := context.Background()

	other := New("other-prefix-")
	other.RunDir = t.TempDir()
	if err := other.CreateVM(ctx, "unrelated", cfg); err != nil {
		t.Fatalf("CreateVM() (other) error: %v", err)
	}
	t.Cleanup(func() { other.DestroyVM(context.Background(), "unrelated") })

	if err := m.CreateVM(ctx, "mine", cfg); err != nil {
		t.Fatalf("CreateVM() (mine) error: %v", err)
	}
	t.Cleanup(func() { m.DestroyVM(context.Background(), "mine") })

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		exists, _ := m.VMExists(ctx, "mine")
		if exists {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	names, err := m.ListVMs(ctx)
	if err != nil {
		t.Fatalf("ListVMs() error: %v", err)
	}
	if len(names) != 1 || names[0] != "mine" {
		t.Errorf("ListVMs() = %v, want [mine] (unrelated VM must be filtered out)", names)
	}
}

func TestIntegration_VMWithDisk(t *testing.T) {
	m, cfg := testManager(t)
	ctx := context.Background()

	diskPath := t.TempDir() + "/disk.img"
	f, err := os.Create(diskPath)
	if err != nil {
		t.Fatalf("creating disk image: %v", err)
	}
	if err := f.Truncate(64 * 1024 * 1024); err != nil {
		t.Fatalf("truncating disk image: %v", err)
	}
	f.Close()

	cfg.DiskPath = diskPath
	if err := m.CreateVM(ctx, "vm-disk", cfg); err != nil {
		t.Fatalf("CreateVM() with disk error: %v", err)
	}
	t.Cleanup(func() { m.DestroyVM(context.Background(), "vm-disk") })

	deadline := time.Now().Add(5 * time.Second)
	var exists bool
	for time.Now().Before(deadline) {
		exists, err = m.VMExists(ctx, "vm-disk")
		if err != nil {
			t.Fatalf("VMExists() error: %v", err)
		}
		if exists {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !exists {
		t.Fatalf("VMExists() = false after creation with a disk attached (timed out waiting)")
	}
}

func TestQualifiedName_RejectsInvalidCharacters(t *testing.T) {
	m := New("apiary-")
	for _, name := range []string{"", "has space", "has.dot", "has/slash", "has\"quote"} {
		if _, err := m.qualifiedName(name); err == nil {
			t.Errorf("qualifiedName(%q) = nil error, want rejection", name)
		}
	}
}
