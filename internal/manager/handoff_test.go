package manager

// Tests for ADR-0146's managerd self-restart handoff.
//
// WHAT IS REAL HERE, and it is most of the path:
//
//   - a real raft cluster on loopback (raftnode.Node, Bootstrap,
//     AddVoter), so the single-flight acquire, the fence, the handover,
//     the restart lease and the durable step record are all decided by
//     the real FSM;
//   - a real managerd gRPC server per node behind the real auth
//     interceptors, so the restart-guardrail token check, the
//     leader-forward of the lease reservation and of the handover, and
//     the FSM applies are all real;
//   - the real internal/buildgate over a REAL libexec directory and REAL
//     log files, with genuinely executable stand-in daemons that print a
//     build line under -version. The "running versus on disk" claim is
//     the load-bearing part of rule 5, and a stubbed Report would test
//     the stub.
//
// WHAT IS NOT REAL, and exactly two things:
//
//   - the peer DIAL, for the reason restartcaller_test.go's own harness
//     documents at length: three loopback raft addresses all reduce to
//     one 127.0.0.1 managerd port, so three managerds cannot all be
//     addressed. handoffForwarder replaces the dial and nothing else.
//   - the rc.d service controller, because `service apiary_managerd
//     restart` cannot be run on a test machine. recordingRestarter
//     records what would have been issued and asserts the ARGUMENTS the
//     real implementation would have used, because the arguments are
//     where ADR-0146 rule 4's three failure modes are closed or not.
//
// The one thing neither fake touches is the fork itself. Rule 4 is
// explicit that forking the restart out of a live managerd has not been
// reproduced on a real Comb, and TestSetsidServiceRestarter_AimsAtANewSession
// pins the properties that make it plausible rather than pretending to
// have proved it. That reproduction is a FreeBSD exercise against brood
// or drone and is not something a Go test on macOS can stand in for.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc/metadata"

	internalpb "github.com/glenjbarber/apiary/api/internalpb"

	rpcpb "github.com/glenjbarber/apiary/api/rpc"
	"github.com/glenjbarber/apiary/internal/buildgate"
	"github.com/glenjbarber/apiary/internal/isostore"
)

// directTokenCtx is a context carrying the guardrail token as INCOMING
// metadata, for calls a test makes straight at a handler rather than over
// a real gRPC connection.
//
// It is a different helper from tokenCtx above on purpose rather than by
// accident: tokenCtx is an OUTGOING context, because it is used to dial a
// real listener, and extractBearerToken reads INCOMING metadata because
// on the server side that is where a client's credential arrives. Using
// the outgoing form for a direct handler call tests nothing except that
// the handler refuses everything, which is exactly what several tests
// below caught while being written.
func directTokenCtx() context.Context {
	return metadata.NewIncomingContext(context.Background(), metadata.Pairs("authorization", "Bearer "+planHarnessToken))
}

// ---------------------------------------------------------------------------
// The detached restart mechanism itself.
// ---------------------------------------------------------------------------

// TestSetsidServiceRestarter_AimsAtANewSession pins every property of the
// child that ADR-0146 rule 4 depends on, in the one place they are
// written down.
//
// The three are checked as ARGUMENTS rather than by forking anything,
// because forking /bin/sh with a real sleep in a unit test would make the
// test slow and would prove nothing about the process group the real
// child ends up in. What matters here is that the script waits inside the
// child (a parent that slept would be killed during its own sleep, and
// the delay exists so the peer has its ACK before the stop half begins)
// and that the start is exec'd, so the child becomes `service` rather
// than a shell that has to survive long enough to run it.
func TestSetsidServiceRestarter_AimsAtANewSession(t *testing.T) {
	var gotName string
	var gotArgs []string
	r := &setsidServiceRestarter{
		shell:   "/bin/sh",
		service: "/usr/sbin/service",
		start: func(name string, args ...string) error {
			gotName, gotArgs = name, args
			return nil
		},
	}

	if err := r.ArrangeRestart(managerdServiceName, 2*time.Second); err != nil {
		t.Fatalf("ArrangeRestart(): %v", err)
	}
	if gotName != "/bin/sh" {
		t.Errorf("the child ran %q, want /bin/sh", gotName)
	}
	if len(gotArgs) != 2 || gotArgs[0] != "-c" {
		t.Fatalf("child args = %v, want exactly -c followed by one script", gotArgs)
	}
	script := gotArgs[len(gotArgs)-1]
	for _, want := range []string{"sleep 2", "exec /usr/sbin/service apiary_managerd restart"} {
		if !strings.Contains(script, want) {
			t.Errorf("child script = %q, want it to contain %q", script, want)
		}
	}
}

// TestClampHandoffDelay pins the bound on the only timing a caller may
// influence, at both ends, because each end is a real failure: too short
// and the peer may never receive the ACK; too long and a Comb looks
// stuck for a reason nobody can diagnose.
func TestClampHandoffDelay(t *testing.T) {
	cases := []struct {
		name string
		in   uint64
		want time.Duration
	}{
		{"zero selects the documented default", 0, HandoffRestartDelay},
		{"below the floor is raised, not refused", 1, MinHandoffRestartDelay},
		{"inside the range is kept", 750, 750 * time.Millisecond},
		{"above the ceiling is lowered, not refused", 600000, MaxHandoffRestartDelay},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := clampHandoffDelay(tc.in); got != tc.want {
				t.Errorf("clampHandoffDelay(%d) = %s, want %s", tc.in, got, tc.want)
			}
		})
	}
}

// TestRestartDelayRoundsUp pins that a sub-second request is rounded UP
// before it becomes a sleep(1) argument. Rounding down would shorten the
// window the ACK needs, which is the one thing the delay exists for, and
// the rounding happens in ArrangeRestart rather than in the caller so
// there is only one place it can happen.
func TestRestartDelayRoundsUp(t *testing.T) {
	var script string
	r := &setsidServiceRestarter{
		shell: "/bin/sh", service: "/usr/sbin/service",
		start: func(_ string, args ...string) error {
			script = args[len(args)-1]
			return nil
		},
	}
	if err := r.ArrangeRestart(managerdServiceName, 1100*time.Millisecond); err != nil {
		t.Fatalf("ArrangeRestart(): %v", err)
	}
	if !strings.Contains(script, "sleep 2") {
		t.Errorf("script = %q, want a 1100ms delay rounded UP to 2s, never down to 1s", script)
	}
}

// ---------------------------------------------------------------------------
// The durable marker.
// ---------------------------------------------------------------------------

// TestManagerdHandoffStore_RoundTripsAndIsPrivate pins the record's own
// properties, because a marker an unprivileged process could forge is a
// marker a replacement could be talked into confirming.
func TestManagerdHandoffStore_RoundTripsAndIsPrivate(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "guardrail")
	store := NewManagerdHandoffStore(dir)

	if _, found, err := store.Load(); err != nil || found {
		t.Fatalf("Load() on a fresh store = found %v, err %v; want false, nil - an ordinary managerd startup has no marker and must be silent", found, err)
	}

	want := ManagerdHandoffRecord{
		OperationID: "op-1", NodeID: "comb-2", Step: "issue-restart",
		Service: managerdServiceName, LeaseID: 41, ExpectedBuild: "9c43262d358a",
		ScheduledAtUnix: 1759000000,
	}
	if err := store.Save(want); err != nil {
		t.Fatalf("Save(): %v", err)
	}
	info, err := os.Stat(filepath.Join(dir, ManagerdHandoffFileName))
	if err != nil {
		t.Fatalf("Stat(marker): %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("marker mode = %o, want 600: it names a control-plane restart and its operation", perm)
	}

	got, found, err := store.Load()
	if err != nil || !found {
		t.Fatalf("Load() = found %v, err %v", found, err)
	}
	if got != want {
		t.Errorf("round-tripped record = %+v, want %+v", got, want)
	}

	// A second Save replaces rather than accumulating: a stale marker
	// surviving into a new scheduled restart would have the replacement
	// confirm the PREVIOUS operation's step.
	want.OperationID = "op-2"
	if err := store.Save(want); err != nil {
		t.Fatalf("second Save(): %v", err)
	}
	if got, _, _ := store.Load(); got.OperationID != "op-2" {
		t.Errorf("after a second Save the record names %q, want op-2", got.OperationID)
	}

	if err := store.Clear(); err != nil {
		t.Fatalf("Clear(): %v", err)
	}
	if _, found, _ := store.Load(); found {
		t.Error("the record survived Clear()")
	}
}

