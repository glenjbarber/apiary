package migration

import (
	"strings"
	"testing"
	"time"
)

// TestFenceHeldForEveryOutcome: the fence is the safety mechanism, so
// every outcome is asserted explicitly rather than derived.
func TestFenceHeldForEveryOutcome(t *testing.T) {
	cases := []struct {
		outcome Outcome
		held    bool
		why     string
	}{
		{OutcomeInProgress, true, "the migration is still running"},
		{OutcomeObservedComplete, false, "the guest is observed running on the target"},
		{OutcomeFailed, false, "the guest never left, or is back where it was"},
		{OutcomeAborted, false, "the operator asked for the guest to go back"},
		{OutcomeUnobserved, true, "we do not know where the guest is, so we must not resume it on a guess"},
		{OutcomeUnverified, true, "we never got standing to find out, so we must not resume it on a guess"},
	}
	for _, tc := range cases {
		t.Run(string(tc.outcome), func(t *testing.T) {
			if got := FenceHeld(Record{Outcome: tc.outcome}); got != tc.held {
				t.Errorf("FenceHeld(%s) = %v, want %v (%s)", tc.outcome, got, tc.held, tc.why)
			}
		})
	}
}

// TestUnobservedCompletionKeepsTheFence is the one non-obvious case,
// asserted through the whole settle path rather than on a bare struct:
// a migration whose completion was never observed must leave the guest
// fenced, because unfencing on an unknown is how two live copies of one
// guest's disk happen.
func TestUnobservedCompletionKeepsTheFence(t *testing.T) {
	cut := advanceTo(t, newRecordAt("mig-unobserved", baseUnix), PhaseCutover)
	at := baseUnix + 600

	// The target never answered.
	unobs, err := Settle(cut, OutcomeUnobserved, nil, "dial to comb-b: i/o timeout", at)
	requireNoError(t, err, "settle unobserved")
	if !unobs.IsTerminal() {
		t.Error("an unobserved settle is not terminal; it will keep changing under the operator")
	}
	if !FenceHeld(unobs) {
		t.Fatal("an unobserved completion released the fence - this is the split-brain the fence exists to prevent")
	}
	d := EvaluateFence(testGuest(), &unobs)
	if !d.Blocked {
		t.Fatal("the reconciler was told it may touch an unobserved guest")
	}
	if !strings.Contains(d.Detail, "never observed") || !strings.Contains(d.Detail, "abort the migration or observe the target") {
		t.Errorf("unobserved fence detail does not tell the operator what to do: %q", d.Detail)
	}

	// Same for the unverified case.
	unver, err := Settle(cut, OutcomeUnverified, nil, "quorum could not be established", at)
	requireNoError(t, err, "settle unverified")
	if !FenceHeld(unver) {
		t.Error("an unverified completion released the fence")
	}

	// And an observed completion does release it.
	rec, err := RecordVerification(cut, matchingVerification(at))
	requireNoError(t, err, "record verification")
	ok, err := Settle(rec, OutcomeObservedComplete, observedEvidence(at), "target reported running", at)
	requireNoError(t, err, "settle observed complete")
	if FenceHeld(ok) {
		t.Error("an observed completion kept the fence; the guest would stay unfreeable forever")
	}
	if d := EvaluateFence(testGuest(), &ok); d.Blocked {
		t.Errorf("an observed completion still blocks the reconciler: %s", d.Detail)
	}
}

// TestNoRecordMeansNotFenced: the hundreds of existing guests that
// never migrate must be unaffected, so an absent record is the
// un-blocked case. A default that fails closed on every existing guest
// is not shippable, and ADR-0128 says the same thing about the proto
// bool.
func TestNoRecordMeansNotFenced(t *testing.T) {
	d := EvaluateFence(testGuest(), nil)
	if d.Blocked {
		t.Errorf("a guest with no migration record is blocked: %s", d.Detail)
	}
	if d.RecordID != "" {
		t.Errorf("an unblocked decision named record %q", d.RecordID)
	}
}

