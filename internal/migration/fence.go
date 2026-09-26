package migration

import (
	"fmt"
	"time"
)

// FenceHeld reports whether the ordinary reconcilers must leave this
// guest alone right now.
//
// This is the whole safety mechanism in one predicate, and the case
// that matters is the third one. A record in flight fences the guest.
// A record that has settled as observed_complete, failed, or aborted
// does not - the guest is either owned elsewhere or back where it
// started. A record whose completion was NEVER OBSERVED still fences,
// and that is deliberate:
//
// Unfencing on an unknown is exactly how you get two live copies. If
// the source reconciler resumes the guest while the record says the
// cutover may or may not have happened, and the target has in fact
// already provisioned and started it, you now have one guest's disk
// running on two kernels - which no quorum check can detect, because
// both sides are each individually doing the right thing with the log
// they hold. ADR-0128's recovery rule is "a half-migrated guest must
// never silently exist in two places, and must never silently exist in
// neither", and keeping the fence on an unknown is how "in two places"
// is prevented. Not being able to un-fence automatically is a real
// operational cost and is the intended trade: the operator resolves it
// with Abort (before cutover) or by observing the target (after it),
// and until one of those happens the guest's state is genuinely
// unknown to the control plane and the control plane says so.
func FenceHeld(r Record) bool {
	if r.Outcome == OutcomeInProgress {
		return true
	}
	return r.Outcome.IsCompletionNeverObserved()
}

// FenceDecision is the answer the ordinary reconciler acts on: may I
// start, stop, or reclaim this guest, and if not, why not.
type FenceDecision struct {
	// Blocked is true when the reconciler must not touch the guest.
	Blocked bool
	// RecordID is the live migration responsible, for the log line and
	// the operator's UI. Empty only when Blocked is false.
	RecordID string
	// Rule is a stable id, in the internal/guardrail.Finding.Rule
	// convention, so a UI can key copy off it without string-matching
	// Detail.
	Rule   string
	Detail string
}

// RuleFencedGuest is the stable id of the fence finding.
const RuleFencedGuest = "migration-fence"

// EvaluateFence is the check an ordinary reconciler makes at the top of
// its per-guest work. It is a pure function over the guest's live
// record, so a reconciler that cannot read the record at all must
// decide separately - and the honest answer there is to fail closed and
// say so, which is what the caller does with a read error. This
// function is only ever handed a record that was actually read, or nil
// for a guest that genuinely has none.
func EvaluateFence(g Workload, r *Record) FenceDecision {
	if r == nil {
		// No record at all is the overwhelmingly common case: the
		// hundreds of existing guests that never migrate. It must
		// therefore be the un-blocked case, or this feature would
		// fail closed for every guest in the cluster.
		return FenceDecision{Blocked: false, Rule: RuleFencedGuest}
	}
	if r.Guest != g {
		// A record for some other guest must never fence this one. The
		// guest index (statewire.go) is supposed to guarantee this, but
		// a corrupt or hand-edited index is exactly the case a
		// belt-and-braces check exists for, and the cost of being wrong
		// in this direction is one guest refusing to start.
		return FenceDecision{
			Blocked: true,
			Rule:    RuleFencedGuest,
			Detail: fmt.Sprintf("migration record %s belongs to %s, not %s - refusing to let one guest's migration block another",
				r.ID, r.Guest, g),
		}
	}
	if !FenceHeld(*r) {
		return FenceDecision{
			Blocked: false,
			Rule:    RuleFencedGuest,
			Detail:  fmt.Sprintf("migration %s settled as %s", r.ID, r.Outcome.Verdict()),
		}
	}

	detail := fmt.Sprintf("%s is migrating from %s to %s (phase %s, outcome %q)", g, r.SourceID, r.TargetID, r.Phase, r.Outcome)
	switch {
	case r.Outcome.IsCompletionNeverObserved():
		detail += " - the migration's completion was never observed, so the guest stays fenced rather than being resumed on a guess; " +
			"abort the migration or observe the target"
	case r.Stall != nil:
		detail += fmt.Sprintf(" - stalled (%s: %s)", r.Stall.Reason, r.Stall.Detail)
	}
	return FenceDecision{Blocked: true, RecordID: r.ID, Rule: RuleFencedGuest, Detail: detail}
}

// DefaultFreezeTimeout is the bounded-freeze guarantee ADR-0128 calls
// non-negotiable: a frozen guest is an outage, and an unbounded outage
// is a different incident from a slow link. It is a default rather than
// a hardcoded constant at the call site so a caller can pass the
// operator's own number, which is ADR-0128's own open question 1 - this
// package will not decide that for them, but it will refuse to let a
// migration run without one.
const DefaultFreezeTimeout = 15 * time.Minute

// FreezeStatus answers the bounded-freeze question: how long has this
// guest been frozen, and has that exceeded the budget?
//
// ok=false with reached=false means the guest was never frozen, which
// is the normal state for a record still in preflight. It is reported
// as its own case rather than a zero duration so a caller cannot
// mistake "never frozen" for "frozen for no time at all".
func FreezeStatus(r Record, now time.Time, budget time.Duration) (elapsed time.Duration, reached bool, exceeded bool) {
	d, ok := r.Timestamps.Since(PhaseFreeze, now)
	if !ok {
		return 0, false, false
	}
	return d, true, d > budget
}

// FreezeExpired is the one-line form: should this migration be aborted
// and the source unfrozen right now because the guest has been frozen
// too long? It is a question, not an action - deciding to abort is the
// migration reconciler's call, and doing it is an operator-visible,
// destructive step.
func FreezeExpired(r Record, now time.Time, budget time.Duration) (bool, string) {
	if budget <= 0 {
		budget = DefaultFreezeTimeout
	}
	elapsed, reached, exceeded := FreezeStatus(r, now, budget)
	switch {
	case !reached:
		return false, ""
	case exceeded:
		return true, fmt.Sprintf("guest has been frozen for %s, over the %s budget - the ADR's bounded-freeze guarantee says abort and resume the source rather than keep an unbounded outage",
			elapsed.Round(time.Second), budget)
	default:
		return false, fmt.Sprintf("guest has been frozen for %s of a %s budget", elapsed.Round(time.Second), budget)
	}
}

// CutoverDowntime is the guest's real, measured outage: from the moment
// the ownership swap was committed to the moment the target reported a
// running guest, or to right now if that has not happened.
//
// ADR-0128 is explicit that this window is bounded but non-zero and
// that the frontend must not promise otherwise. Returning a real
// measurement rather than a field somebody can set to zero is the
// point; ok=false means "the cutover has not been observed to complete
// yet", and the caller must render that as unknown downtime rather than
// as a running total.
func CutoverDowntime(r Record, now time.Time) (d time.Duration, ok bool) {
	elapsed, reached := r.Timestamps.Since(PhaseCutover, now)
	if !reached {
		return 0, false
	}
	entry, _ := r.Timestamps.FirstEnteredUnix[PhaseCutover]
	if !r.Verification.Matches() {
		// Still open. Report what has elapsed so far but say it is not
		// yet a measurement, because the outage is not yet known to
		// have ended.
		return elapsed, false
	}
	// The measurement ends at the moment the target reported the guest
	// running, not at the moment this function is called: the outage
	// ended when the guest came back, and a figure that keeps growing
	// with every poll is not a downtime measurement at all.
	return time.Unix(r.Verification.ObservedAt, 0).Sub(time.Unix(entry, 0)), true
}
