package frontend

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	rpcpb "github.com/glenjbarber/apiary/api/rpc"
)

func TestHandleCoveragePage_HiveFailureMakesZeroSimulateNodeFailureCalls(t *testing.T) {
	client := &fakeClient{
		statusResp: &rpcpb.StatusResponse{
			ManagerNodeId: "node-a", RaftReachable: true, RaftLeaderId: "node-a",
			Members: []*rpcpb.RaftMember{
				{NodeId: "node-a", Suffrage: "Voter"},
				{NodeId: "node-b", Suffrage: "Voter"},
			},
		},
		listResp: &rpcpb.ListVMsResponse{Vms: []*rpcpb.VMDefinition{
			{Id: "vm-1", Name: "web-1", NodeId: "node-a"},
		}},
	}
	s := newTestServer(t, client)

	req := httptest.NewRequest(http.MethodGet, "/resilience-coverage", nil)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if client.simulateNodeFailureCalls != 0 {
		t.Errorf("expected zero SimulateNodeFailure calls (finding 1 - owned/replica counts come from local VM/jail lists), got %d", client.simulateNodeFailureCalls)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "hive-failure") || !strings.Contains(body, "node-a") {
		t.Fatalf("expected a hive-failure row for node-a, got: %s", body)
	}
	if !strings.Contains(body, "owns 1 cell") {
		t.Errorf("expected node-a's hive-failure row to cite owning 1 cell, got: %s", body)
	}
}

func TestHandleCoveragePage_QuorumLostRendersUnsafeOrImpossible(t *testing.T) {
	client := &fakeClient{
		statusResp: &rpcpb.StatusResponse{
			ManagerNodeId: "node-a", RaftReachable: true, RaftLeaderId: "node-a",
			Members: []*rpcpb.RaftMember{
				{NodeId: "node-a", Suffrage: "Voter"},
				{NodeId: "node-b", Suffrage: "Voter"},
			},
		},
	}
	// A 2-voter cluster with node-b's HostStats fetch failing
	// (unreachable) has zero fault tolerance left - losing EITHER voter
	// loses quorum (quorum size for 2 total is 2), so the aggregate
	// quorum-tolerance scenario must resolve unsafe_or_impossible.
	peers := &fakePeerHostStatsClient{err: errors.New("dial tcp: connection refused")}
	s, err := NewServer(client, nil, nil, peers, ".test", "17700", nil)
	if err != nil {
		t.Fatalf("NewServer() error: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/resilience-coverage", nil)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	body := rec.Body.String()
	if !strings.Contains(body, "quorum-tolerance") {
		t.Fatalf("expected a quorum-tolerance row, got: %s", body)
	}
	if !strings.Contains(body, `class="badge false"`) {
		t.Errorf("expected the quorum-tolerance row to render unsafe_or_impossible (badge false), got: %s", body)
	}
}

func TestHandleCoveragePage_NetworkFailureAndConnectivityBothRender(t *testing.T) {
	client := &fakeClient{
		statusResp: &rpcpb.StatusResponse{ManagerNodeId: "node-a", RaftReachable: true, RaftLeaderId: "node-a"},
		listNetworksResp: &rpcpb.ListNetworksResponse{Networks: []*rpcpb.NetworkDefinition{
			{Id: "net-1", Name: "prod", Subnet: "10.60.0.0/24"},
		}},
		simulateNetworkResp: &rpcpb.SimulateNetworkFailureResponse{
			AffectedResources: []*rpcpb.NetworkFailureImpact{{Id: "vm-1", Name: "web-1"}},
		},
	}
	s := newTestServer(t, client)

	req := httptest.NewRequest(http.MethodGet, "/resilience-coverage", nil)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	body := rec.Body.String()
	if !strings.Contains(body, "network-failure") {
		t.Errorf("expected a network-failure row, got: %s", body)
	}
	if !strings.Contains(body, "affect 1 attached cell") {
		t.Errorf("expected the network-failure row to cite the affected-resource count, got: %s", body)
	}
	if !strings.Contains(body, "network-connectivity") {
		t.Errorf("expected a network-connectivity row (distinct from network-failure), got: %s", body)
	}
}

func TestHandleCoveragePage_HASTDualPrimaryAlwaysUntestedNeverSimulated(t *testing.T) {
	client := &fakeClient{
		statusResp: &rpcpb.StatusResponse{ManagerNodeId: "node-a", RaftReachable: true, RaftLeaderId: "node-a"},
		listResp: &rpcpb.ListVMsResponse{Vms: []*rpcpb.VMDefinition{
			{Id: "vm-1", Name: "web-1", NodeId: "node-a", ReplicaNodeId: "node-b"},
		}},
	}
	s := newTestServer(t, client)

	req := httptest.NewRequest(http.MethodGet, "/resilience-coverage", nil)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	body := rec.Body.String()
	idx := strings.Index(body, "hast-dual-primary")
	if idx == -1 {
		t.Fatalf("expected a hast-dual-primary row, got: %s", body)
	}
	// Find the badge rendered on the SAME row as hast-dual-primary,
	// not just anywhere on the page.
	row := body[idx:]
	if end := strings.Index(row, "</tr>"); end != -1 {
		row = row[:end]
	}
	if !strings.Contains(row, `class="badge unknown"`) {
		t.Errorf("expected the hast-dual-primary row to render untested (badge unknown), got row: %s", row)
	}
}

func TestHandleCoveragePage_CountsTallyShowsAllFiveStatusesZeroFilled(t *testing.T) {
	client := &fakeClient{statusResp: &rpcpb.StatusResponse{ManagerNodeId: "node-a", RaftReachable: true}}
	s := newTestServer(t, client)

	req := httptest.NewRequest(http.MethodGet, "/resilience-coverage", nil)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	body := rec.Body.String()
	for _, want := range []string{"simulated", "physically_rehearsed", "stale", "untested", "unsafe_or_impossible"} {
		if !strings.Contains(body, want) {
			t.Errorf("expected the Counts tally to show %q even when zero, got: %s", want, body)
		}
	}
}

func TestHandleCoveragePage_KnownGapsCalloutRenders(t *testing.T) {
	client := &fakeClient{statusResp: &rpcpb.StatusResponse{ManagerNodeId: "node-a", RaftReachable: true}}
	s := newTestServer(t, client)

	req := httptest.NewRequest(http.MethodGet, "/resilience-coverage", nil)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	body := rec.Body.String()
	if !strings.Contains(body, "Known unmodeled failure classes") || !strings.Contains(body, "Uplink") {
		t.Fatalf("expected the disclosed Gaps callout to render, got: %s", body)
	}
}
