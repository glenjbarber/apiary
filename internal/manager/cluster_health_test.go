package manager

import (
	"context"
	"strings"
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

	dialed, verdict, probeErr := s.clusterNodeHealth(
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
	// A non-healthy verdict with no stated reason is not actionable,
	// so the reason the probe never happened must be reported.
	if !strings.Contains(probeErr, "peer forwarding") {
		t.Errorf("probeErr = %q, want it to state that no peer forwarding is configured", probeErr)
	}
	if !hasObservation(verdict.Observations, "peer_probe", probeErr) {
		t.Errorf("Observations = %+v, want a peer_probe observation carrying the reason", verdict.Observations)
	}
}

// hasObservation reports whether observations contains a raw observation
// with the given source and detail, i.e. whether the failure reason was
// actually surfaced to the consumer rather than only returned internally.
func hasObservation(observations []health.Observation, source, detail string) bool {
	for _, observation := range observations {
		if observation.Source == source && observation.Detail == detail {
			return true
		}
	}
	return false
}

func TestClusterNodeHealth_RemoteUnreachableVoterIsContradictory(t *testing.T) {
	// A voter that cannot be reached is ADR-0056's own named example,
	// reached here through the ClusterHealth derivation path.
	s := NewServer(nil, "node-a", nil, nil, nil, nil, nil, "", nil, nil, nil, 0, nil)
	member := &rpcpb.RaftMember{NodeId: "node-b", Address: "", Suffrage: "Voter"}

	dialed, verdict, probeErr := s.clusterNodeHealth(
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
	// This server has no peer forwarding at all, so the stated reason is
	// that one - the point is only that a reason is always given.
	if probeErr == "" {
		t.Error("probeErr = empty, want a stated reason this node was not observed")
	}
}

// TestClusterNodeHealth_LocalNodeIsDialed covers the review finding
// that the answering node reported dialed=false, contradicting
// ClusterNodeHealth.dialed's own documented contract ("established
// trivially because it answered the request itself"). A consumer
// reading the field would conclude its reachability was never
// established.
func TestClusterNodeHealth_LocalNodeIsDialed(t *testing.T) {
	s := NewServer(nil, "node-a", nil, nil, nil, nil, nil, "", nil, nil, nil, 0, nil)

	dialed, _, probeErr := s.clusterNodeHealth(
		context.Background(), "node-a", "node-a",
		anchorFor(&rpcpb.RaftMember{NodeId: "node-a", Address: "", Suffrage: "Voter"}), true,
		map[string]*rpcpb.RaftMember{"node-a": {NodeId: "node-a", Suffrage: "Voter"}},
		time.Now(),
	)
	if !dialed {
		t.Error("dialed = false for the answering node, want true - it is answering, which is the trivial establishment the field documents")
	}
	if probeErr != "" {
		t.Errorf("probeErr = %q, want empty for the answering node", probeErr)
	}
}

func TestClusterNodeHealth_MemberWithoutAddressIsNeverDialed(t *testing.T) {
	// A placement-only member has no address to dial. That must be
	// "not attempted", not "attempted and failed" - the same distinction
	// ADR-0121 makes for the simulator's HAST observations.
	s := NewServer(nil, "node-a", nil, nil, nil, nil, nil, "", nil, nil, nil, 0, nil)
	member := &rpcpb.RaftMember{NodeId: "node-b", Suffrage: "Nonvoter"}

	dialed, _, _ := s.clusterNodeHealth(
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

	_, verdict, _ := s.clusterNodeHealth(
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

// slowHostStatsPeerForwarder answers HostStats only after its context
// has expired - a peer that is slow to respond rather than absent - and
// answers Status immediately. It is the shape that made the shared
// single three-second context a bug: with one context for both probes,
// a peer that spent the whole budget on HostStats handed Status an
// already-dead context, so the heartbeat evidence was lost to a timing
// accident instead of to anything true about that peer.
type slowHostStatsPeerForwarder struct {
	PeerForwarder

	statusCalls          int
	statusCtxHadTimeleft bool
}

func (f *slowHostStatsPeerForwarder) HostStats(ctx context.Context, _ string) (*rpcpb.HostStatsResponse, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

func (f *slowHostStatsPeerForwarder) Status(ctx context.Context, _ string) (*rpcpb.StatusResponse, error) {
	f.statusCalls++
	f.statusCtxHadTimeleft = ctx.Err() == nil
	return &rpcpb.StatusResponse{
		RaftReachable: true, RaftAppliedIndex: 42, RaftLastLogIndex: 42,
	}, nil
}

// TestClusterNodeHealth_SlowHostStatsDoesNotStarveTheStatusProbe is the
// regression guard for the shared-context review finding. The peer is
// slow, not absent: HostStats times out, and Status must still be
// attempted on a fresh budget rather than inheriting the dead one.
func TestClusterNodeHealth_SlowHostStatsDoesNotStarveTheStatusProbe(t *testing.T) {
	peers := &slowHostStatsPeerForwarder{}
	s := NewServer(nil, "node-a", nil, nil, nil, nil, peers, "", nil, nil, nil, 0, nil)
	member := &rpcpb.RaftMember{NodeId: "node-b", Address: "10.0.0.2:17700", Suffrage: "Voter"}

	dialed, verdict, probeErr := s.clusterNodeHealth(
		context.Background(), "node-b", "node-a",
		anchorFor(member), true,
		map[string]*rpcpb.RaftMember{"node-b": member},
		time.Now(),
	)

	if !dialed {
		t.Error("dialed = false, want true - an address was dialed")
	}
	if peers.statusCalls != 1 {
		t.Errorf("Status probe calls = %d, want 1 - the peer was slow, not absent, so it must still be asked", peers.statusCalls)
	}
	if !peers.statusCtxHadTimeleft {
		t.Error("Status was called with an already-expired context; each probe needs its own budget")
	}
	if !hasObservation(verdict.Observations, "manager_heartbeat", "") && verdict.Status == health.StatusHealthy {
		t.Errorf("Status = %q with heartbeat evidence missing", verdict.Status)
	}
	// The slow HostStats must still be reported, with its reason.
	if probeErr == "" {
		t.Error("probeErr = empty, want the HostStats failure stated")
	}
}
