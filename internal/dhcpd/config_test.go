package dhcpd

import (
	"strings"
	"testing"
)

func TestRenderConfig_SingleScopeNoLeases(t *testing.T) {
	body, err := RenderConfig([]NetworkScope{
		{Bridge: "apiary-net-1", Subnet: "10.60.0.0/24"},
	})
	if err != nil {
		t.Fatalf("RenderConfig() error: %v", err)
	}
	for _, want := range []string{
		"port=0",
		"bind-interfaces",
		"interface=apiary-net-1",
		"dhcp-range=10.60.0.2,10.60.0.254,255.255.255.0,12h",
	} {
		if !containsLine(body, want) {
			t.Errorf("RenderConfig() missing %q, got:\n%s", want, body)
		}
	}
}

func TestRenderConfig_WithLeases(t *testing.T) {
	body, err := RenderConfig([]NetworkScope{
		{
			Bridge: "apiary-net-1",
			Subnet: "10.60.0.0/24",
			Leases: []Lease{
				{MAC: "02:aa:bb:cc:dd:ee", IP: "10.60.0.5", Hostname: "vm-1"},
			},
		},
	})
	if err != nil {
		t.Fatalf("RenderConfig() error: %v", err)
	}
	if !containsLine(body, "dhcp-host=02:aa:bb:cc:dd:ee,10.60.0.5,vm-1") {
		t.Errorf("RenderConfig() missing the static lease line, got:\n%s", body)
	}
}

func TestRenderConfig_LeaseWithoutHostnameUsesWildcard(t *testing.T) {
	body, err := RenderConfig([]NetworkScope{
		{
			Bridge: "apiary-net-1",
			Subnet: "10.60.0.0/24",
			Leases: []Lease{{MAC: "02:aa:bb:cc:dd:ee", IP: "10.60.0.5"}},
		},
	})
	if err != nil {
		t.Fatalf("RenderConfig() error: %v", err)
	}
	if !containsLine(body, "dhcp-host=02:aa:bb:cc:dd:ee,10.60.0.5,*") {
		t.Errorf("RenderConfig() missing the wildcard-hostname lease line, got:\n%s", body)
	}
}

// TestRenderConfig_DNSServerOptionOmittedByDefault guards against the
// real bug this field fixes: without an explicit DNSServer, the config
// must never claim to offer one - dnsmasq's own default behavior
// (advertising itself as DNS server despite port=0 disabling its
// resolver) already produces a dead end on its own, so RenderConfig
// must not add anything that makes that worse or masks it.
func TestRenderConfig_DNSServerOptionOmittedByDefault(t *testing.T) {
	body, err := RenderConfig([]NetworkScope{
		{Bridge: "apiary-net-1", Subnet: "10.60.0.0/24"},
	})
	if err != nil {
		t.Fatalf("RenderConfig() error: %v", err)
	}
	if strings.Contains(body, "dhcp-option") {
		t.Errorf("RenderConfig() should not emit dhcp-option without a configured DNSServer, got:\n%s", body)
	}
}

func TestRenderConfig_DNSServerOptionScopedToInterface(t *testing.T) {
	body, err := RenderConfig([]NetworkScope{
		{Bridge: "apiary-net-1", Subnet: "10.60.0.0/24", DNSServer: "10.60.0.1"},
	})
	if err != nil {
		t.Fatalf("RenderConfig() error: %v", err)
	}
	if !containsLine(body, "dhcp-option=tag:apiary-net-1,6,10.60.0.1") {
		t.Errorf("RenderConfig() missing the DNS server option, got:\n%s", body)
	}
}

// TestRenderConfig_GatewayOptionOmittedByDefault guards the real bug
// this field fixes: without an explicit external Gateway, dnsmasq's own
// default (advertise the interface's own address as the router) is
// exactly what's wanted when Apiary's own bridge is the gateway, so
// RenderConfig must not override that on its own.
func TestRenderConfig_GatewayOptionOmittedByDefault(t *testing.T) {
	body, err := RenderConfig([]NetworkScope{
		{Bridge: "apiary-net-1", Subnet: "10.60.0.0/24"},
	})
	if err != nil {
		t.Fatalf("RenderConfig() error: %v", err)
	}
	if strings.Contains(body, ",3,") {
		t.Errorf("RenderConfig() should not emit a router option without a configured Gateway, got:\n%s", body)
	}
}

func TestRenderConfig_GatewayOptionScopedToInterface(t *testing.T) {
	body, err := RenderConfig([]NetworkScope{
		{Bridge: "apiary-net-1", Subnet: "10.60.0.0/24", Gateway: "10.60.0.1"},
	})
	if err != nil {
		t.Fatalf("RenderConfig() error: %v", err)
	}
	if !containsLine(body, "dhcp-option=tag:apiary-net-1,3,10.60.0.1") {
		t.Errorf("RenderConfig() missing the gateway/router option, got:\n%s", body)
	}
}

func TestRenderConfig_MultipleScopes(t *testing.T) {
	body, err := RenderConfig([]NetworkScope{
		{Bridge: "apiary-net-1", Subnet: "10.60.0.0/24"},
		{Bridge: "apiary-net-2", Subnet: "10.61.0.0/24"},
	})
	if err != nil {
		t.Fatalf("RenderConfig() error: %v", err)
	}
	if !containsLine(body, "interface=apiary-net-1") || !containsLine(body, "interface=apiary-net-2") {
		t.Errorf("RenderConfig() missing one of the two scopes' interface lines, got:\n%s", body)
	}
}

