package colonyupdate

import (
	"context"
	"time"
)

// Inert is the only Controller implementation in this branch, and it is
// NOT production. It is here for exactly two reasons: so the page
// compiles and renders, and so that a live page can be seen to say out
// loud that no update system is attached.
//
// Read this before mistaking it for anything else:
//
//   - It never nominates a Comb. NominatedNodeID is always "", which
//     means the page enables NO Comb's update control at all. That is
//     not a rendering bug; it is the honest consequence of having
//     nothing to derive a target from.
//   - It never reports an operation. Phase is always
//     OutcomeUnobserved and TargetNodeID is always "".
//   - It never starts an update. RequestColonyUpdate returns
//     ErrNoImplementation, every time, for every Comb, for every caller.
//   - It does not enforce colony-wide single-flight, because there is
//     nothing to enforce. BackendEnforced is false and EnforcedDetail
//     says so in the operator's own words on the page.
//
// The page this backs is INERT. It cannot start a real update, and it
// cannot pretend to. The real adapter is another branch's job: it
// implements Controller over the durable, Raft-backed colony-wide
// single-flight state, and cmd/frontend wires it in through
// frontend.Server.SetColonyUpdateController. Until that lands, the page
// is a read-only report of its own disconnection.
//
// This type is named rather than left anonymous so that every call site
// that can reach it says `colonyupdate.Inert{}` and a reader can grep
// for exactly what is missing.
type Inert struct{}

// Compile-time proof that the inert implementation really is a
// Controller, so the day it stops satisfying the interface the build
// says so here rather than at a call site.
var _ Controller = Inert{}

// ColonyUpdateState always reports that nothing was observed.
//
// The reported State is fully populated with the honest values rather
// than left zero-valued, because a zero State would render as an
// operation with no phase, no target and no detail - which reads as a
// working system that has nothing to do, rather than as a disconnected
// one. Every field the UI reads is stated here on purpose.
func (Inert) ColonyUpdateState(context.Context) (State, error) {
	return State{
		Observed:          false,
		UnavailableReason: "no Colony update system is attached to this frontend, so nothing about an update can be read",
		Phase:             OutcomeUnobserved,
		Detail: "this frontend has no Colony update system attached. The page below is inert: it reports its own " +
			"disconnection and nothing else, and it cannot start an update. The real implementation is a separate, " +
			"unfinished piece of work (ADR-0145).",

		// No nomination, and the reason it is absent is stated rather
		// than implied by a blank field.
		NominatedNodeID: "",
		NominatedDetail: "no Comb can be offered for update, because nothing is attached that could derive one",

		// Explicitly NOT enforced, so the page cannot imply that its own
		// disabled buttons are protecting the Colony.
		BackendEnforced: false,
		EnforcedDetail: "nothing is enforcing colony-wide single-flight, because there is no update system to " +
			"enforce it. The greyed-out controls on this page are a consequence of there being no target, not a " +
			"safety mechanism, and they refuse nothing.",

		// Zero time rather than time.Now(): a start time is a fact about
		// an operation, and there is no operation. The UI renders the
		// zero time as unobserved, never as a reading taken in 1970.
		StartedAt: time.Time{},
		Steps:     nil,
	}, nil
}

// RequestColonyUpdate always fails with ErrNoImplementation.
//
// It does not consult req.NodeID at all. There is no order it could
// honour even in principle: the whole point of the real implementation
// is that the system derives the order at execution time, and this
// derives nothing. Returning an error rather than a zero State is the
// honest answer, and it is the reason the page's own POST path has a
// refusal branch that is exercised by a fake in the tests.
func (Inert) RequestColonyUpdate(context.Context, Request) (State, error) {
	return State{}, ErrNoImplementation
}
