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
	s, err := NewServer(client, nil, nil, peers, ".test", "17700", nil, false)
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
	s, err := NewServer(client, nil, nil, peers, ".test", "17700", nil, false)
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
		s, err := NewServer(client, nil, nil, peers, ".test", "17700", nil, false)
		if err != nil {
			t.Fatalf("NewServer() error: %v", err)
		}
		req := httptest.NewRequest(http.MethodGet, "/invariants", nil)
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, req)
		return rec.Body.String()
	}

	// The page's reachability snapshot is gathered by THIS frontend
	// (node-a) dialling each voter directly - HostStats is a node-local
	// read, never a leader-forwarded one - so the leader-loss downgrade
	// is only entitled to fire when this frontend IS the leader. That
	// is the whole point of recovery.QuorumVantage: the argument behind
	// the downgrade is that leader-to-voter reachability proves nothing
	// about voter-to-voter reachability, and that argument only ever
	// described data the leader itself produced.
	//
	// So when node-a is the leader, the probes really are the leader's
	// and node-a's own loss is downgraded to Unknown. When node-b is
	// the leader, node-a's probes are its own and no voter's loss is
	// downgraded for a reason that does not apply - which is what
	// turns the page's long-standing "unknown" verdict into "true" on a
	// healthy colony, the correction this vantage change exists to
	// make.
	leaderIsLocal := run("node-a")
	leaderIsRemote := run("node-b")

	if !strings.Contains(leaderIsLocal, "Losing node-a has an UNKNOWN") {
		t.Errorf("leader-is-node-a run: a frontend that IS the leader still downgrades its own loss, got: %s", leaderIsLocal)
	}
	if strings.Contains(leaderIsLocal, "Losing node-b has an UNKNOWN") {
		t.Errorf("leader-is-node-a run: node-b must NOT be downgraded, got: %s", leaderIsLocal)
	}
	if strings.Contains(leaderIsRemote, "has an UNKNOWN") {
		t.Errorf("leader-is-node-b run: a non-leader frontend's own probes must not be downgraded at all, got: %s", leaderIsRemote)
	}
	// The strong claim, stated directly: a healthy three-voter colony
	// seen from a node that is not the leader now reads as tolerating
	// the loss of any one more voter, which is what the evidence
	// actually supports.
	if !strings.Contains(leaderIsRemote, "quorum-tolerance") {
		t.Errorf("leader-is-node-b run: expected a quorum-tolerance evaluation, got: %s", leaderIsRemote)
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
	s, err := NewServer(client, nil, nil, slowRecoveryPeerClient{}, ".test", "17700", nil, false)
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
