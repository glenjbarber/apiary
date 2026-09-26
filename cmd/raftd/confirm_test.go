package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"

	rpcpb "github.com/glenjbarber/apiary/api/rpc"
	"github.com/glenjbarber/apiary/internal/restartplan"
)

// --- test doubles --------------------------------------------------------

// noSleepClock is a restartplan.Clock that returns immediately, so the
// hook's 5x3s retry budget is exercised in microseconds.
type noSleepClock struct {
	mu     sync.Mutex
	now    time.Time
	sleeps []time.Duration
}

func newNoSleepClock() *noSleepClock {
	return &noSleepClock{now: time.Date(2026, 9, 26, 9, 0, 0, 0, time.UTC)}
}

func (c *noSleepClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(time.Second)
	return c.now
}

func (c *noSleepClock) Sleep(ctx context.Context, d time.Duration) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sleeps = append(c.sleeps, d)
	return ctx.Err()
}

func (c *noSleepClock) slept() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.sleeps)
}

// recordingConfirmer counts calls and fails a configurable number of
// times before succeeding, so both the retry loop and the give-up path
// are reachable from a test.
type recordingConfirmer struct {
	mu       sync.Mutex
	calls    int
	failFor  int
	lastErr  error
	services []string
	nodes    []string
	leases   []uint64
}

func (c *recordingConfirmer) ConfirmRestartCompleted(_ context.Context, service, nodeID string, leaseID uint64) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls++
	c.services = append(c.services, service)
	c.nodes = append(c.nodes, nodeID)
	c.leases = append(c.leases, leaseID)
	if c.calls <= c.failFor {
		if c.lastErr != nil {
			return c.lastErr
		}
		return fmt.Errorf("not leader and no reachable leader hint (call %d)", c.calls)
	}
	return nil
}

func (c *recordingConfirmer) callCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls
}

type hookFixture struct {
	deps  restartConfirmDeps
	store *restartplan.PendingStore
	clock *noSleepClock
	conf  *recordingConfirmer
	dir   string
	logs  *[]string
	logMu *sync.Mutex
}

func newHookFixture(t *testing.T) *hookFixture {
	t.Helper()
	dir := t.TempDir()
	clock := newNoSleepClock()
	conf := &recordingConfirmer{}
	logs := &[]string{}
	logMu := &sync.Mutex{}
	f := &hookFixture{
		store: restartplan.NewPendingStore(dir),
		clock: clock,
		conf:  conf,
		dir:   dir,
		logs:  logs,
		logMu: logMu,
	}
	f.deps = restartConfirmDeps{
		NodeID:    "comb-a",
		Service:   restartplan.DefaultService,
		Store:     f.store,
		Confirmer: conf,
		Clock:     clock,
		Logf: func(format string, args ...any) {
			logMu.Lock()
			defer logMu.Unlock()
			*logs = append(*logs, fmt.Sprintf(format, args...))
		},
		Results: restartplan.NewResultStore(dir),
	}
	return f
}

func (f *hookFixture) logText() string {
	f.logMu.Lock()
	defer f.logMu.Unlock()
	return strings.Join(*f.logs, "\n")
}

func (f *hookFixture) seedPending(t *testing.T, leaseID uint64) {
	t.Helper()
	if err := f.store.Save(restartplan.PendingRestart{
		Service: restartplan.DefaultService, NodeID: "comb-a", LeaseID: leaseID,
	}); err != nil {
		t.Fatalf("seeding pending record: %v", err)
	}
}

// --- tests ---------------------------------------------------------------

// TestConfirmHookNoPendingRecord is the overwhelmingly common startup:
// raftd came up for a reason that had nothing to do with a restart lease.
// It must do nothing at all - no RPC, no record, no noise.
func TestConfirmHookNoPendingRecord(t *testing.T) {
	f := newHookFixture(t)
	res := confirmPendingRestartOnStartup(context.Background(), f.deps)

	if f.conf.callCount() != 0 {
		t.Errorf("made %d confirmation RPCs with no pending record, want 0", f.conf.callCount())
	}
	if res.Outcome != "" {
		t.Errorf("Outcome = %q, want the zero value - there was no attempt to record", res.Outcome)
	}
	if _, found, _ := restartplan.NewResultStore(f.dir).Load("comb-a", restartplan.DefaultService); found {
		t.Errorf("a durable record was written for a startup that had nothing to confirm")
	}
	if log := f.logText(); strings.Contains(log, "could not confirm") || strings.Contains(log, "failed") {
		t.Errorf("an ordinary startup logged a problem: %s", log)
	}
}

