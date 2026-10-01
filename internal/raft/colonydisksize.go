// ADR-0148: the Colony-wide VM disk-size floor - the state, the one
// command that changes it, and the two places a per-VM override is
// checked against it.
//
// Why this is replicated raft state rather than a per-Comb config
// value: "throughout the colony" wants ONE value every Comb reads,
// and "can never be reduced" wants a guarantee rather than a
// convention. Four Combs each holding 20480 is four independent
// numbers that happen to agree at one moment; the moment one is
// edited, is restored from an older managerd.json, or joins the
// Colony from a config saying 40960, the Colony has two defaults and
// nothing says so. The reconciler on each Comb would then size a
// disk from its own local file, so the SAME VMDefinition in the same
// log would produce DIFFERENT disk sizes depending on which Comb
// owned the VM - the one thing this architecture forbids outright.
// AcquireRestartLease, ColonyJoinWindow and AcquireColonyUpdate are
// raft state for the same stated reason.
//
// Why the monotonicity comparison lives HERE rather than in a
// managerd handler: a handler check is advisory. It runs on one
// process, before the command enters the log, and protects only the
// path that went through it. The same command can arrive from a
// peer-forwarded write, from a raftd invoked directly, or from a
// future client that was not in the handler's mind when the check
// was written - and a follower applying that log without the check
// would hold a lower floor than the leader, so the two would size
// different disks for the same VMDefinition. The apply function is
// the only place that runs, in the same order, on every replica: it
// sees the committed log AND the state it is about to change, so "is
// this below what we already have" is a question only it can answer
// without racing. That is the same posture applyPinTrustedPeer takes
// at the FSM boundary, and it is why the state digest (ADR-0143)
// covers the floor: the value is inside FSMSnapshotState, so it is
// inside the digest.
//
// Why REFUSAL rather than clamping: a clamp silently overrides
// operator intent, and there is no way for the operator to tell a
// clamped success from a real one. Worse it makes the log lie - the
// command says floor_mb = 1024, the state says 20480, and every
// future reader of that entry has to know which happened. Apiary
// already refuses rather than clamps for exactly this reason, in
// applyOpenColonyJoinWindow (a window over the configured ceiling)
// and applyPinTrustedPeer (a pin claiming voter membership).
//
// Nothing here RESIZES. The floor is a minimum at creation: both
// disk paths size a freshly created sparse file and return early when
// it already exists, so a raised floor never touches a disk that is
// already there.

package raft

import (
	"fmt"

	"google.golang.org/protobuf/proto"

	internalpb "github.com/glenjbarber/apiary/api/internalpb"
)

// applySetColonyDiskSize raises the Colony-wide floor, and is the only
// way it is ever changed. Two refusals, both by name and both naming
// the two numbers involved:
//
//   - floor_mb is zero. Not treated as "unset": there is exactly one
//     way to express "no floor recorded" in this design, and it is the
//     absence of any SetColonyDiskSize entry in the log. A zero floor
//     would be a second way to say the same thing, and the two could
//     disagree.
//   - floor_mb is at or below the floor already in state. "At" is
//     refused as well as "below" on purpose: a no-op apply would enter
//     the log and rewrite set_by/set_at_unix, recording a raise that
//     did not happen and moving the audit trail's timestamp forward
//     for nothing.
//
// A repeated raise is not a special case. It is the same command with
// a bigger number, and it is what makes the floor climbable at all.
func (f *FSM) applySetColonyDiskSize(index uint64, req *internalpb.SetColonyDiskSize) *FSMApplyResult {
	if req.GetFloorMb() == 0 {
		return &FSMApplyResult{Index: index, Error: fmt.Sprintf(
			"SetColonyDiskSize: refusing a floor of 0 MB; there is exactly one way to say \"no floor recorded\" and it is the absence of any SetColonyDiskSize in this Colony's log, so 0 is not an unset marker")}
	}

	// The caller must hold f.mu. Every apply function in this FSM is
	// called from Apply, which takes it once for the whole dispatch,
	// so no re-locking happens here - see the f.mu doc comment.
	current := f.colonyDiskSize
	if current.GetFloorMb() >= req.GetFloorMb() {
		return &FSMApplyResult{Index: index, Error: fmt.Sprintf(
			"SetColonyDiskSize: refusing to set the Colony disk-size floor to %d MB because this Colony's floor is already %d MB, and the floor can only be raised; there is no way to lower it and nothing that resizes an existing disk, so a lower floor would only apply to disks created from here on",
			req.GetFloorMb(), current.GetFloorMb())}
	}

	f.colonyDiskSize = &internalpb.ColonyDiskSize{
		FloorMb:   req.GetFloorMb(),
		SetBy:     req.GetSetBy(),
		SetByKey:  req.GetSetByKey(),
		SetAtUnix: req.GetNowUnix(),
	}
	return &FSMApplyResult{Index: index, ColonyDiskSize: cloneColonyDiskSize(f.colonyDiskSize)}
}

