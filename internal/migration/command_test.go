package migration

import (
	"reflect"
	"strings"
	"testing"
)

// sameWire compares two stored records field for field. WireRecord holds
// a slice, so == does not compile, and a test that asserted state was
// unchanged by comparing only a couple of scalars would miss exactly the
// field a rejected command is most likely to touch.
func sameWire(t *testing.T, got, want WireRecord) bool {
	t.Helper()
	return reflect.DeepEqual(got, want)
}

// startCmd is the canonical, valid start command. Tests clone and
// corrupt it rather than building their own, so a change to what a
// start must contain is visible in every test at once.
func startCmd() Command {
	return Command{
		Kind:     CommandStart,
		ID:       "mig-cmd",
		Guest:    testGuest(),
		SourceID: testSource,
		TargetID: testTarget,
		Token:    testToken1,
		Detail:   "operator requested a move off comb-a",
		AtUnix:   baseUnix,
	}
}

// stateWithMigration returns a state holding one in-flight record at
// the given phase, built by walking the real Apply path so the fixture
// cannot drift from what the code actually produces.
func stateWithMigration(t *testing.T, id string, phase Phase) (MigrationState, Command, Record) {
	t.Helper()
	st := *NewMigrationState()
	cmds := []Command{startCmd()}
	cmds[0].ID = id
	next, err := Apply(st, cmds[0])
	requireNoError(t, err, "apply start")
	st = next

	rec, ok := st.Lookup(id)
	if !ok {
		t.Fatalf("record %s is not in state after a successful start", id)
	}
	tokens := []uint64{testToken2, testToken3, testToken4, testToken5, testToken6, testToken7}
	target := phase.Index()
	for i := 1; i <= target; i++ {
		rec, _ = st.Lookup(id)
		c := Command{
			Kind:   CommandAdvance,
			ID:     id,
			To:     PhaseOrder[i],
			Token:  tokens[i-1],
			Guest:  testGuest(),
			AtUnix: rec.Timestamps.UpdatedUnix + stepSeconds,
		}
		next, err := Apply(st, c)
		requireNoError(t, err, "apply advance to "+string(PhaseOrder[i]))
		st = next
		cmds = append(cmds, c)
	}
	rec, _ = st.Lookup(id)
	return st, cmds[len(cmds)-1], rec
}

// TestValidCommandsAreAccepted is the baseline: without it, a suite of
// rejection tests would pass against a gate that rejects everything.
func TestValidCommandsAreAccepted(t *testing.T) {
	st, _, rec := stateWithMigration(t, "mig-ok", PhaseBulk)
	if rec.Phase != PhaseBulk {
		t.Fatalf("fixture phase = %s, want %s", rec.Phase, PhaseBulk)
	}
	if d := st.FenceFor(testGuest()); !d.Blocked {
		t.Error("an in-flight migration did not fence its guest")
	}
	if held, ok := st.FencingFor(testGuest()); !ok {
		t.Error("the guest index has no entry for an in-flight migration")
	} else if held.ID != "mig-ok" {
		t.Errorf("the guest index points at %s, want mig-ok", held.ID)
	}

	// Settling to an unobserved completion is a valid command and is
	// the one that must not release the fence.
	settle := Command{
		Kind:         CommandSettle,
		ID:           "mig-ok",
		Guest:        testGuest(),
		Token:        rec.Token,
		Verification: Verification{Observed: false, QueryError: "dial tcp comb-b: i/o timeout"},
		Detail:       "the target never answered",
		AtUnix:       baseUnix + 500,
	}
	next, err := Apply(st, settle)
	requireNoError(t, err, "apply an unobserved settle")
	if d := next.FenceFor(testGuest()); !d.Blocked {
		t.Error("an unobserved settle released the fence")
	}
	if held, ok := next.FencingFor(testGuest()); !ok {
		t.Error("a settled-with-unknown record released the guest index; the guest is no longer fenced")
	} else if held.Outcome != OutcomeUnobserved {
		t.Errorf("the guest index points at a record with outcome %s, want %s", held.Outcome, OutcomeUnobserved)
	}
	if _, ok := next.Lookup("mig-ok"); !ok {
		t.Error("a settled record was dropped from the record map; it is history, not garbage")
	}
}

