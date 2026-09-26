package restartplan

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/glenjbarber/apiary/internal/guardrail"
)

// This file is the plan/step state machine ADR-0125 describes, expressed
// as one linear sequence of named steps over already-injected effects.
// There is no hidden state and no "defer and retry later" logic (ADR-0125
// §8): every step either runs, or the workflow stops and records why.
//
// INTEGRATION NOTES - what still has to be wired up, and where.
//
// ADR-0125's Implementation Order puts the evaluator in
// internal/guardrail and the probe/preflight wiring in
// internal/manager. Neither file is owned by this change, and
// internal/guardrail is not where the logic ended up: it lives here, in
// its own package, so that it is testable without a raft node, a
// managerd, or a FreeBSD host, and so that a concurrent change to
// internal/manager cannot be blocked on it. The seams it needs are all
// defined above. Concretely, the remaining work is:
//
//  1. internal/manager: add a probeRaftdVoters-equivalent that reads
//     raft.Status().Servers, filters Suffrage == "Voter", drops the
//     target, and returns a restartplan.FactGatherer. It is
//     ProbeVoters(Voter{NodeID: s.ID, RaftBindAddress: s.Address},
//     ProbeOptions{Dial: <the existing dialReachable>}) plus the two
//     boolean fields from the same status read (IsTargetVoter,
//     IsTargetLeader). Roughly: the leader's own code, calling
//     ProbeVoters and EvaluateQuorumSafety, with Existing set to
//     internal/guardrail's own EvaluateConcurrentManagerRestart report.
//  2. internal/manager: add "apiary_raftd" to apiaryServices with
//     restartable: true, and generalize the
//     `name == restartGuardrailService` comparisons in
//     PreflightRestartNodeService and RestartNodeService to a
//     restartGuardrailServices set, so a raftd restart reserves a lease
//     and writes the pending record exactly as a managerd one does.
//  3. internal/manager: the pending record it writes must be readable by
//     this package's PendingStore and by cmd/raftd. That is asserted
//     bidirectionally in
//     TestPendingStoreMatchesManagerRestartConfirmStore, so if the
//     integration does not reuse manager.PendingRestart and
//     manager.RestartConfirmStore verbatim, that test fails rather than
//     the lease quietly stranding in production.
//
// None of steps 1-3 need a new proto message (ADR-0125 §9 is right about
// that): PreflightRestartNodeService already returns Verdict plus
// Findings[], GuardrailFinding.Rule already carries "raftd-quorum-safety"
// and "raftd-leader-restart" as plain strings, and the frontend's
// preflight panel already renders Findings[].Detail generically (ADR-0125
// §10).
//
// The order of the steps is the whole safety argument, and two of the
// orderings are load-bearing rather than incidental:
//
//  1. The quorum evaluation happens BEFORE the lease is reserved. A
//     refused restart must not leave a lease behind, because a lease is
//     a cluster-wide raft-replicated block on every other node's restarts
//     and this process has no TTL that would eventually clear it.
//  2. The pending record is written BEFORE the restart command is
//     issued. If this process dies between the two, the next raftd
//     startup still finds the record and self-confirms; a lease can
//     therefore never be stranded by a crash in exactly this window
//     (ADR-0103, restated in ADR-0125's own Negative/Risks section).
//
// Every effect the state machine performs is behind one of the small
// interfaces below (FactGatherer, Leaser, Restarter, Confirmer, plus the
// two file-backed stores), so the whole sequence - including the
// acknowledgment-timeout and never-came-back paths - runs in a unit test
// with no host, no raft, no network, and no sleeping real time.

// Step names one stage of the workflow, in execution order.
type Step string

