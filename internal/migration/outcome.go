package migration

import (
	"fmt"
	"strings"
)

// Outcome is what has actually been established about a migration so
// far. It is orthogonal to Phase on purpose (see Phase's doc comment):
// a record at cutover can be in flight, failed, or unobserved, and
// collapsing those into a single enum value is how an unfinished
// migration ends up rendered as a finished one.
//
// The vocabulary is internal/cluster's RecoveryVerdict and
// internal/restartplan's Outcome, extended for the one thing neither
// has to answer: "did this end the way we were trying to make it end?".
// The unobserved/unverified split is preserved exactly as those two
// packages keep it, because the distinction is real and this codebase
// has twice refused to drop it:
//
//   - unobserved  ("we tried, and learned nothing"): a verification
//     query was sent to the target and came back unusable.
//   - unverified  ("we never got to try"): no observation was even
//     possible - the quorum read failed, the leader changed, the
//     process died before asking.
type Outcome string

const (
	// OutcomeInProgress means the migration is still legitimately
	// running. It is the only non-terminal outcome besides the two
	// completion-never-observed ones, and it is the record's state
	// from creation until an evidence-backed settle.
	OutcomeInProgress Outcome = "in_progress"

	// OutcomeObservedComplete is the only outcome that may ever be
	// rendered as a success. Reaching it requires a direct
	// observation from the target node of a running guest whose
	// dataset GUID and preserved IP both match - see
	// Verification.Matches. No other route to this value exists, and
	// Record.Validate refuses a record that claims it without the
	// matching observation.
	OutcomeObservedComplete Outcome = "observed_complete"

	// OutcomeFailed means there is positive evidence the migration did
	// not work: a target that answered and reported the guest not
	// running, a dataset GUID that did not match, a storage step that
	// returned a real error. A failure is a thing that was observed.
	OutcomeFailed Outcome = "failed"

	// OutcomeUnobserved means an attempt was made to find out how the
	// migration ended and the answer was unusable - the target did not
	// answer, or answered with something this code cannot interpret.
	// Never success, never failure: the direct analog of
	// internal/cluster's replica_unobserved.
	OutcomeUnobserved Outcome = "completion_unobserved"

	// OutcomeUnverified means no attempt to find out was even possible.
	// Never success, never failure: the direct analog of
	// internal/cluster's unverified_replica.
	OutcomeUnverified Outcome = "completion_unverified"

	// OutcomeAborted means an operator deliberately stopped the
	// migration and asked for the guest to go back to its source. It is
	// neither a success (the guest did not move) nor a failure (nothing
	// broke); it is the absence of a completed attempt, named as such -
	// the same distinction internal/restartplan's OutcomeBlocked draws
	// for a guardrail that refused to act.
	//
	// Abort is the one outcome that is evidenced by an operator's
	// decision rather than by an observation of the system, and it is
	// fenced to RoleOperator for that reason: it is the only
	// destructive action in the whole feature.
	OutcomeAborted Outcome = "aborted"
)

// Recognized reports whether o is one of this package's outcomes. An
// unrecognized value is refused rather than defaulted: defaulting an
// unknown outcome to in_progress would leave a guest fenced forever,
// and defaulting it to anything terminal would unfence a guest we know
// nothing about. Both directions of a wrong default are dangerous here,
// so there is no default.
func (o Outcome) Recognized() bool {
	switch o {
	case OutcomeInProgress, OutcomeObservedComplete, OutcomeFailed,
		OutcomeUnobserved, OutcomeUnverified, OutcomeAborted:
		return true
	}
	return false
}

// IsSuccess reports whether o may be rendered as "the migration
// worked". Only OutcomeObservedComplete qualifies.
func (o Outcome) IsSuccess() bool { return o == OutcomeObservedComplete }

// IsFailure reports whether o may be rendered as "the migration
// failed", i.e. a positive observation of breakage. Only OutcomeFailed
// qualifies - an unreachable target is not evidence of a failed
// migration, and treating it as one is the inversion this whole
// vocabulary exists to prevent.
func (o Outcome) IsFailure() bool { return o == OutcomeFailed }

// IsCompletionNeverObserved reports whether o means "we do not know
// how this ended". Both the unobserved and the unverified value answer
// that question with yes, which is the four-way distinction the ADR's
// operator view needs: in flight, known-good, known-bad, unknown.
func (o Outcome) IsCompletionNeverObserved() bool {
	return o == OutcomeUnobserved || o == OutcomeUnverified
}

