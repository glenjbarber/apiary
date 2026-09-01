package frontend

import (
	"bytes"
	"context"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"google.golang.org/grpc"

	rpcpb "github.com/glenjbarber/apiary/api/rpc"
	"github.com/glenjbarber/apiary/internal/manager"
)

// fakeClient implements rpcpb.ManagerServiceClient with canned responses,
// mirroring internal/restshim's test fake.
type fakeClient struct {
	statusResp *rpcpb.StatusResponse

	listResp *rpcpb.ListVMsResponse
	listErr  error

	createResp *rpcpb.CreateVMResponse
	createErr  error

	deleteResp *rpcpb.DeleteVMResponse

	lastCreateReq *rpcpb.CreateVMRequest
	lastDeleteReq *rpcpb.DeleteVMRequest

	listISOsResp     *rpcpb.ListISOsResponse
	deleteISOResp    *rpcpb.DeleteISOResponse
	lastDeleteISOReq *rpcpb.DeleteISORequest

	hostStatsResp *rpcpb.HostStatsResponse

	getVMConsoleResp *rpcpb.GetVMConsoleResponse
	getVMConsoleErr  error

	getVMSerialLogResp *rpcpb.GetVMSerialLogResponse
	getVMSerialLogErr  error

	listNetworksResp     *rpcpb.ListNetworksResponse
	createNetworkResp    *rpcpb.CreateNetworkResponse
	deleteNetworkResp    *rpcpb.DeleteNetworkResponse
	lastCreateNetworkReq *rpcpb.CreateNetworkRequest
	lastDeleteNetworkReq *rpcpb.DeleteNetworkRequest

	listJailsResp     *rpcpb.ListJailsResponse
	createJailResp    *rpcpb.CreateJailResponse
	deleteJailResp    *rpcpb.DeleteJailResponse
	lastCreateJailReq *rpcpb.CreateJailRequest
	lastDeleteJailReq *rpcpb.DeleteJailRequest

	listAPIKeysResp     *rpcpb.ListAPIKeysResponse
	createAPIKeyResp    *rpcpb.CreateAPIKeyResponse
	revokeAPIKeyResp    *rpcpb.RevokeAPIKeyResponse
	lastCreateAPIKeyReq *rpcpb.CreateAPIKeyRequest
	lastRevokeAPIKeyReq *rpcpb.RevokeAPIKeyRequest

	uploadStream *fakeUploadClientStream
	uploadErr    error
}

// fakeUploadClientStream is a fake grpc.ClientStreamingClient for
// UploadISO - it records every message the handler sends and returns a
// canned final response, without any real gRPC connection.
type fakeUploadClientStream struct {
	grpc.ClientStream
	sent []*rpcpb.UploadISORequest
	resp *rpcpb.UploadISOResponse
	err  error
}

func (f *fakeUploadClientStream) Send(req *rpcpb.UploadISORequest) error {
	f.sent = append(f.sent, req)
	return nil
}

func (f *fakeUploadClientStream) CloseAndRecv() (*rpcpb.UploadISOResponse, error) {
	return f.resp, f.err
}

