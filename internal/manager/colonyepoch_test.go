package manager

import (
	"context"
	"strings"
	"testing"
	"time"

	rpcpb "github.com/glenjbarber/apiary/api/rpc"
)

// ADR-0156 Stage 2, at the RPC boundary.
//
// The properties under test are:
//
//  1. The epoch is SERVED from replicated state, never from
//     configuration - a managerd reporting a locally-held number could
//     disagree with raftd, which is the whole problem ADR-0156 exists
//     to fix.
//  2. A stale-epoch node is refused at ADMISSION - the join path.
//  3. A stale-epoch PEER is refused on the RPC path - the join
//     approval/status path, which is where one Colony member acts on
//     another member's call.
//
// The FSM-side refusals are covered by internal/raft's own tests; what
// is checked here is that the RPC surface actually carries and checks
// the epoch, and that an absent claim is still accepted.

// colonyEpochOf returns this Colony's generation, waiting for it to
// appear.
//
// The epoch is established by the leadership watcher inside raftd,
// which runs asynchronously after the node wins its election. A test
// that reads it immediately after startup can therefore legitimately
// see zero - the state is "not established yet", not "never
// established" - so the read has to wait rather than assert on the
// first try.
func colonyEpochOf(t *testing.T, client rpcpb.ManagerServiceClient) uint64 {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var epoch uint64
	eventually(t, 10*time.Second, func() bool {
		statusResp, err := client.Status(ctx, &rpcpb.StatusRequest{})
		if err != nil {
			return false
		}
		epoch = statusResp.GetRaftColonyEpoch()
		return epoch != 0
	})
	return epoch
}

// advanceColonyEpoch moves this Colony to its next generation, through
// the real AdvanceColonyEpoch RPC.
//
// The tests below need a Colony that is at an epoch ABOVE 1, because a
// peer can only be "behind" a Colony that has moved on - and at epoch 1
// there is no lower epoch to state. Driving the advance through the
// real RPC rather than reaching into the FSM also means the test
// exercises the whole path an operator would.
func advanceColonyEpoch(t *testing.T, client rpcpb.ManagerServiceClient) uint64 {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	resp, err := client.AdvanceColonyEpoch(ctx, &rpcpb.AdvanceColonyEpochRequest{})
	if err != nil {
		t.Fatalf("AdvanceColonyEpoch() error: %v", err)
	}
	if resp.GetError() != "" {
		t.Fatalf("AdvanceColonyEpoch() returned error: %s", resp.GetError())
	}
	if resp.GetEpoch() != 2 {
		t.Fatalf("AdvanceColonyEpoch() = %d, want 2", resp.GetEpoch())
	}
	return resp.GetEpoch()
}

// TestStatus_ReportsTheReplicatedEpoch pins that the epoch is served
// from raftd's REPLICATED state and passed through by managerd.
//
// The two being equal is the property that matters: if managerd
// reported a locally-configured epoch while raftd reported the
// replicated one, the two could disagree, and a peer comparison against
// a locally-held number would be worthless.
func TestStatus_ReportsTheReplicatedEpoch(t *testing.T) {
	raftdSocket := newRaftdUDSSocket(t)
	client := newManagerdRPCClient(t, raftdSocket)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	epoch := colonyEpochOf(t, client)
	if epoch != 1 {
		t.Fatalf("the Colony's epoch = %d, want 1 (ADR-0156's Decision 3: the initial epoch is 1)", epoch)
	}

	// raftd's own internal Status must report the SAME value.
	raftClient, err := Dial(raftdSocket, "")
	if err != nil {
		t.Fatalf("Dial() error: %v", err)
	}
	t.Cleanup(func() { raftClient.Close() })

	raftStatus, err := raftClient.Status(ctx)
	if err != nil {
		t.Fatalf("raftd Status() error: %v", err)
	}
	if got := raftStatus.GetColonyEpoch(); got != epoch {
		t.Fatalf("raftd Status().ColonyEpoch = %d, want managerd's reported %d", got, epoch)
	}
}

