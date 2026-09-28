package statedigest

import (
	"strings"
	"testing"
)

// These cover what a SET of digests is allowed to claim (ADR-0143), and -
// at least as importantly - what it refuses to claim. The states that
// matter are the ones a naive implementation gets wrong: a single
// observation, a sample taken while the colony moved, and a Comb that
// could not be read at all.
//
// The verdicts are asserted here rather than in internal/frontend because
// this is where they now live and because their MEANINGS are the
// property, not their badge colour. The rendering is checked in
// internal/frontend/statedigest_test.go.

func observed(nodeID, digest string, index uint64) Observation {
	return Observation{NodeID: nodeID, Digest: digest, AppliedIndex: index}
}

func unobservedDigest(nodeID string) Observation {
	return Observation{NodeID: nodeID}
}

func TestVerdictsAllAgree(t *testing.T) {
	views, colony := Verdicts([]Observation{
		observed("brood", "aaaa", 181),
		observed("drone", "aaaa", 181),
		observed("buzz", "aaaa", 181),
		observed("sting", "aaaa", 181),
	})

	if !colony.Agreed {
		t.Errorf("Agreed = false, want true - every reported digest is identical")
	}
	if colony.Verdict != Match {
		t.Errorf("colony Verdict = %q, want %q", colony.Verdict, Match)
	}
	if colony.Observed != 4 || colony.Total != 4 {
		t.Errorf("Observed/Total = %d/%d, want 4/4", colony.Observed, colony.Total)
	}
	for _, nodeID := range []string{"brood", "drone", "buzz", "sting"} {
		view := views[nodeID]
		if view.Verdict != Match {
			t.Errorf("%s: Verdict = %q, want %q", nodeID, view.Verdict, Match)
		}
		if view.Detail == "" {
			t.Errorf("%s: Detail is empty, want it populated - an unexplained verdict is not actionable", nodeID)
		}
	}
	// A matched colony is the one state that permits a controller to
	// proceed, and this is the only case in the whole table where that
	// is true.
	if !colony.Verdict.Permits() {
		t.Error("a colony of four identical digests does not permit proceeding")
	}
	if colony.Verdict.Stops() {
		t.Error("a colony of four identical digests is reported as a stop")
	}
}

// TestVerdictsAllAgreeCountsOtherCombsAndNotItself pins the count in the
// sentence. The original frontend wording said "matches the N other
// Combs" using N = the number observed INCLUDING this one, so a
// four-Combs colony rendered "matches the 4 other Combs" beside three
// others. The count is now the number of other Combs that actually hold
// the same digest, so the sentence and the Agreeing field agree.
func TestVerdictsAllAgreeCountsOtherCombsAndNotItself(t *testing.T) {
	views, _ := Verdicts([]Observation{
		observed("brood", "aaaa", 181),
		observed("drone", "aaaa", 181),
		observed("buzz", "aaaa", 181),
		observed("sting", "aaaa", 181),
	})
	view := views["brood"]
	if view.Agreeing != 3 {
		t.Errorf("brood: Agreeing = %d, want 3 - it agrees with three other Combs, not with itself", view.Agreeing)
	}
	if !strings.Contains(view.Detail, "the 3 other Combs") {
		t.Errorf("brood: Detail = %q, want it to count three OTHER Combs", view.Detail)
	}
	if strings.Contains(view.Detail, "the 4 other Combs") {
		t.Errorf("brood: Detail = %q, counts this Comb among its own peers", view.Detail)
	}
}

// TestVerdictsProvenDivergence is the case the whole mechanism exists
// for: every Comb at the SAME applied index, holding different state.
// Nothing about timing can explain that away, so it must read as
// divergence rather than as unsettled.
func TestVerdictsProvenDivergence(t *testing.T) {
	views, colony := Verdicts([]Observation{
		observed("brood", "aaaa", 181),
		observed("drone", "aaaa", 181),
		observed("buzz", "aaaa", 181),
		observed("sting", "bbbb", 181),
	})

	if colony.Agreed {
		t.Error("Agreed = true, want false - sting holds different state at the same applied index")
	}
	if colony.Verdict != Mismatch {
		t.Errorf("colony Verdict = %q, want %q", colony.Verdict, Mismatch)
	}
	if !strings.Contains(colony.Detail, "diverged") {
		t.Errorf("Detail = %q, want it to say the state machines have diverged", colony.Detail)
	}
	if !colony.Verdict.Stops() {
		t.Error("a proven divergence is not reported as a stop")
	}
	if colony.Verdict.Permits() {
		t.Error("a proven divergence permits proceeding, which would roll a diverged colony through four restarts")
	}

	// EVERY observed row is marked, not just the odd one out. A green
	// verdict beside three red ones would read as "this Comb is fine",
	// and nothing here supports that: the colony has diverged, and which
	// side is wrong is not something a digest can say.
	for _, nodeID := range []string{"brood", "drone", "buzz", "sting"} {
		if views[nodeID].Verdict != Mismatch {
			t.Errorf("%s: Verdict = %q, want %q - a majority row must not be reported as agreeing while the colony has diverged",
				nodeID, views[nodeID].Verdict, Mismatch)
		}
	}
	// The minority row names its own position precisely.
	if detail := views["sting"].Detail; !strings.Contains(detail, "matches 0 of the 3") {
		t.Errorf("sting: Detail = %q, want it to state that it matches none of the other three", detail)
	}
	// And a majority row says the same in its own terms, rather than
	// claiming to be correct.
	if detail := views["brood"].Detail; !strings.Contains(detail, "matches 2 of the 3") {
		t.Errorf("brood: Detail = %q, want it to state that it matches two of the other three", detail)
	}
	if views["brood"].Agreeing != 2 || views["brood"].Others != 1 {
		t.Errorf("brood: Agreeing/Others = %d/%d, want 2/1", views["brood"].Agreeing, views["brood"].Others)
	}
}

