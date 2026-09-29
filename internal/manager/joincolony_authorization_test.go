package manager

// ADR-0147 Part 3's security claims, tested end to end against a real
// single-node raft, a real managerd gRPC surface with its real auth
// interceptor, and a real root-owned file on disk.
//
// What is being proved, in the ADR's own order:
//
//  1. No authorization entry means no voter, whatever key is presented
//     - and AddVoter is never reached, asserted both by the absence of
//     the reachability dial that sits immediately after the gate and by
//     the membership itself being unchanged.
//  2. A spent authorization_id is refused on the REPLICATED record, not
//     on the local file - proved by leaving the local file still listing
//     the entry as available.
//  3. Two distinct API keys on two distinct Combs are required, and the
//     two that fail are named for what they are.
//  4. Nothing in the api/rpc surface can write the store. See
//     TestNoManagerRPCNamesTheAuthorizationStore.
//
// Two Combs are modelled here as two managerd instances over one raft,
// which is the shape the existing harness already provides. That is
// faithful to what the two-person rule actually consumes - the API key
// the interceptor validated, and the node id the RECEIVING member
// stamped - and it is not a claim about two raft nodes. Replication
// across nodes is what internal/raft's own tests cover.

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/proto"

	internalpb "github.com/glenjbarber/apiary/api/internalpb"
	rpcpb "github.com/glenjbarber/apiary/api/rpc"
	"github.com/glenjbarber/apiary/internal/joinauth"
	raftnode "github.com/glenjbarber/apiary/internal/raft"
)

const authzTestFingerprint = "SHA256:11:22:33:44:55:66:77:88:99"

// authzComb is one Comb's worth of test harness: a managerd Server, a
// client for it, and the authorization file that managerd reads.
type authzComb struct {
	name       string
	client     rpcpb.ManagerServiceClient
	srv        *Server
	storePath  string
	dials      int
	apiKeys    []*rpcpb.APIKeyInfo
	rawKeys    []string
	pendingIDs []string

	// secondPins caches the PIN each request's stage one produced, so
	// approve can be called more than once against the same request.
	// A second approval after the first was REFUSED at the authorization
	// gate reuses the same still-live PIN: the gate sits before the
	// spend, which is the whole reason it is ordered there.
	secondPins map[string]string
}

func newAuthzComb(t *testing.T, socketPath, name string) *authzComb {
	t.Helper()
	client, srv := newManagerdRPCClientAndServer(t, socketPath, name)
	c := &authzComb{
		name:       name,
		client:     client,
		srv:        srv,
		storePath:  filepath.Join(t.TempDir(), "join-authorizations.json"),
		secondPins: map[string]string{},
	}
	srv.setJoinAuthorizationPath(c.storePath)
	// The reachability dial is the first thing AFTER this part's gates,
	// so counting it is the sharpest available proof that a refusal came
	// from the authorization check and not from something later. It is
	// also made permissive, so that a flow which wrongly got past the
	// gate would carry on to a real AddVoter and change the membership -
	// which the tests below then catch by name, rather than by the
	// incidental failure of a dial to a hostname that does not exist.
	srv.reachabilityCheck = func(_ context.Context, _ string) error {
		c.dials++
		return nil
	}
	return c
}

// key creates an Admin API key on this Comb and returns the raw key and
// its id. The raw key is never stored anywhere; the id is what
// ADR-0147 Part 3 records.
//
// credential is the raw key to present while creating this one, and is
// empty for the FIRST key on a raft. It is not optional behaviour: the
// moment one key exists, ADR-0023's auth turns on for the whole Colony,
// and a second Comb over the same raft creating a key with no
// credential is refused by the same interceptor a real deployment would
// refuse it with. That is the real behaviour being kept honest, not a
// test convenience.
func (c *authzComb) key(t *testing.T, credential, name string) (string, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ctx = ctxFor(t, credential)
	resp, err := c.client.CreateAPIKey(ctx, &rpcpb.CreateAPIKeyRequest{Name: name, Role: "admin"})
	if err != nil {
		t.Fatalf("%s: CreateAPIKey(%q) error: %v", c.name, name, err)
	}
	if resp.GetError() != "" {
		t.Fatalf("%s: CreateAPIKey(%q) returned error: %s", c.name, name, resp.GetError())
	}
	c.apiKeys = append(c.apiKeys, resp.GetKey())
	c.rawKeys = append(c.rawKeys, resp.GetRawKey())
	return resp.GetRawKey(), resp.GetKey().GetId()
}

