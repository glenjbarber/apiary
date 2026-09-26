package managerlink

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
	"io"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

// pipeDialer returns a DialFunc whose connections are served by peer. A
// fresh net.Pipe per dial, and peer is handed the server end, is enough to
// drive Detect through every answer it can get - including "closed without
// answering", which no real loopback listener produces on demand and which
// is exactly the ambiguous case the code must refuse to round.
func pipeDialer(peer func(conn net.Conn)) DialFunc {
	return func(ctx context.Context, _, _ string) (net.Conn, error) {
		client, server := net.Pipe()
		go peer(server)
		return client, nil
	}
}

// writeFirstByte drains the probe's preface, then answers with b - the
// first byte of whatever the fake peer "says" - and closes.
func writeFirstByte(b byte) func(net.Conn) {
	return func(conn net.Conn) {
		buf := make([]byte, 24)
		io.ReadFull(conn, buf)
		conn.Write([]byte{b})
		conn.Close()
	}
}

func TestDetect_TLSRecordTypeIsIdentifiedAsTLS(t *testing.T) {
	// 0x15 is a TLS alert record, 0x16 a handshake record: a TLS listener
	// answering a plaintext HTTP/2 preface. This is the positive
	// identification, and it needs no string matching to make it.
	for _, b := range []byte{0x14, 0x15, 0x16, 0x17} {
		scheme, err := detectPreface(context.Background(), "test:1", pipeDialer(writeFirstByte(b)), time.Second)
		if err != nil {
			t.Fatalf("detectPreface() with first byte %#x error: %v", b, err)
		}
		if scheme != SchemeTLS {
			t.Errorf("detectPreface() with first byte %#x = %q, want %q", b, scheme, SchemeTLS)
		}
	}
}

func TestDetect_NonTLSRecordTypeIsIdentifiedAsPlaintext(t *testing.T) {
	// 0x00 is the high byte of an HTTP/2 frame length and 0x04 is the
	// SETTINGS frame type: what a plaintext gRPC server answers a preface
	// with. Nothing else is a legal first byte for a TLS listener, so
	// "not a TLS record type" is a sound answer here.
	for _, b := range []byte{0x00, 0x04, 0x07, 'H'} {
		scheme, err := detectPreface(context.Background(), "test:1", pipeDialer(writeFirstByte(b)), time.Second)
		if err != nil {
			t.Fatalf("detectPreface() with first byte %#x error: %v", b, err)
		}
		if scheme != SchemePlaintext {
			t.Errorf("detectPreface() with first byte %#x = %q, want %q", b, scheme, SchemePlaintext)
		}
	}
}

func TestDetect_SilenceIsUnknownFromEitherDirection(t *testing.T) {
	// A peer that never answers is reported as unknown by both probes,
	// and Detect does not round that toward either scheme: an operator
	// must never have "the config is wrong" asserted on the strength of a
	// peer that said nothing.
	dial := pipeDialer(func(conn net.Conn) {
		buf := make([]byte, 64)
		io.ReadFull(conn, buf)
		conn.Close()
	})
	scheme, err := Detect(context.Background(), "test:1", dial, time.Second)
	if scheme != SchemeUnknown {
		t.Errorf("Detect() = %q, want %q for a peer that answered nothing", scheme, SchemeUnknown)
	}
	if err == nil {
		t.Error("Detect() error = nil, want an error naming the unanswered probe")
	}
}