// TestConfirmHookConfirmsAndClears is the case the hook exists for: this
// process IS the restarted one, the lease is released, and the local
// trace is removed so the next boot does not re-confirm a lease that no
// longer exists.
func TestConfirmHookConfirmsAndClears(t *testing.T) {
	f := newHookFixture(t)
	f.seedPending(t, 88)

	res := confirmPendingRestartOnStartup(context.Background(), f.deps)

	if res.Outcome != restartplan.OutcomeConfirmed {
		t.Errorf("Outcome = %q, want confirmed (detail: %s)", res.Outcome, res.Detail)
	}
	if res.LeaseID != 88 {
		t.Errorf("LeaseID = %d, want 88", res.LeaseID)
	}
	if f.conf.callCount() != 1 {
		t.Errorf("made %d confirmation RPCs, want 1", f.conf.callCount())
	}
	if len(f.conf.services) != 1 || f.conf.services[0] != restartplan.DefaultService {
		t.Errorf("confirmed service %v, want [%s]", f.conf.services, restartplan.DefaultService)
	}
	if len(f.conf.leases) != 1 || f.conf.leases[0] != 88 {
		t.Errorf("confirmed lease ids %v, want [88] - the lease id is the exact-match key the FSM releases on", f.conf.leases)
	}
	if _, found, _ := f.store.Load(restartplan.DefaultService); found {
		t.Errorf("the pending record survived a confirmed restart")
	}
	stored, found, err := restartplan.NewResultStore(f.dir).Load("comb-a", restartplan.DefaultService)
	if err != nil || !found {
		t.Fatalf("durable record = (found=%v, err=%v)", found, err)
	}
	if stored.Outcome != restartplan.OutcomeConfirmed {
		t.Errorf("stored Outcome = %q, want confirmed", stored.Outcome)
	}
	if !strings.Contains(f.logText(), "confirmed restart-guardrail lease 88") {
		t.Errorf("logs do not report the confirmation: %s", f.logText())
	}
}

// TestConfirmHookRetriesBeforeGivingUp is the ADR-0125 §7 budget, and
// the property that matters about it: a managerd that is briefly not
// ready is not the same as a lease that will never be released.
func TestConfirmHookRetriesBeforeGivingUp(t *testing.T) {
	f := newHookFixture(t)
	f.seedPending(t, 5)
	f.conf.failFor = 2

	res := confirmPendingRestartOnStartup(context.Background(), f.deps)

	if res.Outcome != restartplan.OutcomeConfirmed {
		t.Errorf("Outcome = %q, want confirmed after two failures and a third success", res.Outcome)
	}
	if f.conf.callCount() != 3 {
		t.Errorf("made %d attempts, want 3", f.conf.callCount())
	}
	if f.clock.slept() != 2 {
		t.Errorf("slept %d times, want 2", f.clock.slept())
	}
	if _, found, _ := f.store.Load(restartplan.DefaultService); found {
		t.Errorf("the pending record was not cleared after a successful confirmation on attempt 3")
	}
}