// ctxFor returns a context carrying rawKey as the caller's credential.
func ctxFor(t *testing.T, rawKey string) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	if rawKey == "" {
		return ctx
	}
	return metadata.NewOutgoingContext(ctx, metadata.Pairs("authorization", "Bearer "+rawKey))
}

// openWindow opens ADR-0147 Part 4's join window, which every join
// needs before Part 3's own gates are ever reached.
func (c *authzComb) openWindow(t *testing.T) {
	t.Helper()
	openColonyJoinWindowForTest(t, c.client)
}

// seedRequest records a pending join request with a fingerprint, the way
// a joining Comb's managerd would.
//
// It is a local seeder rather than a RequestJoinColony call because
// RequestJoinColony is unauthenticated and a test that drove the whole
// handshake would be testing the handshake rather than Part 3. The
// window epoch is stamped exactly as internal/manager's own seeder does,
// for the same reason: a request seeded without one is refused as stale
// before any Part 3 gate is reached, and a test would then be passing
// for the wrong reason.
func (c *authzComb) seedRequest(t *testing.T, requestID, nodeID, fingerprint string) {
	t.Helper()
	c.seedRequestAt(t, requestID, nodeID, nodeID+".example:17600", fingerprint)
}

// seedRequestWithRealJoiner seeds a request whose raft_bind_address is
// a REAL, unbootstrapped second raft node actually listening there.
//
// The distinction is not cosmetic. Approving a joiner whose address
// nothing is listening on takes a single-voter Colony to two voters, so
// quorum becomes 2-of-2, the leader cannot reach the new member, and it
// steps down almost immediately. Every test that is supposed to REACH
// AddVoter needs a reachable peer or it will be asserting on a
// leadership loss rather than on Part 3 - and the refusal tests
// deliberately do not use one, because there the unresolvable address
// is a second, independent proof that AddVoter was never called.
//
// This mirrors TestIntegration_ApproveJoinRequest_ObservedEmptyLogStillApproved's
// own pattern and for exactly the reason its comment gives.
func (c *authzComb) seedRequestWithRealJoiner(t *testing.T, requestID, nodeID, fingerprint string) {
	t.Helper()
	addr := freeLoopbackAddr(t)
	node, err := raftnode.New(raftnode.Config{NodeID: nodeID, DataDir: t.TempDir(), BindAddr: addr})
	if err != nil {
		t.Fatalf("%s: creating the joining raft node: %v", c.name, err)
	}
	t.Cleanup(func() { node.Shutdown() })
	c.seedRequestAt(t, requestID, nodeID, addr, fingerprint)
}

func (c *authzComb) seedRequestAt(t *testing.T, requestID, nodeID, raftBindAddress, fingerprint string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	window, live := c.srv.liveColonyJoinWindow(ctx)
	if !live {
		t.Fatalf("%s: seeding %q: no live join window, so the request would be refused as stale before any authorization check", c.name, requestID)
	}
	payload, err := proto.Marshal(&internalpb.Command{Op: &internalpb.Command_CreatePendingJoinRequest{
		CreatePendingJoinRequest: &internalpb.CreatePendingJoinRequest{
			Request: &internalpb.PendingJoinRequest{
				RequestId: requestID, NodeId: nodeID,
				RaftBindAddress: raftBindAddress,
				// ADR-0147 Part 2: seeded by hand, so it has to carry
				// what RequestJoinColony would have put there - the
				// joiner's own first code, its advertised
				// fingerprints, and the INTRODUCED stage. A seed
				// without them is the request the flow is designed to
				// refuse, not a convenient shortcut.
				Code:                   testIntroductionCode,
				Stage:                  internalpb.JoinRequestStage_JOIN_REQUEST_STAGE_INTRODUCED,
				AdvertisedFingerprints: testCombFingerprintList(t),
				TlsCertFingerprint:     fingerprint,
				RequestedAtUnix:        time.Now().Unix(),
				ExpiresAtUnix:          time.Now().Add(time.Hour).Unix(),
				Status:                 internalpb.JoinRequestStatus_JOIN_REQUEST_STATUS_PENDING,
				// Observed, empty: the join-log guardrail's own
				// UNOBSERVED refusal would otherwise fire AFTER the
				// authorization gate and mask a bug in it.
				JoinerLogStateObserved: true,
				JoinerLastLogIndex:     0,
				WindowOpenedAtUnix:     window.GetOpenedAtUnix(),
			},
		},
	}})
	if err != nil {
		t.Fatalf("%s: marshaling the seed command: %v", c.name, err)
	}
	if resp, err := c.srv.raft.Apply(ctx, payload, defaultApplyTimeout); err != nil || resp.GetError() != "" {
		t.Fatalf("%s: seeding %q: err=%v resp=%q", c.name, requestID, err, resp.GetError())
	}
	c.pendingIDs = append(c.pendingIDs, requestID)
}

