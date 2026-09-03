package dhcpd

import (
	"fmt"
	"net"
	"strings"
)

// DefaultConfigPath is where dnsmasq(8) reads its configuration from by
// default on a pkg-installed FreeBSD system.
const DefaultConfigPath = "/usr/local/etc/dnsmasq.conf"

// DefaultLeaseFilePath is where dnsmasq(8) persists its lease database
// by default (no dhcp-leasefile= override is ever rendered into
// dnsmasq.conf, so this default always applies).
const DefaultLeaseFilePath = "/var/db/dnsmasq.leases"

// Lease is one VM's static DHCP assignment within a network.
type Lease struct {
	MAC      string
	IP       string
	Hostname string
}

// NetworkScope is one Apiary-managed network's dnsmasq configuration:
// the bridge interface it's reachable on, its subnet, and every VM
// currently assigned an IP within it.
type NetworkScope struct {
	Bridge string
	Subnet string // CIDR, e.g. "10.60.0.0/24"
	Leases []Lease

	// DNSServer, if set, is handed to DHCP clients on this network via
	// option 6. Deliberately not implied by dnsmasq's own default
	// behavior (advertising itself) - port=0 above disables dnsmasq's
	// resolver entirely, so a client left to that default gets a
	// DNS server address that never actually answers queries. This
	// was a real, previously-undiscovered bug: every VM on an
	// Apiary-managed network got a dead-end DNS server until this
	// field existed, invisible until a workload actually needed
	// working internet DNS resolution (a real kubeadm init's image
	// pulls were the first thing to need it).
	DNSServer string

	// Gateway, if set, is handed to DHCP clients on this network via
	// option 3 (router), overriding dnsmasq's own default behavior of
	// advertising the interface's own address. Set this when the
	// network has a real external router (NetworkDefinition's
	// ExternalGateway) rather than Apiary's own per-node bridge -
	// otherwise a client uses the interface address as its default
	// route, which is correct only when Apiary's bridge really is the
	// gateway.
	Gateway string
}

// RenderConfig renders a full dnsmasq.conf body serving DHCP for the
// given scopes. This is pure and requires no dnsmasq installed to test.
//
// port=0 disables dnsmasq's DNS resolver entirely - Apiary only wants
// DHCP, and leaving DNS enabled risks it answering queries on an
// interface/network it was never meant to serve. bind-interfaces
// restricts it to exactly the bridges listed here, never the uplink or
// any other interface on the host.
func RenderConfig(scopes []NetworkScope) (string, error) {
	var b strings.Builder
	b.WriteString("# Managed by Apiary (internal/dhcpd) - do not edit by hand.\n")
	b.WriteString("port=0\n")
	b.WriteString("bind-interfaces\n")
	b.WriteString("dhcp-authoritative\n\n")

	for _, s := range scopes {
		if s.Bridge == "" {
			return "", fmt.Errorf("dhcpd: scope subnet %q: bridge must not be empty", s.Subnet)
		}
		start, end, netmask, err := dhcpRange(s.Subnet)
		if err != nil {
			return "", err
		}

		fmt.Fprintf(&b, "interface=%s\n", s.Bridge)
		fmt.Fprintf(&b, "dhcp-range=%s,%s,%s,12h\n", start, end, netmask)
		// tag:<bridge>, not interface:<bridge> - a real, previously-
		// undiscovered bug found live via packet capture: "interface:"
		// is not a valid dnsmasq scope selector for dhcp-option (unlike
		// dhcp-range/interface= above, which do support literal
		// interface names). dnsmasq silently accepted the malformed
		// line and just never applied it - no error, no warning, no
		// option 6/3 in any DHCP reply, ever, on any network that ever
		// set DNSServer/Gateway - confirmed by a client's own requested
		// parameter list explicitly asking for option 6 and getting
		// back an ACK with no such option present at all. dnsmasq DOES
		// automatically tag every request with its arriving interface's
		// own name, usable as tag:<name> - that's the correct selector.
		if s.DNSServer != "" {
			fmt.Fprintf(&b, "dhcp-option=tag:%s,6,%s\n", s.Bridge, s.DNSServer)
		}
		if s.Gateway != "" {
			fmt.Fprintf(&b, "dhcp-option=tag:%s,3,%s\n", s.Bridge, s.Gateway)
		}
		for _, l := range s.Leases {
			if l.MAC == "" || l.IP == "" {
				return "", fmt.Errorf("dhcpd: scope %q: lease MAC and IP must both be set", s.Bridge)
			}
			name := l.Hostname
			if name == "" {
				name = "*"
			}
			fmt.Fprintf(&b, "dhcp-host=%s,%s,%s\n", l.MAC, l.IP, name)
		}
		b.WriteString("\n")
	}
	return b.String(), nil
}

