// Package invariant implements Operational Invariants v1 (ADR-0060,
// CODEX.md's "Operational Invariants"): a small, named catalog of
// safety rules, each continuously evaluated to CODEX's own literal
// three-state vocabulary - true, false, or unknown - with the evidence
// and freshness behind that result. Like internal/health and
// internal/recovery, this package is pure computation with no I/O:
// internal/frontend gathers the raw facts via RPC and calls the
// Evaluate* functions below.
//
// This is deliberately the "continuously evaluate and report" half of
// CODEX's own text only - the "proactively block an unsafe plan" half
// needs a Flight Plan execution engine that does not exist in Apiary
// yet (CODEX.md itself calls Flight Plan "a future design direction,
// not implemented functionality").
package invariant

import (
	"time"

	"github.com/glenjbarber/apiary/internal/recovery"
)

// Result is CODEX's own literal three-state vocabulary - never a
// fourth "not applicable" state the way pathtrace/assumptions have,
// since Operational Invariants' own spec names exactly these three.
type Result string

const (
	ResultTrue    Result = "true"
	ResultFalse   Result = "false"
	ResultUnknown Result = "unknown"
)

// Evidence carries freshness explicitly - CODEX's own text requires
// "the evidence and freshness behind that result," and
// internal/health.Observation (the established precedent this package
// mirrors) already has an ObservedAt field. A zero ObservedAt means
// this evidence was never runtime-observed at all (see
// EvaluateOwnershipGatedDeletion below), distinct from "observed a
// while ago."
type Evidence struct {
	Source     string
	Detail     string
	ObservedAt time.Time
}

// Evaluation is one invariant's current verdict for one scope.
type Evaluation struct {
	Name        string // stable id: "quorum-tolerance", "hast-dual-primary", "cell-recoverability", "network-route-dns", "ownership-gated-deletion"
	Scope       string // "cluster", or a specific resource/network id
	Result      Result
	Explanation string
	Evidence    []Evidence
}

// Reachability is a small, deliberate duplicate of the same 3-value
// concept internal/health.Reachability already duplicates from
// cluster.Reachability - internal/recovery does not define one itself
// (it only carries raw counts in QuorumFact), so this is a third,
// equally-deliberate copy rather than an awkward import of
// internal/health for one unrelated enum.
type Reachability string

const (
	ReachabilityReachable   Reachability = "reachable"
	ReachabilityUnreachable Reachability = "unreachable"
	ReachabilityUnknown     Reachability = "unknown"
)

// VoterReachability is one current raft voter's real, current
// reachability - gathered ONCE per voter by the caller, then reused
// here to classify every voter's own hypothetical loss without any
// further RPCs (see EvaluateQuorumTolerance).
type VoterReachability struct {
	NodeID       string
	Reachability Reachability
}

// hastGapDetail is the disclosed, permanent reason both the HAST
// dual-primary invariant and cell-recoverability's own sync half can
// never resolve better than Unknown - live HAST sync/role status has
// no RPC exposure anywhere in this codebase (internal/hast.Manager's
// SetRole/Status only ever shell out to hastctl locally; the only
// caller, internal/cluster/hast.go's ensureHASTResourceAndRole, re-reads
// local status purely to self-verify a role change it just made).
const hastGapDetail = "live HAST role/sync status has no RPC exposure anywhere in this codebase - a node cannot learn another node's current HAST role or sync state, only its own."

// EvaluateHASTDualPrimary is always Unknown - one evaluation per
// resource ID that has a replica configured - citing the permanent
// no-RPC-exposure gap above. "No HAST resource has two writable
// primaries" is structurally unverifiable in v1, never silently
// promoted to a passing check.
func EvaluateHASTDualPrimary(resourceIDs []string) []Evaluation {
	evals := make([]Evaluation, 0, len(resourceIDs))
	for _, id := range resourceIDs {
		evals = append(evals, Evaluation{
			Name:        "hast-dual-primary",
			Scope:       id,
			Result:      ResultUnknown,
			Explanation: "Cannot confirm this resource has exactly one writable HAST primary.",
			Evidence: []Evidence{{
				Source: "internal/hast (no cluster-wide role RPC)",
				Detail: hastGapDetail,
			}},
		})
	}
	return evals
}

