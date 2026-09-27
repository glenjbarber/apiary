package raft

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/hashicorp/raft"
)

// DefaultStepAsideTimeout bounds a whole step-aside when the caller does
// not name one. It is deliberately generous relative to raft's own 1s
// heartbeat and 1s election timeouts: the operation should only ever be
// waiting on a transfer that is already under way, and a caller that is
// about to kill this process wants an answer well before it gives up
// waiting rather than after.
const DefaultStepAsideTimeout = 15 * time.Second

// stepAsidePoll is how often the confirmation is re-read. The window we
// are waiting through is measured in milliseconds once the transfer has
// been initiated, so polling finely costs nothing and shortens the answer.
const stepAsidePoll = 25 * time.Millisecond

// ErrStepAsideIncomplete means the node was leading and leadership did
// not reach a different voter before the deadline. It is deliberately
// distinct from a transfer that was never needed: the caller must treat
// this one as "do not restart yet", not as "retry".
var ErrStepAsideIncomplete = errors.New("raft: leadership did not reach another voter before the step-aside deadline")

// StepAsideResult is what a step-aside attempt actually did.
//
// The distinction that matters to a caller is SafeToRestart. Everything
// else is evidence for a human reading the detail, and none of it is a
// licence on its own: a result that reports WasLeader with
// SafeToRestart false is a node that is still leading and must be left
// running.
type StepAsideResult struct {
	// WasLeader is whether this node held leadership when asked. It is
	// false for the common case, three steps out of four in a sweep.
	WasLeader bool

	// Transferred is whether a transfer was actually initiated and
	// reported success by the raft library.
	Transferred bool

	// NewLeaderID is the voter leadership landed on, empty when no
	// transfer happened.
	NewLeaderID string

	// SafeToRestart is the answer the caller is after. It is true only
	// when this node is not the leader, either because it never was or
	// because leadership demonstrably moved elsewhere first.
	SafeToRestart bool

	// Detail is operator-readable prose carrying the evidence. It is
	// never empty, on any path, including failures.
	Detail string
}

// StepAsideForRestart makes it safe to restart this node's services by
// ensuring it is not the raft leader, and reports whether it is.
//
// It is the answer to ADR-0142's missing preflight half and the mechanism
// ADR-0145 is built on. The property it guarantees is deliberately
// stronger than "the leader is updated last": the node that is the leader
// at the moment of its own step steps down first. A plan computed at the
// start of a sweep goes stale the moment leadership moves, and during the
// 2026-09-27 sweep it did - term advanced to 30 mid-restart.
//
// Three properties this deliberately has:
//
//   - It is a no-op when this node is not the leader. The caller learns
//     nothing it had to already know, and pays a single state read.
//   - It confirms rather than assumes. The raft library's future reports
//     that the transfer routine finished; that is not the same claim as
//     "another voter is now leading", so this re-reads the actual state
//     and requires a different, non-empty leader before answering that a
//     restart is safe.
//   - It never reports success on a transfer it could not verify. A node
//     that was leading and could not hand over is told so, and stays up.
func (n *Node) StepAsideForRestart(ctx context.Context, timeout time.Duration) (StepAsideResult, error) {
	if timeout <= 0 {
		timeout = DefaultStepAsideTimeout
	}
	self := n.config.NodeID

	// The common case, and the one that must stay cheap. Reading one
	// state and returning is the whole cost for a follower.
	if n.raft.State() != raft.Leader {
		return StepAsideResult{
			SafeToRestart: true,
			Detail: fmt.Sprintf(
				"%s is not the raft leader, so there is no leadership to move; nothing on this node was changed",
				self),
		}, nil
	}

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// LeadershipTransfer() with no target deliberately lets the library
	// choose the transferee: a caller that had to nominate one would be
	// back to needing to know the cluster's membership, which is the
	// knowledge this design exists to remove from the operator. The
	// library picks a peer, waits for it to catch up, and sends it a
	// TimeoutNow that makes it campaign immediately.
	if err := n.awaitTransfer(ctx); err != nil {
		return StepAsideResult{
			WasLeader:   true,
			Transferred: false,
			Detail: fmt.Sprintf(
				"%s is the raft leader and the leadership transfer did not complete (%v); "+
					"this node is still leading, so it is NOT safe to restart yet",
				self, err),
		}, err
	}

	newLeader, err := n.awaitAnotherLeader(ctx, self)
	if err != nil {
		return StepAsideResult{
			WasLeader:   true,
			Transferred: true,
			Detail: fmt.Sprintf(
				"%s handed over leadership but no other voter was observed leading before the deadline (%v); "+
					"it is NOT safe to restart yet",
				self, err),
		}, err
	}

	return StepAsideResult{
		WasLeader:     true,
		Transferred:   true,
		NewLeaderID:   newLeader,
		SafeToRestart: true,
		Detail: fmt.Sprintf(
			"%s was the raft leader; leadership has moved to %s and this node is a follower, "+
				"so it is now safe to restart", self, newLeader),
	}, nil
}

// awaitTransfer runs the raft leadership-transfer future without letting
// it outlive ctx. The future is itself bounded by the library's election
// timeout, but this must not inherit that bound implicitly: the caller's
// deadline is the one that counts.
func (n *Node) awaitTransfer(ctx context.Context) error {
	done := make(chan error, 1)
	go func() { done <- n.raft.LeadershipTransfer().Error() }()

	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// awaitAnotherLeader re-reads the real raft state until this node is no
// longer the leader AND some other, named voter is. Both halves matter:
// a node that has merely stopped being the leader while the cluster has
// no leader at all is not a node it is safe to kill, because the
// remaining voters have not yet proven they can elect.
func (n *Node) awaitAnotherLeader(ctx context.Context, self string) (string, error) {
	ticker := time.NewTicker(stepAsidePoll)
	defer ticker.Stop()

	for {
		if _, leaderID := n.raft.LeaderWithID(); leaderID != "" && string(leaderID) != self && n.raft.State() != raft.Leader {
			return string(leaderID), nil
		}
		select {
		case <-ctx.Done():
			return "", fmt.Errorf("%w: state=%v", ErrStepAsideIncomplete, n.raft.State())
		case <-ticker.C:
		}
	}
}
