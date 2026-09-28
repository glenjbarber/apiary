package raft

// Wire-level tests for ADR-0145's controlled-update read RPC, against a
// real three-voter cluster rather than a bare FSM.
//
// The reason is the same as everywhere else in this file's neighbours:
// GetColonyUpdateStateLocal's whole contract is about what a caller
// learns when it asks a real raftd, including on a follower where the
// answer is honestly labelled non-authoritative. A fake would answer
// whatever the fake was told.

import (
	"context"
	"testing"
	"time"

	internalpb "github.com/glenjbarber/apiary/api/internalpb"
)

// newColonyUpdateServerCluster is threeNodeCluster plus the raftd gRPC
// Server each node exposes, so the tests below exercise
// Server.GetColonyUpdateStateLocal rather than Node.ColonyUpdateStateLocal.
func newColonyUpdateServerCluster(t *testing.T) []*Server {
	t.Helper()
	nodes := threeNodeCluster(t)
	servers := make([]*Server, len(nodes))
	for i, n := range nodes {
		servers[i] = NewServer(n)
	}
	return servers
}

func getState(t *testing.T, s *Server, operationID string) *internalpb.GetColonyUpdateStateResponse {
	t.Helper()
	resp, err := s.GetColonyUpdateStateLocal(context.Background(), &internalpb.GetColonyUpdateStateRequest{OperationId: operationID})
	if err != nil {
		t.Fatalf("GetColonyUpdateStateLocal() error: %v", err)
	}
	return resp
}

func TestServerGetColonyUpdateState_NothingRunningIsAnAnswerNotAnError(t *testing.T) {
	servers := newColonyUpdateServerCluster(t)
	leader := servers[0]
	eventually(t, 10*time.Second, func() bool { return leader.node.Status().IsLeader })

	// The most important answer this RPC gives is "nothing is in
	// progress" - it is what lets a coordinator decide to START one. If
	// it were an error, every caller would have to guess, and guessing
	// there is how two updates start.
	//
	// A full acquire/release cycle runs first purely so the leader has a
	// real applied index to report. A freshly bootstrapped cluster has
	// applied nothing, and 0 is a CORRECT applied index there - the
	// field is not assertable as non-zero without making a false claim
	// about a fresh cluster.
	rec := leader.applyColonyUpdate(t, acquireColonyUpdateCmd("op-0", "comb-a", "boot-1", false))
	if rec.Error != "" {
		t.Fatalf("acquire refused: %s", rec.Error)
	}
	leader.applyColonyUpdate(t, releaseColonyUpdateCmd(fenceOf(rec.ColonyUpdate), colonyOutcomeConfirmed, "settled"))

	resp := getState(t, leader, "")
	if resp.GetError() != "" {
		t.Errorf("an idle colony answered with an error %q; nothing running is an answer", resp.GetError())
	}
	if resp.GetActive() != nil {
		t.Errorf("an idle colony reports an active update: %+v", resp.GetActive())
	}
	if !resp.GetAuthoritative() {
		t.Error("the leader reports itself as non-authoritative")
	}
	if resp.GetAppliedIndex() == 0 {
		t.Error("applied_index = 0 after a committed apply; a reader cannot reason about freshness without it")
	}
	// And the settled operation is history, not an active lock - the
	// distinction a caller starting a new update is relying on.
	if len(resp.GetHistory()) != 1 || resp.GetHistory()[0].GetOperationId() != "op-0" {
		t.Errorf("history = %d entries, want exactly op-0", len(resp.GetHistory()))
	}
}

