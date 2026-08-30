package pf

import "testing"

func TestRenderRules_SimpleBlock(t *testing.T) {
	body, err := RenderRules([]Rule{{Direction: "in", Action: "block", Protocol: "tcp", PortRange: "22"}})
	if err != nil {
		t.Fatalf("RenderRules() error: %v", err)
	}
	want := "block in proto tcp from any to any port 22\n"
	if body != want {
		t.Errorf("RenderRules() = %q, want %q", body, want)
	}
}

func TestRenderRules_PortRangeUsesColonSyntax(t *testing.T) {
	body, err := RenderRules([]Rule{{Direction: "out", Action: "pass", Protocol: "udp", PortRange: "8000-9000"}})
	if err != nil {
		t.Fatalf("RenderRules() error: %v", err)
	}
	want := "pass out proto udp from any to any port 8000:9000\n"
	if body != want {
		t.Errorf("RenderRules() = %q, want %q", body, want)
	}
}

func TestRenderRules_NoProtocolOrPort(t *testing.T) {
	body, err := RenderRules([]Rule{{Direction: "in", Action: "block"}})
	if err != nil {
		t.Fatalf("RenderRules() error: %v", err)
	}
	want := "block in from any to any\n"
	if body != want {
		t.Errorf("RenderRules() = %q, want %q", body, want)
	}
}

func TestRenderRules_ICMPWithNoPort(t *testing.T) {
	body, err := RenderRules([]Rule{{Direction: "in", Action: "pass", Protocol: "icmp"}})
	if err != nil {
		t.Fatalf("RenderRules() error: %v", err)
	}
	want := "pass in proto icmp from any to any\n"
	if body != want {
		t.Errorf("RenderRules() = %q, want %q", body, want)
	}
}

func TestRenderRules_MultipleRules(t *testing.T) {
	body, err := RenderRules([]Rule{
		{Direction: "in", Action: "pass", Protocol: "tcp", PortRange: "22"},
		{Direction: "in", Action: "block", Protocol: "tcp", PortRange: "23"},
	})
	if err != nil {
		t.Fatalf("RenderRules() error: %v", err)
	}
	want := "pass in proto tcp from any to any port 22\nblock in proto tcp from any to any port 23\n"
	if body != want {
		t.Errorf("RenderRules() = %q, want %q", body, want)
	}
}

func TestRenderRules_NoRulesIsEmptyBody(t *testing.T) {
	body, err := RenderRules(nil)
	if err != nil {
		t.Fatalf("RenderRules() error: %v", err)
	}
	if body != "" {
		t.Errorf("RenderRules(nil) = %q, want empty", body)
	}
}

func TestRenderRules_RejectsInvalidDirection(t *testing.T) {
	if _, err := RenderRules([]Rule{{Direction: "sideways", Action: "pass"}}); err == nil {
		t.Errorf("RenderRules() = nil error, want one for an invalid direction")
	}
}

func TestRenderRules_RejectsInvalidAction(t *testing.T) {
	if _, err := RenderRules([]Rule{{Direction: "in", Action: "maybe"}}); err == nil {
		t.Errorf("RenderRules() = nil error, want one for an invalid action")
	}
}

func TestRenderRules_RejectsInvalidProtocol(t *testing.T) {
	if _, err := RenderRules([]Rule{{Direction: "in", Action: "pass", Protocol: "sctp"}}); err == nil {
		t.Errorf("RenderRules() = nil error, want one for an unsupported protocol")
	}
}

func TestRenderRules_RejectsPortRangeWithoutTCPOrUDP(t *testing.T) {
	if _, err := RenderRules([]Rule{{Direction: "in", Action: "pass", Protocol: "icmp", PortRange: "22"}}); err == nil {
		t.Errorf("RenderRules() = nil error, want one for a port range on icmp")
	}
	if _, err := RenderRules([]Rule{{Direction: "in", Action: "pass", PortRange: "22"}}); err == nil {
		t.Errorf("RenderRules() = nil error, want one for a port range with no protocol")
	}
}

func TestRenderRules_RejectsInvalidPortRange(t *testing.T) {
	cases := []string{"", "abc", "0", "70000", "9000-8000", "22-"}
	for _, c := range cases {
		if c == "" {
			continue // empty PortRange is valid (means "any")
		}
		if _, err := RenderRules([]Rule{{Direction: "in", Action: "pass", Protocol: "tcp", PortRange: c}}); err == nil {
			t.Errorf("RenderRules() with PortRange=%q = nil error, want one", c)
		}
	}
}