// TestReplayedStartIsRejectedAsDuplicate: the same start command
// delivered twice must not create a second record, and must not be
// absorbed as a silent no-op either.
func TestReplayedStartIsRejectedAsDuplicate(t *testing.T) {
	st := *NewMigrationState()
	cmd := startCmd()
	first, err := Apply(st, cmd)
	requireNoError(t, err, "first start")

	second, err := Apply(first, cmd)
	requireSentinel(t, err, ErrDuplicateCommand, "replayed start")
	if err != nil && !strings.Contains(err.Error(), "already exists") {
		t.Errorf("replayed start error does not say the record already exists: %v", err)
	}
	if len(second.Migrations) != 1 {
		t.Errorf("a replayed start produced %d records, want 1", len(second.Migrations))
	}
	if !sameWire(t, second.Migrations[cmd.ID], first.Migrations[cmd.ID]) {
		t.Error("a rejected start modified the stored record")
	}
	if len(second.GuestIndex) != 1 {
		t.Errorf("a rejected start left %d guest index entries, want 1", len(second.GuestIndex))
	}
}

// TestSecondStartForTheSameGuestIsRejected: a different record id does
// not make a second migration of the same guest a new idea. The guest
// index exists precisely so this is a point read.
func TestSecondStartForTheSameGuestIsRejected(t *testing.T) {
	st, _, _ := stateWithMigration(t, "mig-first", PhaseFreeze)
	second := startCmd()
	second.ID = "mig-second"
	second.Token = testToken8

	out, err := Apply(st, second)
	requireSentinel(t, err, ErrGuestAlreadyMigrating, "second start for the same guest")
	if !strings.Contains(err.Error(), "mig-first") {
		t.Errorf("the rejection does not name the live record: %v", err)
	}
	if len(out.Migrations) != 1 {
		t.Errorf("the rejected start created a record: %d present", len(out.Migrations))
	}
	if len(out.GuestIndex) != 1 {
		t.Errorf("the rejected start touched the guest index: %d entries", len(out.GuestIndex))
	}
}

// TestReplayedAdvanceIsRejectedAsDuplicate: re-entering the current
// phase without Retry is a duplicate, and it must be labelled as one
// rather than as an illegal transition, because the two demand
// different responses from the caller.
func TestReplayedAdvanceIsRejectedAsDuplicate(t *testing.T) {
	st, advance, rec := stateWithMigration(t, "mig-replay-adv", PhaseBulk)

	out, err := Apply(st, advance)
	requireSentinel(t, err, ErrDuplicateCommand, "replayed advance")
	if !strings.Contains(err.Error(), "already at") {
		t.Errorf("duplicate advance error does not say the phase was already reached: %v", err)
	}
	if !sameWire(t, out.Migrations[advance.ID], st.Migrations[advance.ID]) {
		t.Error("a rejected advance modified the record")
	}
	stillThere, _ := out.Lookup("mig-replay-adv")
	if stillThere.Attempt != rec.Attempt {
		t.Errorf("a rejected advance bumped the attempt from %d to %d", rec.Attempt, stillThere.Attempt)
	}
	if stillThere.Token != rec.Token {
		t.Errorf("a rejected advance changed the token")
	}
}

