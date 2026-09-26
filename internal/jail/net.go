package jail

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
)

// This file holds the in-jail half of VNET jail networking (ADR-0117
// Stage 2): everything needed to observe and repair a running jail's own
// network stack through jexec(8), plus the pure parsers for the
// ifconfig(8)/netstat(1) output they consume.
//
// The observation is deliberately a *snapshot* (JailNetState) rather
// than a set of yes/no accessors. Reconciling means comparing observed
// against desired and then acting on the difference, and a snapshot is
// what makes that comparison atomic enough to reason about: an error
// here means "could not look", which the caller must report as unknown,
// never as "the interface is missing" or "no address is configured".

// Address is one IPv4 address configured on an interface, as observed
// inside a jail's own network stack.
type Address struct {
	IP        string
	PrefixLen int
}

// String renders the address in the "ip/prefixlen" form used by
// ifconfig(8) arguments and by log/error text alike.
func (a Address) String() string {
	return fmt.Sprintf("%s/%d", a.IP, a.PrefixLen)
}

// JailNetState is a snapshot of one jail's own network stack as seen
// from inside it, at the moment of observation.
type JailNetState struct {
	// InterfacePresent is false only when the jail's own ifconfig
	// gave a definite "no such interface" answer. It is never false as
	// a fallback for an observation failure - that is an error return.
	InterfacePresent bool

	// Addresses are the interface's IPv4 addresses, in the order
	// ifconfig listed them. IPv6 is deliberately not modeled: ADR-0117's
	// addressing design is IPv4-only, matching every other allocator in
	// this project, so reconciling an unmodeled address family would be
	// guesswork.
	Addresses []Address

	// DefaultRoute is the jail's IPv4 default gateway, or "" if the
	// jail has no default route at all.
	DefaultRoute string
}

// ObserveNet snapshots jail's own view of iface (its IPv4 addresses and
// the jail's IPv4 default route) via jexec(8).
//
// Only a definite "no such interface" from the jail's own ifconfig
// yields InterfacePresent=false with a nil error. A definite "no such
// jail" from jexec is reported instead as an error wrapping
// ErrJailNotFound, because "the jail is gone" and "the jail is running
// but its vnet interface is missing" call for completely different
// responses from a reconciler (stop, vs. restart) and must never be
// the same answer. Any other failure - a permission error, a missing
// binary - is also an error, so the caller reports unknown instead of
// silently concluding the interface is gone. A jail's own route table
// is read in the same jexec round trip, so one failed jexec yields one
// unknown, not a half observation that looks like a half-missing
// configuration.
func (m *Manager) ObserveNet(ctx context.Context, jail, iface string) (JailNetState, error) {
	var state JailNetState

	out, err := m.run(ctx, "jexec", jail, "ifconfig", iface)
	if err != nil {
		if notFound(err, "ifconfig") {
			return JailNetState{InterfacePresent: false}, nil
		}
		if notFound(err, "jexec") {
			return JailNetState{}, fmt.Errorf("observing interface %s: %w", iface, errors.Join(ErrJailNotFound, err))
		}
		return JailNetState{}, fmt.Errorf("observing interface %s inside jail %q: %w", iface, jail, err)
	}
	state.InterfacePresent = true
	state.Addresses = parseIfconfigAddresses(out)

	routes, err := m.run(ctx, "jexec", jail, "netstat", "-rn", "-f", "inet")
	if err != nil {
		// Includes the case where the jail itself has gone: its
		// interface cannot be meaningfully reported as absent when we
		// just read it back successfully, and a state assembled from
		// two different jails' worth of facts would be worse than no
		// state at all. Either way this is "could not look", which
		// the caller must report as unknown.
		if notFound(err, "jexec") {
			return JailNetState{}, fmt.Errorf("reading routing table: %w", errors.Join(ErrJailNotFound, err))
		}
		return JailNetState{}, fmt.Errorf("reading routing table inside jail %q: %w", jail, err)
	}
	state.DefaultRoute = parseNetstatDefaultRoute(routes)

	return state, nil
}

