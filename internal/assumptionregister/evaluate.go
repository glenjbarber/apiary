package assumptionregister

import (
	"fmt"
	"strings"
	"time"
)

// State is the register's own evaluation vocabulary - a claim's
// CURRENT answer to "does Apiary still have grounds to believe this?",
// derived on every read from Claim.Evaluate.
//
// It deliberately reuses the names of the two vocabularies this
// codebase already uses for exactly this question rather than inventing
// a third: internal/health.Status's own five states
// (healthy/degraded/unknown/stale/contradictory) and
// internal/assumptions.StatusNotApplicable's "not_applicable" (which
// the pathtrace and recovery packages each duplicate). There is no
// "ok", no "expired", and no boolean: the whole point of this type is
// that "not proven" and "not looked at" must never be spelled the same
// way as "still true".
//
// Supported is the ONLY affirmative state. Nothing else - not an
// expired claim, not a claim whose evidence was never recorded, not an
// unrecognized status token - can produce it.
type State string

const (
	// StateSupported means the claim's evidence was affirmatively
	// confirmed within the claim's own lifetime: EvidenceStatus is
	// EvidenceSupported and LastVerified is set at or before now.
	StateSupported State = "supported"

	// StateContradicted means the last recorded check of the claim's
	// evidence found it contradicted the claim. A contradicted claim
	// is not a failed system - it is a belief the operator (or a
	// later checker) has already found to be untrue, and anything
	// resting on it is resting on something false.
	StateContradicted State = "contradicted"

	// StateUnknown means Apiary has no grounds either way. It is the
	// default and the fail-closed answer: an unobserved claim, a
	// claim with no evidence recorded, a claim whose recorded status
	// is unobserved, a claim asserting "supported" with no
	// verification time behind it, and a status token this build does
	// not recognize all land here. Per this project's own guardrail,
	// unknown means no evidence - never healthy and never failed.
	StateUnknown State = "unknown"

	// StateStale means the claim's own ExpiresAt has passed. A stale
	// claim is still reported, never dropped: the direction this
	// register implements requires that a claim which expires makes
	// dependent conclusions conditional, stale, contradictory, or
	// unknown "instead of remaining proven", and a claim that
	// silently disappeared would be read as an all-clear.
	StateStale State = "stale"

	// StateNotApplicable means the claim was recorded as not applying
	// here at all - a positive statement about scope, exactly
	// internal/assumptions' own definition of StatusNotApplicable,
	// and never to be conflated with StateUnknown.
	StateNotApplicable State = "not_applicable"
)

// EvidenceStatus is a RECORDED outcome - the answer an operator (or,
// in a later slice, an automated checker) last wrote down for this
// claim's supporting evidence. It is a raw observation about the
// world, never a verdict about the claim.
//
// The split is the same one internal/health draws between
// health.Observation and health.NodeHealth, and the same one ADR-0055
// established with observed_status/status: safety lives in the derived
// value a consumer actually reads. Persisting a "supported" here and
// calling it the claim's state would freeze a verdict that ages
// silently; recording only this and deriving State on every read means
// there is no stored verdict that can rot.
type EvidenceStatus string

const (
	// EvidenceUnobserved is the zero value: nobody has checked this
	// claim's evidence. It evaluates to StateUnknown, never to
	// StateSupported.
	EvidenceUnobserved EvidenceStatus = "unobserved"

	// EvidenceSupported is a recorded affirmative check. On its own it
	// is not enough: Evaluate also requires a LastVerified time, and
	// Validate refuses to store the pair with the time missing.
	EvidenceSupported EvidenceStatus = "supported"

	// EvidenceContradicted is a recorded negative check, and evaluates
	// to StateContradicted.
	EvidenceContradicted EvidenceStatus = "contradicted"

	// EvidenceNotApplicable is a recorded positive statement that the
	// claim does not apply in this environment, and evaluates to
	// StateNotApplicable.
	EvidenceNotApplicable EvidenceStatus = "not_applicable"
)

