// The cross-voter verdicts (ADR-0143), moved here from internal/frontend
// and re-exposed, because a controller taking one Comb at a time has to
// be able to read the same four verdicts the colony view renders and be
// held to the same meanings.
//
// Nothing in raftd or managerd can answer any of this. Each reports ONE
// voter's digest, and a single state machine cannot know whether the
// colony agrees with it. Deciding what a SET of digests means is a
// separate question with a separate answer, and that is what this file
// is. The verdicts are the evidence-aware-health rule of ADR-0056
// applied to FSM state, and they inherit its central refusal: silence
// is a state with its own verdict, and it is never health.
//
// It is pure. No I/O, no clock, no ordering dependence beyond the input
// slice, so every verdict below is reachable by test without a live
// cluster and the one-observation and unsettled cases in particular
// cannot regress silently into a green answer.
package statedigest

import "strconv"

// Observation is one Comb's raw reading: the digest of its own FSM
// state, and the applied index it was read at.
//
// An empty Digest means no digest was observed - the Comb could not be
// dialed, did not answer, could not reach its own raftd, or runs a
// raftd predating ADR-0143 - which are different causes sharing one
// honest rendering, and each already appears in that row's own health
// observations.
type Observation struct {
	NodeID string

	// Digest is lowercase hex, or "" when unobserved.
	Digest string

	// AppliedIndex is the index Digest was read at. It is meaningful
	// only when Digest is non-empty: there is no index to compare when
	// there is no digest, and a zero index beside a real digest would be
	// a fabricated reading.
	AppliedIndex uint64
}

// Verdict is what a set of observations established. Each is a distinct
// claim, and the distinctions between them are the whole contract.
type Verdict string

const (
	// Match means every Comb that reported a digest reported the same
	// one, and at least two of them did. It is the only verdict that
	// permits a caller to proceed.
	Match Verdict = "match"

	// Mismatch means the digests differ AND the Combs involved agreed
	// on their applied index, so no sampling artifact can explain it.
	// This is the strongest statement the evidence supports: these
	// state machines really do hold different state. It is a stop.
	Mismatch Verdict = "mismatch"

	// Unsettled means the digests differ, but at least one involved Comb
	// was at a different applied index, so the difference may be nothing
	// more than the sample being taken while the colony moved.
	// Deliberately NOT a Mismatch: the colony is not known to have
	// diverged, and saying so would be reading a conclusion into an
	// unsettled reading. The caller waits and reads again.
	Unsettled Verdict = "unsettled"

	// Unobserved means this Comb's own digest could not be read, or
	// nothing else could be read to compare it against. It is never
	// folded into the match count and never rendered or reported as
	// agreement, no matter what the other Combs reported. It is a stop.
	Unobserved Verdict = "unobserved"
)

// Permits reports whether a verdict is permission to take the next step.
// Only Match is. Unsettled is not a stop - the colony is probably just
// moving, and the right response is to wait and read again - but it is
// equally not permission, and Permits answers only the permission
// question so a caller cannot read a wait as a yes.
func (v Verdict) Permits() bool { return v == Match }

// Stops reports whether a verdict is a stop: proven divergence
// (Mismatch) or absent evidence (Unobserved).
//
// The second is the one worth arguing for, because it is the case a
// fail-open implementation gets wrong. "I could not read the evidence"
// is not "the evidence says go", and a gate that returns the same
// answer for both cannot tell an operator which one happened. Unsettled
// is deliberately NOT a stop, and Permits is false for it too: it is
// the one answer that calls for a re-read rather than for an
// intervention.
func (v Verdict) Stops() bool { return v == Mismatch || v == Unobserved }

// NodeVerdict is one Comb's verdict. Like every other view in this
// project, the raw reading (Digest, AppliedIndex) is kept apart from
// the conclusion (Verdict, Detail) so a consumer renders a verdict it
// was handed and never reasons about evidence itself.
type NodeVerdict struct {
	// NodeID is the Comb this verdict is about.
	NodeID string

	// Verdict is one of the four constants above.
	Verdict Verdict

	// Digest is this Comb's own digest, or "" when unobserved.
	Digest string

	// AppliedIndex is the index Digest was read at, 0 when unobserved.
	AppliedIndex uint64

	// Agreeing and Others count the OTHER Combs that reported a digest,
	// split by whether they hold the same one as this Comb. They exist
	// to make the sentence specific. They are never used to imply which
	// side is correct, because a digest cannot say that.
	Agreeing int
	Others   int

	// Observed and Total are the colony-wide counts, carried per node so
	// a row can say how big the comparison it took part in was.
	Observed int
	Total    int

	// Detail is the operator-readable sentence behind the verdict,
	// always non-empty. An unexplained verdict is the one thing an
	// operator cannot act on, so every verdict carries a reason - and
	// the reason for a Mismatch never names a culprit.
	Detail string
}

