package zfs

// End-to-end shape of one replication run, with both ends faked.
//
// Nothing here is policy, scheduling or raft — those belong to other
// workers. What it proves is that the primitives compose into the
// sequence ADR-0130 §2 describes, and that the three states survive
// the composition: a run that never reached the target must not leave
// an acked generation, a target nobody could reach must not be reported
// as current, and an interrupted receive must be resumable rather than
// something to force past.

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
)

func TestPlanSendArgsMatchWhatOpenRuns(t *testing.T) {
	// The plan's Args and the executed argument vector must agree, or
	// the plan stops describing the run and a test that asserts Args
	// is asserting a fiction.
	cases := []struct {
		name  string
		state SourceState
		req   SendRequest
	}{
		{
			name:  "first run",
			state: SourceState{Dataset: "vm-1"},
			req:   SendRequest{TargetGeneration: 1},
		},
		{
			name: "incremental",
			state: SourceState{
				Dataset: "jail-2", LastAckedGeneration: 4, HaveLastAcked: true,
				LastAckedSnapshot: "jail-2@apiary-repl-00000004", LastAckedSnapshotPresent: true,
			},
			req: SendRequest{TargetGeneration: 5},
		},
		{
			name: "resync",
			state: SourceState{
				Dataset: "jail-2", LastAckedGeneration: 4, HaveLastAcked: true,
				LastAckedSnapshot: "jail-2@apiary-repl-00000004", LastAckedSnapshotPresent: false,
				FallbackFromSnapshot: "jail-2@apiary-repl-00000003",
			},
			req: SendRequest{TargetGeneration: 5},
		},
		{
			name: "resume",
			state: SourceState{
				Dataset: "jail-2", LastAckedGeneration: 4, HaveLastAcked: true,
			},
			req: SendRequest{TargetGeneration: 5, ObservedResumeToken: "1-feedface"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			plan, err := PlanSend(tc.state, tc.req)
			if err != nil {
				t.Fatalf("PlanSend: %v", err)
			}
			r := &fakeRunner{start: func(args []string) (Stream, error) { return &fakeSend{}, nil }}
			m := newFakeManager("zroot/apiary", r)
			stream, err := m.Open(context.Background(), plan)
			if err != nil {
				t.Fatalf("Open: %v", err)
			}
			stream.Close()
			executed := r.sent()
			if len(executed) != 1 {
				t.Fatalf("%d commands ran", len(executed))
			}
			// The executed vector is the plan's own vector with Base
			// prepended to every dataset it names.
			if len(executed[0]) != len(plan.Args) {
				t.Fatalf("plan.Args = %v but %v ran", plan.Args, executed[0])
			}
			for i, ran := range executed[0] {
				want := plan.Args[i]
				if strings.Contains(want, "@") {
					want = "zroot/apiary/" + want
				}
				if ran != want {
					t.Errorf("arg %d: ran %q, want %q from plan.Args", i, ran, want)
				}
			}
		})
	}
}

// fakeSource answers a source's own observation with a scripted
// snapshot list, which is the only filesystem question the fence asks.
func fakeSource(snapshots ...string) *fakeRunner {
	out := ""
	for _, s := range snapshots {
		out += "zroot/apiary/vm-1@" + s + "\n"
	}
	return &fakeRunner{run: func(args []string) (string, error) { return out, nil }}
}

func TestReplicationRunHappyPath(t *testing.T) {
	// Generation 1: a full send from origin, the target acks, the
	// counter advances. Then generation 2 goes incrementally, because
	// that is the steady state ADR-0130 asks for.
	source := fakeSource("apiary-repl-00000001")
	sm := newFakeManager("zroot/apiary", source)
	la := NewLastAcked("policy-1", "vm-1")

	// Run 1.
	state, err := sm.ObserveSource(context.Background(), la)
	if err != nil {
		t.Fatalf("ObserveSource: %v", err)
	}
	plan, err := PlanSend(state, SendRequest{TargetGeneration: 1})
	if err != nil {
		t.Fatalf("PlanSend: %v", err)
	}
	if plan.Kind != SendFull {
		t.Fatalf("run 1 planned %q, want a full send", plan.Kind)
	}
	source.start = func(args []string) (Stream, error) { return &fakeSend{payload: "full-stream"}, nil }
	w := &recordingWriter{}
	if err := sm.Pump(context.Background(), plan, w); err != nil {
		t.Fatalf("Pump: %v", err)
	}
	if w.String() != "full-stream" {
		t.Errorf("delivered %q", w.String())
	}

	// The target acks, and only the target may do that.
	la, err = la.RecordAck(1)
	if err != nil {
		t.Fatalf("RecordAck: %v", err)
	}

	// Run 2: the snapshot now exists, so the send is incremental.
	source2 := fakeSource("apiary-repl-00000001", "apiary-repl-00000002")
	sm2 := newFakeManager("zroot/apiary", source2)
	state2, err := sm2.ObserveSource(context.Background(), la)
	if err != nil {
		t.Fatalf("ObserveSource: %v", err)
	}
	if !state2.LastAckedSnapshotPresent {
		t.Fatal("the last acked snapshot is reported absent after it was acked")
	}
	plan2, err := PlanSend(state2, SendRequest{TargetGeneration: 2})
	if err != nil {
		t.Fatalf("PlanSend: %v", err)
	}
	if plan2.Kind != SendIncremental {
		t.Fatalf("run 2 planned %q, want an incremental send", plan2.Kind)
	}
	source2.start = func(args []string) (Stream, error) { return &fakeSend{payload: "delta"}, nil }
	if err := sm2.Pump(context.Background(), plan2, &recordingWriter{}); err != nil {
		t.Fatalf("Pump: %v", err)
	}
	la, err = la.RecordAck(2)
	if err != nil {
		t.Fatalf("RecordAck: %v", err)
	}

	// The target's own view is current, and only because the target
	// was asked.
	obs := observedTarget(2, true)
	obs.Token = ObservedNoToken("vm-1")
	if v, _ := Classify(obs, la.Generation, true); v != ReplicaCurrent {
		t.Errorf("verdict = %q, want %q", v, ReplicaCurrent)
	}
}

