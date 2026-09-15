package frontend

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	rpcpb "github.com/glenjbarber/apiary/api/rpc"
)

// TestServer_MachinePage_ShowsSiblingDaemonConfigPanels confirms the
// three new ADR-0102 panels render on the Machine page.
func TestServer_MachinePage_ShowsSiblingDaemonConfigPanels(t *testing.T) {
	client := &fakeClient{
		getFrontendConfigResp:  &rpcpb.GetFrontendConfigResponse{ManagerAddr: "10.50.0.9:17700"},
		getRestshimdConfigResp: &rpcpb.GetRestshimdConfigResponse{ManagerAddr: "10.50.0.9:17700"},
		getRaftdConfigResp:     &rpcpb.GetRaftdConfigResponse{DataDir: "/var/db/apiary/raftd"},
	}
	s := newTestServer(t, client)

	req := httptest.NewRequest(http.MethodGet, "/machine", nil)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	body := rec.Body.String()
	for _, want := range []string{
		`id="frontend-config-panel"`, `id="restshimd-config-panel"`, `id="raftd-config-panel"`,
		`hx-post="/machine/frontend-config"`, `hx-post="/machine/restshimd-config"`, `hx-post="/machine/raftd-config"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %s in machine page body", want)
		}
	}
}

func TestServer_UpdateFrontendConfig_ForwardsFieldsAndSecret(t *testing.T) {
	client := &fakeClient{
		getFrontendConfigResp:    &rpcpb.GetFrontendConfigResponse{ManagerAddr: "127.0.0.1:17700", HttpAddr: "127.0.0.1:8080"},
		updateFrontendConfigResp: &rpcpb.UpdateFrontendConfigResponse{Scheduled: true},
	}
	s := newTestServer(t, client)

	form := url.Values{"http_addr": {"0.0.0.0:8080"}, "peer_tls": {"true"}, "manager_api_key": {"new-secret-key"}}
	req := httptest.NewRequest(http.MethodPost, "/machine/frontend-config", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	got := client.lastUpdateFrontendConfigReq
	if got.GetManagerAddr() != "127.0.0.1:17700" {
		t.Errorf("ManagerAddr = %q, want the resent current value 127.0.0.1:17700 (untouched field must survive)", got.GetManagerAddr())
	}
	if got.GetHttpAddr() != "0.0.0.0:8080" {
		t.Errorf("HttpAddr = %q, want the submitted value 0.0.0.0:8080", got.GetHttpAddr())
	}
	if !got.GetPeerTls() {
		t.Error("PeerTls = false, want true from the submitted form")
	}
	if got.GetManagerApiKey() != "new-secret-key" {
		t.Errorf("ManagerApiKey = %q, want new-secret-key", got.GetManagerApiKey())
	}
	if strings.Contains(rec.Body.String(), "new-secret-key") {
		t.Errorf("response must never echo back the raw secret, got: %s", rec.Body.String())
	}
}

func TestServer_UpdateFrontendConfig_ClearsAPIKey(t *testing.T) {
	client := &fakeClient{updateFrontendConfigResp: &rpcpb.UpdateFrontendConfigResponse{}}
	s := newTestServer(t, client)

	form := url.Values{"clear_manager_api_key": {"true"}}
	req := httptest.NewRequest(http.MethodPost, "/machine/frontend-config", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if !client.lastUpdateFrontendConfigReq.GetClearManagerApiKey() {
		t.Errorf("forwarded request should have ClearManagerApiKey=true, got: %+v", client.lastUpdateFrontendConfigReq)
	}
}

func TestServer_UpdateFrontendConfig_ErrorShowsInPanel(t *testing.T) {
	client := &fakeClient{updateFrontendConfigResp: &rpcpb.UpdateFrontendConfigResponse{Error: "disk full"}}
	s := newTestServer(t, client)

	form := url.Values{"http_addr": {"0.0.0.0:8080"}}
	req := httptest.NewRequest(http.MethodPost, "/machine/frontend-config", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if !strings.Contains(rec.Body.String(), "disk full") {
		t.Errorf("response missing error message, got: %s", rec.Body.String())
	}
}

// TestServer_UpdateRestshimdConfig_PreservesUntouchedFields mirrors
// TestServer_UpdateJailProvisioning_PreservesNetworkFields for
// restshimd's own config - a form touching only http_addr must not
// clobber manager_addr.
func TestServer_UpdateRestshimdConfig_PreservesUntouchedFields(t *testing.T) {
	client := &fakeClient{
		getRestshimdConfigResp:    &rpcpb.GetRestshimdConfigResponse{ManagerAddr: "10.50.0.9:17700", HttpAddr: "127.0.0.1:8081"},
		updateRestshimdConfigResp: &rpcpb.UpdateRestshimdConfigResponse{Scheduled: true},
	}
	s := newTestServer(t, client)

	form := url.Values{"http_addr": {"0.0.0.0:8081"}}
	req := httptest.NewRequest(http.MethodPost, "/machine/restshimd-config", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	got := client.lastUpdateRestshimdConfigReq
	if got.GetManagerAddr() != "10.50.0.9:17700" {
		t.Errorf("ManagerAddr = %q, want the preserved current value 10.50.0.9:17700", got.GetManagerAddr())
	}
	if got.GetHttpAddr() != "0.0.0.0:8081" {
		t.Errorf("HttpAddr = %q, want the submitted value 0.0.0.0:8081", got.GetHttpAddr())
	}
}

func TestServer_UpdateRaftdConfig_OnlyForwardsTheFiveEditableFields(t *testing.T) {
	client := &fakeClient{
		getRaftdConfigResp:    &rpcpb.GetRaftdConfigResponse{DataDir: "/var/db/apiary/raftd", RaftTlsCert: "/old.pem"},
		updateRaftdConfigResp: &rpcpb.UpdateRaftdConfigResponse{},
	}
	s := newTestServer(t, client)

	form := url.Values{"raft_tls_cert": {"/new.pem"}}
	req := httptest.NewRequest(http.MethodPost, "/machine/raftd-config", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if got := client.lastUpdateRaftdConfigReq.GetRaftTlsCert(); got != "/new.pem" {
		t.Errorf("RaftTlsCert = %q, want /new.pem", got)
	}
	// No panel copy of the response ever mentions data_dir being
	// editable - the "preserved" behavior here is structural (the proto
	// message has no field for it at all), not a merge this test could
	// otherwise break.
	if strings.Contains(rec.Body.String(), `name="data_dir"`) {
		t.Error("raftd config panel must never render an editable data_dir input")
	}
}

func TestServer_UpdateRaftdConfig_ClearsInternalToken(t *testing.T) {
	client := &fakeClient{updateRaftdConfigResp: &rpcpb.UpdateRaftdConfigResponse{}}
	s := newTestServer(t, client)

	form := url.Values{"clear_internal_token": {"true"}}
	req := httptest.NewRequest(http.MethodPost, "/machine/raftd-config", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if !client.lastUpdateRaftdConfigReq.GetClearInternalToken() {
		t.Errorf("forwarded request should have ClearInternalToken=true, got: %+v", client.lastUpdateRaftdConfigReq)
	}
}

func TestServer_UpdateRaftdConfig_ErrorShowsInPanel(t *testing.T) {
	client := &fakeClient{updateRaftdConfigResp: &rpcpb.UpdateRaftdConfigResponse{Error: "permission denied"}}
	s := newTestServer(t, client)

	form := url.Values{"raft_tls_cert": {"/new.pem"}}
	req := httptest.NewRequest(http.MethodPost, "/machine/raftd-config", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if !strings.Contains(rec.Body.String(), "permission denied") {
		t.Errorf("response missing error message, got: %s", rec.Body.String())
	}
}
