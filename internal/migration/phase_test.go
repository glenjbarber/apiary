package migration

import (
	"strings"
	"testing"
	"time"
)

// TestEveryLegalForwardTransition walks the whole protocol once, one
// step at a time, asserting that each adjacent pair in PhaseOrder is
// accepted and that the resulting record is coherent. If this fails,
// the protocol cannot be run end to end at all.
func TestEveryLegalForwardTransition(t *testing.T) {
	r := newRecordAt("mig-1", baseUnix)
	if r.Phase != PhasePreflight {
		t.Fatalf("new record is at %s, want %s", r.Phase, PhasePreflight)
	}
	if r.Attempt != 1 {
		t.Errorf("new record attempt = %d, want 1", r.Attempt)
	}
	if r.Token != testToken1 {
		t.Errorf("new record token = %d, want %d", r.Token, testToken1)
	}

	// preflight -> freeze -> bulk -> sync -> cutover -> teardown
	for i := 1; i < len(PhaseOrder); i++ {
		from, to := PhaseOrder[i-1], PhaseOrder[i]
		token := []uint64{testToken2, testToken3, testToken4, testToken5, testToken6, testToken7}[i-1]
		at := baseUnix + int64(i)*stepSeconds

		if _, err := Classify(r, Transition{From: from, To: to}); err != nil {
			t.Fatalf("Classify(%s -> %s) rejected a legal step: %v", from, to, err)
		}
		res, err := Advance(r, Transition{From: from, To: to}, token, at)
		requireNoError(t, err, "Advance "+string(from)+"->"+string(to))
		if res.Record.Phase != to {
			t.Errorf("after advance, phase = %s, want %s", res.Record.Phase, to)
		}
		if res.Record.Attempt != uint32(i+1) {
			t.Errorf("after advance, attempt = %d, want %d", res.Record.Attempt, i+1)
		}
		// "now" for this test is after the last step, so each phase's
		// elapsed time is the distance from its own entry.
		now := time.Unix(baseUnix+int64(len(PhaseOrder))*stepSeconds, 0).UTC()
		wantSince := time.Duration(int64(len(PhaseOrder)-i)*stepSeconds) * time.Second
		if first, ok := res.Record.Timestamps.Since(to, now); !ok || first != wantSince {
			t.Errorf("phase %s first-entered = %v (ok=%v), want %v", to, first, ok, wantSince)
		}
		requireNoError(t, res.Record.Validate(), "record after "+string(to))
		r = res.Record
	}

	if r.Phase != PhaseTeardown {
		t.Fatalf("final phase = %s, want %s", r.Phase, PhaseTeardown)
	}
	// Reaching the last phase is emphatically not completion.
	if r.Outcome.IsSuccess() {
		t.Errorf("record at teardown claims success - entering the last phase must never imply it finished")
	}
	if !r.InFlight() {
		t.Errorf("record at teardown is not in flight (outcome %s)", r.Outcome)
	}
}

// TestForwardSkipIsIllegal is the hole-in-the-journal test. Every
// forward skip, from every source phase, must be rejected - and the
// error must name the skipped phase, because ADR-0128's operator-facing
// story depends on an operator being told which step was bypassed.
func TestForwardSkipIsIllegal(t *testing.T) {
	for fromIdx := range PhaseOrder {
		for toIdx := fromIdx + 2; toIdx < len(PhaseOrder); toIdx++ {
			from, to := PhaseOrder[fromIdx], PhaseOrder[toIdx]
			r := advanceTo(t, newRecordAt("mig-skip", baseUnix), from)

			_, err := Classify(r, Transition{From: from, To: to})
			if err == nil {
				t.Errorf("Classify(%s -> %s) was accepted; a forward skip must be illegal", from, to)
				continue
			}
			requireSentinel(t, err, ErrIllegalTransition, "skip "+string(from)+"->"+string(to))
			if !strings.Contains(err.Error(), "skipping") {
				t.Errorf("skip %s->%s error does not say it was skipping: %v", from, to, err)
			}
			for skipped := fromIdx + 1; skipped < toIdx; skipped++ {
				if !strings.Contains(err.Error(), string(PhaseOrder[skipped])) {
					t.Errorf("skip %s->%s error does not name the skipped phase %s: %v", from, to, PhaseOrder[skipped], err)
				}
			}

			// The rejected Advance must hand back the ORIGINAL record,
			// not a half-moved one.
			_, aerr := Advance(r, Transition{From: from, To: to}, testToken8, baseUnix+999)
			requireSentinel(t, aerr, ErrIllegalTransition, "Advance skipping "+string(from)+"->"+string(to))
			if verr := r.Validate(); verr != nil {
				t.Errorf("record was left incoherent by a rejected skip: %v", verr)
			}
		}
	}
}

