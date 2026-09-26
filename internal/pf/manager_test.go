package pf

import (
	"context"
	"os"
	"strings"
	"testing"
)

// TestApplyNAT_RejectsNewlineInUplinkOrSubnet is the regression test for
// a 2026-09-06 security-audit finding: uplink (node-config data with no
// validation at the time) was interpolated verbatim into a pf ruleset
// loaded via `pfctl -a anchor -f -` - a newline would inject an
// arbitrary additional pf rule. This is checked before ApplyNAT ever
// shells out to pfctl, so it's testable without a real pf(8) binary.
func TestApplyNAT_RejectsNewlineInUplinkOrSubnet(t *testing.T) {
	m := &Manager{}
	ctx := context.Background()

	cases := []struct{ subnet, uplink string }{
		{subnet: "10.60.0.0/24", uplink: "re0\npass in all"},
		{subnet: "10.60.0.0/24\npass in all", uplink: "re0"},
	}
	for _, c := range cases {
		if err := m.ApplyNAT(ctx, "apiary/net-1", c.subnet, c.uplink); err == nil {
			t.Errorf("subnet=%q uplink=%q: ApplyNAT returned nil error, want a rejection", c.subnet, c.uplink)
		}
	}
}

// --- ADR-0137: the load path's evidence ------------------------------

// TestApply_RecordsTheLastKnownGoodAndReadsBack: the ordinary success
// path, end to end, with pfctl stood in for. It is the test that says
// "a load leaves two things behind: a durable record of what was
// loaded, and an observation of what pf is enforcing".
func TestApply_RecordsTheLastKnownGoodAndReadsBack(t *testing.T) {
	rules := []Rule{{Direction: "in", Action: "block", Protocol: "tcp", PortRange: "22", Interface: "vtnet0", Any: true}}
	running, err := RenderRules(rules)
	if err != nil {
		t.Fatalf("RenderRules() error: %v", err)
	}
	m := managerWithReadback(t, running, nil)
	anchor := "apiary/vm-1"
	ctx := context.Background()

	if err := m.Apply(ctx, anchor, rules); err != nil {
		t.Fatalf("Apply() error: %v", err)
	}

	rec, state, err := m.knownGood().load(anchor)
	if err != nil || state != KnownGoodLoaded {
		t.Fatalf("record state = %q (err %v), want %q", state, err, KnownGoodLoaded)
	}
	if rec.Body != running {
		t.Errorf("recorded body = %q, want the exact rendered ruleset %q", rec.Body, running)
	}
	if d, ok := m.LastDrift(anchor); !ok || d.State != DriftInSync {
		t.Errorf("LastDrift() = %+v (ok=%v), want %q", d, ok, DriftInSync)
	}
}

// TestApply_AnUnrenderableRulesetNeverReachesPfctl: the render happens
// first and is all-or-nothing, so a ruleset with one unscoped rule
// cannot be half-loaded into an anchor.
func TestApply_AnUnrenderableRulesetNeverReachesPfctl(t *testing.T) {
	m, calls := newTestManager(t)
	err := m.Apply(context.Background(), "apiary/vm-1", []Rule{
		{Direction: "in", Action: "pass", Protocol: "tcp", PortRange: "22", Any: true},
		{Direction: "in", Action: "block"}, // no scope
	})
	if err == nil {
		t.Fatal("Apply() with an unscoped rule = nil error, want a rejection")
	}
	if len(*calls) != 0 {
		t.Errorf("pfctl was invoked %d time(s) for a ruleset that does not render: %v", len(*calls), *calls)
	}
	if _, state, _ := m.knownGood().load("apiary/vm-1"); state != KnownGoodAbsent {
		t.Errorf("record state = %q, want %q - a ruleset that never loaded leaves no record", state, KnownGoodAbsent)
	}
}

// TestApply_AFailedLoadIsNotRecordedAndNotClaimedAsSuccess: the record
// is the last *successfully loaded* ruleset, so pfctl's refusal leaves
// both the previous record and the honest error in place.
func TestApply_AFailedLoadIsNotRecordedAndNotClaimedAsSuccess(t *testing.T) {
	m, _ := newTestManager(t)
	ctx := context.Background()
	anchor := "apiary/vm-1"
	rule := []Rule{{Direction: "in", Action: "block", Protocol: "tcp", PortRange: "22", Any: true}}
	if err := m.Apply(ctx, anchor, rule); err != nil {
		t.Fatalf("Apply() error: %v", err)
	}

	m.exec = func(_ context.Context, _ string, name string, _ ...string) (string, error) {
		return "", &execError{name: name, msg: "pfctl: /etc/pf.conf: syntax error"}
	}
	if err := m.Apply(ctx, anchor, []Rule{{Direction: "in", Action: "block", Protocol: "tcp", PortRange: "23", Any: true}}); err == nil {
		t.Fatal("Apply() with a failing pfctl = nil error, want one")
	}
	rec, state, err := m.knownGood().load(anchor)
	if err != nil || state != KnownGoodLoaded {
		t.Fatalf("state = %q (err %v), want the earlier successful load to still be the record", state, err)
	}
	if !strings.Contains(rec.Body, "port 22") {
		t.Errorf("record body = %q, want the first (successful) ruleset, not the failed one", rec.Body)
	}
}

// TestApply_AFailedRecordIsAnErrorEvenThoughTheLoadSucceeded: pf has
// the ruleset; the baseline every later drift check depends on does
// not. Reporting success there would be the worst of both worlds -
// an enforced ruleset nobody can prove, and a silent loss of the
// evidence this ADR exists to create.
func TestApply_AFailedRecordIsAnErrorEvenThoughTheLoadSucceeded(t *testing.T) {
	m, _ := newTestManager(t)
	// A path that cannot be created: a file where a directory must be.
	dir := t.TempDir() + "/not-a-dir"
	if err := os.WriteFile(dir, []byte("x"), 0o600); err != nil {
		t.Fatalf("WriteFile() error: %v", err)
	}
	m.KnownGoodDir = dir

	err := m.Apply(context.Background(), "apiary/vm-1", []Rule{{Direction: "in", Action: "block", Any: true}})
	if err == nil {
		t.Fatal("Apply() with an unwritable record store = nil error, want one")
	}
	if !strings.Contains(err.Error(), "last-known-good") {
		t.Errorf("error = %q, want it to name the record rather than looking like a pfctl failure", err)
	}
}

// TestApplyNAT_RecordsAndReadsBack: a NAT ruleset occupies the same
// anchor namespace and is loaded through the same path, so it gets the
// same evidence. A partial NAT load is just as invisible as a partial
// filter load, and worse to debug.
func TestApplyNAT_RecordsAndReadsBack(t *testing.T) {
	body := "match out on re0 from 10.60.0.0/24 to any nat-to (re0)\n"
	m := managerWithReadback(t, body, nil)
	anchor := "apiary/net-1"
	ctx := context.Background()

	if err := m.ApplyNAT(ctx, anchor, "10.60.0.0/24", "re0"); err != nil {
		t.Fatalf("ApplyNAT() error: %v", err)
	}
	rec, state, err := m.knownGood().load(anchor)
	if err != nil || state != KnownGoodLoaded {
		t.Fatalf("state = %q (err %v), want %q", state, err, KnownGoodLoaded)
	}
	if rec.Body != body {
		t.Errorf("record body = %q, want %q", rec.Body, body)
	}
	if d := m.CheckDrift(ctx, anchor); d.State != DriftInSync {
		t.Errorf("CheckDrift() = %+v, want %q", d, DriftInSync)
	}
}
