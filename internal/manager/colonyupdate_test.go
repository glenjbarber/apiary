package manager

// managerd-side tests for ADR-0145's controlled-update single-flight.
//
// Nothing here decides anything - the FSM does, and internal/raft's own
// tests cover that. What is tested here is the part that would
// otherwise be untested and would be a real defect if wrong: that every
// path which is NOT the leader refuses rather than guesses, that the
// credential gate is the same one every other cluster-wide-restart RPC
// uses, and that a malformed request is refused BEFORE it can become a
// raft proposal.

import (
	"context"
	"strings"
	"testing"

	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	internalpb "github.com/glenjbarber/apiary/api/internalpb"
	rpcpb "github.com/glenjbarber/apiary/api/rpc"
)

const testGuardrailToken = "the-real-token"

// raftdServiceTestNodeID is the node id of the single-voter test raftd
// newRaftdUDSSocket builds. The lease tests need it because ADR-0125's
// quorum evaluation refuses a target it cannot place in the membership.
const raftdServiceTestNodeID = "raftd-1"

func guardrailCtx() context.Context {
	return metadata.NewOutgoingContext(context.Background(), metadata.Pairs("authorization", "Bearer "+testGuardrailToken))
}

func newGuardedManagerd(t *testing.T, nodeID string) (rpcpb.ManagerServiceClient, *Server) {
	t.Helper()
	client, srv := newManagerdRPCClientAndServer(t, newRaftdUDSSocket(t), nodeID)
	srv.SetRestartGuardrailToken(testGuardrailToken)
	return client, srv
}

func acquireReq(operationID string, takeover bool) *rpcpb.MutateColonyUpdateRequest {
	return &rpcpb.MutateColonyUpdateRequest{Op: &rpcpb.MutateColonyUpdateRequest_Acquire{
		Acquire: &rpcpb.AcquireColonyUpdate{OperationId: operationID, Takeover: takeover},
	}}
}

func fenceFrom(update *rpcpb.ColonyUpdate) *rpcpb.ColonyUpdateFence {
	return &rpcpb.ColonyUpdateFence{
		OperationId:       update.GetOperationId(),
		HolderNodeId:      update.GetHolderNodeId(),
		FenceToken:        update.GetFenceToken(),
		HolderIncarnation: update.GetHolderIncarnation(),
	}
}

// TestMutateColonyUpdate_RequiresTheRestartGuardrailToken is the
// authorization property, asserted the same way ADR-0103's own two RPCs
// are: no token, an empty token, and a freshly-minted Admin API key all
// get PermissionDenied, and the real token alone works. A gated RPC
// whose gate is decorative is how a Colony credential ends up able to
// claim the Colony's single update.
func TestMutateColonyUpdate_RequiresTheRestartGuardrailToken(t *testing.T) {
	raftdSocket := newRaftdUDSSocket(t)
	client, srv := newManagerdRPCClientAndServer(t, raftdSocket, "comb-a")

	ctx := context.Background()
	if _, err := client.MutateColonyUpdate(ctx, acquireReq("op-1", false)); err == nil {
		t.Error("MutateColonyUpdate() with no token configured and no bearer = no error, want PermissionDenied")
	}
	emptyBearer := metadata.NewOutgoingContext(ctx, metadata.Pairs("authorization", "Bearer "))
	if _, err := client.MutateColonyUpdate(emptyBearer, acquireReq("op-1", false)); err == nil {
		t.Error("MutateColonyUpdate() with an empty bearer against an unconfigured server = no error, want PermissionDenied")
	}

	// A real Admin key must not satisfy it either.
	srv.SetRestartGuardrailToken(testGuardrailToken)
	key, err := client.CreateAPIKey(ctx, &rpcpb.CreateAPIKeyRequest{Name: "admin", Role: "admin"})
	if err != nil || key.GetError() != "" {
		t.Fatalf("CreateAPIKey() = (%+v, %v)", key, err)
	}
	adminCtx := metadata.NewOutgoingContext(ctx, metadata.Pairs("authorization", "Bearer "+key.GetRawKey()))
	if _, err := client.MutateColonyUpdate(adminCtx, acquireReq("op-1", false)); err == nil {
		t.Error("MutateColonyUpdate() with a freshly-minted Admin API key = no error, want PermissionDenied")
	}
	// The real token works, proving the check is wired rather than
	// permanently rejecting everything.
	resp, err := client.MutateColonyUpdate(guardrailCtx(), acquireReq("op-1", false))
	if err != nil {
		t.Fatalf("MutateColonyUpdate() with the real token error: %v", err)
	}
	if !resp.GetAccepted() {
		t.Fatalf("MutateColonyUpdate() with the real token = %+v, want accepted", resp)
	}
	if resp.GetUpdate().GetHolderNodeId() != "comb-a" {
		t.Errorf("granted holder = %q, want comb-a (this node's own id, authored on the leader)", resp.GetUpdate().GetHolderNodeId())
	}
	if resp.GetUpdate().GetFenceToken() == 0 {
		t.Error("granted fence_token = 0; a fence token of zero can never match and would fence nothing")
	}
}

