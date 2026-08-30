package dhcpd

import (
	"fmt"
	"net"
	"strings"
)

// DefaultConfigPath is where dnsmasq(8) reads its configuration from by
// default on a pkg-installed FreeBSD system.
const DefaultConfigPath = "/usr/local/etc/dnsmasq.conf"

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
