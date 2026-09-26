package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	rpcpb "github.com/glenjbarber/apiary/api/rpc"
	"github.com/glenjbarber/apiary/internal/managerlink"
	"github.com/glenjbarber/apiary/internal/restshim"
	"github.com/glenjbarber/apiary/internal/restshimdconfig"
	"github.com/glenjbarber/apiary/internal/tlsdial"
)

func TestRequireTLSPair_BothSetIsFine(t *testing.T) {
	if err := requireTLSPair("/path/cert.pem", "/path/key.pem"); err != nil {
		t.Errorf("requireTLSPair() error = %v, want nil when both are set", err)
	}
}

func TestRequireTLSPair_OnlyCertIsAnError(t *testing.T) {
	if err := requireTLSPair("/path/cert.pem", ""); err == nil {
		t.Error("requireTLSPair() error = nil, want an error when only tls_cert is set")
	}
}

func TestRequireTLSPair_OnlyKeyIsAnError(t *testing.T) {
	if err := requireTLSPair("", "/path/key.pem"); err == nil {
		t.Error("requireTLSPair() error = nil, want an error when only tls_key is set")
	}
}

// --- manager link startup verification -------------------------------------
//
// No live network and no live managerd anywhere below: every "managerd" is
// an in-process gRPC server on a loopback listener, with a real TLS
// handshake where one is needed, because the thing under test is precisely
// what happens on the wire.

func selfSignedCert(t *testing.T) (certPEM, keyPEM []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey() error: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{Organization: []string{"apiary-test"}},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
		DNSNames:              []string{"localhost"},
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("CreateCertificate() error: %v", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatalf("MarshalECPrivateKey() error: %v", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
}

// startFakeManagerd serves gRPC on 127.0.0.1:0, over TLS when certPEM is
// non-empty, and returns its address. No ManagerService is registered: the
// startup check never gets far enough to need one, and neither does a
// wrong-scheme client.
func startFakeManagerd(t *testing.T, certPEM, keyPEM []byte) string {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen() error: %v", err)
	}
	var opts []grpc.ServerOption
	if len(certPEM) > 0 {
		cert, err := tls.X509KeyPair(certPEM, keyPEM)
		if err != nil {
			t.Fatalf("X509KeyPair() error: %v", err)
		}
		opts = append(opts, grpc.Creds(credentials.NewTLS(&tls.Config{Certificates: []tls.Certificate{cert}})))
	}
	srv := grpc.NewServer(opts...)
	go srv.Serve(lis)
	t.Cleanup(srv.Stop)
	return lis.Addr().String()
}

func writeCA(t *testing.T, certPEM []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "managerd-ca.pem")
	if err := os.WriteFile(path, certPEM, 0o600); err != nil {
		t.Fatalf("WriteFile() error: %v", err)
	}
	return path
}

func newLink(t *testing.T, addr string, useTLS bool, caFile string) *managerlink.Checker {
	t.Helper()
	return managerlink.New(managerlink.Config{
		Addr:        addr,
		UseTLS:      useTLS,
		CAFile:      caFile,
		ProcessName: "restshimd",
		ConfigPath:  restshimdconfig.DefaultPath,
		Timeout:     2 * time.Second,
	})
}

// TestCheckManagerLink_PlaintextConfigAgainstTLSManagerdRefusesToStart is
// the live failure at the place it belongs: before serving. restshimd is
// configured manager_tls=false, managerd speaks TLS, and the operator gets
// the file, the key, the value and the restart - not a 502 per request.
func TestCheckManagerLink_PlaintextConfigAgainstTLSManagerdRefusesToStart(t *testing.T) {
	certPEM, keyPEM := selfSignedCert(t)
	addr := startFakeManagerd(t, certPEM, keyPEM)

	var logged []string
	err := checkManagerLink(context.Background(), newLink(t, addr, false, ""), 0,
		func(format string, args ...any) { logged = append(logged, fmt.Sprintf(format, args...)) })

	if err == nil {
		t.Fatal("checkManagerLink() = nil, want a refusal: the configured scheme is not what managerd speaks")
	}
	if !managerlink.IsPermanent(err) {
		t.Errorf("checkManagerLink() error is not permanent, so it should not have been a refusal: %v", err)
	}
	for _, want := range []string{`"manager_tls": true`, restshimdconfig.DefaultPath, "restart restshimd", addr} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal %q does not mention %q", err.Error(), want)
		}
	}
	if len(logged) != 0 {
		t.Errorf("logged %q, want nothing: a refused start does not also claim to be serving degraded", logged)
	}
}

