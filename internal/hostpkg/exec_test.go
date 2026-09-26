package hostpkg

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The real Runner is exercised here even though this host is not
// FreeBSD. What is being checked is the plumbing - that the real
// implementation really executes, really separates stdout from stderr,
// and really surfaces stderr on failure - not anything pkg-specific,
// which no macOS test could establish and which this package's parsers
// therefore always fail closed on.
func TestExecRunnerRunsAndSeparatesStreams(t *testing.T) {
	stdout, stderr, err := NewExecRunner().Run(context.Background(), "/bin/echo", "hello")
	if err != nil {
		t.Fatalf("Run() error: %v", err)
	}
	if strings.TrimSpace(stdout) != "hello" {
		t.Errorf("stdout = %q, want %q", stdout, "hello")
	}
	if stderr != "" {
		t.Errorf("stderr = %q, want empty", stderr)
	}
}

func TestExecRunnerSurfacesFailure(t *testing.T) {
	_, _, err := NewExecRunner().Run(context.Background(), "/nonexistent/binary")
	if err == nil {
		t.Fatal("Run() of a missing binary returned no error")
	}
	// The error text must name what was actually run, so a host-level
	// failure is diagnosable from a page without a shell.
	if !strings.Contains(err.Error(), "binary") {
		t.Errorf("error = %q, want it to name the command", err)
	}
}

func TestExecRunnerRespectsContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := NewExecRunner().Run(ctx, "/bin/echo", "hello"); err == nil {
		t.Error("a cancelled context still ran the command")
	}
}

func TestOSStatterReadsModTime(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "local.sqlite")
	if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
		t.Fatalf("writing fixture: %v", err)
	}
	got, err := osStatter{}.ModTime(path)
	if err != nil {
		t.Fatalf("ModTime() error: %v", err)
	}
	if time.Since(got) > time.Hour {
		t.Errorf("ModTime() = %s, which is not a plausible mtime for a file just written", got)
	}
	if _, err := (osStatter{}).ModTime(filepath.Join(dir, "absent")); err == nil {
		t.Error("ModTime() of a missing file returned no error; the caller relies on this failing to report CatalogueUnknown")
	}
}

// The real Privilege check is a pure read of the effective UID, so it is
// safe to run on any host. What matters is that its detail names the
// value it actually observed, since that string is what a refusal
// shows an operator.
func TestEUIDPrivilegeDescribesItself(t *testing.T) {
	root, detail := NewEUIDPrivilege().Describe()
	if detail == "" {
		t.Error("Describe() returned no detail; a refusal would show nothing")
	}
	if !strings.Contains(detail, "uid") {
		t.Errorf("detail = %q, want it to name the uid that was read", detail)
	}
	wantRoot := os.Geteuid() == 0
	if root != wantRoot {
		t.Errorf("Describe() root = %t, want %t on a process with euid %d", root, wantRoot, os.Geteuid())
	}
}

// The real statter and the real privilege check together must produce a
// total, explained unknown on a host that is not FreeBSD - never a
// silent "up to date", and never an error that hides the answer.
func TestRealDefaultsOnANonFreeBSDHost(t *testing.T) {
	c := NewCollector(Options{CataloguePath: filepath.Join(t.TempDir(), "no-such-catalogue")})
	inv, err := c.Collect(context.Background())
	if err != nil {
		t.Fatalf("Collect() with real defaults errored: %v", err)
	}
	if inv.Catalogue.State != CatalogueUnknown {
		t.Errorf("catalogue state = %q, want %q", inv.Catalogue.State, CatalogueUnknown)
	}
	if inv.Headline() == HeadlineUpToDate {
		t.Error("a host with no catalogue reported itself up to date")
	}
	if len(inv.Unknown) == 0 {
		t.Error("a host with no pkg and no catalogue recorded no evidence gaps")
	}
}
