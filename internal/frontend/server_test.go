package frontend

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"google.golang.org/grpc"

	rpcpb "github.com/glenjbarber/apiary/api/rpc"
)

// fakeClient implements rpcpb.ManagerServiceClient with canned responses,
// mirroring internal/restshim's test fake.
type fakeClient struct {
	listResp *rpcpb.ListVMsResponse
	listErr  error

	createResp *rpcpb.CreateVMResponse
	createErr  error

	deleteResp *rpcpb.DeleteVMResponse

	lastCreateReq *rpcpb.CreateVMRequest
	lastDeleteReq *rpcpb.DeleteVMRequest
}

func (f *fakeClient) Status(context.Context, *rpcpb.StatusRequest, ...grpc.CallOption) (*rpcpb.StatusResponse, error) {
	return &rpcpb.StatusResponse{}, nil
}

func (f *fakeClient) CreateVM(_ context.Context, in *rpcpb.CreateVMRequest, _ ...grpc.CallOption) (*rpcpb.CreateVMResponse, error) {
	f.lastCreateReq = in
	return f.createResp, f.createErr
}

func (f *fakeClient) UpdateVM(context.Context, *rpcpb.UpdateVMRequest, ...grpc.CallOption) (*rpcpb.UpdateVMResponse, error) {
	return &rpcpb.UpdateVMResponse{}, nil
}

func (f *fakeClient) DeleteVM(_ context.Context, in *rpcpb.DeleteVMRequest, _ ...grpc.CallOption) (*rpcpb.DeleteVMResponse, error) {
	f.lastDeleteReq = in
	return f.deleteResp, nil
}

func (f *fakeClient) GetVM(context.Context, *rpcpb.GetVMRequest, ...grpc.CallOption) (*rpcpb.GetVMResponse, error) {
	return &rpcpb.GetVMResponse{}, nil
}

func (f *fakeClient) ListVMs(context.Context, *rpcpb.ListVMsRequest, ...grpc.CallOption) (*rpcpb.ListVMsResponse, error) {
	return f.listResp, f.listErr
}

var _ rpcpb.ManagerServiceClient = (*fakeClient)(nil)

func newTestServer(t *testing.T, client *fakeClient) *Server {
	t.Helper()
	s, err := NewServer(client)
	if err != nil {
		t.Fatalf("NewServer() error: %v", err)
	}
	return s
}

func TestServer_Index(t *testing.T) {
	client := &fakeClient{listResp: &rpcpb.ListVMsResponse{
		Vms: []*rpcpb.VMDefinition{{Id: "vm-1", Name: "web-1", Phase: rpcpb.VMPhase_VM_PHASE_READY}},
	}}
	s := newTestServer(t, client)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "vm-1") || !strings.Contains(body, "web-1") {
		t.Errorf("index page missing expected VM data, got: %s", body)
	}
	if !strings.Contains(body, "ready") {
		t.Errorf("index page missing observed phase, got: %s", body)
	}
}

func TestServer_ListVMs_ReturnsRowsFragmentOnly(t *testing.T) {
	client := &fakeClient{listResp: &rpcpb.ListVMsResponse{
		Vms: []*rpcpb.VMDefinition{{Id: "vm-1", Name: "web-1", Phase: rpcpb.VMPhase_VM_PHASE_CREATING}},
	}}
	s := newTestServer(t, client)

	req := httptest.NewRequest(http.MethodGet, "/vms", nil)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "vm-1") || !strings.Contains(body, "creating") {
		t.Errorf("response missing expected VM row, got: %s", body)
	}
	if strings.Contains(body, "<html") || strings.Contains(body, "Create VM") {
		t.Errorf("response should be just the rows fragment, not the full page, got: %s", body)
	}
}