// TestStaleTokenIsRejectedBeforeAnythingElse: a delayed report from a
// superseded attempt must be rejected as superseded, and never as
// anything else - otherwise the attempt token is not load-bearing and a
// caller debugging "illegal transition" is sent looking in the wrong
// place.
func TestStaleTokenIsRejectedBeforeAnythingElse(t *testing.T) {
	st, _, rec := stateWithMigration(t, "mig-stale", PhaseBulk)

	cases := []struct {
		name string
		cmd  Command
		sent string
	}{
		{
			name: "advance with a superseded token",
			cmd:  Command{Kind: CommandAdvance, ID: "mig-stale", To: PhaseSync, Token: rec.Token - 1, AtUnix: baseUnix + 400},
			sent: "advance",
		},
		{
			// A settle does not mint a token, so a settle carrying a
			// token the record has not reached is as superseded as one
			// carrying a token it has moved past: the difference is only
			// which direction, and neither is the current attempt.
			name: "settle with a token the record has not reached",
			cmd:  Command{Kind: CommandSettle, ID: "mig-stale", Token: rec.Token + 500, FailureDetail: "zfs recv failed", AtUnix: baseUnix + 400},
			sent: "settle",
		},
		{
			name: "settle with a superseded token",
			cmd:  Command{Kind: CommandSettle, ID: "mig-stale", Token: rec.Token - 1, FailureDetail: "zfs recv failed", AtUnix: baseUnix + 400},
			sent: "settle",
		},
		{
			name: "abort with a superseded token",
			cmd:  Command{Kind: CommandAbort, ID: "mig-stale", Token: rec.Token - 1, AbortReason: "changed my mind", AtUnix: baseUnix + 400},
			sent: "abort",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Apply(st, tc.cmd)
			requireSentinel(t, err, ErrStaleToken, tc.sent)
			if !strings.Contains(err.Error(), "superseded attempt") {
				t.Errorf("stale-token error does not say the command is from a superseded attempt: %v", err)
			}
		})
	}

	// And a command that presents no token at all is refused outright:
	// zero must never match anything.
	for _, kind := range []CommandKind{CommandStart, CommandAdvance, CommandSettle, CommandAbort} {
		c := Command{Kind: kind, ID: "mig-stale", Guest: testGuest(), SourceID: testSource, TargetID: testTarget}
		_, err := Apply(st, c)
		if err == nil {
			t.Errorf("%s with token 0 was accepted", kind)
			continue
		}
		if !strings.Contains(err.Error(), "token 0") {
			t.Errorf("%s with token 0 was rejected for the wrong reason: %v", kind, err)
		}
	}
}

// TestReplayOfARetryIsRejected: the ADR's resume path retries the
// current phase with a fresh token. Delivering that retry twice must be
// caught by the token check, not by the transition rules - the second
// delivery genuinely is a different phase request from the record's
// point of view, and only the token distinguishes it.
func TestReplayOfARetryIsRejected(t *testing.T) {
	st, _, rec := stateWithMigration(t, "mig-retry-replay", PhaseBulk)

	retry := Command{
		Kind:   CommandAdvance,
		ID:     "mig-retry-replay",
		To:     PhaseBulk,
		Retry:  true,
		Token:  rec.Token + 10,
		AtUnix: baseUnix + 400,
	}
	first, err := Apply(st, retry)
	requireNoError(t, err, "first retry")
	if _, err := Apply(first, retry); !errorsIs(err, ErrStaleToken) {
		t.Errorf("a replayed retry: err = %v, want ErrStaleToken (the record's token has moved on)", err)
	}
}

// TestCraftedCommandsAreRejected: a command that names one guest's
// record id and another guest's identity, or that asks for a record to
// be born mid-protocol, must be refused by name.
func TestCraftedCommandsAreRejected(t *testing.T) {
	st, _, rec := stateWithMigration(t, "mig-crafted", PhaseBulk)

	cases := []struct {
		name    string
		cmd     Command
		wantErr error
		wantIn  string
	}{
		{
			name:    "record id of one guest, identity of another",
			cmd:     Command{Kind: CommandAdvance, ID: "mig-crafted", Guest: jailGuest(), To: PhaseSync, Token: rec.Token + 1, AtUnix: baseUnix + 400},
			wantErr: nil,
			wantIn:  "that record is migrating",
		},
		{
			name:    "source node does not match the record",
			cmd:     Command{Kind: CommandAdvance, ID: "mig-crafted", SourceID: "comb-evil", To: PhaseSync, Token: rec.Token + 1, AtUnix: baseUnix + 400},
			wantErr: nil,
			wantIn:  "the record's source is",
		},
		{
			name:    "target node does not match the record",
			cmd:     Command{Kind: CommandAbort, ID: "mig-crafted", TargetID: "comb-evil", Token: rec.Token, AbortReason: "operator asked", AtUnix: baseUnix + 400},
			wantErr: nil,
			wantIn:  "the record's target is",
		},
		{
			name:    "record that does not exist",
			cmd:     Command{Kind: CommandAdvance, ID: "mig-does-not-exist", To: PhaseSync, Token: testToken8, AtUnix: baseUnix + 400},
			wantErr: ErrNoSuchRecord,
			wantIn:  "no such migration record",
		},
		{
			name:    "unrecognized command kind",
			cmd:     Command{Kind: "delete_guest_migration", ID: "mig-crafted", Token: rec.Token, AtUnix: baseUnix + 400},
			wantErr: nil,
			wantIn:  "unrecognized command kind",
		},
		{
			name:    "no record id",
			cmd:     Command{Kind: CommandAdvance, Token: rec.Token + 1, AtUnix: baseUnix + 400},
			wantErr: nil,
			wantIn:  "no record id",
		},
		{
			name:    "nil state",
			cmd:     Command{Kind: CommandAdvance, ID: "mig-crafted", Token: rec.Token + 1, AtUnix: baseUnix + 400},
			wantErr: nil,
			wantIn:  "no state to validate against",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			base := st
			if tc.name == "nil state" {
				err := ValidateCommand(nil, tc.cmd)
				if err == nil || !strings.Contains(err.Error(), tc.wantIn) {
					t.Fatalf("err = %v, want one containing %q", err, tc.wantIn)
				}
				return
			}
			_, err := Apply(base, tc.cmd)
			if err == nil {
				t.Fatalf("crafted command was accepted")
			}
			if tc.wantErr != nil && !errorsIs(err, tc.wantErr) {
				t.Errorf("err = %v, want it to wrap %v", err, tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantIn) {
				t.Errorf("err = %v, want it to name %q", err, tc.wantIn)
			}
		})
	}
}