// EnsureAddressing makes jail's own network stack match addr on iface,
// with gw as the default route (gw may be "" to leave the default route
// alone entirely - see ADR-0117's "no address at all" carve-out for
// jails that manage their own addressing).
//
// It observes first and only acts on a real difference, which is what
// makes it idempotent: repeated calls against an already-correct jail
// issue no commands at all. A foreign address on the interface is
// removed rather than left alongside the intended one, because a VNET
// jail's whole point is that its address is the one Apiary allocated -
// two addresses would mean the jail's traffic is going out with an
// address nothing in Apiary's state accounts for.
//
// A missing interface is reported as an error, not silently ignored:
// there is no way to conjure a vnet interface into a running jail, so
// the caller (internal/jailnet) has to treat that as "this jail must be
// restarted", not as "nothing to do".
func (m *Manager) EnsureAddressing(ctx context.Context, jail, iface string, addr Address, gw string) error {
	// Canonicalized up front so the "already correct?" comparison
	// below is a plain string match against the canonical form the
	// observer produces, rather than depending on the caller having
	// spelled the address exactly the way ifconfig renders it.
	addr, ok := canonicalAddress(addr)
	if !ok {
		return fmt.Errorf("refusing to configure %q/%d inside jail %q: not a usable IPv4 address", addr.IP, addr.PrefixLen, jail)
	}
	// Checked on the error, not on the canonicalized value: an unusable
	// gateway canonicalizes to "" (which is also the legitimate "no
	// gateway wanted" answer), and testing gw != "" would let a typo'd
	// gateway silently disable routing configuration instead of being
	// reported.
	gw, err := canonicalIP(gw)
	if err != nil {
		return fmt.Errorf("refusing to configure default gateway %q inside jail %q: not a usable IPv4 address", gw, jail)
	}

	state, obsErr := m.ObserveNet(ctx, jail, iface)
	if obsErr != nil {
		return obsErr
	}
	if !state.InterfacePresent {
		return fmt.Errorf("interface %s is not present inside jail %q; jail(8) cannot add a vnet interface to an already-running jail, so this jail must be restarted", iface, jail)
	}

	haveAddr := false
	for _, existing := range state.Addresses {
		if existing == addr {
			haveAddr = true
			continue
		}
		if err := m.DeleteAddress(ctx, jail, iface, existing.IP); err != nil {
			return err
		}
	}
	if !haveAddr {
		if err := m.SetAddress(ctx, jail, iface, addr); err != nil {
			return err
		}
	}

	if gw != "" && state.DefaultRoute != gw {
		if state.DefaultRoute != "" {
			if err := m.DeleteDefaultRoute(ctx, jail); err != nil {
				return err
			}
		}
		if err := m.SetDefaultRoute(ctx, jail, gw); err != nil {
			return err
		}
	}
	return nil
}

// canonicalAddress returns addr with its IP in the same rendering
// net.ParseIP produces, and ok=false if it is not a usable IPv4
// address/prefix pair.
func canonicalAddress(addr Address) (Address, bool) {
	ip, err := canonicalIP(addr.IP)
	if err != nil || addr.PrefixLen < 0 || addr.PrefixLen > 32 {
		return addr, false
	}
	return Address{IP: ip, PrefixLen: addr.PrefixLen}, true
}

// canonicalIP returns ip in the rendering net.ParseIP produces, so that
// a value from raft state and a value parsed out of ifconfig output
// compare equal as plain strings. An empty input returns "" with no
// error: "no address" and "no gateway" are both meaningful, and the
// IPv6 carve-out is explicit rather than a silent To4() returning nil.
func canonicalIP(ip string) (string, error) {
	if ip == "" {
		return "", nil
	}
	parsed := net.ParseIP(ip)
	if parsed == nil || parsed.To4() == nil {
		return "", fmt.Errorf("%q is not an IPv4 address", ip)
	}
	return parsed.To4().String(), nil
}

// SetAddress assigns addr/prefixLen to iface inside the jail and brings
// it up. Idempotent by way of the caller: SetAddress itself is the
// "make it so" step, and the reconciler only calls it after observing
// that it is not already so, because ifconfig refuses to re-add an
// address that is already present ("File exists").
func (m *Manager) SetAddress(ctx context.Context, jail, iface string, addr Address) error {
	if _, err := m.run(ctx, "jexec", jail, "ifconfig", iface, "inet", addr.String(), "up"); err != nil {
		return fmt.Errorf("assigning %s to %s inside jail %q: %w", addr, iface, jail, err)
	}
	return nil
}

