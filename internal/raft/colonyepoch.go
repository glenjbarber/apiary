// ADR-0156 Stage 2: the Colony's epoch - the monotonic generation
// counter that distinguishes two generations of the same Colony, the
// rules for incrementing it, and the refusal of a stale-epoch node.
//
// WHY A SECOND ANCHOR ALONGSIDE THE COLONY ID: a Colony ID answers
// "which Colony is this?" and it deliberately never changes. That is
// exactly what makes it useless for the question "which generation of
// this Colony is this?". A Colony that is wiped, reset and rebuilt has
// the same identity question and a different answer, and a node that
// comes back from a partition, or from a restored backup taken before
// the rebuild, holds an identity that MATCHES and a state that does
// not. Stage 1's identity check cannot see that difference, because
// the identity is invariant across precisely the case that needs
// detecting.
//
// WHY THE EPOCH IS REPLICATED RAFT STATE RATHER THAN A PER-COMB CONFIG
// VALUE: the same reasoning that put ColonyJoinWindow (ADR-0147) and
// ColonyDiskSize (ADR-0148) in replicated state, and the same
// reasoning internal/raft/colonydisksize.go opens with at length. Four
// Combs each holding an epoch of 1 are four independent numbers that
// happen to agree at one moment; the moment one is restored from an
// older snapshot, or joins from a config saying 3, the Colony has two
// epochs and nothing says so. The refusal of a stale-epoch node would
// then be a comparison of two unrelated numbers. Replicated through
// the log, every voter derives the SAME epoch from the same bytes, so
// "is this node behind?" is a decidable question rather than a
// coincidence.
//
// WHY THE INCREMENT RULES LIVE HERE RATHER THAN IN A HANDLER: a
// handler check is advisory. It runs on one process, before the
// command enters the log, and protects only the path that went through
// it. The same command can arrive from a peer-forwarded write, from a
// raftd invoked directly, or from a future client that was not in the
// handler's mind when the check was written. The apply function is the
// only place that runs, in the same order, on every replica: it sees
// the committed log AND the state it is about to change. That is the
// same posture applySetColonyIdentity and applySetColonyDiskSize take
// at the FSM boundary, and it is the whole reason the refusal of a
// stale-epoch node at admission is worth anything.
//
// WHY THE EPOCH IS A COUNTER AND NOT A CLOCK AND NOT A HASH:
// ADR-0156 is explicit that HashiCorp Raft's own term cannot serve
// this purpose - Raft's term is per-cluster, not per-Colony, and two
// independently bootstrapped clusters both start at term 1 and both
// increment from there, so their terms are unrelated numbers that only
// LOOK comparable. Treating them as a shared sequence is exactly the
// comparison that falsely concludes "same epoch, no conflict" in the
// split-leader case. A hash of the state is stable across nodes but is
// not monotonic in any meaningful sense, and would not order a genuine
// reset against an ordinary restore.
//
// WHY A GAP IS REFUSED RATHER THAN ACCEPTED: the epoch advances by
// exactly one, and a jump of two or more is refused. A jump would
// make the log lie: an epoch of 7 with no record of 2 through 6 means
// some of those transitions happened off the log, and a node later
// found holding epoch 5 could not be told from one holding 4. The
// sequence in the log IS the sequence of generations, and a hole in it
// destroys that property.

package raft

import (
	"fmt"

	"google.golang.org/protobuf/proto"

	internalpb "github.com/glenjbarber/apiary/api/internalpb"
)

// initialColonyEpoch is the epoch a Colony starts at.
//
// ADR-0156's Decision 3: there is nothing in the existing state from
// which a generation count could be recovered, so 1 is the only
// consistent starting value for an existing Colony. It is not derived
// from a hash of the state, because a hash is stable across nodes but
// is not monotonic in any meaningful sense, and would not order a
// genuine reset against an ordinary restore.
const initialColonyEpoch uint64 = 1

