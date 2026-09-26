package pf

import (
	"strings"
	"testing"
)

// These tests predate ADR-0137 and asserted the renderer's output for
// rules with no declared scope. The expected pf text is unchanged - a
// broad rule still renders exactly ` from any to any`, byte for byte -
// but the *call* now has to say so with Any: true, which is the whole
// point of the change: the widening is a caller's explicit decision
// rather than a zero value's silent default. Every other assertion here
// is untouched, and the scope cases the new tests need are appended
// below rather than folded in.

func TestRenderRules_SimpleBlock(t *testing.T) {
	body, err := RenderRules([]Rule{{Direction: "in", Action: "block", Protocol: "tcp", PortRange: "22", Any: true}})
	if err != nil {
		t.Fatalf("RenderRules() error: %v", err)
	}
	want := "block in proto tcp from any to any port 22\n"
	if body != want {
		t.Errorf("RenderRules() = %q, want %q", body, want)
	}
}

func TestRenderRules_PortRangeUsesColonSyntax(t *testing.T) {
	body, err := RenderRules([]Rule{{Direction: "out", Action: "pass", Protocol: "udp", PortRange: "8000-9000", Any: true}})
	if err != nil {
		t.Fatalf("RenderRules() error: %v", err)
	}
	want := "pass out proto udp from any to any port 8000:9000\n"
	if body != want {
		t.Errorf("RenderRules() = %q, want %q", body, want)
	}
}

func TestRenderRules_NoProtocolOrPort(t *testing.T) {
	body, err := RenderRules([]Rule{{Direction: "in", Action: "block", Any: true}})
	if err != nil {
		t.Fatalf("RenderRules() error: %v", err)
	}
	want := "block in from any to any\n"
	if body != want {
		t.Errorf("RenderRules() = %q, want %q", body, want)
	}
}

func TestRenderRules_ICMPWithNoPort(t *testing.T) {
	body, err := RenderRules([]Rule{{Direction: "in", Action: "pass", Protocol: "icmp", Any: true}})
	if err != nil {
		t.Fatalf("RenderRules() error: %v", err)
	}
	want := "pass in proto icmp from any to any\n"
	if body != want {
		t.Errorf("RenderRules() = %q, want %q", body, want)
	}
}

func TestRenderRules_MultipleRules(t *testing.T) {
	body, err := RenderRules([]Rule{
		{Direction: "in", Action: "pass", Protocol: "tcp", PortRange: "22", Any: true},
		{Direction: "in", Action: "block", Protocol: "tcp", PortRange: "23", Any: true},
	})
	if err != nil {
		t.Fatalf("RenderRules() error: %v", err)
	}
	want := "pass in proto tcp from any to any port 22\nblock in proto tcp from any to any port 23\n"
	if body != want {
		t.Errorf("RenderRules() = %q, want %q", body, want)
	}
}

func TestRenderRules_NoRulesIsEmptyBody(t *testing.T) {
	body, err := RenderRules(nil)
	if err != nil {
		t.Fatalf("RenderRules() error: %v", err)
	}
	if body != "" {
		t.Errorf("RenderRules(nil) = %q, want empty", body)
	}
}

func TestRenderRules_RejectsInvalidDirection(t *testing.T) {
	if _, err := RenderRules([]Rule{{Direction: "sideways", Action: "pass", Any: true}}); err == nil {
		t.Errorf("RenderRules() = nil error, want one for an invalid direction")
	}
}

func TestRenderRules_RejectsInvalidAction(t *testing.T) {
	if _, err := RenderRules([]Rule{{Direction: "in", Action: "maybe", Any: true}}); err == nil {
		t.Errorf("RenderRules() = nil error, want one for an invalid action")
	}
}

func TestRenderRules_RejectsInvalidProtocol(t *testing.T) {
	if _, err := RenderRules([]Rule{{Direction: "in", Action: "pass", Protocol: "sctp", Any: true}}); err == nil {
		t.Errorf("RenderRules() = nil error, want one for an unsupported protocol")
	}
}