// DeleteAddress removes addr from iface inside the jail. A no-op if the
// address is not there: ifconfig reports that with its own "not in table"
// wording, which is a successful reconciliation, not a failure.
func (m *Manager) DeleteAddress(ctx context.Context, jail, iface string, addr string) error {
	_, err := m.run(ctx, "jexec", jail, "ifconfig", iface, "inet", addr, "delete")
	if err != nil && !strings.Contains(err.Error(), "Can't assign requested address") {
		return fmt.Errorf("removing %s from %s inside jail %q: %w", addr, iface, jail, err)
	}
	return nil
}

// SetDefaultRoute makes gw the jail's default route. Idempotent by way
// of the caller, same reasoning as SetAddress: `route add default`
// fails outright if a default route already exists.
func (m *Manager) SetDefaultRoute(ctx context.Context, jail, gw string) error {
	if _, err := m.run(ctx, "jexec", jail, "route", "add", "default", gw); err != nil {
		return fmt.Errorf("setting default route %s inside jail %q: %w", gw, jail, err)
	}
	return nil
}

// DeleteDefaultRoute removes the jail's default route. A no-op if there
// is none, same reasoning as DeleteAddress.
func (m *Manager) DeleteDefaultRoute(ctx context.Context, jail string) error {
	_, err := m.run(ctx, "jexec", jail, "route", "delete", "default")
	if err != nil && !strings.Contains(err.Error(), "not in table") {
		return fmt.Errorf("removing default route inside jail %q: %w", jail, err)
	}
	return nil
}

