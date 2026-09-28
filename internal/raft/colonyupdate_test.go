package raft

// FSM-level tests for ADR-0145's colony-wide controlled-update
// single-flight and its durable state.
//
// The single-node NewFSM() tests below are the decision table: they are
// where every branch of the fence, the history cap and the evidence rule
// is pinned, and they are fast enough that a table can be exhaustive.
// The concurrency, durability and cross-coordinator properties are NOT
// testable here - a bare FSM has no quorum and no process to kill - and
// they live in colonyupdate_cluster_test.go against a real raft.

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/hashicorp/raft"
	"google.golang.org/protobuf/proto"

	internalpb "github.com/glenjbarber/apiary/api/internalpb"
	"github.com/glenjbarber/apiary/internal/restartplan"
)

// index counter for the bare-FSM tests. A bare FSM is driven with
// hand-chosen log indexes so the fence token is predictable, which is
// what makes "a takeover produces a strictly higher token" assertable
// rather than merely observable.
type fsmLog struct {
	t     *testing.T
	index uint64
	fsm   *FSM
}

func newFSMLog(t *testing.T, fsm *FSM) *fsmLog {
	t.Helper()
	return &fsmLog{t: t, index: 1, fsm: fsm}
}

func (l *fsmLog) apply(cmd *internalpb.Command) *FSMApplyResult {
	l.t.Helper()
	res, ok := l.fsm.Apply(&raft.Log{Index: l.index, Data: mustMarshalCommand(l.t, cmd)}).(*FSMApplyResult)
	if !ok {
		l.t.Fatalf("FSM.Apply() did not return an *FSMApplyResult")
	}
	l.index++
	return res
}

func (l *fsmLog) mustSucceed(cmd *internalpb.Command) *FSMApplyResult {
	l.t.Helper()
	res := l.apply(cmd)
	if res.Error != "" {
		l.t.Fatalf("apply of %T was refused: %s", cmd.GetOp(), res.Error)
	}
	return res
}

func (l *fsmLog) mustRefuse(cmd *internalpb.Command) string {
	l.t.Helper()
	res := l.apply(cmd)
	if res.Error == "" {
		l.t.Fatalf("apply of %T was accepted, want a refusal", cmd.GetOp())
	}
	return res.Error
}

func acquireColonyUpdateCmd(operationID, nodeID, incarnation string, takeover bool) *internalpb.Command {
	return &internalpb.Command{Op: &internalpb.Command_AcquireColonyUpdate{
		AcquireColonyUpdate: &internalpb.AcquireColonyUpdate{
			OperationId:       operationID,
			HolderNodeId:      nodeID,
			HolderIncarnation: incarnation,
			RequestedAtUnix:   1_700_000_000,
			Takeover:          takeover,
		},
	}}
}

func advanceColonyUpdateCmd(fence *internalpb.ColonyUpdateFence, step, target, detail string, rec *internalpb.ColonyUpdateStepRecord) *internalpb.Command {
	return &internalpb.Command{Op: &internalpb.Command_AdvanceColonyUpdate{
		AdvanceColonyUpdate: &internalpb.AdvanceColonyUpdate{
			Fence: fence, Step: step, TargetNodeId: target, Detail: detail, StepRecord: rec,
		},
	}}
}

func releaseColonyUpdateCmd(fence *internalpb.ColonyUpdateFence, outcome, detail string) *internalpb.Command {
	return &internalpb.Command{Op: &internalpb.Command_ReleaseColonyUpdate{
		ReleaseColonyUpdate: &internalpb.ReleaseColonyUpdate{
			Fence: fence, Outcome: outcome, Detail: detail, CompletedAtUnix: 1_700_000_100,
		},
	}}
}

func fenceOf(rec *internalpb.ColonyUpdate) *internalpb.ColonyUpdateFence {
	return &internalpb.ColonyUpdateFence{
		OperationId:       rec.GetOperationId(),
		HolderNodeId:      rec.GetHolderNodeId(),
		FenceToken:        rec.GetFenceToken(),
		HolderIncarnation: rec.GetHolderIncarnation(),
	}
}

// confirmedStep builds a step record that satisfies the evidence rule, so
// a test about something else does not fail for this reason instead.
func confirmedStep(index uint32, step, node, evidence string) *internalpb.ColonyUpdateStepRecord {
	return &internalpb.ColonyUpdateStepRecord{
		Index:    index,
		Step:     step,
		Outcome:  colonyOutcomeConfirmed,
		Detail:   "the step ran and reported itself back",
		Evidence: []string{evidence},
		NodeId:   node,
	}
}

func TestFSM_AcquireColonyUpdate_GrantsExactlyOneAndItIsReadable(t *testing.T) {
	log := newFSMLog(t, NewFSM())

	res := log.mustSucceed(acquireColonyUpdateCmd("op-1", "comb-a", "boot-1", false))
	rec := res.ColonyUpdate
	if rec.GetOperationId() != "op-1" || rec.GetHolderNodeId() != "comb-a" || rec.GetHolderIncarnation() != "boot-1" {
		t.Errorf("granted record = %+v, want it to name op-1 held by comb-a/boot-1", rec)
	}
	if !rec.GetActive() {
		t.Error("granted record active = false, want true")
	}
	// The fence token is the granting command's own log index. That is
	// the whole fencing mechanism and it is worth pinning: a token that
	// came from anywhere else would not be monotonic across grants, and
	// monotonicity is what makes a takeover invalidate every outstanding
	// token at one instant.
	if rec.GetFenceToken() != 1 {
		t.Errorf("FenceToken = %d, want 1 (the granting command's own log index)", rec.GetFenceToken())
	}
	if rec.GetTakeover() {
		t.Error("Takeover = true on the first grant, want false")
	}

	// A record applied to one FSM is not visible to an unrelated one.
	// Stated explicitly because it is the assumption the durability
	// tests below replace: replication is what makes the state
	// colony-wide, and an in-process map is not.
	if active, _ := NewFSM().ColonyUpdateState(); active != nil {
		t.Error("a fresh, unrelated FSM reports an active controlled update")
	}
}

