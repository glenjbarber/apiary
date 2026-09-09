package vlan

import (
	"context"
	"os"
	"os/exec"
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
func testUplink(t *testing.T) string {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Skip("requires root (interface creation)")
	}
	if _, err := exec.LookPath("ifconfig"); err != nil {
		t.Skip("ifconfig not available on this host")
	}
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

	exists, err := ifaceExists(ctx, name)
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

// TestIntegration_DownUp_TogglesInterfaceState exercises Down/Up
// (ADR-0085) against a disposable bridge(4) interface created just for
// this test - deliberately NOT the real uplink NIC named by
// APIARY_VLAN_TEST_UPLINK, since that would risk actually severing
// whatever host runs this test suite (exactly the hazard ADR-0085
// itself documents).
func TestIntegration_DownUp_TogglesInterfaceState(t *testing.T) {
	testUplink(t) // just for the root/ifconfig checks
	ctx := context.Background()
	bridge := "apiary-it-br3"
	bridgeMgr := &Manager{}
	if _, err := bridgeMgr.EnsureBridge(ctx, bridge); err != nil {
		t.Fatalf("EnsureBridge() error: %v", err)
	}
	t.Cleanup(func() { bridgeMgr.DestroyBridge(ctx, bridge) })

	m := &Manager{Uplink: bridge}
	if err := m.Down(ctx); err != nil {
		t.Fatalf("Down() error: %v", err)
	}
	exists, up, err := m.InterfaceStatus(ctx, bridge)
	if err != nil || !exists || up {
		t.Errorf("InterfaceStatus() after Down() = (%v, %v, %v), want (true, false, nil)", exists, up, err)
	}

	if err := m.Up(ctx); err != nil {
		t.Fatalf("Up() error: %v", err)
	}
	exists, up, err = m.InterfaceStatus(ctx, bridge)
	if err != nil || !exists || !up {
		t.Errorf("InterfaceStatus() after Up() = (%v, %v, %v), want (true, true, nil)", exists, up, err)
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
