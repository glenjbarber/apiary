package frontend

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	rpcpb "github.com/glenjbarber/apiary/api/rpc"
)

// validQuorumImpact is a small, internally-consistent QuorumImpact
// fixture - recovery.ValidQuorumFact rejects the zero-valued
// &rpcpb.QuorumImpact{} several existing /simulate tests use, so every
// "successful generation" test here needs real, consistent numbers.
func validQuorumImpact(survives bool) *rpcpb.QuorumImpact {
	reachable := uint32(2)
	if !survives {
		reachable = 0
	}
	return &rpcpb.QuorumImpact{
		TotalVoters: 3, RemainingVoters: 2, RemainingReachableVoters: reachable,
		RemainingUnknownVoters: 0, QuorumSize: 2, Survives: survives,
	}
}

func TestHandleRecoveryHandbookPage_NoNodeIDShowsPickerScopeAndTopology(t *testing.T) {
	client := &fakeClient{
		statusResp: &rpcpb.StatusResponse{
			ManagerNodeId: "node-a", KnownNodeIds: []string{"node-a", "node-b"}, RaftReachable: true,
			Members: []*rpcpb.RaftMember{{NodeId: "node-a", Suffrage: "Voter"}, {NodeId: "node-b", Suffrage: "Voter"}},
		},
	}
	s := newTestServer(t, client)

	req := httptest.NewRequest(http.MethodGet, "/recovery-handbook", nil)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	body := rec.Body.String()
	if !strings.Contains(body, "node-a") || !strings.Contains(body, "node-b") {
		t.Errorf("picker/topology missing known nodes, got: %s", body)
	}
	if !strings.Contains(body, "Scope of this edition") {
		t.Errorf("scope block missing, got: %s", body)
	}
	if strings.Contains(body, "Recovery steps") {
		t.Errorf("steps section rendered with no node_id chosen, got: %s", body)
	}
}

func TestHandleRecoveryHandbookPage_CleanResponseRendersStepsAndAppendix(t *testing.T) {
	client := &fakeClient{
		statusResp: &rpcpb.StatusResponse{
			ManagerNodeId: "node-a", RaftReachable: true, RaftLeaderId: "node-c",
			Members: []*rpcpb.RaftMember{{NodeId: "node-a", Suffrage: "Voter"}},
		},
		simulateResp: &rpcpb.SimulateNodeFailureResponse{
			Quorum: validQuorumImpact(true),
			OwnedResources: []*rpcpb.OwnedResourceImpact{
				{Id: "vm-1", Name: "web-1", Kind: rpcpb.ResourceKind_RESOURCE_KIND_VM, ReplicaNodeId: "node-b", Verdict: rpcpb.RecoveryVerdict_RECOVERY_VERDICT_UNVERIFIED_REPLICA},
			},
		},
	}
	s := newTestServer(t, client)

	req := httptest.NewRequest(http.MethodGet, "/recovery-handbook?node_id=node-a", nil)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	body := rec.Body.String()
	if !strings.Contains(body, "Recovery steps") {
		t.Fatalf("steps section missing, got: %s", body)
	}
	if !strings.Contains(body, "web-1") || !strings.Contains(body, "MigrateVM") {
		t.Errorf("migration step missing expected content, got: %s", body)
	}
	if !strings.Contains(body, "Node context") {
		t.Errorf("node context section missing, got: %s", body)
	}
	if !strings.Contains(body, "Evidence appendix") {
		t.Errorf("evidence appendix missing, got: %s", body)
	}
	if !strings.Contains(body, "Snapshot fingerprint") {
		t.Errorf("fingerprint missing, got: %s", body)
	}
}

func TestHandleRecoveryHandbookPage_SimulateTransportErrorRendersBanner(t *testing.T) {
	client := &fakeClient{
		statusResp:  &rpcpb.StatusResponse{ManagerNodeId: "node-a", RaftReachable: true},
		simulateErr: errors.New("connection refused"),
	}
	s := newTestServer(t, client)

	req := httptest.NewRequest(http.MethodGet, "/recovery-handbook?node_id=node-a", nil)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	body := rec.Body.String()
	if !strings.Contains(body, "could not be generated") || !strings.Contains(body, "connection refused") {
		t.Errorf("expected a generation-failed banner naming the transport error, got: %s", body)
	}
	if strings.Contains(body, "Recovery steps") {
		t.Errorf("steps must not render alongside a generation error, got: %s", body)
	}
}

