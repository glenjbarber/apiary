package frontend

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	rpcpb "github.com/glenjbarber/apiary/api/rpc"
)

func TestHandleWhyNotPage_UnknownCellIDRendersExplicitError(t *testing.T) {
	client := &fakeClient{statusResp: &rpcpb.StatusResponse{ManagerNodeId: "node-a"}}
	s := newTestServer(t, client)

	req := httptest.NewRequest(http.MethodGet, "/why-not?cell_id=vm-missing", nil)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	body := rec.Body.String()
	if !strings.Contains(body, "vm-missing") || !strings.Contains(body, "was not found") {
		t.Fatalf("expected an explicit not-found error, got: %s", body)
	}
	if strings.Contains(body, "cell-migrate") || strings.Contains(body, "cell-recoverable") {
		t.Errorf("must not render an empty answer for an unknown cell, got: %s", body)
	}
}

func TestHandleWhyNotPage_UnknownNodeIDRendersExplicitError(t *testing.T) {
	client := &fakeClient{
		statusResp:   &rpcpb.StatusResponse{ManagerNodeId: "node-a"},
		simulateResp: &rpcpb.SimulateNodeFailureResponse{Error: `node_id "node-x" is not recognized`},
	}
	s := newTestServer(t, client)

	req := httptest.NewRequest(http.MethodGet, "/why-not?node_id=node-x", nil)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	body := rec.Body.String()
	if !strings.Contains(body, "not recognized") {
		t.Fatalf("expected the SimulateNodeFailure not-recognized error to surface, got: %s", body)
	}
}

func TestHandleWhyNotPage_UnknownNetworkIDRendersExplicitError(t *testing.T) {
	client := &fakeClient{statusResp: &rpcpb.StatusResponse{ManagerNodeId: "node-a"}}
	s := newTestServer(t, client)

	req := httptest.NewRequest(http.MethodGet, "/why-not?network_id=net-missing", nil)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	body := rec.Body.String()
	if !strings.Contains(body, "net-missing") || !strings.Contains(body, "was not found") {
		t.Fatalf("expected an explicit not-found error, got: %s", body)
	}
}

func TestHandleWhyNotPage_CellDropdownIncludesVMsAndJails(t *testing.T) {
	client := &fakeClient{
		statusResp: &rpcpb.StatusResponse{ManagerNodeId: "node-a"},
		listResp: &rpcpb.ListVMsResponse{Vms: []*rpcpb.VMDefinition{
			{Id: "vm-1", Name: "web", NodeId: "node-a"},
		}},
		listJailsResp: &rpcpb.ListJailsResponse{Jails: []*rpcpb.JailDefinition{
			{Id: "jail-1", Name: "worker", NodeId: "node-b"},
			{Id: "timemachine", Name: "timemachine", NodeId: "node-a"},
		}},
	}
	s := newTestServer(t, client)

	req := httptest.NewRequest(http.MethodGet, "/why-not", nil)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	body := rec.Body.String()
	for _, want := range []string{
		`<select name="cell_id" required>`,
		`VM web (vm-1) · node-a`,
		`Jail worker (jail-1) · node-b`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("expected Why Not Cell dropdown to contain %q, got: %s", want, body)
		}
	}
	if strings.Contains(body, "timemachine") {
		t.Fatalf("protected jail timemachine must not be offered as a Why Not Cell choice, got: %s", body)
	}
}

func TestHandleWhyNotPage_CellMigrateBlockedWithProvenRemedyWhenNoReplica(t *testing.T) {
	client := &fakeClient{
		statusResp: &rpcpb.StatusResponse{ManagerNodeId: "node-a"},
		getVMResp: &rpcpb.GetVMResponse{Found: true, Vm: &rpcpb.VMDefinition{
			Id: "vm-1", Name: "web-1", NodeId: "node-a", DesiredState: rpcpb.VMState_VM_STATE_RUNNING,
		}},
	}
	s := newTestServer(t, client)

	req := httptest.NewRequest(http.MethodGet, "/why-not?cell_id=vm-1", nil)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	body := rec.Body.String()
	if !strings.Contains(body, "cell-migrate") || !strings.Contains(body, `class="badge blocked"`) {
		t.Fatalf("expected a blocked cell-migrate answer, got: %s", body)
	}
	if !strings.Contains(body, "proven") {
		t.Errorf("expected the proven remedy to render, got: %s", body)
	}
	if !strings.Contains(body, "cell-recoverable") {
		t.Errorf("expected the cell-recoverable answer to render alongside cell-migrate for the same cell_id, got: %s", body)
	}
}

func TestHandleWhyNotPage_CellFallsBackToJailWhenNotAVM(t *testing.T) {
	client := &fakeClient{
		statusResp: &rpcpb.StatusResponse{ManagerNodeId: "node-a"},
		getVMResp:  &rpcpb.GetVMResponse{Found: false},
		getJailResp: &rpcpb.GetJailResponse{Found: true, Jail: &rpcpb.JailDefinition{
			Id: "jail-1", Name: "web-jail", NodeId: "node-a", ReplicaNodeId: "node-b", DesiredState: rpcpb.JailState_JAIL_STATE_RUNNING,
		}},
	}
	s := newTestServer(t, client)

	req := httptest.NewRequest(http.MethodGet, "/why-not?cell_id=jail-1", nil)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	body := rec.Body.String()
	if !strings.Contains(body, "(jail-1)") {
		t.Fatalf("expected the jail to be resolved and rendered (scoped to jail-1), got: %s", body)
	}
	// A jail has a replica configured and no capability signal at all -
	// neither cell-migrate nor cell-recoverable may resolve blocked.
	if strings.Contains(body, `class="badge blocked"`) {
		t.Errorf("a jail with a replica configured but no capability signal must not resolve blocked, got: %s", body)
	}
}

