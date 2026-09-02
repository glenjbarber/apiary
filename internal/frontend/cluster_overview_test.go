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