const (
	// StepGatherFacts reads current raft membership and dials the other
	// voters (ADR-0125 §3). The one step that talks to the network.
	StepGatherFacts Step = "gather-facts"

	// StepEvaluate runs the pure quorum-safety decision over the gathered
	// facts (ADR-0125 §4). A verdict other than Allow stops the workflow
	// here, before any cluster-wide state has been touched.
	StepEvaluate Step = "evaluate"

	// StepReserveLease acquires the cluster-wide raft-replicated restart
	// lease, which is the real serialization point (ADR-0103).
	StepReserveLease Step = "reserve-lease"

	// StepRecordPending writes the local pending-restart record that the
	// restarted process reads back on its next startup.
	StepRecordPending Step = "record-pending"

	// StepIssueRestart runs the actual `service apiary_raftd restart`.
	StepIssueRestart Step = "issue-restart"

	// StepAwaitConfirm waits, with a bounded budget, for the new process
	// to report itself back healthy (ADR-0125 §2).
	StepAwaitConfirm Step = "await-confirmation"

	// StepRecordOutcome persists the durable Result, whatever it says.
	StepRecordOutcome Step = "record-outcome"
)

// StepOrder is the canonical order, for callers and tests that want to
// assert it rather than re-derive it.
var StepOrder = []Step{
	StepGatherFacts,
	StepEvaluate,
	StepReserveLease,
	StepRecordPending,
	StepIssueRestart,
	StepAwaitConfirm,
	StepRecordOutcome,
}

// Phase is how a step ended.
type Phase string

const (
	// PhaseEnter means the step's effect was performed.
	PhaseEnter Phase = "enter"
	// PhaseSkip means the step was not reached because an earlier step
	// stopped the workflow.
	PhaseSkip Phase = "skip"
	// PhaseFail means the step was reached and its effect failed.
	PhaseFail Phase = "fail"
)

// StepEvent is one transition of the state machine, delivered to
// Engine.OnStep when set. It exists so the *order* of steps is
// observable: TestEngineWritesPendingBeforeRestartingCommand asserts from
// this stream that the pending record is written before the restart
// command is issued, and that a blocked evaluation never issues one at
// all. Order is the safety argument, so it is a thing tests check, not a
// thing they take on trust.
type StepEvent struct {
	Step   Step
	Phase  Phase
	Detail string
}

// Clock is the injected time source. A test drives the
// acknowledgment-timeout path by handing back a fake Clock, so
// TestEngineRestartThatNeverReturns exercises a 15-second timeout budget
// without spending 15 seconds and without any real timer flakiness.
type Clock interface {
	Now() time.Time
	// Sleep waits for d, or returns early with the context's error if
	// the context ends first. It must return a non-nil error in that
	// case - a Sleep that silently returns early would turn a cancelled
	// confirmation budget into a fast, wrong "we waited and gave up".
	Sleep(ctx context.Context, d time.Duration) error
}

// SystemClock is the production Clock.
type SystemClock struct{}

// Now implements Clock.
func (SystemClock) Now() time.Time { return time.Now() }

// Sleep implements Clock.
func (SystemClock) Sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// FactGatherer is the leader-side read-and-dial boundary: it produces
// everything the pure evaluator needs, already gathered. The production
// implementation necessarily lives in internal/manager (it holds the raft
// Status() handle and the peer dialer); the interface is here so this
// state machine can be driven end to end in a unit test with no host, no
// raft and no network.
//
// A gatherer that cannot read membership or run the probe must return
// fact.ProbeReadOK false rather than an optimistic guess - that is
// ADR-0125 §8's fail-closed rule, and it is the gatherer's
// responsibility precisely so this state machine never has to invent it.
type FactGatherer interface {
	GatherQuorumFact(ctx context.Context, service, nodeID string) QuorumFact
}

// FactGathererFunc adapts a plain function to FactGatherer.
type FactGathererFunc func(ctx context.Context, service, nodeID string) QuorumFact

// GatherQuorumFact implements FactGatherer.
func (f FactGathererFunc) GatherQuorumFact(ctx context.Context, service, nodeID string) QuorumFact {
	return f(ctx, service, nodeID)
}

// Lease is a granted, cluster-wide restart lease.
type Lease struct {
	ID     uint64
	Detail string
}

// Leaser is the raft-replicated restart-lease boundary. Reserved before
// the restart and released by the restarted process's own confirmation;
// this state machine never releases it itself, because only the
// confirmation path does (ADR-0103).
type Leaser interface {
	ReserveRestartLease(ctx context.Context, service, nodeID string, force bool) (Lease, error)
}

