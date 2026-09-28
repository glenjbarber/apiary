// Package colonyupdate is the UI's entire dependency on the controlled
// Colony update workflow (ADR-0145).
//
// It exists because the workflow itself is being built on another branch
// and does not exist yet. Rather than guess that branch's final shape,
// stub it out as if it did, or push durable single-flight state into the
// frontend, this package declares the smallest honest surface the UI
// needs and nothing more:
//
//   - "what does the Colony know about the update right now?"
//   - "the operator asked to update this one Comb."
//
// Everything the page renders is derived from the answers to those two
// questions. The frontend never derives update state itself, never
// remembers what it showed last time, and never decides on its own that
// an update is allowed. See internal/frontend/colony_update.go.
//
// # Why the surface is this small
//
// Two reasons, both deliberate.
//
// First, ADR-0145's load-bearing point is that "UI state is advisory.
// The backend independently enforces colony-wide single-flight." If this
// interface grew a method like CanUpdate(nodeID) or IsUpdateRunning(),
// the frontend would start making the decision the backend is supposed to
// make, and the greyed-out button would quietly become the enforcement
// mechanism. A disabled <button> is a courtesy to the operator. A second
// tab, a stale page and a curl must all be refused by whatever implements
// Controller, not by anything in this package or its caller.
//
// Second, the real implementation is another branch's work, and this
// surface has to survive that landing as a thin, mechanical change. The
// natural home for the real adapter is internal/restartplan, which
// already owns ADR-0125's quorum arithmetic, the reachability probe, the
// durable result record and the leadership step-aside, and whose own
// Outcome vocabulary maps onto this package's Outcome nearly one for one
// (see Outcome's doc comment). Adapting should mean writing one type
// that implements Controller over restartplan's durable state and
// calling SetColonyUpdateController, not reshaping this package.
//
// # The inert implementation
//
// Inert returns a State that is honestly nothing, and RequestColonyUpdate
// always refuses. It exists so the page compiles and renders, and so a
// reader of a live UI can see unmistakably that no update system is
// attached. It is NOT a stub pretending to work: it never reports a
// nomination, never reports a phase beyond the unobserved one, and can
// never start an update.
package colonyupdate

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// Outcome names what is known about the Colony update operation as a
// whole, or about one step of it.
//
// These are deliberately distinct words with a distinct meaning each,
// and the two at the end are the ones this type exists to protect. A
// caller must never render OutcomeUnknown or OutcomeUnobserved as
// success, and must never render either as failure: "nobody could tell
// us" is a real answer that carries no information about whether
// anything worked.
//
// Note on the count: the operation and its steps share this one enum
// rather than having two near-identical ones, so a step's outcome and
// the operation's phase can never drift into disagreeing vocabularies.
type Outcome string

const (
	// OutcomeInProgress means the operation is under way right now:
	// something is happening and the next observation will tell us
	// more. It is not a promise that it will succeed.
	OutcomeInProgress Outcome = "in-progress"

	// OutcomeConfirmedComplete means the operation (or step) is
	// finished AND its confirmation evidence was actually read. This
	// is the only outcome that may be rendered as success, matching
	// restartplan.OutcomeConfirmed - whose doc comment names the same
	// restriction, and whose own ResultStore.Save already refuses to
	// persist an unbacked one.
	OutcomeConfirmedComplete Outcome = "confirmed-complete"

	// OutcomeBlocked means the update was deliberately refused and
	// nothing was attempted: a health gate, a quorum preflight, or the
	// colony-wide single-flight. Distinct from OutcomeFailed, because
	// nothing went wrong - the system said no on purpose, which is the
	// recoverable outcome ADR-0145 calls for.
	OutcomeBlocked Outcome = "blocked"

	// OutcomeFailed means there is positive evidence the operation (or
	// step) did not succeed. A failure is a thing that was observed,
	// and it is therefore the only outcome that may be rendered as
	// failure.
	OutcomeFailed Outcome = "failed"

	// OutcomeUnknown means an attempt was made, the answer came back,
	// and it does not settle the question. Distinct from
	// OutcomeUnobserved: this is a reading we have, it just is not
	// conclusive.
	OutcomeUnknown Outcome = "unknown"

	// OutcomeUnobserved means nothing was learned at all: the
	// question was not asked, the read failed, or the system holding
	// the answer is not attached. The direct analogue of
	// internal/cluster's replica_unobserved, and the honest resting
	// state of this page before a real implementation exists.
	OutcomeUnobserved Outcome = "unobserved"
)

