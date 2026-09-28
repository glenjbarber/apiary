// Package guardrail implements ADR-0103's action-preflight guardrails: a
// deliberately small set of pure functions that turn already-gathered
// facts about a proposed action into an allow/block verdict with cited
// evidence, mirroring internal/invariant and internal/whynot's own
// convention of pure, no-I/O evaluation over caller-supplied facts.
//
// This is not a generic per-RPC preflight system - it covers exactly three
// concrete guardrails named in the roadmap and grounded in real incidents:
// join-approval reachability (EvaluateJoinReachability, generalizing
// ADR-0097's own check into a previewable, single source of truth), a
// concurrent-manager-restart guardrail (EvaluateConcurrentManagerRestart),
// and the joining Comb's own raft log state at the moment of approval
// (EvaluateJoinLogState), which is the only check here whose dangerous
// input is the ABSENCE of evidence. Every other mutating RPC in this
// codebase is unaffected.
package guardrail

import (
	"fmt"

	"github.com/glenjbarber/apiary/internal/invariant"
)

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

// JoinerLogStateFact is the input to EvaluateJoinLogState - the joining
// Comb's own evidence about its local raft log, already gathered by the
// caller. This function makes no RPC and reads no disk, exactly like every
// other guardrail here.
type JoinerLogStateFact struct {
	// NodeID is the joining Comb, for the message only.
	NodeID string
	// Observed is the presence flag. False means nobody reported a value,
	// which is NOT the same claim as "somebody reported zero" - an older
	// managerd, a hand-seeded record, and a read that failed all land
	// here, and all three are refused.
	Observed bool
	// LastLogIndex is raftd's own last_log_index: the index of the last
	// entry this node ever appended to its local log. Zero on a Comb that
	// has never been in a cluster; greater than zero on one that has,
	// including one that was a member and has since been removed.
	LastLogIndex uint64
	// ReadError is the reason the read failed, when it did. It is what
	// makes an Unknown verdict actionable rather than merely negative.
	ReadError string
}

// EvaluateJoinLogState decides whether a joiner whose local raft log may
// already hold committed history is safe to make a voter.
//
// This is the one guardrail in this package where the dangerous input is
// the absence of evidence rather than the presence of it. AddVoter on a
// node that already has a non-empty log is the only path in this codebase
// that can lose committed Colony history: raft cannot reconcile two
// divergent committed logs, it has no procedure for it, and the recovery
// this codebase actually offers is a full state wipe of the affected node.
// ADR-0097's reachability check exists because approving an unreachable
// node has stranded this cluster before; this exists because approving a
// node carrying a foreign log can do worse, and it fails just as quietly.
//
// last_log_index is the right signal, and it is the only one available
// without a second mechanism. The approver cannot read the joiner's disk:
// raftd holds bbolt's exclusive lock on raft.db for as long as it is
// running, which on an -await-join node is exactly when a join request is
// being made, so a second open of that file from managerd would block
// rather than answer. raftd already reports last_log_index in its own
// StatusResponse, and it is authoritative there for the same reason
// hadState is authoritative at startup.
//
// Zero means empty, and the implication is worth stating because it is the
// whole basis of an Allow: a node with no log entries has no committed
// entries, and cannot have snapshots either, since a snapshot is only ever
// taken from log entries this node held. So last_log_index == 0 rules out
// every form of prior membership that AddVoter could conflict with.
//
// What this does NOT do: it does not prove the joiner's log agrees with
// the Colony's, only that there is nothing there to disagree. And the
// evidence is a snapshot taken when the request was created, not a lock
// held across the approval - see ApproveJoinRequest's own comment for the
// race that leaves, which is why the read happens as late as it does.
//
// Unknown, never Allow, on absent evidence: the same fail-closed rule
// EvaluateConcurrentManagerRestart already follows, and for the same
// reason. An older managerd that does not send this field at all must be
// refused, because "this build never heard of the check" is not a reason
// to perform the one irreversible action the check exists to prevent.
func EvaluateJoinLogState(fact JoinerLogStateFact) Report {
	intent := "approve-join-request"
	if !fact.Observed {
		detail := "no evidence of the joining Comb's raft log state was supplied"
		if fact.ReadError != "" {
			detail = "the joining Comb's raft log state could not be read: " + fact.ReadError
		}
		return Report{
			Intent:  intent,
			Verdict: Unknown,
			Findings: []Finding{{
				Rule: "joiner-log-state",
				Detail: detail + " - refusing rather than assuming the log is empty, because making a voter of a " +
					"node carrying a foreign committed log is the one way this Colony can lose committed history, " +
					"and the only recovery is a full state wipe of that node",
			}},
		}
	}
	if fact.LastLogIndex > 0 {
		return Report{
			Intent:  intent,
			Verdict: Block,
			Findings: []Finding{{
				Rule: "joiner-log-state",
				Detail: fmt.Sprintf("the joining Comb reports raft log index %d, so its local log is not empty - it "+
					"was previously part of a cluster, or bootstrapped on its own. Approving it would ask raft to "+
					"reconcile that log against the Colony's committed one, which raft cannot do. Wipe this Comb's "+
					"raft state (%s) and let it join fresh, or use ConvertStandaloneToJoiner if its log is a clean "+
					"prefix of the Colony's", fact.LastLogIndex, raftDataDirHint),
			}},
		}
	}
	return Report{Intent: intent, Verdict: Allow}
}

// raftDataDirHint is the operator-facing pointer in a join-log-state Block
// finding. It is the raftd.json default rather than this node's configured
// value because the block is about a DIFFERENT Comb, whose configured path
// this node has no way to know.
const raftDataDirHint = "the data_dir from raftd.json on that Comb"

// Combine folds several guardrail evaluations of the SAME action into one
// Report, so a preview that checks more than one thing can still answer a
// single question: is this action safe to take right now.
//
// Severity is Block over Unknown over Allow, in that order. Block outranks
// Unknown because a known disqualification is a more useful thing to tell
// an operator than an absent measurement, and both outrank Allow, so the
// result is Allow only when every input was Allow. Findings from every
// input are kept rather than only the winning one's, so an operator sees
// everything that is wrong, not just the first thing that was checked. They
// are ordered blocking-first rather than input-first, so that a UI which
// shows only the first finding - which is exactly what the join preflight
// page does - shows the thing that actually stopped the action rather than
// whichever check happened to be written first.
//
// The intent is taken from the first non-empty one, which is how every
// caller here uses it: one action, several independent checks.
func Combine(reports ...Report) Report {
	bySeverity := map[Verdict][]Finding{}
	out := Report{Verdict: Allow}
	for _, r := range reports {
		if out.Intent == "" {
			out.Intent = r.Intent
		}
		out.Caveats = append(out.Caveats, r.Caveats...)
		bySeverity[r.Verdict] = append(bySeverity[r.Verdict], r.Findings...)
		switch r.Verdict {
		case Block:
			out.Verdict = Block
		case Unknown:
			if out.Verdict != Block {
				out.Verdict = Unknown
			}
		}
	}
	for _, v := range []Verdict{Block, Unknown, Allow} {
		out.Findings = append(out.Findings, bySeverity[v]...)
	}
	return out
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
