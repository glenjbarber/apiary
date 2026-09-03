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

	// FirewallPaused, when true, tells the reconciler to leave this
	// VM's pf(8) anchor with no active rules (everything allowed)
	// regardless of FirewallRules - a temporary, reversible override
	// set only via ManagerService.SetVMFirewallPaused, never via
	// UpdateVM. See ADR-0049.
	FirewallPaused bool

	// ReplicaNodeID, if set, names a second node that HAST-replicates
	// this VM's disk (ADR-0026) - data redundancy, not automatic
	// failover. Caller-set, exactly like NodeID.
	ReplicaNodeID string

	// BaseImageName, if set, names a raw disk image the reconciler
	// should resolve (via Reconciler.ISOs, reusing the same store
	// ISOName does) and copy into this VM's disk file the first time it
	// creates it, instead of creating a blank file. See ADR-0031.
	// Ignored once the disk file already exists.
	BaseImageName string
}

// JailPlacement mirrors VMPlacement, deliberately minimal like
// JailDefinition itself (see api/internalpb/state.proto) - no vcpus/
// memory/ISO/network/firewall fields, since internal/jail's v1 scope
// is flat ip4=inherit networking with no dedicated resource limits.
type JailPlacement struct {
	ID       string
	Name     string
	Hostname string
	NodeID   string

	// Deleting is true once the jail has been soft-deleted (see
	// JAIL_STATE_DELETING) - ensureJail tears its resources down
	// instead of provisioning them.
	Deleting bool

	// Phase is the last phase this reconciler itself recorded for this
	// jail, mirroring VMPlacement.Phase.
	Phase string

	// ReplicaNodeID, if set, names a second node that HAST-replicates
	// this jail's root filesystem (ADR-0026) - the same data-redundancy-
	// not-failover semantics as VMPlacement.ReplicaNodeID.
	ReplicaNodeID string
}

// PlanJail mirrors Plan exactly, for jails instead of VMs.
func PlanJail(desired []JailPlacement, localNodeID string) []JailPlacement {
	var assigned []JailPlacement
	for _, jail := range desired {
		if jail.NodeID == localNodeID {
			assigned = append(assigned, jail)
		}
	}
	sort.Slice(assigned, func(i, j int) bool { return assigned[i].ID < assigned[j].ID })
	return assigned
}

// PlanJailReclaim mirrors PlanReclaim exactly, for jails instead of VMs.
func PlanJailReclaim(desired []JailPlacement, localNodeID string) []string {
	var ids []string
	for _, jail := range desired {
		if jail.NodeID != localNodeID {
			ids = append(ids, jail.ID)
		}
	}
	sort.Strings(ids)
	return ids
}

// PlanJailReplica mirrors PlanReplica exactly, for jails instead of VMs.
func PlanJailReplica(desired []JailPlacement, localNodeID string) []JailPlacement {
	var replicas []JailPlacement
	for _, jail := range desired {
		if jail.ReplicaNodeID == localNodeID && !jail.Deleting {
			replicas = append(replicas, jail)
		}
	}
	sort.Slice(replicas, func(i, j int) bool { return replicas[i].ID < replicas[j].ID })
	return replicas
}

// PlanJailReplicaReclaim mirrors PlanReplicaReclaim exactly, for jails
// instead of VMs - including the same owner-skip guard (a node is
// never simultaneously primary and secondary for the same jail).
func PlanJailReplicaReclaim(desired []JailPlacement, localNodeID string) []string {
	var ids []string
	for _, jail := range desired {
		if jail.NodeID == localNodeID {
			continue
		}
		if jail.ReplicaNodeID != localNodeID || jail.Deleting {
			ids = append(ids, jail.ID)
		}
	}
	sort.Strings(ids)
	return ids
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

// PlanReclaim returns the IDs of every VM in desired that is NOT
// currently assigned to localNodeID - candidates whose local resources
// (if any exist under that ID on this node, left over from before a
// reassignment) the reconciler should tear down. See
// Reconciler.reclaimStaleVM.
//
// This is deliberately different from - and safer than - inferring
// teardown from a VM's absence, which Plan's own doc comment explains
// this package avoids. Here, the record still exists: it explicitly
// states a different current owner, an unambiguous, caller-originated
// fact (the same kind of signal Deleting already is), not an absence
// that could just as easily mean "the fetch failed" or "raced with a
// concurrent create." A node that never hosted a given VM in the first
// place is unaffected: its resource-existence checks simply come back
// negative and reclaimStaleVM does nothing.
func PlanReclaim(desired []VMPlacement, localNodeID string) []string {
	var ids []string
	for _, vm := range desired {
		if vm.NodeID != localNodeID {
			ids = append(ids, vm.ID)
		}
	}
	sort.Strings(ids)
	return ids
}

// PlanReplica returns the VMs this node should hold a HAST secondary
// replica for - ReplicaNodeID names localNodeID, and the VM isn't
// tombstoned (a deleting VM's replica is torn down instead, via
// PlanReplicaReclaim below; ensuring it here would just create work
// immediately undone). Sorted by ID for deterministic ordering, mirroring
// Plan itself.
func PlanReplica(desired []VMPlacement, localNodeID string) []VMPlacement {
	var replicas []VMPlacement
	for _, vm := range desired {
		if vm.ReplicaNodeID == localNodeID && !vm.Deleting {
			replicas = append(replicas, vm)
		}
	}
	sort.Slice(replicas, func(i, j int) bool { return replicas[i].ID < replicas[j].ID })
	return replicas
}

// PlanReplicaReclaim returns the IDs of every VM in desired that this
// node should NOT currently hold a secondary replica for (not named as
// ReplicaNodeID, or tombstoned) - candidates whose local secondary-role
// HAST resource (if any exists, left over from before a reassignment or
// deletion) should be torn down. Mirrors PlanReclaim's own reasoning
// exactly, just against ReplicaNodeID instead of NodeID.
//
// A VM this node OWNS (NodeID == localNodeID) is always skipped here,
// even though NodeID and ReplicaNodeID are naturally never equal - a
// node is never simultaneously primary and secondary for the same VM,
// so the naive "ReplicaNodeID != localNodeID" check would otherwise
// treat the owner's own just-provisioned PRIMARY-role resource as a
// stale secondary to reclaim, destroying it the very same tick it was
// created. Caught live: real end-to-end HAST testing showed a
// primary's zvol vanish immediately after creation, on the same node
// that had just created it as primary (see ADR-0026).
func PlanReplicaReclaim(desired []VMPlacement, localNodeID string) []string {
	var ids []string
	for _, vm := range desired {
		if vm.NodeID == localNodeID {
			continue
		}
		if vm.ReplicaNodeID != localNodeID || vm.Deleting {
			ids = append(ids, vm.ID)
		}
	}
	sort.Strings(ids)
	return ids
}
