package pf

import (
	"context"
	"os"
	"strings"
	"testing"
)

// Read-back drift: after a load, read the running ruleset back and
// compare it against the last-known-good record. The whole model is
// pure, so all four states - and, importantly, the states that are not
// "in sync" - are testable with no pfctl, no root and no network.

func loaded(t *testing.T, m *Manager, anchor string, rules []Rule) {
	t.Helper()
	if err := m.Apply(context.Background(), anchor, rules); err != nil {
		t.Fatalf("Apply() error: %v", err)
	}
}

// managerWithReadback returns a Manager whose exec answers `pfctl -sr`
// (the read-back) with running, and treats any other invocation as a
// successful load. This is the seam that makes the read-back testable
// on a machine with no pf in it.
func managerWithReadback(t *testing.T, running string, readbackErr error) *Manager {
	t.Helper()
	m, _ := newTestManager(t)
	m.exec = func(_ context.Context, stdin, name string, args ...string) (string, error) {
		if len(args) >= 2 && args[len(args)-1] == "-sr" {
			if readbackErr != nil {
				return "", readbackErr
			}
			return running, nil
		}
		_ = stdin
		return "", nil
	}
	return m
}

func TestDrift_InSyncAfterASuccessfulLoad(t *testing.T) {
	rules := []Rule{{Direction: "in", Action: "pass", Protocol: "tcp", PortRange: "22", Interface: "vtnet0", Any: true}}
	running, err := RenderRules(rules)
	if err != nil {
		t.Fatalf("RenderRules() error: %v", err)
	}
	m := managerWithReadback(t, running, nil)
	anchor := "apiary/vm-1"

	loaded(t, m, anchor, rules)

	d := m.CheckDrift(context.Background(), anchor)
	if d.State != DriftInSync {
		t.Fatalf("CheckDrift() = %+v, want %q", d, DriftInSync)
	}
	if !d.InSync() {
		t.Error("InSync() = false for a confirmed match")
	}
	// The observation made during Apply is retained, so the verdict
	// survives the call that made it.
	last, ok := m.LastDrift(anchor)
	if !ok {
		t.Fatal("LastDrift() = false; the post-load observation was dropped instead of being retained")
	}
	if last.State != DriftInSync {
		t.Errorf("LastDrift() = %q, want %q", last.State, DriftInSync)
	}
}

// TestDrift_FailedReadBackIsUnknownNeverInSync is the test that matters
// most in this file. A missing pfctl, a permission error, a pfctl that
// exits non-zero - all of them mean "we did not look", and a model
// that folds them into "in sync" reports a firewall nobody has
// checked.
func TestDrift_FailedReadBackIsUnknownNeverInSync(t *testing.T) {
	for _, readbackErr := range []error{
		&execError{name: "pfctl", msg: "pfctl: not found"},
		&execError{name: "pfctl", msg: "pfctl: Permission denied"},
		&execError{name: "pfctl", msg: "pfctl: Anchor 'apiary/vm-1' does not exist."},
	} {
		m := managerWithReadback(t, "", readbackErr)
		anchor := "apiary/vm-1"
		// Record a baseline directly: Apply's own load would have
		// failed to read back too, which is the same state, but this
		// keeps the test about CheckDrift's classification.
		if err := m.knownGood().record(anchor, "block in proto tcp from any to any port 22\n"); err != nil {
			t.Fatalf("record() error: %v", err)
		}

		d := m.CheckDrift(context.Background(), anchor)
		if d.State != DriftUnknown {
			t.Errorf("readback err %q: state = %q, want %q", readbackErr, d.State, DriftUnknown)
		}
		if d.InSync() {
			t.Errorf("readback err %q: InSync() = true; a failed read must never read as in sync", readbackErr)
		}
		if d.Detail == "" {
			t.Errorf("readback err %q: no detail; an operator cannot act on an unexplained unknown", readbackErr)
		}
		if !strings.Contains(d.String(), "UNKNOWN") {
			t.Errorf("readback err %q: String() = %q, want it to say UNKNOWN", readbackErr, d.String())
		}
	}
}

