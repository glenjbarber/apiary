package zfs

// Argument construction and the run-level evidence rules. Every `zfs`
// invocation this package makes is asserted here as an exact argument
// vector through the injected CommandRunner, because a wrong flag
// produces a wrong stream under a status line that still reads
// healthy.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
)

func TestManagerSendIncrementalArgumentConstruction(t *testing.T) {
	// -i carries a single delta and no intermediate snapshots. It is
	// the steady-state form, so its arguments are the ones that run
	// most often and the ones most worth pinning.
	r := &fakeRunner{start: func(args []string) (Stream, error) {
		return &fakeSend{payload: "stream"}, nil
	}}
	m := newFakeManager("zroot/apiary", r)
	if _, err := m.SendIncremental(context.Background(), "vm-1@apiary-repl-00000007", "vm-1@apiary-repl-00000008"); err != nil {
		t.Fatalf("SendIncremental: %v", err)
	}
	sent := r.sent()
	if len(sent) != 1 {
		t.Fatalf("sent %d commands, want 1", len(sent))
	}
	want := []string{"send", "-i", "zroot/apiary/vm-1@apiary-repl-00000007", "zroot/apiary/vm-1@apiary-repl-00000008"}
	if !equalArgs(sent[0], want...) {
		t.Errorf("args = %v, want %v", sent[0], want)
	}
}

func TestManagerSendAllIncrementalArgumentConstruction(t *testing.T) {
	// -I is the resync form: from and to inclusive, with every
	// intermediate snapshot, so a destination that never received the
	// chain can still be brought forward in one stream.
	r := &fakeRunner{start: func(args []string) (Stream, error) { return &fakeSend{}, nil }}
	m := newFakeManager("zroot/apiary", r)
	if _, err := m.SendAllIncremental(context.Background(), "jail-3@apiary-repl-00000002", "jail-3@apiary-repl-00000009"); err != nil {
		t.Fatalf("SendAllIncremental: %v", err)
	}
	sent := r.sent()
	if len(sent) != 1 {
		t.Fatalf("sent %d commands, want 1", len(sent))
	}
	if sent[0][1] != "-I" {
		t.Errorf("flag = %q, want -I; a resync that sends one delta cannot be applied by a destination holding the chain", sent[0][1])
	}
	// -I and -i are mutually exclusive in zfs(8): a request carrying
	// both is a bug in this package, not in the operator's data.
	for _, a := range sent[0] {
		if a == "-i" {
			t.Errorf("args %v carry both -I and -i", sent[0])
		}
	}
}

func TestManagerSendFromIsTheSameFullHistoryForm(t *testing.T) {
	// SendFrom is the name ADR-0130 uses; it must be the -I form and
	// nothing else, or "full resync" would quietly mean "one delta".
	r := &fakeRunner{start: func(args []string) (Stream, error) { return &fakeSend{}, nil }}
	m := newFakeManager("zroot/apiary", r)
	if _, err := m.SendFrom(context.Background(), "vm-1@apiary-repl-00000001", "vm-1@apiary-repl-00000005"); err != nil {
		t.Fatalf("SendFrom: %v", err)
	}
	sent := r.sent()
	if len(sent) != 1 || !equalArgs(sent[0], "send", "-I", "zroot/apiary/vm-1@apiary-repl-00000001", "zroot/apiary/vm-1@apiary-repl-00000005") {
		t.Errorf("args = %v", sent)
	}
}

