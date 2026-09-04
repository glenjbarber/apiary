package recovery

import "testing"

func TestClassifyQuorum_ReachableAloneSufficientIsSurvives(t *testing.T) {
	f := QuorumFact{TotalVoters: 3, RemainingVoters: 2, RemainingReachable: 2, RemainingUnknown: 0, QuorumSize: 2}
	if got := ClassifyQuorum(f, false); got != QuorumSurvives {
		t.Errorf("ClassifyQuorum() = %v, want Survives", got)
	}
}

func TestClassifyQuorum_ReachableShortButReachablePlusUnknownSufficientIsUnknown(t *testing.T) {
	// 1 confirmed reachable, 1 unknown, quorum needs 2 - LOST only if even
	// crediting the unknown voter still falls short, which isn't the case
	// here (1+1=2 meets quorumSize).
	f := QuorumFact{TotalVoters: 3, RemainingVoters: 2, RemainingReachable: 1, RemainingUnknown: 1, QuorumSize: 2}
	if got := ClassifyQuorum(f, false); got != QuorumUnknown {
		t.Errorf("ClassifyQuorum() = %v, want Unknown", got)
	}
}

func TestClassifyQuorum_EvenCreditingUnknownsInsufficientIsLost(t *testing.T) {
	// 1 confirmed reachable, 1 unknown, quorum needs 3 - even crediting the
	// unknown voter (1+1=2) still falls short of 3.
	f := QuorumFact{TotalVoters: 5, RemainingVoters: 4, RemainingReachable: 1, RemainingUnknown: 1, QuorumSize: 3}
	if got := ClassifyQuorum(f, false); got != QuorumLost {
		t.Errorf("ClassifyQuorum() = %v, want Lost", got)
	}
}

func TestClassifyQuorum_CurrentLeaderDowngradesSurvivesToUnknown(t *testing.T) {
	f := QuorumFact{TotalVoters: 3, RemainingVoters: 2, RemainingReachable: 2, RemainingUnknown: 0, QuorumSize: 2}
	if got := ClassifyQuorum(f, true); got != QuorumUnknown {
		t.Errorf("ClassifyQuorum() = %v, want Unknown (leader-loss downgrade)", got)
	}
}

func TestClassifyQuorum_CurrentLeaderDowngradesUnknownToUnknown(t *testing.T) {
	f := QuorumFact{TotalVoters: 3, RemainingVoters: 2, RemainingReachable: 1, RemainingUnknown: 1, QuorumSize: 2}
	if got := ClassifyQuorum(f, true); got != QuorumUnknown {
		t.Errorf("ClassifyQuorum() = %v, want Unknown", got)
	}
}

func TestClassifyQuorum_CurrentLeaderNeverChangesLost(t *testing.T) {
	f := QuorumFact{TotalVoters: 5, RemainingVoters: 4, RemainingReachable: 1, RemainingUnknown: 1, QuorumSize: 3}
	if got := ClassifyQuorum(f, true); got != QuorumLost {
		t.Errorf("ClassifyQuorum() = %v, want Lost - a pure count-based Lost verdict must never be downgraded", got)
	}
}

func TestValidQuorumFact_RejectsZeroQuorumSize(t *testing.T) {
	// The real, concrete trigger this guards: internal/raft.Node.Status
	// silently leaving Servers nil looks exactly like "0 total voters."
	f := QuorumFact{TotalVoters: 0, RemainingVoters: 0, RemainingReachable: 0, RemainingUnknown: 0, QuorumSize: 0}
	if ValidQuorumFact(f) {
		t.Error("ValidQuorumFact() = true, want false for QuorumSize == 0")
	}
}

func TestValidQuorumFact_RejectsWrongQuorumSize(t *testing.T) {
	f := QuorumFact{TotalVoters: 5, RemainingVoters: 4, RemainingReachable: 4, RemainingUnknown: 0, QuorumSize: 2}
	if ValidQuorumFact(f) {
		t.Error("ValidQuorumFact() = true, want false - QuorumSize should be TotalVoters/2+1 = 3, not 2")
	}
}

func TestValidQuorumFact_RejectsReachablePlusUnknownExceedingRemaining(t *testing.T) {
	f := QuorumFact{TotalVoters: 3, RemainingVoters: 2, RemainingReachable: 2, RemainingUnknown: 1, QuorumSize: 2}
	if ValidQuorumFact(f) {
		t.Error("ValidQuorumFact() = true, want false - reachable+unknown (3) exceeds RemainingVoters (2)")
	}
}

func TestValidQuorumFact_RejectsRemainingExceedingTotal(t *testing.T) {
	f := QuorumFact{TotalVoters: 3, RemainingVoters: 4, RemainingReachable: 4, RemainingUnknown: 0, QuorumSize: 2}
	if ValidQuorumFact(f) {
		t.Error("ValidQuorumFact() = true, want false - RemainingVoters (4) exceeds TotalVoters (3)")
	}
}

func TestValidQuorumFact_RejectsRemovingMoreThanOneVoter(t *testing.T) {
	// Removing exactly one target can only ever change the voter count by
	// 0 (not a voter) or 1 (was a voter) - a difference of 2 means the
	// data is internally inconsistent.
	f := QuorumFact{TotalVoters: 5, RemainingVoters: 3, RemainingReachable: 3, RemainingUnknown: 0, QuorumSize: 3}
	if ValidQuorumFact(f) {
		t.Error("ValidQuorumFact() = true, want false - TotalVoters-RemainingVoters is 2, not 0 or 1")
	}
}

func TestValidQuorumFact_AcceptsInternallyConsistentData(t *testing.T) {
	cases := []QuorumFact{
		{TotalVoters: 3, RemainingVoters: 2, RemainingReachable: 2, RemainingUnknown: 0, QuorumSize: 2},
		{TotalVoters: 3, RemainingVoters: 3, RemainingReachable: 1, RemainingUnknown: 1, QuorumSize: 2},
		{TotalVoters: 1, RemainingVoters: 0, RemainingReachable: 0, RemainingUnknown: 0, QuorumSize: 1},
	}
	for i, f := range cases {
		if !ValidQuorumFact(f) {
			t.Errorf("case %d: ValidQuorumFact(%+v) = false, want true", i, f)
		}
	}
}