// filterStaleLeases returns leaseFileBody (dnsmasq's own lease-database
// format: "<expiry> <mac> <ip> <hostname> <client-id>" per line) with
// any line removed whose IP is now reserved (via a scope's Lease -
// i.e. a dhcp-host line) for a *different* MAC than the one holding
// that lease.
//
// This exists because of a real, confirmed bug: dnsmasq refuses to
// honor a dhcp-host static reservation for an address still recorded
// as leased to someone else in its own persistent lease file - logged
// as "not using configured address <ip> because it is leased to <mac>"
// - and won't reconsider until that old lease's own timer naturally
// expires (up to the full lease-time, 12h here), regardless of what
// dnsmasq.conf now says. Since this project's networks routinely reuse
// the same subnet across a NetworkDefinition's delete+recreate (there's
// no Update RPC - see ADR-0047) or a VM's disposable-and-recreated
// lifecycle (this session's own CAPI-driven VM churn), a stale lease
// from a long-gone VM permanently blocked every subsequent VM's FSM-
// assigned IP (internal/raft's allocateIP) from ever actually being
// granted - confirmed live: every VM created after the first ended up
// with an effectively random pool address instead of its intended one.
// Comparison is case-insensitive since dnsmasq itself normalizes MACs
// to lowercase in this file, but a caller-supplied reservation's MAC
// case shouldn't be assumed.
func filterStaleLeases(leaseFileBody string, reservations map[string]string) string {
	var kept []string
	for _, line := range strings.Split(leaseFileBody, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 3 {
			kept = append(kept, line)
			continue
		}
		mac, ip := fields[1], fields[2]
		if reservedMAC, ok := reservations[ip]; ok && !strings.EqualFold(reservedMAC, mac) {
			continue
		}
		kept = append(kept, line)
	}
	if len(kept) == 0 {
		return ""
	}
	return strings.Join(kept, "\n") + "\n"
}

// dhcpRange computes the DHCP-servable range for subnet: from the first
// address after the reserved gateway (.1, see internal/vlan's
// EnsureBridgeAddress and internal/raft's allocateIP, both of which
// also reserve it) through the last host address before the broadcast
// address, plus the subnet's dotted-decimal netmask.
func dhcpRange(subnet string) (start, end, netmask string, err error) {
	_, ipnet, err := net.ParseCIDR(subnet)
	if err != nil {
		return "", "", "", fmt.Errorf("dhcpd: invalid subnet %q: %w", subnet, err)
	}
	base := ipnet.IP.To4()
	if base == nil {
		return "", "", "", fmt.Errorf("dhcpd: subnet %q is not IPv4", subnet)
	}
	mask := net.IP(ipnet.Mask).String()

	ones, bits := ipnet.Mask.Size()
	hostBits := bits - ones
	if hostBits < 2 {
		return "", "", "", fmt.Errorf("dhcpd: subnet %q is too small to serve any DHCP range", subnet)
	}
	// Only /24-or-smaller subnets are supported for now: the arithmetic
	// below only varies the last octet, matching every other size
	// assumption already made for this project's home-lab scale (see
	// internal/raft's own IP-allocation range checks). A real need for
	// larger networks would need real multi-octet arithmetic here.
	if hostBits > 8 {
		return "", "", "", fmt.Errorf("dhcpd: subnet %q is larger than /24, not supported yet", subnet)
	}

	startIP := net.IPv4(base[0], base[1], base[2], base[3]+2)
	lastHostOffset := base[3] + byte(1<<uint(hostBits)-2) // broadcast - 1
	endIP := net.IPv4(base[0], base[1], base[2], lastHostOffset)

	return startIP.String(), endIP.String(), mask, nil
}
