package frontend

import "testing"

func TestAssessReplicaFreshness(t *testing.T) {
	complete := []hastObservationView{
		{Role: "primary", Status: "complete", Replication: "memsync", ObservedAt: "2026-09-24T12:00:00Z"},
		{Role: "secondary", Status: "complete", Replication: "memsync", ObservedAt: "2026-09-24T12:00:00Z"},
	}
	tests := []struct {
		name         string
		observations []hastObservationView
		want         string
	}{
		{name: "complete is point-in-time evidence", observations: complete, want: "complete-observed"},
		{name: "missing observation is unknown", observations: complete[:1], want: "unknown"},
		{name: "transport error is unknown", observations: []hastObservationView{{Error: "offline"}, complete[1]}, want: "unknown"},
		{name: "missing timestamp is unknown", observations: []hastObservationView{{Role: "primary", Status: "complete", Replication: "memsync"}, complete[1]}, want: "unknown"},
		{name: "wrong role is degraded", observations: []hastObservationView{{Role: "secondary", Status: "complete", Replication: "memsync", ObservedAt: "now"}, complete[1]}, want: "degraded"},
		{name: "noncomplete is degraded", observations: []hastObservationView{complete[0], {Role: "secondary", Status: "degraded", Replication: "memsync", ObservedAt: "now"}}, want: "degraded"},
		{name: "mode mismatch is unknown", observations: []hastObservationView{complete[0], {Role: "secondary", Status: "complete", Replication: "async", ObservedAt: "now"}}, want: "unknown"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, _ := assessReplicaFreshness(tt.observations)
			if got != tt.want {
				t.Fatalf("assessReplicaFreshness() = %q, want %q", got, tt.want)
			}
		})
	}
}
