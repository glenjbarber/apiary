// Tests for apiaryctl join-introduce's dial behavior (issue #45): this
// Comb has to default to TLS against a real TLS-only managerd, has to
// still be able to reach a genuinely plaintext one with --plaintext, and
// has to turn the resulting handshake EOF into a sentence an operator can
// act on rather than grpc-go's own "error reading server preface" text.

package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"io"
	"math/big"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	rpcpb "github.com/glenjbarber/apiary/api/rpc"
)

// stubManagerService is just enough of rpcpb.ManagerServiceServer to
// prove a dial actually completed an RPC round trip.
type stubManagerService struct {
	rpcpb.UnimplementedManagerServiceServer
}

func (stubManagerService) RequestJoinColony(ctx context.Context, req *rpcpb.RequestJoinColonyRequest) (*rpcpb.RequestJoinColonyResponse, error) {
	return &rpcpb.RequestJoinColonyResponse{RequestId: "req-1"}, nil
}

func startPlaintextManager(t *testing.T) string {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listening: %v", err)
	}
	srv := grpc.NewServer()
	rpcpb.RegisterManagerServiceServer(srv, stubManagerService{})
	go srv.Serve(lis)
	t.Cleanup(srv.Stop)
	return lis.Addr().String()
}

// selfSignedCert generates a certificate valid for 127.0.0.1, entirely
// in memory - this is a server-side-only test certificate, never written
// to disk, so nothing here needs to match hostcert's own fingerprint
// format or any of the join-handshake's own PEM files.
func selfSignedCert(t *testing.T) tls.Certificate {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generating key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "127.0.0.1"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &priv.PublicKey, priv)
	if err != nil {
		t.Fatalf("creating certificate: %v", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parsing certificate: %v", err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: priv, Leaf: cert}
}

func startTLSManager(t *testing.T) string {
	t.Helper()
	cert := selfSignedCert(t)
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listening: %v", err)
	}
	srv := grpc.NewServer(grpc.Creds(credentials.NewTLS(&tls.Config{Certificates: []tls.Certificate{cert}})))
	rpcpb.RegisterManagerServiceServer(srv, stubManagerService{})
	go srv.Serve(lis)
	t.Cleanup(srv.Stop)
	return lis.Addr().String()
}

// TestDialTargetManager_DefaultsToTLS is issue #45's acceptance case: a
// target serving TLS (as it does whenever pam_service is set) has to be
// dialed with TLS, with no flags at all, rather than the old unconditional
// plaintext dial. It is proven here the only way a test can prove a dial
// actually attempted TLS without reaching into grpc's internals: a
// self-signed test certificate, untrusted by the system pool, has to
// fail verification - a plaintext dial against this same server would
// instead fail at the HTTP/2 preface, a different error entirely.
func TestDialTargetManager_DefaultsToTLS(t *testing.T) {
	addr := startTLSManager(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	client, closeFn, err := dialTargetManager(ctx, addr, false, "")
	if err != nil {
		t.Fatalf("dialTargetManager: %v", err)
	}
	defer closeFn()
	_, rpcErr := client.RequestJoinColony(ctx, &rpcpb.RequestJoinColonyRequest{})
	if rpcErr == nil {
		t.Fatalf("expected the default TLS dial to fail certificate verification against an untrusted self-signed certificate, proving it actually attempted TLS rather than silently falling back to plaintext")
	}
	if !strings.Contains(rpcErr.Error(), "certificate") && !strings.Contains(rpcErr.Error(), "x509") {
		t.Errorf("expected a certificate-verification failure proving TLS was attempted, got: %v", rpcErr)
	}
}

// TestDialTargetManager_PlaintextOptIn is the pre-existing behavior this
// fix must not break: a Colony that genuinely serves no TLS still has to
// be joinable, now only with --plaintext.
func TestDialTargetManager_PlaintextOptIn(t *testing.T) {
	addr := startPlaintextManager(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	client, closeFn, err := dialTargetManager(ctx, addr, true, "")
	if err != nil {
		t.Fatalf("dialTargetManager: %v", err)
	}
	defer closeFn()
	if _, err := client.RequestJoinColony(ctx, &rpcpb.RequestJoinColonyRequest{}); err != nil {
		t.Fatalf("RequestJoinColony over the plaintext dial: %v", err)
	}
}

// TestDialTargetManager_PlaintextAgainstTLSTarget reproduces the issue's
// observed symptom end to end: a plaintext client against a real
// TLS-only managerd gets the preface EOF, and translateDialErr turns it
// into the sentence an operator needs instead of grpc-go's own text.
func TestDialTargetManager_PlaintextAgainstTLSTarget(t *testing.T) {
	addr := startTLSManager(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	client, closeFn, err := dialTargetManager(ctx, addr, true, "")
	if err != nil {
		t.Fatalf("dialTargetManager: %v", err)
	}
	defer closeFn()
	_, rpcErr := client.RequestJoinColony(ctx, &rpcpb.RequestJoinColonyRequest{})
	if rpcErr == nil {
		t.Fatalf("expected the plaintext dial against a TLS-only server to fail")
	}
	translated := translateDialErr(addr, true, rpcErr)
	if !strings.Contains(translated.Error(), "plaintext client dialing a TLS-only server") {
		t.Errorf("translateDialErr did not translate the preface EOF, got: %v", translated)
	}
	if !strings.Contains(translated.Error(), "Drop --plaintext") {
		t.Errorf("translateDialErr did not point at --plaintext, got: %v", translated)
	}
}

// TestTranslateDialErr_LeavesOtherErrorsAlone proves this is a
// translation for the one known failure shape, not a catch-all: a
// connection refused (no listener at all) is the common case of a typo'd
// --target, and rewriting it as a TLS hint would be actively misleading.
func TestTranslateDialErr_LeavesOtherErrorsAlone(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	client, closeFn, err := dialTargetManager(ctx, "127.0.0.1:1", true, "")
	if err != nil {
		t.Fatalf("dialTargetManager: %v", err)
	}
	defer closeFn()
	_, rpcErr := client.RequestJoinColony(ctx, &rpcpb.RequestJoinColonyRequest{})
	if rpcErr == nil {
		t.Fatalf("expected dialing a closed port to fail")
	}
	translated := translateDialErr("127.0.0.1:1", true, rpcErr)
	if translated != rpcErr {
		t.Errorf("translateDialErr rewrote an unrelated error: %v", translated)
	}
}

func TestJoinIntroduceHelpNamesSecondPINSource(t *testing.T) {
	oldStderr := os.Stderr
	read, write, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stderr = write
	runJoinIntroduce([]string{"--help"})
	write.Close()
	os.Stderr = oldStderr
	output, err := io.ReadAll(read)
	read.Close()
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"first-code     the 6-digit first code generated by and read from the",
		"JOINING Comb's screen",
		"second-pin     the 8-digit second PIN generated by the TARGET Colony;",
		"read it off the JOINING Comb's screen when available",
	} {
		if !strings.Contains(string(output), want) {
			t.Errorf("join-introduce help missing %q:\n%s", want, output)
		}
	}
}

func TestJoinIntroducePollInstructionsNameSecondPINSource(t *testing.T) {
	for _, want := range []string{
		"second PIN was generated by the TARGET Colony",
		"read off the JOINING\nComb's screen",
	} {
		if !strings.Contains(secondPINPollInstructions(), want) {
			t.Errorf("poll instructions missing %q: %s", want, secondPINPollInstructions())
		}
	}
}
