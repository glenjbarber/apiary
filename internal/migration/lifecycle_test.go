package migration

import (
	"strings"
	"testing"
	"time"
)

// TestFullMigrationLifecycle walks one migration from an operator's
// request to an observed completion, through the real Apply path and
// the real quorum gate, with every step's outcome asserted. It is the
// end-to-end proof that the pieces fit together: if any of them drift
// apart, this is what notices.
func TestFullMigrationLifecycle(t *testing.T) {
	st := *NewMigrationState()
	rec := func() Record {
		t.Helper()
		r, ok := st.Lookup("mig-life")
		if !ok {
			t.Fatal("record missing from state")
		}
		return r
	}
	apply := func(c Command) {
		t.Helper()
		next, err := Apply(st, c)
		requireNoError(t, err, "apply "+string(c.Kind)+" to "+string(c.To))
		st = next
	}
	advance := func(to Phase) {
		t.Helper()
		r := rec()
		apply(Command{
			Kind:   CommandAdvance,
			ID:     "mig-life",
			To:     to,
			Token:  r.Token + 100,
			Guest:  testGuest(),
			Detail: "driver reached " + string(to),
			AtUnix: r.Timestamps.UpdatedUnix + stepSeconds,
		})
	}

	// 1. Preflight passes, quorum is held, and the guest is fenced.
	q := GateOnQuorum(QuorumFact{Fact: quorumSurvives(true), ReadOK: true, ObservedAt: baseUnix})
	if !q.Proceed {
		t.Fatalf("quorum gate refused to start: %s", q.Detail)
	}
	apply(Command{
		Kind: CommandStart, ID: "mig-life",
		Guest: testGuest(), SourceID: testSource, TargetID: testTarget,
		Token: testToken1, AtUnix: baseUnix,
	})
	if !st.FenceFor(testGuest()).Blocked {
		t.Fatal("the guest is not fenced immediately after a start; the ordinary reconciler could start it")
	}
	if r := rec(); r.Timestamps.FirstEnteredUnix[PhasePreflight] != baseUnix {
		t.Errorf("preflight first-entered = %d, want %d - the start time is the first thing an operator asks about a stuck migration",
			r.Timestamps.FirstEnteredUnix[PhasePreflight], baseUnix)
	}

	// 2. Freeze. The guest is now an outage, and the bounded-freeze
	//    budget starts running.
	advance(PhaseFreeze)
	if !rec().Frozen() {
		t.Fatal("after freeze the guest does not report as frozen")
	}

	// 3. Bulk. A genuine copy this time (not the HAST-replica no-op).
	r := rec()
	apply(Command{
		Kind: CommandAdvance, ID: "mig-life", To: PhaseBulk, Token: r.Token + 100,
		AtUnix:   r.Timestamps.UpdatedUnix + stepSeconds,
		Transfer: &Transfer{RequiredBytes: 4096},
	})
	r = rec()
	apply(Command{
		Kind: CommandAdvance, ID: "mig-life", To: PhaseBulk, Retry: true, Token: r.Token + 100,
		AtUnix:   r.Timestamps.UpdatedUnix + stepSeconds,
		Transfer: &Transfer{RequiredBytes: 4096, MovedBytes: 1500, FromToken: 7, ToToken: 7},
	})
	r = rec()
	if r.Attempt != 4 {
		t.Errorf("attempt after preflight, freeze, bulk and one bulk retry = %d, want 4", r.Attempt)
	}
	if r.Timestamps.FirstEnteredUnix[PhaseBulk] != r.Timestamps.LastEnteredUnix[PhaseBulk]-stepSeconds {
		t.Error("the bulk retry overwrote the phase's first-entered time; a retried guest must not look less-frozen than it is")
	}

	// 4. The link stalls. The record says so, by name.
	d := GateOnQuorum(QuorumFact{ReadOK: false, ReadError: "raft status unavailable", ObservedAt: r.Timestamps.UpdatedUnix})
	if d.Proceed {
		t.Fatal("a stalled link was allowed to advance")
	}
	r, err := StallFromDecision(rec(), d, r.Timestamps.UpdatedUnix+1)
	requireNoError(t, err, "stall from the gate")
	next, err := storeRecord(st, r)
	requireNoError(t, err, "store the stalled record")
	st = next
	if st.FenceFor(testGuest()).Detail == "" {
		t.Error("a stalled migration produced no fence detail for the operator")
	}

	// 5. Sync, then cutover.
	advance(PhaseSync)
	advance(PhaseCutover)
	if !rec().OwnershipMoved() {
		t.Error("cutover did not move ownership")
	}
	// Abort is now refused: the guest belongs to the target.
	if _, err := Apply(st, Command{Kind: CommandAbort, ID: "mig-life", Token: rec().Token, AbortReason: "too late", AtUnix: baseUnix + 900}); !errorsIs(err, ErrAbortTooLate) {
		t.Errorf("abort after cutover: err = %v, want ErrAbortTooLate", err)
	}

	// 6. The target answers, and the answer is checked rather than
	//    believed.
	observedAt := baseUnix + 1000
	apply(Command{
		Kind: CommandSettle, ID: "mig-life", To: "", Token: rec().Token,
		Verification: Verification{
			Observed: true, GuestRunning: true,
			ObservedGUID: "guid-abc", ExpectedGUID: "guid-abc",
			ObservedIP: "10.0.0.5", ExpectedIP: "10.0.0.5",
			ObservedAt: observedAt, ObservedOn: testTarget,
		},
		Evidence: observedEvidence(observedAt),
		Detail:   "target comb-b reported the guest running",
		AtUnix:   observedAt,
	})

	done := rec()
	if !done.Outcome.IsSuccess() {
		t.Fatalf("final outcome = %s, want an observed success", done.Outcome)
	}
	if done.Detail == "" || len(done.Evidence) == 0 {
		t.Error("the completed record lost its reason or its evidence")
	}
	if st.FenceFor(testGuest()).Blocked {
		t.Error("the guest is still fenced after an observed completion")
	}
	if _, ok := st.FencingFor(testGuest()); ok {
		t.Error("the guest index still holds a resolved migration")
	}
	if _, ok := st.Lookup("mig-life"); !ok {
		t.Error("the completed record was dropped; it is history, not garbage")
	}

	// The measured downtime is real and non-zero, and it ends when the
	// target said the guest came back.
	down, ok := CutoverDowntime(done, time.Unix(observedAt+10, 0).UTC())
	if !ok {
		t.Error("a completed migration reported no downtime measurement")
	}
	if down <= 0 {
		t.Errorf("downtime = %v; ADR-0128 promises bounded, not zero", down)
	}
}

