package frontend

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	rpcpb "github.com/glenjbarber/apiary/api/rpc"
)

func TestServer_AssumptionsPage_FanOut_OneErrorsOneSucceeds(t *testing.T) {
	client := &fakeClient{
		statusResp: &rpcpb.StatusResponse{ManagerNodeId: "apiarium", KnownNodeIds: []string{"apiarium", "freebsd-apiary"}},
		assumptionsResp: &rpcpb.ListAssumptionResultsResponse{
			Latest: []*rpcpb.AssumptionResult{
				{
					Key:            &rpcpb.AssumptionKey{Kind: rpcpb.AssumptionKind_ASSUMPTION_KIND_PEER_MANAGER_RPC_SUCCEEDED, DependencyId: "freebsd-apiary"},
					ObservedStatus: rpcpb.AssumptionStatus_ASSUMPTION_STATUS_TRUE,
					Status:         rpcpb.AssumptionStatus_ASSUMPTION_STATUS_TRUE,
				},
			},
		},
	}
	peers := &fakePeerHostStatsClient{err: errors.New("connection refused")}
	s, err := NewServer(client, nil, nil, peers, ".apiary.work", "17700", nil)
	if err != nil {
		t.Fatalf("NewServer() error: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/assumptions", nil)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	body := rec.Body.String()
	if !strings.Contains(body, "apiarium") || !strings.Contains(body, "true") {
		t.Errorf("page missing the local (working) node's result, got: %s", body)
	}
	if !strings.Contains(body, "freebsd-apiary") || !strings.Contains(body, "connection refused") {
		t.Errorf("page missing the unreachable peer node's error, got: %s", body)
	}
}

func TestServer_AssumptionsPage_NotApplicableAndStaleRenderDistinctly(t *testing.T) {
	client := &fakeClient{
		statusResp: &rpcpb.StatusResponse{ManagerNodeId: "apiarium", KnownNodeIds: []string{"apiarium"}},
		assumptionsResp: &rpcpb.ListAssumptionResultsResponse{
			Latest: []*rpcpb.AssumptionResult{
				{
					Key:            &rpcpb.AssumptionKey{Kind: rpcpb.AssumptionKind_ASSUMPTION_KIND_NAT_UPLINK_DEFAULT_ROUTE},
					ObservedStatus: rpcpb.AssumptionStatus_ASSUMPTION_STATUS_NOT_APPLICABLE,
					Status:         rpcpb.AssumptionStatus_ASSUMPTION_STATUS_NOT_APPLICABLE,
					ReasonCode:     "uplink_not_configured",
				},
				{
					Key:            &rpcpb.AssumptionKey{Kind: rpcpb.AssumptionKind_ASSUMPTION_KIND_PEER_MANAGER_RPC_SUCCEEDED, DependencyId: "freebsd-apiary"},
					ObservedStatus: rpcpb.AssumptionStatus_ASSUMPTION_STATUS_TRUE,
					// Effective status already safely collapsed to UNKNOWN by
					// the server - the page must render THIS, not
					// ObservedStatus, and must mark it stale distinctly.
					Status: rpcpb.AssumptionStatus_ASSUMPTION_STATUS_UNKNOWN,
					Stale:  true,
				},
			},
		},
	}
	s := newTestServer(t, client)

	req := httptest.NewRequest(http.MethodGet, "/assumptions", nil)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	body := rec.Body.String()
	if !strings.Contains(body, `badge not_applicable`) {
		t.Errorf("not_applicable result must render with its own distinct badge class, got: %s", body)
	}
	if !strings.Contains(body, `badge unknown`) || !strings.Contains(body, `badge stale`) {
		t.Errorf("a stale result must render the effective (unknown) status plus a distinct stale marker, got: %s", body)
	}
	if strings.Contains(body, `badge true">true`) {
		t.Errorf("a stale result's stored true value must never be rendered as the trusted status, got: %s", body)
	}
}

func TestServer_AssumptionsPage_StorageDegradedShowsBanner(t *testing.T) {
	client := &fakeClient{
		statusResp: &rpcpb.StatusResponse{ManagerNodeId: "apiarium", KnownNodeIds: []string{"apiarium"}},
		assumptionsResp: &rpcpb.ListAssumptionResultsResponse{
			StorageDegraded:       true,
			StorageDegradedDetail: "assumptions file was corrupt - preserved as assumptions.json.corrupt-123",
		},
	}
	s := newTestServer(t, client)

	req := httptest.NewRequest(http.MethodGet, "/assumptions", nil)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	body := rec.Body.String()
	if !strings.Contains(body, "Storage warning") || !strings.Contains(body, "corrupt-123") {
		t.Errorf("page missing the storage-degraded banner, got: %s", body)
	}
}
