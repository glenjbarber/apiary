package raft

// FSM-level tests for HandoverColonyUpdate, the PLANNED cooperative
// transfer of an operation that is already in progress (ADR-0146 rule 2).
//
// The property these tests exist to pin is not "a handover works". It is
// that a handover is indistinguishable from an ordinary step of a healthy
// colony-wide sweep in every field an operator reads, while a takeover is
// loudly different - because the moment both are routed through the same
// flag, neither means anything.

import (
	"bytes"
	"io"
	"testing"

	internalpb "github.com/glenjbarber/apiary/api/internalpb"
)

// accepts reports whether the FSM would grant cmd, by really applying it.
//
// It exists because fsmLog.apply returns an *FSMApplyResult rather than an
// error, so the obvious `if err := log.apply(...); err != nil` is
// vacuously true on every success - a refusal is a non-nil result with
// Error set, and a grant is a non-nil result with Error empty. Reading
// .Error is the only correct form.
func (l *fsmLog) accepts(cmd *internalpb.Command) bool {
	l.t.Helper()
	return l.apply(cmd).Error == ""
}

func handoverColonyUpdateCmd(from *internalpb.ColonyUpdateFence, toNode, toIncarnation, reason string) *internalpb.Command {
	return &internalpb.Command{Op: &internalpb.Command_HandoverColonyUpdate{
		HandoverColonyUpdate: &internalpb.HandoverColonyUpdate{
			FromFence:           from,
			ToNodeId:            toNode,
			ToHolderIncarnation: toIncarnation,
			Reason:              reason,
			RequestedAtUnix:     1_700_000_050,
		},
	}}
}

// TestFSM_HandoverColonyUpdate_ChangesTheHolderWithoutSettlingTheOperation
// is the positive path, and every clause of the name is load-bearing. A
// handover that quietly settled the operation would drop it out of the
// single-flight entirely, and a handover that reset the fence token
// without a strictly higher one would leave the outgoing coordinator
// holding a valid token for a record it no longer coordinates.
func TestFSM_HandoverColonyUpdate_ChangesTheHolderWithoutSettlingTheOperation(t *testing.T) {
	fsm := NewFSM()
	log := newFSMLog(t, fsm)

	rec := log.mustSucceed(acquireColonyUpdateCmd("op-1", "comb-a", "boot-1", false)).ColonyUpdate
	fence := fenceOf(rec)
	log.mustSucceed(advanceColonyUpdateCmd(fence, "step-aside", "buzz", "working on buzz", confirmedStep(0, "step-aside", "buzz", "leadership moved")))

	before, _ := fsm.ColonyUpdateState()
	beforeToken := before.GetFenceToken()
	beforeSteps := len(before.GetSteps())

	got := log.mustSucceed(handoverColonyUpdateCmd(fence, "comb-b", "boot-9", "comb-a is the last Comb left and cannot restart itself")).ColonyUpdate

	if !got.GetActive() {
		t.Error("a handover settled the operation: active = false, so the colony now believes nothing is running while the sweep is plainly still going")
	}
	if got.GetOutcome() != "" {
		t.Errorf("a handover set outcome %q on a still-running operation; a handover is not a verdict", got.GetOutcome())
	}
	if got.GetOperationId() != "op-1" {
		t.Errorf("operation id = %q, want op-1 - a handover must not renumber the operation it is moving", got.GetOperationId())
	}
	if got.GetHolderNodeId() != "comb-b" || got.GetHolderIncarnation() != "boot-9" {
		t.Errorf("holder = %s/%s, want comb-b/boot-9", got.GetHolderNodeId(), got.GetHolderIncarnation())
	}
	if got.GetFenceToken() <= beforeToken {
		t.Errorf("fence token = %d, want strictly greater than the outgoing %d - a handover that does not mint a higher token leaves the old coordinator's token valid",
			got.GetFenceToken(), beforeToken)
	}
	if got.GetTargetNodeId() != "buzz" || got.GetStep() != "step-aside" {
		t.Errorf("progress after handover = target %q step %q, want buzz/step-aside - a handover moves the coordinator, not the work",
			got.GetTargetNodeId(), got.GetStep())
	}
	if len(got.GetSteps()) != beforeSteps {
		t.Errorf("step history after handover has %d entries, want the %d it had; a handover must not discard durable progress",
			len(got.GetSteps()), beforeSteps)
	}

	// The handover evidence itself: both coordinators, both tokens, the
	// reason, and a leader-authored time.
	if len(got.GetHandovers()) != 1 {
		t.Fatalf("handovers = %d entries, want exactly 1", len(got.GetHandovers()))
	}
	h := got.GetHandovers()[0]
	if h.GetFromNodeId() != "comb-a" || h.GetFromIncarnation() != "boot-1" || h.GetFromFenceToken() != beforeToken {
		t.Errorf("handover records the outgoing coordinator as %s/%s at token %d, want comb-a/boot-1 at %d",
			h.GetFromNodeId(), h.GetFromIncarnation(), h.GetFromFenceToken(), beforeToken)
	}
	if h.GetToNodeId() != "comb-b" || h.GetToIncarnation() != "boot-9" || h.GetToFenceToken() != got.GetFenceToken() {
		t.Errorf("handover records the incoming coordinator as %s/%s at token %d, want comb-b/boot-9 at %d",
			h.GetToNodeId(), h.GetToIncarnation(), h.GetToFenceToken(), got.GetFenceToken())
	}
	if h.GetReason() == "" || h.GetReason() != "comb-a is the last Comb left and cannot restart itself" {
		t.Errorf("handover reason = %q, want the reason that was given", h.GetReason())
	}
	if h.GetHandedOverAtUnix() == 0 {
		t.Error("handover has no leader-authored time")
	}

	// Exactly one active record still. This is the invariant the whole
	// mechanism rests on, so it is re-checked rather than assumed.
	active, history := fsm.ColonyUpdateState()
	if active == nil || len(history) != 0 {
		t.Errorf("after a handover the FSM reports %d active and %d settled, want exactly 1 active and 0 settled",
			1, len(history))
	}
}