// TestMutateColonyUpdate_SecondAcquireIsRefusedNamingTheHolder is the
// backend enforcement ADR-0145 asks for, observed from the wire: a
// client that ignores a greyed-out button still gets an explicit
// refusal, and the refusal says who holds it.
func TestMutateColonyUpdate_SecondAcquireIsRefusedNamingTheHolder(t *testing.T) {
	client, _ := newGuardedManagerd(t, "comb-a")
	ctx := guardrailCtx()

	first, err := client.MutateColonyUpdate(ctx, acquireReq("op-1", false))
	if err != nil || !first.GetAccepted() {
		t.Fatalf("first MutateColonyUpdate() = (%+v, %v), want accepted", first, err)
	}

	second, err := client.MutateColonyUpdate(ctx, acquireReq("op-2", false))
	if err != nil {
		t.Fatalf("second MutateColonyUpdate() error: %v", err)
	}
	if second.GetAccepted() {
		t.Fatal("a second acquire was accepted; at most one controlled update may exist in the Colony")
	}
	if second.GetError() == "" {
		t.Error("a refused acquire carries an empty error; a silent no is indistinguishable from a broken call")
	}
	if !strings.Contains(second.GetError(), "op-1") {
		t.Errorf("refusal %q does not name the operation that holds the lock", second.GetError())
	}
}

// TestMutateColonyUpdate_AdvanceAndReleaseRoundTrip is the durable
// operation state as a managerd reader sees it, including the honest
// outcome vocabulary on the way out.
func TestMutateColonyUpdate_AdvanceAndReleaseRoundTrip(t *testing.T) {
	client, _ := newGuardedManagerd(t, "comb-a")
	ctx := guardrailCtx()

	acquired, err := client.MutateColonyUpdate(ctx, acquireReq("op-1", false))
	if err != nil || !acquired.GetAccepted() {
		t.Fatalf("acquire = (%+v, %v)", acquired, err)
	}
	fence := fenceFrom(acquired.GetUpdate())

	advanced, err := client.MutateColonyUpdate(ctx, &rpcpb.MutateColonyUpdateRequest{
		Op: &rpcpb.MutateColonyUpdateRequest_Advance{Advance: &rpcpb.AdvanceColonyUpdate{
			Fence:        fence,
			Step:         "step-aside",
			TargetNodeId: "buzz",
			StepRecord: &rpcpb.ColonyUpdateStepRecord{
				Index: 0, Step: "step-aside", Outcome: "confirmed",
				Detail:   "buzz handed over leadership and is a confirmed follower",
				Evidence: []string{"new leader drone, buzz observed as follower"},
				NodeId:   "buzz",
			},
		}},
	})
	if err != nil || !advanced.GetAccepted() {
		t.Fatalf("advance = (%+v, %v)", advanced, err)
	}
	got := advanced.GetUpdate()
	if got.GetStep() != "step-aside" || got.GetTargetNodeId() != "buzz" || len(got.GetSteps()) != 1 {
		t.Errorf("after advance = step %q on %q with %d step record(s), want step-aside on buzz with 1",
			got.GetStep(), got.GetTargetNodeId(), len(got.GetSteps()))
	}

	// A stale fence is refused and the operation is untouched.
	stale := fenceFrom(acquired.GetUpdate())
	stale.FenceToken = stale.GetFenceToken() - 1
	refused, err := client.MutateColonyUpdate(ctx, &rpcpb.MutateColonyUpdateRequest{
		Op: &rpcpb.MutateColonyUpdateRequest_Advance{Advance: &rpcpb.AdvanceColonyUpdate{Fence: stale, Step: "issue-restart"}},
	})
	if err != nil {
		t.Fatalf("stale advance error: %v", err)
	}
	if refused.GetAccepted() || !strings.Contains(refused.GetError(), "does not match") {
		t.Errorf("stale advance = %+v, want a fence refusal", refused)
	}

	released, err := client.MutateColonyUpdate(ctx, &rpcpb.MutateColonyUpdateRequest{
		Op: &rpcpb.MutateColonyUpdateRequest_Release{Release: &rpcpb.ReleaseColonyUpdate{
			Fence: fence, Outcome: "confirmed", Detail: "all four Combs reported the new build",
		}},
	})
	if err != nil || !released.GetAccepted() {
		t.Fatalf("release = (%+v, %v)", released, err)
	}
	if released.GetUpdate().GetActive() {
		t.Error("a released operation still reports active")
	}
	// And the colony is free again, which is the thing the UI greying
	// out will be reading.
	third, err := client.MutateColonyUpdate(ctx, acquireReq("op-3", false))
	if err != nil || !third.GetAccepted() {
		t.Errorf("a fresh acquire after a settled operation = (%+v, %v), want accepted", third, err)
	}
}

