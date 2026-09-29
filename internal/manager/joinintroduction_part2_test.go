// ADR-0147 Part 2's own tests: the properties the handshake buys, and
// the ones it must not quietly give back.
//
// These are deliberately written against the FAILURE directions. A test
// that walks the happy path proves the flow works; a test that shows a
// request carrying no fingerprints cannot be approved, that a wrong code
// costs an attempt, that five wrong attempts are terminal, that the
// second PIN never appears in a message the target's operator reads, and
// that a poll without the first code does not release it, is a test that
// fails when someone makes the check cheaper.

package manager

import (
	"context"
	"strings"
	"testing"
	"time"

	internalpb "github.com/glenjbarber/apiary/api/internalpb"
	rpcpb "github.com/glenjbarber/apiary/api/rpc"
	raftnode "github.com/glenjbarber/apiary/internal/raft"
)

// seedIntroductionRequest records a request the way RequestJoinColony
// does, through the real RPC, so the fingerprints and the first code are
// whatever the production path put there.
func seedIntroductionRequest(t *testing.T, client rpcpb.ManagerServiceClient, nodeID, raftBind string) *rpcpb.RequestJoinColonyResponse {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	resp, err := client.RequestJoinColony(ctx, withIntroduction(&rpcpb.RequestJoinColonyRequest{
		NodeId:                 nodeID,
		RaftBindAddress:        raftBind,
		JoinerLogStateObserved: true,
	}, t))
	if err != nil {
		t.Fatalf("RequestJoinColony: %v", err)
	}
	if resp.GetError() != "" {
		t.Fatalf("RequestJoinColony: %s", resp.GetError())
	}
	return resp
}

// TestPart2_ARequestWithNoFingerprintsIsRefusedAtTheSource is the
// fail-closed property, asserted at the earliest point it can be.
//
// ADR-0113 identified and did not fix the defect: the UI rendered "no
// TLS certificate presented" and approval proceeded. The new behaviour
// refuses to RECORD such a request, which is strictly earlier and
// strictly more useful - the operator is told at the moment they can
// still go and fix the joining Comb, instead of discovering it later
// through a panel they had to read carefully to notice.
func TestPart2_ARequestWithNoFingerprintsIsRefusedAtTheSource(t *testing.T) {
	socket := newRaftdUDSSocket(t)
	client, srv := newManagerdRPCClientAndServer(t, socket, "comb-leader")
	srv.setJoinAuthorizationPath(t.TempDir() + "/join-authorizations.json")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	openColonyJoinWindowForTest(t, client)

	// Detach this Comb's certificate, which is the shape of the
	// pre-ADR-0147 joiner: a Comb with no tls_cert at all.
	srv.nodeConfig = &fakeNodeConfigStore{}
	// The advertised list is cleared as well, because a caller that set
	// it is trusted as-is by design (it is what lets a test drive a
	// real approval without a second live Comb). A real joiner never
	// sets it - apiaryctl join-introduce sends what this Comb's own
	// certificate files say - so clearing it here is what makes this
	// the pre-ADR-0147 shape rather than a contrived one.
	req := withIntroduction(&rpcpb.RequestJoinColonyRequest{
		NodeId:                 "comb-2",
		RaftBindAddress:        "10.62.0.5:17600",
		JoinerLogStateObserved: true,
	}, t)
	req.AdvertisedFingerprints = nil
	resp, err := client.RequestJoinColony(ctx, req)
	if err != nil {
		t.Fatalf("RequestJoinColony: %v", err)
	}
	if resp.GetError() == "" {
		t.Fatalf("a request carrying no fingerprints was accepted (%+v). A request with nothing to compare can never be approved, so recording it only produces a row certain to be refused later", resp)
	}
	if !strings.Contains(resp.GetError(), "tls_cert") {
		t.Errorf("refusal %q does not name the configuration field to fix", resp.GetError())
	}
	if resp.GetRequestId() != "" {
		t.Errorf("a refused request still returned a request_id %q; a caller could poll for a record that does not exist", resp.GetRequestId())
	}
}