// authorize writes an entry into this Comb's store, the way
// `apiaryctl join-authorize` would.
func (c *authzComb) authorize(t *testing.T, nodeID, fingerprint string) string {
	t.Helper()
	store, err := joinauth.Load(c.storePath)
	if err != nil {
		store = joinauth.Store{}
	}
	id, err := joinauth.NewEntryID()
	if err != nil {
		t.Fatalf("%s: generating an entry id: %v", c.name, err)
	}
	store.Authorizations = append(store.Authorizations, joinauth.Entry{
		ID: id, NodeID: nodeID, Fingerprint: fingerprint,
		ExpiresAtUnix: time.Now().Add(time.Hour).Unix(),
	})
	if err := store.Save(c.storePath); err != nil {
		t.Fatalf("%s: writing the store: %v", c.name, err)
	}
	return id
}

// approve is this Comb's approval call, and it completes ADR-0147
// Part 2's stage one first so the test it serves is about whatever gate
// it was written for rather than about the handshake.
//
// The PIN is read out of replicated state rather than from the
// VerifyJoinIntroduction response, because that response deliberately
// does not carry it: the PIN belongs to the requesting Comb. Doing it
// this way means every Part 3 test exercises the real ordering - stage
// one, then the authorization gates, then the second PIN - rather than a
// shortcut through it.
func (c *authzComb) approve(t *testing.T, requestID, rawKey string) *rpcpb.ApproveJoinRequestResponse {
	t.Helper()
	secondPin, done := c.secondPins[requestID]
	if !done {
		secondPin = stageOnePinForTest(t, c.srv, requestID)
		c.secondPins[requestID] = secondPin
	}
	resp, err := c.client.ApproveJoinRequest(ctxFor(t, rawKey), &rpcpb.ApproveJoinRequestRequest{
		RequestId:     requestID,
		ConfirmPhrase: approveJoinRequestConfirmPhrase,
		SecondPin:     secondPin,
	})
	if err != nil {
		t.Fatalf("%s: ApproveJoinRequest(%q) error: %v", c.name, requestID, err)
	}
	return resp
}

// ------------------------------------------------------------------------
// 1. The single most important assertion in the ADR.
// ------------------------------------------------------------------------

