package hast

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"testing"
	"time"
)

// These tests exercise real hastctl(8)/hastd(8) and require root, same
// as internal/jail - hastctl's control socket rejects unprivileged
// callers outright. Cross-compile
// (GOOS=freebsd GOARCH=amd64 go test -c ./internal/hast) and run the
// resulting binary as root on a FreeBSD host. TestIntegration_ResourceLifecycle
// additionally needs APIARY_HAST_TEST_PEER_NAME and
// APIARY_HAST_TEST_PEER_ADDR set to the *other* project VM's hostname
// and address (e.g. freebsd-apiary2 / 10.50.0.12), and restarts hastd
// itself as part of deploying its test config - do not run this against
// a host with unrelated hastd resources you care about.
//
// Verified for real: hast.conf rendering/deployment, hastctl
// create/role/status, and this package's status parsing, all against a
// live single-node resource. NOT verified here: actual cross-node
// replication reaching "complete" status - see docs/adr/0008, which
// documents a persistent "degraded" status observed between the two
// project VMs (apiary-vm/apiary-vm2) that several standard causes
// (firewall, MTU, NIC offload, role-ordering) did not explain. This test
// only asserts that a resource can be created and moved to the primary
// role and that Status() correctly parses whatever hastd reports, not
// that ResourceStatus reaches "complete".
func newTestDevice(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("hastctl"); err != nil {
		t.Skip("hastctl not available on this host; see package doc comment for how to run these tests")
	}

	backing := t.TempDir() + "/hasttest.img"
	if _, err := runCmd(context.Background(), "truncate", "-s", "64M", backing); err != nil {
		t.Fatalf("creating backing file: %v", err)
	}

	out, err := runCmd(context.Background(), "mdconfig", "-a", "-t", "vnode", "-f", backing)
	if err != nil {
		t.Fatalf("mdconfig -a: %v", err)
	}
	unit := strings.TrimSpace(out) // mdconfig -a prints e.g. "md11"
	if !regexp.MustCompile(`^md[0-9]+$`).MatchString(unit) {
		t.Fatalf("unexpected mdconfig -a output: %q", out)
	}

	t.Cleanup(func() {
		runCmd(context.Background(), "mdconfig", "-d", "-u", unit)
	})
	return "/dev/" + unit
}

func TestIntegration_ResourceLifecycle(t *testing.T) {
	device := newTestDevice(t)
	ctx := context.Background()

	hostname, err := os.Hostname()
	if err != nil {
		t.Fatalf("os.Hostname() error: %v", err)
	}

	// peerAddr/peerName identify the *other* project VM. They don't need
	// to have this resource configured for this node's own create/role
	// operations to succeed - only for the resource to ever leave
	// "degraded" status, which this test does not require (see the
	// package doc comment and docs/adr/0008).
	peerName := os.Getenv("APIARY_HAST_TEST_PEER_NAME")
	peerAddr := os.Getenv("APIARY_HAST_TEST_PEER_ADDR")
	if peerName == "" || peerAddr == "" {
		t.Skip("APIARY_HAST_TEST_PEER_NAME/APIARY_HAST_TEST_PEER_ADDR not set; see package doc comment")
	}

	name := fmt.Sprintf("it%d", time.Now().UnixNano()%1_000_000)
	resources := []Resource{{
		Name: name,
		Nodes: []Node{
			{Name: hostname, Local: device, Remote: peerAddr},
			{Name: peerName, Local: device, Remote: hostname},
		},
	}}

	configPath := t.TempDir() + "/hast.conf"
	m := &Manager{ConfigPath: configPath}
	if err := m.WriteConfig(resources); err != nil {
		t.Fatalf("WriteConfig() error: %v", err)
	}

	// hastctl/hastd read the real /etc/hast.conf, not an arbitrary path
	// (there is no -c flag plumbed through this package's v1 API), so
	// deploy the rendered file there for the daemon to pick up.
	rendered, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("reading rendered config: %v", err)
	}
	if err := os.WriteFile(DefaultConfigPath, rendered, 0o644); err != nil {
		t.Fatalf("deploying config to %s: %v", DefaultConfigPath, err)
	}

	// hastd does not hot-reload hast.conf; it must be restarted to pick
	// up a newly defined resource. This package does not manage the
	// hastd service (see Manager's doc comment) - a real caller is
	// responsible for this same restart after WriteConfig.
	if _, err := runCmd(ctx, "service", "hastd", "onestop"); err != nil {
		t.Logf("service hastd onestop: %v (may simply not have been running)", err)
	}
	if _, err := runCmd(ctx, "service", "hastd", "onestart"); err != nil {
		t.Fatalf("service hastd onestart: %v", err)
	}
	t.Cleanup(func() { runCmd(context.Background(), "service", "hastd", "onestop") })

	t.Cleanup(func() {
		m.SetRole(context.Background(), name, RoleInit)
	})

	if err := m.CreateResource(ctx, name); err != nil {
		t.Fatalf("CreateResource() error: %v", err)
	}

	if err := m.SetRole(ctx, name, RolePrimary); err != nil {
		t.Fatalf("SetRole(primary) error: %v", err)
	}

	status, err := m.Status(ctx, name)
	if err != nil {
		t.Fatalf("Status() error: %v", err)
	}
	if status.Role != "primary" {
		t.Errorf("Status().Role = %q, want %q", status.Role, "primary")
	}
	if status.LocalPath != device {
		t.Errorf("Status().LocalPath = %q, want %q", status.LocalPath, device)
	}
	// Not asserting ResourceStatus == "complete": see the doc comment
	// above and docs/adr/0008 for the unresolved cross-node sync issue.
	// It should at least be non-empty (a primary always reports one).
	if status.ResourceStatus == "" {
		t.Errorf("Status().ResourceStatus is empty for a primary, want complete or degraded")
	}
}

func TestIntegration_RestartService(t *testing.T) {
	if _, err := exec.LookPath("hastctl"); err != nil {
		t.Skip("hastctl not available on this host; see package doc comment for how to run these tests")
	}
	ctx := context.Background()

	if _, err := runCmd(ctx, "service", "hastd", "onestart"); err != nil {
		t.Fatalf("service hastd onestart: %v", err)
	}
	t.Cleanup(func() { runCmd(context.Background(), "service", "hastd", "onestop") })

	m := New()
	if err := m.RestartService(ctx); err != nil {
		t.Fatalf("RestartService() error: %v", err)
	}
}

func TestIntegration_StatusOnUnknownResourceFails(t *testing.T) {
	newTestDevice(t) // just for the LookPath("hastctl") skip check
	m := New()

	if _, err := m.Status(context.Background(), "definitely-not-a-real-resource"); err == nil {
		t.Errorf("Status() on an unknown resource = nil error, want an error")
	}
}