// TestDetect_TLSListenerThatAnswersSilenceIsResolvedByTheReverseProbe is
// the measured behaviour this design exists for: a Go gRPC server with TLS
// enabled answers the HTTP/2 preface with a clean EOF and no alert record,
// so a preface-only probe reports the live failure as "unknown". The
// reverse probe - a real ClientHello - resolves it.
func TestDetect_TLSListenerThatAnswersSilenceIsResolvedByTheReverseProbe(t *testing.T) {
	certPEM, keyPEM := selfSignedFor(t)
	srv := startGRPCServer(t, certPEM, keyPEM)

	// Documenting the premise: the preface alone is not enough.
	scheme, err := detectPreface(context.Background(), srv.addr, nil, 2*time.Second)
	if scheme != SchemeUnknown {
		t.Fatalf("detectPreface() against a Go TLS gRPC server = %q, want %q - if this ever starts "+
			"returning a scheme, the reverse probe in Detect may no longer be what makes this work",
			scheme, SchemeUnknown)
	}
	if err == nil {
		t.Error("detectPreface() error = nil, want the EOF it actually gets")
	}

	scheme, err = detectClientHello(context.Background(), srv.addr, nil, 2*time.Second)
	if err != nil || scheme != SchemeTLS {
		t.Errorf("detectClientHello() = (%q, %v), want (%q, nil)", scheme, err, SchemeTLS)
	}
	scheme, err = Detect(context.Background(), srv.addr, nil, 2*time.Second)
	if err != nil || scheme != SchemeTLS {
		t.Errorf("Detect() = (%q, %v), want (%q, nil)", scheme, err, SchemeTLS)
	}
}

func TestDetect_PlaintextServerResolvesTheReverseProbeToo(t *testing.T) {
	// The reverse probe must not call a plaintext peer a TLS server: a
	// plaintext gRPC server answers the ClientHello with an HTTP/2 GOAWAY,
	// which is a non-TLS record and therefore a definitive "not TLS".
	srv := startGRPCServer(t, nil, nil)
	scheme, err := detectClientHello(context.Background(), srv.addr, nil, 2*time.Second)
	if err != nil || scheme != SchemePlaintext {
		t.Errorf("detectClientHello() against a plaintext server = (%q, %v), want (%q, nil)", scheme, err, SchemePlaintext)
	}
}

func TestDetect_UnreachableIsUnknownWithTheDialError(t *testing.T) {
	dial := func(context.Context, string, string) (net.Conn, error) {
		return nil, errConnRefused
	}
	scheme, err := Detect(context.Background(), "10.90.0.94:17700", dial, time.Second)
	if scheme != SchemeUnknown {
		t.Errorf("Detect() = %q, want %q", scheme, SchemeUnknown)
	}
	if err == nil || !strings.Contains(err.Error(), "10.90.0.94:17700") {
		t.Errorf("Detect() error = %v, want one naming the address it could not reach", err)
	}
}

var errConnRefused = errors.New("connect: connection refused")

// selfSignedFor returns a certificate/key PEM pair valid for 127.0.0.1
// (as both a DNS name and an IP SAN, so a handshake against a loopback
// address verifies with no server_name override), generated fresh per test.
func selfSignedFor(t *testing.T) (certPEM, keyPEM []byte) {
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
	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	return certPEM, keyPEM
}

// writePEM writes content to a temp file and returns its path, cleaned up
// with the test.
func writePEM(t *testing.T, name string, content []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatalf("WriteFile(%s) error: %v", name, err)
	}
	return path
}

// grpcServer is a real in-process gRPC server on a real loopback
// listener - no live network, no live managerd, but the same wire
// behaviour (HTTP/2 preface, TLS record types, TLS handshake) that the
// probe reasons about.
type grpcServer struct{ addr string }

// startGRPCServer serves gRPC on 127.0.0.1:0, with TLS when certPEM is
// non-empty.
func startGRPCServer(t *testing.T, certPEM, keyPEM []byte) *grpcServer {
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
	return &grpcServer{addr: lis.Addr().String()}
}

func newChecker(t *testing.T, addr string, useTLS bool, caFile string) *Checker {
	t.Helper()
	return New(Config{
		Addr:        addr,
		UseTLS:      useTLS,
		CAFile:      caFile,
		ProcessName: "restshimd",
		ConfigPath:  "/usr/local/etc/apiary/restshimd.json",
		Timeout:     2 * time.Second,
	})
}

