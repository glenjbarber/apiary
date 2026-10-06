package raft

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/raft"
)

// threeNodeCluster builds a real three-voter raft cluster on loopback
// and returns the nodes in creation order. These tests deliberately do
// not mock the raft library: the whole value of a step-aside is what
// hashicorp/raft does when several nodes are really talking, and a mock
// would only prove the mock agrees with itself.
func threeNodeCluster(t *testing.T) []*Node {
	t.Helper()

	// Bind each node before selecting the next address. Selecting all
	// addresses first closes every temporary listener, so the OS can give
	// two nodes the same port before either Raft listener claims it.
	nodes := make([]*Node, 3)
	for i := range nodes {
		cfg := Config{
			NodeID:   "n" + string(rune('1'+i)),
			DataDir:  t.TempDir(),
			BindAddr: freeLoopbackAddr(t),
		}
		n, err := New(cfg)
		if err != nil {
			t.Fatalf("New(%s) error: %v", cfg.NodeID, err)
		}
		t.Cleanup(func() { n.Shutdown() })
		nodes[i] = n
	}
	if err := nodes[0].Bootstrap(); err != nil {
		t.Fatalf("Bootstrap() error: %v", err)
	}
	eventually(t, 10*time.Second, func() bool { return nodes[0].Status().IsLeader })

	// Join the other two as voters. prevIndex 0 is correct here: the
	// leader's log is empty, so there is nothing to catch up from. The
	// address is the joining node's own BindAddr, which is what its
	// transport advertises - it is not in the leader's configuration
	// until this call succeeds, which is why it cannot be looked up
	// there first.
	for _, n := range nodes[1:] {
		if err := nodes[0].AddVoter(n.config.NodeID, n.config.BindAddr, 0, 5*time.Second); err != nil {
			t.Fatalf("AddVoter(%s) error: %v", n.config.NodeID, err)
		}
	}
	eventually(t, 10*time.Second, func() bool { return len(nodes[0].Status().Servers) == 3 })
	return nodes
}

func leaderOf(t *testing.T, nodes []*Node) *Node {
	t.Helper()
	for _, n := range nodes {
		if n.Status().IsLeader {
			return n
		}
	}
	t.Fatal("no leader among the cluster nodes")
	return nil
}

// TestStepAside_FollowerIsANoOp is the common case: three steps out of
// four in a sweep, the node being restarted is not leading. It must
// answer immediately, report no transfer, and change nothing.
func TestStepAside_FollowerIsANoOp(t *testing.T) {
	nodes := threeNodeCluster(t)
	follower := leaderOf(t, nodes)
	for _, n := range nodes {
		if n != follower {
			follower = n
			break
		}
	}
	before := follower.Status().LeaderID

	res, err := follower.StepAsideForRestart(context.Background(), 5*time.Second)
	if err != nil {
		t.Fatalf("StepAsideForRestart() error: %v", err)
	}
	if res.WasLeader {
		t.Error("WasLeader = true on a follower")
	}
	if res.Transferred {
		t.Error("Transferred = true on a follower; nothing should have moved")
	}
	if res.NewLeaderID != "" {
		t.Errorf("NewLeaderID = %q, want empty when no transfer happened", res.NewLeaderID)
	}
	if !res.SafeToRestart {
		t.Error("SafeToRestart = false on a follower; it never held leadership")
	}
	if after := follower.Status().LeaderID; after != before {
		t.Errorf("leadership moved on a follower: %q -> %q", before, after)
	}
	if !strings.Contains(res.Detail, "not the raft leader") {
		t.Errorf("Detail = %q, want it to say no leadership was moved", res.Detail)
	}
}

