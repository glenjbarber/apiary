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

// VMPlacement is the minimal information the reconciler needs about a VM
// to decide whether it belongs on the local node.
type VMPlacement struct {
	ID     string
	NodeID string
}

// Plan compares the VMs assigned to localNodeID against the datasets
// that already exist locally, and returns the dataset names (VM IDs)
// that need to be created to catch up.
//
// This deliberately does not compute anything to destroy. Automatically
// destroying local storage because a VM no longer appears in the list -
// whether because it was deleted, reassigned to another node, or the
// list fetch simply failed - is a real design question of its own
// (grace periods, confirming the removal is intentional rather than
// transient) that a naive "not in the list means gone" rule would get
// dangerously wrong. Cleanup is left as a deliberate future decision,
// not an accidental default.
func Plan(desired []VMPlacement, existingDatasets []string, localNodeID string) []string {
	existing := make(map[string]bool, len(existingDatasets))
	for _, name := range existingDatasets {
		existing[name] = true
	}

	var toCreate []string
	for _, vm := range desired {
		if vm.NodeID != localNodeID {
			continue
		}
		if !existing[vm.ID] {
			toCreate = append(toCreate, vm.ID)
		}
	}

	sort.Strings(toCreate)
	return toCreate
}