// LeaserFunc adapts a plain function to Leaser.
type LeaserFunc func(ctx context.Context, service, nodeID string, force bool) (Lease, error)

// ReserveRestartLease implements Leaser.
func (f LeaserFunc) ReserveRestartLease(ctx context.Context, service, nodeID string, force bool) (Lease, error) {
	return f(ctx, service, nodeID, force)
}

// Restarter is the one platform-touching effect in the whole package: the
// FreeBSD `service <name> restart` call. It is an interface so this
// package never shells out at import time and so a test can make it fail
// on demand - which is the only honest way to test the OutcomeFailed
// path, and the path where a non-zero exit must never be confused with
// "the node came back and we could not see it".
type Restarter interface {
	RestartService(ctx context.Context, service string) error
}

// RestarterFunc adapts a plain function to Restarter.
type RestarterFunc func(ctx context.Context, service string) error

// RestartService implements Restarter.
func (f RestarterFunc) RestartService(ctx context.Context, service string) error {
	return f(ctx, service)
}

// Confirmer is the one call that releases a restart lease, made by the
// restarted process itself once it is up and healthy. It returns an error
// when the confirmation did not happen, for any reason at all -
// transport, refusal, or managerd reporting an error of its own - because
// from this side of the boundary those are the same fact: no
// confirmation arrived.
//
// The production implementation is cmd/raftd's own; see that file for why
// it reaches managerd over the ManagerService gRPC surface rather than
// calling ConfirmRestartCompletedLocal directly (ADR-0125 §2's stated
// mechanism does not exist in this codebase - the gap is documented
// there and in this package's integration notes).
type Confirmer interface {
	ConfirmRestartCompleted(ctx context.Context, service, nodeID string, leaseID uint64) error
}

// ConfirmerFunc adapts a plain function to Confirmer.
type ConfirmerFunc func(ctx context.Context, service, nodeID string, leaseID uint64) error

// ConfirmRestartCompleted implements Confirmer.
func (f ConfirmerFunc) ConfirmRestartCompleted(ctx context.Context, service, nodeID string, leaseID uint64) error {
	return f(ctx, service, nodeID, leaseID)
}

// ConfirmOptions bounds the wait for the restarted process to report
// itself back. The defaults are ADR-0125 §2/§7's own numbers: 5 attempts,
// 3s apart - identical to cmd/managerd's proven hook, and bounded rather
// than indefinite, because an unbounded retry loop against a process that
// is not coming back is just a way of never finding out.
type ConfirmOptions struct {
	Attempts int
	Backoff  time.Duration
}

// DefaultConfirmOptions is ADR-0125 §7's confirm-on-startup budget.
func DefaultConfirmOptions() ConfirmOptions {
	return ConfirmOptions{Attempts: 5, Backoff: 3 * time.Second}
}

// WithDefaults fills in unset fields from DefaultConfirmOptions.
func (o ConfirmOptions) WithDefaults() ConfirmOptions {
	d := DefaultConfirmOptions()
	if o.Attempts <= 0 {
		o.Attempts = d.Attempts
	}
	if o.Backoff <= 0 {
		o.Backoff = d.Backoff
	}
	return o
}

// ConfirmOutcome is the whole result of waiting for a confirmation.
//
// Confirmed false is deliberately not a failure and not a success: it is
// the answer to "the restarted process never told us it was healthy",
// which is a different fact from "the restart failed" and is recorded as
// OutcomeUnobserved by whatever consumes this.
type ConfirmOutcome struct {
	Confirmed bool
	Attempts  int

	// LastDetail is the most recent reason the confirmation did not
	// happen, for an operator-facing record.
	LastDetail string

	// Cancelled is true when the wait ended because the caller's context
	// ended rather than because the attempt budget ran out.
	Cancelled bool

	// Evidence is one line per failed attempt, oldest first. It is
	// persisted with the Result so that "unknown" is always a record
	// that says what was tried, not a bare shrug.
	Evidence []string
}