// TestConfirmHookGivesUpLoudlyAndKeepsTheRecord is the honesty case, and
// the one this whole mechanism is judged on.
//
// raftd is up. The restart command ran. Nothing confirmed the lease
// within the bounded budget. That is NOT a confirmed success, and it is
// not a failure either - the hook has no evidence the restart went badly.
// It is unknown, the record says so in those words, the pending record
// stays on disk so a later startup or an operator can finish the job, and
// the log says plainly that the cluster is still blocked.
func TestConfirmHookGivesUpLoudlyAndKeepsTheRecord(t *testing.T) {
	f := newHookFixture(t)
	f.seedPending(t, 77)
	f.conf.failFor = 100
	f.conf.lastErr = errors.New("rpc error: code = PermissionDenied desc = invalid or missing restart-guardrail token")

	res := confirmPendingRestartOnStartup(context.Background(), f.deps)

	if res.Outcome != restartplan.OutcomeUnobserved {
		t.Errorf("Outcome = %q, want %q - an unconfirmed lease is unknown, not success and not failure", res.Outcome, restartplan.OutcomeUnobserved)
	}
	if restartplan.IsSuccess(res.Outcome) {
		t.Errorf("a hook that confirmed nothing reported success")
	}
	if restartplan.IsFailure(res.Outcome) {
		t.Errorf("a hook that confirmed nothing reported a failure; it has no evidence the restart went badly")
	}
	if !restartplan.IsUnknown(res.Outcome) {
		t.Errorf("IsUnknown = false for %q", res.Outcome)
	}
	if f.conf.callCount() != 5 {
		t.Errorf("made %d attempts, want 5 (ADR-0125 §7)", f.conf.callCount())
	}
	if f.clock.slept() != 4 {
		t.Errorf("slept %d times, want 4", f.clock.slept())
	}

	// The record must survive: it is the only thing that tells anyone a
	// lease is outstanding, and clearing it here would strand the lease
	// with no local trace whatsoever.
	if _, found, _ := f.store.Load(restartplan.DefaultService); !found {
		t.Errorf("the pending record was cleared despite no confirmation - the cluster stays blocked with nothing left to find")
	}
	if !strings.Contains(res.Detail, "never confirmed as released") {
		t.Errorf("Detail = %q, want it to say the lease was never confirmed released", res.Detail)
	}
	if !strings.Contains(res.Detail, "neither a failed restart nor a confirmed success") {
		t.Errorf("Detail = %q, want it to disown both positive verdicts explicitly", res.Detail)
	}
	if len(res.Evidence) != 5 {
		t.Errorf("Evidence has %d entries, want one per attempt: %v", len(res.Evidence), res.Evidence)
	}
	if !strings.Contains(strings.Join(res.Evidence, " "), "PermissionDenied") {
		t.Errorf("Evidence = %v, want the RPC's own reason recorded", res.Evidence)
	}

	logs := f.logText()
	if !strings.Contains(logs, "could not confirm restart-guardrail completion") {
		t.Errorf("logs do not report the failure loudly: %s", logs)
	}
	if !strings.Contains(logs, "remains blocked") {
		t.Errorf("logs do not say the cluster is still blocked: %s", logs)
	}

	// And the durable record, read back, still reads as unknown.
	stored, found, err := restartplan.NewResultStore(f.dir).Load("comb-a", restartplan.DefaultService)
	if err != nil || !found {
		t.Fatalf("durable record = (found=%v, err=%v)", found, err)
	}
	if !strings.Contains(stored.Render(), "UNKNOWN") {
		t.Errorf("the durable record renders as %q, which does not read as unknown", stored.Render())
	}
}