// storeRecord puts a Record the test built through the mutation
// functions directly back into state, the way the FSM's apply path
// does after validating. It is a test-only convenience; production
// writes go through Apply, which validates the command first.
func storeRecord(s MigrationState, r Record) (MigrationState, error) {
	if err := r.Validate(); err != nil {
		return s, err
	}
	return s.withMigration(r), nil
}

// TestHASTReplicaTargetIsANoOpBulkNotASkippedPhase: the one case that
// looks like an exception to the no-skipping rule, asserted explicitly.
// The target already holds the data, so there is nothing to receive -
// but the record still goes through bulk, because a journal that could
// skip a step could not answer whether the guest was ever frozen.
func TestHASTReplicaTargetIsANoOpBulkNotASkippedPhase(t *testing.T) {
	st := *NewMigrationState()
	c := startCmd()
	next, err := Apply(st, c)
	requireNoError(t, err, "start")
	st = next

	r, _ := st.Lookup(c.ID)
	next, err = Apply(st, Command{Kind: CommandAdvance, ID: c.ID, To: PhaseFreeze, Token: r.Token + 10, AtUnix: baseUnix + 60})
	requireNoError(t, err, "advance to freeze")
	st = next

	r, _ = st.Lookup(c.ID)
	bulk := Command{
		Kind: CommandAdvance, ID: c.ID, To: PhaseBulk, Token: r.Token + 10,
		AtUnix:   baseUnix + 120,
		Transfer: &Transfer{RequiredBytes: 0, TargetAlreadyReplica: true},
	}
	next, err = Apply(st, bulk)
	requireNoError(t, err, "advance to a no-op bulk")
	st = next

	r, _ = st.Lookup(c.ID)
	if r.Phase != PhaseBulk {
		t.Errorf("phase = %s, want %s - bulk is entered, not skipped", r.Phase, PhaseBulk)
	}
	if _, ok := r.Timestamps.FirstEnteredUnix[PhaseBulk]; !ok {
		t.Error("a no-op bulk was not timestamped; an un-entered phase and a skipped one are indistinguishable in the journal")
	}
	if r.Transfer.Complete() {
		t.Error("a zero-byte bulk reported itself Complete")
	}
	if !r.Transfer.TargetAlreadyReplica {
		t.Error("the record does not say why the bulk was empty, so it cannot be told apart from one that moved nothing")
	}
	if r.Frozen() != true {
		t.Error("a record at bulk does not report the guest as frozen")
	}
}