// TestStepAside_LeaderHandsOverThenIsSafe is the crux. A node that IS
// leading must hand over, and only then - after another voter is
// observably leading - may it report that a restart is safe.
func TestStepAside_LeaderHandsOverThenIsSafe(t *testing.T) {
	nodes := threeNodeCluster(t)
	leader := leaderOf(t, nodes)
	self := leader.config.NodeID

	res, err := leader.StepAsideForRestart(context.Background(), 15*time.Second)
	if err != nil {
		t.Fatalf("StepAsideForRestart() error: %v", err)
	}
	if !res.WasLeader {
		t.Error("WasLeader = false on the leader")
	}
	if !res.Transferred {
		t.Error("Transferred = false; the leader should have handed over")
	}
	if res.NewLeaderID == "" {
		t.Fatal("NewLeaderID is empty; a transfer that named nobody proves nothing")
	}
	if res.NewLeaderID == self {
		t.Errorf("NewLeaderID = %q, which is this node; leadership did not actually move", self)
	}
	if !res.SafeToRestart {
		t.Error("SafeToRestart = false after a confirmed handover")
	}

	// The claim is about real raft state, not about what the future
	// reported, so check the cluster itself.
	eventually(t, 5*time.Second, func() bool { return !leader.Status().IsLeader })
	if got := leader.Status().LeaderID; got != res.NewLeaderID {
		t.Errorf("cluster now reports leader %q, but the result claimed %q", got, res.NewLeaderID)
	}

	// And someone must actually be leading. A colony with no leader is
	// not a colony it is safe to have just removed its leader from.
	someone := false
	for _, n := range nodes {
		if n.Status().IsLeader {
			someone = true
		}
	}
	if !someone {
		t.Error("no node is leading after the handover; the cluster is leaderless")
	}
}

// TestStepAside_AfterHandoverTheOldLeaderIsANoOp proves the operation
// is idempotent in the direction that matters: a second call on the same
// node must not try to transfer anything, because it is no longer leading.
func TestStepAside_AfterHandoverTheOldLeaderIsANoOp(t *testing.T) {
	nodes := threeNodeCluster(t)
	leader := leaderOf(t, nodes)

	if _, err := leader.StepAsideForRestart(context.Background(), 15*time.Second); err != nil {
		t.Fatalf("first StepAsideForRestart() error: %v", err)
	}
	res, err := leader.StepAsideForRestart(context.Background(), 5*time.Second)
	if err != nil {
		t.Fatalf("second StepAsideForRestart() error: %v", err)
	}
	if res.Transferred {
		t.Error("the second call transferred again; a follower has nothing to hand over")
	}
	if !res.SafeToRestart {
		t.Error("SafeToRestart = false on a second call from a node that already handed over")
	}
}

// TestStepAside_DeadlineIsRespected pins the negative case. A timeout
// too short to complete a handover must produce NOT safe, and must say
// so in words that cannot be mistaken for permission.
func TestStepAside_DeadlineIsRespected(t *testing.T) {
	nodes := threeNodeCluster(t)
	leader := leaderOf(t, nodes)

	// A deadline this short cannot cover the library's own election
	// timeout, so the operation must run out rather than claim success.
	res, err := leader.StepAsideForRestart(context.Background(), time.Nanosecond)
	if err == nil {
		t.Fatal("a 1ns deadline reported success; it cannot have observed a handover")
	}
	if res.SafeToRestart {
		t.Error("SafeToRestart = true on an unconfirmed handover; that is exactly the bug this guards")
	}
	if res.WasLeader != true {
		t.Error("WasLeader = false; the node was the leader even though the handover timed out")
	}
	if !strings.Contains(res.Detail, "NOT safe to restart") {
		t.Errorf("Detail = %q, want it to say plainly that restarting is not safe", res.Detail)
	}
}

// TestStepAside_ResultDetailIsNeverEmpty keeps every path
// operator-readable. A result whose only content is a bool is a result
// an operator cannot act on.
func TestStepAside_ResultDetailIsNeverEmpty(t *testing.T) {
	nodes := threeNodeCluster(t)

	follower := nodes[0]
	if follower.Status().IsLeader {
		follower = nodes[1]
	}
	res, err := follower.StepAsideForRestart(context.Background(), 5*time.Second)
	if err != nil {
		t.Fatalf("follower StepAsideForRestart() error: %v", err)
	}
	if strings.TrimSpace(res.Detail) == "" {
		t.Error("follower result has empty Detail")
	}

	leader := leaderOf(t, nodes)
	lres, err := leader.StepAsideForRestart(context.Background(), 15*time.Second)
	if err != nil {
		t.Fatalf("leader StepAsideForRestart() error: %v", err)
	}
	if strings.TrimSpace(lres.Detail) == "" {
		t.Error("leader result has empty Detail")
	}
}

