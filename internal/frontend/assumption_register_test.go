package frontend

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	rpcpb "github.com/glenjbarber/apiary/api/rpc"
)

func TestServer_AssumptionRegisterPageRendersLocalClaim(t *testing.T) {
	client := &fakeClient{assumptionClaimsResp: &rpcpb.ListAssumptionClaimsResponse{
		Claims: []*rpcpb.AssumptionClaim{{
			Id: "route-uplink", Statement: "NAT owns the default route",
			Owner: "operations", Scope: "hive:apiarium",
			Evidence: "route -n get default", ExpiresAtUnix: time.Now().Add(time.Hour).Unix(),
		}},
	}}
	s := newTestServer(t, client)
	req := httptest.NewRequest(http.MethodGet, "/assumption-register", nil)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	for _, want := range []string{"Assumption Register", "route-uplink", "operations", "hive:apiarium", "route -n get default"} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Errorf("page missing %q: %s", want, rec.Body.String())
		}
	}
}