// TestTimestampsNormalizeOnANilMap: a Record assembled by hand (or by
// a decoder that left the maps out entirely) must not panic on its
// first timestamp write. The bug this guards is a nil-map assignment,
// which in a raft FSM is a panic on one node and a divergence on all.
func TestTimestampsNormalizeOnANilMap(t *testing.T) {
	r := Record{
		ID: "mig-nilmap", Guest: testGuest(), SourceID: testSource, TargetID: testTarget,
		Phase: PhaseBulk, Outcome: OutcomeInProgress, Attempt: 2, Token: 42,
		Timestamps: Timestamps{CreatedUnix: baseUnix, UpdatedUnix: baseUnix},
	}
	if r.Timestamps.FirstEnteredUnix != nil {
		t.Fatal("fixture did not actually have nil timestamp maps")
	}
	got, err := RecordVerification(r, Verification{Observed: false, QueryError: "dial timeout"})
	requireNoError(t, err, "record verification against nil maps")
	if got.Timestamps.FirstEnteredUnix == nil {
		t.Error("the timestamp maps were still nil after a write")
	}
	stalled, err := Stall(got, Stalled{Reason: "target-unreachable", Detail: "dial timed out"}, baseUnix+1)
	requireNoError(t, err, "stall against nil maps")

	// A wire decode is the other way a nil map arrives, and it must
	// survive Validate with the maps populated.
	w, err := ToWire(stalled)
	requireNoError(t, err, "to wire")
	back, err := FromWire(w)
	requireNoError(t, err, "from wire")
	if back.Timestamps.FirstEnteredUnix == nil {
		t.Error("a decoded record has nil timestamp maps")
	}
}

// TestOutcomeVerdictWordsNeverConflateTheUnknowns: the short strings
// a UI puts on a badge. They must be distinguishable from each other
// and from the positive outcomes.
func TestOutcomeVerdictWordsNeverConflateTheUnknowns(t *testing.T) {
	words := map[Outcome]string{}
	for _, o := range []Outcome{
		OutcomeInProgress, OutcomeObservedComplete, OutcomeFailed,
		OutcomeUnobserved, OutcomeUnverified, OutcomeAborted,
	} {
		words[o] = o.Verdict()
		if words[o] == "" {
			t.Errorf("outcome %s renders as an empty string", o)
		}
		if strings.Contains(words[o], "unrecognized") {
			t.Errorf("recognized outcome %s rendered as unrecognized", o)
		}
	}
	seen := map[string]Outcome{}
	for o, w := range words {
		if other, dup := seen[w]; dup {
			t.Errorf("outcomes %s and %s render identically as %q", other, o, w)
		}
		seen[w] = o
	}
	// The word a UI would colour green must only ever belong to the
	// one outcome that is a confirmed success.
	for _, o := range []Outcome{OutcomeFailed, OutcomeUnobserved, OutcomeUnverified, OutcomeAborted, OutcomeInProgress} {
		if strings.Contains(words[o], "complete") {
			t.Errorf("outcome %s renders with the word 'complete': %q", o, words[o])
		}
	}
	if !strings.Contains(words[OutcomeObservedComplete], "observed") {
		t.Errorf("the success word does not say it was observed: %q", words[OutcomeObservedComplete])
	}
}

// TestConcurrentMigrationsOfDifferentGuestsDoNotInterfere: the fence
// and the index are per guest. A second guest moving at the same time
// must be entirely unaffected, because the blast radius of a migration
// is exactly one guest.
func TestConcurrentMigrationsOfDifferentGuestsDoNotInterfere(t *testing.T) {
	st := *NewMigrationState()

	a := startCmd()
	a.ID = "mig-a"
	a.Guest = Workload{Kind: WorkloadKindVM, ID: "vm-a"}
	next, err := Apply(st, a)
	requireNoError(t, err, "start vm-a")
	st = next

	b := startCmd()
	b.ID = "mig-b"
	b.Guest = jailGuest()
	b.Token = testToken8
	next, err = Apply(st, b)
	requireNoError(t, err, "start the jail")
	st = next

	if len(st.GuestIndex) != 2 {
		t.Errorf("guest index has %d entries, want 2: %v", len(st.GuestIndex), st.SortedGuestKeys())
	}
	for _, g := range []Workload{{Kind: WorkloadKindVM, ID: "vm-a"}, jailGuest()} {
		if d := st.FenceFor(g); !d.Blocked {
			t.Errorf("%s is not fenced by its own migration", g)
		}
	}
	if d := st.FenceFor(Workload{Kind: WorkloadKindVM, ID: "vm-untouched"}); d.Blocked {
		t.Errorf("an unrelated guest is fenced: %s", d.Detail)
	}

	// Aborting one leaves the other alone.
	ra, _ := st.Lookup("mig-a")
	next, err = Apply(st, Command{
		Kind: CommandAbort, ID: "mig-a", Token: ra.Token,
		AbortReason: "the machine came back before we needed to move it", AtUnix: baseUnix + 300,
	})
	requireNoError(t, err, "abort vm-a")
	st = next

	if d := st.FenceFor(Workload{Kind: WorkloadKindVM, ID: "vm-a"}); d.Blocked {
		t.Errorf("vm-a is still fenced after its migration aborted: %s", d.Detail)
	}
	if d := st.FenceFor(jailGuest()); !d.Blocked {
		t.Error("aborting vm-a released the unrelated jail")
	}
	if _, ok := st.Lookup("mig-b"); !ok {
		t.Error("aborting vm-a removed the jail's record")
	}
}
