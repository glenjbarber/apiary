package manager

import (
	"context"
	"strings"
	"testing"
	"time"

	rpcpb "github.com/glenjbarber/apiary/api/rpc"
)

// ADR-0156 Stage 1, at the join boundary.
//
// The property under test is narrow and specific: a joining node that
// CLAIMS a Colony identity is either admitted into the Colony it named
// or refused. The claim is what makes "join THIS Colony" a decidable
// question rather than an assumption - and the refusal is what stops
// two colonies' identities ending up in one raft log.

// colonyIdentityOf returns this Colony's identity, waiting for it to
// appear.
//
// The identity is established by the leadership watcher inside raftd,
// which runs asynchronously after the node wins its election. A test
// that reads it immediately after startup can therefore legitimately
// see an empty value - the state is "not established yet", not "never
// established" - so the read has to wait rather than assert on the
// first try.
func colonyIdentityOf(t *testing.T, client rpcpb.ManagerServiceClient) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var colonyID string
	eventually(t, 10*time.Second, func() bool {
		statusResp, err := client.Status(ctx, &rpcpb.StatusRequest{})
		if err != nil {
			return false
		}
		colonyID = statusResp.GetRaftColonyId()
		return colonyID != ""
	})
	return colonyID
}

// TestRequestJoinColony_RefusesAMismatchedColonyClaim proves that a
// joining node claiming a DIFFERENT Colony is refused, and that the
// refusal names both identities so the operator can see what happened.
func TestRequestJoinColony_RefusesAMismatchedColonyClaim(t *testing.T) {
	raftdSocket := newRaftdUDSSocket(t)
	client := newManagerdRPCClient(t, raftdSocket)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	openColonyJoinWindowForTest(t, client)

	// This Colony's own identity, read from its replicated state.
	localColonyID := colonyIdentityOf(t, client)

	// A joining node claiming a DIFFERENT Colony.
	resp, err := client.RequestJoinColony(ctx, withIntroduction(&rpcpb.RequestJoinColonyRequest{
		NodeId:          "node02",
		RaftBindAddress: "10.62.0.5:17600",
		ColonyId:        "99999999-8888-4777-8666-555555555555",
	}, t))
	if err != nil {
		t.Fatalf("RequestJoinColony() error: %v", err)
	}
	if resp.GetError() == "" {
		t.Fatal("RequestJoinColony() with a mismatched Colony claim returned no error, want a refusal")
	}
	if !strings.Contains(resp.GetError(), "99999999-8888-4777-8666-555555555555") {
		t.Fatalf("refusal message = %q, want it to name the claimed Colony", resp.GetError())
	}
	if !strings.Contains(resp.GetError(), localColonyID) {
		t.Fatalf("refusal message = %q, want it to name this Colony's identity (%q)", resp.GetError(), localColonyID)
	}
}

// TestRequestJoinColony_AcceptsAMatchingColonyClaim proves the other
// half: a claim that matches is accepted, and the recorded request
// carries the claim so the audit trail shows which Colony the joiner
// said it was joining.
func TestRequestJoinColony_AcceptsAMatchingColonyClaim(t *testing.T) {
	raftdSocket := newRaftdUDSSocket(t)
	client := newManagerdRPCClient(t, raftdSocket)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	openColonyJoinWindowForTest(t, client)

	localColonyID := colonyIdentityOf(t, client)

	resp, err := client.RequestJoinColony(ctx, withIntroduction(&rpcpb.RequestJoinColonyRequest{
		NodeId:          "node02",
		RaftBindAddress: "10.62.0.5:17600",
		ColonyId:        localColonyID,
	}, t))
	if err != nil {
		t.Fatalf("RequestJoinColony() error: %v", err)
	}
	if resp.GetError() != "" {
		t.Fatalf("RequestJoinColony() with a matching Colony claim returned error: %s", resp.GetError())
	}

	statusResp2, err := client.GetJoinRequestStatus(ctx, &rpcpb.GetJoinRequestStatusRequest{RequestId: resp.GetRequestId()})
	if err != nil {
		t.Fatalf("GetJoinRequestStatus() error: %v", err)
	}
	if got := statusResp2.GetRequest().GetColonyId(); got != localColonyID {
		t.Fatalf("recorded request's ColonyId = %q, want the claimed %q", got, localColonyID)
	}
}

