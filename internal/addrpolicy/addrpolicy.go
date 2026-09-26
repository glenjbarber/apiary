// Package addrpolicy holds the one distinction this project kept getting
// wrong: an address a process binds ON is not the same kind of thing as
// an address a process dials, and the same string means opposite things
// in the two roles.
//
// The concrete failure this package exists to make impossible lives in
// the four-node FreeBSD colony. managerd's `rpc_addr` in
// /usr/local/etc/apiary/managerd.json is a net.Listen address: "bind on
// every interface". The shipped guidance also told operators to write
// the wildcard there, so the same value was copied into the fields
// that are DIALS - restshimd's and frontend's `manager_addr`, and
// cmd/raftd/confirm.go's startup confirmation hook, which reads
// rpc_addr straight out of the other daemon's config. A wildcard binds
// perfectly and then refuses every connection aimed at it. On sting
// that produced, verbatim from the live restshimd log:
//
//	cannot reach managerd at 0.0.0.0:17700 (connecting to
//	0.0.0.0:17700: dial tcp 0.0.0.0:17700: connect: connection
//	refused)
//
// so restshimd and frontend - the two daemons that carry the whole
// operator-facing API - could not reach managerd at all, while
// managerd itself looked perfectly healthy because its listener was
// fine. The listener being up is exactly what hides this class of
// mistake: nothing crashes, nothing restarts, the only symptom is an
// unreachable address in a working config file.
//
// The rule this package encodes is therefore one line long: a wildcard
// (or an empty host) is a legal destination for net.Listen and an
// illegal one for net.Dial, and the second role must never be given the
// first's value. Which role a field has is a fact about that field, not
// about the string, so the two are validated by two separately named
// functions rather than one predicate with a boolean - a boolean here
// would let a future call site flip the distinction by accident, which
// is the mistake being fixed.
//
// What this package deliberately does NOT decide:
//
//   - Whether a bind address is assigned to this host. That needs an
//     interface inventory and is inventory-aware by nature, so it stays
//     in internal/manager's UpdateManagerdBindAddress, which is the
//     only caller that can actually ask the kernel.
//   - Whether a dial target resolves, or whether the TLS certificate
//     behind it verifies. Both are properties of the network and of
//     another process's certificate, and no config file can know either
//     one - internal/managerlink already answers the live half at
//     startup.
//
// Both validators take the field name as well as the value so an error
// can say which line of which file to edit. A config file that names
// the offending field is an instruction; one that says "invalid
// address" is a puzzle.
package addrpolicy

import (
	"fmt"
	"net"
	"strconv"
	"strings"
)