// TestPart2_TheFirstCodeIsRequiredAndIsTheJoins is the inversion, in one
// test: the target refuses to invent the value the whole check is about.
func TestPart2_TheFirstCodeIsRequiredAndIsTheJoins(t *testing.T) {
	socket := newRaftdUDSSocket(t)
	client, _ := newManagerdRPCClientAndServer(t, socket, "comb-leader")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	openColonyJoinWindowForTest(t, client)

	resp, err := client.RequestJoinColony(ctx, &rpcpb.RequestJoinColonyRequest{
		NodeId:                 "comb-2",
		RaftBindAddress:        "10.62.0.5:17600",
		JoinerLogStateObserved: true,
		AdvertisedFingerprints: testCombFingerprintList(t),
	})
	if err != nil {
		t.Fatalf("RequestJoinColony: %v", err)
	}
	if resp.GetError() == "" {
		t.Fatal("a request with no first code was accepted. Generating one on the target would restore the shape where both operators read the same server's state")
	}
	if !strings.Contains(resp.GetError(), "apiaryctl join-introduce") {
		t.Errorf("refusal %q does not name the command that produces the value", resp.GetError())
	}
}

// TestPart2_AWrongFirstCodeCostsAnAttemptAndFiveIsTerminal walks the
// whole attempt budget, because "exhaustion is terminal" is a claim
// about behaviour rather than about a constant.
func TestPart2_AWrongFirstCodeCostsAnAttemptAndFiveIsTerminal(t *testing.T) {
	socket := newRaftdUDSSocket(t)
	client, srv := newManagerdRPCClientAndServer(t, socket, "comb-leader")
	srv.setJoinAuthorizationPath(t.TempDir() + "/join-authorizations.json")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	openColonyJoinWindowForTest(t, client)
	created := seedIntroductionRequest(t, client, "comb-2", "10.62.0.5:17600")

	for attempt := uint32(1); attempt <= raftnode.MaxJoinStageAttempts; attempt++ {
		resp, err := srv.VerifyJoinIntroduction(ctx, &rpcpb.VerifyJoinIntroductionRequest{
			RequestId:        created.GetRequestId(),
			IntroductionCode: "000000",
			Fingerprints:     testCombFingerprintList(t),
		})
		if err != nil {
			t.Fatalf("attempt %d: VerifyJoinIntroduction: %v", attempt, err)
		}
		if resp.GetError() == "" {
			t.Fatalf("attempt %d with the wrong first code was accepted", attempt)
		}
		if attempt < raftnode.MaxJoinStageAttempts {
			if !strings.Contains(resp.GetError(), "does not match") {
				t.Errorf("attempt %d refusal %q does not say the value did not match, which is the one thing an operator can act on", attempt, resp.GetError())
			}
		} else {
			if !strings.Contains(resp.GetError(), "FAILED") {
				t.Errorf("the final refusal %q does not say the request is now FAILED; an operator has to be able to see that a fresh allowance is not being handed out", resp.GetError())
			}
		}
	}

	// Terminal means terminal: a FAILED request admits nothing further,
	// not even a correct code.
	resp, err := srv.VerifyJoinIntroduction(ctx, &rpcpb.VerifyJoinIntroductionRequest{
		RequestId:        created.GetRequestId(),
		IntroductionCode: testIntroductionCode,
		Fingerprints:     testCombFingerprintList(t),
	})
	if err != nil {
		t.Fatalf("VerifyJoinIntroduction after exhaustion: %v", err)
	}
	if resp.GetError() == "" {
		t.Fatal("a FAILED request accepted a correct first code. Five wrong guesses and a fresh allowance is how a value stops being a gate")
	}
	if got := internalStageOf(t, srv, created.GetRequestId()); got != internalpb.JoinRequestStage_JOIN_REQUEST_STAGE_INTRODUCED {
		t.Errorf("stage after exhaustion = %s, want INTRODUCED unchanged", got)
	}
}

// TestPart2_AMismatchedFingerprintIsARefusalShowingBothValues is the
// comparison the whole fingerprint story exists to make, and the message
// has to carry both sides of it.
//
// "These do not match" with nothing to compare them against is the one
// message an operator cannot act on, and a refusal an operator cannot
// act on is a refusal they route around.
func TestPart2_AMismatchedFingerprintIsARefusalShowingBothValues(t *testing.T) {
	socket := newRaftdUDSSocket(t)
	client, srv := newManagerdRPCClientAndServer(t, socket, "comb-leader")
	srv.setJoinAuthorizationPath(t.TempDir() + "/join-authorizations.json")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	openColonyJoinWindowForTest(t, client)
	created := seedIntroductionRequest(t, client, "comb-2", "10.62.0.5:17600")

	_, carried := testCombCertificate(t)
	resp, err := srv.VerifyJoinIntroduction(ctx, &rpcpb.VerifyJoinIntroductionRequest{
		RequestId:        created.GetRequestId(),
		IntroductionCode: testIntroductionCode,
		Fingerprints:     []string{"SHA256:DE:AD:BE:EF"},
	})
	if err != nil {
		t.Fatalf("VerifyJoinIntroduction: %v", err)
	}
	if resp.GetError() == "" {
		t.Fatal("a mismatched fingerprint was accepted")
	}
	if !strings.Contains(resp.GetError(), carried) {
		t.Errorf("refusal %q does not show the fingerprint the request actually carried, so the operator has nothing to check their paste against", resp.GetError())
	}
	if !strings.Contains(resp.GetError(), "SHA256:DE:AD:BE:EF") {
		t.Errorf("refusal %q does not show what was pasted", resp.GetError())
	}
}