// TestAwaitAnotherLeader_WillNotAcceptThisNodeOrANamelessLeader covers
// the confirmation loop directly, which StepAsideForRestart only ever
// reaches on a handover the library has already reported complete - so
// no other test in this file enters it on a deliberately wrong state.
//
// The loop is the whole of ADR-0145's safety claim on the raft side:
// "the transfer future finished" is not "another voter is leading", and
// only a re-read of real state says so. It has three clauses, and this
// test pins the middle one - the nameless-leader guard - which nothing
// else in the package reaches.
//
// WHAT THIS DOES AND DOES NOT KILL, measured on 2026-09-27 against
// hashicorp/raft v1.8.0 rather than assumed:
//
//   - Dropping `leaderID != ""` is caught, and clearly: with a quorum
//     lost, this node steps down while naming no leader, and the loop
//     would otherwise report a confirmed handover with an empty leader
//     name. That is the failure this whole check exists to prevent.
//   - Dropping `string(leaderID) != self` is NOT caught, and cannot be
//     against a real cluster. This node names itself in LeaderWithID()
//     only while it is in state Leader, and setState clears the known
//     leader before it changes the state (raft.go's own comment says so,
//     and its implementation calls setLeader("", "") first). A reader
//     can therefore observe an unnamed leader while still leading, but
//     never this node's own id while no longer leading - the surviving
//     `State() != raft.Leader` clause covers every window the removed
//     one did. The two are redundant against this library version, and
//     separating them would take a fake, which is precisely what this
//     file's other tests exist to avoid.
//   - Dropping `n.raft.State() != raft.Leader` is NOT caught, for the
//     mirror reason: while this node still names itself as leader it is
//     by definition still in state Leader, so the `!= self` clause
//     covers it. The first case below therefore pins the pair, not
//     either half - it fails if both are removed, and on neither alone.
//
// The two clauses this test cannot separate are recorded here rather
// than papered over with a stub: a test that passed against those
// mutations by construction would be worse than an honest gap.
func TestAwaitAnotherLeader_WillNotAcceptThisNodeOrANamelessLeader(t *testing.T) {
	nodes := threeNodeCluster(t)
	leader := leaderOf(t, nodes)
	self := leader.config.NodeID

	// A node that is still the leader names ITSELF as the leader. A
	// loop that only checked "leaderID != ''" would return here
	// immediately and report a completed handover that never happened.
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	got, err := leader.awaitAnotherLeader(ctx, self)
	if err == nil {
		t.Fatalf("awaitAnotherLeader() on the current leader returned (%q, nil); it accepted this node as its own successor", got)
	}
	if got != "" {
		t.Errorf("awaitAnotherLeader() = %q on a failed confirmation, want empty: naming a leader that was never confirmed is how a restart gets authorized on a node that is still leading", got)
	}
	if !errors.Is(err, ErrStepAsideIncomplete) {
		t.Errorf("err = %v, want it to wrap ErrStepAsideIncomplete so a caller can tell \"not confirmed\" from \"stopped working\"", err)
	}
	if !leader.Status().IsLeader {
		t.Fatal("this node stopped leading during a check that only waited; nothing in this test should have moved leadership")
	}

	// Now the second guard. Shutting the other two voters down leaves
	// this node leading a cluster with no quorum, and it steps down on
	// its own leader-lease timeout - at which point LeaderWithID() is
	// briefly empty and there is no new leader to name. A loop that
	// accepted a nameless leader would answer "safe" here, on a
	// leaderless cluster, which is the failure this whole check exists
	// to prevent.
	for _, n := range nodes {
		if n != leader {
			n.Shutdown()
		}
	}
	eventually(t, 10*time.Second, func() bool { return leader.raft.State() != raft.Leader })

	// A fresh wait, and only once the node really is a follower with
	// nothing to name: if the state has already settled onto some other
	// value the test has nothing to assert, so it says so rather than
	// passing vacuously.
	if _, id := leader.raft.LeaderWithID(); id != "" {
		t.Skipf("this node still names leader %q after the quorum was lost; the nameless-leader window has already closed and this test cannot reach it", id)
	}
	ctx2, cancel2 := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel2()
	got2, err2 := leader.awaitAnotherLeader(ctx2, self)
	if err2 == nil {
		t.Fatalf("awaitAnotherLeader() on a leaderless cluster returned (%q, nil); it accepted an unnamed leader as a confirmed handover", got2)
	}
	if got2 != "" {
		t.Errorf("awaitAnotherLeader() = %q with no leader in the cluster, want empty", got2)
	}
}