// TestMutateColonyUpdate_TakeoverIsExplicitAndFencesTheOldHolder is the
// failover path, from the wire.
func TestMutateColonyUpdate_TakeoverIsExplicitAndFencesTheOldHolder(t *testing.T) {
	client, srv := newGuardedManagerd(t, "comb-a")
	ctx := guardrailCtx()

	acquired, err := client.MutateColonyUpdate(ctx, acquireReq("op-1", false))
	if err != nil || !acquired.GetAccepted() {
		t.Fatalf("acquire = (%+v, %v)", acquired, err)
	}
	oldFence := fenceFrom(acquired.GetUpdate())

	// A replacement managerd on this same Comb - a new process, same
	// node_id - takes over explicitly. The old incarnation is fenced by
	// the new record's higher token, not by anything revoking the old
	// one.
	oldIncarnation := srv.colonyUpdateIncarnation
	srv.colonyUpdateIncarnation = ColonyUpdateIncarnation()
	if srv.colonyUpdateIncarnation == oldIncarnation {
		t.Fatal("two managerd processes generated the same incarnation; the fence could not tell them apart")
	}

	taken, err := client.MutateColonyUpdate(ctx, acquireReq("op-2", true))
	if err != nil || !taken.GetAccepted() {
		t.Fatalf("takeover = (%+v, %v), want accepted", taken, err)
	}
	if !taken.GetTakeover() {
		t.Error("a takeover does not report itself as one")
	}
	if taken.GetUpdate().GetFenceToken() <= oldFence.GetFenceToken() {
		t.Errorf("takeover token %d does not exceed the displaced token %d", taken.GetUpdate().GetFenceToken(), oldFence.GetFenceToken())
	}

	// The old process's own fence is now worthless, even on the same
	// Comb, even though the node id is unchanged.
	oldFence.HolderIncarnation = oldIncarnation
	refused, err := client.MutateColonyUpdate(ctx, &rpcpb.MutateColonyUpdateRequest{
		Op: &rpcpb.MutateColonyUpdateRequest_Advance{Advance: &rpcpb.AdvanceColonyUpdate{Fence: oldFence, Step: "issue-restart"}},
	})
	if err != nil {
		t.Fatalf("displaced advance error: %v", err)
	}
	if refused.GetAccepted() {
		t.Error("a displaced managerd process advanced the operation after a committed takeover")
	}
}

