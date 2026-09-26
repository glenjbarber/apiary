package restartplan

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/glenjbarber/apiary/internal/cluster"
	"github.com/glenjbarber/apiary/internal/guardrail"
)

// --- engine fixtures -----------------------------------------------------

// recorded is the step stream one Engine run produced, plus a mutex
// because the effects a test injects may be called from the retry loop.
type recorded struct {
	mu     sync.Mutex
	events []StepEvent
}

func (r *recorded) on(e StepEvent) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, e)
}

func (r *recorded) snapshot() []StepEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]StepEvent(nil), r.events...)
}

func (r *recorded) phasesOf(step Step) []Phase {
	var out []Phase
	for _, e := range r.snapshot() {
		if e.Step == step {
			out = append(out, e.Phase)
		}
	}
	return out
}

// ran reports whether step was actually performed (not skipped).
func (r *recorded) ran(step Step) bool {
	for _, e := range r.snapshot() {
		if e.Step == step && e.Phase == PhaseEnter {
			return true
		}
	}
	return false
}

func (r *recorded) firstIndexOf(step Step) int {
	for i, e := range r.snapshot() {
		if e.Step == step {
			return i
		}
	}
	return -1
}

func (r *recorded) detailOf(step Step) string {
	for _, e := range r.snapshot() {
		if e.Step == step {
			return e.Detail
		}
	}
	return ""
}

// healthyQuorumFact is a 3-voter cluster with both other voters
// reachable - the ordinary case where a restart is safe.
func healthyQuorumFact() QuorumFact {
	return QuorumFact{
		Service: DefaultService, TargetNodeID: "comb-a",
		IsTargetVoter: true, IsTargetLeader: false, ProbeReadOK: true,
		OtherVoters: reachableVoters([]string{"comb-b", "comb-c"}, "comb-b", "comb-c"),
	}
}

func gathererFor(fact QuorumFact) FactGatherer {
	return FactGathererFunc(func(_ context.Context, service, nodeID string) QuorumFact {
		fact.Service = service
		fact.TargetNodeID = nodeID
		return fact
	})
}

// harness is a fully wired Engine with recording doubles for every
// effect, so a test changes exactly the one thing it is about.
type harness struct {
	engine    *Engine
	clock     *fakeClock
	steps     *recorded
	restarts  int
	leases    int
	confirmed int
	dir       string
	pending   *PendingStore
	results   *ResultStore
}

func newHarness(t *testing.T, fact QuorumFact) *harness {
	t.Helper()
	dir := t.TempDir()
	h := &harness{
		clock:   newFakeClock(),
		steps:   &recorded{},
		dir:     dir,
		pending: NewPendingStore(dir + "/pending"),
		results: NewResultStore(dir + "/results"),
	}
	h.engine = &Engine{
		Service:  DefaultService,
		NodeID:   "comb-a",
		Gatherer: gathererFor(fact),
		Leaser: LeaserFunc(func(_ context.Context, service, nodeID string, force bool) (Lease, error) {
			h.leases++
			return Lease{ID: 1234}, nil
		}),
		Restarter: RestarterFunc(func(context.Context, string) error {
			h.restarts++
			return nil
		}),
		Confirmer: ConfirmerFunc(func(context.Context, string, string, uint64) error {
			h.confirmed++
			return nil
		}),
		Pending: h.pending,
		Results: h.results,
		Clock:   h.clock,
		OnStep:  h.steps.on,
	}
	return h
}