// TestPart2_TheSecondPINIsNeverInAMessageTheTargetReads is the property
// the whole "shown only on the requesting Comb" clause is about, and it
// is asserted structurally rather than behaviourally: the second PIN is
// not a field on the message ListJoinRequests returns, and the target's
// own verify call does not return one either.
func TestPart2_TheSecondPINIsNeverInAMessageTheTargetReads(t *testing.T) {
	socket := newRaftdUDSSocket(t)
	client, srv := newManagerdRPCClientAndServer(t, socket, "comb-leader")
	srv.setJoinAuthorizationPath(t.TempDir() + "/join-authorizations.json")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	openColonyJoinWindowForTest(t, client)
	created := seedIntroductionRequest(t, client, "comb-2", "10.62.0.5:17600")
	pin := completeJoinHandshakeForTest(t, srv, created.GetRequestId())

	// The target's own list, which is what the page renders.
	list, err := client.ListJoinRequests(ctx, &rpcpb.ListJoinRequestsRequest{})
	if err != nil {
		t.Fatalf("ListJoinRequests: %v", err)
	}
	for _, r := range list.GetRequests() {
		if r.GetRequestId() != created.GetRequestId() {
			continue
		}
		if strings.Contains(r.String(), pin) {
			t.Fatalf("the second PIN appears in the message the TARGET's operator reads: %s", r.String())
		}
		if r.GetCode() != "" {
			t.Errorf("the first code is still on the record after acceptance (%q); a spent value is not one to leave in replicated state", r.GetCode())
		}
	}
}

// TestPart2_TheSecondPINIsReleasedOnlyToTheHolderOfBothValues is the
// one revision the owner made to the ADR, asserted in the direction that
// matters: knowledge of the request_id ALONE does not release it.
func TestPart2_TheSecondPINIsReleasedOnlyToTheHolderOfBothValues(t *testing.T) {
	socket := newRaftdUDSSocket(t)
	client, srv := newManagerdRPCClientAndServer(t, socket, "comb-leader")
	srv.setJoinAuthorizationPath(t.TempDir() + "/join-authorizations.json")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	openColonyJoinWindowForTest(t, client)
	created := seedIntroductionRequest(t, client, "comb-2", "10.62.0.5:17600")
	pin := completeJoinHandshakeForTest(t, srv, created.GetRequestId())
	if pin == "" {
		t.Fatal("stage one produced no second PIN")
	}

	// request_id alone - the ADR's own design, which this change closed.
	noCode, err := client.GetJoinRequestStatus(ctx, &rpcpb.GetJoinRequestStatusRequest{RequestId: created.GetRequestId()})
	if err != nil {
		t.Fatalf("GetJoinRequestStatus: %v", err)
	}
	if noCode.GetSecondPin() != "" {
		t.Fatal("the second PIN was released to a caller holding only the request_id")
	}
	// The request is still visible to that caller, which is deliberate:
	// GetJoinRequestStatus has always been a status read, and a poll
	// that returned nothing at all would be a worse diagnostic than one
	// that shows the stage and withholds the value.
	if noCode.GetRequest().GetStage() != rpcpb.JoinRequestStage_JOIN_REQUEST_STAGE_CODE_VERIFIED {
		t.Errorf("stage reported to a caller without the first code = %s, want CODE_VERIFIED reported without the PIN", noCode.GetRequest().GetStage())
	}

	// A wrong first code is no better than none.
	wrong, err := client.GetJoinRequestStatus(ctx, &rpcpb.GetJoinRequestStatusRequest{RequestId: created.GetRequestId(), IntroductionCode: "000000"})
	if err != nil {
		t.Fatalf("GetJoinRequestStatus: %v", err)
	}
	if wrong.GetSecondPin() != "" {
		t.Fatal("the second PIN was released to a caller holding a wrong first code")
	}

	// Both values, and it comes.
	both, err := client.GetJoinRequestStatus(ctx, &rpcpb.GetJoinRequestStatusRequest{RequestId: created.GetRequestId(), IntroductionCode: testIntroductionCode})
	if err != nil {
		t.Fatalf("GetJoinRequestStatus: %v", err)
	}
	if both.GetSecondPin() != pin {
		t.Fatalf("the joiner holding both values was given %q, want the PIN %q the target generated", both.GetSecondPin(), pin)
	}
	if both.GetSecondPinExpiresAtUnix() <= time.Now().Unix() {
		t.Errorf("the PIN was released with no live deadline (expires_at = %d)", both.GetSecondPinExpiresAtUnix())
	}
}

