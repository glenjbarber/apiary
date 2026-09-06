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
			Uplink: "re0", NatUplink: "bridge0", DhcpDnsServer: "10.62.0.1", JailEnabled: boolPtr(true),
			AvailableInterfaces: []*rpcpb.NetworkInterface{
				{Name: "bridge0", Up: true, Addresses: []string{"10.50.0.14/24"}},
				{Name: "re0", Up: false},
			},
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
	if !strings.Contains(body, "re0") || !strings.Contains(body, "bridge0") || !strings.Contains(body, "self-hosted NAT via bridge0") || !strings.Contains(body, "10.62.0.1") || !strings.Contains(body, "Jail provisioning") || !strings.Contains(body, "Current mode") || !strings.Contains(body, "Enabled") {
		t.Errorf("machine page missing node config values, got: %s", body)
	}
	if !strings.Contains(body, `<select name="uplink">`) || !strings.Contains(body, `<option value="bridge0"`) || !strings.Contains(body, `bridge0 (up) - 10.50.0.14/24`) {
		t.Errorf("machine page missing discovered interface choices, got: %s", body)
	}
	if strings.Contains(body, "<th>Jail provisioning</th>") {
		t.Errorf("jail provisioning should be in its own Machine page section, got: %s", body)
	}
	if !strings.Contains(body, "vm-1") {
		t.Errorf("machine page missing local VM vm-1, got: %s", body)
	}
	if strings.Contains(body, "vm-2") {
		t.Errorf("machine page shows vm-2, which belongs to a different node: %s", body)
	}
}

func TestServer_MachinePage_ShowsLocalServiceControls(t *testing.T) {
	client := &fakeClient{listNodeServicesResp: &rpcpb.ListNodeServicesResponse{
		Services: []*rpcpb.NodeService{
			{Name: "apiary_managerd", Status: "running", Enabled: true, Restartable: true},
			{Name: "apiary_raftd", Status: "running", Enabled: true},
		},
	}}
	s := newTestServer(t, client)

	req := httptest.NewRequest(http.MethodGet, "/machine", nil)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	body := rec.Body.String()
	if !strings.Contains(body, "Apiary services") || !strings.Contains(body, "apiary_managerd") || !strings.Contains(body, `hx-post="/machine/services/apiary_managerd/restart"`) {
		t.Errorf("machine page missing managerd service control, got: %s", body)
	}
	if strings.Contains(body, `hx-post="/machine/services/apiary_raftd/restart"`) {
		t.Errorf("machine page offered a restart for raftd, got: %s", body)
	}
}

func TestServer_RestartNodeService_ForwardsAllowlistedName(t *testing.T) {
	client := &fakeClient{
		restartNodeServiceResp: &rpcpb.RestartNodeServiceResponse{Scheduled: true},
		listNodeServicesResp:   &rpcpb.ListNodeServicesResponse{},
	}
	s := newTestServer(t, client)

	req := httptest.NewRequest(http.MethodPost, "/machine/services/apiary_managerd/restart", nil)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if got := client.lastRestartNodeServiceReq.GetName(); got != "apiary_managerd" {
		t.Errorf("restart request name = %q, want apiary_managerd", got)
	}
	if !strings.Contains(rec.Body.String(), "restart scheduled") {
		t.Errorf("response missing scheduled confirmation, got: %s", rec.Body.String())
	}
}