// ColonyVerdict is the set-level conclusion. It is kept separate from
// the rows because "the colony's state machines do not all agree" is a
// fact about the SET, and no single row can state it.
type ColonyVerdict struct {
	// Verdict is the set-level conclusion, one of the four constants.
	Verdict Verdict

	// Agreed is true only when at least two Combs reported a digest and
	// every one of them reported the same one. It is false both for
	// proven disagreement and for a sample too small or too incomplete
	// to conclude anything - Agreed false means "not established",
	// which Detail always distinguishes for the reader. Prefer Verdict
	// where the reason matters; this field is kept because callers
	// rendering a boolean need it.
	Agreed bool

	// Observed and Total count Combs whose digest was read, and Combs
	// known in all. A Total greater than Observed is stated in the
	// sentence, because a comparison that silently excluded a Comb
	// would overstate what was established.
	Observed int
	Total    int

	// Detail is the one-sentence colony verdict, always non-empty.
	Detail string
}

// Verdicts compares a whole set of per-Comb readings at once, and
// returns one verdict per Comb keyed by NodeID plus the colony-wide
// conclusion.
//
// Every path through this function is a statement this package makes
// about a real cluster, and each of the three refusals the package
// comment lists is enforced here rather than in prose: a single
// observation cannot produce Match, a Mismatch requires equal applied
// indexes, and an unreadable Comb is excluded from the comparison and
// reported as Unobserved rather than being allowed to vanish.
func Verdicts(observations []Observation) (map[string]NodeVerdict, ColonyVerdict) {
	views := make(map[string]NodeVerdict, len(observations))

	observed := make([]Observation, 0, len(observations))
	for _, observation := range observations {
		if observation.Digest == "" {
			views[observation.NodeID] = UnobservedNode(observation, len(observations))
			continue
		}
		observed = append(observed, observation)
	}

	colony := ColonyVerdict{Observed: len(observed), Total: len(observations)}

	switch {
	case len(observed) == 0:
		colony.Detail = "no Comb reported an FSM state digest, so colony-wide state agreement is unobserved"
		colony.Verdict = Unobserved
	case len(observed) == 1:
		colony.Detail = "only one Comb reported an FSM state digest, so agreement cannot be established - " +
			"a single voter always agrees with itself"
		colony.Verdict = Unobserved
	default:
		uniform, uniformIndex := uniformity(observed)
		excluded := exclusionSuffix(len(observed), colony.Total)
		switch {
		case uniform:
			colony.Verdict, colony.Agreed = Match, true
			colony.Detail = "all " + strconv.Itoa(len(observed)) +
				" Combs that reported a digest report the same FSM state" + excluded
		case !uniformIndex:
			colony.Verdict = Unsettled
			colony.Detail = "the Combs that reported a digest are at different applied indexes, so the difference " +
				"may be only this sample being taken while the colony moved" + excluded
		default:
			colony.Verdict = Mismatch
			colony.Detail = "all " + strconv.Itoa(len(observed)) + " Combs report applied index " +
				strconv.FormatUint(observed[0].AppliedIndex, 10) +
				" and still hold different FSM state - the state machines have diverged" + excluded
		}
	}

	// The one-observation case rewrites even the observed row: with
	// nothing to compare it against, its own digest is not evidence of
	// anything, and a green answer beside one green node would be the
	// single most misleading thing this package could produce.
	if len(observed) < 2 {
		for _, observation := range observed {
			views[observation.NodeID] = UnobservedNode(observation, len(observations))
		}
		return views, colony
	}

	for _, observation := range observed {
		views[observation.NodeID] = nodeVerdictFor(observation, observed, colony)
	}
	return views, colony
}

