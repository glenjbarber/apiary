// The multi-certificate introduction, at the layer the bug escaped from.
//
// internal/raft/joinintroduction_test.go pins fingerprintsMatch as a
// function. That is the right place for the set-comparison claim, and it
// is where the first fix was pinned. It is not the only place the claim
// lives, and it is not the place the production bug was found in.
//
// A Comb advertises MORE THAN ONE fingerprint in production, and the
// ordinary case is not exotic at all: any Comb with Raft transport TLS
// configured holds a raft_tls_cert as well as its serving tls_cert, so
// localAdvertisedCertificates returns two and localAdvertisedFingerprints
// hands both to the operator. Those tests are where the bug had the
// reach to hurt, and every existing test in this package built its
// advertisement through testCombFingerprintList, which returns exactly
// one element. The suite was therefore green on a Colony that could not
// join.
//
// So the advertisement here is DERIVED, not typed in: this file sets a
// real second certificate on raft_tls_cert and lets the production
// localAdvertisedFingerprints produce the list an operator would
// actually see. Two literals would have tested the FSM twice; this
// tests the producer and the consumer together, which is where a
// request and the thing it carries can drift apart.

package manager

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	internalpb "github.com/glenjbarber/apiary/api/internalpb"
	rpcpb "github.com/glenjbarber/apiary/api/rpc"
	"github.com/glenjbarber/apiary/internal/hostcert"
	"github.com/glenjbarber/apiary/internal/raftdconfig"
)

// attachTestRaftTransportCertificate gives the server under test a
// SECOND real certificate, the way a Comb with Raft transport TLS has
// one, and points raftdConfig at it.
//
// The certificate is a genuine x509 serving certificate rather than a
// bare parseable PEM for the same reason the serving one is: the values
// under test are real hostcert.Fingerprint digests, and a digest of a
// throwaway key is still a real digest, so this does not weaken the
// claim. It is a DIFFERENT key from the serving certificate, which is
// what makes the two advertised fingerprints distinct rather than a
// duplicate pair - a duplicate pair would pass a buggy comparison for
// the wrong reason, and test A asserts they are distinct before it
// asserts anything else.
func attachTestRaftTransportCertificate(t *testing.T, srv *Server) string {
	t.Helper()
	certPEM, _, err := hostcert.Generate(hostcert.GenerateOptions{
		CommonName: "test-comb-raft.example",
		DNSNames:   []string{"test-comb-raft.example"},
		IPAddresses: []net.IP{
			net.ParseIP("127.0.0.1"),
		},
	})
	if err != nil {
		t.Fatalf("generating the test raft transport certificate: %v", err)
	}
	path := filepath.Join(t.TempDir(), "raft-cert.crt")
	if err := os.WriteFile(path, certPEM, 0o600); err != nil {
		t.Fatalf("writing the test raft transport certificate: %v", err)
	}
	srv.raftdConfig = &fakeRaftdConfigStore{cfg: raftdconfig.Config{RaftTLSCert: path}}
	return path
}

// advertisedFingerprintsForTest reads back what the request actually
// carried, rather than assuming the advertisement it was given. The
// whole point of the file is that what was sent and what was recorded
// are the two halves that can disagree.
func advertisedFingerprintsForTest(t *testing.T, srv *Server, requestID string) []string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	resp, err := srv.raft.GetPendingJoinRequestLocal(ctx, requestID)
	if err != nil {
		t.Fatalf("reading request %q for its advertised fingerprints: %v", requestID, err)
	}
	return resp.GetRequest().GetAdvertisedFingerprints()
}

// pendingRecordForTest reads one request out of this member's own
// replicated copy, which is where a claim about what a request actually
// holds has to be read from. A response body can report success while
// the state did not move, so anything asserted about the stored record
// is asserted against the record.
func pendingRecordForTest(t *testing.T, srv *Server, requestID string) *internalpb.PendingJoinRequest {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	resp, err := srv.raft.GetPendingJoinRequestLocal(ctx, requestID)
	if err != nil {
		t.Fatalf("reading request %q: %v", requestID, err)
	}
	req := resp.GetRequest()
	if req == nil {
		t.Fatalf("request %q does not exist", requestID)
	}
	return req
}

// pendingCodeForTest reads the request's first code, which must be
// empty once an introduction has been accepted.
func pendingCodeForTest(t *testing.T, srv *Server, requestID string) string {
	t.Helper()
	return pendingRecordForTest(t, srv, requestID).GetCode()
}

// pendingFirstCodeAttemptsForTest reads the per-stage attempt counter.
func pendingFirstCodeAttemptsForTest(t *testing.T, srv *Server, requestID string) uint32 {
	t.Helper()
	return pendingRecordForTest(t, srv, requestID).GetFirstCodeAttempts()
}

