package restshim

import (
	"bytes"
	"context"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"

	rpcpb "github.com/glenjbarber/apiary/api/rpc"
)

// fakeClient implements rpcpb.ManagerServiceClient with canned responses,
// so Server's HTTP translation logic can be tested without any real
// gRPC connection or managerd process.
type fakeClient struct {
	statusResp *rpcpb.StatusResponse
	statusErr  error

	createResp *rpcpb.CreateVMResponse
	createErr  error

	updateResp *rpcpb.UpdateVMResponse

	deleteResp *rpcpb.DeleteVMResponse

	getResp *rpcpb.GetVMResponse

	listResp *rpcpb.ListVMsResponse

	lastCreateReq *rpcpb.CreateVMRequest
	lastUpdateReq *rpcpb.UpdateVMRequest
	lastGetReq    *rpcpb.GetVMRequest
	lastDeleteReq *rpcpb.DeleteVMRequest

	lastStatusCtx context.Context

	createJailResp *rpcpb.CreateJailResponse
	updateJailResp *rpcpb.UpdateJailResponse
	deleteJailResp *rpcpb.DeleteJailResponse
	getJailResp    *rpcpb.GetJailResponse
	listJailsResp  *rpcpb.ListJailsResponse

	lastCreateJailReq *rpcpb.CreateJailRequest
	lastUpdateJailReq *rpcpb.UpdateJailRequest
	lastGetJailReq    *rpcpb.GetJailRequest
	lastDeleteJailReq *rpcpb.DeleteJailRequest

	migrateVMResp      *rpcpb.MigrateVMResponse
	migrateJailResp    *rpcpb.MigrateJailResponse
	lastMigrateVMReq   *rpcpb.MigrateVMRequest
	lastMigrateJailReq *rpcpb.MigrateJailRequest

	createNetworkResp *rpcpb.CreateNetworkResponse
	deleteNetworkResp *rpcpb.DeleteNetworkResponse
	listNetworksResp  *rpcpb.ListNetworksResponse

	lastCreateNetworkReq *rpcpb.CreateNetworkRequest
	lastDeleteNetworkReq *rpcpb.DeleteNetworkRequest

	uploadStream *fakeUploadClientStream
	uploadErr    error
}

// fakeUploadClientStream is a fake grpc.ClientStreamingClient for
// UploadISO - records every message the handler sends and returns a
// canned final response, without any real gRPC connection. Mirrors
// internal/frontend's identical test helper.
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

func (f *fakeClient) Status(ctx context.Context, _ *rpcpb.StatusRequest, _ ...grpc.CallOption) (*rpcpb.StatusResponse, error) {
	f.lastStatusCtx = ctx
	return f.statusResp, f.statusErr
}

func (f *fakeClient) CreateVM(_ context.Context, in *rpcpb.CreateVMRequest, _ ...grpc.CallOption) (*rpcpb.CreateVMResponse, error) {
	f.lastCreateReq = in
	return f.createResp, f.createErr
}