// TestManagerdHandoffStore_AnUnreadableRecordIsNotAMissingOne pins the
// distinction rule 6 is made of. A missing record is an ordinary startup
// and is silent. A record that EXISTS and cannot be parsed means a
// restart was scheduled here and nobody can find out what for, and it is
// reported rather than swallowed.
func TestManagerdHandoffStore_AnUnreadableRecordIsNotAMissingOne(t *testing.T) {
	dir := t.TempDir()
	store := NewManagerdHandoffStore(dir)
	if err := os.WriteFile(filepath.Join(dir, ManagerdHandoffFileName), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, found, err := store.Load()
	if !found {
		t.Error("found = false for a record that exists but is corrupt; the caller would treat that as an ordinary startup and stay silent")
	}
	if err == nil {
		t.Error("err = nil for a corrupt record; a restart was scheduled here and nothing would say so")
	}
}

// ---------------------------------------------------------------------------
// The build-identity gate, as a pure function.
// ---------------------------------------------------------------------------

// TestHandoffBuildVerdict covers every branch of the rule 5 gate.
//
// The two that matter most are the last two, and both are the ones a
// naive implementation gets wrong. An absent expected build is not a
// match: treating it as vacuously satisfied turns the gate into a rubber
// stamp on exactly the Combs whose binaries were installed by hand or
// built without -ldflags. And a running build that differs from the one
// the restart was scheduled against is a FAILURE, not an unknown: the
// Comb came back on a build nobody asked for, which is positive evidence
// and not the absence of any.
func TestHandoffBuildVerdict(t *testing.T) {
	report := func(onDisk, running string) buildgate.Report {
		return buildgate.Report{Services: []buildgate.Service{{
			Name: buildGateManagerd,
			// The real comparison, so this test exercises buildgate's
			// own rule rather than restating it.
			Status:  buildgate.Compare(onDisk, running),
			OnDisk:  onDisk,
			Running: running,
			Detail:  "synthetic",
		}}}
	}

	cases := []struct {
		name        string
		marker      ManagerdHandoffRecord
		report      buildgate.Report
		wantOutcome string
		wantSubstr  string
	}{
		{
			name:        "no answer at all for managerd",
			marker:      ManagerdHandoffRecord{OperationID: "op-1", ExpectedBuild: "abc"},
			report:      buildgate.Report{},
			wantOutcome: restartOutcomeUnobserved,
			wantSubstr:  "produced no answer for managerd",
		},
		{
			name:        "the restart was scheduled with no expected build",
			marker:      ManagerdHandoffRecord{OperationID: "op-1"},
			report:      report("abc", "abc"),
			wantOutcome: restartOutcomeUnobserved,
			wantSubstr:  "without a recorded expected build identity",
		},
		{
			name:        "the running process is older than the binary on disk",
			marker:      ManagerdHandoffRecord{OperationID: "op-1", ExpectedBuild: "abc"},
			report:      report("abc", "old"),
			wantOutcome: restartOutcomeFailed,
			wantSubstr:  "is not running the build sitting beside it",
		},
		{
			name:        "the Comb came back on a build nobody asked for",
			marker:      ManagerdHandoffRecord{OperationID: "op-1", ExpectedBuild: "some-other-build"},
			report:      report("abc", "abc"),
			wantOutcome: restartOutcomeFailed,
			wantSubstr:  "the Comb came back on a build nobody asked for",
		},
		{
			name:        "running and on disk agree with the marker",
			marker:      ManagerdHandoffRecord{OperationID: "op-1", ExpectedBuild: "abc"},
			report:      report("abc", "abc"),
			wantOutcome: restartOutcomeConfirmed,
			wantSubstr:  "is running build abc",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			outcome, detail, evidence := handoffBuildVerdict(tc.marker, tc.report)
			if outcome != tc.wantOutcome {
				t.Errorf("outcome = %q, want %q", outcome, tc.wantOutcome)
			}
			if !strings.Contains(detail, tc.wantSubstr) {
				t.Errorf("detail = %q, want it to contain %q", detail, tc.wantSubstr)
			}
			if len(evidence) == 0 {
				t.Error("no evidence cited; a confirmed or failed step record is refused by the FSM without it")
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Rule 3: a managerd may not ask itself to restart.
// ---------------------------------------------------------------------------

// TestRequestManagerdRestart_RefusesToAskThisNodeToRestartItself is
// ADR-0146 rule 3's whole surviving guarantee, and the test that matters
// is not that it refuses but that there is no way to override it.
//
// There is no force field on the message, so the assertion cannot be
// "force does not help" - it is that the request shape itself offers
// nothing to force with, and that the refusal names the two things an
// operator can actually do instead. A refusal that only says no leaves
// the reader with a dead control plane and no next step, which is the
// failure mode ADR-0142's own operator-facing refusal was written to
// avoid.
func TestRequestManagerdRestart_RefusesToAskThisNodeToRestartItself(t *testing.T) {
	srv := &Server{nodeID: "comb-1", restartGuardrailToken: planHarnessToken}
	srv.peers = &planHarnessForwarder{} // must never be reached

	resp, err := srv.RequestManagerdRestart(directTokenCtx(), &rpcpb.RequestManagerdRestartRequest{
		TargetNodeId: "comb-1",
		Fence: &rpcpb.ColonyUpdateFence{
			OperationId: "op-1", HolderNodeId: "comb-1", FenceToken: 7, HolderIncarnation: "boot-1",
		},
		Step: "issue-restart",
	})
	if err != nil {
		t.Fatalf("RequestManagerdRestart() returned a transport error rather than a refusal: %v", err)
	}
	if resp.GetAccepted() {
		t.Fatal("a managerd asked itself to restart and accepted; the request never reached a peer and nothing was reserved")
	}
	for _, want := range []string{
		"cannot restart itself",           // the reason
		"HandoverColonyUpdate",            // the managed path out
		"service apiary_managerd restart", // the operator's own path out
	} {
		if !strings.Contains(resp.GetError(), want) {
			t.Errorf("refusal = %q, want it to contain %q", resp.GetError(), want)
		}
	}
}

// TestRequestManagerdRestart_RefusesWithoutTheGuardrailToken pins that
// the new RPC is behind the dedicated root-owned token and not merely
// absent from the role map. A gRPC PermissionDenied, not a response
// field: "you may not ask this" is a different fact from "I tried and
// could not do it".
func TestRequestManagerdRestart_RefusesWithoutTheGuardrailToken(t *testing.T) {
	srv := &Server{nodeID: "comb-1", restartGuardrailToken: planHarnessToken}
	_, err := srv.RequestManagerdRestart(context.Background(), &rpcpb.RequestManagerdRestartRequest{
		TargetNodeId: "comb-2",
		Fence:        &rpcpb.ColonyUpdateFence{OperationId: "op-1", HolderNodeId: "comb-1", FenceToken: 7, HolderIncarnation: "boot-1"},
		Step:         "issue-restart",
	})
	if err == nil {
		t.Fatal("an unauthenticated request was not refused; any credential could stop the Colony's control plane")
	}
	if !strings.Contains(err.Error(), "restart-guardrail token") {
		t.Errorf("err = %v, want it to name the guardrail token", err)
	}
}

// TestRequestManagerdRestart_RefusesAnIncompleteRequest walks the
// refusals that cost nothing to check locally, and asserts each one
// actually refuses rather than only that its message reads well. An
// earlier shape of this kind of table checked the message on the happy
// path, so deleting a check made every case pass by taking the success
// branch.
func TestRequestManagerdRestart_RefusesAnIncompleteRequest(t *testing.T) {
	good := &rpcpb.ColonyUpdateFence{OperationId: "op-1", HolderNodeId: "comb-1", FenceToken: 7, HolderIncarnation: "boot-1"}
	cases := []struct {
		name       string
		req        *rpcpb.RequestManagerdRestartRequest
		wantSubstr string
	}{
		{
			name:       "no target",
			req:        &rpcpb.RequestManagerdRestartRequest{Fence: good, Step: "issue-restart"},
			wantSubstr: "no target Comb was named",
		},
		{
			name:       "no fence",
			req:        &rpcpb.RequestManagerdRestartRequest{TargetNodeId: "comb-2", Step: "issue-restart"},
			wantSubstr: "no fence was presented",
		},
		{
			name:       "an incomplete fence",
			req:        &rpcpb.RequestManagerdRestartRequest{TargetNodeId: "comb-2", Fence: &rpcpb.ColonyUpdateFence{OperationId: "op-1"}, Step: "issue-restart"},
			wantSubstr: "the fence is incomplete",
		},
		{
			name:       "no step",
			req:        &rpcpb.RequestManagerdRestartRequest{TargetNodeId: "comb-2", Fence: good},
			wantSubstr: "no step was named",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := &Server{nodeID: "comb-1", restartGuardrailToken: planHarnessToken}
			srv.peers = &planHarnessForwarder{}
			resp, err := srv.RequestManagerdRestart(directTokenCtx(), tc.req)
			if err != nil {
				t.Fatalf("transport error instead of a refusal: %v", err)
			}
			if resp.GetAccepted() {
				t.Fatalf("accepted a request that names %s", tc.name)
			}
			if !strings.Contains(resp.GetError(), tc.wantSubstr) {
				t.Errorf("refusal = %q, want it to contain %q", resp.GetError(), tc.wantSubstr)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Rule 4: the target's own handler.
// ---------------------------------------------------------------------------

// bareHandoffServer is a Server with only what IssueManagerdRestart
// touches, so a refusal is provably the handler's own decision rather
// than a side effect of some unrelated dependency being nil.
func bareHandoffServer(t *testing.T, dir string, restarter DetachedRestarter) *Server {
	t.Helper()
	srv := &Server{nodeID: "comb-2", restartGuardrailToken: planHarnessToken}
	srv.SetColonyUpdateHandoffStore(NewManagerdHandoffStore(dir), restarter)
	return srv
}

func issueRequest() *rpcpb.IssueManagerdRestartRequest {
	return &rpcpb.IssueManagerdRestartRequest{
		TargetNodeId: "comb-2",
		OperationId:  "op-1",
		Fence: &rpcpb.ColonyUpdateFence{
			OperationId: "op-1", HolderNodeId: "comb-1", FenceToken: 7, HolderIncarnation: "boot-1",
		},
		Step:    "issue-restart",
		LeaseId: 41,
	}
}

// TestIssueManagerdRestart_RefusesRatherThanRestartingInProcess is the
// single most important refusal in this file.
//
// A managerd with no detached restarter could "helpfully" fall back to
// running `service apiary_managerd restart` from a goroutine inside
// itself. That is ADR-0142's recorded fault: the stop half kills the
// process the goroutine lives in and the daemon does not come back. The
// test asserts the refusal AND that nothing was written, because a marker
// left behind by a refused request would have the replacement confirm a
// restart that was never going to happen.
func TestIssueManagerdRestart_RefusesRatherThanRestartingInProcess(t *testing.T) {
	dir := t.TempDir()
	srv := bareHandoffServer(t, dir, nil)

	resp, err := srv.IssueManagerdRestart(directTokenCtx(), issueRequest())
	if err != nil {
		t.Fatalf("transport error instead of a refusal: %v", err)
	}
	if resp.GetAccepted() {
		t.Fatal("accepted with no detached restart mechanism; the only arrangement left would be the one that does not come back")
	}
	if !strings.Contains(resp.GetError(), "ADR-0142") {
		t.Errorf("refusal = %q, want it to name the fault it is avoiding", resp.GetError())
	}
	if _, found, _ := NewManagerdHandoffStore(dir).Load(); found {
		t.Error("a marker was written for a restart that was refused; a replacement would confirm a step that never began")
	}
}

// TestIssueManagerdRestart_RefusesAMisroutedRequest pins the same guard
// StepAsideForRestart has: the handler always acts on the node that
// received it, so a request naming another Comb is refused rather than
// honoured and a misplaced call stops the wrong machine loudly.
func TestIssueManagerdRestart_RefusesAMisroutedRequest(t *testing.T) {
	dir := t.TempDir()
	srv := bareHandoffServer(t, dir, &recordingRestarter{})

	req := issueRequest()
	req.TargetNodeId = "comb-3"
	resp, err := srv.IssueManagerdRestart(directTokenCtx(), req)
	if err != nil {
		t.Fatalf("transport error instead of a refusal: %v", err)
	}
	if resp.GetAccepted() {
		t.Fatal("a request addressed to comb-3 was acted on by comb-2")
	}
	for _, want := range []string{"comb-3", "only ever arranged on the node that received it"} {
		if !strings.Contains(resp.GetError(), want) {
			t.Errorf("refusal = %q, want it to contain %q", resp.GetError(), want)
		}
	}
}

// TestIssueManagerdRestart_RefusesLeaseZero pins the one piece of state
// rule 5 could not otherwise act on. Lease 0 is ADR-0103's emergency
// path and stays manual; a marker naming it would have the replacement
// "confirm" a lease that is not a lease.
func TestIssueManagerdRestart_RefusesLeaseZero(t *testing.T) {
	dir := t.TempDir()
	srv := bareHandoffServer(t, dir, &recordingRestarter{})

	req := issueRequest()
	req.LeaseId = 0
	resp, _ := srv.IssueManagerdRestart(directTokenCtx(), req)
	if resp.GetAccepted() {
		t.Fatal("accepted a restart with no lease")
	}
	if !strings.Contains(resp.GetError(), "lease 0 is the emergency path and stays manual") {
		t.Errorf("refusal = %q, want it to say why lease 0 is not acceptable here", resp.GetError())
	}
}

// TestIssueManagerdRestart_RefusesAFenceForAnotherOperation pins the
// pairing of the two identifiers. They look redundant - the fence
// carries an operation id too - and they are not: a caller that named
// one operation and held the fence for another would produce a marker
// whose replacement recorded a step against the wrong operation.
func TestIssueManagerdRestart_RefusesAFenceForAnotherOperation(t *testing.T) {
	dir := t.TempDir()
	srv := bareHandoffServer(t, dir, &recordingRestarter{})

	req := issueRequest()
	req.OperationId = "op-2"
	resp, _ := srv.IssueManagerdRestart(directTokenCtx(), req)
	if resp.GetAccepted() {
		t.Fatal("accepted a request whose operation and fence name different operations")
	}
	if !strings.Contains(resp.GetError(), "op-2") || !strings.Contains(resp.GetError(), "op-1") {
		t.Errorf("refusal = %q, want it to name both operations so the mismatch is visible", resp.GetError())
	}
}

// TestIssueManagerdRestart_WritesTheMarkerBeforeArrangingTheRestart is
// the ordering rule 4 is explicit about, tested through the one failure
// that exposes it: a restart that could not be arranged.
//
// The marker is deliberately LEFT IN PLACE on that path. The alternative
// - tidying up - would leave a Comb whose replacement finds nothing at
// all, which is indistinguishable from a Comb that was never scheduled,
// and rule 6 exists to make that state impossible to mistake for success.
func TestIssueManagerdRestart_WritesTheMarkerBeforeArrangingTheRestart(t *testing.T) {
	dir := t.TempDir()
	restarter := &recordingRestarter{err: fmt.Errorf("fork: resource temporarily unavailable")}
	srv := bareHandoffServer(t, dir, restarter)
	// The build gate cannot read a Comb with no libexec directory, so
	// the expected build comes back empty - which the confirmation path
	// treats as unobserved rather than as a match.
	srv.SetColonyUpdateHandoffBuildPaths(buildgate.Paths{LibexecDir: t.TempDir(), LogDir: t.TempDir()})

	resp, err := srv.IssueManagerdRestart(directTokenCtx(), issueRequest())
	if err != nil {
		t.Fatalf("transport error instead of a refusal: %v", err)
	}
	if resp.GetAccepted() {
		t.Fatal("accepted a restart that was never arranged")
	}
	if !strings.Contains(resp.GetError(), "is NOT restarting") {
		t.Errorf("refusal = %q, want it to state plainly that this managerd is not restarting", resp.GetError())
	}

	marker, found, loadErr := NewManagerdHandoffStore(dir).Load()
	if loadErr != nil || !found {
		t.Fatalf("the marker is gone (found %v, err %v); the whole point is that a later startup can see a restart was scheduled and never happened", found, loadErr)
	}
	if marker.OperationID != "op-1" || marker.LeaseID != 41 || marker.Service != managerdServiceName {
		t.Errorf("marker = %+v, want it to name op-1, lease 41 and %s", marker, managerdServiceName)
	}
}

// TestIssueManagerdRestart_HandsOverBeforeRestartingItself is ADR-0146
// rules 2 and 4's first step, and the case rule 1 is supposed to have
// made impossible.
//
// The check on the coordinator's side lives on the other side of a
// network hop, and a check that exists on only one side of a hop is a
// check a caller reaching the other side directly can aim around. The
// target therefore re-derives it, and the test here is that a target
// which IS the holder refuses without a named successor rather than
// inventing one.
func TestIssueManagerdRestart_HandsOverBeforeRestartingItself(t *testing.T) {
	dir := t.TempDir()
	restarter := &recordingRestarter{}
	srv := bareHandoffServer(t, dir, restarter)
	srv.SetColonyUpdateHandoffBuildPaths(buildgate.Paths{LibexecDir: t.TempDir(), LogDir: t.TempDir()})

	// The fence now says this node holds the operation.
	req := issueRequest()
	req.Fence = &rpcpb.ColonyUpdateFence{
		OperationId: "op-1", HolderNodeId: "comb-2", FenceToken: 7, HolderIncarnation: "boot-1",
	}

	resp, _ := srv.IssueManagerdRestart(directTokenCtx(), req)
	if resp.GetAccepted() {
		t.Fatal("a holder restarted itself with no successor named; the operation would have died with the process")
	}
	if !strings.Contains(resp.GetError(), "no successor was named") {
		t.Errorf("refusal = %q, want it to say the operation has to move first", resp.GetError())
	}
	if restarter.calls() != 0 {
		t.Error("a restart was arranged for a node that refused")
	}

	// And the same request naming this Comb's own current incarnation is
	// a no-op handover, which the FSM would refuse anyway; refusing it
	// here keeps a change of coordinator that did not happen out of the
	// record entirely.
	req.HandoverToNodeId = "comb-2"
	req.HandoverToIncarnation = "boot-1"
	resp, _ = srv.IssueManagerdRestart(directTokenCtx(), req)
	if resp.GetAccepted() {
		t.Fatal("accepted a handover to the process that already holds the operation")
	}
	if !strings.Contains(resp.GetError(), "not a change of coordinator at all") {
		t.Errorf("refusal = %q, want it to name the no-op", resp.GetError())
	}
}

// ---------------------------------------------------------------------------
// Rule 6: absent evidence is unobserved, and it stops.
// ---------------------------------------------------------------------------

// TestConfirmManagerdRestartHandoff_AnOrdinaryStartupIsSilent pins the
// common case, and the common case is the one that must be free: a
// managerd that was not scheduled for a controlled restart finds no
// marker and says nothing at all.
func TestConfirmManagerdRestartHandoff_AnOrdinaryStartupIsSilent(t *testing.T) {
	srv := bareHandoffServer(t, t.TempDir(), &recordingRestarter{})
	confirmation, err := srv.ConfirmManagerdRestartHandoff(context.Background())
	if err != nil {
		t.Fatalf("an ordinary startup reported an error: %v", err)
	}
	if confirmation.Found {
		t.Errorf("confirmation = %+v, want Found false; there is no marker to have found", confirmation)
	}
}

// TestConfirmManagerdRestartHandoff_AnUnreadableRecordIsAnError is rule
// 6 itself.
//
// A marker that exists and cannot be parsed is the failure this whole
// rule exists for: a controlled-update restart WAS scheduled for this
// Comb and nobody can work out what it was for. The test asserts all
// three halves - it is an error, it is not a confirmation, and the
// record is left alone for whoever has to look at it.
func TestConfirmManagerdRestartHandoff_AnUnreadableRecordIsAnError(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ManagerdHandoffFileName), []byte("not json at all"), 0o600); err != nil {
		t.Fatal(err)
	}
	srv := bareHandoffServer(t, dir, &recordingRestarter{})

	confirmation, err := srv.ConfirmManagerdRestartHandoff(context.Background())
	if err == nil {
		t.Fatal("a corrupt record was reported as a clean startup; the scheduled restart vanished without a word")
	}
	if confirmation.Found {
		t.Errorf("confirmation = %+v, want Found false: nothing was established, and Found true with an empty outcome would render as a step that was somehow reached", confirmation)
	}
	if _, found, _ := NewManagerdHandoffStore(dir).Load(); !found {
		t.Error("the corrupt record was removed; an operator has to be able to read it")
	}
}

// TestConfirmManagerdRestartHandoff_AMarkerForAnotherCombIsNotOurs
// pins the last fail-closed case: a marker naming a different Comb. The
// lease, the operation and the build all belong to somebody else, so the
// only honest answer is unobserved with the record left in place.
func TestConfirmManagerdRestartHandoff_AMarkerForAnotherCombIsNotOurs(t *testing.T) {
	dir := t.TempDir()
	if err := NewManagerdHandoffStore(dir).Save(ManagerdHandoffRecord{
		OperationID: "op-1", NodeID: "comb-9", Step: "issue-restart",
		Service: managerdServiceName, LeaseID: 41, ExpectedBuild: "abc",
	}); err != nil {
		t.Fatal(err)
	}
	srv := bareHandoffServer(t, dir, &recordingRestarter{})

	confirmation, err := srv.ConfirmManagerdRestartHandoff(context.Background())
	if err != nil {
		t.Fatalf("ConfirmManagerdRestartHandoff(): %v", err)
	}
	if confirmation.Outcome != restartOutcomeUnobserved {
		t.Errorf("outcome = %q, want %q", confirmation.Outcome, restartOutcomeUnobserved)
	}
	if !strings.Contains(confirmation.Detail, "comb-9") {
		t.Errorf("detail = %q, want it to name the Comb the record claims", confirmation.Detail)
	}
}

// ---------------------------------------------------------------------------
// The whole handoff, against a real cluster.
// ---------------------------------------------------------------------------

// recordingRestarter stands in for the detached child. It records what
// it was asked to do and can be made to fail, because a mechanism that
// cannot be made to fail cannot have its failure ordering tested.
type recordingRestarter struct {
	mu       sync.Mutex
	err      error
	arranged []time.Time
	services []string
	delays   []time.Duration
}

func (r *recordingRestarter) ArrangeRestart(service string, delay time.Duration) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.err != nil {
		return r.err
	}
	r.arranged = append(r.arranged, time.Now())
	r.services = append(r.services, service)
	r.delays = append(r.delays, delay)
	return nil
}

func (r *recordingRestarter) calls() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.arranged)
}

// handoffForwarder replaces the peer dial and nothing else, for the
// reason restartcaller_test.go's own forwarder documents: three loopback
// raft addresses all reduce to one managerd port, so a real dial cannot
// reach three managerds on one machine.
//
// It embeds the nil PeerForwarder, so any method a handoff does not
// actually call panics rather than silently returning a zero value.
type handoffForwarder struct {
	PeerForwarder
	h *planHarness
}

func (f *handoffForwarder) ctx(ctx context.Context) context.Context {
	// The same outgoing-token helper the plan harness uses, so the two
	// harnesses cannot drift on which token is presented - and a test
	// that reaches a handler through a forward is therefore also a test
	// that the token is actually on the wire.
	return tokenCtx()
}

func (f *handoffForwarder) leaderClient() (rpcpb.ManagerServiceClient, error) {
	for i, node := range f.h.nodes {
		if node.Status().IsLeader {
			return f.h.clients[i], nil
		}
	}
	return nil, fmt.Errorf("no leader among the harness nodes")
}

func (f *handoffForwarder) clientForNode(nodeID string) (rpcpb.ManagerServiceClient, error) {
	for i, id := range f.h.ids {
		if id == nodeID {
			return f.h.clients[i], nil
		}
	}
	return nil, fmt.Errorf("the harness has no managerd for node %q", nodeID)
}

func (f *handoffForwarder) MutateColonyUpdate(ctx context.Context, _ string, req *rpcpb.MutateColonyUpdateRequest) (*rpcpb.MutateColonyUpdateResponse, error) {
	c, err := f.leaderClient()
	if err != nil {
		return nil, err
	}
	return c.MutateColonyUpdate(f.ctx(ctx), req)
}

func (f *handoffForwarder) ReserveRestartLease(ctx context.Context, _ string, req *rpcpb.ReserveRestartLeaseRequest) (*rpcpb.ReserveRestartLeaseResponse, error) {
	c, err := f.leaderClient()
	if err != nil {
		return nil, err
	}
	return c.ReserveRestartLease(f.ctx(ctx), req)
}

func (f *handoffForwarder) ConfirmRestartCompleted(ctx context.Context, _ string, req *rpcpb.ConfirmRestartCompletedRequest) (*rpcpb.ConfirmRestartCompletedResponse, error) {
	c, err := f.leaderClient()
	if err != nil {
		return nil, err
	}
	return c.ConfirmRestartCompleted(f.ctx(ctx), req)
}

func (f *handoffForwarder) IssueManagerdRestart(ctx context.Context, _ string, req *rpcpb.IssueManagerdRestartRequest) (*rpcpb.IssueManagerdRestartResponse, error) {
	c, err := f.clientForNode(req.GetTargetNodeId())
	if err != nil {
		return nil, err
	}
	return c.IssueManagerdRestart(f.ctx(ctx), req)
}

// handoffCluster is the planHarness with the handoff mechanism wired on
// every node: a real durable store in its own directory, a recording
// stand-in for the detached child, and a REAL buildgate over real
// directories holding real executable stand-in daemons.
type handoffCluster struct {
	*planHarness
	dirs       []string
	restarters []*recordingRestarter
}

func newHandoffCluster(t *testing.T, n int) *handoffCluster {
	t.Helper()
	h := newPlanHarness(t, n)
	c := &handoffCluster{planHarness: h}

	// One libexec/log pair per node, because the gate reads local paths
	// only and a shared directory would have every Comb answering about
	// the same daemons.
	for i := range h.servers {
		dir := t.TempDir()
		c.dirs = append(c.dirs, dir)
		restarter := &recordingRestarter{}
		c.restarters = append(c.restarters, restarter)
		h.servers[i].SetColonyUpdateHandoffStore(NewManagerdHandoffStore(dir), restarter)
		writeBuildEvidence(t, buildgate.Paths{LibexecDir: filepath.Join(dir, "libexec"), LogDir: filepath.Join(dir, "log")}, "comb-build-1")
		h.servers[i].SetColonyUpdateHandoffBuildPaths(buildgate.Paths{LibexecDir: filepath.Join(dir, "libexec"), LogDir: filepath.Join(dir, "log")})
	}
	for _, srv := range h.servers {
		srv.peers = &handoffForwarder{h: h}
	}
	return c
}

// acquireOperation takes the colony-wide single-flight on behalf of
// nodeID's own managerd, and returns the fence the FSM actually granted.
func acquireOperation(t *testing.T, c *handoffCluster, nodeID, opID string) *rpcpb.ColonyUpdateFence {
	t.Helper()
	var srv *Server
	for i, id := range c.ids {
		if id == nodeID {
			srv = c.servers[i]
		}
	}
	if srv == nil {
		t.Fatalf("the harness has no managerd for %q", nodeID)
	}
	resp, err := srv.mutateColonyUpdate(directTokenCtx(), &rpcpb.MutateColonyUpdateRequest{
		Op: &rpcpb.MutateColonyUpdateRequest_Acquire{
			Acquire: &rpcpb.AcquireColonyUpdate{OperationId: opID},
		},
	})
	if err != nil || resp.GetError() != "" {
		t.Fatalf("acquiring %q for %s: resp %v err %v", opID, nodeID, resp.GetError(), err)
	}
	return &rpcpb.ColonyUpdateFence{
		OperationId:       resp.GetUpdate().GetOperationId(),
		HolderNodeId:      resp.GetUpdate().GetHolderNodeId(),
		FenceToken:        resp.GetUpdate().GetFenceToken(),
		HolderIncarnation: resp.GetUpdate().GetHolderIncarnation(),
	}
}

// managerdLeaseOf reads this node's own replicated copy of the restart
// lease for apiary_managerd.
//
// The harness's own eventuallyLeaseHeld and assertLeaseHeldOrGone are
// written against apiary_raftd, because ADR-0125's plan is the only
// thing that reserved one. This handoff reserves the lease for
// apiary_managerd, and a helper that quietly looked at the other service
// would have made every assertion below a test of nothing: the
// raftd lease is never held in any of these tests, so a wait on it can
// only time out and a held-check on it can only ever be vacuous.
func managerdLeaseOf(t *testing.T, srv *Server) *internalpb.RestartLease {
	t.Helper()
	resp, err := srv.raft.GetRestartLeaseStateLocal(context.Background(), managerdServiceName)
	if err != nil {
		t.Fatalf("GetRestartLeaseStateLocal(%s): %v", managerdServiceName, err)
	}
	return resp.GetLease()
}

func eventuallyManagerdLeaseHeld(t *testing.T, srv *Server, wantLeaseID uint64) {
	t.Helper()
	eventually(t, 10*time.Second, func() bool {
		lease := managerdLeaseOf(t, srv)
		if lease == nil {
			return false
		}
		if wantLeaseID != 0 && lease.GetLeaseId() != wantLeaseID {
			t.Fatalf("the lease this node shows is %d, want %d", lease.GetLeaseId(), wantLeaseID)
		}
		return true
	})
}

// coordinatorAndTarget pick the harness's real leader as the coordinator
// and any other Comb as the target.
//
// The leader is not an arbitrary convenience. An ACQUIRED operation is
// held by whichever managerd authored the grant, and internal/manager
// authors it from s.nodeID on the node that actually applied the command
// - which, after the leader forward every acquire performs, is the raft
// leader. So the holder of a freshly acquired operation IS the leader,
// and rule 1 says the target must not be the holder. Choosing the target
// any other way makes the test depend on which node won the last
// election.
func coordinatorAndTarget(t *testing.T, c *handoffCluster) (coordinator int, target int) {
	t.Helper()
	leader := c.leaderIndex(t)
	for i := range c.ids {
		if i != leader {
			return leader, i
		}
	}
	t.Fatal("the harness has one node; there is no second Comb to target")
	return -1, -1
}

func indexOfNode(c *handoffCluster, nodeID string) int {
	for i, id := range c.ids {
		if id == nodeID {
			return i
		}
	}
	return -1
}

// writeBuildEvidence writes a real libexec directory and a real log
// directory that the build gate can genuinely read: each daemon is an
// executable shell script that prints a build line under -version, and
// each log holds a startup line carrying the same build id.
//
// Both readings are real files and a real subprocess, which is the only
// way "running versus on disk" is tested rather than assumed. A report
// assembled by hand in a test would only prove that the test's own struct
// literal reached the comparison.
func writeBuildEvidence(t *testing.T, paths buildgate.Paths, buildID string) {
	t.Helper()
	if err := os.MkdirAll(paths.LibexecDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(paths.LogDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, svc := range buildgate.Services {
		bin := filepath.Join(paths.LibexecDir, svc)
		body := fmt.Sprintf("#!/bin/sh\necho '%s build=%s commit=%s built=2026-09-26T11:52:03-04:00 go=freebsd/amd64'\n", svc, buildID, buildID)
		if err := os.WriteFile(bin, []byte(body), 0o700); err != nil {
			t.Fatal(err)
		}
		line := fmt.Sprintf("2026/09/26 02:03:23 %s: build=%s commit=%s built=2026-09-26T11:52:03-04:00 go=freebsd/amd64 listening on 127.0.0.1:0 (node-id=comb)\n", svc, buildID, buildID)
		if err := os.WriteFile(filepath.Join(paths.LogDir, svc+".log"), []byte(line), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// TestManagerdRestartHandoff_EndToEnd is the whole of ADR-0146 rules 1
// through 5 on a real cluster: a coordinator on one Comb asks a peer to
// restart its own managerd, the target leaves a durable marker and
// arranges a restart in a process that is not itself, and this process's
// own successor - a second Server standing where a replacement process
// would be, over the same raftd and the same directory - confirms the
// step from its own evidence and releases the lease.
func TestManagerdRestartHandoff_EndToEnd(t *testing.T) {
	c := newHandoffCluster(t, 3)
	coordIdx, target := coordinatorAndTarget(t, c)
	fence := acquireOperation(t, c, c.ids[coordIdx], "op-handoff-1")

	coordinator := c.servers[coordIdx]
	resp, err := coordinator.RequestManagerdRestart(directTokenCtx(), &rpcpb.RequestManagerdRestartRequest{
		TargetNodeId: c.ids[target],
		Fence:        fence,
		Step:         "issue-restart",
	})
	if err != nil {
		t.Fatalf("RequestManagerdRestart(): %v", err)
	}
	if !resp.GetAccepted() {
		t.Fatalf("the handoff was refused: %s", resp.GetError())
	}
	if resp.GetLeaseId() == 0 {
		t.Error("no restart lease was reserved; the replacement would have nothing to confirm")
	}

	// The target arranged exactly one restart, for the right service,
	// after a delay long enough for the ACK to have landed.
	if got := c.restarters[target].calls(); got != 1 {
		t.Fatalf("the target arranged %d restarts, want exactly 1", got)
	}
	if got := c.restarters[target].services[0]; got != managerdServiceName {
		t.Errorf("the target arranged a restart of %q, want %q", got, managerdServiceName)
	}
	if got := c.restarters[target].delays[0]; got < MinHandoffRestartDelay {
		t.Errorf("the restart was scheduled after %s, want at least the %s the ACK needs", got, MinHandoffRestartDelay)
	}

	// The marker is on disk, on the TARGET's machine, naming the
	// operation, the step, the lease and the build the replacement must
	// find itself running.
	marker, found, err := NewManagerdHandoffStore(c.dirs[target]).Load()
	if err != nil || !found {
		t.Fatalf("the target left no durable marker (found %v, err %v)", found, err)
	}
	if marker.NodeID != c.ids[target] || marker.OperationID != "op-handoff-1" ||
		marker.Step != "issue-restart" || marker.LeaseID != resp.GetLeaseId() {
		t.Errorf("marker = %+v, want it to name %s, op-handoff-1, issue-restart and lease %d", marker, c.ids[target], resp.GetLeaseId())
	}
	if marker.ExpectedBuild != "comb-build-1" {
		t.Errorf("marker expected build = %q, want the id the on-disk binary reports", marker.ExpectedBuild)
	}

	// The lease is HELD right now, and that is not a leftover to be
	// tidied: it is what the restarted process is about to confirm.
	eventuallyManagerdLeaseHeld(t, c.servers[target], marker.LeaseID)

	// The replacement process. A second Server over the same raftd and
	// the same guardrail directory is exactly what one looks like from
	// this code's point of view: same node id, same state, new process.
	replacement := NewServer(c.servers[target].raft, c.ids[target], isostore.New(t.TempDir()), nil, nil, nil, nil, "", nil, nil, nil, 0, nil)
	replacement.peers = &handoffForwarder{h: c.planHarness}
	replacement.SetColonyUpdateHandoffStore(NewManagerdHandoffStore(c.dirs[target]), &recordingRestarter{})
	replacement.SetColonyUpdateHandoffBuildPaths(buildgate.Paths{
		LibexecDir: filepath.Join(c.dirs[target], "libexec"),
		LogDir:     filepath.Join(c.dirs[target], "log"),
	})

	confirmation, err := replacement.ConfirmManagerdRestartHandoff(context.Background())
	if err != nil {
		t.Fatalf("the replacement could not confirm its own restart: %v (%s)", err, confirmation.Detail)
	}
	if confirmation.Outcome != restartOutcomeConfirmed {
		t.Fatalf("outcome = %q, want %q; detail: %s", confirmation.Outcome, restartOutcomeConfirmed, confirmation.Detail)
	}
	if !confirmation.LeaseReleased {
		t.Error("the lease was not released even though the step was confirmed; it would block every later guarded restart of apiary_managerd")
	}

	// The step is in the operation's own durable history, on comb-2's
	// replicated copy, citing the two readings that made it positive.
	eventually(t, 10*time.Second, func() bool {
		state, err := c.servers[target].raft.GetColonyUpdateStateLocal(context.Background(), "op-handoff-1")
		if err != nil || state.GetActive() == nil {
			return false
		}
		steps := state.GetActive().GetSteps()
		return len(steps) > 0 && steps[0].GetOutcome() == restartOutcomeConfirmed
	})
	state, err := c.servers[target].raft.GetColonyUpdateStateLocal(context.Background(), "op-handoff-1")
	step := state.GetActive().GetSteps()[0]
	if step.GetNodeId() != c.ids[target] || step.GetStep() != "issue-restart" {
		t.Errorf("the recorded step = %+v, want it to be issue-restart on %s", step, c.ids[target])
	}
	if len(step.GetEvidence()) == 0 {
		t.Error("the step record cites no evidence; the FSM would have refused it if it had")
	}
	if !strings.Contains(strings.Join(step.GetEvidence(), "\n"), "comb-build-1") {
		t.Errorf("the step record's evidence = %v, want it to name the build id it confirmed", step.GetEvidence())
	}

	// The marker is gone, so a later startup of this same process does
	// not report the step a second time.
	if _, found, _ := NewManagerdHandoffStore(c.dirs[target]).Load(); found {
		t.Error("the marker survived a confirmed step; the next startup would re-report it")
	}

	// And the lease really is released on the cluster, not merely in the
	// response the replacement read.
	eventually(t, 10*time.Second, func() bool {
		return managerdLeaseOf(t, c.servers[target]) == nil
	})
}

// TestManagerdRestartHandoff_TheLeaseStaysHeldWhenTheForwardFails is the
// one property that makes a failed handoff safe rather than merely
// reported.
//
// The coordinator cannot know whether the target acted on the request
// before the transport gave up. Releasing the lease on that evidence
// would recreate the exact state ADR-0103's lease exists to make
// impossible - a second Comb free to restart apiary_managerd while the
// first may be doing it.
func TestManagerdRestartHandoff_TheLeaseStaysHeldWhenTheForwardFails(t *testing.T) {
	c := newHandoffCluster(t, 3)
	coordIdx, target := coordinatorAndTarget(t, c)
	fence := acquireOperation(t, c, c.ids[coordIdx], "op-handoff-2")

	// A forwarder that answers every call except the one that carries
	// the handoff, which is the shape of a Comb that went away between
	// the lease and the request.
	coordinator := c.servers[coordIdx]
	coordinator.peers = &failingIssueForwarder{handoffForwarder: &handoffForwarder{h: c.planHarness}}

	resp, err := coordinator.RequestManagerdRestart(directTokenCtx(), &rpcpb.RequestManagerdRestartRequest{
		TargetNodeId: c.ids[target], Fence: fence, Step: "issue-restart",
	})
	if err != nil {
		t.Fatalf("transport error instead of a refusal: %v", err)
	}
	if resp.GetAccepted() {
		t.Fatal("accepted a handoff whose forward failed")
	}
	if resp.GetLeaseId() == 0 {
		t.Fatal("no lease id in the refusal, so nobody can find out what is blocking")
	}
	if !strings.Contains(resp.GetError(), "stays held and blocking") {
		t.Errorf("refusal = %q, want it to say the lease is still held and why", resp.GetError())
	}

	// The lease is genuinely still there, on the cluster, for the
	// service it was reserved against.
	eventuallyManagerdLeaseHeld(t, coordinator, resp.GetLeaseId())
}

// failingIssueForwarder fails exactly the handoff's own forward and
// answers everything else, so the test exercises the lease-handling path
// rather than a dead coordinator.
type failingIssueForwarder struct {
	*handoffForwarder
}

func (f *failingIssueForwarder) IssueManagerdRestart(context.Context, string, *rpcpb.IssueManagerdRestartRequest) (*rpcpb.IssueManagerdRestartResponse, error) {
	return nil, fmt.Errorf("connection refused")
}

// TestManagerdRestartHandoff_HolderHandsOverBeforeItself is rules 2 and
// 4's first step, run against the real FSM so the handover is a real
// commit rather than a request that merely looked well-formed.
//
// The coordinator-side rule-1 check refuses this before it ever reaches
// the wire, so the target's handler is called directly - which is exactly
// the situation that check exists to prevent and which the target must
// therefore defend against on its own.
func TestManagerdRestartHandoff_HolderHandsOverBeforeItself(t *testing.T) {
	c := newHandoffCluster(t, 3)
	// The operation is acquired, so its holder is the raft leader, and
	// that leader is deliberately the Comb about to be restarted - the
	// exact case rule 1 says the coordinator must never create and rule
	// 2 says has to be handed over before it is entered.
	holder := c.leaderIndex(t)
	fence := acquireOperation(t, c, c.ids[holder], "op-handoff-3")
	if fence.GetHolderNodeId() != c.ids[holder] {
		t.Fatalf("the fence names holder %q, want %q", fence.GetHolderNodeId(), c.ids[holder])
	}
	successor := c.ids[(holder+1)%len(c.ids)]

	resp, err := c.servers[holder].IssueManagerdRestart(directTokenCtx(), &rpcpb.IssueManagerdRestartRequest{
		TargetNodeId:          c.ids[holder],
		OperationId:           "op-handoff-3",
		Fence:                 fence,
		Step:                  "issue-restart",
		LeaseId:               77,
		HandoverToNodeId:      successor,
		HandoverToIncarnation: successor + "-boot-1",
	})
	if err != nil {
		t.Fatalf("transport error instead of a refusal: %v", err)
	}
	if !resp.GetAccepted() {
		t.Fatalf("the holder refused to hand over and restart itself: %s", resp.GetError())
	}
	if !resp.GetHandoverPerformed() {
		t.Error("handover_performed = false, but this Comb held the operation and the operation's fence token must have moved")
	}

	// The durable record now names the successor, at a strictly higher
	// token, and names the outgoing holder in its handovers list with
	// the reason. That is the whole of what a handover is: the
	// operation relocated, the progress intact, the old token stale.
	eventually(t, 10*time.Second, func() bool {
		state, err := c.servers[holder].raft.GetColonyUpdateStateLocal(context.Background(), "op-handoff-3")
		return err == nil && state.GetActive() != nil && state.GetActive().GetHolderNodeId() == successor
	})
	state, _ := c.servers[holder].raft.GetColonyUpdateStateLocal(context.Background(), "op-handoff-3")
	active := state.GetActive()
	if active.GetFenceToken() <= fence.GetFenceToken() {
		t.Errorf("the new fence token is %d, want it strictly above the outgoing %d", active.GetFenceToken(), fence.GetFenceToken())
	}
	if active.GetOperationId() != "op-handoff-3" {
		t.Errorf("the operation id changed to %q; a handover relocates an operation, it does not replace one", active.GetOperationId())
	}
	if len(active.GetHandovers()) != 1 {
		t.Fatalf("the record carries %d handovers, want 1", len(active.GetHandovers()))
	}
	h := active.GetHandovers()[0]
	if h.GetFromNodeId() != c.ids[holder] || h.GetToNodeId() != successor || h.GetToFenceToken() != active.GetFenceToken() {
		t.Errorf("handover = %+v, want it to name %s handing to %s at the new token", h, c.ids[holder], successor)
	}
	if !strings.Contains(h.GetReason(), "issue-restart") {
		t.Errorf("handover reason = %q, want it to say which step caused the move", h.GetReason())
	}
}

// TestManagerdRestartHandoff_TheLeasesStepIndexIsTheOperationsOwn pins
// the one indexing rule a replacement has to get right and cannot guess.
//
// The FSM appends a step record only at exactly the next free ordinal
// and refuses a gap, so a replacement that guessed would be refused with
// a message about a missing position rather than about anything it did
// wrong. Three steps are recorded by the coordinator first, and the
// replacement's own record has to land at index 3.
func TestManagerdRestartHandoff_TheStepIndexIsTheOperationsOwn(t *testing.T) {
	c := newHandoffCluster(t, 3)
	coordIdx, target := coordinatorAndTarget(t, c)
	fence := acquireOperation(t, c, c.ids[coordIdx], "op-handoff-4")
	coordinator := c.servers[coordIdx]

	// Two steps already recorded, by the coordinator, each with the
	// evidence a positive claim requires.
	for i, step := range []string{"step-aside", "gather-facts"} {
		resp, err := coordinator.mutateColonyUpdate(directTokenCtx(), &rpcpb.MutateColonyUpdateRequest{
			Op: &rpcpb.MutateColonyUpdateRequest_Advance{Advance: &rpcpb.AdvanceColonyUpdate{
				Fence: fence, Step: step, TargetNodeId: c.ids[target],
				Detail: "the coordinator recorded " + step,
				StepRecord: &rpcpb.ColonyUpdateStepRecord{
					Index: uint32(i), Step: step, Outcome: restartOutcomeConfirmed,
					Detail:   "the coordinator recorded " + step,
					Evidence: []string{"synthetic evidence for " + step},
					NodeId:   "comb-1",
				},
			}},
		})
		if err != nil || resp.GetError() != "" {
			t.Fatalf("recording %s: resp %v err %v", step, resp.GetError(), err)
		}
	}

	if err := NewManagerdHandoffStore(c.dirs[target]).Save(ManagerdHandoffRecord{
		OperationID: "op-handoff-4", NodeID: c.ids[target], Step: "issue-restart",
		Service: managerdServiceName, LeaseID: 88, ExpectedBuild: "comb-build-1",
	}); err != nil {
		t.Fatal(err)
	}
	replacement := NewServer(c.servers[target].raft, c.ids[target], isostore.New(t.TempDir()), nil, nil, nil, nil, "", nil, nil, nil, 0, nil)
	replacement.peers = &handoffForwarder{h: c.planHarness}
	replacement.SetColonyUpdateHandoffStore(NewManagerdHandoffStore(c.dirs[target]), &recordingRestarter{})
	replacement.SetColonyUpdateHandoffBuildPaths(buildgate.Paths{
		LibexecDir: filepath.Join(c.dirs[target], "libexec"),
		LogDir:     filepath.Join(c.dirs[target], "log"),
	})

	confirmation, err := replacement.ConfirmManagerdRestartHandoff(context.Background())
	if err != nil {
		t.Fatalf("ConfirmManagerdRestartHandoff(): %v (%s)", err, confirmation.Detail)
	}

	// Read the replicated copy, not the response: the advance is
	// committed on the leader and this node is a follower, whose own
	// state trails by whatever replication costs. Reading the instant
	// the call returns would be a flaky test that would eventually be
	// reported as a real finding.
	var steps []*internalpb.ColonyUpdateStepRecord
	eventually(t, 10*time.Second, func() bool {
		state, err := c.servers[target].raft.GetColonyUpdateStateLocal(context.Background(), "op-handoff-4")
		if err != nil || state.GetActive() == nil {
			return false
		}
		steps = state.GetActive().GetSteps()
		return len(steps) == 3
	})
	if len(steps) != 3 {
		t.Fatalf("the operation has %d step records, want 3; detail: %s", len(steps), confirmation.Detail)
	}
	if steps[2].GetStep() != "issue-restart" || steps[2].GetNodeId() != c.ids[target] {
		t.Errorf("the last step record = %+v, want the replacement's own record at index 2", steps[2])
	}
}

// TestManagerdRestartHandoff_WrongBuildIsFailedNotConfirmed is rule 6's
// load-bearing case: the replacement came back, the restart happened,
// and the step is still NOT a success.
//
// This is the answer that is easiest to get wrong in the direction that
// matters, because every other signal says it worked. The process is up.
// The lease confirmed. The restart demonstrably occurred. And the Comb is
// running a build nobody asked for, which is a failure - positive
// evidence, not the absence of evidence - and the difference decides
// whether the sweep may continue.
func TestManagerdRestartHandoff_WrongBuildIsFailedNotConfirmed(t *testing.T) {
	c := newHandoffCluster(t, 3)
	coordIdx, target := coordinatorAndTarget(t, c)
	acquireOperation(t, c, c.ids[coordIdx], "op-handoff-5")

	// The marker says the restart was scheduled against the build that
	// was on disk; the log now claims a different one is running, which
	// is what a Comb whose binary was replaced again mid-flight looks
	// like.
	marker := ManagerdHandoffRecord{
		OperationID: "op-handoff-5", NodeID: c.ids[target], Step: "issue-restart",
		Service: managerdServiceName, LeaseID: 99, ExpectedBuild: "comb-build-1",
	}
	if err := NewManagerdHandoffStore(c.dirs[target]).Save(marker); err != nil {
		t.Fatal(err)
	}
	// Replace the running log line so the two readings disagree.
	runLog := filepath.Join(c.dirs[target], "log", "managerd.log")
	line := "2026/09/26 02:03:23 managerd: build=some-other-build commit=some-other-build built=2026-09-26T11:52:03-04:00 go=freebsd/amd64 listening on 127.0.0.1:0 (node-id=comb)\n"
	if err := os.WriteFile(runLog, []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}

	replacement := NewServer(c.servers[target].raft, c.ids[target], isostore.New(t.TempDir()), nil, nil, nil, nil, "", nil, nil, nil, 0, nil)
	replacement.peers = &handoffForwarder{h: c.planHarness}
	replacement.SetColonyUpdateHandoffStore(NewManagerdHandoffStore(c.dirs[target]), &recordingRestarter{})
	replacement.SetColonyUpdateHandoffBuildPaths(buildgate.Paths{
		LibexecDir: filepath.Join(c.dirs[target], "libexec"),
		LogDir:     filepath.Join(c.dirs[target], "log"),
	})

	confirmation, err := replacement.ConfirmManagerdRestartHandoff(context.Background())
	if err != nil {
		t.Fatalf("ConfirmManagerdRestartHandoff(): %v", err)
	}
	if confirmation.Outcome == restartOutcomeConfirmed {
		t.Fatalf("a Comb running the wrong build was reported as confirmed: %s", confirmation.Detail)
	}
	if confirmation.Outcome != restartOutcomeFailed {
		t.Errorf("outcome = %q, want %q; a build that differs is positive evidence, not an absence of it", confirmation.Outcome, restartOutcomeFailed)
	}
	// The lease is still released: the restart DID happen, and the
	// build's freshness is a different question with a different answer.
	if !confirmation.LeaseReleased {
		t.Error("the lease was not released even though this process is the restarted process; that would block the Colony on a restart that demonstrably occurred")
	}

	// And the failure is in the operation's durable history, not just in
	// a log line nobody keeps.
	eventually(t, 10*time.Second, func() bool {
		state, err := c.servers[target].raft.GetColonyUpdateStateLocal(context.Background(), "op-handoff-5")
		if err != nil || state.GetActive() == nil || len(state.GetActive().GetSteps()) == 0 {
			return false
		}
		return state.GetActive().GetSteps()[0].GetOutcome() == restartOutcomeFailed
	})
}
