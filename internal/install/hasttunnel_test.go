package install

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// ADR-0158: the encrypted HAST transport's host prerequisite check.

func TestHASTTunnelInterfaceCheck_NotApplicableWithoutAnInterface(t *testing.T) {
	// A host that does not use the encrypted transport must not be
	// asked about a tunnel interface it never named.
	if hastTunnelInterfaceCheck.Applicable(Options{}) {
		t.Error("the check applies with no interface named, want it not to apply")
	}
	if !hastTunnelInterfaceCheck.Applicable(Options{HASTTunnelInterface: "wg0"}) {
		t.Error("the check does not apply with an interface named, want it to apply")
	}
}

func TestHASTTunnelInterfaceCheck_ReportsAMissingInterface(t *testing.T) {
	r := newFakeRunner()
	r.on("ifconfig wg0", fakeResponse{stderr: "wg0: does not exist", err: errors.New("exit status 1")})

	res := hastTunnelInterfaceCheck.Probe(context.Background(), r, Options{HASTTunnelInterface: "wg0"})

	if res.Status != StatusMissing {
		t.Errorf("Status = %v, want %v", res.Status, StatusMissing)
	}
	// The fix hint must say plainly that Apiary does not create the
	// tunnel itself, so an operator does not go looking for a
	// provisioner that does not exist.
	if !strings.Contains(res.FixHint, "does not create or configure") {
		t.Errorf("FixHint = %q, want it to say Apiary does not provision the tunnel", res.FixHint)
	}
	if !strings.Contains(res.FixHint, "wg0") {
		t.Errorf("FixHint = %q, want it to name the missing interface", res.FixHint)
	}
}

func TestHASTTunnelInterfaceCheck_ReportsAnExistingInterface(t *testing.T) {
	r := newFakeRunner()
	r.on("ifconfig wg0", fakeResponse{stdout: "wg0: flags=4305<UP,POINTING,RUNNING,MULTICAST> metric 0 mtu 1420"})

	res := hastTunnelInterfaceCheck.Probe(context.Background(), r, Options{HASTTunnelInterface: "wg0"})

	if res.Status != StatusOK {
		t.Errorf("Status = %v, want %v", res.Status, StatusOK)
	}
	if !strings.Contains(res.Detail, "wg0") {
		t.Errorf("Detail = %q, want it to name the interface", res.Detail)
	}
}