func TestHandleRecoveryHandbookPage_SimulateResponseErrorRendersBanner(t *testing.T) {
	client := &fakeClient{
		statusResp:   &rpcpb.StatusResponse{ManagerNodeId: "node-a", RaftReachable: true},
		simulateResp: &rpcpb.SimulateNodeFailureResponse{Error: "node_id \"ghost\" is not recognized"},
	}
	s := newTestServer(t, client)

	req := httptest.NewRequest(http.MethodGet, "/recovery-handbook?node_id=ghost", nil)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	body := rec.Body.String()
	if !strings.Contains(body, "could not be generated") || !strings.Contains(body, "not recognized") {
		t.Errorf("expected a generation-failed banner naming the RPC-level error, got: %s", body)
	}
}

func TestHandleRecoveryHandbookPage_RaftUnreachableRendersBannerNotSilentFalse(t *testing.T) {
	// Status() itself succeeds at the transport level but reports
	// RaftReachable=false - a handler checking only the transport error
	// would silently treat this as "not the leader" instead of refusing
	// to generate (the exact gap the second design-review pass found).
	client := &fakeClient{
		statusResp: &rpcpb.StatusResponse{ManagerNodeId: "node-a", RaftReachable: false, RaftError: "raftd unreachable"},
		simulateResp: &rpcpb.SimulateNodeFailureResponse{
			Quorum: validQuorumImpact(true),
		},
	}
	s := newTestServer(t, client)

	req := httptest.NewRequest(http.MethodGet, "/recovery-handbook?node_id=node-a", nil)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	body := rec.Body.String()
	if !strings.Contains(body, "could not be generated") || !strings.Contains(body, "raftd unreachable") {
		t.Errorf("expected a generation-failed banner naming the raft-unreachable cause, got: %s", body)
	}
	if strings.Contains(body, "Recovery steps") {
		t.Errorf("steps must not render when raft itself is unreachable, got: %s", body)
	}
}

func TestHandleRecoveryHandbookPage_ZeroQuorumSizeRendersBannerNotFabricatedLost(t *testing.T) {
	// A QuorumSize of 0 is the concrete signature of internal/raft.Node.Status
	// silently leaving Servers nil (ADR-0056's own disclosed gap) - this
	// must never flow through as a fabricated "quorum LOST" finding.
	client := &fakeClient{
		statusResp:   &rpcpb.StatusResponse{ManagerNodeId: "node-a", RaftReachable: true},
		simulateResp: &rpcpb.SimulateNodeFailureResponse{Quorum: &rpcpb.QuorumImpact{}},
	}
	s := newTestServer(t, client)

	req := httptest.NewRequest(http.MethodGet, "/recovery-handbook?node_id=node-a", nil)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	body := rec.Body.String()
	if !strings.Contains(body, "could not be generated") {
		t.Errorf("expected a generation-failed banner for a structurally-invalid quorum reading, got: %s", body)
	}
	if strings.Contains(body, "Recovery steps") || strings.Contains(strings.ToLower(body), "lost") {
		t.Errorf("must never render a fabricated LOST verdict from empty/invalid quorum data, got: %s", body)
	}
}

func TestHandleRecoveryHandbookPage_CurrentLeaderShowsUnknownDowngrade(t *testing.T) {
	client := &fakeClient{
		statusResp: &rpcpb.StatusResponse{ManagerNodeId: "node-a", RaftReachable: true, RaftLeaderId: "node-a"},
		simulateResp: &rpcpb.SimulateNodeFailureResponse{
			Quorum: validQuorumImpact(true),
		},
	}
	s := newTestServer(t, client)

	req := httptest.NewRequest(http.MethodGet, "/recovery-handbook?node_id=node-a", nil)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	body := rec.Body.String()
	if !strings.Contains(body, "UNKNOWN") {
		t.Errorf("expected the leader-loss quorum downgrade to UNKNOWN, got: %s", body)
	}
	if !strings.Contains(body, "leadership") {
		t.Errorf("expected the leader-loss caveat explanation, got: %s", body)
	}
}

// slowRecoveryPeerClient is a dedicated fake peerHostStatsClient for
// exercising gatherNodeContext's real per-node/overall timeouts - it
// blocks until its own context is cancelled (mirroring a genuinely
// unreachable peer under a real gRPC deadline) rather than a fixed
// short error, so the test proves the timeout itself is what unblocks
// the request, not a fast failure path.
type slowRecoveryPeerClient struct{}