// TestFSM_AcquireColonyUpdate_SecondHolderIsRefusedNamingTheHolder is
// the refusal an operator sees. It has to name WHO, or a second operator
// is told only "no" and has to go looking for the answer themselves.
func TestFSM_AcquireColonyUpdate_SecondHolderIsRefusedNamingTheHolder(t *testing.T) {
	log := newFSMLog(t, NewFSM())
	first := log.mustSucceed(acquireColonyUpdateCmd("op-1", "comb-a", "boot-1", false)).ColonyUpdate

	err := log.mustRefuse(acquireColonyUpdateCmd("op-2", "comb-b", "boot-9", false))

	for _, want := range []string{
		`"op-1"`,                // which operation
		`"comb-a"`,              // which Comb holds it
		`"boot-1"`,              // which managerd PROCESS holds it
		"one controlled update", // and the rule that refused it
	} {
		if !strings.Contains(err, want) {
			t.Errorf("refusal %q does not mention %s", err, want)
		}
	}
	// And it must NOT have displaced anything: the second acquire is a
	// refusal, not a takeover.
	got, _ := log.fsm.ColonyUpdateState()
	if got.GetFenceToken() != first.GetFenceToken() || got.GetOperationId() != "op-1" {
		t.Errorf("after a refused acquire the colony runs %q at token %d, want op-1 at token %d unchanged",
			got.GetOperationId(), got.GetFenceToken(), first.GetFenceToken())
	}
}

// TestFSM_AcquireColonyUpdate_ConcurrentRequestsOnlyOneGranted is the
// core safety property. A bare FSM applies serially under its own lock,
// so this proves the DECISION TABLE admits one winner; the genuinely
// concurrent version - N callers, M voters, real raft - is
// TestColonyUpdate_ConcurrentCoordinatorsAcrossVotersGrantOneWinner in
// colonyupdate_cluster_test.go.
func TestFSM_AcquireColonyUpdate_ConcurrentRequestsOnlyOneGranted(t *testing.T) {
	fsm := NewFSM()
	granted := 0
	for i := 0; i < 8; i++ {
		res := fsm.Apply(&raft.Log{Index: uint64(i + 1), Data: mustMarshalCommand(t,
			acquireColonyUpdateCmd("op-"+string(rune('a'+i)), "comb-x", "boot-x", false))}).(*FSMApplyResult)
		if res.Error == "" {
			granted++
		}
	}
	if granted != 1 {
		t.Errorf("%d acquires were granted, want exactly 1 - the colony-wide single-flight is the whole point", granted)
	}
}

// TestFSM_AcquireColonyUpdate_RefusesIncompleteIdentity covers the three
// fields without which the fence cannot do its job. An omitted
// incarnation is the sharpest: without it a replacement managerd on a
// Comb is indistinguishable from the predecessor it replaced.
func TestFSM_AcquireColonyUpdate_RefusesIncompleteIdentity(t *testing.T) {
	cases := []struct {
		name    string
		cmd     *internalpb.Command
		wantSay string
	}{
		{
			name:    "no operation id",
			cmd:     acquireColonyUpdateCmd("", "comb-a", "boot-1", false),
			wantSay: "operation id",
		},
		{
			name:    "no holder node id",
			cmd:     acquireColonyUpdateCmd("op-1", "", "boot-1", false),
			wantSay: "holder node id",
		},
		{
			name:    "no holder incarnation",
			cmd:     acquireColonyUpdateCmd("op-1", "comb-a", "", false),
			wantSay: "holder incarnation",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			log := newFSMLog(t, NewFSM())
			err := log.mustRefuse(tc.cmd)
			if !strings.Contains(err, tc.wantSay) {
				t.Errorf("refusal %q does not name the missing field (%s)", err, tc.wantSay)
			}
			active, _ := log.fsm.ColonyUpdateState()
			if active != nil {
				t.Error("a refused acquire still created an active record")
			}
		})
	}
}

// TestFSM_AcquireColonyUpdate_IdReuseIsRefused protects the fence from
// the most tempting way to lose it: re-granting the same operation id.
// That would reset the token and silently un-fence whatever the previous
// process still holds.
func TestFSM_AcquireColonyUpdate_IdReuseIsRefused(t *testing.T) {
	log := newFSMLog(t, NewFSM())
	log.mustSucceed(acquireColonyUpdateCmd("op-1", "comb-a", "boot-1", false))
	fence := fenceOf(log.fsm.ColonyUpdateActive(t))
	log.mustSucceed(releaseColonyUpdateCmd(fence, colonyOutcomeConfirmed, "all four Combs reported the new build"))

	err := log.mustRefuse(acquireColonyUpdateCmd("op-1", "comb-b", "boot-9", false))
	if !strings.Contains(err, "already been used") {
		t.Errorf("refusal %q does not say the id was already used", err)
	}
	// A takeover does not make an id reusable either, which is why the
	// takeover below is refused for the same reason.
	if got := log.mustRefuse(acquireColonyUpdateCmd("op-1", "comb-b", "boot-9", true)); !strings.Contains(got, "already been used") {
		t.Errorf("takeover of a used id = %q, want the same already-used refusal", got)
	}
}