func (f *fakeClient) Status(context.Context, *rpcpb.StatusRequest, ...grpc.CallOption) (*rpcpb.StatusResponse, error) {
	if f.statusResp != nil {
		return f.statusResp, nil
	}
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

func (f *fakeClient) ForcePurgeVM(context.Context, *rpcpb.ForcePurgeVMRequest, ...grpc.CallOption) (*rpcpb.ForcePurgeVMResponse, error) {
	return &rpcpb.ForcePurgeVMResponse{}, nil
}

func (f *fakeClient) ForcePurgeJail(context.Context, *rpcpb.ForcePurgeJailRequest, ...grpc.CallOption) (*rpcpb.ForcePurgeJailResponse, error) {
	return &rpcpb.ForcePurgeJailResponse{}, nil
}

func (f *fakeClient) MigrateVM(context.Context, *rpcpb.MigrateVMRequest, ...grpc.CallOption) (*rpcpb.MigrateVMResponse, error) {
	return &rpcpb.MigrateVMResponse{}, nil
}

func (f *fakeClient) ReportVMPhase(context.Context, *rpcpb.ReportVMPhaseRequest, ...grpc.CallOption) (*rpcpb.ReportVMPhaseResponse, error) {
	return &rpcpb.ReportVMPhaseResponse{}, nil
}

func (f *fakeClient) ReportVMTeardownComplete(context.Context, *rpcpb.ReportVMTeardownCompleteRequest, ...grpc.CallOption) (*rpcpb.ReportVMTeardownCompleteResponse, error) {
	return &rpcpb.ReportVMTeardownCompleteResponse{}, nil
}

func (f *fakeClient) ReportJailPhase(context.Context, *rpcpb.ReportJailPhaseRequest, ...grpc.CallOption) (*rpcpb.ReportJailPhaseResponse, error) {
	return &rpcpb.ReportJailPhaseResponse{}, nil
}

func (f *fakeClient) ReportJailTeardownComplete(context.Context, *rpcpb.ReportJailTeardownCompleteRequest, ...grpc.CallOption) (*rpcpb.ReportJailTeardownCompleteResponse, error) {
	return &rpcpb.ReportJailTeardownCompleteResponse{}, nil
}

func (f *fakeClient) MigrateJail(context.Context, *rpcpb.MigrateJailRequest, ...grpc.CallOption) (*rpcpb.MigrateJailResponse, error) {
	return &rpcpb.MigrateJailResponse{}, nil
}

func (f *fakeClient) GetVM(context.Context, *rpcpb.GetVMRequest, ...grpc.CallOption) (*rpcpb.GetVMResponse, error) {
	return &rpcpb.GetVMResponse{}, nil
}

func (f *fakeClient) UploadISO(context.Context, ...grpc.CallOption) (grpc.ClientStreamingClient[rpcpb.UploadISORequest, rpcpb.UploadISOResponse], error) {
	if f.uploadErr != nil {
		return nil, f.uploadErr
	}
	if f.uploadStream == nil {
		f.uploadStream = &fakeUploadClientStream{resp: &rpcpb.UploadISOResponse{}}
	}
	return f.uploadStream, nil
}

func (f *fakeClient) ListISOs(context.Context, *rpcpb.ListISOsRequest, ...grpc.CallOption) (*rpcpb.ListISOsResponse, error) {
	if f.listISOsResp != nil {
		return f.listISOsResp, nil
	}
	return &rpcpb.ListISOsResponse{}, nil
}

func (f *fakeClient) DeleteISO(_ context.Context, in *rpcpb.DeleteISORequest, _ ...grpc.CallOption) (*rpcpb.DeleteISOResponse, error) {
	f.lastDeleteISOReq = in
	if f.deleteISOResp != nil {
		return f.deleteISOResp, nil
	}
	return &rpcpb.DeleteISOResponse{}, nil
}

func (f *fakeClient) HostStats(context.Context, *rpcpb.HostStatsRequest, ...grpc.CallOption) (*rpcpb.HostStatsResponse, error) {
	if f.hostStatsResp != nil {
		return f.hostStatsResp, nil
	}
	return &rpcpb.HostStatsResponse{}, nil
}

func (f *fakeClient) ListVMs(context.Context, *rpcpb.ListVMsRequest, ...grpc.CallOption) (*rpcpb.ListVMsResponse, error) {
	return f.listResp, f.listErr
}

func (f *fakeClient) GetVMConsole(context.Context, *rpcpb.GetVMConsoleRequest, ...grpc.CallOption) (*rpcpb.GetVMConsoleResponse, error) {
	if f.getVMConsoleErr != nil {
		return nil, f.getVMConsoleErr
	}
	if f.getVMConsoleResp != nil {
		return f.getVMConsoleResp, nil
	}
	return &rpcpb.GetVMConsoleResponse{}, nil
}

func (f *fakeClient) GetVMSerialLog(context.Context, *rpcpb.GetVMSerialLogRequest, ...grpc.CallOption) (*rpcpb.GetVMSerialLogResponse, error) {
	if f.getVMSerialLogErr != nil {
		return nil, f.getVMSerialLogErr
	}
	if f.getVMSerialLogResp != nil {
		return f.getVMSerialLogResp, nil
	}
	return &rpcpb.GetVMSerialLogResponse{}, nil
}

func (f *fakeClient) ListNetworks(context.Context, *rpcpb.ListNetworksRequest, ...grpc.CallOption) (*rpcpb.ListNetworksResponse, error) {
	if f.listNetworksResp != nil {
		return f.listNetworksResp, nil
	}
	return &rpcpb.ListNetworksResponse{}, nil
}

func (f *fakeClient) CreateNetwork(_ context.Context, in *rpcpb.CreateNetworkRequest, _ ...grpc.CallOption) (*rpcpb.CreateNetworkResponse, error) {
	f.lastCreateNetworkReq = in
	if f.createNetworkResp != nil {
		return f.createNetworkResp, nil
	}
	return &rpcpb.CreateNetworkResponse{}, nil
}

func (f *fakeClient) DeleteNetwork(_ context.Context, in *rpcpb.DeleteNetworkRequest, _ ...grpc.CallOption) (*rpcpb.DeleteNetworkResponse, error) {
	f.lastDeleteNetworkReq = in
	if f.deleteNetworkResp != nil {
		return f.deleteNetworkResp, nil
	}
	return &rpcpb.DeleteNetworkResponse{}, nil
}

func (f *fakeClient) CreateJail(_ context.Context, in *rpcpb.CreateJailRequest, _ ...grpc.CallOption) (*rpcpb.CreateJailResponse, error) {
	f.lastCreateJailReq = in
	if f.createJailResp != nil {
		return f.createJailResp, nil
	}
	return &rpcpb.CreateJailResponse{}, nil
}

func (f *fakeClient) UpdateJail(context.Context, *rpcpb.UpdateJailRequest, ...grpc.CallOption) (*rpcpb.UpdateJailResponse, error) {
	return &rpcpb.UpdateJailResponse{}, nil
}

func (f *fakeClient) DeleteJail(_ context.Context, in *rpcpb.DeleteJailRequest, _ ...grpc.CallOption) (*rpcpb.DeleteJailResponse, error) {
	f.lastDeleteJailReq = in
	if f.deleteJailResp != nil {
		return f.deleteJailResp, nil
	}
	return &rpcpb.DeleteJailResponse{}, nil
}

func (f *fakeClient) GetJail(context.Context, *rpcpb.GetJailRequest, ...grpc.CallOption) (*rpcpb.GetJailResponse, error) {
	return &rpcpb.GetJailResponse{}, nil
}

func (f *fakeClient) ListJails(context.Context, *rpcpb.ListJailsRequest, ...grpc.CallOption) (*rpcpb.ListJailsResponse, error) {
	if f.listJailsResp != nil {
		return f.listJailsResp, nil
	}
	return &rpcpb.ListJailsResponse{}, nil
}

func (f *fakeClient) ListAPIKeys(context.Context, *rpcpb.ListAPIKeysRequest, ...grpc.CallOption) (*rpcpb.ListAPIKeysResponse, error) {
	if f.listAPIKeysResp != nil {
		return f.listAPIKeysResp, nil
	}
	return &rpcpb.ListAPIKeysResponse{}, nil
}

func (f *fakeClient) CreateAPIKey(_ context.Context, in *rpcpb.CreateAPIKeyRequest, _ ...grpc.CallOption) (*rpcpb.CreateAPIKeyResponse, error) {
	f.lastCreateAPIKeyReq = in
	if f.createAPIKeyResp != nil {
		return f.createAPIKeyResp, nil
	}
	return &rpcpb.CreateAPIKeyResponse{}, nil
}

func (f *fakeClient) RevokeAPIKey(_ context.Context, in *rpcpb.RevokeAPIKeyRequest, _ ...grpc.CallOption) (*rpcpb.RevokeAPIKeyResponse, error) {
	f.lastRevokeAPIKeyReq = in
	if f.revokeAPIKeyResp != nil {
		return f.revokeAPIKeyResp, nil
	}
	return &rpcpb.RevokeAPIKeyResponse{}, nil
}

var _ rpcpb.ManagerServiceClient = (*fakeClient)(nil)

// fakeAuthenticator implements pam.Authenticator with a single fixed
// username/password pair, standing in for a real PAM stack the same
// way every other external dependency in this project gets faked for
// tests.
type fakeAuthenticator struct {
	user, pass string
}

func (f fakeAuthenticator) Authenticate(username, password string) (bool, error) {
	return username == f.user && password == f.pass, nil
}

func newTestServer(t *testing.T, client *fakeClient) *Server {
	t.Helper()
	s, err := NewServer(client, nil, nil)
	if err != nil {
		t.Fatalf("NewServer() error: %v", err)
	}
	return s
}

func newTestServerWithAuth(t *testing.T, client *fakeClient, user, pass string) *Server {
	t.Helper()
	s, err := NewServer(client, fakeAuthenticator{user: user, pass: pass}, map[string]manager.Role{user: manager.RoleAdmin})
	if err != nil {
		t.Fatalf("NewServer() error: %v", err)
	}
	return s
}

func TestServer_VMsPage(t *testing.T) {
	client := &fakeClient{listResp: &rpcpb.ListVMsResponse{
		Vms: []*rpcpb.VMDefinition{{Id: "vm-1", Name: "web-1", Phase: rpcpb.VMPhase_VM_PHASE_READY}},
	}}
	s := newTestServer(t, client)

	req := httptest.NewRequest(http.MethodGet, "/vms", nil)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "vm-1") || !strings.Contains(body, "web-1") {
		t.Errorf("VMs page missing expected VM data, got: %s", body)
	}
	if !strings.Contains(body, "ready") {
		t.Errorf("VMs page missing observed phase, got: %s", body)
	}
	if !strings.Contains(body, `class="active"`) {
		t.Errorf("VMs page nav should mark its own link active, got: %s", body)
	}
}

