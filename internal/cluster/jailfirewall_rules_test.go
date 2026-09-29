package cluster

import (
	"context"
	"errors"
	"testing"

	"github.com/glenjbarber/apiary/internal/jailnet"
	"github.com/glenjbarber/apiary/internal/pf"
)

// The jail firewall stage (ADR-0117). internal/jailnet already had the
// renderer and the anchor; what was missing was the field to read rules
// from and the reconciler calls to apply them. These tests cover the
// reconciler half only - the anchor name, the rendering, and the
// unfiltered-versus-unchecked distinction are internal/jailnet's own
// tests, and duplicating them here would be a second copy to keep in
// step rather than a second opinion.

type jailFirewallPFStub struct {
	appliedAnchors []string
	appliedRules   [][]pf.Rule
	flushedAnchors []string
	applyErr       error
	flushErr       error
}

func (s *jailFirewallPFStub) Apply(_ context.Context, anchor string, rules []pf.Rule) error {
	if s.applyErr != nil {
		return s.applyErr
	}
	s.appliedAnchors = append(s.appliedAnchors, anchor)
	s.appliedRules = append(s.appliedRules, rules)
	return nil
}

func (s *jailFirewallPFStub) Flush(_ context.Context, anchor string) error {
	if s.flushErr != nil {
		return s.flushErr
	}
	s.flushedAnchors = append(s.flushedAnchors, anchor)
	return nil
}

func (s *jailFirewallPFStub) ApplyNAT(context.Context, string, string, string) error { return nil }

func jailWithRules(id string, rules ...FirewallRule) JailPlacement {
	return JailPlacement{ID: id, Name: id, VNET: true, FirewallRules: rules}
}

func TestApplyJailFirewall_AppliesRulesIntoTheJailsOwnAnchor(t *testing.T) {
	pfStub := &jailFirewallPFStub{}
	r := &Reconciler{PF: pfStub}
	j := jailWithRules("jail-1", FirewallRule{Direction: "in", Action: "pass", Protocol: "tcp", PortRange: "22"})

	if err := r.applyJailFirewall(context.Background(), j); err != nil {
		t.Fatalf("applyJailFirewall: %v", err)
	}
	if len(pfStub.appliedAnchors) != 1 {
		t.Fatalf("applied anchors = %v, want exactly one", pfStub.appliedAnchors)
	}
	// The anchor must be the jail's own, derived in one place
	// (jailnet.Anchor) rather than re-spelled here.
	if got, want := pfStub.appliedAnchors[0], jailnet.Anchor("jail-1"); got != want {
		t.Errorf("anchor = %q, want %q", got, want)
	}
	if len(pfStub.appliedRules[0]) != 1 {
		t.Fatalf("applied rules = %+v, want one", pfStub.appliedRules[0])
	}
}

func TestApplyJailFirewall_FlushesWhenRulesWereRemoved(t *testing.T) {
	// A jail whose rules were all deleted must not keep enforcing the
	// last set that loaded. An anchor left behind is a filter nothing in
	// Apiary's state accounts for - the same orphaning shape as any
	// other resource the reconciler fails to clean up.
	pfStub := &jailFirewallPFStub{}
	r := &Reconciler{PF: pfStub}

	if err := r.applyJailFirewall(context.Background(), jailWithRules("jail-1")); err != nil {
		t.Fatalf("applyJailFirewall: %v", err)
	}
	if len(pfStub.flushedAnchors) != 1 {
		t.Fatalf("flushed anchors = %v, want exactly one", pfStub.flushedAnchors)
	}
	if got, want := pfStub.flushedAnchors[0], jailnet.Anchor("jail-1"); got != want {
		t.Errorf("flushed anchor = %q, want %q", got, want)
	}
	if len(pfStub.appliedAnchors) != 0 {
		t.Errorf("a jail with no rules must be flushed, not applied; applied = %v", pfStub.appliedAnchors)
	}
}

func TestApplyJailFirewall_NoRulesAndNoPFIsNotAFailure(t *testing.T) {
	// A jail with nothing to filter needs no pf support, and a node
	// without pf configured must not report those jails as broken.
	r := &Reconciler{}
	if err := r.applyJailFirewall(context.Background(), jailWithRules("jail-1")); err != nil {
		t.Fatalf("applyJailFirewall on a ruleless jail with no pf: %v", err)
	}
}

func TestApplyJailFirewall_RulesWithNoPFIsRefused(t *testing.T) {
	// The converse: rules that cannot be enforced must say so rather
	// than being silently accepted, because a jail reported as
	// converged while unfiltered is exactly the failure this closes.
	r := &Reconciler{}
	err := r.applyJailFirewall(context.Background(),
		jailWithRules("jail-1", FirewallRule{Direction: "in", Action: "block"}))
	if err == nil {
		t.Fatal("expected a refusal when rules are configured but pf support is not")
	}
}

func TestApplyJailFirewall_PFFailureIsReturnedNotSwallowed(t *testing.T) {
	// A jail whose rules could not be loaded must not look filtered.
	pfStub := &jailFirewallPFStub{applyErr: errors.New("pfctl: anchor busy")}
	r := &Reconciler{PF: pfStub}
	err := r.applyJailFirewall(context.Background(),
		jailWithRules("jail-1", FirewallRule{Direction: "in", Action: "block"}))
	if err == nil {
		t.Fatal("expected the pf failure to be returned")
	}
}

func TestEffectiveJailPFRules_OrdersByPriority(t *testing.T) {
	// A jail's rules go through the same toPFRules a VM's do, so the
	// two kinds of resource cannot drift into different orderings.
	j := jailWithRules("jail-1",
		FirewallRule{Direction: "in", Action: "block", Priority: 10},
		FirewallRule{Direction: "in", Action: "pass", Priority: 1},
	)
	got := effectiveJailPFRules(j)
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2", len(got))
	}
	if got[0].Action != "pass" || got[1].Action != "block" {
		t.Errorf("order = %q,%q, want pass,block (ascending priority)", got[0].Action, got[1].Action)
	}
}

func TestInternalFirewallRules_RoundTripsEveryField(t *testing.T) {
	// The conversion exists so JailDefinition and JailPlacement share
	// one rule type rather than a translation layer between them.
	in := jailWithRules("jail-1", FirewallRule{
		Direction: "out", Action: "pass", Protocol: "udp", PortRange: "8000-9000", Priority: 7,
	})
	got := effectiveJailPFRules(in)
	if len(got) != 1 {
		t.Fatalf("len = %d, want 1", len(got))
	}
	if got[0].Direction != "out" || got[0].Protocol != "udp" || got[0].PortRange != "8000-9000" {
		t.Errorf("rule = %+v, want out/udp/8000-9000 carried through", got[0])
	}
}