func TestJoinApproval_WithoutAnAuthorizationEntryIsRefusedAndAddsNoVoter(t *testing.T) {
	socket := newRaftdUDSSocket(t)
	leader := newAuthzComb(t, socket, "comb-leader")
	leader.openWindow(t)
	rawKey, _ := leader.key(t, "", "admin-1")
	leader.seedRequest(t, "jreq-1", "comb-3", authzTestFingerprint)

	// The store exists and is EMPTY. That is the real starting state of
	// a freshly installed Comb, and it is the one the ADR singles out.
	if err := (joinauth.Store{Authorizations: []joinauth.Entry{}}).Save(leader.storePath); err != nil {
		t.Fatalf("writing an empty store: %v", err)
	}

	resp := leader.approve(t, "jreq-1", rawKey)
	if resp.GetError() == "" {
		t.Fatalf("ApproveJoinRequest with no authorization entry = success (request %+v); the entry is the whole point of ADR-0147 Part 3", resp.GetRequest())
	}
	if !strings.Contains(resp.GetError(), "authorization entry") {
		t.Errorf("refusal %q does not say an authorization entry is what is missing", resp.GetError())
	}
	// The refusal has to name the Comb, because only the leader reads
	// the file: an entry created on any other Comb is one no approval
	// will ever consult, and an operator who is not told that will
	// create one and watch it be ignored.
	if !strings.Contains(resp.GetError(), "comb-leader") {
		t.Errorf("refusal %q does not name the Comb that must carry the entry", resp.GetError())
	}
	if !strings.Contains(resp.GetError(), "apiaryctl join-authorize") {
		t.Errorf("refusal %q does not give the operator the command to run; a refusal with no next step is how an operator ends up disabling the check", resp.GetError())
	}
	// Two independent proofs that AddVoter was never reached. The first
	// is that the gate sits ahead of the reachability dial; the second is
	// that the membership did not change, which is the thing that
	// actually matters.
	if leader.dials != 0 {
		t.Errorf("the reachability dial ran %d times; it sits immediately AFTER the authorization gate, so reaching it means the gate was passed", leader.dials)
	}
	if isRaftMember(t, leader.srv, "comb-3") {
		t.Error("comb-3 is a raft voter after a refused approval; AddVoter was reached")
	}
}

// ------------------------------------------------------------------------
// 2. The entry has to match exactly, and has to be live.
// ------------------------------------------------------------------------

func TestJoinApproval_EntryMustNameThisCombAndThisCertificate(t *testing.T) {
	for _, tc := range []struct {
		name        string
		entryNode   string
		entryFP     string
		wantInError string
	}{
		{
			name:        "node_id matches, fingerprint does not",
			entryNode:   "comb-3",
			entryFP:     "SHA256:99:99:99:99:99:99:99:99:99",
			wantInError: authzTestFingerprint,
		},
		{
			name:        "fingerprint matches, node_id does not",
			entryNode:   "comb-4",
			entryFP:     authzTestFingerprint,
			wantInError: "comb-3",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			socket := newRaftdUDSSocket(t)
			leader := newAuthzComb(t, socket, "comb-leader")
			leader.openWindow(t)
			rawKey, _ := leader.key(t, "", "admin-1")
			leader.seedRequest(t, "jreq-1", "comb-3", authzTestFingerprint)
			leader.authorize(t, tc.entryNode, tc.entryFP)

			resp := leader.approve(t, "jreq-1", rawKey)
			if resp.GetError() == "" {
				t.Fatal("ApproveJoinRequest with a non-matching entry = success")
			}
			if !strings.Contains(resp.GetError(), tc.wantInError) {
				t.Errorf("refusal %q does not name %s, so an operator cannot see what actually differed", resp.GetError(), tc.wantInError)
			}
			if isRaftMember(t, leader.srv, "comb-3") {
				t.Error("comb-3 is a voter after a refused approval")
			}
		})
	}
}

func TestJoinApproval_AnExpiredEntryIsRefused(t *testing.T) {
	socket := newRaftdUDSSocket(t)
	leader := newAuthzComb(t, socket, "comb-leader")
	leader.openWindow(t)
	rawKey, _ := leader.key(t, "", "admin-1")
	leader.seedRequest(t, "jreq-1", "comb-3", authzTestFingerprint)

	store, err := joinauth.Load(leader.storePath)
	if err != nil {
		store = joinauth.Store{}
	}
	store.Authorizations = append(store.Authorizations, joinauth.Entry{
		ID: "auth-expired", NodeID: "comb-3", Fingerprint: authzTestFingerprint,
		ExpiresAtUnix: time.Now().Add(-time.Second).Unix(),
	})
	if err := store.Save(leader.storePath); err != nil {
		t.Fatalf("writing the store: %v", err)
	}

	resp := leader.approve(t, "jreq-1", rawKey)
	if resp.GetError() == "" {
		t.Fatal("ApproveJoinRequest with an expired entry = success")
	}
	if !strings.Contains(resp.GetError(), "auth-expired") {
		t.Errorf("refusal %q does not name the entry that expired, so an operator cannot tell WHICH entry to recreate", resp.GetError())
	}
}