// TestVerify_PlaintextConfigAgainstTLSServerIsTheLiveFailure is the whole
// bug, end to end and with no live network: a plaintext restshimd config
// pointed at a TLS managerd, detected at startup, naming the fix.
func TestVerify_PlaintextConfigAgainstTLSServerIsTheLiveFailure(t *testing.T) {
	certPEM, keyPEM := selfSignedFor(t)
	srv := startGRPCServer(t, certPEM, keyPEM)
	c := newChecker(t, srv.addr, false, "")

	err := c.Verify(context.Background(), 0)
	if err == nil {
		t.Fatal("Verify() = nil, want a mismatch: the config says plaintext and the server speaks TLS")
	}
	var mismatch *MismatchError
	if !errors.As(err, &mismatch) {
		t.Fatalf("Verify() error = %T (%v), want *MismatchError", err, err)
	}
	if mismatch.Detected != SchemeTLS || mismatch.Configured != SchemePlaintext {
		t.Errorf("mismatch = detected %q / configured %q, want %q / %q",
			mismatch.Detected, mismatch.Configured, SchemeTLS, SchemePlaintext)
	}
	if !IsPermanent(err) {
		t.Error("IsPermanent(mismatch) = false, want true: no amount of retrying fixes a wrong scheme")
	}
	// The message has to be actionable on its own - an operator reading
	// only this line should know which file, which key, which value.
	for _, want := range []string{
		"manager_tls", "true", "/usr/local/etc/apiary/restshimd.json", "restart restshimd",
		serverPrefaceEOF, srv.addr, "502",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("Verify() error %q does not mention %q", err.Error(), want)
		}
	}
}

// TestVerify_TLSConfigAgainstTLSServerIsAccepted covers the other half: a
// correct config must not be refused, including a real certificate
// handshake against a real server rather than just a probe.
func TestVerify_TLSConfigAgainstTLSServerIsAccepted(t *testing.T) {
	certPEM, keyPEM := selfSignedFor(t)
	srv := startGRPCServer(t, certPEM, keyPEM)
	ca := writePEM(t, "ca.pem", certPEM)

	if err := newChecker(t, srv.addr, true, ca).Verify(context.Background(), 0); err != nil {
		t.Fatalf("Verify() error = %v, want nil for a matching TLS config and a verifiable certificate", err)
	}
}

func TestVerify_TLSConfigAgainstPlaintextServerIsAMismatch(t *testing.T) {
	srv := startGRPCServer(t, nil, nil)
	err := newChecker(t, srv.addr, true, "").Verify(context.Background(), 0)
	var mismatch *MismatchError
	if !errors.As(err, &mismatch) {
		t.Fatalf("Verify() error = %T (%v), want *MismatchError", err, err)
	}
	if mismatch.Detected != SchemePlaintext || mismatch.Configured != SchemeTLS {
		t.Errorf("mismatch = detected %q / configured %q, want %q / %q",
			mismatch.Detected, mismatch.Configured, SchemePlaintext, SchemeTLS)
	}
	for _, want := range []string{`"manager_tls": false`, "no TLS handshake can ever complete"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("Verify() error %q does not mention %q", err.Error(), want)
		}
	}
}

func TestVerify_PlaintextConfigAgainstPlaintextServerIsAccepted(t *testing.T) {
	srv := startGRPCServer(t, nil, nil)
	if err := newChecker(t, srv.addr, false, "").Verify(context.Background(), 0); err != nil {
		t.Fatalf("Verify() error = %v, want nil for the default deployment (both sides plaintext)", err)
	}
}

func TestVerify_TLSConfigWithUntrustedCertificateIsPermanentButNotAMismatch(t *testing.T) {
	certPEM, keyPEM := selfSignedFor(t)
	srv := startGRPCServer(t, certPEM, keyPEM)
	otherCert, _ := selfSignedFor(t) // a different CA, so the handshake must fail

	err := newChecker(t, srv.addr, true, writePEM(t, "other-ca.pem", otherCert)).Verify(context.Background(), 0)
	var untrusted *UntrustedError
	if !errors.As(err, &untrusted) {
		t.Fatalf("Verify() error = %T (%v), want *UntrustedError", err, err)
	}
	if !IsPermanent(err) {
		t.Error("IsPermanent(untrusted) = false, want true")
	}
	// The scheme agreed, so the message must not claim a mismatch - that
	// would send the operator to flip manager_tls and break a working
	// link.
	if strings.Contains(err.Error(), "speaks plaintext") || strings.Contains(err.Error(), "speaks tls, but") {
		t.Errorf("Verify() error %q misreports an untrusted certificate as a scheme mismatch", err.Error())
	}
	if !strings.Contains(err.Error(), "does not verify") || !strings.Contains(err.Error(), "manager_tls_ca") {
		t.Errorf("Verify() error %q does not name the trust problem and the setting to check", err.Error())
	}
}