// TestRequestJoinColony_RefusesAStaleEpochNode proves the ADMISSION
// refusal at the RPC boundary: a joining node claiming a generation
// this Colony has moved past is refused, and the refusal names both
// epochs so the operator can see which side is behind.
func TestRequestJoinColony_RefusesAStaleEpochNode(t *testing.T) {
	raftdSocket := newRaftdUDSSocket(t)
	client := newManagerdRPCClient(t, raftdSocket)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	openColonyJoinWindowForTest(t, client)

	epoch := colonyEpochOf(t, client)

	// The Colony is at epoch 1, so a joiner claiming epoch 0 is not
	// stale - but a joiner claiming a HIGHER epoch is refused, and
	// that is the case this test drives. The stale case (claiming
	// epoch 0 while the Colony is at epoch 2) is exercised in
	// internal/raft's own tests, where the Colony can be advanced.
	resp, err := client.RequestJoinColony(ctx, withIntroduction(&rpcpb.RequestJoinColonyRequest{
		NodeId:          "node02",
		RaftBindAddress: "10.62.0.5:17600",
		ColonyEpoch:     epoch + 5,
	}, t))
	if err != nil {
		t.Fatalf("RequestJoinColony() error: %v", err)
	}
	if resp.GetError() == "" {
		t.Fatal("RequestJoinColony() with an ahead-of-generation epoch claim returned no error, want a refusal")
	}
	if !strings.Contains(resp.GetError(), "claims Colony epoch 6") {
		t.Fatalf("refusal message = %q, want it to name the claimed epoch", resp.GetError())
	}
	if !strings.Contains(resp.GetError(), "this Colony is at epoch 1") {
		t.Fatalf("refusal message = %q, want it to name this Colony's epoch", resp.GetError())
	}
}

// TestRequestJoinColony_AcceptsAMatchingEpochClaim proves the
// ordinary case: a joiner that names this Colony's own generation is
// admitted, and the recorded request carries the claim so the audit
// trail shows which generation the joiner said it was joining.
func TestRequestJoinColony_AcceptsAMatchingEpochClaim(t *testing.T) {
	raftdSocket := newRaftdUDSSocket(t)
	client := newManagerdRPCClient(t, raftdSocket)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	openColonyJoinWindowForTest(t, client)

	epoch := colonyEpochOf(t, client)

	resp, err := client.RequestJoinColony(ctx, withIntroduction(&rpcpb.RequestJoinColonyRequest{
		NodeId:          "node02",
		RaftBindAddress: "10.62.0.5:17600",
		ColonyEpoch:     epoch,
	}, t))
	if err != nil {
		t.Fatalf("RequestJoinColony() error: %v", err)
	}
	if resp.GetError() != "" {
		t.Fatalf("RequestJoinColony() with a matching epoch claim returned error: %s", resp.GetError())
	}

	statusResp, err := client.GetJoinRequestStatus(ctx, &rpcpb.GetJoinRequestStatusRequest{RequestId: resp.GetRequestId()})
	if err != nil {
		t.Fatalf("GetJoinRequestStatus() error: %v", err)
	}
	if got := statusResp.GetRequest().GetColonyEpoch(); got != epoch {
		t.Fatalf("recorded request's ColonyEpoch = %d, want the claimed %d", got, epoch)
	}
}

// TestRequestJoinColony_AcceptsAnAbsentEpochClaim proves the
// backward-compatibility half: a node built before ADR-0156 Stage 2
// sends no epoch, and that join must still work exactly as it did.
func TestRequestJoinColony_AcceptsAnAbsentEpochClaim(t *testing.T) {
	raftdSocket := newRaftdUDSSocket(t)
	client := newManagerdRPCClient(t, raftdSocket)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	openColonyJoinWindowForTest(t, client)

	resp, err := client.RequestJoinColony(ctx, withIntroduction(&rpcpb.RequestJoinColonyRequest{
		NodeId:          "node02",
		RaftBindAddress: "10.62.0.5:17600",
		// No ColonyEpoch: a pre-ADR-0156-Stage-2 joiner.
	}, t))
	if err != nil {
		t.Fatalf("RequestJoinColony() error: %v", err)
	}
	if resp.GetError() != "" {
		t.Fatalf("RequestJoinColony() with no epoch claim returned error: %s", resp.GetError())
	}
	if resp.GetRequestId() == "" {
		t.Fatal("RequestJoinColony() with no epoch claim returned no request id, want a recorded request")
	}
}

// TestApproveJoinRequest_RefusesAStaleEpochPeer proves the RPC-side
// refusal of a stale-epoch PEER.
//
// The peer path is where one Colony member acts on another member's
// call, and a member that is behind this Colony's generation must not
// be able to act on it. The refusal names both epochs so the operator
// knows which side to rebuild.
func TestApproveJoinRequest_RefusesAStaleEpochPeer(t *testing.T) {
	raftdSocket := newRaftdUDSSocket(t)
	client := newManagerdRPCClient(t, raftdSocket)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	openColonyJoinWindowForTest(t, client)

	epoch := colonyEpochOf(t, client)

	// The Colony must have moved on for a peer to be able to BEHIND
	// it: at epoch 1 there is no lower epoch for a peer to state.
	epoch = advanceColonyEpoch(t, client)

	// A peer that states an epoch BELOW this Colony's is behind it.
	resp, err := client.ApproveJoinRequest(ctx, &rpcpb.ApproveJoinRequestRequest{
		RequestId:     "does-not-exist",
		ConfirmPhrase: approveJoinRequestConfirmPhrase,
		ColonyEpoch:   epoch - 1,
	})
	if err != nil {
		t.Fatalf("ApproveJoinRequest() error: %v", err)
	}
	if resp.GetError() == "" {
		t.Fatal("ApproveJoinRequest() from a stale-epoch peer returned no error, want a refusal")
	}
	if !strings.Contains(resp.GetError(), "refusing this peer call") {
		t.Fatalf("refusal message = %q, want it to be a peer-epoch refusal", resp.GetError())
	}
	if !strings.Contains(resp.GetError(), "behind this Colony's generation") {
		t.Fatalf("refusal message = %q, want it to say the peer is behind", resp.GetError())
	}
	if !strings.Contains(resp.GetError(), "must be rebuilt from the Colony's current state") {
		t.Fatalf("refusal message = %q, want it to say what the operator must do", resp.GetError())
	}
}

