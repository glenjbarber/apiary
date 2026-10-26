package frontend

// The no-store header on every rendered page.
//
// Why this exists: the templates are compiled INTO the binary, so an
// upgraded frontend serves a DIFFERENT page from the same URL. A
// browser that cached the old one keeps showing the old layout with
// nothing indicating a problem - which is exactly the symptom that
// sent this whole investigation down a false path.

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	rpcpb "github.com/glenjbarber/apiary/api/rpc"
)

// Every rendered page says not to be cached.
func TestRender_SetsNoStore(t *testing.T) {
	s := newTestServer(t, &fakeClient{
		statusResp: &rpcpb.StatusResponse{ManagerNodeId: "brood"},
		listResp:   &rpcpb.ListVMsResponse{},
	})

	for _, path := range []string{"/vms", "/machine", "/"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("%s: status = %d, want 200", path, rec.Code)
		}
		got := rec.Header().Get("Cache-Control")
		if !strings.Contains(got, "no-store") {
			t.Errorf("%s: Cache-Control = %q, want it to contain no-store; a browser may otherwise cache a page whose server has already replaced it", path, got)
		}
		if got := rec.Header().Get("Expires"); got != "0" {
			t.Errorf("%s: Expires = %q, want 0", path, got)
		}
	}
}

// The header must be on FRAGMENT renders too.
//
// The polled fragments carry the same per-operator state as the pages
// that contain them, and a cached fragment would show a stale row
// without the page around it changing at all.
func TestRender_FragmentSetsNoStore(t *testing.T) {
	s := newTestServer(t, &fakeClient{
		statusResp: &rpcpb.StatusResponse{ManagerNodeId: "brood"},
		listResp:   &rpcpb.ListVMsResponse{},
	})
	req := httptest.NewRequest(http.MethodGet, "/vms/rows", nil)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if got := rec.Header().Get("Cache-Control"); !strings.Contains(got, "no-store") {
		t.Errorf("Cache-Control = %q on a fragment render, want it to contain no-store", got)
	}
}

// A redirect to the login page must not be cached either.
//
// The session store is IN-MEMORY, so a frontend restart invalidates
// every session. A cached redirect to the login page then keeps
// serving it even after the operator logs in again - the browser
// never asks the server at all.
func TestRedirectToLogin_SetsNoStore(t *testing.T) {
	// A server WITH authentication, so an unauthenticated request is
	// redirected rather than served.
	s := newTestServerWithAuth(t, &fakeClient{
		statusResp: &rpcpb.StatusResponse{ManagerNodeId: "brood"},
		listResp:   &rpcpb.ListVMsResponse{},
	}, "gjb", "secret")
	req := httptest.NewRequest(http.MethodGet, "/vms", nil)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if rec.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302", rec.Code)
	}
	if got := rec.Header().Get("Cache-Control"); !strings.Contains(got, "no-store") {
		t.Errorf("Cache-Control = %q on a login redirect, want it to contain no-store", got)
	}
}
