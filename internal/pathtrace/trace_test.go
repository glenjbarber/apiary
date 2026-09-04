package pathtrace

import (
	"strings"
	"testing"
)

func baseRequest() Request {
	return Request{
		Cell:        Cell{ID: "vm-1", Name: "web", NodeID: "hive-a", DesiredState: "running", Phase: "ready", NetworkID: "net-1", IPAddress: "10.60.0.10", MACAddress: "02:aa:bb:cc:dd:ee"},
		Network:     &Network{ID: "net-1", Name: "services", Subnet: "10.60.0.0/24"},
		Destination: "1.1.1.1",
		Protocol:    "tcp",
		Port:        443,
		Evidence:    Evidence{BridgeStatus: "up", PFObserved: true, PFEnabled: true, NATStatus: StatusClear, NATDetail: "bridge0 owns the default route"},
	}
}

func traceStep(t *testing.T, trace Trace, stage string) Step {
	t.Helper()
	for _, step := range trace.Steps {
		if step.Stage == stage {
			return step
		}
	}
	t.Fatalf("no %q step in %+v", stage, trace.Steps)
	return Step{}
}

func TestCompute_UnobservedTapKeepsManagedNATPathUnknown(t *testing.T) {
	trace, err := Compute(baseRequest())
	if err != nil {
		t.Fatal(err)
	}
	if trace.Status != StatusUnknown || !strings.Contains(trace.Summary, "Tap attachment") {
		t.Fatalf("trace = %+v, want tap evidence gap", trace)
	}
	if trace.ActiveProbe {
		t.Fatal("ActiveProbe = true, want false")
	}
}

func TestCompute_BridgeDownIsFirstBlocker(t *testing.T) {
	req := baseRequest()
	req.Evidence.BridgeStatus = "down"
	trace, err := Compute(req)
	if err != nil {
		t.Fatal(err)
	}
	if trace.Status != StatusBlocked || !strings.Contains(trace.Summary, "Owner-Hive bridge") {
		t.Fatalf("trace = %+v", trace)
	}
}

func TestCompute_StoppedCellIsFirstBlocker(t *testing.T) {
	req := baseRequest()
	req.Cell.DesiredState = "stopped"
	trace, err := Compute(req)
	if err != nil {
		t.Fatal(err)
	}
	if trace.Status != StatusBlocked || !strings.Contains(trace.Summary, "Cell") {
		t.Fatalf("trace = %+v", trace)
	}
}

func TestCompute_StaleNATAssumptionIsUnknown(t *testing.T) {
	req := baseRequest()
	req.Evidence.NATStatus = StatusUnknown
	trace, err := Compute(req)
	if err != nil {
		t.Fatal(err)
	}
	if step := traceStep(t, trace, "Route and NAT"); step.Status != StatusUnknown {
		t.Fatalf("route/NAT step = %+v, want unknown", step)
	}
}

func TestCompute_ExternalGatewayDoesNotRequireNAT(t *testing.T) {
	req := baseRequest()
	req.Network.ExternalGateway = "10.60.0.1"
	req.Evidence.PFObserved = false
	req.Evidence.NATStatus = StatusUnknown
	trace, err := Compute(req)
	if err != nil {
		t.Fatal(err)
	}
	if trace.Status != StatusUnknown {
		t.Fatalf("status = %q, want unknown: %+v", trace.Status, trace.Steps)
	}
	if step := traceStep(t, trace, "Route"); step.Status != StatusClear {
		t.Fatalf("route step = %+v, want clear", step)
	}
}

func TestCompute_OnLinkDestinationDoesNotRequireNAT(t *testing.T) {
	req := baseRequest()
	req.Destination = "10.60.0.25"
	req.Evidence.PFObserved = false
	req.Evidence.NATStatus = StatusUnknown
	trace, err := Compute(req)
	if err != nil {
		t.Fatal(err)
	}
	if trace.Status != StatusUnknown {
		t.Fatalf("status = %q, want unknown: %+v", trace.Status, trace.Steps)
	}
	if step := traceStep(t, trace, "Route"); step.Status != StatusClear {
		t.Fatalf("route step = %+v, want clear", step)
	}
}