// TestVerdictsDoNotLocaliseTheFault pins the ADR's own limitation as a
// test. A digest says the state machines differ; it cannot say which is
// wrong, so no row's detail may name another Comb or assert that any Comb
// is at fault.
func TestVerdictsDoNotLocaliseTheFault(t *testing.T) {
	views, _ := Verdicts([]Observation{
		observed("brood", "aaaa", 181),
		observed("drone", "aaaa", 181),
		observed("buzz", "bbbb", 181),
	})
	for nodeID, view := range views {
		for _, other := range []string{"brood", "drone", "buzz"} {
			if other != nodeID && strings.Contains(view.Detail, other) {
				t.Errorf("%s: Detail = %q, names %s - the digest cannot say which side is wrong", nodeID, view.Detail, other)
			}
		}
		// Assertive claims the evidence cannot support. "wrong" on its own
		// is deliberately not in this list: the detail's own disclaimer
		// says the digest does not say which side is wrong, and the
		// presence of that disclaimer is asserted below.
		for _, claim := range []string{"is incorrect", "should be", "is stale", "is behind", "is faulty", "culprit", "diverged because"} {
			if strings.Contains(view.Detail, claim) {
				t.Errorf("%s: Detail = %q, asserts %q - that is a conclusion the evidence does not support", nodeID, view.Detail, claim)
			}
		}
		// And the disclaimer itself is required on every diverged row, so
		// the limitation cannot be quietly dropped from the rendering.
		if !strings.Contains(view.Detail, "does not say which side is wrong") {
			t.Errorf("%s: Detail = %q, want the explicit statement that the digest cannot identify the wrong side", nodeID, view.Detail)
		}
	}
}

// TestVerdictsMovingColonyIsUnsettled covers the sampling race the ADR
// names. A mismatch at differing applied indexes may be nothing but the
// sample being taken while entries were still being applied, and
// reporting it as divergence would be a false alarm on a healthy colony.
func TestVerdictsMovingColonyIsUnsettled(t *testing.T) {
	views, colony := Verdicts([]Observation{
		observed("brood", "aaaa", 181),
		observed("drone", "aaaa", 181),
		observed("buzz", "bbbb", 182),
	})

	if colony.Agreed {
		t.Error("Agreed = true, want false - the digests are not all equal")
	}
	if colony.Verdict != Unsettled {
		t.Errorf("colony Verdict = %q, want %q", colony.Verdict, Unsettled)
	}
	if !strings.Contains(colony.Detail, "different applied indexes") {
		t.Errorf("Detail = %q, want it to name the differing applied indexes as the reason", colony.Detail)
	}
	for _, nodeID := range []string{"brood", "drone", "buzz"} {
		if views[nodeID].Verdict != Unsettled {
			t.Errorf("%s: Verdict = %q, want %q", nodeID, views[nodeID].Verdict, Unsettled)
		}
		if !strings.Contains(views[nodeID].Detail, "moving") {
			t.Errorf("%s: Detail = %q, want it to say this may be a sample of a moving colony", nodeID, views[nodeID].Detail)
		}
	}
	// Unsettled is the one verdict that is neither permission nor a stop:
	// the colony is probably just moving, so the answer is to wait and
	// read again. Getting this wrong in either direction is the same
	// mistake - treating a moving sample as a decision.
	if Unsettled.Permits() {
		t.Error("unsettled permits proceeding")
	}
	if Unsettled.Stops() {
		t.Error("unsettled is reported as a stop, want a wait-and-re-read")
	}
}

