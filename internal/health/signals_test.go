package health

import (
	"testing"
	"time"
)

// These cover SignalsFrom, the shared derivation ADR-0122 introduced so
// the web UI and managerd's ClusterHealth RPC cannot build NodeSignals
// by two different rule sets. The failure this guards against is
// specific: a remote node quietly inheriting the local node's trivially
// -true reachability and being reported as reachable when nothing ever
// dialed it.

func TestSignalsFrom_LocalIsReachableWithoutDialing(t *testing.T) {
	s := SignalsFrom(Inputs{NodeID: "node-a", IsLocal: true})
	if s.PeerReachability != ReachabilityReachable {
		t.Errorf("PeerReachability = %q, want reachable", s.PeerReachability)
	}
}

func TestSignalsFrom_NoPeerForwardingIsUnknown(t *testing.T) {
	// A remote node with no peer forwarding is NOT reachable, and
	// certainly is not reachable because the local one is.
	s := SignalsFrom(Inputs{NodeID: "node-b", PeerForwardingConfigured: false})
	if s.PeerReachability != ReachabilityUnknown {
		t.Errorf("PeerReachability = %q, want unknown - nothing was dialed, so nothing was learned", s.PeerReachability)
	}
}

func TestSignalsFrom_NotDialedIsUnknownEvenIfSucceededFlagSet(t *testing.T) {
	// Defensive: a caller that set PeerDialSucceeded without ever
	// attempting a dial must still not be believed.
	s := SignalsFrom(Inputs{
		NodeID:                   "node-b",
		PeerForwardingConfigured: true,
		PeerDialSucceeded:        true,
	})
	if s.PeerReachability != ReachabilityUnknown {
		t.Errorf("PeerReachability = %q, want unknown - no dial was attempted", s.PeerReachability)
	}
}

func TestSignalsFrom_DialFailedIsUnreachable(t *testing.T) {
	s := SignalsFrom(Inputs{
		NodeID:                   "node-b",
		PeerForwardingConfigured: true,
		PeerDialAttempted:        true,
	})
	if s.PeerReachability != ReachabilityUnreachable {
		t.Errorf("PeerReachability = %q, want unreachable", s.PeerReachability)
	}
}

func TestSignalsFrom_DialSucceededIsReachable(t *testing.T) {
	s := SignalsFrom(Inputs{
		NodeID:                   "node-b",
		PeerForwardingConfigured: true,
		PeerDialAttempted:        true,
		PeerDialSucceeded:        true,
	})
	if s.PeerReachability != ReachabilityReachable {
		t.Errorf("PeerReachability = %q, want reachable", s.PeerReachability)
	}
}

func TestSignalsFrom_MembershipRequiresObservedRead(t *testing.T) {
	// A member row alone is never enough: if the shared anchor read did
	// not confirm membership, IsRaftMember must stay false so the
	// verdict cannot be built on an unconfirmed claim.
	s := SignalsFrom(Inputs{
		NodeID:             "node-b",
		MemberFound:        true,
		MembershipObserved: false,
		Suffrage:           SuffrageVoter,
	})
	if s.IsRaftMember {
		t.Error("IsRaftMember = true, want false when membership was never observed")
	}
}

func TestSignalsFrom_MembershipObservedAndFound(t *testing.T) {
	s := SignalsFrom(Inputs{
		NodeID:             "node-b",
		MemberFound:        true,
		MembershipObserved: true,
		Suffrage:           SuffrageVoter,
	})
	if !s.IsRaftMember || s.Suffrage != SuffrageVoter {
		t.Errorf("got IsRaftMember=%v Suffrage=%q, want true/voter", s.IsRaftMember, s.Suffrage)
	}
}

func TestSignalsFrom_NotAMemberIsNotAMember(t *testing.T) {
	s := SignalsFrom(Inputs{NodeID: "node-b", MembershipObserved: true, MemberFound: false})
	if s.IsRaftMember {
		t.Error("IsRaftMember = true, want false for a genuinely absent member")
	}
}

func TestSignalsFrom_ReconcilerConfiguredDerivedFromInterval(t *testing.T) {
	if s := SignalsFrom(Inputs{NodeID: "n", ReconcileIntervalSeconds: 0}); s.ReconcilerConfigured {
		t.Error("ReconcilerConfigured = true at interval 0, want false - 0 is the only reliable 'no Reconciler' signal")
	}
	if s := SignalsFrom(Inputs{NodeID: "n", ReconcileIntervalSeconds: 30}); !s.ReconcilerConfigured {
		t.Error("ReconcilerConfigured = false at interval 30, want true")
	}
}

func TestSignalsFrom_UnknownSuffrageSurvivesDerivation(t *testing.T) {
	// ADR-0056 review finding 2: raft's own "Unknown" must reach
	// ComputeNodeHealth as Unknown, never be normalized into a healthy
	// answer. The derivation must not silently drop or upgrade it.
	now := time.Now()
	s := SignalsFrom(Inputs{
		NodeID: "node-b", IsLocal: true, MembershipObserved: true,
		MemberFound: true, Suffrage: SuffrageUnknown, MembershipObservedAt: now,
		HeartbeatObserved: true, HeartbeatOK: true,
	})
	h := ComputeNodeHealth(s, now)
	if h.Status == StatusHealthy {
		t.Errorf("Status = %q, want not healthy when raft's own suffrage is unknown", h.Status)
	}
}

func TestSignalsFrom_VoterUnreachableIsStillContradictory(t *testing.T) {
	// ADR-0056's own named example must survive the refactor into
	// SignalsFrom unchanged.
	now := time.Now()
	s := SignalsFrom(Inputs{
		NodeID: "node-b", MembershipObserved: true, MemberFound: true,
		Suffrage: SuffrageVoter, MembershipObservedAt: now,
		PeerForwardingConfigured: true, PeerDialAttempted: true,
	})
	h := ComputeNodeHealth(s, now)
	if h.Status != StatusContradictory {
		t.Errorf("Status = %q, want contradictory - membership arithmetic is not availability proof", h.Status)
	}
}
