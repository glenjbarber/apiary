package frontend

import (
	"strings"
	"testing"
)

// These cover the consumer half of ADR-0143: what the colony view claims
// about cross-voter FSM state agreement, and - at least as importantly -
// what it refuses to claim. The states that matter are the ones a naive
// implementation gets wrong: a single observation, a sample taken while
// the colony moved, and a Comb that could not be read at all.

func observed(nodeID, digest string, index uint64) stateDigestObservation {
	return stateDigestObservation{NodeID: nodeID, Digest: digest, AppliedIndex: index}
}

func unobservedDigest(nodeID string) stateDigestObservation {
	return stateDigestObservation{NodeID: nodeID}
}

func TestStateDigestVerdictsAllAgree(t *testing.T) {
	views, colony := stateDigestVerdicts([]stateDigestObservation{
		observed("brood", "aaaa", 181),
		observed("drone", "aaaa", 181),
		observed("buzz", "aaaa", 181),
		observed("sting", "aaaa", 181),
	})

	if !colony.Agreed {
		t.Errorf("Agreed = false, want true - every reported digest is identical")
	}
	if colony.Observed != 4 || colony.Total != 4 {
		t.Errorf("Observed/Total = %d/%d, want 4/4", colony.Observed, colony.Total)
	}
	if colony.BadgeClass != "ready" || colony.BadgeLabel != colonyAgreedLabel {
		t.Errorf("BadgeClass/BadgeLabel = %q/%q, want ready/%q", colony.BadgeClass, colony.BadgeLabel, colonyAgreedLabel)
	}
	for _, nodeID := range []string{"brood", "drone", "buzz", "sting"} {
		view := views[nodeID]
		if view.State != stateDigestMatch || view.BadgeClass != "ready" {
			t.Errorf("%s: State/BadgeClass = %q/%q, want %q/ready", nodeID, view.State, view.BadgeClass, stateDigestMatch)
		}
		if view.Label == "" || view.Detail == "" {
			t.Errorf("%s: Label = %q, Detail = %q, want both populated - an unexplained badge is not actionable", nodeID, view.Label, view.Detail)
		}
	}
}