// TestStartMustCreateARecordAtPreflight: a start that also names a
// phase, a settled outcome, or an abort reason is asking for a record
// that was born mid-protocol or with a history it never had.
func TestStartMustCreateARecordAtPreflight(t *testing.T) {
	for name, mutate := range map[string]func(*Command){
		"names a phase":              func(c *Command) { c.To = PhaseBulk },
		"names a phase at preflight": func(c *Command) { c.To = PhasePreflight },
		"claims an outcome":          func(c *Command) { c.Outcome = OutcomeObservedComplete },
		"carries an abort reason":    func(c *Command) { c.AbortReason = "pre-emptive" },
		"no guest kind":              func(c *Command) { c.Guest.Kind = "container" },
		"no guest id":                func(c *Command) { c.Guest.ID = "  " },
		"no source":                  func(c *Command) { c.SourceID = "" },
		"no target":                  func(c *Command) { c.TargetID = "" },
		"source equals target":       func(c *Command) { c.TargetID = c.SourceID },
	} {
		t.Run(name, func(t *testing.T) {
			c := startCmd()
			mutate(&c)
			st := *NewMigrationState()
			_, err := Apply(st, c)
			if err == nil {
				t.Fatalf("start command with %s was accepted", name)
			}
			if len(st.Migrations) != 0 {
				t.Error("a rejected start wrote to state")
			}
		})
	}
}

// TestSettleCannotClaimAnOutcomeItsEvidenceDoesNotSupport is the
// central honesty check at the command boundary: a caller that believes
// it completed, against evidence that says otherwise, is refused
// outright rather than corrected.
func TestSettleCannotClaimAnOutcomeItsEvidenceDoesNotSupport(t *testing.T) {
	st, _, rec := stateWithMigration(t, "mig-claim", PhaseCutover)
	at := baseUnix + 500

	// Claims success, evidence is a query error.
	lie := Command{
		Kind:         CommandSettle,
		ID:           "mig-claim",
		Token:        rec.Token,
		Outcome:      OutcomeObservedComplete,
		Verification: Verification{Observed: false, QueryError: "dial tcp comb-b: i/o timeout"},
		AtUnix:       at,
	}
	out, err := Apply(st, lie)
	if err == nil {
		t.Fatal("a settle claiming success with no observation was accepted")
	}
	if !errorsIs(err, ErrUnbackedOutcome) {
		t.Errorf("err = %v, want ErrUnbackedOutcome", err)
	}
	if !strings.Contains(err.Error(), "refusing to record a verdict the evidence does not support") {
		t.Errorf("error does not explain itself: %v", err)
	}
	if len(out.Migrations) != 1 {
		t.Error("a rejected settle wrote to state")
	}
	stillThere, _ := out.Lookup("mig-claim")
	if stillThere.Outcome != OutcomeInProgress {
		t.Errorf("a rejected settle changed the outcome to %s", stillThere.Outcome)
	}

	// The honest version of the same command, with the outcome left
	// off entirely, is accepted and lands on the unobserved value.
	honest := lie
	honest.Outcome = ""
	next, err := Apply(st, honest)
	requireNoError(t, err, "honest settle")
	settled, _ := next.Lookup("mig-claim")
	if settled.Outcome != OutcomeUnobserved {
		t.Errorf("honest settle produced %s, want %s", settled.Outcome, OutcomeUnobserved)
	}
	if settled.Outcome.IsSuccess() {
		t.Error("an unobserved settle is being reported as a success")
	}
}