func (f *fakeClient) UpdateVM(_ context.Context, in *rpcpb.UpdateVMRequest, _ ...grpc.CallOption) (*rpcpb.UpdateVMResponse, error) {
	f.lastUpdateReq = in
	return f.updateResp, nil
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

func (f *fakeClient) MigrateVM(_ context.Context, in *rpcpb.MigrateVMRequest, _ ...grpc.CallOption) (*rpcpb.MigrateVMResponse, error) {
	f.lastMigrateVMReq = in
	if f.migrateVMResp != nil {
		return f.migrateVMResp, nil
	}
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

func (f *fakeClient) MigrateJail(_ context.Context, in *rpcpb.MigrateJailRequest, _ ...grpc.CallOption) (*rpcpb.MigrateJailResponse, error) {
	f.lastMigrateJailReq = in
	if f.migrateJailResp != nil {
		return f.migrateJailResp, nil
	}
	return &rpcpb.MigrateJailResponse{}, nil
}

func (f *fakeClient) GetVM(_ context.Context, in *rpcpb.GetVMRequest, _ ...grpc.CallOption) (*rpcpb.GetVMResponse, error) {
	f.lastGetReq = in
	return f.getResp, nil
}

func (f *fakeClient) ListVMs(context.Context, *rpcpb.ListVMsRequest, ...grpc.CallOption) (*rpcpb.ListVMsResponse, error) {
	return f.listResp, nil
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
	return &rpcpb.ListISOsResponse{}, nil
}

func (f *fakeClient) DeleteISO(context.Context, *rpcpb.DeleteISORequest, ...grpc.CallOption) (*rpcpb.DeleteISOResponse, error) {
	return &rpcpb.DeleteISOResponse{}, nil
}

func (f *fakeClient) HostStats(context.Context, *rpcpb.HostStatsRequest, ...grpc.CallOption) (*rpcpb.HostStatsResponse, error) {
	return &rpcpb.HostStatsResponse{}, nil
}

func (f *fakeClient) GetVMConsole(context.Context, *rpcpb.GetVMConsoleRequest, ...grpc.CallOption) (*rpcpb.GetVMConsoleResponse, error) {
	return &rpcpb.GetVMConsoleResponse{}, nil
}

func (f *fakeClient) GetVMSerialLog(context.Context, *rpcpb.GetVMSerialLogRequest, ...grpc.CallOption) (*rpcpb.GetVMSerialLogResponse, error) {
	return &rpcpb.GetVMSerialLogResponse{}, nil
}

func (f *fakeClient) CreateNetwork(_ context.Context, in *rpcpb.CreateNetworkRequest, _ ...grpc.CallOption) (*rpcpb.CreateNetworkResponse, error) {
	f.lastCreateNetworkReq = in
	if f.createNetworkResp != nil {
		return f.createNetworkResp, nil
	}
	return &rpcpb.CreateNetworkResponse{}, nil
}

func (f *fakeClient) ListNetworks(context.Context, *rpcpb.ListNetworksRequest, ...grpc.CallOption) (*rpcpb.ListNetworksResponse, error) {
	if f.listNetworksResp != nil {
		return f.listNetworksResp, nil
	}
	return &rpcpb.ListNetworksResponse{}, nil
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

func (f *fakeClient) UpdateJail(_ context.Context, in *rpcpb.UpdateJailRequest, _ ...grpc.CallOption) (*rpcpb.UpdateJailResponse, error) {
	f.lastUpdateJailReq = in
	if f.updateJailResp != nil {
		return f.updateJailResp, nil
	}
	return &rpcpb.UpdateJailResponse{}, nil
}

func (f *fakeClient) DeleteJail(_ context.Context, in *rpcpb.DeleteJailRequest, _ ...grpc.CallOption) (*rpcpb.DeleteJailResponse, error) {
	f.lastDeleteJailReq = in
	if f.deleteJailResp != nil {
		return f.deleteJailResp, nil
	}
	return &rpcpb.DeleteJailResponse{}, nil
}

func (f *fakeClient) GetJail(_ context.Context, in *rpcpb.GetJailRequest, _ ...grpc.CallOption) (*rpcpb.GetJailResponse, error) {
	f.lastGetJailReq = in
	if f.getJailResp != nil {
		return f.getJailResp, nil
	}
	return &rpcpb.GetJailResponse{}, nil
}

func (f *fakeClient) ListJails(context.Context, *rpcpb.ListJailsRequest, ...grpc.CallOption) (*rpcpb.ListJailsResponse, error) {
	if f.listJailsResp != nil {
		return f.listJailsResp, nil
	}
	return &rpcpb.ListJailsResponse{}, nil
}

func (f *fakeClient) CreateAPIKey(context.Context, *rpcpb.CreateAPIKeyRequest, ...grpc.CallOption) (*rpcpb.CreateAPIKeyResponse, error) {
	return &rpcpb.CreateAPIKeyResponse{}, nil
}

func (f *fakeClient) ListAPIKeys(context.Context, *rpcpb.ListAPIKeysRequest, ...grpc.CallOption) (*rpcpb.ListAPIKeysResponse, error) {
	return &rpcpb.ListAPIKeysResponse{}, nil
}

func (f *fakeClient) RevokeAPIKey(context.Context, *rpcpb.RevokeAPIKeyRequest, ...grpc.CallOption) (*rpcpb.RevokeAPIKeyResponse, error) {
	return &rpcpb.RevokeAPIKeyResponse{}, nil
}

var _ rpcpb.ManagerServiceClient = (*fakeClient)(nil)

func doRequest(t *testing.T, s *Server, method, path string, body interface{}) *httptest.ResponseRecorder {
	t.Helper()
	var bodyReader *bytes.Buffer
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshaling request body: %v", err)
		}
		bodyReader = bytes.NewBuffer(data)
	} else {
		bodyReader = bytes.NewBuffer(nil)
	}

	req := httptest.NewRequest(method, path, bodyReader)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	return rec
}

func TestServer_Status(t *testing.T) {
	client := &fakeClient{statusResp: &rpcpb.StatusResponse{
		ManagerNodeId: "manager-1",
		RaftReachable: true,
		RaftIsLeader:  true,
	}}
	s := NewServer(client)

	rec := doRequest(t, s, http.MethodGet, "/v1/status", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	var got map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if got["manager_node_id"] != "manager-1" {
		t.Errorf("manager_node_id = %v, want manager-1", got["manager_node_id"])
	}
	if got["raft_is_leader"] != true {
		t.Errorf("raft_is_leader = %v, want true", got["raft_is_leader"])
	}
}

func TestServer_ForwardsAuthorizationHeaderToManagerd(t *testing.T) {
	client := &fakeClient{statusResp: &rpcpb.StatusResponse{}}
	s := NewServer(client)

	req := httptest.NewRequest(http.MethodGet, "/v1/status", nil)
	req.Header.Set("Authorization", "Bearer apk_test123")
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	md, ok := metadata.FromOutgoingContext(client.lastStatusCtx)
	if !ok {
		t.Fatalf("no outgoing gRPC metadata attached to the context passed to managerd")
	}
	got := md.Get("authorization")
	if len(got) != 1 || got[0] != "Bearer apk_test123" {
		t.Errorf("forwarded authorization metadata = %v, want [Bearer apk_test123]", got)
	}
}

func TestServer_NoAuthorizationHeaderForwardsNothing(t *testing.T) {
	client := &fakeClient{statusResp: &rpcpb.StatusResponse{}}
	s := NewServer(client)

	rec := doRequest(t, s, http.MethodGet, "/v1/status", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if md, ok := metadata.FromOutgoingContext(client.lastStatusCtx); ok && len(md.Get("authorization")) != 0 {
		t.Errorf("forwarded authorization metadata = %v, want none when no header was sent", md.Get("authorization"))
	}
}

func TestServer_CreateVM(t *testing.T) {
	client := &fakeClient{createResp: &rpcpb.CreateVMResponse{
		Vm: &rpcpb.VMDefinition{Id: "vm-1", Name: "web-1", Vcpus: 2, DesiredState: rpcpb.VMState_VM_STATE_RUNNING},
	}}
	s := NewServer(client)

	rec := doRequest(t, s, http.MethodPost, "/v1/vms", vm{ID: "vm-1", Name: "web-1", VCPUs: 2, DesiredState: "running"})
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%s", rec.Code, rec.Body.String())
	}

	if client.lastCreateReq.GetVm().GetName() != "web-1" {
		t.Errorf("request forwarded vm.Name = %q, want web-1", client.lastCreateReq.GetVm().GetName())
	}

	var got vm
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if got.DesiredState != "running" {
		t.Errorf("response DesiredState = %q, want running", got.DesiredState)
	}
}

func TestServer_CreateVM_InvalidJSON(t *testing.T) {
	s := NewServer(&fakeClient{})
	req := httptest.NewRequest(http.MethodPost, "/v1/vms", bytes.NewBufferString("not json"))
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

func TestServer_CreateVM_ApplicationErrorIsBadRequest(t *testing.T) {
	client := &fakeClient{createResp: &rpcpb.CreateVMResponse{Error: `CreateVM: id "vm-1" already exists`}}
	s := NewServer(client)

	rec := doRequest(t, s, http.MethodPost, "/v1/vms", vm{ID: "vm-1"})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}

	var body errorBody
	json.Unmarshal(rec.Body.Bytes(), &body)
	if body.Error == "" {
		t.Errorf("error body is empty, want the rejection message")
	}
}

func TestServer_CreateVM_NotLeaderIsServiceUnavailable(t *testing.T) {
	client := &fakeClient{createResp: &rpcpb.CreateVMResponse{
		Error:      "raft: this node is not the leader",
		LeaderHint: "10.0.0.2:17600",
	}}
	s := NewServer(client)

	rec := doRequest(t, s, http.MethodPost, "/v1/vms", vm{ID: "vm-1"})
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503; body=%s", rec.Code, rec.Body.String())
	}

	var body errorBody
	json.Unmarshal(rec.Body.Bytes(), &body)
	if body.LeaderHint != "10.0.0.2:17600" {
		t.Errorf("LeaderHint = %q, want 10.0.0.2:17600", body.LeaderHint)
	}
}

func TestServer_CreateVM_TransportErrorIsBadGateway(t *testing.T) {
	client := &fakeClient{createErr: context.DeadlineExceeded}
	s := NewServer(client)

	rec := doRequest(t, s, http.MethodPost, "/v1/vms", vm{ID: "vm-1"})
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502; body=%s", rec.Code, rec.Body.String())
	}
}

