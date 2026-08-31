package restshim

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
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

func (f *fakeClient) GetVM(_ context.Context, in *rpcpb.GetVMRequest, _ ...grpc.CallOption) (*rpcpb.GetVMResponse, error) {
	f.lastGetReq = in
	return f.getResp, nil
}

func (f *fakeClient) ListVMs(context.Context, *rpcpb.ListVMsRequest, ...grpc.CallOption) (*rpcpb.ListVMsResponse, error) {
	return f.listResp, nil
}

func (f *fakeClient) UploadISO(context.Context, ...grpc.CallOption) (grpc.ClientStreamingClient[rpcpb.UploadISORequest, rpcpb.UploadISOResponse], error) {
	return nil, fmt.Errorf("fakeClient: UploadISO not implemented")
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

func (f *fakeClient) CreateNetwork(context.Context, *rpcpb.CreateNetworkRequest, ...grpc.CallOption) (*rpcpb.CreateNetworkResponse, error) {
	return &rpcpb.CreateNetworkResponse{}, nil
}

func (f *fakeClient) ListNetworks(context.Context, *rpcpb.ListNetworksRequest, ...grpc.CallOption) (*rpcpb.ListNetworksResponse, error) {
	return &rpcpb.ListNetworksResponse{}, nil
}

func (f *fakeClient) DeleteNetwork(context.Context, *rpcpb.DeleteNetworkRequest, ...grpc.CallOption) (*rpcpb.DeleteNetworkResponse, error) {
	return &rpcpb.DeleteNetworkResponse{}, nil
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