// seedTwoFingerprintRequest records a request whose advertised list is
// whatever this Comb's own certificate files say, with the first code
// supplied and AdvertisedFingerprints deliberately LEFT EMPTY so the
// manager derives it. Leaving it empty is the production path:
// apiaryctl join-introduce does not have to know how a Comb's
// certificates are configured, and neither should the test that is
// standing in for it.
//
// It returns the recorded request_id and the two fingerprints the
// request carried.
func seedTwoFingerprintRequest(t *testing.T, client rpcpb.ManagerServiceClient, srv *Server) (requestID string, carried []string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	resp, err := client.RequestJoinColony(ctx, &rpcpb.RequestJoinColonyRequest{
		NodeId:                 "comb-2",
		RaftBindAddress:        "10.62.0.5:17600",
		JoinerLogStateObserved: true,
		IntroductionCode:       testIntroductionCode,
		// AdvertisedFingerprints deliberately unset.
	})
	if err != nil {
		t.Fatalf("RequestJoinColony: %v", err)
	}
	if resp.GetError() != "" {
		t.Fatalf("RequestJoinColony: %s", resp.GetError())
	}
	return resp.GetRequestId(), advertisedFingerprintsForTest(t, srv, resp.GetRequestId())
}

// TestPart2_TwoAdvertisedFingerprintsVerifyOverRPC is the success path
// the suite was missing, and it is the whole reason the fix is
// trustworthy: two DISTINCT certificates, advertised by the production
// code, read back off the recorded request, and pasted back REVERSED.
//
// The reversal is not decoration. Order is stated to be irrelevant, and
// an operator transcribing two lines by hand will not reliably preserve
// the order they were printed in, so the reversed paste is the case
// production actually has to survive. Pasting in order would have passed
// a comparison that only worked positionally.
func TestPart2_TwoAdvertisedFingerprintsVerifyOverRPC(t *testing.T) {
	socket := newRaftdUDSSocket(t)
	client, srv := newManagerdRPCClientAndServer(t, socket, "comb-leader")
	srv.setJoinAuthorizationPath(t.TempDir() + "/join-authorizations.json")
	attachTestRaftTransportCertificate(t, srv)

	openColonyJoinWindowForTest(t, client)
	requestID, carried := seedTwoFingerprintRequest(t, client, srv)

	// The fixture's own precondition. A two-element list of one
	// repeated value would satisfy a comparison that is wrong in the
	// way this file is about, so assert distinctness before asserting
	// behaviour: this test only means what it says if these are two
	// different certificates.
	if len(carried) != 2 {
		t.Fatalf("the request carried %d fingerprint(s) %v, want 2. A Comb with both tls_cert and raft_tls_cert configured advertises both, and this test is about that shape", len(carried), carried)
	}
	if carried[0] == carried[1] {
		t.Fatalf("the two advertised fingerprints are the same value %q. Two DISTINCT certificates are the case; a repeated value would let a broken comparison pass", carried[0])
	}

	pasted := []string{carried[1], carried[0]}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	verified, err := client.VerifyJoinIntroduction(ctx, &rpcpb.VerifyJoinIntroductionRequest{
		RequestId:        requestID,
		IntroductionCode: testIntroductionCode,
		Fingerprints:     pasted,
	})
	if err != nil {
		t.Fatalf("VerifyJoinIntroduction: %v", err)
	}
	if verified.GetError() != "" {
		t.Fatalf("a correct two-fingerprint introduction was refused: %s", verified.GetError())
	}

	// The stage is the claim, read from replicated state rather than
	// from the response, because a response could report success while
	// the state did not move.
	if got := internalStageOf(t, srv, requestID); got != internalpb.JoinRequestStage_JOIN_REQUEST_STAGE_CODE_VERIFIED {
		t.Errorf("stage after a successful two-fingerprint introduction = %s, want CODE_VERIFIED", got)
	}
	// The first code is spent in the same entry, so an accepted
	// introduction leaves nothing to re-introduce with.
	if code := pendingCodeForTest(t, srv, requestID); code != "" {
		t.Errorf("the first code survived acceptance as %q; there is no window in which an accepted code is still sitting in replicated state", code)
	}

	// And the pin is written, against the SERVING certificate, not the
	// raft transport one. buildPinForRequest deliberately pins certs[0]:
	// the raft certificate is advertised for the operator to compare
	// but is not what a peer dials this Comb with. A test that only
	// counted pins would pass if the wrong one were chosen.
	peers := listTrustedPeersForTest(t, srv)
	if len(peers) != 1 {
		t.Fatalf("the trust store holds %d entries after a successful two-fingerprint introduction, want 1", len(peers))
	}
	if got, want := peers[0].GetFingerprint(), carried[0]; got != want {
		t.Errorf("pinned fingerprint = %q, want the serving certificate %q. The raft transport certificate is advertised for comparison and is not the pin", got, want)
	}
}

