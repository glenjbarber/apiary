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
	"strings"
)

// ResourceKind distinguishes a VM from a jail.
type ResourceKind string

const (
	ResourceKindVM   ResourceKind = "vm"
	ResourceKindJail ResourceKind = "jail"
)

// RecoveryVerdict is deliberately limited to what raft configuration
// data, node_id/replica_node_id placement, and a direct read of the
// configured replica's own HAST status can prove. ADR-0052 originally
// capped every replica-backed resource at
// RecoveryVerdictUnverifiedReplica because live HAST sync status had no
// RPC exposure anywhere in this codebase; ADR-0121 closes that gap
// using the per-node GetLocalHASTResourceStatus RPC, so the three
// replica verdicts below now separate a confirmed-healthy replica from
// a confirmed-unusable one and from one that could not be read at all.
//
// No verdict here - including the strongest, RecoveryVerdictReplicaInSync
// - claims that recovery "will work cleanly". A sync observation is a
// point-in-time fact about the replica's hastd worker; it cannot prove
// the replica stays reachable, that the owning disk survives, or that
// the Cell can actually be recreated from it. See the explanation
// strings, which are part of the contract, not decoration.
type RecoveryVerdict string

const (
	// RecoveryVerdictUnprotected means no replica_node_id is configured.
	// This proves only the absence of HAST-based redundancy Apiary
	// knows about - not that the resource's data is permanently
	// unrecoverable by any means (a surviving physical disk or an
	// external backup might still exist outside Apiary's own tracking).
	RecoveryVerdictUnprotected RecoveryVerdict = "unprotected"

	// RecoveryVerdictUnverifiedReplica means replica_node_id is set, but
	// no live observation was even attempted - e.g. this node has no
	// peer forwarding configured, so the replica could not be queried
	// at all. This is deliberately distinct from
	// RecoveryVerdictReplicaUnobserved below, which means a query WAS
	// attempted and did not return a usable answer.
	RecoveryVerdictUnverifiedReplica RecoveryVerdict = "unverified_replica"

	// RecoveryVerdictReplicaInSync means the configured replica was
	// queried directly and its own hastd reported a real role (primary
	// or secondary) with status "complete" (ADR-0121). This is the
	// strongest verdict available, and it still is not a guarantee -
	// see the type comment.
	RecoveryVerdictReplicaInSync RecoveryVerdict = "replica_in_sync"

	// RecoveryVerdictReplicaOutOfSync means the replica was queried
	// directly and is confirmed NOT usable as-is: hastd reports role
	// "init" (never initialized on that node), or a real role with a
	// status other than "complete" (e.g. "degraded"). This is the
	// verdict that should make an operator stop and investigate before
	// assuming redundancy exists.
	RecoveryVerdictReplicaOutOfSync RecoveryVerdict = "replica_out_of_sync"

	// RecoveryVerdictReplicaUnobserved means the replica was queried but
	// could not be read: the RPC returned an error, or hastd itself
	// reported status "unknown". This is a real third state and must
	// never be silently folded into either in-sync or out-of-sync -
	// "we could not check" is not "checked and fine", and it is not
	// "checked and broken" either.
	RecoveryVerdictReplicaUnobserved RecoveryVerdict = "replica_unobserved"
)

// ReplicaSyncObservation is one directly-queried, node-local HAST
// status for a resource's configured replica. It is a raw observation
// gathered by the RPC handler (which does the I/O); this package only
// interprets it. Observed=false means the replica's own status could not
// be read - never that the replica is healthy, and never that it is not.
type ReplicaSyncObservation struct {
	ResourceID    string
	Kind          ResourceKind
	ReplicaNodeID string

	// Observed is true only when the replica node answered with its own
	// hastctl view. When false, Detail carries the reason.
	Observed       bool
	Role           string
	ResourceStatus string
	Replication    string
	Detail         string
}

// replicaInSync classifies one observation. hastd's own `status` field
// is the authority here (ADR-0057 recorded a real primary/secondary pair
// both reporting "status: complete" on this project's FreeBSD 16.0
// build), and role "init" means the resource was never initialized on
// that node at all, which is a confirmed-unusable replica rather than an
// unknown one. Only hastd's own "unknown" is treated as undeterminable.
func replicaInSync(o ReplicaSyncObservation) (inSync bool, undeterminable bool) {
	if !o.Observed {
		return false, true
	}
	switch {
	case strings.EqualFold(o.Role, hastRoleInit):
		return false, false
	case strings.EqualFold(o.ResourceStatus, hastStatusUnknown):
		return false, true
	case strings.EqualFold(o.ResourceStatus, hastStatusComplete) &&
		(strings.EqualFold(o.Role, hastRolePrimary) || strings.EqualFold(o.Role, hastRoleSecondary)):
		return true, false
	default:
		return false, false
	}
}