func TestManagerSendResumeArgumentConstruction(t *testing.T) {
	r := &fakeRunner{start: func(args []string) (Stream, error) { return &fakeSend{}, nil }}
	m := newFakeManager("zroot/apiary", r)
	if _, err := m.SendResume(context.Background(), "  1-abc123def456  "); err != nil {
		t.Fatalf("SendResume: %v", err)
	}
	sent := r.sent()
	// A token is opaque zfs(8) state, so the command carries no
	// dataset at all and the token is trimmed but otherwise untouched.
	if len(sent) != 1 || !equalArgs(sent[0], "send", "-t", "1-abc123def456") {
		t.Errorf("args = %v, want [send -t 1-abc123def456]", sent)
	}
	// An empty token would become `zfs send -t` and zfs would either
	// error obscurely or, worse, be filled in by a shell somewhere
	// upstream.
	for _, bad := range []string{"", "   ", "\t\n"} {
		if _, err := m.SendResume(context.Background(), bad); err == nil {
			t.Errorf("SendResume(%q) returned no error", bad)
		}
	}
	if n := len(r.sent()); n != 1 {
		t.Errorf("%d commands were started; a refused token must spawn nothing", n)
	}
}

func TestManagerOpenQualifiesEverySendForm(t *testing.T) {
	// The plan carries base-relative names; Open must expand them
	// through Base. If it did not, every command would address a
	// dataset outside the configured base.
	cases := []struct {
		name string
		plan SendPlan
		want []string
	}{
		{
			name: "full",
			plan: SendPlan{Kind: SendFull, Dataset: "vm-1", ToGeneration: 1, ToSnapshot: "vm-1@apiary-repl-00000001"},
			want: []string{"send", "zroot/apiary/vm-1@apiary-repl-00000001"},
		},
		{
			name: "incremental",
			plan: SendPlan{Kind: SendIncremental, Dataset: "vm-1", ToSnapshot: "vm-1@apiary-repl-00000002", FromSnapshot: "vm-1@apiary-repl-00000001"},
			want: []string{"send", "-i", "zroot/apiary/vm-1@apiary-repl-00000001", "zroot/apiary/vm-1@apiary-repl-00000002"},
		},
		{
			name: "full incremental resync",
			plan: SendPlan{Kind: SendFullIncremental, Dataset: "vm-1", ToSnapshot: "vm-1@apiary-repl-00000005", FromSnapshot: "vm-1@apiary-repl-00000001"},
			want: []string{"send", "-I", "zroot/apiary/vm-1@apiary-repl-00000001", "zroot/apiary/vm-1@apiary-repl-00000005"},
		},
		{
			name: "resume carries no dataset",
			plan: SendPlan{Kind: SendResume, Dataset: "vm-1", ToSnapshot: "vm-1@apiary-repl-00000002", Token: "1-feedface"},
			want: []string{"send", "-t", "1-feedface"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := &fakeRunner{start: func(args []string) (Stream, error) { return &fakeSend{}, nil }}
			m := newFakeManager("zroot/apiary", r)
			stream, err := m.Open(context.Background(), tc.plan)
			if err != nil {
				t.Fatalf("Open: %v", err)
			}
			stream.Close()
			sent := r.sent()
			if len(sent) != 1 || !equalArgs(sent[0], tc.want...) {
				t.Errorf("args = %v, want %v", sent, tc.want)
			}
			// What Open ran must be exactly what the plan said it
			// would run, with Base prepended. A divergence here means
			// the plan stops describing the run.
			qualified, err := m.qualifyPlan(tc.plan)
			if err != nil {
				t.Fatalf("qualifyPlan: %v", err)
			}
			if !equalArgs(qualified, tc.want...) {
				t.Errorf("qualifyPlan = %v, want %v", qualified, tc.want)
			}
		})
	}
}

