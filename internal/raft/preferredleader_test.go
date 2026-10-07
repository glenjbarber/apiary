package raft

import (
	"context"
	"testing"
	"time"

	"github.com/hashicorp/raft"
)

// rawServerID and rawServerAddress exist only so this test can call
// transferToServer directly (bypassing PreferredLeaderTransfer's own
// cooldown bookkeeping) to simulate a transfer initiated by some other
// means, exactly as a human operator's own LeadershipTransferToServer
// call would. Everywhere else in this package the raft.ServerID/
// raft.ServerAddress conversions happen inline (see node.go, seed.go),
// but a bare raft.ServerID(x) at the call site this test needs reads
// as a type assertion at a glance - naming the conversion makes clear
// it is deliberate test scaffolding, not production logic.
func rawServerID(id string) raft.ServerID { return raft.ServerID(id) }

func rawServerAddress(addr string) raft.ServerAddress { return raft.ServerAddress(addr) }

// TestPreferredLeaderTransfer_FollowerIsANoOp mirrors
// TestStepAside_FollowerIsANoOp's own reasoning: the common case is a
// node that is not leading, and it must answer immediately without
// touching raft state.
func TestPreferredLeaderTransfer_FollowerIsANoOp(t *testing.T) {
	nodes := threeNodeCluster(t)
	leader := leaderOf(t, nodes)

	var follower *Node
	for _, n := range nodes {
		if n != leader {
			follower = n
			break
		}
	}

	follower.config.PreferredLeaderID = "someone-else"

	result, err := follower.PreferredLeaderTransfer(context.Background())
	if err != nil {
		t.Fatalf("PreferredLeaderTransfer() error: %v", err)
	}
	if result.Attempted {
		t.Fatalf("follower attempted a transfer: %+v", result)
	}
}

// TestPreferredLeaderTransfer_NotConfigured documents that calling this
// with no configured preference is a caller bug, not a silent no-op.
func TestPreferredLeaderTransfer_NotConfigured(t *testing.T) {
	nodes := threeNodeCluster(t)
	leader := leaderOf(t, nodes)

	_, err := leader.PreferredLeaderTransfer(context.Background())
	if err != ErrPreferredLeaderNotConfigured {
		t.Fatalf("got err %v, want ErrPreferredLeaderNotConfigured", err)
	}
}

// TestPreferredLeaderTransfer_AlreadyPreferred covers the leader
// already being its own preference: no transfer, no error.
func TestPreferredLeaderTransfer_AlreadyPreferred(t *testing.T) {
	nodes := threeNodeCluster(t)
	leader := leaderOf(t, nodes)
	leader.config.PreferredLeaderID = leader.config.NodeID

	result, err := leader.PreferredLeaderTransfer(context.Background())
	if err != nil {
		t.Fatalf("PreferredLeaderTransfer() error: %v", err)
	}
	if result.Attempted {
		t.Fatalf("leader attempted a transfer to itself: %+v", result)
	}
}

// TestPreferredLeaderTransfer_UnknownPreferredIsANoOp covers a
// configured preference that does not (yet, or any longer) name a
// real member of the cluster - must do nothing, not error.
func TestPreferredLeaderTransfer_UnknownPreferredIsANoOp(t *testing.T) {
	nodes := threeNodeCluster(t)
	leader := leaderOf(t, nodes)
	leader.config.PreferredLeaderID = "nonexistent-node"

	result, err := leader.PreferredLeaderTransfer(context.Background())
	if err != nil {
		t.Fatalf("PreferredLeaderTransfer() error: %v", err)
	}
	if result.Attempted {
		t.Fatalf("leader attempted a transfer to an unknown node: %+v", result)
	}
}

// TestPreferredLeaderTransfer_RealTransfer is this feature's
// load-bearing proof: against a real three-node raft cluster, the
// current leader actually hands leadership to the configured preferred
// voter via the raft library's own LeadershipTransferToServer, and the
// cluster settles on that voter as leader.
func TestPreferredLeaderTransfer_RealTransfer(t *testing.T) {
	nodes := threeNodeCluster(t)
	leader := leaderOf(t, nodes)

	var preferred *Node
	for _, n := range nodes {
		if n != leader {
			preferred = n
			break
		}
	}
	leader.config.PreferredLeaderID = preferred.config.NodeID

	result, err := leader.PreferredLeaderTransfer(context.Background())
	if err != nil {
		t.Fatalf("PreferredLeaderTransfer() error: %v", err)
	}
	if !result.Attempted || !result.Transferred {
		t.Fatalf("expected a successful transfer, got %+v", result)
	}

	eventually(t, 10*time.Second, func() bool {
		return preferred.Status().IsLeader
	})
	if leader.Status().IsLeader {
		t.Fatalf("old leader %s is still reporting itself as leader", leader.config.NodeID)
	}
}

// TestPreferredLeaderTransfer_CooldownPreventsThrashing proves the
// thrash-prevention policy: once a node has INITIATED a transfer
// attempt, it will not initiate another one toward its own
// preference until PreferredLeaderCooldown elapses, even if it
// regains leadership in the meantime. Without this, a leader that
// keeps winning leadership back (e.g. because its peers are also
// biasing toward the same preference, or an election simply lands on
// it again) would re-trigger a transfer on every single tick, which
// is exactly the repeated-handover thrashing the issue asks this
// feature to avoid.
func TestPreferredLeaderTransfer_CooldownPreventsThrashing(t *testing.T) {
	nodes := threeNodeCluster(t)
	leader := leaderOf(t, nodes)

	var preferred *Node
	for _, n := range nodes {
		if n != leader {
			preferred = n
			break
		}
	}
	leader.config.PreferredLeaderID = preferred.config.NodeID

	first, err := leader.PreferredLeaderTransfer(context.Background())
	if err != nil {
		t.Fatalf("first PreferredLeaderTransfer() error: %v", err)
	}
	if !first.Attempted || !first.Transferred {
		t.Fatalf("expected the first call to transfer, got %+v", first)
	}
	eventually(t, 10*time.Second, func() bool { return preferred.Status().IsLeader })

	// Hand leadership straight back to the original node, bypassing
	// this package entirely (a direct raft call), so the only thing
	// under test is whether THAT node's own cooldown - set by the
	// first call above - suppresses a second attempt.
	if err := preferred.transferToServer(context.Background(), rawServerID(leader.config.NodeID), rawServerAddress(leader.config.BindAddr)); err != nil {
		t.Fatalf("handing leadership back: %v", err)
	}
	eventually(t, 10*time.Second, func() bool { return leader.Status().IsLeader })

	second, err := leader.PreferredLeaderTransfer(context.Background())
	if err != nil {
		t.Fatalf("second PreferredLeaderTransfer() error: %v", err)
	}
	if second.Attempted {
		t.Fatalf("expected the cooldown to suppress a second transfer, got %+v", second)
	}
	if !leader.Status().IsLeader {
		t.Fatalf("leadership moved despite the cooldown")
	}
}