// knownEvidenceStatuses is the complete set of tokens EvidenceStatus
// may hold. Anything outside it is rejected by Validate rather than
// quietly degrading to unknown at read time, where the operator would
// never learn their typo was the reason.
var knownEvidenceStatuses = map[EvidenceStatus]bool{
	EvidenceUnobserved:    true,
	EvidenceSupported:     true,
	EvidenceContradicted:  true,
	EvidenceNotApplicable: true,
}

// Evaluation is one claim's derived verdict plus the words saying why.
// The reason is carried rather than reconstructed by the UI, so the
// same text that decides the state is the text an operator reads -
// this project's rule is that a verdict is never conveyed by colour or
// by a bare enum alone.
type Evaluation struct {
	State  State
	Reason string
}

// Evaluate derives this claim's current state against now.
//
// It is a pure function of the stored claim and the caller's clock, and
// nothing else. That is the whole design: the state is computed lazily
// at READ time and is never written back, so a claim cannot carry a
// durable verdict that expired, or that an automated observation
// stamped over it, without the next read noticing.
//
// Precedence, highest first:
//
//	not_applicable  the claim was recorded as not applying here
//	stale           the claim's own expiry has passed
//	contradicted    the recorded check found the evidence false
//	supported       recorded affirmative evidence, verified in time
//	unknown         everything else, always
//
// not_applicable outranks staleness deliberately: a claim that does
// not apply has nothing left to expire, and it is already reported as
// something other than true. Staleness outranks a recorded
// "supported" because a confirmation made during the claim's lifetime
// says nothing about it now - the direction this register implements
// requires expired claims to stop counting as true even when their
// evidence was once confirmed.
func (c Claim) Evaluate(now time.Time) Evaluation {
	if c.EvidenceStatus == EvidenceNotApplicable {
		return Evaluation{
			State:  StateNotApplicable,
			Reason: "recorded as not applying to this environment; it is not a claim about anything here, so it is neither true nor false.",
		}
	}
	if !c.ExpiresAt.After(now) {
		return Evaluation{
			State: StateStale,
			Reason: fmt.Sprintf("expired %s - its evidence is no longer current, and Apiary has not re-confirmed it since.",
				c.ExpiresAt.Local().Format("2006-01-02 15:04 MST")),
		}
	}
	if strings.TrimSpace(c.Evidence) == "" {
		return Evaluation{
			State:  StateUnknown,
			Reason: "no supporting evidence is recorded for this claim, so there is nothing to have confirmed it.",
		}
	}
	switch c.EvidenceStatus {
	case EvidenceContradicted:
		return Evaluation{
			State:  StateContradicted,
			Reason: "the last recorded check found the supporting evidence does not hold this claim up; anything resting on it is resting on something false.",
		}
	case EvidenceSupported:
		if c.LastVerified.IsZero() {
			return Evaluation{
				State:  StateUnknown,
				Reason: "the evidence was recorded as supported but no verification time was ever recorded with it, so Apiary cannot tell how current that confirmation is.",
			}
		}
		if c.LastVerified.After(now) {
			return Evaluation{
				State:  StateUnknown,
				Reason: "the recorded verification time is in the future, so it is not evidence of anything yet.",
			}
		}
		return Evaluation{
			State: StateSupported,
			Reason: fmt.Sprintf("evidence recorded as supported and confirmed %s, within this claim's own lifetime.",
				c.LastVerified.Local().Format("2006-01-02 15:04 MST")),
		}
	case EvidenceUnobserved:
		return Evaluation{
			State:  StateUnknown,
			Reason: "nobody has checked this claim's evidence since it was recorded; that is no evidence either way, not a pass.",
		}
	}
	return Evaluation{
		State: StateUnknown,
		Reason: fmt.Sprintf("the recorded evidence status %q is not one this build recognizes, so it is treated as no evidence at all rather than as a pass.",
			string(c.EvidenceStatus)),
	}
}

// State is the short form of the claim's current evaluation, for
// callers that want the verdict without the sentence explaining it.
func (c Claim) State(now time.Time) State {
	return c.Evaluate(now).State
}