// TestBackwardTransitionIsIllegal covers every rewinding move. The
// reasoning is in Transition's doc comment: nothing in the protocol
// moves backwards, and a rewind could re-open a window already closed.
func TestBackwardTransitionIsIllegal(t *testing.T) {
	for toIdx := range PhaseOrder {
		for fromIdx := toIdx + 1; fromIdx < len(PhaseOrder); fromIdx++ {
			from, to := PhaseOrder[fromIdx], PhaseOrder[toIdx]
			r := advanceTo(t, newRecordAt("mig-rewind", baseUnix), from)

			_, err := Classify(r, Transition{From: from, To: to})
			if err == nil {
				t.Errorf("Classify(%s -> %s) was accepted; a rewind must be illegal", from, to)
				continue
			}
			requireSentinel(t, err, ErrIllegalTransition, "rewind "+string(from)+"->"+string(to))
			if !strings.Contains(err.Error(), "rewind") {
				t.Errorf("rewind %s->%s error does not say it was a rewind: %v", from, to, err)
			}
		}
	}
}

// TestSelfTransitionIsIllegalWithoutRetry: re-entering the current
// phase is a retry or it is nothing. Accepting an unmarked self-move
// would let a duplicate delivery look like progress.
func TestSelfTransitionIsIllegalWithoutRetry(t *testing.T) {
	for _, p := range PhaseOrder {
		r := advanceTo(t, newRecordAt("mig-self", baseUnix), p)
		_, err := Classify(r, Transition{From: p, To: p, Retry: false})
		requireSentinel(t, err, ErrIllegalTransition, "unmarked self-move at "+string(p))
		if !strings.Contains(err.Error(), "retry") {
			t.Errorf("unmarked self-move at %s does not mention retry: %v", p, err)
		}
	}
}

// TestSelfTransitionIsLegalAsRetry, and its two load-bearing effects:
// a new token is required, and the FIRST-entered timestamp is preserved
// across the retry so a guest does not look less-frozen than it is.
func TestSelfTransitionIsLegalAsRetry(t *testing.T) {
	for i, p := range PhaseOrder {
		r := advanceTo(t, newRecordAt("mig-retry", baseUnix), p)
		firstBefore := r.Timestamps.FirstEnteredUnix[p]
		lastBefore := r.Timestamps.LastEnteredUnix[p]
		attemptBefore := r.Attempt
		tokenBefore := r.Token

		newToken := tokenBefore + 50
		retryAt := lastBefore + 5*stepSeconds

		reason, err := Classify(r, Transition{From: p, To: p, Retry: true})
		if err != nil {
			t.Fatalf("Classify(retry at %s) rejected a legal retry: %v", p, err)
		}
		if reason == "" {
			t.Errorf("retry at %s gave no reason for the caller to act on", p)
		}
		if !strings.Contains(reason, "resume") {
			t.Errorf("retry reason at %s does not tell the caller to resume: %q", p, reason)
		}

		res, err := Advance(r, Transition{From: p, To: p, Retry: true}, newToken, retryAt)
		requireNoError(t, err, "retry at "+string(p))
		got := res.Record
		if got.Phase != p {
			t.Errorf("after retry, phase = %s, want %s", got.Phase, p)
		}
		if got.Attempt != attemptBefore+1 {
			t.Errorf("after retry, attempt = %d, want %d", got.Attempt, attemptBefore+1)
		}
		if got.Token != newToken {
			t.Errorf("after retry, token = %d, want %d", got.Token, newToken)
		}
		if got.Timestamps.FirstEnteredUnix[p] != firstBefore {
			t.Errorf("retry overwrote the first-entered timestamp for %s: %d -> %d", p, firstBefore, got.Timestamps.FirstEnteredUnix[p])
		}
		if got.Timestamps.LastEnteredUnix[p] != retryAt {
			t.Errorf("after retry, last-entered for %s = %d, want %d", p, got.Timestamps.LastEnteredUnix[p], retryAt)
		}
		requireNoError(t, got.Validate(), "record after retrying "+string(p))
		_ = i
	}
}

