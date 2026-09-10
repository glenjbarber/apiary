package frontend

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	rpcpb "github.com/glenjbarber/apiary/api/rpc"
)

// TestServer_VMDetailPage_ShowsSnapshots confirms the Snapshots panel
// lists whatever ListVMSnapshots reports, mirroring TestServer_VMDetailPage's
// own shape for a new detail-page section.
func TestServer_VMDetailPage_ShowsSnapshots(t *testing.T) {
	client := &fakeClient{
		getVMResp:           &rpcpb.GetVMResponse{Found: true, Vm: &rpcpb.VMDefinition{Id: "vm-1", Name: "database"}},
		listVMSnapshotsResp: &rpcpb.ListVMSnapshotsResponse{SnapshotNames: []string{"before-upgrade", "nightly-1"}},
	}
	s, err := NewServer(client, nil, nil, nil, "", "", nil, false)
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/vms/vm-1", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{"Snapshots", "before-upgrade", "nightly-1"} {
		if !strings.Contains(body, want) {
			t.Errorf("detail page missing %q", want)
		}
	}
}

// TestServer_HandleCreateVMSnapshot_PostsToManagerd confirms the create
// form submits the trimmed snapshot name and re-renders the VM page.
func TestServer_HandleCreateVMSnapshot_PostsToManagerd(t *testing.T) {
	client := &fakeClient{getVMResp: &rpcpb.GetVMResponse{Found: true, Vm: &rpcpb.VMDefinition{Id: "vm-1"}}}
	s, err := NewServer(client, nil, nil, nil, "", "", nil, false)
	if err != nil {
		t.Fatal(err)
	}
	form := url.Values{"snapshot_name": {"  before-upgrade  "}}
	req := httptest.NewRequest(http.MethodPost, "/vms/vm-1/snapshots", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if client.lastCreateVMSnapshotReq.GetId() != "vm-1" || client.lastCreateVMSnapshotReq.GetSnapshotName() != "before-upgrade" {
		t.Errorf("CreateVMSnapshot request = %+v, want Id=vm-1 SnapshotName=before-upgrade", client.lastCreateVMSnapshotReq)
	}
}

// TestServer_HandleCreateVMSnapshot_ReportsManagerdError confirms a
// managerd-reported error surfaces in the re-rendered page rather than
// being silently dropped.
func TestServer_HandleCreateVMSnapshot_ReportsManagerdError(t *testing.T) {
	client := &fakeClient{
		getVMResp:            &rpcpb.GetVMResponse{Found: true, Vm: &rpcpb.VMDefinition{Id: "vm-1"}},
		createVMSnapshotResp: &rpcpb.CreateVMSnapshotResponse{Error: "snapshot name already exists"},
	}
	s, err := NewServer(client, nil, nil, nil, "", "", nil, false)
	if err != nil {
		t.Fatal(err)
	}
	form := url.Values{"snapshot_name": {"dup"}}
	req := httptest.NewRequest(http.MethodPost, "/vms/vm-1/snapshots", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if !strings.Contains(rec.Body.String(), "snapshot name already exists") {
		t.Errorf("expected managerd error in re-rendered page, got: %s", rec.Body.String())
	}
}

// TestServer_HandleRestoreVMSnapshot_PostsToManagerd confirms the
// restore route reaches the right VM/snapshot name pair from the URL.
func TestServer_HandleRestoreVMSnapshot_PostsToManagerd(t *testing.T) {
	client := &fakeClient{getVMResp: &rpcpb.GetVMResponse{Found: true, Vm: &rpcpb.VMDefinition{Id: "vm-1"}}}
	s, err := NewServer(client, nil, nil, nil, "", "", nil, false)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/vms/vm-1/snapshots/before-upgrade/restore", nil)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if client.lastRestoreVMSnapshotReq.GetId() != "vm-1" || client.lastRestoreVMSnapshotReq.GetSnapshotName() != "before-upgrade" {
		t.Errorf("RestoreVMSnapshot request = %+v, want Id=vm-1 SnapshotName=before-upgrade", client.lastRestoreVMSnapshotReq)
	}
}

// TestServer_HandleDeleteVMSnapshot_PostsToManagerd mirrors the restore
// test for the delete route.
func TestServer_HandleDeleteVMSnapshot_PostsToManagerd(t *testing.T) {
	client := &fakeClient{getVMResp: &rpcpb.GetVMResponse{Found: true, Vm: &rpcpb.VMDefinition{Id: "vm-1"}}}
	s, err := NewServer(client, nil, nil, nil, "", "", nil, false)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/vms/vm-1/snapshots/before-upgrade/delete", nil)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if client.lastDeleteVMSnapshotReq.GetId() != "vm-1" || client.lastDeleteVMSnapshotReq.GetSnapshotName() != "before-upgrade" {
		t.Errorf("DeleteVMSnapshot request = %+v, want Id=vm-1 SnapshotName=before-upgrade", client.lastDeleteVMSnapshotReq)
	}
}