func TestManagerOpenRefusesNamesThatEscapeBase(t *testing.T) {
	// A plan is data. A policy id or dataset name that arrived from
	// raft must not be able to address a dataset outside Base, and the
	// refusal must happen before any command is started.
	for _, plan := range []SendPlan{
		{Kind: SendFull, Dataset: "vm-1", ToSnapshot: "../escape@apiary-repl-00000001"},
		{Kind: SendFull, Dataset: "vm-1", ToSnapshot: "/absolute@apiary-repl-00000001"},
		{Kind: SendFull, Dataset: "vm-1", ToSnapshot: "vm-1"}, // no snapshot
		{Kind: SendFull, Dataset: "vm-1", ToSnapshot: "vm-1@"},
		{Kind: SendFull, Dataset: "vm-1", ToSnapshot: "vm-1@apiary-repl-00000001/extra"},
		{Kind: SendIncremental, Dataset: "vm-1", ToSnapshot: "vm-1@apiary-repl-00000002", FromSnapshot: "../x@apiary-repl-00000001"},
		{Kind: SendResume, Dataset: "vm-1", ToSnapshot: "vm-1@apiary-repl-00000002", Token: "   "},
		{Kind: "nonsense", Dataset: "vm-1", ToSnapshot: "vm-1@apiary-repl-00000002"},
	} {
		r := &fakeRunner{start: func(args []string) (Stream, error) { return &fakeSend{}, nil }}
		m := newFakeManager("zroot/apiary", r)
		if _, err := m.Open(context.Background(), plan); err == nil {
			t.Errorf("Open(%+v) returned no error", plan)
		}
		if n := len(r.sent()); n != 0 {
			t.Errorf("Open(%+v) started %d commands despite failing validation", plan, n)
		}
	}
}

func TestObserveSourceAnswersTheFenceFilesystemQuestion(t *testing.T) {
	// The fence cannot know what the filesystem holds, so it asks.
	// These rows are the answers that decide incremental versus full
	// resend, and each one is a real dataset state.
	cases := []struct {
		name        string
		out         string
		la          LastAcked
		wantPresent bool
		wantOrigin  string
	}{
		{
			name:        "the last acked snapshot is still there, so the send is incremental",
			out:         "zroot/apiary/vm-1@apiary-repl-00000007\nzroot/apiary/vm-1@apiary-repl-00000009\nzroot/apiary/vm-1@before-migration\n",
			la:          NewLastAckedAt("policy-1", "vm-1", 9, "vm-1@apiary-repl-00000009"),
			wantPresent: true,
		},
		{
			name:       "the last acked snapshot was destroyed and an older one survives",
			out:        "zroot/apiary/vm-1@apiary-repl-00000007\nzroot/apiary/vm-1@apiary-repl-00000008\n",
			la:         NewLastAckedAt("policy-1", "vm-1", 9, "vm-1@apiary-repl-00000009"),
			wantOrigin: "vm-1@apiary-repl-00000008",
		},
		{
			name:       "no snapshot at or before the last acked generation remains",
			out:        "zroot/apiary/vm-1@apiary-repl-00000012\nzroot/apiary/vm-1@before-migration\n",
			la:         NewLastAckedAt("policy-1", "vm-1", 9, "vm-1@apiary-repl-00000009"),
			wantOrigin: "",
		},
		{
			name:        "no history at all has no resync origin by definition",
			out:         "zroot/apiary/vm-1@before-migration\n",
			la:          NewLastAcked("policy-1", "vm-1"),
			wantPresent: false,
			wantOrigin:  "",
		},
		{
			name:        "an empty list is a confirmed absence, not an unanswered question",
			out:         "",
			la:          NewLastAckedAt("policy-1", "vm-1", 9, "vm-1@apiary-repl-00000009"),
			wantPresent: false,
		},
		{
			name: "a snapshot of a child dataset is not this dataset's",
			out:  "zroot/apiary/vm-1/child@apiary-repl-00000009\nzroot/apiary/vm-1@apiary-repl-00000002\n",
			la:   NewLastAckedAt("policy-1", "vm-1", 9, "vm-1@apiary-repl-00000009"),
			// 00000002 is at or below 9, so it is a legal resync origin;
			// the child's 00000009 is not, and the last acked one is
			// absent.
			wantOrigin: "vm-1@apiary-repl-00000002",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := tc.out
			r := &fakeRunner{run: func(args []string) (string, error) { return out, nil }}
			m := newFakeManager("zroot/apiary", r)
			state, err := m.ObserveSource(context.Background(), tc.la)
			if err != nil {
				t.Fatalf("ObserveSource: %v", err)
			}
			if state.LastAckedSnapshotPresent != tc.wantPresent {
				t.Errorf("LastAckedSnapshotPresent = %v, want %v", state.LastAckedSnapshotPresent, tc.wantPresent)
			}
			if state.FallbackFromSnapshot != tc.wantOrigin {
				t.Errorf("FallbackFromSnapshot = %q, want %q", state.FallbackFromSnapshot, tc.wantOrigin)
			}
			if state.Dataset != "vm-1" {
				t.Errorf("carried-through Dataset = %q", state.Dataset)
			}
			if state.HaveLastAcked != tc.la.HaveGeneration || state.LastAckedGeneration != tc.la.Generation {
				t.Errorf("carried-through generation fields = %+v, want (%d, %v)", state, tc.la.Generation, tc.la.HaveGeneration)
			}
			// The query itself must be the non-recursive snapshot list
			// of this dataset, fully qualified.
			ran := r.ran()
			if len(ran) != 1 || !equalArgs(ran[0], "list", "-H", "-o", "name", "-t", "snapshot", "zroot/apiary/vm-1") {
				t.Errorf("query = %v", ran)
			}
		})
	}
}

