package raft

import (
	"testing"
	"time"
)

// ADR-0156 Stage 1, at the cluster level.
//
// The single-node tests above prove the FSM's refusals. This one proves
// the property the whole feature exists for: EVERY VOTER OF ONE COLONY
// ANSWERS WITH THE SAME IDENTITY.
//
// That is the difference between "the identity is stored somewhere" and
// "the identity is usable". Two independently bootstrapped raftd
// instances each form their own single-node cluster, and each would
// answer from its own config file with a different value - which is the
// condition ADR-0156 records as verified and undetectable. Replicated
// through the log, both members of ONE colony answer with the SAME
// value, so the difference between two colonies becomes visible instead
// of silent.

// TestColonyIdentity_ReplicatesToEveryVoter proves that a Colony
// identity committed on the leader reaches every voter, and that every
// voter answers Status with the same value.
func TestColonyIdentity_ReplicatesToEveryVoter(t *testing.T) {
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

	// The leader establishes the identity.
	colonyID, err := leader.EnsureColonyIdentity(5 * time.Second)
	if err != nil {
		t.Fatalf("EnsureColonyIdentity() error: %v", err)
	}
	if colonyID == "" {
		t.Fatal("EnsureColonyIdentity() returned an empty identity, want one")
	}

	// Every voter must converge on the SAME identity. The follower has
	// no identity of its own until the leader's commit replicates, so
	// this is the property that matters: the answer is the same on both
	// sides because it came from the same committed log entry.
	eventually(t, 5*time.Second, func() bool {
		return nodeB.Status().ColonyID == colonyID
	})

	if got := leader.Status().ColonyID; got != colonyID {
		t.Fatalf("leader Status().ColonyID = %q, want %q", got, colonyID)
	}

	// And a follower asked to establish an identity of its own must NOT
	// create a second one. It already HAS the identity - it was
	// replicated to it - so it answers with the same one. The check
	// that matters is that it does not generate a different one.
	fromFollower, err := nodeB.EnsureColonyIdentity(5 * time.Second)
	if err != nil {
		t.Fatalf("EnsureColonyIdentity() on a follower error: %v", err)
	}
	if fromFollower != colonyID {
		t.Fatalf("EnsureColonyIdentity() on the follower returned %q, want the replicated %q", fromFollower, colonyID)
	}

	// A node that has NOT been told the identity and is not the leader
	// cannot commit one, and says so rather than inventing one.
	fresh, _ := newUnbootstrappedNode(t, "node-c")
	if _, err := fresh.EnsureColonyIdentity(5 * time.Second); err == nil {
		t.Fatal("EnsureColonyIdentity() on a node with no identity and no leadership returned no error, want a refusal")
	}

	// The leader asked a second time must return the SAME identity
	// rather than generating a new one.
	again, err := leader.EnsureColonyIdentity(5 * time.Second)
	if err != nil {
		t.Fatalf("EnsureColonyIdentity() called twice error: %v", err)
	}
	if again != colonyID {
		t.Fatalf("EnsureColonyIdentity() called twice returned %q, want the original %q", again, colonyID)
	}
}

// TestColonyIdentity_IsSharedByAMultiNodeColony proves the property
// that makes the identity usable in practice: a three-node Colony, one
// identity, every member agreeing.
func TestColonyIdentity_IsSharedByAMultiNodeColony(t *testing.T) {
	leader, _ := newUnbootstrappedNode(t, "node-a")
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
	eventually(t, 5*time.Second, func() bool { return len(leader.Status().Servers) == 3 })

	colonyID, err := leader.EnsureColonyIdentity(5 * time.Second)
	if err != nil {
		t.Fatalf("EnsureColonyIdentity() error: %v", err)
	}

	eventually(t, 10*time.Second, func() bool {
		return nodeB.Status().ColonyID == colonyID && nodeC.Status().ColonyID == colonyID
	})

	// Every member answers with one identity, and the leader's is the
	// one they all agree on.
	if leader.Status().ColonyID != colonyID {
		t.Fatalf("leader identity = %q, want %q", leader.Status().ColonyID, colonyID)
	}
}

// TestColonyIdentity_TwoIndependentColoniesHaveDifferentIdentities is
// the negative case, and it is the one that matters most.
//
// Two separately bootstrapped nodes each form their own single-node
// colony. Before ADR-0156 nothing could distinguish those two colonies
// from one colony with two voters - and the second of them looks
// exactly like a healthy single-node colony to every system in the
// tree. With the identity, each answers with its own, and the two are
// distinguishable.
func TestColonyIdentity_TwoIndependentColoniesHaveDifferentIdentities(t *testing.T) {
	colonyA, _ := newUnbootstrappedNode(t, "node-a")
	if err := colonyA.Bootstrap(); err != nil {
		t.Fatalf("Bootstrap() error: %v", err)
	}
	eventually(t, 5*time.Second, func() bool { return colonyA.Status().IsLeader })

	colonyB, _ := newUnbootstrappedNode(t, "node-b")
	if err := colonyB.Bootstrap(); err != nil {
		t.Fatalf("Bootstrap() error: %v", err)
	}
	eventually(t, 5*time.Second, func() bool { return colonyB.Status().IsLeader })

	idA, err := colonyA.EnsureColonyIdentity(5 * time.Second)
	if err != nil {
		t.Fatalf("EnsureColonyIdentity() on colony A error: %v", err)
	}
	idB, err := colonyB.EnsureColonyIdentity(5 * time.Second)
	if err != nil {
		t.Fatalf("EnsureColonyIdentity() on colony B error: %v", err)
	}

	if idA == "" || idB == "" {
		t.Fatalf("two independent colonies produced identities %q and %q, want two non-empty identities", idA, idB)
	}
	if idA == idB {
		t.Fatalf("two independently bootstrapped colonies produced the SAME identity %q; they must be distinguishable", idA)
	}
}
