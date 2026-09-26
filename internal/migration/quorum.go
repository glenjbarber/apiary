package migration

import (
	"fmt"

	"github.com/glenjbarber/apiary/internal/recovery"
)

// Rule ids for the quorum gate's cited evidence, in the style of
// internal/guardrail.Finding.Rule and internal/restartplan.RuleQuorumSafety:
// a stable string a caller can key UI copy or a test off, never free
// text, so nobody has to string-match Detail to find out which check
// produced a verdict.
const (
	RuleQuorumReadable = "migration-quorum-readable"
	RuleQuorumGate     = "migration-quorum-gate"
	RuleQuorumStall    = "migration-quorum-stall"
)

// QuorumFact is the complete, already-gathered input to GateOnQuorum.
// Every field is populated by the caller from its own raft status read
// and its own reachability probes; this package makes no raft read and
// no dial of its own, which is what makes the gate exhaustively
// testable on macOS with no cluster.
//
// The shape deliberately mirrors recovery.QuorumFact rather than
// re-deriving quorum arithmetic. The arithmetic is recovery's single
// implementation in this codebase (ADR-0052's, classified by ADR-0125's
// ClassifyQuorum) and duplicating it here would create a second opinion
// about whether a cluster has quorum - precisely the class of bug this
// codebase has twice refused to write.
type QuorumFact struct {
	// IsLeader records whether the node evaluating this is the current
	// raft leader. It is passed through to ClassifyQuorum, whose
	// leader-loss downgrade is load-bearing: reachability gathered via
	// a leader proves the leader can reach each voter, and nothing
	// about whether those voters can reach each other, which is the
	// actual precondition for the next election.
	IsLeader bool

	// Fact is the raw quorum arithmetic input, passed verbatim to
	// recovery.ValidQuorumFact and recovery.ClassifyQuorum.
	Fact recovery.QuorumFact

	// ReadOK is false when the raft status read itself failed, or was
	// cancelled before it completed. This is the "we never got to
	// check" case, and it is checked before anything else - not because
	// it is first on a list, but because without a successful read we
	// cannot even establish the voter set, so no conclusion about
	// quorum is entitled to be drawn at all.
	ReadOK    bool
	ReadError string

	// ObservedAt stamps the cited evidence. A zero value is left
	// visible as zero rather than replaced with "now": an invented
	// observation time on a quorum gate is the same quiet lie an
	// invented verification time would be.
	ObservedAt int64

	// Error is the read failure reason, carried for display when
	// ReadOK is false.
	Error string
}

// QuorumDecision is the gate's whole output: whether the caller may
// proceed, and the evidence to write into the record if it may not.
//
// The three-state shape is not decoration. Proceed=true only ever
// happens on a genuine survives. A lost or unknown quorum halts, and
// the record says so with OutcomeUnverified - never observed_complete,
// never failed. This is the requirement that "lost or unknown quorum
// is represented as UNKNOWN, never as success and never as failure",
// and it is why the gate cannot simply return a bool.
type QuorumDecision struct {
	// Proceed is true only when quorum is established.
	Proceed bool

	// Verdict is recovery.QuorumVerdict's own three-state answer.
	Verdict recovery.QuorumVerdict

	// Detail is the operator-facing reason, always non-empty when
	// Proceed is false.
	Detail string

	// Evidence cites what the verdict was based on: the arithmetic when
	// there was a valid fact, the read failure when there was not.
	Evidence []Evidence

	// Outcome is the outcome a record must settle to if the caller
	// stops here because of this gate. It is OutcomeUnverified for
	// every blocking case - lost, unknown, and unreadable alike -
	// because in all three the honest statement is "we could not find
	// out", and in none of them do we have evidence that the migration
	// itself broke.
	Outcome Outcome
}