func TestObserveSourceUnobservableIsNotAnEmptyList(t *testing.T) {
	// The distinction that matters most in this file: "this dataset
	// genuinely has no replication snapshots" escalates to a full
	// send, while "we could not find out" must stop the fence from
	// deciding anything at all.
	r := &fakeRunner{run: func(args []string) (string, error) {
		return "", errors.New("zfs list -H -o name -t snapshot zroot/apiary/vm-1: cannot open control socket: Operation not permitted")
	}}
	m := newFakeManager("zroot/apiary", r)
	state, err := m.ObserveSource(context.Background(), NewLastAckedAt("policy-1", "vm-1", 9, "vm-1@apiary-repl-00000009"))
	if !IsUnobserved(err) {
		t.Fatalf("err = %v, want an *UnobservedError", err)
	}
	if state.Dataset != "" {
		t.Errorf("an unobserved observation returned a usable state: %+v", state)
	}
}

func TestObserveSourceMissingDatasetIsAConfirmedAbsence(t *testing.T) {
	// A destroyed source is a fact, not silence, and it escalates to a
	// full send with no error at all.
	r := &fakeRunner{run: func(args []string) (string, error) {
		return "", zfsErr("list", "-H", "-o", "name", "-t", "snapshot", "zroot/apiary/vm-1")
	}}
	m := newFakeManager("zroot/apiary", r)
	state, err := m.ObserveSource(context.Background(), NewLastAckedAt("policy-1", "vm-1", 9, "vm-1@apiary-repl-00000009"))
	if err != nil {
		t.Fatalf("ObserveSource on a missing dataset = %v, want no error", err)
	}
	if state.LastAckedSnapshotPresent {
		t.Error("a missing dataset reported its last acked snapshot as present")
	}
	if state.FallbackFromSnapshot != "" {
		t.Errorf("a missing dataset produced a resync origin %q", state.FallbackFromSnapshot)
	}
	// And that is what escalates the plan:
	plan, err := PlanSend(state, SendRequest{TargetGeneration: 10})
	if err != nil {
		t.Fatalf("PlanSend: %v", err)
	}
	if plan.Kind != SendFull || !plan.EscalatedFromIncremental {
		t.Errorf("plan = %+v, want an escalated full send", plan)
	}
}

func TestObserveSourceRejectsAnEmptyDataset(t *testing.T) {
	m := newFakeManager("zroot/apiary", &fakeRunner{})
	if _, err := m.ObserveSource(context.Background(), LastAcked{PolicyID: "p"}); err == nil {
		t.Fatal("ObserveSource with no dataset returned no error")
	}
}

