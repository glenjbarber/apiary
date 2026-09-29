package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	rpcpb "github.com/glenjbarber/apiary/api/rpc"
	"github.com/glenjbarber/apiary/internal/frontendconfig"
	"github.com/glenjbarber/apiary/internal/managerlink"
)

// fakeStatusClient embeds the (nil) ManagerServiceClient interface so
// it satisfies the full interface without stubbing every method by
// hand - only Status, the one checkPAMConfigured actually calls, is
// overridden.
type fakeStatusClient struct {
	rpcpb.ManagerServiceClient

	// errsThenSucceeds, if set, is returned in order on the first N
	// calls (N = len(errsThenSucceeds)) before every call after that
	// succeeds - lets a test model managerd coming up after a few
	// failed attempts.
	errsThenSucceeds []error
	calls            int

	resp *rpcpb.StatusResponse
}

func (f *fakeStatusClient) Status(context.Context, *rpcpb.StatusRequest, ...grpc.CallOption) (*rpcpb.StatusResponse, error) {
	i := f.calls
	f.calls++
	if i < len(f.errsThenSucceeds) {
		return nil, f.errsThenSucceeds[i]
	}
	if f.resp != nil {
		return f.resp, nil
	}
	return &rpcpb.StatusResponse{}, nil
}

func TestCheckPAMConfigured_SucceedsImmediately(t *testing.T) {
	client := &fakeStatusClient{resp: &rpcpb.StatusResponse{PamConfigured: true}}

	configured, err := checkPAMConfigured(client, 5, time.Millisecond)
	if err != nil {
		t.Fatalf("checkPAMConfigured() error: %v", err)
	}
	if !configured {
		t.Error("configured = false, want true")
	}
	if client.calls != 1 {
		t.Errorf("calls = %d, want 1 (no retries needed)", client.calls)
	}
}

// TestCheckPAMConfigured_RetriesThroughTransientErrors guards the exact
// live race this function exists to close: managerd briefly
// unreachable at frontend's own startup (e.g. still connecting to
// raftd's socket), then reachable moments later.
func TestCheckPAMConfigured_RetriesThroughTransientErrors(t *testing.T) {
	client := &fakeStatusClient{
		errsThenSucceeds: []error{errors.New("connection refused"), errors.New("connection refused")},
		resp:             &rpcpb.StatusResponse{PamConfigured: true},
	}

	configured, err := checkPAMConfigured(client, 5, time.Millisecond)
	if err != nil {
		t.Fatalf("checkPAMConfigured() error: %v", err)
	}
	if !configured {
		t.Error("configured = false, want true")
	}
	if client.calls != 3 {
		t.Errorf("calls = %d, want 3 (2 failures then a success)", client.calls)
	}
}

func TestCheckPAMConfigured_FallsBackAfterExhaustingAttempts(t *testing.T) {
	wantErr := errors.New("connection refused")
	client := &fakeStatusClient{errsThenSucceeds: []error{wantErr, wantErr, wantErr}}

	configured, err := checkPAMConfigured(client, 3, time.Millisecond)
	if err == nil {
		t.Fatal("checkPAMConfigured() error = nil, want the last attempt's error surfaced")
	}
	if configured {
		t.Error("configured = true, want false when every attempt failed")
	}
	if client.calls != 3 {
		t.Errorf("calls = %d, want exactly 3 (attempts), not more", client.calls)
	}
}

func TestCheckPAMConfigured_NotConfiguredIsNotAnError(t *testing.T) {
	client := &fakeStatusClient{resp: &rpcpb.StatusResponse{PamConfigured: false}}

	configured, err := checkPAMConfigured(client, 5, time.Millisecond)
	if err != nil {
		t.Fatalf("checkPAMConfigured() error: %v", err)
	}
	if configured {
		t.Error("configured = true, want false")
	}
}

func TestManagerAuthenticator_RefusesUnknownPAMState(t *testing.T) {
	client := &fakeStatusClient{errsThenSucceeds: []error{errors.New("connection refused")}}

	auth, err := managerAuthenticator(client, 1, time.Millisecond)
	if err == nil {
		t.Fatal("managerAuthenticator() error = nil, want failure when PAM state is unknown")
	}
	if auth != nil {
		t.Error("managerAuthenticator() returned an authenticator when PAM state is unknown")
	}
}

func TestManagerAuthenticator_PreservesExplicitNoLoginMode(t *testing.T) {
	client := &fakeStatusClient{resp: &rpcpb.StatusResponse{PamConfigured: false}}

	auth, err := managerAuthenticator(client, 1, time.Millisecond)
	if err != nil {
		t.Fatalf("managerAuthenticator() error: %v", err)
	}
	if auth != nil {
		t.Error("managerAuthenticator() returned an authenticator with PAM explicitly disabled")
	}
}

// --- manager link startup verification -------------------------------------
//
// No live managerd anywhere below: every "managerd" is an in-process gRPC
// server on a loopback listener, with a real TLS handshake where one is
// needed, because what is under test is precisely what happens on the
// wire. The two daemons must agree, so both this file and
// cmd/restshimd exercise the same three outcomes.

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
		ProcessName: "frontend",
		ConfigPath:  frontendconfig.DefaultPath,
		Timeout:     2 * time.Second,
	})
}

// TestCheckManagerLink_PlaintextConfigAgainstTLSManagerdRefusesToStart is
// the live failure at the place it belongs: before serving. frontend is
// configured manager_tls=false, managerd speaks TLS, and the operator gets
// the file, the value and the restart - not a failed render per page load,
// with nothing in either daemon's log saying the two files disagreed.
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
	for _, want := range []string{`"manager_tls": true`, frontendconfig.DefaultPath, "restart frontend", addr} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal %q does not mention %q", err.Error(), want)
		}
	}
	if len(logged) != 0 {
		t.Errorf("logged %q, want nothing: a refused start does not also claim to be serving degraded", logged)
	}
}

// TestCheckManagerLink_UntrustedCertificateRefusesToStart: the other
// permanent case. The scheme matches but the certificate does not verify,
// which is just as unfixable by retrying.
func TestCheckManagerLink_UntrustedCertificateRefusesToStart(t *testing.T) {
	certPEM, keyPEM := selfSignedCert(t)
	addr := startFakeManagerd(t, certPEM, keyPEM)
	// A CA file that did not sign the certificate above.
	otherCert, _ := selfSignedCert(t)

	err := checkManagerLink(context.Background(), newLink(t, addr, true, writeCA(t, otherCert)), 0,
		func(string, ...any) {})
	if err == nil {
		t.Fatal("checkManagerLink() = nil, want a refusal: the certificate does not verify")
	}
	if !managerlink.IsPermanent(err) {
		t.Errorf("checkManagerLink() error is not permanent, so it should not have been a refusal: %v", err)
	}
	if !strings.Contains(err.Error(), frontendconfig.DefaultPath) {
		t.Errorf("refusal %q does not name the file to edit", err.Error())
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
// frontend starts - but says plainly that the TLS setting is unverified,
// rather than implying it was checked. The two daemons are co-located and
// either can win the startup race, so refusing here would be wrong.
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