// TestPart2_ASubsetOfTwoAdvertisedFingerprintsIsRefused is the
// direction the fix is most likely to have broken, and it is why the
// success test above is not enough on its own.
//
// The first code was correct. One of the two advertised fingerprints
// was pasted, and it is the right one. If the comparison were a subset
// test - "is anything the operator pasted among what we advertised" -
// this would verify, and the handshake would accept an operator who
// compared one of two certificates without ever looking at the other.
// Every carried fingerprint has to be answered.
func TestPart2_ASubsetOfTwoAdvertisedFingerprintsIsRefused(t *testing.T) {
	socket := newRaftdUDSSocket(t)
	client, srv := newManagerdRPCClientAndServer(t, socket, "comb-leader")
	srv.setJoinAuthorizationPath(t.TempDir() + "/join-authorizations.json")
	attachTestRaftTransportCertificate(t, srv)

	openColonyJoinWindowForTest(t, client)
	requestID, carried := seedTwoFingerprintRequest(t, client, srv)
	if len(carried) != 2 {
		t.Fatalf("the request carried %d fingerprint(s) %v, want 2", len(carried), carried)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	// The correct first code and ONE correct fingerprint. This is the
	// paste a subset comparison would wave through.
	refused, err := client.VerifyJoinIntroduction(ctx, &rpcpb.VerifyJoinIntroductionRequest{
		RequestId:        requestID,
		IntroductionCode: testIntroductionCode,
		Fingerprints:     carried[:1],
	})
	if err != nil {
		t.Fatalf("VerifyJoinIntroduction: %v", err)
	}
	if refused.GetError() == "" {
		t.Fatalf("a request carrying two fingerprints verified on a paste of one of them (%+v). Every advertised certificate has to be compared; one of two is not a comparison", refused)
	}
	if got := internalStageOf(t, srv, requestID); got != internalpb.JoinRequestStage_JOIN_REQUEST_STAGE_INTRODUCED {
		t.Errorf("stage after a refused subset paste = %s, want INTRODUCED. A refused comparison must not advance the request", got)
	}
	if peers := listTrustedPeersForTest(t, srv); len(peers) != 0 {
		t.Errorf("a refused introduction left %d pin(s) behind", len(peers))
	}
	// The attempt is counted, because an incomplete comparison is a
	// failed one and costs a budget like any other.
	if attempts := pendingFirstCodeAttemptsForTest(t, srv, requestID); attempts != 1 {
		t.Errorf("first_code_attempts after one refused subset paste = %d, want 1", attempts)
	}
}

// TestPart2_OneRightAndOneWrongFingerprintIsRefused is the third shape,
// and the one a careless rewrite produces: right length, right first
// element, wrong second element. A comparison that only checks the
// values it finds in common, or that stops at the first match, accepts
// this.
func TestPart2_OneRightAndOneWrongFingerprintIsRefused(t *testing.T) {
	socket := newRaftdUDSSocket(t)
	client, srv := newManagerdRPCClientAndServer(t, socket, "comb-leader")
	srv.setJoinAuthorizationPath(t.TempDir() + "/join-authorizations.json")
	attachTestRaftTransportCertificate(t, srv)

	openColonyJoinWindowForTest(t, client)
	requestID, carried := seedTwoFingerprintRequest(t, client, srv)
	if len(carried) != 2 {
		t.Fatalf("the request carried %d fingerprint(s) %v, want 2", len(carried), carried)
	}

	// A well-formed fingerprint shape that is not either of the two.
	// It has to LOOK right, or the refusal could be a parse complaint
	// rather than a comparison, and the test would be proving less than
	// its name claims.
	const impostor = "SHA256:" + "0000000000000000000000000000000000000000000000000000000000000000"

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	refused, err := client.VerifyJoinIntroduction(ctx, &rpcpb.VerifyJoinIntroductionRequest{
		RequestId:        requestID,
		IntroductionCode: testIntroductionCode,
		Fingerprints:     []string{carried[0], impostor},
	})
	if err != nil {
		t.Fatalf("VerifyJoinIntroduction: %v", err)
	}
	if refused.GetError() == "" {
		t.Fatalf("a paste carrying one right and one wrong fingerprint verified (%+v). Matching values in common is not the same as answering every advertised certificate", refused)
	}
	if got := internalStageOf(t, srv, requestID); got != internalpb.JoinRequestStage_JOIN_REQUEST_STAGE_INTRODUCED {
		t.Errorf("stage after a refused partial paste = %s, want INTRODUCED", got)
	}
	if peers := listTrustedPeersForTest(t, srv); len(peers) != 0 {
		t.Errorf("a refused introduction left %d pin(s) behind", len(peers))
	}
}