// TestVerdictsEqualDigestsAtDifferingIndexesStillMatch closes the other
// side of the moving-colony case. A colony whose digests all agree is in
// agreement whatever its applied indexes are doing; the indexes only
// matter when the digests already differ.
func TestVerdictsEqualDigestsAtDifferingIndexesStillMatch(t *testing.T) {
	_, colony := Verdicts([]Observation{
		observed("brood", "aaaa", 181),
		observed("drone", "aaaa", 182),
		observed("buzz", "aaaa", 183),
	})
	if colony.Verdict != Match {
		t.Errorf("colony Verdict = %q, want %q - identical digests agree at any applied index", colony.Verdict, Match)
	}
}

// TestVerdictsSingleObservationIsNeverAgreement is the most important
// negative case. One node always agrees with itself, so a one-node sample
// satisfies "all observed digests match" trivially. A green answer there
// would be the absence of evidence rendered as health, which is the exact
// mistake this package's health work exists to prevent.
func TestVerdictsSingleObservationIsNeverAgreement(t *testing.T) {
	views, colony := Verdicts([]Observation{
		observed("brood", "aaaa", 181),
		unobservedDigest("drone"),
		unobservedDigest("buzz"),
	})

	if colony.Agreed {
		t.Error("Agreed = true from a single observation, want false - one voter always agrees with itself")
	}
	if colony.Verdict != Unobserved {
		t.Errorf("colony Verdict = %q, want %q", colony.Verdict, Unobserved)
	}
	if colony.Observed != 1 || colony.Total != 3 {
		t.Errorf("Observed/Total = %d/%d, want 1/3", colony.Observed, colony.Total)
	}
	brood := views["brood"]
	if brood.Verdict != Unobserved {
		t.Errorf("brood: Verdict = %q, want %q - its own digest is not evidence of agreement", brood.Verdict, Unobserved)
	}
	if !strings.Contains(brood.Detail, "no other Comb did") {
		t.Errorf("brood: Detail = %q, want it to say there was nothing to compare against", brood.Detail)
	}
	// The colony is unobserved, so a controller must not proceed. This is
	// the fail-open direction, and it is the one that matters.
	if colony.Verdict.Permits() {
		t.Error("a single observation permits proceeding")
	}
	if !colony.Verdict.Stops() {
		t.Error("a single observation is not reported as a stop, want one - no evidence is not permission")
	}
}

func TestVerdictsNoObservationsAtAll(t *testing.T) {
	views, colony := Verdicts([]Observation{
		unobservedDigest("brood"),
		unobservedDigest("drone"),
	})

	if colony.Agreed || colony.Verdict != Unobserved || colony.Detail == "" {
		t.Errorf("colony = %+v, want no agreement, an unobserved verdict and a stated reason", colony)
	}
	if !strings.Contains(colony.Detail, "no Comb reported") {
		t.Errorf("Detail = %q, want it to state that nothing was observed", colony.Detail)
	}
	for nodeID, view := range views {
		if view.Verdict != Unobserved {
			t.Errorf("%s: Verdict = %q, want %q", nodeID, view.Verdict, Unobserved)
		}
		if !strings.Contains(view.Detail, "could not be read") {
			t.Errorf("%s: Detail = %q, want it to say the digest could not be read", nodeID, view.Detail)
		}
	}
}

// TestVerdictsUnobservedCombIsExcludedFromAgreement covers the
// half-heard case: two Combs agree and a third could not be read. The two
// may match, but the colony sentence must not imply all three were
// compared, and the unreadable Comb must not be marked as agreeing.
func TestVerdictsUnobservedCombIsExcludedFromAgreement(t *testing.T) {
	views, colony := Verdicts([]Observation{
		observed("brood", "aaaa", 181),
		observed("drone", "aaaa", 181),
		unobservedDigest("sting"),
	})

	if !colony.Agreed {
		t.Error("Agreed = false, want true - both digests that were read are identical")
	}
	if !strings.Contains(colony.Detail, "excluded from the comparison") {
		t.Errorf("Detail = %q, want it to say one Comb was excluded", colony.Detail)
	}
	if colony.Observed != 2 || colony.Total != 3 {
		t.Errorf("Observed/Total = %d/%d, want 2/3", colony.Observed, colony.Total)
	}
	if views["sting"].Verdict != Unobserved {
		t.Errorf("sting: Verdict = %q, want %q - an unread digest is never folded into the match", views["sting"].Verdict, Unobserved)
	}
	if !strings.Contains(views["sting"].Detail, "3 Combs") {
		t.Errorf("sting: Detail = %q, want it to name the size of the comparison it was excluded from", views["sting"].Detail)
	}
	// And the two that were read are told the set was partial, on their
	// own rows as well as in the colony sentence.
	for _, nodeID := range []string{"brood", "drone"} {
		if views[nodeID].Total != 3 {
			t.Errorf("%s: Total = %d, want 3 so the row knows the comparison was partial", nodeID, views[nodeID].Total)
		}
	}
}

