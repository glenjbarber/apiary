// Dependency Graph Simulator v1 (ADR-0052): a read-only, deterministic
// answer to "what happens if this node disappears right now?" - raft
// quorum impact using live reachability (not just configured
// membership), which VMs/jails this node owns, and which VMs/jails it
// backs as a HAST replica. Deliberately plain Go types independent of
// the wire schema, mirroring plan.go's own separation - the RPC
// handler in internal/manager does all I/O (raft reads, peer
// reachability checks) and translation to/from proto; everything here
// is a pure function over already-fetched data.
package cluster

import (
	"fmt"
	"sort"
)

// ResourceKind distinguishes a VM from a jail.
type ResourceKind string

const (
	ResourceKindVM   ResourceKind = "vm"
	ResourceKindJail ResourceKind = "jail"
)

// RecoveryVerdict is deliberately limited to what raft configuration
// data and node_id/replica_node_id placement alone can prove - see
// ADR-0052 for why there is no confident "will recover cleanly"
// verdict in v1: live HAST sync status has no RPC exposure anywhere in
// this codebase (internal/hast.Manager.Status is called only
// internally, by internal/cluster/hast.go, to self-verify a role
// change it just made).
type RecoveryVerdict string

const (
	// RecoveryVerdictUnprotected means no replica_node_id is configured.
	// This proves only the absence of HAST-based redundancy Apiary
	// knows about - not that the resource's data is permanently
	// unrecoverable by any means (a surviving physical disk or an
	// external backup might still exist outside Apiary's own tracking).
	RecoveryVerdictUnprotected RecoveryVerdict = "unprotected"

	// RecoveryVerdictUnverifiedReplica means replica_node_id is set, but
	// this package never queries live HAST sync status - the replica
	// may or may not actually be caught up.
	RecoveryVerdictUnverifiedReplica RecoveryVerdict = "unverified_replica"
)

// Reachability is a real three-state result, not a bool: a remaining
// voter's live status is either confirmed one way, confirmed the
// other, or genuinely unknown (e.g. no peer forwarding is configured
// on the node answering the simulation, so nothing could be checked at
// all). Unknown must never be silently folded into either reachable or
// unreachable.
type Reachability string

const (
	ReachabilityReachable   Reachability = "reachable"
	ReachabilityUnreachable Reachability = "unreachable"
	ReachabilityUnknown     Reachability = "unknown"
)

// ServerSuffrage is a plain-Go mirror of one entry from raft's own
// Configuration (internal/raft.ServerInfo), with a Reachability the
// RPC handler populates via a live check - meaningless/ignored for the
// simulated target itself and for non-voters.
type ServerSuffrage struct {
	ID           string
	Suffrage     string // "Voter", "Nonvoter", "Staging", or "Unknown"
	Reachability Reachability
}

// QuorumImpact reports what raft's own quorum arithmetic looks like if
// targetNodeID vanished right now.
type QuorumImpact struct {
	TargetIsVoter      bool
	TotalVoters        uint32
	RemainingVoters    uint32
	RemainingReachable uint32
	RemainingUnknown   uint32
	QuorumSize         uint32
	Survives           bool
	Note               string
}

// OwnedResourcePlacement is the minimal shape the simulator needs per
// VM/jail: identity plus the two placement fields that matter here.
type OwnedResourcePlacement struct {
	ID            string
	Name          string
	Kind          ResourceKind
	NodeID        string
	ReplicaNodeID string
}

// OwnedResourceImpact is one VM/jail this node OWNS (hosts), with a
// recovery verdict.
type OwnedResourceImpact struct {
	ID            string
	Name          string
	Kind          ResourceKind
	ReplicaNodeID string
	Verdict       RecoveryVerdict
	Explanation   string
}

// ReplicaBackedImpact is one VM/jail owned by a DIFFERENT node, for
// which the simulated node is the configured HAST replica target. It
// keeps running unaffected on its real owner if the simulated node
// disappears, but loses its redundancy until a new replica is
// configured elsewhere - a real, distinct consequence from anything in
// OwnedResourceImpact.
type ReplicaBackedImpact struct {
	ID          string
	Name        string
	Kind        ResourceKind
	OwnerNodeID string
	Explanation string
}

// NodeFailureReport bundles all three computations for one simulated
// node.
type NodeFailureReport struct {
	Quorum                 QuorumImpact
	OwnedResources         []OwnedResourceImpact
	ReplicaBackedResources []ReplicaBackedImpact
}

// IsKnownTarget reports whether targetNodeID is recognized at all -
// either as a raft server, or as the owner or replica of some VM/jail.
// The RPC handler calls this before computing anything else: a
// mistyped or unknown target must return an explicit error, never a
// "quorum survives, 0 resources owned" report that looks identical to
// a genuinely safe finding.
func IsKnownTarget(servers []ServerSuffrage, resources []OwnedResourcePlacement, targetNodeID string) bool {
	for _, s := range servers {
		if s.ID == targetNodeID {
			return true
		}
	}
	for _, r := range resources {
		if r.NodeID == targetNodeID || r.ReplicaNodeID == targetNodeID {
			return true
		}
	}
	return false
}