// IsTerminal reports whether the outcome can still change. in_progress
// is live, and a stalled in_progress record is still live - a stall is
// a reason not to advance right now, never a terminal state.
func (o Outcome) IsTerminal() bool {
	return o.Recognized() && o != OutcomeInProgress
}

// Verdicts renders the outcome for an operator, in words chosen so the
// unknown cases cannot be skimmed as a success. The unknown string
// deliberately includes the word "unknown" and the words "not a
// confirmed success" - a UI that truncates a long string still shows
// enough to be honest.
func (o Outcome) Verdict() string {
	switch o {
	case OutcomeInProgress:
		return "in progress"
	case OutcomeObservedComplete:
		return "complete (observed)"
	case OutcomeFailed:
		return "failed"
	case OutcomeUnobserved:
		return "completion never observed"
	case OutcomeUnverified:
		return "completion unverified"
	case OutcomeAborted:
		return "aborted by operator"
	default:
		return "unrecognized outcome " + string(o)
	}
}

// SettleInput is everything known at the moment someone proposes to
// stop calling this migration in flight. It is the complete input to
// DeriveOutcome: nothing is read from the record, the package's
// globals, or the clock, so the decision is a pure function of
// gathered facts and is exhaustively testable.
type SettleInput struct {
	// Verification is the target's own report. Observed false with a
	// non-empty QueryError is "tried and learned nothing";
	// Observed false with an empty QueryError is a caller bug and is
	// treated as the weaker of the two.
	Verification Verification

	// FailureDetail is a real, positive failure reason from a real
	// error - a storage step that returned non-zero, a target that
	// answered "not running", a GUID mismatch. Empty means "no
	// positive evidence of breakage was gathered", which is NOT the
	// same as "everything is fine".
	FailureDetail string

	// QuorumUnavailable is true when the settle was reached without
	// trustworthy consensus: the quorum fact could not be read, or it
	// classified as lost or unknown. It does not mean the migration
	// failed - it means this process has no standing to declare
	// anything about it. See quorum.go.
	QuorumUnavailable bool

	// QueryAttempted is true when a check was actually sent to the
	// target and came back unusable. It is a separate field from
	// Verification.QueryError rather than inferred from it, because
	// "we asked and got nothing" and "nobody asked" are different
	// facts and only the first of them is an unobserved completion.
	//
	// Leaving it false with no failure and no verification is what
	// makes DeriveOutcome refuse: a caller that gathered literally
	// nothing has no basis for any verdict, and a guessed one would be
	// the exact failure this vocabulary exists to prevent.
	QueryAttempted bool

	// Aborted and AbortReason are the operator's deliberate stop. Abort
	// is decided by a human, evidenced by the reason they gave, and is
	// not an observation of the system.
	Aborted     bool
	AbortReason string
}