// ColonyUpdateActive is a tiny test helper for "the one record, or fail
// loudly" - a nil-dereference in a test reads like a nil-dereference in
// production, which helps nobody.
func (f *FSM) ColonyUpdateActive(t *testing.T) *internalpb.ColonyUpdate {
	t.Helper()
	active, _ := f.ColonyUpdateState()
	if active == nil {
		t.Fatal("no active controlled update, but the test expected one")
	}
	return active
}

// TestFSM_AdvanceColonyUpdate_FenceMustMatchExactly is the fencing
// table. Every row is a way a caller can be a real holder of a real
// operation and still be wrong, and each must be refused BY NAME.
func TestFSM_AdvanceColonyUpdate_FenceMustMatchExactly(t *testing.T) {
	base := func(t *testing.T) (*fsmLog, *internalpb.ColonyUpdateFence) {
		t.Helper()
		log := newFSMLog(t, NewFSM())
		rec := log.mustSucceed(acquireColonyUpdateCmd("op-1", "comb-a", "boot-1", false)).ColonyUpdate
		return log, fenceOf(rec)
	}

	cases := []struct {
		name   string
		mutate func(*internalpb.ColonyUpdateFence)
	}{
		{"a different operation id", func(f *internalpb.ColonyUpdateFence) { f.OperationId = "op-9" }},
		{"a different holder node", func(f *internalpb.ColonyUpdateFence) { f.HolderNodeId = "comb-z" }},
		{"a different incarnation", func(f *internalpb.ColonyUpdateFence) { f.HolderIncarnation = "boot-9" }},
		{"an older fence token", func(f *internalpb.ColonyUpdateFence) { f.FenceToken = 0 }},
		{"a higher fence token it was never granted", func(f *internalpb.ColonyUpdateFence) { f.FenceToken = 99 }},
		{"no fence at all", func(f *internalpb.ColonyUpdateFence) {}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			log, fence := base(t)
			if tc.name == "no fence at all" {
				fence = nil
			} else {
				tc.mutate(fence)
			}
			err := log.mustRefuse(advanceColonyUpdateCmd(fence, "step-aside", "comb-b", "about to begin", nil))
			// Every one of these refusals must NAME the real holder. A
			// refusal that only says "no" leaves a displaced coordinator
			// unable to tell a fence problem from an outage.
			if !strings.Contains(err, "comb-a") || !strings.Contains(err, "boot-1") {
				t.Errorf("refusal %q does not name the actual holder (comb-a/boot-1)", err)
			}
		})
	}
}

// TestFSM_AdvanceColonyUpdate_RecordsDurableStepState is the durable
// operation state itself: operation id, target Comb, the step reached,
// and each step's outcome with its confirmation evidence - all of it in
// replicated state, so any Comb can answer "what is the state".
func TestFSM_AdvanceColonyUpdate_RecordsDurableStepState(t *testing.T) {
	log := newFSMLog(t, NewFSM())
	rec := log.mustSucceed(acquireColonyUpdateCmd("op-1", "comb-a", "boot-1", false)).ColonyUpdate
	fence := fenceOf(rec)

	log.mustSucceed(advanceColonyUpdateCmd(fence, "step-aside", "buzz", "beginning the leader step-aside on buzz", nil))
	log.mustSucceed(advanceColonyUpdateCmd(fence, "step-aside", "buzz", "",
		confirmedStep(0, "step-aside", "buzz", "buzz was the raft leader and handed over to drone, confirmed follower")))

	got := log.fsm.ColonyUpdateActive(t)
	if got.GetStep() != "step-aside" {
		t.Errorf("Step = %q, want step-aside - the step reached must be in the record", got.GetStep())
	}
	if got.GetTargetNodeId() != "buzz" {
		t.Errorf("TargetNodeId = %q, want buzz - the target Comb must be in the record", got.GetTargetNodeId())
	}
	if len(got.GetSteps()) != 1 {
		t.Fatalf("steps recorded = %d, want 1", len(got.GetSteps()))
	}
	step := got.GetSteps()[0]
	if step.GetStep() != "step-aside" || step.GetOutcome() != colonyOutcomeConfirmed {
		t.Errorf("step record = %+v, want step-aside/confirmed", step)
	}
	if len(step.GetEvidence()) != 1 || !strings.Contains(step.GetEvidence()[0], "handed over") {
		t.Errorf("step evidence = %v, want the confirmation that backs the outcome", step.GetEvidence())
	}
	// recorded_at_unix is stamped by the FSM, not taken from the caller:
	// every replica must record the same value, and a caller-chosen one
	// would let two voters disagree about when this happened.
	if step.GetRecordedAtUnix() == 0 {
		t.Error("step record has no recorded_at_unix; it must be stamped by the FSM, not accepted from the caller")
	}
}

