package manager

import (
	"context"
	"testing"
	"time"

	rpcpb "github.com/glenjbarber/apiary/api/rpc"
	"github.com/glenjbarber/apiary/internal/health"
)

// These cover the decision core of ClusterHealth (ADR-0122) directly,
// calling clusterNodeHealth with hand-built inputs rather than a live
// raftd. The expensive end-to-end path is the real raft harness's job;
// what matters here is that a remote Comb is never reported healthy
// merely because nothing dialed it.

func anchorFor(members ...*rpcpb.RaftMember) *rpcpb.StatusResponse {
	return &rpcpb.StatusResponse{RaftReachable: true, Members: members}
}

func TestClusterNodeHealth_RemoteWithNoPeerForwardingIsNotHealthy(t *testing.T) {
	s := NewServer(nil, "node-a", nil, nil, nil, nil, nil, "", nil, nil, nil, 0, nil)
	now := time.Now()

	dialed, verdict := s.clusterNodeHealth(
		context.Background(), "node-b", "node-a",
		anchorFor(&rpcpb.RaftMember{NodeId: "node-b", Address: "10.0.0.2:17700", Suffrage: "Voter"}),
		true,
		map[string]*rpcpb.RaftMember{"node-b": {NodeId: "node-b", Address: "10.0.0.2:17700", Suffrage: "Voter"}},
		now,
	)

	if dialed {
		t.Error("dialed = true with no peer forwarding configured, want false")
	}
	if verdict.Status == health.StatusHealthy {
		t.Errorf("Status = %q, want not healthy - nothing about node-b was ever observed", verdict.Status)
	}
}

func TestClusterNodeHealth_RemoteUnreachableVoterIsContradictory(t *testing.T) {
	// A voter that cannot be reached is ADR-0056's own named example,
	// reached here through the ClusterHealth derivation path.
	s := NewServer(nil, "node-a", nil, nil, nil, nil, nil, "", nil, nil, nil, 0, nil)
	member := &rpcpb.RaftMember{NodeId: "node-b", Address: "", Suffrage: "Voter"}

	dialed, verdict := s.clusterNodeHealth(
		context.Background(), "node-b", "node-a",
		anchorFor(member), true,
		map[string]*rpcpb.RaftMember{"node-b": member},
		time.Now(),
	)
	if dialed {
		t.Error("dialed = true for a member with no address, want false")
	}
	if verdict.Status == health.StatusHealthy {
		t.Errorf("Status = %q, want never healthy for a voter that could not be reached", verdict.Status)
	}
}

func TestClusterNodeHealth_MemberWithoutAddressIsNeverDialed(t *testing.T) {
	// A placement-only member has no address to dial. That must be
	// "not attempted", not "attempted and failed" - the same distinction
	// ADR-0121 makes for the simulator's HAST observations.
	s := NewServer(nil, "node-a", nil, nil, nil, nil, nil, "", nil, nil, nil, 0, nil)
	member := &rpcpb.RaftMember{NodeId: "node-b", Suffrage: "Nonvoter"}

	dialed, _ := s.clusterNodeHealth(
		context.Background(), "node-b", "node-a",
		anchorFor(member), true,
		map[string]*rpcpb.RaftMember{"node-b": member},
		time.Now(),
	)
	if dialed {
		t.Error("dialed = true for a member with no address, want false")
	}
}

func TestClusterNodeHealth_UnknownSuffrageNeverReportsHealthy(t *testing.T) {
	s := NewServer(nil, "node-a", nil, nil, nil, nil, nil, "", nil, nil, nil, 0, nil)
	member := &rpcpb.RaftMember{NodeId: "node-a", Suffrage: "Unknown"}

	_, verdict := s.clusterNodeHealth(
		context.Background(), "node-a", "node-a",
		anchorFor(member), true,
		map[string]*rpcpb.RaftMember{"node-a": member},
		time.Now(),
	)
	if verdict.Status == health.StatusHealthy {
		t.Errorf("Status = %q, want not healthy when raft's own suffrage is Unknown", verdict.Status)
	}
}

func TestToRPCHealthObservationsPreservesEveryField(t *testing.T) {
	at := time.Unix(1700000000, 0)
	out := toRPCHealthObservations([]health.Observation{{
		Source: "raft_membership", ObservedAt: at,
		FreshnessLimit: 90 * time.Second,
		Value:          "voter", Detail: "raft member, suffrage=voter",
	}})
	if len(out) != 1 {
		t.Fatalf("len = %d, want 1", len(out))
	}
	if out[0].GetSource() != "raft_membership" || out[0].GetObservedUnix() != at.Unix() ||
		out[0].GetFreshnessLimitSeconds() != 90 || out[0].GetValue() != "voter" ||
		out[0].GetDetail() != "raft member, suffrage=voter" {
		t.Errorf("observation = %+v, want every field carried verbatim", out[0])
	}
}

func TestToRPCHealthObservationsZeroTimeStaysZero(t *testing.T) {
	// A check that never happened must read as "no observation" (0),
	// not as "observed at the epoch" - the same distinction
	// HealthObservation's own doc comment draws.
	out := toRPCHealthObservations([]health.Observation{{Source: "raft_membership", Value: "unobserved"}})
	if out[0].GetObservedUnix() != 0 {
		t.Errorf("ObservedUnix = %d, want 0 for a zero ObservedAt", out[0].GetObservedUnix())
	}
}
