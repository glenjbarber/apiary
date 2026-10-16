package frontend

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestServer_GuidedJoinRedirectsToRealProcedure(t *testing.T) {
	s := newTestServer(t, &fakeClient{})
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/colony/join/guided", nil))

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusSeeOther, rec.Body.String())
	}
	if got := rec.Header().Get("Location"); got != "/machine#join-colony" {
		t.Errorf("Location = %q, want /machine#join-colony", got)
	}
}

func TestServer_GuidedJoinDoesNotAcceptFakeStepPosts(t *testing.T) {
	s := newTestServer(t, &fakeClient{})
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/colony/join/guided", nil))

	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusMethodNotAllowed, rec.Body.String())
	}
}