// uniformity reports whether every observed digest is the same, and -
// separately - whether every observed applied index is the same. The
// two are asked together and answered apart because their combination
// is the whole classification: equal digests at differing indexes are
// agreement, and differing digests at equal indexes are proven
// divergence, while differing on both may be only a moving sample.
func uniformity(observed []Observation) (uniformDigest, uniformIndex bool) {
	first := observed[0]
	uniformDigest, uniformIndex = true, true
	for _, observation := range observed[1:] {
		if observation.Digest != first.Digest {
			uniformDigest = false
		}
		if observation.AppliedIndex != first.AppliedIndex {
			uniformIndex = false
		}
	}
	return uniformDigest, uniformIndex
}

// exclusionSuffix never lets "all of the ones we could read agree" read
// as "the colony agrees". A Comb whose digest was unreadable is not
// evidence of anything, and saying so in the colony sentence as well as
// on that Comb's own row is the difference between an honest summary and
// an overstatement.
func exclusionSuffix(observed, total int) string {
	if observed >= total {
		return ""
	}
	return ", but " + strconv.Itoa(total-observed) + " of " + strconv.Itoa(total) +
		" Combs reported no digest and were excluded from the comparison"
}

// nodeVerdictFor is one row's verdict, given the whole observed set.
func nodeVerdictFor(observation Observation, observed []Observation, colony ColonyVerdict) NodeVerdict {
	view := NodeVerdict{
		NodeID:       observation.NodeID,
		Verdict:      Match,
		Digest:       observation.Digest,
		AppliedIndex: observation.AppliedIndex,
		Observed:     colony.Observed,
		Total:        colony.Total,
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
	view.Agreeing, view.Others = agreeing, others

	if colony.Verdict == Match {
		view.Detail = "this Comb's FSM state digest matches the " + strconv.Itoa(agreeing) +
			" other Combs that reported one, all at applied index " + strconv.FormatUint(observation.AppliedIndex, 10)
		return view
	}

	if colony.Verdict == Unsettled {
		view.Verdict = Unsettled
		view.Detail = "this Comb's FSM state digest differs from " + strconv.Itoa(others) + " of the " +
			strconv.Itoa(colony.Observed-1) + " other Combs that reported one, but those Combs are at different applied " +
			"indexes, so this may be a sample of a moving colony rather than a divergence"
		return view
	}

	// Proven disagreement: every involved Comb is at the same applied
	// index, so nothing about the timing can explain it. The wording is
	// deliberately symmetric - it says how many agree and how many do
	// not, and never which side is wrong, because a digest cannot tell
	// that and a verdict that appeared to would be lying.
	view.Verdict = Mismatch
	view.Detail = "this Comb's FSM state digest matches " + strconv.Itoa(agreeing) + " of the " + strconv.Itoa(colony.Observed-1) +
		" other Combs that reported one and differs from " + strconv.Itoa(others) +
		", all at applied index " + strconv.FormatUint(observation.AppliedIndex, 10) +
		" - the state machines have diverged, and the digest does not say which side is wrong"
	return view
}

// UnobservedNode is the verdict for a Comb that cannot be compared at all,
// whether because its own digest was unreadable or because there was
// nothing to compare it against.
//
// It is exported because a consumer that has read a single Comb's
// digest and has no others - a per-Comb page, a health check, one step
// of a sweep - still owes that Comb an honest row rather than a blank
// one, and this is that row. total is the size of the comparison the
// Comb was excluded from.
func UnobservedNode(observation Observation, total int) NodeVerdict {
	view := NodeVerdict{
		NodeID:  observation.NodeID,
		Verdict: Unobserved,
		Digest:  observation.Digest,
		// AppliedIndex travels with the reading even here. The verdict
		// is that the reading cannot be COMPARED, not that it was never
		// taken - a consumer deciding whether to go looking for a missing
		// voter needs to know the reading was real, and dropping the index
		// here would render a row whose applied index is a fabricated 0
		// beside a real digest.
		AppliedIndex: observation.AppliedIndex,
		Total:        total,
		// Observed is 0 on this path on purpose. A Comb that could not
		// be read has no place in the count of Combs that were, and
		// leaving the field at its zero value is more honest than
		// inventing a number for it.
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