// TestSettleWithNoEvidenceAtAllIsRefused: the command boundary holds
// the same line the outcome layer does.
func TestSettleWithNoEvidenceAtAllIsRefused(t *testing.T) {
	st, _, rec := stateWithMigration(t, "mig-noev", PhaseCutover)
	c := Command{
		Kind:   CommandSettle,
		ID:     "mig-noev",
		Token:  rec.Token,
		AtUnix: baseUnix + 500,
	}
	if _, err := Apply(st, c); err == nil {
		t.Fatal("a settle with no evidence at all was accepted")
	}
}

// TestAbortIsRefusedAtAndAfterCutover is the deliberate narrowing of
// ADR-0128's abort, and the single most safety-relevant rule in this
// file: past cutover the guest's owner is the target, and "return the
// guest to the source" is not an action this protocol defines.
func TestAbortIsRefusedAtAndAfterCutover(t *testing.T) {
	// Before the freeze, abort is allowed and says so.
	for _, p := range []Phase{PhasePreflight, PhaseFreeze} {
		st, _, rec := stateWithMigration(t, "mig-abort-"+string(p), p)
		c := Command{
			Kind:        CommandAbort,
			ID:          "mig-abort-" + string(p),
			Token:       rec.Token,
			AbortReason: "the operator needs the machine back now",
			AtUnix:      baseUnix + 400,
		}
		next, err := Apply(st, c)
		requireNoError(t, err, "abort at "+string(p))
		aborted, _ := next.Lookup("mig-abort-" + string(p))
		if aborted.Outcome != OutcomeAborted {
			t.Errorf("abort at %s produced %s, want %s", p, aborted.Outcome, OutcomeAborted)
		}
		if FenceHeld(aborted) {
			t.Errorf("an aborted record at %s still fences the guest; the operator asked for the guest back", p)
		}
		if d := next.FenceFor(testGuest()); d.Blocked {
			t.Errorf("the guest is still fenced after an abort at %s: %s", p, d.Detail)
		}
		if !strings.Contains(aborted.Render(), "aborted by operator") {
			t.Errorf("abort render = %q", aborted.Render())
		}
	}

	// From bulk onward, the record is frozen: the guest is an outage,
	// but the ownership has not moved, so abort is still defined.
	for _, p := range []Phase{PhaseBulk, PhaseSync} {
		st, _, rec := stateWithMigration(t, "mig-abort2-"+string(p), p)
		c := Command{
			Kind:        CommandAbort,
			ID:          "mig-abort2-" + string(p),
			Token:       rec.Token,
			AbortReason: "the link is hopeless",
			AtUnix:      baseUnix + 400,
		}
		if _, err := Apply(st, c); err != nil {
			t.Errorf("abort at %s was refused: %v", p, err)
		}
	}

	// At and after cutover, refused.
	for _, p := range []Phase{PhaseCutover, PhaseTeardown} {
		st, _, rec := stateWithMigration(t, "mig-abort3-"+string(p), p)
		c := Command{
			Kind:        CommandAbort,
			ID:          "mig-abort3-" + string(p),
			Token:       rec.Token,
			AbortReason: "too late",
			AtUnix:      baseUnix + 400,
		}
		out, err := Apply(st, c)
		requireSentinel(t, err, ErrAbortTooLate, "abort at "+string(p))
		if !strings.Contains(err.Error(), "new migration") {
			t.Errorf("abort-too-late error does not say what to do instead: %v", err)
		}
		stillThere, _ := out.Lookup("mig-abort3-" + string(p))
		if stillThere.Outcome != OutcomeInProgress {
			t.Errorf("a refused abort changed the outcome to %s", stillThere.Outcome)
		}
		if !stillThere.Fenced() {
			t.Error("a refused abort unfenced the guest")
		}
	}
}

