package invariant

import (
	"strings"
	"testing"
)

// noLeader is a node ID never present in these tests' voter lists, so
// the leader-loss downgrade in recovery.ClassifyQuorum never fires -
// isolating each test's own condition from that separate behavior
// (which TestEvaluateQuorumTolerance_LeaderVoterGetsDowngrade covers on
// its own).
const noLeader = "not-a-voter"

func TestEvaluateQuorumTolerance_TrueWhenEveryVoterLossSurvives(t *testing.T) {
	// 3 voters, all reachable - losing any one leaves 2 of 3 remaining
	// reachable, quorum size for 3 total is 2, so every single loss
	// still meets quorum.
	voters := []VoterReachability{
		{NodeID: "a", Reachability: ReachabilityReachable},
		{NodeID: "b", Reachability: ReachabilityReachable},
		{NodeID: "c", Reachability: ReachabilityReachable},
	}
	got := EvaluateQuorumTolerance(voters, noLeader)
	if got.Result != ResultTrue {
		t.Errorf("Result = %v, want True; evidence=%+v", got.Result, got.Evidence)
	}
}

func TestEvaluateQuorumTolerance_FalseWhenAnyVoterLossIsLost(t *testing.T) {
	// 3 voters: a reachable, b unreachable (implicit - neither reachable
	// nor unknown), c reachable. Losing "a" leaves {b unreachable, c
	// reachable} -> remainingReachable=1, quorumSize=2 -> Lost.
	voters := []VoterReachability{
		{NodeID: "a", Reachability: ReachabilityReachable},
		{NodeID: "b", Reachability: ReachabilityUnreachable},
		{NodeID: "c", Reachability: ReachabilityReachable},
	}
	got := EvaluateQuorumTolerance(voters, noLeader)
	if got.Result != ResultFalse {
		t.Errorf("Result = %v, want False; evidence=%+v", got.Result, got.Evidence)
	}
}

func TestEvaluateQuorumTolerance_UnknownWhenNoneLostButSomeUnknown(t *testing.T) {
	// 3 voters, one has unknown reachability, none confirmed unreachable.
	// Losing "a" leaves {b unknown, c reachable} -> remainingReachable=1,
	// remainingUnknown=1, quorumSize=2 -> 1 < 2 but 1+1 >= 2 -> Unknown.
	voters := []VoterReachability{
		{NodeID: "a", Reachability: ReachabilityReachable},
		{NodeID: "b", Reachability: ReachabilityUnknown},
		{NodeID: "c", Reachability: ReachabilityReachable},
	}
	got := EvaluateQuorumTolerance(voters, noLeader)
	if got.Result != ResultUnknown {
		t.Errorf("Result = %v, want Unknown; evidence=%+v", got.Result, got.Evidence)
	}
}

func TestEvaluateQuorumTolerance_LeaderVoterGetsDowngrade(t *testing.T) {
	// Same reachable-only 3-voter set as the "always True" case above,
	// but "a" is the current leader - recovery.ClassifyQuorum downgrades
	// a would-be Survives to Unknown specifically for the leader's own
	// evaluation (leader-to-voter reachability doesn't prove voter-to-
	// voter connectivity). This must surface as at least one Unknown
	// evidence entry even though the OTHER two voters' losses still
	// resolve Survives - proving isCurrentLeader is recomputed per
	// voter, not hoisted out of the loop.
	voters := []VoterReachability{
		{NodeID: "a", Reachability: ReachabilityReachable},
		{NodeID: "b", Reachability: ReachabilityReachable},
		{NodeID: "c", Reachability: ReachabilityReachable},
	}
	got := EvaluateQuorumTolerance(voters, "a")
	if got.Result != ResultUnknown {
		t.Fatalf("Result = %v, want Unknown (leader-loss downgrade for voter a)", got.Result)
	}
	foundLeaderDowngrade := false
	for _, e := range got.Evidence {
		if strings.Contains(e.Detail, "Losing a has an UNKNOWN") {
			foundLeaderDowngrade = true
		}
		if strings.Contains(e.Detail, "Losing b has an UNKNOWN") || strings.Contains(e.Detail, "Losing c has an UNKNOWN") {
			t.Errorf("non-leader voter incorrectly downgraded: %q", e.Detail)
		}
	}
	if !foundLeaderDowngrade {
		t.Errorf("expected an UNKNOWN evidence entry for leader voter a, got: %+v", got.Evidence)
	}
}