// EvaluateOwnershipGatedDeletion is a single, cluster-wide, static
// True evaluation - unlike every other invariant in this package, it
// is not computed from live request-time evidence at all. Its
// Evidence has a zero ObservedAt and names the actual enforcing code:
// ForcePurgeVM/ForcePurgeJail (internal/manager/server.go) both gate on
// the resource's raft-replicated desired_state already being
// VM_STATE_DELETING/JAIL_STATE_DELETING before submitting a purge, and
// the reconciler's physical-destroy path (internal/cluster/reconciler.go's
// teardownVM and its jail equivalent) is only ever reached once that
// same tombstone is already set - there is no code path that
// physically destroys a dataset/VM/jail without it. This is a
// structural guarantee verified by code review and this project's own
// regression tests (e.g. TestIntegration_ForcePurgeVM_RequiresDeletingState),
// not a live per-request check the way every other evaluation in this
// package is - callers should render it in a visually distinct
// section, discriminated by this zero ObservedAt, not by parsing
// Explanation text.
func EvaluateOwnershipGatedDeletion() Evaluation {
	return Evaluation{
		Name:   "ownership-gated-deletion",
		Scope:  "cluster",
		Result: ResultTrue,
		Explanation: "No physical resource (ZFS dataset, bhyve VM, or jail) is destroyed without its " +
			"raft-replicated desired_state already recording deletion - this is enforced by construction, " +
			"not independently monitored at runtime.",
		Evidence: []Evidence{{
			Source: "code construction (not runtime-monitored)",
			Detail: "ForcePurgeVM/ForcePurgeJail (internal/manager/server.go) require desired_state == " +
				"DELETING before submitting a purge; internal/cluster/reconciler.go's teardownVM and its jail " +
				"equivalent are only reachable once that same tombstone is already set.",
		}},
	}
}

// quorumFactFromVoters assembles a recovery.QuorumFact for the
// hypothetical loss of exactly one voter (x) from the full current
// voter set, using the single, already-gathered reachability snapshot -
// no further RPCs. x is by definition a voter (callers only ever pass
// voters), so TargetIsVoter is always true.
func quorumFactFromVoters(voters []VoterReachability, x VoterReachability) recovery.QuorumFact {
	total := uint32(len(voters))
	var remainingVoters, remainingReachable, remainingUnknown uint32
	for _, v := range voters {
		if v.NodeID == x.NodeID {
			continue
		}
		remainingVoters++
		switch v.Reachability {
		case ReachabilityReachable:
			remainingReachable++
		case ReachabilityUnknown:
			remainingUnknown++
		}
	}
	return recovery.QuorumFact{
		TargetIsVoter:      true,
		TotalVoters:        total,
		RemainingVoters:    remainingVoters,
		RemainingReachable: remainingReachable,
		RemainingUnknown:   remainingUnknown,
		QuorumSize:         total/2 + 1,
	}
}

