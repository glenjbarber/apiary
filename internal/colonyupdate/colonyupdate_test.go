package colonyupdate

import (
	"errors"
	"strings"
	"testing"
)

// Tests for the vocabulary and error contract this package exports.
//
// The interface itself has no logic to test - it is a type - but the
// Outcome predicates and the RefusedError carry real rules that a wrong
// implementation would violate silently. The rules are worth pinning here,
// where the whole package's honesty claim lives, rather than only at the
// call site in internal/frontend.

// allOutcomes is every Outcome constant, so a new one added later cannot
// be added without appearing in every test below.
func allOutcomes() []Outcome {
	return []Outcome{
		OutcomeInProgress,
		OutcomeConfirmedComplete,
		OutcomeBlocked,
		OutcomeFailed,
		OutcomeUnknown,
		OutcomeUnobserved,
	}
}

func TestOutcomeOnlyConfirmedIsSuccess(t *testing.T) {
	for _, outcome := range allOutcomes() {
		want := outcome == OutcomeConfirmedComplete
		if got := outcome.IsSuccess(); got != want {
			t.Errorf("%q.IsSuccess() = %v, want %v; only a confirmed-complete outcome may ever be rendered as success", outcome, got, want)
		}
	}
}

func TestOutcomeInconclusiveOutcomesAreNeitherSuccessNorFailure(t *testing.T) {
	// These two are the whole reason the type exists. An implementation
	// that rendered either as success, or as failure, would be lying to
	// an operator about a restart.
	for _, outcome := range []Outcome{OutcomeUnknown, OutcomeUnobserved} {
		if !outcome.IsInconclusive() {
			t.Errorf("%q.IsInconclusive() = false, want true", outcome)
		}
		if outcome.IsSuccess() {
			t.Errorf("%q is inconclusive but reports itself as a success", outcome)
		}
	}
	for _, outcome := range []Outcome{OutcomeInProgress, OutcomeConfirmedComplete, OutcomeBlocked, OutcomeFailed} {
		if outcome.IsInconclusive() {
			t.Errorf("%q.IsInconclusive() = true, want false: it is a real observation, not an absence of one", outcome)
		}
	}
}

func TestOutcomeTerminalStates(t *testing.T) {
	// Terminal means "will not change without a new operation", which is
	// what a caller would use to stop polling. In-progress and both
	// inconclusive outcomes must not be terminal: a reading that does not
	// settle can still settle, and a poll that stopped on one would freeze
	// the page on a question mark.
	for _, outcome := range allOutcomes() {
		want := outcome == OutcomeConfirmedComplete || outcome == OutcomeBlocked || outcome == OutcomeFailed
		if got := outcome.IsTerminal(); got != want {
			t.Errorf("%q.IsTerminal() = %v, want %v", outcome, got, want)
		}
	}
}

func TestRefusedErrorCarriesTheBackendWords(t *testing.T) {
	err := Refusal("sting.lab3.home.arpa", "an update of %s is already in progress", "sting.lab3.home.arpa")

	var refused *RefusedError
	if !errors.As(error(err), &refused) {
		t.Fatalf("Refusal() returned %T, which errors.As could not unwrap to *RefusedError", err)
	}
	if refused.Holder != "sting.lab3.home.arpa" {
		t.Errorf("Holder = %q, want the Comb that holds the update", refused.Holder)
	}
	if want := "an update of sting.lab3.home.arpa is already in progress"; refused.Detail != want {
		t.Errorf("Detail = %q, want %q", refused.Detail, want)
	}
	// The message a caller surfaces must contain the backend's own words
	// verbatim, so the operator reads the server's objection rather than
	// the UI's paraphrase of it.
	if got := err.Error(); !strings.Contains(got, "an update of sting.lab3.home.arpa is already in progress") {
		t.Errorf("Error() = %q, want it to carry the backend's reason verbatim", got)
	}
}

func TestRefusedErrorWithoutADetailStillReads(t *testing.T) {
	// A bare refusal must still be a sentence an operator can be shown,
	// not an empty string that renders as a blank banner.
	err := Refusal("", "")
	if err.Error() == "" {
		t.Error("a detail-less RefusedError has an empty message, which would render as an empty error banner")
	}
}

func TestErrNoImplementationIsDistinctFromARefusal(t *testing.T) {
	// "There is no update system attached" and "the update system said
	// no" are very different things to show an operator, so the two must
	// not be confusable.
	refused := Refusal("", "no")
	if errors.Is(error(refused), ErrNoImplementation) {
		t.Error("a RefusedError must not satisfy errors.Is(err, ErrNoImplementation); the caller renders them differently")
	}
	if !errors.Is(ErrNoImplementation, ErrNoImplementation) {
		t.Error("ErrNoImplementation does not match itself")
	}
}

func TestNilRefusedErrorDoesNotPanic(t *testing.T) {
	// A nil *RefusedError reaching Error() is reachable in practice: a
	// caller that does `var e *RefusedError; return e` compiles. It must
	// not take the process down.
	if got := (*RefusedError)(nil).Error(); got == "" {
		t.Error("a nil *RefusedError produced an empty message rather than a safe placeholder")
	}
}