// applySetColonyEpoch establishes this Colony's epoch, and is the only
// way an epoch is ever established.
//
// A repeated SetColonyEpoch is NOT idempotent-and-successful. It is
// refused, and the refusal names both the epoch already in state and
// the one proposed, so that a node trying to re-assert an epoch learns
// that it lost the race instead of believing it succeeded.
//
// Two refusals, both by name:
//
//   - epoch is zero. Not treated as "unset": there is exactly one way
//     to express "no epoch recorded" in this design, and it is the
//     absence of any SetColonyEpoch entry in the log. An epoch of 0
//     would be a second way to say the same thing, and the two could
//     disagree.
//   - epoch is not 1. ADR-0156's Decision 3 fixes the initial epoch of
//     an existing Colony at 1, because there is nothing in the
//     existing state from which a generation count could be recovered.
//     Establishing an epoch of 7 on a fresh Colony would leave the
//     log claiming seven generations that never happened.
func (f *FSM) applySetColonyEpoch(index uint64, req *internalpb.SetColonyEpoch) *FSMApplyResult {
	if req.GetEpoch() == 0 {
		return &FSMApplyResult{Index: index, Error: fmt.Sprintf(
			"SetColonyEpoch: refusing an epoch of 0; there is exactly one way to say \"no Colony epoch recorded\" and it is the absence of any SetColonyEpoch in this Colony's log, so 0 is not an unset marker")}
	}
	if req.GetEpoch() != initialColonyEpoch {
		return &FSMApplyResult{Index: index, Error: fmt.Sprintf(
			"SetColonyEpoch: refusing to establish epoch %d because a Colony's epoch is initialised to %d (ADR-0156's Decision 3: there is nothing in the existing state from which a generation count could be recovered, so 1 is the only consistent starting value); use AdvanceColonyEpoch to move past it",
			req.GetEpoch(), initialColonyEpoch)}
	}

	// The caller must hold f.mu. Every apply function in this FSM is
	// called from Apply, which takes it once for the whole dispatch,
	// so no re-locking happens here - see the f.mu doc comment.
	current := f.colonyEpoch
	if current.GetEpoch() != 0 {
		return &FSMApplyResult{Index: index, Error: fmt.Sprintf(
			"SetColonyEpoch: refusing to re-establish this Colony's epoch at %d because it is already at %d, and an epoch is set exactly once and only ever advances from there; a Colony that wants a different generation is advancing, not re-establishing",
			req.GetEpoch(), current.GetEpoch())}
	}

	f.colonyEpoch = &internalpb.ColonyEpoch{
		Epoch:         req.GetEpoch(),
		PreviousEpoch: 0,
		SetBy:         req.GetSetBy(),
		SetAtUnix:     req.GetSetAtUnix(),
	}
	return &FSMApplyResult{Index: index, ColonyEpoch: cloneColonyEpoch(f.colonyEpoch)}
}

// applyAdvanceColonyEpoch moves this Colony to the next generation,
// and is the only way the epoch ever changes after it is established.
//
// Three refusals, all by name and all naming the two epochs involved:
//
//   - this Colony has no epoch yet. "Advance from nothing" has no
//     defined answer, and allowing it would let the first advance
//     establish an arbitrary starting point - which is exactly what
//     SetColonyEpoch exists to prevent.
//   - the requested epoch is not the current epoch plus one. A repeat
//     of the current epoch is refused as well as a jump, because a
//     no-op apply would enter the log and rewrite set_by/set_at_unix,
//     recording a generation change that did not happen.
//   - a jump of two or more is refused, for the reason the package
//     comment gives: a hole in the sequence would make the log lie
//     about which generations ever existed.
//
// There is deliberately NO command that lowers the epoch. The epoch is
// a monotonic counter, and the only operation on it is "advance by
// one".
func (f *FSM) applyAdvanceColonyEpoch(index uint64, req *internalpb.AdvanceColonyEpoch) *FSMApplyResult {
	// The caller must hold f.mu. See the f.mu doc comment.
	current := f.colonyEpoch
	if current.GetEpoch() == 0 {
		return &FSMApplyResult{Index: index, Error: fmt.Sprintf(
			"AdvanceColonyEpoch: refusing to advance to epoch %d because this Colony has no epoch recorded yet; \"advance from nothing\" has no defined answer, and the epoch must be established by SetColonyEpoch first",
			req.GetEpoch())}
	}
	if req.GetEpoch() != current.GetEpoch()+1 {
		return &FSMApplyResult{Index: index, Error: fmt.Sprintf(
			"AdvanceColonyEpoch: refusing to advance this Colony's epoch from %d to %d; an epoch advances by exactly one, so the only valid target is %d. A jump leaves a hole in the sequence, and the sequence in this log IS the sequence of generations that existed",
			current.GetEpoch(), req.GetEpoch(), current.GetEpoch()+1)}
	}

	f.colonyEpoch = &internalpb.ColonyEpoch{
		Epoch:         req.GetEpoch(),
		PreviousEpoch: current.GetEpoch(),
		SetBy:         req.GetSetBy(),
		SetAtUnix:     req.GetSetAtUnix(),
	}
	return &FSMApplyResult{Index: index, ColonyEpoch: cloneColonyEpoch(f.colonyEpoch)}
}

// ColonyEpochState returns a copy of the Colony's epoch record, or nil
// if this Colony has never had one set. A copy, because the callers
// here are on the request path rather than the apply path, and every
// other FSM accessor here hands out a clone for the same reason.
func (f *FSM) ColonyEpochState() *internalpb.ColonyEpoch {
	f.mu.Lock()
	defer f.mu.Unlock()
	return cloneColonyEpoch(f.colonyEpoch)
}

// ColonyEpoch returns this Colony's current epoch, or 0 if it has never
// had one set.
//
// The zero value is a REAL state, not an error: it means "this node
// has not applied a SetColonyEpoch", which is true of every node built
// before ADR-0156 Stage 2 and of a node that has just been restored
// from a pre-Stage-2 snapshot.
//
// A caller that needs to tell "no epoch" apart from "this node is not
// reachable" has to ask a different question for each; collapsing them
// here would hide the very disagreement this value exists to expose.
func (f *FSM) ColonyEpoch() uint64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.colonyEpoch.GetEpoch()
}

// cloneColonyEpoch deep-copies through proto.Clone rather than by
// value, for the reason cloneColonyDiskSize's own comment gives: a
// generated message carries a sync.Mutex in its MessageState, and
// `c := *s` copies it.
func cloneColonyEpoch(s *internalpb.ColonyEpoch) *internalpb.ColonyEpoch {
	if s == nil {
		return nil
	}
	return proto.Clone(s).(*internalpb.ColonyEpoch)
}
