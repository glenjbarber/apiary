package restartplan

import (
	"fmt"
	"time"

	"github.com/glenjbarber/apiary/internal/cluster"
	"github.com/glenjbarber/apiary/internal/guardrail"
	"github.com/glenjbarber/apiary/internal/invariant"
)

// RuleQuorumSafety and RuleLeaderRestart are the stable rule ids this
// package reports, matching ADR-0125 §4's own strings. They are stable
// ids rather than free text precisely so a caller (the frontend's
// preflight panel, a test) can key copy off them without string-matching
// Detail - the same contract internal/guardrail.Finding.Rule states.
const (
	RuleQuorumSafety  = "raftd-quorum-safety"
	RuleLeaderRestart = "raftd-leader-restart"
)

// VoterReachability is one OTHER raft voter's identity plus the live
// probe result for it. Reachability deliberately reuses
// internal/cluster's own three-state type rather than a bool or a local
// copy of it: a dial that failed ("unreachable") and a voter whose
// status was never obtained at all ("unknown") are different facts, and
// ADR-0052's quorum arithmetic is already careful about the difference.
// This package adds no new reachability vocabulary of its own.
type VoterReachability struct {
	NodeID          string
	RaftBindAddress string

	// Reachability is cluster.ReachabilityReachable or
	// cluster.ReachabilityUnreachable for a voter that was actually
	// dialed. cluster.ReachabilityUnknown means no dial result exists
	// for this voter at all - never a synonym for unreachable, though
	// the quorum arithmetic below counts it as not-up, because "we
	// could not confirm it is up" cannot be credited as survival.
	Reachability cluster.Reachability

	// ProbeError carries the dial's own failure reason for display. It
	// is never itself evidence: a non-empty ProbeError alongside
	// ReachabilityReachable is a caller bug this package will not
	// paper over (see EvaluateQuorumSafety).
	ProbeError string
}

// QuorumFact is the complete, already-gathered input to
// EvaluateQuorumSafety. Every field is populated by the caller (the
// leader-side RPC handler in production) from facts it read itself;
// this function makes no RPC, no raft read and no dial of its own,
// which is what makes it exhaustively unit-testable - the property
// ADR-0103 established for internal/guardrail, and the reason this
// logic is not written inline inside internal/manager.
type QuorumFact struct {
	// Service is the rc.d service being restarted ("apiary_raftd" for
	// ADR-0125, "apiary_managerd" for ADR-0103's own path). Only used
	// to label the resulting Report's Intent, so a preflight response
	// names the service it actually judged.
	Service string

	// TargetNodeID is the node whose raftd is about to be restarted.
	TargetNodeID string

	// IsTargetVoter and IsTargetLeader come from the same raft status
	// read that produced OtherVoters - and ProbeReadOK is the honest
	// report of whether that read (and the probe built on it) actually
	// happened. If it did not, both of these are unknowable, which is
	// why ProbeReadOK is checked first below.
	IsTargetVoter  bool
	IsTargetLeader bool

	// OtherVoters is every voter except the target, each with its own
	// live probe result. A nil/empty slice is legitimate: a
	// single-voter cluster has no other voters, and the arithmetic
	// below handles that case explicitly rather than treating it as a
	// malformed fact.
	OtherVoters []VoterReachability

	// ProbeReadOK is false when the probe itself could not run - raft
	// status unreadable, context cancelled before any dial completed.
	// The verdict is then Unknown (never Allow), per ADR-0125 §8's
	// fail-closed rule and internal/guardrail's own existing ReadOK
	// handling.
	ProbeReadOK bool
	ProbeError  string

	// Force is ADR-0103/0125's operator-acknowledgment override. It
	// downgrades a positive Block (the arithmetic says the cluster
	// would lose quorum; this node is the leader) to Allow with the
	// reason preserved as a cited caveat - but it deliberately does
	// NOT rescue an Unknown: you cannot acknowledge your way past a
	// fact nobody established. The real enforcement remains the
	// raft-replicated lease FSM's own apply, which this preflight can
	// never outrank.
	Force bool

	// Existing carries ADR-0103's own concurrent-restart / cooldown
	// report for the same service, already computed by the caller, so
	// one Report can carry every reason a restart is or is not safe.
	// Its rules are distinct from the two above, which is exactly how
	// a caller (or the frontend) tells which check produced which
	// reason. nil means "no other check was run".
	Existing *guardrail.Report

	// ObservedAt stamps the evaluation's cited evidence. A zero value
	// means "unset", in which case evidence carries no timestamp rather
	// than a fabricated one - an invented observation time is exactly
	// the kind of quiet lie this package exists to avoid.
	ObservedAt time.Time
}

