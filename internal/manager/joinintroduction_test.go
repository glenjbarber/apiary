// Test fixtures for ADR-0147 Part 2: a real serving certificate this
// Comb can advertise, and the handful of helpers the existing join
// tests need now that a request carries a first code and fingerprints.
//
// The certificate is a package-level singleton rather than a per-test
// temp file because the fingerprint has to appear in two places at once
// - on the managerd under test's own nodeConfig, and inside the
// RequestJoinColonyRequest the test body builds - and a per-test
// certificate would make those two disagree in every test that has to
// paste a fingerprint.
//
// It is a REAL x509 certificate with the SANs a Comb serving
// certificate has to carry, because the trust gate re-evaluates the leaf
// fail-closed on the pin path. A certificate that merely parses would
// pass RequestJoinColony and then be refused at stage one, which is a
// confusing way for a test to fail.
package manager

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	internalpb "github.com/glenjbarber/apiary/api/internalpb"
	rpcpb "github.com/glenjbarber/apiary/api/rpc"
	"github.com/glenjbarber/apiary/internal/hostcert"
	"github.com/glenjbarber/apiary/internal/nodeconfig"
)

// testIntroductionCode is the joiner's own 6-digit first code, fixed so
// a test can paste it back without having captured it first. It is a
// correlation value in a test, not a secret, and a fixed one makes an
// assertion about "the code that was carried" readable.
const testIntroductionCode = "418263"

var (
	testCombCertOnce        sync.Once
	testCombCertPath        string
	testCombCertFingerprint string
)

// testCombCertificate returns a package-level real serving certificate:
// its on-disk path and its hostcert.Fingerprint digest.
func testCombCertificate(t *testing.T) (path, fingerprint string) {
	t.Helper()
	testCombCertOnce.Do(func() {
		dir, err := os.MkdirTemp("", "apiary-test-comb-cert-")
		if err != nil {
			t.Fatalf("creating a temp dir for the test serving certificate: %v", err)
		}
		certPEM, _, err := hostcert.Generate(hostcert.GenerateOptions{
			CommonName: "test-comb.example",
			DNSNames:   []string{"test-comb.example", "localhost"},
			IPAddresses: []net.IP{
				net.ParseIP("127.0.0.1"),
				net.ParseIP("::1"),
			},
		})
		if err != nil {
			t.Fatalf("generating the test serving certificate: %v", err)
		}
		path = filepath.Join(dir, "test-comb.crt")
		if err := os.WriteFile(path, certPEM, 0o600); err != nil {
			t.Fatalf("writing the test serving certificate: %v", err)
		}
		testCombCertPath = path
		block, _ := pem.Decode(certPEM)
		if block == nil {
			t.Fatalf("the generated test certificate does not decode as PEM")
		}
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			t.Fatalf("the generated test certificate does not parse: %v", err)
		}
		testCombCertFingerprint = hostcert.Fingerprint(cert.Raw)
	})
	return testCombCertPath, testCombCertFingerprint
}

// attachTestCombCertificate points this managerd's own nodeConfig at the
// shared test serving certificate, which is what makes
// localAdvertisedFingerprints resolve and what makes a pin's leaf pass
// the fail-closed evaluation gate.
func attachTestCombCertificate(t *testing.T, srv *Server) string {
	t.Helper()
	path, fingerprint := testCombCertificate(t)
	srv.nodeConfig = &fakeNodeConfigStore{cfg: nodeconfig.Config{TLSCert: path}}
	return fingerprint
}

// testCombFingerprintList is what a joining Comb in these tests
// advertises. The SAME digest attachTestCombCertificate installs, which
// is what the pin step requires: the certificate a Colony pins is the
// one a peer will dial the joiner with, and in a single-process test the
// joiner and the target are the same managerd.
func testCombFingerprintList(t *testing.T) []string {
	t.Helper()
	_, fingerprint := testCombCertificate(t)
	return []string{fingerprint}
}