// GateOnQuorum is the single source of truth for "may this migration
// start, or take its next step, right now".
//
// Precedence:
//
//  1. !ReadOK -> halt, verdict unknown, OutcomeUnverified. Not
//     overridable by anything in this package: you cannot acknowledge
//     your way past a fact nobody established. This matches
//     internal/guardrail's own ReadOK handling and ADR-0125 §8's
//     fail-closed rule.
//  2. !recovery.ValidQuorumFact -> halt, verdict unknown,
//     OutcomeUnverified. A structurally impossible fact means the
//     upstream read failed silently (recovery's own doc comment names
//     QuorumSize == 0 as the concrete real trigger), and classifying
//     it as a genuine QuorumLost would report a fabricated outage.
//  3. recovery.ClassifyQuorum -> survives proceeds; lost and unknown
//     both halt, with different details because they mean different
//     things to an operator and must not be rendered identically.
//
// The asymmetry between lost and unknown is inherited from
// recovery.ClassifyQuorum and is not flattened here: lost is a
// majority that cannot be reached even crediting every unknown voter,
// unknown is a deficit that could still close. Both halt, because both
// are "do not cut a guest over on this", and both are recorded as
// UNKNOWN rather than as a failure, because neither is evidence about
// the migration.
func GateOnQuorum(f QuorumFact) QuorumDecision {
	if !f.ReadOK {
		detail := f.ReadError
		if detail == "" {
			detail = f.Error
		}
		if detail == "" {
			detail = "the raft membership read did not complete"
		}
		return QuorumDecision{
			Proceed: false,
			Verdict: recovery.QuorumUnknown,
			Outcome: OutcomeUnverified,
			Detail: "could not read raft membership and voter reachability (" + detail +
				") - this is 'could not check', not 'checked and unsafe'; refusing rather than assuming quorum survives",
			Evidence: []Evidence{{
				Rule:           RuleQuorumReadable,
				Detail:         detail,
				ObservedAtUnix: f.ObservedAt,
			}},
		}
	}

	if !recovery.ValidQuorumFact(f.Fact) {
		return QuorumDecision{
			Proceed: false,
			Verdict: recovery.QuorumUnknown,
			Outcome: OutcomeUnverified,
			Detail: "the raft quorum fact read back is not internally consistent (total voters " +
				fmt.Sprintf("%d, remaining %d, reachable %d, unknown %d, quorum size %d)",
					f.Fact.TotalVoters, f.Fact.RemainingVoters, f.Fact.RemainingReachable, f.Fact.RemainingUnknown, f.Fact.QuorumSize) +
				" - treating a malformed read as a genuine quorum loss would report an outage that did not happen",
			Evidence: []Evidence{{
				Rule:           RuleQuorumReadable,
				Detail:         "recovery.ValidQuorumFact rejected the read",
				ObservedAtUnix: f.ObservedAt,
			}},
		}
	}

	verdict := recovery.ClassifyQuorum(f.Fact, f.IsLeader)
	arith := fmt.Sprintf("%d/%d voters confirmed reachable, %d unknown, quorum size %d, target is %s",
		f.Fact.RemainingReachable, f.Fact.RemainingVoters, f.Fact.RemainingUnknown, f.Fact.QuorumSize, voterWord(f.Fact.TargetIsVoter))

	if verdict == recovery.QuorumSurvives {
		return QuorumDecision{
			Proceed:  true,
			Verdict:  verdict,
			Outcome:  OutcomeInProgress,
			Detail:   "quorum established: " + arith,
			Evidence: []Evidence{{Rule: RuleQuorumGate, Detail: arith, ObservedAtUnix: f.ObservedAt}},
		}
	}

	detail := "raft quorum is not established (" + arith + "): " + verdictWord(verdict, f) +
		" - a migration may not start or advance, and this is recorded as an unknown outcome, never as a failure"
	return QuorumDecision{
		Proceed: false,
		Verdict: verdict,
		Outcome: OutcomeUnverified,
		Detail:  detail,
		Evidence: []Evidence{{
			Rule:           RuleQuorumStall,
			Detail:         arith,
			ObservedAtUnix: f.ObservedAt,
		}},
	}
}

func voterWord(isVoter bool) string {
	if isVoter {
		return "a voter"
	}
	return "a non-voter"
}

func verdictWord(v recovery.QuorumVerdict, f QuorumFact) string {
	switch v {
	case recovery.QuorumLost:
		return "a majority cannot be reached even crediting every voter whose reachability is unknown"
	case recovery.QuorumUnknown:
		if f.IsLeader {
			return "the confirmed-reachable count alone falls short, and this node is the leader, so the underlying " +
				"reachability was gathered through the leader and proves nothing about whether the remaining voters can reach each other"
		}
		return "the confirmed-reachable count alone falls short, but the deficit could close if the unknown voters turn out reachable"
	default:
		return string(v)
	}
}

// StallFromDecision builds the record-level stall that corresponds to a
// blocking QuorumDecision. A migration halted by the quorum gate is
// still in flight and still fenced - the ADR's rule is "halt; do not
// cut over", not "fail" - so what changes is that the record now says
// why it stopped, and the guest stays frozen with a named reason
// instead of an unexplained one.
//
// It returns the record unchanged when the decision allowed the move:
// a gate that passed is not a stall, and writing a stall for it would
// leave a record that says it is waiting when it is not.
func StallFromDecision(r Record, d QuorumDecision, atUnix int64) (Record, error) {
	if d.Proceed {
		return r, nil
	}
	reason := RuleQuorumStall
	switch d.Verdict {
	case recovery.QuorumLost:
		reason = "quorum-lost"
	case recovery.QuorumUnknown:
		reason = "quorum-unknown"
	}
	return Stall(r, Stalled{
		Reason:    reason,
		Detail:    d.Detail,
		Verdict:   string(d.Verdict),
		SinceUnix: atUnix,
	}, atUnix)
}
