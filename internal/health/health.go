// Package health implements Evidence-Aware Health v1 (ADR-0056,
// CODEX.md's "Priority 3"): a status contract where a Healthy verdict
// means recently proven by cited evidence, never merely "no error
// reported." ComputeNodeHealth is a pure function - this package does no
// I/O and holds no state; every fact it reasons about is supplied by the
// caller as NodeSignals, gathered fresh per request (this feature is
// deliberately on-demand, not a persisted background checker - see
// ADR-0056's own scoping decision, unlike internal/assumptions).
package health

import "time"

// Status is the five-state verdict CODEX.md itself names. Healthy is
// never the default - every other state must be affirmatively ruled out
// by a specific, cited Observation.
type Status string

const (
	StatusHealthy       Status = "healthy"
	StatusDegraded      Status = "degraded"
	StatusUnknown       Status = "unknown"
	StatusStale         Status = "stale"
	StatusContradictory Status = "contradictory"
)

// Reachability mirrors internal/cluster.Reachability's exact three-state
// semantics (Reachable/Unreachable/Unknown-when-uncheckable), deliberately
// duplicated rather than imported - internal/cluster is a large package
// (ZFS/bhyve/jail/HAST reconciliation) and importing it into this small,
// pure package for one 3-value type is the wrong trade. This project's
// own established convention already accepts small duplication across
// package boundaries (peerManagerdAddr, defaultPeerManagerdPort,
// apiKeyCredentials are each duplicated 2-3x by design) - see
// cluster.Reachability's own doc comment for the reasoning this mirrors.
type Reachability string

const (
	ReachabilityReachable   Reachability = "reachable"
	ReachabilityUnreachable Reachability = "unreachable"
	ReachabilityUnknown     Reachability = "unknown"
)

// Suffrage is this package's own translation of raft's wire-level
// suffrage string (api/rpc's RaftMember.suffrage). ParseSuffrage is the
// ONLY place that raw string is ever read - no verdict-logic branch in
// compute.go compares raw strings directly, since an earlier draft of
// this design did exactly that and silently never matched (a lowercase
// comparison against the real, capitalized "Voter"/"Nonvoter"/
// "Staging"/"Unknown" values raft actually reports).
type Suffrage string

const (
	SuffrageVoter    Suffrage = "voter"
	SuffrageNonvoter Suffrage = "nonvoter"
	SuffrageStaging  Suffrage = "staging"

	// SuffrageUnknown covers BOTH raft's own real reported "Unknown"
	// value (internal/raft.suffrageString's explicit default case - a
	// genuine, live value, not a placeholder for "we didn't check") AND
	// any raw string this package doesn't recognize (e.g. a future
	// hashicorp/raft upgrade introducing a new suffrage kind). Both
	// cases must cap ComputeNodeHealth's verdict at Unknown, never
	// Healthy - raft's own admission of uncertainty must never be
	// silently promoted to a healthy answer.
	SuffrageUnknown Suffrage = "unknown"
)

// ParseSuffrage converts a raw wire-level suffrage string (as reported by
// RaftMember.suffrage / internal/raft.suffrageString) into a Suffrage.
// Any value other than the three real non-Unknown raft suffrages maps to
// SuffrageUnknown.
func ParseSuffrage(raw string) Suffrage {
	switch raw {
	case "Voter":
		return SuffrageVoter
	case "Nonvoter":
		return SuffrageNonvoter
	case "Staging":
		return SuffrageStaging
	default:
		return SuffrageUnknown
	}
}

// Observation is one raw, independently-gathered fact - never itself a
// conclusion. CODEX.md's own instruction is to "keep raw observations
// separate from the conclusions derived from them"; NodeHealth.
// Observations is where every fact ComputeNodeHealth reasoned about is
// preserved, regardless of which branch of that reasoning fired.
type Observation struct {
	// Source names what was checked, e.g. "peer_reachability",
	// "raft_membership", "raft_heartbeat", "raft_applied_index",
	// "reconciler_last_success".
	Source string

	// ObservedAt is when THIS check/fetch happened - not when the
	// underlying fact last changed.
	ObservedAt time.Time

	// FreshnessLimit is the age past which this Observation should be
	// considered too old to trust for its own purpose. Zero means no
	// freshness limit applies (e.g. a point-in-time fact gathered fresh
	// this same request, like reachability itself).
	FreshnessLimit time.Duration

	// Value is a short, stable token, e.g. "unreachable", "voter",
	// "not_applicable". Detail is free-text human prose that may state a
	// caveat plainly (e.g. "last_log_index includes uncommitted entries
	// - not used to derive this verdict").
	Value  string
	Detail string
}