func TestReplicationRunFailureDoesNotAdvanceTheGeneration(t *testing.T) {
	// zfs(8) rejects the send. The run is a failure with a reason, the
	// generation stays un-acked, and a late ack of the rejected
	// generation cannot be applied afterwards.
	source := fakeSource()
	sm := newFakeManager("zroot/apiary", source)
	la := NewLastAcked("policy-1", "vm-1")
	gen, err := la.NextGeneration()
	if err != nil {
		t.Fatalf("NextGeneration: %v", err)
	}
	state, err := sm.ObserveSource(context.Background(), la)
	if err != nil {
		t.Fatalf("ObserveSource: %v", err)
	}
	plan, err := PlanSend(state, SendRequest{TargetGeneration: gen})
	if err != nil {
		t.Fatalf("PlanSend: %v", err)
	}
	source.start = func(args []string) (Stream, error) {
		return &fakeSend{payload: "partial", closeErr: errors.New("zfs send: warning: cannot send 'zroot/apiary/vm-1@apiary-repl-00000001': snapshot not found")}, nil
	}
	err = sm.Pump(context.Background(), plan, &recordingWriter{})
	if err == nil {
		t.Fatal("Pump returned nil for a rejected send")
	}
	if got := ClassifySendError(err); got != SendFailed {
		t.Errorf("outcome = %q, want %q", got, SendFailed)
	}
	if la.HaveGeneration {
		t.Error("a failed run advanced the acked generation")
	}
	// The next run mints generation 1 again: the counter only moves on
	// an ack, and the refused generation was never acked.
	next, err := la.NextGeneration()
	if err != nil {
		t.Fatalf("NextGeneration after a failure: %v", err)
	}
	if next != gen {
		t.Errorf("next generation = %d, want %d: a failed run must not consume a generation", next, gen)
	}
}

func TestReplicationRunUnobservedTargetIsNeverCurrent(t *testing.T) {
	// The target cannot be reached. The source's own copy is fine and
	// the source's own state is known — but nothing about the target is
	// known, so nothing about the target may be claimed.
	la := NewLastAckedAt("policy-1", "vm-1", 7, "vm-1@apiary-repl-00000007")
	unreachable := TargetObservation{
		Dataset: "vm-1", NodeID: "comb-2",
		Observed: false,
		Detail:   "dial tcp 10.90.0.96:9443: connect: connection refused",
	}
	verdict, why := Classify(unreachable, la.Generation, true)
	if verdict != ReplicaUnobserved {
		t.Fatalf("verdict = %q, want %q", verdict, ReplicaUnobserved)
	}
	if !strings.Contains(why, "could not check") {
		t.Errorf("reason does not distinguish 'could not check': %q", why)
	}
	if !strings.Contains(why, "connection refused") {
		t.Errorf("reason dropped the transport's own text: %q", why)
	}
	// And the target's fence is equally undecided: a target that could
	// not be asked has no generation to compare against.
	if _, known := la.Lag(0, false); known {
		t.Error("lag was reported as known for an unreachable target")
	}
}