const (
	hastRoleInit       = "init"
	hastRolePrimary    = "primary"
	hastRoleSecondary  = "secondary"
	hastStatusComplete = "complete"
	hastStatusUnknown  = "unknown"
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

	// Voters is every OTHER raft voter (never the target itself, never a
	// non-voter) with its own live reachability, sorted by ID. This is
	// the same data RemainingReachable/RemainingUnknown already
	// summarize as counts - kept here too so a caller can show which
	// specific voter(s) drove the verdict, not just the aggregate.
	Voters []VoterReachability
}

// VoterReachability is one remaining raft voter's identity and live
// reachability.
type VoterReachability struct {
	ID           string
	Reachability Reachability
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

	// ReplicaSync is the raw evidence behind Verdict, present only when
	// a replica is configured AND a direct query to that replica was
	// actually attempted. nil means either "no replica configured" or
	// "no query was possible" - in both cases the verdict must not be
	// read as evidence-backed. Verdict and this field are always
	// consistent: an in-sync/out-of-sync/unobserved verdict always
	// carries the observation that produced it.
	ReplicaSync *ReplicaSyncEvidence
}

// ReplicaSyncEvidence is the verbatim, node-local HAST observation a
// recovery verdict was derived from, so an operator can see the fact
// itself instead of only Apiary's conclusion drawn from it.
type ReplicaSyncEvidence struct {
	NodeID         string
	Observed       bool
	Role           string
	ResourceStatus string
	Replication    string

	// Detail carries the failure reason when Observed is false.
	Detail string
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
	ImageAvailability      []ImageAvailabilityImpact
}

type ImageRole string

const (
	ImageRoleISO       ImageRole = "iso"
	ImageRoleBaseImage ImageRole = "base_image"

	// ImageRoleBaseArchive is a jail's base_archive_name (ADR-0098) -
	// the jail equivalent of ImageRoleBaseImage.
	ImageRoleBaseArchive ImageRole = "base_archive"
)

type ImageAvailabilityVerdict string

const (
	ImageAvailabilityAvailable   ImageAvailabilityVerdict = "available"
	ImageAvailabilityUnavailable ImageAvailabilityVerdict = "unavailable"
	ImageAvailabilityUnknown     ImageAvailabilityVerdict = "unknown"
)

type ImageRequirement struct {
	ResourceID   string
	ResourceName string
	ImageName    string
	Role         ImageRole
}

// ImageInventoryObservation is one remaining Hive's directly queried image
// inventory. Observed=false means the inventory could not be read, never that
// the Hive has no images.
type ImageInventoryObservation struct {
	NodeID   string
	Observed bool
	Names    []string
}

type ImageAvailabilityImpact struct {
	ResourceID   string
	ResourceName string
	ImageName    string
	Role         ImageRole
	Verdict      ImageAvailabilityVerdict
	SourceNodes  []string
	UnknownNodes []string
	Explanation  string
}

// ManagedNetworkPlacement is the replicated portion of a managed network
// needed for a read-only network-failure simulation. It deliberately excludes
// bridge status because that observation is node-local and cannot describe the
// cluster-wide logical network consistently.
type ManagedNetworkPlacement struct {
	ID              string
	Name            string
	VLANID          uint32
	Subnet          string
	BridgeName      string
	ExternalGateway string
}

// NetworkAttachedResourcePlacement is a Cell whose declared connectivity
// depends on a managed network. Apiary currently attaches only VMs to managed
// networks; jails still use ip4=inherit.
type NetworkAttachedResourcePlacement struct {
	ID        string
	Name      string
	NodeID    string
	NetworkID string
}

type NetworkFailureImpact struct {
	ID          string
	Name        string
	NodeID      string
	Explanation string
}

