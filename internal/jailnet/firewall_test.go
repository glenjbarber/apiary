package jailnet

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/glenjbarber/apiary/internal/pf"
)

// fakePF is an in-memory PFLoader that records what was loaded where,
// so a test can assert on the anchor name and the exact rules without a
// pf(8) anywhere in sight.
type fakePF struct {
	applied map[string][]pf.Rule
	flushed []string
	fail    string
}

func newFakePF() *fakePF { return &fakePF{applied: map[string][]pf.Rule{}} }

func (p *fakePF) Apply(ctx context.Context, anchor string, rules []pf.Rule) error {
	if p.fail != "" {
		return errors.New(p.fail)
	}
	p.applied[anchor] = rules
	return nil
}

func (p *fakePF) Flush(ctx context.Context, anchor string) error {
	if p.fail != "" {
		return errors.New(p.fail)
	}
	p.flushed = append(p.flushed, anchor)
	delete(p.applied, anchor)
	return nil
}

// TestAnchorNaming confirms each jail gets its own anchor, namespaced so
// it cannot collide with a VM's or an operator's. pf anchors are a flat
// global namespace on the host, so an unnamespaced name is a name that
// will eventually collide.
func TestAnchorNaming(t *testing.T) {
	if got := Anchor("web-1"); got != "apiary/jail/web-1" {
		t.Errorf("Anchor(web-1) = %q, want %q", got, "apiary/jail/web-1")
	}
	if Anchor("a") == Anchor("b") {
		t.Error("two different jails share an anchor")
	}
}

// TestFirewall_NoRulesIsUnfilteredAndTouchesNothing confirms an empty
// rule set is a no-op rather than a flush. "This jail has no rules" and
// "this jail's rules were removed" are different events, and only one
// of them is an instruction - conflating them would make every
// unfiltered jail flush its own (empty) anchor on every tick.
func TestFirewall_NoRulesIsUnfilteredAndTouchesNothing(t *testing.T) {
	p := newFakePF()
	f := &Firewall{PF: p}

	status, err := f.Apply(t.Context(), "web-1", nil)
	if err != nil {
		t.Fatalf("Apply() error = %v", err)
	}
	if status != FirewallStatusUnfiltered {
		t.Errorf("status = %q, want %q", status, FirewallStatusUnfiltered)
	}
	if len(p.applied) != 0 || len(p.flushed) != 0 {
		t.Errorf("pf was touched: applied=%v flushed=%v, want neither", p.applied, p.flushed)
	}
}

// TestFirewall_LoadsIntoTheJailsOwnAnchor confirms the rules land in
// this jail's anchor and nowhere else, reusing internal/pf's own
// renderer rather than inventing a second ruleset language.
func TestFirewall_LoadsIntoTheJailsOwnAnchor(t *testing.T) {
	p := newFakePF()
	f := &Firewall{PF: p}

	rules := JailRules([]JailRule{
		{Direction: "in", Action: "block", Protocol: "tcp"},
		{Direction: "in", Action: "pass", Protocol: "tcp", PortRange: "22"},
	})
	status, err := f.Apply(t.Context(), "web-1", rules)
	if err != nil {
		t.Fatalf("Apply() error = %v", err)
	}
	if status != FirewallStatusFiltered {
		t.Errorf("status = %q, want %q", status, FirewallStatusFiltered)
	}
	if got := p.applied["apiary/jail/web-1"]; len(got) != 2 {
		t.Errorf("anchor holds %d rules, want 2", len(got))
	}
	if len(p.applied) != 1 {
		t.Errorf("loaded into %d anchors, want exactly 1: one anchor per jail", len(p.applied))
	}
}

// TestFirewall_MalformedRuleIsRefusedBeforeLoading confirms a bad rule
// is a loud local error rather than a half-applied ruleset: pf would
// otherwise keep whatever it managed to load, and the operator would be
// left with a ruleset they did not write.
func TestFirewall_MalformedRuleIsRefusedBeforeLoading(t *testing.T) {
	p := newFakePF()
	f := &Firewall{PF: p}

	status, err := f.Apply(t.Context(), "web-1", JailRules([]JailRule{{Direction: "sideways", Action: "pass"}}))
	if err == nil {
		t.Fatalf("Apply() = status %q, want an error for a malformed rule", status)
	}
	if status != FirewallStatusUnknown {
		t.Errorf("status = %q, want %q: nothing was loaded, so nothing is known about the anchor", status, FirewallStatusUnknown)
	}
	if len(p.applied) != 0 {
		t.Errorf("anchor was written despite the malformed rule: %v", p.applied)
	}
}

