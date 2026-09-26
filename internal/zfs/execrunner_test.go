package zfs

// The exec seam, exercised against a real subprocess.
//
// Everything else in this package's tests substitutes a CommandRunner.
// That is the right way to test decisions, but it proves nothing about
// the one place a real exit status, a real stderr, and a real killed
// child are turned into one of the three states. So these tests put a
// script named `zfs` at the front of PATH and run the production
// execRunner against it.
//
// No ZFS, no pool, no dataset: the script never touches storage. What
// is under test is the classification of the process outcome, which is
// the one thing a fake runner cannot falsify.

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// fakeZFS puts an executable named `zfs` at the front of PATH for the
// duration of the test and returns its directory. script is the shell
// body; it receives the zfs arguments as "$@".
func fakeZFS(t *testing.T, script string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the fake zfs is a POSIX shell script")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "zfs")
	body := "#!/bin/sh\n" + script + "\n"
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatalf("writing fake zfs: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return dir
}

func TestExecRunnerRunSurfacesZfsStderrAsAFailure(t *testing.T) {
	// A non-zero exit carrying zfs(8)'s own message is a statement
	// about the dataset. It must be a plain, diagnosable failure — and
	// emphatically NOT unobserved, because a caller treating it as
	// silence would go and look for a network fault that does not
	// exist.
	fakeZFS(t, `echo "cannot open 'zroot/apiary/vm-1': dataset does not exist" >&2
exit 1`)
	_, err := (execRunner{}).Run(context.Background(), "list", "-H", "-o", "name", "zroot/apiary/vm-1")
	if err == nil {
		t.Fatal("Run returned no error for a non-zero exit")
	}
	if IsUnobserved(err) {
		t.Fatalf("a zfs(8) statement was classified unobserved: %v", err)
	}
	if !strings.Contains(err.Error(), "dataset does not exist") {
		t.Errorf("the zfs message was lost: %v", err)
	}
	if !strings.Contains(err.Error(), "zfs list -H -o name zroot/apiary/vm-1") {
		t.Errorf("the command line was lost: %v", err)
	}
	if got := ClassifySendError(err); got != SendFailed {
		t.Errorf("ClassifySendError = %q, want %q", got, SendFailed)
	}
}

func TestExecRunnerRunSucceedsAndTrimsStdout(t *testing.T) {
	fakeZFS(t, `echo "  zroot/apiary/vm-1@apiary-repl-00000007  "`)
	out, err := (execRunner{}).Run(context.Background(), "list")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out != "zroot/apiary/vm-1@apiary-repl-00000007" {
		t.Errorf("stdout = %q, want it trimmed", out)
	}
}

func TestExecRunnerMissingBinaryIsUnobserved(t *testing.T) {
	// No zfs at all. That is silence, and this is the single most
	// important classification in the file: a node that cannot run the
	// binary has not got a broken dataset.
	t.Setenv("PATH", t.TempDir())
	_, err := (execRunner{}).Run(context.Background(), "list")
	if !IsUnobserved(err) {
		t.Fatalf("err = %v, want an *UnobservedError for a missing binary", err)
	}
	if !strings.Contains(err.Error(), "could not be executed") {
		t.Errorf("err = %v, want it to say the binary could not be run", err)
	}
	if got := ClassifySendError(err); got != SendUnobserved {
		t.Errorf("ClassifySendError = %q, want %q", got, SendUnobserved)
	}
	// Start must agree with Run about the same fact.
	if _, err := (execRunner{}).Start(context.Background(), "send", "x"); !IsUnobserved(err) {
		t.Errorf("Start with a missing binary = %v, want an *UnobservedError", err)
	}
	// And so must receive.
	err = (execRunner{}).receive(context.Background(), []string{"receive", "x"}, strings.NewReader(""))
	if !IsUnobserved(err) {
		t.Errorf("receive with a missing binary = %v, want an *UnobservedError", err)
	}
}

func TestExecSendStreamCloseSurfacesZfsStderr(t *testing.T) {
	// The real send contract: read live, and on Close learn whether the
	// child exited zero. A non-zero exit with a message is a failure.
	fakeZFS(t, `echo "warning: cannot send 'x': snapshot not found" >&2
exit 1`)
	stream, err := (execRunner{}).Start(context.Background(), "send", "-i", "a", "b")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	// Draining first, as a real pusher does.
	buf := make([]byte, 64)
	stream.Read(buf)
	closeErr := stream.Close()
	if closeErr == nil {
		t.Fatal("Close returned nil for a non-zero exit; a failed send would be reported as a successful one")
	}
	if IsUnobserved(closeErr) {
		t.Fatalf("a zfs(8) statement was classified unobserved: %v", closeErr)
	}
	if !strings.Contains(closeErr.Error(), "snapshot not found") {
		t.Errorf("Close lost zfs's own message: %v", closeErr)
	}
}

func TestExecSendStreamCloseSuccess(t *testing.T) {
	// The positive case: a payload arrives, the child exits zero, and
	// Close says nothing. Only then is a send reportable as
	// successful.
	fakeZFS(t, `printf 'stream-bytes'
exit 0`)
	stream, err := (execRunner{}).Start(context.Background(), "send", "x")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	var got strings.Builder
	buf := make([]byte, 64)
	for {
		n, err := stream.Read(buf)
		got.Write(buf[:n])
		if err != nil {
			break
		}
	}
	if err := stream.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if got.String() != "stream-bytes" {
		t.Errorf("read %q", got.String())
	}
}

func TestExecSendStreamKilledChildIsUnobservedNotAStatement(t *testing.T) {
	// The important one. A send killed by a cancelled context exits
	// non-zero with an EMPTY stderr. Read naively that is
	// "zfs send: signal: killed" — a fabricated diagnosis, reported as
	// a zfs failure, which would send an operator to their pool when
	// the run was merely cut off. It is silence, and it is silence that
	// must leave the generation un-acked without pretending to know
	// why.
	fakeZFS(t, `sleep 30
exit 0`)
	ctx, cancel := context.WithCancel(context.Background())
	stream, err := (execRunner{}).Start(ctx, "send", "x")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	time.Sleep(50 * time.Millisecond)
	cancel()
	// Drain whatever is buffered so the child can be reaped.
	buf := make([]byte, 64)
	for i := 0; i < 100; i++ {
		if _, err := stream.Read(buf); err != nil {
			break
		}
	}
	closeErr := stream.Close()
	if closeErr == nil {
		t.Fatal("Close returned nil for a killed child")
	}
	if !IsUnobserved(closeErr) {
		t.Fatalf("a killed child was classified as a zfs statement: %v", closeErr)
	}
	if got := ClassifySendError(closeErr); got != SendUnobserved {
		t.Errorf("ClassifySendError = %q, want %q", got, SendUnobserved)
	}
	if strings.Contains(closeErr.Error(), "signal: killed") {
		t.Errorf("the fabricated errno leaked into the message: %v", closeErr)
	}
}

func TestExecRunnerReceivePipesStdinAndReportsRejection(t *testing.T) {
	// The real receive: stdin is the stream, a non-zero exit with a
	// message is a diagnosable failure, and the error names both the
	// command and zfs's own reason.
	dir := fakeZFS(t, `cat > "$0.stdin"
cat >&2 <<'EOF'
cannot receive incremental stream: destination has snapshots (eg. apiary-repl-00000007) must destroy them to overwrite it
EOF
exit 1`)
	script := filepath.Join(dir, "zfs")
	err := (execRunner{}).receive(context.Background(), []string{"receive", "-u", "zroot/apiary/vm-1"}, strings.NewReader("the-stream"))
	if err == nil {
		t.Fatal("receive returned nil for a rejected stream")
	}
	if IsUnobserved(err) {
		t.Fatalf("a zfs(8) rejection was classified unobserved: %v", err)
	}
	if !strings.Contains(err.Error(), "must destroy them to overwrite it") {
		t.Errorf("the rejection lost zfs's own reason: %v", err)
	}
	if !strings.Contains(err.Error(), "zroot/apiary/vm-1") {
		t.Errorf("the rejection lost the destination: %v", err)
	}
	// The stream really did reach the child.
	data, readErr := os.ReadFile(script + ".stdin")
	if readErr != nil {
		t.Fatalf("reading captured stdin: %v", readErr)
	}
	if string(data) != "the-stream" {
		t.Errorf("child stdin = %q, want the stream verbatim", data)
	}
}

func TestExecRunnerReceiveKilledMidStreamIsUnobserved(t *testing.T) {
	// A receive cut off mid-stream leaves a RESUMABLE destination. If
	// that were reported as a clean failure, a retrying caller would
	// reasonably reach for -F and destroy the half-received data. It is
	// silence, and the resume token is the way forward.
	// `exec sleep` so the process this package kills IS the one holding
	// the command open: no shell wrapper, no grandchild, and a child
	// that is provably still running when the context is cancelled.
	// Nothing reads stdin here, which is fine — the 8 KiB below fit in
	// the pipe buffer, so the copy finishes and only the kill matters.
	fakeZFS(t, `exec sleep 30`)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- (execRunner{}).receive(ctx, []string{"receive", "zroot/apiary/vm-1"}, strings.NewReader(strings.Repeat("x", 4096)))
	}()
	time.Sleep(100 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if !IsUnobserved(err) {
			t.Fatalf("a cancelled receive = %v, want an *UnobservedError so the destination is left resumable", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("receive did not return after its context was cancelled")
	}
}

func TestExecRunnerReceiveCannotHangOnAProcessThatHoldsThePipe(t *testing.T) {
	// A killed child that leaves a grandchild holding the stdin pipe
	// would otherwise keep the wait open forever, which is the worst
	// shape this package can have: a receive that never returns holds
	// its own slot with no verdict to show. The wait is bounded, and the
	// bound is classified like every other process outcome.
	old := streamWaitDelay
	streamWaitDelay = 250 * time.Millisecond
	t.Cleanup(func() { streamWaitDelay = old })

	fakeZFS(t, `(cat > /dev/null &)
sleep 30
exit 0`)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- (execRunner{}).receive(ctx, []string{"receive", "zroot/apiary/vm-1"}, strings.NewReader(strings.Repeat("x", 8192)))
	}()
	time.Sleep(150 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("receive returned nil for a child killed with its stdin still held open")
		}
		if !IsUnobserved(err) {
			t.Fatalf("err = %v, want an *UnobservedError", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the bounded wait did not take effect")
	}
}

func TestCommandFailureClassifiesEveryProcessOutcome(t *testing.T) {
	// The decision, table-driven, with no subprocess: the inputs are
	// exactly the three shapes exec produces.
	live := context.Background()
	done, cancel := context.WithCancel(context.Background())
	cancel()

	cases := []struct {
		name      string
		ctx       context.Context
		stderr    string
		err       error
		unobserve bool
	}{
		{"a zfs statement", live, "dataset does not exist", os.ErrProcessDone, false},
		{"a zfs statement with no message", live, "", os.ErrProcessDone, false},
		{"a killed child, context cancelled", done, "", os.ErrProcessDone, true},
		{"a killed child, context alive", live, "", os.ErrProcessDone, false},
		{"a binary that could not be run", live, "", &exec.Error{Name: "zfs", Err: os.ErrNotExist}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := commandFailure(tc.ctx, []string{"list", "pool/ds"}, tc.stderr, tc.err)
			if got := IsUnobserved(err); got != tc.unobserve {
				t.Errorf("IsUnobserved = %v, want %v (err = %v)", got, tc.unobserve, err)
			}
			if tc.unobserve && ClassifySendError(err) != SendUnobserved {
				t.Errorf("ClassifySendError = %q, want %q", ClassifySendError(err), SendUnobserved)
			}
			if !tc.unobserve && ClassifySendError(err) != SendFailed {
				t.Errorf("ClassifySendError = %q, want %q", ClassifySendError(err), SendFailed)
			}
		})
	}
	// A nil context must not panic: it is not something the production
	// callers do, but a decision function that panics on a nil argument
	// is a decision function that can crash a status path.
	if err := commandFailure(nil, []string{"list"}, "boom", os.ErrProcessDone); err == nil {
		t.Error("commandFailure with a nil context and a statement returned no error")
	}
}

func TestDefaultRunnerIsTheRealOne(t *testing.T) {
	// Production callers use New, and New must use the real binary.
	// Getting this wrong would make every replication run in a
	// production Manager a no-op.
	if _, ok := defaultRunner().(execRunner); !ok {
		t.Errorf("defaultRunner() = %T, want execRunner", defaultRunner())
	}
	m := New("zroot/apiary")
	if _, ok := m.runner.(execRunner); !ok {
		t.Errorf("New() installed %T, want execRunner", m.runner)
	}
}