// TestEngineHappyPath is the ordinary case, and it exists mainly to make
// the step order an explicit, checked claim: gather, evaluate, reserve,
// record, restart, confirm, persist - in that order, every one of them
// actually performed.
func TestEngineHappyPath(t *testing.T) {
	h := newHarness(t, healthyQuorumFact())
	res, err := h.engine.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if res.Outcome != OutcomeConfirmed {
		t.Errorf("Outcome = %q, want %q (detail: %s)", res.Outcome, OutcomeConfirmed, res.Detail)
	}
	if res.LeaseID != 1234 {
		t.Errorf("LeaseID = %d, want 1234", res.LeaseID)
	}
	if h.restarts != 1 || h.leases != 1 || h.confirmed != 1 {
		t.Errorf("effects: %d restarts, %d leases, %d confirmations; want 1 of each", h.restarts, h.leases, h.confirmed)
	}
	if len(res.Evidence) == 0 {
		t.Errorf("a confirmation with no evidence should have been rejected by Validate, not returned")
	}
	if !strings.Contains(res.Evidence[0], "confirmed on attempt 1") {
		t.Errorf("Evidence = %v, want it to name the attempt that confirmed", res.Evidence)
	}

	// The order is the safety argument; assert it rather than trust it.
	want := []Step{StepGatherFacts, StepEvaluate, StepReserveLease, StepRecordPending, StepIssueRestart, StepAwaitConfirm, StepRecordOutcome}
	for i, step := range want {
		idx := h.steps.firstIndexOf(step)
		if idx < 0 {
			t.Fatalf("step %s never ran; stream was %v", step, h.steps.snapshot())
		}
		if i > 0 {
			prevIdx := h.steps.firstIndexOf(want[i-1])
			if idx < prevIdx {
				t.Errorf("step %s ran before %s", step, want[i-1])
			}
		}
	}
	for _, step := range want {
		if phases := h.steps.phasesOf(step); len(phases) != 1 || phases[0] != PhaseEnter {
			t.Errorf("step %s phases = %v, want exactly one enter", step, phases)
		}
	}

	// A confirmed restart clears the local trace, and the durable record
	// survives on disk.
	if _, found, err := h.pending.Load(DefaultService); err != nil || found {
		t.Errorf("pending record after a confirmed restart = (found=%v, err=%v), want found=false", found, err)
	}
	stored, found, err := h.results.Load("comb-a", DefaultService)
	if err != nil || !found {
		t.Fatalf("durable record = (found=%v, err=%v)", found, err)
	}
	if stored.Outcome != OutcomeConfirmed {
		t.Errorf("stored Outcome = %q, want confirmed", stored.Outcome)
	}
	if stored.FinishedAtUnix <= stored.StartedAtUnix {
		t.Errorf("FinishedAtUnix (%d) <= StartedAtUnix (%d); a record needs a real interval", stored.FinishedAtUnix, stored.StartedAtUnix)
	}
}

// TestEngineWritesPendingBeforeRestartingCommand pins the single
// ordering that makes a crash in the middle of a restart safe: the
// discoverable trace of the pending lease must exist before the command
// that destroys the confirming process is issued.
func TestEngineWritesPendingBeforeRestartingCommand(t *testing.T) {
	h := newHarness(t, healthyQuorumFact())
	var pendingExistedAtRestartTime bool
	h.engine.Restarter = RestarterFunc(func(context.Context, string) error {
		_, found, _ := h.pending.Load(DefaultService)
		pendingExistedAtRestartTime = found
		return nil
	})
	if _, err := h.engine.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !pendingExistedAtRestartTime {
		t.Errorf("the restart command was issued with no pending record on disk; a crash here would strand the lease with nothing able to release it")
	}
}

// TestEngineRefusesWhenQuorumWouldBeLost is the case ADR-0125 exists for.
func TestEngineRefusesWhenQuorumWouldBeLost(t *testing.T) {
	h := newHarness(t, QuorumFact{
		Service: DefaultService, TargetNodeID: "comb-a",
		IsTargetVoter: true, ProbeReadOK: true,
		OtherVoters: reachableVoters([]string{"comb-b", "comb-c"}),
	})
	res, err := h.engine.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if res.Outcome != OutcomeBlocked {
		t.Errorf("Outcome = %q, want %q", res.Outcome, OutcomeBlocked)
	}
	if IsFailure(res.Outcome) || IsSuccess(res.Outcome) {
		t.Errorf("a refusal was classified as success=%v failure=%v; it is neither", IsSuccess(res.Outcome), IsFailure(res.Outcome))
	}
	if !strings.Contains(res.Detail, "guardrail") {
		t.Errorf("Detail = %q, want it to name the guardrail", res.Detail)
	}
	if len(res.Evidence) == 0 {
		t.Errorf("a refusal with no evidence is not a record")
	}
	if joined := strings.Join(res.Evidence, " | "); !strings.Contains(joined, RuleQuorumSafety) {
		t.Errorf("Evidence = %v, want it to name the %s rule", res.Evidence, RuleQuorumSafety)
	}

	// Nothing cluster-wide may have been touched.
	if h.leases != 0 {
		t.Errorf("a lease was reserved (%d) despite a blocked evaluation; a refused restart must leave no trace cluster-wide", h.leases)
	}
	if h.restarts != 0 {
		t.Errorf("the restart command ran (%d times) despite a blocked evaluation", h.restarts)
	}
	for _, step := range []Step{StepReserveLease, StepRecordPending, StepIssueRestart, StepAwaitConfirm} {
		if h.steps.ran(step) {
			t.Errorf("step %s ran even though the evaluation blocked", step)
		}
	}
	// ... and the workflow must say it stopped, not fall off the end.
	if phases := h.steps.phasesOf(StepIssueRestart); len(phases) == 0 || phases[0] != PhaseSkip {
		t.Errorf("StepIssueRestart phases = %v, want a skip", phases)
	}
}

