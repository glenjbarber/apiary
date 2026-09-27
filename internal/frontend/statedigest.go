package frontend

import "strconv"

// Cross-voter FSM state agreement, rendered as a badge on the colony
// view (ADR-0143).
//
// This is the consumer half of the digest raftd now computes. raftd and
// managerd only ever report ONE voter's digest; nothing in either of
// them claims the colony agrees, because neither can know that from a
// single state machine. Deciding whether the digests agree is a
// colony-wide question, and this file is where it is answered - once,
// from the whole set, with the answer written the same way the rest of
// this package writes verdicts: an observation and a conclusion kept
// visibly separate, and silence never rendered as health.
//
// The three failure modes this has to refuse to commit:
//
//   - A single observation. One node always agrees with itself, so
//     "everyone observed agrees" is TRUE of a one-node sample. Reporting
//     it as agreement would turn the absence of evidence into a green
//     badge, which is the exact mistake ADR-0056 exists to prevent.
//   - A digest with no index beside it. Sampling a moving cluster
//     produces mismatches that are not divergence, so a mismatch is only
//     reported as proven when the Combs involved agree on their applied
//     index.
//   - Localising the fault. Two Combs with different state means one of
//     them applied something differently. Which one is a question this
//     evidence cannot answer, so no verdict here ever implies it.

// stateDigestObservation is one Comb's raw reading: the digest of its own
// FSM state, and the applied index it was read at. An empty Digest means
// no digest was observed - the Comb could not be dialed, did not answer,
// could not reach its own raftd, or runs a raftd predating ADR-0143 -
// which are different causes that share one honest rendering, and each
// already appears in that row's own health observations.
type stateDigestObservation struct {
	NodeID string

	// Digest is lowercase hex, or "" when unobserved.
	Digest string

	// AppliedIndex is the index Digest was read at. It is meaningful
	// only when Digest is non-empty: there is no index to compare when
	// there is no digest, and a zero index beside a real digest would be
	// a fabricated reading.
	AppliedIndex uint64
}

// Badge states. Each is a distinct claim about what was observed, and the
// distinction between the last two is the whole reason AppliedIndex
// travels with the digest.
const (
	// stateDigestMatch means every Comb that reported a digest reported
	// the same one.
	stateDigestMatch = "match"

	// stateDigestMismatch means the digests differ AND the Combs
	// involved agreed on their applied index, so no sampling artifact
	// can explain it. This is the strongest statement the evidence
	// supports: these state machines really do hold different state.
	stateDigestMismatch = "mismatch"

	// stateDigestUnsettled means the digests differ, but at least one
	// involved Comb was at a different applied index, so the difference
	// may be nothing more than the sample being taken while the colony
	// moved. Deliberately NOT a mismatch: the colony is not known to
	// have diverged, and saying so would be reading a conclusion into
	// an unsettled reading.
	stateDigestUnsettled = "unsettled"

	// stateDigestUnobserved means this Comb's own digest could not be
	// read. It is never folded into the match count and never rendered
	// as agreement, no matter what the other Combs reported.
	stateDigestUnobserved = "unobserved"
)

// stateDigestView is one row's verdict. Like colonyLeaderView, the raw
// reading (StateDigest, AppliedIndex) is kept apart from the conclusion
// (State, Label, Detail) so a template renders a verdict it was handed
// and never reasons about evidence itself.
type stateDigestView struct {
	// State is one of the stateDigest* constants above.
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
	// always non-empty. An unexplained badge is the one thing an
	// operator cannot act on, so every state carries a reason - and the
	// reason for a mismatch never names a culprit.
	Detail string

	// BadgeClass maps State to the badge vocabulary in layout.html,
	// chosen here so no two templates can disagree about which colour
	// means what.
	BadgeClass string
}