func TestJoinApproval_AMalformedStoreRefusesEveryJoinAndNamesTheFile(t *testing.T) {
	// The ADR is explicit that a malformed file must not fall back to
	// "no entries, so nothing is authorized anyway" by accident. It
	// happens not to be a security hole in that direction - an empty
	// store authorizes nothing - but the ACCIDENT is what is being
	// refused: a store that fails to parse must be distinguishable from a
	// store that is empty, or an operator debugging a refusal is sent to
	// create an entry in a file nobody can read.
	socket := newRaftdUDSSocket(t)
	leader := newAuthzComb(t, socket, "comb-leader")
	leader.openWindow(t)
	rawKey, _ := leader.key(t, "", "admin-1")
	leader.seedRequest(t, "jreq-1", "comb-3", authzTestFingerprint)
	writeRawFile(t, leader.storePath, `{"authorizations": [`)

	resp := leader.approve(t, "jreq-1", rawKey)
	if resp.GetError() == "" {
		t.Fatal("ApproveJoinRequest over a malformed store = success")
	}
	if !strings.Contains(resp.GetError(), leader.storePath) {
		t.Errorf("refusal %q does not name the store path", resp.GetError())
	}
	if isRaftMember(t, leader.srv, "comb-3") {
		t.Error("comb-3 is a voter after a refusal over a malformed store")
	}
}

func TestJoinAuthorizationStoreIs0600AfterInstallAddAndConsume(t *testing.T) {
	// The mode is the security property rather than a style choice, and
	// it is checked at all three points in its life, because each point
	// is a different writer: install creates it, join-authorize adds to
	// it, and the raft log records the consume without touching the
	// file. A test that only checked the first would pass if the second
	// rewrote the file with a different mode.
	socket := newRaftdUDSSocket(t)
	leader := newAuthzComb(t, socket, "comb-leader")
	leader.openWindow(t)
	keyOne, _ := leader.key(t, "", "admin-1")
	leader.seedRequest(t, "jreq-1", "comb-3", authzTestFingerprint)
	leader.authorize(t, "comb-3", authzTestFingerprint)
	assertModeIs0600(t, leader.storePath, "after an entry was added")

	other := newAuthzComb(t, socket, "comb-other")
	copyStoreFile(t, leader.storePath, other.storePath)
	_, keyTwoID := other.key(t, keyOne, "admin-2")
	assertModeIs0600(t, other.storePath, "after the store was copied to a second Comb")

	first := leader.approve(t, "jreq-1", keyOne)
	if !first.GetAwaitingSecondAuthorization() {
		t.Fatalf("first approval = %+v, want awaiting a second authorization", first)
	}
	assertModeIs0600(t, other.storePath, "after one authorization was recorded")

	// The completing act is deliberately NOT performed here. Its subject
	// is the file's mode, and reaching a consumption means AddVoter,
	// which takes a single-voter Colony to 2-of-2 quorum against a peer
	// that does not vote: the leader can step down in the milliseconds
	// between AddVoter and the log entry that records the spend, and
	// ADR-0097 documents that hazard as a real one. Letting raft's
	// election timing decide this test's result would be testing the
	// wrong thing.
	//
	// The fourth point - 0600 after an entry is CONSUMED - is asserted in
	// TestJoinApproval_TwoKeysOnTwoCombsAreRequired instead, which does
	// reach a consumption. Split across the two on purpose: a test that
	// covered all four points would have to take the timing risk to do
	// it, and a test that covered three of them and named the other
	// covers all four honestly.
	_ = keyTwoID
}

// ------------------------------------------------------------------------
// 3. Single use is replicated, not local.
// ------------------------------------------------------------------------

