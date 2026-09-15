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
// ADR-0102 panels render on the Machine page - frontend/restshimd get
// real edit forms, raftd is read-only only (see
// TestServer_MachinePage_RaftdConfigPanelHasNoWriteForm below for why).
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
		`hx-post="/machine/frontend-config"`, `hx-post="/machine/restshimd-config"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %s in machine page body", want)
		}
	}
}

// TestServer_MachinePage_RaftdConfigPanelHasNoWriteForm is the
// regression test for a 2026-09-15 audit finding (P1): raftd's
// internal_token must match managerd's own separately-configured
// raftd_token for RaftInternal auth to keep working, and raft TLS
// material is cluster-coupled (changing it on one voter and restarting
// can lose quorum) - neither is safe to edit through a plain per-host
// save form without a real coordinated rotation workflow, which
// doesn't exist yet. The raftd panel must never render a form or POST
// route at all.
func TestServer_MachinePage_RaftdConfigPanelHasNoWriteForm(t *testing.T) {
	client := &fakeClient{
		getRaftdConfigResp: &rpcpb.GetRaftdConfigResponse{
			DataDir: "/var/db/apiary/raftd", RaftTlsCert: "/a/cert.pem", InternalTokenSet: true,
		},
	}
	s := newTestServer(t, client)

	req := httptest.NewRequest(http.MethodGet, "/machine", nil)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	body := rec.Body.String()
	if strings.Contains(body, `hx-post="/machine/raftd-config"`) {
		t.Error("raftd config panel must never post to a write route")
	}
	if strings.Contains(body, `name="internal_token"`) || strings.Contains(body, `name="raft_tls_cert"`) {
		t.Error("raftd config panel must never render an editable internal_token or raft_tls_cert input")
	}
	if !strings.Contains(body, "/a/cert.pem") {
		t.Error("raftd config panel should still display the current raft TLS certificate path read-only")
	}

	// The write route itself must not exist at all.
	form := url.Values{"raft_tls_cert": {"/new.pem"}}
	postReq := httptest.NewRequest(http.MethodPost, "/machine/raftd-config", strings.NewReader(form.Encode()))
	postReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	postRec := httptest.NewRecorder()
	s.ServeHTTP(postRec, postReq)
	if postRec.Code == http.StatusOK {
		t.Errorf("POST /machine/raftd-config should not be routed to a handler, got status %d", postRec.Code)
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
