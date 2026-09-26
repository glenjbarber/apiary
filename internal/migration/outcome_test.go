package migration

import (
	"strings"
	"testing"
)

// TestOutcomeFourWayDistinction is the requirement stated directly: an
// operator must be able to tell in-flight, known-good, known-bad, and
// completion-never-observed apart, and none of them may be rendered as
// another.
func TestOutcomeFourWayDistinction(t *testing.T) {
	observedAt := baseUnix + 300

	cases := []struct {
		name        string
		in          SettleInput
		want        Outcome
		wantSuccess bool
		wantFailure bool
		wantUnknown bool
	}{
		{
			name:        "observed running with matching identity is the only success",
			in:          SettleInput{Verification: matchingVerification(observedAt)},
			want:        OutcomeObservedComplete,
			wantSuccess: true,
		},
		{
			name:        "positive failure evidence is a failure",
			in:          SettleInput{FailureDetail: "zfs send exited 1: dataset is busy"},
			want:        OutcomeFailed,
			wantFailure: true,
		},
		{
			name:        "target answered and said the guest is not running is a failure",
			in:          SettleInput{Verification: Verification{Observed: true, GuestRunning: false, ObservedAt: observedAt, ObservedOn: testTarget}},
			want:        OutcomeFailed,
			wantFailure: true,
		},
		{
			name:        "target answered running but with the wrong dataset is a failure, not a success",
			in:          SettleInput{Verification: Verification{Observed: true, GuestRunning: true, ObservedGUID: "guid-other", ExpectedGUID: "guid-abc", ObservedIP: "10.0.0.5", ExpectedIP: "10.0.0.5", ObservedAt: observedAt}},
			want:        OutcomeFailed,
			wantFailure: true,
		},
		{
			name:        "tried to ask and got nothing is unobserved, not failure",
			in:          SettleInput{Verification: Verification{Observed: false, QueryError: "dial tcp comb-b: i/o timeout"}},
			want:        OutcomeUnobserved,
			wantUnknown: true,
		},
		{
			name:        "could not even ask is unverified, not failure",
			in:          SettleInput{QuorumUnavailable: true},
			want:        OutcomeUnverified,
			wantUnknown: true,
		},
		{
			name: "unavailable quorum outranks an unobserved query: we never got standing to ask",
			in: SettleInput{
				Verification:      Verification{Observed: false, QueryError: "dial timeout"},
				QuorumUnavailable: true,
			},
			want:        OutcomeUnverified,
			wantUnknown: true,
		},
		{
			name: "an operator abort is neither success nor failure",
			in:   SettleInput{Aborted: true, AbortReason: "maintenance window closed"},
			want: OutcomeAborted,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := DeriveOutcome(tc.in)
			requireNoError(t, err, tc.name)
			if got != tc.want {
				t.Fatalf("DeriveOutcome = %s, want %s", got, tc.want)
			}
			if got.IsSuccess() != tc.wantSuccess {
				t.Errorf("IsSuccess(%s) = %v, want %v", got, got.IsSuccess(), tc.wantSuccess)
			}
			if got.IsFailure() != tc.wantFailure {
				t.Errorf("IsFailure(%s) = %v, want %v", got, got.IsFailure(), tc.wantFailure)
			}
			if got.IsCompletionNeverObserved() != tc.wantUnknown {
				t.Errorf("IsCompletionNeverObserved(%s) = %v, want %v", got, got.IsCompletionNeverObserved(), tc.wantUnknown)
			}
			// The three-way checks must be mutually exclusive across
			// the whole vocabulary, not merely true for this case.
			n := 0
			for _, b := range []bool{got.IsSuccess(), got.IsFailure(), got.IsCompletionNeverObserved()} {
				if b {
					n++
				}
			}
			if got != OutcomeInProgress && got != OutcomeAborted && n != 1 {
				t.Errorf("outcome %s matched %d of {success, failure, unknown}; exactly one is required", got, n)
			}
		})
	}
}

// TestDeriveOutcomeRefusesAnUnbackedClaim: no evidence at all must be
// an error, never a default. The two default-shaped answers - "it
// worked" and "it failed" - are both lies, and a third default of
// "in progress" would make an abandoned migration look like a slow one.
func TestDeriveOutcomeRefusesAnUnbackedClaim(t *testing.T) {
	if _, err := DeriveOutcome(SettleInput{}); err == nil {
		t.Fatal("DeriveOutcome with no evidence at all returned a verdict; it must refuse")
	}
	if _, err := DeriveOutcome(SettleInput{Aborted: true}); err == nil {
		t.Fatal("an abort with no reason was accepted")
	}
}