// IsTerminal reports whether o is a settled answer - one that will not
// change on its own without a new update operation. Used by the UI to
// decide whether to keep polling, and never as a synonym for success.
func (o Outcome) IsTerminal() bool {
	switch o {
	case OutcomeConfirmedComplete, OutcomeBlocked, OutcomeFailed:
		return true
	default:
		return false
	}
}

// IsSuccess reports whether o may be rendered as "the update worked".
// Only OutcomeConfirmedComplete qualifies; see the type comment.
func (o Outcome) IsSuccess() bool { return o == OutcomeConfirmedComplete }

// IsInconclusive reports whether o carries no evidence either way, and
// so must never be rendered as success or as failure. Both
// OutcomeUnknown and OutcomeUnobserved qualify, and they stay
// distinguishable from each other by name wherever they are rendered.
func (o Outcome) IsInconclusive() bool {
	return o == OutcomeUnknown || o == OutcomeUnobserved
}

// Step is one stage of the Colony update operation, as far as the
// implementing system is willing to say out loud.
//
// Name identifies the stage to the operator in the implementing
// system's own words - it is displayed, not matched, so an adapter is
// free to change it freely.
//
// Outcome is the honest classification. An implementation that has not
// reached a stage should report OutcomeUnobserved for it rather than
// leaving it out, so the page can show the whole shape of the
// operation instead of only the part that went well.
//
// Detail is the operator-readable reason for that Outcome, and an
// implementation must not leave it empty for a settled outcome: an
// unexplained badge is the one thing an operator cannot act on, and an
// unbacked OutcomeConfirmedComplete is the exact thing this whole
// package is designed to make hard to write down.
//
// Evidence is the raw observation that backs Outcome, verbatim, when
// there is one - a build identity, a digest, a probe result. It is
// rendered as confirmation evidence and is never used to derive a
// verdict the implementer did not already reach.
type Step struct {
	Name     string
	Outcome  Outcome
	Detail   string
	Evidence string

	// ObservedAt is when this step's outcome was read. The zero time
	// means "not observed", which the UI renders as unobserved rather
	// than as a reading taken at the epoch.
	ObservedAt time.Time
}

// State is the whole of what the Colony knows about the update right
// now, as one read.
//
// Observed is the load-bearing field. When it is false there is no
// evidence at all, UnavailableReason says why, and every other field in
// this struct must be ignored - a State with Observed true and an
// OutcomeUnobserved phase is a genuine reading of "we asked and learned
// nothing", while Observed false means the question was never asked.
type State struct {
	// Observed reports whether the update system was reachable and
	// actually answered. See the field comment above.
	Observed bool

	// UnavailableReason explains a false Observed in one sentence, for
	// the operator. Required whenever Observed is false.
	UnavailableReason string

	// Phase is the operation-level outcome. OutcomeUnobserved when the
	// system answered but has no operation to report.
	Phase Outcome

	// Detail explains Phase, and must be non-empty whenever Observed is
	// true.
	Detail string

	// OperationID identifies this run, so an operator looking at two
	// browser tabs can tell whether they are watching the same
	// operation. Empty when there is none.
	OperationID string

	// TargetNodeID is the Comb currently being updated, or the Comb an
	// operation is aimed at. Empty when idle. The UI renders this
	// value, never a node ID submitted by a browser.
	TargetNodeID string

	// RequestedBy is who asked, as the update system recorded it.
	RequestedBy string

	// StartedAt is when the operation began. Zero means not observed.
	StartedAt time.Time

	// NominatedNodeID is the ONE Comb the update system will step on
	// if an update were requested right now, derived at read time
	// rather than stored as a plan - ADR-0145's "leadership is a
	// derived fact, re-derived at execution time". Empty means the
	// system is offering no Comb at the moment (a different Comb is
	// mid-update, the health gate is not met, or no implementation is
	// attached). The UI enables exactly this Comb's control and
	// disables every other, which is why an implementation that cannot
	// yet derive a target must return "" rather than a guess.
	NominatedNodeID string

	// NominatedDetail explains, in one sentence, why this Comb is (or
	// is not) the one on offer. Required whenever Observed is true,
	// because an enabled button whose reason is invisible is an
	// operator being asked to trust the page.
	NominatedDetail string

	// SingleFlightHolder names what is holding the colony-wide update
	// right now, or is empty when nothing is. It is shown so a second
	// tab's operator can see that the refusal they just received had a
	// real holder, rather than a generic denial.
	SingleFlightHolder string

	// BackendEnforced reports that the component answering this
	// State independently refuses a concurrent update request, so the
	// greying-out of every other Comb on the page is a courtesy and not
	// the mechanism. The UI states this on the page verbatim, so it
	// must be set from the enforcement point's own knowledge and never
	// merely because the page happened to disable a button.
	//
	// It is false on the Inert implementation below, deliberately: the
	// inert page must not imply an enforcement that does not exist.
	BackendEnforced bool

	// EnforcedDetail names what enforces the single-flight, or why that
	// is unobserved, in one sentence.
	EnforcedDetail string

	// Steps are the operation's stages in execution order. Nil when
	// there is no operation to describe; a non-nil empty slice with
	// Phase set is a real observation of an operation with no readable
	// steps, which is different and is rendered differently.
	Steps []Step
}