// TestFenceRefusesToActOnAnotherGuestsRecord: a corrupt index must not
// be able to block an innocent guest, and must not be able to pass one
// guest's fence off as another's.
func TestFenceRefusesToActOnAnotherGuestsRecord(t *testing.T) {
	r := newRecordAt("mig-other", baseUnix)
	other := Workload{Kind: WorkloadKindJail, ID: "some-jail"}

	d := EvaluateFence(other, &r)
	if !d.Blocked {
		t.Error("a record for one guest did not block a look-up for another; the index is corrupt and must fail closed")
	}
	if !strings.Contains(d.Detail, "belongs to") {
		t.Errorf("mismatched-guest fence detail does not say so: %q", d.Detail)
	}

	// Same ID, different kind, is also a different guest.
	sameID := Workload{Kind: WorkloadKindJail, ID: testVM}
	if d := EvaluateFence(sameID, &r); !d.Blocked {
		t.Error("a jail with a VM's id was treated as the same guest")
	}
}

// TestFenceDetailNamesTheStallWhenThereIsOne: a stalled record must say
// why it stalled, not merely that it is stalled.
func TestFenceDetailNamesTheStallWhenThereIsOne(t *testing.T) {
	r := advanceTo(t, newRecordAt("mig-fence-stall", baseUnix), PhaseBulk)
	stalled, err := Stall(r, Stalled{Reason: "target-unreachable", Detail: "dial to comb-b timed out after 5s"}, baseUnix+200)
	requireNoError(t, err, "stall")
	d := EvaluateFence(testGuest(), &stalled)
	if !d.Blocked {
		t.Fatal("a stalled record did not block the reconciler")
	}
	if !strings.Contains(d.Detail, "target-unreachable") || !strings.Contains(d.Detail, "timed out") {
		t.Errorf("stall fence detail does not carry the reason: %q", d.Detail)
	}
	if d.RecordID != "mig-fence-stall" {
		t.Errorf("fence named record %q, want mig-fence-stall", d.RecordID)
	}
	if d.Rule != RuleFencedGuest {
		t.Errorf("fence rule = %q, want %q", d.Rule, RuleFencedGuest)
	}
}

// TestFreezeStatusAndBoundedGuarantee: ADR-0128's bounded-freeze rule,
// including the case where the guest was never frozen at all, which
// must not read as a zero-length freeze.
func TestFreezeStatusAndBoundedGuarantee(t *testing.T) {
	budget := 15 * time.Minute

	// Never frozen.
	pre := newRecordAt("mig-nofreeze", baseUnix)
	elapsed, reached, exceeded := FreezeStatus(pre, baseTime.Add(time.Hour), budget)
	if reached {
		t.Error("a record still in preflight reported the guest as frozen")
	}
	if exceeded {
		t.Error("a never-frozen guest reported an exceeded budget")
	}
	if elapsed != 0 {
		t.Errorf("never-frozen elapsed = %v, want 0", elapsed)
	}
	if expired, _ := FreezeExpired(pre, baseTime.Add(time.Hour), budget); expired {
		t.Error("FreezeExpired fired for a guest that was never frozen")
	}

	// Frozen, inside budget.
	frozen := advanceTo(t, pre, PhaseBulk)
	now := time.Unix(frozen.Timestamps.FirstEnteredUnix[PhaseFreeze], 0).UTC().Add(5 * time.Minute)
	elapsed, reached, exceeded = FreezeStatus(frozen, now, budget)
	if !reached || exceeded {
		t.Errorf("5 minutes into a 15 minute freeze: reached=%v exceeded=%v", reached, exceeded)
	}
	if elapsed != 5*time.Minute {
		t.Errorf("elapsed = %v, want 5m", elapsed)
	}
	if expired, note := FreezeExpired(frozen, now, budget); expired {
		t.Errorf("FreezeExpired fired inside the budget (note %q)", note)
	} else if !strings.Contains(note, "of a 15m0s budget") {
		t.Errorf("inside-budget note = %q, want it to report progress against the budget", note)
	}

	// Frozen, over budget.
	late := time.Unix(frozen.Timestamps.FirstEnteredUnix[PhaseFreeze], 0).UTC().Add(20 * time.Minute)
	expired, note := FreezeExpired(frozen, late, budget)
	if !expired {
		t.Error("FreezeExpired did not fire at 20 minutes of a 15 minute freeze")
	}
	if !strings.Contains(note, "bounded-freeze") || !strings.Contains(note, "abort") {
		t.Errorf("over-budget note does not say what to do: %q", note)
	}

	// A zero budget falls back to the documented default rather than
	// firing immediately - a caller that forgot to pass a budget must
	// not get an instant abort.
	if expired, _ := FreezeExpired(frozen, late, 0); !expired {
		t.Error("a zero budget did not fall back to the default")
	}
	if expired, _ := FreezeExpired(frozen, time.Unix(frozen.Timestamps.FirstEnteredUnix[PhaseFreeze], 0).UTC().Add(time.Minute), 0); expired {
		t.Error("a zero budget aborted after one minute instead of using the default")
	}
}