func TestServer_Index_ShowsPendingPhaseForUnreconciledVM(t *testing.T) {
	client := &fakeClient{listResp: &rpcpb.ListVMsResponse{
		Vms: []*rpcpb.VMDefinition{{Id: "vm-1", Name: "web-1"}}, // Phase left unset
	}}
	s := newTestServer(t, client)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if !strings.Contains(rec.Body.String(), "pending") {
		t.Errorf("index page should show 'pending' for a VM with no observed phase yet, got: %s", rec.Body.String())
	}
}

func TestServer_Index_ListError(t *testing.T) {
	client := &fakeClient{listResp: &rpcpb.ListVMsResponse{Error: "raftd unreachable"}}
	s := newTestServer(t, client)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (errors render inline, not as HTTP failures)", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "raftd unreachable") {
		t.Errorf("index page missing error message, got: %s", rec.Body.String())
	}
}

func TestServer_CreateVM(t *testing.T) {
	client := &fakeClient{
		createResp: &rpcpb.CreateVMResponse{Vm: &rpcpb.VMDefinition{Id: "vm-1"}},
		listResp:   &rpcpb.ListVMsResponse{Vms: []*rpcpb.VMDefinition{{Id: "vm-1", Name: "web-1"}}},
	}
	s := newTestServer(t, client)

	form := url.Values{"id": {"vm-1"}, "name": {"web-1"}, "vcpus": {"2"}, "memory_mb": {"1024"}, "desired_state": {"running"}}
	req := httptest.NewRequest(http.MethodPost, "/vms", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if client.lastCreateReq.GetVm().GetName() != "web-1" {
		t.Errorf("forwarded vm.Name = %q, want web-1", client.lastCreateReq.GetVm().GetName())
	}
	if client.lastCreateReq.GetVm().GetVcpus() != 2 {
		t.Errorf("forwarded vm.Vcpus = %d, want 2", client.lastCreateReq.GetVm().GetVcpus())
	}
	if !strings.Contains(rec.Body.String(), "vm-1") {
		t.Errorf("response fragment missing created VM, got: %s", rec.Body.String())
	}
}

func TestServer_CreateVM_ErrorStillShowsCurrentList(t *testing.T) {
	client := &fakeClient{
		createResp: &rpcpb.CreateVMResponse{Error: `id "vm-1" already exists`},
		listResp:   &rpcpb.ListVMsResponse{Vms: []*rpcpb.VMDefinition{{Id: "vm-1", Name: "existing"}}},
	}
	s := newTestServer(t, client)

	form := url.Values{"id": {"vm-1"}}
	req := httptest.NewRequest(http.MethodPost, "/vms", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	body := rec.Body.String()
	if !strings.Contains(body, "already exists") {
		t.Errorf("response missing error message, got: %s", body)
	}
	if !strings.Contains(body, "existing") {
		t.Errorf("response missing the existing (unchanged) VM list, got: %s", body)
	}
}

func TestServer_DeleteVM(t *testing.T) {
	client := &fakeClient{
		deleteResp: &rpcpb.DeleteVMResponse{Vm: &rpcpb.VMDefinition{Id: "vm-1"}},
		listResp:   &rpcpb.ListVMsResponse{},
	}
	s := newTestServer(t, client)

	req := httptest.NewRequest(http.MethodDelete, "/vms/vm-1", nil)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if client.lastDeleteReq.GetId() != "vm-1" {
		t.Errorf("forwarded id = %q, want vm-1", client.lastDeleteReq.GetId())
	}
	if !strings.Contains(rec.Body.String(), "No VMs") {
		t.Errorf("response should show empty list after delete, got: %s", rec.Body.String())
	}
}

func TestServer_StaticAssets(t *testing.T) {
	s := newTestServer(t, &fakeClient{})

	req := httptest.NewRequest(http.MethodGet, "/static/htmx.min.js", nil)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 for the vendored htmx.min.js", rec.Code)
	}
	if rec.Body.Len() == 0 {
		t.Errorf("htmx.min.js served empty body")
	}
}
