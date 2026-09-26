package migration

import (
	"fmt"
	"strings"
)

// CommandKind is the raft command arm this package is validating. The
// three arms mirror the narrow-command style internalpb.Command already
// uses for every other lifecycle action in this codebase (SetVMPhase,
// SetVMDesiredState, PurgeVM): a start, a step, and a finish. There is
// deliberately no general "update migration" arm, because a general
// update is how a crafted command sets a phase and an outcome that were
// never reachable together.
type CommandKind string

const (
	CommandStart   CommandKind = "start_guest_migration"
	CommandAdvance CommandKind = "update_guest_migration_phase"
	CommandSettle  CommandKind = "finish_guest_migration"
	CommandAbort   CommandKind = "abort_guest_migration"
)

// Recognized reports whether k is one of the four arms.
func (k CommandKind) Recognized() bool {
	switch k {
	case CommandStart, CommandAdvance, CommandSettle, CommandAbort:
		return true
	}
	return false
}

// Command is one proposed raft command for this feature, in the plain
// Go shape the FSM applies from and the RPC handler submits.
//
// It is validated in two places, deliberately, because a raft command
// can arrive from two directions with different trust:
//
//   - ValidateCommand, below, before the FSM applies anything. This is
//     the real enforcement. It is the only thing standing between a
//     crafted command, a replayed command, and a buggy caller and the
//     state machine.
//   - The per-field checks inside the mutation functions
//     (Record.Validate, Advance, Settle), which hold even when a caller
//     reaches them directly.
//
// Belt and braces is the right shape here specifically because a
// migration's failure modes include two live copies of one guest, and
// the cheapest possible recovery from that is never getting there.
type Command struct {
	Kind CommandKind

	// ID is the migration record id. A start mints it; every other arm
	// names an existing one.
	ID string

	// Guest, SourceID and TargetID are required on a start and are
	// cross-checked against the existing record on every other arm -
	// a command that names one guest's record id and another guest's
	// identity is a crafted command, and the mismatch is named rather
	// than ignored.
	Guest    Workload
	SourceID string
	TargetID string

	// To is the phase an advance is asking to enter. For a start it is
	// ignored and must be empty: a record is created at preflight and
	// nothing else, so a start that also names a phase is asking for a
	// record that was born mid-protocol.
	To Phase

	// Retry marks an advance that re-enters the record's current phase
	// (see Transition.Retry).
	Retry bool

	// Token is the attempt token the caller minted. Zero is never
	// valid, and on a start it is the leader's first allocation for
	// this record.
	Token uint64

	// Outcome is set only on a settle, and only to a terminal value.
	// Settle ignores it entirely and re-derives the outcome from
	// evidence, so a caller cannot assert an outcome the evidence does
	// not support; the field exists so a mismatch is reportable rather
	// than silently corrected.
	Outcome Outcome

	// Verification is the target's observation, set by a settle.
	Verification Verification

	// FailureDetail is positive evidence of breakage, set by a settle.
	FailureDetail string

	// AbortReason is required on an abort and forbidden everywhere
	// else.
	AbortReason string

	// QuorumUnavailable marks a settle reached without trustworthy
	// consensus. It never means "the migration failed".
	QuorumUnavailable bool

	// Evidence is the cited basis for a settle.
	Evidence []Evidence

	// Transfer is the copy provenance an advance carries alongside the
	// phase move - required on an advance into bulk or sync, where
	// bytes actually move. Optional elsewhere, where there is nothing
	// to report.
	Transfer *Transfer

	// Detail is the operator-facing reason.
	Detail string

	// AtUnix is when the submitting node says this happened. It is
	// display and evidence only; it is never compared against another
	// node's clock to decide legality, because a clock-skewed follower
	// must not be able to move a record backwards in time.
	AtUnix int64
}