func TestVerify_TLSConfigWithNoCAAgainstSelfSignedServerFails(t *testing.T) {
	// A self-signed managerd certificate with no manager_tls_ca configured
	// cannot verify against the system pool. That is a configuration
	// error worth catching at startup, not at the first request.
	certPEM, keyPEM := selfSignedFor(t)
	srv := startGRPCServer(t, certPEM, keyPEM)

	err := newChecker(t, srv.addr, true, "").Verify(context.Background(), 0)
	var untrusted *UntrustedError
	if !errors.As(err, &untrusted) {
		t.Fatalf("Verify() error = %T (%v), want *UntrustedError", err, err)
	}
}

func TestVerify_UnreachableIsNotAMismatchAndNotPermanent(t *testing.T) {
	// Nothing listening: bind and close, so the port is almost certainly
	// free (mirrors internal/restartplan's own probe_test.go approach).
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen() error: %v", err)
	}
	addr := lis.Addr().String()
	lis.Close()

	err = newChecker(t, addr, false, "").Verify(context.Background(), 0)
	var unreachable *UnreachableError
	if !errors.As(err, &unreachable) {
		t.Fatalf("Verify() error = %T (%v), want *UnreachableError", err, err)
	}
	if IsPermanent(err) {
		t.Error("IsPermanent(unreachable) = true, want false: an endpoint nobody could reach is not a proven mismatch")
	}
	// "unverified" is the word that matters: no evidence, not a verdict.
	if !strings.Contains(err.Error(), "unverified") || !strings.Contains(err.Error(), "not a TLS setting") {
		t.Errorf("Verify() error %q should say the TLS posture is unverified rather than wrong", err.Error())
	}
}

// TestVerify_RetriesUntilManagerdAppears covers the startup race: managerd
// not up yet is not fatal, as long as it shows up inside the grace window.
func TestVerify_RetriesUntilManagerdAppears(t *testing.T) {
	certPEM, keyPEM := selfSignedFor(t)
	srv := startGRPCServer(t, certPEM, keyPEM)
	ca := writePEM(t, "ca.pem", certPEM)

	var attempts atomic.Int64
	c := New(Config{
		Addr: srv.addr, UseTLS: true, CAFile: ca,
		ProcessName: "restshimd", ConfigPath: "/usr/local/etc/apiary/restshimd.json",
		Timeout: 2 * time.Second,
		Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
			// Refuse the first three dials, then let the real thing
			// through - i.e. managerd starting late.
			if attempts.Add(1) <= 3 {
				return nil, errConnRefused
			}
			return DefaultDial(ctx, network, address)
		},
	})
	if err := c.Verify(context.Background(), 5*time.Second); err != nil {
		t.Fatalf("Verify() error = %v, want nil once managerd answered within the grace window", err)
	}
	if got := attempts.Load(); got < 4 {
		t.Errorf("dials = %d, want at least 4 (three refusals then a real answer)", got)
	}
}

func TestVerify_PermanentFailureDoesNotWaitOutTheGraceWindow(t *testing.T) {
	certPEM, keyPEM := selfSignedFor(t)
	srv := startGRPCServer(t, certPEM, keyPEM)

	start := time.Now()
	if err := newChecker(t, srv.addr, false, "").Verify(context.Background(), 30*time.Second); err == nil {
		t.Fatal("Verify() = nil, want a mismatch")
	}
	// A permanent answer is returned at once; retrying cannot change it,
	// and a 30s grace would otherwise hold startup open for nothing.
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("Verify() took %s to report a permanent mismatch, want immediate", elapsed)
	}
}