func TestServer_StatsPage_IsDefaultLandingPage(t *testing.T) {
	client := &fakeClient{hostStatsResp: &rpcpb.HostStatsResponse{
		NodeId: "apiarium",
		Cpu:    &rpcpb.CPUStats{Cores: 8, LoadAvg_1: 1.23},
		Mem:    &rpcpb.MemStats{TotalBytes: 1000, FreeBytes: 400},
		Pools:  []*rpcpb.PoolStats{{Name: "zroot", Health: "ONLINE", CapacityPct: 5}},
		Disks:  []*rpcpb.DiskStats{{Name: "ada0", Healthy: true}},
		Net:    []*rpcpb.NetIfaceStats{{Name: "re0", RxBytes: 100, TxBytes: 200, Up: true}},
		Pf:     &rpcpb.PFStats{Enabled: true, CurrentStates: 3, Matches: 42},
	}}
	s := newTestServer(t, client)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{"apiarium", "8 cores", "zroot", "ONLINE", "ada0", "healthy", "re0"} {
		if !strings.Contains(body, want) {
			t.Errorf("stats page missing %q, got: %s", want, body)
		}
	}
	if !strings.Contains(body, `<span class="success">up</span>`) {
		t.Errorf("stats page missing green 'up' status for re0, got: %s", body)
	}
	if !strings.Contains(body, "42 rule match") {
		t.Errorf("stats page missing pf match count, got: %s", body)
	}
	if !strings.Contains(body, `<a href="/" class="active">Stats</a>`) {
		t.Errorf("stats page nav should mark Stats active, got: %s", body)
	}
}

func TestServer_StatsPage_DiskQueryFailureShownWithoutFalseHealthClaim(t *testing.T) {
	client := &fakeClient{hostStatsResp: &rpcpb.HostStatsResponse{
		Disks: []*rpcpb.DiskStats{{Name: "ada1", Error: "smart: permission denied"}},
	}}
	s := newTestServer(t, client)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	body := rec.Body.String()
	if strings.Contains(body, "healthy") || strings.Contains(body, "FAILING") {
		t.Errorf("a disk with a query error should show neither healthy nor failing, got: %s", body)
	}
	if !strings.Contains(body, "unknown") {
		t.Errorf("stats page missing 'unknown' health for a disk query failure, got: %s", body)
	}
}

func TestServer_StatsPage_ColorsDownInterfaceRed(t *testing.T) {
	client := &fakeClient{hostStatsResp: &rpcpb.HostStatsResponse{
		Net: []*rpcpb.NetIfaceStats{{Name: "tap0", Up: false}},
	}}
	s := newTestServer(t, client)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if !strings.Contains(rec.Body.String(), `<span class="error">down</span>`) {
		t.Errorf("stats page missing red 'down' status for tap0, got: %s", rec.Body.String())
	}
}

func TestServer_ImagesPage(t *testing.T) {
	client := &fakeClient{listISOsResp: &rpcpb.ListISOsResponse{
		Isos: []*rpcpb.ISOInfo{{Name: "debian.iso", SizeBytes: 100, Sha256: "abc"}},
	}}
	s := newTestServer(t, client)

	req := httptest.NewRequest(http.MethodGet, "/images", nil)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "debian.iso") {
		t.Errorf("images page missing stored ISO, got: %s", body)
	}
	if !strings.Contains(body, `id="iso-upload-form"`) {
		t.Errorf("images page missing upload form, got: %s", body)
	}
}

func TestServer_NewVMPage(t *testing.T) {
	client := &fakeClient{}
	s := newTestServer(t, client)

	req := httptest.NewRequest(http.MethodGet, "/vms/new", nil)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, `class="create-vm`) {
		t.Errorf("new VM page missing create form, got: %s", body)
	}
	if !strings.Contains(body, `id="create-error"`) {
		t.Errorf("new VM page missing error slot, got: %s", body)
	}
}

// TestServer_NewJailPage mirrors TestServer_NewVMPage exactly - the
// jail create form now lives on its own page (/jails/new), matching
// the VM create form's own established pattern (ADR-0018), instead of
// inline on the jails list.
func TestServer_NewJailPage(t *testing.T) {
	client := &fakeClient{}
	s := newTestServer(t, client)

	req := httptest.NewRequest(http.MethodGet, "/jails/new", nil)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, `class="create-vm`) {
		t.Errorf("new jail page missing create form, got: %s", body)
	}
	if !strings.Contains(body, `id="create-error"`) {
		t.Errorf("new jail page missing error slot, got: %s", body)
	}
}

// TestServer_JailsPage_HasCreateJailButton confirms the jails list page
// links to the new create-jail page, rather than embedding the form
// inline (the inconsistency a user reported live: VMs had this button/
// separate-page pattern, Jails didn't).
func TestServer_JailsPage_HasCreateJailButton(t *testing.T) {
	client := &fakeClient{}
	s := newTestServer(t, client)

	req := httptest.NewRequest(http.MethodGet, "/jails", nil)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	body := rec.Body.String()
	if !strings.Contains(body, `href='/jails/new'`) {
		t.Errorf("jails page missing link to the create-jail page, got: %s", body)
	}
	if strings.Contains(body, `name="id" required`) {
		t.Errorf("jails page should no longer embed the create form inline, got: %s", body)
	}
}

func TestServer_ListVMs_ReturnsRowsFragmentOnly(t *testing.T) {
	client := &fakeClient{listResp: &rpcpb.ListVMsResponse{
		Vms: []*rpcpb.VMDefinition{{Id: "vm-1", Name: "web-1", Phase: rpcpb.VMPhase_VM_PHASE_CREATING}},
	}}
	s := newTestServer(t, client)

	req := httptest.NewRequest(http.MethodGet, "/vms/rows", nil)
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

func TestServer_ListVMs_DefaultsToSortedByID(t *testing.T) {
	// ListVMs's own order is unspecified - the response here is
	// deliberately not alphabetical, to prove the handler sorts rather
	// than passing the fetch order straight through.
	client := &fakeClient{listResp: &rpcpb.ListVMsResponse{
		Vms: []*rpcpb.VMDefinition{
			{Id: "web-2"}, {Id: "api-1"}, {Id: "db-3"},
		},
	}}
	s := newTestServer(t, client)

	req := httptest.NewRequest(http.MethodGet, "/vms/rows", nil)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	body := rec.Body.String()
	if i, j, k := strings.Index(body, "api-1"), strings.Index(body, "db-3"), strings.Index(body, "web-2"); !(i < j && j < k) {
		t.Errorf("rows not in default alphabetical order by ID, got: %s", body)
	}
}

func TestServer_ListVMs_SortByNodeDescending(t *testing.T) {
	client := &fakeClient{listResp: &rpcpb.ListVMsResponse{
		Vms: []*rpcpb.VMDefinition{
			{Id: "vm-1", NodeId: "node-a"},
			{Id: "vm-2", NodeId: "node-b"},
		},
	}}
	s := newTestServer(t, client)

	req := httptest.NewRequest(http.MethodGet, "/vms/rows?sort=node&dir=desc", nil)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	body := rec.Body.String()
	if i, j := strings.Index(body, "vm-2"), strings.Index(body, "vm-1"); !(i < j) {
		t.Errorf("rows not sorted by node descending, got: %s", body)
	}
}

func TestServer_VMsPage_ShowsPendingPhaseForUnreconciledVM(t *testing.T) {
	client := &fakeClient{listResp: &rpcpb.ListVMsResponse{
		Vms: []*rpcpb.VMDefinition{{Id: "vm-1", Name: "web-1"}}, // Phase left unset
	}}
	s := newTestServer(t, client)

	req := httptest.NewRequest(http.MethodGet, "/vms", nil)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if !strings.Contains(rec.Body.String(), "pending") {
		t.Errorf("VMs page should show 'pending' for a VM with no observed phase yet, got: %s", rec.Body.String())
	}
}

func TestServer_VMsPage_ListError(t *testing.T) {
	client := &fakeClient{listResp: &rpcpb.ListVMsResponse{Error: "raftd unreachable"}}
	s := newTestServer(t, client)

	req := httptest.NewRequest(http.MethodGet, "/vms", nil)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (errors render inline, not as HTTP failures)", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "raftd unreachable") {
		t.Errorf("VMs page missing error message, got: %s", rec.Body.String())
	}
}