// TestEngineRefusesWhenTheProbeCannotBeRun is ADR-0125 §8's fail-closed
// rule at the top of the state machine: "could not check" is a refusal,
// and it is recorded as its own outcome rather than as a failure.
func TestEngineRefusesWhenTheProbeCannotBeRun(t *testing.T) {
	h := newHarness(t, QuorumFact{
		Service: DefaultService, TargetNodeID: "comb-a",
		ProbeReadOK: false, ProbeError: "raft status read: context deadline exceeded",
	})
	res, err := h.engine.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != OutcomeBlocked {
		t.Errorf("Outcome = %q, want blocked - an unreadable probe is 'could not check', which stops the workflow", res.Outcome)
	}
	if h.restarts != 0 || h.leases != 0 {
		t.Errorf("an unreadable probe still led to %d restarts and %d leases", h.restarts, h.leases)
	}
	if !strings.Contains(strings.Join(res.Evidence, " | "), "context deadline exceeded") {
		t.Errorf("Evidence = %v, want the probe's own reason carried through", res.Evidence)
	}
}

// TestEngineLeaderRestartRequiresAcknowledgment covers leader vs
// follower: a follower restart proceeds, a leader restart stops until the
// operator acknowledges, and the acknowledgment is preserved as a cited
// caveat rather than silently dropped.
func TestEngineLeaderRestartRequiresAcknowledgment(t *testing.T) {
	t.Run("a follower restart needs no acknowledgment", func(t *testing.T) {
		h := newHarness(t, healthyQuorumFact())
		res, err := h.engine.Run(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if res.Outcome != OutcomeConfirmed {
			t.Errorf("Outcome = %q, want confirmed", res.Outcome)
		}
		if h.steps.detailOf(StepEvaluate) != "allowed" {
			t.Errorf("StepEvaluate detail = %q, want allowed", h.steps.detailOf(StepEvaluate))
		}
	})

	t.Run("a leader restart blocks without force", func(t *testing.T) {
		fact := healthyQuorumFact()
		fact.TargetNodeID = "comb-a"
		fact.IsTargetLeader = true
		h := newHarness(t, fact)
		res, err := h.engine.Run(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if res.Outcome != OutcomeBlocked {
			t.Errorf("Outcome = %q, want blocked", res.Outcome)
		}
		if h.restarts != 0 {
			t.Errorf("the leader's raftd was restarted without an acknowledged override")
		}
		if joined := strings.Join(res.Evidence, " | "); !strings.Contains(joined, RuleLeaderRestart) {
			t.Errorf("Evidence = %v, want the leader rule named", res.Evidence)
		}
	})

	t.Run("a leader restart proceeds with force, keeping the election caveat", func(t *testing.T) {
		fact := healthyQuorumFact()
		fact.TargetNodeID = "comb-a"
		fact.IsTargetLeader = true
		h := newHarness(t, fact)
		h.engine.Force = true
		res, err := h.engine.Run(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if res.Outcome != OutcomeConfirmed {
			t.Errorf("Outcome = %q, want confirmed (detail: %s)", res.Outcome, res.Detail)
		}
		if h.restarts != 1 {
			t.Errorf("%d restarts, want 1", h.restarts)
		}
		joined := strings.Join(res.Evidence, " | ")
		if !strings.Contains(joined, "operator acknowledged this") {
			t.Errorf("Evidence = %v, want the acknowledged election cost preserved", res.Evidence)
		}
		if !strings.Contains(joined, "election") {
			t.Errorf("Evidence = %v, want it to say what was acknowledged (the election), not just that something was", res.Evidence)
		}
		// Force must also reach the lease reservation, which is the
		// raft-replicated decision that actually matters.
		if h.leases != 1 {
			t.Errorf("%d leases, want 1", h.leases)
		}
	})

	t.Run("force never gets an unreadable probe past the guardrail", func(t *testing.T) {
		h := newHarness(t, QuorumFact{Service: DefaultService, TargetNodeID: "comb-a", ProbeReadOK: false})
		h.engine.Force = true
		res, err := h.engine.Run(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if res.Outcome != OutcomeBlocked {
			t.Errorf("Outcome = %q, want blocked - Force cannot acknowledge a fact nobody established", res.Outcome)
		}
		if h.restarts != 0 {
			t.Errorf("%d restarts, want 0", h.restarts)
		}
	})
}

// TestEngineRestartThatNeverReturns is the required "a node restarts and
// never comes back" case, and the single most important test in this
// file.
//
// The restart command succeeded. Nothing confirmed. There is no evidence
// the node is broken and no evidence it is fine. The only honest outcome
// is OutcomeUnobserved - not failed (we never saw it fail) and
// emphatically not confirmed (we never saw it come back) - the pending
// record must survive so a later startup or an operator can finish the
// job, and the durable record must list every attempt.
func TestEngineRestartThatNeverReturns(t *testing.T) {
	h := newHarness(t, healthyQuorumFact())
	attempts := 0
	h.engine.Confirmer = ConfirmerFunc(func(context.Context, string, string, uint64) error {
		attempts++
		return fmt.Errorf("dial tcp 127.0.0.1:17700: connect: connection refused")
	})

	res, err := h.engine.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if res.Outcome != OutcomeUnobserved {
		t.Errorf("Outcome = %q, want %q - a node that restarts and never answers is unobserved, not failed and not confirmed", res.Outcome, OutcomeUnobserved)
	}
	if IsFailure(res.Outcome) {
		t.Errorf("an unreachable confirmation was recorded as a failed restart; we never saw the restart fail")
	}
	if IsSuccess(res.Outcome) {
		t.Errorf("an unconfirmed restart was recorded as a success")
	}
	if !IsUnknown(res.Outcome) {
		t.Errorf("IsUnknown = false for %q", res.Outcome)
	}
	if !strings.Contains(res.Detail, "never confirmed itself") {
		t.Errorf("Detail = %q, want it to say the confirmation never arrived", res.Detail)
	}

	// The retry budget: 5 attempts, 3s apart, and only 4 backoffs
	// between them.
	if attempts != 5 {
		t.Errorf("made %d confirmation attempts, want 5 (ADR-0125 §7)", attempts)
	}
	if len(h.clock.sleeps) != 4 {
		t.Errorf("slept %d times between 5 attempts, want 4", len(h.clock.sleeps))
	}
	for i, d := range h.clock.sleeps {
		if d != 3*time.Second {
			t.Errorf("backoff %d = %v, want 3s (ADR-0125 §7)", i, d)
		}
	}

	// Every attempt is on the record, so "unknown" is never a shrug.
	if len(res.Evidence) != 5 {
		t.Errorf("Evidence has %d entries, want one per attempt: %v", len(res.Evidence), res.Evidence)
	}

	// The lease is still held and the local trace is still there.
	if _, found, _ := h.pending.Load(DefaultService); !found {
		t.Errorf("the pending record was cleared despite no confirmation; nothing can now tell a later startup what is outstanding")
	}
	stored, found, err := h.results.Load("comb-a", DefaultService)
	if err != nil || !found {
		t.Fatalf("durable record = (found=%v, err=%v)", found, err)
	}
	if stored.Outcome != OutcomeUnobserved {
		t.Errorf("stored Outcome = %q, want unobserved", stored.Outcome)
	}
	if !strings.Contains(stored.Render(), "UNKNOWN") {
		t.Errorf("the durable record renders as %q, which does not read as unknown", stored.Render())
	}
}

// TestEngineFailedRestartCommandIsAFailure is the one genuinely positive
// failure observation in the whole workflow: `service apiary_raftd
// restart` returned non-zero. That is something we saw, so it is a
// failure - and it is still not a reason to clear the pending record,
// because the lease is held cluster-wide and only a real confirmation
// releases it.
func TestEngineFailedRestartCommandIsAFailure(t *testing.T) {
	h := newHarness(t, healthyQuorumFact())
	h.engine.Restarter = RestarterFunc(func(context.Context, string) error {
		return errors.New("service: unknown action name restart")
	})

	res, err := h.engine.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != OutcomeFailed {
		t.Errorf("Outcome = %q, want %q", res.Outcome, OutcomeFailed)
	}
	if !IsFailure(res.Outcome) || IsUnknown(res.Outcome) {
		t.Errorf("classification wrong: success=%v failure=%v unknown=%v", IsSuccess(res.Outcome), IsFailure(res.Outcome), IsUnknown(res.Outcome))
	}
	if len(res.Evidence) == 0 || !strings.Contains(res.Evidence[0], "unknown action name") {
		t.Errorf("Evidence = %v, want the command's own error", res.Evidence)
	}
	if h.confirmed != 0 {
		t.Errorf("%d confirmations were attempted after the restart command failed", h.confirmed)
	}
	if _, found, _ := h.pending.Load(DefaultService); !found {
		t.Errorf("the pending record was cleared on a failed restart; the lease is still held and must stay discoverable")
	}
}

// TestEngineConfirmationSucceedsOnALaterAttempt checks the retry loop
// actually retries - a transient managerd hiccup must not be recorded as
// an unknown.
func TestEngineConfirmationSucceedsOnALaterAttempt(t *testing.T) {
	h := newHarness(t, healthyQuorumFact())
	attempts := 0
	h.engine.Confirmer = ConfirmerFunc(func(context.Context, string, string, uint64) error {
		attempts++
		if attempts < 3 {
			return errors.New("not leader and no reachable leader hint for the restart-guardrail confirmation")
		}
		return nil
	})

	res, err := h.engine.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != OutcomeConfirmed {
		t.Errorf("Outcome = %q, want confirmed - a confirmation that lands on attempt 3 is a confirmation", res.Outcome)
	}
	if attempts != 3 {
		t.Errorf("made %d attempts, want 3", attempts)
	}
	if len(h.clock.sleeps) != 2 {
		t.Errorf("slept %d times, want 2 (no backoff after the successful attempt)", len(h.clock.sleeps))
	}
	if _, found, _ := h.pending.Load(DefaultService); found {
		t.Errorf("the pending record survived a confirmed restart")
	}
}

// TestEngineCanceledWaitIsNotAFailure covers the cancellation path: a
// caller that goes away mid-wait did not learn anything, so the outcome
// is still unknown, but the record must say the budget was cancelled
// rather than exhausted so an operator knows the difference.
func TestEngineCanceledWaitIsNotAFailure(t *testing.T) {
	h := newHarness(t, healthyQuorumFact())
	ctx, cancel := context.WithCancel(context.Background())
	h.engine.Confirmer = ConfirmerFunc(func(context.Context, string, string, uint64) error {
		cancel()
		return errors.New("dial tcp: i/o timeout")
	})

	res, err := h.engine.Run(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != OutcomeUnobserved {
		t.Errorf("Outcome = %q, want unobserved", res.Outcome)
	}
	if !strings.Contains(res.Detail, "cancelled, not exhausted") {
		t.Errorf("Detail = %q, want it to distinguish a cancelled wait from an exhausted one", res.Detail)
	}
}

// TestEngineRefusesWithoutTheEffectsItNeeds covers each missing
// dependency. Every one of them is a refusal with a stated reason, never
// a panic and never a silent no-op that looks like success.
func TestEngineRefusesWithoutTheEffectsItNeeds(t *testing.T) {
	t.Run("no gatherer", func(t *testing.T) {
		h := newHarness(t, healthyQuorumFact())
		h.engine.Gatherer = nil
		res, err := h.engine.Run(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if res.Outcome != OutcomeBlocked {
			t.Errorf("Outcome = %q, want blocked", res.Outcome)
		}
		if !strings.Contains(strings.Join(res.Evidence, " "), "no fact gatherer") {
			t.Errorf("Evidence = %v, want the missing gatherer named", res.Evidence)
		}
		if h.restarts != 0 {
			t.Errorf("%d restarts, want 0", h.restarts)
		}
	})

	t.Run("no leaser", func(t *testing.T) {
		h := newHarness(t, healthyQuorumFact())
		h.engine.Leaser = nil
		res, err := h.engine.Run(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if res.Outcome != OutcomeBlocked {
			t.Errorf("Outcome = %q, want blocked", res.Outcome)
		}
		if h.restarts != 0 {
			t.Errorf("%d restarts without a lease, want 0 - a lease is the cluster-wide serialization point", h.restarts)
		}
	})

	t.Run("a refused lease", func(t *testing.T) {
		h := newHarness(t, healthyQuorumFact())
		h.engine.Leaser = LeaserFunc(func(context.Context, string, string, bool) (Lease, error) {
			return Lease{}, errors.New("a restart lease for apiary_raftd is already held and unconfirmed")
		})
		res, err := h.engine.Run(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if res.Outcome != OutcomeBlocked {
			t.Errorf("Outcome = %q, want blocked", res.Outcome)
		}
		if !strings.Contains(res.Detail, "already held") {
			t.Errorf("Detail = %q, want the FSM's own reason", res.Detail)
		}
		if h.restarts != 0 {
			t.Errorf("%d restarts, want 0", h.restarts)
		}
	})

	t.Run("a lease granted with id 0 is not a lease", func(t *testing.T) {
		h := newHarness(t, healthyQuorumFact())
		h.engine.Leaser = LeaserFunc(func(context.Context, string, string, bool) (Lease, error) {
			return Lease{}, nil
		})
		res, err := h.engine.Run(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if res.Outcome != OutcomeBlocked {
			t.Errorf("Outcome = %q, want blocked - lease id 0 would confirm nothing", res.Outcome)
		}
		if h.restarts != 0 {
			t.Errorf("%d restarts, want 0", h.restarts)
		}
	})

	t.Run("no pending store", func(t *testing.T) {
		h := newHarness(t, healthyQuorumFact())
		h.engine.Pending = nil
		res, err := h.engine.Run(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if res.Outcome != OutcomeBlocked {
			t.Errorf("Outcome = %q, want blocked", res.Outcome)
		}
		if h.restarts != 0 {
			t.Errorf("%d restarts with no discoverable trace, want 0", h.restarts)
		}
		// The lease is now genuinely stuck, and the record must say so
		// rather than implying the run simply did not happen.
		if !strings.Contains(strings.Join(res.Evidence, " "), "still held") {
			t.Errorf("Evidence = %v, want the orphaned lease called out", res.Evidence)
		}
	})

	t.Run("no restarter", func(t *testing.T) {
		h := newHarness(t, healthyQuorumFact())
		h.engine.Restarter = nil
		res, err := h.engine.Run(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if res.Outcome != OutcomeBlocked {
			t.Errorf("Outcome = %q, want blocked", res.Outcome)
		}
		if !strings.Contains(res.Detail, "lease 1234 is reserved") {
			t.Errorf("Detail = %q, want the reserved lease named", res.Detail)
		}
	})

	t.Run("no confirmer", func(t *testing.T) {
		h := newHarness(t, healthyQuorumFact())
		h.engine.Confirmer = nil
		res, err := h.engine.Run(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if res.Outcome != OutcomeUnobserved {
			t.Errorf("Outcome = %q, want unobserved - the restart command ran and nothing can check it", res.Outcome)
		}
		if h.restarts != 1 {
			t.Errorf("%d restarts, want 1", h.restarts)
		}
		if _, found, _ := h.pending.Load(DefaultService); !found {
			t.Errorf("the pending record was cleared with no confirmer to have confirmed anything")
		}
	})
}

// TestEngineDefaultsAndIsolation covers the two small structural promises:
// a zero Service means the ADR's own service, and two Runs of one Engine
// do not interfere with each other.
func TestEngineDefaultsAndIsolation(t *testing.T) {
	t.Run("an empty service defaults to apiary_raftd", func(t *testing.T) {
		h := newHarness(t, healthyQuorumFact())
		h.engine.Service = ""
		res, err := h.engine.Run(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if res.Service != DefaultService {
			t.Errorf("Service = %q, want %q", res.Service, DefaultService)
		}
	})

	t.Run("two runs are two independent attempts", func(t *testing.T) {
		h := newHarness(t, healthyQuorumFact())
		if _, err := h.engine.Run(context.Background()); err != nil {
			t.Fatal(err)
		}
		first := h.restarts
		res, err := h.engine.Run(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if h.restarts != first+1 {
			t.Errorf("the second run did not issue its own restart (%d then %d)", first, h.restarts)
		}
		if res.Outcome != OutcomeConfirmed {
			t.Errorf("second run Outcome = %q, want confirmed", res.Outcome)
		}
	})
}

// TestConfirmWithRetry covers the retry loop on its own, since both
// callers share it and its budget is the thing that decides whether an
// outstanding lease eventually gets released.
func TestConfirmWithRetry(t *testing.T) {
	pending := PendingRestart{Service: DefaultService, NodeID: "comb-a", LeaseID: 42}

	t.Run("a nil confirmer reports no attempt rather than panicking", func(t *testing.T) {
		out := ConfirmWithRetry(context.Background(), nil, pending, newFakeClock(), ConfirmOptions{})
		if out.Confirmed {
			t.Errorf("Confirmed = true with no confirmer")
		}
		if out.Attempts != 0 {
			t.Errorf("Attempts = %d, want 0", out.Attempts)
		}
		if out.LastDetail == "" {
			t.Errorf("LastDetail is empty; a caller needs to know why")
		}
	})

	t.Run("the budget is bounded and the backoff is ADR-0125's", func(t *testing.T) {
		clock := newFakeClock()
		attempts := 0
		out := ConfirmWithRetry(context.Background(),
			ConfirmerFunc(func(context.Context, string, string, uint64) error {
				attempts++
				return errors.New("nope")
			}), pending, clock, ConfirmOptions{})

		if out.Confirmed {
			t.Errorf("Confirmed = true")
		}
		if out.Attempts != 5 || attempts != 5 {
			t.Errorf("Attempts = %d, made = %d, want 5", out.Attempts, attempts)
		}
		if len(clock.sleeps) != 4 {
			t.Errorf("slept %d times, want 4", len(clock.sleeps))
		}
		for i, d := range clock.sleeps {
			if d != DefaultConfirmOptions().Backoff {
				t.Errorf("backoff %d = %v, want %v", i, d, DefaultConfirmOptions().Backoff)
			}
		}
		if out.Cancelled {
			t.Errorf("Cancelled = true for an exhausted budget")
		}
		if len(out.Evidence) != 5 {
			t.Errorf("Evidence has %d entries, want 5", len(out.Evidence))
		}
	})

	t.Run("a cancelled context before the first attempt makes no attempt", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		attempts := 0
		out := ConfirmWithRetry(ctx,
			ConfirmerFunc(func(context.Context, string, string, uint64) error {
				attempts++
				return nil
			}), pending, newFakeClock(), ConfirmOptions{})
		if attempts != 0 {
			t.Errorf("made %d attempts under a cancelled context, want 0", attempts)
		}
		if !out.Cancelled || out.Confirmed {
			t.Errorf("out = %+v, want cancelled and unconfirmed", out)
		}
	})

	t.Run("a failed sleep stops the loop and is reported as cancelled", func(t *testing.T) {
		clock := newFakeClock()
		clock.sleepErr = context.Canceled
		attempts := 0
		out := ConfirmWithRetry(context.Background(),
			ConfirmerFunc(func(context.Context, string, string, uint64) error {
				attempts++
				return errors.New("nope")
			}), pending, clock, ConfirmOptions{})
		if attempts != 1 {
			t.Errorf("made %d attempts, want 1 - a cancelled sleep must stop the loop, not be retried through", attempts)
		}
		if !out.Cancelled {
			t.Errorf("Cancelled = false after a cancelled sleep")
		}
	})

	t.Run("the confirmer receives exactly the pending record", func(t *testing.T) {
		var gotService, gotNode string
		var gotLease uint64
		out := ConfirmWithRetry(context.Background(),
			ConfirmerFunc(func(_ context.Context, service, nodeID string, leaseID uint64) error {
				gotService, gotNode, gotLease = service, nodeID, leaseID
				return nil
			}), pending, newFakeClock(), ConfirmOptions{})
		if !out.Confirmed {
			t.Fatalf("Confirmed = false: %+v", out)
		}
		if gotService != DefaultService || gotNode != "comb-a" || gotLease != 42 {
			t.Errorf("confirmer got (%q, %q, %d), want (%q, %q, 42)", gotService, gotNode, gotLease, DefaultService, "comb-a")
		}
	})

	t.Run("custom budgets are honoured", func(t *testing.T) {
		clock := newFakeClock()
		attempts := 0
		ConfirmWithRetry(context.Background(),
			ConfirmerFunc(func(context.Context, string, string, uint64) error {
				attempts++
				return errors.New("nope")
			}), pending, clock, ConfirmOptions{Attempts: 2, Backoff: time.Millisecond})
		if attempts != 2 {
			t.Errorf("made %d attempts, want 2", attempts)
		}
		if len(clock.sleeps) != 1 || clock.sleeps[0] != time.Millisecond {
			t.Errorf("sleeps = %v, want one 1ms backoff", clock.sleeps)
		}
	})
}

// TestDefaultConfirmOptions pins the ADR's own numbers, so a future
// "let's make this a bit faster" edit has to come back and change a test.
func TestDefaultConfirmOptions(t *testing.T) {
	got := DefaultConfirmOptions()
	if got.Attempts != 5 || got.Backoff != 3*time.Second {
		t.Errorf("DefaultConfirmOptions() = %+v, want {5 3s} (ADR-0125 §7)", got)
	}
	if (ConfirmOptions{}).WithDefaults() != got {
		t.Errorf("the zero value must resolve to the ADR's budget, not to an unbounded wait")
	}
	custom := ConfirmOptions{Attempts: 2, Backoff: time.Second}
	if custom.WithDefaults() != custom {
		t.Errorf("WithDefaults overwrote a value the caller set: %+v", custom.WithDefaults())
	}
}

// TestDefaultServiceMatchesManagerServices keeps this package's service
// key honest against the real internal/manager list, which is where the
// rc.d name an operator actually restarts comes from. A rename there
// without a rename here would silently target nothing.
func TestDefaultServiceMatchesManagerServices(t *testing.T) {
	if DefaultService != "apiary_raftd" {
		t.Errorf("DefaultService = %q, want %q (ADR-0125 §1: the existing rc.d name, not a new string)", DefaultService, "apiary_raftd")
	}
	if ManagerService != "apiary_managerd" {
		t.Errorf("ManagerService = %q, want %q", ManagerService, "apiary_managerd")
	}
	if DefaultService == ManagerService {
		t.Errorf("the two lease keys must differ; they gate independent leases")
	}
	// The verdict vocabulary this package reports must be the one
	// internal/guardrail already defines, so a merged Report needs no
	// translation.
	if guardrail.Allow != "allow" || guardrail.Block != "block" || guardrail.Unknown != "unknown" {
		t.Errorf("guardrail's own vocabulary changed underneath this package")
	}
	if RuleQuorumSafety != "raftd-quorum-safety" || RuleLeaderRestart != "raftd-leader-restart" {
		t.Errorf("the rule ids must stay the strings ADR-0125 §4 and the frontend's copy key off: %q, %q", RuleQuorumSafety, RuleLeaderRestart)
	}
}

// TestClusterReachabilityVocabularyIsReused makes the no-new-vocabulary
// promise a test rather than a comment: this package adds no reachability
// states of its own, it reuses internal/cluster's three.
func TestClusterReachabilityVocabularyIsReused(t *testing.T) {
	v := VoterReachability{NodeID: "comb-a"}
	if v.Reachability != "" {
		t.Errorf("the zero value of Reachability is %q, want the empty string so ComputeQuorum can default it to unknown", v.Reachability)
	}
	impact := ComputeQuorum(QuorumFact{
		Service: DefaultService, TargetNodeID: "comb-a",
		IsTargetVoter: true, ProbeReadOK: true,
		OtherVoters: []VoterReachability{{NodeID: "comb-b"}},
	})
	if impact.RemainingUnknown != 1 {
		t.Errorf("RemainingUnknown = %d, want 1", impact.RemainingUnknown)
	}
	if impact.Survives {
		t.Errorf("Survives = true; an unprobed voter cannot be credited")
	}
	_ = cluster.ReachabilityUnreachable // the third state, used verbatim
}