func TestServerGetColonyUpdateState_ReportsActiveAndHistory(t *testing.T) {
	servers := newColonyUpdateServerCluster(t)
	leader := servers[0]
	eventually(t, 10*time.Second, func() bool { return leader.node.Status().IsLeader })

	first := leader.applyColonyUpdate(t, acquireColonyUpdateCmd("op-1", "comb-a", "boot-1", false))
	if first.Error != "" {
		t.Fatalf("acquire refused: %s", first.Error)
	}
	firstFence := fenceOf(first.ColonyUpdate)
	leader.applyColonyUpdate(t, advanceColonyUpdateCmd(firstFence, "issue-restart", "buzz", "restarting buzz",
		confirmedStep(0, "issue-restart", "buzz", "buzz was a confirmed follower before its restart")))
	leader.applyColonyUpdate(t, releaseColonyUpdateCmd(firstFence, colonyOutcomeConfirmed, "buzz took the new build"))

	second := leader.applyColonyUpdate(t, acquireColonyUpdateCmd("op-2", "comb-b", "boot-9", false))
	if second.Error != "" {
		t.Fatalf("second acquire refused: %s", second.Error)
	}

	t.Run("the colony-wide answer", func(t *testing.T) {
		resp := getState(t, leader, "")
		if resp.GetActive() == nil {
			t.Fatal("no active update while op-2 is in flight")
		}
		if resp.GetActive().GetOperationId() != "op-2" {
			t.Errorf("active = %q, want op-2", resp.GetActive().GetOperationId())
		}
		if len(resp.GetHistory()) != 1 || resp.GetHistory()[0].GetOperationId() != "op-1" {
			t.Errorf("history = %d entries, want exactly op-1", len(resp.GetHistory()))
		}
	})

	t.Run("one operation by id, active", func(t *testing.T) {
		resp := getState(t, leader, "op-2")
		if resp.GetActive() == nil || resp.GetActive().GetOperationId() != "op-2" {
			t.Errorf("by-id answer for op-2 = %+v, want it as the active record", resp.GetActive())
		}
		if len(resp.GetHistory()) != 0 {
			t.Errorf("by-id answer for the active operation also returned %d history entries, want 0", len(resp.GetHistory()))
		}
	})

	t.Run("one operation by id, settled", func(t *testing.T) {
		resp := getState(t, leader, "op-1")
		if resp.GetActive() != nil {
			t.Error("a settled operation is reported as active")
		}
		if len(resp.GetHistory()) != 1 || resp.GetHistory()[0].GetOperationId() != "op-1" {
			t.Errorf("by-id answer for op-1 = %+v, want it as history", resp.GetHistory())
		}
		if got := resp.GetHistory()[0]; len(got.GetSteps()) != 1 || got.GetSteps()[0].GetEvidence() == nil {
			t.Errorf("the settled record lost its step evidence: %+v", got.GetSteps())
		}
		if got := resp.GetHistory()[0]; got.GetOutcome() != colonyOutcomeConfirmed || got.GetSettledAtUnix() == 0 {
			t.Errorf("the settled record lost its terminal outcome: %+v", got)
		}
	})

	t.Run("an operation id that was never used", func(t *testing.T) {
		resp := getState(t, leader, "op-never-existed")
		if resp.GetError() != "" {
			t.Errorf("an unknown operation id answered with an error %q; it is an answer, and a distinct one from a transport failure", resp.GetError())
		}
		if resp.GetActive() != nil || len(resp.GetHistory()) != 0 {
			t.Error("an unknown operation id returned a record")
		}
	})
}

// TestServerGetColonyUpdateState_FollowerAnswersButSaysItIsNotTheLeader
// is the honesty property, and it is the reason authoritative is a
// field at all.
//
// A follower CAN answer - the state is replicated, which is the whole
// design - but its copy can lag the leader's by a commit, and a reader
// handed a lagging copy while believing it is authoritative would read a
// stale lock as a live one. So it says so.
func TestServerGetColonyUpdateState_FollowerAnswersButSaysItIsNotTheLeader(t *testing.T) {
	servers := newColonyUpdateServerCluster(t)
	leader := servers[0]
	eventually(t, 10*time.Second, func() bool { return leader.node.Status().IsLeader })

	var follower *Server
	for _, s := range servers {
		if s != leader {
			follower = s
			break
		}
	}
	leader.applyColonyUpdate(t, acquireColonyUpdateCmd("op-1", "comb-a", "boot-1", false))

	// It answers, and it answers truthfully about its own authority.
	var resp *internalpb.GetColonyUpdateStateResponse
	eventually(t, 10*time.Second, func() bool {
		resp = getState(t, follower, "")
		return resp.GetActive() != nil
	})
	if resp.GetAuthoritative() {
		t.Error("a follower reports itself authoritative for a state read; a lagging copy would then read as a live lock")
	}
	if resp.GetActive().GetOperationId() != "op-1" {
		t.Errorf("follower's active = %q, want op-1", resp.GetActive().GetOperationId())
	}
	if resp.GetError() != "" {
		t.Errorf("a follower refused a state read: %q", resp.GetError())
	}
}

// applyColonyUpdate is the Server-side test helper: the same raft Apply
// internal/manager's handler performs, so the wire tests above are
// testing state that was genuinely committed rather than poked into an
// FSM directly.
func (s *Server) applyColonyUpdate(t *testing.T, cmd *internalpb.Command) *FSMApplyResult {
	t.Helper()
	res, err := s.node.Apply(mustMarshalCommand(t, cmd), 10*time.Second)
	if err != nil {
		return &FSMApplyResult{Error: err.Error()}
	}
	return res
}