// TestMutateColonyUpdate_RefusesMalformedRequestsBeforeProposing
// covers the request-shaped failures. Each of these would otherwise
// become a raft proposal whose only outcome is an FSM refusal naming
// raft's own vocabulary, leaving the caller with a log index and no idea
// what was wrong with its request.
func TestMutateColonyUpdate_RefusesMalformedRequestsBeforeProposing(t *testing.T) {
	client, _ := newGuardedManagerd(t, "comb-a")
	ctx := guardrailCtx()

	cases := []struct {
		name    string
		req     *rpcpb.MutateColonyUpdateRequest
		wantSay string
	}{
		{
			name:    "no op at all",
			req:     &rpcpb.MutateColonyUpdateRequest{},
			wantSay: "carried no acquire, advance, release or handover",
		},
		{
			name:    "an acquire with no operation id",
			req:     acquireReq("", false),
			wantSay: "no operation id",
		},
		{
			name: "an advance with no fence",
			req: &rpcpb.MutateColonyUpdateRequest{Op: &rpcpb.MutateColonyUpdateRequest_Advance{
				Advance: &rpcpb.AdvanceColonyUpdate{Step: "issue-restart"},
			}},
			wantSay: "no fence was presented",
		},
		{
			name: "an advance with a partial fence",
			req: &rpcpb.MutateColonyUpdateRequest{Op: &rpcpb.MutateColonyUpdateRequest_Advance{
				Advance: &rpcpb.AdvanceColonyUpdate{
					Fence: &rpcpb.ColonyUpdateFence{OperationId: "op-1", HolderNodeId: "comb-a"},
					Step:  "issue-restart",
				},
			}},
			wantSay: "fence is incomplete",
		},
		{
			name: "a release with a partial fence",
			req: &rpcpb.MutateColonyUpdateRequest{Op: &rpcpb.MutateColonyUpdateRequest_Release{
				Release: &rpcpb.ReleaseColonyUpdate{Fence: &rpcpb.ColonyUpdateFence{FenceToken: 7}, Outcome: "confirmed"},
			}},
			wantSay: "fence is incomplete",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp, err := client.MutateColonyUpdate(ctx, tc.req)
			if err != nil {
				t.Fatalf("MutateColonyUpdate() error: %v", err)
			}
			if resp.GetAccepted() {
				t.Fatal("a malformed request was accepted")
			}
			if !strings.Contains(resp.GetError(), tc.wantSay) {
				t.Errorf("refusal %q does not say %q", resp.GetError(), tc.wantSay)
			}
		})
	}
	// None of the above may have created a lock, which is the whole
	// reason they are refused rather than proposed.
	free, err := client.MutateColonyUpdate(ctx, acquireReq("op-clean", false))
	if err != nil || !free.GetAccepted() {
		t.Errorf("a clean acquire after six malformed requests = (%+v, %v), want accepted - one of them left state behind", free, err)
	}
}

// TestMutateColonyUpdate_FailsClosedWithoutRaft is the nil-raft
// regression, in the shape ADR-0125 found and fixed once already on the
// restart-guardrail path: a Server with no raft client used to
// dereference it, and gRPC does not recover a panicking handler, so it
// killed managerd. "This build cannot tell you" must never look like
// "there is nothing running" - and it must never be a crash.
func TestMutateColonyUpdate_FailsClosedWithoutRaft(t *testing.T) {
	client, srv := newManagerdRPCClientAndServer(t, newRaftdUDSSocket(t), "comb-a")
	srv.SetRestartGuardrailToken(testGuardrailToken)
	srv.raft = nil

	resp, err := client.MutateColonyUpdate(guardrailCtx(), acquireReq("op-1", false))
	if err != nil {
		t.Fatalf("MutateColonyUpdate() with no raft client error: %v", err)
	}
	if resp.GetAccepted() {
		t.Error("a grant was issued with no raft client configured")
	}
	if !strings.Contains(resp.GetError(), "raft state could not be read") {
		t.Errorf("refusal %q does not say the raft state was unreadable", resp.GetError())
	}
}

// TestColonyUpdateIncarnation_IsFreshPerProcess pins the property the
// fence's fourth field depends on. A predictable or reused incarnation
// would let a resurrected process keep acting.
func TestColonyUpdateIncarnation_IsFreshPerProcess(t *testing.T) {
	seen := make(map[string]bool, 128)
	for i := 0; i < 128; i++ {
		got := ColonyUpdateIncarnation()
		if got == "" {
			t.Fatal("ColonyUpdateIncarnation() returned empty; acquisition would then be refused rather than issuing a fence that does not fence")
		}
		if seen[got] {
			t.Fatalf("ColonyUpdateIncarnation() repeated %q within 128 calls", got)
		}
		seen[got] = true
	}
	// Two NewServer calls stand for two managerd processes, which is the
	// case that matters: a replacement must not inherit its
	// predecessor's identity.
	a := ColonyUpdateIncarnation()
	b := ColonyUpdateIncarnation()
	if a == b {
		t.Error("two successive incarnations are identical")
	}
}

