package frontend

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	rpcpb "github.com/glenjbarber/apiary/api/rpc"
)

func TestHandleNetworksPage_TeardownStatusAggregatesAcrossKnownNodes(t *testing.T) {
	client := &fakeClient{
		statusResp: &rpcpb.StatusResponse{ManagerNodeId: "apiarium", KnownNodeIds: []string{"apiarium", "apiverse"}},
		getNetworkTeardownStatusResp: &rpcpb.GetNetworkTeardownStatusResponse{
			Present: true, Bridge: "apnet-deadbeef", OwnBridge: true,
		},
	}
	peers := &fakePeerHostStatsClient{teardownResp: &rpcpb.GetNetworkTeardownStatusResponse{Present: false}}
	s, err := NewServer(client, nil, nil, peers, ".apiary.work", "17700", nil, false)
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/networks?teardown_network_id=retired", nil)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "apiarium") || !strings.Contains(body, `<span class="badge soon">still present</span>`) {
		t.Errorf("expected apiarium to show still-present status, got: %s", body)
	}
	if !strings.Contains(body, "apiverse") || !strings.Contains(body, `<span class="badge ok">clear</span>`) {
		t.Errorf("expected apiverse to show clear status, got: %s", body)
	}
	if peers.lastTeardownAddr != "apiverse.apiary.work:17700" || peers.lastTeardownNetwork != "retired" {
		t.Errorf("peer call = (addr=%q network=%q), want apiverse.apiary.work:17700 / retired", peers.lastTeardownAddr, peers.lastTeardownNetwork)
	}
}

func TestHandleNetworksPage_TeardownStatusUnreachableNodeIsUnknownNotClear(t *testing.T) {
	client := &fakeClient{
		statusResp: &rpcpb.StatusResponse{ManagerNodeId: "apiarium", KnownNodeIds: []string{"apiarium", "apiverse"}},
		getNetworkTeardownStatusResp: &rpcpb.GetNetworkTeardownStatusResponse{
			Present: false,
		},
	}
	peers := &fakePeerHostStatsClient{teardownErr: errors.New("connection refused")}
	s, err := NewServer(client, nil, nil, peers, ".apiary.work", "17700", nil, false)
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/networks?teardown_network_id=retired", nil)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	body := rec.Body.String()
	if !strings.Contains(body, `<span class="badge unknown"`) {
		t.Errorf("expected an unreachable peer to render as unknown (never clear), got: %s", body)
	}
	if strings.Count(body, `<span class="badge ok">clear</span>`) != 1 {
		t.Errorf("expected exactly one clear status (apiarium only), got: %s", body)
	}
}

func TestHandleNetworksPage_NoTeardownCheckWithoutQueryParam(t *testing.T) {
	client := &fakeClient{listNetworksResp: &rpcpb.ListNetworksResponse{}}
	s := newTestServer(t, client)

	req := httptest.NewRequest(http.MethodGet, "/networks", nil)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	body := rec.Body.String()
	if strings.Contains(body, "badge ok") || strings.Contains(body, "badge soon") {
		t.Errorf("expected no teardown-status table without a query param, got: %s", body)
	}
}