// TestFSM_AdvanceColonyUpdate_RefusesUnbackedVerdicts is
// internal/restartplan's Result.Validate rule, applied here because a
// durable record is exactly as capable of outliving the process that
// wrote it, and exactly as capable of being believed.
func TestFSM_AdvanceColonyUpdate_RefusesUnbackedVerdicts(t *testing.T) {
	cases := []struct {
		name        string
		rec         *internalpb.ColonyUpdateStepRecord
		wantSay     string
		wantRefused bool
	}{
		{
			name:        "confirmed with no evidence",
			rec:         &internalpb.ColonyUpdateStepRecord{Index: 0, Step: "issue-restart", Outcome: colonyOutcomeConfirmed, Detail: "it went fine"},
			wantSay:     "no evidence",
			wantRefused: true,
		},
		{
			name:        "failed with no evidence",
			rec:         &internalpb.ColonyUpdateStepRecord{Index: 0, Step: "issue-restart", Outcome: colonyOutcomeFailed, Detail: "it broke"},
			wantSay:     "no evidence",
			wantRefused: true,
		},
		{
			name:        "confirmed with no detail",
			rec:         &internalpb.ColonyUpdateStepRecord{Index: 0, Step: "issue-restart", Outcome: colonyOutcomeConfirmed, Evidence: []string{"x"}},
			wantSay:     "no detail",
			wantRefused: true,
		},
		{
			name:        "no step named",
			rec:         &internalpb.ColonyUpdateStepRecord{Index: 0, Outcome: colonyOutcomeBlocked, Detail: "refused"},
			wantSay:     "names no step",
			wantRefused: true,
		},
		{
			name:        "an unrecognised outcome",
			rec:         &internalpb.ColonyUpdateStepRecord{Index: 0, Step: "issue-restart", Outcome: "probably-fine", Detail: "sure"},
			wantSay:     "not one of the recognised outcomes",
			wantRefused: true,
		},
		{
			name:        "an empty outcome",
			rec:         &internalpb.ColonyUpdateStepRecord{Index: 0, Step: "issue-restart", Detail: "something happened"},
			wantSay:     "not one of the recognised outcomes",
			wantRefused: true,
		},
		{
			// The two honest third states are exempt, and this row is
			// what proves it: "we could not check" is a real answer and
			// must not be made unrepresentable by an evidence rule.
			name: "unobserved with no evidence is allowed",
			rec:  &internalpb.ColonyUpdateStepRecord{Index: 0, Step: "await-confirmation", Outcome: colonyOutcomeUnobserved, Detail: "the confirmation never arrived within 5 attempts"},
		},
		{
			name: "unverified with no evidence is allowed",
			rec:  &internalpb.ColonyUpdateStepRecord{Index: 0, Step: "gather-facts", Outcome: colonyOutcomeUnverified, Detail: "the probe never ran"},
		},
		{
			name: "blocked with no evidence is allowed",
			rec:  &internalpb.ColonyUpdateStepRecord{Index: 0, Step: "evaluate", Outcome: colonyOutcomeBlocked, Detail: "quorum would have been lost"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			log := newFSMLog(t, NewFSM())
			rec := log.mustSucceed(acquireColonyUpdateCmd("op-1", "comb-a", "boot-1", false)).ColonyUpdate
			fence := fenceOf(rec)
			res := log.apply(advanceColonyUpdateCmd(fence, tc.rec.GetStep(), "buzz", "advancing", tc.rec))
			if !tc.wantRefused {
				// The exempt rows exist to prove the rule is not simply
				// "demand evidence always": a step whose honest answer is
				// "we could not check" has no evidence, and refusing it
				// would push a caller to invent one.
				if res.Error != "" {
					t.Errorf("a legitimate %q step record was refused: %s", tc.rec.GetOutcome(), res.Error)
				}
				if got := log.fsm.ColonyUpdateActive(t); len(got.GetSteps()) != 1 {
					t.Errorf("step records = %d, want the accepted one recorded", len(got.GetSteps()))
				}
				return
			}
			if res.Error == "" {
				t.Fatalf("an unbacked %q step record was accepted, want a refusal", tc.rec.GetOutcome())
			}
			if !strings.Contains(res.Error, tc.wantSay) {
				t.Errorf("refusal %q does not say %q", res.Error, tc.wantSay)
			}
			if got := log.fsm.ColonyUpdateActive(t); len(got.GetSteps()) != 0 {
				t.Error("a refused step record was still written to the durable history")
			}
		})
	}
}

