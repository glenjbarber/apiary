package migration

import (
	"errors"
	"fmt"
	"strings"
)

// Sentinel errors for the transition and validation rules. They are
// exported so the RPC handler can map each one to a specific,
// operator-legible error string - ADR-0128's whole reason for
// enumerating preflight rejections is that an operator needs to be told
// which thing was wrong, not merely that something was - and so a
// caller can test on the class of failure with errors.Is rather than
// string-matching a message.
//
// Every one of these is a rejection. None of them is ever logged and
// swallowed, and none of them results in a partial mutation: the
// functions that return them return the original record unchanged.
var (
	// ErrIllegalTransition is a move the phase machine does not allow,
	// in either direction: skipping forward, rewinding, or re-entering
	// a phase without marking it as a retry.
	ErrIllegalTransition = errors.New("migration: illegal phase transition")

	// ErrIllegalOutcome is a settle to an outcome that is not
	// reachable from the record's current state.
	ErrIllegalOutcome = errors.New("migration: illegal outcome transition")

	// ErrUnbackedOutcome is a settle whose outcome has no supporting
	// evidence. Distinct from ErrIllegalOutcome because it is a
	// different bug: the transition was legal, the record just has
	// nothing behind it.
	ErrUnbackedOutcome = errors.New("migration: outcome not backed by evidence")

	// ErrStaleToken is a report presenting a token other than the
	// record's current one - a delayed RPC from a superseded attempt.
	ErrStaleToken = errors.New("migration: stale attempt token")

	// ErrDuplicateCommand is a command that has already been applied:
	// a re-delivered advance, a second start for a guest that is
	// already migrating, or a settle for an already-settled record.
	// Rejected rather than absorbed as a no-op, because a duplicate
	// reaching this package means the caller's attempt bookkeeping is
	// wrong, and a silent no-op would hide that until it mattered.
	ErrDuplicateCommand = errors.New("migration: duplicate command")

	// ErrNoSuchRecord is a command naming a record that is not in
	// state. Distinct from ErrDuplicateCommand: nothing was ever
	// created.
	ErrNoSuchRecord = errors.New("migration: no such migration record")

	// ErrQuorumNotEstablished is a gate refused because quorum is lost
	// or could not be determined. It is never reported as a migration
	// failure - see quorum.go.
	ErrQuorumNotEstablished = errors.New("migration: quorum not established")

	// ErrGuestAlreadyMigrating is a start for a guest that already has
	// a live migration record.
	ErrGuestAlreadyMigrating = errors.New("migration: guest already migrating")

	// ErrAbortTooLate is an abort requested at or after cutover, when
	// the guest's ownership has already moved and "return it to the
	// source" is not a defined action.
	ErrAbortTooLate = errors.New("migration: too late to abort")
)

// Transition is a requested move from one phase to another.
//
// Retry is the whole content of the self-transition. A migration that
// drops its link during bulk does not rewind, and it does not skip; it
// re-enters the same phase with a new token, having resumed from its
// storage resume token. Retry=true is how the record distinguishes
// that from a caller that simply sent the current phase again, which
// is a duplicate and is rejected.
type Transition struct {
	From  Phase
	To    Phase
	Retry bool
}

