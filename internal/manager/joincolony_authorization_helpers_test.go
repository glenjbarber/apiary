package manager

// ADR-0147 Part 3 changed what an approval REQUIRES, so every
// pre-existing test that calls ApproveJoinRequest now has to do what an
// operator does first: authorize the join, from two Combs, with two
// keys. This is that step, in one place.
//
// The point of putting it here rather than inline in a dozen tests is
// that it must be impossible to satisfy Part 3 by accident. Every path
// goes through the same real Store, the same real RaftClient and the
// same real ApproveJoinRequest; there is no bypass below.
//
// What these helpers do NOT cover, and where that is covered instead:
// the auth interceptor's identification of the caller's key, and the
// stamping of approving_node_id by the member that RECEIVED the call.
// Those need two real managerd gRPC surfaces, and
// TestJoinApproval_TwoKeysOnTwoCombsAreRequired builds them.

import (
	"context"
	"testing"
	"time"

	"google.golang.org/grpc/metadata"

	rpcpb "github.com/glenjbarber/apiary/api/rpc"
	"github.com/glenjbarber/apiary/internal/joinauth"
)

// secondCombNodeID is the identity the helper stamps onto the second
// authorizing act. A fixed, obviously-fake value on purpose: a reader
// looking at a failing assertion should be tell at a glance that this
// Comb does not exist and the value came from a helper.
const secondCombNodeID = "comb-second-authorization-helper"

