// ADR-0147 Part 4's Colony-wide join window: the state, its two
// command arms, and the one predicate everything else in the feature
// asks about - "is a window live right now".
//
// Why this is replicated at all, when the obvious implementation is a
// bool in managerd: an election. A window held in one managerd's memory
// is a window that silently disappears when that Comb steps aside, and
// reappears when it comes back - which would make "is this Colony
// accepting members" a fact that changes with leadership for no reason
// an operator did anything. The same reasoning as the restart lease
// (ADR-0103) and the controlled-update single-flight (ADR-0145): the
// guarantee has to survive the process that is holding it.
//
// Why there is exactly ONE of these per Colony rather than one per
// member: two members disagreeing about whether the Colony is open to
// new Combs is not a state this codebase wants to be able to represent.

package raft

import (
	"fmt"

	"google.golang.org/protobuf/proto"

	internalpb "github.com/glenjbarber/apiary/api/internalpb"
)

// ColonyJoinWindowLive reports whether w is a window an operator could
// still act on at now. It is a function of now and never of when the
// state was written, which is the whole point of expires_at_unix being
// absolute: a lapsed window is not swept by any background task, it
// simply stops being live the moment its deadline passes, on every
// Comb, without anything having to run.
//
// A nil or zero window is not live, which is what makes "this Colony
// has never had a window opened" and "this Colony's window closed" the
// same case for every caller - the normal state of a Colony is closed.
func ColonyJoinWindowLive(w *internalpb.ColonyJoinWindow, now int64) bool {
	if w == nil || !w.GetEnabled() {
		return false
	}
	return now < w.GetExpiresAtUnix()
}

// applyOpenColonyJoinWindow opens the window, and is the only way an
// existing window is ever replaced.
//
// Three refusals, each a specific way a window becomes a permanent
// capability rather than a bounded act:
//
//   - A live window already exists. There is deliberately no "or
//     longer" case: extending is the operation this whole window
//     exists to prevent, and the alternative - silently lengthening -
//     is what would let one deliberate act become an indefinite one.
//     An operator who wants a different length closes this one and
//     opens another, which is two visible acts in the log.
//   - The requested duration is not positive. A zero or negative
//     window is a window that is closed the instant it is opened,
//     which reads in the UI as a failure to open rather than as the
//     request it was.
//   - The requested duration exceeds the caller's configured ceiling.
//     Refused by name rather than clamped, because clamping is how an
//     RPC becomes a way around configured policy, and because an
//     operator who asked for twenty minutes and silently got ten has
//     no way to tell the difference from a Colony that opened fine.
//
// A LAPSED window is not refused. Opening over one is the normal
// reopen, and it is what changes opened_at_unix - the epoch every
// pending request records - so everything created under the previous
// window stops being approvable.
func (f *FSM) applyOpenColonyJoinWindow(index uint64, req *internalpb.OpenColonyJoinWindow) *FSMApplyResult {
	if req.GetMaxDurationSeconds() <= 0 {
		return &FSMApplyResult{Index: index, Error: fmt.Sprintf(
			"OpenColonyJoinWindow: refusing to open a window with no configured ceiling (max_duration_seconds is %d); the leader's colony_join_window_seconds is what bounds this, and a window with no bound is not a bounded window",
			req.GetMaxDurationSeconds())}
	}
	if req.GetDurationSeconds() <= 0 {
		return &FSMApplyResult{Index: index, Error: fmt.Sprintf(
			"OpenColonyJoinWindow: duration_seconds must be positive, got %d", req.GetDurationSeconds())}
	}
	if req.GetDurationSeconds() > req.GetMaxDurationSeconds() {
		return &FSMApplyResult{Index: index, Error: fmt.Sprintf(
			"OpenColonyJoinWindow: refusing a %d-second window because this Colony's configured ceiling is %d seconds (colony_join_window_seconds in managerd.json on the leader); close the current window and open a new one if that is what you want",
			req.GetDurationSeconds(), req.GetMaxDurationSeconds())}
	}

	if live := f.colonyJoinWindowLocked(); ColonyJoinWindowLive(live, req.GetNowUnix()) {
		return &FSMApplyResult{Index: index, Error: fmt.Sprintf(
			"OpenColonyJoinWindow: a join window is already open on this Colony until %d (%d seconds from now); close it first if you want a different one, because extending a window in place is exactly what a join window must not do",
			live.GetExpiresAtUnix(), live.GetExpiresAtUnix()-req.GetNowUnix())}
	}

	f.colonyJoinWindow = &internalpb.ColonyJoinWindow{
		Enabled:       true,
		OpenedBy:      req.GetOpenedBy(),
		OpenedByKey:   req.GetOpenedByKey(),
		OpenedAtUnix:  req.GetNowUnix(),
		ExpiresAtUnix: req.GetNowUnix() + req.GetDurationSeconds(),
	}
	return &FSMApplyResult{Index: index, ColonyJoinWindow: cloneColonyJoinWindow(f.colonyJoinWindow)}
}

