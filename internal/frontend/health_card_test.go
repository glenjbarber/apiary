package frontend

import "testing"

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
