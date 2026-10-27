package frontend

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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

	proxy, err := newAPIProxy(restshimdUpstream{address: upstream.URL})
	if err != nil {
		t.Fatalf("newAPIProxy() error: %v", err)
	}
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
	proxy, err := newAPIProxy(restshimdUpstream{address: "127.0.0.1:1"})
	if err != nil {
		t.Fatalf("newAPIProxy() error: %v", err)
	}
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

// TestAPIProxyTalksTLSWhenRestshimdServesTLS is the regression test for
// the defect this branch would otherwise have shipped: cmd/restshimd
// serves HTTPS whenever its config sets tls_cert/tls_key (the shipped
// restshimd.json.sample sets both), and a proxy that dialled it over
// plaintext got "Client sent an HTTP request to an HTTPS server" for
// every single request.
//
// The upstream here is a real TLS server with a self-signed
// certificate, and the proxy is built exactly as it is in production:
// useTLS true, with the certificate's PEM file named as the trust
// anchor.
func TestAPIProxyTalksTLSWhenRestshimdServesTLS(t *testing.T) {
	dir := t.TempDir()
	certPath := filepath.Join(dir, "cert.pem")
	keyPath := filepath.Join(dir, "key.pem")

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generating key: %v", err)
	}
	template := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "localhost"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
		KeyUsage:     x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, &template, &template, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("creating certificate: %v", err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	if err := os.WriteFile(certPath, certPEM, 0o600); err != nil {
		t.Fatalf("writing certificate: %v", err)
	}
	if err := os.WriteFile(keyPath, keyPEM, 0o600); err != nil {
		t.Fatalf("writing key: %v", err)
	}

	listener, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{
		Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}},
		MinVersion:   tls.VersionTLS12,
	})
	if err != nil {
		t.Fatalf("listening: %v", err)
	}
	defer listener.Close()

	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Path; got != "/v1/status" {
			t.Errorf("upstream path = %q, want /v1/status", got)
		}
		w.WriteHeader(http.StatusNoContent)
	})}
	go func() { _ = server.Serve(listener) }()
	defer server.Close()

	proxy, err := newAPIProxy(restshimdUpstream{
		address: listener.Addr().String(),
		useTLS:  true,
		caFile:  certPath,
	})
	if err != nil {
		t.Fatalf("newAPIProxy() error: %v", err)
	}
	req := httptest.NewRequest(http.MethodGet, "/api/v1/status", nil)
	req.Header.Set("Authorization", "Bearer apk_client_key")
	rec := httptest.NewRecorder()
	proxy.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; body=%s", rec.Code, rec.Body.String())
	}
}

// TestNewAPIProxyReportsAnUnusableRestshimdCertificate covers the other
// half of the contract: a restshimd certificate that cannot be loaded is
// reported at startup rather than turning into a handshake failure on
// the first proxied request.
func TestNewAPIProxyReportsAnUnusableRestshimdCertificate(t *testing.T) {
	_, err := newAPIProxy(restshimdUpstream{
		address: "127.0.0.1:8081",
		useTLS:  true,
		caFile:  filepath.Join(t.TempDir(), "absent.pem"),
	})
	if err == nil {
		t.Fatal("newAPIProxy() error = nil, want an error for an unreadable certificate")
	}
	if !strings.Contains(err.Error(), "absent.pem") {
		t.Errorf("error = %q, want it to name the certificate path", err)
	}
}
