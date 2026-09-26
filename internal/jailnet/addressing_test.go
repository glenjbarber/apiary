package jailnet

import (
	"net"
	"strings"
	"testing"
)

// TestDeriveAddressing confirms the intended addressing for the common
// cases, including the two ways a gateway is chosen: the network's own
// external gateway when it has one, and the subnet's first host address
// (the same ".1" the bridge itself gets) when it does not.
func TestDeriveAddressing(t *testing.T) {
	tests := []struct {
		name     string
		bridge   string
		subnet   string
		ip       string
		external string
		want     Addressing
		wantErr  string
	}{
		{
			name:   "no external gateway, so the bridge's own address is the gateway",
			bridge: "bridge1", subnet: "10.0.1.0/24", ip: "10.0.1.5",
			want: Addressing{Bridge: "bridge1", Interface: "epair0b", IP: "10.0.1.5", PrefixLen: 24, Gateway: "10.0.1.1"},
		},
		{
			name:   "a real external router answers for the subnet",
			bridge: "bridge1", subnet: "10.0.1.0/24", ip: "10.0.1.5", external: "10.0.0.1",
			want: Addressing{Bridge: "bridge1", Interface: "epair0b", IP: "10.0.1.5", PrefixLen: 24, Gateway: "10.0.0.1"},
		},
		{
			name:   "a non-byte-aligned prefix",
			bridge: "bridge3", subnet: "192.168.7.64/26", ip: "192.168.7.70",
			want: Addressing{Bridge: "bridge3", Interface: "epair0b", IP: "192.168.7.70", PrefixLen: 26, Gateway: "192.168.7.65"},
		},
		{
			name:   "a /32 leaves no usable gateway, which is refused rather than guessed",
			bridge: "bridge4", subnet: "10.9.9.9/32", ip: "10.9.9.9",
			want: Addressing{}, wantErr: "gateway",
		},
		{
			name:   "a /31 peer pair, where each end is the other's gateway",
			bridge: "bridge5", subnet: "10.0.0.0/31", ip: "10.0.0.0",
			want: Addressing{Bridge: "bridge5", Interface: "epair0b", IP: "10.0.0.0", PrefixLen: 31, Gateway: "10.0.0.1"},
		},
		{
			// A /31 jail holding the ".1" end has no distinct gateway:
			// the only candidate is itself. Refused rather than
			// self-routed, which would black-hole the jail's traffic
			// with a configuration that looks entirely normal.
			name:   "a /31 jail on the address that would be the gateway",
			bridge: "bridge5", subnet: "10.0.0.0/31", ip: "10.0.0.1",
			wantErr: "gateway address",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := DeriveAddressing(tt.bridge, "epair0b", tt.subnet, tt.ip, tt.external)
			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("DeriveAddressing() = %+v, want an error mentioning %q", got, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("DeriveAddressing() error = %v", err)
			}
			if got != tt.want {
				t.Errorf("DeriveAddressing() = %+v, want %+v", got, tt.want)
			}
		})
	}
}

// TestDeriveAddressing_UnallocatedJail confirms a jail on a network that
// skips allocation (ADR-0117's uplink_bridged carve-out, the same one
// internal/raft's allocator already makes for VMs) still gets a valid
// prefix length but no address and no gateway. Inventing a gateway for
// an addressless jail would point it at a router it has no reason to
// use.
func TestDeriveAddressing_UnallocatedJail(t *testing.T) {
	got, err := DeriveAddressing("bridge1", "", "10.0.1.0/24", "", "")
	if err != nil {
		t.Fatalf("DeriveAddressing() error = %v", err)
	}
	if got.HasAddress() {
		t.Errorf("HasAddress() = true, want false")
	}
	if got.Gateway != "" {
		t.Errorf("Gateway = %q, want empty", got.Gateway)
	}
	if got.PrefixLen != 24 {
		t.Errorf("PrefixLen = %d, want 24", got.PrefixLen)
	}
	// A missing interface is not a failure: it means the pair has not
	// been provisioned yet, which is the reconciler's cue to make one.
	if got.Interface != "" {
		t.Errorf("Interface = %q, want empty before provisioning", got.Interface)
	}
}

