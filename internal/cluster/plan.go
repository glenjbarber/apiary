// Package cluster provides node-local reconciliation: given the cluster's
// ephemeral VM definitions (from raft) and this node's own local storage,
// it figures out what local resources this node needs to provision.
//
// This is deliberately separate from *scheduling* - deciding which node a
// VM's node_id should be - which doesn't exist yet (a VMDefinition's
// node_id is just whatever a caller sets directly today, per CLAUDE.md).
// What lives here answers a narrower question: "given VM assignments as
// they already are, what does *this* node need to do locally?"
//
// This split exists because raft's FSM.Apply runs identically on every
// node as they replay the log - provisioning local resources as a side
// effect of Apply would make every node try to provision every VM,
// regardless of which node_id it's actually assigned to. A per-node
// reconciler that only acts on VMs assigned to its own node ID is the
// correct shape instead.
package cluster

import "sort"

// VMPlacement is the information the reconciler needs about a VM to
// provision (or tear down) it locally: which node it's assigned to, the
// resource shape to provision with, and its current tombstone/phase
// state. Deliberately plain Go types rather than internalpb's, keeping
// this package's core logic independent of the wire schema's evolution -
// reconciler.go does the translation.
type VMPlacement struct {
	ID       string
	NodeID   string
	Vcpus    uint32
	MemoryMB uint64

	// Deleting is true once the VM has been soft-deleted (see
	// api/internalpb's VM_STATE_DELETING) - ensureVM tears its resources
	// down instead of provisioning them.
	Deleting bool

	// Phase is the last phase this reconciler itself recorded for this
	// VM ("", "creating", "ready", "deleting", "error") - read back so a
	// tick doesn't redundantly re-submit a phase update it already made.
	Phase string

	// ISOName, if set, names an image the reconciler should resolve
	// (via Reconciler.ISOs) to a local path and attach as a CD-ROM.
	ISOName string

	// NetworkID, if set, names a NetworkDefinition this VM's NIC
	// belongs to - the reconciler ensures the network's vlan/bridge
	// exist locally and attaches the VM to that bridge instead of
	// Reconciler's own node-wide Bridge. IPAddress/MACAddress are
	// assigned by the FSM (never computed here) when NetworkID is set;
	// FirewallRules is a simple allow/block list applied via a per-VM
	// pf(8) anchor. See ADR-0022.
	NetworkID     string
	IPAddress     string
	MACAddress    string
	FirewallRules []FirewallRule
}

// FirewallRule mirrors api/internalpb's FirewallRule, kept as a plain
// Go type for the same reason as the rest of VMPlacement's fields.
type FirewallRule struct {
	Direction string
	Action    string
	Protocol  string
	PortRange string
}

// Plan returns the VMs assigned to localNodeID, sorted by ID for
// deterministic ordering. Each resource type (dataset, disk image, bhyve
// VM) has its own independent existence check in Reconciler.RunOnce,
// run fresh every tick - Plan's job is only "what is this node
// responsible for," not "what's missing," since idempotent
// convergence-per-resource is simpler and safer than trying to track
// per-resource-type existence in one combined pass.
//
// This deliberately never *infers* anything to remove from a VM's
// absence. A VM disappearing from the list - whether reassigned, or the
// fetch simply failed - is not safely distinguishable from "tear this
// down" without more care than a first slice should assume (grace
// periods, confirming the removal is intentional). Teardown only ever
// happens for a VM that is still present and explicitly marked Deleting
// (see ADR-0016) - an unambiguous, caller-originated signal, not an
// absence Plan would have to guess about.
func Plan(desired []VMPlacement, localNodeID string) []VMPlacement {
	var assigned []VMPlacement
	for _, vm := range desired {
		if vm.NodeID == localNodeID {
			assigned = append(assigned, vm)
		}
	}
	sort.Slice(assigned, func(i, j int) bool { return assigned[i].ID < assigned[j].ID })
	return assigned
}