func TestServer_CreateVM(t *testing.T) {
	client := &fakeClient{
		createResp: &rpcpb.CreateVMResponse{Vm: &rpcpb.VMDefinition{Id: "vm-1"}},
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
	// On its own page now (see new_vm.html) with no VM table to refresh -
	// success is reported via HX-Redirect to the VMs page, not a
	// re-rendered fragment.
	if got := rec.Header().Get("HX-Redirect"); got != "/vms" {
		t.Errorf("HX-Redirect = %q, want /vms", got)
	}
}

func TestServer_CreateVM_ErrorRendersDirectlyNoRedirect(t *testing.T) {
	client := &fakeClient{
		createResp: &rpcpb.CreateVMResponse{Error: `id "vm-1" already exists`},
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
	if got := rec.Header().Get("HX-Redirect"); got != "" {
		t.Errorf("HX-Redirect = %q, want none on error", got)
	}
}

func TestServer_CreateVM_WithNetworkAndFirewallRules(t *testing.T) {
	client := &fakeClient{
		createResp: &rpcpb.CreateVMResponse{Vm: &rpcpb.VMDefinition{Id: "vm-1", NetworkId: "net-1", IpAddress: "10.60.0.2"}},
	}
	s := newTestServer(t, client)

	form := url.Values{
		"id":           {"vm-1"},
		"network_id":   {"net-1"},
		"fw_direction": {"in", ""},
		"fw_action":    {"block", "pass"},
		"fw_protocol":  {"tcp", ""},
		"fw_port":      {"22", ""},
	}
	req := httptest.NewRequest(http.MethodPost, "/vms", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	vm := client.lastCreateReq.GetVm()
	if vm.GetNetworkId() != "net-1" {
		t.Errorf("forwarded vm.NetworkId = %q, want net-1", vm.GetNetworkId())
	}
	if len(vm.GetFirewallRules()) != 1 {
		t.Fatalf("forwarded vm.FirewallRules = %v, want exactly one (the blank second row skipped)", vm.GetFirewallRules())
	}
	rule := vm.GetFirewallRules()[0]
	if rule.GetDirection() != "in" || rule.GetAction() != "block" || rule.GetProtocol() != "tcp" || rule.GetPortRange() != "22" {
		t.Errorf("forwarded rule = %+v, want direction=in action=block protocol=tcp port=22", rule)
	}
}

func TestServer_NewVMPage_ShowsNetworksAndFirewallRuleForm(t *testing.T) {
	client := &fakeClient{listNetworksResp: &rpcpb.ListNetworksResponse{
		Networks: []*rpcpb.NetworkDefinition{{Id: "net-1", Name: "prod", Subnet: "10.60.0.0/24"}},
	}}
	s := newTestServer(t, client)

	req := httptest.NewRequest(http.MethodGet, "/vms/new", nil)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	body := rec.Body.String()
	if !strings.Contains(body, "net-1") {
		t.Errorf("new VM page missing the known network in its picker, got: %s", body)
	}
	if !strings.Contains(body, `id="firewall-rule-rows"`) {
		t.Errorf("new VM page missing the firewall rules table, got: %s", body)
	}
}

func TestServer_NetworksPage(t *testing.T) {
	client := &fakeClient{listNetworksResp: &rpcpb.ListNetworksResponse{
		Networks: []*rpcpb.NetworkDefinition{{Id: "net-1", Name: "prod", VlanId: 100, Subnet: "10.60.0.0/24"}},
	}}
	s := newTestServer(t, client)

	req := httptest.NewRequest(http.MethodGet, "/networks", nil)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "net-1") || !strings.Contains(body, "10.60.0.0/24") || !strings.Contains(body, "100") {
		t.Errorf("networks page missing expected network row, got: %s", body)
	}
}

func TestServer_NetworksPage_ColorsBridgeStatus(t *testing.T) {
	client := &fakeClient{listNetworksResp: &rpcpb.ListNetworksResponse{
		Networks: []*rpcpb.NetworkDefinition{
			{Id: "net-up", Subnet: "10.60.0.0/24", BridgeStatus: "up"},
			{Id: "net-down", Subnet: "10.61.0.0/24", BridgeStatus: "down"},
			{Id: "net-unknown", Subnet: "10.62.0.0/24", BridgeStatus: "unknown"},
		},
	}}
	s := newTestServer(t, client)

	req := httptest.NewRequest(http.MethodGet, "/networks", nil)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	body := rec.Body.String()
	if !strings.Contains(body, `<span class="success">up</span>`) {
		t.Errorf("networks page missing green 'up' status, got: %s", body)
	}
	if !strings.Contains(body, `<span class="error">down</span>`) {
		t.Errorf("networks page missing red 'down' status, got: %s", body)
	}
	if !strings.Contains(body, "<em>unknown</em>") {
		t.Errorf("networks page missing 'unknown' status, got: %s", body)
	}
}

func TestServer_NetworksPage_ListError(t *testing.T) {
	client := &fakeClient{listNetworksResp: &rpcpb.ListNetworksResponse{Error: "raftd unreachable"}}
	s := newTestServer(t, client)

	req := httptest.NewRequest(http.MethodGet, "/networks", nil)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if !strings.Contains(rec.Body.String(), "raftd unreachable") {
		t.Errorf("networks page missing error message, got: %s", rec.Body.String())
	}
}

func TestServer_CreateNetwork(t *testing.T) {
	client := &fakeClient{
		createNetworkResp: &rpcpb.CreateNetworkResponse{Network: &rpcpb.NetworkDefinition{Id: "net-1"}},
	}
	s := newTestServer(t, client)

	form := url.Values{"id": {"net-1"}, "name": {"prod"}, "vlan_id": {"100"}, "subnet": {"10.60.0.0/24"}}
	req := httptest.NewRequest(http.MethodPost, "/networks", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	req2 := client.lastCreateNetworkReq.GetNetwork()
	if req2.GetId() != "net-1" || req2.GetVlanId() != 100 || req2.GetSubnet() != "10.60.0.0/24" {
		t.Errorf("forwarded network = %+v, want id=net-1 vlan_id=100 subnet=10.60.0.0/24", req2)
	}
}

func TestServer_CreateNetwork_ErrorShowsInPanel(t *testing.T) {
	client := &fakeClient{createNetworkResp: &rpcpb.CreateNetworkResponse{Error: `id "net-1" already exists`}}
	s := newTestServer(t, client)

	form := url.Values{"id": {"net-1"}, "subnet": {"10.60.0.0/24"}}
	req := httptest.NewRequest(http.MethodPost, "/networks", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if !strings.Contains(rec.Body.String(), "already exists") {
		t.Errorf("response missing error message, got: %s", rec.Body.String())
	}
}

func TestServer_DeleteNetwork(t *testing.T) {
	client := &fakeClient{deleteNetworkResp: &rpcpb.DeleteNetworkResponse{Network: &rpcpb.NetworkDefinition{Id: "net-1"}}}
	s := newTestServer(t, client)

	req := httptest.NewRequest(http.MethodDelete, "/networks/net-1", nil)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if client.lastDeleteNetworkReq.GetId() != "net-1" {
		t.Errorf("forwarded delete id = %q, want net-1", client.lastDeleteNetworkReq.GetId())
	}
}

func TestServer_DeleteNetwork_ErrorShowsInPanel(t *testing.T) {
	client := &fakeClient{deleteNetworkResp: &rpcpb.DeleteNetworkResponse{Error: "still referenced by VM vm-1"}}
	s := newTestServer(t, client)

	req := httptest.NewRequest(http.MethodDelete, "/networks/net-1", nil)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if !strings.Contains(rec.Body.String(), "still referenced") {
		t.Errorf("response missing error message, got: %s", rec.Body.String())
	}
}

func TestServer_JailsPage(t *testing.T) {
	client := &fakeClient{listJailsResp: &rpcpb.ListJailsResponse{
		Jails: []*rpcpb.JailDefinition{{Id: "jail-1", Name: "web-1", Hostname: "web-1.local", NodeId: "node-a"}},
	}}
	s := newTestServer(t, client)

	req := httptest.NewRequest(http.MethodGet, "/jails", nil)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "jail-1") || !strings.Contains(body, "web-1.local") || !strings.Contains(body, "node-a") {
		t.Errorf("jails page missing expected jail row, got: %s", body)
	}
}

// TestServer_NewJailPage_NodeIDIsADropdownOfKnownNodes guards against a
// real inconsistency: the jail create form's Node ID field was a plain
// text input while the VM create form's own equivalent already used a
// dropdown of known cluster members (see handleNewVMForm) - easy to
// leave blank or mistype, silently creating a jail record no node will
// ever reconcile.
func TestServer_NewJailPage_NodeIDIsADropdownOfKnownNodes(t *testing.T) {
	client := &fakeClient{
		statusResp: &rpcpb.StatusResponse{KnownNodeIds: []string{"apiarium", "freebsd-apiary"}},
	}
	s := newTestServer(t, client)

	req := httptest.NewRequest(http.MethodGet, "/jails/new", nil)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	body := rec.Body.String()
	if !strings.Contains(body, `<select name="node_id">`) {
		t.Errorf("new jail page node_id is not a <select>, got: %s", body)
	}
	if !strings.Contains(body, `<option value="apiarium">apiarium</option>`) {
		t.Errorf("new jail page node_id dropdown missing known node apiarium, got: %s", body)
	}
}

func TestServer_JailsPage_ListError(t *testing.T) {
	client := &fakeClient{listJailsResp: &rpcpb.ListJailsResponse{Error: "raftd unreachable"}}
	s := newTestServer(t, client)

	req := httptest.NewRequest(http.MethodGet, "/jails", nil)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if !strings.Contains(rec.Body.String(), "raftd unreachable") {
		t.Errorf("jails page missing error message, got: %s", rec.Body.String())
	}
}

func TestServer_CreateJail(t *testing.T) {
	client := &fakeClient{
		createJailResp: &rpcpb.CreateJailResponse{Jail: &rpcpb.JailDefinition{Id: "jail-1"}},
	}
	s := newTestServer(t, client)

	form := url.Values{"id": {"jail-1"}, "name": {"web-1"}, "hostname": {"web-1.local"}, "node_id": {"node-a"}}
	req := httptest.NewRequest(http.MethodPost, "/jails", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	got := client.lastCreateJailReq.GetJail()
	if got.GetId() != "jail-1" || got.GetHostname() != "web-1.local" || got.GetNodeId() != "node-a" {
		t.Errorf("forwarded jail = %+v, want id=jail-1 hostname=web-1.local node_id=node-a", got)
	}
	// On its own page now (see new_jail.html) with no jail table to
	// refresh - success is reported via HX-Redirect to the jails page,
	// not a re-rendered fragment.
	if got := rec.Header().Get("HX-Redirect"); got != "/jails" {
		t.Errorf("HX-Redirect = %q, want /jails", got)
	}
}

func TestServer_CreateJail_ErrorRendersDirectlyNoRedirect(t *testing.T) {
	client := &fakeClient{createJailResp: &rpcpb.CreateJailResponse{Error: `id "jail-1" already exists`}}
	s := newTestServer(t, client)

	form := url.Values{"id": {"jail-1"}}
	req := httptest.NewRequest(http.MethodPost, "/jails", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if !strings.Contains(rec.Body.String(), "already exists") {
		t.Errorf("response missing error message, got: %s", rec.Body.String())
	}
	if got := rec.Header().Get("HX-Redirect"); got != "" {
		t.Errorf("HX-Redirect = %q, want none on error", got)
	}
}

func TestServer_DeleteJail(t *testing.T) {
	client := &fakeClient{deleteJailResp: &rpcpb.DeleteJailResponse{Jail: &rpcpb.JailDefinition{Id: "jail-1"}}}
	s := newTestServer(t, client)

	req := httptest.NewRequest(http.MethodDelete, "/jails/jail-1", nil)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if client.lastDeleteJailReq.GetId() != "jail-1" {
		t.Errorf("forwarded delete id = %q, want jail-1", client.lastDeleteJailReq.GetId())
	}
}

func TestServer_DeleteJail_ErrorShowsInPanel(t *testing.T) {
	client := &fakeClient{deleteJailResp: &rpcpb.DeleteJailResponse{Error: "some jail error"}}
	s := newTestServer(t, client)

	req := httptest.NewRequest(http.MethodDelete, "/jails/jail-1", nil)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if !strings.Contains(rec.Body.String(), "some jail error") {
		t.Errorf("response missing error message, got: %s", rec.Body.String())
	}
}

func TestServer_APIKeysPage(t *testing.T) {
	client := &fakeClient{listAPIKeysResp: &rpcpb.ListAPIKeysResponse{
		Keys: []*rpcpb.APIKeyInfo{{Id: "key-1", Name: "terraform", Role: "operator", CreatedUnix: 1700000000}},
	}}
	s := newTestServer(t, client)

	req := httptest.NewRequest(http.MethodGet, "/apikeys", nil)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "key-1") || !strings.Contains(body, "terraform") || !strings.Contains(body, "operator") {
		t.Errorf("apikeys page missing expected key row (with role), got: %s", body)
	}
}

func TestServer_APIKeysPage_ListError(t *testing.T) {
	client := &fakeClient{listAPIKeysResp: &rpcpb.ListAPIKeysResponse{Error: "raftd unreachable"}}
	s := newTestServer(t, client)

	req := httptest.NewRequest(http.MethodGet, "/apikeys", nil)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if !strings.Contains(rec.Body.String(), "raftd unreachable") {
		t.Errorf("apikeys page missing error message, got: %s", rec.Body.String())
	}
}

func TestServer_CreateAPIKey_ShowsRawKeyOnce(t *testing.T) {
	client := &fakeClient{
		createAPIKeyResp: &rpcpb.CreateAPIKeyResponse{
			Key:    &rpcpb.APIKeyInfo{Id: "key-1", Name: "terraform"},
			RawKey: "apk_supersecretvalue",
		},
	}
	s := newTestServer(t, client)

	form := url.Values{"name": {"terraform"}, "role": {"operator"}}
	req := httptest.NewRequest(http.MethodPost, "/apikeys", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if client.lastCreateAPIKeyReq.GetName() != "terraform" {
		t.Errorf("forwarded name = %q, want terraform", client.lastCreateAPIKeyReq.GetName())
	}
	if client.lastCreateAPIKeyReq.GetRole() != "operator" {
		t.Errorf("forwarded role = %q, want operator", client.lastCreateAPIKeyReq.GetRole())
	}
	if !strings.Contains(rec.Body.String(), "apk_supersecretvalue") {
		t.Errorf("create response missing the one-time raw key, got: %s", rec.Body.String())
	}
}

func TestServer_CreateAPIKey_ErrorShowsInPanel(t *testing.T) {
	client := &fakeClient{createAPIKeyResp: &rpcpb.CreateAPIKeyResponse{Error: "generating API key: boom"}}
	s := newTestServer(t, client)

	form := url.Values{"name": {"terraform"}}
	req := httptest.NewRequest(http.MethodPost, "/apikeys", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if !strings.Contains(rec.Body.String(), "boom") {
		t.Errorf("response missing error message, got: %s", rec.Body.String())
	}
}

func TestServer_APIKeysPage_ListNeverShowsRawKeyAgain(t *testing.T) {
	client := &fakeClient{listAPIKeysResp: &rpcpb.ListAPIKeysResponse{
		Keys: []*rpcpb.APIKeyInfo{{Id: "key-1", Name: "terraform", CreatedUnix: 1700000000}},
	}}
	s := newTestServer(t, client)

	req := httptest.NewRequest(http.MethodGet, "/apikeys", nil)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if strings.Contains(rec.Body.String(), "apk_") {
		t.Errorf("apikeys list render leaked raw key material, got: %s", rec.Body.String())
	}
}

func TestServer_RevokeAPIKey(t *testing.T) {
	client := &fakeClient{revokeAPIKeyResp: &rpcpb.RevokeAPIKeyResponse{}}
	s := newTestServer(t, client)

	req := httptest.NewRequest(http.MethodDelete, "/apikeys/key-1", nil)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if client.lastRevokeAPIKeyReq.GetId() != "key-1" {
		t.Errorf("forwarded revoke id = %q, want key-1", client.lastRevokeAPIKeyReq.GetId())
	}
}

func TestServer_RevokeAPIKey_ErrorShowsInPanel(t *testing.T) {
	client := &fakeClient{revokeAPIKeyResp: &rpcpb.RevokeAPIKeyResponse{Error: `id "key-1" does not exist`}}
	s := newTestServer(t, client)

	req := httptest.NewRequest(http.MethodDelete, "/apikeys/key-1", nil)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if !strings.Contains(rec.Body.String(), "does not exist") {
		t.Errorf("response missing error message, got: %s", rec.Body.String())
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

func TestServer_ListISOs_ShowsStoredImages(t *testing.T) {
	client := &fakeClient{listISOsResp: &rpcpb.ListISOsResponse{
		Isos: []*rpcpb.ISOInfo{{Name: "debian.iso", SizeBytes: 12345, Sha256: "abc123"}},
	}}
	s := newTestServer(t, client)

	req := httptest.NewRequest(http.MethodGet, "/isos", nil)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	body := rec.Body.String()
	if !strings.Contains(body, "debian.iso") || !strings.Contains(body, "abc123") {
		t.Errorf("response missing expected ISO data, got: %s", body)
	}
}

// buildUploadRequest constructs a multipart/form-data POST /isos request
// with expected_sha256 encoded before file - the order handleUploadISO
// requires, since it streams parts as they arrive rather than buffering
// the whole form first.
func buildUploadRequest(t *testing.T, hash, filename, contents string) *http.Request {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	if err := mw.WriteField("expected_sha256", hash); err != nil {
		t.Fatalf("WriteField: %v", err)
	}
	fw, err := mw.CreateFormFile("file", filename)
	if err != nil {
		t.Fatalf("CreateFormFile: %v", err)
	}
	if _, err := fw.Write([]byte(contents)); err != nil {
		t.Fatalf("writing file part: %v", err)
	}
	if err := mw.Close(); err != nil {
		t.Fatalf("closing multipart writer: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/isos", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	return req
}

func TestServer_UploadISO_StreamsMetadataThenChunksInOrder(t *testing.T) {
	client := &fakeClient{uploadStream: &fakeUploadClientStream{
		resp: &rpcpb.UploadISOResponse{Name: "test.iso", SizeBytes: 4, Sha256: "deadbeef"},
	}}
	s := newTestServer(t, client)

	req := buildUploadRequest(t, "deadbeef", "test.iso", "data")
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	sent := client.uploadStream.sent
	if len(sent) < 2 {
		t.Fatalf("stream got %d messages, want at least metadata + 1 chunk", len(sent))
	}
	meta := sent[0].GetMetadata()
	if meta == nil {
		t.Fatalf("first message = %+v, want metadata", sent[0])
	}
	if meta.GetName() != "test.iso" || meta.GetExpectedSha256() != "deadbeef" {
		t.Errorf("metadata = %+v, want name=test.iso hash=deadbeef", meta)
	}
	var gotData []byte
	for _, m := range sent[1:] {
		gotData = append(gotData, m.GetChunk()...)
	}
	if string(gotData) != "data" {
		t.Errorf("chunk data = %q, want %q", gotData, "data")
	}
	if !strings.Contains(rec.Body.String(), `id="iso-error" class="error"></div>`) {
		t.Errorf("expected an empty iso-error div on success, got: %s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "Uploaded test.iso successfully") {
		t.Errorf("expected an explicit success confirmation, got: %s", rec.Body.String())
	}
}

func TestServer_UploadISO_HashMismatchShowsErrorNotInline(t *testing.T) {
	client := &fakeClient{uploadStream: &fakeUploadClientStream{
		resp: &rpcpb.UploadISOResponse{Error: `sha256 mismatch: got aaa, want bbb`},
	}}
	s := newTestServer(t, client)

	req := buildUploadRequest(t, "bbb", "test.iso", "data")
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	body := rec.Body.String()
	if !strings.Contains(body, "sha256 mismatch") {
		t.Errorf("response missing hash-mismatch error, got: %s", body)
	}
	if !strings.Contains(body, `id="iso-error"`) {
		t.Errorf("error should render inside #iso-error, got: %s", body)
	}
	if strings.Contains(body, "Uploaded") {
		t.Errorf("a failed upload should not show a success confirmation, got: %s", body)
	}
}

func TestServer_DeleteISO(t *testing.T) {
	client := &fakeClient{listISOsResp: &rpcpb.ListISOsResponse{}}
	s := newTestServer(t, client)

	req := httptest.NewRequest(http.MethodDelete, "/isos/old.iso", nil)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if client.lastDeleteISOReq.GetName() != "old.iso" {
		t.Errorf("forwarded name = %q, want old.iso", client.lastDeleteISOReq.GetName())
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

func TestServer_NoAuthConfigured_AllPagesReachableWithoutLogin(t *testing.T) {
	s := newTestServer(t, &fakeClient{})

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (no login configured, no gate)", rec.Code)
	}
}

func TestServer_LoginPage_RedirectsToHomeWhenAuthDisabled(t *testing.T) {
	s := newTestServer(t, &fakeClient{})

	req := httptest.NewRequest(http.MethodGet, "/login", nil)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/" {
		t.Errorf("GET /login with auth disabled = %d %q, want 302 to /", rec.Code, rec.Header().Get("Location"))
	}
}

func TestServer_AuthEnabled_UnauthenticatedRequestRedirectsToLogin(t *testing.T) {
	s := newTestServerWithAuth(t, &fakeClient{}, "admin", "secret")

	req := httptest.NewRequest(http.MethodGet, "/vms", nil)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if rec.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302 to /login", rec.Code)
	}
	loc := rec.Header().Get("Location")
	if !strings.HasPrefix(loc, "/login") {
		t.Errorf("Location = %q, want a redirect to /login", loc)
	}
	if !strings.Contains(loc, url.QueryEscape("/vms")) {
		t.Errorf("Location = %q, want it to carry the original path as next", loc)
	}
}

func TestServer_AuthEnabled_HTMXRequestGetsHXRedirectNotBare302(t *testing.T) {
	s := newTestServerWithAuth(t, &fakeClient{}, "admin", "secret")

	req := httptest.NewRequest(http.MethodGet, "/vms/rows", nil)
	req.Header.Set("HX-Request", "true")
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 (HX-Redirect handles navigation, not a 302 status)", rec.Code)
	}
	if got := rec.Header().Get("HX-Redirect"); !strings.HasPrefix(got, "/login") {
		t.Errorf("HX-Redirect = %q, want a redirect to /login", got)
	}
}

func TestServer_Login_WrongCredentialsShowsErrorNoSession(t *testing.T) {
	s := newTestServerWithAuth(t, &fakeClient{}, "admin", "secret")

	form := url.Values{"username": {"admin"}, "password": {"wrong"}}
	req := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (re-render with error, not a redirect)", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "invalid username or password") {
		t.Errorf("response missing login error, got: %s", rec.Body.String())
	}
	for _, c := range rec.Result().Cookies() {
		if c.Name == sessionCookieName {
			t.Errorf("a session cookie should not be set on a failed login")
		}
	}
}

func TestServer_Login_CorrectCredentialsGrantsSessionAndRedirects(t *testing.T) {
	s := newTestServerWithAuth(t, &fakeClient{}, "admin", "secret")

	form := url.Values{"username": {"admin"}, "password": {"secret"}}
	req := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/" {
		t.Fatalf("status/location = %d %q, want 302 to /", rec.Code, rec.Header().Get("Location"))
	}
	var token string
	for _, c := range rec.Result().Cookies() {
		if c.Name == sessionCookieName {
			token = c.Value
		}
	}
	if token == "" {
		t.Fatalf("no session cookie set on successful login")
	}

	// The session should now actually work for a subsequent request.
	req2 := httptest.NewRequest(http.MethodGet, "/vms", nil)
	req2.AddCookie(&http.Cookie{Name: sessionCookieName, Value: token})
	rec2 := httptest.NewRecorder()
	s.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Errorf("authenticated request status = %d, want 200", rec2.Code)
	}
}

func TestServer_Login_RedirectsToSafeNextURL(t *testing.T) {
	s := newTestServerWithAuth(t, &fakeClient{}, "admin", "secret")

	form := url.Values{"username": {"admin"}, "password": {"secret"}, "next": {"/images"}}
	req := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if got := rec.Header().Get("Location"); got != "/images" {
		t.Errorf("Location = %q, want /images (the requested next page)", got)
	}
}

func TestServer_Login_RejectsOpenRedirectNextURL(t *testing.T) {
	s := newTestServerWithAuth(t, &fakeClient{}, "admin", "secret")

	for _, next := range []string{"//evil.com", "https://evil.com", "http://evil.com/x"} {
		form := url.Values{"username": {"admin"}, "password": {"secret"}, "next": {next}}
		req := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, req)

		if got := rec.Header().Get("Location"); got != "/" {
			t.Errorf("next=%q: Location = %q, want / (unsafe next rejected)", next, got)
		}
	}
}

func TestServer_Logout_InvalidatesSession(t *testing.T) {
	s := newTestServerWithAuth(t, &fakeClient{}, "admin", "secret")
	token, err := s.sessions.Create("admin", manager.RoleAdmin)
	if err != nil {
		t.Fatalf("sessions.Create() error: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/logout", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: token})
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/login" {
		t.Fatalf("status/location = %d %q, want 302 to /login", rec.Code, rec.Header().Get("Location"))
	}
	if _, ok := s.sessions.Valid(token); ok {
		t.Errorf("session should be invalidated after logout")
	}

	// The old token must no longer grant access.
	req2 := httptest.NewRequest(http.MethodGet, "/vms", nil)
	req2.AddCookie(&http.Cookie{Name: sessionCookieName, Value: token})
	rec2 := httptest.NewRecorder()
	s.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusFound {
		t.Errorf("status after logout = %d, want 302 back to login", rec2.Code)
	}
}

func TestServer_AuthEnabled_NavShowsLogoutLink(t *testing.T) {
	s := newTestServerWithAuth(t, &fakeClient{}, "admin", "secret")
	token, _ := s.sessions.Create("admin", manager.RoleAdmin)

	req := httptest.NewRequest(http.MethodGet, "/vms", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: token})
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if !strings.Contains(rec.Body.String(), `action="/logout"`) {
		t.Errorf("nav should show a logout control when auth is enabled, got: %s", rec.Body.String())
	}
}

func TestServer_AuthDisabled_NavHasNoLogoutLink(t *testing.T) {
	s := newTestServer(t, &fakeClient{})

	req := httptest.NewRequest(http.MethodGet, "/vms", nil)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if strings.Contains(rec.Body.String(), `action="/logout"`) {
		t.Errorf("nav should not show a logout control when auth is disabled, got: %s", rec.Body.String())
	}
}

func TestServer_Login_UnmappedUserIsRejectedDespiteValidCredentials(t *testing.T) {
	s, err := NewServer(&fakeClient{}, fakeAuthenticator{user: "eve", pass: "secret"}, map[string]manager.Role{
		// "eve" deliberately absent - a valid PAM login for a real
		// account nobody has granted an Apiary role to.
		"admin": manager.RoleAdmin,
	})
	if err != nil {
		t.Fatalf("NewServer() error: %v", err)
	}

	form := url.Values{"username": {"eve"}, "password": {"secret"}}
	req := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (re-render with error, not a redirect)", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "no Apiary role is assigned") {
		t.Errorf("response missing no-role error, got: %s", rec.Body.String())
	}
	for _, c := range rec.Result().Cookies() {
		if c.Name == sessionCookieName {
			t.Errorf("a session cookie should not be set for an unmapped user")
		}
	}
}

func TestServer_RoleGate_ViewerCannotReachOperatorRoute(t *testing.T) {
	s, err := NewServer(&fakeClient{}, fakeAuthenticator{user: "carol", pass: "secret"}, map[string]manager.Role{
		"carol": manager.RoleViewer,
	})
	if err != nil {
		t.Fatalf("NewServer() error: %v", err)
	}
	token, _ := s.sessions.Create("carol", manager.RoleViewer)

	req := httptest.NewRequest(http.MethodGet, "/vms/new", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: token})
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403 for a Viewer reaching an Operator-only route", rec.Code)
	}
}

