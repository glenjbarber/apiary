package pf

import (
	"fmt"
	"net/netip"
	"strings"
)

// Scope: which traffic a rule is allowed to match.
//
// A rule's scope is the part of a pf rule that says *where* the rule
// applies - `on <iface>`, `from <address>`, `to <address>` - and it is
// the part that decides a rule's blast radius. Before this file,
// internal/pf's renderer wrote ` from any to any` unconditionally and
// had no field to say anything else, so every rule Apiary ever loaded
// matched every interface to every interface regardless of what the
// operator configured. That is the defect ADR-0137 closes; the shape
// it takes is: a rule must *declare* its scope, the declaration is
// validated at render time, and the one declaration that widens to
// everything - an any-to-any rule - is only rendered when the caller
// explicitly asked for it (`Rule.Any`).
//
// The three ways to declare a scope, and the one that is refused:
//
//   - an interface: `on vtnet0`. A per-VM anchor's natural shape, and
//     the narrowest thing a rule can name.
//   - a source and a destination: `from 10.60.0.0/24 to 10.60.0.5`.
//     Both ends, or neither - a half-declared address scope is a
//     rejection, never a default. A named address set is also
//     acceptable on either end: `from table apiary-trusted`.
//   - nothing, plus `Rule.Any = true`: ` from any to any`, the
//     explicit opt-in. A rule with an interface and `Any` is fine and
//     renders as `on <iface> from any to any`, which is strictly
//     narrower than the bare opt-in.
//
// Deliberately NOT expressible: a rule naming an anchor from inside
// an anchor. pf has no such construct, and inventing one that renders
// to something pf cannot parse would be worse than not offering it.
// The anchor is already the outermost scope; what this file governs is
// the clause inside it.

const (
	// anyLiteral is the only spelling of "any" a caller may write into
	// a source or destination. It is accepted, but it is not by itself
	// the opt-in for a broad rule: `Source: "any", Destination:
	// "any"` is rejected in favour of `Any: true`, so that "I meant
	// this rule to be about everything" is always one explicit,
	// greppable field rather than two strings that happen to say "any".
	anyLiteral = "any"

	// maxInterfaceNameLen is IFNAMSIZ-1 on FreeBSD: the longest name
	// an interface can have, and therefore the longest one that can
	// ever match. Mirrors internal/nodeconfig's own validInterfaceName
	// length check; this one additionally allows '.' and '_', which
	// FreeBSD permits and this project uses in bridge names.
	maxInterfaceNameLen = 15
)

// scopeClause renders r's scope, or returns an error explaining why the
// rule cannot be rendered safely. It never returns a clause that is
// broader than what the rule declared: the ` from any to any` case is
// reachable only through Rule.Any.
//
// The returned string always begins with a leading space, so a caller
// can write it straight after the direction/`on` clauses.
func scopeClause(r *Rule) (string, error) {
	if err := validateInterface(r.Interface); err != nil {
		return "", err
	}

	var b strings.Builder
	if r.Interface != "" {
		fmt.Fprintf(&b, " on %s", r.Interface)
	}

	switch {
	case r.Any && (r.Source != "" || r.Destination != ""):
		// The opt-in is specifically "addresses are not constrained".
		// A rule that also names an address is not asking for that,
		// and rendering the address away would silently widen it -
		// so the contradiction is the caller's to resolve, loudly.
		return "", fmt.Errorf("pf: rule sets Any (from any to any) and also names addresses (source %q, destination %q); these contradict - either set Any alone, or drop Any and name both ends", r.Source, r.Destination)

	case r.Any:
		b.WriteString(" from any to any")
		return b.String(), nil

	case r.Source == "" && r.Destination == "":
		if r.Interface == "" {
			// The zero value of Rule, which is the accident this whole
			// file exists to prevent: a rule with no scope at all
			// would render as the broad any-to-any rule.
			return "", fmt.Errorf("pf: rule has no scope: it names no interface, no source, and no destination, so rendering it would produce a from-any-to-any rule. Set Rule.Any = true to ask for that explicitly, or set Rule.Interface and/or both Rule.Source and Rule.Destination")
		}
		// An interface alone is not a complete scope here. `on vtnet0`
		// with no address clause still means "any address", which is
		// the broad rule minus the on - quietly wider than an operator
		// reading "on vtnet0" is likely to believe. Require the
		// addresses, or require the explicit opt-in.
		return "", fmt.Errorf("pf: rule names interface %q but no source or destination, so it would match every address on that interface; set Rule.Any = true for that deliberately, or name both Rule.Source and Rule.Destination", r.Interface)

	case r.Source == "" || r.Destination == "":
		return "", fmt.Errorf("pf: rule names only one end of its address scope (source %q, destination %q): pf address scope is always a from/to pair, and this renderer will not guess the other half", r.Source, r.Destination)
	}

	if r.Source == anyLiteral && r.Destination == anyLiteral {
		return "", fmt.Errorf("pf: rule spells an any-to-any scope as two \"any\" literals; set Rule.Any = true instead, so a deliberately broad rule is one obvious field rather than two strings")
	}
	if err := validateAddress("source", r.Source); err != nil {
		return "", err
	}
	if err := validateAddress("destination", r.Destination); err != nil {
		return "", err
	}

	fmt.Fprintf(&b, " from %s to %s", r.Source, r.Destination)
	return b.String(), nil
}