// ComputeQuorumImpact answers "does raft still have quorum without
// targetNodeID" using the current raft configuration plus each
// remaining voter's live reachability - not configured membership
// counts alone. A configuration can look survivable by pure arithmetic
// (e.g. "3 voters minus 1 leaves 2, majority of 3 is 2") while actually
// failing right now if one of the "remaining" voters is already
// unreachable for an unrelated reason; Survives is computed from
// RemainingReachable only, never crediting an unknown-reachability
// voter as survival.
func ComputeQuorumImpact(servers []ServerSuffrage, targetNodeID string) QuorumImpact {
	var total, remaining, remainingReachable, remainingUnknown uint32
	targetIsVoter := false
	targetFound := false

	for _, s := range servers {
		isVoter := s.Suffrage == "Voter"
		if s.ID == targetNodeID {
			targetFound = true
			targetIsVoter = isVoter
			continue
		}
		if !isVoter {
			continue
		}
		total++
		remaining++
		switch s.Reachability {
		case ReachabilityReachable:
			remainingReachable++
		case ReachabilityUnknown:
			remainingUnknown++
		}
	}
	if targetIsVoter {
		total++
	}
	quorumSize := total/2 + 1
	survives := remainingReachable >= quorumSize

	var note string
	switch {
	case !targetFound:
		note = "node_id is not part of the current raft configuration - quorum arithmetic unaffected."
	case !targetIsVoter:
		note = "this node is a non-voting raft member (or staging) - quorum arithmetic unaffected."
	case total == 1:
		note = "this is the cluster's only voter - losing it ends the cluster's ability to reach quorum entirely."
	case !survives:
		note = "quorum is LOST - the confirmed-reachable remaining voters do not form a majority."
	case remainingUnknown > 0:
		note = fmt.Sprintf("quorum survives on confirmed-reachable voters alone, but %d remaining voter(s) have unverified reachability (no peer forwarding configured) - this verdict does not depend on them, but investigate before trusting it fully.", remainingUnknown)
	default:
		note = "quorum survives."
	}

	return QuorumImpact{
		TargetIsVoter:      targetIsVoter,
		TotalVoters:        total,
		RemainingVoters:    remaining,
		RemainingReachable: remainingReachable,
		RemainingUnknown:   remainingUnknown,
		QuorumSize:         quorumSize,
		Survives:           survives,
		Note:               note,
	}
}

// ComputeOwnedResourceImpacts returns a recovery verdict for every
// resource owned by targetNodeID, sorted by ID for deterministic
// ordering.
func ComputeOwnedResourceImpacts(all []OwnedResourcePlacement, targetNodeID string) []OwnedResourceImpact {
	var impacts []OwnedResourceImpact
	for _, r := range all {
		if r.NodeID != targetNodeID {
			continue
		}
		impact := OwnedResourceImpact{ID: r.ID, Name: r.Name, Kind: r.Kind, ReplicaNodeID: r.ReplicaNodeID}
		if r.ReplicaNodeID == "" {
			impact.Verdict = RecoveryVerdictUnprotected
			impact.Explanation = fmt.Sprintf("no HAST replica configured - Apiary has no redundancy path for this resource; recovery would depend on means outside its own tracking if %s is gone for good.", targetNodeID)
		} else {
			impact.Verdict = RecoveryVerdictUnverifiedReplica
			impact.Explanation = fmt.Sprintf("a HAST replica is configured on %s, but this simulation does not check live sync status - confirm `hastctl status` on %s shows `status: complete` before attempting recovery.", r.ReplicaNodeID, r.ReplicaNodeID)
		}
		impacts = append(impacts, impact)
	}
	sort.Slice(impacts, func(i, j int) bool { return impacts[i].ID < impacts[j].ID })
	return impacts
}

// ComputeReplicaBackedImpacts returns every resource for which
// targetNodeID is the configured HAST replica (not the owner), sorted
// by ID.
func ComputeReplicaBackedImpacts(all []OwnedResourcePlacement, targetNodeID string) []ReplicaBackedImpact {
	var impacts []ReplicaBackedImpact
	for _, r := range all {
		if r.ReplicaNodeID != targetNodeID {
			continue
		}
		impacts = append(impacts, ReplicaBackedImpact{
			ID: r.ID, Name: r.Name, Kind: r.Kind, OwnerNodeID: r.NodeID,
			Explanation: fmt.Sprintf("this resource keeps running unaffected on %s, but loses its HAST redundancy until a new replica is configured elsewhere.", r.NodeID),
		})
	}
	sort.Slice(impacts, func(i, j int) bool { return impacts[i].ID < impacts[j].ID })
	return impacts
}

// SimulateNodeFailure is the single entry point the RPC handler calls
// once IsKnownTarget has confirmed targetNodeID is real - a thin
// composition of the three computations above, kept separate so each
// is independently unit-testable.
func SimulateNodeFailure(servers []ServerSuffrage, resources []OwnedResourcePlacement, targetNodeID string) NodeFailureReport {
	return NodeFailureReport{
		Quorum:                 ComputeQuorumImpact(servers, targetNodeID),
		OwnedResources:         ComputeOwnedResourceImpacts(resources, targetNodeID),
		ReplicaBackedResources: ComputeReplicaBackedImpacts(resources, targetNodeID),
	}
}
