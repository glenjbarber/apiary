package cluster

import (
	"context"
	"errors"
	"strings"
	"testing"

	internalpb "github.com/glenjbarber/apiary/api/internalpb"
)

// A bridge this reconcile pass created, on a step that then failed, is
// destroyed before returning.
//
// Confirmed live on brood: a network whose VLAN could not be added to its
// bridge left apnet-<hash> on the host after the network was deleted.
// The artifact value is the only durable record of what this pass owns,
// and it is only persisted on the success path - so any failure after
// EnsureBridge but before the return abandoned the bridge with nothing
// on record that Apiary had ever created it. reconcileNetworkArtifacts
// only destroys a bridge whose recorded OwnBridge is true, so the
// interface outlived the network that caused it.

// The exact live failure from brood, as the error ifconfig produced:
// a VLAN whose parent is a bridge is a Bridge SVI and cannot itself be
// added to another bridge.
var errBridgeSVI = errors.New("ifconfig: BRDGADD vlan2: Invalid argument (Bridge SVI cannot be added to a bridge)")

func TestEnsureNetwork_MemberFailureDestroysTheBridgeItCreated(t *testing.T) {
	vlan := newFakeVLANManager()
	vlan.memberErr = errBridgeSVI
	r := &Reconciler{VLAN: vlan, Uplink: "bridge0"}

	_, err := r.ensureNetwork(context.Background(), &internalpb.NetworkDefinition{
		Id: "apnet", VlanId: 2, Subnet: "10.90.1.0/24",
	})
	if err == nil {
		t.Fatal("ensureNetwork() error = nil, want the addm failure")
	}

	// The operator must still see the real cause, not a cleanup error.
	if !strings.Contains(err.Error(), "Bridge SVI cannot be added to a bridge") {
		t.Errorf("error = %q, want it to name the real ifconfig cause", err)
	}

	if len(vlan.destroyedBridges) != 1 {
		t.Fatalf("destroyedBridges = %v, want exactly the bridge this call created", vlan.destroyedBridges)
	}
	if vlan.destroyedBridges[0] != vlan.ensuredBridges[0] {
		t.Errorf("destroyed %q but created %q", vlan.destroyedBridges[0], vlan.ensuredBridges[0])
	}
}

// A bridge that already existed before this call must never be
// destroyed, even when a later step fails. It predates this network, and
// removing it would be far worse than the leak being fixed.
func TestEnsureNetwork_MemberFailureLeavesAPreExistingBridgeAlone(t *testing.T) {
	vlan := newFakeVLANManager()
	vlan.memberErr = errBridgeSVI
	// EnsureBridge reports created=false: the interface was already there.
	vlan.bridgeErr = nil
	original := vlan.EnsureBridge
	_ = original
	r := &Reconciler{VLAN: vlan, Uplink: "bridge0"}

	// Force the "pre-existing" case by wrapping the fake.
	preExisting := &preExistingBridgeVLAN{fakeVLANManager: vlan}
	r.VLAN = preExisting

	_, err := r.ensureNetwork(context.Background(), &internalpb.NetworkDefinition{
		Id: "apnet", VlanId: 2, Subnet: "10.90.1.0/24",
	})
	if err == nil {
		t.Fatal("ensureNetwork() error = nil, want the addm failure")
	}
	if len(vlan.destroyedBridges) != 0 {
		t.Errorf("destroyedBridges = %v, want none: a pre-existing bridge must never be torn down by a failed network", vlan.destroyedBridges)
	}
}

// preExistingBridgeVLAN reports every bridge as already present, which is
// what "this call did not create it" looks like to ensureNetwork.
type preExistingBridgeVLAN struct {
	*fakeVLANManager
}

func (p *preExistingBridgeVLAN) EnsureBridge(ctx context.Context, name string) (bool, error) {
	if _, err := p.fakeVLANManager.EnsureBridge(ctx, name); err != nil {
		return false, err
	}
	return false, nil
}

// Every other failure point after the bridge exists has the same leak, so
// each is covered: address assignment and NAT.
func TestEnsureNetwork_AddressFailureDestroysTheBridgeItCreated(t *testing.T) {
	vlan := newFakeVLANManager()
	vlan.addressErr = errors.New("ifconfig: Can't assign requested address")
	r := &Reconciler{VLAN: vlan, Uplink: "bridge0"}

	_, err := r.ensureNetwork(context.Background(), &internalpb.NetworkDefinition{
		Id: "apnet", VlanId: 2, Subnet: "10.90.1.0/24",
	})
	if err == nil {
		t.Fatal("ensureNetwork() error = nil, want the address failure")
	}
	if len(vlan.destroyedBridges) != 1 {
		t.Errorf("destroyedBridges = %v, want the bridge this call created to be removed", vlan.destroyedBridges)
	}
}

func TestEnsureNetwork_NATFailureDestroysTheBridgeItCreated(t *testing.T) {
	vlan := newFakeVLANManager()
	pf := newFakePFManager()
	pf.natErr = errors.New("pfctl: BRDGADD failed")
	r := &Reconciler{VLAN: vlan, PF: pf, Uplink: "bridge0"}

	_, err := r.ensureNetwork(context.Background(), &internalpb.NetworkDefinition{
		Id: "apnet", VlanId: 2, Subnet: "10.90.1.0/24",
	})
	if err == nil {
		t.Fatal("ensureNetwork() error = nil, want the NAT failure")
	}
	if len(vlan.destroyedBridges) != 1 {
		t.Errorf("destroyedBridges = %v, want the bridge this call created to be removed", vlan.destroyedBridges)
	}
}

// The success path must not destroy anything - the bridge is recorded as
// owned and torn down later, by the network's own deletion, exactly once.
func TestEnsureNetwork_SuccessDestroysNothing(t *testing.T) {
	vlan := newFakeVLANManager()
	r := &Reconciler{VLAN: vlan, Uplink: "bridge0"}

	artifact, err := r.ensureNetwork(context.Background(), &internalpb.NetworkDefinition{
		Id: "apnet", VlanId: 2, Subnet: "10.90.1.0/24",
	})
	if err != nil {
		t.Fatalf("ensureNetwork() error = %v, want success", err)
	}
	if !artifact.OwnBridge {
		t.Error("OwnBridge = false on a bridge this call created; the deletion path would never clean it up")
	}
	if len(vlan.destroyedBridges) != 0 {
		t.Errorf("destroyedBridges = %v, want none on the success path", vlan.destroyedBridges)
	}
}