// withIntroduction is the one-line amendment every existing
// RequestJoinColony test literal needs: a first code and the advertised
// fingerprints. Both are REQUIRED now, so a literal without them is
// testing the refusal rather than the behavior it was written for.
func withIntroduction(req *rpcpb.RequestJoinColonyRequest, t *testing.T) *rpcpb.RequestJoinColonyRequest {
	t.Helper()
	req.IntroductionCode = testIntroductionCode
	req.AdvertisedFingerprints = testCombFingerprintList(t)
	return req
}

// verifyIntroductionForTest drives stage one the way an operator would:
// paste the first code and the fingerprints the request carried. It
// returns nothing, because VerifyJoinIntroduction deliberately does not
// return the PIN - a test that wants the PIN reads it from replicated
// state, which is exactly where the only copy is.
func verifyIntroductionForTest(t *testing.T, srv *Server, requestID string) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	resp, err := srv.VerifyJoinIntroduction(ctx, &rpcpb.VerifyJoinIntroductionRequest{
		RequestId:        requestID,
		IntroductionCode: testIntroductionCode,
		Fingerprints:     testCombFingerprintList(t),
	})
	if err != nil {
		return err
	}
	if resp.GetError() != "" {
		return errString(resp.GetError())
	}
	return nil
}

type errString string

func (e errString) Error() string { return string(e) }

// secondPinForTest reads the second PIN out of replicated state.
//
// The RPC layer never returns it to the target - that is the whole
// property - so a test that needs it has to read the internal record,
// which is the honest shape of the thing being tested.
func secondPinForTest(t *testing.T, srv *Server, requestID string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	resp, err := srv.raft.GetPendingJoinRequestLocal(ctx, requestID)
	if err != nil {
		t.Fatalf("reading request %q for its second PIN: %v", requestID, err)
	}
	pin := resp.GetRequest().GetSecondPin()
	if pin == "" {
		t.Fatalf("request %q holds no second PIN; stage one did not complete", requestID)
	}
	return pin
}

// completeJoinHandshakeForTest runs stage one and returns the second PIN
// an operator would have carried to the target's page.
func completeJoinHandshakeForTest(t *testing.T, srv *Server, requestID string) string {
	t.Helper()
	if err := verifyIntroductionForTest(t, srv, requestID); err != nil {
		t.Fatalf("VerifyJoinIntroduction() for %q: %v", requestID, err)
	}
	return secondPinForTest(t, srv, requestID)
}

// stageOnePinForTest is completeJoinHandshakeForTest for a call site
// that does not know whether stage one has already run - a second Comb
// approving the same replicated request, for instance, which is exactly
// what the two-person rule is.
//
// Stage one is run only if the request is still INTRODUCED; a request
// already at CODE_VERIFIED has its existing PIN read instead. The FSM
// refuses a re-introduction by name, and that refusal is correct: the
// first code was cleared when it was accepted, so there is nothing left
// to verify.
func stageOnePinForTest(t *testing.T, srv *Server, requestID string) string {
	t.Helper()
	if internalStageOf(t, srv, requestID) == internalpb.JoinRequestStage_JOIN_REQUEST_STAGE_INTRODUCED {
		return completeJoinHandshakeForTest(t, srv, requestID)
	}
	return secondPinForTest(t, srv, requestID)
}

// internalStage is a small assertion helper for the stage axis, kept
// here so no test has to reach into internalpb for a field number.
func internalStageOf(t *testing.T, srv *Server, requestID string) internalpb.JoinRequestStage {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	resp, err := srv.raft.GetPendingJoinRequestLocal(ctx, requestID)
	if err != nil {
		t.Fatalf("reading request %q: %v", requestID, err)
	}
	return resp.GetRequest().GetStage()
}

// selfSignedTestCertPEM generates a bare self-signed certificate with no
// SANs, for the tests that only need a parseable PEM. It will NOT pass
// hostcert.Evaluate, which is why it is not the certificate the join
// tests advertise.
func selfSignedTestCertPEM(t *testing.T) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generating test key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "test-comb.example"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
	}
	certDER, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("creating test certificate: %v", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})
}