// TestAbortRequiresAReason.
func TestAbortRequiresAReason(t *testing.T) {
	st, _, rec := stateWithMigration(t, "mig-abort-noreason", PhaseBulk)
	for _, reason := range []string{"", "   ", "\t\n"} {
		c := Command{Kind: CommandAbort, ID: "mig-abort-noreason", Token: rec.Token, AbortReason: reason, AtUnix: baseUnix + 400}
		if _, err := Apply(st, c); err == nil {
			t.Errorf("abort with reason %q was accepted", reason)
		}
	}
}

// TestReplayedAbortIsRejectedAsDuplicate.
func TestReplayedAbortIsRejected(t *testing.T) {
	st, _, rec := stateWithMigration(t, "mig-abort-replay", PhaseBulk)
	c := Command{
		Kind:        CommandAbort,
		ID:          "mig-abort-replay",
		Token:       rec.Token,
		AbortReason: "changed my mind",
		AtUnix:      baseUnix + 400,
	}
	first, err := Apply(st, c)
	requireNoError(t, err, "first abort")
	_, err = Apply(first, c)
	requireSentinel(t, err, ErrDuplicateCommand, "replayed abort")
	if !strings.Contains(err.Error(), "already settled as aborted") {
		t.Errorf("replayed abort error does not name the prior outcome: %v", err)
	}
}

// TestSettleToFailureCarriesItsEvidenceThrough: a recorded failure
// keeps the reason it happened, so an operator reading the record later
// sees why rather than a bare verdict.
func TestSettleToFailureCarriesItsEvidenceThrough(t *testing.T) {
	st, _, rec := stateWithMigration(t, "mig-fail", PhaseSync)
	c := Command{
		Kind:          CommandSettle,
		ID:            "mig-fail",
		Token:         rec.Token,
		FailureDetail: "zfs recv exited 1: cannot receive new filesystem stream: most recent snapshot",
		Evidence:      failureEvidence("zfs recv exited 1", baseUnix+500),
		AtUnix:        baseUnix + 500,
	}
	next, err := Apply(st, c)
	requireNoError(t, err, "settle to failed")
	failed, _ := next.Lookup("mig-fail")
	if failed.Outcome != OutcomeFailed {
		t.Fatalf("outcome = %s, want %s", failed.Outcome, OutcomeFailed)
	}
	if !strings.Contains(failed.Detail, "zfs recv exited 1") {
		t.Errorf("the settled detail does not carry the reason: %q", failed.Detail)
	}
	if len(failed.Evidence) != 1 || !strings.Contains(failed.Evidence[0].Detail, "zfs recv exited 1") {
		t.Errorf("the settled record lost its evidence: %+v", failed.Evidence)
	}
	if !strings.Contains(failed.Render(), "FAILED") {
		t.Errorf("failure render = %q", failed.Render())
	}
}

// TestSettleDetailIsWrittenFromTheEvidence: a settle that carries no
// Detail of its own still gets a true one, derived from what it
// actually proved, so a record read without its evidence slice is not
// left saying nothing.
func TestSettleDetailIsWrittenFromTheEvidence(t *testing.T) {
	st, _, rec := stateWithMigration(t, "mig-detail", PhaseCutover)
	at := baseUnix + 500

	// Unobserved.
	next, err := Apply(st, Command{
		Kind:         CommandSettle,
		ID:           "mig-detail",
		Token:        rec.Token,
		Verification: Verification{Observed: false, QueryError: "dial tcp comb-b: i/o timeout"},
		AtUnix:       at,
	})
	requireNoError(t, err, "settle unobserved with no detail")
	got, _ := next.Lookup("mig-detail")
	if !strings.Contains(got.Detail, "i/o timeout") {
		t.Errorf("unobserved detail does not carry the query error: %q", got.Detail)
	}

	// Unverified.
	next, err = Apply(st, Command{
		Kind:              CommandSettle,
		ID:                "mig-detail",
		Token:             rec.Token,
		QuorumUnavailable: true,
		AtUnix:            at,
	})
	requireNoError(t, err, "settle unverified with no detail")
	got, _ = next.Lookup("mig-detail")
	if !strings.Contains(got.Detail, "quorum") {
		t.Errorf("unverified detail does not mention quorum: %q", got.Detail)
	}
}