// Request is the operator's intent, and nothing else.
//
// The operator supplies a Comb and their own name. They do not supply an
// order, a leader, a plan, a lease, or a force flag, because ADR-0145's
// requirement is that they never need to know which Comb is the Raft
// leader and never choose the update order. An implementation that finds
// itself wanting any of those is the implementation's problem, not a
// reason to widen this struct.
type Request struct {
	// NodeID is the Comb the operator clicked. Implementations must
	// verify it against current membership themselves.
	NodeID string

	// RequestedBy is the authenticated session's username, for the
	// record. It is never a routing input.
	RequestedBy string
}

// ErrNoImplementation is returned by every method of the Inert
// implementation. It exists so a caller can distinguish "there is no
// update system attached here" from "the update system is attached and
// said no", which are very different things to show an operator.
var ErrNoImplementation = errors.New("colonyupdate: no Colony update system is attached to this frontend")

// RefusedError is what RequestColonyUpdate returns when the backend
// refuses an operator's update request. It is a refusal, not a failure:
// nothing was attempted against the Colony, and the Colony is exactly as
// it was.
//
// This is the whole answer to ADR-0145's "a client that ignores the
// greyed-out buttons must still be refused". The frontend renders the
// Detail verbatim and re-reads state afterwards, rather than treating a
// refused POST as a failure of the page or optimistically showing what
// the browser asked for.
type RefusedError struct {
	// Detail is the backend's own reason, in its own words. Carried
	// verbatim so the operator reads the server's objection rather
	// than the UI's paraphrase of it.
	Detail string

	// Holder names what already holds the colony-wide update, when the
	// backend knows. Empty when the refusal has another cause.
	Holder string
}

func (e *RefusedError) Error() string {
	if e == nil {
		return "<nil>"
	}
	if e.Detail == "" {
		return "colonyupdate: the Colony update request was refused"
	}
	return "colonyupdate: the Colony update request was refused: " + e.Detail
}

// Refusal is a small helper for an implementation returning the error
// type, so call sites read as one expression.
func Refusal(holder, format string, args ...any) *RefusedError {
	detail := format
	if len(args) > 0 {
		detail = fmt.Sprintf(format, args...)
	}
	return &RefusedError{Detail: detail, Holder: holder}
}

// Controller is the entire dependency the update UI has on the update
// system. Two methods: read, and ask.
//
// Both return the resulting State rather than a bare error, on purpose.
// A caller that re-reads after acting has one code path and one thing
// to render; a caller that trusted the request it sent has two, and the
// second one is how a UI ends up showing success for something the
// server refused.
type Controller interface {
	// ColonyUpdateState reports what the Colony knows right now. It
	// must be a fresh read: the frontend calls it on every page render
	// and on every poll, and a cached answer would make a second tab
	// and a stale page disagree with the server.
	ColonyUpdateState(ctx context.Context) (State, error)

	// RequestColonyUpdate records the operator's intent to update
	// NodeID, and returns the state that resulted.
	//
	// It returns a *RefusedError when the backend declines - because
	// another update holds the colony, or because a health or quorum
	// gate said no, or because NodeID is not a current member. It
	// returns ErrNoImplementation when no system is attached. Any
	// other error is a genuine failure to reach or act, and the UI
	// renders it as an error rather than as a verdict.
	//
	// Implementations MUST enforce colony-wide single-flight here,
	// independently of anything a UI did. A disabled control is not
	// enforcement.
	RequestColonyUpdate(ctx context.Context, req Request) (State, error)
}