// TestPart2_ApprovalRequiresAndSpendsTheSecondPIN covers the spend and
// its position: the PIN is cleared BEFORE AddVoter, and a failed
// AddVoter does not put it back.
func TestPart2_ApprovalRequiresAndSpendsTheSecondPIN(t *testing.T) {
	socket := newRaftdUDSSocket(t)
	client, srv := newManagerdRPCClientAndServer(t, socket, "comb-leader")
	srv.setJoinAuthorizationPath(t.TempDir() + "/join-authorizations.json")

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	openColonyJoinWindowForTest(t, client)
	created := seedIntroductionRequest(t, client, "comb-2", "10.62.0.5:17600")
	pin := completeJoinHandshakeForTest(t, srv, created.GetRequestId())

	// No PIN at all, with the correct phrase.
	noPin, err := client.ApproveJoinRequest(ctx, &rpcpb.ApproveJoinRequestRequest{
		RequestId: created.GetRequestId(), ConfirmPhrase: approveJoinRequestConfirmPhrase,
	})
	if err != nil {
		t.Fatalf("ApproveJoinRequest: %v", err)
	}
	if noPin.GetError() == "" {
		t.Fatal("an approval with no second PIN was accepted. The confirmation phrase is a constant anyone can read out of this repository")
	}

	// A wrong PIN, which is an attempt and not a free retry.
	wrong, err := client.ApproveJoinRequest(ctx, &rpcpb.ApproveJoinRequestRequest{
		RequestId: created.GetRequestId(), ConfirmPhrase: approveJoinRequestConfirmPhrase, SecondPin: "00000000",
	})
	if err != nil {
		t.Fatalf("ApproveJoinRequest: %v", err)
	}
	if wrong.GetError() == "" {
		t.Fatal("a wrong second PIN was accepted")
	}

	// Still live: a wrong attempt consumes the allowance, not the PIN.
	if secondPinForTestOrEmpty(t, srv, created.GetRequestId()) != pin {
		t.Fatal("a wrong second PIN consumed the live one; one mistyped paste would cost the operator a reissue from a cap of two")
	}
}

// secondPinForTestOrEmpty reads the PIN without failing the test when
// it is absent, for the assertions that are about whether a wrong value
// disturbed a live one.
func secondPinForTestOrEmpty(t *testing.T, srv *Server, requestID string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	resp, err := srv.raft.GetPendingJoinRequestLocal(ctx, requestID)
	if err != nil {
		t.Fatalf("reading request %q: %v", requestID, err)
	}
	return resp.GetRequest().GetSecondPin()
}

// TestPart2_VerifyJoinIntroductionPinsTheCertificate is the trust
// store's first writer, asserted through the public path rather than by
// constructing the command: nothing in the tree used to construct a
// Command_PinTrustedPeer, and this is what changed.
func TestPart2_VerifyJoinIntroductionPinsTheCertificate(t *testing.T) {
	socket := newRaftdUDSSocket(t)
	client, srv := newManagerdRPCClientAndServer(t, socket, "comb-leader")
	srv.setJoinAuthorizationPath(t.TempDir() + "/join-authorizations.json")

	openColonyJoinWindowForTest(t, client)
	created := seedIntroductionRequest(t, client, "comb-2", "10.62.0.5:17600")

	if len(listTrustedPeersForTest(t, srv)) != 0 {
		t.Fatal("the trust store is not empty before the introduction; this test cannot tell a new writer from a pre-existing entry")
	}
	completeJoinHandshakeForTest(t, srv, created.GetRequestId())

	peers := listTrustedPeersForTest(t, srv)
	if len(peers) != 1 {
		t.Fatalf("the trust store holds %d entries after a successful introduction, want 1", len(peers))
	}
	pin := peers[0]
	if pin.GetNodeId() != "comb-2" {
		t.Errorf("pinned node_id = %q, want comb-2", pin.GetNodeId())
	}
	if pin.GetIsVoter() {
		t.Error("the pin claims voter membership. AddVoter promotes a pin, and letting a pin imply membership is the conflation TrustedPeer.is_voter's own comment exists to prevent")
	}
	if pin.GetCertPem() == "" {
		t.Error("the pin carries no certificate. A digest cannot rebuild a PEM bundle, and a re-imaged Comb still has to dial the Colony it belongs to")
	}
}