// checkPerVMDiskSize refuses a per-VM disk_size_mb below the Colony
// floor, and is the enforcement of "a floor is a floor, not a
// recommendation".
//
// carried is the value the record already holds, or nil where there
// is no record yet (CreateVM). The UpdateVM case is deliberately
// narrow: UpdateVM is a FULL-RECORD REPLACE, so a VM that already
// records a disk_size_mb below a LATER floor would be refused on any
// later update for a reason that has nothing to do with what was
// being updated - which would make the record un-updatable. That is a
// bug this codebase has form about: SetVMFirewallPaused exists
// (ADR-0049) precisely because UpdateVM's full-replace semantics make
// it unsafe for a narrow change. So a value that is unchanged is not
// a new request and is not refused.
//
// The stored value is deliberately NOT rewritten when the floor rises
// past it either. The record says what was asked for; the reconciler
// resolves the effective size. Rewriting it would destroy the
// operator's original intent behind a number nobody can then explain.
func (f *FSM) checkPerVMDiskSize(kind, id string, requested, carried uint64) *FSMApplyResult {
	if requested == 0 {
		// Zero means "no override": this VM gets the Colony floor.
		// Every optional field on VMDefinition means this, and it is
		// what keeps a VM created before ADR-0148 working untouched.
		return nil
	}
	if carried != 0 && requested == carried {
		// Not a new request: UpdateVM is carrying an existing value
		// forward, and the floor has since risen past it. Refusing
		// would make the record un-updatable for an unrelated reason.
		// Checked BEFORE the floor comparison deliberately.
		return nil
	}
	if requested < f.colonyDiskSize.GetFloorMb() {
		return &FSMApplyResult{Error: fmt.Sprintf(
			"%s: VM %q names a disk_size_mb of %d MB, which is below this Colony's disk-size floor of %d MB (ADR-0148); a per-VM size may only go above the floor, never below it, so set disk_size_mb to at least %d, or leave it at 0 to take the floor itself",
			kind, id, requested, f.colonyDiskSize.GetFloorMb(), f.colonyDiskSize.GetFloorMb())}
	}
	return nil
}

// ColonyDiskSizeState returns a copy of the current floor, or nil if
// this Colony has never had one set. A copy, because the callers here
// are on the reconciler's request path rather than the apply path, and
// every other FSM accessor here hands out a clone for the same reason.
func (f *FSM) ColonyDiskSizeState() *internalpb.ColonyDiskSize {
	f.mu.Lock()
	defer f.mu.Unlock()
	return cloneColonyDiskSize(f.colonyDiskSize)
}

// ColonyDiskSizeFloorMB returns the Colony floor in MiB, or 0 when
// none has ever been set. A convenience for the many callers that
// want the number and not the audit trail - notably the reconciler,
// which resolves a maximum rather than rendering who set it.
func (f *FSM) ColonyDiskSizeFloorMB() uint64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.colonyDiskSize.GetFloorMb()
}

// cloneColonyDiskSize deep-copies through proto.Clone rather than by
// value, for the reason cloneColonyJoinWindow's own comment gives: a
// generated message carries a sync.Mutex in its MessageState, and
// `c := *s` copies it.
func cloneColonyDiskSize(s *internalpb.ColonyDiskSize) *internalpb.ColonyDiskSize {
	if s == nil {
		return nil
	}
	return proto.Clone(s).(*internalpb.ColonyDiskSize)
}