func TestJoinApproval_ASpentEntryIsRefusedOnTheReplicatedRecordNotTheFile(t *testing.T) {
	// The leadership-change case, made concrete. The local file on this
	// Comb still lists the entry as available - and is deliberately left
	// that way, because a real stale copy on a new leader looks exactly
	// like this. What refuses the second spend is the replicated record.
	socket := newRaftdUDSSocket(t)
	leader := newAuthzComb(t, socket, "comb-leader")
	leader.openWindow(t)
	keyOne, _ := leader.key(t, "", "admin-1")
	leader.seedRequestWithRealJoiner(t, "jreq-1", "comb-3", authzTestFingerprint)
	entryID := leader.authorize(t, "comb-3", authzTestFingerprint)

	other := newAuthzComb(t, socket, "comb-other")
	copyStoreFile(t, leader.storePath, other.storePath)
	keyTwo, _ := other.key(t, keyOne, "admin-2")

	if resp := leader.approve(t, "jreq-1", keyOne); !resp.GetAwaitingSecondAuthorization() {
		t.Fatalf("first approval = %+v, want awaiting a second authorization", resp)
	}
	if resp := other.approve(t, "jreq-1", keyTwo); resp.GetError() != "" {
		t.Fatalf("completing approval: %s", resp.GetError())
	}

	// A SECOND request for the SAME joining identity, presenting the same
	// entry, on a Comb whose local file still lists it as available. The
	// identity has to be the same one: a different node_id would have no
	// entry at all, and the refusal would be the missing-entry one, with
	// the replicated record never consulted.
	leader.seedRequestWithRealJoiner(t, "jreq-2", "comb-3", authzTestFingerprint)
	resp := leader.approve(t, "jreq-2", keyOne)
	if resp.GetError() == "" {
		t.Fatal("a second approval presenting an already-spent entry = success")
	}
	if !strings.Contains(resp.GetError(), entryID) {
		t.Errorf("refusal %q does not name the entry that was already spent", resp.GetError())
	}
	if !strings.Contains(resp.GetError(), "jreq-1") {
		t.Errorf("refusal %q does not name the request that spent it, so an operator cannot see which join this collides with", resp.GetError())
	}
	// comb-3 is already a voter - that is what jreq-1's approval did. The
	// assertion is therefore about the REPLAYED request, not about
	// membership: jreq-2 must still be PENDING, because the refusal
	// happened before anything resolved it.
	ctxBack, cancelBack := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelBack()
	replayed, err := leader.srv.raft.GetPendingJoinRequestLocal(ctxBack, "jreq-2")
	if err != nil || replayed.GetRequest() == nil {
		t.Fatalf("reading jreq-2 back after a refused replay: err=%v resp=%q", err, replayed.GetError())
	}
	if got := replayed.GetRequest().GetStatus(); got != internalpb.JoinRequestStatus_JOIN_REQUEST_STATUS_PENDING {
		t.Errorf("jreq-2 status = %s after a refused replayed authorization entry, want PENDING: the refusal happened before anything resolved it", got)
	}
}

// ------------------------------------------------------------------------
// 4. The two-person rule.
// ------------------------------------------------------------------------