// Classify decides what a requested move means, in terms this package
// is willing to act on. A nil error is the only "yes": every move is
// either accepted (with an optional note for the caller) or rejected
// with a reason that names what was wrong. There is no separate
// legal bool, because a bool and an error that can disagree is a bool
// that will eventually be read on the wrong side of the error.
//
// The three decisions below are the whole state machine, and each one
// is deliberate:
//
//   - Forward skips are ILLEGAL. ADR-0128 says the Colony "may never
//     skip a phase", and the reasoning holds up: a journal whose entries
//     can be skipped cannot answer "was this guest ever frozen?", and
//     that question is exactly what the ADR's recovery rule turns on.
//     The case that looks like an exception is the HAST-replica target,
//     where the bulk copy is a genuine no-op because the target already
//     holds the data. That is expressed by DOING bulk with zero bytes
//     (Transfer.RequiredBytes == 0), not by skipping it - which is
//     also why a no-op bulk is still timestamped, still attempts the
//     transfer, and still fails if the target cannot confirm the copy
//     it already claims to have.
//
//   - Backwards transitions are ILLEGAL. Nothing in the protocol moves a
//     record backwards: a failed phase is retried in place, a dropped
//     link resumes from the storage token within the same phase, and
//     giving up is an abort (a terminal outcome), not a rewind. A
//     rewind would also be the one move that could re-open a window
//     already closed: un-freeze the guest while the record claims the
//     guest is syncing is precisely the split-brain ADR-0128's fence
//     exists to prevent.
//
//   - Self-transitions are legal ONLY as a marked retry. Unmarked, they
//     are a duplicate and are rejected by command.go.
func Classify(r Record, t Transition) (reason string, err error) {
	if !t.To.Valid() {
		return "", fmt.Errorf("%w: %q is not a migration phase", ErrIllegalTransition, t.To)
	}
	if !t.From.Valid() {
		return "", fmt.Errorf("%w: record %s is at invalid phase %q", ErrIllegalTransition, r.ID, t.From)
	}
	if t.From != r.Phase {
		return "", fmt.Errorf("%w: record %s is at %s, cannot move to %s", ErrIllegalTransition, r.ID, r.Phase, t.To)
	}
	if r.Outcome != OutcomeInProgress {
		return "", fmt.Errorf("%w: record %s has settled as %s and no longer moves between phases", ErrIllegalTransition, r.ID, r.Outcome)
	}
	if t.To == t.From {
		if !t.Retry {
			return "", fmt.Errorf("%w: record %s is already at %s - re-entering a phase is a retry and must say so", ErrIllegalTransition, r.ID, t.To)
		}
		return fmt.Sprintf("retrying %s with a new attempt token; resume from the recorded storage token", t.To), nil
	}
	fromIdx, toIdx := t.From.Index(), t.To.Index()
	if toIdx < fromIdx {
		return "", fmt.Errorf("%w: record %s cannot rewind from %s back to %s", ErrIllegalTransition, r.ID, t.From, t.To)
	}
	if toIdx != fromIdx+1 {
		skipped := PhaseOrder[fromIdx+1 : toIdx]
		names := make([]string, 0, len(skipped))
		for _, p := range skipped {
			names = append(names, string(p))
		}
		return "", fmt.Errorf("%w: record %s cannot jump from %s to %s, skipping %s - a migration journal with holes cannot answer whether the guest was ever frozen",
			ErrIllegalTransition, r.ID, t.From, t.To, strings.Join(names, ", "))
	}
	return "", nil
}

// PhaseResult is the outcome of a move that was allowed: the new
// record, and - for a retry - the note worth writing into the record's
// own history about what the caller is now expected to do differently.
// A caller that retries a phase and ignores the note is retrying blind,
// so it is returned rather than buried. There is no "moved" flag
// because a nil error already says so, and a bool that can disagree
// with an error will eventually be read on the wrong side of it.
type PhaseResult struct {
	Record Record
	Reason string
}

// Advance applies a transition to a record and returns the new record.
//
// A rejected transition returns the ORIGINAL record, not a zero value
// and not a partially-updated one. There is no partial application
// anywhere in this package: every function that can fail either returns
// the input unchanged or an error, so a caller that ignores the error
// still holds a coherent record.
//
// token must be strictly greater than the record's current token. It is
// the leader's to mint, and requiring it to advance on every phase
// entry is what makes a delayed peer report from an earlier attempt
// unable to act (command.go checks the presented token against the
// record before it ever gets here, so a stale report never reaches
// this function; requiring the increase as well means a caller that
// mints tokens wrong cannot reintroduce the problem through the back
// door).
func Advance(r Record, t Transition, token uint64, atUnix int64) (PhaseResult, error) {
	reason, err := Classify(r, t)
	if err != nil {
		return PhaseResult{Record: r}, err
	}
	if token <= r.Token {
		return PhaseResult{Record: r}, fmt.Errorf("%w: record %s is at token %d and cannot enter a phase with token %d - a token must strictly increase so a delayed attempt cannot act",
			ErrStaleToken, r.ID, r.Token, token)
	}

	out := r.Clone()
	out.Phase = t.To
	out.Token = token
	out.Attempt++
	out.Timestamps.Entered(t.To, atUnix)
	// A retry clears the previous stall: the operator-visible reason
	// for not advancing is gone, because the record just advanced. A
	// forward move clears it for the same reason.
	out.Stall = nil

	return PhaseResult{Record: out, Reason: reason}, nil
}

