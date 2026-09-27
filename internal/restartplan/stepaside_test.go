package restartplan

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
)

// These cover ADR-0145's step-aside composed into ADR-0125's existing
// restart sequence. The composition has one load-bearing property and
// one load-bearing refusal, and both are pinned below: a leader-target
// that steps aside must become restartable WITHOUT an operator
// acknowledgment, and a step-aside that cannot be confirmed must not be
// overridable by one.

// stepAsideAs records what the fake was asked and lets a test control
// the answer, including the node it was asked about - which is the only
// way to catch a wiring bug where the asider is handed the leader's id
// instead of the target's.
type stepAsideAs struct {
	askedFor   string
	calls      int
	outcome    StepAsideOutcome
	err        error
	overrideFn func() (StepAsideOutcome, error)
}

func (s *stepAsideAs) asider() StepAside {
	return StepAsideFunc(func(_ context.Context, nodeID string) (StepAsideOutcome, error) {
		s.askedFor = nodeID
		s.calls++
		if s.overrideFn != nil {
			return s.overrideFn()
		}
		return s.outcome, s.err
	})
}

func okAside(newLeader string, wasLeader bool) StepAsideOutcome {
	detail := "comb-a is not the raft leader, so there is no leadership to move"
	transferred := false
	if wasLeader {
		transferred = true
		detail = "comb-a was the raft leader; leadership has moved to " + newLeader + " and this node is a follower"
	}
	return StepAsideOutcome{
		SafeToRestart: true,
		WasLeader:     wasLeader,
		Transferred:   transferred,
		NewLeaderID:   newLeader,
		Detail:        detail,
	}
}

// TestEngineStepAsideIsFirstInTheOrder pins the placement that the whole
// composition rests on. If the step-aside is not first, RuleLeaderRestart
// blocks a leader-target at evaluate and the handover is never reached -
// the step becomes unreachable for the only case it exists for.
func TestEngineStepAsideIsFirstInTheOrder(t *testing.T) {
	if len(StepOrder) == 0 || StepOrder[0] != StepStepAside {
		t.Fatalf("StepOrder starts with %v, want it to start with %q", StepOrder, StepStepAside)
	}
	seen := map[Step]bool{}
	for _, s := range StepOrder {
		if seen[s] {
			t.Errorf("StepOrder lists %q twice", s)
		}
		seen[s] = true
	}
	if !seen[StepStepAside] {
		t.Error("StepOrder does not mention the step-aside at all")
	}
}

// TestEngineStepAsideRunsBeforeTheEvaluation asserts the order from the
// event stream rather than from the constant, so a future edit that
// reorders Run without reordering StepOrder still fails here.
func TestEngineStepAsideRunsBeforeTheEvaluation(t *testing.T) {
	h := newHarness(t, healthyQuorumFact())
	as := &stepAsideAs{outcome: okAside("", false)}
	h.engine.StepAsider = as.asider()

	if _, err := h.engine.Run(context.Background()); err != nil {
		t.Fatalf("Run() error: %v", err)
	}

	asIdx := h.steps.firstIndexOf(StepStepAside)
	evalIdx := h.steps.firstIndexOf(StepEvaluate)
	if asIdx < 0 || evalIdx < 0 {
		t.Fatalf("expected both a step-aside and an evaluate event, got stream %v", h.steps.snapshot())
	}
	if asIdx > evalIdx {
		t.Errorf("step-aside ran at index %d, after evaluate at %d; a leader-target would be blocked before the handover is ever attempted", asIdx, evalIdx)
	}
	if !h.steps.ran(StepStepAside) {
		t.Error("the step-aside was not actually performed")
	}
	if as.askedFor != "comb-a" {
		t.Errorf("step-asider was asked about %q, want the target node %q", as.askedFor, "comb-a")
	}
}