// EvaluateQuorumSafety is the single source of truth for "is it safe to
// restart this node's raftd right now", implementing ADR-0125 §4's
// logic over facts the caller has already gathered. Both the advisory
// preflight and the real pre-restart check call this same function, so
// the preview and the enforcement cannot drift apart.
//
// The quorum arithmetic is NOT reimplemented here: it delegates to
// internal/cluster.ComputeQuorumImpact, the function ADR-0052 built and
// ADR-0125 §4 says this logic "mirrors ... exactly". Delegating rather
// than copying means the two answers cannot diverge, and means
// ComputeQuorumImpact's careful handling of unknown-reachability voters,
// of a non-voter target, and of a single-voter cluster all apply here
// unchanged.
//
// Precedence, in ADR-0125 §4's own order:
//
//  1. !ProbeReadOK -> Unknown. Not overridable by Force. Checked first,
//     and not merely because the ADR lists it first: without a
//     successful read we cannot even establish that the target is a
//     non-voter, so "a non-voter restart carries no quorum risk" is
//     not a conclusion this call is entitled to reach.
//  2. !IsTargetVoter -> Allow (no quorum risk at all).
//  3. Quorum arithmetic -> Block when the confirmed-reachable remaining
//     voters would not form a majority.
//  4. IsTargetLeader -> an additional Block-by-default finding, since
//     restarting the leader forces a fresh election.
//
// A Block from (3) or (4) is downgradable by Force to Allow with the
// reason preserved as a cited caveat; an Unknown from (1) never is. A
// Block already present in Existing is likewise never softened here.
func EvaluateQuorumSafety(fact QuorumFact) guardrail.Report {
	service := fact.Service
	if service == "" {
		service = DefaultService
	}
	report := guardrail.Report{Intent: "restart-" + service, Verdict: guardrail.Allow}
	if fact.Existing != nil {
		report.Verdict = fact.Existing.Verdict
		report.Findings = append(report.Findings, fact.Existing.Findings...)
		report.Caveats = append(report.Caveats, fact.Existing.Caveats...)
	}

	if !fact.ProbeReadOK {
		// Fail closed, and do not touch the verdict an existing
		// check already reached - Unknown is at least as restrictive
		// as Allow, and a caller merging two reports needs to see
		// the worse of the two, which is what Unknown is.
		if report.Verdict == guardrail.Allow {
			report.Verdict = guardrail.Unknown
		}
		detail := fact.ProbeError
		if detail == "" {
			detail = "the probe could not run"
		}
		report.Findings = append(report.Findings, guardrail.Finding{
			Rule: RuleQuorumSafety,
			Detail: "could not read the raft membership and peer reachability needed to judge this restart (" + detail +
				") - this is 'could not check', not 'checked and safe'; refusing rather than assuming quorum survives",
		})
		return report
	}

	if !fact.IsTargetVoter {
		// Nothing to weigh: a non-voting member's restart cannot
		// change what quorum remains. Recorded as a caveat so an
		// operator can see the question was asked, not skipped.
		report.Caveats = append(report.Caveats, invariant.Evidence{
			Source:     RuleQuorumSafety,
			Detail:     fact.TargetNodeID + " is a non-voting raft member - restarting it cannot cost the cluster quorum",
			ObservedAt: fact.ObservedAt,
		})
		return report
	}

	impact := ComputeQuorum(fact)
	switch {
	case !impact.Survives:
		report.Verdict = guardrail.Block
		report.Findings = append(report.Findings, guardrail.Finding{
			Rule:   RuleQuorumSafety,
			Detail: "restarting " + fact.TargetNodeID + " would cost this cluster its quorum: " + impact.Note,
			Evidence: []invariant.Evidence{{
				Source:     RuleQuorumSafety,
				Detail:     impact.Arithmetic(),
				ObservedAt: fact.ObservedAt,
			}},
		})
	case impact.RemainingUnknown > 0:
		report.Caveats = append(report.Caveats, invariant.Evidence{
			Source:     RuleQuorumSafety,
			Detail:     impact.Note,
			ObservedAt: fact.ObservedAt,
		})
	}

	if fact.IsTargetLeader {
		report.Verdict = guardrail.Block
		report.Findings = append(report.Findings, guardrail.Finding{
			Rule: RuleLeaderRestart,
			Detail: fact.TargetNodeID + " is the current raft leader - restarting it will trigger a leader election, " +
				"costing a short window of write availability; restart followers first wherever the choice exists",
			Evidence: []invariant.Evidence{{
				Source:     RuleLeaderRestart,
				Detail:     fmt.Sprintf("%s is the leader of a %d-voter cluster right now", fact.TargetNodeID, impact.TotalVoters),
				ObservedAt: fact.ObservedAt,
			}},
		})
	}

	if fact.Force {
		return applyForce(report, fact.ObservedAt)
	}
	return report
}

// applyForce is ADR-0103/0125's Force semantics, made explicit here
// because "what does an override actually override" is exactly the kind
// of thing that must not be left to be rediscovered per call site: it
// downgrades the Block findings THIS evaluation produced into cited
// caveats, leaves any pre-existing foreign finding alone, and never
// touches an Unknown.
//
// The verdict is only moved to Allow when one of this package's own
// findings was actually downgraded. Without that condition, a Block
// contributed by a caller-supplied Existing report (ADR-0103's
// concurrent-restart or cooldown check, say) would be silently cleared
// by an override that was only ever meant to acknowledge an election or
// a probe snapshot - turning a real, still-present block into a false
// "allow" with no finding left to explain it.
func applyForce(report guardrail.Report, observedAt time.Time) guardrail.Report {
	if report.Verdict != guardrail.Block {
		return report
	}
	kept := make([]guardrail.Finding, 0, len(report.Findings))
	downgraded := false
	for _, f := range report.Findings {
		if f.Rule != RuleQuorumSafety && f.Rule != RuleLeaderRestart {
			kept = append(kept, f)
			continue
		}
		downgraded = true
		report.Caveats = append(report.Caveats, invariant.Evidence{
			Source:     f.Rule,
			Detail:     "operator acknowledged this and restarted anyway: " + f.Detail,
			ObservedAt: observedAt,
		})
	}
	if !downgraded {
		// The Block came entirely from findings this override does not
		// cover. Put the caveats back the way they were and leave the
		// verdict alone.
		return report
	}
	report.Findings = kept
	report.Verdict = guardrail.Allow
	return report
}