func TestEvaluateHASTDualPrimary_AlwaysUnknown(t *testing.T) {
	evals := EvaluateHASTDualPrimary([]string{"vm-1", "jail-2"})
	if len(evals) != 2 {
		t.Fatalf("len(evals) = %d, want 2", len(evals))
	}
	for _, e := range evals {
		if e.Result != ResultUnknown {
			t.Errorf("Result = %v, want Unknown for %s", e.Result, e.Scope)
		}
		if e.Name != "hast-dual-primary" {
			t.Errorf("Name = %q, want hast-dual-primary", e.Name)
		}
	}
}

func TestEvaluateCellRecoverability_FalseForIncapableDestination(t *testing.T) {
	facts := []ResourceFact{
		{ID: "vm-1", Name: "web-1", Kind: "vm", ReplicaNodeID: "node-b", DestinationCapable: ResultFalse, DestinationCapableDetail: "node-b: bhyve not configured"},
	}
	evals := EvaluateCellRecoverability(facts)
	if len(evals) != 1 || evals[0].Result != ResultFalse {
		t.Fatalf("evals = %+v, want one False evaluation", evals)
	}
}

func TestEvaluateCellRecoverability_NeverTrueEvenWhenDestinationCapable(t *testing.T) {
	facts := []ResourceFact{
		{ID: "vm-1", Name: "web-1", Kind: "vm", ReplicaNodeID: "node-b", DestinationCapable: ResultTrue, DestinationCapableDetail: "node-b: bhyve configured"},
	}
	evals := EvaluateCellRecoverability(facts)
	if len(evals) != 1 || evals[0].Result != ResultUnknown {
		t.Fatalf("evals = %+v, want Unknown (never True) even for a capable destination", evals)
	}
}

func TestEvaluateCellRecoverability_JailAlwaysUnknownRegardlessOfInput(t *testing.T) {
	facts := []ResourceFact{
		{ID: "jail-1", Name: "svc-1", Kind: "jail", ReplicaNodeID: "node-b", DestinationCapable: ResultUnknown, DestinationCapableDetail: "no capability signal exists for jails"},
	}
	evals := EvaluateCellRecoverability(facts)
	if len(evals) != 1 || evals[0].Result != ResultUnknown {
		t.Fatalf("evals = %+v, want Unknown for a jail", evals)
	}
}

func TestEvaluateNetworkRoute_FalseWhenAnyNodeReportsDown(t *testing.T) {
	facts := []NetworkFact{
		{ID: "net-1", Name: "services", Observations: []BridgeObservation{
			{NodeID: "node-a", Status: "up"},
			{NodeID: "node-b", Status: "down"},
		}},
	}
	evals := EvaluateNetworkRoute(facts)
	if len(evals) != 1 || evals[0].Result != ResultFalse {
		t.Fatalf("evals = %+v, want False", evals)
	}
}

func TestEvaluateNetworkRoute_UnknownWhenNothingAttached(t *testing.T) {
	facts := []NetworkFact{{ID: "net-1", Name: "services", Observations: nil}}
	evals := EvaluateNetworkRoute(facts)
	if len(evals) != 1 || evals[0].Result != ResultUnknown {
		t.Fatalf("evals = %+v, want Unknown for nothing attached", evals)
	}
}

func TestEvaluateNetworkRoute_NeverTrueEvenWhenRouteFullyClear(t *testing.T) {
	facts := []NetworkFact{
		{ID: "net-1", Name: "services", Observations: []BridgeObservation{
			{NodeID: "node-a", Status: "up"},
			{NodeID: "node-b", Status: "up"},
		}},
	}
	evals := EvaluateNetworkRoute(facts)
	if len(evals) != 1 || evals[0].Result != ResultUnknown {
		t.Fatalf("evals = %+v, want Unknown (DNS never resolves True) even with a fully clear route", evals)
	}
}

func TestEvaluateNetworkRoute_FetchErrorIsUnknownNotDown(t *testing.T) {
	facts := []NetworkFact{
		{ID: "net-1", Name: "services", Observations: []BridgeObservation{
			{NodeID: "node-a", Err: "dial tcp: connection refused"},
		}},
	}
	evals := EvaluateNetworkRoute(facts)
	if len(evals) != 1 || evals[0].Result != ResultUnknown {
		t.Fatalf("evals = %+v, want Unknown for a fetch error, never False/down", evals)
	}
}

func TestEvaluateOwnershipGatedDeletion_AlwaysTrueWithZeroObservedAt(t *testing.T) {
	eval := EvaluateOwnershipGatedDeletion()
	if eval.Result != ResultTrue {
		t.Fatalf("Result = %v, want True", eval.Result)
	}
	if len(eval.Evidence) != 1 || !eval.Evidence[0].ObservedAt.IsZero() {
		t.Fatalf("Evidence = %+v, want exactly one entry with a zero ObservedAt (structural, not runtime-observed)", eval.Evidence)
	}
}