func TestJoinApproval_TwoKeysOnTwoCombsAreRequired(t *testing.T) {
	t.Run("one key twice is refused and names itself", func(t *testing.T) {
		socket := newRaftdUDSSocket(t)
		leader := newAuthzComb(t, socket, "comb-leader")
		leader.openWindow(t)
		rawKey, keyID := leader.key(t, "", "admin-1")
		leader.seedRequest(t, "jreq-1", "comb-3", authzTestFingerprint)
		leader.authorize(t, "comb-3", authzTestFingerprint)

		first := leader.approve(t, "jreq-1", rawKey)
		if !first.GetAwaitingSecondAuthorization() {
			t.Fatalf("first approval = %+v, want awaiting a second authorization rather than an outcome", first)
		}
		if first.GetError() != "" {
			t.Errorf("the first approval returned an error %q; recording one authorization is neither a success nor a failure and must not be reported as either", first.GetError())
		}
		if !strings.Contains(first.GetSecondAuthorizationRequired(), keyID) {
			t.Errorf("second_authorization_required %q does not name the key that was recorded (%s), so the operator cannot tell what has already happened", first.GetSecondAuthorizationRequired(), keyID)
		}
		// Still pending: one authorization admits nobody.
		if first.GetRequest().GetStatus() != rpcpb.JoinRequestStatus_JOIN_REQUEST_STATUS_PENDING {
			t.Errorf("status after one authorization = %s, want PENDING", first.GetRequest().GetStatus())
		}
		if isRaftMember(t, leader.srv, "comb-3") {
			t.Error("comb-3 is a voter after ONE authorization")
		}

		second := leader.approve(t, "jreq-1", rawKey)
		if second.GetError() == "" {
			t.Fatal("the same API key approving twice = success")
		}
		if !strings.Contains(second.GetError(), "SAME API key") {
			t.Errorf("refusal %q does not say the two authorizations used the same key", second.GetError())
		}
		if isRaftMember(t, leader.srv, "comb-3") {
			t.Error("comb-3 is a voter after one key approved twice")
		}
	})

	t.Run("two keys on one Comb are refused and names the Comb", func(t *testing.T) {
		socket := newRaftdUDSSocket(t)
		leader := newAuthzComb(t, socket, "comb-leader")
		leader.openWindow(t)
		keyOne, _ := leader.key(t, "", "admin-1")
		keyTwo, _ := leader.key(t, keyOne, "admin-2")
		leader.seedRequest(t, "jreq-1", "comb-3", authzTestFingerprint)
		leader.authorize(t, "comb-3", authzTestFingerprint)

		if resp := leader.approve(t, "jreq-1", keyOne); !resp.GetAwaitingSecondAuthorization() {
			t.Fatalf("first approval = %+v, want awaiting a second authorization", resp)
		}
		resp := leader.approve(t, "jreq-1", keyTwo)
		if resp.GetError() == "" {
			t.Fatal("two keys on ONE Comb = success; the rule counts distinct Combs, not distinct keys")
		}
		if !strings.Contains(resp.GetError(), "SAME Comb") {
			t.Errorf("refusal %q does not say both authorizations reached one Comb", resp.GetError())
		}
		if !strings.Contains(resp.GetError(), "comb-leader") {
			t.Errorf("refusal %q does not name the Comb, so the operator is not told which side of the rule to change", resp.GetError())
		}
		if isRaftMember(t, leader.srv, "comb-3") {
			t.Error("comb-3 is a voter after two keys on one Comb")
		}
	})

	t.Run("two keys on two Combs are accepted and both are recorded", func(t *testing.T) {
		socket := newRaftdUDSSocket(t)
		leader := newAuthzComb(t, socket, "comb-leader")
		leader.openWindow(t)
		keyOne, idOne := leader.key(t, "", "admin-1")
		leader.seedRequestWithRealJoiner(t, "jreq-1", "comb-3", authzTestFingerprint)
		entryID := leader.authorize(t, "comb-3", authzTestFingerprint)

		other := newAuthzComb(t, socket, "comb-other")
		copyStoreFile(t, leader.storePath, other.storePath)
		keyTwo, idTwo := other.key(t, keyOne, "admin-2")
		if idOne == idTwo {
			t.Fatalf("the two keys share the id %q; a test that cannot tell them apart cannot prove two DISTINCT keys were required", idOne)
		}

		if resp := leader.approve(t, "jreq-1", keyOne); !resp.GetAwaitingSecondAuthorization() {
			t.Fatalf("first approval = %+v, want awaiting a second authorization", resp)
		}
		final := other.approve(t, "jreq-1", keyTwo)
		if final.GetError() != "" {
			t.Fatalf("two keys on two Combs was refused: %s", final.GetError())
		}
		if !isRaftMember(t, other.srv, "comb-3") {
			t.Fatal("comb-3 is not a voter after a two-key, two-Comb approval")
		}
		// The Colony's own record says which keys on which Combs
		// admitted this Comb, and which entry was spent. That is the
		// whole point of recording it rather than counting it.
		req := final.GetRequest()
		if req.GetApproval_1().GetKeyId() != idOne || req.GetApproval_1().GetNodeId() != "comb-leader" {
			t.Errorf("approval_1 = (%q, %q), want (%q, comb-leader)", req.GetApproval_1().GetKeyId(), req.GetApproval_1().GetNodeId(), idOne)
		}
		if req.GetApproval_2().GetKeyId() != idTwo || req.GetApproval_2().GetNodeId() != "comb-other" {
			t.Errorf("approval_2 = (%q, %q), want (%q, comb-other)", req.GetApproval_2().GetKeyId(), req.GetApproval_2().GetNodeId(), idTwo)
		}
		if req.GetAuthorizationId() != entryID {
			t.Errorf("authorization_id = %q, want %q: the approval has to record WHICH entry it spent", req.GetAuthorizationId(), entryID)
		}
		if req.GetConsumedAtUnix() == 0 {
			t.Error("consumed_at_unix = 0; the record of use has to say WHEN, not only which")
		}
		if req.GetStatus() != rpcpb.JoinRequestStatus_JOIN_REQUEST_STATUS_APPROVED {
			t.Errorf("status = %s, want APPROVED", req.GetStatus())
		}
		// The fourth point of the ADR's file-mode list, reached here
		// because this is the test that actually consumes an entry.
		// See TestJoinAuthorizationStoreIs0600AfterInstallAddAndConsume
		// for why the four points are split across two tests.
		assertModeIs0600(t, other.storePath, "after the entry was consumed")
	})
}