func TestDrift_DetectedWhenTheRunningRulesetDiffers(t *testing.T) {
	rules := []Rule{{Direction: "in", Action: "pass", Protocol: "tcp", PortRange: "22", Any: true}}
	applied, err := RenderRules(rules)
	if err != nil {
		t.Fatalf("RenderRules() error: %v", err)
	}
	// Someone hand-edited the anchor, or the load only partly took.
	running := "block in proto tcp from any to any port 22\n" + applied
	m := managerWithReadback(t, running, nil)
	anchor := "apiary/vm-1"
	loaded(t, m, anchor, rules)

	d := m.CheckDrift(context.Background(), anchor)
	if d.State != DriftDetected {
		t.Fatalf("CheckDrift() = %+v, want %q", d, DriftDetected)
	}
	if d.InSync() {
		t.Error("InSync() = true for a ruleset that differs")
	}
	if d.Expected == d.Observed {
		t.Error("a drift result must carry both sides, or the operator cannot see what differs")
	}
	if d.Detail == "" {
		t.Error("drift must name the difference, not just assert that one exists")
	}
	if !strings.Contains(d.String(), "DRIFT") {
		t.Errorf("String() = %q, want it to shout DRIFT", d.String())
	}
}

func TestDrift_NotLoadedWhenThereIsNoBaseline(t *testing.T) {
	m := managerWithReadback(t, "block in from any to any\n", nil)
	anchor := "apiary/vm-never"

	d := m.CheckDrift(context.Background(), anchor)
	if d.State != DriftNotLoaded {
		t.Fatalf("CheckDrift() = %+v, want %q", d, DriftNotLoaded)
	}
	if d.InSync() {
		t.Error("InSync() = true with no baseline to compare against")
	}
	if d.Detail == "" {
		t.Error("not-loaded must say why there is nothing to compare against")
	}
}

// TestDrift_CorruptBaselineIsUnknownNotDrift: a record we cannot read
// is not evidence of a change in pf. It is evidence that we cannot
// see. Reporting it as drift would send an operator hunting for a
// pf problem they do not have; reporting it as in sync would be a
// lie about a firewall whose baseline is gone.
func TestDrift_CorruptBaselineIsUnknownNotDrift(t *testing.T) {
	m := managerWithReadback(t, "block in from any to any\n", nil)
	anchor := "apiary/vm-corrupt"
	if err := m.knownGood().record(anchor, "block in from any to any\n"); err != nil {
		t.Fatalf("record() error: %v", err)
	}
	path, err := m.knownGood().path(anchor)
	if err != nil {
		t.Fatalf("path() error: %v", err)
	}
	if err := os.WriteFile(path, []byte("garbage\n"), 0o600); err != nil {
		t.Fatalf("WriteFile() error: %v", err)
	}

	d := m.CheckDrift(context.Background(), anchor)
	if d.State != DriftUnknown {
		t.Fatalf("CheckDrift() = %+v, want %q", d, DriftUnknown)
	}
	if d.InSync() || d.State == DriftDetected {
		t.Error("a corrupt baseline must be neither in sync nor detected drift")
	}
	if d.KnownGood != KnownGoodCorrupt {
		t.Errorf("KnownGood = %q, want %q", d.KnownGood, KnownGoodCorrupt)
	}
}

func TestDrift_EmptyBaselineComparesAgainstNothing(t *testing.T) {
	m := managerWithReadback(t, "", nil)
	anchor := "apiary/vm-empty"
	loaded(t, m, anchor, nil)

	inSync := m.CheckDrift(context.Background(), anchor)
	if inSync.State != DriftInSync {
		t.Errorf("empty ruleset, nothing running: state = %q, want %q", inSync.State, DriftInSync)
	}

	// ...and a rule that has appeared since is drift, not "we have
	// nothing to compare".
	m.exec = func(_ context.Context, _ string, _ string, args ...string) (string, error) {
		if len(args) >= 1 && args[len(args)-1] == "-sr" {
			return "pass in from any to any\n", nil
		}
		return "", nil
	}
	drifted := m.CheckDrift(context.Background(), anchor)
	if drifted.State != DriftDetected {
		t.Errorf("empty ruleset with a rule added out of band: state = %q, want %q", drifted.State, DriftDetected)
	}
}