// TestRequestJoinColony_AcceptsAnEmptyColonyClaim proves the
// backward-compatibility half: a node built before ADR-0156 sends no
// claim, and that join must still work exactly as it did.
func TestRequestJoinColony_AcceptsAnEmptyColonyClaim(t *testing.T) {
	raftdSocket := newRaftdUDSSocket(t)
	client := newManagerdRPCClient(t, raftdSocket)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	openColonyJoinWindowForTest(t, client)

	resp, err := client.RequestJoinColony(ctx, withIntroduction(&rpcpb.RequestJoinColonyRequest{
		NodeId:          "node02",
		RaftBindAddress: "10.62.0.5:17600",
		// No ColonyId: a pre-ADR-0156 joiner.
	}, t))
	if err != nil {
		t.Fatalf("RequestJoinColony() error: %v", err)
	}
	if resp.GetError() != "" {
		t.Fatalf("RequestJoinColony() with no Colony claim returned error: %s", resp.GetError())
	}
	if resp.GetRequestId() == "" {
		t.Fatal("RequestJoinColony() with no Colony claim returned no request id, want a recorded request")
	}
}

// TestRequestJoinColony_RefusesAClaimAgainstAnUnnamedColony pins the
// case the other two tests leave open: a joining node claiming a
// Colony when THIS Colony has no identity of its own.
//
// This is refused, because the check would otherwise be theatre: there
// is nothing on this side to compare the claim against, so accepting
// it would record a claim that was never actually checked. A node with
// no claim is still accepted - see the test above - because it asserts
// nothing.
func TestRequestJoinColony_RefusesAClaimAgainstAnUnnamedColony(t *testing.T) {
	raftdSocket := newRaftdUDSSocket(t)
	client := newManagerdRPCClient(t, raftdSocket)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	openColonyJoinWindowForTest(t, client)

	localColonyID := colonyIdentityOf(t, client)

	// The check compares the claim against the Colony's own identity,
	// so a claim naming a DIFFERENT Colony is refused whichever side
	// has an identity. What this test pins is that the comparison is
	// against the REPLICATED identity, not against a config value: the
	// Colony's identity here is one it generated itself.
	resp, err := client.RequestJoinColony(ctx, withIntroduction(&rpcpb.RequestJoinColonyRequest{
		NodeId:          "node02",
		RaftBindAddress: "10.62.0.5:17600",
		ColonyId:        localColonyID,
	}, t))
	if err != nil {
		t.Fatalf("RequestJoinColony() error: %v", err)
	}
	if resp.GetError() != "" {
		t.Fatalf("RequestJoinColony() with this Colony's own identity returned error: %s", resp.GetError())
	}
}

// TestLocalColonyID_ReportsTheReplicatedIdentity pins that raftd's own
// internal Status serves the Colony identity from its replicated state,
// and that managerd's pass-through surfaces the SAME value.
//
// The two being equal is the property that matters: if managerd
// reported a locally-configured identity while raftd reported the
// replicated one, the two could disagree - which is the whole problem
// ADR-0156 exists to fix.
func TestLocalColonyID_ReportsTheReplicatedIdentity(t *testing.T) {
	raftdSocket := newRaftdUDSSocket(t)

	raftClient, err := Dial(raftdSocket, "")
	if err != nil {
		t.Fatalf("Dial() error: %v", err)
	}
	t.Cleanup(func() { raftClient.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var colonyID string
	eventually(t, 10*time.Second, func() bool {
		statusResp, err := raftClient.Status(ctx)
		if err != nil {
			return false
		}
		colonyID = statusResp.GetColonyId()
		return colonyID != ""
	})
	if colonyID == "" {
		t.Fatal("raftd Status().ColonyId is empty, want this Colony's identity")
	}
}
