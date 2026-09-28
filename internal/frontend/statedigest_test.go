package frontend

import (
	"strings"
	"testing"

	"github.com/glenjbarber/apiary/internal/statedigest"
)

// The verdicts themselves are tested in internal/statedigest, where they
// now live. What is here is the half that is this package's own: that
// every verdict reaches a rendered row, that it lands in
// layout.html's palette, and that no verdict renders as an unstyled row
// that would read as a plain label instead of a conclusion.

func renderObservation(nodeID, digest string, index uint64) statedigest.Observation {
	return statedigest.Observation{NodeID: nodeID, Digest: digest, AppliedIndex: index}
}

func renderUnobserved(nodeID string) statedigest.Observation {
	return statedigest.Observation{NodeID: nodeID}
}

// everyVerdict reaches all four ADR-0143 verdicts through real
// comparisons and returns one representative rendered row per verdict.
//
// It takes four comparisons rather than one because a comparison produces
// a single SET-level verdict: no single set of observations can contain
// all four at once. Building the verdicts by hand instead would test the
// mapping against values the comparison cannot actually produce, which is
// exactly the mistake a rendering layer invites once the classification
// lives somewhere else.
func everyVerdict() map[statedigest.Verdict]stateDigestView {
	comparisons := [][]statedigest.Observation{
		// equal digests, equal indexes
		{
			renderObservation("brood", "aaaa", 181),
			renderObservation("drone", "aaaa", 181),
		},
		// differing digests, equal indexes
		{
			renderObservation("brood", "aaaa", 181),
			renderObservation("drone", "aaaa", 181),
			renderObservation("buzz", "bbbb", 181),
		},
		// differing digests, differing indexes
		{
			renderObservation("brood", "aaaa", 181),
			renderObservation("drone", "aaaa", 181),
			renderObservation("buzz", "bbbb", 181),
			renderObservation("sting", "cccc", 182),
		},
		// one reading, and nothing to compare it against
		{
			renderObservation("brood", "aaaa", 181),
			renderUnobserved("drone"),
			renderUnobserved("comb"),
		},
	}

	// Rows are walked in SLICE order, not in map order. stateDigestVerdicts
	// returns a map, so iterating it would pick an arbitrary row for each
	// verdict and two of the four Unobserved rows below say different
	// (equally honest) things - which would make this test pass or fail on
	// Go's map iteration randomization.
	out := map[statedigest.Verdict]stateDigestView{}
	for _, observations := range comparisons {
		views, _ := stateDigestVerdicts(observations)
		for _, observation := range observations {
			view := views[observation.NodeID]
			verdict := statedigest.Verdict(view.State)
			if _, already := out[verdict]; already {
				continue
			}
			out[verdict] = view
		}
	}
	return out
}

