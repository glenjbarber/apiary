package frontend

import (
	"testing"

	"github.com/glenjbarber/apiary/internal/health"
)

func TestNewHealthCardView(t *testing.T) {
	v := newHealthCardView(3, 4, map[string]int{
		"ok":            2,
		"contradictory": 1,
		// "critical", "warn", "stale", "unknown", "not-applicable" left
		// absent on purpose - must render as zero, not be dropped.
	}, 1, 2)

	if v.ReachableCombs != 3 || v.TotalCombs != 4 {
		t.Fatalf("got ReachableCombs=%d TotalCombs=%d, want 3/4", v.ReachableCombs, v.TotalCombs)
	}
	if v.GuardrailHolds != 1 || v.PendingJoins != 2 {
		t.Fatalf("got GuardrailHolds=%d PendingJoins=%d, want 1/2", v.GuardrailHolds, v.PendingJoins)
	}
	if len(v.Stats) != len(healthCardStateOrder) {
		t.Fatalf("got %d stats, want all %d vocabulary states present", len(v.Stats), len(healthCardStateOrder))
	}

	byState := map[string]healthCardStat{}
	for _, s := range v.Stats {
		byState[s.State] = s
	}
	wantCounts := map[string]int{
		"ok": 2, "contradictory": 1,
		"critical": 0, "warn": 0, "stale": 0, "unknown": 0, "not-applicable": 0,
	}
	for state, want := range wantCounts {
		got, ok := byState[state]
		if !ok {
			t.Errorf("state %q missing from Stats - an absent-from-input state must still appear as zero", state)
			continue
		}
		if got.Count != want {
			t.Errorf("state %q Count = %d, want %d", state, got.Count, want)
		}
		if got.Label == "" {
			t.Errorf("state %q has an empty Label", state)
		}
	}

	// Worst-first order: critical before ok, wherever both appear.
	criticalIdx, okIdx := -1, -1
	for i, s := range v.Stats {
		if s.State == "critical" {
			criticalIdx = i
		}
		if s.State == "ok" {
			okIdx = i
		}
	}
	if criticalIdx == -1 || okIdx == -1 || criticalIdx > okIdx {
		t.Errorf("expected critical (index %d) to sort before ok (index %d)", criticalIdx, okIdx)
	}
}

func TestHealthCardVocabFor(t *testing.T) {
	cases := []struct {
		status health.Status
		want   string
	}{
		{health.StatusHealthy, "ok"},
		{health.StatusDegraded, "warn"},
		{health.StatusUnknown, "unknown"},
		{health.StatusStale, "stale"},
		{health.StatusContradictory, "contradictory"},
	}
	for _, c := range cases {
		if got := healthCardVocabFor(c.status); got != c.want {
			t.Errorf("healthCardVocabFor(%q) = %q, want %q", c.status, got, c.want)
		}
	}
}

func TestNewHealthCardViewFromNodes(t *testing.T) {
	nodes := []clusterNodeView{
		{NodeID: "a", Reachable: true, HealthStatus: health.StatusHealthy},
		{NodeID: "b", Reachable: true, HealthStatus: health.StatusHealthy},
		{NodeID: "c", Reachable: false, HealthStatus: health.StatusUnknown},
		{NodeID: "d", Reachable: true, HealthStatus: health.StatusContradictory},
	}
	v := newHealthCardViewFromNodes(nodes, 0, 3)

	if v.ReachableCombs != 3 || v.TotalCombs != 4 {
		t.Fatalf("got ReachableCombs=%d TotalCombs=%d, want 3/4", v.ReachableCombs, v.TotalCombs)
	}
	if v.PendingJoins != 3 {
		t.Errorf("PendingJoins = %d, want 3 (passed through unchanged)", v.PendingJoins)
	}
	byState := map[string]int{}
	for _, s := range v.Stats {
		byState[s.State] = s.Count
	}
	want := map[string]int{"ok": 2, "unknown": 1, "contradictory": 1, "critical": 0, "warn": 0, "stale": 0, "not-applicable": 0}
	for state, count := range want {
		if byState[state] != count {
			t.Errorf("state %q count = %d, want %d", state, byState[state], count)
		}
	}
	// The unreachable node (c) still counts toward TotalCombs and still
	// contributes its own HealthStatus (unknown) to the grid - being
	// unreachable does not erase it from the count of what was seen.
	if v.TotalCombs-v.ReachableCombs != 1 {
		t.Errorf("expected exactly one unreachable Comb accounted for")
	}
}