// ConfirmWithRetry is the single retry loop both callers use: this
// package's Engine (waiting for its own restart) and cmd/raftd's startup
// hook (confirming a restart issued by a previous process). Having one
// loop rather than two is the point - managerd's hook and raftd's must
// not drift into different budgets, and the raft-replicated lease they
// are both responsible for releasing has no TTL to fall back on.
//
// The loop stops at the first success, at the attempt budget, or at a
// cancelled context, whichever comes first, and reports which of those
// happened so the caller can say so precisely.
func ConfirmWithRetry(ctx context.Context, confirmer Confirmer, pending PendingRestart, clock Clock, opts ConfirmOptions) ConfirmOutcome {
	opts = opts.WithDefaults()
	if confirmer == nil {
		return ConfirmOutcome{Attempts: 0, LastDetail: "no confirmer configured"}
	}
	if clock == nil {
		clock = SystemClock{}
	}
	out := ConfirmOutcome{}
	for attempt := 1; attempt <= opts.Attempts; attempt++ {
		if err := ctx.Err(); err != nil {
			out.Cancelled = true
			if out.LastDetail == "" {
				out.LastDetail = "cancelled before attempt " + fmt.Sprint(attempt) + ": " + err.Error()
			}
			return out
		}
		out.Attempts = attempt
		err := confirmer.ConfirmRestartCompleted(ctx, pending.Service, pending.NodeID, pending.LeaseID)
		if err == nil {
			out.Confirmed = true
			out.Evidence = append(out.Evidence, fmt.Sprintf("lease %d for %s on %s confirmed on attempt %d", pending.LeaseID, pending.Service, pending.NodeID, attempt))
			return out
		}
		out.LastDetail = err.Error()
		out.Evidence = append(out.Evidence, fmt.Sprintf("attempt %d/%d: %v", attempt, opts.Attempts, err))
		if attempt == opts.Attempts {
			break
		}
		if serr := clock.Sleep(ctx, opts.Backoff); serr != nil {
			out.Cancelled = true
			return out
		}
	}
	return out
}

// Engine drives the whole workflow once and produces the durable Result.
//
// Every field is an injected effect or a plain setting; Engine itself
// holds no hidden state, so two Runs of the same Engine are two
// independent attempts and a test can assert on each.
type Engine struct {
	// Service and NodeID name what is being restarted. Service defaults
	// to DefaultService when empty.
	Service string
	NodeID  string

	// Force is the operator-acknowledgment override. It is forwarded
	// BOTH to the pure evaluator (applied to the gathered fact, so the
	// advisory verdict and the enforced one can never disagree about
	// whether it was acknowledged) and to the lease reservation. It
	// never rescues an Unknown: see QuorumFact.Force.
	Force bool

	// Gatherer, Leaser, Restarter and Confirmer are the four effects.
	// Gatherer and Leaser are required for a run that can reach the
	// restart; Restarter and Confirmer are required for one that can
	// finish. A nil interface is reported as its own blocked outcome
	// rather than panicking, because "this build cannot do that" is a
	// fact worth recording, not a crash.
	Gatherer  FactGatherer
	Leaser    Leaser
	Restarter Restarter
	Confirmer Confirmer

	// Pending and Results are the two file-backed stores. Pending may be
	// nil, in which case the workflow refuses to issue a restart (it
	// cannot leave the discoverable trace that makes confirmation
	// possible) - see StepRecordPending.
	Pending *PendingStore
	Results *ResultStore

	// Clock and OnStep are the two test seams. Clock defaults to
	// SystemClock; OnStep may be nil.
	Clock  Clock
	OnStep func(StepEvent)

	// Confirm bounds the acknowledgment wait; zero means
	// DefaultConfirmOptions.
	Confirm ConfirmOptions
}