// TestRetryWithANonIncreasingTokenIsRejected: the attempt token is the
// mechanism that makes a delayed report from a superseded attempt
// harmless, so a retry that reuses or lowers it must be refused.
func TestRetryWithANonIncreasingTokenIsRejected(t *testing.T) {
	r := advanceTo(t, newRecordAt("mig-token", baseUnix), PhaseBulk)
	for _, bad := range []uint64{0, 1, r.Token - 1, r.Token} {
		if _, err := Advance(r, Transition{From: PhaseBulk, To: PhaseBulk, Retry: true}, bad, baseUnix+1); !errorsIs(err, ErrStaleToken) {
			t.Errorf("retry at bulk with token %d: err = %v, want ErrStaleToken", bad, err)
		}
	}
	if _, err := Advance(r, Transition{From: PhaseBulk, To: PhaseBulk, Retry: true}, r.Token+1, baseUnix+1); err != nil {
		t.Errorf("retry at bulk with a higher token was rejected: %v", err)
	}
}

// TestSettledRecordRefusesPhaseMovement: a record that has settled is
// done moving. A late report from before the settle must not be able to
// drag it back into the protocol.
func TestSettledRecordRefusesPhaseMovement(t *testing.T) {
	r := advanceTo(t, newRecordAt("mig-settled", baseUnix), PhaseSync)
	observedAt := baseUnix + 500
	rec, err := RecordVerification(r, matchingVerification(observedAt))
	requireNoError(t, err, "record verification")
	settled, err := Settle(rec, OutcomeObservedComplete, observedEvidence(observedAt), "target reported the guest running", observedAt)
	requireNoError(t, err, "settle")

	_, err = Classify(settled, Transition{From: PhaseSync, To: PhaseCutover})
	requireSentinel(t, err, ErrIllegalTransition, "advancing a settled record")
	if !strings.Contains(err.Error(), "settled") {
		t.Errorf("advancing a settled record does not say it settled: %v", err)
	}
	if _, err := Advance(settled, Transition{From: PhaseSync, To: PhaseCutover}, settled.Token+100, observedAt+1); !errorsIs(err, ErrIllegalTransition) {
		t.Errorf("Advance on a settled record: err = %v, want ErrIllegalTransition", err)
	}
}

// TestStallRequiresAReasonAndDetail, and TestStallClearsOnAdvance -
// a stall is a named pause, not a hang, and the next real move must
// clear it.
func TestStallRequiresAReasonAndDetail(t *testing.T) {
	r := advanceTo(t, newRecordAt("mig-stall", baseUnix), PhaseBulk)

	if _, err := Stall(r, Stalled{Detail: "target unreachable"}, baseUnix+1); err == nil {
		t.Error("stall with no reason was accepted")
	}
	if _, err := Stall(r, Stalled{Reason: "target-unreachable"}, baseUnix+1); err == nil {
		t.Error("stall with a reason but no detail was accepted")
	}

	stalled, err := Stall(r, Stalled{Reason: "target-unreachable", Detail: "dial to comb-b timed out"}, baseUnix+100)
	requireNoError(t, err, "stall")
	if stalled.Stall == nil {
		t.Fatal("stall was not recorded")
	}
	if stalled.Stall.SinceUnix != baseUnix+100 {
		t.Errorf("stall since = %d, want %d", stalled.Stall.SinceUnix, baseUnix+100)
	}
	if stalled.Outcome != OutcomeInProgress {
		t.Errorf("a stalled record is %s, want it still in flight", stalled.Outcome)
	}
	if !strings.Contains(stalled.Render(), "stalled") {
		t.Errorf("render of a stalled record does not mention the stall: %s", stalled.Render())
	}
	requireNoError(t, stalled.Validate(), "stalled record")

	res, err := Advance(stalled, Transition{From: PhaseBulk, To: PhaseSync}, stalled.Token+1, baseUnix+200)
	requireNoError(t, err, "advance past a stall")
	if res.Record.Stall != nil {
		t.Errorf("advancing left a stale stall on the record: %+v", res.Record.Stall)
	}
}