// EvaluateQuorumTolerance answers "does the cluster currently tolerate
// losing any ONE more voter" - CODEX's "a plan cannot remove raft
// quorum," reframed for v1 since no Flight Plan exists yet to name a
// specific plan to check. False if any voter's simulated loss would be
// QuorumLost; Unknown if any is QuorumUnknown (or structurally
// invalid) and none are Lost; True only if every voter's own loss
// still leaves quorum SURVIVES. voters must be exactly the current
// raft membership's Suffrage == "Voter" entries, each with a real,
// already-gathered Reachability - this function makes no further
// observations and issues no RPCs. leaderID identifies the current
// raft leader so each voter's own leader-loss downgrade
// (recovery.ClassifyQuorum's own isCurrentLeader parameter) is
// recomputed correctly per voter, not hoisted out of the loop.
func EvaluateQuorumTolerance(voters []VoterReachability, leaderID string) Evaluation {
	evidence := make([]Evidence, 0, len(voters))
	worst := ResultTrue // Survives < Unknown < Lost in severity; start optimistic, only downgrade
	now := time.Now()

	for _, v := range voters {
		fact := quorumFactFromVoters(voters, v)
		var verdict recovery.QuorumVerdict
		valid := recovery.ValidQuorumFact(fact)
		if valid {
			verdict = recovery.ClassifyQuorum(fact, v.NodeID == leaderID)
		}

		switch {
		case !valid:
			evidence = append(evidence, Evidence{
				Source:     "raft membership + HostStats reachability for " + v.NodeID,
				Detail:     "Quorum arithmetic for losing " + v.NodeID + " was internally inconsistent - treated as unknown, never a fabricated finding.",
				ObservedAt: now,
			})
			if worst == ResultTrue {
				worst = ResultUnknown
			}
		case verdict == recovery.QuorumLost:
			evidence = append(evidence, Evidence{
				Source:     "raft membership + HostStats reachability for " + v.NodeID,
				Detail:     "Losing " + v.NodeID + " would LOSE quorum - even crediting every voter with unknown reachability as reachable, a majority cannot be reached.",
				ObservedAt: now,
			})
			worst = ResultFalse
		case verdict == recovery.QuorumUnknown:
			evidence = append(evidence, Evidence{
				Source:     "raft membership + HostStats reachability for " + v.NodeID,
				Detail:     "Losing " + v.NodeID + " has an UNKNOWN quorum outcome - see internal/recovery.ClassifyQuorum for why (unverified voter reachability, or this voter is the current leader and reachability was only checked from the leader's own vantage point).",
				ObservedAt: now,
			})
			if worst == ResultTrue {
				worst = ResultUnknown
			}
		default: // QuorumSurvives
			evidence = append(evidence, Evidence{
				Source:     "raft membership + HostStats reachability for " + v.NodeID,
				Detail:     "Losing " + v.NodeID + " would leave quorum intact.",
				ObservedAt: now,
			})
		}
	}

	explanation := "The cluster tolerates losing any one more voter."
	switch worst {
	case ResultFalse:
		explanation = "The cluster does NOT tolerate losing at least one current voter - quorum would be lost."
	case ResultUnknown:
		explanation = "Whether the cluster tolerates losing every current voter could not be fully confirmed."
	}

	return Evaluation{
		Name:        "quorum-tolerance",
		Scope:       "cluster",
		Result:      worst,
		Explanation: explanation,
		Evidence:    evidence,
	}
}

// ResourceFact is one VM/jail with a replica configured - unprotected
// resources (no ReplicaNodeID) are out of scope for this invariant,
// not violations of it, so callers should never include them here.
type ResourceFact struct {
	ID, Name, Kind string // Kind: "vm" | "jail"
	ReplicaNodeID  string

	// DestinationCapable is Result, not bool: True only ever applies to
	// a VM whose replica-target node confirmed bhyve_configured; False
	// means that node confirmed it is NOT bhyve-configured; Unknown
	// covers a jail (no capability signal exists for jails at all), a
	// HostStats fetch failure/timeout to the replica node, or any other
	// unconfirmed case.
	DestinationCapable       Result
	DestinationCapableDetail string
}

// EvaluateCellRecoverability never returns ResultTrue: CODEX's own
// definition is conjunctive - "a cell called recoverable has A
// SYNCHRONIZED REPLICA AND a capable destination" - and live HAST sync
// status is never confirmable (the same gap EvaluateHASTDualPrimary
// names). A resource whose destination is confirmed capable is still
// only Unknown overall, never True, because the sync half of the
// conjunction was never actually confirmed - collapsing that to True
// would be exactly the "missing observation treated as a passed safety
// check" CODEX's own text warns against.
func EvaluateCellRecoverability(facts []ResourceFact) []Evaluation {
	now := time.Now()
	evals := make([]Evaluation, 0, len(facts))
	for _, f := range facts {
		eval := Evaluation{Name: "cell-recoverability", Scope: f.ID}
		syncEvidence := Evidence{
			Source:     "internal/hast (no cluster-wide sync-status RPC)",
			Detail:     hastGapDetail,
			ObservedAt: time.Time{}, // never observed - the point of this evidence entry
		}
		destEvidence := Evidence{
			Source:     "HostStats for " + f.ReplicaNodeID,
			Detail:     f.DestinationCapableDetail,
			ObservedAt: now,
		}
		switch {
		case f.DestinationCapable == ResultFalse:
			eval.Result = ResultFalse
			eval.Explanation = f.Name + " (" + f.Kind + ") is not recoverable: its replica target " + f.ReplicaNodeID + " is confirmed incapable of running it."
		default: // ResultTrue or ResultUnknown for DestinationCapable - overall never True
			eval.Result = ResultUnknown
			eval.Explanation = f.Name + " (" + f.Kind + ") cannot be confirmed recoverable: its replica's sync status is never verifiable in v1, regardless of destination capability."
		}
		eval.Evidence = []Evidence{destEvidence, syncEvidence}
		evals = append(evals, eval)
	}
	return evals
}

