package zfs

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"testing"
	"time"
)

// These tests exercise real zfs(8) and require a FreeBSD host with a test
// pool available. They're not run as part of `go test ./...` on an
// ordinary dev machine (no zfs binary there) — cross-compile
// (GOOS=freebsd GOARCH=amd64 go test -c ./internal/zfs) and run the
// resulting binary directly on a FreeBSD host instead. Set
// APIARY_ZFS_TEST_POOL to override the default test pool name.
func testManager(t *testing.T) *Manager {
	t.Helper()
	if _, err := exec.LookPath("zfs"); err != nil {
		t.Skip("zfs not available on this host; see package doc comment for how to run these tests")
	}

	pool := os.Getenv("APIARY_ZFS_TEST_POOL")
	if pool == "" {
		pool = "apiarytest"
	}
	base := fmt.Sprintf("%s/it-%d", pool, time.Now().UnixNano())

	m := New(base)
	ctx := context.Background()
	if _, err := runZFS(ctx, "create", "-p", base); err != nil {
		t.Fatalf("creating test base dataset %s: %v", base, err)
	}
	t.Cleanup(func() {
		runZFS(context.Background(), "destroy", "-r", base)
	})
	return m
}

func TestIntegration_DatasetLifecycle(t *testing.T) {
	m := testManager(t)
	ctx := context.Background()

	exists, err := m.DatasetExists(ctx, "vm-1")
	if err != nil {
		t.Fatalf("DatasetExists() error: %v", err)
	}
	if exists {
		t.Fatalf("DatasetExists() = true before creation")
	}

	if err := m.CreateDataset(ctx, "vm-1"); err != nil {
		t.Fatalf("CreateDataset() error: %v", err)
	}

	exists, err = m.DatasetExists(ctx, "vm-1")
	if err != nil {
		t.Fatalf("DatasetExists() error: %v", err)
	}
	if !exists {
		t.Fatalf("DatasetExists() = false after creation")
	}

	names, err := m.ListDatasets(ctx)
	if err != nil {
		t.Fatalf("ListDatasets() error: %v", err)
	}
	if len(names) != 1 || names[0] != "vm-1" {
		t.Errorf("ListDatasets() = %v, want [vm-1]", names)
	}

	if err := m.SetProperty(ctx, "vm-1", "compression", "lz4"); err != nil {
		t.Fatalf("SetProperty() error: %v", err)
	}
	val, err := m.GetProperty(ctx, "vm-1", "compression")
	if err != nil {
		t.Fatalf("GetProperty() error: %v", err)
	}
	if val != "lz4" {
		t.Errorf("GetProperty(compression) = %q, want %q", val, "lz4")
	}

	if err := m.DestroyDataset(ctx, "vm-1"); err != nil {
		t.Fatalf("DestroyDataset() error: %v", err)
	}

	exists, err = m.DatasetExists(ctx, "vm-1")
	if err != nil {
		t.Fatalf("DatasetExists() error: %v", err)
	}
	if exists {
		t.Fatalf("DatasetExists() = true after destroy")
	}
}

func TestIntegration_DestroyRefusesDatasetWithChild(t *testing.T) {
	m := testManager(t)
	ctx := context.Background()

	if err := m.CreateDataset(ctx, "parent"); err != nil {
		t.Fatalf("CreateDataset(parent) error: %v", err)
	}
	if err := m.CreateDataset(ctx, "parent/child"); err != nil {
		t.Fatalf("CreateDataset(parent/child) error: %v", err)
	}

	if err := m.DestroyDataset(ctx, "parent"); err == nil {
		t.Fatalf("DestroyDataset(parent) succeeded despite having a child, want an error")
	}

	// Verify it's still there (destroy didn't partially apply).
	exists, err := m.DatasetExists(ctx, "parent")
	if err != nil {
		t.Fatalf("DatasetExists() error: %v", err)
	}
	if !exists {
		t.Fatalf("parent dataset gone after a refused destroy")
	}
}

func TestIntegration_CloneFromSnapshot(t *testing.T) {
	m := testManager(t)
	ctx := context.Background()

	if err := m.CreateDataset(ctx, "templates/freebsd-14"); err != nil {
		t.Fatalf("CreateDataset(templates/freebsd-14) error: %v", err)
	}

	exists, err := m.SnapshotExists(ctx, "templates/freebsd-14@apiary-template")
	if err != nil {
		t.Fatalf("SnapshotExists() error: %v", err)
	}
	if exists {
		t.Fatalf("SnapshotExists() = true before the snapshot was taken")
	}

	if _, err := runZFS(ctx, "snapshot", m.Base+"/templates/freebsd-14@apiary-template"); err != nil {
		t.Fatalf("taking template snapshot: %v", err)
	}

	exists, err = m.SnapshotExists(ctx, "templates/freebsd-14@apiary-template")
	if err != nil {
		t.Fatalf("SnapshotExists() error: %v", err)
	}
	if !exists {
		t.Fatalf("SnapshotExists() = false after the snapshot was taken")
	}

	if err := m.Clone(ctx, "templates/freebsd-14@apiary-template", "jail-1"); err != nil {
		t.Fatalf("Clone() error: %v", err)
	}

	cloned, err := m.DatasetExists(ctx, "jail-1")
	if err != nil {
		t.Fatalf("DatasetExists(jail-1) error: %v", err)
	}
	if !cloned {
		t.Fatalf("DatasetExists(jail-1) = false after Clone()")
	}

	// The clone is an independent dataset from the caller's own point of
	// view - DestroyDataset works on it exactly like any dataset created
	// directly, even though the template snapshot it came from is still
	// held (this destroy would need "-r" on the template dataset, not
	// the clone, if it ever failed - the clone itself has no children).
	if err := m.DestroyDataset(ctx, "jail-1"); err != nil {
		t.Fatalf("DestroyDataset(jail-1) error: %v", err)
	}
}

