// Package ufsmount formats and mounts a raw block device as UFS,
// wrapping newfs(8)/mount(8)/umount(8). It exists specifically for
// jail orchestration's HAST-replicated roots (ADR-0026): a HAST device
// (/dev/hast/<name>) is a raw block device, not a filesystem - unlike
// a bhyve VM's disk (internal/bhyve), which uses a raw device directly,
// a jail must chroot into a real mounted filesystem tree. A
// non-replicated jail's root is a plain ZFS dataset instead (already
// mounted by ZFS itself), so this package is only ever needed on the
// replicated path.
package ufsmount

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
)

// Manager has no state of its own - every method operates purely on
// its arguments, mirroring internal/hast.Manager's own stateless shape
// where the caller (internal/cluster's Reconciler) owns all identity.
type Manager struct{}

// New returns a Manager.
func New() *Manager {
	return &Manager{}
}

// IsFormatted reports whether devicePath already holds a UFS
// filesystem, checked via dumpfs(8) (read-only - never risks writing
// to a device that might already hold real data). Any failure
// (dumpfs's own "not a valid filesystem" error, or a device that
// doesn't exist yet) is reported as "not formatted" rather than an
// error: the caller's response either way is the same (call
// FormatIfNeeded), and a false negative here just costs one redundant,
// idempotent newfs - safer than treating an ambiguous result as
// "already formatted" and skipping a needed one.
func (m *Manager) IsFormatted(ctx context.Context, devicePath string) bool {
	cmd := exec.CommandContext(ctx, "dumpfs", devicePath)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return false
	}
	return strings.Contains(string(out), "magic")
}

// FormatIfNeeded runs newfs(8) on devicePath unless IsFormatted already
// reports it holds a UFS filesystem - newfs itself has no built-in
// idempotency guard (running it again would silently wipe existing
// data), so this check is load-bearing, not a convenience.
func (m *Manager) FormatIfNeeded(ctx context.Context, devicePath string) error {
	if m.IsFormatted(ctx, devicePath) {
		return nil
	}
	_, err := runCmd(ctx, "newfs", devicePath)
	return err
}

// IsMounted reports whether mountPoint currently has anything mounted
// on it, via mount(8)'s own listing (avoids assuming devicePath is the
// only thing that could ever be mounted there).
func (m *Manager) IsMounted(ctx context.Context, mountPoint string) (bool, error) {
	out, err := runCmd(ctx, "mount", "-p")
	if err != nil {
		return false, err
	}
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[1] == mountPoint {
			return true, nil
		}
	}
	return false, nil
}

// Mount mounts devicePath (already UFS-formatted - see FormatIfNeeded)
// at mountPoint, creating mountPoint if it doesn't exist. A no-op if
// mountPoint already has something mounted on it - see IsMounted.
func (m *Manager) Mount(ctx context.Context, devicePath, mountPoint string) error {
	mounted, err := m.IsMounted(ctx, mountPoint)
	if err != nil {
		return err
	}
	if mounted {
		return nil
	}
	if err := runMkdir(ctx, mountPoint); err != nil {
		return err
	}
	_, err = runCmd(ctx, "mount", devicePath, mountPoint)
	return err
}

// Unmount unmounts mountPoint if it's currently mounted - a no-op
// otherwise, mirroring the exists-check-first pattern every other
// teardown path in this project follows (e.g.
// internal/cluster.teardownVM).
func (m *Manager) Unmount(ctx context.Context, mountPoint string) error {
	mounted, err := m.IsMounted(ctx, mountPoint)
	if err != nil {
		return err
	}
	if !mounted {
		return nil
	}
	_, err = runCmd(ctx, "umount", mountPoint)
	return err
}

func runMkdir(ctx context.Context, path string) error {
	_, err := runCmd(ctx, "mkdir", "-p", path)
	return err
}

// runCmd executes `name args...`, returning trimmed stdout. On
// failure, the returned error includes the command's own stderr
// output - mirrors internal/jail and internal/hast's own runCmd.
func runCmd(ctx context.Context, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return "", fmt.Errorf("%s %s: %s", name, strings.Join(args, " "), msg)
	}
	return strings.TrimSpace(stdout.String()), nil
}
