package frontend

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	rpcpb "github.com/glenjbarber/apiary/api/rpc"
)

// TestTheConsoleIsRefusedOverPlainHTTP is the property that closes
// the first hop of the console channel.
//
// A VM console carries the guest's screen and keyboard, live. Over a
// plain HTTP WebSocket it crosses the network in cleartext, so the
// session cookie in front of it is not enough: the cookie
// authenticates the OPENING of the socket, and everything after it -
// every keystroke, every pixel of the guest's screen - would be
// readable on the wire.
//
// So the console is refused outright when the frontend is not serving
// TLS. A refusal rather than a warning, because the alternative is a
// console that looks protected and is not.
func TestTheConsoleIsRefusedOverPlainHTTP(t *testing.T) {
	client := &fakeClient{
		getVMConsoleResp: &rpcpb.GetVMConsoleResponse{Available: true, Host: "apiarium", Port: 5901},
	}
	// A frontend NOT serving TLS.
	s := newTestServer(t, client)

	req := httptest.NewRequest(http.MethodGet, "/vms/vm-1/console/ws", nil)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403: a console over plain HTTP must be refused", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "cleartext") {
		t.Errorf("the refusal does not say the console would cross the network in cleartext: %s", body)
	}
}

// TestTheConsolePageSaysWhyBeforeTheOperatorWaitsForAConnection:
// the page the operator is already looking at says so, rather than the
// failure arriving as a WebSocket error after they have clicked into
// the console.
func TestTheConsolePageSaysWhyBeforeTheOperatorWaitsForAConnection(t *testing.T) {
	client := &fakeClient{
		getVMConsoleResp: &rpcpb.GetVMConsoleResponse{Available: true, Host: "apiarium", Port: 5901},
	}
	s := newTestServer(t, client)

	req := httptest.NewRequest(http.MethodGet, "/vms/vm-1/console", nil)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	body := rec.Body.String()
	if !strings.Contains(body, "cleartext") {
		t.Errorf("the console page does not explain that the console would be in cleartext: %s", body)
	}
	// And it must not offer the widget, because the console cannot
	// work.
	if strings.Contains(body, "novnc/core/rfb.js") {
		t.Error("the console page offers the noVNC widget for a console that will be refused")
	}
}

// TestTheConsoleWorksOverTLS: the guard must not have broken the
// console it is protecting.
func TestTheConsoleWorksOverTLS(t *testing.T) {
	client := &fakeClient{
		getVMConsoleResp: &rpcpb.GetVMConsoleResponse{Available: true, Host: "apiarium", Port: 5901},
	}
	s := newTestServerTLS(t, client)

	req := httptest.NewRequest(http.MethodGet, "/vms/vm-1/console", nil)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "novnc/core/rfb.js") {
		t.Errorf("the console page is missing the noVNC module import over TLS: %s", body)
	}
	if strings.Contains(body, `class="error"`) {
		t.Errorf("the console page shows an error banner for an available console over TLS: %s", body)
	}
}