func TestServer_GetVM_Found(t *testing.T) {
	client := &fakeClient{getResp: &rpcpb.GetVMResponse{
		Found: true,
		Vm:    &rpcpb.VMDefinition{Id: "vm-1", Name: "web-1"},
	}}
	s := NewServer(client)

	rec := doRequest(t, s, http.MethodGet, "/v1/vms/vm-1", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if client.lastGetReq.GetId() != "vm-1" {
		t.Errorf("request forwarded id = %q, want vm-1", client.lastGetReq.GetId())
	}
}

func TestServer_GetVM_NotFound(t *testing.T) {
	client := &fakeClient{getResp: &rpcpb.GetVMResponse{Found: false}}
	s := NewServer(client)

	rec := doRequest(t, s, http.MethodGet, "/v1/vms/vm-missing", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
}

func TestServer_UpdateVM_PathIDOverridesBody(t *testing.T) {
	client := &fakeClient{updateResp: &rpcpb.UpdateVMResponse{Vm: &rpcpb.VMDefinition{Id: "vm-1"}}}
	s := NewServer(client)

	// Body claims a different id than the URL path - the path must win.
	rec := doRequest(t, s, http.MethodPut, "/v1/vms/vm-1", vm{ID: "wrong-id", Name: "renamed"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if client.lastUpdateReq.GetVm().GetId() != "vm-1" {
		t.Errorf("forwarded vm.Id = %q, want vm-1 (from the URL path)", client.lastUpdateReq.GetVm().GetId())
	}
}

func TestServer_DeleteVM(t *testing.T) {
	client := &fakeClient{deleteResp: &rpcpb.DeleteVMResponse{Vm: &rpcpb.VMDefinition{Id: "vm-1", Name: "web-1"}}}
	s := NewServer(client)

	rec := doRequest(t, s, http.MethodDelete, "/v1/vms/vm-1", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if client.lastDeleteReq.GetId() != "vm-1" {
		t.Errorf("forwarded id = %q, want vm-1", client.lastDeleteReq.GetId())
	}
}

func TestServer_ListVMs(t *testing.T) {
	client := &fakeClient{listResp: &rpcpb.ListVMsResponse{
		Vms: []*rpcpb.VMDefinition{
			{Id: "vm-1", Name: "web-1"},
			{Id: "vm-2", Name: "web-2"},
		},
	}}
	s := NewServer(client)

	rec := doRequest(t, s, http.MethodGet, "/v1/vms", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	var got []vm
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d VMs, want 2", len(got))
	}
}

func TestServer_ListVMs_Empty(t *testing.T) {
	client := &fakeClient{listResp: &rpcpb.ListVMsResponse{}}
	s := NewServer(client)

	rec := doRequest(t, s, http.MethodGet, "/v1/vms", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if rec.Body.String() != "[]\n" {
		t.Errorf("body = %q, want an empty JSON array, not null", rec.Body.String())
	}
}

func TestServer_CreateJail(t *testing.T) {
	client := &fakeClient{createJailResp: &rpcpb.CreateJailResponse{
		Jail: &rpcpb.JailDefinition{Id: "jail-1", Name: "web-1", Hostname: "web-1.local"},
	}}
	s := NewServer(client)

	rec := doRequest(t, s, http.MethodPost, "/v1/jails", jail{ID: "jail-1", Name: "web-1", Hostname: "web-1.local"})
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%s", rec.Code, rec.Body.String())
	}
	if client.lastCreateJailReq.GetJail().GetHostname() != "web-1.local" {
		t.Errorf("request forwarded jail.Hostname = %q, want web-1.local", client.lastCreateJailReq.GetJail().GetHostname())
	}

	var got jail
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if got.ID != "jail-1" {
		t.Errorf("response ID = %q, want jail-1", got.ID)
	}
}

func TestServer_CreateJail_InvalidJSON(t *testing.T) {
	s := NewServer(&fakeClient{})
	req := httptest.NewRequest(http.MethodPost, "/v1/jails", bytes.NewBufferString("not json"))
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

func TestServer_CreateJail_ApplicationErrorIsBadRequest(t *testing.T) {
	client := &fakeClient{createJailResp: &rpcpb.CreateJailResponse{Error: `CreateJail: id "jail-1" already exists`}}
	s := NewServer(client)

	rec := doRequest(t, s, http.MethodPost, "/v1/jails", jail{ID: "jail-1"})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
}

func TestServer_GetJail_Found(t *testing.T) {
	client := &fakeClient{getJailResp: &rpcpb.GetJailResponse{
		Found: true,
		Jail:  &rpcpb.JailDefinition{Id: "jail-1", Name: "web-1"},
	}}
	s := NewServer(client)

	rec := doRequest(t, s, http.MethodGet, "/v1/jails/jail-1", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if client.lastGetJailReq.GetId() != "jail-1" {
		t.Errorf("request forwarded id = %q, want jail-1", client.lastGetJailReq.GetId())
	}
}

func TestServer_GetJail_NotFound(t *testing.T) {
	client := &fakeClient{getJailResp: &rpcpb.GetJailResponse{Found: false}}
	s := NewServer(client)

	rec := doRequest(t, s, http.MethodGet, "/v1/jails/jail-1", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
}

func TestServer_UpdateJail_PathIDOverridesBody(t *testing.T) {
	client := &fakeClient{updateJailResp: &rpcpb.UpdateJailResponse{Jail: &rpcpb.JailDefinition{Id: "jail-1"}}}
	s := NewServer(client)

	rec := doRequest(t, s, http.MethodPut, "/v1/jails/jail-1", jail{ID: "wrong-id", Name: "renamed"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if client.lastUpdateJailReq.GetJail().GetId() != "jail-1" {
		t.Errorf("forwarded jail.Id = %q, want jail-1 (from the URL path)", client.lastUpdateJailReq.GetJail().GetId())
	}
}

func TestServer_DeleteJail(t *testing.T) {
	client := &fakeClient{deleteJailResp: &rpcpb.DeleteJailResponse{Jail: &rpcpb.JailDefinition{Id: "jail-1", Name: "web-1"}}}
	s := NewServer(client)

	rec := doRequest(t, s, http.MethodDelete, "/v1/jails/jail-1", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if client.lastDeleteJailReq.GetId() != "jail-1" {
		t.Errorf("forwarded id = %q, want jail-1", client.lastDeleteJailReq.GetId())
	}
}

func TestServer_ListJails(t *testing.T) {
	client := &fakeClient{listJailsResp: &rpcpb.ListJailsResponse{
		Jails: []*rpcpb.JailDefinition{
			{Id: "jail-1", Name: "web-1"},
			{Id: "jail-2", Name: "web-2"},
		},
	}}
	s := NewServer(client)

	rec := doRequest(t, s, http.MethodGet, "/v1/jails", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	var got []jail
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d jails, want 2", len(got))
	}
}

func TestServer_ListJails_Empty(t *testing.T) {
	client := &fakeClient{listJailsResp: &rpcpb.ListJailsResponse{}}
	s := NewServer(client)

	rec := doRequest(t, s, http.MethodGet, "/v1/jails", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if rec.Body.String() != "[]\n" {
		t.Errorf("body = %q, want an empty JSON array, not null", rec.Body.String())
	}
}

func TestServer_CreateNetwork(t *testing.T) {
	client := &fakeClient{createNetworkResp: &rpcpb.CreateNetworkResponse{
		Network: &rpcpb.NetworkDefinition{Id: "net-1", Name: "prod", VlanId: 100, Subnet: "10.60.0.0/24"},
	}}
	s := NewServer(client)

	rec := doRequest(t, s, http.MethodPost, "/v1/networks", network{ID: "net-1", Name: "prod", VLANID: 100, Subnet: "10.60.0.0/24"})
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%s", rec.Code, rec.Body.String())
	}
	if client.lastCreateNetworkReq.GetNetwork().GetSubnet() != "10.60.0.0/24" {
		t.Errorf("forwarded network.Subnet = %q, want 10.60.0.0/24", client.lastCreateNetworkReq.GetNetwork().GetSubnet())
	}
}

func TestServer_CreateNetwork_ApplicationErrorIsBadRequest(t *testing.T) {
	client := &fakeClient{createNetworkResp: &rpcpb.CreateNetworkResponse{Error: `CreateNetwork: id "net-1" already exists`}}
	s := NewServer(client)

	rec := doRequest(t, s, http.MethodPost, "/v1/networks", network{ID: "net-1", Subnet: "10.60.0.0/24"})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
}

func TestServer_DeleteNetwork(t *testing.T) {
	client := &fakeClient{deleteNetworkResp: &rpcpb.DeleteNetworkResponse{Network: &rpcpb.NetworkDefinition{Id: "net-1"}}}
	s := NewServer(client)

	rec := doRequest(t, s, http.MethodDelete, "/v1/networks/net-1", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if client.lastDeleteNetworkReq.GetId() != "net-1" {
		t.Errorf("forwarded id = %q, want net-1", client.lastDeleteNetworkReq.GetId())
	}
}

func TestServer_ListNetworks(t *testing.T) {
	client := &fakeClient{listNetworksResp: &rpcpb.ListNetworksResponse{
		Networks: []*rpcpb.NetworkDefinition{
			{Id: "net-1", Name: "prod", Subnet: "10.60.0.0/24"},
			{Id: "net-2", Name: "dev", Subnet: "10.61.0.0/24"},
		},
	}}
	s := NewServer(client)

	rec := doRequest(t, s, http.MethodGet, "/v1/networks", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	var got []network
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d networks, want 2", len(got))
	}
}

func TestServer_ListNetworks_Empty(t *testing.T) {
	client := &fakeClient{listNetworksResp: &rpcpb.ListNetworksResponse{}}
	s := NewServer(client)

	rec := doRequest(t, s, http.MethodGet, "/v1/networks", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if rec.Body.String() != "[]\n" {
		t.Errorf("body = %q, want an empty JSON array, not null", rec.Body.String())
	}
}

func TestServer_MigrateVM(t *testing.T) {
	client := &fakeClient{migrateVMResp: &rpcpb.MigrateVMResponse{
		Vm: &rpcpb.VMDefinition{Id: "vm-1", NodeId: "node-b", ReplicaNodeId: "node-a"},
	}}
	s := NewServer(client)

	rec := doRequest(t, s, http.MethodPost, "/v1/vms/vm-1/migrate", migrateRequestBody{TargetNodeID: "node-b"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if client.lastMigrateVMReq.GetId() != "vm-1" || client.lastMigrateVMReq.GetTargetNodeId() != "node-b" {
		t.Errorf("forwarded request = %+v, want id=vm-1 target_node_id=node-b", client.lastMigrateVMReq)
	}

	var got vm
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if got.NodeID != "node-b" || got.ReplicaNodeID != "node-a" {
		t.Errorf("response = %+v, want node_id=node-b replica_node_id=node-a", got)
	}
}

func TestServer_MigrateVM_ApplicationErrorIsBadRequest(t *testing.T) {
	client := &fakeClient{migrateVMResp: &rpcpb.MigrateVMResponse{Error: "MigrateVM requires target_node_id to already be this VM's replica_node_id"}}
	s := NewServer(client)

	rec := doRequest(t, s, http.MethodPost, "/v1/vms/vm-1/migrate", migrateRequestBody{TargetNodeID: "node-b"})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
}

func TestServer_MigrateJail(t *testing.T) {
	client := &fakeClient{migrateJailResp: &rpcpb.MigrateJailResponse{
		Jail: &rpcpb.JailDefinition{Id: "jail-1", NodeId: "node-b", ReplicaNodeId: "node-a"},
	}}
	s := NewServer(client)

	rec := doRequest(t, s, http.MethodPost, "/v1/jails/jail-1/migrate", migrateRequestBody{TargetNodeID: "node-b"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if client.lastMigrateJailReq.GetId() != "jail-1" || client.lastMigrateJailReq.GetTargetNodeId() != "node-b" {
		t.Errorf("forwarded request = %+v, want id=jail-1 target_node_id=node-b", client.lastMigrateJailReq)
	}
}

func TestServer_MigrateVM_InvalidJSON(t *testing.T) {
	s := NewServer(&fakeClient{})
	req := httptest.NewRequest(http.MethodPost, "/v1/vms/vm-1/migrate", bytes.NewBufferString("not json"))
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

// buildUploadRequest constructs a multipart/form-data POST /v1/isos
// request with expected_sha256 encoded before file - the order
// handleUploadISO requires, since it streams parts as they arrive
// rather than buffering the whole form first. Mirrors
// internal/frontend's identical test helper.
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

	req := httptest.NewRequest(http.MethodPost, "/v1/isos", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	return req
}

func TestServer_UploadISO_StreamsMetadataThenChunksInOrder(t *testing.T) {
	client := &fakeClient{uploadStream: &fakeUploadClientStream{
		resp: &rpcpb.UploadISOResponse{Name: "base.raw", SizeBytes: 4},
	}}
	s := NewServer(client)

	req := buildUploadRequest(t, "deadbeef", "base.raw", "data")
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%s", rec.Code, rec.Body.String())
	}

	sent := client.uploadStream.sent
	if len(sent) < 2 {
		t.Fatalf("stream got %d messages, want at least metadata + 1 chunk", len(sent))
	}
	meta := sent[0].GetMetadata()
	if meta == nil {
		t.Fatalf("first message = %+v, want metadata", sent[0])
	}
	if meta.GetName() != "base.raw" || meta.GetExpectedSha256() != "deadbeef" {
		t.Errorf("metadata = %+v, want name=base.raw hash=deadbeef", meta)
	}
	var gotData []byte
	for _, m := range sent[1:] {
		gotData = append(gotData, m.GetChunk()...)
	}
	if string(gotData) != "data" {
		t.Errorf("chunk data = %q, want %q", gotData, "data")
	}

	var body isoUploadResult
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if body.Name != "base.raw" {
		t.Errorf("response name = %q, want base.raw", body.Name)
	}
}

func TestServer_UploadISO_ApplicationErrorIsBadRequest(t *testing.T) {
	client := &fakeClient{uploadStream: &fakeUploadClientStream{
		resp: &rpcpb.UploadISOResponse{Error: `sha256 mismatch: got aaa, want bbb`},
	}}
	s := NewServer(client)

	req := buildUploadRequest(t, "bbb", "base.raw", "data")
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "sha256 mismatch") {
		t.Errorf("response missing hash-mismatch error, got: %s", rec.Body.String())
	}
}
