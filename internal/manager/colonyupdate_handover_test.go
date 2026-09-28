package manager

// Tests for the handover mapping on ManagerService (ADR-0146 rule 2).
//
// The FSM tests in internal/raft pin the decision. These pin the two
// things only this layer can get wrong: that the OUTGOING fence survives
// the trip from the wire to the command, and that the record's handovers
// survive the trip back. A handover that reached the FSM with an empty
// from_fence would be refused there - correctly, but with a refusal that
// describes the wrong problem, and one that costs a leader round trip to
// learn.

import (
	"strings"
	"testing"

	rpcpb "github.com/glenjbarber/apiary/api/rpc"
)

func handoverReq(from *rpcpb.ColonyUpdateFence, toNode, toInc, reason string) *rpcpb.MutateColonyUpdateRequest {
	return &rpcpb.MutateColonyUpdateRequest{
		Op: &rpcpb.MutateColonyUpdateRequest_Handover{
			Handover: &rpcpb.HandoverColonyUpdate{
				FromFence:           from,
				ToNodeId:            toNode,
				ToHolderIncarnation: toInc,
				Reason:              reason,
			},
		},
	}
}

func TestManagerdColonyUpdateCommand_HandoverCarriesTheOutgoingFence(t *testing.T) {
	s := &Server{nodeID: "comb-a", colonyUpdateIncarnation: "boot-1"}

	cases := []struct {
		name string
		from *rpcpb.ColonyUpdateFence
		// wantSubstr is only consulted once the case is known to have
		// been refused. The refusal itself is unconditional, and that is
		// the part that matters: an earlier version of this table only
		// CHECKED the message when an error happened to come back, so
		// deleting the completeness check made every case pass by
		// quietly taking the success path. Asserting that the refusal
		// happens is what pins it.
		wantSubstr string
	}{
		{
			name:       "no fence at all",
			from:       nil,
			wantSubstr: "no fence was presented",
		},
		{
			name:       "a fence with no token and no incarnation",
			from:       &rpcpb.ColonyUpdateFence{OperationId: "op-1", HolderNodeId: "comb-a"},
			wantSubstr: "fence_token",
		},
		{
			name:       "a fence with no incarnation",
			from:       &rpcpb.ColonyUpdateFence{OperationId: "op-1", HolderNodeId: "comb-a", FenceToken: 7},
			wantSubstr: "holder_incarnation",
		},
		{
			name:       "a fence with no token",
			from:       &rpcpb.ColonyUpdateFence{OperationId: "op-1", HolderNodeId: "comb-a", HolderIncarnation: "boot-1"},
			wantSubstr: "fence_token",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := s.colonyUpdateCommand(handoverReq(tc.from, "comb-b", "boot-9", "planned move before the last Comb"))
			if err == nil {
				t.Fatalf("colonyUpdateCommand accepted a handover presenting %v. An incomplete fence can never match exactly, so the FSM will refuse it - but only after a leader round trip, and with a refusal that describes the wrong problem.",
					tc.from)
			}
			if !strings.Contains(err.Error(), tc.wantSubstr) {
				t.Errorf("refusal = %q, want it to say %q", err.Error(), tc.wantSubstr)
			}
		})
	}
}

// TestManagerdColonyUpdateCommand_ACompleteHandoverReachesTheFSMIntact is
// the positive half, kept apart from the refusal table so that neither can
// be satisfied by the other's behaviour.
func TestManagerdColonyUpdateCommand_ACompleteHandoverReachesTheFSMIntact(t *testing.T) {
	s := &Server{nodeID: "comb-a", colonyUpdateIncarnation: "boot-1"}
	from := &rpcpb.ColonyUpdateFence{
		OperationId: "op-1", HolderNodeId: "comb-a",
		FenceToken: 7, HolderIncarnation: "boot-1",
	}

	cmd, err := s.colonyUpdateCommand(handoverReq(from, "comb-b", "boot-9", "planned move before the last Comb"))
	if err != nil {
		t.Fatalf("a complete handover was refused: %v", err)
	}
	h := cmd.GetHandoverColonyUpdate()
	if h == nil {
		t.Fatalf("the request produced a command with no handover op: %v", cmd.GetOp())
	}
	// The outgoing fence must arrive whole: operation, holder, token and
	// incarnation are all of it, and dropping any one turns a valid
	// handover into a refusal at the leader.
	if f := h.GetFromFence(); f.GetOperationId() != from.GetOperationId() ||
		f.GetHolderNodeId() != from.GetHolderNodeId() ||
		f.GetFenceToken() != from.GetFenceToken() ||
		f.GetHolderIncarnation() != from.GetHolderIncarnation() {
		t.Errorf("the outgoing fence reached the command as %v, want %v", f, from)
	}
	if h.GetToNodeId() != "comb-b" || h.GetToHolderIncarnation() != "boot-9" {
		t.Errorf("the incoming coordinator reached the command as %s/%s, want comb-b/boot-9",
			h.GetToNodeId(), h.GetToHolderIncarnation())
	}
	if h.GetReason() != "planned move before the last Comb" {
		t.Errorf("reason reached the command as %q", h.GetReason())
	}
	if h.GetRequestedAtUnix() == 0 {
		t.Error("the handover carries no requested time")
	}
}

