package frontend

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	rpcpb "github.com/glenjbarber/apiary/api/rpc"
)

// followRedirect issues the GET a browser would make against a 302's own
// Location header - used throughout this file because
// handleConvertStandaloneToJoiner (issue #46) never renders its own
// result directly; it always redirects to a GET first, specifically so a
// page reload re-fetches instead of resubmitting the POST and restarting
// raftd again.
func followRedirect(t *testing.T, s *Server, rec *httptest.ResponseRecorder) string {
	t.Helper()
	if rec.Code != http.StatusFound {
		t.Fatalf("expected a redirect (302 Found), got %d: %s", rec.Code, rec.Body.String())
	}
	loc := rec.Header().Get("Location")
	if loc == "" {
		t.Fatalf("redirect carried no Location header")
	}
	req := httptest.NewRequest(http.MethodGet, loc, nil)
	rec2 := httptest.NewRecorder()
	s.ServeHTTP(rec2, req)
	return rec2.Body.String()
}

func TestServer_ConvertStandaloneToJoiner_ForwardsFormValues(t *testing.T) {
	client := &fakeClient{
		convertStandaloneToJoinerResp: &rpcpb.ConvertStandaloneToJoinerResponse{
			BackupDataDir: "/var/db/apiary/raftd.reset-backup-123", JoinRequestId: "jreq-1", JoinRequestCode: "654321",
		},
	}
	s := newTestServer(t, client)

	form := url.Values{
		"target_managerd_host": {"10.90.0.1"},
		"raft_bind_host":       {"10.90.0.20"},
		"confirm_phrase":       {"yes-convert-to-joiner"},
	}
	req := httptest.NewRequest(http.MethodPost, "/machine/convert-to-joiner", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	// ADR: the port is fixed server-side (fixedport.go), never taken
	// from the form - so the submitted host-only values must come back
	// joined with managerd/raftd's own fixed ports, not verbatim.
	if got := client.lastConvertStandaloneToJoinerReq; got.GetTargetManagerdAddress() != "10.90.0.1:17700" ||
		got.GetRaftBind() != "10.90.0.20:17600" || got.GetConfirmPhrase() != "yes-convert-to-joiner" {
		t.Fatalf("RPC request = %+v, want the submitted hosts joined with the fixed managerd/raftd ports", got)
	}

	body := followRedirect(t, s, rec)
	if !strings.Contains(body, "654321") || !strings.Contains(body, "raftd.reset-backup-123") {
		t.Errorf("response missing join code/backup path, got: %s", body)
	}
}

// TestServer_ConvertStandaloneToJoiner_ReloadIsIdempotent is issue #46's
// own regression: a reload of the result page (a GET of the same
// redirect Location, exactly what a browser's reload button sends) must
// not call ConvertStandaloneToJoiner a second time.
func TestServer_ConvertStandaloneToJoiner_ReloadIsIdempotent(t *testing.T) {
	client := &fakeClient{
		convertStandaloneToJoinerResp: &rpcpb.ConvertStandaloneToJoinerResponse{
			BackupDataDir: "/var/db/apiary/raftd.reset-backup-123", JoinRequestId: "jreq-1", JoinRequestCode: "654321",
		},
	}
	s := newTestServer(t, client)

	form := url.Values{
		"target_managerd_host": {"10.90.0.1"},
		"raft_bind_host":       {"10.90.0.20"},
		"confirm_phrase":       {"yes-convert-to-joiner"},
	}
	req := httptest.NewRequest(http.MethodPost, "/machine/convert-to-joiner", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	loc := rec.Header().Get("Location")

	client.convertStandaloneToJoinerCalls = 0
	// Two reloads of the same Location, as a browser's reload button
	// would send them.
	for i := 0; i < 2; i++ {
		req := httptest.NewRequest(http.MethodGet, loc, nil)
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, req)
		body := rec.Body.String()
		if !strings.Contains(body, "654321") || !strings.Contains(body, "raftd.reset-backup-123") {
			t.Fatalf("reload %d: response missing join code/backup path, got: %s", i, body)
		}
	}
	if client.convertStandaloneToJoinerCalls != 0 {
		t.Errorf("ConvertStandaloneToJoiner was called %d times by GET reloads, want 0", client.convertStandaloneToJoinerCalls)
	}
}

func TestServer_ConvertStandaloneToJoiner_ErrorRendersWithoutResult(t *testing.T) {
	client := &fakeClient{
		convertStandaloneToJoinerResp: &rpcpb.ConvertStandaloneToJoinerResponse{
			Error: `confirm_phrase "wrong" does not match the required confirmation phrase "yes-convert-to-joiner" - nothing was done`,
		},
	}
	s := newTestServer(t, client)

	form := url.Values{
		"target_managerd_address": {"10.90.0.1:17700"},
		"raft_bind":               {"10.90.0.20:17600"},
		"confirm_phrase":          {"wrong"},
	}
	req := httptest.NewRequest(http.MethodPost, "/machine/convert-to-joiner", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	body := followRedirect(t, s, rec)
	if !strings.Contains(body, "does not match the required confirmation phrase") {
		t.Errorf("response missing the server's own error, got: %s", body)
	}
	if strings.Contains(body, "Relay this code") {
		t.Errorf("response rendered a success result alongside an error: %s", body)
	}
}

// TestRaftdRestartingNotice proves the translation (issue #46) fires only
// for the specific raftd-socket-restarting symptom, and leaves every
// other error exactly as reported - the same shape as
// explainPeerLinkError's own test coverage in cluster_overview_test.go.
func TestRaftdRestartingNotice(t *testing.T) {
	const socketErr = `dial unix /var/run/apiary/raftd.sock: connect: no such file or directory`
	got := raftdRestartingNotice(socketErr)
	if !strings.Contains(got, "raftd is restarting") {
		t.Errorf("raftdRestartingNotice(%q) = %q, want it translated", socketErr, got)
	}
	if !strings.Contains(got, socketErr) {
		t.Errorf("raftdRestartingNotice(%q) = %q, want the original error preserved", socketErr, got)
	}

	const unrelatedErr = "permission denied"
	if got := raftdRestartingNotice(unrelatedErr); got != unrelatedErr {
		t.Errorf("raftdRestartingNotice(%q) = %q, want it unchanged", unrelatedErr, got)
	}
}