// TestCutoverDowntimeIsMeasuredNotPromised: ADR-0128 is explicit that
// the cutover window is bounded but non-zero. The measurement reports
// "not yet known" until the target actually confirms.
func TestCutoverDowntimeIsMeasuredNotPromised(t *testing.T) {
	pre := advanceTo(t, newRecordAt("mig-downtime", baseUnix), PhaseSync)
	cutAt := baseUnix + 4*stepSeconds
	cut, err := Advance(pre, Transition{From: PhaseSync, To: PhaseCutover}, pre.Token+100, cutAt)
	requireNoError(t, err, "advance to cutover")
	r := cut.Record

	// Cutover not yet reached at all.
	if _, ok := CutoverDowntime(pre, baseTime); ok {
		t.Error("a record that has not cut over reported a downtime measurement")
	}

	// Cutover reached, target has not confirmed: elapsed but not a
	// measurement, because the outage is not known to have ended.
	open := time.Unix(cutAt, 0).UTC().Add(7 * time.Second)
	d, ok := CutoverDowntime(r, open)
	if ok {
		t.Error("an unconfirmed cutover reported a closed downtime measurement")
	}
	if d != 7*time.Second {
		t.Errorf("open downtime = %v, want the 7s elapsed so far", d)
	}

	// Target confirms: now it is a real measurement.
	confirmedAt := cutAt + 3
	rec, err := RecordVerification(r, matchingVerification(confirmedAt))
	requireNoError(t, err, "record verification")
	d, ok = CutoverDowntime(rec, time.Unix(confirmedAt+1000, 0).UTC())
	if !ok {
		t.Error("a confirmed cutover did not report a measurement")
	}
	if d != 3*time.Second {
		t.Errorf("confirmed downtime = %v, want 3s (cutover entry to the target's own report)", d)
	}
}

// TestFrozenReportsAnOutageNotJustBusy: a record past preflight is
// costing the guest its uptime, and the predicate that says so is the
// one the bounded-freeze check hangs off.
func TestFrozenReportsAnOutageNotJustBusy(t *testing.T) {
	r := newRecordAt("mig-frozen", baseUnix)
	if r.Frozen() {
		t.Error("a record in preflight reported the guest as frozen")
	}
	for _, p := range []Phase{PhaseFreeze, PhaseBulk, PhaseSync, PhaseCutover, PhaseTeardown} {
		at := advanceTo(t, r, p)
		if !at.Frozen() {
			t.Errorf("a record at %s does not report the guest as frozen", p)
		}
	}
}
