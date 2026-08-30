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

	listISOsResp     *rpcpb.ListISOsResponse
	deleteISOResp    *rpcpb.DeleteISOResponse
	lastDeleteISOReq *rpcpb.DeleteISORequest

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
	if !strings.Contains(body, `class="active"`) {
		t.Errorf("VMs page nav should mark its own link active, got: %s", body)
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
	if !strings.Contains(body, `class="create-vm"`) {
		t.Errorf("new VM page missing create form, got: %s", body)
	}
	if !strings.Contains(body, `id="create-error"`) {
		t.Errorf("new VM page missing error slot, got: %s", body)
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

	req := httptest.NewRequest(http.MethodGet, "/vms", nil)
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

	req := httptest.NewRequest(http.MethodGet, "/vms?sort=node&dir=desc", nil)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	body := rec.Body.String()
	if i, j := strings.Index(body, "vm-2"), strings.Index(body, "vm-1"); !(i < j) {
		t.Errorf("rows not sorted by node descending, got: %s", body)
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
	if got := rec.Header().Get("HX-Redirect"); got != "/" {
		t.Errorf("HX-Redirect = %q, want /", got)
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