// TestEngineStepAsideLetsALeaderTargetBeRestartedWithoutOperatorForce is
// the payoff, and it asserts BOTH halves because either alone proves
// nothing. Without the step-aside the same leader-target must block
// behind RuleLeaderRestart; with it, the same target must be allowed -
// and never through Force, which stays false throughout.
func TestEngineStepAsideLetsALeaderTargetBeRestartedWithoutOperatorForce(t *testing.T) {
	// The colony before the handover: comb-a is a voter AND the leader.
	leaderFact := healthyQuorumFact()
	leaderFact.IsTargetLeader = true

	// First half: no step-asider, so the existing guardrail stands. This
	// is the behaviour the colony has today, and it is a Block.
	without := newHarness(t, leaderFact)
	res, err := without.engine.Run(context.Background())
	if err != nil {
		t.Fatalf("Run() without a step-asider error: %v", err)
	}
	if res.Outcome != OutcomeBlocked {
		t.Fatalf("without a step-asider, outcome = %q, want %q: a leader-target must still need an acknowledgment",
			res.Outcome, OutcomeBlocked)
	}
	if without.restarts != 0 || without.leases != 0 {
		t.Errorf("the blocked run reserved a lease (%d) or issued a restart (%d)", without.leases, without.restarts)
	}
	if without.engine.Force {
		t.Error("the comparison run was given Force, which would invalidate the comparison")
	}

	// Second half: the same cluster, with the step-aside standing
	// between them. The handover is modelled honestly - the fact is
	// gathered AFTER the handover, and reads the target as a follower,
	// which is what the leader's own fresh status will say.
	h := newHarness(t, leaderFact)
	as := &stepAsideAs{outcome: okAside("comb-c", true)}
	h.engine.StepAsider = as.asider()
	h.engine.Gatherer = FactGathererFunc(func(_ context.Context, service, nodeID string) QuorumFact {
		f := healthyQuorumFact()
		f.Service = service
		f.TargetNodeID = nodeID
		// comb-a handed leadership over, so it is a follower now.
		f.IsTargetLeader = false
		return f
	})

	res, err = h.engine.Run(context.Background())
	if err != nil {
		t.Fatalf("Run() with a step-asider error: %v", err)
	}
	if h.engine.Force {
		t.Error("Force was set; the whole point is that the handover replaces the acknowledgment")
	}
	if res.Outcome != OutcomeConfirmed {
		t.Fatalf("outcome = %q (%s), want %q: a confirmed handover should leave nothing for the guardrail to block",
			res.Outcome, res.Detail, OutcomeConfirmed)
	}
	if h.restarts != 1 {
		t.Errorf("restarts = %d, want exactly 1", h.restarts)
	}
	if h.leases != 1 {
		t.Errorf("leases = %d, want exactly 1: a real lease, not a bypass", h.leases)
	}
	if !h.steps.ran(StepEvaluate) {
		t.Error("the evaluation never ran, so nothing was actually allowed")
	}
}

// TestEngineStepAsideRefusalStopsTheRunAndLeavesNothingBehind pins the
// refusal. Because the step is first, a refusal happens before any
// cluster-wide state exists, so there is no lease to strand and no
// pending record to clear - which is the whole reason it was placed
// here rather than after the lease.
func TestEngineStepAsideRefusalStopsTheRunAndLeavesNothingBehind(t *testing.T) {
	h := newHarness(t, healthyQuorumFact())
	as := &stepAsideAs{
		outcome: StepAsideOutcome{
			WasLeader:   true,
			Transferred: true,
			Detail:      "comb-a handed over leadership but no other voter was observed leading before the deadline; it is NOT safe to restart yet",
		},
	}
	h.engine.StepAsider = as.asider()

	res, err := h.engine.Run(context.Background())
	if err != nil {
		t.Fatalf("Run() error: %v", err)
	}
	if res.Outcome != OutcomeBlocked {
		t.Fatalf("outcome = %q, want %q", res.Outcome, OutcomeBlocked)
	}
	if !strings.Contains(res.Detail, "NOT safe to restart") {
		t.Errorf("Detail = %q, want the step-aside's own words so the operator sees the real reason", res.Detail)
	}
	if h.leases != 0 {
		t.Errorf("leases = %d, want 0: nothing cluster-wide may exist after a step-aside refusal", h.leases)
	}
	if h.restarts != 0 {
		t.Errorf("restarts = %d, want 0", h.restarts)
	}
	if h.confirmed != 0 {
		t.Errorf("confirmed = %d, want 0: no confirmation without a restart", h.confirmed)
	}
	if _, found, err := h.pending.Load(DefaultService); err != nil {
		t.Fatalf("Load() on the pending store: %v", err)
	} else if found {
		t.Error("a pending record was written even though the step-aside refused; the next startup would confirm a restart that never happened")
	}
	for _, s := range []Step{StepGatherFacts, StepEvaluate, StepReserveLease, StepRecordPending, StepIssueRestart} {
		if h.steps.ran(s) {
			t.Errorf("step %q ran after a step-aside refusal", s)
		}
	}
	if res.StepAside == nil || res.StepAside.SafeToRestart {
		t.Error("Result.StepAside does not record the refusal")
	}
}