// stateDigestColony is the colony-wide conclusion the per-row badges sit
// under. It is kept separate from the rows because "the colony's state
// machines do not all agree" is a fact about the SET, and no single row
// can state it.
type stateDigestColony struct {
	// Agreed is true only when at least two Combs reported a digest and
	// every one of them reported the same one. It is false both for
	// proven disagreement and for a sample too small or too incomplete
	// to conclude anything - Agreed false means "not established",
	// which Detail always distinguishes for the reader.
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

// stateDigestVerdicts compares a whole set of per-Comb readings at once.
//
// It is pure: no I/O, no clock, no ordering dependence beyond the input
// slice. That is deliberate and matches colonyLeaderFromStatus - every
// state above is reachable by test without a live cluster, so the
// one-observation and unsettled cases in particular cannot regress
// silently into a green badge.
func stateDigestVerdicts(observations []stateDigestObservation) (map[string]stateDigestView, stateDigestColony) {
	views := make(map[string]stateDigestView, len(observations))

	observed := make([]stateDigestObservation, 0, len(observations))
	for _, observation := range observations {
		if observation.Digest == "" {
			views[observation.NodeID] = stateDigestUnobservedView(observation, len(observations))
			continue
		}
		observed = append(observed, observation)
	}

	colony := stateDigestColony{Observed: len(observed), Total: len(observations)}

	switch {
	case len(observed) == 0:
		colony.Detail = "no Comb reported an FSM state digest, so colony-wide state agreement is unobserved"
		colony.BadgeClass = "unknown"
		colony.BadgeLabel = "State unobserved"
	case len(observed) == 1:
		colony.Detail = "only one Comb reported an FSM state digest, so agreement cannot be established - " +
			"a single voter always agrees with itself"
		colony.BadgeClass = "unknown"
		colony.BadgeLabel = "State unobserved"
	default:
		colony.Agreed, colony.Detail, colony.BadgeClass = colonyAgreement(observed, colony.Total)
		colony.BadgeLabel = colonyAgreedLabel
		if !colony.Agreed {
			colony.BadgeLabel = colonyDivergedLabel
			if colony.BadgeClass == "degraded" {
				colony.BadgeLabel = colonyUnsettledLabel
			}
		}
	}

	// The one-observation case rewrites even the observed row: with
	// nothing to compare it against, its own digest is not evidence of
	// anything, and a green badge beside one green node would be the
	// single most misleading thing this badge could render.
	if len(observed) < 2 {
		for _, observation := range observed {
			views[observation.NodeID] = stateDigestUnobservedView(observation, len(observations))
		}
		return views, colony
	}

	for _, observation := range observed {
		views[observation.NodeID] = stateDigestVerdictFor(observation, observed, colony)
	}
	return views, colony
}

// Colony-level badge labels. Kept beside the comparison rather than
// written in the template so the wording that claims agreement and the
// wording that refuses it are decided in one place, by the same code that
// decides which applies.
const (
	colonyAgreedLabel    = "State agreed"
	colonyDivergedLabel  = "State diverged"
	colonyUnsettledLabel = "State unsettled"
)

// colonyAgreement decides the set-level question: do all observed digests
// match, and if not, is the difference proven or merely unsettled?
func colonyAgreement(observed []stateDigestObservation, total int) (agreed bool, detail, badgeClass string) {
	first := observed[0].Digest
	uniform := true
	uniformIndex := true
	for _, observation := range observed[1:] {
		if observation.Digest != first {
			uniform = false
		}
		if observation.AppliedIndex != observed[0].AppliedIndex {
			uniformIndex = false
		}
	}
	// Never let "all of the ones we could read agree" read as "the colony
	// agrees". A Comb whose digest was unreadable is not evidence of
	// anything, and saying so in the colony sentence as well as on that
	// Comb's own row is the difference between an honest summary and an
	// overstatement.
	excluded := ""
	if len(observed) < total {
		excluded = ", but " + strconv.Itoa(total-len(observed)) + " of " + strconv.Itoa(total) +
			" Combs reported no digest and were excluded from the comparison"
	}

	if uniform {
		return true, "all " + strconv.Itoa(len(observed)) +
			" Combs that reported a digest report the same FSM state" + excluded, "ready"
	}
	if !uniformIndex {
		return false, "the Combs that reported a digest are at different applied indexes, so the difference " +
			"may be only this sample being taken while the colony moved" + excluded, "degraded"
	}
	return false, "all " + strconv.Itoa(len(observed)) + " Combs report applied index " +
		strconv.FormatUint(observed[0].AppliedIndex, 10) +
		" and still hold different FSM state - the state machines have diverged" + excluded, "error"
}

// stateDigestVerdictFor is one row's verdict, given the whole observed
// set. It counts how many OTHER Combs share this row's digest, purely to
// make the sentence specific; it never uses that count to imply which side
// is correct.
func stateDigestVerdictFor(observation stateDigestObservation, observed []stateDigestObservation, colony stateDigestColony) stateDigestView {
	view := stateDigestView{
		State:        stateDigestMatch,
		StateDigest:  observation.Digest,
		AppliedIndex: observation.AppliedIndex,
		BadgeClass:   "ready",
		Label:        "State matches",
	}

	if colony.Agreed {
		view.Detail = "this Comb's FSM state digest matches the " + strconv.Itoa(len(observed)) +
			" other Combs that reported one, all at applied index " + strconv.FormatUint(observation.AppliedIndex, 10)
		return view
	}

	agreeing, others := 0, 0
	for _, other := range observed {
		if other.NodeID == observation.NodeID {
			continue
		}
		if other.Digest == observation.Digest {
			agreeing++
		} else {
			others++
		}
	}

	if colony.BadgeClass == "degraded" {
		view.State = stateDigestUnsettled
		view.BadgeClass = "degraded"
		view.Label = "State unsettled"
		view.Detail = "this Comb's FSM state digest differs from " + strconv.Itoa(others) + " of the " +
			strconv.Itoa(len(observed)-1) + " other Combs that reported one, but those Combs are at different applied " +
			"indexes, so this may be a sample of a moving colony rather than a divergence"
		return view
	}

	// Proven disagreement: every involved Comb is at the same applied
	// index, so nothing about the timing can explain it. The wording is
	// deliberately symmetric - it says how many agree and how many do
	// not, and never which side is wrong, because a digest cannot tell
	// that and a badge that appears to would be lying.
	view.State = stateDigestMismatch
	view.BadgeClass = "error"
	view.Label = "State differs"
	view.Detail = "this Comb's FSM state digest matches " + strconv.Itoa(agreeing) + " of the " + strconv.Itoa(len(observed)-1) +
		" other Combs that reported one and differs from " + strconv.Itoa(others) +
		", all at applied index " + strconv.FormatUint(observation.AppliedIndex, 10) +
		" - the state machines have diverged, and the digest does not say which side is wrong"
	return view
}

// stateDigestUnobservedView is the verdict for a row that cannot be
// compared at all, whether because its own digest was unreadable or
// because there was nothing to compare it against.
func stateDigestUnobservedView(observation stateDigestObservation, total int) stateDigestView {
	view := stateDigestView{
		State:        stateDigestUnobserved,
		StateDigest:  observation.Digest,
		AppliedIndex: observation.AppliedIndex,
		BadgeClass:   "unknown",
		Label:        "State unobserved",
	}
	if observation.Digest != "" {
		// Its digest was read, but there was no second reading to
		// compare it with. Saying so is more useful than implying the
		// digest was unreadable, and the difference matters to anyone
		// deciding whether to go looking for a missing voter.
		view.Detail = "this Comb reported an FSM state digest, but no other Comb did, so this reading cannot be " +
			"compared with anything"
		return view
	}
	view.Detail = "this Comb's FSM state digest could not be read, so it is excluded from the colony-wide " +
		"comparison of " + strconv.Itoa(total) + " Combs"
	return view
}