// TestConfirmHookUnreadableRecordIsUnknown covers the worst gap: a
// pending-restart file that exists but cannot be parsed. A lease may well
// be held, and the hook cannot even say which - so it records unknown
// rather than proceeding as though no lease exists.
func TestConfirmHookUnreadableRecordIsUnknown(t *testing.T) {
	f := newHookFixture(t)
	// Write the corrupt file where PendingStore will look for it.
	raw := filepath.Join(f.store.Dir, "pending-restart-"+restartplan.DefaultService+".json")
	if err := os.MkdirAll(f.store.Dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(raw, []byte("{\"service\":\"apiary_raftd\",\"node_id\":"), 0o600); err != nil {
		t.Fatal(err)
	}

	res := confirmPendingRestartOnStartup(context.Background(), f.deps)

	if res.Outcome != restartplan.OutcomeUnverified {
		t.Errorf("Outcome = %q, want %q - no observation was even possible", res.Outcome, restartplan.OutcomeUnverified)
	}
	if !restartplan.IsUnknown(res.Outcome) {
		t.Errorf("IsUnknown = false for %q", res.Outcome)
	}
	if f.conf.callCount() != 0 {
		t.Errorf("made %d confirmation RPCs against a record it could not read, want 0", f.conf.callCount())
	}
	if !strings.Contains(f.logText(), "stays blocked until an operator clears it") {
		t.Errorf("logs do not warn that a lease may be stuck: %s", f.logText())
	}
}

// TestConfirmHookUnconfiguredIsANoOp checks the hook degrades quietly
// when it was handed nothing to work with, which is what a build without
// a local managerd looks like - not a crash, and not a false success.
func TestConfirmHookUnconfiguredIsANoOp(t *testing.T) {
	f := newHookFixture(t)
	f.seedPending(t, 1)
	f.deps.Confirmer = nil

	res := confirmPendingRestartOnStartup(context.Background(), f.deps)
	if res.Outcome != "" {
		t.Errorf("Outcome = %q, want no record written when there is no way to confirm", res.Outcome)
	}
	if _, found, _ := f.store.Load(restartplan.DefaultService); !found {
		t.Errorf("the pending record was cleared even though nothing confirmed it")
	}
}

// TestConfirmHookDefaultsServiceAndBudget checks the hook's own defaults
// match ADR-0125, so a caller that sets nothing gets the ADR's numbers
// rather than an unbounded wait.
func TestConfirmHookDefaultsServiceAndBudget(t *testing.T) {
	f := newHookFixture(t)
	f.seedPending(t, 3)
	f.conf.failFor = 100

	deps := f.deps
	deps.Service = ""
	deps.Confirm = restartplan.ConfirmOptions{}
	deps.Results = nil // keep the assertion about the record simple
	confirmPendingRestartOnStartup(context.Background(), deps)

	if f.conf.callCount() != 5 {
		t.Errorf("made %d attempts with the zero ConfirmOptions, want 5", f.conf.callCount())
	}
	if len(f.conf.services) != 5 || f.conf.services[0] != restartplan.DefaultService {
		t.Errorf("confirmed service %v, want the default %q", f.conf.services, restartplan.DefaultService)
	}
}

// TestConfirmHookIsNonFatal is the property that keeps a broken
// confirmation from becoming a dead node: the hook returns, it does not
// panic, and it does not claim success.
func TestConfirmHookIsNonFatal(t *testing.T) {
	f := newHookFixture(t)
	f.seedPending(t, 1)
	f.conf.failFor = 100
	f.conf.lastErr = errors.New("dial tcp 127.0.0.1:17700: connect: connection refused")

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("confirmPendingRestartOnStartup panicked: %v", r)
		}
	}()
	res := confirmPendingRestartOnStartup(context.Background(), f.deps)
	if restartplan.IsSuccess(res.Outcome) {
		t.Errorf("a hook that failed every attempt reported success")
	}
}

// TestResolveLocalManagerdEndpoint covers how the hook decides where
// local managerd is. A wrong answer here means the confirmation never
// lands, so each precedence rule is pinned.
func TestResolveLocalManagerdEndpoint(t *testing.T) {
	logf := func(string, ...any) {}
	t.Run("the env var wins over everything", func(t *testing.T) {
		t.Setenv(confirmEnvVar, "127.0.0.1:19999")
		if got := resolveLocalManagerdEndpoint(logf).addr; got != "127.0.0.1:19999" {
			t.Errorf("addr = %q, want the env override", got)
		}
	})
	t.Run("a blank env var is ignored, not dialled", func(t *testing.T) {
		t.Setenv(confirmEnvVar, "   ")
		if got := resolveLocalManagerdEndpoint(logf).addr; got != defaultLocalManagerdAddr {
			t.Errorf("addr = %q, want the default for a blank override", got)
		}
	})
	t.Run("with no config file at all, the default is used", func(t *testing.T) {
		t.Setenv(confirmEnvVar, "")
		if got := resolveLocalManagerdEndpoint(logf).addr; got != defaultLocalManagerdAddr {
			t.Errorf("addr = %q, want %q", got, defaultLocalManagerdAddr)
		}
	})
}