func TestPumpAlwaysClosesTheStreamAndReportsItsExit(t *testing.T) {
	// The Close contract is the only signal that distinguishes "the
	// send ran" from "the send was cut off", so Pump closes it exactly
	// once even when the copy failed, and its exit status is what the
	// caller gets when the copy succeeded.
	stream := &fakeSend{payload: "zfs-stream-bytes", closeErr: errors.New("zfs send: warning: cannot send incremental: previous snapshot not found")}
	r := &fakeRunner{start: func(args []string) (Stream, error) { return stream, nil }}
	m := newFakeManager("zroot/apiary", r)
	plan, err := PlanSend(SourceState{
		Dataset: "vm-1", LastAckedGeneration: 3, HaveLastAcked: true,
		LastAckedSnapshot: "vm-1@apiary-repl-00000003", LastAckedSnapshotPresent: true,
	}, SendRequest{TargetGeneration: 4})
	if err != nil {
		t.Fatalf("PlanSend: %v", err)
	}
	w := &recordingWriter{}
	if err := m.Pump(context.Background(), plan, w); err == nil {
		t.Fatal("Pump returned nil despite a non-zero zfs exit")
	}
	if w.String() != "zfs-stream-bytes" {
		t.Errorf("copied %q", w.String())
	}
	if stream.closed != 1 {
		t.Errorf("stream was closed %d times, want exactly 1", stream.closed)
	}
	// zfs(8) said something, so this is a failure, not silence.
	if got := ClassifySendError(errors.New("zfs send: warning: cannot send incremental")); got != SendFailed {
		t.Errorf("a zfs statement classified as %q, want %q", got, SendFailed)
	}
}

func TestPumpReportsSuccessOnlyWhenTheSendExitedZero(t *testing.T) {
	stream := &fakeSend{payload: "payload"}
	r := &fakeRunner{start: func(args []string) (Stream, error) { return stream, nil }}
	m := newFakeManager("zroot/apiary", r)
	plan, _ := PlanSend(SourceState{Dataset: "vm-1"}, SendRequest{TargetGeneration: 1})
	w := &recordingWriter{}
	if err := m.Pump(context.Background(), plan, w); err != nil {
		t.Fatalf("Pump: %v", err)
	}
	if stream.closed != 1 {
		t.Errorf("stream closed %d times, want 1", stream.closed)
	}
	if got := ClassifySendError(nil); got != SendSucceeded {
		t.Errorf("ClassifySendError(nil) = %q, want %q", got, SendSucceeded)
	}
}

func TestPumpBrokenTransportIsUnobservedNotFailed(t *testing.T) {
	// The peer link died mid-stream. zfs(8) may well have completed the
	// send perfectly; what did not complete is the delivery. Calling
	// that a zfs failure would tell the operator to go and look at
	// their pool when the fault is a network, and calling it success
	// would advance a generation the target never received.
	stream := &fakeSend{payload: "some-bytes-then-the-link-drops"}
	r := &fakeRunner{start: func(args []string) (Stream, error) { return stream, nil }}
	m := newFakeManager("zroot/apiary", r)
	plan, _ := PlanSend(SourceState{Dataset: "vm-1"}, SendRequest{TargetGeneration: 1})
	err := m.Pump(context.Background(), plan, &brokenWriter{remaining: 5})
	if err == nil {
		t.Fatal("Pump returned nil despite a broken transport")
	}
	if got := ClassifySendError(err); got != SendUnobserved {
		t.Errorf("a broken transport classified as %q, want %q", got, SendUnobserved)
	}
	if !IsUnobserved(err) {
		t.Errorf("err = %v, want an *UnobservedError", err)
	}
	if stream.closed != 1 {
		t.Errorf("stream closed %d times after a broken transport, want 1", stream.closed)
	}
}