func TestExplain_PlaintextConfigAgainstTLSServerNamesTheTLSCause(t *testing.T) {
	certPEM, keyPEM := selfSignedFor(t)
	srv := startGRPCServer(t, certPEM, keyPEM)
	c := newChecker(t, srv.addr, false, "")

	d := c.Explain(errors.New(`rpc error: code = Unavailable desc = connection error: desc = "transport: ` + serverPrefaceEOF + `"`))
	if d.Class != ClassSchemeMismatch {
		t.Errorf("Explain() class = %q, want %q", d.Class, ClassSchemeMismatch)
	}
	if !d.Permanent {
		t.Error("Explain() Permanent = false, want true: retrying cannot fix a wrong scheme")
	}
	// The hint has to carry the same actionable text as the startup
	// refusal, since a caller may only ever see this.
	for _, want := range []string{"manager_tls", "true", "restshimd", srv.addr} {
		if !strings.Contains(d.Detail, want) {
			t.Errorf("Explain() detail %q does not mention %q", d.Detail, want)
		}
	}
}

func TestExplain_UnreachableIsNotAMisconfiguration(t *testing.T) {
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen() error: %v", err)
	}
	addr := lis.Addr().String()
	lis.Close()

	d := newChecker(t, addr, true, "").Explain(errors.New(`rpc error: code = Unavailable desc = connection error: desc = "transport: Error while dialing dial tcp ` + addr + `: connect: connection refused"`))
	if d.Class != ClassUnreachable {
		t.Errorf("Explain() class = %q, want %q", d.Class, ClassUnreachable)
	}
	if d.Permanent {
		t.Error("Explain() Permanent = true, want false: an unreachable managerd is not a wrong config")
	}
	if !strings.Contains(d.Detail, "unverified") {
		t.Errorf("Explain() detail %q should say the TLS posture is unverified", d.Detail)
	}
}

func TestExplain_UpAndMatchingSchemeFallsBackToTheRawError(t *testing.T) {
	// Nothing to blame on the link: the scheme matches and the call still
	// failed, so the raw error is passed through rather than dressed up as
	// something it is not.
	srv := startGRPCServer(t, nil, nil)
	raw := errors.New("rpc error: code = Unavailable desc = raft leader is restarting")
	d := newChecker(t, srv.addr, false, "").Explain(raw)
	if d.Class != ClassCallFailed {
		t.Errorf("Explain() class = %q, want %q", d.Class, ClassCallFailed)
	}
	if d.Permanent {
		t.Error("Explain() Permanent = true, want false")
	}
	if !strings.Contains(d.Detail, raw.Error()) {
		t.Errorf("Explain() detail %q dropped the underlying error %q", d.Detail, raw)
	}
}

func TestExplain_TLSConfigWithUntrustedCertificateNamesTrust(t *testing.T) {
	certPEM, keyPEM := selfSignedFor(t)
	srv := startGRPCServer(t, certPEM, keyPEM)
	d := newChecker(t, srv.addr, true, "").Explain(errors.New("rpc error: code = Unavailable desc = transport: authentication handshake failed"))
	if d.Class != ClassTLSUntrusted || !d.Permanent {
		t.Errorf("Explain() = (%q, permanent=%v), want (%q, true)", d.Class, d.Permanent, ClassTLSUntrusted)
	}
}

// scriptedDialer answers the first n calls as a plaintext HTTP/2 server
// and every call after that as a TLS listener, so a verdict can be made to
// change underneath a running watcher.
func scriptedDialer(plaintextCalls int64) DialFunc {
	var calls atomic.Int64
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		if calls.Add(1) <= plaintextCalls {
			return pipeDialer(writeFirstByte(0x00))(ctx, network, address)
		}
		return pipeDialer(writeFirstByte(0x15))(ctx, network, address)
	}
}