// TestFirewall_FailedLoadIsUnknownNotUnfiltered is the honesty case that
// matters most here. A load that failed must never be reported as
// "unfiltered": Apiary believes a firewall is in place, is wrong, and
// an operator reading "unfiltered" would conclude nothing is protecting
// the jail - which is the opposite of what the system believes.
func TestFirewall_FailedLoadIsUnknownNotUnfiltered(t *testing.T) {
	for _, tt := range []struct {
		name     string
		pf       *fakePF
		noLoader bool
	}{
		{name: "pfctl failed", pf: func() *fakePF { p := newFakePF(); p.fail = "pfctl: Permission denied"; return p }()},
		{name: "no pf support on this node", noLoader: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := &Firewall{}
			if !tt.noLoader {
				f.PF = tt.pf
			}
			status, err := f.Apply(t.Context(), "web-1", JailRules([]JailRule{{Direction: "in", Action: "pass", Protocol: "tcp"}}))
			if err == nil {
				t.Fatalf("Apply() = status %q, want an error", status)
			}
			if status == FirewallStatusUnfiltered {
				t.Error("status = unfiltered, want unknown: a failed load must never read as \"nothing is filtering this jail\"")
			}
			if status != FirewallStatusUnknown {
				t.Errorf("status = %q, want %q", status, FirewallStatusUnknown)
			}
		})
	}
}

// TestFirewall_FlushIsIdempotent confirms teardown can be retried after
// a partial attempt, which is the whole reason every teardown helper in
// this project treats "already gone" as success.
func TestFirewall_FlushIsIdempotent(t *testing.T) {
	p := newFakePF()
	f := &Firewall{PF: p}

	if _, err := f.Apply(t.Context(), "web-1", JailRules([]JailRule{{Direction: "in", Action: "block", Protocol: "tcp"}})); err != nil {
		t.Fatalf("Apply() error = %v", err)
	}
	for i := 0; i < 2; i++ {
		if err := f.Flush(t.Context(), "web-1"); err != nil {
			t.Fatalf("Flush() pass %d error = %v", i+1, err)
		}
	}
	if len(p.flushed) != 2 {
		t.Errorf("flushed %d times, want 2: both attempts should be honoured and neither should fail", len(p.flushed))
	}
	if _, still := p.applied["apiary/jail/web-1"]; still {
		t.Error("the anchor still holds rules after being flushed")
	}
}

// TestFirewall_FlushWithoutSupportIsANoOp confirms a node with no pf
// support does not fail a jail's teardown, and does not silently
// pretend it cleaned anything up either.
func TestFirewall_FlushWithoutSupportIsANoOp(t *testing.T) {
	f := &Firewall{}
	if err := f.Flush(t.Context(), "web-1"); err != nil {
		t.Errorf("Flush() error = %v, want nil on a node with no pf support", err)
	}
	if err := f.Flush(t.Context(), ""); err != nil {
		t.Errorf("Flush(\"\") error = %v, want nil", err)
	}
}

// TestJailRulesIsAFieldForFieldCopy confirms the neutral shape this
// package takes really is identical to pf.Rule, so the eventual
// JailDefinition.firewall_rules field is a call and not a translation
// layer with a bug in it.
func TestJailRulesIsAFieldForFieldCopy(t *testing.T) {
	got := JailRules([]JailRule{
		{Direction: "out", Action: "pass", Protocol: "udp", PortRange: "8000-9000"},
	})
	want := []pf.Rule{{Direction: "out", Action: "pass", Protocol: "udp", PortRange: "8000-9000"}}
	if len(got) != 1 || got[0] != want[0] {
		t.Errorf("JailRules() = %+v, want %+v", got, want)
	}
	// And the rendered output must be what an operator already knows
	// from a VM's ruleset.
	rendered, err := pf.RenderRules(got)
	if err != nil {
		t.Fatalf("RenderRules() error = %v", err)
	}
	// "8000-9000" is this project's input spelling; pf's own syntax uses
	// a colon. That translation is internal/pf's job, and reusing it
	// unchanged is the point - a jail's rules compile to byte-identical
	// output to a VM's.
	if !strings.Contains(rendered, "pass out proto udp") || !strings.Contains(rendered, "port 8000:9000") {
		t.Errorf("rendered = %q, want the same pf syntax a VM's rules render to", rendered)
	}
}

// TestDescribeFirewallIsStable is a small assertion that the human-facing
// form of a status names its anchor, so a UI row and a log line can be
// read against a pfctl prompt and match.
func TestDescribeFirewallIsStable(t *testing.T) {
	got := DescribeFirewall("web-1", FirewallStatusFiltered)
	if !strings.Contains(got, string(FirewallStatusFiltered)) || !strings.Contains(got, Anchor("web-1")) {
		t.Errorf("DescribeFirewall() = %q, want it to name both the status and the anchor", got)
	}
	if got := DescribeFirewall("", FirewallStatusFiltered); got != string(FirewallStatusUnknown) {
		t.Errorf("DescribeFirewall(\"\") = %q, want %q: there is no anchor to describe", got, FirewallStatusUnknown)
	}
}