func TestPumpCancelledContextIsUnobserved(t *testing.T) {
	// The child is killed along with the context, so its exit status
	// describes nothing about the dataset.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	stream := &constantStream{data: "partial", err: context.Canceled}
	r := &fakeRunner{start: func(args []string) (Stream, error) { return stream, nil }}
	m := newFakeManager("zroot/apiary", r)
	plan, _ := PlanSend(SourceState{Dataset: "vm-1"}, SendRequest{TargetGeneration: 1})
	err := m.Pump(ctx, plan, &recordingWriter{})
	if !IsUnobserved(err) {
		t.Fatalf("err = %v, want an *UnobservedError", err)
	}
	if !strings.Contains(err.Error(), "interrupted") {
		t.Errorf("error text does not say the stream was interrupted: %v", err)
	}
}

func TestPumpPropagatesAnOpenFailure(t *testing.T) {
	openErr := &UnobservedError{Op: "send -i a b", Detail: "zfs could not be executed: exec: \"zfs\": executable file not found in $PATH"}
	r := &fakeRunner{start: func(args []string) (Stream, error) { return nil, openErr }}
	m := newFakeManager("zroot/apiary", r)
	plan, _ := PlanSend(SourceState{Dataset: "vm-1"}, SendRequest{TargetGeneration: 1})
	if err := m.Pump(context.Background(), plan, &recordingWriter{}); !IsUnobserved(err) {
		t.Fatalf("err = %v, want the open failure passed through as unobserved", err)
	}
}

func TestClassifySendErrorHasExactlyThreeOutcomes(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want SendOutcome
	}{
		{"no error is success", nil, SendSucceeded},
		{"a zfs statement is a failure", errors.New("zfs send: invalid snapshot name"), SendFailed},
		{"silence is unobserved", &UnobservedError{Op: "send"}, SendUnobserved},
		{"a wrapped unobserved error stays unobserved", wrap(&UnobservedError{Op: "send"}), SendUnobserved},
		{"a zfs failure is not unobserved", errors.New("zfs receive: cannot receive incremental stream"), SendFailed},
	}
	for _, tc := range cases {
		if got := ClassifySendError(tc.err); got != tc.want {
			t.Errorf("%s: ClassifySendError() = %q, want %q", tc.name, got, tc.want)
		}
	}
	// The three values must be the only ones, and each must be
	// distinct: a vocabulary where two states share a spelling is a
	// vocabulary where the states get conflated.
	seen := map[SendOutcome]bool{}
	for _, o := range []SendOutcome{SendSucceeded, SendFailed, SendUnobserved} {
		if o == "" {
			t.Error("a SendOutcome is the empty string")
		}
		if seen[o] {
			t.Errorf("duplicate SendOutcome %q", o)
		}
		seen[o] = true
	}
}

func TestManagerFallsBackToTheRealRunner(t *testing.T) {
	// A Manager built as a bare struct literal is legal, since Base is
	// exported, and it must not panic on a nil runner.
	m := &Manager{Base: "zroot/apiary"}
	if m.runnerOrDefault() == nil {
		t.Error("runnerOrDefault() = nil")
	}
	if NewWithRunner("zroot/apiary", nil).runnerOrDefault() == nil {
		t.Error("NewWithRunner(base, nil) left a nil runner")
	}
	// A manager whose runner cannot stream stdin into a receive must
	// say so rather than handing zfs an unread stream.
	err := (&Manager{Base: "zroot/apiary", runner: queryOnlyRunner{}}).ReceiveInto(context.Background(), "vm-1", strings.NewReader("x"), ReceiveOptions{SkipResumeCheck: true})
	if err == nil {
		t.Fatal("ReceiveInto with a query-only runner returned no error")
	}
	if !strings.Contains(err.Error(), "cannot pipe stdin") {
		t.Errorf("err = %v, want it to name the missing capability", err)
	}
}

