package raft

import (
	"context"
	"strings"
	"testing"
	"time"

	internalpb "github.com/glenjbarber/apiary/api/internalpb"
)

// These tests exercise the RPC surface, not the node method underneath
// it, and they run against a real three-voter loopback cluster for the
// same reason stepaside_test.go does: a wire contract that has only ever
// been checked against a fake proves nothing about what a caller
// receives when leadership genuinely moves.

// stepAsideLocal calls the RPC and fails the test on a gRPC status error.
// Every failure this service reports is a response field, never a
// transport error, so a returned error here is a real defect rather than
// an expected outcome.
func stepAsideLocal(t *testing.T, srv *Server, timeoutMs uint64) *internalpb.StepAsideForRestartResponse {
	t.Helper()
	resp, err := srv.StepAsideForRestartLocal(context.Background(), &internalpb.StepAsideForRestartRequest{
		TimeoutMs: timeoutMs,
	})
	if err != nil {
		t.Fatalf("StepAsideForRestartLocal() transport error: %v", err)
	}
	return resp
}

// assertAnswerIsCoherent enforces the invariant every caller depends on:
// an answer that carries an error must never also grant permission. A
// response that said "safe" and carried an error would be a licence to
// kill a node that is still leading, and that is the single worst thing
// this RPC could do.
func assertAnswerIsCoherent(t *testing.T, resp *internalpb.StepAsideForRestartResponse) {
	t.Helper()
	if resp.GetError() != "" && resp.GetSafeToRestart() {
		t.Errorf("response reports safe_to_restart with error %q; that combination must never be produced", resp.GetError())
	}
	if strings.TrimSpace(resp.GetDetail()) == "" {
		t.Error("response has empty detail; an operator cannot act on a bare bool")
	}
	if resp.GetNodeId() == "" {
		t.Error("response has empty node_id, so a misplaced call could not be detected")
	}
}

// TestServerStepAside_FollowerIsANoOp is the common wire case. Three
// steps out of four in a sweep land on a follower, and that step must be
// immediate, free, and free of any claim to have moved anything.
func TestServerStepAside_FollowerIsANoOp(t *testing.T) {
	nodes := threeNodeCluster(t)
	leader := leaderOf(t, nodes)

	var follower *Node
	for _, n := range nodes {
		if n != leader {
			follower = n
			break
		}
	}
	before := follower.Status().LeaderID

	resp := stepAsideLocal(t, NewServer(follower), 0)
	assertAnswerIsCoherent(t, resp)

	if !resp.GetSafeToRestart() {
		t.Errorf("safe_to_restart = false on a follower (error %q, detail %q); it never held leadership",
			resp.GetError(), resp.GetDetail())
	}
	if resp.GetWasLeader() {
		t.Error("was_leader = true on a follower")
	}
	if resp.GetTransferred() {
		t.Error("transferred = true on a follower; nothing should have moved")
	}
	if resp.GetNewLeaderId() != "" {
		t.Errorf("new_leader_id = %q, want empty when no transfer happened", resp.GetNewLeaderId())
	}
	if resp.GetError() != "" {
		t.Errorf("error = %q, want empty on the follower no-op", resp.GetError())
	}
	if resp.GetNodeId() != follower.Status().NodeID {
		t.Errorf("node_id = %q, want the node that served the request %q", resp.GetNodeId(), follower.Status().NodeID)
	}
	if after := follower.Status().LeaderID; after != before {
		t.Errorf("leadership moved on a follower: %q -> %q", before, after)
	}
}

// TestServerStepAside_LeaderTransfersAndIsSafe is the crux at the wire
// level. The response must name a leader that is demonstrably different
// from this node, and the real cluster must agree with what it claimed.
func TestServerStepAside_LeaderTransfersAndIsSafe(t *testing.T) {
	nodes := threeNodeCluster(t)
	leader := leaderOf(t, nodes)
	self := leader.Status().NodeID

	resp := stepAsideLocal(t, NewServer(leader), 15_000)
	assertAnswerIsCoherent(t, resp)

	if !resp.GetWasLeader() {
		t.Error("was_leader = false on the leader")
	}
	if !resp.GetTransferred() {
		t.Error("transferred = false; the leader should have handed over")
	}
	if resp.GetNewLeaderId() == "" {
		t.Fatal("new_leader_id is empty; a transfer that named nobody proves nothing")
	}
	if resp.GetNewLeaderId() == self {
		t.Errorf("new_leader_id = %q, which is this node; leadership did not actually move", self)
	}
	if !resp.GetSafeToRestart() {
		t.Errorf("safe_to_restart = false after a confirmed handover (error %q)", resp.GetError())
	}
	if resp.GetError() != "" {
		t.Errorf("error = %q, want empty on a confirmed handover", resp.GetError())
	}

	// The claim is about real raft state, not about the string we just
	// sent, so check the cluster itself.
	eventually(t, 5*time.Second, func() bool { return !leader.Status().IsLeader })
	if got := leader.Status().LeaderID; got != resp.GetNewLeaderId() {
		t.Errorf("cluster reports leader %q, but the response claimed %q", got, resp.GetNewLeaderId())
	}
}

