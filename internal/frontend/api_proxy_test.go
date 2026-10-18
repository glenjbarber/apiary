package frontend

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	rpcpb "github.com/glenjbarber/apiary/api/rpc"
)

func TestAPIProxyRoutesHaveManagerRPCAndAuthorization(t *testing.T) {
	methods := make(map[string]bool)
	for _, method := range rpcpb.ManagerService_ServiceDesc.Methods {
		methods[method.MethodName] = true
	}
	for _, stream := range rpcpb.ManagerService_ServiceDesc.Streams {
		methods[stream.StreamName] = true
	}
	for _, route := range apiProxyRoutes {
		method := route.rpcMethod
		if !methods[method] {
			t.Errorf("allow-listed route %q maps to unknown ManagerService RPC %q", route.pattern, method)
		}
		if !hasAuthorizationPolicy(method) {
			t.Errorf("allow-listed route %q maps to RPC %q without an explicit role requirement", route.pattern, method)
		}
	}
}

func TestAPIProxyStripsPrefixAndCookieAndForwardsBearer(t *testing.T) {
	var gotPath, gotQuery, gotAuthorization, gotCookie string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotQuery = r.URL.RawQuery
		gotAuthorization = r.Header.Get("Authorization")
		gotCookie = r.Header.Get("Cookie")
		w.WriteHeader(http.StatusNoContent)
	}))
	defer upstream.Close()

	proxy := newAPIProxy(upstream.URL)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/status?detail=true", nil)
	req.Header.Set("Authorization", "Bearer apk_client_key")
	req.Header.Set("Cookie", "apiary_session=browser-session")
	rec := httptest.NewRecorder()
	proxy.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; body=%s", rec.Code, rec.Body.String())
	}
	if gotPath != "/v1/status" || gotQuery != "detail=true" {
		t.Errorf("upstream URL = %q?%q, want /v1/status?detail=true", gotPath, gotQuery)
	}
	if gotAuthorization != "Bearer apk_client_key" {
		t.Errorf("upstream Authorization = %q, want original bearer header", gotAuthorization)
	}
	if gotCookie != "" {
		t.Errorf("upstream Cookie = %q, want cookie stripped", gotCookie)
	}
}

func TestAPIProxyRejectsUnlistedRoute(t *testing.T) {
	proxy := newAPIProxy("http://127.0.0.1:1")
	req := httptest.NewRequest(http.MethodGet, "/api/v1/not-registered", nil)
	rec := httptest.NewRecorder()
	proxy.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

func TestServerAPIProxyRequiresBearerEvenWithValidSessionCookie(t *testing.T) {
	s := newTestServerWithAuth(t, &fakeClient{}, "operator", "secret")
	login := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader("username=operator&password=secret"))
	login.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	loginRec := httptest.NewRecorder()
	s.ServeHTTP(loginRec, login)
	if loginRec.Code != http.StatusFound {
		t.Fatalf("login status = %d, want 302; body=%s", loginRec.Code, loginRec.Body.String())
	}
	cookies := loginRec.Result().Cookies()
	if len(cookies) == 0 {
		t.Fatal("login did not issue a session cookie")
	}

	upstreamCalled := false
	s.SetAPIProxy(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalled = true
		w.WriteHeader(http.StatusNoContent)
	}))
	req := httptest.NewRequest(http.MethodPost, "/api/v1/vms", nil)
	req.AddCookie(cookies[0])
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401; body=%s", rec.Code, rec.Body.String())
	}
	if upstreamCalled {
		t.Fatal("proxy reached restshimd without a bearer header")
	}
}

func TestServerAPIDisabledReturnsNotFound(t *testing.T) {
	s := newTestServer(t, &fakeClient{})
	req := httptest.NewRequest(http.MethodGet, "/api/v1/status", nil)
	req.Header.Set("Authorization", "Bearer apk_test")
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}
