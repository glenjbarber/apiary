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

// Inputs is the raw, per-node material a caller gathered by whatever
// means it likes, reduced to plain Go values. It exists so the rules
// that turn scattered observations into a NodeSignals live in exactly
// one place instead of once per caller - ADR-0122 moved them here from
// internal/frontend, which was then the only implementation, and
// internal/manager's ClusterHealth RPC as a second consumer.
//
// A caller still owns all I/O. This type and SignalsFrom do no
// gathering, no dialing, and hold no state: they exist to keep the
// *derivation* from drifting between a UI path and an API path, which
// would be the worst possible failure for a feature whose entire
// promise is that one answer means the same thing everywhere.
type Inputs struct {
	NodeID string

	// Membership comes from the one anchor read shared by every node
	// being evaluated in a single request - raft membership is a
	// cluster-wide-consistent replicated fact, so it is never fetched
	// per-node. MemberFound false with MembershipObserved true means
	// this node is genuinely not a raft member.
	MembershipObserved   bool
	MembershipObservedAt time.Time
	MemberFound          bool
	Suffrage             Suffrage

	// IsLocal is the one caller-side fact that is true by definition
	// rather than by observation: a node answering its own request needs
	// no dial to be reachable.
	IsLocal bool

	// PeerForwardingConfigured records whether the answering node can
	// dial peers at all. When false, no remote node's reachability is
	// ever established by observation, and every one of them must read
	// as Unknown rather than inheriting a local answer.
	PeerForwardingConfigured bool

	// PeerDialAttempted/PeerDialSucceeded describe an actual dial to
	// this node. Attempted false means nothing was tried, which is
	// different from tried-and-failed.
	PeerDialAttempted bool
	PeerDialSucceeded bool

	// Heartbeat is this node's own self-report about its local raft.
	// Meaningful only once the node is known reachable.
	HeartbeatObserved bool
	HeartbeatOK       bool

	AppliedIndexObserved bool
	AppliedIndex         uint64
	LastLogIndex         uint64
	IndicesObservedAt    time.Time

	// ReconcilerConfigured is derived from a nonzero interval by the
	// caller (see SignalsFrom) - a 0 interval is the only reliable
	// "no Reconciler here" signal, since a 0 timestamp alone cannot
	// distinguish that from "configured but never ticked."
	ReconcileIntervalSeconds uint32
	ReconcileObservedAt      time.Time
	ReconcileEverAttempted   bool
	LastReconcileAttempt     time.Time
	ReconcileEverSucceeded   bool
	LastReconcileSuccess     time.Time
}

// SignalsFrom applies the fixed derivation rules that turn gathered
// observations into the exact NodeSignals ComputeNodeHealth expects. It
// performs no evaluation of its own and never decides a verdict - it
// only decides what was and was not observed, which is the part two
// independent implementations would otherwise get subtly different.
func SignalsFrom(in Inputs) NodeSignals {
	s := NodeSignals{
		NodeID:                   in.NodeID,
		MembershipObserved:       in.MembershipObserved,
		MembershipObservedAt:     in.MembershipObservedAt,
		AppliedIndexObserved:     in.AppliedIndexObserved,
		AppliedIndex:             in.AppliedIndex,
		LastLogIndex:             in.LastLogIndex,
		IndicesObservedAt:        in.IndicesObservedAt,
		ReconcileIntervalSeconds: in.ReconcileIntervalSeconds,
		ReconcileObservedAt:      in.ReconcileObservedAt,
		ReconcileEverAttempted:   in.ReconcileEverAttempted,
		LastReconcileAttempt:     in.LastReconcileAttempt,
		ReconcileEverSucceeded:   in.ReconcileEverSucceeded,
		LastReconcileSuccess:     in.LastReconcileSuccess,
	}

	if in.MembershipObserved && in.MemberFound {
		s.IsRaftMember = true
		s.Suffrage = in.Suffrage
	} else if in.MemberFound {
		// A member row was found but the shared anchor read itself did
		// not confirm - membership must not be inferred from the row.
		s.Suffrage = in.Suffrage
	}

	switch {
	case in.IsLocal:
		s.PeerReachability = ReachabilityReachable
	case !in.PeerForwardingConfigured || !in.PeerDialAttempted:
		// Nothing was dialed, so nothing was learned. This must never
		// inherit the local node's trivially-true reachability.
		s.PeerReachability = ReachabilityUnknown
	case in.PeerDialSucceeded:
		s.PeerReachability = ReachabilityReachable
	default:
		s.PeerReachability = ReachabilityUnreachable
	}

	s.HeartbeatObserved = in.HeartbeatObserved
	s.HeartbeatOK = in.HeartbeatOK
	s.ReconcilerConfigured = in.ReconcileIntervalSeconds > 0

	return s
}