// TestServerStepAside_DeadlineIsNotSafe pins the negative case at the
// wire. A caller that names a timeout too short to observe a handover
// must get a refusal, not a permission. One millisecond is the shortest
// expressible value and cannot cover an election, so the answer here is
// deterministic rather than timing-dependent.
func TestServerStepAside_DeadlineIsNotSafe(t *testing.T) {
	nodes := threeNodeCluster(t)
	leader := leaderOf(t, nodes)

	resp := stepAsideLocal(t, NewServer(leader), 1)
	assertAnswerIsCoherent(t, resp)

	if resp.GetSafeToRestart() {
		t.Error("safe_to_restart = true on an unconfirmed handover; that is exactly the bug this guards")
	}
	if resp.GetError() == "" {
		t.Error("error is empty on a timed-out step-aside; the caller would have no reason to distrust the refusal")
	}
	if !resp.GetWasLeader() {
		t.Error("was_leader = false; the node was leading even though the handover timed out")
	}
	if !strings.Contains(resp.GetDetail(), "NOT safe to restart") {
		t.Errorf("detail = %q, want it to say plainly that restarting is not safe", resp.GetDetail())
	}
}

// TestServerStepAside_CancelledContextIsNotSafe covers a caller that goes
// away mid-flight, which is what happens when the coordinator is the
// managerd on the Comb being restarted.
//
// Both outcomes this can produce are correct, so the test asserts the
// property rather than one branch: a refusal is always honest, and a
// success is only honest if a different voter really is leading. What
// must never happen is success without evidence.
func TestServerStepAside_CancelledContextIsNotSafe(t *testing.T) {
	nodes := threeNodeCluster(t)
	leader := leaderOf(t, nodes)
	self := leader.Status().NodeID

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	resp, err := NewServer(leader).StepAsideForRestartLocal(ctx, &internalpb.StepAsideForRestartRequest{TimeoutMs: 15_000})
	if err != nil {
		t.Fatalf("StepAsideForRestartLocal() transport error: %v", err)
	}
	assertAnswerIsCoherent(t, resp)

	if resp.GetSafeToRestart() {
		if resp.GetNewLeaderId() == "" || resp.GetNewLeaderId() == self {
			t.Fatalf("safe_to_restart = true naming new_leader_id %q on a node that was %q; a restart was permitted without a handover",
				resp.GetNewLeaderId(), self)
		}
		if leader.Status().LeaderID != resp.GetNewLeaderId() {
			t.Errorf("safe_to_restart = true for %q, but the cluster reports leader %q",
				resp.GetNewLeaderId(), leader.Status().LeaderID)
		}
	}
}

// TestServerStepAside_ConcurrentCallerIsRefused covers two coordinators
// arriving at the same node at the same time. The latch is taken
// directly rather than raced for, so the refusal is observed
// deterministically instead of depending on which goroutine wins - the
// contract under test is the refusal, not the scheduling.
func TestServerStepAside_ConcurrentCallerIsRefused(t *testing.T) {
	nodes := threeNodeCluster(t)
	leader := leaderOf(t, nodes)
	srv := NewServer(leader)

	srv.stepAsideMu.Lock()
	resp := stepAsideLocal(t, srv, 5_000)
	srv.stepAsideMu.Unlock()

	assertAnswerIsCoherent(t, resp)

	if resp.GetSafeToRestart() {
		t.Error("safe_to_restart = true on a refused concurrent call")
	}
	if resp.GetError() == "" {
		t.Error("error is empty on a refused concurrent call; the caller would retry blindly")
	}
	if !strings.Contains(resp.GetError(), "already in progress") {
		t.Errorf("error = %q, want it to name the real cause", resp.GetError())
	}
	if resp.GetTransferred() {
		t.Error("transferred = true on a call that never ran")
	}
	if !strings.Contains(resp.GetDetail(), "refused") {
		t.Errorf("detail = %q, want it to say the request was refused", resp.GetDetail())
	}

	// The refusal must not have damaged anything, and the latch must
	// have been released: a subsequent call still works.
	after := stepAsideLocal(t, srv, 15_000)
	assertAnswerIsCoherent(t, after)
	if !after.GetSafeToRestart() {
		t.Errorf("a call after the refusal failed (error %q); the latch was not released", after.GetError())
	}
}

// TestServerStepAside_UnsetTimeoutUsesADefault proves timeout_ms = 0 is
// a working default rather than a zero-length deadline. A leader needs
// the default to complete a handover, so a call that omits the field and
// still reports a real handover is the proof.
func TestServerStepAside_UnsetTimeoutUsesADefault(t *testing.T) {
	nodes := threeNodeCluster(t)
	leader := leaderOf(t, nodes)
	self := leader.Status().NodeID

	resp := stepAsideLocal(t, NewServer(leader), 0)
	assertAnswerIsCoherent(t, resp)

	if !resp.GetSafeToRestart() {
		t.Fatalf("safe_to_restart = false with the default timeout (error %q, detail %q)",
			resp.GetError(), resp.GetDetail())
	}
	if resp.GetNewLeaderId() == "" || resp.GetNewLeaderId() == self {
		t.Errorf("new_leader_id = %q; the default timeout did not leave room for a real handover", resp.GetNewLeaderId())
	}
}

// TestServerStepAside_AnswerNamesTheNodeThatAnswered exists because a
// misplaced call is silent otherwise. The RPC is local by contract and
// never forwarded, so the only way a caller learns it reached the wrong
// Comb is by reading node_id off the response.
func TestServerStepAside_AnswerNamesTheNodeThatAnswered(t *testing.T) {
	nodes := threeNodeCluster(t)
	leader := leaderOf(t, nodes)

	var follower *Node
	for _, n := range nodes {
		if n != leader {
			follower = n
			break
		}
	}

	resp := stepAsideLocal(t, NewServer(follower), 0)
	if resp.GetNodeId() != follower.Status().NodeID {
		t.Errorf("node_id = %q, want the node that actually served the request %q, not the leader %q",
			resp.GetNodeId(), follower.Status().NodeID, leader.Status().NodeID)
	}
}
