package nodeconfig

import "testing"

// ADR-0158: the HAST tunnel address plan's format validation.

func TestValidateHASTTunnelAddresses(t *testing.T) {
	cases := []struct {
		name    string
		value   string
		wantErr bool
	}{
		{name: "empty is allowed", value: ""},
		{name: "single entry", value: "node-a=10.90.0.1:8457"},
		{name: "two entries", value: "node-a=10.90.0.1:8457,node-b=10.90.0.2:8457"},
		{name: "whitespace tolerated", value: " node-a = 10.90.0.1:8457 "},
		{name: "missing equals sign is rejected", value: "node-a", wantErr: true},
		{name: "empty node id is rejected", value: "=10.90.0.1:8457", wantErr: true},
		{name: "empty address is rejected", value: "node-a=", wantErr: true},
		{name: "address without a port is rejected", value: "node-a=10.90.0.1", wantErr: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateHASTTunnelAddresses(tc.value)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("validateHASTTunnelAddresses(%q) = nil error, want rejection", tc.value)
				}
				return
			}
			if err != nil {
				t.Fatalf("validateHASTTunnelAddresses(%q) error: %v", tc.value, err)
			}
		})
	}
}

// A config carrying a malformed tunnel plan must fail validation, so a
// Comb never starts with a plan that cannot be parsed.
func TestValidateHASTTunnelAddresses_RejectsAMalformedPlanInAConfig(t *testing.T) {
	cfg := Config{HASTTunnelEnabled: true, HASTTunnelAddresses: "node-a=10.90.0.1"}
	if err := validate(cfg); err == nil {
		t.Fatal("Validate() = nil error with a malformed hast_tunnel_addresses plan, want rejection")
	}
}

// A valid plan must not break validation.
func TestValidateHASTTunnelAddresses_AcceptsAValidPlanInAConfig(t *testing.T) {
	cfg := Config{HASTTunnelEnabled: true, HASTTunnelAddresses: "node-a=10.90.0.1:8457,node-b=10.90.0.2:8457"}
	if err := validate(cfg); err != nil {
		t.Fatalf("Validate() error: %v", err)
	}
}
