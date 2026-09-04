package frontend

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	rpcpb "github.com/glenjbarber/apiary/api/rpc"
)

func TestHandleInvariantsPage_CurrentVMsFetchFailureRendersUnknownNotEmpty(t *testing.T) {
	client := &fakeClient{
		statusResp: &rpcpb.StatusResponse{ManagerNodeId: "node-a", RaftReachable: true},
		listErr:    errors.New("raftd unreachable"),
	}
	s := newTestServer(t, client)

	req := httptest.NewRequest(http.MethodGet, "/invariants", nil)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	body := rec.Body.String()
	if !strings.Contains(body, "cell-recoverability") || !strings.Contains(body, "raftd unreachable") {
		t.Fatalf("expected a cell-recoverability finding citing the fetch error, got: %s", body)
	}
	if !strings.Contains(body, `class="badge unknown"`) {
		t.Errorf("expected an unknown-badged finding, not silently empty, got: %s", body)
	}
}

func TestHandleInvariantsPage_HostStatsFetchFailureToReplicaTargetIsUnknown(t *testing.T) {
	client := &fakeClient{
		statusResp: &rpcpb.StatusResponse{ManagerNodeId: "node-a", RaftReachable: true},
		listResp: &rpcpb.ListVMsResponse{Vms: []*rpcpb.VMDefinition{
			{Id: "vm-1", Name: "web-1", NodeId: "node-a", ReplicaNodeId: "node-b"},
		}},
	}
	peers := &fakePeerHostStatsClient{err: errors.New("dial tcp: connection refused")}
	s, err := NewServer(client, nil, nil, peers, ".test", "17700", nil)
	if err != nil {
		t.Fatalf("NewServer() error: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/invariants", nil)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	body := rec.Body.String()
	if !strings.Contains(body, "cell-recoverability") || !strings.Contains(body, "web-1") {
		t.Fatalf("expected a cell-recoverability finding for web-1, got: %s", body)
	}
	// Must never resolve True (destination fetch failed) or silently drop
	// the resource - the finding must appear as unknown.
	if strings.Contains(body, `cell-recoverability true`) {
		t.Errorf("must never resolve true when the destination HostStats fetch failed, got: %s", body)
	}
}

func TestHandleInvariantsPage_QuorumOnlyQueriesActualVoters(t *testing.T) {
	client := &fakeClient{
		statusResp: &rpcpb.StatusResponse{
			ManagerNodeId: "node-a", RaftReachable: true, RaftLeaderId: "node-a",
			Members: []*rpcpb.RaftMember{
				{NodeId: "node-a", Suffrage: "Voter"},
				{NodeId: "node-b", Suffrage: "Voter"},
				{NodeId: "node-c", Suffrage: "Nonvoter"},
			},
		},
	}
	peers := &fakePeerHostStatsClient{resp: &rpcpb.HostStatsResponse{NodeId: "node-b"}}
	s, err := NewServer(client, nil, nil, peers, ".test", "17700", nil)
	if err != nil {
		t.Fatalf("NewServer() error: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/invariants", nil)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if peers.lastAddr == "" {
		t.Fatal("expected a HostStats fetch to the non-local voter node-b")
	}
	if strings.Contains(peers.lastAddr, "node-c") {
		t.Errorf("must never query node-c (Nonvoter) for quorum-tolerance, got addr: %q", peers.lastAddr)
	}
}

func TestHandleInvariantsPage_LeaderVsNonLeaderQuorumEvaluationDiffers(t *testing.T) {
	membersFor := func(leaderID string) *rpcpb.StatusResponse {
		return &rpcpb.StatusResponse{
			ManagerNodeId: "node-a", RaftReachable: true, RaftLeaderId: leaderID,
			Members: []*rpcpb.RaftMember{
				{NodeId: "node-a", Suffrage: "Voter"},
				{NodeId: "node-b", Suffrage: "Voter"},
				{NodeId: "node-c", Suffrage: "Voter"},
			},
		}
	}
	peers := &fakePeerHostStatsClient{resp: &rpcpb.HostStatsResponse{}}

	run := func(leaderID string) string {
		client := &fakeClient{statusResp: membersFor(leaderID)}
		s, err := NewServer(client, nil, nil, peers, ".test", "17700", nil)
		if err != nil {
			t.Fatalf("NewServer() error: %v", err)
		}
		req := httptest.NewRequest(http.MethodGet, "/invariants", nil)
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, req)
		return rec.Body.String()
	}

	// With 3 voters all reachable, the AGGREGATE quorum-tolerance
	// Result is "unknown" either way (whichever voter is currently
	// leader always downgrades to its own Unknown entry) - that alone
	// doesn't prove isCurrentLeader is recomputed per voter rather than
	// hoisted out of the loop. What must differ between the two runs is
	// WHICH voter's own per-voter evidence entry carries the downgrade:
	// when node-a is leader, node-a's own entry is the downgraded one;
	// when node-b is leader, node-b's own entry is, and node-a's own
	// loss resolves cleanly instead.
	leaderIsLocal := run("node-a")
	leaderIsRemote := run("node-b")

	if !strings.Contains(leaderIsLocal, "Losing node-a has an UNKNOWN") {
		t.Errorf("leader-is-node-a run: expected node-a's own evidence to carry the leader-loss downgrade, got: %s", leaderIsLocal)
	}
	if strings.Contains(leaderIsLocal, "Losing node-b has an UNKNOWN") {
		t.Errorf("leader-is-node-a run: node-b must NOT be downgraded, got: %s", leaderIsLocal)
	}
	if !strings.Contains(leaderIsRemote, "Losing node-b has an UNKNOWN") {
		t.Errorf("leader-is-node-b run: expected node-b's own evidence to carry the leader-loss downgrade, got: %s", leaderIsRemote)
	}
	if strings.Contains(leaderIsRemote, "Losing node-a has an UNKNOWN") {
		t.Errorf("leader-is-node-b run: node-a must NOT be downgraded, got: %s", leaderIsRemote)
	}
}

func TestHandleInvariantsPage_RespectsTimeouts(t *testing.T) {
	oldTimeout, oldOverall := nodeContextTimeout, nodeContextOverallTimeout
	nodeContextTimeout = 10 * time.Millisecond
	nodeContextOverallTimeout = 50 * time.Millisecond
	t.Cleanup(func() { nodeContextTimeout, nodeContextOverallTimeout = oldTimeout, oldOverall })

	client := &fakeClient{
		statusResp: &rpcpb.StatusResponse{
			ManagerNodeId: "node-a", RaftReachable: true, RaftLeaderId: "node-a",
			Members: []*rpcpb.RaftMember{
				{NodeId: "node-a", Suffrage: "Voter"},
				{NodeId: "node-b", Suffrage: "Voter"},
			},
		},
		listResp: &rpcpb.ListVMsResponse{Vms: []*rpcpb.VMDefinition{
			{Id: "vm-1", Name: "web-1", NodeId: "node-a", ReplicaNodeId: "node-b"},
		}},
	}
	s, err := NewServer(client, nil, nil, slowRecoveryPeerClient{}, ".test", "17700", nil)
	if err != nil {
		t.Fatalf("NewServer() error: %v", err)
	}

	done := make(chan struct{})
	go func() {
		req := httptest.NewRequest(http.MethodGet, "/invariants", nil)
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, req)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("handleInvariantsPage did not return within 3s of a permanently-blocking peer - timeouts are not being respected")
	}
}
