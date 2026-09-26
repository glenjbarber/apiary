package jail

import (
	"context"
	"fmt"
	"strings"
	"sync"
)

// fakeRunner is a scripted CommandRunner: every call is recorded, and
// the reply is chosen by a per-test handler keyed on the full command
// line. It exists so this package's FreeBSD orchestration can be
// exercised on any host - the same trick internal/cluster's own fake
// managers use.
//
// An unscripted command is an error rather than empty output: a test
// that forgets to script a command must fail loudly instead of passing
// because the un-scripted call happened to return nothing, which would
// read as "this interface has no addresses" to the code under test.
type fakeRunner struct {
	mu      sync.Mutex
	handles map[string]func(ctx context.Context, args []string) (string, error)
	calls   []string
}

func newFakeRunner() *fakeRunner {
	return &fakeRunner{handles: make(map[string]func(ctx context.Context, args []string) (string, error))}
}

// on scripts name+args (joined by spaces) to return out with no error.
func (f *fakeRunner) on(command, out string) *fakeRunner {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.handles[command] = func(ctx context.Context, args []string) (string, error) {
		return out, nil
	}
	return f
}

// onErr scripts name+args (joined by spaces) to fail with an error
// carrying exactly msg, the way this package's execCommandRunner would
// wrap a tool's own stderr.
func (f *fakeRunner) onErr(command, msg string) *fakeRunner {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.handles[command] = func(ctx context.Context, args []string) (string, error) {
		return "", fmt.Errorf("%s: %s", command, msg)
	}
	return f
}

func (f *fakeRunner) Run(ctx context.Context, name string, args ...string) (string, error) {
	command := strings.TrimSpace(name + " " + strings.Join(args, " "))
	f.mu.Lock()
	f.calls = append(f.calls, command)
	handle, ok := f.handles[command]
	f.mu.Unlock()
	if !ok {
		return "", fmt.Errorf("fakeRunner: unexpected command %q", command)
	}
	return handle(ctx, args)
}

// callsTo returns the recorded invocations of exactly command.
func (f *fakeRunner) callsTo(command string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if c == command {
			n++
		}
	}
	return n
}

// count returns how many commands were run in total.
func (f *fakeRunner) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}