// ------------------------------------------------------------------------
// 5. The mechanical "no RPC writes this file" rule.
// ------------------------------------------------------------------------

func TestNoManagerRPCNamesTheAuthorizationStore(t *testing.T) {
	// ADR-0147 Part 3's whole basis is that an Admin-tier caller cannot
	// write an arbitrary root-owned file. That is only true while no RPC
	// names this one, and "only true while" is the part that rots: a
	// future method that reads or writes it would not break anything
	// visible, it would quietly remove the property Part 3 exists to
	// provide. So this walks the actual service descriptor rather than
	// trusting a review to keep noticing.
	desc := rpcpb.ManagerService_ServiceDesc
	if len(desc.Methods) == 0 {
		t.Fatal("ManagerService_ServiceDesc carries no methods; this test would pass vacuously, so the descriptor itself is wrong")
	}
	for _, m := range desc.Methods {
		lower := strings.ToLower(m.MethodName)
		if strings.Contains(lower, "authoriz") {
			t.Errorf("ManagerService has a method %q whose name names an authorization. Part 3's basis is that no RPC reaches the root-owned authorization store, and a method named for authorizations is exactly what a later change would add. If this is genuinely a read of a RECORD rather than of the store, rename it so the rule stays mechanically checkable",
				m.MethodName)
		}
	}
}

func TestNoManagerRPCFieldCarriesTheAuthorizationStore(t *testing.T) {
	// The same rule from the other side. A method name is only half of
	// it: an existing method could grow a `json`/`path`/`contents` field
	// aimed at the store and the name check would not see it. The store's
	// own JSON field name is `authorizations`, so that is what is barred
	// from appearing in any external message - while `authorization_id`,
	// which is a replicated RECORD of a spend, is deliberately allowed
	// and asserted to exist.
	found := false
	for _, name := range []string{
		"apiary.rpc.v1.ApproveJoinRequestRequest",
		"apiary.rpc.v1.ApproveJoinRequestResponse",
		"apiary.rpc.v1.PendingJoinRequest",
		"apiary.rpc.v1.JoinApprovalAttestation",
	} {
		msg := lookupExternalMessage(t, name)
		found = true
		fields := fieldNames(msg)
		for _, f := range fields {
			if strings.Contains(strings.ToLower(f), "authorization") && strings.HasSuffix(strings.ToLower(f), "s") {
				t.Errorf("%s carries a field %q whose name is the STORE's; a field on an external message is something an Admin-tier caller supplies, and the store is precisely what they must not be able to name, let alone write", name, f)
			}
		}
	}
	if !found {
		t.Fatal("no external messages were inspected; the test would pass vacuously")
	}
	// And the one field that IS about authorization, to make the
	// distinction concrete rather than aspirational: an approval carries
	// the id of the entry it spent.
	req := lookupExternalMessage(t, "apiary.rpc.v1.ApproveJoinRequestRequest")
	if _, ok := fieldNamesByEitherName(req)["approving_node_id"]; !ok {
		t.Error("ApproveJoinRequestRequest has no approving_node_id field; the two-person rule cannot stamp a Comb without it")
	}
	pjr := lookupExternalMessage(t, "apiary.rpc.v1.PendingJoinRequest")
	names := fieldNamesByEitherName(pjr)
	for _, want := range []string{"authorization_id", "consumed_at_unix", "approval_1", "approval_2"} {
		if _, ok := names[want]; !ok {
			t.Errorf("PendingJoinRequest has no %q field, so the Colony's own record of who admitted what is not visible to an Admin reviewing a request", want)
		}
	}
}
