package frontend

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	rpcpb "github.com/glenjbarber/apiary/api/rpc"
)

// fakePeerHostStatsClient is a fake peerHostStatsClient for testing the
// cluster overview/host pages' cross-node fetch logic without a real
// peer managerd.
type fakePeerHostStatsClient struct {
	lastAddr string
	resp     *rpcpb.HostStatsResponse
	err      error

	lastStatusAddr string
	statusResp     *rpcpb.StatusResponse
	statusErr      error
	statusCalls    int

	lastBridgeAddr    string
	lastBridgeNetwork string
	bridgeResp        *rpcpb.GetLocalNetworkBridgeStatusResponse
	bridgeErr         error
}

func (f *fakePeerHostStatsClient) HostStats(_ context.Context, addr string) (*rpcpb.HostStatsResponse, error) {
	f.lastAddr = addr
	if f.err != nil {
		return nil, f.err
	}
	if f.resp != nil {
		return f.resp, nil
	}
	return &rpcpb.HostStatsResponse{}, nil
}

func (f *fakePeerHostStatsClient) ListISOs(_ context.Context, addr string) (*rpcpb.ListISOsResponse, error) {
	f.lastAddr = addr
	if f.err != nil {
		return nil, f.err
	}
	return &rpcpb.ListISOsResponse{}, nil
}

func (f *fakePeerHostStatsClient) ListAssumptionResults(_ context.Context, addr string, _ *rpcpb.ListAssumptionResultsRequest) (*rpcpb.ListAssumptionResultsResponse, error) {
	f.lastAddr = addr
	if f.err != nil {
		return nil, f.err
	}
	return &rpcpb.ListAssumptionResultsResponse{}, nil
}

func (f *fakePeerHostStatsClient) Status(_ context.Context, addr string) (*rpcpb.StatusResponse, error) {
	f.lastStatusAddr = addr
	f.statusCalls++
	if f.statusErr != nil {
		return nil, f.statusErr
	}
	if f.statusResp != nil {
		return f.statusResp, nil
	}
	return &rpcpb.StatusResponse{}, nil
}

func (f *fakePeerHostStatsClient) GetLocalNetworkBridgeStatus(_ context.Context, addr, networkID string) (*rpcpb.GetLocalNetworkBridgeStatusResponse, error) {
	f.lastBridgeAddr = addr
	f.lastBridgeNetwork = networkID
	if f.bridgeErr != nil {
		return nil, f.bridgeErr
	}
	if f.bridgeResp != nil {
		return f.bridgeResp, nil
	}
	return &rpcpb.GetLocalNetworkBridgeStatusResponse{}, nil
}