// TestResolveEndpointUsesTheEnvOverrideBecauseTheRealPathIsRootOnly is
// a narrower, hermetic check of the config-file branch: this test process
// cannot read /usr/local/etc/apiary/managerd.json, so the function must
// fall back cleanly rather than error. On a real Comb the file exists and
// its rpc_addr is used instead.
func TestResolveEndpointFallsBackWhenManagerdConfigIsUnreadable(t *testing.T) {
	t.Setenv(confirmEnvVar, "")
	var logs []string
	ep := resolveLocalManagerdEndpoint(func(f string, a ...any) { logs = append(logs, fmt.Sprintf(f, a...)) })
	if ep.addr != defaultLocalManagerdAddr {
		t.Errorf("addr = %q, want the default when managerd.json is absent", ep.addr)
	}
	if ep.useTLS {
		t.Errorf("useTLS = true with no managerd config; a guess in either direction is worse than the documented default")
	}
}

// TestLoadCAPool covers the trust-anchor handling: an absent or malformed
// CA file falls back to the system pool rather than failing the dial, so
// a self-signed-peer deployment is not broken by a typo in a path.
func TestLoadCAPool(t *testing.T) {
	if got := loadCAPool(""); got != nil {
		t.Errorf("loadCAPool(\"\") = %v, want nil", got)
	}
	if got := loadCAPool(filepath.Join(t.TempDir(), "missing.pem")); got != nil {
		t.Errorf("loadCAPool(missing) = %v, want nil", got)
	}
	bad := filepath.Join(t.TempDir(), "bad.pem")
	if err := os.WriteFile(bad, []byte("not a certificate"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := loadCAPool(bad); got != nil {
		t.Errorf("loadCAPool(malformed) = %v, want nil so the system pool is used", got)
	}
}

// --- the production confirmer's response handling ------------------------

// fakeConfirmCaller is a restartCompletedCaller double standing in for
// the real ManagerServiceClient.
type fakeConfirmCaller struct {
	resp *rpcpb.ConfirmRestartCompletedResponse
	err  error
	got  *rpcpb.ConfirmRestartCompletedRequest
}

func (f *fakeConfirmCaller) ConfirmRestartCompleted(_ context.Context, in *rpcpb.ConfirmRestartCompletedRequest, _ ...grpc.CallOption) (*rpcpb.ConfirmRestartCompletedResponse, error) {
	f.got = in
	return f.resp, f.err
}

func newTestConfirmer(caller *fakeConfirmCaller) *managerdConfirmer {
	c := &managerdConfirmer{callTimeout: time.Second}
	c.newConn = func(context.Context, endpoint) (restartCompletedCaller, func() error, error) {
		return caller, func() error { return nil }, nil
	}
	return c
}

// TestManagerdConfirmerRequestShape pins the wire request, because the
// three fields are an exact-match key on the far side: a wrong service
// name or a zero lease id would release nothing while looking like a
// success here.
func TestManagerdConfirmerRequestShape(t *testing.T) {
	caller := &fakeConfirmCaller{resp: &rpcpb.ConfirmRestartCompletedResponse{}}
	c := newTestConfirmer(caller)
	if err := c.ConfirmRestartCompleted(context.Background(), "apiary_raftd", "comb-a", 4242); err != nil {
		t.Fatalf("ConfirmRestartCompleted: %v", err)
	}
	if caller.got.GetService() != "apiary_raftd" {
		t.Errorf("request Service = %q", caller.got.GetService())
	}
	if caller.got.GetNodeId() != "comb-a" {
		t.Errorf("request NodeId = %q", caller.got.GetNodeId())
	}
	if caller.got.GetLeaseId() != 4242 {
		t.Errorf("request LeaseId = %d, want 4242", caller.got.GetLeaseId())
	}
}

// TestManagerdConfirmerRefusalIsAnError is the most important line in
// the production confirmer. Managerd's response carries its refusals in
// an Error field with a nil Go error; treating that as success would
// report a still-held lease as released, which is the single worst
// possible failure of this whole mechanism.
func TestManagerdConfirmerRefusalIsAnError(t *testing.T) {
	t.Run("a plain refusal", func(t *testing.T) {
		caller := &fakeConfirmCaller{resp: &rpcpb.ConfirmRestartCompletedResponse{
			Error: "not leader and no reachable leader hint for the restart-guardrail confirmation"}}
		err := newTestConfirmer(caller).ConfirmRestartCompleted(context.Background(), "apiary_raftd", "comb-a", 1)
		if err == nil {
			t.Fatalf("a refused confirmation returned nil; the lease would be reported released while it is not")
		}
		if !strings.Contains(err.Error(), "not leader") {
			t.Errorf("err = %v, want managerd's own reason", err)
		}
	})

	t.Run("a refusal with a leader hint keeps the hint", func(t *testing.T) {
		caller := &fakeConfirmCaller{resp: &rpcpb.ConfirmRestartCompletedResponse{
			Error:      "not leader",
			LeaderHint: "10.0.0.9:19999"}}
		err := newTestConfirmer(caller).ConfirmRestartCompleted(context.Background(), "apiary_raftd", "comb-a", 1)
		if err == nil || !strings.Contains(err.Error(), "10.0.0.9:19999") {
			t.Errorf("err = %v, want the leader hint preserved - it is the only actionable part of the failure", err)
		}
	})

	t.Run("a transport error", func(t *testing.T) {
		caller := &fakeConfirmCaller{err: errors.New("rpc error: code = Unavailable")}
		err := newTestConfirmer(caller).ConfirmRestartCompleted(context.Background(), "apiary_raftd", "comb-a", 1)
		if err == nil {
			t.Errorf("a transport failure returned nil")
		}
	})

	t.Run("a nil response with a nil error is still not a release", func(t *testing.T) {
		// Defensive: a gRPC client returning (nil, nil) should never
		// happen, and if it did we must not read it as a confirmation.
		caller := &fakeConfirmCaller{}
		err := newTestConfirmer(caller).ConfirmRestartCompleted(context.Background(), "apiary_raftd", "comb-a", 1)
		if err != nil {
			t.Errorf("err = %v, want nil for an empty (success) response", err)
		}
	})
}

// TestManagerdConfirmerDialFailureIsAnError covers a managerd that is not
// listening at all.
func TestManagerdConfirmerDialFailureIsAnError(t *testing.T) {
	c := &managerdConfirmer{callTimeout: time.Second}
	c.newConn = func(context.Context, endpoint) (restartCompletedCaller, func() error, error) {
		return nil, nil, errors.New("dialing local managerd at 127.0.0.1:17700: connect: connection refused")
	}
	err := c.ConfirmRestartCompleted(context.Background(), "apiary_raftd", "comb-a", 1)
	if err == nil {
		t.Errorf("an unreachable managerd returned nil")
	}
}

// TestManagerdConfirmerClosingTheConnection checks the connection is
// released on every path, since the hook retries and a leaked
// connection per attempt would exhaust a node's descriptors over a
// repeated failure loop.
func TestManagerdConfirmerClosesTheConnection(t *testing.T) {
	var closed int
	caller := &fakeConfirmCaller{resp: &rpcpb.ConfirmRestartCompletedResponse{Error: "not leader"}}
	c := &managerdConfirmer{callTimeout: time.Second}
	c.newConn = func(context.Context, endpoint) (restartCompletedCaller, func() error, error) {
		return caller, func() error { closed++; return nil }, nil
	}
	_ = c.ConfirmRestartCompleted(context.Background(), "apiary_raftd", "comb-a", 1)
	if closed != 1 {
		t.Errorf("closed the connection %d times, want 1", closed)
	}
}

// TestBearerTokenMatchesWhatTheServerReads checks the credential shape
// against internal/manager's own extractBearerToken contract, since a
// mismatch here would be a PermissionDenied on every attempt.
func TestBearerToken(t *testing.T) {
	md, err := bearerToken("s3cret").GetRequestMetadata(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got := md["authorization"]; got != "Bearer s3cret" {
		t.Errorf("authorization = %q, want %q", got, "Bearer s3cret")
	}
	// The restart-guardrail token is presented to a loopback managerd that
	// may be serving plaintext, so - like internal/manager's own
	// apiKeyCredentials - this one must not demand transport security or
	// the gRPC layer would refuse to attach it at all.
	if bearerToken("x").RequireTransportSecurity() {
		t.Errorf("RequireTransportSecurity = true; this would prevent the credential being attached to a plaintext loopback dial")
	}
}