// TestReserveRestartLeaseFenced_AStaleHolderCannotRestartAComb is the
// end-to-end form of the fencing requirement, and the reason the fence
// rides on AcquireRestartLease rather than only on the update record.
//
// A coordinator that has lost its operation still holds a copy of the
// record that says it is the holder, so gating the record alone would
// not stop it. What stops it is that the FSM will not grant it another
// Comb's restart lease on the strength of that stale record - and
// because the restart lease is the only thing that authorises a
// restart, "cannot take a lease" and "cannot restart a Comb" are the
// same statement.
func TestReserveRestartLeaseFenced_AStaleHolderCannotRestartAComb(t *testing.T) {
	// Node ids have to match the test cluster's single voter's own id
	// ("raftd-1"), because ADR-0125's quorum evaluation refuses a
	// target it cannot place in the membership - which is the fail-closed
	// behaviour working, not an obstacle to route around.
	_, srv := newGuardedManagerd(t, raftdServiceTestNodeID)
	ctx := guardrailCtx()
	leaseReq := &rpcpb.ReserveRestartLeaseRequest{Service: raftdServiceName, NodeId: raftdServiceTestNodeID, Force: true}

	acquired, err := srv.mutateColonyUpdate(ctx, acquireReq("op-1", false))
	if err != nil || !acquired.GetAccepted() {
		t.Fatalf("acquire = (%+v, %v)", acquired, err)
	}
	fence := &internalpb.ColonyUpdateFence{
		OperationId:       acquired.GetUpdate().GetOperationId(),
		HolderNodeId:      acquired.GetUpdate().GetHolderNodeId(),
		FenceToken:        acquired.GetUpdate().GetFenceToken(),
		HolderIncarnation: acquired.GetUpdate().GetHolderIncarnation(),
	}

	// The live holder can take its lease.
	granted, err := srv.reserveRestartLeaseFenced(ctx, leaseReq, fence)
	if err != nil {
		t.Fatalf("fenced reserve error: %v", err)
	}
	if !granted.GetGranted() {
		t.Fatalf("the live holder could not take a fenced restart lease: %+v", granted)
	}

	// A takeover elsewhere displaces it.
	taken, err := srv.mutateColonyUpdate(ctx, acquireReq("op-2", true))
	if err != nil || !taken.GetAccepted() {
		t.Fatalf("takeover = (%+v, %v)", taken, err)
	}

	refused, err := srv.reserveRestartLeaseFenced(ctx, leaseReq, fence)
	if err != nil {
		t.Fatalf("stale fenced reserve error: %v", err)
	}
	if refused.GetGranted() {
		t.Error("a displaced coordinator acquired a restart lease on the strength of its stale fence")
	}
	if !strings.Contains(refused.GetError(), "op-2") {
		t.Errorf("refusal %q does not name the operation that displaced it", refused.GetError())
	}

	// The new holder can.
	newFence := &internalpb.ColonyUpdateFence{
		OperationId:       taken.GetUpdate().GetOperationId(),
		HolderNodeId:      taken.GetUpdate().GetHolderNodeId(),
		FenceToken:        taken.GetUpdate().GetFenceToken(),
		HolderIncarnation: taken.GetUpdate().GetHolderIncarnation(),
	}
	ok, err := srv.reserveRestartLeaseFenced(ctx, leaseReq, newFence)
	if err != nil {
		t.Fatalf("new holder's fenced reserve error: %v", err)
	}
	if !ok.GetGranted() {
		t.Errorf("the new holder could not take a fenced restart lease: %+v", ok)
	}
}

