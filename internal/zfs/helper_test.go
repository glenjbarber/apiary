package zfs

// Test-only doubles for the exec seam. Everything in the replication
// layer goes through CommandRunner, so these make the whole package —
// argument construction, the generation fence, the resume-token state
// machine, and the verdict classification — exercisable on a machine
// with no zfs(8) and no pool, which is the only place macOS can
// honestly test any of it.
//
// The fake records every invocation it was asked to make and never
// spawns anything, so a test can assert the exact argument vector AND
// assert that a refused operation spawned no command at all — the
// second assertion is the one that proves a refusal is a refusal and
// not a warning that was then ignored.

import (
	"context"
	"fmt"
	"io"
	"strings"
	"sync"
)

// fakeRunner is a scripted CommandRunner. Any of run/start/recv may be
// nil, in which case that operation returns an explicit "the test did
// not script this" error rather than silently succeeding, so a test
// that forgets to script something fails loudly instead of proving
// nothing.
type fakeRunner struct {
	mu         sync.Mutex
	runCalls   [][]string
	startCalls [][]string
	recvCalls  [][]string
	recvStdin  []string

	run   func(args []string) (string, error)
	start func(args []string) (Stream, error)
	recv  func(args []string, stdin io.Reader) error
}

func (f *fakeRunner) Run(ctx context.Context, args ...string) (string, error) {
	f.mu.Lock()
	f.runCalls = append(f.runCalls, append([]string(nil), args...))
	fn := f.run
	f.mu.Unlock()
	if fn == nil {
		return "", fmt.Errorf("fakeRunner: Run(%s) was not scripted", strings.Join(args, " "))
	}
	return fn(args)
}

func (f *fakeRunner) Start(ctx context.Context, args ...string) (Stream, error) {
	f.mu.Lock()
	f.startCalls = append(f.startCalls, append([]string(nil), args...))
	fn := f.start
	f.mu.Unlock()
	if fn == nil {
		return nil, fmt.Errorf("fakeRunner: Start(%s) was not scripted", strings.Join(args, " "))
	}
	return fn(args)
}

func (f *fakeRunner) receive(ctx context.Context, args []string, stdin io.Reader) error {
	f.mu.Lock()
	f.recvCalls = append(f.recvCalls, append([]string(nil), args...))
	f.recvStdin = append(f.recvStdin, readAllString(stdin))
	fn := f.recv
	f.mu.Unlock()
	if fn == nil {
		return fmt.Errorf("fakeRunner: receive(%s) was not scripted", strings.Join(args, " "))
	}
	return fn(args, stdin)
}

// sent returns every recorded start() argument vector, joined, for
// substring and full-vector assertions.
func (f *fakeRunner) sent() [][]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([][]string, len(f.startCalls))
	copy(out, f.startCalls)
	return out
}

func (f *fakeRunner) ran() [][]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([][]string, len(f.runCalls))
	copy(out, f.runCalls)
	return out
}

func (f *fakeRunner) received() [][]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([][]string, len(f.recvCalls))
	copy(out, f.recvCalls)
	return out
}

func (f *fakeRunner) receivedStdin() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.recvStdin...)
}

// fakeSend is a Stream whose payload, read behaviour, and Close result
// are all scripted independently, because those three are the three
// things a real `zfs send` can do to a replication run.
type fakeSend struct {
	payload   string
	readErr   error
	closeErr  error
	closed    int
	readBytes int
}

func (s *fakeSend) Read(p []byte) (int, error) {
	if s.payload == "" {
		if s.readErr != nil {
			return 0, s.readErr
		}
		return 0, io.EOF
	}
	n := copy(p, s.payload)
	s.payload = s.payload[n:]
	s.readBytes += n
	return n, nil
}

func (s *fakeSend) Close() error {
	s.closed++
	return s.closeErr
}

// constantStream yields payload once and then a fixed error, which is
// how a send cut off mid-stream looks to the reader: some bytes, then
// no more, and no judgement from zfs about why.
type constantStream struct {
	data string
	err  error
	off  int
}

func (s *constantStream) Read(p []byte) (int, error) {
	if s.off < len(s.data) {
		n := copy(p, s.data[s.off:])
		s.off += n
		return n, nil
	}
	if s.err != nil {
		return 0, s.err
	}
	return 0, io.EOF
}

func (s *constantStream) Close() error { return nil }

// recordingWriter is the transport Pump copies a stream into.
type recordingWriter struct {
	mu  sync.Mutex
	buf strings.Builder
	err error
}

func (w *recordingWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.err != nil {
		return 0, w.err
	}
	return w.buf.Write(p)
}

func (w *recordingWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.String()
}

// brokenWriter is a transport that dies after n bytes, i.e. the peer
// link dropping mid-stream. It is the case that must never be reported
// as a zfs failure.
type brokenWriter struct {
	remaining int
}

func (w *brokenWriter) Write(p []byte) (int, error) {
	if w.remaining <= 0 {
		return 0, io.ErrUnexpectedEOF
	}
	if len(p) > w.remaining {
		n := w.remaining
		w.remaining = 0
		return n, io.ErrUnexpectedEOF
	}
	w.remaining -= len(p)
	return len(p), nil
}

// newFakeManager returns a Manager over a scripted runner.
func newFakeManager(base string, r *fakeRunner) *Manager { return NewWithRunner(base, r) }

// queryOnlyRunner implements CommandRunner but deliberately NOT the
// unexported receiveStarter, so ReceiveInto's "this runner cannot pipe
// stdin" path can be exercised. The interface is split in two
// precisely so a test can hold a runner that cannot do the one thing
// it is not asked to do.
type queryOnlyRunner struct{}

func (queryOnlyRunner) Run(ctx context.Context, args ...string) (string, error) { return "", nil }
func (queryOnlyRunner) Start(ctx context.Context, args ...string) (Stream, error) {
	return nil, fmt.Errorf("queryOnlyRunner cannot start")
}

// zfsErr builds the error shape execRunner produces when zfs(8) ran
// and said no, so tests exercise the same text the real runner yields.
func zfsErr(args ...string) error {
	return fmt.Errorf("zfs %s: cannot open '%s': dataset does not exist", strings.Join(args, " "), args[len(args)-1])
}

// equalArgs is a readable assertion helper for one recorded argument
// vector.
func equalArgs(got []string, want ...string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

func readAllString(r io.Reader) string {
	if r == nil {
		return ""
	}
	b, err := io.ReadAll(r)
	if err != nil {
		return string(b) + "<read error: " + err.Error() + ">"
	}
	return string(b)
}
