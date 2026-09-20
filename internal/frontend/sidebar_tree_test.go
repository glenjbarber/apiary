package frontend

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	rpcpb "github.com/glenjbarber/apiary/api/rpc"
)

// TestServer_SidebarTreePanel_GroupsCellsByOwningComb confirms every
// known Comb gets its own section (even with zero Cells, so an
// operator sees every Comb at a glance), and that VMs/jails are listed
// under their owning node - not the responding one - since listing is
// already Colony-wide (ADR-0106).
func TestServer_SidebarTreePanel_GroupsCellsByOwningComb(t *testing.T) {
	client := &fakeClient{
		statusResp: &rpcpb.StatusResponse{ManagerNodeId: "brood", KnownNodeIds: []string{"brood", "drone"}},
		listResp: &rpcpb.ListVMsResponse{Vms: []*rpcpb.VMDefinition{
			{Id: "vm-1", Name: "web-1", NodeId: "brood", Phase: rpcpb.VMPhase_VM_PHASE_READY},
			{Id: "vm-2", Name: "db-1", NodeId: "drone", Phase: rpcpb.VMPhase_VM_PHASE_ERROR},
		}},
		listJailsResp: &rpcpb.ListJailsResponse{Jails: []*rpcpb.JailDefinition{
			{Id: "jail-1", Name: "cache-1", NodeId: "drone", Phase: rpcpb.JailPhase_JAIL_PHASE_READY},
		}},
	}
	s := newTestServer(t, client)

	req := httptest.NewRequest(http.MethodGet, "/nav/tree", nil)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	body := rec.Body.String()
	broodSection := body[strings.Index(body, "brood"):strings.Index(body, "drone")]
	if !strings.Contains(broodSection, "web-1") {
		t.Errorf("brood section missing its own VM, got: %s", broodSection)
	}
	if strings.Contains(broodSection, "db-1") || strings.Contains(broodSection, "cache-1") {
		t.Errorf("brood section leaked drone's Cells, got: %s", broodSection)
	}
	droneSection := body[strings.Index(body, "drone"):]
	if !strings.Contains(droneSection, "db-1") || !strings.Contains(droneSection, "cache-1") {
		t.Errorf("drone section missing its own VM/jail, got: %s", droneSection)
	}
	if !strings.Contains(body, `dot-error`) {
		t.Errorf("errored VM missing its error status dot, got: %s", body)
	}
}

// TestServer_SidebarTreePanel_NoCellsShowsPlaceholder confirms a Comb
// with zero VMs/jails still renders its own section (visible at a
// glance, not silently omitted), with an explicit empty-state message
// rather than a blank gap.
func TestServer_SidebarTreePanel_NoCellsShowsPlaceholder(t *testing.T) {
	client := &fakeClient{
		statusResp: &rpcpb.StatusResponse{ManagerNodeId: "apiarium", KnownNodeIds: []string{"apiarium"}},
	}
	s := newTestServer(t, client)

	req := httptest.NewRequest(http.MethodGet, "/nav/tree", nil)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	body := rec.Body.String()
	if !strings.Contains(body, "apiarium") || !strings.Contains(body, "No Cells") {
		t.Errorf("expected apiarium's own section with a No Cells placeholder, got: %s", body)
	}
}

// TestServer_SidebarTreePanel_FetchErrorSurfaced confirms a Status
// fetch failure renders a visible error rather than a silently empty
// tree - matching every other fan-out fetch's own error-surfacing
// convention in this package.
func TestServer_SidebarTreePanel_FetchErrorSurfaced(t *testing.T) {
	client := &fakeClient{statusErr: errors.New("simulated: raft unreachable")}
	s := newTestServer(t, client)

	req := httptest.NewRequest(http.MethodGet, "/nav/tree", nil)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if !strings.Contains(rec.Body.String(), "raft unreachable") {
		t.Errorf("expected the fetch error surfaced in the fragment, got: %s", rec.Body.String())
	}
}
