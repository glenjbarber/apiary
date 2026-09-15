package guardrail

import "testing"

func TestEvaluateJoinReachability_Reachable(t *testing.T) {
	got := EvaluateJoinReachability(JoinReachabilityFact{Address: "10.62.0.5:17600", Reachable: true})
	if got.Verdict != Allow {
		t.Errorf("Verdict = %v, want Allow", got.Verdict)
	}
	if len(got.Findings) != 0 {
		t.Errorf("Findings = %v, want none for an allowed verdict", got.Findings)
	}
}

func TestEvaluateJoinReachability_Unreachable(t *testing.T) {
	got := EvaluateJoinReachability(JoinReachabilityFact{Address: "10.62.0.5:17600", Reachable: false, DialError: "connection refused"})
	if got.Verdict != Block {
		t.Errorf("Verdict = %v, want Block", got.Verdict)
	}
	if len(got.Findings) != 1 || got.Findings[0].Rule != "join-reachability" {
		t.Errorf("Findings = %v, want one join-reachability finding", got.Findings)
	}
}

func TestEvaluateConcurrentManagerRestart_AllowsNonVoterImmediately(t *testing.T) {
	got := EvaluateConcurrentManagerRestart(RestartCooldownFact{
		TargetService:    "apiary_managerd",
		IsLocalNodeVoter: false,
		ReadOK:           false, // even a failed read must not matter for a non-voter
	})
	if got.Verdict != Allow {
		t.Errorf("Verdict = %v, want Allow for a non-voter regardless of read state", got.Verdict)
	}
}

func TestEvaluateConcurrentManagerRestart_UnknownOnReadFailure(t *testing.T) {
	got := EvaluateConcurrentManagerRestart(RestartCooldownFact{
		TargetService:    "apiary_managerd",
		IsLocalNodeVoter: true,
		ReadOK:           false,
	})
	if got.Verdict != Unknown {
		t.Errorf("Verdict = %v, want Unknown, not Allow, when the raft-internal read failed", got.Verdict)
	}
}

func TestEvaluateConcurrentManagerRestart_BlocksOnActiveLease(t *testing.T) {
	got := EvaluateConcurrentManagerRestart(RestartCooldownFact{
		TargetService:    "apiary_managerd",
		IsLocalNodeVoter: true,
		ReadOK:           true,
		ActiveLease:      &LeaseInfo{HolderNodeID: "node01", RequestedAtUnix: 1000},
	})
	if got.Verdict != Block {
		t.Errorf("Verdict = %v, want Block", got.Verdict)
	}
}

func TestEvaluateConcurrentManagerRestart_BlocksOnRecentRestart(t *testing.T) {
	got := EvaluateConcurrentManagerRestart(RestartCooldownFact{
		TargetService:    "apiary_managerd",
		IsLocalNodeVoter: true,
		ReadOK:           true,
		RecentRestart:    &RestartInfo{NodeID: "node01", CompletedAtUnix: 1000},
	})
	if got.Verdict != Block {
		t.Errorf("Verdict = %v, want Block", got.Verdict)
	}
}

func TestEvaluateConcurrentManagerRestart_AllowsWhenClear(t *testing.T) {
	got := EvaluateConcurrentManagerRestart(RestartCooldownFact{
		TargetService:    "apiary_managerd",
		IsLocalNodeVoter: true,
		ReadOK:           true,
	})
	if got.Verdict != Allow {
		t.Errorf("Verdict = %v, want Allow", got.Verdict)
	}
}