// validateInterface rejects an interface name that pf could not match
// as written, and - more importantly - anything that is not an
// interface name at all. Validation is a whitelist, not a blacklist:
// a string that is not a plausible FreeBSD interface name never
// reaches the ruleset, which is what keeps `on <iface>` from being an
// injection point for a second pf rule.
func validateInterface(iface string) error {
	if iface == "" {
		return nil // no interface is a legal scope declaration
	}
	if len(iface) > maxInterfaceNameLen {
		return fmt.Errorf("pf: interface %q is longer than FreeBSD's %d-character interface name limit, so it could never match a real interface", iface, maxInterfaceNameLen)
	}
	if iface == anyLiteral {
		return fmt.Errorf("pf: interface %q is not an interface: pf has no \"on any\"; a rule that is not tied to one interface names addresses instead", iface)
	}
	for _, r := range iface {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '-', r == '_', r == '.':
		default:
			return fmt.Errorf("pf: interface %q contains %q, which is not valid in a FreeBSD interface name", iface, string(r))
		}
	}
	return nil
}

// validateAddress accepts exactly three spellings for one end of an
// address scope: the literal `any`, an IP literal or CIDR prefix, or a
// named address set (`table <name>`). Anything else - a pf macro, a
// nested anchor, a shell-ish fragment, an address with embedded
// whitespace, a second rule after a newline - is rejected.
//
// Like validateInterface this is a whitelist: a value is only ever
// emitted if it parsed as one of those three forms, so the rendered
// ruleset cannot be talked into carrying a clause the caller did not
// name.
func validateAddress(field, value string) error {
	if value == "" {
		return fmt.Errorf("pf: empty %s address", field)
	}
	if value == anyLiteral {
		return nil
	}
	if rest, ok := strings.CutPrefix(value, "table "); ok {
		if err := validateSetName(rest); err != nil {
			return fmt.Errorf("pf: invalid %s address set %q: %w", field, value, err)
		}
		return nil
	}
	if _, err := netip.ParseAddr(value); err == nil {
		return nil
	}
	if p, err := netip.ParsePrefix(value); err == nil {
		// ParsePrefix accepts a host address with a mask (10.0.0.1/24)
		// without checking the host bits, which pf tolerates too, and
		// rejects an out-of-range mask itself.
		_ = p
		return nil
	}
	return fmt.Errorf("pf: invalid %s address %q: want \"any\", an IP address, a CIDR prefix, or \"table <name>\"", field, value)
}

// validateSetName is the whitelist for a named address set. A pf
// table name may not contain whitespace or the characters that would
// let it terminate its own clause.
func validateSetName(name string) error {
	if name == "" {
		return fmt.Errorf("the set name is empty")
	}
	if len(name) > 63 {
		return fmt.Errorf("set name is longer than 63 characters")
	}
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '-', r == '_', r == '.':
		default:
			return fmt.Errorf("set name contains %q, which is not valid in a pf table name", string(r))
		}
	}
	return nil
}
