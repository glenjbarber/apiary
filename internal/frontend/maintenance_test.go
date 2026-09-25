package frontend

import "testing"

func TestClassifyMaintenanceQuorum(t *testing.T) {
	tests := []struct {
		name      string
		reachable uint32
		unknown   uint32
		majority  uint32
		want      string
	}{
		{name: "known majority", reachable: 2, unknown: 1, majority: 2, want: "quorum-preserved"},
		{name: "possible only with unknown voters", reachable: 1, unknown: 1, majority: 2, want: "unknown"},
		{name: "known loss", reachable: 1, unknown: 0, majority: 2, want: "blocked"},
		{name: "missing quorum is unknown", reachable: 3, unknown: 0, majority: 0, want: "unknown"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, _, _ := classifyMaintenanceQuorum(tt.reachable, tt.unknown, tt.majority)
			if got != tt.want {
				t.Fatalf("classifyMaintenanceQuorum() = %q, want %q", got, tt.want)
			}
		})
	}
}