func TestServer_UpdateNodeConfig_ForwardsFormValues(t *testing.T) {
	client := &fakeClient{updateNodeConfigResp: &rpcpb.UpdateNodeConfigResponse{}}
	s := newTestServer(t, client)

	form := url.Values{"uplink": {"em0"}, "nat_uplink": {"em0"}, "dhcp_dns_server": {"10.62.0.1"}, "jail_enabled": {"enabled"}}
	req := httptest.NewRequest(http.MethodPost, "/machine/uplink", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if client.lastUpdateNodeConfigReq.GetUplink() != "em0" || client.lastUpdateNodeConfigReq.GetNatUplink() != "em0" || client.lastUpdateNodeConfigReq.GetDhcpDnsServer() != "10.62.0.1" || client.lastUpdateNodeConfigReq.JailEnabled == nil || !client.lastUpdateNodeConfigReq.GetJailEnabled() {
		t.Errorf("forwarded request = %+v, want Uplink=em0 NatUplink=em0 DhcpDnsServer=10.62.0.1 JailEnabled=true", client.lastUpdateNodeConfigReq)
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

func TestServer_UpdateNodeConfig_PreservesJailProvisioningWhenAbsent(t *testing.T) {
	client := &fakeClient{
		getNodeConfigResp:    &rpcpb.GetNodeConfigResponse{Uplink: "re0", NatUplink: "bridge0", DhcpDnsServer: "10.62.0.1", JailEnabled: boolPtr(true)},
		updateNodeConfigResp: &rpcpb.UpdateNodeConfigResponse{},
	}
	s := newTestServer(t, client)

	form := url.Values{"uplink": {"em0"}, "nat_uplink": {"bridge1"}}
	req := httptest.NewRequest(http.MethodPost, "/machine/uplink", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if client.lastUpdateNodeConfigReq.GetDhcpDnsServer() != "10.62.0.1" {
		t.Errorf("DhcpDnsServer = %q, want preserved DNS server", client.lastUpdateNodeConfigReq.GetDhcpDnsServer())
	}
	if client.lastUpdateNodeConfigReq.JailEnabled == nil || !client.lastUpdateNodeConfigReq.GetJailEnabled() {
		t.Errorf("JailEnabled = %v, want preserved enabled value", client.lastUpdateNodeConfigReq.JailEnabled)
	}
}

func TestServer_UpdateJailProvisioning_PreservesNetworkFields(t *testing.T) {
	client := &fakeClient{
		getNodeConfigResp:    &rpcpb.GetNodeConfigResponse{Uplink: "re0", NatUplink: "bridge0", DhcpDnsServer: "10.62.0.1"},
		updateNodeConfigResp: &rpcpb.UpdateNodeConfigResponse{},
	}
	s := newTestServer(t, client)

	form := url.Values{"jail_enabled": {"enabled"}}
	req := httptest.NewRequest(http.MethodPost, "/machine/jail-provisioning", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if client.lastUpdateNodeConfigReq.GetUplink() != "re0" || client.lastUpdateNodeConfigReq.GetNatUplink() != "bridge0" || client.lastUpdateNodeConfigReq.GetDhcpDnsServer() != "10.62.0.1" || client.lastUpdateNodeConfigReq.JailEnabled == nil || !client.lastUpdateNodeConfigReq.GetJailEnabled() {
		t.Errorf("forwarded request = %+v, want preserved network fields and JailEnabled=true", client.lastUpdateNodeConfigReq)
	}
	if !strings.Contains(rec.Body.String(), `hx-post="/machine/jail-provisioning"`) {
		t.Errorf("jail provisioning fragment should still include the admin form, got: %s", rec.Body.String())
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

func TestServer_MachinePage_ShowsSystemSettingsPanels(t *testing.T) {
	client := &fakeClient{
		getNodeConfigResp: &rpcpb.GetNodeConfigResponse{
			Uplink: "re0", NatUplink: "bridge0", DhcpDnsServer: "10.62.0.1",
			ZfsBase: "zroot/apiary", BhyvePrefix: "apiary-", IsoDir: "/var/db/apiary/isos",
			JailPrefix: "apiary-", JailMountBase: "/apiary-jails",
			BhyveBootrom: "/usr/local/share/uefi-firmware/BHYVE_UEFI.fd", BhyveBridge: "bridge0",
			DiskSizeMb: 20480, ReconcileInterval: "30s",
			HastEnabled:      boolPtr(true),
			PeerManagerdPort: "17700", PeerTls: boolPtr(true), PeerTlsHostnameMap: "10.50.0.9=apiverse.apiary.work",
			PeerApiKeySet: true, RaftdTokenSet: true,
			TlsCert: "/home/claude/apiary-tls/fullchain.pem", TlsKey: "/home/claude/apiary-tls/key.pem",
			CloudflareTokenFile: "/home/claude/cf-token", CloudflareZoneId: "zone-1", CloudflareTunnelId: "tunnel-1", CloudflareTunnelCredentialsFile: "/home/claude/cf-creds.json",
			AssumptionCheckInterval: "60s", AssumptionHeartbeatInterval: "1h0m0s", AssumptionHistoryLimit: 200,
		},
	}
	s := newTestServer(t, client)

	req := httptest.NewRequest(http.MethodGet, "/machine", nil)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{
		"zroot/apiary", "/var/db/apiary/isos", "/apiary-jails",
		"BHYVE_UEFI.fd", "bridge0", "20480",
		`hx-post="/machine/hast"`,
		"apiverse.apiary.work", "17700",
		"/home/claude/apiary-tls/fullchain.pem",
		"/home/claude/cf-token", "zone-1", "tunnel-1",
		"60s",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("machine page missing %q, got: %s", want, body)
		}
	}
	if !strings.Contains(body, `<span class="badge true">set</span>`) {
		t.Errorf("machine page should show peer API key/raftd token as set, got: %s", body)
	}
	if strings.Contains(body, `name="zfs_base"`) {
		t.Errorf("zfs_base should be read-only once set, got: %s", body)
	}
}

func TestServer_MachinePage_ResourceScopeEditableWhenUnset(t *testing.T) {
	client := &fakeClient{getNodeConfigResp: &rpcpb.GetNodeConfigResponse{Uplink: "re0"}}
	s := newTestServer(t, client)

	req := httptest.NewRequest(http.MethodGet, "/machine", nil)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	body := rec.Body.String()
	if !strings.Contains(body, `name="zfs_base"`) || !strings.Contains(body, `name="bhyve_prefix"`) || !strings.Contains(body, `name="iso_dir"`) || !strings.Contains(body, `name="jail_prefix"`) || !strings.Contains(body, `name="jail_mount_base"`) {
		t.Errorf("resource scope fields should be editable when unset, got: %s", body)
	}
}

func TestServer_UpdateResourceScope_ForwardsFormValues(t *testing.T) {
	client := &fakeClient{updateNodeConfigResp: &rpcpb.UpdateNodeConfigResponse{}}
	s := newTestServer(t, client)

	form := url.Values{"zfs_base": {"zroot/apiary"}, "bhyve_prefix": {"apiary-"}, "iso_dir": {"/var/db/apiary/isos"}, "jail_prefix": {"apiary-"}, "jail_mount_base": {"/apiary-jails"}}
	req := httptest.NewRequest(http.MethodPost, "/machine/resource-scope", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	got := client.lastUpdateNodeConfigReq
	if got.GetZfsBase() != "zroot/apiary" || got.GetBhyvePrefix() != "apiary-" || got.GetIsoDir() != "/var/db/apiary/isos" || got.GetJailPrefix() != "apiary-" || got.GetJailMountBase() != "/apiary-jails" {
		t.Errorf("forwarded request = %+v, want all resource scope fields set", got)
	}
}

func TestServer_UpdateBhyveConfig_ForwardsFormValues(t *testing.T) {
	client := &fakeClient{updateNodeConfigResp: &rpcpb.UpdateNodeConfigResponse{}}
	s := newTestServer(t, client)

	form := url.Values{"bhyve_bootrom": {"/path/BHYVE_UEFI.fd"}, "bhyve_bridge": {"bridge0"}, "disk_size_mb": {"20480"}, "reconcile_interval": {"45s"}}
	req := httptest.NewRequest(http.MethodPost, "/machine/bhyve", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	got := client.lastUpdateNodeConfigReq
	if got.GetBhyveBootrom() != "/path/BHYVE_UEFI.fd" || got.GetBhyveBridge() != "bridge0" || got.GetDiskSizeMb() != 20480 || got.GetReconcileInterval() != "45s" {
		t.Errorf("forwarded request = %+v, want bhyve config fields set", got)
	}
}

func TestServer_UpdateHASTProvisioning_ForwardsTriState(t *testing.T) {
	client := &fakeClient{updateNodeConfigResp: &rpcpb.UpdateNodeConfigResponse{}}
	s := newTestServer(t, client)

	form := url.Values{"hast_enabled": {"enabled"}}
	req := httptest.NewRequest(http.MethodPost, "/machine/hast", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	got := client.lastUpdateNodeConfigReq
	if got.HastEnabled == nil || !got.GetHastEnabled() {
		t.Errorf("forwarded request = %+v, want HastEnabled=true", got)
	}
}

func TestServer_UpdatePeerForwarding_ForwardsFieldsAndSecret(t *testing.T) {
	client := &fakeClient{updateNodeConfigResp: &rpcpb.UpdateNodeConfigResponse{}}
	s := newTestServer(t, client)

	form := url.Values{"peer_managerd_port": {"17700"}, "peer_tls": {"enabled"}, "peer_tls_hostname_map": {"10.50.0.9=apiverse.apiary.work"}, "peer_api_key": {"new-secret-key"}}
	req := httptest.NewRequest(http.MethodPost, "/machine/peer-forwarding", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	got := client.lastUpdateNodeConfigReq
	if got.GetPeerManagerdPort() != "17700" || got.PeerTls == nil || !got.GetPeerTls() || got.GetPeerTlsHostnameMap() != "10.50.0.9=apiverse.apiary.work" || got.GetPeerApiKey() != "new-secret-key" {
		t.Errorf("forwarded request = %+v, want peer forwarding fields and new key set", got)
	}
	if rec.Body.String() == "" || strings.Contains(rec.Body.String(), "new-secret-key") {
		t.Errorf("response must never echo back the raw secret, got: %s", rec.Body.String())
	}
}

func TestServer_UpdatePeerForwarding_ClearsAPIKey(t *testing.T) {
	client := &fakeClient{updateNodeConfigResp: &rpcpb.UpdateNodeConfigResponse{}}
	s := newTestServer(t, client)

	form := url.Values{"clear_peer_api_key": {"true"}}
	req := httptest.NewRequest(http.MethodPost, "/machine/peer-forwarding", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if !client.lastUpdateNodeConfigReq.GetClearPeerApiKey() {
		t.Errorf("forwarded request should have ClearPeerApiKey=true, got: %+v", client.lastUpdateNodeConfigReq)
	}
}

func TestServer_UpdateTLSConfig_ForwardsFormValues(t *testing.T) {
	client := &fakeClient{updateNodeConfigResp: &rpcpb.UpdateNodeConfigResponse{}}
	s := newTestServer(t, client)

	form := url.Values{"tls_cert": {"/path/fullchain.pem"}, "tls_key": {"/path/key.pem"}}
	req := httptest.NewRequest(http.MethodPost, "/machine/tls", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	got := client.lastUpdateNodeConfigReq
	if got.GetTlsCert() != "/path/fullchain.pem" || got.GetTlsKey() != "/path/key.pem" {
		t.Errorf("forwarded request = %+v, want TLS cert/key set", got)
	}
}

func TestServer_UpdateCloudflareConfig_ForwardsFormValues(t *testing.T) {
	client := &fakeClient{updateNodeConfigResp: &rpcpb.UpdateNodeConfigResponse{}}
	s := newTestServer(t, client)

	form := url.Values{"cloudflare_token_file": {"/path/cf-token"}, "cloudflare_zone_id": {"zone-1"}, "cloudflare_tunnel_id": {"tunnel-1"}, "cloudflare_tunnel_credentials_file": {"/path/cf-creds.json"}}
	req := httptest.NewRequest(http.MethodPost, "/machine/cloudflare-config", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	got := client.lastUpdateNodeConfigReq
	if got.GetCloudflareTokenFile() != "/path/cf-token" || got.GetCloudflareZoneId() != "zone-1" || got.GetCloudflareTunnelId() != "tunnel-1" || got.GetCloudflareTunnelCredentialsFile() != "/path/cf-creds.json" {
		t.Errorf("forwarded request = %+v, want all cloudflare config fields set", got)
	}
}

func TestServer_UpdateAssumptionTuning_ForwardsFormValues(t *testing.T) {
	client := &fakeClient{updateNodeConfigResp: &rpcpb.UpdateNodeConfigResponse{}}
	s := newTestServer(t, client)

	form := url.Values{
		"assumption_check_interval": {"60s"}, "assumption_heartbeat_interval": {"1h"},
		"assumption_stale_after": {"3m"}, "assumption_run_deadline": {"20s"},
		"assumption_history_limit": {"200"}, "assumption_history_max_age": {"720h"},
	}
	req := httptest.NewRequest(http.MethodPost, "/machine/assumption-tuning", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	got := client.lastUpdateNodeConfigReq
	if got.GetAssumptionCheckInterval() != "60s" || got.GetAssumptionHeartbeatInterval() != "1h" || got.GetAssumptionStaleAfter() != "3m" || got.GetAssumptionRunDeadline() != "20s" || got.GetAssumptionHistoryLimit() != 200 || got.GetAssumptionHistoryMaxAge() != "720h" {
		t.Errorf("forwarded request = %+v, want all assumption tuning fields set", got)
	}
}

func TestServer_UpdateInternalSecurity_SetsAndClearsRaftdToken(t *testing.T) {
	client := &fakeClient{updateNodeConfigResp: &rpcpb.UpdateNodeConfigResponse{}}
	s := newTestServer(t, client)

	form := url.Values{"raftd_token": {"new-raftd-secret"}}
	req := httptest.NewRequest(http.MethodPost, "/machine/internal-security", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if client.lastUpdateNodeConfigReq.GetRaftdToken() != "new-raftd-secret" {
		t.Errorf("forwarded request = %+v, want RaftdToken=new-raftd-secret", client.lastUpdateNodeConfigReq)
	}
	if strings.Contains(rec.Body.String(), "new-raftd-secret") {
		t.Errorf("response must never echo back the raw secret, got: %s", rec.Body.String())
	}

	form = url.Values{"clear_raftd_token": {"true"}}
	req = httptest.NewRequest(http.MethodPost, "/machine/internal-security", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec = httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if !client.lastUpdateNodeConfigReq.GetClearRaftdToken() {
		t.Errorf("forwarded request should have ClearRaftdToken=true, got: %+v", client.lastUpdateNodeConfigReq)
	}
}

func boolPtr(v bool) *bool {
	return &v
}
