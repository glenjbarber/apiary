package raft

import (
	"testing"
	"time"
)

// newUnbootstrappedNode creates a Node that has not bootstrapped or joined
// any cluster yet — it's ready to be added as a voter by an existing
// leader via Node.AddVoter.
func newUnbootstrappedNode(t *testing.T, id string) (*Node, Config) {
	t.Helper()
	cfg := Config{
		NodeID:   id,
		DataDir:  t.TempDir(),
		BindAddr: freeLoopbackAddr(t),
	}
	node, err := New(cfg)
	if err != nil {
		t.Fatalf("New(%s) error: %v", id, err)
	}
	t.Cleanup(func() { node.Shutdown() })
	return node, cfg
}

func TestMultiNode_AddVoterFormsCluster(t *testing.T) {
	leader, leaderCfg := newUnbootstrappedNode(t, "node-a")
	if err := leader.Bootstrap(); err != nil {
		t.Fatalf("Bootstrap() error: %v", err)
	}
	eventually(t, 5*time.Second, func() bool { return leader.Status().IsLeader })

	nodeB, cfgB := newUnbootstrappedNode(t, "node-b")
	nodeC, cfgC := newUnbootstrappedNode(t, "node-c")

	if err := leader.AddVoter(cfgB.NodeID, cfgB.BindAddr, 0, 5*time.Second); err != nil {
		t.Fatalf("AddVoter(node-b) error: %v", err)
	}
	if err := leader.AddVoter(cfgC.NodeID, cfgC.BindAddr, 0, 5*time.Second); err != nil {
		t.Fatalf("AddVoter(node-c) error: %v", err)
	}

	eventually(t, 5*time.Second, func() bool {
		return len(leader.Status().Servers) == 3
	})

	// Both new members should recognize node-a as leader once they've
	// received the configuration change and subsequent heartbeats.
	eventually(t, 5*time.Second, func() bool {
		return nodeB.Status().LeaderID == leaderCfg.NodeID
	})
	eventually(t, 5*time.Second, func() bool {
		return nodeC.Status().LeaderID == leaderCfg.NodeID
	})

	// A command applied on the leader should replicate to both followers.
	if _, err := leader.Apply(mustMarshalCommand(t, createVMCmd("vm-1", "replicate-me")), 5*time.Second); err != nil {
		t.Fatalf("Apply() error: %v", err)
	}
	eventually(t, 5*time.Second, func() bool { return nodeB.Status().AppliedIndex > 0 })
	eventually(t, 5*time.Second, func() bool { return nodeC.Status().AppliedIndex > 0 })

	// Removing node-c should shrink the configuration back to 2 servers.
	if err := leader.RemoveServer(cfgC.NodeID, 0, 5*time.Second); err != nil {
		t.Fatalf("RemoveServer(node-c) error: %v", err)
	}
	eventually(t, 5*time.Second, func() bool {
		return len(leader.Status().Servers) == 2
	})
}

func TestMultiNode_AddVoterFailsAgainstNonLeader(t *testing.T) {
	nodeB, _ := newUnbootstrappedNode(t, "node-b")

	// nodeB has no configuration at all (never bootstrapped, never
	// joined), so it is never the leader of anything; AddVoter against it
	// must fail rather than silently doing nothing.
	err := nodeB.AddVoter("node-x", "127.0.0.1:1", 0, 2*time.Second)
	if err == nil {
		t.Fatalf("AddVoter() against a non-leader = nil error, want an error")
	}
}
