package raft

import (
	"context"
	"strings"
	"testing"
	"time"
)

// threeNodeCluster builds a real three-voter raft cluster on loopback
// and returns the nodes in creation order. These tests deliberately do
// not mock the raft library: the whole value of a step-aside is what
// hashicorp/raft does when several nodes are really talking, and a mock
// would only prove the mock agrees with itself.
func threeNodeCluster(t *testing.T) []*Node {
	t.Helper()

	cfgs := make([]Config, 3)
	for i := range cfgs {
		cfgs[i] = Config{
			NodeID:   "n" + string(rune('1'+i)),
			DataDir:  t.TempDir(),
			BindAddr: freeLoopbackAddr(t),
		}
	}

	nodes := make([]*Node, len(cfgs))
	for i, cfg := range cfgs {
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
