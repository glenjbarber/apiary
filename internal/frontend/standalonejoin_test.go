package frontend

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	rpcpb "github.com/glenjbarber/apiary/api/rpc"
)

func TestServer_ConvertStandaloneToJoiner_ForwardsFormValues(t *testing.T) {
	client := &fakeClient{
		convertStandaloneToJoinerResp: &rpcpb.ConvertStandaloneToJoinerResponse{
			BackupDataDir: "/var/db/apiary/raftd.reset-backup-123", JoinRequestId: "jreq-1", JoinRequestCode: "654321",
		},
	}
	s := newTestServer(t, client)

	form := url.Values{
		"target_managerd_address": {"10.90.0.1:17700"},
		"raft_bind":               {"10.90.0.20:17600"},
		"confirm_phrase":          {"yes-convert-to-joiner"},
	}
	req := httptest.NewRequest(http.MethodPost, "/machine/convert-to-joiner", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if got := client.lastConvertStandaloneToJoinerReq; got.GetTargetManagerdAddress() != "10.90.0.1:17700" ||
		got.GetRaftBind() != "10.90.0.20:17600" || got.GetConfirmPhrase() != "yes-convert-to-joiner" {
		t.Fatalf("RPC request = %+v, want the submitted form values verbatim", got)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "654321") || !strings.Contains(body, "raftd.reset-backup-123") {
		t.Errorf("response missing join code/backup path, got: %s", body)
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

	body := rec.Body.String()
	if !strings.Contains(body, "does not match the required confirmation phrase") {
		t.Errorf("response missing the server's own error, got: %s", body)
	}
	if strings.Contains(body, "Relay this code") {
		t.Errorf("response rendered a success result alongside an error: %s", body)
	}
}