// TestStallRefusedOnSettledRecord: there is nothing left to wait for.
func TestStallRefusedOnSettledRecord(t *testing.T) {
	r := advanceTo(t, newRecordAt("mig-stall-settled", baseUnix), PhaseCutover)
	observedAt := baseUnix + 900
	rec, err := RecordVerification(r, matchingVerification(observedAt))
	requireNoError(t, err, "record verification")
	settled, err := Settle(rec, OutcomeObservedComplete, observedEvidence(observedAt), "done", observedAt)
	requireNoError(t, err, "settle")
	_, err = Stall(settled, Stalled{Reason: "x", Detail: "y"}, observedAt+1)
	requireSentinel(t, err, ErrIllegalTransition, "stalling a settled record")
}

// TestRecordVerificationRefusesUnbackedObservation: recording "we did
// not observe" with no reason is how an unobserved migration becomes an
// unexplained one.
func TestRecordVerificationRefusesUnbackedObservation(t *testing.T) {
	r := newRecordAt("mig-verify", baseUnix)
	if _, err := RecordVerification(r, Verification{Observed: false}); err == nil {
		t.Error("an unobserved verification with no query error was accepted")
	}
	if _, err := RecordVerification(r, Verification{Observed: true, GuestRunning: true, ObservedAt: 0}); err == nil {
		t.Error("an observed verification with no observation time was accepted")
	}
	if _, err := RecordVerification(r, Verification{Observed: false, QueryError: "dial timeout"}); err != nil {
		t.Errorf("an unobserved verification with a query error was rejected: %v", err)
	}
}

// TestUpdateTransferClampsNothingSilently: an over-reported byte count
// is a named error, not a corrected number. A silently clamped counter
// is a lie that looks like data.
func TestUpdateTransferClampsNothingSilently(t *testing.T) {
	r := advanceTo(t, newRecordAt("mig-xfer", baseUnix), PhaseBulk)

	updated, err := UpdateTransfer(r, Transfer{RequiredBytes: 1000, MovedBytes: 400, FromToken: 7, ToToken: 9})
	requireNoError(t, err, "update transfer")
	if updated.Transfer.MovedBytes != 400 {
		t.Errorf("moved = %d, want 400", updated.Transfer.MovedBytes)
	}

	_, err = UpdateTransfer(r, Transfer{RequiredBytes: 1000, MovedBytes: 5000})
	if err == nil {
		t.Error("a byte count past the requirement was accepted")
	}
	if !strings.Contains(err.Error(), "progress cannot exceed") {
		t.Errorf("over-report error is not the named one: %v", err)
	}

	// A resume point behind the one it started from is a silent replay
	// of data the target already has, so it is refused too.
	if _, err := UpdateTransfer(r, Transfer{FromToken: 9, ToToken: 7}); err == nil {
		t.Error("a backwards resume token was accepted")
	}

	// A zero-byte bulk is legitimate and must not be read as a failure:
	// this is the HAST-replica path, where the copy is a genuine no-op.
	noop, err := UpdateTransfer(r, Transfer{RequiredBytes: 0, TargetAlreadyReplica: true})
	requireNoError(t, err, "no-op bulk transfer")
	if noop.Transfer.Complete() {
		t.Error("a zero-byte transfer reported Complete; RequiredBytes==0 means nothing was scheduled, not that it finished")
	}
}