type NetworkFailureReport struct {
	Network           ManagedNetworkPlacement
	AffectedResources []NetworkFailureImpact
	Note              string
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
	var voters []VoterReachability
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
		voters = append(voters, VoterReachability{ID: s.ID, Reachability: s.Reachability})
		switch s.Reachability {
		case ReachabilityReachable:
			remainingReachable++
		case ReachabilityUnknown:
			remainingUnknown++
		}
	}
	sort.Slice(voters, func(i, j int) bool { return voters[i].ID < voters[j].ID })
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
		Voters:             voters,
	}
}

// ComputeOwnedResourceImpacts returns a recovery verdict for every
// resource owned by targetNodeID, sorted by ID for deterministic
// ordering. syncObservations are the direct per-replica HAST reads the
// RPC handler gathered; a resource whose replica could not be queried
// at all still falls back to RecoveryVerdictUnverifiedReplica, so
// dropping the observations entirely is safe and simply reproduces
// ADR-0052's original placement-only behaviour.
func ComputeOwnedResourceImpacts(all []OwnedResourcePlacement, syncObservations []ReplicaSyncObservation, targetNodeID string) []OwnedResourceImpact {
	byResource := make(map[string]ReplicaSyncObservation, len(syncObservations))
	for _, o := range syncObservations {
		byResource[o.ResourceID] = o
	}

	var impacts []OwnedResourceImpact
	for _, r := range all {
		if r.NodeID != targetNodeID {
			continue
		}
		impact := OwnedResourceImpact{ID: r.ID, Name: r.Name, Kind: r.Kind, ReplicaNodeID: r.ReplicaNodeID}
		switch {
		case r.ReplicaNodeID == "":
			impact.Verdict = RecoveryVerdictUnprotected
			impact.Explanation = fmt.Sprintf("no HAST replica configured - Apiary has no redundancy path for this resource; recovery would depend on means outside its own tracking if %s is gone for good.", targetNodeID)
		default:
			observation, attempted := byResource[r.ID]
			if !attempted {
				impact.Verdict = RecoveryVerdictUnverifiedReplica
				impact.Explanation = fmt.Sprintf("a HAST replica is configured on %s, but this simulation could not query its live HAST status at all - confirm `hastctl status` on %s shows `status: complete` before attempting recovery.", r.ReplicaNodeID, r.ReplicaNodeID)
				break
			}
			impact.ReplicaSync = &ReplicaSyncEvidence{
				NodeID: observation.ReplicaNodeID, Observed: observation.Observed,
				Role: observation.Role, ResourceStatus: observation.ResourceStatus,
				Replication: observation.Replication, Detail: observation.Detail,
			}
			inSync, undeterminable := replicaInSync(observation)
			switch {
			case inSync:
				impact.Verdict = RecoveryVerdictReplicaInSync
				impact.Explanation = fmt.Sprintf("a HAST replica is configured on %s and its own hastd reported role %q with status %q just now. This is the strongest evidence available, not a guarantee: it says nothing about whether that node stays reachable, or whether the Cell can be recreated from the replica.", observation.ReplicaNodeID, observation.Role, observation.ResourceStatus)
			case undeterminable:
				impact.Verdict = RecoveryVerdictReplicaUnobserved
				impact.Explanation = fmt.Sprintf("a HAST replica is configured on %s, but its live HAST status could not be determined (%s) - this is 'could not check', not 'checked and fine'. Confirm `hastctl status` on %s by hand before attempting recovery.", observation.ReplicaNodeID, syncDetailOrUnknown(observation), observation.ReplicaNodeID)
			default:
				impact.Verdict = RecoveryVerdictReplicaOutOfSync
				impact.Explanation = fmt.Sprintf("a HAST replica is configured on %s but is confirmed NOT usable as-is: hastd reported role %q with status %q. Treat this Cell as having no working redundancy until an operator resolves it on %s.", observation.ReplicaNodeID, observation.Role, observation.ResourceStatus, observation.ReplicaNodeID)
			}
		}
		impacts = append(impacts, impact)
	}
	sort.Slice(impacts, func(i, j int) bool { return impacts[i].ID < impacts[j].ID })
	return impacts
}

