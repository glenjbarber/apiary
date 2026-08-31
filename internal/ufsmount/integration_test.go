// Real integration tests against actual FreeBSD newfs(8)/mount(8)/
// umount(8). Skip cleanly on a dev machine without those tools -
// cross-compile (GOOS=freebsd GOARCH=amd64 go test -c
// ./internal/ufsmount) and run the resulting binary on a real FreeBSD
// host to exercise these for real, mirroring internal/hast's own
// integration test pattern.
package ufsmount

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func requireTools(t *testing.T) {
	t.Helper()
	for _, tool := range []string{"newfs", "mount", "umount", "dumpfs"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not available on this host; see package doc comment", tool)
		}
	}
}

// TestIntegration_FormatMountUnmount exercises the full real lifecycle
// against a memory-backed md(4) device, so it needs no spare real disk
// - the same approach this project's own live HAST debugging used to
// isolate zvol-vs-file/md(4) provider behavior (see ADR-0026).
func TestIntegration_FormatMountUnmount(t *testing.T) {
	requireTools(t)
	if os.Geteuid() != 0 {
		t.Skip("requires root to attach an md(4) device")
	}

	out, err := runCmd(context.Background(), "mdconfig", "-a", "-t", "swap", "-s", "64m")
	if err != nil {
		t.Fatalf("mdconfig -a: %v", err)
	}
	mdName := out
	device := "/dev/" + mdName
	t.Cleanup(func() { _, _ = runCmd(context.Background(), "mdconfig", "-d", "-u", mdName) })

	m := New()
	ctx := context.Background()

	if m.IsFormatted(ctx, device) {
		t.Fatalf("IsFormatted(%s) = true before any newfs", device)
	}

	if err := m.FormatIfNeeded(ctx, device); err != nil {
		t.Fatalf("FormatIfNeeded() error: %v", err)
	}
	if !m.IsFormatted(ctx, device) {
		t.Fatalf("IsFormatted(%s) = false after FormatIfNeeded", device)
	}

	// Calling FormatIfNeeded again must be a no-op (idempotent), not a
	// re-format that would wipe whatever's written below.
	if err := m.FormatIfNeeded(ctx, device); err != nil {
		t.Fatalf("FormatIfNeeded() (second call) error: %v", err)
	}

	mountPoint := filepath.Join(t.TempDir(), "mnt")
	t.Cleanup(func() { _ = m.Unmount(ctx, mountPoint) })

	if err := m.Mount(ctx, device, mountPoint); err != nil {
		t.Fatalf("Mount() error: %v", err)
	}
	mounted, err := m.IsMounted(ctx, mountPoint)
	if err != nil {
		t.Fatalf("IsMounted() error: %v", err)
	}
	if !mounted {
		t.Fatalf("IsMounted(%s) = false after Mount", mountPoint)
	}

	testFile := filepath.Join(mountPoint, "hello")
	if err := os.WriteFile(testFile, []byte("hello"), 0o644); err != nil {
		t.Fatalf("writing test file: %v", err)
	}

	if err := m.Unmount(ctx, mountPoint); err != nil {
		t.Fatalf("Unmount() error: %v", err)
	}
	mounted, err = m.IsMounted(ctx, mountPoint)
	if err != nil {
		t.Fatalf("IsMounted() (after unmount) error: %v", err)
	}
	if mounted {
		t.Fatalf("IsMounted(%s) = true after Unmount", mountPoint)
	}

	// Unmounting an already-unmounted path must be a no-op, not an error.
	if err := m.Unmount(ctx, mountPoint); err != nil {
		t.Fatalf("Unmount() (second call) error: %v", err)
	}
}
