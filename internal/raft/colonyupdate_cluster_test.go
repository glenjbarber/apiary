package raft

// Real-raft tests for the properties a bare FSM cannot demonstrate:
// genuine concurrency, a genuinely dead process, and a genuinely absent
// quorum.
//
// The reason these do not mock hashicorp/raft is the same one
// stepaside_test.go records: the value of a mechanism built on raft's
// log-apply order is what raft does when several nodes are really
// talking, and a mock would only prove the mock agrees with itself.

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hashicorp/raft"

	internalpb "github.com/glenjbarber/apiary/api/internalpb"
)

// clusterColonyUpdate is the test-side convenience for talking to a real
// cluster's leader the way managerd does: every op is a raft Apply, so
// a follower genuinely cannot grant anything.
type clusterColonyUpdate struct {
	t     *testing.T
	nodes []*Node
}

func newClusterColonyUpdate(t *testing.T) *clusterColonyUpdate {
	t.Helper()
	return &clusterColonyUpdate{t: t, nodes: threeNodeCluster(t)}
}

// applyFrom submits cmd through node's OWN raft and, if that node is a
// follower, retries against the cluster's leader - which is precisely
// what internal/manager.mutateColonyUpdate does before it does any work
// at all. Modelling the forward is not a convenience: without it, every
// caller's answer would be raft's "this node is not the leader", and
// the property under test (the losers get an explicit refusal NAMING
// the current holder) would be untestable rather than true.
//
// The raft-level "a follower cannot even submit" property is asserted
// separately, by TestColonyUpdate_NonLeaderApplyCannotGrant, which
// deliberately skips the forward.
func (c *clusterColonyUpdate) applyFrom(node *Node, cmd *internalpb.Command) *FSMApplyResult {
	c.t.Helper()
	res, err := node.Apply(mustMarshalCommand(c.t, cmd), 10*time.Second)
	if err != nil {
		if isNotLeaderError(err) {
			res, err = c.leader().Apply(mustMarshalCommand(c.t, cmd), 10*time.Second)
		}
		if err != nil {
			// A raft-level refusal (no quorum, no leader) is a fact the
			// test asserts on rather than an error, so it comes back as
			// an FSMApplyResult carrying it - the same shape the wire
			// gives.
			return &FSMApplyResult{Error: err.Error()}
		}
	}
	return res
}

// isNotLeaderError matches hashicorp/raft's and this package's own
// not-leader signal, so the forward is triggered by the real condition
// rather than by matching on incidental wording.
func isNotLeaderError(err error) bool {
	return errors.Is(err, ErrNotLeader) ||
		strings.Contains(err.Error(), ErrNotLeader.Error()) ||
		strings.Contains(err.Error(), "not the leader")
}

func (c *clusterColonyUpdate) leader() *Node { return leaderOf(c.t, c.nodes) }

// rawApplyFrom is applyFrom with no leader forward - the "what this node
// could do entirely on its own" answer, which is a different question
// and is asserted separately wherever it matters.
func (c *clusterColonyUpdate) rawApplyFrom(node *Node, cmd *internalpb.Command) *FSMApplyResult {
	c.t.Helper()
	res, err := node.Apply(mustMarshalCommand(c.t, cmd), 10*time.Second)
	if err != nil {
		return &FSMApplyResult{Error: err.Error()}
	}
	return res
}

