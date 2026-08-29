package jail

import (
	"context"
	"fmt"
	"os/exec"
	"testing"
	"time"
)

// These tests exercise real jail(8)/jls(8) and require root - unlike
// internal/zfs, FreeBSD has no delegation mechanism for jail creation
// (it's an unconditional PRIV_JAIL_SET/PRIV_JAIL_REMOVE check), so the
// test binary itself must run as root. Cross-compile
// (GOOS=freebsd GOARCH=amd64 go test -c ./internal/jail) and run the
// resulting binary as root on a FreeBSD host.
func testManager(t *testing.T) (*Manager, string) {
	t.Helper()
	if _, err := exec.LookPath("jail"); err != nil {
		t.Skip("jail(8) not available on this host; see package doc comment for how to run these tests")
	}

	prefix := fmt.Sprintf("apiary-it-%d-", time.Now().UnixNano())
	root := t.TempDir()

	return New(prefix), root
}

func TestIntegration_JailLifecycle(t *testing.T) {
	m, root := testManager(t)
	ctx := context.Background()

	exists, err := m.JailExists(ctx, "web-1")
	if err != nil {
		t.Fatalf("JailExists() error: %v", err)
	}
	if exists {
		t.Fatalf("JailExists() = true before creation")
	}

	cfg := Config{Path: root, Hostname: "web-1.apiary.test"}
	if err := m.CreateJail(ctx, "web-1", cfg); err != nil {
		t.Fatalf("CreateJail() error: %v", err)
	}
	t.Cleanup(func() { m.RemoveJail(context.Background(), "web-1") })

	exists, err = m.JailExists(ctx, "web-1")
	if err != nil {
		t.Fatalf("JailExists() error: %v", err)
	}
	if !exists {
		t.Fatalf("JailExists() = false after creation")
	}

	info, err := m.JailInfo(ctx, "web-1")
	if err != nil {
		t.Fatalf("JailInfo() error: %v", err)
	}
	if info.Name != "web-1" {
		t.Errorf("JailInfo().Name = %q, want %q", info.Name, "web-1")
	}
	if info.Path != root {
		t.Errorf("JailInfo().Path = %q, want %q", info.Path, root)
	}
	if info.Hostname != "web-1.apiary.test" {
		t.Errorf("JailInfo().Hostname = %q, want %q", info.Hostname, "web-1.apiary.test")
	}
	if info.JID <= 0 {
		t.Errorf("JailInfo().JID = %d, want a positive JID", info.JID)
	}

	names, err := m.ListJails(ctx)
	if err != nil {
		t.Fatalf("ListJails() error: %v", err)
	}
	if len(names) != 1 || names[0] != "web-1" {
		t.Errorf("ListJails() = %v, want [web-1]", names)
	}

	if err := m.RemoveJail(ctx, "web-1"); err != nil {
		t.Fatalf("RemoveJail() error: %v", err)
	}

	exists, err = m.JailExists(ctx, "web-1")
	if err != nil {
		t.Fatalf("JailExists() error: %v", err)
	}
	if exists {
		t.Fatalf("JailExists() = true after removal")
	}
}

func TestIntegration_ListJailsOnlyReturnsOwnPrefix(t *testing.T) {
	m, root := testManager(t)
	ctx := context.Background()

	// A jail created directly (not through m, so a different prefix)
	// must not show up in m.ListJails().
	other := New("other-prefix-")
	if err := other.CreateJail(ctx, "unrelated", Config{Path: root, Hostname: "unrelated"}); err != nil {
		t.Fatalf("CreateJail() (other) error: %v", err)
	}
	t.Cleanup(func() { other.RemoveJail(context.Background(), "unrelated") })

	if err := m.CreateJail(ctx, "mine", Config{Path: root, Hostname: "mine"}); err != nil {
		t.Fatalf("CreateJail() (mine) error: %v", err)
	}
	t.Cleanup(func() { m.RemoveJail(context.Background(), "mine") })

	names, err := m.ListJails(ctx)
	if err != nil {
		t.Fatalf("ListJails() error: %v", err)
	}
	if len(names) != 1 || names[0] != "mine" {
		t.Errorf("ListJails() = %v, want [mine] (unrelated jail must be filtered out)", names)
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