func (slowRecoveryPeerClient) HostStats(ctx context.Context, addr string) (*rpcpb.HostStatsResponse, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}
func (slowRecoveryPeerClient) ListISOs(ctx context.Context, addr string) (*rpcpb.ListISOsResponse, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}
func (slowRecoveryPeerClient) ListAssumptionResults(ctx context.Context, addr string, _ *rpcpb.ListAssumptionResultsRequest) (*rpcpb.ListAssumptionResultsResponse, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}
func (slowRecoveryPeerClient) Status(ctx context.Context, addr string) (*rpcpb.StatusResponse, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}
func (slowRecoveryPeerClient) GetLocalNetworkBridgeStatus(ctx context.Context, addr, networkID string) (*rpcpb.GetLocalNetworkBridgeStatusResponse, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

func TestHandleRecoveryHandbookPage_NodeContextRespectsTimeouts(t *testing.T) {
	oldTimeout, oldOverall := nodeContextTimeout, nodeContextOverallTimeout
	nodeContextTimeout = 10 * time.Millisecond
	nodeContextOverallTimeout = 50 * time.Millisecond
	t.Cleanup(func() { nodeContextTimeout, nodeContextOverallTimeout = oldTimeout, oldOverall })

	client := &fakeClient{
		statusResp: &rpcpb.StatusResponse{ManagerNodeId: "node-a", RaftReachable: true},
		simulateResp: &rpcpb.SimulateNodeFailureResponse{
			Quorum: validQuorumImpact(true),
			OwnedResources: []*rpcpb.OwnedResourceImpact{
				{Id: "vm-1", Name: "web-1", Kind: rpcpb.ResourceKind_RESOURCE_KIND_VM, ReplicaNodeId: "node-b", Verdict: rpcpb.RecoveryVerdict_RECOVERY_VERDICT_UNVERIFIED_REPLICA},
			},
		},
	}
	s, err := NewServer(client, nil, nil, slowRecoveryPeerClient{}, ".test", "17700", nil)
	if err != nil {
		t.Fatalf("NewServer() error: %v", err)
	}

	done := make(chan struct{})
	var rec *httptest.ResponseRecorder
	go func() {
		req := httptest.NewRequest(http.MethodGet, "/recovery-handbook?node_id=node-a", nil)
		rec = httptest.NewRecorder()
		s.ServeHTTP(rec, req)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("handleRecoveryHandbookPage did not return within 2s of a permanently-blocking peer - timeouts are not being respected")
	}

	body := rec.Body.String()
	if !strings.Contains(body, "node-b") {
		t.Errorf("expected node-b still listed in Node Context despite its fetch timing out, got: %s", body)
	}
	if !strings.Contains(body, "unknown") {
		t.Errorf("expected node-b's health to surface as unknown after a timeout, got: %s", body)
	}
}

func TestHandleRecoveryHandbookPage_NodeContextHealthAndAssumptionsVaryIndependently(t *testing.T) {
	client := &fakeClient{
		statusResp: &rpcpb.StatusResponse{ManagerNodeId: "node-a", RaftReachable: true},
		simulateResp: &rpcpb.SimulateNodeFailureResponse{
			Quorum: validQuorumImpact(true),
			OwnedResources: []*rpcpb.OwnedResourceImpact{
				{Id: "vm-1", Name: "web-1", Kind: rpcpb.ResourceKind_RESOURCE_KIND_VM, ReplicaNodeId: "node-b", Verdict: rpcpb.RecoveryVerdict_RECOVERY_VERDICT_UNVERIFIED_REPLICA},
			},
		},
	}
	peers := &fakePeerHostStatsClient{
		resp:      &rpcpb.HostStatsResponse{NodeId: "node-b"},
		statusErr: errors.New("dial tcp: connection refused"),
	}
	s, err := NewServer(client, nil, nil, peers, ".test", "17700", nil)
	if err != nil {
		t.Fatalf("NewServer() error: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/recovery-handbook?node_id=node-a", nil)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	body := rec.Body.String()
	if !strings.Contains(body, "node-b") {
		t.Fatalf("expected node-b in Node Context, got: %s", body)
	}
	// HostStats and ListAssumptionResults both succeed (fakePeerHostStatsClient.err
	// is unset), but the separate peer Status() call - used only for
	// health's own heartbeat/applied-index signal - fails via statusErr.
	// health.ComputeNodeHealth should reflect that as unknown while
	// assumptions has no concerns at all, proving the two evidence types
	// vary independently rather than one flag covering both.
	if !strings.Contains(body, "unknown") {
		t.Errorf("expected node-b's health to be unknown (peer Status failed), got: %s", body)
	}
	if !strings.Contains(body, "no concerns") {
		t.Errorf("expected node-b's assumptions to show no concerns despite health being unknown, got: %s", body)
	}
}
