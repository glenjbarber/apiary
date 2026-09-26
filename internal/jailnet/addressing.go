package jailnet

import (
	"fmt"
	"net"
)

// Addressing is one jail's intended VNET addressing, derived from its
// NetworkDefinition and its FSM-assigned address. It is the "what
// should be true" half of every comparison in this package; everything
// else here is either pure derivation from it or the observed state it
// is compared against.
type Addressing struct {
	// Bridge is the bridge(4) interface this jail's epair host side
	// must be a member of - the same bridge the jail's NetworkDefinition
	// maps to, and the same one a VM on that network attaches its tap
	// to.
	Bridge string

	// Interface is the jail-side epair(4) end, as recorded from the
	// node-local epair state file. Empty means the pair has not been
	// provisioned yet, which the reconciler reads as "provision it" and
	// not as a failure.
	Interface string

	// IP is the jail's assigned IPv4 address, or "" when the jail has
	// no Apiary-assigned address at all. Empty is legitimate and
	// important: an uplink_bridged network skips allocation exactly as
	// it does for a VM, and a jail in that position is expected to
	// configure its own addressing. It is not a degraded mode and the
	// reconciler does not invent an address for it.
	IP string

	// PrefixLen is the subnet's prefix length.
	PrefixLen int

	// Gateway is the jail's default gateway: the network's
	// ExternalGateway if it has one (a real router already answers for
	// this subnet), otherwise the subnet's own first host address, which
	// is the address this node's own EnsureBridgeAddress puts on the
	// bridge. Empty when the jail has no address, since a jail with no
	// address on a subnet has no business defaulting to that subnet's
	// gateway either.
	Gateway string
}

// HasAddress reports whether this jail has an Apiary-assigned address
// at all. Callers use it to decide whether to reconcile addressing or
// deliberately leave the jail's own networking alone.
func (a Addressing) HasAddress() bool { return a.IP != "" }

// DeriveAddressing computes a jail's intended VNET addressing from its
// subnet and its assigned IP, and is pure: no shelling out, no state,
// no error paths that depend on the outside world. Keeping it pure is
// what makes the refusal cases below exhaustively testable on any host,
// which matters because they are the cases where Apiary must decline to
// act rather than guess.
//
// ip may be "" - a jail on a network that skips allocation (see
// Addressing.IP) - in which case only the prefix length and subnet
// sanity are computed and no gateway is produced.
func DeriveAddressing(bridge, iface, subnet, ip, externalGateway string) (Addressing, error) {
	if bridge == "" {
		return Addressing{}, fmt.Errorf("no bridge name: a jail that names no network has no bridge to attach its vnet interface to")
	}
	_, ipnet, err := net.ParseCIDR(subnet)
	if err != nil {
		return Addressing{}, fmt.Errorf("subnet %q is not a valid CIDR: %w", subnet, err)
	}
	ones, bits := ipnet.Mask.Size()
	if bits != 32 {
		return Addressing{}, fmt.Errorf("subnet %q is IPv%d, but jail VNET addressing is IPv4-only: every other allocator in this project is IPv4, so reconciling an unmodeled address family would be guesswork", subnet, bits)
	}
	addr := Addressing{Bridge: bridge, Interface: iface, PrefixLen: ones}
	if ip == "" {
		return addr, nil
	}

	parsed := net.ParseIP(ip)
	if parsed == nil || parsed.To4() == nil {
		return Addressing{}, fmt.Errorf("assigned address %q is not an IPv4 address", ip)
	}
	addr.IP = parsed.To4().String()

	// Three refusals, each of which would otherwise produce a jail
	// that is quietly, permanently wrong:
	//
	//  1. An address outside the network's own subnet. A VNET jail
	//     reaches its peers by L2 on this network's bridge; an address
	//     from another subnet has no path to anything and, worse, looks
	//     configured.
	//  2. An address that is the subnet's network or broadcast address.
	//     The FSM's allocator skips both, so reaching here means the
	//     definition was hand-edited or the subnet changed underneath
	//     an existing jail.
	//  3. An address that collides with the bridge's own gateway. Two
	//     devices answering for one address produces intermittent,
	//     unreproducible ARP conflicts - the worst possible failure
	//     mode, and one Apiary caused rather than merely failed to
	//     prevent.
	if !ipnet.Contains(parsed) {
		return Addressing{}, fmt.Errorf("assigned address %s is not inside network subnet %s", addr.IP, ipnet.String())
	}
	// A /31 or /32 has no distinct network and broadcast address
	// (RFC 3021 point-to-point semantics), so neither of those two
	// refusals means anything there and applying them would reject a
	// legitimate host-to-host link.
	if ones <= 30 {
		if parsed.Equal(ipnet.IP.To4()) {
			return Addressing{}, fmt.Errorf("assigned address %s is the network address of subnet %s", addr.IP, ipnet.String())
		}
		if parsed.Equal(lastAddress(ipnet)) {
			return Addressing{}, fmt.Errorf("assigned address %s is the broadcast address of subnet %s", addr.IP, ipnet.String())
		}
	}

	if externalGateway != "" {
		gw := net.ParseIP(externalGateway)
		if gw == nil || gw.To4() == nil {
			return Addressing{}, fmt.Errorf("external gateway %q is not an IPv4 address", externalGateway)
		}
		addr.Gateway = gw.To4().String()
		return addr, nil
	}

	gw := firstHostAddress(ipnet)
	if gw == "" {
		return Addressing{}, fmt.Errorf("subnet %s has no usable gateway address", ipnet.String())
	}
	// The allocator reserves .0 and .1 (see raft.allocateIP), so a jail
	// can never legitimately be handed the bridge's own address - but
	// this is the last place before it is configured, so it is checked
	// here too rather than assumed.
	if addr.IP == gw {
		return Addressing{}, fmt.Errorf("assigned address %s is the gateway address of subnet %s", addr.IP, ipnet.String())
	}
	addr.Gateway = gw
	return addr, nil
}

// firstHostAddress returns the subnet's first host address - the same
// ".1" that internal/cluster's jailSubnetAddressing and vlan's
// gatewayCIDR both compute for the bridge itself, so the jail's gateway
// and the bridge's address are guaranteed to be the same value by
// construction rather than by two implementations agreeing.
func firstHostAddress(ipnet *net.IPNet) string {
	base := ipnet.IP.To4()
	if base == nil {
		return ""
	}
	return net.IPv4(base[0], base[1], base[2], base[3]|1).String()
}

// lastAddress returns a subnet's broadcast address, or nil if the
// subnet is IPv6 or otherwise has no broadcast address to compute.
func lastAddress(ipnet *net.IPNet) net.IP {
	ones, bits := ipnet.Mask.Size()
	if bits != 32 {
		return nil
	}
	ip := ipnet.IP.To4()
	if ip == nil {
		return nil
	}
	full, rem := ones/8, ones%8
	out := make(net.IP, len(ip))
	for i := range ip {
		// Bytes are most-significant first, so byte i lies wholly
		// inside the prefix only while i < full; byte full is the one
		// that may be part network and part host, and every byte after
		// it is entirely host bits.
		var hostMask byte
		switch {
		case i < full:
			hostMask = 0
		case rem == 0:
			// The prefix ends exactly on a byte boundary, so this byte
			// is already entirely host bits.
			hostMask = 0xff
		case i == full:
			hostMask = 0xff >> rem
		default:
			hostMask = 0xff
		}
		// net.ParseCIDR already masked off every host bit, so OR-ing the
		// host mask in sets exactly the broadcast and nothing else.
		out[i] = ip[i] | hostMask
	}
	return out
}