func TestRenderRules_RejectsPortRangeWithoutTCPOrUDP(t *testing.T) {
	if _, err := RenderRules([]Rule{{Direction: "in", Action: "pass", Protocol: "icmp", PortRange: "22", Any: true}}); err == nil {
		t.Errorf("RenderRules() = nil error, want one for a port range on icmp")
	}
	if _, err := RenderRules([]Rule{{Direction: "in", Action: "pass", PortRange: "22", Any: true}}); err == nil {
		t.Errorf("RenderRules() = nil error, want one for a port range with no protocol")
	}
}

func TestRenderRules_RejectsInvalidPortRange(t *testing.T) {
	cases := []string{"", "abc", "0", "70000", "9000-8000", "22-"}
	for _, c := range cases {
		if c == "" {
			continue // empty PortRange is valid (means "any")
		}
		if _, err := RenderRules([]Rule{{Direction: "in", Action: "pass", Protocol: "tcp", PortRange: c, Any: true}}); err == nil {
			t.Errorf("RenderRules() with PortRange=%q = nil error, want one", c)
		}
	}
}

// --- ADR-0137: scope -------------------------------------------------

// TestRenderRules_UnscopedRuleIsRefused is the regression test for the
// defect this whole file was written to close: before it, a Rule with
// no scope field rendered ` from any to any` because that string was
// written unconditionally, so every rule Apiary ever loaded matched
// every interface to every interface.
func TestRenderRules_UnscopedRuleIsRefused(t *testing.T) {
	if _, err := RenderRules([]Rule{{Direction: "in", Action: "pass", Protocol: "tcp", PortRange: "22"}}); err == nil {
		t.Fatal("RenderRules() with no scope = nil error, want a refusal")
	}
}