// TestVerdictsAreOrderIndependent guards the one assumption the rows
// depend on: a Combs position in the input must not change its verdict,
// or the badge would flicker with Go's map and slice ordering.
func TestVerdictsAreOrderIndependent(t *testing.T) {
	forward := []Observation{
		observed("brood", "aaaa", 181),
		observed("drone", "aaaa", 181),
		observed("buzz", "bbbb", 181),
		unobservedDigest("sting"),
	}
	reversed := []Observation{
		unobservedDigest("sting"),
		observed("buzz", "bbbb", 181),
		observed("drone", "aaaa", 181),
		observed("brood", "aaaa", 181),
	}

	firstViews, firstColony := Verdicts(forward)
	secondViews, secondColony := Verdicts(reversed)

	if firstColony != secondColony {
		t.Errorf("colony verdict differs by input order:\n%+v\n%+v", firstColony, secondColony)
	}
	for nodeID, want := range firstViews {
		got := secondViews[nodeID]
		if got != want {
			t.Errorf("%s: verdict differs by input order:\n got %+v\nwant %+v", nodeID, got, want)
		}
	}
}

// TestVerdictsEmptyInputNamesItself covers the degenerate call, so a page
// with no Combs at all renders a stated verdict rather than an empty one.
func TestVerdictsEmptyInput(t *testing.T) {
	views, colony := Verdicts(nil)
	if len(views) != 0 {
		t.Errorf("views = %+v, want empty", views)
	}
	if colony.Agreed || colony.Verdict != Unobserved || colony.Observed != 0 || colony.Total != 0 {
		t.Errorf("colony = %+v, want no agreement and no observations", colony)
	}
	if colony.Detail == "" {
		t.Error("colony Detail is empty, want a stated reason even for the empty case")
	}
	if colony.Verdict.Permits() {
		t.Error("an empty set of observations permits proceeding")
	}
}

// TestVerdictPermitsAndStopsAreComplementary pins the contract a caller
// relies on: Permits and Stops must never both be true, every verdict
// must land on exactly one side, and the two verdicts that are neither -
// which is only unsettled - must be the wait-and-re-read case.
func TestVerdictPermitsAndStopsAreComplementary(t *testing.T) {
	all := []Verdict{Match, Mismatch, Unsettled, Unobserved}
	for _, v := range all {
		if v.Permits() && v.Stops() {
			t.Errorf("%q both permits and stops, so a caller cannot tell what to do", v)
		}
	}
	if Match != "match" || Mismatch != "mismatch" || Unsettled != "unsettled" || Unobserved != "unobserved" {
		t.Error("the four verdict strings changed; they are the values ADR-0143, the colony view and any future wire format all name")
	}
}

// TestUnobservedNodeIsUsableWithoutASet covers the exported single-Comb
// helper. A consumer holding one Comb's digest and no others still owes
// it an honest row rather than a blank one, and this is that row.
func TestUnobservedNodeIsUsableWithoutASet(t *testing.T) {
	// A digest that WAS read, with nothing to compare it against.
	read := UnobservedNode(Observation{NodeID: "brood", Digest: "aaaa", AppliedIndex: 181}, 4)
	if read.Verdict != Unobserved {
		t.Errorf("Verdict = %q, want %q", read.Verdict, Unobserved)
	}
	if read.Digest != "aaaa" || read.AppliedIndex != 181 {
		t.Errorf("Digest/AppliedIndex = %q/%d, want the reading itself carried through - dropping it would tell an "+
			"operator to go looking for a missing voter when the digest was read fine", read.Digest, read.AppliedIndex)
	}
	if !strings.Contains(read.Detail, "no other Comb did") {
		t.Errorf("Detail = %q, want it to say there was nothing to compare against", read.Detail)
	}

	// A digest that could not be read at all.
	missing := UnobservedNode(Observation{NodeID: "drone"}, 4)
	if missing.Digest != "" {
		t.Errorf("Digest = %q, want empty for a Comb that was not read", missing.Digest)
	}
	if missing.Observed != 0 {
		t.Errorf("Observed = %d, want 0 - a Comb that could not be read has no place in the count of Combs that were", missing.Observed)
	}
	if !strings.Contains(missing.Detail, "could not be read") {
		t.Errorf("Detail = %q, want it to say the digest could not be read", missing.Detail)
	}
	if !strings.Contains(missing.Detail, "4 Combs") {
		t.Errorf("Detail = %q, want it to name the size of the comparison", missing.Detail)
	}
	if missing.Detail == "" {
		t.Error("Detail is empty, want it always populated")
	}
}
