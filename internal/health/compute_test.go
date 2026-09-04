package health

import (
	"testing"
	"time"
)

var baseTime = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

func healthySignals() NodeSignals {
	return NodeSignals{
		NodeID:                   "node-a",
		PeerReachability:         ReachabilityReachable,
		HeartbeatObserved:        true,
		HeartbeatOK:              true,
		MembershipObserved:       true,
		IsRaftMember:             true,
		Suffrage:                 SuffrageVoter,
		MembershipObservedAt:     baseTime,
		AppliedIndexObserved:     true,
		AppliedIndex:             100,
		LastLogIndex:             100,
		IndicesObservedAt:        baseTime,
		ReconcilerConfigured:     true,
		ReconcileIntervalSeconds: 30,
		ReconcileEverSucceeded:   true,
		LastReconcileSuccess:     baseTime.Add(-10 * time.Second),
		ReconcileObservedAt:      baseTime,
	}
}

func TestComputeNodeHealth_VoterUnreachableIsContradictory(t *testing.T) {
	s := healthySignals()
	s.PeerReachability = ReachabilityUnreachable
	s.HeartbeatObserved = false
	got := ComputeNodeHealth(s, baseTime)
	if got.Status != StatusContradictory {
		t.Errorf("Status = %v, want Contradictory (CODEX's own named example: voter marked unreachable)", got.Status)
	}
}

func TestComputeNodeHealth_NonvoterUnreachableIsDegradedNotContradictory(t *testing.T) {
	s := healthySignals()
	s.Suffrage = SuffrageNonvoter
	s.PeerReachability = ReachabilityUnreachable
	s.HeartbeatObserved = false
	got := ComputeNodeHealth(s, baseTime)
	if got.Status != StatusDegraded {
		t.Errorf("Status = %v, want Degraded - the sharper Contradictory rule must not fire for a non-voter", got.Status)
	}
}

func TestComputeNodeHealth_UnrecognizedSuffrageNeverHealthy(t *testing.T) {
	tests := []struct {
		name     string
		suffrage Suffrage
	}{
		{"raft's real Unknown value", ParseSuffrage("Unknown")},
		{"an unrecognized raw string", ParseSuffrage("Whatever")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := healthySignals()
			s.Suffrage = tt.suffrage
			got := ComputeNodeHealth(s, baseTime)
			if got.Status == StatusHealthy {
				t.Error("Status = Healthy, want anything but Healthy when suffrage is Unknown - raft's own uncertainty must never be promoted")
			}
			if got.Status != StatusUnknown {
				t.Errorf("Status = %v, want Unknown specifically", got.Status)
			}
		})
	}
}

func TestComputeNodeHealth_MembershipNotObservedIsUnknownRegardlessOfOtherSignals(t *testing.T) {
	s := healthySignals()
	s.MembershipObserved = false
	got := ComputeNodeHealth(s, baseTime)
	if got.Status != StatusUnknown {
		t.Errorf("Status = %v, want Unknown - membership itself was never observed, even though every other signal looks perfect", got.Status)
	}
}

func TestComputeNodeHealth_ReachableButHeartbeatFalseIsContradictory(t *testing.T) {
	s := healthySignals()
	s.HeartbeatOK = false
	got := ComputeNodeHealth(s, baseTime)
	if got.Status != StatusContradictory {
		t.Errorf("Status = %v, want Contradictory - reachable managerd whose own report says its raft is down", got.Status)
	}
}

func TestComputeNodeHealth_StaleNeverAttemptedVsDegradedActivelyFailing(t *testing.T) {
	t.Run("never attempted is Stale", func(t *testing.T) {
		s := healthySignals()
		s.ReconcileEverSucceeded = false
		s.ReconcileEverAttempted = false
		got := ComputeNodeHealth(s, baseTime)
		if got.Status != StatusStale {
			t.Errorf("Status = %v, want Stale", got.Status)
		}
	})
	t.Run("recently attempted but not succeeded is Degraded", func(t *testing.T) {
		s := healthySignals()
		s.ReconcileEverSucceeded = false
		s.ReconcileEverAttempted = true
		s.LastReconcileAttempt = baseTime.Add(-5 * time.Second)
		got := ComputeNodeHealth(s, baseTime)
		if got.Status != StatusDegraded {
			t.Errorf("Status = %v, want Degraded - actively trying, not succeeding, must not look the same as never having tried", got.Status)
		}
	})
}