func TestIsUnobservedSeeksThroughWrapping(t *testing.T) {
	inner := &UnobservedError{Op: "get receive_resume_token", Detail: "no answer"}
	if !IsUnobserved(inner) {
		t.Error("IsUnobserved did not recognise an *UnobservedError")
	}
	if !IsUnobserved(wrap(inner)) {
		t.Error("IsUnobserved did not see through a wrap")
	}
	if IsUnobserved(errors.New("something else")) {
		t.Error("IsUnobserved matched an unrelated error")
	}
	if IsUnobserved(nil) {
		t.Error("IsUnobserved(nil) = true")
	}
	// A nil *UnobservedError must be safe: a caller holding a typed
	// nil must not panic classifying it.
	var nilU *UnobservedError
	if nilU.Error() == "" {
		t.Error("(*UnobservedError)(nil).Error() is empty")
	}
	if nilU.Unwrap() != nil {
		t.Error("(*UnobservedError)(nil).Unwrap() != nil")
	}
	var nilF *FenceError
	if !errors.Is(nilF, ErrFenceRefused) {
		t.Error("a nil *FenceError does not match ErrFenceRefused")
	}
	if nilF.Error() == "" {
		t.Error("(*FenceError)(nil).Error() is empty")
	}
	var nilR *ResumableDestError
	if !errors.Is(nilR, ErrDestResumable) {
		t.Error("a nil *ResumableDestError does not match ErrDestResumable")
	}
	var nilD *DestinationAheadError
	if !errors.Is(nilD, ErrDestinationAhead) {
		t.Error("a nil *DestinationAheadError does not match ErrDestinationAhead")
	}
	var zeroObs ResumeTokenObservation
	if zeroObs.Live() {
		t.Error("a zero ResumeTokenObservation reports a live token")
	}
	if zeroObs.Observed() {
		t.Error("a zero ResumeTokenObservation reports an observed state; a caller that never asked has not observed anything")
	}
}

func TestDetailOfStripsOnlyOurOwnPrefix(t *testing.T) {
	// detailOf feeds evidence text, so it must remove the "zfs <args>: "
	// wrapper this package adds and nothing else. A message we do not
	// recognise is passed through intact rather than truncated.
	cases := []struct {
		in   string
		want string
	}{
		// Only OUR prefix is stripped, and only up to the first colon:
		// zfs messages contain further colons and cutting at the last
		// one would reduce a whole sentence to its last word.
		{"zfs get -H -o value receive_resume_token pool/ds: cannot open 'pool/ds': dataset does not exist", "cannot open 'pool/ds': dataset does not exist"},
		{"zfs list: permission denied", "permission denied"},
		{"zfs send -i a b: cannot open 'b': dataset does not exist", "cannot open 'b': dataset does not exist"},
		{"zfs: bare prefix", "zfs: bare prefix"},
		{"zfs no colon at all", "zfs no colon at all"},
		{"cannot open 'pool/ds': dataset does not exist", "cannot open 'pool/ds': dataset does not exist"},
		{"something went wrong: with a colon", "something went wrong: with a colon"},
		{"plain", "plain"},
		{"", ""},
	}
	for _, tc := range cases {
		if got := detailOf(errors.New(tc.in)); got != tc.want {
			t.Errorf("detailOf(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
	if got := detailOf(nil); got != "" {
		t.Errorf("detailOf(nil) = %q, want empty", got)
	}
}

func TestUnobservedErrorReadsAsSilenceNotFailure(t *testing.T) {
	err := &UnobservedError{Op: "get -H -o value receive_resume_token pool/ds", Detail: "no usable answer", Err: io.EOF}
	msg := err.Error()
	if !strings.Contains(msg, "no usable answer") {
		t.Errorf("message loses the detail: %q", msg)
	}
	if !strings.Contains(msg, "get -H -o value") {
		t.Errorf("message loses the operation: %q", msg)
	}
	if !errors.Is(err, io.EOF) {
		t.Error("the cause is not reachable through errors.Is")
	}
	var u *UnobservedError
	if !errors.As(fmt.Errorf("context: %w", err), &u) {
		t.Error("errors.As does not see through a wrap")
	}
}

type wrapper struct{ err error }

func (w wrapper) Error() string { return "wrapped: " + w.err.Error() }
func (w wrapper) Unwrap() error { return w.err }

func wrap(err error) error { return wrapper{err: err} }
