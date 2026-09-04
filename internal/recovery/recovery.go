// Package recovery implements Offline Recovery Handbook v1 (ADR-0057,
// CODEX.md's "Priority 4"): a printable, on-demand, versioned recovery
// document for exactly one scenario - loss of one hive - built entirely
// on top of already-exposed data (the Dependency Graph Simulator's
// SimulateNodeFailure RPC, ADR-0052/53/54, plus raft membership/leader
// identity). Like internal/health, this package is pure computation with
// no I/O: internal/frontend gathers the raw facts via RPC and calls
// BuildHandbook. Unlike internal/health, it imports internal/health
// directly rather than duplicating it - internal/health is itself small
// and dependency-free, so importing it carries none of the baggage the
// duplication of internal/cluster's own (much larger, OS-exec-heavy)
// types exists to avoid.
package recovery

import "github.com/glenjbarber/apiary/internal/health"

// QuorumVerdict is a real three-state result - see ClassifyQuorum.
// ComputeQuorumImpact's own Survives bool conflates "even counting every
// unknown-reachability voter as reachable, a majority is impossible"
// (genuinely Lost) with "the confirmed-reachable count alone falls
// short, but the deficit could close if the unknown voters turn out
// reachable" (Unknown, not Lost) - these need different operator
// guidance, so this package never reads a bare Survives bool.
type QuorumVerdict string

const (
	QuorumSurvives QuorumVerdict = "survives"
	QuorumLost     QuorumVerdict = "lost"
	QuorumUnknown  QuorumVerdict = "unknown"
)

// QuorumFact mirrors cluster.QuorumImpact fully (minus Survives/Note,
// which ClassifyQuorum and step-generation derive themselves) - carrying
// every field ValidQuorumFact needs to check internal consistency, not
// just what ClassifyQuorum's own branches read.
type QuorumFact struct {
	TargetIsVoter      bool
	TotalVoters        uint32
	RemainingVoters    uint32
	RemainingReachable uint32
	RemainingUnknown   uint32
	QuorumSize         uint32
}

// ResourceFact mirrors cluster.OwnedResourceImpact. ReplicaConfigured -
// not "Protected" - deliberately matches RecoveryVerdictUnverifiedReplica's
// own hedged naming (internal/cluster/simulate.go): a configured replica
// is not a *confirmed* one. Live HAST sync status has no cluster-wide RPC
// exposure - internal/hast.Manager.Status only ever runs hastctl locally,
// invoked only by the reconciler to self-verify its own role change -
// this package inherits that gap rather than silently fixing it.
type ResourceFact struct {
	ID, Name, Kind    string // Kind: "vm" or "jail"
	ReplicaNodeID     string
	ReplicaConfigured bool
}

// HASTResourceName mirrors vmHASTResourceName/jailHASTResourceName
// (internal/cluster/hast.go, internal/cluster/jail.go) exactly - a
// resource's HAST identifier is never its display Name. Small,
// deliberate duplication across the package boundary, the same
// convention internal/health already established for cluster.Reachability.
func HASTResourceName(kind, id string) string {
	if kind == "jail" {
		return "jail-" + id
	}
	return "vm-" + id
}

// ReplicaBackedFact mirrors cluster.ReplicaBackedImpact: a resource
// owned by a DIFFERENT node, for which the simulated target is the
// configured HAST replica.
type ReplicaBackedFact struct {
	ID, Name, Kind, OwnerNodeID string
}

// ImageVerdict mirrors cluster.ImageAvailabilityVerdict's three states
// exactly - "unknown" (inventory unreadable) and "unavailable" (a
// confirmed absence) must never be collapsed into one bool, since they
// carry very different operator guidance (verify vs. blocked).
type ImageVerdict string

const (
	ImageAvailable   ImageVerdict = "available"
	ImageUnavailable ImageVerdict = "unavailable"
	ImageUnknown     ImageVerdict = "unknown"
)

// ImageFact mirrors cluster.ImageAvailabilityImpact.
type ImageFact struct {
	ResourceID, ResourceName, ImageName string
	Verdict                             ImageVerdict
}

// AssumptionConcern mirrors internal/frontend's own assumptionResultView
// complete field set rather than a hand-picked subset: SubjectKind/
// SubjectID/DependencyID/Qualifier identify *what* was checked, and
// ObservedStatus vs. Status separates a merely-stale-but-once-true
// observation from a genuinely negative one (ADR-0055). Only non-"true"
// results are meant to be carried here, matching /assumptions' own
// filtering intent - a diligent reader needs to see status vs. staleness
// vs. why separately, the same "raw Observations, not just a conclusion"
// lesson internal/health already established.
type AssumptionConcern struct {
	Kind, SubjectKind, SubjectID, DependencyID, Qualifier string
	ObservedStatus                                        string // raw, as last measured - diagnostic only
	Status                                                string // effective status - "false" | "unknown" | "not_applicable"
	Stale                                                 bool
	LastObservedAt                                        string // pre-formatted, mirroring assumptionResultView's own display format
	ReasonCode                                            string
	Detail                                                string
}

// NodeContextFact is one scenario-relevant peer's bounded, embedded
// evidence - see internal/frontend/recovery_handbook.go for how the
// (at most 10) detail-checked nodes are selected and fetched.
// EvidenceLimitReached means this node was relevant but excluded from
// detailed checking because the node-count cap was hit; every other
// field is then zero-valued, but the node ID itself always stays listed
// on the rendered page - never silently dropped.
//
// Health reuses health.NodeHealth as-is rather than a separate
// "reachable" boolean: nodeHealthSignals (internal/frontend) never
// returns an error - a failed or timed-out peer call already flows into
// health.NodeSignals and comes back out as StatusUnknown or
// StatusContradictory with a citing Observation, which is exactly the
// existing, correct vocabulary for "this couldn't be verified." A
// second, less-informative boolean the underlying helper has no way to
// populate would only duplicate it worse. Assumptions are different:
// nodeAssumptions does return a real fetch error, so AssumptionsFetchError
// is a genuine, populatable field, not invented for symmetry.
type NodeContextFact struct {
	NodeID string
	Reason string // why this node is relevant, e.g. "replica target for vm-abc123"

	EvidenceLimitReached bool

	Health health.NodeHealth

	AssumptionsFetchError string // empty = succeeded
	Assumptions           []AssumptionConcern
	StorageDegraded       bool
	StorageDegradedDetail string
}

// Inputs is the raw per-request state BuildHandbook computes from - the
// only place new facts enter this package.
type Inputs struct {
	TargetNodeID    string
	IsCurrentLeader bool
	Quorum          QuorumFact

	OwnedResources []ResourceFact
	ReplicaBacked  []ReplicaBackedFact
	Images         []ImageFact
	NodeContext    []NodeContextFact
}

// Step is one numbered recovery action. StopCondition empty means none.
type Step struct {
	Order         int
	Title, Detail string
	Irreversible  bool
	StopCondition string
}

// Handbook is BuildHandbook's full output for one target hive.
type Handbook struct {
	TargetNodeID  string
	QuorumVerdict QuorumVerdict
	Steps         []Step
}