// TestIntegration_SnapshotCreateRestoreDestroy exercises the full
// checkpoint/rollback cycle (ADR-0090): create a VM-shaped dataset,
// write something, snapshot it, overwrite that content, roll back, and
// confirm the original content actually came back - not just that the
// commands returned no error.
func TestIntegration_SnapshotCreateRestoreDestroy(t *testing.T) {
	m := testManager(t)
	ctx := context.Background()

	if err := m.CreateDataset(ctx, "vm-1"); err != nil {
		t.Fatalf("CreateDataset(vm-1) error: %v", err)
	}
	mountpoint, err := m.GetProperty(ctx, "vm-1", "mountpoint")
	if err != nil {
		t.Fatalf("GetProperty(mountpoint) error: %v", err)
	}
	diskPath := mountpoint + "/disk.img"
	if err := os.WriteFile(diskPath, []byte("original content"), 0o644); err != nil {
		t.Fatalf("writing original content: %v", err)
	}

	names, err := m.ListSnapshots(ctx, "vm-1")
	if err != nil {
		t.Fatalf("ListSnapshots() error: %v", err)
	}
	if len(names) != 0 {
		t.Fatalf("ListSnapshots() = %v, want none before any snapshot exists", names)
	}

	if err := m.CreateSnapshot(ctx, "vm-1@before-migration"); err != nil {
		t.Fatalf("CreateSnapshot() error: %v", err)
	}

	names, err = m.ListSnapshots(ctx, "vm-1")
	if err != nil {
		t.Fatalf("ListSnapshots() error: %v", err)
	}
	if len(names) != 1 || names[0] != "before-migration" {
		t.Fatalf("ListSnapshots() = %v, want [before-migration]", names)
	}

	if err := os.WriteFile(diskPath, []byte("corrupted by a bad migration"), 0o644); err != nil {
		t.Fatalf("overwriting with corrupted content: %v", err)
	}

	if err := m.RollbackSnapshot(ctx, "vm-1@before-migration"); err != nil {
		t.Fatalf("RollbackSnapshot() error: %v", err)
	}

	restored, err := os.ReadFile(diskPath)
	if err != nil {
		t.Fatalf("reading restored content: %v", err)
	}
	if string(restored) != "original content" {
		t.Errorf("restored content = %q, want %q", restored, "original content")
	}

	if err := m.DestroySnapshot(ctx, "vm-1@before-migration"); err != nil {
		t.Fatalf("DestroySnapshot() error: %v", err)
	}
	names, err = m.ListSnapshots(ctx, "vm-1")
	if err != nil {
		t.Fatalf("ListSnapshots() error: %v", err)
	}
	if len(names) != 0 {
		t.Errorf("ListSnapshots() = %v, want none after DestroySnapshot()", names)
	}
}

// TestIntegration_RollbackRefusesWithNewerSnapshot guards the
// deliberate choice not to pass zfs rollback's own -r flag: rolling
// back past a newer snapshot must fail loudly, never silently destroy
// it.
func TestIntegration_RollbackRefusesWithNewerSnapshot(t *testing.T) {
	m := testManager(t)
	ctx := context.Background()

	if err := m.CreateDataset(ctx, "vm-1"); err != nil {
		t.Fatalf("CreateDataset(vm-1) error: %v", err)
	}
	if err := m.CreateSnapshot(ctx, "vm-1@first"); err != nil {
		t.Fatalf("CreateSnapshot(first) error: %v", err)
	}
	if err := m.CreateSnapshot(ctx, "vm-1@second"); err != nil {
		t.Fatalf("CreateSnapshot(second) error: %v", err)
	}

	if err := m.RollbackSnapshot(ctx, "vm-1@first"); err == nil {
		t.Fatal("RollbackSnapshot(first) succeeded despite a newer snapshot existing, want a refusal")
	}

	names, err := m.ListSnapshots(ctx, "vm-1")
	if err != nil {
		t.Fatalf("ListSnapshots() error: %v", err)
	}
	if len(names) != 2 {
		t.Errorf("ListSnapshots() = %v, want both snapshots still present after a refused rollback", names)
	}
}

func TestSnapshotPath_RejectsInvalidNames(t *testing.T) {
	m := New("apiarytest/base")
	for _, name := range []string{"", "no-at-sign", "@missing-dataset", "templates/x@", "templates/x@a/b", "../escape@snap"} {
		if _, err := m.snapshotPath(name); err == nil {
			t.Errorf("snapshotPath(%q) = nil error, want rejection", name)
		}
	}
	got, err := m.snapshotPath("templates/freebsd-14@apiary-template")
	if err != nil {
		t.Fatalf("snapshotPath() error: %v", err)
	}
	if want := "apiarytest/base/templates/freebsd-14@apiary-template"; got != want {
		t.Errorf("snapshotPath() = %q, want %q", got, want)
	}
}

func TestPath_RejectsEscapeAttempts(t *testing.T) {
	m := New("apiarytest/base")
	for _, name := range []string{"", "..", "../escape", "a/../../etc", "/absolute", "a//b"} {
		if _, err := m.path(name); err == nil {
			t.Errorf("path(%q) = nil error, want rejection", name)
		}
	}
}