// TestCheckManagerLink_MatchingTLSIsAccepted: the other half - a correct
// config must start silently.
func TestCheckManagerLink_MatchingTLSIsAccepted(t *testing.T) {
	certPEM, keyPEM := selfSignedCert(t)
	addr := startFakeManagerd(t, certPEM, keyPEM)

	var logged []string
	err := checkManagerLink(context.Background(), newLink(t, addr, true, writeCA(t, certPEM)), 0,
		func(format string, args ...any) { logged = append(logged, fmt.Sprintf(format, args...)) })
	if err != nil {
		t.Fatalf("checkManagerLink() error = %v, want nil for a matching, verifiable config", err)
	}
	if len(logged) != 0 {
		t.Errorf("logged %q, want silence for a verified link", logged)
	}
}

// TestCheckManagerLink_UnreachableManagerdStartsDegradedButSaysSo: a
// managerd that is not up yet is not evidence of a misconfiguration, so
// restshimd starts - but says plainly that the TLS setting is unverified,
// rather than implying it was checked.
func TestCheckManagerLink_UnreachableManagerdStartsDegradedButSaysSo(t *testing.T) {
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen() error: %v", err)
	}
	addr := lis.Addr().String()
	lis.Close()

	var logged []string
	err = checkManagerLink(context.Background(), newLink(t, addr, true, ""), 0,
		func(format string, args ...any) { logged = append(logged, fmt.Sprintf(format, args...)) })
	if err != nil {
		t.Fatalf("checkManagerLink() error = %v, want nil: an unreachable managerd is not a refusal", err)
	}
	joined := strings.Join(logged, "\n")
	for _, want := range []string{"WARNING", "DEGRADED", "UNVERIFIED"} {
		if !strings.Contains(joined, want) {
			t.Errorf("degraded-start log %q does not say %q", joined, want)
		}
	}
}

// TestCheckManagerLink_VerifiedLinkIsSilent: nothing to say, so say
// nothing - a daemon that logs a line about its own health on every start
// trains its operator to ignore it.
func TestCheckManagerLink_VerifiedLinkIsSilent(t *testing.T) {
	addr := startFakeManagerd(t, nil, nil)
	var logged []string
	if err := checkManagerLink(context.Background(), newLink(t, addr, false, ""), 0,
		func(format string, args ...any) { logged = append(logged, fmt.Sprintf(format, args...)) }); err != nil {
		t.Fatalf("checkManagerLink() error = %v, want nil", err)
	}
	if len(logged) != 0 {
		t.Errorf("logged %q, want silence", logged)
	}
}

// TestPerRequestErrorNamesTLSInsteadOfBare502 is the end-to-end version of
// the same bug with nothing faked: a real plaintext gRPC dial against a
// real TLS gRPC server, through the same restshim wiring cmd/restshimd
// builds. The caller must get a named, non-retryable error rather than the
// 502 that made this look transient.
func TestPerRequestErrorNamesTLSInsteadOfBare502(t *testing.T) {
	certPEM, keyPEM := selfSignedCert(t)
	addr := startFakeManagerd(t, certPEM, keyPEM)
	link := newLink(t, addr, false, "")

	// Exactly what run() builds: a plaintext dial to a TLS server, which
	// grpc.NewClient accepts without complaint because it never connects.
	dialOpt, err := tlsdial.ManagerDialOption(false, "", "")
	if err != nil {
		t.Fatalf("tlsdial.ManagerDialOption() error: %v", err)
	}
	conn, err := grpc.NewClient(addr, dialOpt)
	if err != nil {
		t.Fatalf("grpc.NewClient() error: %v", err)
	}
	defer conn.Close()

	srv := restshim.NewServer(rpcpb.NewManagerServiceClient(conn),
		restshim.WithDiagnosis(func(err error) restshim.LinkDiagnosis {
			d := link.Explain(err)
			return restshim.LinkDiagnosis{Class: d.Class, Detail: d.Detail, Permanent: d.Permanent}
		}))

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/status", nil))

	if rec.Code == http.StatusBadGateway {
		t.Fatalf("status = 502 with no diagnosis attached; body=%s - a caller cannot tell a "+
			"permanent misconfiguration from an outage if both look the same", rec.Body.String())
	}
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 for a permanent misconfiguration; body=%s", rec.Code, rec.Body.String())
	}
	var body struct {
		Error      string `json:"error"`
		ErrorClass string `json:"error_class"`
		Hint       string `json:"hint"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshaling body: %v", err)
	}
	if body.ErrorClass != managerlink.ClassSchemeMismatch {
		t.Errorf("error_class = %q, want %q", body.ErrorClass, managerlink.ClassSchemeMismatch)
	}
	if !strings.Contains(body.Hint, `"manager_tls": true`) {
		t.Errorf("hint = %q, want it to name the exact fix", body.Hint)
	}
	// The raw transport error is preserved, so the operator who already has
	// the "error reading server preface" log line can connect the two.
	if !strings.Contains(body.Error, "error reading server preface") {
		t.Errorf("error = %q, want the underlying gRPC error preserved", body.Error)
	}
}
