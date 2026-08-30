package vlan

import "testing"

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
