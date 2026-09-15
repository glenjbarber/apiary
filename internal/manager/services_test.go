package manager

import "testing"

// TestRestartableService_Allowlist confirms ADR-0102's allowlist
// change: restshimd is now restartable (stateless, non-consensus,
// UpdateRestshimdConfig needs to trigger it after a config save), and
// raftd remains excluded (consensus-critical) - UpdateRaftdConfig
// deliberately never auto-restarts it either, the same judgment
// applied consistently in two places.
func TestRestartableService_Allowlist(t *testing.T) {
	cases := []struct {
		name string
		want bool
	}{
		{"apiary_managerd", true},
		{"apiary_frontend", true},
		{"apiary_restshimd", true},
		{"apiary_raftd", false},
		{"apiary_unknown", false},
	}
	for _, c := range cases {
		if got := restartableService(c.name); got != c.want {
			t.Errorf("restartableService(%q) = %v, want %v", c.name, got, c.want)
		}
	}
}