// TestSettleToSuccessRequiresAMatchingObservation, at both layers: the
// Settle function and Record.Validate, which is what a decoded-from-
// wire record goes through.
func TestSettleToSuccessRequiresAMatchingObservation(t *testing.T) {
	r := advanceTo(t, newRecordAt("mig-success", baseUnix), PhaseCutover)

	// No verification recorded at all.
	if _, err := Settle(r, OutcomeObservedComplete, observedEvidence(baseUnix+400), "done", baseUnix+400); !errorsIs(err, ErrUnbackedOutcome) {
		t.Errorf("settling to success with no observation: err = %v, want ErrUnbackedOutcome", err)
	}

	// A verification that was observed but did not match.
	mismatch := matchingVerification(baseUnix + 400)
	mismatch.ObservedGUID = "guid-someone-else"
	rec, err := RecordVerification(r, mismatch)
	requireNoError(t, err, "record mismatched verification")
	if _, err := Settle(rec, OutcomeObservedComplete, observedEvidence(baseUnix+400), "done", baseUnix+400); !errorsIs(err, ErrUnbackedOutcome) {
		t.Errorf("settling to success on a mismatched observation: err = %v, want ErrUnbackedOutcome", err)
	}

	// And a record that claims success without the observation is
	// refused by Validate itself, so it cannot even be constructed
	// from a wire value.
	forged := r
	forged.Outcome = OutcomeObservedComplete
	forged.Detail = "done"
	if err := forged.Validate(); err == nil {
		t.Error("a record claiming success with no matching observation passed Validate")
	}

	// The honest path works.
	observedAt := baseUnix + 400
	rec, err = RecordVerification(r, matchingVerification(observedAt))
	requireNoError(t, err, "record matching verification")
	settled, err := Settle(rec, OutcomeObservedComplete, observedEvidence(observedAt), "target reported the guest running", observedAt)
	requireNoError(t, err, "settle to success")
	if !settled.Outcome.IsSuccess() {
		t.Fatalf("settled outcome = %s, want success", settled.Outcome)
	}
	if settled.Timestamps.SettledUnix != observedAt {
		t.Errorf("settled at %d, want %d", settled.Timestamps.SettledUnix, observedAt)
	}
	requireNoError(t, settled.Validate(), "settled record")
}

// TestSettleRequiresEvidenceForThePositiveOutcomes: an unbacked
// verdict is not a record, in either direction.
func TestSettleRequiresEvidenceForThePositiveOutcomes(t *testing.T) {
	r := newRecordAt("mig-evidence", baseUnix)
	observedAt := baseUnix + 50
	rec, err := RecordVerification(r, matchingVerification(observedAt))
	requireNoError(t, err, "record verification")

	if _, err := Settle(rec, OutcomeObservedComplete, nil, "done", observedAt); !errorsIs(err, ErrUnbackedOutcome) {
		t.Errorf("success with no evidence: err = %v, want ErrUnbackedOutcome", err)
	}
	if _, err := Settle(rec, OutcomeFailed, nil, "it broke", observedAt); !errorsIs(err, ErrUnbackedOutcome) {
		t.Errorf("failure with no evidence: err = %v, want ErrUnbackedOutcome", err)
	}
	if _, err := Settle(rec, OutcomeAborted, nil, "", observedAt); !errorsIs(err, ErrUnbackedOutcome) {
		t.Errorf("abort with no reason: err = %v, want ErrUnbackedOutcome", err)
	}
	// The unknown outcomes need no evidence - their whole content is
	// that there is none - but they do need a detail.
	if _, err := Settle(rec, OutcomeUnobserved, nil, "", observedAt); err != nil {
		t.Errorf("unobserved with no evidence was rejected: %v", err)
	}
	if _, err := Settle(rec, OutcomeUnverified, nil, "quorum could not be established", observedAt); err != nil {
		t.Errorf("unverified with no evidence was rejected: %v", err)
	}
}

// TestSettleRefusesToReSettle is the replay defence at the record
// level: a second settle cannot rewrite a recorded failure into a
// recorded success.
func TestSettleRefusesToReSettle(t *testing.T) {
	r := newRecordAt("mig-resettle", baseUnix)
	at := baseUnix + 50
	failed, err := Settle(r, OutcomeFailed, failureEvidence("zfs recv failed", at), "zfs recv failed", at)
	requireNoError(t, err, "settle to failed")

	observedAt := at + 100
	rec, err := RecordVerification(r, matchingVerification(observedAt))
	requireNoError(t, err, "record verification on the pre-settle record")

	_, err = Settle(failed, OutcomeObservedComplete, observedEvidence(observedAt), "now it worked", observedAt)
	requireSentinel(t, err, ErrIllegalTransition, "re-settling a settled record")
	if !strings.Contains(err.Error(), "already settled as failed") {
		t.Errorf("re-settle error does not name the prior outcome: %v", err)
	}
	_ = rec
}

// TestSettleRefusesInProgressAndUnknownOutcomes: in_progress is not a
// settlement, and a string this build does not recognize is not a
// settlement either.
func TestSettleRefusesInProgressAndUnknownOutcomes(t *testing.T) {
	r := newRecordAt("mig-badsettle", baseUnix)
	at := baseUnix + 10
	if _, err := Settle(r, OutcomeInProgress, nil, "", at); !errorsIs(err, ErrIllegalTransition) {
		t.Errorf("settling to in_progress: err = %v, want ErrIllegalTransition", err)
	}
	if _, err := Settle(r, Outcome("probably_fine"), nil, "", at); !errorsIs(err, ErrIllegalTransition) {
		t.Errorf("settling to an unrecognized outcome: err = %v, want ErrIllegalTransition", err)
	}
}