// TestRenderRules_ScopeRendersNarrowRules keeps the shape of a rule
// that names its scope byte-stable: the scope clause lands exactly
// where ` from any to any` used to, so an operator reading a ruleset
// sees the same rule with three more words in it and nothing else
// moved.
func TestRenderRules_ScopeRendersNarrowRules(t *testing.T) {
	cases := []struct {
		name string
		rule Rule
		want string
	}{
		{
			name: "interface only, with the any-to-any opt-in",
			rule: Rule{Direction: "in", Action: "pass", Protocol: "tcp", PortRange: "22", Interface: "vtnet0", Any: true},
			want: "pass in proto tcp on vtnet0 from any to any port 22\n",
		},
		{
			name: "source and destination, no opt-in",
			rule: Rule{Direction: "in", Action: "block", Protocol: "tcp", PortRange: "22", Source: "10.60.0.0/24", Destination: "10.60.0.7"},
			want: "block in proto tcp from 10.60.0.0/24 to 10.60.0.7 port 22\n",
		},
		{
			name: "addresses only, no interface",
			rule: Rule{Direction: "out", Action: "pass", Protocol: "udp", PortRange: "53", Source: "10.60.0.7", Destination: "8.8.8.8"},
			want: "pass out proto udp from 10.60.0.7 to 8.8.8.8 port 53\n",
		},
		{
			name: "interface, source and destination together",
			rule: Rule{Direction: "in", Action: "block", Interface: "bridge0", Source: "192.168.1.0/24", Destination: "10.60.0.7"},
			want: "block in on bridge0 from 192.168.1.0/24 to 10.60.0.7\n",
		},
		{
			name: "named address set on one end",
			rule: Rule{Direction: "in", Action: "pass", Protocol: "tcp", PortRange: "443", Source: "table apiary-trusted", Destination: "10.60.0.7"},
			want: "pass in proto tcp from table apiary-trusted to 10.60.0.7 port 443\n",
		},
		{
			name: "IPv6 literal",
			rule: Rule{Direction: "in", Action: "block", Source: "fd00::/8", Destination: "fd00::7"},
			want: "block in from fd00::/8 to fd00::7\n",
		},
		{
			name: "one end any, the other named",
			rule: Rule{Direction: "in", Action: "pass", Protocol: "tcp", PortRange: "22", Source: "any", Destination: "10.60.0.7"},
			want: "pass in proto tcp from any to 10.60.0.7 port 22\n",
		},
		{
			name: "comment above the rule",
			rule: Rule{Direction: "in", Action: "block", Protocol: "tcp", PortRange: "22", Any: true, Comment: "no telnet"},
			want: "# no telnet\nblock in proto tcp from any to any port 22\n",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := RenderRules([]Rule{tc.rule})
			if err != nil {
				t.Fatalf("RenderRules() error: %v", err)
			}
			if got != tc.want {
				t.Errorf("RenderRules() = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestRenderRules_BroadRuleNeedsTheExplicitOptIn covers every way a
// caller can ask for - or accidentally get - an any-to-any rule. The
// two that are *not* errors are the one explicit opt-in, and that same
// opt-in combined with an interface (which is strictly narrower than
// the bare form and so is not the thing being guarded here).
func TestRenderRules_BroadRuleNeedsTheExplicitOptIn(t *testing.T) {
	// Accepted: the opt-in, and the opt-in plus an interface.
	accepted := []Rule{
		{Direction: "in", Action: "pass", Any: true},
		{Direction: "in", Action: "pass", Interface: "vtnet0", Any: true},
		{Direction: "in", Action: "pass", Source: "any", Destination: "10.60.0.7"},
		{Direction: "in", Action: "pass", Source: "10.0.0.0/8", Destination: "any"},
	}
	for _, rule := range accepted {
		if _, err := RenderRules([]Rule{rule}); err != nil {
			t.Errorf("RenderRules(%+v) error: %v, want it accepted", rule, err)
		}
	}

	// Refused: anything that would render a broad rule without the
	// opt-in field.
	refused := []Rule{
		{Direction: "in", Action: "pass"},                                    // no scope at all
		{Direction: "in", Action: "pass", Interface: "vtnet0"},               // interface, no addresses, no opt-in
		{Direction: "in", Action: "pass", Source: "any", Destination: "any"}, // the broad rule spelled as two strings
	}
	for _, rule := range refused {
		if _, err := RenderRules([]Rule{rule}); err == nil {
			t.Errorf("RenderRules(%+v) = nil error, want a refusal", rule)
		}
	}
}

// TestRenderRules_ContradictoryScopeIsRefused: the opt-in is "addresses
// are unconstrained". A rule that also names addresses is not saying
// that, and rendering the address away would widen it silently.
func TestRenderRules_ContradictoryScopeIsRefused(t *testing.T) {
	cases := []Rule{
		{Action: "pass", Any: true, Source: "10.0.0.0/8", Destination: "10.0.0.1"},
		{Action: "pass", Any: true, Source: "10.0.0.0/8"},
		{Action: "pass", Any: true, Destination: "10.0.0.1"},
		{Action: "pass", Any: true, Interface: "vtnet0", Source: "any", Destination: "any"},
	}
	for _, rule := range cases {
		rule.Direction = "in"
		if _, err := RenderRules([]Rule{rule}); err == nil {
			t.Errorf("RenderRules(%+v) = nil error, want a refusal", rule)
		}
	}
}

// TestRenderRules_RejectsHalfDeclaredAddressScope: pf address scope is
// a from/to pair. Guessing the other half would mean rendering a rule
// broader than the caller asked for in the one direction they did not
// mention.
func TestRenderRules_RejectsHalfDeclaredAddressScope(t *testing.T) {
	for _, rule := range []Rule{
		{Action: "pass", Source: "10.0.0.0/8"},
		{Action: "pass", Destination: "10.0.0.1"},
		{Action: "pass", Interface: "vtnet0", Source: "10.0.0.0/8"},
	} {
		rule.Direction = "in"
		if _, err := RenderRules([]Rule{rule}); err == nil {
			t.Errorf("RenderRules(%+v) = nil error, want a refusal for a one-ended address scope", rule)
		}
	}
}

// TestRenderRules_RejectsMalformedScope is the injection-defence half:
// every scope field is validated by whitelist, so nothing that is not
// a real interface name or a real address can reach the ruleset, and a
// newline can never start a second pf rule.
func TestRenderRules_RejectsMalformedScope(t *testing.T) {
	cases := []struct {
		name string
		rule Rule
	}{
		{"interface with a newline", Rule{Interface: "vtnet0\npass in all"}},
		{"interface with a space", Rule{Interface: "vtnet0 pass in all"}},
		{"interface that is the word any", Rule{Interface: "any", Any: true}},
		{"interface over FreeBSD's name limit", Rule{Interface: "thisnameiswaytoolong01", Any: true}},
		{"interface with a shell-ish character", Rule{Interface: "vtnet0;", Any: true}},
		{"source that is not an address", Rule{Source: "not-an-address", Destination: "10.0.0.1"}},
		{"source with a newline", Rule{Source: "10.0.0.0/24\npass in all", Destination: "10.0.0.1"}},
		{"source with an embedded space", Rule{Source: "10.0.0.0/24 to 10.0.0.1", Destination: "10.0.0.1"}},
		{"source with a bad prefix length", Rule{Source: "10.0.0.0/99", Destination: "10.0.0.1"}},
		{"destination that is a pf macro", Rule{Source: "10.0.0.0/8", Destination: "<trusted>"}},
		{"destination naming a table with a bad name", Rule{Source: "10.0.0.0/8", Destination: "table bad name"}},
		{"table keyword with no name", Rule{Source: "10.0.0.0/8", Destination: "table "}},
		{"comment with a newline", Rule{Any: true, Comment: "ok\npass in all"}},
		{"comment with a carriage return", Rule{Any: true, Comment: "ok\rpass in all"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tc.rule.Direction = "in"
			tc.rule.Action = "pass"
			body, err := RenderRules([]Rule{tc.rule})
			if err == nil {
				t.Fatalf("RenderRules(%+v) = %q, want a rejection", tc.rule, body)
			}
			// Belt and braces: whatever the error, no rule text came
			// back at all. RenderRules is all-or-nothing, so a rejected
			// rule can never be half-loaded.
			if body != "" {
				t.Errorf("RenderRules() returned %q alongside an error; want no body at all", body)
			}
		})
	}
}

// TestRenderRules_OneBadRuleRejectsTheWholeRuleset: the load path is
// `pfctl -f -` on the whole body, so a ruleset that renders "everything
// but the broken rule" would be a silently-wider ruleset than the caller
// intended.
func TestRenderRules_OneBadRuleRejectsTheWholeRuleset(t *testing.T) {
	body, err := RenderRules([]Rule{
		{Direction: "in", Action: "pass", Protocol: "tcp", PortRange: "22", Any: true},
		{Direction: "in", Action: "block"}, // unscoped
		{Direction: "in", Action: "block", Protocol: "tcp", PortRange: "23", Any: true},
	})
	if err == nil {
		t.Fatalf("RenderRules() = %q, want a rejection", body)
	}
	if body != "" {
		t.Errorf("RenderRules() = %q with an error, want no body", body)
	}
}

// TestRenderRules_NarrowRuleIsNarrowerThanBroad pins the invariant the
// whole change exists to protect, in the only form a test can state it
// on macOS: whatever a scoped rule renders, it must not contain the
// tokens that make the unscoped version broad.
func TestRenderRules_NarrowRuleIsNarrowerThanBroad(t *testing.T) {
	broad, err := RenderRules([]Rule{{Direction: "in", Action: "pass", Any: true}})
	if err != nil {
		t.Fatalf("RenderRules() error: %v", err)
	}
	narrow, err := RenderRules([]Rule{{Direction: "in", Action: "pass", Interface: "vtnet0", Any: true}})
	if err != nil {
		t.Fatalf("RenderRules() error: %v", err)
	}
	if !strings.Contains(broad, "from any to any") {
		t.Fatalf("precondition: broad rule %q does not render from any to any", broad)
	}
	if strings.Count(narrow, "any") != 2 {
		t.Errorf("narrow rule %q should be the broad rule plus an interface, and nothing else changed", narrow)
	}
}