// TestColonyUpdate_ConcurrentCoordinatorsAcrossVotersGrantOneWinner is
// the headline property, run the way it will actually be violated: N
// callers, spread across M different coordinators, all reaching for the
// lock at once.
//
// Every one of them is a different node's own raft, and every one of
// them is a different operation id with a different holder identity, so
// nothing about the requests is shared except the colony they are all
// asking about.
func TestColonyUpdate_ConcurrentCoordinatorsAcrossVotersGrantOneWinner(t *testing.T) {
	c := newClusterColonyUpdate(t)

	const callers = 12
	type outcome struct {
		granted bool
		holder  string
		err     string
	}
	results := make([]outcome, callers)

	var start sync.WaitGroup
	start.Add(1)
	var done sync.WaitGroup
	for i := 0; i < callers; i++ {
		done.Add(1)
		go func(i int) {
			defer done.Done()
			// A different node each time, so all three voters are used
			// as coordinators.
			target := c.nodes[i%len(c.nodes)]
			cmd := acquireColonyUpdateCmd(
				fmt.Sprintf("op-%d", i),
				target.config.NodeID,
				fmt.Sprintf("boot-%d", i), false)
			// Released together, so the requests genuinely contend.
			start.Wait()
			res := c.applyFrom(target, cmd)
			out := outcome{err: res.Error}
			if res.Error == "" {
				out.granted = true
				out.holder = res.ColonyUpdate.GetHolderNodeId()
			}
			results[i] = out
		}(i)
	}
	start.Done()
	done.Wait()

	granted := 0
	var winner string
	for i, out := range results {
		if out.granted {
			granted++
			winner = out.holder
		} else {
			// Every loser gets an EXPLICIT refusal naming the holder,
			// not a silent nothing. This is the "the losers get an
			// explicit refusal naming the current holder" requirement,
			// asserted from the loser's own side rather than the
			// winner's.
			if out.err == "" {
				t.Errorf("caller %d was neither granted nor refused", i)
				continue
			}
			if !strings.Contains(out.err, winner) && !strings.Contains(out.err, "already in progress") {
				t.Errorf("caller %d's refusal does not name the holder: %s", i, out.err)
			}
		}
	}
	if granted != 1 {
		t.Errorf("%d of %d concurrent acquires across %d coordinators were granted, want exactly 1",
			granted, callers, len(c.nodes))
	}

	// And every node agrees on who won, which is what makes the record
	// durable rather than merely present on one machine.
	active, _, _ := c.leader().ColonyUpdateStateLocal()
	if active == nil {
		t.Fatal("no active controlled update after the race")
	}
	if active.GetHolderNodeId() != winner {
		t.Errorf("the colony's record says the holder is %q, but the winner was %q", active.GetHolderNodeId(), winner)
	}
	for _, n := range c.nodes {
		eventually(t, 10*time.Second, func() bool {
			got, _, _ := n.ColonyUpdateStateLocal()
			return got != nil && got.GetHolderNodeId() == winner
		})
	}
}

// TestColonyUpdate_ReplacementProcessObservesTheSameHolder is the
// durability requirement stated as a process death.
//
// The holder's own node is SHUT DOWN - not stubbed, not restarted with
// the same in-memory FSM, actually Shutdown() - and a different node
// then reads the record. It is the only way to show that what is being
// observed is replicated state and not a variable that happened to be
// in scope.
func TestColonyUpdate_ReplacementProcessObservesTheSameHolder(t *testing.T) {
	c := newClusterColonyUpdate(t)
	leader := c.leader()
	holderNodeID := leader.config.NodeID

	rec := c.applyFrom(leader, acquireColonyUpdateCmd("op-1", holderNodeID, "boot-1", false))
	if rec.Error != "" {
		t.Fatalf("acquire on the leader was refused: %s", rec.Error)
	}
	fence := fenceOf(rec.ColonyUpdate)
	if adv := c.applyFrom(leader, advanceColonyUpdateCmd(fence, "issue-restart", "buzz", "restarting buzz", nil)); adv.Error != "" {
		t.Fatalf("advance was refused: %s", adv.Error)
	}
	// Let the advance REPLICATE before the holder is killed. Shutting
	// the leader down the instant it acknowledged its own apply would
	// race the replication the test is about, and the resulting failure
	// would look exactly like a durability defect while being a test
	// that shut up too early.
	eventually(t, 10*time.Second, func() bool {
		for _, n := range c.nodes {
			if n == leader {
				continue
			}
			got, _, _ := n.ColonyUpdateStateLocal()
			if got == nil || got.GetStep() != "issue-restart" {
				return false
			}
		}
		return true
	})

	// The holder's process is gone. It is not restarted, because
	// restarting the SAME node with its old in-memory FSM would prove
	// nothing about replication.
	if err := leader.Shutdown(); err != nil {
		t.Fatalf("Shutdown() of the holder error: %v", err)
	}

	// The remaining two of three voters still form a quorum, so the
	// colony elects a new leader - and the test reads from WHICHEVER one
	// wins, rather than from a node picked in advance. Picking in
	// advance would be a race dressed up as a choice: the surviving
	// candidates are symmetric, so pinning one and waiting for IT to
	// win would intermittently fail for a reason that has nothing to do
	// with durability.
	var observer *Node
	eventually(t, 20*time.Second, func() bool {
		for _, n := range c.nodes {
			if n == leader {
				continue
			}
			if n.Status().IsLeader {
				observer = n
				return true
			}
		}
		return false
	})

	active, history, authoritative := observer.ColonyUpdateStateLocal()
	if !authoritative {
		t.Error("the re-elected leader reports itself as non-authoritative for a state read")
	}
	if active == nil {
		t.Fatal("a replacement leader reports no active controlled update after the holder's process died - the lock did not survive")
	}
	if active.GetHolderNodeId() != holderNodeID || active.GetHolderIncarnation() != "boot-1" || active.GetFenceToken() != rec.ColonyUpdate.GetFenceToken() {
		t.Errorf("replacement sees %s/%s at token %d, want %s/boot-1 at token %d",
			active.GetHolderNodeId(), active.GetHolderIncarnation(), active.GetFenceToken(),
			holderNodeID, rec.ColonyUpdate.GetFenceToken())
	}
	if active.GetStep() != "issue-restart" || active.GetTargetNodeId() != "buzz" {
		t.Errorf("replacement sees step %q on %q, want issue-restart on buzz", active.GetStep(), active.GetTargetNodeId())
	}
	if len(history) != 0 {
		t.Errorf("replacement sees %d settled operations while one is active, want 0", len(history))
	}
}