// Outcome reasons the state machine produces, kept as constants so a test
// and an operator-facing record name the same thing.
const (
	reasonEvalNotAllow  = "the quorum-safety guardrail did not allow this restart"
	reasonLeaseRefused  = "the cluster-wide restart lease was not granted"
	reasonPendingFailed = "the pending-restart record could not be written, so a restart could not be safely confirmed afterwards"
	reasonNoRestarter   = "no service restarter is configured, so the restart command could not be issued"
	reasonNoConfirmer   = "no confirmer is configured, so the restarted process could not be asked to confirm itself"
	reasonNoLeaseStore  = "no pending-restart store is configured, so no discoverable trace would be left for the restarted process"
)

// Run drives the workflow once and returns its durable Result.
//
// The returned error is non-nil in exactly one case: the Result could not
// be persisted. Everything else - a refused guardrail, a failed restart
// command, a node that never came back - is a legitimate outcome of the
// workflow and is returned in the Result with no error, because
// swallowing those into an error is exactly how "the node vanished" ends
// up logged as "restart succeeded" somewhere downstream.
func (e *Engine) Run(ctx context.Context) (Result, error) {
	e.init()
	started := e.Clock.Now()
	res := Result{
		Service:       e.Service,
		NodeID:        e.NodeID,
		StartedAtUnix: started.Unix(),
	}

	// Step 1: gather the facts the decision needs. A gatherer that
	// cannot read them reports ProbeReadOK false, and the evaluator
	// turns that into Unknown - which stops the workflow below. So this
	// step itself cannot fail the run; it only produces facts.
	e.step(StepGatherFacts, PhaseEnter, "")
	var fact QuorumFact
	if e.Gatherer == nil {
		// A missing gatherer is reported as "could not read", not as
		// "nothing to check": the honest verdict for a workflow that
		// never established quorum safety is Unknown, and it reaches the
		// same refusal below through the same path a real failed read
		// would.
		e.step(StepGatherFacts, PhaseFail, "no fact gatherer configured")
		fact = QuorumFact{
			Service: e.Service, TargetNodeID: e.NodeID,
			ProbeReadOK: false,
			ProbeError:  "no fact gatherer is configured, so cluster membership and peer reachability were never read",
		}
	} else {
		fact = e.Gatherer.GatherQuorumFact(ctx, e.Service, e.NodeID)
	}
	// The operator's acknowledgment belongs to the *request*, not to the
	// read of the cluster, so it is applied here rather than being left
	// to the gatherer - a gatherer has no way to know whether this
	// particular request carried Force, and a Force that reached the
	// lease but not the advisory evaluation (or the reverse) would let
	// the preview and the enforcement disagree. A gatherer that already
	// set Force keeps it; Force is additive, never subtractive.
	fact.Force = fact.Force || e.Force

	// Step 2: the pure decision. A refusal here has touched no
	// cluster-wide state, which is exactly why it comes before the lease.
	report := EvaluateQuorumSafety(fact)
	if report.Verdict != guardrail.Allow {
		e.step(StepEvaluate, PhaseFail, string(report.Verdict))
		res.Outcome = OutcomeBlocked
		res.Detail = reasonEvalNotAllow + " (" + string(report.Verdict) + ")"
		res.Evidence = evidenceOf(report)
		e.skipFrom(StepReserveLease)
		return e.finish(res, started)
	}
	e.step(StepEvaluate, PhaseEnter, "allowed")
	// Whatever the evaluation had to say - including the caveats a Force
	// override leaves behind - is evidence for whatever happens next, and
	// a later step must not silently drop it by assigning over it. Every
	// evidence assignment below this line therefore appends.
	res.Evidence = evidenceOf(report)

	// Step 3: the real, raft-replicated serialization point.
	if e.Leaser == nil {
		e.step(StepReserveLease, PhaseFail, "no leaser configured")
		res.Outcome = OutcomeBlocked
		res.Detail = reasonLeaseRefused + ": no leaser is configured"
		e.skipFrom(StepRecordPending)
		return e.finish(res, started)
	}
	lease, err := e.Leaser.ReserveRestartLease(ctx, e.Service, e.NodeID, e.Force)
	if err != nil || lease.ID == 0 {
		e.step(StepReserveLease, PhaseFail, firstNonEmpty(errString(err), "lease id 0 is not a granted lease"))
		res.Outcome = OutcomeBlocked
		res.Detail = reasonLeaseRefused + ": " + firstNonEmpty(errString(err), "granted lease carried id 0")
		e.skipFrom(StepRecordPending)
		return e.finish(res, started)
	}
	res.LeaseID = lease.ID
	e.step(StepReserveLease, PhaseEnter, fmt.Sprintf("lease %d", lease.ID))

	// Step 4: the local trace, written BEFORE the restart command. A
	// crash in the window between here and the command being issued
	// still leaves the record, so the next startup self-confirms.
	if e.Pending == nil {
		e.step(StepRecordPending, PhaseFail, "no pending store configured")
		res.Outcome = OutcomeBlocked
		res.Detail = reasonPendingFailed + ": " + reasonNoLeaseStore
		res.Evidence = []string{fmt.Sprintf("lease %d for %s on %s is now reserved and still held; an operator must confirm or force-clear it", lease.ID, e.Service, e.NodeID)}
		e.skipFrom(StepIssueRestart)
		return e.finish(res, started)
	}
	pending := PendingRestart{Service: e.Service, NodeID: e.NodeID, LeaseID: lease.ID}
	if err := e.Pending.Save(pending); err != nil {
		e.step(StepRecordPending, PhaseFail, err.Error())
		res.Outcome = OutcomeBlocked
		res.Detail = reasonPendingFailed + ": " + err.Error()
		res.Evidence = append(res.Evidence, fmt.Sprintf("lease %d for %s on %s is now reserved and still held; an operator must confirm or force-clear it", lease.ID, e.Service, e.NodeID))
		e.skipFrom(StepIssueRestart)
		return e.finish(res, started)
	}
	e.step(StepRecordPending, PhaseEnter, pendingFileNote(pending))

	// Step 5: the actual restart. A non-zero exit here is a positive
	// observation of failure - the one case in this whole workflow where
	// "failed" is a fact rather than an absence of one.
	if e.Restarter == nil {
		e.step(StepIssueRestart, PhaseFail, "no restarter configured")
		res.Outcome = OutcomeBlocked
		res.Detail = reasonNoRestarter + " (lease " + fmt.Sprint(lease.ID) + " is reserved and its pending record is on disk)"
		e.skipFrom(StepAwaitConfirm)
		return e.finish(res, started)
	}
	if err := e.Restarter.RestartService(ctx, e.Service); err != nil {
		e.step(StepIssueRestart, PhaseFail, err.Error())
		res.Outcome = OutcomeFailed
		res.Detail = "the restart command for " + e.Service + " returned an error"
		res.Evidence = append(res.Evidence, err.Error())
		// The pending record is deliberately NOT cleared: a lease is
		// held cluster-wide and only a real confirmation releases it.
		// Leaving the record is the conservative choice, and it matches
		// managerd's own hook exactly.
		e.skipFrom(StepAwaitConfirm)
		return e.finish(res, started)
	}
	e.step(StepIssueRestart, PhaseEnter, "restart command returned success")

	// Step 6: did the new process come back and say so?
	if e.Confirmer == nil {
		e.step(StepAwaitConfirm, PhaseFail, "no confirmer configured")
		res.Outcome = OutcomeUnobserved
		res.Detail = reasonNoConfirmer + "; the restart command itself succeeded, so this is 'could not check', not 'restarted fine'"
		e.step(StepRecordOutcome, PhaseEnter, "")
		return e.finish(res, started)
	}
	confirm := ConfirmWithRetry(ctx, e.Confirmer, pending, e.Clock, e.Confirm)
	if !confirm.Confirmed {
		e.step(StepAwaitConfirm, PhaseFail, confirm.LastDetail)
		// The heart of the honesty requirement. The restart was issued
		// and nothing came back within a bounded budget. That is not a
		// failed restart (we never saw it fail) and emphatically not a
		// successful one (we never saw it come back): it is unobserved,
		// and it is recorded with every attempt that was made.
		res.Outcome = OutcomeUnobserved
		res.Detail = "the restart command succeeded but the restarted process never confirmed itself within " +
			fmt.Sprint(e.Confirm.WithDefaults().Attempts) + " attempts; the lease remains held and the node's state is genuinely unknown"
		if confirm.Cancelled {
			res.Detail += " (the wait was cancelled, not exhausted)"
		}
		res.Evidence = append(res.Evidence, confirm.Evidence...)
		e.step(StepRecordOutcome, PhaseEnter, "")
		return e.finish(res, started)
	}
	e.step(StepAwaitConfirm, PhaseEnter, fmt.Sprintf("confirmed on attempt %d", confirm.Attempts))

	// Only now, on an actual confirmation, is the local trace cleared.
	if err := e.Pending.Clear(e.Service); err != nil {
		e.step(StepRecordOutcome, PhaseFail, "clearing pending record: "+err.Error())
		res.Outcome = OutcomeConfirmed
		res.Detail = "the restarted process confirmed itself, but its pending-restart record could not be removed"
		res.Evidence = append(res.Evidence, confirm.Evidence...)
		res.Evidence = append(res.Evidence, err.Error())
		return e.finish(res, started)
	}

	res.Outcome = OutcomeConfirmed
	res.Detail = "the restarted process reported itself back healthy and the restart lease was released"
	res.Evidence = append(res.Evidence, confirm.Evidence...)
	e.step(StepRecordOutcome, PhaseEnter, "")
	return e.finish(res, started)
}

