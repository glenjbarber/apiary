package vlan

import (
	"context"
	"testing"
)

func TestGatewayCIDR(t *testing.T) {
	cases := []struct {
		subnet   string
		wantCIDR string
		wantIP   string
	}{
		{"10.60.0.0/24", "10.60.0.1/24", "10.60.0.1"},
		{"192.168.5.0/28", "192.168.5.1/28", "192.168.5.1"},
	}
	for _, c := range cases {
		cidr, ip, err := gatewayCIDR(c.subnet)
		if err != nil {
			t.Fatalf("gatewayCIDR(%q) error: %v", c.subnet, err)
		}
		if cidr != c.wantCIDR || ip != c.wantIP {
			t.Errorf("gatewayCIDR(%q) = (%q, %q), want (%q, %q)", c.subnet, cidr, ip, c.wantCIDR, c.wantIP)
		}
	}
}

func TestGatewayCIDR_RejectsInvalidSubnet(t *testing.T) {
	if _, _, err := gatewayCIDR("not-a-cidr"); err == nil {
		t.Errorf("gatewayCIDR(invalid) = nil error, want one")
	}
}

func TestVLANIfaceName(t *testing.T) {
	if got := vlanIfaceName(100); got != "vlan100" {
		t.Errorf("vlanIfaceName(100) = %q, want vlan100", got)
	}
}

// TestEnsureVLAN_UntaggedIsANoOp asserts EnsureVLAN(0) (the
// untagged/raw-uplink case, hit on every reconciler tick for any
// untagged network) never issues an ifconfig call at all. No root
// needed - the fake runner makes "no call" an assertion rather than an
// inference from a code path, and it matters more since ADR-0138 added
// an observation step ahead of the tagged path (ADR-0085's own
// guarantee: the untagged early return happens before any observation,
// so an administratively managed interface is never touched).
func TestEnsureVLAN_UntaggedIsANoOp(t *testing.T) {
	h := newFakeHost()
	m := &Manager{Uplink: "em0", Runner: h, Bridges: h}
	name, created, err := m.EnsureVLAN(context.Background(), 0)
	if err != nil {
		t.Fatalf("EnsureVLAN(0) error: %v", err)
	}
	if name != "em0" || created {
		t.Errorf("EnsureVLAN(0) = (%q, %v), want (\"em0\", false)", name, created)
	}
	if len(h.calls) != 0 {
		t.Errorf("EnsureVLAN(0) issued %v, want no commands at all", h.calls)
	}
}

// A tagged VLAN with no uplink configured is a configuration error, and
// it must not become a guess - in particular it must not fall through
// into the Bridge SVI observation with an empty interface name.
func TestEnsureVLAN_TaggedWithNoUplinkIsAnError(t *testing.T) {
	h := newFakeHost()
	m := &Manager{Runner: h, Bridges: h}
	if _, _, err := m.EnsureVLAN(context.Background(), 2); err == nil {
		t.Fatal("EnsureVLAN(2) with no uplink = nil error, want one")
	}
	if len(h.calls) != 0 {
		t.Errorf("EnsureVLAN(2) issued %v before failing on the missing uplink, want no commands", h.calls)
	}
}

// A failed tagging destroys the interface it just created rather than
// leaving a half-made VLAN behind - the same no-leftovers rule the
// reconciler applies to bridges it creates.
func TestEnsureVLAN_FailedTaggingDestroysWhatItCreated(t *testing.T) {
	h := newFakeHost().nic("em0")
	h.failTag = "vlandev: Invalid argument" // the kernel refuses this parent
	m := &Manager{Uplink: "em0", Runner: h, Bridges: h}

	if _, _, err := m.EnsureVLAN(context.Background(), 2); err == nil {
		t.Fatal("EnsureVLAN(2) with a refused vlandev = nil error, want one")
	}
	if !h.issued("vlan2 destroy") {
		t.Errorf("commands issued: %v, want a destroy of the interface it created", h.calls)
	}
}

func TestIsUp(t *testing.T) {
	cases := []struct {
		name string
		out  string
		want bool
	}{
		{
			name: "up bridge",
			out:  "apnet-abcd1234: flags=8843<UP,BROADCAST,RUNNING,SIMPLEX,MULTICAST> metric 0 mtu 1500\n\tether 58:9c:fc:10:f1:5b\n",
			want: true,
		},
		{
			name: "down bridge",
			out:  "apnet-abcd1234: flags=8802<BROADCAST,SIMPLEX,MULTICAST> metric 0 mtu 1500\n\tether 58:9c:fc:10:f1:5b\n",
			want: false,
		},
		{
			name: "malformed output has no flags",
			out:  "not ifconfig output at all\n",
			want: false,
		},
	}
	for _, c := range cases {
		if got := isUp(c.out); got != c.want {
			t.Errorf("%s: isUp() = %v, want %v", c.name, got, c.want)
		}
	}
}
