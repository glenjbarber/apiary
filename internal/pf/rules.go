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
//
// The first four fields are the pre-ADR-0137 shape and render
// byte-for-byte as they always did *provided* Any is set; the scope
// fields after them are what stopped every rule from being silently
// `from any to any` (see scope.go, and ADR-0137).
//
// A rule MUST declare a scope. The zero value of Rule - no interface,
// no source, no destination, no opt-in - is a render error, not a
// broad rule: a caller that forgot to say where its rule applies gets a
// loud local failure instead of a rule that quietly matches everything.
type Rule struct {
	Direction string // "in" or "out"
	Action    string // "pass" or "block"
	Protocol  string // "tcp", "udp", "icmp", or "" (any)
	PortRange string // "22", "8000-9000", or "" (any)

	// Scope. These four fields, together, are where the rule applies:
	// the `on` / `from` / `to` clauses of the rendered pf rule. See
	// scope.go for the full vocabulary and the rules for combining
	// them. Naming the fields the way ADR-0129 names its planned
	// FirewallRule fields (Source, Destination, Interface) is
	// deliberate, so that ADR-0129's proto work can carry them
	// through without this renderer being rewritten.
	Interface   string // "vtnet0", or "" for no `on` clause
	Source      string // "10.60.0.0/24", "10.60.0.7", "table <name>", or ""
	Destination string // same shapes as Source
	Any         bool   // explicit opt-in for a from-any-to-any rule

	// Comment, if non-empty, is rendered as a pf `#` comment above
	// the rule. It is validated as a single line with no newline,
	// because a comment is still a line pfctl reads: an unvalidated
	// comment is a rule this package did not intend to write.
	Comment string
}

// RenderRules compiles rules into a pf.conf-style ruleset body, one
// rule per line, suitable for loading into a pf(8) anchor via `pfctl -a
// <anchor> -f -`. This is pure and requires no pf tooling to test.
//
// It is all-or-nothing: a ruleset where any one rule fails to render
// produces no body at all, so a caller can never load a half-scoped
// ruleset. ADR-0075's no-`quick`, last-match-wins evaluation is
// unchanged - a rule's priority is still expressed purely by its
// position in this slice.
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
	if err := renderComment(r.Comment, &b); err != nil {
		return "", err
	}
	fmt.Fprintf(&b, "%s %s", r.Action, r.Direction)

	switch r.Protocol {
	case "", "tcp", "udp", "icmp":
	default:
		return "", fmt.Errorf("pf: invalid protocol %q (want tcp, udp, icmp, or empty)", r.Protocol)
	}
	if r.Protocol != "" {
		fmt.Fprintf(&b, " proto %s", r.Protocol)
	}

	// The clause that used to be an unconditional ` from any to any`,
	// and is now the rule's declared scope or an error. A rule that
	// omits it gets refused, not widened.
	scope, err := scopeClause(&r)
	if err != nil {
		return "", err
	}
	b.WriteString(scope)

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

// renderComment writes a pf comment line, or nothing for an empty
// comment. A comment containing a newline, a carriage return, or any
// other control character is rejected: pfctl reads a ruleset line by
// line, so an unvalidated comment is a way to smuggle in a second rule
// (the same reasoning, and the same fix, as ApplyNAT's newline check).
func renderComment(comment string, b *strings.Builder) error {
	if comment == "" {
		return nil
	}
	if strings.ContainsAny(comment, "\n\r") || strings.ContainsRune(comment, 0) {
		return fmt.Errorf("pf: comment must be a single line, got %q", comment)
	}
	b.WriteString("# ")
	b.WriteString(comment)
	b.WriteString("\n")
	return nil
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
