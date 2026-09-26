package cluster

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	internalpb "github.com/glenjbarber/apiary/api/internalpb"
	"github.com/glenjbarber/apiary/internal/vlan"
)

// The reconciler half of ADR-0138. The vlan package's own refusal of a
// Bridge SVI is covered there; what is asserted here is that the
// reconciler does the right thing with the answer: no bridge leak on the
// refusal path, and never treating "membership unknown" as "joined".

// An SVI refusal on a bridge this call created must not leave that bridge
// behind - the same no-leak rule the other failure points already have,
// and the one that matters most here, because on a bridge-uplink node
// this is the outcome of every single tick.
func TestEnsureNetwork_BridgeSVIRefusalDestroysTheBridgeItCreated(t *testing.T) {
	vlanMgr := newFakeVLANManager()
	vlanMgr.memberState = vlan.MembershipSVI
	vlanMgr.memberSVIParent = "bridge0"
	vlanMgr.memberErr = fmt.Errorf("adding vlan2 to apnet: %w", vlan.ErrBridgeSVI)
	r := &Reconciler{VLAN: vlanMgr, Uplink: "bridge0"}

	_, err := r.ensureNetwork(context.Background(), &internalpb.NetworkDefinition{
		Id: "apnet", VlanId: 2, Subnet: "10.90.1.0/24",
	})
	if !errors.Is(err, vlan.ErrBridgeSVI) {
		t.Fatalf("ensureNetwork() error = %v, want one wrapping ErrBridgeSVI", err)
	}
	if len(vlanMgr.destroyedBridges) != 1 {
		t.Fatalf("destroyedBridges = %v, want exactly the bridge this call created", vlanMgr.destroyedBridges)
	}
	if vlanMgr.destroyedBridges[0] != vlanMgr.ensuredBridges[0] {
		t.Errorf("destroyed %q but created %q", vlanMgr.destroyedBridges[0], vlanMgr.ensuredBridges[0])
	}
}

// An SVI report with no error - the one shape that means "this interface
// already is that bridge's own L3" - is accepted, because the network's
// bridge is the SVI's parent in that case and there is genuinely nothing
// left to join. A pre-existing bridge is still not destroyed, and the
// pass still records ownership only for what it created.
func TestEnsureNetwork_SVIOfItsOwnBridgeIsAccepted(t *testing.T) {
	vlanMgr := newFakeVLANManager()
	vlanMgr.memberState = vlan.MembershipSVI
	// The network names its own bridge explicitly, so the SVI's parent and
	// the bridge being joined are the same interface.
	vlanMgr.memberSVIParent = "apnet-abcd1234"
	r := &Reconciler{VLAN: vlanMgr, Uplink: "bridge0"}

	artifact, err := r.ensureNetwork(context.Background(), &internalpb.NetworkDefinition{
		Id: "apnet", VlanId: 2, Subnet: "10.90.1.0/24", BridgeName: "apnet-abcd1234",
	})
	if err != nil {
		t.Fatalf("ensureNetwork() error = %v, want success", err)
	}
	if artifact.Bridge != "apnet-abcd1234" {
		t.Errorf("artifact.Bridge = %q, want %q", artifact.Bridge, "apnet-abcd1234")
	}
	if len(vlanMgr.destroyedBridges) != 0 {
		t.Errorf("destroyedBridges = %v, want none on the success path", vlanMgr.destroyedBridges)
	}
}

// MembershipUnknown with a nil error means the membership call
// established nothing and reported no failure. Acting on that as though
// the interface were joined would produce a bridge with a gateway
// address and no uplink, which is the exact silent failure ADR-0138
// exists to rule out - so this pass must refuse and undo what it created.
func TestEnsureNetwork_UnknownMembershipIsNotTreatedAsJoined(t *testing.T) {
	vlanMgr := newFakeVLANManager()
	vlanMgr.memberState = vlan.MembershipUnknown // and no memberErr
	r := &Reconciler{VLAN: vlanMgr, Uplink: "em0"}

	_, err := r.ensureNetwork(context.Background(), &internalpb.NetworkDefinition{
		Id: "apnet", VlanId: 2, Subnet: "10.90.1.0/24",
	})
	if err == nil {
		t.Fatal("ensureNetwork() error = nil, want a refusal when membership is unknown")
	}
	if !strings.Contains(err.Error(), "membership is unknown") {
		t.Errorf("error does not say membership was never confirmed: %v", err)
	}
	if len(vlanMgr.destroyedBridges) != 1 {
		t.Errorf("destroyedBridges = %v, want the bridge this call created removed", vlanMgr.destroyedBridges)
	}
	if len(vlanMgr.addresses) != 0 {
		t.Errorf("addresses = %v, want none: a gateway was assigned to a bridge nothing was joined to", vlanMgr.addresses)
	}
}

// The normal path is unchanged: a confirmed join proceeds to the gateway
// address, and the membership call is made exactly once per pass.
func TestEnsureNetwork_ConfirmedMembershipProceeds(t *testing.T) {
	vlanMgr := newFakeVLANManager() // joins: MembershipAdded
	r := &Reconciler{VLAN: vlanMgr, Uplink: "em0"}

	artifact, err := r.ensureNetwork(context.Background(), &internalpb.NetworkDefinition{
		Id: "apnet", VlanId: 2, Subnet: "10.90.1.0/24",
	})
	if err != nil {
		t.Fatalf("ensureNetwork() error: %v", err)
	}
	if vlanMgr.memberCalls != 1 {
		t.Errorf("EnsureMember called %d times, want 1", vlanMgr.memberCalls)
	}
	if len(vlanMgr.addresses) != 1 {
		t.Errorf("addresses = %v, want exactly one gateway assignment after a confirmed join", vlanMgr.addresses)
	}
	if !artifact.OwnBridge {
		t.Error("OwnBridge = false on a bridge this call created")
	}
}