func TestRenderConfig_RejectsInvalidSubnet(t *testing.T) {
	_, err := RenderConfig([]NetworkScope{{Bridge: "br0", Subnet: "not-a-cidr"}})
	if err == nil {
		t.Errorf("RenderConfig() = nil error, want one for an invalid subnet")
	}
}

func TestRenderConfig_RejectsLargerThanSlash24(t *testing.T) {
	_, err := RenderConfig([]NetworkScope{{Bridge: "br0", Subnet: "10.60.0.0/16"}})
	if err == nil {
		t.Errorf("RenderConfig() = nil error, want one for a subnet larger than /24")
	}
}

func TestRenderConfig_RejectsEmptyBridge(t *testing.T) {
	_, err := RenderConfig([]NetworkScope{{Subnet: "10.60.0.0/24"}})
	if err == nil {
		t.Errorf("RenderConfig() = nil error, want one for a missing bridge")
	}
}

func TestRenderConfig_RejectsLeaseMissingFields(t *testing.T) {
	_, err := RenderConfig([]NetworkScope{
		{Bridge: "br0", Subnet: "10.60.0.0/24", Leases: []Lease{{MAC: "02:aa:bb:cc:dd:ee"}}},
	})
	if err == nil {
		t.Errorf("RenderConfig() = nil error, want one for a lease missing an IP")
	}
}

func TestDHCPRange_SmallerSubnet(t *testing.T) {
	start, end, mask, err := dhcpRange("192.168.5.0/28")
	if err != nil {
		t.Fatalf("dhcpRange() error: %v", err)
	}
	if start != "192.168.5.2" || end != "192.168.5.14" || mask != "255.255.255.240" {
		t.Errorf("dhcpRange(/28) = (%q, %q, %q), want (192.168.5.2, 192.168.5.14, 255.255.255.240)", start, end, mask)
	}
}

// TestFilterStaleLeases_EvictsLeaseConflictingWithReservation guards
// the real bug this function fixes: dnsmasq refuses to honor a
// dhcp-host reservation for an address still recorded as leased to a
// different MAC in its own lease file, and won't reconsider until that
// stale lease's timer naturally expires - confirmed live, every VM
// created on a reused subnet after the first got an effectively random
// address instead of its FSM-assigned one.
func TestFilterStaleLeases_EvictsLeaseConflictingWithReservation(t *testing.T) {
	leases := "1788464793 76:ad:59:4c:d8:f7 10.60.0.2 old-vm 01:76:ad:59:4c:d8:f7\n" +
		"1788493640 f6:7c:97:d2:94:a8 10.60.0.85 other-vm 01:f6:7c:97:d2:94:a8\n"
	reservations := map[string]string{"10.60.0.2": "46:b1:28:98:56:72"}

	got := filterStaleLeases(leases, reservations)

	if strings.Contains(got, "76:ad:59:4c:d8:f7") {
		t.Errorf("filterStaleLeases() kept the stale conflicting lease, got:\n%s", got)
	}
	if !strings.Contains(got, "f6:7c:97:d2:94:a8") {
		t.Errorf("filterStaleLeases() dropped an unrelated lease it shouldn't have, got:\n%s", got)
	}
}

// TestFilterStaleLeases_KeepsLeaseMatchingItsOwnReservation guards
// against over-eager eviction: a lease already held by the exact MAC
// its own dhcp-host reservation names must survive (dnsmasq will just
// keep renewing it - nothing stale about it).
func TestFilterStaleLeases_KeepsLeaseMatchingItsOwnReservation(t *testing.T) {
	leases := "1788464793 46:b1:28:98:56:72 10.60.0.2 vm-1 01:46:b1:28:98:56:72\n"
	reservations := map[string]string{"10.60.0.2": "46:B1:28:98:56:72"} // case differs, still a match

	got := filterStaleLeases(leases, reservations)

	if !strings.Contains(got, "10.60.0.2") {
		t.Errorf("filterStaleLeases() dropped a lease matching its own reservation, got:\n%s", got)
	}
}

// TestFilterStaleLeases_KeepsUnreservedLeases guards against evicting
// leases for addresses no scope has ever reserved at all (e.g. a plain
// dynamic-pool lease with no matching dhcp-host line).
func TestFilterStaleLeases_KeepsUnreservedLeases(t *testing.T) {
	leases := "1788464793 aa:bb:cc:dd:ee:ff 10.60.0.99 dynamic-vm 01:aa:bb:cc:dd:ee:ff\n"

	got := filterStaleLeases(leases, map[string]string{})

	if !strings.Contains(got, "10.60.0.99") {
		t.Errorf("filterStaleLeases() dropped an unreserved lease it shouldn't have, got:\n%s", got)
	}
}

// containsLine reports whether body contains want as one of its lines
// exactly (avoiding accidental substring false-positives/negatives from
// trailing whitespace differences).
func containsLine(body, want string) bool {
	for _, line := range splitLines(body) {
		if line == want {
			return true
		}
	}
	return false
}

func splitLines(s string) []string {
	var lines []string
	start := 0
	for i, r := range s {
		if r == '\n' {
			lines = append(lines, s[start:i])
			start = i + 1
		}
	}
	lines = append(lines, s[start:])
	return lines
}