func TestServer_RoleGate_ViewerCanReachReadOnlyRoute(t *testing.T) {
	s, err := NewServer(&fakeClient{}, fakeAuthenticator{user: "carol", pass: "secret"}, map[string]manager.Role{
		"carol": manager.RoleViewer,
	})
	if err != nil {
		t.Fatalf("NewServer() error: %v", err)
	}
	token, _ := s.sessions.Create("carol", manager.RoleViewer)

	req := httptest.NewRequest(http.MethodGet, "/vms", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: token})
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 for a Viewer reaching a read-only route", rec.Code)
	}
}

func TestServer_RoleGate_OperatorCannotReachAdminRoute(t *testing.T) {
	s, err := NewServer(&fakeClient{}, fakeAuthenticator{user: "bob", pass: "secret"}, map[string]manager.Role{
		"bob": manager.RoleOperator,
	})
	if err != nil {
		t.Fatalf("NewServer() error: %v", err)
	}
	token, _ := s.sessions.Create("bob", manager.RoleOperator)

	req := httptest.NewRequest(http.MethodGet, "/apikeys", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: token})
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403 for an Operator reaching an Admin-only route", rec.Code)
	}
}

func TestServer_RoleGate_OperatorCanReachOperatorRoute(t *testing.T) {
	s, err := NewServer(&fakeClient{}, fakeAuthenticator{user: "bob", pass: "secret"}, map[string]manager.Role{
		"bob": manager.RoleOperator,
	})
	if err != nil {
		t.Fatalf("NewServer() error: %v", err)
	}
	token, _ := s.sessions.Create("bob", manager.RoleOperator)

	req := httptest.NewRequest(http.MethodGet, "/vms/new", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: token})
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 for an Operator reaching an Operator route", rec.Code)
	}
}

