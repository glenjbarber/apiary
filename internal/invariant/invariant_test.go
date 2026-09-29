package invariant

import (
	"strings"
	"testing"

	"github.com/glenjbarber/apiary/internal/recovery"
)

// noLeader is a node ID never present in these tests' voter lists, so
// the leader-loss downgrade in recovery.ClassifyQuorumFromVantage
// never fires - isolating each test's own condition from that separate
// behavior (which TestEvaluateQuorumTolerance_LeaderVoterGetsDowngrade
// covers on its own).
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
	got := EvaluateQuorumTolerance(voters, noLeader, recovery.VantageFromLeader)
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
	got := EvaluateQuorumTolerance(voters, noLeader, recovery.VantageFromLeader)
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
	got := EvaluateQuorumTolerance(voters, noLeader, recovery.VantageFromLeader)
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
	got := EvaluateQuorumTolerance(voters, "a", recovery.VantageFromLeader)
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

func TestEvaluateHASTDualPrimary_UnknownWithoutObservations(t *testing.T) {
	// No live role observation for either end: the answer is Unknown,
	// because silence is never folded into "not a primary".
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

func TestEvaluateHASTDualPrimary_FalseWhenBothEndsReportPrimary(t *testing.T) {
	evals := EvaluateHASTDualPrimary(nil,
		HASTPrimarySpec{
			ID: "vm-1", Name: "web-1", Kind: "vm", OwnerNodeID: "node-a", ReplicaNodeID: "node-b",
			Owner:   HASTObservation{NodeID: "node-a", Attempted: true, Observed: true, Role: "primary", Status: "complete"},
			Replica: HASTObservation{NodeID: "node-b", Attempted: true, Observed: true, Role: "primary", Status: "complete"},
		})
	if len(evals) != 1 || evals[0].Result != ResultFalse {
		t.Fatalf("evals = %+v, want one False evaluation for a dual primary", evals)
	}
}

func TestEvaluateHASTDualPrimary_TrueWhenExactlyOneEndIsPrimary(t *testing.T) {
	evals := EvaluateHASTDualPrimary(nil,
		HASTPrimarySpec{
			ID: "vm-1", Name: "web-1", Kind: "vm", OwnerNodeID: "node-a", ReplicaNodeID: "node-b",
			Owner:   HASTObservation{NodeID: "node-a", Attempted: true, Observed: true, Role: "primary", Status: "complete"},
			Replica: HASTObservation{NodeID: "node-b", Attempted: true, Observed: true, Role: "secondary", Status: "complete"},
		})
	if len(evals) != 1 || evals[0].Result != ResultTrue {
		t.Fatalf("evals = %+v, want one True evaluation when exactly one end is the writable primary", evals)
	}
}

func TestEvaluateHASTDualPrimary_UnknownWhenAnEndIsSilent(t *testing.T) {
	// A missing observation is never treated as a non-primary role, so
	// it cannot produce the True that "exactly one primary" requires.
	evals := EvaluateHASTDualPrimary(nil,
		HASTPrimarySpec{
			ID: "vm-1", Name: "web-1", Kind: "vm", OwnerNodeID: "node-a", ReplicaNodeID: "node-b",
			Owner: HASTObservation{NodeID: "node-a", Attempted: true, Observed: true, Role: "primary", Status: "complete"},
		})
	if len(evals) != 1 || evals[0].Result != ResultUnknown {
		t.Fatalf("evals = %+v, want Unknown when the replica never reported a role", evals)
	}
}

func TestEvaluateHASTDualPrimary_UnknownWhenOneNodeIsBothEnds(t *testing.T) {
	evals := EvaluateHASTDualPrimary(nil,
		HASTPrimarySpec{
			ID: "vm-1", Name: "web-1", Kind: "vm", OwnerNodeID: "node-a", ReplicaNodeID: "node-a",
			Owner: HASTObservation{NodeID: "node-a", Attempted: true, Observed: true, Role: "primary", Status: "complete"},
		})
	if len(evals) != 1 || evals[0].Result != ResultUnknown {
		t.Fatalf("evals = %+v, want Unknown when one node is configured as both ends", evals)
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

func TestEvaluateCellRecoverability_CapableDestinationAloneIsNeverTrue(t *testing.T) {
	// A capable destination is one half of the conjunction. With no
	// live HAST observation for the replica - the zero value, which is
	// silence rather than a passed check - the whole is Unknown.
	facts := []ResourceFact{
		{ID: "vm-1", Name: "web-1", Kind: "vm", ReplicaNodeID: "node-b", DestinationCapable: ResultTrue, DestinationCapableDetail: "node-b: bhyve configured"},
	}
	evals := EvaluateCellRecoverability(facts)
	if len(evals) != 1 || evals[0].Result != ResultUnknown {
		t.Fatalf("evals = %+v, want Unknown - a capable destination alone never satisfies the conjunction", evals)
	}
}

func TestEvaluateCellRecoverability_TrueWhenBothHalvesConfirmed(t *testing.T) {
	facts := []ResourceFact{{
		ID: "vm-1", Name: "web-1", Kind: "vm", ReplicaNodeID: "node-b",
		DestinationCapable: ResultTrue, DestinationCapableDetail: "node-b: bhyve configured",
		ReplicaSync: HASTObservation{
			NodeID: "node-b", Attempted: true, Observed: true,
			Role: "secondary", Status: "complete", Replication: "load-balanced",
		},
	}}
	evals := EvaluateCellRecoverability(facts)
	if len(evals) != 1 || evals[0].Result != ResultTrue {
		t.Fatalf("evals = %+v, want True when the replica is in sync and the destination is capable", evals)
	}
}

func TestEvaluateCellRecoverability_UnknownWhenReplicaConfirmedOutOfSync(t *testing.T) {
	// A confirmed "init" role is a positive statement that the replica
	// is not usable as-is, but it is not a statement that the Cell is
	// unrecoverable - so the conjunction is unconfirmed, not False.
	facts := []ResourceFact{{
		ID: "vm-1", Name: "web-1", Kind: "vm", ReplicaNodeID: "node-b",
		DestinationCapable: ResultTrue, DestinationCapableDetail: "node-b: bhyve configured",
		ReplicaSync: HASTObservation{
			NodeID: "node-b", Attempted: true, Observed: true, Role: "init",
		},
	}}
	evals := EvaluateCellRecoverability(facts)
	if len(evals) != 1 || evals[0].Result != ResultUnknown {
		t.Fatalf("evals = %+v, want Unknown when the replica is confirmed not usable as-is", evals)
	}
}

func TestEvaluateCellRecoverability_UnknownForUnrecognisedHASTRole(t *testing.T) {
	// A role this build does not recognize is Apiary not understanding
	// hastd, which is silence rather than a confirmed outage.
	facts := []ResourceFact{{
		ID: "vm-1", Name: "web-1", Kind: "vm", ReplicaNodeID: "node-b",
		DestinationCapable: ResultTrue, DestinationCapableDetail: "node-b: bhyve configured",
		ReplicaSync: HASTObservation{
			NodeID: "node-b", Attempted: true, Observed: true, Role: "promoted", Status: "complete",
		},
	}}
	evals := EvaluateCellRecoverability(facts)
	if len(evals) != 1 || evals[0].Result != ResultUnknown {
		t.Fatalf("evals = %+v, want Unknown for a role this build does not recognize", evals)
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

func impactFor(impacts []VoterQuorumImpact, nodeID string) VoterQuorumImpact {
	for _, i := range impacts {
		if i.NodeID == nodeID {
			return i
		}
	}
	return VoterQuorumImpact{}
}

func TestClassifyVoterQuorumImpacts_SurvivesForEveryVoterWhenAllReachable(t *testing.T) {
	voters := []VoterReachability{
		{NodeID: "a", Reachability: ReachabilityReachable},
		{NodeID: "b", Reachability: ReachabilityReachable},
		{NodeID: "c", Reachability: ReachabilityReachable},
	}
	impacts := ClassifyVoterQuorumImpacts(voters, noLeader, recovery.VantageFromLeader)
	if len(impacts) != 3 {
		t.Fatalf("len(impacts) = %d, want 3", len(impacts))
	}
	for _, impact := range impacts {
		if !impact.Valid || impact.Verdict != recovery.QuorumSurvives {
			t.Errorf("impact for %s = %+v, want Valid=true Verdict=Survives", impact.NodeID, impact)
		}
	}
}

func TestClassifyVoterQuorumImpacts_LostOnlyForTheVotersWhoseLossBreaksQuorum(t *testing.T) {
	// 3 voters, "b" already confirmed unreachable: only {a, c} are
	// actually reachable right now. Losing the ALREADY-unreachable "b"
	// changes nothing (2 of the remaining 2 stay reachable -> Survives),
	// but losing either "a" or "c" drops the reachable set to 1 of 2
	// remaining voters, short of the quorum size of 2 -> Lost for both.
	// This proves the verdict is computed per-voter from who ELSE
	// remains reachable, not a single shared aggregate.
	voters := []VoterReachability{
		{NodeID: "a", Reachability: ReachabilityReachable},
		{NodeID: "b", Reachability: ReachabilityUnreachable},
		{NodeID: "c", Reachability: ReachabilityReachable},
	}
	impacts := ClassifyVoterQuorumImpacts(voters, noLeader, recovery.VantageFromLeader)
	if got := impactFor(impacts, "a"); !got.Valid || got.Verdict != recovery.QuorumLost {
		t.Errorf("impact for a = %+v, want Valid=true Verdict=Lost", got)
	}
	if got := impactFor(impacts, "b"); !got.Valid || got.Verdict != recovery.QuorumSurvives {
		t.Errorf("impact for b = %+v, want Valid=true Verdict=Survives (b was already unreachable, removing it changes nothing)", got)
	}
	if got := impactFor(impacts, "c"); !got.Valid || got.Verdict != recovery.QuorumLost {
		t.Errorf("impact for c = %+v, want Valid=true Verdict=Lost", got)
	}
}

func TestClassifyVoterQuorumImpacts_LeaderLossDowngradesOnlyTheLeadersOwnImpact(t *testing.T) {
	voters := []VoterReachability{
		{NodeID: "a", Reachability: ReachabilityReachable},
		{NodeID: "b", Reachability: ReachabilityReachable},
		{NodeID: "c", Reachability: ReachabilityReachable},
	}
	impacts := ClassifyVoterQuorumImpacts(voters, "a", recovery.VantageFromLeader)
	if got := impactFor(impacts, "a"); !got.Valid || got.Verdict != recovery.QuorumUnknown {
		t.Errorf("impact for leader a = %+v, want Valid=true Verdict=Unknown (leader-loss downgrade)", got)
	}
	if got := impactFor(impacts, "b"); !got.Valid || got.Verdict != recovery.QuorumSurvives {
		t.Errorf("impact for non-leader b = %+v, must not be downgraded, want Survives", got)
	}
	if got := impactFor(impacts, "c"); !got.Valid || got.Verdict != recovery.QuorumSurvives {
		t.Errorf("impact for non-leader c = %+v, must not be downgraded, want Survives", got)
	}
}

func TestClassifyVoterQuorumImpacts_EvaluateQuorumToleranceStaysConsistentWithPerVoterImpacts(t *testing.T) {
	// Regression guard for the EvaluateQuorumTolerance refactor: the
	// aggregated worst-of Result must always match what a caller would
	// derive by scanning ClassifyVoterQuorumImpacts itself - these must
	// never diverge, since EvaluateQuorumTolerance is now implemented
	// on top of this function.
	voters := []VoterReachability{
		{NodeID: "a", Reachability: ReachabilityReachable},
		{NodeID: "b", Reachability: ReachabilityUnreachable},
		{NodeID: "c", Reachability: ReachabilityReachable},
	}
	eval := EvaluateQuorumTolerance(voters, noLeader, recovery.VantageFromLeader)
	impacts := ClassifyVoterQuorumImpacts(voters, noLeader, recovery.VantageFromLeader)
	anyLost := false
	for _, impact := range impacts {
		if impact.Valid && impact.Verdict == recovery.QuorumLost {
			anyLost = true
		}
	}
	if anyLost && eval.Result != ResultFalse {
		t.Fatalf("ClassifyVoterQuorumImpacts found a Lost voter but EvaluateQuorumTolerance.Result = %v, want False", eval.Result)
	}
}

func TestEvaluateQuorumTolerance_NonLeaderVantageIsNotLeaderDowngraded(t *testing.T) {
	// The same 3-voter all-reachable set and the same current leader as
	// TestEvaluateQuorumTolerance_LeaderVoterGetsDowngrade above, but
	// the reachability counts were gathered by this caller rather than
	// by the leader. The leader-loss argument - that leader-to-voter
	// reachability says nothing about voter-to-voter reachability - does
	// not apply to data this caller produced itself, so the downgrade
	// must not fire and the verdict must be True.
	voters := []VoterReachability{
		{NodeID: "a", Reachability: ReachabilityReachable},
		{NodeID: "b", Reachability: ReachabilityReachable},
		{NodeID: "c", Reachability: ReachabilityReachable},
	}
	got := EvaluateQuorumTolerance(voters, "a", recovery.VantageFromNonLeader)
	if got.Result != ResultTrue {
		t.Fatalf("Result = %v, want True: a non-leader vantage must not be downgraded for a reason that only describes the leader's own", got.Result)
	}
}

func TestEvaluateQuorumTolerance_ZeroVantageFailsClosed(t *testing.T) {
	// A caller that did not say where its data came from gets the
	// conservative leader-vantage reading, never a stronger verdict than
	// it earned. This is the property that makes the new parameter safe
	// to add without auditing every existing caller at once.
	voters := []VoterReachability{
		{NodeID: "a", Reachability: ReachabilityReachable},
		{NodeID: "b", Reachability: ReachabilityReachable},
		{NodeID: "c", Reachability: ReachabilityReachable},
	}
	got := EvaluateQuorumTolerance(voters, "a", recovery.QuorumVantage(""))
	if got.Result != ResultUnknown {
		t.Fatalf("Result = %v, want Unknown for an unnamed vantage", got.Result)
	}
}

func TestEvaluateQuorumTolerance_NonLeaderVantageStillReportsLost(t *testing.T) {
	// The downgrade was never the only thing the leader-vantage path
	// did. A count-based Lost is a voter-count fact independent of
	// reachability, so it must be Lost from either vantage - a
	// non-leader vantage must not soften it.
	voters := []VoterReachability{
		{NodeID: "a", Reachability: ReachabilityUnreachable},
		{NodeID: "b", Reachability: ReachabilityUnreachable},
		{NodeID: "c", Reachability: ReachabilityReachable},
	}
	for _, vantage := range []recovery.QuorumVantage{recovery.VantageFromLeader, recovery.VantageFromNonLeader} {
		got := EvaluateQuorumTolerance(voters, "c", vantage)
		if got.Result != ResultFalse {
			t.Errorf("vantage %q: Result = %v, want False", vantage, got.Result)
		}
	}
}

func TestClassifyVoterQuorumImpacts_NonLeaderVantageKeepsLeaderSurvives(t *testing.T) {
	voters := []VoterReachability{
		{NodeID: "a", Reachability: ReachabilityReachable},
		{NodeID: "b", Reachability: ReachabilityReachable},
		{NodeID: "c", Reachability: ReachabilityReachable},
	}
	impacts := ClassifyVoterQuorumImpacts(voters, "a", recovery.VantageFromNonLeader)
	if got := impactFor(impacts, "a"); !got.Valid || got.Verdict != recovery.QuorumSurvives {
		t.Errorf("impact for leader a = %+v, want Survives from a non-leader vantage", got)
	}
}