// finish stamps the Result, validates it, and persists it. A Result that
// fails its own Validate is a bug in this file, not bad input, and is
// reported as an error rather than written to disk.
func (e *Engine) finish(res Result, started time.Time) (Result, error) {
	res.FinishedAtUnix = e.Clock.Now().Unix()
	if res.Outcome == "" {
		res.Outcome = OutcomeUnverified
		res.Detail = "the workflow ended without reaching a decision"
	}
	if res.Detail == "" {
		res.Detail = "no detail recorded"
	}
	if err := res.Validate(); err != nil {
		return res, err
	}
	if e.Results == nil {
		return res, nil
	}
	if err := e.Results.Save(res); err != nil {
		return res, fmt.Errorf("restartplan: persisting restart result for %s on %s: %w", res.Service, res.NodeID, err)
	}
	return res, nil
}

func (e *Engine) init() {
	if e.Service == "" {
		e.Service = DefaultService
	}
	if e.Clock == nil {
		e.Clock = SystemClock{}
	}
	e.Confirm = e.Confirm.WithDefaults()
}

func (e *Engine) step(s Step, p Phase, detail string) {
	if e.OnStep != nil {
		e.OnStep(StepEvent{Step: s, Phase: p, Detail: detail})
	}
}

// skipFrom reports every step from s onward as skipped, so a caller
// watching the stream can tell "we stopped here" from "the code fell off
// the end".
func (e *Engine) skipFrom(s Step) {
	seen := false
	for _, st := range StepOrder {
		if st == s {
			seen = true
		}
		if seen {
			e.step(st, PhaseSkip, "workflow stopped before this step")
		}
	}
}

func pendingFileNote(p PendingRestart) string {
	return fmt.Sprintf("pending record written: %s lease %d on %s", p.Service, p.LeaseID, p.NodeID)
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// evidenceOf flattens a guardrail Report's findings and caveats into the
// Result's own flat string Evidence, in a stable order, so a blocked or
// refused restart records exactly which rules stopped it rather than just
// that something did.
func evidenceOf(report guardrail.Report) []string {
	var out []string
	for _, f := range report.Findings {
		s := f.Rule + ": " + f.Detail
		if len(f.Evidence) > 0 {
			var cites []string
			for _, e := range f.Evidence {
				cites = append(cites, e.Detail)
			}
			s += " [" + strings.Join(cites, "; ") + "]"
		}
		out = append(out, s)
	}
	for _, c := range report.Caveats {
		out = append(out, "caveat: "+c.Source+": "+c.Detail)
	}
	return out
}