func TestManagerdColonyUpdate_HandoverReachesTheFSMAndComesBackReadable(t *testing.T) {
	client, _ := newGuardedManagerd(t, "comb-a")

	acquired, err := client.MutateColonyUpdate(guardrailCtx(), acquireReq("op-1", false))
	if err != nil {
		t.Fatalf("MutateColonyUpdate(acquire) error: %v", err)
	}
	if !acquired.GetAccepted() {
		t.Fatalf("acquire was refused: %s", acquired.GetError())
	}
	from := fenceFrom(acquired.GetUpdate())

	resp, err := client.MutateColonyUpdate(guardrailCtx(), handoverReq(from, "comb-b", "boot-9", "planned move before the last Comb"))
	if err != nil {
		t.Fatalf("MutateColonyUpdate(handover) error: %v", err)
	}
	if !resp.GetAccepted() {
		t.Fatalf("handover was refused: %s", resp.GetError())
	}
	if resp.GetTakeover() {
		t.Error("the response says takeover = true for a planned handover; the flag that means \"seized\" must not fire on the normal path")
	}

	got := resp.GetUpdate()
	if got.GetHolderNodeId() != "comb-b" || got.GetHolderIncarnation() != "boot-9" {
		t.Errorf("holder = %s/%s, want comb-b/boot-9", got.GetHolderNodeId(), got.GetHolderIncarnation())
	}
	if !got.GetActive() {
		t.Error("the operation came back settled after a handover")
	}
	// The response has to carry the handover itself, or a reader of this
	// RPC cannot answer "who gave this away" without a second call.
	if len(got.GetHandovers()) != 1 {
		t.Fatalf("the response carries %d handovers, want 1", len(got.GetHandovers()))
	}
	h := got.GetHandovers()[0]
	// The outgoing coordinator is whatever incarnation this managerd
	// minted, which is random per process - so it is read back from the
	// acquire rather than hard-coded. Hard-coding it is how a test ends up
	// asserting a value the code under test never chose.
	outgoing := acquired.GetUpdate().GetHolderIncarnation()
	if h.GetFromNodeId() != "comb-a" || h.GetFromIncarnation() != outgoing || h.GetToNodeId() != "comb-b" || h.GetToIncarnation() != "boot-9" {
		t.Errorf("the returned handover is %s/%s -> %s/%s, want comb-a/%s -> comb-b/boot-9",
			h.GetFromNodeId(), h.GetFromIncarnation(), h.GetToNodeId(), h.GetToIncarnation(), outgoing)
	}
	if h.GetFromFenceToken() == 0 || h.GetToFenceToken() == 0 || h.GetToFenceToken() <= h.GetFromFenceToken() {
		t.Errorf("the returned handover records fence tokens %d -> %d, want a strictly increasing pair",
			h.GetFromFenceToken(), h.GetToFenceToken())
	}
	if h.GetReason() != "planned move before the last Comb" {
		t.Errorf("the returned handover's reason is %q", h.GetReason())
	}
}

// TestManagerdColonyUpdate_HandoverDoesNotSettleTheOperationOrGrantTwice
// closes the loop: after a handover the single-flight is still exactly
// one operation, and the colony still refuses a second coordinator.
func TestManagerdColonyUpdate_HandoverDoesNotSettleTheOperationOrGrantTwice(t *testing.T) {
	client, _ := newGuardedManagerd(t, "comb-a")

	acquired, err := client.MutateColonyUpdate(guardrailCtx(), acquireReq("op-1", false))
	if err != nil {
		t.Fatalf("MutateColonyUpdate(acquire) error: %v", err)
	}
	resp, err := client.MutateColonyUpdate(guardrailCtx(), handoverReq(fenceFrom(acquired.GetUpdate()), "comb-b", "boot-9", "planned move"))
	if err != nil {
		t.Fatalf("MutateColonyUpdate(handover) error: %v", err)
	}
	if !resp.GetAccepted() {
		t.Fatalf("handover was refused: %s", resp.GetError())
	}

	second, err := client.MutateColonyUpdate(guardrailCtx(), acquireReq("op-2", false))
	if err != nil {
		t.Fatalf("MutateColonyUpdate(second acquire) error: %v", err)
	}
	if second.GetAccepted() {
		t.Fatal("a second coordinator was granted the colony after a handover; the single-flight did not survive it")
	}
	if !strings.Contains(second.GetError(), "comb-b") {
		t.Errorf("the refusal names %q, want it to name the new holder comb-b so an operator is told who to ask", second.GetError())
	}
}
