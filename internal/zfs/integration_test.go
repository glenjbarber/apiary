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

func TestPath_RejectsEscapeAttempts(t *testing.T) {
	m := New("apiarytest/base")
	for _, name := range []string{"", "..", "../escape", "a/../../etc", "/absolute", "a//b"} {
		if _, err := m.path(name); err == nil {
			t.Errorf("path(%q) = nil error, want rejection", name)
		}
	}
}