func TestServer_ClusterOverviewPage_UnreachableNodeShowsError(t *testing.T) {
	client := &fakeClient{
		statusResp: &rpcpb.StatusResponse{ManagerNodeId: "apiarium", KnownNodeIds: []string{"apiarium", "freebsd-apiary"}},
	}
	peers := &fakePeerHostStatsClient{err: errors.New("connection refused")}
	s, err := NewServer(client, nil, nil, peers, ".apiary.work", "17700", nil)
	if err != nil {
		t.Fatalf("NewServer() error: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	body := rec.Body.String()
	if !strings.Contains(body, "Unreachable") || !strings.Contains(body, "connection refused") {
		t.Errorf("cluster overview page missing unreachable status for freebsd-apiary, got: %s", body)
	}
	// apiarium is the local node (matches ManagerNodeId) - it must still
	// be fetched through the local client, not treated as unreachable
	// just because the peer client is failing.
	if !strings.Contains(body, "Reachable") {
		t.Errorf("cluster overview page should still show apiarium as reachable via the local client, got: %s", body)
	}
}

// The following tests cover Evidence-Aware Health's (ADR-0056) retrofit
// of the cluster overview page - health is computed alongside the
// existing basic-status row, from the same anchor Status() call plus
// each node's own HostStats/Status fetch.

func TestHandleClusterOverviewPage_HostStatsSucceedsStatusFails(t *testing.T) {
	client := &fakeClient{statusResp: &rpcpb.StatusResponse{
		ManagerNodeId: "apiarium",
		KnownNodeIds:  []string{"apiarium", "freebsd-apiary"},
		RaftReachable: true,
		Members: []*rpcpb.RaftMember{
			{NodeId: "apiarium", Suffrage: "Voter"},
			{NodeId: "freebsd-apiary", Suffrage: "Voter"},
		},
	}}
	peers := &fakePeerHostStatsClient{statusErr: errors.New("connection refused")}
	s, err := NewServer(client, nil, nil, peers, ".apiary.work", "17700", nil)
	if err != nil {
		t.Fatalf("NewServer() error: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	body := rec.Body.String()
	if !strings.Contains(body, `class="badge ready">Reachable`) {
		t.Errorf("expected freebsd-apiary to still show Reachable via its own successful HostStats call, got: %s", body)
	}
	if !strings.Contains(body, `class="badge unknown"`) {
		t.Errorf("expected freebsd-apiary's health to be unknown when the peer Status() call fails - not silently Healthy, got: %s", body)
	}
}

func TestHandleClusterOverviewPage_StatusSucceedsHostStatsFails(t *testing.T) {
	client := &fakeClient{statusResp: &rpcpb.StatusResponse{
		ManagerNodeId: "apiarium",
		KnownNodeIds:  []string{"apiarium", "freebsd-apiary"},
		RaftReachable: true,
		Members: []*rpcpb.RaftMember{
			{NodeId: "apiarium", Suffrage: "Voter"},
			// Nonvoter, deliberately: a voter+unreachable combination
			// resolves Contradictory (CODEX's own named example) rather
			// than the plain Degraded this test means to isolate.
			{NodeId: "freebsd-apiary", Suffrage: "Nonvoter"},
		},
	}}
	peers := &fakePeerHostStatsClient{err: errors.New("connection refused"), statusResp: &rpcpb.StatusResponse{RaftReachable: true}}
	s, err := NewServer(client, nil, nil, peers, ".apiary.work", "17700", nil)
	if err != nil {
		t.Fatalf("NewServer() error: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	body := rec.Body.String()
	if !strings.Contains(body, "Unreachable") {
		t.Errorf("expected freebsd-apiary to show Unreachable when its HostStats call fails, got: %s", body)
	}
	if !strings.Contains(body, `class="badge degraded"`) {
		t.Errorf("expected freebsd-apiary's health to be degraded (a confirmed reachability failure) even though its own Status() call succeeded, got: %s", body)
	}
}

func TestHandleClusterOverviewPage_AnchorRaftUnreachableMakesEveryRowMembershipUnknown(t *testing.T) {
	client := &fakeClient{statusResp: &rpcpb.StatusResponse{
		ManagerNodeId: "apiarium",
		KnownNodeIds:  []string{"apiarium", "freebsd-apiary"},
		RaftReachable: false,
	}}
	peers := &fakePeerHostStatsClient{}
	s, err := NewServer(client, nil, nil, peers, ".apiary.work", "17700", nil)
	if err != nil {
		t.Fatalf("NewServer() error: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	body := rec.Body.String()
	if got := strings.Count(body, `class="badge unknown"`); got != 2 {
		t.Errorf(`expected both rows' health to be unknown when the anchor's own raft is unreachable, got %d "badge unknown" occurrences in: %s`, got, body)
	}
}

func TestHandleClusterOverviewPage_LocalNodeNoSecondStatusDial(t *testing.T) {
	client := &fakeClient{statusResp: &rpcpb.StatusResponse{
		ManagerNodeId: "apiarium",
		KnownNodeIds:  []string{"apiarium"},
		RaftReachable: true,
		Members:       []*rpcpb.RaftMember{{NodeId: "apiarium", Suffrage: "Voter"}},
	}}
	peers := &fakePeerHostStatsClient{statusErr: errors.New("must never be called for the local node")}
	s, err := NewServer(client, nil, nil, peers, ".apiary.work", "17700", nil)
	if err != nil {
		t.Fatalf("NewServer() error: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if peers.statusCalls != 0 {
		t.Errorf("peers.Status() was called %d time(s), want 0 for the local node - it must reuse the anchor call", peers.statusCalls)
	}
	if !strings.Contains(rec.Body.String(), `class="badge healthy"`) {
		t.Errorf("expected the local node's health to be healthy, got: %s", rec.Body.String())
	}
}

func TestServer_HostPage_FetchesFromPeerWhenNotLocal(t *testing.T) {
	client := &fakeClient{
		statusResp:    &rpcpb.StatusResponse{ManagerNodeId: "apiarium"},
		hostStatsResp: &rpcpb.HostStatsResponse{NodeId: "apiarium"},
	}
	peers := &fakePeerHostStatsClient{resp: &rpcpb.HostStatsResponse{NodeId: "freebsd-apiary", Cpu: &rpcpb.CPUStats{Cores: 4}}}
	s, err := NewServer(client, nil, nil, peers, ".apiary.work", "17700", nil)
	if err != nil {
		t.Fatalf("NewServer() error: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/host/freebsd-apiary", nil)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if peers.lastAddr != "freebsd-apiary.apiary.work:17700" {
		t.Errorf("peer dial address = %q, want freebsd-apiary.apiary.work:17700", peers.lastAddr)
	}
	if !strings.Contains(rec.Body.String(), "4 cores") {
		t.Errorf("host page missing the peer's own stats, got: %s", rec.Body.String())
	}
}

func TestServer_HostPage_LocalNodeUsesLocalClientNotPeer(t *testing.T) {
	client := &fakeClient{
		statusResp:    &rpcpb.StatusResponse{ManagerNodeId: "apiarium"},
		hostStatsResp: &rpcpb.HostStatsResponse{NodeId: "apiarium", Cpu: &rpcpb.CPUStats{Cores: 8}},
	}
	peers := &fakePeerHostStatsClient{}
	s, err := NewServer(client, nil, nil, peers, ".apiary.work", "17700", nil)
	if err != nil {
		t.Fatalf("NewServer() error: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/host/apiarium", nil)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if peers.lastAddr != "" {
		t.Errorf("peer dial address = %q, want none - the local node should use the local client", peers.lastAddr)
	}
	if !strings.Contains(rec.Body.String(), "8 cores") {
		t.Errorf("host page missing local stats, got: %s", rec.Body.String())
	}
}