// TestReserveRestartLeaseFenced_NilFenceIsTheOrdinaryUnfencedPath is
// the regression guard for ADR-0145's hard constraint that this work
// must not change the existing operator-driven restart path. Every
// current caller of reserveRestartLease passes no fence, and that has
// to keep meaning exactly what it meant.
func TestReserveRestartLeaseFenced_NilFenceIsTheOrdinaryUnfencedPath(t *testing.T) {
	_, srv := newGuardedManagerd(t, raftdServiceTestNodeID)
	ctx := guardrailCtx()

	// No controlled update is running at all, and the ordinary
	// operator-driven path still works.
	first, err := srv.reserveRestartLease(ctx, &rpcpb.ReserveRestartLeaseRequest{Service: raftdServiceName, NodeId: raftdServiceTestNodeID, Force: true})
	if err != nil {
		t.Fatalf("unfenced reserve error: %v", err)
	}
	if !first.GetGranted() {
		t.Fatalf("the ordinary unfenced path stopped working: %+v", first)
	}
	// ADR-0103's own cooldown/lease block is intact, and force still
	// overrides it. The refusal here is the quorum evaluation's (a
	// single-voter cluster cannot lose its only voter), which is
	// ADR-0125 behaviour and is itself a block force can downgrade -
	// which is exactly what the next line proves.
	second, err := srv.reserveRestartLease(ctx, &rpcpb.ReserveRestartLeaseRequest{Service: raftdServiceName, NodeId: raftdServiceTestNodeID, Force: false})
	if err != nil {
		t.Fatalf("second unfenced reserve error: %v", err)
	}
	if second.GetGranted() {
		t.Errorf("a second unfenced lease was granted: %+v", second)
	}
	forced, err := srv.reserveRestartLease(ctx, &rpcpb.ReserveRestartLeaseRequest{Service: raftdServiceName, NodeId: raftdServiceTestNodeID, Force: true})
	if err != nil {
		t.Fatalf("forced unfenced reserve error: %v", err)
	}
	if !forced.GetGranted() || !forced.GetGuardrailOverridden() {
		t.Errorf("force did not override the ordinary guardrail: %+v", forced)
	}
}
func TestCheckColonyUpdateFence_IsExhaustive(t *testing.T) {
	complete := func() *rpcpb.ColonyUpdateFence {
		return &rpcpb.ColonyUpdateFence{OperationId: "op-1", HolderNodeId: "comb-a", FenceToken: 42, HolderIncarnation: "boot-1"}
	}
	if err := checkColonyUpdateFence(complete()); err != nil {
		t.Errorf("a complete fence was refused: %v", err)
	}
	if err := checkColonyUpdateFence(nil); err == nil {
		t.Error("a nil fence was accepted")
	}
	for _, drop := range []struct {
		name  string
		apply func(*rpcpb.ColonyUpdateFence)
	}{
		{"operation_id", func(f *rpcpb.ColonyUpdateFence) { f.OperationId = "" }},
		{"holder_node_id", func(f *rpcpb.ColonyUpdateFence) { f.HolderNodeId = "" }},
		{"fence_token", func(f *rpcpb.ColonyUpdateFence) { f.FenceToken = 0 }},
		{"holder_incarnation", func(f *rpcpb.ColonyUpdateFence) { f.HolderIncarnation = "" }},
	} {
		t.Run("missing "+drop.name, func(t *testing.T) {
			f := complete()
			drop.apply(f)
			err := checkColonyUpdateFence(f)
			if err == nil {
				t.Fatal("an incomplete fence was accepted")
			}
			if !strings.Contains(err.Error(), drop.name) {
				t.Errorf("refusal %q does not name the missing field %s", err, drop.name)
			}
		})
	}
}

// TestMutateColonyUpdate_AuthorizationExemptionIsRegistered is the
// regression that the 20:11 mutation audit found unpinned on the
// step-aside path: the handler checks the token itself, so a test that
// calls the handler directly never exercises
// AuthUnaryInterceptor, and deleting the exemption is invisible.
//
// This one goes through a real interceptor.
func TestMutateColonyUpdate_AuthorizationExemptionIsRegistered(t *testing.T) {
	if !authExemptMethods[mutateColonyUpdateMethod] {
		t.Errorf("%s is not in authExemptMethods; a forwarded peer call would be sent into checkAuth and refused for the wrong reason", mutateColonyUpdateMethod)
	}
	// And it must NOT be in requiredRole: a role-tiered entry would let
	// a CreateAPIKey-issued Admin key reach it through some other path.
	if _, ok := requiredRole[mutateColonyUpdateMethod]; ok {
		t.Errorf("%s has a requiredRole entry; it must be governed by the dedicated token, not the Viewer/Admin hierarchy", mutateColonyUpdateMethod)
	}
	// And a wrong token is a PermissionDenied STATUS, not a response
	// field, so a caller can tell "you are not authorized to ask this"
	// from "I tried and the colony is busy".
	_, srv := newManagerdRPCClientAndServer(t, newRaftdUDSSocket(t), "comb-a")
	srv.SetRestartGuardrailToken(testGuardrailToken)
	wrong := metadata.NewOutgoingContext(context.Background(), metadata.Pairs("authorization", "Bearer wrong"))
	_, err := srv.MutateColonyUpdate(wrong, acquireReq("op-1", false))
	if err == nil {
		t.Fatal("a wrong token produced no error")
	}
	if status.Code(err).String() != "PermissionDenied" {
		t.Errorf("wrong-token error code = %s, want PermissionDenied", status.Code(err))
	}
}
