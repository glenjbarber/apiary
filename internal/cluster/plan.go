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
// provision it locally: which node it's assigned to, and the resource
// shape to provision with.
type VMPlacement struct {
	ID       string
	NodeID   string
	Vcpus    uint32
	MemoryMB uint64
}

// Plan returns the VMs assigned to localNodeID, sorted by ID for
// deterministic ordering. Each resource type (dataset, disk image, bhyve
// VM) has its own independent existence check in Reconciler.RunOnce,
// run fresh every tick - Plan's job is only "what is this node
// responsible for," not "what's missing," since idempotent
// convergence-per-resource is simpler and safer than trying to track
// per-resource-type existence in one combined pass.
//
// This deliberately never computes anything to *remove*. A VM
// disappearing from the list - whether deleted, reassigned, or the
// fetch simply failed - is not safely distinguishable from "tear this
// down" without more care than a first slice should assume (grace
// periods, confirming the removal is intentional). Cleanup of
// datasets/VMs for VMs no longer assigned here is a deliberate future
// decision, not an accidental default.
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