// ValidateCommand is the server-side gate every proposed command passes
// before the FSM touches state. It is pure, it reads only the state
// handed to it, and it never mutates anything.
//
// The checks, per arm:
//
//	start   - the guest is not already migrating (the guest index is a
//	          single point read precisely so this is not a scan), the
//	          source and target are distinct and non-empty, the guest
//	          identity is complete, a token was actually allocated, and
//	          no phase was requested.
//	advance - the record exists, the named guest and nodes match the
//	          record's, the phase requested is not the one the record is
//	          already at, the token presented is one the record has not
//	          already spent, and the move itself is legal. A duplicate is
//	          rejected as a duplicate and a superseded report as a
//	          superseded one, never absorbed and never relabelled as
//	          some other failure.
//	settle  - the record exists, is still in flight, the token matches,
//	          the guest matches, and the proposed outcome equals the
//	          one DeriveOutcome produces from the evidence. A proposed
//	          outcome the evidence does not support is a rejection, not
//	          a correction.
//	abort   - the record exists, is still in flight, the token matches,
//	          the guest matches, a reason was given, and the record has
//	          NOT reached cutover.
//
// That last check is a deliberate narrowing of ADR-0128, which describes
// abort as always meaning "return the guest to the source and run it
// there". That action is well defined right up to the cutover: a frozen,
// half-copied guest on the source is the source's, and undoing is
// reversible. After cutover the source is not the owner - the raft log
// says the target is - and "return it to the source" is not an action
// this protocol defines. Moving it back is a migration, and it must be
// started as one, with its own preflight, its own fence, and its own
// quorum gate. Letting abort reach past cutover would unfence a guest
// whose recorded owner is the target, which is the one move that could
// produce two running copies.
func ValidateCommand(st *MigrationState, cmd Command) error {
	if st == nil {
		return fmt.Errorf("migration: command %s has no state to validate against", cmd.Kind)
	}
	if !cmd.Kind.Recognized() {
		return fmt.Errorf("migration: unrecognized command kind %q", cmd.Kind)
	}
	if strings.TrimSpace(cmd.ID) == "" {
		return fmt.Errorf("migration: command %s has no record id", cmd.Kind)
	}
	if cmd.Token == 0 {
		return fmt.Errorf("migration: command %s for %s presents token 0 - an unallocated token matches nothing, so it must never match anything either",
			cmd.Kind, cmd.ID)
	}

	existing, found := st.Lookup(cmd.ID)

	switch cmd.Kind {
	case CommandStart:
		if found {
			return fmt.Errorf("%w: record %s already exists", ErrDuplicateCommand, cmd.ID)
		}
		if !cmd.Guest.valid() {
			return fmt.Errorf("migration: start command for %s names no usable guest (kind %q id %q)", cmd.ID, cmd.Guest.Kind, cmd.Guest.ID)
		}
		if strings.TrimSpace(cmd.SourceID) == "" || strings.TrimSpace(cmd.TargetID) == "" {
			return fmt.Errorf("migration: start command for %s must name both a source and a target node", cmd.ID)
		}
		if cmd.SourceID == cmd.TargetID {
			return fmt.Errorf("migration: start command for %s has source and target both %q - there is nowhere to move the guest to", cmd.ID, cmd.SourceID)
		}
		if cmd.To != "" {
			return fmt.Errorf("migration: start command for %s also names phase %q - a record is created at preflight, never mid-protocol", cmd.ID, cmd.To)
		}
		if cmd.Outcome != "" && cmd.Outcome != OutcomeInProgress {
			return fmt.Errorf("%w: start command for %s claims outcome %q - a migration is created in flight or not at all", ErrIllegalOutcome, cmd.ID, cmd.Outcome)
		}
		if cmd.AbortReason != "" {
			return fmt.Errorf("migration: start command for %s carries an abort reason", cmd.ID)
		}
		if held, ok := st.FencingFor(cmd.Guest); ok {
			return fmt.Errorf("%w: %s is held by record %s (phase %s, outcome %q) - an unresolved migration must be resolved before another one starts, or a second move could run alongside the first",
				ErrGuestAlreadyMigrating, cmd.Guest, held.ID, held.Phase, held.Outcome)
		}
		return nil

	case CommandAdvance, CommandSettle, CommandAbort:
		if !found {
			return fmt.Errorf("%w: command %s names record %s", ErrNoSuchRecord, cmd.Kind, cmd.ID)
		}
		if cmd.Guest.valid() && cmd.Guest != existing.Guest {
			return fmt.Errorf("migration: command %s for record %s names guest %s, but that record is migrating %s",
				cmd.Kind, cmd.ID, cmd.Guest, existing.Guest)
		}
		if cmd.SourceID != "" && cmd.SourceID != existing.SourceID {
			return fmt.Errorf("migration: command %s for record %s names source %s, but the record's source is %s",
				cmd.Kind, cmd.ID, cmd.SourceID, existing.SourceID)
		}
		if cmd.TargetID != "" && cmd.TargetID != existing.TargetID {
			return fmt.Errorf("migration: command %s for record %s names target %s, but the record's target is %s",
				cmd.Kind, cmd.ID, cmd.TargetID, existing.TargetID)
		}
	}

	switch cmd.Kind {
	case CommandAdvance:
		if existing.Outcome != OutcomeInProgress {
			return fmt.Errorf("%w: record %s has settled as %s and cannot advance", ErrIllegalTransition, cmd.ID, existing.Outcome)
		}
		if !cmd.To.Valid() {
			return fmt.Errorf("%w: advance command for %s names invalid phase %q", ErrIllegalTransition, cmd.ID, cmd.To)
		}
		// Re-entering the phase the record is already at is checked
		// FIRST, before the token, because it is a property of the
		// request alone and needs no context to judge: whatever token
		// it carries, asking to enter a phase the record is already in
		// is a duplicate. Labelling that "stale token" would send a
		// caller hunting for an attempt-management bug they do not
		// have, and labelling it "illegal transition" would not tell
		// them the phase was already reached.
		if cmd.To == existing.Phase && !cmd.Retry {
			return fmt.Errorf("%w: advance command for %s re-enters phase %s, which it is already at (presented token %d, record is at token %d)",
				ErrDuplicateCommand, cmd.ID, cmd.To, cmd.Token, existing.Token)
		}
		// Everything else is decided by the token. An advance MINTS a
		// new token, so anything at or below the record's current one
		// is a token this record has already spent - which is exactly
		// what a report from a superseded attempt carries. This check
		// runs before the transition-legality checks on purpose: a late
		// report of a phase the record has already moved past would
		// otherwise be rejected as "cannot rewind", which is a
		// confusing description of what is really a late report.
		if cmd.Token <= existing.Token {
			return fmt.Errorf("%w: advance command for %s presents token %d, which the record has already spent (it is at token %d) - this command belongs to a superseded attempt",
				ErrStaleToken, cmd.ID, cmd.Token, existing.Token)
		}
		// Classify is the authority on the move itself: it is the same
		// function the mutation path uses, so the preview and the
		// enforcement cannot drift.
		if _, err := Classify(existing, Transition{From: existing.Phase, To: cmd.To, Retry: cmd.Retry}); err != nil {
			return err
		}
		return nil

	case CommandSettle:
		if existing.Outcome != OutcomeInProgress {
			return fmt.Errorf("%w: settle command for record %s, which has already settled as %s", ErrDuplicateCommand, cmd.ID, existing.Outcome)
		}
		// A settle does not mint a token, so it must present the one
		// the record is currently on.
		if cmd.Token != existing.Token {
			return fmt.Errorf("%w: settle command for %s presents token %d, but the record is at token %d - this command belongs to a superseded attempt",
				ErrStaleToken, cmd.ID, cmd.Token, existing.Token)
		}
		want, err := DeriveOutcome(SettleInput{
			Verification:      cmd.Verification,
			FailureDetail:     cmd.FailureDetail,
			QuorumUnavailable: cmd.QuorumUnavailable,
		})
		if err != nil {
			return fmt.Errorf("%w: settle command for %s: %s", ErrUnbackedOutcome, cmd.ID, err)
		}
		if cmd.Outcome != "" && cmd.Outcome != want {
			// Refused, not corrected. A caller that believes it
			// completed when the evidence says otherwise is a bug
			// that must surface at the point it happens; quietly
			// overwriting its claim would hide exactly the
			// unobserved-as-success bug this package exists to
			// prevent.
			return fmt.Errorf("%w: settle command for %s claims %q, but its own evidence yields %q - refusing to record a verdict the evidence does not support",
				ErrUnbackedOutcome, cmd.ID, cmd.Outcome, want)
		}
		if want.IsSuccess() && !existing.Verification.Matches() && !cmd.Verification.Matches() {
			return fmt.Errorf("%w: settle command for %s would record a completion with no matching target observation", ErrUnbackedOutcome, cmd.ID)
		}
		return nil

	case CommandAbort:
		if existing.Outcome != OutcomeInProgress {
			return fmt.Errorf("%w: abort command for record %s, which has already settled as %s", ErrDuplicateCommand, cmd.ID, existing.Outcome)
		}
		if cmd.Token != existing.Token {
			return fmt.Errorf("%w: abort command for %s presents token %d, but the record is at token %d - this command belongs to a superseded attempt",
				ErrStaleToken, cmd.ID, cmd.Token, existing.Token)
		}
		if strings.TrimSpace(cmd.AbortReason) == "" {
			return fmt.Errorf("migration: abort command for %s has no reason - an unexplained stop is not a record", cmd.ID)
		}
		if existing.OwnershipMoved() {
			return fmt.Errorf("%w: record %s has reached %s, so ownership has already moved to %s and 'return the guest to the source' is not an action this protocol defines - move it back with a new migration, or let this one finish",
				ErrAbortTooLate, cmd.ID, existing.Phase, existing.TargetID)
		}
		return nil
	}

	return fmt.Errorf("migration: command kind %q reached no handler", cmd.Kind)
}