// TestFSM_HandoverColonyUpdate_IsNotATakeover is the test that justifies
// the command existing. Both operations move a live operation to a new
// holder, so the fields that differ between them are the only thing that
// keeps a healthy sweep readable.
func TestFSM_HandoverColonyUpdate_IsNotATakeover(t *testing.T) {
	t.Run("a handover leaves takeover false and does not settle the old record", func(t *testing.T) {
		fsm := NewFSM()
		log := newFSMLog(t, fsm)
		rec := log.mustSucceed(acquireColonyUpdateCmd("op-1", "comb-a", "boot-1", false)).ColonyUpdate

		got := log.mustSucceed(handoverColonyUpdateCmd(fenceOf(rec), "comb-b", "boot-9", "planned move before the last Comb")).ColonyUpdate

		if got.GetTakeover() {
			t.Error("takeover = true on a planned handover; the flag that exists to mean \"a second coordinator seized this\" now fires on the normal path of every healthy sweep")
		}
		_, settled := fsm.ColonyUpdateState()
		if len(settled) != 0 {
			t.Errorf("a handover settled %d record(s), want 0 - a takeover settles the displaced record as unobserved, and a handover must not", len(settled))
		}
	})

	t.Run("a takeover still says takeover and still settles the displaced record", func(t *testing.T) {
		fsm := NewFSM()
		log := newFSMLog(t, fsm)
		rec := log.mustSucceed(acquireColonyUpdateCmd("op-1", "comb-a", "boot-1", false)).ColonyUpdate
		_ = rec

		got := log.mustSucceed(acquireColonyUpdateCmd("op-2", "comb-b", "boot-9", true)).ColonyUpdate
		if !got.GetTakeover() {
			t.Error("takeover = false on an explicit takeover")
		}
		active, settled := fsm.ColonyUpdateState()
		if active.GetOperationId() != "op-2" {
			t.Errorf("active = %q, want op-2", active.GetOperationId())
		}
		if len(settled) != 1 || settled[0].GetOutcome() != colonyOutcomeUnobserved {
			t.Errorf("displaced record = %d settled with outcome %q, want 1 settled as %q",
				len(settled), settled[0].GetOutcome(), colonyOutcomeUnobserved)
		}
	})

	t.Run("takeover stays sticky across a later handover", func(t *testing.T) {
		// A takeover that was later followed by a cooperative handover is
		// still a takeover, and an operator reading the record months
		// later must still be able to see that something went wrong.
		fsm := NewFSM()
		log := newFSMLog(t, fsm)
		_ = log.mustSucceed(acquireColonyUpdateCmd("op-1", "comb-a", "boot-1", false))
		seized := log.mustSucceed(acquireColonyUpdateCmd("op-2", "comb-b", "boot-9", true)).ColonyUpdate

		got := log.mustSucceed(handoverColonyUpdateCmd(fenceOf(seized), "comb-c", "boot-3", "planned move after the takeover")).ColonyUpdate
		if !got.GetTakeover() {
			t.Error("takeover = false after a later handover cleared it; the flag is a record of what happened, not of the current holder")
		}
		if got.GetHolderNodeId() != "comb-c" {
			t.Errorf("holder = %q, want comb-c", got.GetHolderNodeId())
		}
	})
}