// DeriveOutcome decides the outcome a record should settle to, given
// only gathered evidence, and refuses to guess.
//
// The rules, in order:
//
//  1. Aborted -> OutcomeAborted, provided a reason was given. Checked
//     first because an operator's deliberate stop is the strongest
//     statement available and because command.go has already refused
//     an abort from cutover onward - this function is not where that
//     legality is decided, and does not pretend to be.
//  2. Abort without a reason -> an error. "Stop the migration" with no
//     recorded reason is not a record.
//  3. FailureDetail present -> OutcomeFailed. A failure needs a reason
//     the way a success needs evidence, and an unbacked failure is
//     just as much a lie.
//  4. Verification.Matches -> OutcomeObservedComplete, carrying the
//     observation as its evidence.
//  5. Observed, not running -> OutcomeFailed. The target answered and
//     told us the guest is not up. That is a real observation of
//     breakage, not a gap in our knowledge.
//  6. Observed, running, but GUID or IP did not match -> OutcomeFailed,
//     with the mismatch spelled out. A running guest that is not the
//     guest we moved is the worst outcome available and is definitely
//     not a success.
//  7. Quorum unavailable -> OutcomeUnverified. We never got the
//     standing to find out, which is precisely "no observation was
//     even possible". Checked before the query result because a
//     process with no consensus standing does not get to narrate what
//     a query it could not validly make would have meant.
//  8. A query was attempted and came back unusable -> OutcomeUnobserved.
//     We tried and learned nothing.
//  9. Otherwise -> an error, never a default.
//
// Rule 9 is the load-bearing one. Every honest answer is already
// covered above, so reaching it means the caller came with no
// evidence at all - not a failure, not an observation, not even a
// failed query. A migration whose completion nobody observed is
// exactly the case that must never be written down as finished, and a
// function that returned a verdict here would be one line away from
// returning the right one by accident.
func DeriveOutcome(in SettleInput) (Outcome, error) {
	if in.Aborted {
		reason := strings.TrimSpace(in.AbortReason)
		if reason == "" {
			return "", fmt.Errorf("migration: abort requires a reason - an unexplained stop is not a record")
		}
		return OutcomeAborted, nil
	}
	if strings.TrimSpace(in.FailureDetail) != "" {
		return OutcomeFailed, nil
	}
	switch {
	case in.Verification.Matches():
		return OutcomeObservedComplete, nil
	case in.Verification.Observed && !in.Verification.GuestRunning:
		// The target answered and told us the guest is not up. A real
		// observation of breakage, not a gap in our knowledge.
		return OutcomeFailed, nil
	case in.Verification.Observed:
		// Observed, running, but not a match: the identity checks
		// failed. A mismatch is a real, observed fact about a real
		// guest, so it is a failure and not an unknown.
		return OutcomeFailed, nil
	case in.QuorumUnavailable:
		return OutcomeUnverified, nil
	case in.QueryAttempted || strings.TrimSpace(in.Verification.QueryError) != "":
		return OutcomeUnobserved, nil
	default:
		return "", fmt.Errorf("migration: no evidence was gathered about how this migration ended - " +
			"a verdict with nothing behind it is not a verdict (a query must either answer, fail, or be recorded as not attempted)")
	}
}

// Settle applies a settled outcome to a record, carrying its evidence
// with it.
//
// It refuses to settle a record that has already settled. That is what
// makes a replayed finish command harmless: the second application is
// rejected outright rather than rewriting one terminal outcome into
// another, so a delayed report from an abandoned attempt cannot turn a
// recorded failure into a recorded success.
func Settle(r Record, outcome Outcome, evidence []Evidence, detail string, atUnix int64) (Record, error) {
	if r.Outcome != OutcomeInProgress {
		return Record{}, fmt.Errorf("%w: record %s already settled as %s - a settled migration cannot be re-settled as %s",
			ErrIllegalTransition, r.ID, r.Outcome, outcome)
	}
	if !outcome.Recognized() || outcome == OutcomeInProgress {
		return Record{}, fmt.Errorf("%w: cannot settle record %s to %q", ErrIllegalTransition, r.ID, outcome)
	}
	if outcome.IsSuccess() && len(evidence) == 0 {
		return Record{}, fmt.Errorf("%w: cannot settle record %s to %q with no evidence", ErrUnbackedOutcome, r.ID, outcome)
	}
	if outcome.IsFailure() && len(evidence) == 0 {
		return Record{}, fmt.Errorf("%w: cannot settle record %s to %q with no evidence", ErrUnbackedOutcome, r.ID, outcome)
	}
	if outcome == OutcomeAborted && strings.TrimSpace(detail) == "" {
		return Record{}, fmt.Errorf("%w: cannot settle record %s to %q with no reason", ErrUnbackedOutcome, r.ID, outcome)
	}
	if outcome.IsSuccess() && !r.Verification.Matches() {
		return Record{}, fmt.Errorf("%w: cannot settle record %s to %q without a matching target observation",
			ErrUnbackedOutcome, r.ID, outcome)
	}

	out := r.Clone()
	out.Outcome = outcome
	out.Detail = detail
	out.Evidence = append(out.Evidence, evidence...)
	out.Timestamps.normalize()
	out.Timestamps.SettledUnix = atUnix
	if atUnix > out.Timestamps.UpdatedUnix {
		out.Timestamps.UpdatedUnix = atUnix
	}
	// A settled record is not stalled, whatever it was doing a moment
	// ago - including an unobserved one. It is still fenced (fence.go),
	// but it is no longer "waiting for something to happen": an
	// operator has to act on it.
	out.Stall = nil

	if err := out.Validate(); err != nil {
		return Record{}, err
	}
	return out, nil
}