// TestStateDigestVerdictsProvenDivergence is the case the whole mechanism
// exists for: every Comb at the SAME applied index, holding different
// state. Nothing about timing can explain that away, so it must read as
// divergence rather than as unsettled.
func TestStateDigestVerdictsProvenDivergence(t *testing.T) {
	views, colony := stateDigestVerdicts([]stateDigestObservation{
		observed("brood", "aaaa", 181),
		observed("drone", "aaaa", 181),
		observed("buzz", "aaaa", 181),
		observed("sting", "bbbb", 181),
	})

	if colony.Agreed {
		t.Error("Agreed = true, want false - sting holds different state at the same applied index")
	}
	if colony.BadgeClass != "error" || colony.BadgeLabel != colonyDivergedLabel {
		t.Errorf("BadgeClass/BadgeLabel = %q/%q, want error/%q", colony.BadgeClass, colony.BadgeLabel, colonyDivergedLabel)
	}
	if !strings.Contains(colony.Detail, "diverged") {
		t.Errorf("Detail = %q, want it to say the state machines have diverged", colony.Detail)
	}

	// EVERY observed row is marked, not just the odd one out. A green
	// badge beside three red ones would read as "this Comb is fine", and
	// nothing here supports that: the colony has diverged, and which side
	// is wrong is not something a digest can say.
	for _, nodeID := range []string{"brood", "drone", "buzz", "sting"} {
		if views[nodeID].State != stateDigestMismatch {
			t.Errorf("%s: State = %q, want %q - a majority row must not be shown as agreeing while the colony has diverged",
				nodeID, views[nodeID].State, stateDigestMismatch)
		}
		if views[nodeID].BadgeClass != "error" {
			t.Errorf("%s: BadgeClass = %q, want error", nodeID, views[nodeID].BadgeClass)
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
}

// TestStateDigestVerdictsDoNotLocaliseTheFault pins the ADR's own
// limitation as a test. A digest says the state machines differ; it
// cannot say which is wrong, so no row's detail may name another Comb or
// assert that any Comb is at fault.
func TestStateDigestVerdictsDoNotLocaliseTheFault(t *testing.T) {
	views, _ := stateDigestVerdicts([]stateDigestObservation{
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

// TestStateDigestVerdictsMovingColonyIsUnsettled covers the sampling race
// the ADR names. A mismatch at differing applied indexes may be nothing
// but the sample being taken while entries were still being applied, and
// reporting it as divergence would be a false alarm on a healthy colony.
func TestStateDigestVerdictsMovingColonyIsUnsettled(t *testing.T) {
	views, colony := stateDigestVerdicts([]stateDigestObservation{
		observed("brood", "aaaa", 181),
		observed("drone", "aaaa", 181),
		observed("buzz", "bbbb", 182),
	})

	if colony.Agreed {
		t.Error("Agreed = true, want false - the digests are not all equal")
	}
	if colony.BadgeClass != "degraded" {
		t.Errorf("BadgeClass = %q, want degraded - differing applied indexes make this unsettled, not divergence", colony.BadgeClass)
	}
	if !strings.Contains(colony.Detail, "different applied indexes") {
		t.Errorf("Detail = %q, want it to name the differing applied indexes as the reason", colony.Detail)
	}
	for _, nodeID := range []string{"brood", "drone", "buzz"} {
		view := views[nodeID]
		if view.State != stateDigestUnsettled {
			t.Errorf("%s: State = %q, want %q", nodeID, view.State, stateDigestUnsettled)
		}
		if view.BadgeClass != "degraded" {
			t.Errorf("%s: BadgeClass = %q, want degraded", nodeID, view.BadgeClass)
		}
		if !strings.Contains(view.Detail, "moving") {
			t.Errorf("%s: Detail = %q, want it to say this may be a sample of a moving colony", nodeID, view.Detail)
		}
	}
}

// TestStateDigestVerdictsSingleObservationIsNeverAgreement is the most
// important negative case. One node always agrees with itself, so a
// one-node sample satisfies "all observed digests match" trivially. A
// green badge there would be the absence of evidence rendered as health,
// which is the exact mistake this package's health work exists to prevent.
func TestStateDigestVerdictsSingleObservationIsNeverAgreement(t *testing.T) {
	views, colony := stateDigestVerdicts([]stateDigestObservation{
		observed("brood", "aaaa", 181),
		unobservedDigest("drone"),
		unobservedDigest("buzz"),
	})

	if colony.Agreed {
		t.Error("Agreed = true from a single observation, want false - one voter always agrees with itself")
	}
	if colony.BadgeClass != "unknown" {
		t.Errorf("BadgeClass = %q, want unknown, not ready", colony.BadgeClass)
	}
	if colony.Observed != 1 || colony.Total != 3 {
		t.Errorf("Observed/Total = %d/%d, want 1/3", colony.Observed, colony.Total)
	}
	brood := views["brood"]
	if brood.State != stateDigestUnobserved || brood.BadgeClass != "unknown" {
		t.Errorf("brood: State/BadgeClass = %q/%q, want %q/unknown - its own digest is not evidence of agreement",
			brood.State, brood.BadgeClass, stateDigestUnobserved)
	}
	if !strings.Contains(brood.Detail, "no other Comb did") {
		t.Errorf("brood: Detail = %q, want it to say there was nothing to compare against", brood.Detail)
	}
}

func TestStateDigestVerdictsNoObservationsAtAll(t *testing.T) {
	views, colony := stateDigestVerdicts([]stateDigestObservation{
		unobservedDigest("brood"),
		unobservedDigest("drone"),
	})

	if colony.Agreed || colony.BadgeClass != "unknown" || colony.BadgeLabel == "" {
		t.Errorf("colony = %+v, want no agreement, unknown class and a populated label", colony)
	}
	if !strings.Contains(colony.Detail, "no Comb reported") {
		t.Errorf("Detail = %q, want it to state that nothing was observed", colony.Detail)
	}
	for nodeID, view := range views {
		if view.State != stateDigestUnobserved {
			t.Errorf("%s: State = %q, want %q", nodeID, view.State, stateDigestUnobserved)
		}
		if !strings.Contains(view.Detail, "could not be read") {
			t.Errorf("%s: Detail = %q, want it to say the digest could not be read", nodeID, view.Detail)
		}
	}
}

// TestStateDigestVerdictsUnobservedCombIsExcludedFromAgreement covers the
// half-heard case: two Combs agree and a third could not be read. The two
// may match, but the colony sentence must not imply all three were
// compared.
func TestStateDigestVerdictsUnobservedCombIsExcludedFromAgreement(t *testing.T) {
	views, colony := stateDigestVerdicts([]stateDigestObservation{
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
	if views["sting"].State != stateDigestUnobserved {
		t.Errorf("sting: State = %q, want %q - an unread digest is never folded into the match", views["sting"].State, stateDigestUnobserved)
	}
	if !strings.Contains(views["sting"].Detail, "3 Combs") {
		t.Errorf("sting: Detail = %q, want it to name the size of the comparison it was excluded from", views["sting"].Detail)
	}
}

// TestStateDigestVerdictsAreOrderIndependent guards the one assumption the
// rows depend on: a Combs position in the input must not change its
// verdict, or the badge would flicker with Go's map and slice ordering.
func TestStateDigestVerdictsAreOrderIndependent(t *testing.T) {
	forward := []stateDigestObservation{
		observed("brood", "aaaa", 181),
		observed("drone", "aaaa", 181),
		observed("buzz", "bbbb", 181),
		unobservedDigest("sting"),
	}
	reversed := []stateDigestObservation{
		unobservedDigest("sting"),
		observed("buzz", "bbbb", 181),
		observed("drone", "aaaa", 181),
		observed("brood", "aaaa", 181),
	}

	firstViews, firstColony := stateDigestVerdicts(forward)
	secondViews, secondColony := stateDigestVerdicts(reversed)

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

// TestStateDigestVerdictsUseOnlyTheKnownBadgeVocabulary keeps the badge
// classes inside layout.html's palette. A class invented here would render
// as unstyled text, which on this page would look like a plain label
// rather than a verdict.
func TestStateDigestVerdictsUseOnlyTheKnownBadgeVocabulary(t *testing.T) {
	known := map[string]bool{"ready": true, "degraded": true, "error": true, "unknown": true}

	cases := [][]stateDigestObservation{
		{observed("brood", "aaaa", 181), observed("drone", "aaaa", 181)},
		{observed("brood", "aaaa", 181), observed("drone", "bbbb", 181)},
		{observed("brood", "aaaa", 181), observed("drone", "bbbb", 182)},
		{observed("brood", "aaaa", 181), unobservedDigest("drone")},
		{unobservedDigest("brood"), unobservedDigest("drone")},
		{},
	}
	for _, observations := range cases {
		views, colony := stateDigestVerdicts(observations)
		if !known[colony.BadgeClass] {
			t.Errorf("colony BadgeClass = %q, want one of the known badge classes", colony.BadgeClass)
		}
		if colony.BadgeLabel == "" {
			t.Error("colony BadgeLabel is empty, want it always populated so the panel renders a badge")
		}
		for nodeID, view := range views {
			if !known[view.BadgeClass] {
				t.Errorf("%s: BadgeClass = %q, want one of the known badge classes", nodeID, view.BadgeClass)
			}
			if view.Label == "" {
				t.Errorf("%s: Label is empty, want it always populated", nodeID)
			}
			if view.Detail == "" {
				t.Errorf("%s: Detail is empty, want it always populated", nodeID)
			}
		}
	}
}

// TestStateDigestVerdictsEmptyInputNamesItself covers the degenerate call,
// so a page with no Combs at all renders a stated verdict rather than an
// empty one.
func TestStateDigestVerdictsEmptyInput(t *testing.T) {
	views, colony := stateDigestVerdicts(nil)
	if len(views) != 0 {
		t.Errorf("views = %+v, want empty", views)
	}
	if colony.Agreed || colony.Observed != 0 || colony.Total != 0 {
		t.Errorf("colony = %+v, want no agreement and no observations", colony)
	}
	if colony.BadgeClass != "unknown" || colony.Detail == "" {
		t.Errorf("colony = %+v, want an unknown class and a stated reason", colony)
	}
}