// applyCloseColonyJoinWindow closes the window early, because an
// operator who changes their mind should not have to wait it out.
//
// It converges on exactly the state an expiry produces - enabled false,
// deadline reached - so there is no third "closed early" shape any
// caller has to learn. The only difference is who asked and what the
// log records, and closing a window that is not open is not an error:
// it is a no-op, which keeps the UI's "close now" button safe to press
// on a window that expired a second before the operator clicked it.
func (f *FSM) applyCloseColonyJoinWindow(index uint64, req *internalpb.CloseColonyJoinWindow) *FSMApplyResult {
	if f.colonyJoinWindow == nil || !f.colonyJoinWindow.GetEnabled() {
		return &FSMApplyResult{Index: index, ColonyJoinWindow: nil}
	}
	f.colonyJoinWindow.Enabled = false
	f.colonyJoinWindow.ExpiresAtUnix = req.GetNowUnix()
	return &FSMApplyResult{Index: index, ColonyJoinWindow: cloneColonyJoinWindow(f.colonyJoinWindow)}
}

// colonyJoinWindowLocked returns the FSM's current window, or nil when
// none has ever been opened. The caller must hold f.mu.
func (f *FSM) colonyJoinWindowLocked() *internalpb.ColonyJoinWindow {
	return f.colonyJoinWindow
}

// ColonyJoinWindowState returns a copy of the current window, or nil
// if this Colony has never had one opened. A copy, because the FSM
// hands out pointers to its own maps everywhere else and the callers
// here are on the request path rather than the apply path.
func (f *FSM) ColonyJoinWindowState() *internalpb.ColonyJoinWindow {
	f.mu.Lock()
	defer f.mu.Unlock()
	return cloneColonyJoinWindow(f.colonyJoinWindow)
}

// ColonyJoinWindowRequestCurrent reports whether req was created under
// the window that is live now, and is the check that makes a reopen
// invalidate everything created under the previous window.
//
// A request that recorded no epoch (field zero) fails this, which is
// what happens to a PendingJoinRequest that already existed in a
// Colony that upgraded: it was created when every member would have
// accepted it, and under ADR-0147 it is not approvable. That is
// deliberate and is not a migration problem - the fix is to have the
// requester start again, and the window is what the operator opens to
// let them.
func ColonyJoinWindowRequestCurrent(w *internalpb.ColonyJoinWindow, req *internalpb.PendingJoinRequest) bool {
	if req.GetWindowOpenedAtUnix() == 0 {
		return false
	}
	return w.GetOpenedAtUnix() == req.GetWindowOpenedAtUnix()
}

// cloneColonyJoinWindow deep-copies through proto.Clone rather than by
// value: a generated message carries a sync.Mutex in its MessageState,
// and `c := *w` copies it. (go vet's copylocks check is what catches
// that, and it is why this is not the one-line body it looks like.)
func cloneColonyJoinWindow(w *internalpb.ColonyJoinWindow) *internalpb.ColonyJoinWindow {
	if w == nil {
		return nil
	}
	return proto.Clone(w).(*internalpb.ColonyJoinWindow)
}