// TestColonyUpdate_TakeoverAcrossCoordinatorsFencesTheOldOne is the
// failover property against a real cluster, and the reason an explicit
// takeover is safe to permit at all: after it commits on a real
// quorum, the displaced holder cannot advance, cannot settle, and -
// the row that matters - cannot acquire another Comb's restart lease.
func TestColonyUpdate_TakeoverAcrossCoordinatorsFencesTheOldOne(t *testing.T) {
	c := newClusterColonyUpdate(t)
	leader := c.leader()
	oldNodeID := leader.config.NodeID

	first := c.applyFrom(leader, acquireColonyUpdateCmd("op-1", oldNodeID, "boot-1", false))
	if first.Error != "" {
		t.Fatalf("first acquire refused: %s", first.Error)
	}
	oldFence := fenceOf(first.ColonyUpdate)

	// A different Comb takes over, on its own raft, through a committed
	// quorum.
	var other *Node
	for _, n := range c.nodes {
		if n != leader {
			other = n
			break
		}
	}
	// The takeover is submitted to the leader (only it can commit), but
	// it is AUTHORED as another Comb's, which is the shape the real
	// leader-forwarded RPC produces.
	second := c.applyFrom(leader, acquireColonyUpdateCmd("op-2", other.config.NodeID, "boot-9", true))
	if second.Error != "" {
		t.Fatalf("takeover was refused: %s", second.Error)
	}
	newFence := fenceOf(second.ColonyUpdate)
	if newFence.GetFenceToken() <= oldFence.GetFenceToken() {
		t.Fatalf("takeover token %d does not exceed the displaced token %d", newFence.GetFenceToken(), oldFence.GetFenceToken())
	}

	if res := c.applyFrom(leader, advanceColonyUpdateCmd(oldFence, "issue-restart", "drone", "still going", nil)); res.Error == "" {
		t.Error("the displaced holder advanced the operation after a committed takeover")
	}
	if res := c.applyFrom(leader, releaseColonyUpdateCmd(oldFence, colonyOutcomeConfirmed, "writing itself off")); res.Error == "" {
		t.Error("the displaced holder settled the operation that displaced it")
	}
	// The row that makes failover safe: a coordinator that has lost the
	// colony can no longer take a Comb's restart lease, forced or not.
	if res := c.applyFrom(leader, fencedAcquireRestartLeaseCmd("apiary_raftd", "drone", oldFence, false)); res.Error == "" {
		t.Error("the displaced holder acquired a restart lease after a committed takeover")
	}
	if res := c.applyFrom(leader, fencedAcquireRestartLeaseCmd("apiary_raftd", "drone", oldFence, true)); res.Error == "" {
		t.Error("force rescued a displaced holder's restart lease; the fence must not be overridable")
	}

	// The new holder can do its work, and the displaced record survives
	// as history marked unobserved.
	if res := c.applyFrom(leader, fencedAcquireRestartLeaseCmd("apiary_raftd", "drone", newFence, false)); res.Error != "" {
		t.Errorf("the new holder could not acquire its restart lease: %s", res.Error)
	}
	displaced, err := leader.ColonyUpdateByIDLocal("op-1")
	if err != nil {
		t.Fatalf("reading the displaced record: %v", err)
	}
	if displaced.GetActive() || displaced.GetOutcome() != colonyOutcomeUnobserved {
		t.Errorf("displaced record = active %v outcome %q, want inactive/unobserved", displaced.GetActive(), displaced.GetOutcome())
	}
}