// TestFSM_HandoverColonyUpdate_ADisplacedHolderCannotGiveItAway is the
// fencing property ADR-0146 insists on: a handover is not a way to escape
// fencing. Once someone else holds the operation, the previous holder's
// token is stale and must be refused by name.
func TestFSM_HandoverColonyUpdate_ADisplacedHolderCannotGiveItAway(t *testing.T) {
	cases := []struct {
		name       string
		settleBy   string // "" to displace by takeover, otherwise settle the operation
		wantSubstr string
	}{
		{
			name:       "after a takeover",
			settleBy:   "",
			wantSubstr: "does not match",
		},
		{
			name:       "after the operation was settled",
			settleBy:   colonyOutcomeBlocked,
			wantSubstr: "no controlled update is currently in progress",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fsm := NewFSM()
			log := newFSMLog(t, fsm)
			rec := log.mustSucceed(acquireColonyUpdateCmd("op-1", "comb-a", "boot-1", false)).ColonyUpdate
			stale := fenceOf(rec)

			if tc.settleBy == "" {
				log.mustSucceed(acquireColonyUpdateCmd("op-2", "comb-b", "boot-9", true))
			} else {
				log.mustSucceed(releaseColonyUpdateCmd(stale, tc.settleBy, "the sweep could not continue"))
			}

			refusal := log.mustRefuse(handoverColonyUpdateCmd(stale, "comb-c", "boot-3", "giving away an operation I no longer hold"))
			if !contains(refusal, tc.wantSubstr) {
				t.Errorf("refusal = %q, want it to say %q", refusal, tc.wantSubstr)
			}

			// And the holder that actually holds it is untouched by the
			// refused attempt: a displaced coordinator must not be able to
			// so much as disturb the record it lost.
			active, _ := fsm.ColonyUpdateState()
			if tc.settleBy == "" {
				if active.GetOperationId() != "op-2" || active.GetHolderNodeId() != "comb-b" {
					t.Errorf("after a refused stale handover the active record is %q held by %q, want op-2 held by comb-b",
						active.GetOperationId(), active.GetHolderNodeId())
				}
			} else if active != nil {
				t.Errorf("after a refused handover the operation %q is active again, want nothing active", active.GetOperationId())
			}
		})
	}
}