func TestServer_Nav_ShowsUsernameAndRole(t *testing.T) {
	s := newTestServerWithAuth(t, &fakeClient{}, "admin", "secret")
	token, _ := s.sessions.Create("admin", manager.RoleAdmin)

	req := httptest.NewRequest(http.MethodGet, "/vms", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: token})
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if !strings.Contains(rec.Body.String(), "admin") || !strings.Contains(rec.Body.String(), "admin)") {
		t.Errorf("nav should show the logged-in username and role, got: %s", rec.Body.String())
	}
}

func TestServer_PostLogin_NoOpWhenAuthDisabled(t *testing.T) {
	s := newTestServer(t, &fakeClient{})

	form := url.Values{"username": {"anyone"}, "password": {"anything"}}
	req := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/" {
		t.Errorf("POST /login with auth disabled = %d %q, want 302 to / (not a crash)", rec.Code, rec.Header().Get("Location"))
	}
}

func TestServer_Login_LocksOutAfterRepeatedFailures(t *testing.T) {
	s := newTestServerWithAuth(t, &fakeClient{}, "admin", "secret")

	form := url.Values{"username": {"admin"}, "password": {"wrong"}}
	for i := 0; i < defaultMaxFailedAttempts; i++ {
		req := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("attempt %d: status = %d, want 200", i+1, rec.Code)
		}
	}

	// The account is now locked - even the CORRECT password must be
	// rejected until the lockout clears.
	correctForm := url.Values{"username": {"admin"}, "password": {"secret"}}
	req := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(correctForm.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (re-render with lockout error, not a redirect)", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "too many failed attempts") {
		t.Errorf("response missing lockout error, got: %s", rec.Body.String())
	}
	for _, c := range rec.Result().Cookies() {
		if c.Name == sessionCookieName {
			t.Errorf("a session cookie should not be set while the account is locked out")
		}
	}
}

func TestServer_Login_SuccessDoesNotCountAsFailureTowardLockout(t *testing.T) {
	s := newTestServerWithAuth(t, &fakeClient{}, "admin", "secret")

	// One failure, then a success, repeated well past the failure
	// threshold - the success should reset the count each time, so the
	// account never locks.
	wrongForm := url.Values{"username": {"admin"}, "password": {"wrong"}}
	correctForm := url.Values{"username": {"admin"}, "password": {"secret"}}
	for i := 0; i < defaultMaxFailedAttempts+2; i++ {
		req := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(wrongForm.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		s.ServeHTTP(httptest.NewRecorder(), req)

		req2 := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(correctForm.Encode()))
		req2.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		rec2 := httptest.NewRecorder()
		s.ServeHTTP(rec2, req2)
		if rec2.Code != http.StatusFound {
			t.Fatalf("iteration %d: correct-password status = %d, want 302 (never locked out)", i, rec2.Code)
		}
	}
}