// syncDetailOrUnknown renders an observation's failure reason for an
// explanation string, never leaving an empty parenthetical.
func syncDetailOrUnknown(o ReplicaSyncObservation) string {
	if o.Detail != "" {
		return o.Detail
	}
	return "no reason reported"
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
// is independently unit-testable. syncObservations is threaded straight
// through to ComputeOwnedResourceImpacts.
func SimulateNodeFailure(servers []ServerSuffrage, resources []OwnedResourcePlacement, syncObservations []ReplicaSyncObservation, targetNodeID string) NodeFailureReport {
	return NodeFailureReport{
		Quorum:                 ComputeQuorumImpact(servers, targetNodeID),
		OwnedResources:         ComputeOwnedResourceImpacts(resources, syncObservations, targetNodeID),
		ReplicaBackedResources: ComputeReplicaBackedImpacts(resources, targetNodeID),
	}
}

// ComputeImageAvailability reports whether each referenced image can still be
// found on a remaining Hive after targetNodeID disappears. Availability is
// based on direct ListISOs observations, not raft state. One confirmed source
// is sufficient for "available"; otherwise any unread inventory makes the
// result "unknown", and "unavailable" requires every remaining inventory to
// have been read successfully.
func ComputeImageAvailability(requirements []ImageRequirement, inventories []ImageInventoryObservation, targetNodeID string) []ImageAvailabilityImpact {
	impacts := make([]ImageAvailabilityImpact, 0, len(requirements))
	for _, requirement := range requirements {
		if requirement.ImageName == "" {
			continue
		}
		impact := ImageAvailabilityImpact{
			ResourceID: requirement.ResourceID, ResourceName: requirement.ResourceName,
			ImageName: requirement.ImageName, Role: requirement.Role,
		}
		for _, inventory := range inventories {
			if inventory.NodeID == targetNodeID {
				continue
			}
			if !inventory.Observed {
				impact.UnknownNodes = append(impact.UnknownNodes, inventory.NodeID)
				continue
			}
			for _, name := range inventory.Names {
				if name == requirement.ImageName {
					impact.SourceNodes = append(impact.SourceNodes, inventory.NodeID)
					break
				}
			}
		}
		sort.Strings(impact.SourceNodes)
		sort.Strings(impact.UnknownNodes)
		switch {
		case len(impact.SourceNodes) > 0:
			impact.Verdict = ImageAvailabilityAvailable
			impact.Explanation = fmt.Sprintf("image remains available from %s", strings.Join(impact.SourceNodes, ", "))
		case len(impact.UnknownNodes) > 0:
			impact.Verdict = ImageAvailabilityUnknown
			impact.Explanation = fmt.Sprintf("no remaining source was confirmed, but inventories could not be read from %s", strings.Join(impact.UnknownNodes, ", "))
		default:
			impact.Verdict = ImageAvailabilityUnavailable
			impact.Explanation = "no remaining Comb reports this image; future provisioning or recovery that needs it would be blocked"
		}
		impacts = append(impacts, impact)
	}
	sort.Slice(impacts, func(i, j int) bool {
		if impacts[i].ResourceID != impacts[j].ResourceID {
			return impacts[i].ResourceID < impacts[j].ResourceID
		}
		if impacts[i].Role != impacts[j].Role {
			return impacts[i].Role < impacts[j].Role
		}
		return impacts[i].ImageName < impacts[j].ImageName
	})
	return impacts
}

// SimulateNetworkFailure reports the Cells whose declared managed-network
// attachment would disappear with target. It does not claim that the Cell
// process or storage stops, nor that every service inside the Cell becomes
// unreachable: Apiary does not model guest routes, additional interfaces, or
// service dependencies yet.
func SimulateNetworkFailure(target ManagedNetworkPlacement, resources []NetworkAttachedResourcePlacement) NetworkFailureReport {
	impacts := make([]NetworkFailureImpact, 0)
	for _, resource := range resources {
		if resource.NetworkID != target.ID {
			continue
		}
		impacts = append(impacts, NetworkFailureImpact{
			ID:     resource.ID,
			Name:   resource.Name,
			NodeID: resource.NodeID,
			Explanation: fmt.Sprintf(
				"managed-network connectivity through %s would be unavailable; this simulation does not claim that the Cell process or storage stops",
				target.ID,
			),
		})
	}
	sort.Slice(impacts, func(i, j int) bool { return impacts[i].ID < impacts[j].ID })

	return NetworkFailureReport{
		Network:           target,
		AffectedResources: impacts,
		Note:              "This report follows declared network_id attachments only. It does not observe guest routing, additional interfaces, service dependencies, or current packet flow.",
	}
}
