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
