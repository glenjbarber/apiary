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
	"strings"
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

// EvaluateHASTDualPrimary lives in hast.go: it resolves from the
// per-node HAST role observations ADR-0119's
// GetLocalHASTResourceStatus makes reachable, and reports Unknown -
// never a pass - for either configured end that stayed silent.

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

// VoterQuorumImpact is one voter's own hypothetical-loss verdict,
// exposed as structured data rather than folded into Evidence prose -
// so a caller (Resilience Coverage Map, ADR-0062) can classify a
// per-node scenario without parsing Evidence.Detail strings, the same
// fix ADR-0061 finding 3 applied to NetworkFact/EvaluateNetworkRoute.
// Valid false means the underlying QuorumFact was internally
// inconsistent - Verdict is meaningless in that case, never a
// fabricated finding (mirrors ValidQuorumFact's own guard).
type VoterQuorumImpact struct {
	NodeID  string
	Verdict recovery.QuorumVerdict
	Valid   bool
}

// ClassifyVoterQuorumImpacts computes every voter's own hypothetical-
// loss verdict from one shared reachability snapshot - the same
// per-voter loop EvaluateQuorumTolerance uses internally, extracted so
// a caller needing the per-node detail (not just the aggregated worst-
// case Evaluation) doesn't have to re-derive it or parse prose.
//
// vantage is the vantage point the shared snapshot was actually
// gathered from, passed through to recovery.ClassifyQuorumFromVantage.
// A caller that dialled each voter itself must say so
// (recovery.VantageFromNonLeader); only a caller whose data came from
// the current leader (recovery.VantageFromLeader) is entitled to the
// leader-loss downgrade for the leader's own verdict.
func ClassifyVoterQuorumImpacts(voters []VoterReachability, leaderID string, vantage recovery.QuorumVantage) []VoterQuorumImpact {
	impacts := make([]VoterQuorumImpact, 0, len(voters))
	for _, v := range voters {
		fact := quorumFactFromVoters(voters, v)
		valid := recovery.ValidQuorumFact(fact)
		var verdict recovery.QuorumVerdict
		if valid {
			verdict = recovery.ClassifyQuorumFromVantage(fact, v.NodeID == leaderID, vantage)
		}
		impacts = append(impacts, VoterQuorumImpact{NodeID: v.NodeID, Verdict: verdict, Valid: valid})
	}
	return impacts
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
// (recovery.ClassifyQuorumFromVantage's targetIsLeader) is
// recomputed correctly per voter, not hoisted out of the loop.
//
// vantage is the vantage point the caller actually gathered those
// reachabilities from, and it is load-bearing rather than decorative:
// the leader-loss downgrade is only applied to data the leader itself
// produced. A caller that dialled each voter itself (internal/
// frontend's bounded HostStats fan-out) passes
// recovery.VantageFromNonLeader, and then a healthy multi-voter colony
// whose only gap is "losing the leader" resolves to a real verdict
// instead of being downgraded to unknown on a premise that was never
// true for that caller's data.
func EvaluateQuorumTolerance(voters []VoterReachability, leaderID string, vantage recovery.QuorumVantage) Evaluation {
	impacts := ClassifyVoterQuorumImpacts(voters, leaderID, vantage)
	evidence := make([]Evidence, 0, len(impacts))
	worst := ResultTrue // Survives < Unknown < Lost in severity; start optimistic, only downgrade
	now := time.Now()

	// The leader-vantage clause is only ever true when the caller's
	// data actually came from the leader. Citing it unconditionally
	// told the reader that losing the leader is unknown "because
	// reachability was only checked from the leader's own vantage
	// point" on pages where no such vantage existed - an explanation
	// for a data property the evidence does not have.
	unknownWhy := "unverified voter reachability"
	if vantage == recovery.VantageFromLeader {
		unknownWhy += ", or this voter is the current leader and reachability was only checked from the leader's own vantage point"
	}

	for _, impact := range impacts {
		switch {
		case !impact.Valid:
			evidence = append(evidence, Evidence{
				Source:     "raft membership + HostStats reachability for " + impact.NodeID,
				Detail:     "Quorum arithmetic for losing " + impact.NodeID + " was internally inconsistent - treated as unknown, never a fabricated finding.",
				ObservedAt: now,
			})
			if worst == ResultTrue {
				worst = ResultUnknown
			}
		case impact.Verdict == recovery.QuorumLost:
			evidence = append(evidence, Evidence{
				Source:     "raft membership + HostStats reachability for " + impact.NodeID,
				Detail:     "Losing " + impact.NodeID + " would LOSE quorum - even crediting every voter with unknown reachability as reachable, a majority cannot be reached.",
				ObservedAt: now,
			})
			worst = ResultFalse
		case impact.Verdict == recovery.QuorumUnknown:
			evidence = append(evidence, Evidence{
				Source:     "raft membership + HostStats reachability for " + impact.NodeID,
				Detail:     "Losing " + impact.NodeID + " has an UNKNOWN quorum outcome - see internal/recovery.ClassifyQuorum for why (" + unknownWhy + ").",
				ObservedAt: now,
			})
			if worst == ResultTrue {
				worst = ResultUnknown
			}
		default: // QuorumSurvives
			evidence = append(evidence, Evidence{
				Source:     "raft membership + HostStats reachability for " + impact.NodeID,
				Detail:     "Losing " + impact.NodeID + " would leave quorum intact.",
				ObservedAt: now,
			})
		}
	}

	explanation := "The cluster tolerates losing any one more voter."
	switch worst {
	case ResultFalse:
		if len(voters) == 1 {
			explanation = "This is a single-node deployment: losing its one voter ends quorum entirely, since there is no other voter to fall back on. This is an expected property of running one node, not a misconfiguration - see ADR-0091."
		} else {
			explanation = "The cluster does NOT tolerate losing at least one current voter - quorum would be lost."
		}
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

	// ReplicaSync is the configured replica node's OWN live HAST
	// observation for this resource - the sync half of CODEX's
	// conjunction, which ADR-0060 could never confirm because no
	// observation of another node's HAST state was reachable. ADR-0119
	// added it (GetLocalHASTResourceStatus) and ADR-0121 already
	// consumed the identical fact. The zero value is honest, not
	// optimistic: Attempted false means no query was made, which is
	// silence and can never satisfy the conjunction.
	ReplicaSync HASTObservation
}

// EvaluateCellRecoverability answers CODEX's own conjunctive
// definition - "a cell called recoverable has A SYNCHRONIZED REPLICA
// AND a capable destination" - from both halves now that the first is
// observable.
//
// True requires BOTH: the replica node's own hastd reported a real
// role with status "complete" (readHAST's hastInSync, ADR-0121's
// replica_in_sync), and the destination was separately confirmed
// capable. False only for a confirmed-incapable destination, as
// before. Everything else - a replica that could not be read, a role
// this build does not recognize, an absent or "unknown" status, a
// jail (no capability signal exists for jails anywhere) - is Unknown.
// Collapsing any of those into True is exactly the "missing
// observation treated as a passed safety check" CODEX's own text warns
// against, and is why the conjunction is evaluated as a conjunction
// rather than either half alone.
//
// No explanation here claims the Cell will recover: a sync
// observation is a point-in-time fact about the replica's hastd
// worker, and says nothing about whether that node stays reachable or
// whether the Cell can be recreated from the replica (ADR-0121's own
// standing constraint).
func EvaluateCellRecoverability(facts []ResourceFact) []Evaluation {
	now := time.Now()
	evals := make([]Evaluation, 0, len(facts))
	for _, f := range facts {
		eval := Evaluation{Name: "cell-recoverability", Scope: f.ID}
		syncEvidence := replicaSyncEvidence(f, now)
		destEvidence := Evidence{
			Source:     "HostStats for " + f.ReplicaNodeID,
			Detail:     f.DestinationCapableDetail,
			ObservedAt: now,
		}
		syncVerdict := readHAST(f.ReplicaSync)
		switch {
		case f.DestinationCapable == ResultFalse:
			eval.Result = ResultFalse
			eval.Explanation = f.Name + " (" + f.Kind + ") is not recoverable: its replica target " + f.ReplicaNodeID + " is confirmed incapable of running it."
		case f.DestinationCapable == ResultTrue && syncVerdict == hastInSync:
			eval.Result = ResultTrue
			eval.Explanation = f.Name + " (" + f.Kind + ") has a synchronized HAST replica on " + f.ReplicaNodeID + " (its own hastd reported " + roleSummary(f.ReplicaSync) + " with status " + quoted(strings.TrimSpace(f.ReplicaSync.Status)) + ", and that node is separately confirmed capable) - both halves of the conjunction were actually confirmed. This is a point-in-time observation of one node's hastd, not a guarantee: it does not prove the replica stays reachable, that the owning disk survives, or that the Cell can be recreated from the replica."
		case f.DestinationCapable == ResultTrue && syncVerdict == hastOutOfSync:
			eval.Result = ResultUnknown
			eval.Explanation = f.Name + " (" + f.Kind + ") has a capable replica target, but its replica on " + f.ReplicaNodeID + " is confirmed NOT usable as-is - treat it as having no working redundancy until an operator resolves it."
		default:
			eval.Result = ResultUnknown
			eval.Explanation = f.Name + " (" + f.Kind + ") cannot be confirmed recoverable: at least one half of the conjunction is unconfirmed - " + unverifiedHalves(f, syncVerdict) + "."
		}
		eval.Evidence = []Evidence{destEvidence, syncEvidence}
		evals = append(evals, eval)
	}
	return evals
}

// unverifiedHalves names, in the caller's words, which half of the
// conjunction is missing - so an Unknown never reads as though both
// halves were checked and one merely came out badly.
func unverifiedHalves(f ResourceFact, syncVerdict hastVerdict) string {
	missing := make([]string, 0, 2)
	if f.DestinationCapable != ResultTrue {
		missing = append(missing, "destination capability on "+f.ReplicaNodeID+" is unconfirmed ("+f.DestinationCapableDetail+")")
	}
	if syncVerdict != hastInSync {
		switch {
		case !f.ReplicaSync.Attempted:
			missing = append(missing, "no live HAST status was queried for the replica on "+f.ReplicaNodeID)
		case !f.ReplicaSync.Observed:
			missing = append(missing, "the replica on "+f.ReplicaNodeID+" did not answer: "+nonEmpty(f.ReplicaSync.Detail, "no reason reported"))
		default:
			missing = append(missing, "the replica on "+f.ReplicaNodeID+" gave no usable synchronization statement ("+hastVerdictReason(f.ReplicaSync, syncVerdict)+")")
		}
	}
	return strings.Join(missing, "; ")
}

// hastVerdictReason renders why a readable observation did not read as
// in-sync, in the same "only a positive statement is badness" language
// ADR-0121 uses.
func hastVerdictReason(o HASTObservation, v hastVerdict) string {
	switch v {
	case hastOutOfSync:
		if strings.EqualFold(strings.TrimSpace(o.Role), hastRoleInit) {
			return "hastd reports role \"init\", so the resource was never initialized there"
		}
		return "hastd reports role " + roleSummary(o) + " with status " + quoted(strings.TrimSpace(o.Status))
	default:
		return "status " + quoted(strings.TrimSpace(o.Status)) + " or role " + roleSummary(o) + " is not a statement this build can act on"
	}
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
