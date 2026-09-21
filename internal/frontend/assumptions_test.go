package frontend

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	rpcpb "github.com/glenjbarber/apiary/api/rpc"
	"github.com/glenjbarber/apiary/internal/manager"
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
	s, err := NewServer(client, nil, nil, peers, ".apiary.work", "17700", nil, false)
	if err != nil {
		t.Fatalf("NewServer() error: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/assumptions", nil)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	body := rec.Body.String()
	if strings.Count(body, `<details class="panel assumption-host">`) != 2 {
		t.Error("expected two host sections collapsed by default")
	}
	if !strings.Contains(body, `badge true">All green`) || !strings.Contains(body, `badge false">Needs attention`) {
		t.Error("expected green and red summaries for the healthy and unreachable hosts")
	}

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

// TestServer_AssumptionsPage_StaleResultsGroupedSeparately confirms a
// stale result never appears in the main table's own rows - only
// inside the collapsed "superseded/stale" details section - so a
// still-current result and its own stale, superseded counterpart (same
// Kind, different DependencyID - e.g. an uplink assumption keyed on an
// old, no-longer-checked interface name) don't visually read as two
// current results for the same thing.
func TestServer_AssumptionsPage_StaleResultsGroupedSeparately(t *testing.T) {
	client := &fakeClient{
		statusResp: &rpcpb.StatusResponse{ManagerNodeId: "apiverse", KnownNodeIds: []string{"apiverse"}},
		assumptionsResp: &rpcpb.ListAssumptionResultsResponse{
			Latest: []*rpcpb.AssumptionResult{
				{
					Key:            &rpcpb.AssumptionKey{Kind: rpcpb.AssumptionKind_ASSUMPTION_KIND_NAT_UPLINK_DEFAULT_ROUTE, DependencyId: "em0"},
					ObservedStatus: rpcpb.AssumptionStatus_ASSUMPTION_STATUS_TRUE,
					Status:         rpcpb.AssumptionStatus_ASSUMPTION_STATUS_TRUE,
				},
				{
					Key:            &rpcpb.AssumptionKey{Kind: rpcpb.AssumptionKind_ASSUMPTION_KIND_NAT_UPLINK_DEFAULT_ROUTE},
					ObservedStatus: rpcpb.AssumptionStatus_ASSUMPTION_STATUS_NOT_APPLICABLE,
					Status:         rpcpb.AssumptionStatus_ASSUMPTION_STATUS_UNKNOWN,
					Stale:          true,
				},
			},
		},
	}
	s := newTestServer(t, client)

	req := httptest.NewRequest(http.MethodGet, "/assumptions", nil)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	body := rec.Body.String()
	mainTable := body[:strings.Index(body, "<details>")]
	if !strings.Contains(mainTable, "badge true") {
		t.Errorf("current result missing from the main table, got: %s", mainTable)
	}
	if strings.Contains(mainTable, "badge stale") {
		t.Errorf("stale result leaked into the main table rather than staying inside <details>, got: %s", mainTable)
	}
	if !strings.Contains(body, "superseded/stale") || !strings.Contains(body, "badge stale") {
		t.Errorf("stale result missing from the collapsed details section, got: %s", body)
	}
}

// TestServer_AssumptionsPage_ClearStaleButtonRequiresOperator confirms
// the "Clear stale results" button (ADR-0106) only renders for a
// session that can actually use it - a Viewer must never see a control
// for a route their own session would get 403'd on.
func TestServer_AssumptionsPage_ClearStaleButtonRequiresOperator(t *testing.T) {
	staleFixture := &rpcpb.ListAssumptionResultsResponse{
		Latest: []*rpcpb.AssumptionResult{
			{
				Key:            &rpcpb.AssumptionKey{Kind: rpcpb.AssumptionKind_ASSUMPTION_KIND_NAT_UPLINK_DEFAULT_ROUTE},
				ObservedStatus: rpcpb.AssumptionStatus_ASSUMPTION_STATUS_TRUE,
				Status:         rpcpb.AssumptionStatus_ASSUMPTION_STATUS_UNKNOWN,
				Stale:          true,
			},
		},
	}

	client := &fakeClient{
		statusResp:      &rpcpb.StatusResponse{ManagerNodeId: "apiarium", KnownNodeIds: []string{"apiarium"}},
		assumptionsResp: staleFixture,
	}
	roleMap := map[string]manager.Role{"viewer": manager.RoleViewer}
	s, err := NewServer(client, fakeAuthenticator{user: "viewer", pass: "secret"}, roleMap, nil, "", "", nil, false)
	if err != nil {
		t.Fatalf("NewServer() error: %v", err)
	}
	token, _ := s.sessions.Create("viewer", manager.RoleViewer)

	req := httptest.NewRequest(http.MethodGet, "/assumptions", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: token})
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if strings.Contains(rec.Body.String(), "Clear stale results") {
		t.Errorf("a Viewer session must not see the Clear stale results control, got: %s", rec.Body.String())
	}

	// The same fixture, viewed with no auth configured at all (this
	// project's "fully open" default), must show the button.
	openServer := newTestServer(t, &fakeClient{
		statusResp:      &rpcpb.StatusResponse{ManagerNodeId: "apiarium", KnownNodeIds: []string{"apiarium"}},
		assumptionsResp: staleFixture,
	})
	req2 := httptest.NewRequest(http.MethodGet, "/assumptions", nil)
	rec2 := httptest.NewRecorder()
	openServer.ServeHTTP(rec2, req2)
	if !strings.Contains(rec2.Body.String(), "Clear stale results") {
		t.Errorf("with no auth configured, the Clear stale results control must render, got: %s", rec2.Body.String())
	}
}