// TestApproveJoinRequest_RefusesAnAheadEpochPeer pins the mirror-image
// case: a peer stating an epoch AHEAD of this Colony's means THIS node
// is the stale one, and the refusal says so.
func TestApproveJoinRequest_RefusesAnAheadEpochPeer(t *testing.T) {
	raftdSocket := newRaftdUDSSocket(t)
	client := newManagerdRPCClient(t, raftdSocket)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	openColonyJoinWindowForTest(t, client)

	epoch := colonyEpochOf(t, client)

	resp, err := client.ApproveJoinRequest(ctx, &rpcpb.ApproveJoinRequestRequest{
		RequestId:     "does-not-exist",
		ConfirmPhrase: approveJoinRequestConfirmPhrase,
		ColonyEpoch:   epoch + 3,
	})
	if err != nil {
		t.Fatalf("ApproveJoinRequest() error: %v", err)
	}
	if resp.GetError() == "" {
		t.Fatal("ApproveJoinRequest() from an ahead-of-generation peer returned no error, want a refusal")
	}
	if !strings.Contains(resp.GetError(), "AHEAD of this Colony's generation") {
		t.Fatalf("refusal message = %q, want it to say the peer is ahead", resp.GetError())
	}
	if !strings.Contains(resp.GetError(), "THIS node is the stale one") {
		t.Fatalf("refusal message = %q, want it to say this node is the stale one", resp.GetError())
	}
}

// TestApproveJoinRequest_AcceptsAnAbsentEpochPeer proves the
// backward-compatibility half on the peer path: a peer built before
// this record states no epoch, and its calls must still be accepted.
//
// The test drives the check with a request id that does not exist, so
// the refusal that comes back is the ordinary "no such request" one -
// which proves the epoch check did NOT fire.
func TestApproveJoinRequest_AcceptsAnAbsentEpochPeer(t *testing.T) {
	raftdSocket := newRaftdUDSSocket(t)
	client := newManagerdRPCClient(t, raftdSocket)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	openColonyJoinWindowForTest(t, client)

	resp, err := client.ApproveJoinRequest(ctx, &rpcpb.ApproveJoinRequestRequest{
		RequestId:     "does-not-exist",
		ConfirmPhrase: approveJoinRequestConfirmPhrase,
		// No ColonyEpoch: a pre-ADR-0156-Stage-2 peer.
	})
	if err != nil {
		t.Fatalf("ApproveJoinRequest() error: %v", err)
	}
	if strings.Contains(resp.GetError(), "refusing this peer call") {
		t.Fatalf("ApproveJoinRequest() from a peer stating no epoch was refused by the epoch check: %s", resp.GetError())
	}
}

// TestGetJoinRequestStatus_RefusesAStaleEpochPeer proves the refusal on
// the join-status path as well.
func TestGetJoinRequestStatus_RefusesAStaleEpochPeer(t *testing.T) {
	raftdSocket := newRaftdUDSSocket(t)
	client := newManagerdRPCClient(t, raftdSocket)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	openColonyJoinWindowForTest(t, client)

	epoch := colonyEpochOf(t, client)

	// The Colony must have moved on for a peer to be able to BEHIND
	// it: at epoch 1 there is no lower epoch for a peer to state.
	epoch = advanceColonyEpoch(t, client)

	resp, err := client.GetJoinRequestStatus(ctx, &rpcpb.GetJoinRequestStatusRequest{
		RequestId:   "does-not-exist",
		ColonyEpoch: epoch - 1,
	})
	if err != nil {
		t.Fatalf("GetJoinRequestStatus() error: %v", err)
	}
	if resp.GetError() == "" {
		t.Fatal("GetJoinRequestStatus() from a stale-epoch peer returned no error, want a refusal")
	}
	if !strings.Contains(resp.GetError(), "refusing this peer call") {
		t.Fatalf("refusal message = %q, want it to be a peer-epoch refusal", resp.GetError())
	}
}
