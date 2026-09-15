// Package guardrail implements ADR-0103's action-preflight guardrails: a
// deliberately small set of pure functions that turn already-gathered
// facts about a proposed action into an allow/block verdict with cited
// evidence, mirroring internal/invariant and internal/whynot's own
// convention of pure, no-I/O evaluation over caller-supplied facts.
//
// This is not a generic per-RPC preflight system - it covers exactly two
// concrete guardrails named in the roadmap and grounded in the real
// 2026-09-11 stranded-cluster incident: join-approval reachability
// (EvaluateJoinReachability, generalizing ADR-0097's own check into a
// previewable, single source of truth) and a concurrent-manager-restart
// guardrail (EvaluateConcurrentManagerRestart). Every other mutating RPC
// in this codebase is unaffected.
package guardrail

import "github.com/glenjbarber/apiary/internal/invariant"

// Verdict is the outcome of evaluating one guardrail. Unknown is
// deliberately treated the same as Block by every caller in this
// codebase (see EvaluateConcurrentManagerRestart's own doc comment) -
// an evaluation that couldn't determine the real state must never be
// silently treated as safe.
type Verdict string

const (
	Allow   Verdict = "allow"
	Block   Verdict = "block"
	Unknown Verdict = "unknown"
)

// Finding is one cited reason behind a Report's Verdict.
type Finding struct {
	// Rule is a stable id, e.g. "join-reachability",
	// "concurrent-manager-restart" - never free text, so callers can
	// key UI copy or tests off it without string-matching Detail.
	Rule     string
	Detail   string
	Evidence []invariant.Evidence
}

// Report is one guardrail evaluation's full result.
type Report struct {
	Intent   string
	Verdict  Verdict
	Findings []Finding
	Caveats  []invariant.Evidence
}

// JoinReachabilityFact is the input to EvaluateJoinReachability.
type JoinReachabilityFact struct {
	Address   string
	Reachable bool
	DialError string
}

// EvaluateJoinReachability is the single source of truth for whether a
// pending join request's claimed raft_bind_address is safe to approve -
// both ApproveJoinRequest's own real enforcement and
// PreflightApproveJoinRequest's preview call this same function, so the
// two can never silently drift apart (ADR-0097/ADR-0103).
//
// There is no Unknown case for this rule in practice: a dial that
// couldn't even complete (timeout, connection refused, DNS failure) is
// exactly as disqualifying as a dial that completed and found nothing
// listening - both mean "refuse to approve," so both map to Block.
func EvaluateJoinReachability(fact JoinReachabilityFact) Report {
	if fact.Reachable {
		return Report{Intent: "approve-join-request", Verdict: Allow}
	}
	detail := fact.DialError
	if detail == "" {
		detail = "not reachable"
	}
	return Report{
		Intent:  "approve-join-request",
		Verdict: Block,
		Findings: []Finding{{
			Rule:   "join-reachability",
			Detail: "raft_bind_address " + fact.Address + " is not reachable: " + detail + " - approving an unreachable node strands the cluster and can only be recovered by wiping raft state",
		}},
	}
}

// LeaseInfo describes a currently-held, unconfirmed restart lease.
type LeaseInfo struct {
	HolderNodeID    string
	RequestedAtUnix int64
}

// RestartInfo describes the most recent confirmed-complete restart of a
// service by a currently-known Raft voter.
type RestartInfo struct {
	NodeID          string
	CompletedAtUnix int64
}

// RestartCooldownFact is the input to EvaluateConcurrentManagerRestart -
// every field here is already voter-filtered and raft-read-derived by
// the caller (PreflightRestartNodeService) before this function ever
// sees it; this function makes no RPC or raft calls of its own.
type RestartCooldownFact struct {
	TargetService    string
	IsLocalNodeVoter bool
	ActiveLease      *LeaseInfo
	RecentRestart    *RestartInfo
	ReadOK           bool
}

// EvaluateConcurrentManagerRestart is advisory/preview-only, used by
// PreflightRestartNodeService - the actual atomic enforcement happens
// inside internal/raft.FSM's own applyAcquireRestartLease, which is the
// real, authoritative decision point and cannot drift from this preview
// in any way that matters for safety (a race between preview and
// submission is expected and fine, since the FSM apply, not this
// function, decides).
//
// !IsLocalNodeVoter always returns Allow immediately: a non-voter's own
// manager restart carries no quorum risk. !ReadOK (the raft-internal
// read itself failed) returns Unknown, never Allow - a guardrail that
// couldn't determine the real state must fail closed, the same
// principle ADR-0081 established for network teardown status.
func EvaluateConcurrentManagerRestart(fact RestartCooldownFact) Report {
	intent := "restart-" + fact.TargetService
	if !fact.IsLocalNodeVoter {
		return Report{Intent: intent, Verdict: Allow}
	}
	if !fact.ReadOK {
		return Report{
			Intent:  intent,
			Verdict: Unknown,
			Findings: []Finding{{
				Rule:   "concurrent-manager-restart",
				Detail: "could not read cluster restart-lease state for " + fact.TargetService + " - refusing rather than assuming it is safe",
			}},
		}
	}
	if fact.ActiveLease != nil {
		return Report{
			Intent:  intent,
			Verdict: Block,
			Findings: []Finding{{
				Rule:   "concurrent-manager-restart",
				Detail: fact.TargetService + " has an unconfirmed restart lease held by " + fact.ActiveLease.HolderNodeID + " - this block persists until that node confirms itself healthy or an operator overrides it",
			}},
		}
	}
	if fact.RecentRestart != nil {
		return Report{
			Intent:  intent,
			Verdict: Block,
			Findings: []Finding{{
				Rule:   "concurrent-manager-restart",
				Detail: fact.TargetService + " was recently restarted by voter " + fact.RecentRestart.NodeID + " - still inside the cooldown window",
			}},
		}
	}
	return Report{Intent: intent, Verdict: Allow}
}
