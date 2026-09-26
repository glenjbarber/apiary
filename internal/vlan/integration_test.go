package vlan

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"testing"
)

// These tests exercise real ifconfig(8) vlan(4)/bridge(4) interface
// creation and require root (interface creation/destruction is a
// superuser-only operation, like jail(8)/vmm(4)) plus a real uplink NIC
// name for the VLAN cases. Cross-compile
// (GOOS=freebsd GOARCH=amd64 go test -c ./internal/vlan) and run the
// resulting binary as root on a FreeBSD host. Set APIARY_VLAN_TEST_UPLINK
// to the uplink interface name (e.g. "re0", "em0" - confirmed to differ
// per node in this project's own fleet).
//
// requireFreeBSDRoot is the first gate for all of them and is a hard
// GOOS check, not just a root check: macOS also has an ifconfig(8), and
// a developer running this suite as root there would otherwise be
// invited to create BSD-flavored interfaces on a machine that is not
// the testbed. macOS cannot establish bhyve, jail, VNET, PF, ZFS, HAST
// or real-network behaviour either, so everything in this file is
// explicitly unverified there.
func requireFreeBSDRoot(t *testing.T) {
	t.Helper()
	if runtime.GOOS != "freebsd" {
		t.Skipf("requires FreeBSD (this host is %s); run the cross-compiled binary on the testbed", runtime.GOOS)
	}
	if os.Geteuid() != 0 {
		t.Skip("requires root (interface creation)")
	}
	if _, err := exec.LookPath("ifconfig"); err != nil {
		t.Skip("ifconfig not available on this host")
	}
}

func testUplink(t *testing.T) string {
	t.Helper()
	requireFreeBSDRoot(t)
	uplink := os.Getenv("APIARY_VLAN_TEST_UPLINK")
	if uplink == "" {
		t.Skip("APIARY_VLAN_TEST_UPLINK not set; see package doc comment")
	}
	return uplink
}

func TestIntegration_EnsureBridge_CreateIsIdempotent(t *testing.T) {
	testUplink(t) // just for the root/ifconfig checks; bridges don't need an uplink
	ctx := context.Background()
	m := &Manager{}
	name := "apiary-it-br0"
	t.Cleanup(func() { m.DestroyBridge(ctx, name) })

	if _, err := m.EnsureBridge(ctx, name); err != nil {
		t.Fatalf("EnsureBridge() error: %v", err)
	}
	if _, err := m.EnsureBridge(ctx, name); err != nil {
		t.Fatalf("EnsureBridge() (2nd call) error: %v", err)
	}

	exists, err := m.ifaceExists(ctx, name)
	if err != nil || !exists {
		t.Errorf("ifaceExists(%s) = (%v, %v), want (true, nil)", name, exists, err)
	}
}

func TestIntegration_EnsureVLAN_UntaggedReturnsUplink(t *testing.T) {
	uplink := testUplink(t)
	m := &Manager{Uplink: uplink}

	got, _, err := m.EnsureVLAN(context.Background(), 0)
	if err != nil {
		t.Fatalf("EnsureVLAN(0) error: %v", err)
	}
	if got != uplink {
		t.Errorf("EnsureVLAN(0) = %q, want uplink %q unchanged", got, uplink)
	}
}

func TestIntegration_EnsureVLAN_CreatesTaggedInterface(t *testing.T) {
	uplink := testUplink(t)
	ctx := context.Background()
	m := &Manager{Uplink: uplink}
	const vlanID = 4094 // reserved/unlikely-to-collide test VLAN

	t.Cleanup(func() { runCmd(ctx, "ifconfig", vlanIfaceName(vlanID), "destroy") })

	name, _, err := m.EnsureVLAN(ctx, vlanID)
	if err != nil {
		t.Fatalf("EnsureVLAN(%d) error: %v", vlanID, err)
	}
	if name != "vlan4094" {
		t.Errorf("EnsureVLAN(%d) = %q, want vlan4094", vlanID, name)
	}

	// Idempotent: calling again must not error.
	if _, _, err := m.EnsureVLAN(ctx, vlanID); err != nil {
		t.Fatalf("EnsureVLAN(%d) (2nd call) error: %v", vlanID, err)
	}

	out, err := runCmd(ctx, "ifconfig", name)
	if err != nil {
		t.Fatalf("ifconfig %s error: %v", name, err)
	}
	if !strings.Contains(out, uplink) {
		t.Errorf("ifconfig %s output missing uplink %q, got: %s", name, uplink, out)
	}
}