// BridgeObservation is one node's own reported bridge state for one
// network it hosts a resource on. Status is the raw
// GetLocalNetworkBridgeStatusResponse value ("up"/"down"/"unknown"/"")
// - never derived from ListNetworks's own bridge_status field, which
// is populated by whichever node answers that (leader-only, forwarded)
// RPC and would silently mislabel the LEADER's bridge state as the
// answering network's own (the exact bug ADR-0055 already found and
// fixed once - see GetLocalNetworkBridgeStatus's own doc comment in
// internal/manager/server.go).
type BridgeObservation struct {
	NodeID string
	Status string
	Err    string // non-empty means the fetch itself failed/timed out - never coerced into "down"
}

// NetworkFact is one managed network plus every distinct node's own
// bridge observation for it - empty Observations means nothing is
// attached to this network to check.
type NetworkFact struct {
	ID, Name     string
	Observations []BridgeObservation
}

// EvaluateNetworkRoute folds "valid route" (from Observations: False if
// any node reports the bridge down, Unknown if any observation is
// unread/errored or nothing is attached, else True) together with
// "working DNS path" (always Unknown in v1 - no DNS observability
// exists anywhere in this codebase). Overall Result is False only when
// the route half is itself a confirmed blocker; otherwise it is never
// better than Unknown, since the DNS half never resolves True - stated
// explicitly in Explanation, not hidden.
func EvaluateNetworkRoute(facts []NetworkFact) []Evaluation {
	now := time.Now()
	evals := make([]Evaluation, 0, len(facts))
	for _, f := range facts {
		eval := Evaluation{Name: "network-route-dns", Scope: f.ID}
		routeBlocked := false
		routeUnknown := len(f.Observations) == 0
		var evidence []Evidence
		for _, obs := range f.Observations {
			switch {
			case obs.Err != "":
				routeUnknown = true
				evidence = append(evidence, Evidence{Source: "GetLocalNetworkBridgeStatus on " + obs.NodeID, Detail: "fetch failed: " + obs.Err, ObservedAt: now})
			case obs.Status == "down":
				routeBlocked = true
				evidence = append(evidence, Evidence{Source: "GetLocalNetworkBridgeStatus on " + obs.NodeID, Detail: "bridge reported down", ObservedAt: now})
			case obs.Status == "up":
				evidence = append(evidence, Evidence{Source: "GetLocalNetworkBridgeStatus on " + obs.NodeID, Detail: "bridge reported up", ObservedAt: now})
			default:
				routeUnknown = true
				evidence = append(evidence, Evidence{Source: "GetLocalNetworkBridgeStatus on " + obs.NodeID, Detail: "bridge status unreported/unknown", ObservedAt: now})
			}
		}
		evidence = append(evidence, Evidence{
			Source:     "DNS (no observability)",
			Detail:     "Apiary does not expose a guest DHCP DNS option or resolver result through RPC - DNS path is always unknown in v1.",
			ObservedAt: time.Time{},
		})

		switch {
		case routeBlocked:
			eval.Result = ResultFalse
			eval.Explanation = "Network " + f.Name + " has a confirmed blocked route on at least one node hosting a resource on it."
		default:
			eval.Result = ResultUnknown
			if routeUnknown {
				eval.Explanation = "Network " + f.Name + "'s route could not be fully confirmed, and its DNS path is never verifiable in v1."
			} else {
				eval.Explanation = "Network " + f.Name + "'s observed route is clear, but its DNS path is never verifiable in v1 - never better than unknown overall."
			}
		}
		eval.Evidence = evidence
		evals = append(evals, eval)
	}
	return evals
}