// TestFSM_AdvanceColonyUpdate_ReplayIsIdempotentButRewriteIsRefused
// covers the two failure modes of an at-least-once caller. A
// coordinator that crashed after its apply committed but before it read
// the response re-sends the same record; accepting that is what stops a
// lost response becoming a wedge. A DIFFERENT record at an occupied
// position is a superseded holder trying to rewrite history, and must
// not be accepted.
func TestFSM_AdvanceColonyUpdate_ReplayIsIdempotentButRewriteIsRefused(t *testing.T) {
	log := newFSMLog(t, NewFSM())
	rec := log.mustSucceed(acquireColonyUpdateCmd("op-1", "comb-a", "boot-1", false)).ColonyUpdate
	fence := fenceOf(rec)

	original := confirmedStep(0, "step-aside", "buzz", "leadership moved from buzz to drone")
	log.mustSucceed(advanceColonyUpdateCmd(fence, "step-aside", "buzz", "", original))

	// Exact replay: accepted, and NOT appended twice.
	log.mustSucceed(advanceColonyUpdateCmd(fence, "step-aside", "buzz", "", confirmedStep(0, "step-aside", "buzz", "leadership moved from buzz to drone")))
	if got := log.fsm.ColonyUpdateActive(t); len(got.GetSteps()) != 1 {
		t.Errorf("an exact replay was appended: %d step records, want 1", len(got.GetSteps()))
	}

	// A gap: refused, because a reader must never be shown a sequence
	// with a hole it cannot explain.
	if err := log.mustRefuse(advanceColonyUpdateCmd(fence, "evaluate", "buzz", "", confirmedStep(2, "evaluate", "buzz", "quorum 3 of 4, 3 reachable"))); !strings.Contains(err, "skips ahead") {
		t.Errorf("a gapped step record = %q, want a skips-ahead refusal", err)
	}

	// A rewrite of an occupied position: refused.
	rewritten := confirmedStep(0, "step-aside", "buzz", "something else entirely")
	if err := log.mustRefuse(advanceColonyUpdateCmd(fence, "step-aside", "buzz", "", rewritten)); !strings.Contains(err, "immutable") {
		t.Errorf("a rewritten step record = %q, want an immutability refusal", err)
	}
	if got := log.fsm.ColonyUpdateActive(t); got.GetSteps()[0].GetEvidence()[0] != original.GetEvidence()[0] {
		t.Error("a refused rewrite still changed the durable step record")
	}
}

// TestFSM_ReleaseColonyUpdate_SettlesWithoutDeleting is the difference
// between "is anything running?" and "what happened to the one that
// was?" - the reason a settled record is marked rather than removed.
func TestFSM_ReleaseColonyUpdate_SettlesWithoutDeleting(t *testing.T) {
	log := newFSMLog(t, NewFSM())
	rec := log.mustSucceed(acquireColonyUpdateCmd("op-1", "comb-a", "boot-1", false)).ColonyUpdate
	fence := fenceOf(rec)

	log.mustSucceed(releaseColonyUpdateCmd(fence, colonyOutcomeConfirmed, "all four Combs reported the new build and the FSM digests agree"))

	active, history := log.fsm.ColonyUpdateState()
	if active != nil {
		t.Errorf("after a release the colony still reports an active update: %+v", active)
	}
	if len(history) != 1 {
		t.Fatalf("settled history = %d entries, want 1 - a release must not delete the record", len(history))
	}
	if got := history[0]; got.GetOutcome() != colonyOutcomeConfirmed || got.GetSettledAtUnix() == 0 {
		t.Errorf("settled record = %+v, want it to carry its outcome and settle time", got)
	}
	// And the colony is genuinely free again.
	log.mustSucceed(acquireColonyUpdateCmd("op-2", "comb-b", "boot-9", false))
}

// TestFSM_ReleaseColonyUpdate_RefusesUnbackedAndMisattributed is the
// same honesty rule on the terminal record, plus the fence requirement
// that stops a displaced coordinator settling the operation that
// displaced it.
func TestFSM_ReleaseColonyUpdate_RefusesUnbackedAndMisattributed(t *testing.T) {
	t.Run("an unrecognised outcome", func(t *testing.T) {
		log := newFSMLog(t, NewFSM())
		fence := fenceOf(log.mustSucceed(acquireColonyUpdateCmd("op-1", "comb-a", "boot-1", false)).ColonyUpdate)
		if err := log.mustRefuse(releaseColonyUpdateCmd(fence, "done", "all finished")); !strings.Contains(err, "not one of the recognised outcomes") {
			t.Errorf("release with outcome \"done\" = %q, want a vocabulary refusal", err)
		}
	})
	t.Run("no detail", func(t *testing.T) {
		log := newFSMLog(t, NewFSM())
		fence := fenceOf(log.mustSucceed(acquireColonyUpdateCmd("op-1", "comb-a", "boot-1", false)).ColonyUpdate)
		if err := log.mustRefuse(releaseColonyUpdateCmd(fence, colonyOutcomeConfirmed, "")); !strings.Contains(err, "no detail") {
			t.Errorf("release with no detail = %q, want a no-detail refusal", err)
		}
	})
	t.Run("a stale fence", func(t *testing.T) {
		log := newFSMLog(t, NewFSM())
		fence := fenceOf(log.mustSucceed(acquireColonyUpdateCmd("op-1", "comb-a", "boot-1", false)).ColonyUpdate)
		stale := &internalpb.ColonyUpdateFence{
			OperationId: fence.GetOperationId(), HolderNodeId: fence.GetHolderNodeId(),
			FenceToken: fence.GetFenceToken() - 1, HolderIncarnation: fence.GetHolderIncarnation(),
		}
		if err := log.mustRefuse(releaseColonyUpdateCmd(stale, colonyOutcomeConfirmed, "pretending it finished")); !strings.Contains(err, "does not match") {
			t.Errorf("release with a stale fence = %q, want a fence refusal", err)
		}
	})
}

