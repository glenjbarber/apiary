package jailnet

import (
	"strings"
	"testing"

	"github.com/glenjbarber/apiary/internal/vlan"
)

// The epair(4) half of ADR-0138's blast radius. vlan.EnsureMember grew a
// Bridge SVI case in this ADR; an epair end can never be one, so none of
// that logic may change what this package does. These two tests pin that:
// the ordinary path is untouched, and the impossible states are still
// refused rather than waved through as success.

// The normal repair path is byte-identical to what it was before
// ADR-0138: a host side found on the wrong bridge (or on none) is moved
// with deletem + EnsureMember, and the pass converges.
func TestEnsure_EpairIsStillMovedWithDeletemAndAddm(t *testing.T) {
	r, runner, _, _, _ := converged(t)
	runner.withState("epair0a", true, "") // lost its membership

	res := r.Ensure(t.Context(), "web", testAddressing())
	if res.Verdict != VerdictRepaired {
		t.Fatalf("Verdict = %q (%s), want %q", res.Verdict, res.Detail, VerdictRepaired)
	}
	if n := runner.ran("ifconfig epair0a up"); n != 0 {
		t.Errorf("ran `ifconfig epair0a up` %d times, want 0: the interface was already up", n)
	}
	if got := runner.memberOf("epair0a"); got != "bridge1" {
		t.Errorf("epair0a is on bridge %q, want bridge1", got)
	}
}

// A membership answer that is not a confirmed join is unknown, never a
// success. MembershipSVI in particular means no addm was issued, so
// recording this jail as joined would be a claim with nothing behind it -
// the same silently-misplaced-network outcome the deletem above exists to
// prevent, arriving by a different route.
func TestEnsure_NonConfirmedMembershipIsUnknownNotRepaired(t *testing.T) {
	for _, state := range []vlan.MembershipState{vlan.MembershipSVI, vlan.MembershipUnknown} {
		t.Run(state.String(), func(t *testing.T) {
			r, runner, bridge, _, _ := converged(t)
			runner.withState("epair0a", true, "") // needs joining
			bridge.forceMemberState = true
			bridge.memberState = state
			bridge.sviParent = "bridge1"

			res := r.Ensure(t.Context(), "web", testAddressing())
			if res.Verdict != VerdictUnknown {
				t.Errorf("Verdict = %q (%s), want %q", res.Verdict, res.Detail, VerdictUnknown)
			}
			if res.Observed {
				t.Error("Observed = true, want false: nothing was confirmed")
			}
			if !strings.Contains(res.Detail, state.String()) {
				t.Errorf("Detail = %q, want it to name the membership state %q", res.Detail, state)
			}
			if got := runner.memberOf("epair0a"); got != "" {
				t.Errorf("epair0a was recorded as a member of %q despite the unconfirmed join", got)
			}
		})
	}
}