func TestReplicationRunInterruptedReceiveResumesRatherThanForces(t *testing.T) {
	// The whole point of the resume path, end to end:
	//
	//   1. a receive is interrupted, so the destination is mid-receive
	//      and holds a live token;
	//   2. the target reports replica_partial_receive — a confirmed
	//      state, distinct from both current and unobserved;
	//   3. the target refuses a bare receive and does NOT force;
	//   4. the source, told about the token for the pending
	//      generation, resumes with -t rather than restarting.
	la := NewLastAckedAt("policy-1", "vm-1", 4, "vm-1@apiary-repl-00000004")
	const token = "1-abcd1234"

	// 2. the target's own view.
	obs := observedTarget(4, true)
	obs.Token = ObservedLiveToken("vm-1", token)
	verdict, why := Classify(obs, la.Generation, true)
	if verdict != ReplicaPartialReceive {
		t.Fatalf("verdict = %q, want %q", verdict, ReplicaPartialReceive)
	}
	if !strings.Contains(why, "interrupted") {
		t.Errorf("reason = %q", why)
	}

	// 3. the receive gate refuses, and refuses without -F.
	if err := CheckDestination(obs, 4, true); !errors.Is(err, ErrDestResumable) {
		t.Fatalf("CheckDestination = %v, want ErrDestResumable", err)
	}
	receiver := &fakeRunner{
		run: func(args []string) (string, error) { return token, nil },
		recv: func(args []string, stdin io.Reader) error {
			t.Error("a receive was spawned onto a destination that is mid-receive")
			return nil
		},
	}
	rm := newFakeManager("zroot/apiary", receiver)
	err := rm.ReceiveInto(context.Background(), "vm-1", strings.NewReader("new-stream"), ReceiveOptions{NoMount: true})
	if !errors.Is(err, ErrDestResumable) {
		t.Fatalf("ReceiveInto = %v, want ErrDestResumable", err)
	}
	if n := len(receiver.received()); n != 0 {
		t.Errorf("%d receive commands were run", n)
	}

	// 4. the source resumes, because the token belongs to the pending
	// generation.
	state := SourceState{
		Dataset: "vm-1", LastAckedGeneration: 4, HaveLastAcked: true,
		LastAckedSnapshot: "vm-1@apiary-repl-00000004", LastAckedSnapshotPresent: true,
	}
	plan, err := PlanSend(state, SendRequest{TargetGeneration: 5, ObservedResumeToken: token})
	if err != nil {
		t.Fatalf("PlanSend: %v", err)
	}
	if plan.Kind != SendResume || !plan.ResumedFromToken {
		t.Fatalf("plan = %+v, want a resumed send", plan)
	}
	// And the resumed receive proceeds with that exact token.
	resumeReceiver := &fakeRunner{
		run:  func(args []string) (string, error) { return token, nil },
		recv: func(args []string, stdin io.Reader) error { return nil },
	}
	rm2 := newFakeManager("zroot/apiary", resumeReceiver)
	if err := rm2.ReceiveInto(context.Background(), "vm-1", strings.NewReader("rest"), ReceiveOptions{NoMount: true, ResumeToken: token}); err != nil {
		t.Fatalf("resumed ReceiveInto: %v", err)
	}
	got := resumeReceiver.received()
	want := []string{"receive", "-u", "-t", token, "zroot/apiary/vm-1"}
	if len(got) != 1 || !equalArgs(got[0], want...) {
		t.Errorf("resumed receive args = %v, want %v", got, want)
	}
}

