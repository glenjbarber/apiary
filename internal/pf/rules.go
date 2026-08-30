package pf

import (
	"fmt"
	"strconv"
	"strings"
)

// Rule mirrors api/internalpb's FirewallRule - kept as a separate,
// pf-package-local type (rather than importing the proto type directly)
// so this package's core logic stays independent of the wire schema,
// the same reasoning internal/cluster's local interfaces already
// follow for raftClient/vmManager/isoResolver.
type Rule struct {
	Direction string // "in" or "out"
	Action    string // "pass" or "block"
	Protocol  string // "tcp", "udp", "icmp", or "" (any)
	PortRange string // "22", "8000-9000", or "" (any)
}

// RenderRules compiles rules into a pf.conf-style ruleset body, one
// rule per line, suitable for loading into a pf(8) anchor via `pfctl -a
// <anchor> -f -`. This is pure and requires no pf tooling to test.
func RenderRules(rules []Rule) (string, error) {
	var b strings.Builder
	for _, r := range rules {
		line, err := renderRule(r)
		if err != nil {
			return "", err
		}
		b.WriteString(line)
		b.WriteString("\n")
	}
	return b.String(), nil
}

func renderRule(r Rule) (string, error) {
	if r.Direction != "in" && r.Direction != "out" {
		return "", fmt.Errorf("pf: invalid direction %q (want \"in\" or \"out\")", r.Direction)
	}
	if r.Action != "pass" && r.Action != "block" {
		return "", fmt.Errorf("pf: invalid action %q (want \"pass\" or \"block\")", r.Action)
	}

	var b strings.Builder
	fmt.Fprintf(&b, "%s %s", r.Action, r.Direction)

	switch r.Protocol {
	case "", "tcp", "udp", "icmp":
	default:
		return "", fmt.Errorf("pf: invalid protocol %q (want tcp, udp, icmp, or empty)", r.Protocol)
	}
	if r.Protocol != "" {
		fmt.Fprintf(&b, " proto %s", r.Protocol)
	}

	b.WriteString(" from any to any")

	if r.PortRange != "" {
		if r.Protocol != "tcp" && r.Protocol != "udp" {
			return "", fmt.Errorf("pf: port_range %q requires protocol \"tcp\" or \"udp\"", r.PortRange)
		}
		portClause, err := renderPortRange(r.PortRange)
		if err != nil {
			return "", err
		}
		fmt.Fprintf(&b, " port %s", portClause)
	}

	return b.String(), nil
}

// renderPortRange converts a user-facing port or port range ("22",
// "8000-9000") into pf's own syntax (single port unchanged, a range
// joined with ":" instead of "-").
func renderPortRange(s string) (string, error) {
	if lo, hi, ok := strings.Cut(s, "-"); ok {
		loN, err1 := strconv.Atoi(lo)
		hiN, err2 := strconv.Atoi(hi)
		if err1 != nil || err2 != nil || loN <= 0 || hiN <= 0 || loN > hiN || hiN > 65535 {
			return "", fmt.Errorf("pf: invalid port range %q", s)
		}
		return fmt.Sprintf("%d:%d", loN, hiN), nil
	}
	port, err := strconv.Atoi(s)
	if err != nil || port <= 0 || port > 65535 {
		return "", fmt.Errorf("pf: invalid port %q", s)
	}
	return strconv.Itoa(port), nil
}