// TestEngineForceDoesNotBypassAFailedStepAside is the most important
// refusal in this file. Force acknowledges a KNOWN cost - that
// restarting the leader triggers an election. An unconfirmed handover is
// not a known cost; it is an unknown state, in which this node may still
// be leading. Acknowledging one's way past a fact nobody established is
// the same error, in miniature, that EvaluateQuorumSafety already
// refuses to commit for an Unknown verdict.
func TestEngineForceDoesNotBypassAFailedStepAside(t *testing.T) {
	for _, tc := range []struct {
		name  string
		aside StepAsideOutcome
		err   error
	}{
		{
			name:  "reported not safe",
			aside: StepAsideOutcome{WasLeader: true, Detail: "leadership did not reach another voter"},
		},
		{
			name:  "reported safe but returned an error",
			aside: StepAsideOutcome{SafeToRestart: true, Detail: "ignored"},
			err:   errors.New("raft: leadership transfer did not complete"),
		},
		{
			name:  "silent and empty",
			aside: StepAsideOutcome{},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, healthyQuorumFact())
			h.engine.Force = true
			h.engine.StepAsider = (&stepAsideAs{outcome: tc.aside, err: tc.err}).asider()

			res, err := h.engine.Run(context.Background())
			if err != nil {
				t.Fatalf("Run() error: %v", err)
			}
			if res.Outcome != OutcomeBlocked {
				t.Errorf("outcome = %q, want %q even with Force set: a step-aside that was not confirmed is not a known cost to acknowledge",
					res.Outcome, OutcomeBlocked)
			}
			if h.restarts != 0 || h.leases != 0 {
				t.Errorf("Force got past the step-aside: %d leases, %d restarts", h.leases, h.restarts)
			}
			if res.StepAside == nil || res.StepAside.SafeToRestart {
				t.Error("Result.StepAside does not record that the step was not confirmed")
			}
		})
	}
}

// TestEngineNilStepAsiderKeepsTheLeaderRestartBlock proves the fallback
// is the SAFE one. A caller that forgets to inject the step-aside gets
// the pre-ADR-0145 behaviour - a leader-target blocks until an operator
// acknowledges - not a silently weakened guardrail.
func TestEngineNilStepAsiderKeepsTheLeaderRestartBlock(t *testing.T) {
	fact := healthyQuorumFact()
	fact.IsTargetLeader = true

	h := newHarness(t, fact)
	h.engine.StepAsider = nil

	res, err := h.engine.Run(context.Background())
	if err != nil {
		t.Fatalf("Run() error: %v", err)
	}
	if res.Outcome != OutcomeBlocked {
		t.Errorf("outcome = %q, want %q with no step-asider configured", res.Outcome, OutcomeBlocked)
	}
	if h.restarts != 0 {
		t.Errorf("restarts = %d, want 0", h.restarts)
	}
	if h.steps.ran(StepStepAside) {
		t.Error("the step-aside reported as performed when none was configured")
	}
	if res.StepAside != nil {
		t.Error("Result.StepAside is set although no step-aside was configured")
	}
}