// Stall records why an in-flight migration is not advancing, without
// settling it. A stalled record is still in flight and still fenced;
// the difference is that something has now said out loud that the
// operator would otherwise have to infer from a stuck spinner.
//
// Refuses to stall a settled record (there is nothing left to wait for)
// and refuses a stall with no reason. It does NOT refuse when quorum is
// the reason - losing quorum is precisely the case a stall exists to
// name, and calling it an error would leave the record indistinguishable
// from one that is simply proceeding.
func Stall(r Record, s Stalled, atUnix int64) (Record, error) {
	if r.Outcome != OutcomeInProgress {
		return r, fmt.Errorf("%w: record %s has settled as %s and cannot be stalled", ErrIllegalTransition, r.ID, r.Outcome)
	}
	if strings.TrimSpace(s.Reason) == "" {
		return r, fmt.Errorf("migration: stalling record %s requires a reason - a stall with no reason is indistinguishable from a hang", r.ID)
	}
	if strings.TrimSpace(s.Detail) == "" {
		return r, fmt.Errorf("migration: stalling record %s with reason %q requires a detail", r.ID, s.Reason)
	}
	out := r.Clone()
	stall := s
	stall.SinceUnix = atUnix
	out.Stall = &stall
	out.Timestamps.normalize()
	if atUnix > out.Timestamps.UpdatedUnix {
		out.Timestamps.UpdatedUnix = atUnix
	}
	return out, nil
}

// RecordVerification stores the target's own last report on the record,
// so a later settle can cite it and so a reader can see what the last
// thing anybody actually heard from the target was.
//
// This is the one place node-local observation enters raft, and it is
// deliberately the smallest possible form: a yes/no, two identifiers
// that identify the guest rather than describe it, and a timestamp. No
// dataset contents, no process state, no handles. An observation is
// evidence ABOUT a node, not state OF the guest.
func RecordVerification(r Record, v Verification) (Record, error) {
	if r.Outcome != OutcomeInProgress {
		return r, fmt.Errorf("%w: record %s has settled as %s and no longer accepts observations", ErrIllegalTransition, r.ID, r.Outcome)
	}
	if !v.Observed && strings.TrimSpace(v.QueryError) == "" {
		return r, fmt.Errorf("migration: recording an unobserved verification for %s with no query error - say why the query produced nothing", r.ID)
	}
	if v.Observed && v.ObservedAt == 0 {
		return r, fmt.Errorf("migration: recording a verification for %s with no observation time", r.ID)
	}
	out := r.Clone()
	out.Verification = v
	out.Timestamps.normalize()
	if v.ObservedAt > out.Timestamps.UpdatedUnix {
		out.Timestamps.UpdatedUnix = v.ObservedAt
	}
	return out, nil
}

// UpdateTransfer records progress against the scheduled copy. Progress
// is clamped by Record.Validate rather than here, so a caller that
// over-reports gets a named error instead of a silently corrected
// number - a clamped counter is a lie that looks like data.
func UpdateTransfer(r Record, t Transfer) (Record, error) {
	if r.Outcome != OutcomeInProgress {
		return r, fmt.Errorf("%w: record %s has settled as %s and no longer accepts transfer progress", ErrIllegalTransition, r.ID, r.Outcome)
	}
	out := r.Clone()
	out.Transfer = t
	// Validated here rather than at the next read: an over-reported
	// byte count or a rewound resume token is rejected at the point it
	// is offered, named, rather than corrected or discovered later.
	if err := out.Validate(); err != nil {
		return r, err
	}
	return out, nil
}