// TestDeriveAddressing_Refusals is the enforcement half of Stage 2.
// Each of these is a case where acting on the desired state would leave
// a jail quietly, permanently wrong - so the reconciler must decline
// rather than apply and hope.
func TestDeriveAddressing_Refusals(t *testing.T) {
	tests := []struct {
		name     string
		bridge   string
		subnet   string
		ip       string
		external string
		wantErr  string
	}{
		{
			name:   "an address from another subnet has no path to anything here",
			bridge: "bridge1", subnet: "10.0.1.0/24", ip: "192.168.5.5",
			wantErr: "not inside",
		},
		{
			name:   "the subnet's own network address",
			bridge: "bridge1", subnet: "10.0.1.0/24", ip: "10.0.1.0",
			wantErr: "network address",
		},
		{
			name:   "the subnet's broadcast address",
			bridge: "bridge1", subnet: "10.0.1.0/24", ip: "10.0.1.255",
			wantErr: "broadcast address",
		},
		{
			name:   "the bridge's own gateway address, which would ARP-conflict",
			bridge: "bridge1", subnet: "10.0.1.0/24", ip: "10.0.1.1",
			wantErr: "gateway address",
		},
		{
			name:   "a hand-edited address is not silently accepted",
			bridge: "bridge1", subnet: "10.0.1.0/24", ip: "not-an-ip",
			wantErr: "not an IPv4",
		},
		{
			name:   "IPv6 is out of scope for this design",
			bridge: "bridge1", subnet: "10.0.1.0/24", ip: "2001:db8::1",
			wantErr: "not an IPv4",
		},
		{
			name:   "an IPv6 subnet is refused rather than half-handled",
			bridge: "bridge1", subnet: "2001:db8::/64", ip: "",
			wantErr: "IPv4-only",
		},
		{
			name:   "a malformed subnet",
			bridge: "bridge1", subnet: "10.0.1.0", ip: "10.0.1.5",
			wantErr: "not a valid CIDR",
		},
		{
			name:   "a jail that names no network has no bridge to attach to",
			bridge: "", subnet: "10.0.1.0/24", ip: "10.0.1.5",
			wantErr: "no bridge name",
		},
		{
			name:   "an unusable external gateway",
			bridge: "bridge1", subnet: "10.0.1.0/24", ip: "10.0.1.5", external: "nope",
			wantErr: "not an IPv4",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := DeriveAddressing(tt.bridge, "epair0b", tt.subnet, tt.ip, tt.external)
			if err == nil {
				t.Fatalf("DeriveAddressing() = %+v, want an error mentioning %q", got, tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error = %v, want it to mention %q", err, tt.wantErr)
			}
		})
	}
}

// TestDeriveAddressing_GatewayMatchesTheBridge is the guarantee that
// the jail's default gateway and the address the bridge itself is given
// are the same value. They are computed independently by this package
// and by internal/vlan's gatewayCIDR, and the only durable protection
// against the two disagreeing is a test that states they must not.
func TestDeriveAddressing_GatewayMatchesTheBridge(t *testing.T) {
	for subnet, host := range map[string]string{
		"10.0.1.0/24":    "10.0.1.5",
		"192.168.0.0/16": "192.168.4.7",
		"172.16.4.0/22":  "172.16.4.9",
		"10.5.0.0/20":    "10.5.1.3",
	} {
		got, err := DeriveAddressing("bridge1", "epair0b", subnet, host, "")
		if err != nil {
			t.Fatalf("DeriveAddressing(%q) error = %v", subnet, err)
		}
		// A ".1" within the subnet is what vlan.gatewayCIDR assigns the
		// bridge, so that is what a jail's default route must be - and
		// the two are computed independently, so this is the only
		// thing keeping them from drifting apart.
		_, ipnet, err := net.ParseCIDR(subnet)
		if err != nil {
			t.Fatalf("ParseCIDR(%q) error = %v", subnet, err)
		}
		base := ipnet.IP.To4()
		want := net.IPv4(base[0], base[1], base[2], base[3]|1).String()
		if got.Gateway != want {
			t.Errorf("subnet %s: Gateway = %q, want %q", subnet, got.Gateway, want)
		}
	}
}