// TestFSM_HandoverColonyUpdate_LeavesTheIncomingHolderUnderTheSameFenceRules
// is the "it does not relax anything" clause. The incoming coordinator
// must be fenced exactly as strictly as the outgoing one was, or a
// handover is just a way to mint a fresh un-fenced token.
func TestFSM_HandoverColonyUpdate_LeavesTheIncomingHolderUnderTheSameFenceRules(t *testing.T) {
	fsm := NewFSM()
	log := newFSMLog(t, fsm)
	rec := log.mustSucceed(acquireColonyUpdateCmd("op-1", "comb-a", "boot-1", false)).ColonyUpdate
	oldFence := fenceOf(rec)

	newRec := log.mustSucceed(handoverColonyUpdateCmd(oldFence, "comb-b", "boot-9", "planned move")).ColonyUpdate
	newFence := fenceOf(newRec)

	// The incoming holder can act.
	if !log.accepts(advanceColonyUpdateCmd(newFence, "gather-facts", "buzz", "", nil)) {
		t.Error("the incoming holder's own fence was refused after the handover")
	}
	// The outgoing one cannot, on either the restart-lease path or the
	// update path. Checking only one of the two would leave half the
	// escape open.
	if log.accepts(advanceColonyUpdateCmd(oldFence, "gather-facts", "buzz", "", nil)) {
		t.Error("the outgoing holder's fence still advances the operation after a handover")
	}
	if err := fsm.applyFenceCheck(oldFence); err == nil {
		t.Error("the outgoing holder's fence still acquires a restart lease after a handover")
	}
	// A token forged from the new holder's node with the OLD token is
	// still stale: the token is the part that cannot be guessed forward.
	forged := &internalpb.ColonyUpdateFence{
		OperationId: "op-1", HolderNodeId: "comb-b",
		FenceToken: oldFence.GetFenceToken(), HolderIncarnation: "boot-9",
	}
	if log.accepts(advanceColonyUpdateCmd(forged, "gather-facts", "buzz", "", nil)) {
		t.Error("a fence carrying the new holder's identity but the old token was accepted")
	}
}

// TestFSM_HandoverColonyUpdate_ToAnotherProcessOnTheSameComb covers the
// managerd-replacement case from ADR-0146 rule 2: the incoming
// coordinator can be on the SAME Comb as the outgoing one, differing only
// in incarnation. Refusing that would break the self-restart handoff the
// handover exists to enable.
func TestFSM_HandoverColonyUpdate_ToAnotherProcessOnTheSameComb(t *testing.T) {
	fsm := NewFSM()
	log := newFSMLog(t, fsm)
	rec := log.mustSucceed(acquireColonyUpdateCmd("op-1", "comb-a", "boot-1", false)).ColonyUpdate

	got := log.mustSucceed(handoverColonyUpdateCmd(fenceOf(rec), "comb-a", "boot-2", "handing to the managerd that will replace me")).ColonyUpdate

	if got.GetHolderNodeId() != "comb-a" || got.GetHolderIncarnation() != "boot-2" {
		t.Errorf("holder = %s/%s, want comb-a/boot-2", got.GetHolderNodeId(), got.GetHolderIncarnation())
	}
	if !got.GetActive() {
		t.Error("handing to a replacement process on the same Comb settled the operation")
	}
}

// TestFSM_HandoverColonyUpdate_RefusesIncompleteAndNoOp is the validation
// table. Every case here is something a real caller can get wrong, and a
// handover that is half-specified is exactly the record an operator would
// read later and not be able to interpret.
func TestFSM_HandoverColonyUpdate_RefusesIncompleteAndNoOp(t *testing.T) {
	cases := []struct {
		name       string
		toNode     string
		toInc      string
		reason     string
		wantSubstr string
	}{
		{
			name: "no receiving node", toInc: "boot-9", reason: "planned move",
			wantSubstr: "no receiving node was named",
		},
		{
			name: "no receiving incarnation", toNode: "comb-b", reason: "planned move",
			wantSubstr: "no receiving incarnation was named",
		},
		{
			name: "no reason", toNode: "comb-b", toInc: "boot-9",
			wantSubstr: "no reason was given",
		},
		{
			name: "handing to the coordinator that already holds it", toNode: "comb-a", toInc: "boot-1",
			reason:     "planned move",
			wantSubstr: "that is the coordinator that already holds it",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fsm := NewFSM()
			log := newFSMLog(t, fsm)
			rec := log.mustSucceed(acquireColonyUpdateCmd("op-1", "comb-a", "boot-1", false)).ColonyUpdate
			tokenBefore := rec.GetFenceToken()

			refusal := log.mustRefuse(handoverColonyUpdateCmd(fenceOf(rec), tc.toNode, tc.toInc, tc.reason))
			if !contains(refusal, tc.wantSubstr) {
				t.Errorf("refusal = %q, want it to say %q", refusal, tc.wantSubstr)
			}

			// Every refusal must leave the record exactly as it was. A
			// rejected handover that still minted a token or appended a
			// record would corrupt the very history it was asked to extend.
			active, settled := fsm.ColonyUpdateState()
			if len(settled) != 0 {
				t.Errorf("a refused handover settled %d record(s), want 0", len(settled))
			}
			if active.GetFenceToken() != tokenBefore {
				t.Errorf("a refused handover moved the fence token from %d to %d", tokenBefore, active.GetFenceToken())
			}
			if len(active.GetHandovers()) != 0 {
				t.Errorf("a refused handover left %d handover record(s), want 0", len(active.GetHandovers()))
			}
		})
	}
}