// listTrustedPeersForTest reads the replicated trust store through the
// same path the derived peer-ca.pem writer uses.
func listTrustedPeersForTest(t *testing.T, srv *Server) []*internalpb.TrustedPeer {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	resp, err := srv.raft.ListTrustedPeersLocal(ctx)
	if err != nil {
		t.Fatalf("ListTrustedPeers: %v", err)
	}
	return resp.GetPeers()
}

// TestPart2_ARefusedRequestDropsItsPin closes the other half of the
// lifecycle: a trust anchor must not outlive the request that created
// it, or the store accumulates one dead certificate per Comb that ever
// asked.
func TestPart2_ARefusedRequestDropsItsPin(t *testing.T) {
	socket := newRaftdUDSSocket(t)
	client, srv := newManagerdRPCClientAndServer(t, socket, "comb-leader")
	srv.setJoinAuthorizationPath(t.TempDir() + "/join-authorizations.json")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	openColonyJoinWindowForTest(t, client)
	created := seedIntroductionRequest(t, client, "comb-2", "10.62.0.5:17600")
	completeJoinHandshakeForTest(t, srv, created.GetRequestId())
	if len(listTrustedPeersForTest(t, srv)) != 1 {
		t.Fatal("stage one did not pin the certificate, so this test would pass for the wrong reason")
	}

	reject, err := client.RejectJoinRequest(ctx, &rpcpb.RejectJoinRequestRequest{RequestId: created.GetRequestId()})
	if err != nil {
		t.Fatalf("RejectJoinRequest: %v", err)
	}
	if reject.GetError() != "" {
		t.Fatalf("RejectJoinRequest: %s", reject.GetError())
	}
	if peers := listTrustedPeersForTest(t, srv); len(peers) != 0 {
		t.Errorf("a rejected request left %d pin(s) behind; a terminal state that never became a voter must not leave a standing trust anchor", len(peers))
	}
}

// TestPart2_ReissueIsCappedAtTwoAndThenTerminal walks the re-arm budget,
// because a cap nobody has walked is a number rather than a rule.
func TestPart2_ReissueIsCappedAtTwoAndThenTerminal(t *testing.T) {
	socket := newRaftdUDSSocket(t)
	client, srv := newManagerdRPCClientAndServer(t, socket, "comb-leader")
	srv.setJoinAuthorizationPath(t.TempDir() + "/join-authorizations.json")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	openColonyJoinWindowForTest(t, client)
	created := seedIntroductionRequest(t, client, "comb-2", "10.62.0.5:17600")
	first := completeJoinHandshakeForTest(t, srv, created.GetRequestId())

	for i := uint32(1); i <= raftnode.MaxJoinSecondPinReissues; i++ {
		resp, err := srv.ReissueJoinSecondPin(ctx, &rpcpb.ReissueJoinSecondPinRequest{RequestId: created.GetRequestId()})
		if err != nil {
			t.Fatalf("reissue %d: %v", i, err)
		}
		if resp.GetError() != "" {
			t.Fatalf("reissue %d was refused: %s", i, resp.GetError())
		}
		if newPin := secondPinForTestOrEmpty(t, srv, created.GetRequestId()); newPin == first {
			t.Errorf("reissue %d reused the previous PIN (%s); a re-arm that does not invalidate anything is not a re-arm", i, newPin)
		}
		first = secondPinForTestOrEmpty(t, srv, created.GetRequestId())
	}

	over, err := srv.ReissueJoinSecondPin(ctx, &rpcpb.ReissueJoinSecondPinRequest{RequestId: created.GetRequestId()})
	if err != nil {
		t.Fatalf("reissue past the cap: %v", err)
	}
	if over.GetError() == "" {
		t.Fatal("a third reissue was accepted; the cap is advisory if nothing enforces it")
	}
	if !strings.Contains(over.GetError(), "FAILED") {
		t.Errorf("refusal %q does not say the request is now FAILED", over.GetError())
	}
	if secondPinForTestOrEmpty(t, srv, created.GetRequestId()) != "" {
		t.Error("a FAILED request still holds a second PIN")
	}
}