func TestReplicationRunExpiredTokenFallsBackToTheLastAckedSnapshot(t *testing.T) {
	// ADR-0130: "if the token is missing or rejected (destination
	// dataset destroyed, token expired), restarts the incremental send
	// from the last acked snapshot, not from the origin full send."
	const dead = "1-this-token-is-dead"
	la := NewLastAckedAt("policy-1", "vm-1", 4, "vm-1@apiary-repl-00000004")

	// The local store still remembers a token; the destination no
	// longer reports one. That is stale, not live.
	live := ObservedNoToken("vm-1")
	stored := TokenRecord{PolicyID: "policy-1", State: ResumeTokenLive, Token: dead}
	combined := ObserveToken("policy-1", live, stored)
	if combined.State != ResumeTokenStale {
		t.Fatalf("combined state = %q, want %q", combined.State, ResumeTokenStale)
	}
	if combined.Usable() {
		t.Error("a stale token is reported as usable")
	}

	// A receive with that token is refused outright: handing it to zfs
	// would fail obscurely, and a caller that "fixed" that by adding
	// -F would destroy the destination's data to work around a
	// bookkeeping problem.
	receiver := &fakeRunner{
		run: func(args []string) (string, error) { return "-", nil },
		recv: func(args []string, stdin io.Reader) error {
			t.Error("a stale token still spawned a receive")
			return nil
		},
	}
	rm := newFakeManager("zroot/apiary", receiver)
	err := rm.ReceiveInto(context.Background(), "vm-1", strings.NewReader("x"), ReceiveOptions{ResumeToken: dead})
	if !errors.Is(err, ErrResumeTokenStale) {
		t.Fatalf("ReceiveInto = %v, want ErrResumeTokenStale", err)
	}
	if n := len(receiver.received()); n != 0 {
		t.Errorf("%d receive commands were run", n)
	}

	// The next run is an ordinary incremental from the last acked
	// snapshot, and the store is cleared.
	source := fakeSource("apiary-repl-00000004")
	sm := newFakeManager("zroot/apiary", source)
	state, err := sm.ObserveSource(context.Background(), la)
	if err != nil {
		t.Fatalf("ObserveSource: %v", err)
	}
	plan, err := PlanSend(state, SendRequest{TargetGeneration: 5})
	if err != nil {
		t.Fatalf("PlanSend: %v", err)
	}
	if plan.Kind != SendIncremental {
		t.Fatalf("plan = %+v, want an incremental restart from the last acked snapshot, not a full send", plan)
	}
	if plan.FromSnapshot != "vm-1@apiary-repl-00000004" {
		t.Errorf("FromSnapshot = %q", plan.FromSnapshot)
	}
	store := NewFileTokenStore(t.TempDir())
	if err := store.Save("policy-1", dead); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if err := store.Delete("policy-1"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	rec, err := store.Load("policy-1")
	if err != nil || rec.State != ResumeTokenNone {
		t.Errorf("after clearing, Load = %+v, %v", rec, err)
	}
}

func TestReplicationRunDestinationAheadIsRefusedAndNeverForced(t *testing.T) {
	// The dangerous case from the task list: the destination already
	// has newer snapshots. zfs would reject the receive, and the only
	// flag that gets past it destroys the destination's newer data. So
	// the gate refuses, the message says what not to do, and nothing in
	// this package produces -F on its own.
	obs := observedTarget(12, true)
	obs.NodeID = "comb-2"
	err := CheckDestination(obs, 7, true)
	if !errors.Is(err, ErrDestinationAhead) {
		t.Fatalf("CheckDestination = %v, want ErrDestinationAhead", err)
	}
	var dae *DestinationAheadError
	if !errors.As(err, &dae) {
		t.Fatalf("err is not a *DestinationAheadError: %v", err)
	}
	if dae.Held != 12 || dae.StreamOrigin != 7 {
		t.Errorf("DestinationAheadError = %+v, want held 12 origin 7", dae)
	}
	if dae.Dataset != "vm-1" {
		t.Errorf("Dataset = %q", dae.Dataset)
	}
	// The verdict the operator sees for that destination is neither
	// current nor a fault: the target is demonstrably AHEAD of what the
	// source believes was acked, which is its own confirmed state.
	if v, _ := Classify(obs, 7, true); v != ReplicaAheadOfSource {
		t.Errorf("verdict = %q, want %q", v, ReplicaAheadOfSource)
	}
	// A full send (no origin) is not judged on generations at all: zfs
	// decides, and this package does not reach for -F on its behalf.
	if err := CheckDestination(obs, 0, false); err != nil {
		t.Errorf("a full send onto an ahead destination = %v, want nil", err)
	}
}

func TestReplicationRunStagingNamingIsSeparateFromTheFinalName(t *testing.T) {
	// ADR-0130's first-receive staging: a failed first receive must
	// not leave a half-populated dataset occupying the final name,
	// where it would be indistinguishable from a real one. This package
	// produces the staging name and refuses to produce a rename —
	// renaming is a policy-lifecycle step, and a half-finished rename
	// is worse than a missing one.
	name, err := StagingDatasetName("policy-1")
	if err != nil {
		t.Fatalf("StagingDatasetName: %v", err)
	}
	if name != ".apiary-replication/policy-1" {
		t.Errorf("staging name = %q", name)
	}
	// It is a legal base-relative dataset name, so a Manager can
	// receive into it and query its resume token.
	m := newFakeManager("zroot/apiary", &fakeRunner{
		run:  func(args []string) (string, error) { return "-", nil },
		recv: func(args []string, stdin io.Reader) error { return nil },
	})
	if _, err := m.ResumeToken(context.Background(), name); err != nil {
		t.Errorf("ResumeToken on the staging dataset: %v", err)
	}
	ran := m.runner.(*fakeRunner).ran()
	if len(ran) != 1 || !strings.HasSuffix(ran[0][len(ran[0])-1], "/.apiary-replication/policy-1") {
		t.Errorf("staging query = %v", ran)
	}
	// The staging dataset is never a replication source's own
	// generation holder, so a policy pointed at one refuses to plan.
	if _, err := PlanSend(SourceState{Dataset: name}, SendRequest{TargetGeneration: 1}); err != nil {
		// It is a legal name, so planning succeeds; what must not
		// happen is any special-casing of it. Assert the plan is an
		// ordinary full send and move on.
		t.Fatalf("unexpected: %v", err)
	}
}