// TestOutcomeVocabularyIsClosed: an unrecognized outcome is refused,
// never defaulted. Defaulting to in_progress would fence a guest
// forever; defaulting to anything terminal would unfence a guest we
// know nothing about.
func TestOutcomeVocabularyIsClosed(t *testing.T) {
	for _, o := range []Outcome{"", "success", "done", "ok", "complete", "in-flight", "unknown", "replica_unobserved"} {
		if o.Recognized() {
			t.Errorf("outcome %q was recognized", o)
		}
		if o.IsSuccess() || o.IsFailure() || o.IsCompletionNeverObserved() || o.IsTerminal() {
			t.Errorf("unrecognized outcome %q claimed a classification", o)
		}
	}
	for _, o := range []Outcome{OutcomeInProgress, OutcomeObservedComplete, OutcomeFailed, OutcomeUnobserved, OutcomeUnverified, OutcomeAborted} {
		if !o.Recognized() {
			t.Errorf("outcome %q was not recognized", o)
		}
	}
	// Only in_progress is live. Everything else, including a stalled
	// record, is terminal - and only in_progress is not.
	if OutcomeInProgress.IsTerminal() {
		t.Error("in_progress reported as terminal")
	}
	for _, o := range []Outcome{OutcomeObservedComplete, OutcomeFailed, OutcomeUnobserved, OutcomeUnverified, OutcomeAborted} {
		if !o.IsTerminal() {
			t.Errorf("%s reported as non-terminal", o)
		}
	}
}

// TestRenderNeverPresentsUnobservedAsSuccess is the requirement stated
// as a rendering test. Every settled record's one-line form is
// inspected: the unknown ones must say unknown, the failed one must say
// failed, and none of the three may contain a word a UI would colour
// green.
func TestRenderNeverPresentsUnobservedAsSuccess(t *testing.T) {
	r := newRecordAt("mig-render", baseUnix)
	at := baseUnix + 77

	// In flight.
	live := r.Render()
	if !strings.Contains(live, "in flight") {
		t.Errorf("in-flight render = %q", live)
	}

	// The three settled-without-success outcomes.
	for _, tc := range []struct {
		outcome  Outcome
		detail   string
		evidence []Evidence
		want     string
	}{
		{OutcomeUnobserved, "asked comb-b, no usable answer", nil, "UNKNOWN"},
		{OutcomeUnverified, "quorum could not be established", nil, "UNKNOWN"},
		{OutcomeFailed, "zfs recv failed", failureEvidence("zfs recv failed", at), "FAILED"},
	} {
		settled, err := Settle(r, tc.outcome, tc.evidence, tc.detail, at)
		requireNoError(t, err, "settle to "+string(tc.outcome))
		line := settled.Render()
		if !strings.Contains(line, tc.want) {
			t.Errorf("render for %s = %q, want it to contain %q", tc.outcome, line, tc.want)
		}
		if strings.Contains(strings.ToLower(line), "complete (") {
			t.Errorf("render for %s reads as a completion: %q", tc.outcome, line)
		}
		if tc.outcome.IsCompletionNeverObserved() {
			// Both halves of the honesty sentence, because a UI that
			// truncates must still not read as success.
			if !strings.Contains(line, "not a confirmed success") || !strings.Contains(line, "not a confirmed failure") {
				t.Errorf("unknown render is missing its disambiguation: %q", line)
			}
			if !strings.Contains(line, "stays fenced") {
				t.Errorf("unknown render does not say the guest stays fenced: %q", line)
			}
		}
	}

	// And the success render, which is the only one allowed to say so.
	rec, err := RecordVerification(r, matchingVerification(at))
	requireNoError(t, err, "record verification")
	ok, err := Settle(rec, OutcomeObservedComplete, observedEvidence(at), "target reported running", at)
	requireNoError(t, err, "settle to success")
	if !strings.Contains(ok.Render(), "complete (phase") {
		t.Errorf("success render = %q", ok.Render())
	}
}

// TestPhaseProgressIsNotCompletion: reaching the last phase reports
// 6/6 and is still in flight. A frontend that renders 6/6 as a
// progress bar at 100% is exactly how an unobserved migration gets
// displayed as a success.
func TestPhaseProgressIsNotCompletion(t *testing.T) {
	r := advanceTo(t, newRecordAt("mig-progress", baseUnix), PhaseTeardown)
	cur, total := r.PhaseProgress()
	if cur != 6 || total != 6 {
		t.Errorf("phase progress = %d/%d, want 6/6", cur, total)
	}
	if r.Outcome.IsSuccess() {
		t.Error("a record at the last phase claims success")
	}
	unknownPhase := Record{Phase: "nonsense"}
	if cur, total := unknownPhase.PhaseProgress(); cur != 0 || total != 6 {
		t.Errorf("unknown phase progress = %d/%d, want 0/6", cur, total)
	}
}