// TestFSM_HandoverColonyUpdate_SurvivesSnapshotRestore: the incoming
// coordinator is usually a process that did not exist when the handover
// was decided, so the whole record has to be readable from replicated
// state alone.
func TestFSM_HandoverColonyUpdate_SurvivesSnapshotRestore(t *testing.T) {
	fsm := NewFSM()
	log := newFSMLog(t, fsm)
	rec := log.mustSucceed(acquireColonyUpdateCmd("op-1", "comb-a", "boot-1", false)).ColonyUpdate
	log.mustSucceed(advanceColonyUpdateCmd(fenceOf(rec), "step-aside", "buzz", "", confirmedStep(0, "step-aside", "buzz", "leadership moved")))
	handover := log.mustSucceed(handoverColonyUpdateCmd(fenceOf(rec), "comb-b", "boot-9", "planned move before the last Comb")).ColonyUpdate

	snap, err := fsm.Snapshot()
	if err != nil {
		t.Fatalf("Snapshot() error: %v", err)
	}
	sink := &fakeSnapshotSink{}
	if err := snap.Persist(sink); err != nil {
		t.Fatalf("Persist() error: %v", err)
	}
	replacement := NewFSM()
	if err := replacement.Restore(io.NopCloser(bytes.NewReader(sink.Bytes()))); err != nil {
		t.Fatalf("Restore() error: %v", err)
	}

	active, _ := replacement.ColonyUpdateState()
	if active == nil {
		t.Fatal("a restored FSM reports no active controlled update")
	}
	if active.GetHolderNodeId() != "comb-b" || active.GetHolderIncarnation() != "boot-9" {
		t.Errorf("restored holder = %s/%s, want comb-b/boot-9", active.GetHolderNodeId(), active.GetHolderIncarnation())
	}
	if active.GetFenceToken() != handover.GetFenceToken() {
		t.Errorf("restored fence token = %d, want %d", active.GetFenceToken(), handover.GetFenceToken())
	}
	if len(active.GetHandovers()) != 1 {
		t.Fatalf("restored handovers = %d, want 1", len(active.GetHandovers()))
	}
	if h := active.GetHandovers()[0]; h.GetReason() != "planned move before the last Comb" || h.GetFromNodeId() != "comb-a" {
		t.Errorf("restored handover = from %s reason %q, want from comb-a reason \"planned move before the last Comb\"", h.GetFromNodeId(), h.GetReason())
	}
	// The restored fence is the live one, and the pre-handover token is
	// still stale after a restore.
	restoredLog := newFSMLog(t, replacement)
	restoredLog.index = handover.GetFenceToken() + 1
	if !restoredLog.accepts(advanceColonyUpdateCmd(fenceOf(active), "gather-facts", "buzz", "", nil)) {
		t.Error("the restored FSM refused the restored holder's own fence")
	}
	if err := replacement.applyFenceCheck(&internalpb.ColonyUpdateFence{
		OperationId: "op-1", HolderNodeId: "comb-a", FenceToken: 1, HolderIncarnation: "boot-1",
	}); err == nil {
		t.Error("the restored FSM accepted the pre-handover fence token")
	}
}

// contains is a tiny local helper so the refusal assertions above read
// as prose rather than as a string-literal grep.
func contains(haystack, needle string) bool {
	return bytes.Contains([]byte(haystack), []byte(needle))
}