// parseIfconfigAddresses extracts IPv4 addresses from `ifconfig <iface>`
// output inside a jail, e.g.
//
//	epair0b: flags=8843<UP,BROADCAST,RUNNING,SIMPLEX,MULTICAST> metric 0 mtu 1500
//	        options=3<RXCSUM,TXCSUM,VLAN_MTU>
//	        ether 02:1a:2b:3c:4d:5e
//	        inet 10.0.1.5 netmask 0xffffff00 broadcast 10.0.1.255
//	        media: Ethernet autoselect
//	        status: active
//
// Two spellings of the address field are accepted because both exist in
// the wild across FreeBSD releases: the classic "inet <ip> netmask
// 0x<hex>", and the newer "inet <ip>/<len>". Only indented "inet "
// lines are considered, so a line that merely mentions the word inet in
// some flag list cannot be mistaken for an address assignment.
// An unparseable "inet" line is skipped rather than guessed at: silently
// treating an address this code cannot read as "no address" would turn
// a parsing gap into a false drift report and then a redundant (failing)
// re-add on every tick.
func parseIfconfigAddresses(out string) []Address {
	var addrs []Address
	for _, line := range strings.Split(out, "\n") {
		if line == "" || line[0] != ' ' && line[0] != '\t' {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 || fields[0] != "inet" {
			continue
		}
		addr, ok := addressFromFields(fields)
		if !ok {
			continue
		}
		addrs = append(addrs, addr)
	}
	return addrs
}

// addressFromFields builds an Address from one already-tokenized "inet"
// line, using the netmask or inline /len whichever is present.
func addressFromFields(fields []string) (Address, bool) {
	addr := Address{PrefixLen: -1}

	// The prefix length can be attached to the address itself, as in
	// `inet 10.0.1.5/26`. It has to be split off before parsing, or
	// net.ParseIP rejects the whole token and a perfectly ordinary
	// address silently reads as unparseable.
	ipField := fields[1]
	if slash := strings.IndexByte(ipField, '/'); slash >= 0 {
		n, err := strconv.Atoi(ipField[slash+1:])
		if err != nil || n < 0 || n > 32 {
			return Address{}, false
		}
		ipField = ipField[:slash]
		addr.PrefixLen = n
	}
	ip := net.ParseIP(ipField)
	if ip == nil || ip.To4() == nil {
		return Address{}, false
	}
	// Canonicalized so comparison against a desired address is a
	// plain string match: net.ParseIP's rendering and the FSM's own
	// allocation agree on the canonical form, but relying on that
	// silently would make idempotency depend on it.
	addr.IP = ip.To4().String()

	// Otherwise the prefix length comes from a netmask, which ifconfig
	// separates from the keyword by whitespace: `inet 10.0.1.5 netmask
	// 0xffffff00 broadcast 10.0.1.255`. The run-together `netmask=` form
	// is accepted too, since it is what the same field renders as under
	// other ifconfig output formats.
	rest := fields[2:]
	for i := 0; i < len(rest); i++ {
		var value string
		switch {
		case rest[i] == "netmask":
			if i+1 >= len(rest) {
				return Address{}, false
			}
			value = rest[i+1]
			i++
		case strings.HasPrefix(rest[i], "netmask="):
			value = strings.TrimPrefix(rest[i], "netmask=")
		default:
			continue
		}
		n, ok := netmaskPrefixLen(value)
		if !ok {
			return Address{}, false
		}
		addr.PrefixLen = n
	}

	if addr.PrefixLen < 0 {
		// No prefix information at all: a /32 is the only honest
		// reading, and refusing to report the address at all is
		// better than reporting a wrong one.
		return Address{}, false
	}
	return addr, true
}

// netmaskPrefixLen converts an ifconfig(8) netmask rendering - a dotted
// quad, optionally dotted-hex bytewise with or without a "0x" prefix,
// e.g. "0xffffff00", "0xff.ff.ff.00", "255.255.255.0" - into a prefix
// length. ok is false if the value is not a contiguous IPv4 mask.
func netmaskPrefixLen(s string) (int, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, false
	}
	raw := make(net.IPMask, 4)
	if strings.Contains(s, ".") {
		if strings.HasPrefix(s, "0x") || strings.HasPrefix(s, "0X") {
			// Dotted-hex bytewise form: each component is two hex
			// digits, e.g. "ff.ff.ff.00".
			parts := strings.Split(s, ".")
			if len(parts) != 4 {
				return 0, false
			}
			for i, p := range parts {
				p = strings.TrimPrefix(strings.TrimPrefix(p, "0x"), "0X")
				if len(p) == 0 || len(p) > 2 {
					return 0, false
				}
				v, err := strconv.ParseUint(p, 16, 8)
				if err != nil {
					return 0, false
				}
				raw[i] = byte(v)
			}
		} else {
			ip := net.ParseIP(s)
			if ip == nil || ip.To4() == nil {
				return 0, false
			}
			copy(raw, ip.To4())
		}
	} else {
		// Plain hex, with or without the 0x prefix.
		s = strings.TrimPrefix(strings.TrimPrefix(s, "0x"), "0X")
		if len(s) == 0 || len(s) > 8 {
			return 0, false
		}
		v, err := strconv.ParseUint(s, 16, 64)
		if err != nil {
			return 0, false
		}
		binary.BigEndian.PutUint32(raw, uint32(v))
	}

	ones, bits := raw.Size()
	if bits != 32 {
		return 0, false
	}
	// Size() reports a non-contiguous mask's total set bits, so confirm
	// the mask really is the contiguous prefix of that length.
	for i, b := range raw {
		want := byte(0)
		if i < ones/8 {
			want = 0xff
		} else if i == ones/8 {
			want = byte(0xff << (8 - ones%8))
		}
		if b != want {
			return 0, false
		}
	}
	return ones, true
}

// parseNetstatDefaultRoute extracts the IPv4 default gateway from
// `netstat -rn -f inet` output, e.g.
//
//	Routing tables
//	Destination        Gateway            Netif Addrs
//	0.0.0.0/0          10.0.1.1           epair0b  U
//	10.0.1.0/24        link#1             epair0b  U
//
// Both the "0.0.0.0/0" and the older bare "default" destination
// spellings are accepted. An unparseable row yields "" - no default
// route observed - rather than a guess; "" is also the honest reading
// of a routing table with no default row, so the two are reported the
// same way on purpose, and the reconciler treats "" as "observe the
// route, and repair it if one is wanted".
func parseNetstatDefaultRoute(out string) string {
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		if fields[0] != "0.0.0.0/0" && fields[0] != "default" {
			continue
		}
		if gw := net.ParseIP(fields[1]); gw != nil && gw.To4() != nil {
			return gw.To4().String()
		}
	}
	return ""
}