func TestWatch_LogsAChangeOfVerdictAndNothingElse(t *testing.T) {
	// Configured plaintext; the endpoint starts out answering like a
	// plaintext server and then starts answering like a TLS one - the
	// "managerd was restarted with TLS turned on" case, which used to
	// surface only as an endless stream of anonymous 502s.
	c := New(Config{
		Addr: "test:1", UseTLS: false,
		ProcessName: "restshimd", ConfigPath: "/usr/local/etc/apiary/restshimd.json",
		Timeout: time.Second, CacheTTL: -1, // a negative CacheTTL disables caching
		Dial: scriptedDialer(2),
	})
	lines := make(chan string, 16)
	logf := func(format string, args ...any) {
		select {
		case lines <- fmt.Sprintf(format, args...):
		default:
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go c.Watch(ctx, 2*time.Millisecond, logf)

	deadline := time.After(5 * time.Second)
	for {
		select {
		case line := <-lines:
			// The seeded (plaintext, agreed) verdict must not be logged;
			// only the change to a mismatch is.
			if !strings.Contains(line, "manager_tls") {
				t.Errorf("Watch() logged %q, want only the mismatch", line)
			}
			return
		case <-deadline:
			t.Fatal("Watch() never logged the change of verdict")
		}
	}
}

func TestWatch_DoesNotLogAnUnchangedVerdict(t *testing.T) {
	// The watcher runs every 30s in production; a verdict that has not
	// changed must be silence, not a periodic line in the log.
	c := New(Config{
		Addr: "test:1", UseTLS: false, Timeout: time.Second, CacheTTL: -1,
		Dial: pipeDialer(writeFirstByte(0x00)),
	})
	var lines atomic.Int64
	// Cancelled rather than deadline-bounded on purpose: a probe honors the
	// caller's deadline (it must not outlive the startup grace window), so
	// a context about to expire would make every probe time out and this
	// test would be measuring its own setup.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	time.AfterFunc(150*time.Millisecond, cancel)
	c.Watch(ctx, 5*time.Millisecond, func(f string, a ...any) { lines.Add(1); t.Logf("LOGGED: "+f, a...) })
	if got := lines.Load(); got != 0 {
		t.Errorf("Watch() logged %d lines for an unchanged verdict, want 0", got)
	}
}

func TestIsPermanent(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"mismatch", &MismatchError{}, true},
		{"untrusted", &UntrustedError{}, true},
		{"unreachable", &UnreachableError{}, false},
		{"undetermined", &UndeterminedError{}, false},
		{"nil", nil, false},
		{"wrapped mismatch", fmt.Errorf("probing: %w", &MismatchError{}), true},
	} {
		if got := IsPermanent(tc.err); got != tc.want {
			t.Errorf("IsPermanent(%s) = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestVerify_SilentEndpointIsUndeterminedNotUnreachable: a peer that
// accepts the connection and then says nothing is "not yet knowable", not
// "nothing is listening" - the two send an operator to different places.
func TestVerify_SilentEndpointIsUndeterminedNotUnreachable(t *testing.T) {
	dial := pipeDialer(func(conn net.Conn) {
		buf := make([]byte, 64)
		io.ReadFull(conn, buf)
		conn.Close()
	})
	c := New(Config{Addr: "test:1", UseTLS: true, Timeout: 500 * time.Millisecond, Dial: dial})
	err := c.Verify(context.Background(), 0)
	var undetermined *UndeterminedError
	if !errors.As(err, &undetermined) {
		t.Fatalf("Verify() error = %T (%v), want *UndeterminedError", err, err)
	}
	if IsPermanent(err) {
		t.Error("IsPermanent(undetermined) = true, want false")
	}
	if strings.Contains(err.Error(), "managerd not started yet") {
		t.Errorf("Verify() error %q claims the endpoint is unreachable, which is not what was observed", err.Error())
	}
}

// TestVerify_TLSConfigWithUnreadableCAFileFailsAtStartup: the trust
// settings have to be checkable, not just present. A manager_tls=true with
// a CA path that does not exist is a startup error naming that path, not a
// per-request failure discovered later.
func TestVerify_TLSConfigWithUnreadableCAFileFailsAtStartup(t *testing.T) {
	certPEM, keyPEM := selfSignedFor(t)
	srv := startGRPCServer(t, certPEM, keyPEM)
	missing := filepath.Join(t.TempDir(), "not-here.pem")

	err := newChecker(t, srv.addr, true, missing).Verify(context.Background(), 0)
	var untrusted *UntrustedError
	if !errors.As(err, &untrusted) {
		t.Fatalf("Verify() error = %T (%v), want *UntrustedError naming the missing CA file", err, err)
	}
	if !IsPermanent(err) {
		t.Error("IsPermanent() = false, want true: no retry can find a file that is not there")
	}
	if !strings.Contains(err.Error(), missing) {
		t.Errorf("Verify() error %q does not name the unreadable CA file %q", err.Error(), missing)
	}
}