// NodeHealth is the ONLY thing a naive consumer needs to read for a safe
// answer - Status already reflects every staleness/unknown/contradiction
// rule ComputeNodeHealth applies. This mirrors the exact structural
// lesson ADR-0055's observed_status/status split established: safety
// lives in the data a consumer actually reads, never in a convention
// they have to separately know about. Observations is for a diligent
// consumer or the UI's expandable detail - never required for a safe
// read of Status alone.
type NodeHealth struct {
	NodeID       string
	Status       Status
	Explanation  string
	Observations []Observation
}

// NodeSignals is the raw per-node gathered state - the only place new
// facts enter this package. Every field's zero value must be a real,
// representable state (e.g. "not observed"), never silently treated as
// a negative or positive answer by ComputeNodeHealth.
type NodeSignals struct {
	NodeID string

	// PeerReachability is transport-level: could the caller actually
	// dial and call this node's managerd just now. Trivially Reachable
	// for the local/serving node (no dial needed at all - see
	// internal/frontend's wiring, which reuses its own already-answered
	// anchor call rather than dialing itself).
	PeerReachability Reachability

	// HeartbeatObserved/HeartbeatOK are meaningful only when
	// PeerReachability == Reachable: did a usable Status() payload come
	// back, and did THAT node's own report say its local raftd is
	// reachable (its own raft_reachable field). Kept as a genuinely
	// separate fact from PeerReachability so "this node's managerd is up
	// but its own raftd died" is never confused with "we could not reach
	// this node at all."
	HeartbeatObserved bool
	HeartbeatOK       bool

	// MembershipObserved/IsRaftMember/Suffrage come from the ONE raft
	// membership read already fetched once per page load - never
	// fetched per-node, since raft membership is a cluster-wide-
	// consistent replicated fact (a per-node self-report would add no
	// defensive value and creates an undesigned disagreement case this
	// v1 does not attempt to resolve). MembershipObserved false means
	// that one anchor read's own raft was unreachable - in that case
	// IsRaftMember/Suffrage are meaningless for EVERY node being
	// evaluated this request, not just one.
	MembershipObserved   bool
	IsRaftMember         bool
	Suffrage             Suffrage
	MembershipObservedAt time.Time

	// AppliedIndex/LastLogIndex are genuinely per-node (each node's own
	// raft log position, not a replicated fact) - this is why they
	// require a fresh per-node Status() call even though membership
	// above does not. ComputeNodeHealth deliberately does NOT derive any
	// verdict from these: LastLogIndex includes uncommitted log entries
	// (raft's own LastIndex, not CommitIndex, which this codebase has no
	// access to), so any numeric threshold here would be fabricated
	// precision, not evidence. They are cited as raw Observations only.
	AppliedIndexObserved bool
	AppliedIndex         uint64
	LastLogIndex         uint64
	IndicesObservedAt    time.Time

	// ReconcilerConfigured false means this node's managerd was not
	// built with a Reconciler at all - kept distinct from "configured
	// but never ticked yet," which is a different, more concerning fact.
	// ReconcileIntervalSeconds is that node's OWN configured
	// -reconcile-interval (meaningless if !ReconcilerConfigured) - used
	// to derive a per-node freshness limit, never a single global
	// constant, since that flag is itself per-node/uncoordinated.
	ReconcilerConfigured     bool
	ReconcileIntervalSeconds uint32
	ReconcileEverAttempted   bool
	LastReconcileAttempt     time.Time
	ReconcileEverSucceeded   bool
	LastReconcileSuccess     time.Time
	ReconcileObservedAt      time.Time
}

// reconcileFreshnessMultiplier mirrors -assumption-stale-after's own
// already-reviewed precedent (cmd/managerd/main.go: "3x the check
// interval, not an invented number") - applied per node against that
// node's own reported ReconcileIntervalSeconds, never one global
// constant.
const reconcileFreshnessMultiplier = 3