// TestServer_HandlePurgeStaleAssumptionResults_CallsRPCAndRerendersPage
// confirms the handler forwards node_id, surfaces an RPC-level error,
// and otherwise re-renders the assumptions page (not a bare redirect or
// empty body), matching handleDeleteAssumptionClaim's own established
// re-render-in-place convention.
func TestServer_HandlePurgeStaleAssumptionResults_CallsRPCAndRerendersPage(t *testing.T) {
	client := &fakeClient{
		statusResp:                &rpcpb.StatusResponse{ManagerNodeId: "apiarium", KnownNodeIds: []string{"apiarium"}},
		assumptionsResp:           &rpcpb.ListAssumptionResultsResponse{},
		purgeStaleAssumptionsResp: &rpcpb.PurgeStaleAssumptionResultsResponse{RemovedCount: 3},
	}
	s := newTestServer(t, client)

	req := httptest.NewRequest(http.MethodPost, "/assumptions/purge-stale", strings.NewReader("node_id=apiarium"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "apiarium") {
		t.Errorf("re-rendered page missing the known node, got: %s", rec.Body.String())
	}
}

func TestServer_HandlePurgeStaleAssumptionResults_RPCErrorSurfacedAsBanner(t *testing.T) {
	client := &fakeClient{
		statusResp:               &rpcpb.StatusResponse{ManagerNodeId: "apiarium", KnownNodeIds: []string{"apiarium"}},
		purgeStaleAssumptionsErr: errors.New("simulated: store unavailable"),
	}
	s := newTestServer(t, client)

	req := httptest.NewRequest(http.MethodPost, "/assumptions/purge-stale", strings.NewReader("node_id=apiarium"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if !strings.Contains(rec.Body.String(), "store unavailable") {
		t.Errorf("page missing the surfaced RPC error, got: %s", rec.Body.String())
	}
}

// TestServer_PurgeStaleAssumptionResultsRoute_ViewerBlockedByRouteGate
// mirrors TestServer_ConsoleRoutes_ViewerBlockedByRouteGate exactly -
// this is a state-changing route and must sit behind requireRole
// (RoleOperator, ...), the same tier its own RPC is gated at.
func TestServer_PurgeStaleAssumptionResultsRoute_ViewerBlockedByRouteGate(t *testing.T) {
	client := &fakeClient{statusResp: &rpcpb.StatusResponse{ManagerNodeId: "apiarium"}}
	roleMap := map[string]manager.Role{"viewer": manager.RoleViewer}
	s, err := NewServer(client, fakeAuthenticator{user: "viewer", pass: "secret"}, roleMap, nil, "", "", nil, false)
	if err != nil {
		t.Fatalf("NewServer() error: %v", err)
	}
	token, _ := s.sessions.Create("viewer", manager.RoleViewer)

	req := httptest.NewRequest(http.MethodPost, "/assumptions/purge-stale", strings.NewReader("node_id=apiarium"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: token})
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want %d (forbidden by the route's own requireRole gate)", rec.Code, http.StatusForbidden)
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

func TestNodeAssumptionsAllGreen(t *testing.T) {
	for _, tt := range []struct {
		name string
		node nodeAssumptionsView
		want bool
	}{
		{"passing", nodeAssumptionsView{Results: []assumptionResultView{{Status: "true"}, {Status: "not_applicable"}}}, true},
		{"not applicable", nodeAssumptionsView{Results: []assumptionResultView{{Status: "not_applicable"}}}, true},
		{"failed", nodeAssumptionsView{Results: []assumptionResultView{{Status: "true"}, {Status: "false"}}}, false},
		{"unknown overrides observed true", nodeAssumptionsView{Results: []assumptionResultView{{Status: "unknown", ObservedStatus: "true"}}}, false},
		{"no observations", nodeAssumptionsView{}, false},
		{"unreachable", nodeAssumptionsView{Error: "unreachable"}, false},
		{"storage degraded", nodeAssumptionsView{StorageDegraded: true, Results: []assumptionResultView{{Status: "true"}}}, false},
		{"historical failures", nodeAssumptionsView{Results: []assumptionResultView{{Status: "true"}}, StaleResults: []assumptionResultView{{Status: "false", Stale: true}}}, true},
		{"stale only", nodeAssumptionsView{StaleResults: []assumptionResultView{{Status: "true", Stale: true}}}, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.node.AllGreen(); got != tt.want {
				t.Errorf("AllGreen() = %v, want %v", got, tt.want)
			}
		})
	}
}
