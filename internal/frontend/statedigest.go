package frontend

// Presentation of ADR-0143's cross-voter FSM state verdicts on the
// colony view.
//
// The verdicts themselves - what `match`, `mismatch`, `unsettled` and
// `unobserved` mean, which of them permit a caller to proceed, and the
// sentence behind each - live in internal/statedigest, because they are
// not a rendering concern. A controller taking one Comb at a time has to
// read the same four and be held to the same meanings, and a controller
// cannot import a web template package to get them.
//
// What is left here is exactly the part that is this package's own: which
// badge class in layout.html's palette each verdict renders as, and the
// short label it carries. Both are chosen here rather than in the
// templates so no two templates can disagree about which colour means
// what, and both are derived from the verdict by an exhaustive switch -
// there is no default arm, so a fifth verdict added to statedigest is a
// compile error here rather than a row that silently renders unstyled.

import (
	"github.com/glenjbarber/apiary/internal/statedigest"
)

// stateDigestView is one row's rendered verdict. The verdict and its
// sentence come from statedigest; State, Label and BadgeClass are added
// here, so a template renders what it was handed and never reasons about
// evidence itself.
type stateDigestView struct {
	// State is one of statedigest's four verdicts, as a string, for the
	// template. It is the verdict itself rather than a separate
	// presentation label, so a row can never show a green badge beside a
	// mismatch verdict.
	State string

	// StateDigest is this Comb's own digest, or "" when unobserved.
	StateDigest string

	// AppliedIndex is the index StateDigest was read at, 0 when
	// unobserved.
	AppliedIndex uint64

	// Label is the short badge text. A matched row says so plainly; a
	// mismatched or unsettled row says which; an unobserved row names
	// itself rather than rendering an empty badge that would read as
	// "nothing to report".
	Label string

	// Detail is the operator-readable sentence behind the verdict,
	// carried through from statedigest rather than reworded here, so
	// the limitation on a mismatch - that the digest does not say which
	// side is wrong - is the same sentence a controller reads.
	Detail string

	// BadgeClass maps State to the badge vocabulary in layout.html,
	// chosen here so no two templates can disagree about which colour
	// means what.
	BadgeClass string
}

// stateDigestColony is the colony-wide conclusion the per-row badges sit
// under, rendered. It is kept separate from the rows because "the
// colony's state machines do not all agree" is a fact about the SET, and
// no single row can state it.
type stateDigestColony struct {
	// Agreed mirrors statedigest's own Agreed, kept as a bool for the
	// template. It is false both for proven disagreement and for a
	// sample too small or too incomplete to conclude anything - Agreed
	// false means "not established", which Detail always distinguishes
	// for the reader.
	Agreed bool

	// Observed and Total count Combs whose digest was read, and Combs
	// known in all. A Total greater than Observed is stated in the
	// sentence, because a comparison that silently excluded a Comb
	// would overstate what was established.
	Observed int
	Total    int

	// Detail is the one-sentence colony verdict, always non-empty.
	Detail string

	// BadgeClass is the colony's own badge class: "ready" when agreed,
	// "error" for proven disagreement, "degraded" for unsettled, and
	// "unknown" when there was not enough evidence to say.
	BadgeClass string

	// BadgeLabel is the short text form, always set so the panel head
	// renders a badge rather than an empty element.
	BadgeLabel string
}

// stateDigestVerdicts renders ADR-0143's verdicts for a page. It is a
// thin map from statedigest's answer onto this package's vocabulary; the
// classification, the refusals and the sentences are all upstream.
func stateDigestVerdicts(observations []statedigest.Observation) (map[string]stateDigestView, stateDigestColony) {
	verdicts, colonyVerdict := statedigest.Verdicts(observations)

	views := make(map[string]stateDigestView, len(verdicts))
	for nodeID, verdict := range verdicts {
		views[nodeID] = stateDigestRow(verdict)
	}
	return views, stateDigestColony{
		Agreed:     colonyVerdict.Agreed,
		Observed:   colonyVerdict.Observed,
		Total:      colonyVerdict.Total,
		Detail:     colonyVerdict.Detail,
		BadgeClass: colonyBadgeClass(colonyVerdict.Verdict),
		BadgeLabel: colonyBadgeLabel(colonyVerdict.Verdict),
	}
}

// stateDigestUnobservedView is the rendered row for a Comb that cannot
// be compared with anything. Used where a single Comb's digest is known
// but no set is, so the row is never left with an empty badge class -
// which would read as "nothing to report" rather than "not observed".
func stateDigestUnobservedView(observation statedigest.Observation, total int) stateDigestView {
	return stateDigestRow(statedigest.UnobservedNode(observation, total))
}

func stateDigestRow(verdict statedigest.NodeVerdict) stateDigestView {
	return stateDigestView{
		State:        string(verdict.Verdict),
		StateDigest:  verdict.Digest,
		AppliedIndex: verdict.AppliedIndex,
		Label:        nodeBadgeLabel(verdict.Verdict),
		Detail:       verdict.Detail,
		BadgeClass:   nodeBadgeClass(verdict.Verdict),
	}
}

// Badge classes, drawn from layout.html's palette. "ready" is green,
// "error" is red, "degraded" is amber, "unknown" is the neutral state
// this project reserves for no evidence - never for health.
const (
	badgeReady    = "ready"
	badgeError    = "error"
	badgeDegraded = "degraded"
	badgeUnknown  = "unknown"
)

func nodeBadgeClass(verdict statedigest.Verdict) string {
	switch verdict {
	case statedigest.Match:
		return badgeReady
	case statedigest.Mismatch:
		return badgeError
	case statedigest.Unsettled:
		return badgeDegraded
	case statedigest.Unobserved:
		return badgeUnknown
	}
	// No default arm. statedigest is the only place a verdict is
	// defined, and a fifth one added there must break this build rather
	// than render an unstyled row that reads as a plain label.
	panic("unhandled FSM state verdict: " + string(verdict))
}

func nodeBadgeLabel(verdict statedigest.Verdict) string {
	switch verdict {
	case statedigest.Match:
		return "State matches"
	case statedigest.Mismatch:
		return "State differs"
	case statedigest.Unsettled:
		return "State unsettled"
	case statedigest.Unobserved:
		return "State unobserved"
	}
	panic("unhandled FSM state verdict: " + string(verdict))
}

// Colony-level badge labels. Kept beside the mapping rather than written
// in the template so the wording that claims agreement and the wording
// that refuses it are decided in one place, by the same code that
// decides which applies.
const (
	colonyAgreedLabel    = "State agreed"
	colonyDivergedLabel  = "State diverged"
	colonyUnsettledLabel = "State unsettled"
)

func colonyBadgeClass(verdict statedigest.Verdict) string {
	switch verdict {
	case statedigest.Match:
		return badgeReady
	case statedigest.Mismatch:
		return badgeError
	case statedigest.Unsettled:
		return badgeDegraded
	case statedigest.Unobserved:
		return badgeUnknown
	}
	panic("unhandled FSM state verdict: " + string(verdict))
}

func colonyBadgeLabel(verdict statedigest.Verdict) string {
	switch verdict {
	case statedigest.Match:
		return colonyAgreedLabel
	case statedigest.Mismatch:
		return colonyDivergedLabel
	case statedigest.Unsettled:
		return colonyUnsettledLabel
	case statedigest.Unobserved:
		return "State unobserved"
	}
	panic("unhandled FSM state verdict: " + string(verdict))
}
