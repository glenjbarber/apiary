package frontend

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	rpcpb "github.com/glenjbarber/apiary/api/rpc"
)

func TestServer_MachinePage_ShowsNodeConfigAndLocalVMsOnly(t *testing.T) {
	client := &fakeClient{
		statusResp: &rpcpb.StatusResponse{ManagerNodeId: "node-a"},
		getNodeConfigResp: &rpcpb.GetNodeConfigResponse{
			Uplink: "re0", NatUplink: "bridge0", JailEnabled: boolPtr(true),
		},
		listResp: &rpcpb.ListVMsResponse{Vms: []*rpcpb.VMDefinition{
			{Id: "vm-1", Name: "web-1", NodeId: "node-a"},
			{Id: "vm-2", Name: "web-2", NodeId: "node-b"},
		}},
	}
	s := newTestServer(t, client)

	req := httptest.NewRequest(http.MethodGet, "/machine", nil)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "re0") || !strings.Contains(body, "bridge0") || !strings.Contains(body, "Jail provisioning") || !strings.Contains(body, "Enabled") {
		t.Errorf("machine page missing node config values, got: %s", body)
	}
	if !strings.Contains(body, "vm-1") {
		t.Errorf("machine page missing local VM vm-1, got: %s", body)
	}
	if strings.Contains(body, "vm-2") {
		t.Errorf("machine page shows vm-2, which belongs to a different node: %s", body)
	}
}

func TestServer_UpdateNodeConfig_ForwardsFormValues(t *testing.T) {
	client := &fakeClient{updateNodeConfigResp: &rpcpb.UpdateNodeConfigResponse{}}
	s := newTestServer(t, client)

	form := url.Values{"uplink": {"em0"}, "nat_uplink": {"em0"}, "jail_enabled": {"enabled"}}
	req := httptest.NewRequest(http.MethodPost, "/machine/uplink", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if client.lastUpdateNodeConfigReq.GetUplink() != "em0" || client.lastUpdateNodeConfigReq.GetNatUplink() != "em0" || client.lastUpdateNodeConfigReq.JailEnabled == nil || !client.lastUpdateNodeConfigReq.GetJailEnabled() {
		t.Errorf("forwarded request = %+v, want Uplink=em0 NatUplink=em0 JailEnabled=true", client.lastUpdateNodeConfigReq)
	}
}

func TestServer_UpdateNodeConfig_CanUseStartupFlagForJails(t *testing.T) {
	client := &fakeClient{updateNodeConfigResp: &rpcpb.UpdateNodeConfigResponse{}}
	s := newTestServer(t, client)

	form := url.Values{"uplink": {"em0"}, "jail_enabled": {"default"}}
	req := httptest.NewRequest(http.MethodPost, "/machine/uplink", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if client.lastUpdateNodeConfigReq.JailEnabled != nil {
		t.Errorf("JailEnabled = %v, want nil for startup-flag mode", client.lastUpdateNodeConfigReq.JailEnabled)
	}
}

func TestServer_UpdateNodeConfig_ErrorShowsInPanel(t *testing.T) {
	client := &fakeClient{updateNodeConfigResp: &rpcpb.UpdateNodeConfigResponse{Error: "disk full"}}
	s := newTestServer(t, client)

	form := url.Values{"uplink": {"em0"}}
	req := httptest.NewRequest(http.MethodPost, "/machine/uplink", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if !strings.Contains(rec.Body.String(), "disk full") {
		t.Errorf("response missing error message, got: %s", rec.Body.String())
	}
}

func TestServer_SetVMFirewallPaused_ForwardsPausedValue(t *testing.T) {
	client := &fakeClient{setVMFirewallPausedResp: &rpcpb.SetVMFirewallPausedResponse{}}
	s := newTestServer(t, client)

	form := url.Values{"paused": {"true"}}
	req := httptest.NewRequest(http.MethodPost, "/machine/vms/vm-1/firewall", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if client.lastSetVMFirewallPausedReq.GetId() != "vm-1" || !client.lastSetVMFirewallPausedReq.GetPaused() {
		t.Errorf("forwarded request = %+v, want Id=vm-1 Paused=true", client.lastSetVMFirewallPausedReq)
	}
}

func TestServer_SetVMFirewallPaused_Resume(t *testing.T) {
	client := &fakeClient{setVMFirewallPausedResp: &rpcpb.SetVMFirewallPausedResponse{}}
	s := newTestServer(t, client)

	form := url.Values{"paused": {"false"}}
	req := httptest.NewRequest(http.MethodPost, "/machine/vms/vm-1/firewall", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if client.lastSetVMFirewallPausedReq.GetPaused() {
		t.Errorf("forwarded Paused = true, want false")
	}
}

func TestServer_SetVMFirewallPaused_ErrorShowsInPanel(t *testing.T) {
	client := &fakeClient{setVMFirewallPausedResp: &rpcpb.SetVMFirewallPausedResponse{Error: "vm-1 not found"}}
	s := newTestServer(t, client)

	form := url.Values{"paused": {"true"}}
	req := httptest.NewRequest(http.MethodPost, "/machine/vms/vm-1/firewall", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if !strings.Contains(rec.Body.String(), "vm-1 not found") {
		t.Errorf("response missing error message, got: %s", rec.Body.String())
	}
}

func TestServer_SetDatasetQuota_ForwardsFormValues(t *testing.T) {
	client := &fakeClient{setDatasetQuotaResp: &rpcpb.SetDatasetQuotaResponse{}}
	s := newTestServer(t, client)

	form := url.Values{"dataset_name": {"vm-1"}, "quota": {"10G"}}
	req := httptest.NewRequest(http.MethodPost, "/machine/quota", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if client.lastSetDatasetQuotaReq.GetDatasetName() != "vm-1" || client.lastSetDatasetQuotaReq.GetQuota() != "10G" {
		t.Errorf("forwarded request = %+v, want DatasetName=vm-1 Quota=10G", client.lastSetDatasetQuotaReq)
	}
	if !strings.Contains(rec.Body.String(), "vm-1") || !strings.Contains(rec.Body.String(), "10G") {
		t.Errorf("response missing success confirmation, got: %s", rec.Body.String())
	}
}

func TestServer_SetDatasetQuota_ErrorShowsInPanel(t *testing.T) {
	client := &fakeClient{setDatasetQuotaResp: &rpcpb.SetDatasetQuotaResponse{Error: "dataset does not exist"}}
	s := newTestServer(t, client)

	form := url.Values{"dataset_name": {"vm-1"}, "quota": {"10G"}}
	req := httptest.NewRequest(http.MethodPost, "/machine/quota", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if !strings.Contains(rec.Body.String(), "dataset does not exist") {
		t.Errorf("response missing error message, got: %s", rec.Body.String())
	}
}

func boolPtr(v bool) *bool {
	return &v
}
