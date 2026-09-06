package frontend

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	rpcpb "github.com/glenjbarber/apiary/api/rpc"
)

func TestServer_SerialLogPage_AvailableShowsContent(t *testing.T) {
	client := &fakeClient{
		getVMSerialLogResp: &rpcpb.GetVMSerialLogResponse{Available: true, Content: "line one\nline two\n"},
	}
	s := newTestServer(t, client)

	req := httptest.NewRequest(http.MethodGet, "/vms/vm-1/serial", nil)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "line one") || !strings.Contains(body, "line two") {
		t.Errorf("serial log page missing expected content, got: %s", body)
	}
	if strings.Contains(body, `class="error"`) {
		t.Errorf("serial log page shows an error banner for available content, got: %s", body)
	}
}

// TestServer_SerialLogPage_HasStopStartPollingToggle is the regression
// test for a tracked follow-up feature: a refresh used to always reset
// the reader's scroll position because it replaced the whole <pre>
// element, with no way to pause it. The page must render a toggle
// control and gate its poll trigger on a JS-evaluated condition (htmx's
// "every <interval> [<expr>]" syntax) rather than an unconditional poll.
func TestServer_SerialLogPage_HasStopStartPollingToggle(t *testing.T) {
	client := &fakeClient{
		getVMSerialLogResp: &rpcpb.GetVMSerialLogResponse{Available: true, Content: "line one\n"},
	}
	s := newTestServer(t, client)

	req := httptest.NewRequest(http.MethodGet, "/vms/vm-1/serial", nil)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `id="serial-log-poll-toggle"`) {
		t.Errorf("serial log page missing the Stop/Start refreshing toggle, got: %s", body)
	}
	if !strings.Contains(body, `hx-trigger="every 3s [window.apiarySerialLogPolling]"`) {
		t.Errorf("serial log panel's poll trigger must be gated by the toggle's condition, got: %s", body)
	}
	if !strings.Contains(body, "htmx:beforeSwap") || !strings.Contains(body, "htmx:afterSwap") {
		t.Errorf("serial log page missing scroll-position preservation across polls, got: %s", body)
	}
}

func TestServer_SerialLogPage_TruncatedShowsNotice(t *testing.T) {
	client := &fakeClient{
		getVMSerialLogResp: &rpcpb.GetVMSerialLogResponse{Available: true, Content: "tail only", Truncated: true},
	}
	s := newTestServer(t, client)

	req := httptest.NewRequest(http.MethodGet, "/vms/vm-1/serial", nil)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if !strings.Contains(rec.Body.String(), "most recent portion only") {
		t.Errorf("serial log page missing truncation notice, got: %s", rec.Body.String())
	}
}

func TestServer_SerialLogPage_UnavailableShowsError(t *testing.T) {
	client := &fakeClient{
		getVMSerialLogResp: &rpcpb.GetVMSerialLogResponse{Available: false},
	}
	s := newTestServer(t, client)

	req := httptest.NewRequest(http.MethodGet, "/vms/vm-1/serial", nil)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if !strings.Contains(rec.Body.String(), "no captured serial log") {
		t.Errorf("serial log page missing unavailable message, got: %s", rec.Body.String())
	}
}

func TestServer_SerialLogPage_RPCErrorShownAsError(t *testing.T) {
	client := &fakeClient{getVMSerialLogErr: errors.New("managerd unreachable")}
	s := newTestServer(t, client)

	req := httptest.NewRequest(http.MethodGet, "/vms/vm-1/serial", nil)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if !strings.Contains(rec.Body.String(), "managerd unreachable") {
		t.Errorf("serial log page missing the RPC error, got: %s", rec.Body.String())
	}
}

func TestServer_SerialLogContent_ServesFragmentForPolling(t *testing.T) {
	client := &fakeClient{
		getVMSerialLogResp: &rpcpb.GetVMSerialLogResponse{Available: true, Content: "fresh content"},
	}
	s := newTestServer(t, client)

	req := httptest.NewRequest(http.MethodGet, "/vms/vm-1/serial/content", nil)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "fresh content") {
		t.Errorf("serial log content fragment missing expected content, got: %s", body)
	}
	if strings.Contains(body, "<!DOCTYPE html>") {
		t.Errorf("serial log content should be a fragment, not a full page, got: %s", body)
	}
}

func TestServer_SerialLogPage_RemoteOwnerUsesPeer(t *testing.T) {
	client := &fakeClient{
		getVMResp:  &rpcpb.GetVMResponse{Found: true, Vm: &rpcpb.VMDefinition{Id: "vm-1", NodeId: "apiverse"}},
		statusResp: &rpcpb.StatusResponse{ManagerNodeId: "apiarium"},
	}
	peers := &fakePeerHostStatsClient{serialResp: &rpcpb.GetVMSerialLogResponse{Available: true, Content: "remote content"}}
	s, err := NewServer(client, nil, nil, peers, ".apiary.work", "17700", nil, false)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/vms/vm-1/serial", nil)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if !strings.Contains(rec.Body.String(), "remote content") {
		t.Errorf("serial log page did not render peer content: %s", rec.Body.String())
	}
	if peers.lastSerialAddr != "apiverse.apiary.work:17700" {
		t.Errorf("peer serial address = %q", peers.lastSerialAddr)
	}
}
