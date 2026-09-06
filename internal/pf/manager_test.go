package pf

import (
	"context"
	"testing"
)

// TestApplyNAT_RejectsNewlineInUplinkOrSubnet is the regression test for
// a 2026-09-06 security-audit finding: uplink (node-config data with no
// validation at the time) was interpolated verbatim into a pf ruleset
// loaded via `pfctl -a anchor -f -` - a newline would inject an
// arbitrary additional pf rule. This is checked before ApplyNAT ever
// shells out to pfctl, so it's testable without a real pf(8) binary.
func TestApplyNAT_RejectsNewlineInUplinkOrSubnet(t *testing.T) {
	m := &Manager{}
	ctx := context.Background()

	cases := []struct{ subnet, uplink string }{
		{subnet: "10.60.0.0/24", uplink: "re0\npass in all"},
		{subnet: "10.60.0.0/24\npass in all", uplink: "re0"},
	}
	for _, c := range cases {
		if err := m.ApplyNAT(ctx, "apiary/net-1", c.subnet, c.uplink); err == nil {
			t.Errorf("subnet=%q uplink=%q: ApplyNAT returned nil error, want a rejection", c.subnet, c.uplink)
		}
	}
}