func TestIntegration_EnsureMemberAndBridgeAddress(t *testing.T) {
	testUplink(t)
	ctx := context.Background()
	m := &Manager{}
	bridge := "apiary-it-br1"
	t.Cleanup(func() { m.DestroyBridge(ctx, bridge) })

	if _, err := m.EnsureBridge(ctx, bridge); err != nil {
		t.Fatalf("EnsureBridge() error: %v", err)
	}
	if err := m.EnsureBridgeAddress(ctx, bridge, "10.250.250.0/24"); err != nil {
		t.Fatalf("EnsureBridgeAddress() error: %v", err)
	}
	// Idempotent.
	if err := m.EnsureBridgeAddress(ctx, bridge, "10.250.250.0/24"); err != nil {
		t.Fatalf("EnsureBridgeAddress() (2nd call) error: %v", err)
	}

	out, err := runCmd(ctx, "ifconfig", bridge)
	if err != nil {
		t.Fatalf("ifconfig %s error: %v", bridge, err)
	}
	if !strings.Contains(out, "10.250.250.1") {
		t.Errorf("ifconfig %s output missing assigned gateway address, got: %s", bridge, out)
	}
}

// The Bridge SVI rule (ADR-0138), on a real kernel.
//
// This is the whole point of that ADR and it is not establishable on
// macOS: it needs a real bridge(4) and a real vlan(4) parented to it.
// The host under test gets a disposable bridge of its own - never
// bridge0, never the interface named by APIARY_VLAN_TEST_UPLINK - and
// the VLAN is created and destroyed by the test itself.

// A VLAN whose parent is a bridge is that bridge's SVI, and FreeBSD
// refuses to enslave it to any bridge. Both halves of that are asserted
// here directly rather than taken from a log line, because the refusal
// is the kernel's own and is the fact the reconciler's failure message
// quotes.
func TestIntegration_BridgeSVIIsRefusedByTheKernelAndByEnsureVLAN(t *testing.T) {
	requireFreeBSDRoot(t)
	ctx := context.Background()
	parent := "apiary-it-svi-par"
	const vlanID = 4093

	probe := &Manager{} // no runner, no bridges: raw ifconfig, as the kernel sees it
	if _, err := probe.EnsureBridge(ctx, parent); err != nil {
		t.Fatalf("creating the disposable parent bridge: %v", err)
	}
	t.Cleanup(func() {
		probe.DestroyVLAN(ctx, vlanID)
		probe.DestroyBridge(ctx, parent)
	})

	// The kernel accepts tagging a VLAN onto a bridge. That is the trap:
	// the interface comes up fine and only fails later, at addm time.
	name := vlanIfaceName(vlanID)
	if _, err := runCmd(ctx, "ifconfig", name, "create"); err != nil {
		t.Fatalf("creating %s: %v", name, err)
	}
	if _, err := runCmd(ctx, "ifconfig", name, "vlan", "4093", "vlandev", parent); err != nil {
		t.Fatalf("tagging %s onto the bridge (expected to be accepted): %v", name, err)
	}

	// ... and refuses to add it to a second bridge, by name.
	other := "apiary-it-svi-other"
	if _, err := probe.EnsureBridge(ctx, other); err != nil {
		t.Fatalf("creating the second disposable bridge: %v", err)
	}
	_, addErr := runCmd(ctx, "ifconfig", other, "addm", name)
	if addErr == nil {
		t.Fatalf("ifconfig %s addm %s succeeded; this kernel does not enforce the Bridge SVI rule the reconciler depends on - re-check ADR-0138's Context", other, name)
	}
	t.Logf("observed kernel refusal (expected): %v", addErr)

	// EnsureVLAN must refuse the same topology up front rather than
	// building an interface that cannot be used.
	m := &Manager{Uplink: parent}
	if _, _, err := m.EnsureVLAN(ctx, vlanID); !errors.Is(err, ErrBridgeSVI) {
		t.Fatalf("EnsureVLAN(%d) with a bridge uplink = %v, want an error matching ErrBridgeSVI", vlanID, err)
	}
}