// TestFSM_AcquireColonyUpdate_TakeoverFencesThePreviousHolder is the
// failover property, stated as one test rather than several because it
// is one property: after a takeover the displaced coordinator can do
// NOTHING - not advance, not settle, and - the part that actually makes
// failover safe - not acquire another Comb's restart lease.
func TestFSM_AcquireColonyUpdate_TakeoverFencesThePreviousHolder(t *testing.T) {
	log := newFSMLog(t, NewFSM())
	first := log.mustSucceed(acquireColonyUpdateCmd("op-1", "comb-a", "boot-1", false)).ColonyUpdate
	oldFence := fenceOf(first)

	// A takeover on ANOTHER Comb, with a NEW operation id and an explicit
	// override. This is the whole recovery story: no TTL, so a wedged
	// coordinator is displaced by somebody deliberately taking over.
	second := log.mustSucceed(acquireColonyUpdateCmd("op-2", "comb-b", "boot-9", true)).ColonyUpdate
	if !second.GetTakeover() {
		t.Error("a takeover grant does not record itself as one")
	}
	if second.GetFenceToken() <= first.GetFenceToken() {
		t.Errorf("takeover fence token %d is not strictly above the displaced token %d - a takeover that does not raise the token fences nothing",
			second.GetFenceToken(), first.GetFenceToken())
	}

	t.Run("the displaced holder cannot advance", func(t *testing.T) {
		if err := log.mustRefuse(advanceColonyUpdateCmd(oldFence, "issue-restart", "drone", "still going", nil)); !strings.Contains(err, "comb-b") {
			t.Errorf("displaced advance = %q, want a refusal naming the NEW holder", err)
		}
	})
	t.Run("the displaced holder cannot settle the new operation", func(t *testing.T) {
		if err := log.mustRefuse(releaseColonyUpdateCmd(oldFence, colonyOutcomeConfirmed, "writing itself off as done")); !strings.Contains(err, "comb-b") {
			t.Errorf("displaced release = %q, want a refusal naming the NEW holder", err)
		}
	})
	t.Run("the displaced holder cannot acquire a restart lease", func(t *testing.T) {
		// This is the row that makes an explicit takeover SAFE. Without
		// it a wrongful takeover would leave two coordinators each free
		// to restart a different Comb, which is the exact disaster.
		err := log.mustRefuse(fencedAcquireRestartLeaseCmd("apiary_raftd", "drone", oldFence, false))
		if !strings.Contains(err, "comb-b") {
			t.Errorf("displaced restart-lease acquisition = %q, want a refusal naming the NEW holder", err)
		}
		// And force does not rescue it: a fence mismatch is the absence
		// of permission, not a cost to acknowledge.
		forced := log.mustRefuse(fencedAcquireRestartLeaseCmd("apiary_raftd", "drone", oldFence, true))
		if forced == "" {
			t.Error("force rescued a fence mismatch; it must not be overridable")
		}
	})
	t.Run("the displaced record is retained and marked unobserved", func(t *testing.T) {
		byID, ok := log.fsm.ColonyUpdateByID("op-1")
		if !ok {
			t.Fatal("the displaced operation's record was deleted; it must be retained as history")
		}
		if byID.GetActive() {
			t.Error("the displaced record is still active - two active entries would break the single-flight invariant")
		}
		// UNOBSERVED, not failed and not confirmed. The displaced holder
		// may well still be running; nobody has looked; and the higher
		// token is exactly why nobody needs to. "We did not check" is
		// what this means, and internal/cluster's own convention insists
		// it never be folded into either of the other two.
		if byID.GetOutcome() != colonyOutcomeUnobserved {
			t.Errorf("displaced record outcome = %q, want %q - its fate really is unknown", byID.GetOutcome(), colonyOutcomeUnobserved)
		}
	})
	t.Run("the new holder can do its work", func(t *testing.T) {
		log.mustSucceed(advanceColonyUpdateCmd(fenceOf(second), "issue-restart", "drone", "proceeding", nil))
		log.mustSucceed(fencedAcquireRestartLeaseCmd("apiary_raftd", "drone", fenceOf(second), false))
	})
}

func fencedAcquireRestartLeaseCmd(service, nodeID string, fence *internalpb.ColonyUpdateFence, force bool) *internalpb.Command {
	return &internalpb.Command{Op: &internalpb.Command_AcquireRestartLease{
		AcquireRestartLease: &internalpb.AcquireRestartLease{
			Service:           service,
			NodeId:            nodeID,
			RequestedAtUnix:   1_700_000_000,
			VoterNodeIds:      []string{"comb-a", "comb-b", "comb-c", "comb-d"},
			CooldownSeconds:   600,
			Force:             force,
			ColonyUpdateFence: fence,
		},
	}}
}

// TestFSM_AcquireRestartLease_AnUnfencedLeaseIsUnchanged is the
// regression guard for ADR-0103's existing behaviour. The fence is
// OPTIONAL precisely so the ordinary operator-driven per-Comb restart -
// the Machine page's restart button - is untouched by any of this, and
// "untouched" has to be proven rather than asserted.
func TestFSM_AcquireRestartLease_AnUnfencedLeaseIsUnchanged(t *testing.T) {
	log := newFSMLog(t, NewFSM())

	// No colony update is running at all, and an unfenced lease still
	// grants. This is the pre-existing path.
	log.mustSucceed(fencedAcquireRestartLeaseCmd("apiary_raftd", "drone", nil, false))
	// The ordinary block still works.
	if err := log.mustRefuse(fencedAcquireRestartLeaseCmd("apiary_raftd", "buzz", nil, false)); !strings.Contains(err, "unconfirmed restart lease") {
		t.Errorf("a second unfenced lease = %q, want ADR-0103's own unconfirmed-lease refusal", err)
	}
	// And force still overrides it.
	log.mustSucceed(fencedAcquireRestartLeaseCmd("apiary_raftd", "buzz", nil, true))
}