// TestStateDigestVerdictsUseOnlyTheKnownBadgeVocabulary keeps the badge
// classes inside layout.html's palette. A class invented here would render
// as unstyled text, which on this page would look like a plain label
// rather than a verdict.
func TestStateDigestVerdictsUseOnlyTheKnownBadgeVocabulary(t *testing.T) {
	known := map[string]bool{"ready": true, "degraded": true, "error": true, "unknown": true}

	cases := [][]statedigest.Observation{
		{renderObservation("brood", "aaaa", 181), renderObservation("drone", "aaaa", 181)},
		{renderObservation("brood", "aaaa", 181), renderObservation("drone", "bbbb", 181)},
		{renderObservation("brood", "aaaa", 181), renderObservation("drone", "bbbb", 182)},
		{renderObservation("brood", "aaaa", 181), renderUnobserved("drone")},
		{renderUnobserved("brood"), renderUnobserved("drone")},
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

// TestStateDigestBadgeClassDistinguishesEveryVerdict is the reason the
// badge class is chosen by an exhaustive switch rather than derived. All
// four verdicts must be visually distinct, and specifically a Mismatch
// must never be able to render as the same colour as a Match: a
// diverged colony shown in the same green as an agreed one is the exact
// failure the badge exists to prevent.
func TestStateDigestBadgeClassDistinguishesEveryVerdict(t *testing.T) {
	views := everyVerdict()

	want := map[statedigest.Verdict]string{
		statedigest.Match:      "ready",
		statedigest.Mismatch:   "error",
		statedigest.Unsettled:  "degraded",
		statedigest.Unobserved: "unknown",
	}
	for verdict, wantClass := range want {
		view, found := views[verdict]
		if !found {
			t.Errorf("verdict %q never appeared in a comparison, so its badge is unpinned", verdict)
			continue
		}
		if view.BadgeClass != wantClass {
			t.Errorf("verdict %q rendered as %q, want %q", verdict, view.BadgeClass, wantClass)
		}
		if view.Label == "" {
			t.Errorf("verdict %q rendered with an empty label, want it always populated", verdict)
		}
	}
	if len(views) != len(want) {
		t.Errorf("everyVerdict() produced %d verdicts, want exactly the %d ADR-0143 defines", len(views), len(want))
	}
}

// TestStateDigestRowStateCarriesTheVerdictNotAPresentationValue checks
// that State is the verdict itself. A template keying off State, or a
// test asserting on it, must be reading the upstream answer rather than
// a second opinion formed here.
func TestStateDigestRowStateCarriesTheVerdictNotAPresentationValue(t *testing.T) {
	for verdict, view := range everyVerdict() {
		if view.State != string(verdict) {
			t.Errorf("verdict %q rendered with State = %q, want the row's State to BE the verdict", verdict, view.State)
		}
		if view.Label == view.State {
			t.Errorf("verdict %q: Label and State are both %q, want a short badge text distinct from the verdict",
				verdict, view.Label)
		}
	}
}

// TestStateDigestDetailIsCarriedThroughNotReworded guards the split made
// when the verdicts moved: the sentence behind a verdict is the same
// sentence a controller reads, so this package must not restate it. In
// particular the mismatch row's disclaimer - that a digest cannot say
// which side is wrong - is a correctness property of the evidence, not a
// phrasing choice.
func TestStateDigestDetailIsCarriedThroughNotReworded(t *testing.T) {
	views := everyVerdict()
	for verdict, view := range views {
		if view.Detail == "" {
			t.Errorf("verdict %q: Detail is empty, want the sentence carried through from internal/statedigest", verdict)
		}
	}
	if d := views[statedigest.Mismatch].Detail; !strings.Contains(d, "does not say which side is wrong") {
		t.Errorf("mismatch Detail = %q, want the disclaimer that the digest cannot identify the wrong side, carried through unchanged", d)
	}
	if d := views[statedigest.Unsettled].Detail; !strings.Contains(d, "moving") {
		t.Errorf("unsettled Detail = %q, want the moving-colony wording carried through unchanged", d)
	}
	// The representative unobserved row is the only Comb that reported a
	// digest in a one-reading comparison, so its wording is the
	// "nothing to compare it against" one. The other unobserved wording -
	// a digest that was never read - is asserted below.
	if d := views[statedigest.Unobserved].Detail; !strings.Contains(d, "no other Comb did") {
		t.Errorf("unobserved Detail = %q, want the incomparable wording carried through unchanged", d)
	}
}

// TestStateDigestUnobservedViewIsNeverBlank covers the single-Comb path
// used while a page's rows are still being gathered, before any
// comparison exists. The row must already carry a class and a label, or
// the template renders an element with no badge vocabulary at all - which
// reads as "nothing to report" rather than "not observed".
func TestStateDigestUnobservedViewIsNeverBlank(t *testing.T) {
	view := stateDigestUnobservedView(statedigest.Observation{
		NodeID: "brood.lab3.home.arpa", Digest: "aaaa", AppliedIndex: 181,
	}, 1)
	if view.State != string(statedigest.Unobserved) {
		t.Errorf("State = %q, want %q", view.State, statedigest.Unobserved)
	}
	if view.BadgeClass == "" || view.Label == "" || view.Detail == "" {
		t.Errorf("view = %+v, want class, label and detail all populated", view)
	}
	if view.StateDigest != "aaaa" || view.AppliedIndex != 181 {
		t.Errorf("StateDigest/AppliedIndex = %q/%d, want the reading carried through - a real digest beside a "+
			"fabricated zero index is worse than no row at all", view.StateDigest, view.AppliedIndex)
	}
	// The other unobserved wording, reached through the same path: a
	// digest that was never read. It is the wording that tells an
	// operator to go looking, and it must be carried through too.
	neverRead := stateDigestUnobservedView(statedigest.Observation{
		NodeID: "drone.lab3.home.arpa",
	}, 1)
	if !strings.Contains(neverRead.Detail, "could not be read") {
		t.Errorf("Detail = %q, want the unread wording carried through unchanged", neverRead.Detail)
	}
}

// TestStateDigestColonyBadgeLabelIsDistinctPerVerdict checks the colony
// panel's own label, which is a different vocabulary from the per-row
// one: a colony is "agreed" or "diverged", where a row "matches" or
// "differs". Collapsing them would let a reader see "State agreed" under
// a column of red rows.
func TestStateDigestColonyBadgeLabelIsDistinctPerVerdict(t *testing.T) {
	cases := []struct {
		name         string
		observations []statedigest.Observation
		wantClass    string
		wantLabel    string
	}{
		{"match", []statedigest.Observation{
			renderObservation("brood", "aaaa", 181), renderObservation("drone", "aaaa", 181),
		}, "ready", colonyAgreedLabel},
		{"mismatch", []statedigest.Observation{
			renderObservation("brood", "aaaa", 181), renderObservation("drone", "bbbb", 181),
		}, "error", colonyDivergedLabel},
		{"unsettled", []statedigest.Observation{
			renderObservation("brood", "aaaa", 181), renderObservation("drone", "bbbb", 182),
		}, "degraded", colonyUnsettledLabel},
		{"unobserved", []statedigest.Observation{
			renderObservation("brood", "aaaa", 181), renderUnobserved("drone"),
		}, "unknown", "State unobserved"},
	}
	seen := map[string]string{}
	for _, c := range cases {
		_, colony := stateDigestVerdicts(c.observations)
		if colony.BadgeClass != c.wantClass {
			t.Errorf("%s: BadgeClass = %q, want %q", c.name, colony.BadgeClass, c.wantClass)
		}
		if colony.BadgeLabel != c.wantLabel {
			t.Errorf("%s: BadgeLabel = %q, want %q", c.name, colony.BadgeLabel, c.wantLabel)
		}
		if previous, duplicate := seen[colony.BadgeLabel]; duplicate {
			t.Errorf("%s and %s both render the colony badge %q, so the panel head cannot tell them apart",
				previous, c.name, colony.BadgeLabel)
		}
		seen[colony.BadgeLabel] = c.name
	}
}