// ValidateDialTarget rejects an address that something will actually
// connect TO, when it could not possibly be reached at that address.
//
// The rejected cases are the ones a bind address legitimately takes
// and a dial target never can:
//
//   - an empty host (":17700"), which net.Dial resolves to the wildcard
//     and then refuses;
//   - an unspecified address ("0.0.0.0", "::", "[::]"), which is
//     net.Listen's way of saying "every interface" and has no meaning
//     as a destination at all - connecting to it is refused, not
//     silently redirected to a local service.
//
// Everything else is allowed, deliberately and on purpose: loopback
// ("127.0.0.1", "::1", "localhost"), a numeric LAN address, and a DNS
// hostname. A DNS hostname is not merely tolerated, it is the value
// that actually works in this deployment: every serving certificate
// Apiary issues carries SANs `IP:127.0.0.1` and `DNS:<node>.<domain>`
// and NO SAN for the node's LAN address, and cmd/raftd/confirm.go
// leaves the TLS serverName empty, so Go verifies whatever host was
// dialed. A numeric LAN address therefore cannot verify against any
// Apiary-issued certificate - which is why the cluster's working
// configuration is a per-node name such as brood.lab3.home.arpa:17700.
// Rejecting hostnames here would reject the only value the production
// colony can actually run.
//
// A valid port is still required: this package is about which host is
// dialable, not about relaxing what the rest of the codebase already
// enforces.
//
// The rejected alternative was a single "is this address valid?"
// check shared by both roles, which is what shipped. It read
// "0.0.0.0:17700" as a perfectly good address everywhere, so the same
// string that made a listener reachable made every client of that
// listener unreachable, and nothing said so at any point the operator
// was looking.
func ValidateDialTarget(field, addr string) error {
	host, port, err := splitAddr(field, addr)
	if err != nil {
		return err
	}
	if err := validatePort(field, addr, port); err != nil {
		return err
	}
	if host == "" {
		return fmt.Errorf("addrpolicy: invalid %s %q: no host - an address with an empty host is the "+
			"wildcard, which is a valid thing to LISTEN on and never a valid thing to CONNECT to; "+
			"dialing it is connection-refused (FreeBSD: dial tcp %s: connect: connection refused). "+
			"Dial an address that names the host explicitly: 127.0.0.1 for this node alone, or a "+
			"resolvable name such as <node>.<domain> whose certificate carries a matching DNS SAN",
			field, addr, addr)
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsUnspecified() {
		return fmt.Errorf("addrpolicy: invalid %s %q: %s is the unspecified (wildcard) address - "+
			"net.Listen accepts it to mean \"every interface\", but dialing it is connection-refused "+
			"(FreeBSD: dial tcp %s: connect: connection refused), which is exactly how a live node "+
			"lost managerd: cannot reach managerd at %s. Use an address that names the host: 127.0.0.1 "+
			"for this node alone, or a resolvable name such as <node>.<domain> whose certificate "+
			"carries a matching DNS SAN",
			field, addr, host, addr, addr)
	}
	return nil
}

// ValidateBindAddress rejects an address that something will LISTEN ON
// and could not possibly listen on.
//
// It is deliberately permissive about the host. The unspecified
// addresses ("0.0.0.0", "::", "[::]", and the empty host that means the
// same thing) and loopback are both legitimate binds - the whole
// codebase's own defaults are loopback, and "listen on every interface"
// is a real choice for a node whose consumers are peers rather than
// itself - so none of them is rejected here. Numerics and hostnames are
// accepted for the same reason ValidateDialTarget accepts both, and
// because a hostname is a perfectly good bind address: net.Listen
// resolves it once, at startup, and binds the resulting address.
//
// A valid port is required, as everywhere else in this codebase.
//
// What this function must NOT grow into is an interface-inventory
// check. "Is this address actually assigned to this host?" needs to ask
// the kernel, and stays in internal/manager, which already does exactly
// that for managerd's own bind address - and which correctly still
// rejects an unassigned numeric address, because that check is a real
// anti-typo guard: a mistyped 10.90.0.12 on a machine holding .13
// produces a config that looks right, starts cleanly, and is reachable
// by nobody.
//
// The rejected alternative was folding the dial-target rule into this
// function, i.e. refusing 0.0.0.0 as a bind address. That would have
// looked like a fix while breaking every existing single-node deployment
// and the frontend's own LAN-exposed HTTP listener, and the resulting
// complaints ("why can't I bind 0.0.0.0 any more?") would have pointed
// at the wrong thing entirely.
func ValidateBindAddress(field, addr string) error {
	_, port, err := splitAddr(field, addr)
	if err != nil {
		return err
	}
	return validatePort(field, addr, port)
}

// splitAddr splits host:port, turning a malformed value into an error
// that names the field. The host is returned even when it is empty,
// because an empty host is a legitimate bind and a broken dial - the
// caller decides which, which is the whole point of this package.
func splitAddr(field, addr string) (host, port string, err error) {
	addr = strings.TrimSpace(addr)
	host, port, err = net.SplitHostPort(addr)
	if err != nil {
		return "", "", fmt.Errorf("addrpolicy: invalid %s %q: %w - an address must be host:port, "+
			"for example 127.0.0.1:17700 or <node>.<domain>:17700", field, addr, err)
	}
	return host, port, nil
}

// validatePort requires a port that is present and in range. An empty or
// non-numeric port is a typo, not a default: every caller in this
// codebase has its own documented default port and applies it
// elsewhere, so a missing port here is always a mistake worth naming.
func validatePort(field, addr, port string) error {
	if port == "" {
		return fmt.Errorf("addrpolicy: invalid %s %q: no port - every address here needs one, "+
			"for example 127.0.0.1:17700", field, addr)
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		return fmt.Errorf("addrpolicy: invalid %s %q: invalid port %q - it must be a number from 1 to 65535", field, addr, port)
	}
	return nil
}
