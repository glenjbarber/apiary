package raft

import (
	"testing"
	"time"
)

// ADR-0156 Stage 2, at the cluster level.
//
// The single-node tests above prove the FSM's refusals. This one proves
// the property the epoch exists for: EVERY VOTER OF ONE COLONY CARRIES
// THE SAME GENERATION, and a node that is behind is refused.
//
// The difference between "the epoch is stored somewhere" and "the epoch
// is usable" is exactly the same as it was for the identity: a value
// held in one process's memory is a value two Combs can disagree
// about, and a stale-epoch node's whole failure mode is that it does
// not know it is stale.

// TestColonyEpoch_ReplicatesToEveryVoter proves that the epoch
// committed on the leader reaches every voter, and that every voter
// answers Status with the same generation.
func TestColonyEpoch_ReplicatesToEveryVoter(t *testing.T) {
	leader, _ := newUnbootstrappedNode(t, "node-a")
	if err := leader.Bootstrap(); err != nil {
		t.Fatalf("Bootstrap() error: %v", err)
	}
	eventually(t, 5*time.Second, func() bool { return leader.Status().IsLeader })

	nodeB, cfgB := newUnbootstrappedNode(t, "node-b")
	if err := leader.AddVoter(cfgB.NodeID, cfgB.BindAddr, 0, 5*time.Second); err != nil {
		t.Fatalf("AddVoter(node-b) error: %v", err)
	}
	eventually(t, 5*time.Second, func() bool { return len(leader.Status().Servers) == 2 })

	// The leader establishes the epoch.
	epoch, err := leader.EnsureColonyEpoch(5 * time.Second)
	if err != nil {
		t.Fatalf("EnsureColonyEpoch() error: %v", err)
	}
	if epoch != initialColonyEpoch {
		t.Fatalf("EnsureColonyEpoch() = %d, want %d (ADR-0156's Decision 3: the initial epoch is 1)", epoch, initialColonyEpoch)
	}

	// Every voter must converge on the SAME generation. The follower
	// has no epoch of its own until the leader's commit replicates, so
	// this is the property that matters: the answer is the same on
	// both sides because it came from the same committed log entry.
	eventually(t, 5*time.Second, func() bool {
		return nodeB.Status().ColonyEpoch == epoch
	})

	if got := leader.Status().ColonyEpoch; got != epoch {
		t.Fatalf("leader Status().ColonyEpoch = %d, want %d", got, epoch)
	}

	// And a follower asked to establish an epoch of its own must NOT
	// create a second one. It already HAS the epoch - it was
	// replicated to it - so it answers with the same one.
	fromFollower, err := nodeB.EnsureColonyEpoch(5 * time.Second)
	if err != nil {
		t.Fatalf("EnsureColonyEpoch() on a follower error: %v", err)
	}
	if fromFollower != epoch {
		t.Fatalf("EnsureColonyEpoch() on the follower returned %d, want the replicated %d", fromFollower, epoch)
	}

	// A node that has NOT been told the epoch and is not the leader
	// cannot commit one, and says so rather than inventing one.
	fresh, _ := newUnbootstrappedNode(t, "node-c")
	if _, err := fresh.EnsureColonyEpoch(5 * time.Second); err == nil {
		t.Fatal("EnsureColonyEpoch() on a node with no epoch and no leadership returned no error, want a refusal")
	}

	// The leader asked a second time must return the SAME epoch rather
	// than establishing a new one.
	again, err := leader.EnsureColonyEpoch(5 * time.Second)
	if err != nil {
		t.Fatalf("EnsureColonyEpoch() a second time error: %v", err)
	}
	if again != epoch {
		t.Fatalf("EnsureColonyEpoch() a second time returned %d, want %d", again, epoch)
	}
}

// TestColonyEpoch_AdvancesOnTheLeaderOnly proves the increment rules at
// the node level: the leader advances the Colony's generation, every
// voter converges on the new one, and a follower cannot advance it.
func TestColonyEpoch_AdvancesOnTheLeaderOnly(t *testing.T) {
	leader, _ := newUnbootstrappedNode(t, "node-a")
	if err := leader.Bootstrap(); err != nil {
		t.Fatalf("Bootstrap() error: %v", err)
	}
	eventually(t, 5*time.Second, func() bool { return leader.Status().IsLeader })

	nodeB, cfgB := newUnbootstrappedNode(t, "node-b")
	if err := leader.AddVoter(cfgB.NodeID, cfgB.BindAddr, 0, 5*time.Second); err != nil {
		t.Fatalf("AddVoter(node-b) error: %v", err)
	}
	eventually(t, 5*time.Second, func() bool { return len(leader.Status().Servers) == 2 })

	if _, err := leader.EnsureColonyEpoch(5 * time.Second); err != nil {
		t.Fatalf("EnsureColonyEpoch() error: %v", err)
	}

	// A follower cannot advance the epoch: it is not the leader, and
	// only the leader can commit.
	if _, err := nodeB.AdvanceColonyEpoch(5 * time.Second); err == nil {
		t.Fatal("AdvanceColonyEpoch() on a follower returned no error, want a refusal")
	}

	// The leader can.
	advanced, err := leader.AdvanceColonyEpoch(5 * time.Second)
	if err != nil {
		t.Fatalf("AdvanceColonyEpoch() error: %v", err)
	}
	if advanced != initialColonyEpoch+1 {
		t.Fatalf("AdvanceColonyEpoch() = %d, want %d", advanced, initialColonyEpoch+1)
	}

	// And every voter converges on the new generation - which is what
	// makes the stale-epoch comparison meaningful at all.
	eventually(t, 5*time.Second, func() bool {
		return nodeB.Status().ColonyEpoch == initialColonyEpoch+1
	})

	// A second advance moves it on again, by exactly one.
	advanced, err = leader.AdvanceColonyEpoch(5 * time.Second)
	if err != nil {
		t.Fatalf("AdvanceColonyEpoch() a second time error: %v", err)
	}
	if advanced != initialColonyEpoch+2 {
		t.Fatalf("AdvanceColonyEpoch() a second time = %d, want %d", advanced, initialColonyEpoch+2)
	}
}