// TestDrift_CanonicalisationIgnoresWhitespaceAndCommentsOnly: pfctl's
// output formatting is not a stable API, but reformatting must not read
// as drift. Sorting, by contrast, must - rule order is pf's
// evaluation order (ADR-0075).
func TestDrift_CanonicalisationIgnoresWhitespaceAndCommentsOnly(t *testing.T) {
	same := [][2]string{
		{"block in from any to any\n", "block  in   from any to any\n"},
		{"block in from any to any\n", "# a comment\nblock in from any to any\n"},
		{"block in from any to any\nblock out to any\n", "block in from any to any\n\nblock out to any\n\n"},
	}
	for _, pair := range same {
		if canonicalRules(pair[0]) != canonicalRules(pair[1]) {
			t.Errorf("canonicalRules(%q) = %q, canonicalRules(%q) = %q; want equal", pair[0], canonicalRules(pair[0]), pair[1], canonicalRules(pair[1]))
		}
	}
	different := [][2]string{
		{"block in from any to any\n", "pass in from any to any\n"},
		{"block in\nblock out\n", "block out\nblock in\n"},
	}
	for _, pair := range different {
		if canonicalRules(pair[0]) == canonicalRules(pair[1]) {
			t.Errorf("canonicalRules(%q) == canonicalRules(%q) = %q; want them to differ", pair[0], pair[1], canonicalRules(pair[0]))
		}
	}
}

func TestDrift_DisableReadbackSkipsTheSecondExec(t *testing.T) {
	m, calls := newTestManager(t)
	m.DisableReadback = true
	anchor := "apiary/vm-1"
	if err := m.Apply(context.Background(), anchor, []Rule{{Direction: "in", Action: "block", Any: true}}); err != nil {
		t.Fatalf("Apply() error: %v", err)
	}
	for _, c := range *calls {
		if strings.HasSuffix(c[1], "-sr") {
			t.Errorf("DisableReadback did not disable the read-back: %v", *calls)
		}
	}
	// The record is still written: read-back is a performance valve,
	// not a way to lose the baseline.
	if _, state, err := m.knownGood().load(anchor); err != nil || state != KnownGoodLoaded {
		t.Errorf("state = %q (err %v), want %q even with the read-back disabled", state, err, KnownGoodLoaded)
	}
	// ...and an explicit check still works.
	m.DisableReadback = false
	m.exec = func(_ context.Context, _ string, _ string, args ...string) (string, error) {
		if len(args) >= 1 && args[len(args)-1] == "-sr" {
			return "block in from any to any\n", nil
		}
		return "", nil
	}
	if d := m.CheckDrift(context.Background(), anchor); d.State != DriftInSync {
		t.Errorf("CheckDrift() = %q, want %q", d.State, DriftInSync)
	}
}

// TestDrift_ApplyRetainsADriftVerdictItDidNotActOn: Apply does not
// fail the tick when a read-back reports drift, because a transient
// pfctl difference is not a reason to claim the load failed. But the
// verdict is not dropped on the floor either - it is retained and
// readable, which is what "observable, not logged and dropped" means
// in practice.
func TestDrift_ApplyRetainsADriftVerdictItDidNotActOn(t *testing.T) {
	rules := []Rule{{Direction: "in", Action: "block", Any: true}}
	running, err := RenderRules(rules)
	if err != nil {
		t.Fatalf("RenderRules() error: %v", err)
	}
	m := managerWithReadback(t, running+"\npass in all\n", nil)
	anchor := "apiary/vm-1"
	if err := m.Apply(context.Background(), anchor, rules); err != nil {
		t.Fatalf("Apply() error: %v (a drift verdict must not fail the load)", err)
	}
	last, ok := m.LastDrift(anchor)
	if !ok {
		t.Fatal("no retained drift observation")
	}
	if last.State != DriftDetected {
		t.Errorf("LastDrift() = %q, want %q", last.State, DriftDetected)
	}
}