// Apply validates a command against state and, if it passes, returns
// the new state with the command applied. It is the one entry point a
// raft FSM apply path needs: validate, mutate, return. A rejected
// command returns the ORIGINAL state, so a caller that ignores the
// error has not corrupted anything.
func Apply(st MigrationState, cmd Command) (MigrationState, error) {
	if err := ValidateCommand(&st, cmd); err != nil {
		return st, err
	}

	switch cmd.Kind {
	case CommandStart:
		rec := newRecord(cmd)
		return st.withMigration(rec), nil

	case CommandAdvance:
		existing, _ := st.Lookup(cmd.ID)
		res, err := Advance(existing, Transition{From: existing.Phase, To: cmd.To, Retry: cmd.Retry}, cmd.Token, cmd.AtUnix)
		if err != nil {
			return st, err
		}
		next := res.Record
		if res.Reason != "" {
			next.Detail = res.Reason
		}
		if cmd.Transfer != nil {
			next.Transfer = *cmd.Transfer
		}
		return st.withMigration(next), nil

	case CommandSettle:
		existing, _ := st.Lookup(cmd.ID)
		next := existing
		if cmd.Verification.Observed {
			rec, err := RecordVerification(next, cmd.Verification)
			if err != nil {
				return st, err
			}
			next = rec
		}
		outcome, err := DeriveOutcome(SettleInput{
			Verification:      cmd.Verification,
			FailureDetail:     cmd.FailureDetail,
			QuorumUnavailable: cmd.QuorumUnavailable,
		})
		if err != nil {
			return st, fmt.Errorf("%w: settle command for %s: %s", ErrUnbackedOutcome, cmd.ID, err)
		}
		detail := cmd.Detail
		if detail == "" {
			detail = settleDetail(outcome, cmd)
		}
		settled, err := Settle(next, outcome, cmd.Evidence, detail, cmd.AtUnix)
		if err != nil {
			return st, err
		}
		return st.withMigration(settled), nil

	case CommandAbort:
		existing, _ := st.Lookup(cmd.ID)
		reason := fmt.Sprintf("aborted by operator at %s: %s", existing.Phase, cmd.AbortReason)
		settled, err := Settle(existing, OutcomeAborted,
			[]Evidence{{Rule: "migration-abort", Detail: cmd.AbortReason, ObservedAtUnix: cmd.AtUnix}}, reason, cmd.AtUnix)
		if err != nil {
			return st, err
		}
		return st.withMigration(settled), nil
	}

	return st, fmt.Errorf("migration: command kind %q reached no handler", cmd.Kind)
}

// settleDetail writes a Detail that is true for the outcome it
// accompanies, so a record read without its Evidence slice still says
// something correct rather than nothing.
func settleDetail(outcome Outcome, cmd Command) string {
	switch {
	case outcome.IsSuccess():
		return fmt.Sprintf("target %s reported %s running with the expected dataset and address", cmd.Verification.ObservedOn, cmd.Guest)
	case outcome.IsFailure():
		return cmd.FailureDetail
	case outcome == OutcomeUnobserved:
		detail := "asked the target how the migration ended and got no usable answer"
		if cmd.Verification.QueryError != "" {
			detail += ": " + cmd.Verification.QueryError
		}
		return detail
	case outcome == OutcomeUnverified:
		return "no trustworthy observation was possible - quorum could not be established when the migration's completion came due"
	default:
		return cmd.Detail
	}
}