// operatorAuthorizedApprovalForTest performs the whole of what a
// successful approval takes under ADR-0147 Part 3 - an operator
// authorization, spent from two Combs with two keys - and returns the
// response of the SECOND, completing act.
//
// It returns the SECOND act's response rather than making a third call
// for a few reasons that are all the same reason. The second act is the
// approval: it is what crosses the two-person gate and then runs the
// reachability dial, the not-already-a-voter check, the join-log
// guardrail and AddVoter. A third call would find the authorization
// entry already spent - correctly - and report THAT, which is a true
// fact about the wrong thing and would replace every refusal these tests
// exist to observe with a refusal about their own setup.
//
// So a test that expects an approval to be REFUSED gets the refusal from
// the check it is about, because the operator's half has already been
// done. That is the whole reason the two authorization acts live in one
// helper rather than at each call site: a test that authorized once and
// then asserted on its own call would be asserting on a refusal from
// the two-person gate instead, and would look green while proving
// nothing.
// bootstrap is an optional existing raw key, needed when this helper is
// called more than once on the same Colony: the first call creates the
// Colony's first API key, which turns authentication on for everything
// after it (ADR-0023), so a second call has to present something. The
// key returned by the first call is exactly that, which is why it is
// returned.
func operatorAuthorizedApprovalForTest(t *testing.T, client rpcpb.ManagerServiceClient, srv *Server, requestID string, bootstrap ...string) (*rpcpb.ApproveJoinRequestResponse, string) {
	t.Helper()

	// The request is read from this managerd's own raft rather than
	// through ListJoinRequests, because this helper is called more than
	// once in some tests and the second call happens AFTER it has
	// created an API key - at which point authentication is on for the
	// whole Colony and an unauthenticated ListJoinRequests is refused.
	// Reading local raft state sidesteps that without weakening
	// anything: the node_id and fingerprint are facts about the request,
	// not things the caller asserts.
	open, cancelOpen := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelOpen()
	get, err := srv.raft.GetPendingJoinRequestLocal(open, requestID)
	if err != nil || get.GetError() != "" || get.GetRequest() == nil {
		t.Fatalf("reading pending request %q: err=%v resp=%q", requestID, err, get.GetError())
	}
	target := get.GetRequest()

	writeAuthorizationEntryForTest(t, srv.joinAuthorizationFile(), target.GetNodeId(), target.GetTlsCertFingerprint())

	firstCtx, cancelFirst := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelFirst()
	if len(bootstrap) > 0 && bootstrap[0] != "" {
		firstCtx = metadata.NewOutgoingContext(firstCtx, metadata.Pairs("authorization", "Bearer "+bootstrap[0]))
	}
	keyOne, err := client.CreateAPIKey(firstCtx, &rpcpb.CreateAPIKeyRequest{Name: "operator-" + requestID, Role: "admin"})
	if err != nil || keyOne.GetError() != "" {
		t.Fatalf("CreateAPIKey(first) err=%v resp=%q", err, keyOne.GetError())
	}
	// The second key is created with the first one presented, which is
	// what a real operator's second key costs once auth is on. Both are
	// Admin because ApproveJoinRequest is Admin-gated, and a Viewer key
	// would be refused by the interceptor before any of this is reached.
	secondCtx, cancelSecond := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelSecond()
	secondCtx = metadata.NewOutgoingContext(secondCtx, metadata.Pairs("authorization", "Bearer "+keyOne.GetRawKey()))
	keyTwo, err := client.CreateAPIKey(secondCtx, &rpcpb.CreateAPIKeyRequest{Name: "operator2-" + requestID, Role: "admin"})
	if err != nil || keyTwo.GetError() != "" {
		t.Fatalf("CreateAPIKey(second) err=%v resp=%q", err, keyTwo.GetError())
	}
	if keyOne.GetKey().GetId() == keyTwo.GetKey().GetId() {
		t.Fatalf("both keys share the id %q; a test that cannot tell them apart cannot be exercising the two-person rule", keyOne.GetKey().GetId())
	}

	// Act one, through the real gRPC surface, so the interceptor's own
	// identification of the caller's key is exercised. It cannot
	// complete: one authorization is not two.
	actOne, err := client.ApproveJoinRequest(metadata.NewOutgoingContext(
		context.Background(), metadata.Pairs("authorization", "Bearer "+keyOne.GetRawKey())),
		&rpcpb.ApproveJoinRequestRequest{RequestId: requestID, ConfirmPhrase: approveJoinRequestConfirmPhrase})
	if err != nil {
		t.Fatalf("first authorizing ApproveJoinRequest() error: %v", err)
	}
	if actOne.GetError() != "" {
		t.Fatalf("first authorizing ApproveJoinRequest() returned error: %s", actOne.GetError())
	}
	if !actOne.GetAwaitingSecondAuthorization() {
		t.Fatalf("first authorizing call = %+v, want awaiting a second authorization rather than an outcome", actOne)
	}

	// Act two, on the Server directly, with the second key's REAL id in
	// the context and a different Comb stamped on the request. This is
	// the call whose response the caller gets: it is the approval.
	ctx, cancelSecond2 := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelSecond2()
	actTwo, err := srv.ApproveJoinRequest(
		context.WithValue(ctx, callerAPIKeyIDContextKey{}, keyTwo.GetKey().GetId()),
		&rpcpb.ApproveJoinRequestRequest{
			RequestId:       requestID,
			ConfirmPhrase:   approveJoinRequestConfirmPhrase,
			ApprovingNodeId: secondCombNodeID,
		})
	if err != nil {
		t.Fatalf("second authorizing ApproveJoinRequest() error: %v", err)
	}
	// The completing key comes back so a caller that makes further calls
	// on this Colony can authenticate as somebody. Creating a key turns
	// authentication on for the whole Colony, so every call after this
	// one needs a credential, and this helper is what created the
	// requirement.
	return actTwo, keyTwo.GetRawKey()
}

// writeAuthorizationEntryForTest writes one live entry into the store at
// path, creating the store if it is not there - the same shape
// `apiaryctl join-authorize` leaves behind.
func writeAuthorizationEntryForTest(t *testing.T, path, nodeID, fingerprint string) {
	t.Helper()
	store, err := joinauth.Load(path)
	if err != nil {
		store = joinauth.Store{}
	}
	id, err := joinauth.NewEntryID()
	if err != nil {
		t.Fatalf("generating an entry id: %v", err)
	}
	store.Authorizations = append(store.Authorizations, joinauth.Entry{
		ID: id, NodeID: nodeID, Fingerprint: fingerprint,
		ExpiresAtUnix: time.Now().Add(time.Hour).Unix(),
	})
	if err := store.Save(path); err != nil {
		t.Fatalf("writing the authorization store: %v", err)
	}
}