// TestEngineStepAsideFollowerNoOpStillProceeds proves the common case is
// not merely tolerated but cheap and complete: a follower reports
// nothing moved, and the restart still happens with a real lease.
func TestEngineStepAsideFollowerNoOpStillProceeds(t *testing.T) {
	h := newHarness(t, healthyQuorumFact())
	as := &stepAsideAs{outcome: okAside("", false)}
	h.engine.StepAsider = as.asider()

	res, err := h.engine.Run(context.Background())
	if err != nil {
		t.Fatalf("Run() error: %v", err)
	}
	if res.Outcome != OutcomeConfirmed {
		t.Fatalf("outcome = %q (%s), want %q", res.Outcome, res.Detail, OutcomeConfirmed)
	}
	if h.restarts != 1 || h.leases != 1 {
		t.Errorf("leases = %d, restarts = %d, want 1 and 1", h.leases, h.restarts)
	}
	if as.calls != 1 {
		t.Errorf("the step-asider was called %d times, want exactly 1", as.calls)
	}
	rec := res.StepAside
	if rec == nil {
		t.Fatal("Result.StepAside is nil")
	}
	if !rec.Attempted {
		t.Error("Attempted = false on a run that did attempt a step-aside")
	}
	if rec.WasLeader || rec.Transferred {
		t.Errorf("a follower reported WasLeader=%v Transferred=%v, want both false", rec.WasLeader, rec.Transferred)
	}
	if rec.NewLeaderID != "" {
		t.Errorf("NewLeaderID = %q, want empty when no transfer happened", rec.NewLeaderID)
	}
	if strings.TrimSpace(rec.Detail) == "" {
		t.Error("Detail is empty; the record must say what happened")
	}
}

// TestEngineStepAsideEvidenceSurvivesTheEvaluation guards a real bug
// this composition introduced: the evaluation used to ASSIGN its
// evidence, which silently dropped whatever the step-aside had recorded
// - and the leader-target case is exactly the one whose evidence matters
// most once the run is over.
func TestEngineStepAsideEvidenceSurvivesTheEvaluation(t *testing.T) {
	h := newHarness(t, healthyQuorumFact())
	h.engine.StepAsider = (&stepAsideAs{outcome: okAside("comb-c", true)}).asider()

	res, err := h.engine.Run(context.Background())
	if err != nil {
		t.Fatalf("Run() error: %v", err)
	}
	joined := strings.Join(res.Evidence, "\n")
	if !strings.Contains(joined, "step-aside:") {
		t.Errorf("the step-aside evidence was dropped by a later step; evidence is:\n%s", joined)
	}
	if !strings.Contains(joined, "comb-c") {
		t.Errorf("the evidence does not name the new leader; evidence is:\n%s", joined)
	}
}

// TestEngineStepAsideRecordSurvivesTheResultStore checks the record is
// really durable, not just present in memory: an operator reading the
// persisted file after the fact must be able to see that the node gave
// up leadership and who took it.
func TestEngineStepAsideRecordSurvivesTheResultStore(t *testing.T) {
	h := newHarness(t, healthyQuorumFact())
	h.engine.StepAsider = (&stepAsideAs{outcome: okAside("comb-d", true)}).asider()

	if _, err := h.engine.Run(context.Background()); err != nil {
		t.Fatalf("Run() error: %v", err)
	}

	loaded, found, err := h.results.Load("comb-a", DefaultService)
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	if !found {
		t.Fatal("the result store holds no record for comb-a")
	}
	if loaded.StepAside == nil {
		t.Fatal("the persisted result carries no step-aside record")
	}
	if !loaded.StepAside.Attempted || !loaded.StepAside.WasLeader || !loaded.StepAside.Transferred {
		t.Errorf("persisted step-aside = %+v, want attempted/was-leader/transferred all true", *loaded.StepAside)
	}
	if loaded.StepAside.NewLeaderID != "comb-d" {
		t.Errorf("persisted NewLeaderID = %q, want %q", loaded.StepAside.NewLeaderID, "comb-d")
	}
	if !loaded.StepAside.SafeToRestart {
		t.Error("persisted SafeToRestart = false on a completed run")
	}

	// And the on-disk form really is what was written, not an artefact
	// of the in-memory struct.
	raw := h.results.path("comb-a", DefaultService)
	blob, err := os.ReadFile(raw)
	if err != nil {
		t.Fatalf("reading %s: %v", raw, err)
	}
	var decoded struct {
		StepAside *StepAsideRecord `json:"step_aside"`
	}
	if err := json.Unmarshal(blob, &decoded); err != nil {
		t.Fatalf("decoding persisted JSON: %v", err)
	}
	if decoded.StepAside == nil || decoded.StepAside.NewLeaderID != "comb-d" {
		t.Errorf("persisted JSON does not carry the step-aside record: %s", blob)
	}
}
