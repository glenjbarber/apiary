package frontend

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	rpcpb "github.com/glenjbarber/apiary/api/rpc"
)

func TestServer_SimulatePage_NoNodeIDShowsPickerOnly(t *testing.T) {
	client := &fakeClient{
		statusResp: &rpcpb.StatusResponse{KnownNodeIds: []string{"node-a", "node-b"}},
	}
	s := newTestServer(t, client)

	req := httptest.NewRequest(http.MethodGet, "/simulate", nil)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "node-a") || !strings.Contains(body, "node-b") {
		t.Errorf("picker missing known nodes, got: %s", body)
	}
	if strings.Contains(body, "Raft quorum impact") {
		t.Errorf("report rendered with no node_id chosen, got: %s", body)
	}
}

func TestServer_SimulatePage_RendersQuorumAndBothImpactSections(t *testing.T) {
	client := &fakeClient{
		statusResp: &rpcpb.StatusResponse{KnownNodeIds: []string{"node-a"}},
		simulateResp: &rpcpb.SimulateNodeFailureResponse{
			Quorum: &rpcpb.QuorumImpact{
				TotalVoters: 2, RemainingVoters: 1, RemainingReachableVoters: 1,
				QuorumSize: 2, Survives: false, Note: "quorum is LOST",
			},
			OwnedResources: []*rpcpb.OwnedResourceImpact{
				{Id: "vm-1", Name: "web-1", Kind: rpcpb.ResourceKind_RESOURCE_KIND_VM, Verdict: rpcpb.RecoveryVerdict_RECOVERY_VERDICT_UNPROTECTED, Explanation: "no replica"},
			},
			ReplicaBackedResources: []*rpcpb.ReplicaBackedImpact{
				{Id: "vm-2", Name: "web-2", Kind: rpcpb.ResourceKind_RESOURCE_KIND_VM, OwnerNodeId: "node-b", Explanation: "keeps running on node-b"},
			},
		},
	}
	s := newTestServer(t, client)

	req := httptest.NewRequest(http.MethodGet, "/simulate?node_id=node-a", nil)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{"quorum LOST", "vm-1", "unprotected", "vm-2", "node-b", "keeps running on node-b"} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q, got: %s", want, body)
		}
	}
	if strings.Contains(body, "\"lost\"") || strings.Contains(strings.ToLower(body), ">lost<") {
		t.Errorf("body must never say \"lost\" (should say \"unprotected\"), got: %s", body)
	}
}

func TestServer_SimulatePage_UnknownTargetShowsErrorBannerNotEmptyReport(t *testing.T) {
	client := &fakeClient{
		statusResp:   &rpcpb.StatusResponse{KnownNodeIds: []string{"node-a"}},
		simulateResp: &rpcpb.SimulateNodeFailureResponse{Error: `node_id "ghost" is not recognized: it does not appear in the raft configuration or as an owner/replica placement for any VM or jail`},
	}
	s := newTestServer(t, client)

	req := httptest.NewRequest(http.MethodGet, "/simulate?node_id=ghost", nil)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	body := rec.Body.String()
	if !strings.Contains(body, "not recognized") {
		t.Errorf("body missing unknown-target error, got: %s", body)
	}
	if strings.Contains(body, "Raft quorum impact") {
		t.Errorf("report tables must not render alongside an error, got: %s", body)
	}
}

func TestServer_SimulatePage_DropdownIncludesPlacementOnlyNode(t *testing.T) {
	client := &fakeClient{
		// "node-c" is only ever a replica target, never in raft
		// membership - the picker must still offer it (design
		// correction 4, ADR-0052).
		statusResp: &rpcpb.StatusResponse{KnownNodeIds: []string{"node-a"}},
		listResp: &rpcpb.ListVMsResponse{Vms: []*rpcpb.VMDefinition{
			{Id: "vm-1", NodeId: "node-a", ReplicaNodeId: "node-c"},
		}},
	}
	s := newTestServer(t, client)

	req := httptest.NewRequest(http.MethodGet, "/simulate", nil)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	body := rec.Body.String()
	if !strings.Contains(body, "node-c") {
		t.Errorf("picker missing placement-only node node-c, got: %s", body)
	}
}

func TestServer_SimulatePage_NonAtomicDisclaimerAlwaysShownWithReport(t *testing.T) {
	client := &fakeClient{
		statusResp:   &rpcpb.StatusResponse{},
		simulateResp: &rpcpb.SimulateNodeFailureResponse{Quorum: &rpcpb.QuorumImpact{}},
	}
	s := newTestServer(t, client)

	req := httptest.NewRequest(http.MethodGet, "/simulate?node_id=node-a", nil)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if !strings.Contains(rec.Body.String(), "not one atomic snapshot") {
		t.Errorf("report rendered without the non-atomicity disclaimer, got: %s", rec.Body.String())
	}
}

func TestServer_SimulatePage_RendersManagedNetworkImpact(t *testing.T) {
	client := &fakeClient{
		listNetworksResp: &rpcpb.ListNetworksResponse{Networks: []*rpcpb.NetworkDefinition{{Id: "net-1", Name: "services"}}},
		simulateNetworkResp: &rpcpb.SimulateNetworkFailureResponse{
			Network:           &rpcpb.NetworkDefinition{Id: "net-1", Name: "services", Subnet: "10.60.0.0/24", VlanId: 100},
			AffectedResources: []*rpcpb.NetworkFailureImpact{{Id: "vm-1", Name: "frontend", NodeId: "node-a", Explanation: "declared connectivity unavailable"}},
			Note:              "declared attachments only",
		},
	}
	s := newTestServer(t, client)

	req := httptest.NewRequest(http.MethodGet, "/simulate?network_id=net-1", nil)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	for _, want := range []string{"Managed network services", "10.60.0.0/24", "vm-1", "node-a", "declared connectivity unavailable", "declared attachments only"} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Errorf("body missing %q, got: %s", want, rec.Body.String())
		}
	}
}

func TestServer_SimulatePage_RendersImageAvailability(t *testing.T) {
	client := &fakeClient{
		statusResp: &rpcpb.StatusResponse{KnownNodeIds: []string{"node-a"}},
		simulateResp: &rpcpb.SimulateNodeFailureResponse{
			Quorum: &rpcpb.QuorumImpact{},
			ImageAvailability: []*rpcpb.ImageAvailabilityImpact{{
				ResourceId: "vm-1", ResourceName: "frontend", ImageName: "ubuntu.raw",
				Role:        rpcpb.ImageRole_IMAGE_ROLE_BASE_IMAGE,
				Verdict:     rpcpb.ImageAvailabilityVerdict_IMAGE_AVAILABILITY_VERDICT_UNAVAILABLE,
				Explanation: "no remaining Hive reports this image",
			}},
		},
	}
	s := newTestServer(t, client)
	req := httptest.NewRequest(http.MethodGet, "/simulate?node_id=node-a", nil)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	for _, want := range []string{"Image availability after Hive loss", "frontend (vm-1)", "ubuntu.raw", "base image", "unavailable", "no remaining Hive"} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Errorf("body missing %q, got: %s", want, rec.Body.String())
		}
	}
}