// TestFSM_AcquireRestartLease_FenceMismatchIsRefused is the
// fail-closed half: a fence that names an operation nobody is running is
// refused, not treated as "no fence supplied".
func TestFSM_AcquireRestartLease_FenceMismatchIsRefused(t *testing.T) {
	log := newFSMLog(t, NewFSM())

	refused := log.mustRefuse(fencedAcquireRestartLeaseCmd("apiary_raftd", "drone", &internalpb.ColonyUpdateFence{
		OperationId: "op-9", HolderNodeId: "comb-a", FenceToken: 7, HolderIncarnation: "boot-1",
	}, false))
	if !strings.Contains(refused, "no controlled update is currently in progress") {
		t.Errorf("a fence for a non-existent operation = %q, want a refusal that says nothing is running", refused)
	}
}

// TestFSM_ColonyUpdate_SurvivesSnapshotRestoreIntoAFreshFSM is the
// "ownership is durable, not in-process memory" requirement, at the
// level it can honestly be tested: a brand-new FSM with no shared
// memory, no live process and no reference to the one that wrote the
// state, reading the record back.
//
// This is the snapshot path, not a process restart - a real process
// restart is TestColonyUpdate_ReplacementProcessObservesTheSameHolder
// in colonyupdate_cluster_test.go, which kills a raft node. Both matter
// and neither substitutes for the other: a snapshot restore is a
// DIFFERENT code path (FSM.Restore), and one of the two is routinely
// skipped when only the other is tested.
func TestFSM_ColonyUpdate_SurvivesSnapshotRestoreIntoAFreshFSM(t *testing.T) {
	fsm := NewFSM()
	log := newFSMLog(t, fsm)
	rec := log.mustSucceed(acquireColonyUpdateCmd("op-1", "comb-a", "boot-1", false)).ColonyUpdate
	fence := fenceOf(rec)
	log.mustSucceed(advanceColonyUpdateCmd(fence, "step-aside", "buzz", "", confirmedStep(0, "step-aside", "buzz", "leadership moved")))

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
		t.Fatalf("Restore() into a fresh FSM error: %v", err)
	}

	active, history := replacement.ColonyUpdateState()
	if active == nil {
		t.Fatal("a fresh FSM restored from a snapshot reports no active controlled update - the lock did not survive")
	}
	// Same holder, same fence, same progress. A replacement that saw a
	// DIFFERENT holder here would be a replacement that could act on
	// somebody else's operation.
	if active.GetHolderNodeId() != "comb-a" || active.GetHolderIncarnation() != "boot-1" || active.GetFenceToken() != rec.GetFenceToken() {
		t.Errorf("restored holder = %s/%s at token %d, want comb-a/boot-1 at token %d",
			active.GetHolderNodeId(), active.GetHolderIncarnation(), active.GetFenceToken(), rec.GetFenceToken())
	}
	if active.GetTargetNodeId() != "buzz" || active.GetStep() != "step-aside" || len(active.GetSteps()) != 1 {
		t.Errorf("restored progress = target %q step %q with %d step record(s), want buzz/step-aside with 1",
			active.GetTargetNodeId(), active.GetStep(), len(active.GetSteps()))
	}
	if len(history) != 0 {
		t.Errorf("restored history = %d entries, want 0 while the operation is still active", len(history))
	}
	// And the restored fence is still the live one: the restored FSM
	// accepts it and refuses a stale token, so a replacement process is
	// fenced by exactly the same rules its predecessor was.
	if err := replacement.applyFenceCheck(&internalpb.ColonyUpdateFence{
		OperationId: "op-1", HolderNodeId: "comb-a", FenceToken: rec.GetFenceToken() - 1, HolderIncarnation: "boot-1",
	}); err == nil {
		t.Error("a restored FSM accepted a stale fence token")
	}
}

// applyFenceCheck is the test-side of the fence rule, expressed the same
// way the FSM expresses it: through a real AcquireRestartLease apply,
// not by calling the helper directly. A test that called
// fenceMatchesActive would prove the helper works; this proves the FSM
// still USES it after a restore.
func (f *FSM) applyFenceCheck(stale *internalpb.ColonyUpdateFence) error {
	payload, err := proto.Marshal(fencedAcquireRestartLeaseCmd("apiary_raftd", "drone", stale, false))
	if err != nil {
		return err
	}
	res := f.Apply(&raft.Log{Index: 9_000, Data: payload}).(*FSMApplyResult)
	if res.Error == "" {
		return nil
	}
	return errors.New(res.Error)
}

