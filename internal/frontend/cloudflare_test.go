package frontend

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	rpcpb "github.com/glenjbarber/apiary/api/rpc"
)

func TestHandleSetVMCloudflareExposure_ForwardsHostnameAndPort(t *testing.T) {
	client := &fakeClient{
		getVMResp:                   &rpcpb.GetVMResponse{Found: true, Vm: &rpcpb.VMDefinition{Id: "vm-1", Name: "web-1"}},
		setVMCloudflareExposureResp: &rpcpb.SetVMCloudflareExposureResponse{},
	}
	s := newTestServer(t, client)

	form := url.Values{"cloudflare_hostname": {"web.example.com"}, "cloudflare_port": {"8080"}}
	req := httptest.NewRequest(http.MethodPost, "/vms/vm-1/cloudflare-exposure", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	got := client.lastSetVMCloudflareExposureReq
	if got.GetId() != "vm-1" || got.GetHostname() != "web.example.com" || got.GetPort() != 8080 {
		t.Errorf("forwarded request = %+v, want Id=vm-1 Hostname=web.example.com Port=8080", got)
	}
}

func TestHandleSetVMCloudflareExposure_EmptyHostnameClearsWithoutPortValidation(t *testing.T) {
	client := &fakeClient{
		getVMResp:                   &rpcpb.GetVMResponse{Found: true, Vm: &rpcpb.VMDefinition{Id: "vm-1"}},
		setVMCloudflareExposureResp: &rpcpb.SetVMCloudflareExposureResponse{},
	}
	s := newTestServer(t, client)

	// No cloudflare_port at all - clearing exposure must never fail
	// port validation, since there's nothing to validate when turning
	// exposure off.
	form := url.Values{"cloudflare_hostname": {""}}
	req := httptest.NewRequest(http.MethodPost, "/vms/vm-1/cloudflare-exposure", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if client.lastSetVMCloudflareExposureReq.GetHostname() != "" {
		t.Errorf("forwarded Hostname = %q, want empty", client.lastSetVMCloudflareExposureReq.GetHostname())
	}
}

func TestHandleSetVMCloudflareExposure_InvalidPortRendersFormError(t *testing.T) {
	client := &fakeClient{getVMResp: &rpcpb.GetVMResponse{Found: true, Vm: &rpcpb.VMDefinition{Id: "vm-1"}}}
	s := newTestServer(t, client)

	form := url.Values{"cloudflare_hostname": {"web.example.com"}, "cloudflare_port": {"not-a-number"}}
	req := httptest.NewRequest(http.MethodPost, "/vms/vm-1/cloudflare-exposure", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if !strings.Contains(rec.Body.String(), "invalid port") {
		t.Fatalf("expected an invalid-port form error, got: %s", rec.Body.String())
	}
	if client.lastSetVMCloudflareExposureReq != nil {
		t.Errorf("expected SetVMCloudflareExposure to never be called with an invalid port, got: %+v", client.lastSetVMCloudflareExposureReq)
	}
}

func TestHandleSetVMCloudflareExposure_RPCErrorRendersFormError(t *testing.T) {
	client := &fakeClient{
		getVMResp:                   &rpcpb.GetVMResponse{Found: true, Vm: &rpcpb.VMDefinition{Id: "vm-1"}},
		setVMCloudflareExposureResp: &rpcpb.SetVMCloudflareExposureResponse{Error: "VM \"vm-1\" has no network_id set"},
	}
	s := newTestServer(t, client)

	form := url.Values{"cloudflare_hostname": {"web.example.com"}, "cloudflare_port": {"8080"}}
	req := httptest.NewRequest(http.MethodPost, "/vms/vm-1/cloudflare-exposure", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if !strings.Contains(rec.Body.String(), "no network_id set") {
		t.Fatalf("expected the RPC's own error to render, got: %s", rec.Body.String())
	}
}

func TestHandleVMPage_RendersPublicExposureWhenSet(t *testing.T) {
	client := &fakeClient{getVMResp: &rpcpb.GetVMResponse{Found: true, Vm: &rpcpb.VMDefinition{
		Id: "vm-1", Name: "web-1", NetworkId: "net-1",
		CloudflareHostname: "web.example.com", CloudflarePort: 8080,
	}}}
	s := newTestServer(t, client)

	req := httptest.NewRequest(http.MethodGet, "/vms/vm-1", nil)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	body := rec.Body.String()
	if !strings.Contains(body, "web.example.com") || !strings.Contains(body, "8080") {
		t.Fatalf("expected the configured public hostname/port to render, got: %s", body)
	}
}