// An epair(4) end on the very same bridge is an ordinary member and must
// still be added, even though the node's configured uplink is that
// bridge. This is the blast-radius check for ADR-0117's jail path: the
// SVI logic must not leak into it.
func TestIntegration_EpairIsStillAddedToABridgeOnABridgeUplinkNode(t *testing.T) {
	requireFreeBSDRoot(t)
	ctx := context.Background()
	bridge := "apiary-it-epair-br"
	probe := &Manager{}
	if _, err := probe.EnsureBridge(ctx, bridge); err != nil {
		t.Fatalf("creating the disposable bridge: %v", err)
	}
	t.Cleanup(func() { probe.DestroyBridge(ctx, bridge) })

	m := &Manager{Uplink: bridge} // a bridge uplink, deliberately
	hostSide, jailSide, err := m.EnsureEpair(ctx, bridge)
	if err != nil {
		t.Fatalf("EnsureEpair() error: %v", err)
	}
	t.Cleanup(func() { probe.DestroyEpair(ctx, hostSide) })

	membership, err := probe.EnsureMember(ctx, bridge, hostSide)
	if err != nil {
		t.Fatalf("EnsureMember(%s) error: %v", hostSide, err)
	}
	if membership.State != MembershipPresent {
		t.Errorf("EnsureMember(%s) state = %v, want %v", hostSide, membership.State, MembershipPresent)
	}
	if jailSide == "" {
		t.Error("EnsureEpair returned no jail side")
	}
}

// The plain-NIC path, end to end on a real uplink: a VLAN tagged onto a
// physical NIC is an ordinary bridge member and is added. This is the
// case that must keep working; the refusal above must not generalize
// past bridge uplinks.
func TestIntegration_PlainNICUplinkStillAddsTheVLAN(t *testing.T) {
	uplink := testUplink(t)
	ctx := context.Background()
	const vlanID = 4092
	bridge := "apiary-it-nic-br"

	probe := &Manager{}
	t.Cleanup(func() {
		probe.DestroyVLAN(ctx, vlanID)
		probe.DestroyBridge(ctx, bridge)
	})
	if _, err := probe.EnsureBridge(ctx, bridge); err != nil {
		t.Fatalf("creating the disposable bridge: %v", err)
	}

	m := &Manager{Uplink: uplink}
	name, _, err := m.EnsureVLAN(ctx, vlanID)
	if err != nil {
		t.Fatalf("EnsureVLAN(%d) on uplink %s: %v", vlanID, uplink, err)
	}
	membership, err := m.EnsureMember(ctx, bridge, name)
	if err != nil {
		t.Fatalf("EnsureMember(%s, %s): %v", bridge, name, err)
	}
	if membership.State != MembershipAdded {
		t.Errorf("EnsureMember state = %v, want %v", membership.State, MembershipAdded)
	}
	// Idempotent across a second pass, as the reconciler requires.
	again, err := m.EnsureMember(ctx, bridge, name)
	if err != nil {
		t.Fatalf("EnsureMember() (2nd call): %v", err)
	}
	if again.State != MembershipPresent {
		t.Errorf("EnsureMember() (2nd call) state = %v, want %v", again.State, MembershipPresent)
	}
}