func TestHandleWhyNotPage_CellHostStatsFetchFailureIsUnknownNotBlocked(t *testing.T) {
	client := &fakeClient{
		statusResp: &rpcpb.StatusResponse{ManagerNodeId: "node-a"},
		getVMResp: &rpcpb.GetVMResponse{Found: true, Vm: &rpcpb.VMDefinition{
			Id: "vm-1", Name: "web-1", NodeId: "node-a", ReplicaNodeId: "node-b", DesiredState: rpcpb.VMState_VM_STATE_RUNNING,
		}},
	}
	peers := &fakePeerHostStatsClient{err: errors.New("dial tcp: connection refused")}
	s, err := NewServer(client, nil, nil, peers, ".test", "17700", nil)
	if err != nil {
		t.Fatalf("NewServer() error: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/why-not?cell_id=vm-1", nil)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	body := rec.Body.String()
	if !strings.Contains(body, "(vm-1)") {
		t.Fatalf("expected the cell to be found and rendered (scoped to vm-1), got: %s", body)
	}
	if strings.Contains(body, "cell-recoverable") && strings.Contains(body, `class="badge blocked"`) {
		t.Errorf("a HostStats fetch failure to the replica target must resolve unknown, never blocked, got: %s", body)
	}
}

func TestHandleWhyNotPage_NetworkConnectivityBlockedOnDownBridge(t *testing.T) {
	client := &fakeClient{
		statusResp:       &rpcpb.StatusResponse{ManagerNodeId: "node-a"},
		listNetworksResp: &rpcpb.ListNetworksResponse{Networks: []*rpcpb.NetworkDefinition{{Id: "net-1", Name: "prod", Subnet: "10.60.0.0/24"}}},
		listResp: &rpcpb.ListVMsResponse{Vms: []*rpcpb.VMDefinition{
			{Id: "vm-1", Name: "web-1", NodeId: "node-a", NetworkId: "net-1"},
		}},
		bridgeStatusResp: &rpcpb.GetLocalNetworkBridgeStatusResponse{BridgeStatus: "down"},
	}
	s := newTestServer(t, client)

	req := httptest.NewRequest(http.MethodGet, "/why-not?network_id=net-1", nil)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	body := rec.Body.String()
	if !strings.Contains(body, "network-connectivity") || !strings.Contains(body, `class="badge blocked"`) {
		t.Fatalf("expected a blocked network-connectivity answer, got: %s", body)
	}
	if client.bridgeStatusCalls != 1 {
		t.Errorf("expected exactly one bridge status fetch (one owning node on the one requested network), got %d", client.bridgeStatusCalls)
	}
}

func TestHandleWhyNotPage_NetworkConnectivityUnknownWithNoResourcesAttached(t *testing.T) {
	client := &fakeClient{
		statusResp:       &rpcpb.StatusResponse{ManagerNodeId: "node-a"},
		listNetworksResp: &rpcpb.ListNetworksResponse{Networks: []*rpcpb.NetworkDefinition{{Id: "net-1", Name: "prod", Subnet: "10.60.0.0/24"}}},
	}
	s := newTestServer(t, client)

	req := httptest.NewRequest(http.MethodGet, "/why-not?network_id=net-1", nil)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	body := rec.Body.String()
	if !strings.Contains(body, `class="badge unknown"`) {
		t.Fatalf("expected an unknown verdict when nothing is attached to the network, got: %s", body)
	}
	if client.bridgeStatusCalls != 0 {
		t.Errorf("expected zero bridge status fetches when no VM owns this network, got %d", client.bridgeStatusCalls)
	}
}

func TestHandleWhyNotPage_HiveRebootBlockedWhenQuorumDoesNotSurvive(t *testing.T) {
	client := &fakeClient{
		statusResp: &rpcpb.StatusResponse{ManagerNodeId: "node-a"},
		simulateResp: &rpcpb.SimulateNodeFailureResponse{
			Quorum: &rpcpb.QuorumImpact{Survives: false, Note: "quorum would be lost"},
		},
	}
	s := newTestServer(t, client)

	req := httptest.NewRequest(http.MethodGet, "/why-not?node_id=node-a", nil)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	body := rec.Body.String()
	if !strings.Contains(body, "hive-reboot") || !strings.Contains(body, `class="badge blocked"`) {
		t.Fatalf("expected a blocked hive-reboot answer, got: %s", body)
	}
	if !strings.Contains(body, "quorum-tolerance") {
		t.Errorf("expected the quorum-tolerance blocker to be cited, got: %s", body)
	}
}

func TestHandleWhyNotPage_HiveRebootReplicaBackedNeverBlocksOnlyCaveat(t *testing.T) {
	client := &fakeClient{
		statusResp: &rpcpb.StatusResponse{ManagerNodeId: "node-a"},
		simulateResp: &rpcpb.SimulateNodeFailureResponse{
			Quorum: &rpcpb.QuorumImpact{Survives: true},
			ReplicaBackedResources: []*rpcpb.ReplicaBackedImpact{
				{Id: "vm-9", Name: "db-1", Kind: rpcpb.ResourceKind_RESOURCE_KIND_VM, OwnerNodeId: "node-c"},
			},
		},
	}
	s := newTestServer(t, client)

	req := httptest.NewRequest(http.MethodGet, "/why-not?node_id=node-a", nil)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	body := rec.Body.String()
	if !strings.Contains(body, `class="badge clear"`) {
		t.Fatalf("a replica-backed-only resource must not block a reboot - expected a clear verdict, got: %s", body)
	}
	if !strings.Contains(body, "db-1") {
		t.Errorf("expected the replica-backed resource to still appear as a disclosed caveat, got: %s", body)
	}
}
