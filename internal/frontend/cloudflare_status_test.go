package frontend

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	rpcpb "github.com/glenjbarber/apiary/api/rpc"
)

func TestHandleMachinePage_ShowsConfiguredStatus(t *testing.T) {
	client := &fakeClient{hostStatsResp: &rpcpb.HostStatsResponse{CloudflareConfigured: true}}
	s := newTestServer(t, client)

	req := httptest.NewRequest(http.MethodGet, "/machine", nil)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	body := rec.Body.String()
	if !strings.Contains(body, "configured") || strings.Contains(body, "not configured") {
		t.Fatalf("expected the configured state to render without the not-configured setup steps, got: %s", body)
	}
}

func TestHandleMachinePage_ShowsSetupStepsWhenNotConfigured(t *testing.T) {
	client := &fakeClient{hostStatsResp: &rpcpb.HostStatsResponse{CloudflareConfigured: false}}
	s := newTestServer(t, client)

	req := httptest.NewRequest(http.MethodGet, "/machine", nil)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	body := rec.Body.String()
	if !strings.Contains(body, "not configured") || !strings.Contains(body, "cloudflared tunnel create") {
		t.Fatalf("expected the not-configured state with setup steps to render, got: %s", body)
	}
}

func TestHandleVMPage_WarnsWhenOwningHiveHasNoCloudflareConfigured(t *testing.T) {
	client := &fakeClient{
		statusResp:    &rpcpb.StatusResponse{ManagerNodeId: "node-a"},
		getVMResp:     &rpcpb.GetVMResponse{Found: true, Vm: &rpcpb.VMDefinition{Id: "vm-1", NodeId: "node-a"}},
		hostStatsResp: &rpcpb.HostStatsResponse{CloudflareConfigured: false},
	}
	s := newTestServer(t, client)

	req := httptest.NewRequest(http.MethodGet, "/vms/vm-1", nil)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	body := rec.Body.String()
	if !strings.Contains(body, "no Cloudflare Tunnel configured") {
		t.Fatalf("expected the capability warning to render, got: %s", body)
	}
}

func TestHandleVMPage_NoWarningWhenOwningHiveHasCloudflareConfigured(t *testing.T) {
	client := &fakeClient{
		statusResp:    &rpcpb.StatusResponse{ManagerNodeId: "node-a"},
		getVMResp:     &rpcpb.GetVMResponse{Found: true, Vm: &rpcpb.VMDefinition{Id: "vm-1", NodeId: "node-a"}},
		hostStatsResp: &rpcpb.HostStatsResponse{CloudflareConfigured: true},
	}
	s := newTestServer(t, client)

	req := httptest.NewRequest(http.MethodGet, "/vms/vm-1", nil)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	body := rec.Body.String()
	if strings.Contains(body, "no Cloudflare Tunnel configured") {
		t.Fatalf("expected no capability warning when the owning Hive has Cloudflare configured, got: %s", body)
	}
}