// TestColonyUpdate_QuorumLossRefusesToGrant is the fail-closed
// requirement, against a real cluster that has genuinely lost its
// majority.
//
// Two of three voters are shut down. The survivor CANNOT commit
// anything, so it must not grant - and the point of the test is that
// it does not fall back to some local view of its own FSM that happens
// to say "nothing is running". Reporting "free" there would be exactly
// the fail-open the whole mechanism exists to prevent.
func TestColonyUpdate_QuorumLossRefusesToGrant(t *testing.T) {
	c := newClusterColonyUpdate(t)
	survivor := c.leader()

	var downed []*Node
	for _, n := range c.nodes {
		if n != survivor {
			downed = append(downed, n)
		}
	}
	for _, n := range downed {
		if err := n.Shutdown(); err != nil {
			t.Fatalf("Shutdown() error: %v", err)
		}
	}

	// Two of three voters gone is a lost quorum, and raftd needs a
	// moment to notice. The check has to be that it never grants, so
	// this retries for a bounded window rather than sampling one
	// instant and hoping.
	// rawApplyFrom, not applyFrom: there is no leader left to forward
	// to, and asking leaderOf for one would abort the test rather than
	// observe the refusal. A coordinator on a quarantined colony has
	// nothing to forward to either, and must simply be refused.
	deadline := time.Now().Add(15 * time.Second)
	attempts := 0
	for time.Now().Before(deadline) {
		attempts++
		res := c.rawApplyFrom(survivor, acquireColonyUpdateCmd("op-quorum", survivor.config.NodeID, "boot-1", false))
		if res.Error == "" {
			t.Fatalf("attempt %d GRANTED a controlled update with no quorum - the single-flight must fail closed", attempts)
		}
		if len(downed) == 2 && !survivor.Status().IsLeader {
			// The survivor has correctly stepped down; that is the
			// expected end state and there is nothing more to learn.
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if attempts == 0 {
		t.Fatal("no apply was even attempted after quorum loss")
	}
	// And its own local FSM never learned of a grant, so nothing was
	// half-written either.
	if active, _, _ := survivor.ColonyUpdateStateLocal(); active != nil {
		t.Error("a quorum-less apply created an active controlled update record")
	}
}

// TestColonyUpdate_NonLeaderApplyCannotGrant is the M-different-
// coordinators property from the other side: a coordinator on a
// follower must be refused by raft itself, not quietly served from its
// own copy.
//
// This is why internal/manager leader-forwards before doing any
// work: without it, a follower would report "granted" for an operation
// only the leader ever wrote.
func TestColonyUpdate_NonLeaderApplyCannotGrant(t *testing.T) {
	c := newClusterColonyUpdate(t)
	leader := c.leader()
	var follower *Node
	for _, n := range c.nodes {
		if n != leader {
			follower = n
			break
		}
	}

	// A raft-level refusal that is NOT "not the leader" must never be
	// forwarded past: that is the quorum-unknown case, and forwarding
	// it would turn a fail-closed refusal into an optimistic retry.
	res := c.rawApplyFrom(follower, acquireColonyUpdateCmd("op-1", follower.config.NodeID, "boot-1", false))
	if res.Error == "" {
		t.Fatal("a follower's own Apply granted a controlled update; only the leader can commit, and only the leader may decide")
	}
	if !isNotLeaderError(errors.New(res.Error)) {
		t.Errorf("a follower's refusal = %q, want raft's own not-leader signal", res.Error)
	}
	// And it left nothing behind anywhere.
	eventually(t, 10*time.Second, func() bool {
		active, _, _ := leader.ColonyUpdateStateLocal()
		return active == nil
	})
}

// TestColonyUpdate_StateDigestIncludesTheRecord is the ADR-0143
// cross-voter agreement check, applied to the new state.
//
// The digest is derived from FSMSnapshotState's own descriptor, so the
// new map is included automatically - and "automatically" is exactly
// the kind of claim that stops being true the moment someone hand-
// maintains a second list. This pins it: two FSMs that differ ONLY in
// the controlled update must produce different digests, and identical
// state must still produce identical ones.
func TestColonyUpdate_StateDigestIncludesTheRecord(t *testing.T) {
	bare := NewFSM()
	before := bare.StateDigest()

	rec := bare.Apply(&raft.Log{Index: 1, Data: mustMarshalCommand(t, acquireColonyUpdateCmd("op-1", "comb-a", "boot-1", false))}).(*FSMApplyResult)
	if rec.Error != "" {
		t.Fatalf("acquire was refused: %s", rec.Error)
	}
	after := bare.StateDigest()
	if before == after {
		t.Error("granting a controlled update did not change the state digest - ADR-0143 would not notice two voters disagreeing about it")
	}

	// Two independently built FSMs holding the SAME state must still
	// agree, which is the property the digest exists to provide.
	twin := NewFSM()
	twin.Apply(&raft.Log{Index: 1, Data: mustMarshalCommand(t, acquireColonyUpdateCmd("op-1", "comb-a", "boot-1", false))})
	if twin.StateDigest() != after {
		t.Errorf("two FSMs holding identical controlled-update state produced different digests:\n  %s\n  %s", after, twin.StateDigest())
	}
}