func TestCompute_BlockingFirewallRule(t *testing.T) {
	req := baseRequest()
	req.Cell.FirewallRules = []Rule{{Direction: "out", Action: "pass", Protocol: "tcp", PortRange: "443"}, {Direction: "out", Action: "block", Protocol: "tcp", PortRange: "443"}}
	trace, err := Compute(req)
	if err != nil {
		t.Fatal(err)
	}
	if trace.Status != StatusBlocked || !strings.Contains(trace.Summary, "Firewall policy") {
		t.Fatalf("trace = %+v", trace)
	}
}

func TestCompute_BlockingRuleWithoutPFEnforcementIsUnknown(t *testing.T) {
	req := baseRequest()
	req.Evidence.PFEnabled = false
	req.Cell.FirewallRules = []Rule{{Direction: "out", Action: "block", Protocol: "tcp", PortRange: "443"}}
	trace, err := Compute(req)
	if err != nil {
		t.Fatal(err)
	}
	if step := traceStep(t, trace, "Firewall policy"); step.Status != StatusUnknown {
		t.Fatalf("firewall step = %+v, want unknown", step)
	}
}

func TestCompute_PortSpecificRulesNeedPort(t *testing.T) {
	req := baseRequest()
	req.Port = 0
	req.Cell.FirewallRules = []Rule{{Direction: "out", Action: "block", Protocol: "tcp", PortRange: "443"}}
	trace, err := Compute(req)
	if err != nil {
		t.Fatal(err)
	}
	if step := traceStep(t, trace, "Firewall policy"); step.Status != StatusUnknown {
		t.Fatalf("firewall step = %+v, want unknown", step)
	}
}

func TestCompute_PortSpecificPassDoesNotNeedPort(t *testing.T) {
	req := baseRequest()
	req.Port = 0
	req.Cell.FirewallRules = []Rule{{Direction: "out", Action: "pass", Protocol: "tcp", PortRange: "443"}}
	trace, err := Compute(req)
	if err != nil {
		t.Fatal(err)
	}
	if trace.Status != StatusUnknown {
		t.Fatalf("status = %q, want unknown: %+v", trace.Status, trace.Steps)
	}
	if step := traceStep(t, trace, "Firewall policy"); step.Status != StatusClear {
		t.Fatalf("firewall step = %+v, want clear", step)
	}
}

func TestCompute_HostnameMakesDNSUnknown(t *testing.T) {
	req := baseRequest()
	req.Destination = "example.com"
	trace, err := Compute(req)
	if err != nil {
		t.Fatal(err)
	}
	if step := traceStep(t, trace, "DNS"); step.Status != StatusUnknown {
		t.Fatalf("DNS step = %+v, want unknown", step)
	}
}

func TestCompute_FlatBridgeIsUnknown(t *testing.T) {
	req := baseRequest()
	req.Cell.NetworkID = ""
	trace, err := Compute(req)
	if err != nil {
		t.Fatal(err)
	}
	if trace.Status != StatusUnknown {
		t.Fatalf("status = %q, want unknown", trace.Status)
	}
}

func TestCompute_InvalidInputs(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*Request)
	}{
		{"empty destination", func(r *Request) { r.Destination = "" }},
		{"IPv6", func(r *Request) { r.Destination = "2001:db8::1" }},
		{"bad protocol", func(r *Request) { r.Protocol = "sctp" }},
		{"port without transport", func(r *Request) { r.Protocol = "icmp"; r.Port = 8 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := baseRequest()
			tc.mutate(&req)
			if _, err := Compute(req); err == nil {
				t.Fatal("error = nil")
			}
		})
	}
}