// TestFSM_ColonyUpdateHistoryIsBoundedAndEvictsOldestFirst keeps
// replicated state from growing without limit. The rule is lowest fence
// token first, which is deterministic - every replica computes the same
// survivors from the same state with no clock and no coordination, and
// getting that wrong would surface only as an ADR-0143 digest mismatch.
func TestFSM_ColonyUpdateHistoryIsBoundedAndEvictsOldestFirst(t *testing.T) {
	fsm := NewFSM()
	log := newFSMLog(t, fsm)

	// One more than the cap, each a full acquire/release cycle so each
	// leaves a settled record behind.
	for i := 0; i <= maxSettledColonyUpdates; i++ {
		opID := "op-" + string(rune('a'+i/26)) + string(rune('a'+i%26))
		rec := log.mustSucceed(acquireColonyUpdateCmd(opID, "comb-a", "boot-1", i > 0)).ColonyUpdate
		log.mustSucceed(releaseColonyUpdateCmd(fenceOf(rec), colonyOutcomeConfirmed, "settled"))
	}

	_, history := fsm.ColonyUpdateState()
	if len(history) != maxSettledColonyUpdates {
		t.Errorf("settled history = %d entries, want the cap of %d", len(history), maxSettledColonyUpdates)
	}
	// Newest first, and the oldest is the one that went.
	if history[0].GetFenceToken() <= history[len(history)-1].GetFenceToken() {
		t.Error("history is not ordered newest-first by fence token")
	}
	if _, ok := fsm.ColonyUpdateByID("op-aa"); ok {
		t.Error("the oldest settled operation survived the cap; eviction is supposed to drop the lowest fence token first")
	}
}

// TestFSM_ColonyUpdate_ActiveRecordIsNeverEvicted is the other half of
// the cap, and the one that matters: the lock must never be something
// housekeeping can remove.
//
// The shape here is deliberate. Eviction can only ever be tested with a
// full history and a live lock in the same state, and a takeover always
// displaces whatever was live - so the live record is taken LAST, after
// the history is already over its cap. If the cap were implemented by a
// sweep that did not exclude the active record, this is the state that
// would expose it.
func TestFSM_ColonyUpdate_ActiveRecordIsNeverEvicted(t *testing.T) {
	fsm := NewFSM()
	log := newFSMLog(t, fsm)

	// Well past the cap, so eviction has demonstrably run many times.
	for i := 0; i < maxSettledColonyUpdates*2; i++ {
		opID := "op-" + string(rune('a'+i/26)) + string(rune('a'+i%26))
		rec := log.mustSucceed(acquireColonyUpdateCmd(opID, "comb-a", "boot-1", i > 0)).ColonyUpdate
		log.mustSucceed(releaseColonyUpdateCmd(fenceOf(rec), colonyOutcomeConfirmed, "settled"))
	}
	live := log.mustSucceed(acquireColonyUpdateCmd("op-live", "comb-b", "boot-9", false)).ColonyUpdate

	active, history := fsm.ColonyUpdateState()
	if active == nil {
		t.Fatal("the active record was evicted by housekeeping - the single-flight must never be something a cap can remove")
	}
	if active.GetOperationId() != "op-live" || active.GetFenceToken() != live.GetFenceToken() {
		t.Errorf("active = %q at token %d, want op-live at token %d", active.GetOperationId(), active.GetFenceToken(), live.GetFenceToken())
	}
	if len(history) != maxSettledColonyUpdates {
		t.Errorf("settled history alongside a live lock = %d, want the cap of %d", len(history), maxSettledColonyUpdates)
	}
	// The active record is in neither the history nor its own right, and
	// in particular a reader must not be able to find the live lock
	// among the settled ones.
	for _, rec := range history {
		if rec.GetActive() {
			t.Errorf("an active record (%q) is present in the settled history", rec.GetOperationId())
		}
	}
}

// TestColonyUpdateOutcomeVocabularyMatchesRestartplan is the
// cross-package agreement test. The strings in this package are a
// restatement of internal/restartplan's Outcome constants rather than an
// import, and a restatement can drift; this asserts against the real
// constants so a rename there cannot silently leave this package
// accepting a word nothing else recognises.
//
// It lives here rather than in internal/restartplan because
// internal/manager imports BOTH, so a test there importing back would
// be an import cycle Go rejects outright - the same constraint
// internal/manager/restartplan_agreement_test.go records.
func TestColonyUpdateOutcomeVocabularyMatchesRestartplan(t *testing.T) {
	cases := []struct {
		got  string
		want restartplan.Outcome
	}{
		{colonyOutcomeConfirmed, restartplan.OutcomeConfirmed},
		{colonyOutcomeFailed, restartplan.OutcomeFailed},
		{colonyOutcomeBlocked, restartplan.OutcomeBlocked},
		{colonyOutcomeUnobserved, restartplan.OutcomeUnobserved},
		{colonyOutcomeUnverified, restartplan.OutcomeUnverified},
	}
	for _, tc := range cases {
		if tc.got != string(tc.want) {
			t.Errorf("outcome %q does not match restartplan's %q", tc.got, tc.want)
		}
	}
	// And the exemption rule must agree too, or a step record this
	// package accepts could still be refused by a Result written from
	// the same facts.
	for _, o := range []restartplan.Outcome{restartplan.OutcomeUnobserved, restartplan.OutcomeUnverified, restartplan.OutcomeBlocked} {
		if outcomeRequiresEvidence(string(o)) {
			t.Errorf("restartplan treats %q as evidence-free, but this package would demand evidence for it", o)
		}
	}
	for _, o := range []restartplan.Outcome{restartplan.OutcomeConfirmed, restartplan.OutcomeFailed} {
		if !outcomeRequiresEvidence(string(o)) {
			t.Errorf("restartplan demands evidence for %q, but this package would accept it without any", o)
		}
	}
}