func TestComputeNodeHealth_ReconcilerNotConfiguredDoesNotDragHealthyDown(t *testing.T) {
	s := healthySignals()
	s.ReconcilerConfigured = false
	s.ReconcileEverSucceeded = false
	s.ReconcileEverAttempted = false
	got := ComputeNodeHealth(s, baseTime)
	if got.Status != StatusHealthy {
		t.Errorf("Status = %v, want Healthy - a node with no reconciler configured must not be penalized for it", got.Status)
	}
	found := false
	for _, o := range got.Observations {
		if o.Source == "reconciler_last_success" && o.Value == "not_applicable" {
			found = true
		}
	}
	if !found {
		t.Error("expected a not_applicable reconciler observation to be present")
	}
}

func TestComputeNodeHealth_AppliedIndexNeverDrivesVerdict(t *testing.T) {
	s := healthySignals()
	s.AppliedIndex = 1
	s.LastLogIndex = 1_000_000 // a huge gap
	got := ComputeNodeHealth(s, baseTime)
	if got.Status != StatusHealthy {
		t.Errorf("Status = %v, want Healthy - a large applied-index gap must never by itself drive the verdict (last_log_index includes uncommitted entries)", got.Status)
	}
	found := false
	for _, o := range got.Observations {
		if o.Source == "raft_applied_index" {
			found = true
		}
	}
	if !found {
		t.Error("expected the applied-index gap to still be cited as a raw Observation")
	}
}

func TestComputeNodeHealth_PeerReachabilityUnknownIsUnknownNotHealthy(t *testing.T) {
	s := healthySignals()
	s.PeerReachability = ReachabilityUnknown
	s.HeartbeatObserved = false
	got := ComputeNodeHealth(s, baseTime)
	if got.Status != StatusUnknown {
		t.Errorf("Status = %v, want Unknown - unverifiable reachability must never look Healthy", got.Status)
	}
}

func TestComputeNodeHealth_ReconcileFreshnessScalesWithReportedInterval(t *testing.T) {
	elapsed := 2 * time.Minute

	t.Run("long interval node stays healthy", func(t *testing.T) {
		s := healthySignals()
		s.ReconcileIntervalSeconds = 60 // 3x = 180s, elapsed 120s is within
		s.LastReconcileSuccess = baseTime.Add(-elapsed)
		got := ComputeNodeHealth(s, baseTime)
		if got.Status != StatusHealthy {
			t.Errorf("Status = %v, want Healthy for a node with a long reconcile interval", got.Status)
		}
	})
	t.Run("short interval node goes stale for the same elapsed time", func(t *testing.T) {
		s := healthySignals()
		s.ReconcileIntervalSeconds = 10 // 3x = 30s, elapsed 120s exceeds it
		s.LastReconcileSuccess = baseTime.Add(-elapsed)
		s.ReconcileEverAttempted = false
		got := ComputeNodeHealth(s, baseTime)
		if got.Status != StatusStale {
			t.Errorf("Status = %v, want Stale - the SAME elapsed time must be stale for a node reporting a short interval", got.Status)
		}
	})
}

func TestParseSuffrage(t *testing.T) {
	tests := []struct {
		raw  string
		want Suffrage
	}{
		{"Voter", SuffrageVoter},
		{"Nonvoter", SuffrageNonvoter},
		{"Staging", SuffrageStaging},
		{"Unknown", SuffrageUnknown},
		{"garbage", SuffrageUnknown},
		{"", SuffrageUnknown},
	}
	for _, tt := range tests {
		if got := ParseSuffrage(tt.raw); got != tt.want {
			t.Errorf("ParseSuffrage(%q) = %q, want %q", tt.raw, got, tt.want)
		}
	}
}
