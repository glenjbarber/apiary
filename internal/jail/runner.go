package jail

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

// CommandRunner runs one external command (jail(8), jls(8), jexec(8),
// ifconfig(8), ...) and returns its trimmed stdout. It exists so this
// package's FreeBSD command execution is injectable: every method below
// is pure orchestration around it, and every one of them is therefore
// unit-testable on any development host (macOS included) with a fake
// runner, instead of requiring root on a real FreeBSD box - the same
// reasoning internal/deadman's atRunner and internal/cluster's own
// per-dependency manager interfaces use.
//
// An error's text is the single channel through which "the thing I asked
// about is not there" is signalled, since every FreeBSD tool here
// (jls, ifconfig, netstat) reports an absent object by writing to
// stderr and exiting non-zero rather than by a structured code. Every
// caller that cares about that distinction matches on a specific
// substring scoped to the tool that produced it (see
// absentObjectMarkers) rather than treating any error as "absent" - an
// unobservable fact is unknown, never false.
type CommandRunner interface {
	// Run executes name with args and returns trimmed stdout. On
	// failure the returned error should include the command's own
	// stderr, since that is where FreeBSD's not-found wording lives.
	Run(ctx context.Context, name string, args ...string) (string, error)
}

// execCommandRunner is the production CommandRunner: it shells out for
// real, exactly as this package has always done.
type execCommandRunner struct{}

func (execCommandRunner) Run(ctx context.Context, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return "", fmt.Errorf("%s %s: %s", name, strings.Join(args, " "), msg)
	}
	return strings.TrimSpace(stdout.String()), nil
}

// absentObjectMarkers maps each tool this package drives to the
// substrings *that tool itself* writes to its own stderr to say "the
// object you named does not exist":
//
//   - ifconfig(8): "does not exist" for a missing interface (the same
//     marker internal/vlan's own ifaceExists already matches on).
//   - jls(8): "not found" for a missing jail.
//   - jexec(8): "not found" for a missing or non-running jail.
//
// Keyed per tool rather than kept as one loose list so that a message
// one tool happens to use for something else can never be read as
// another tool's definite absent-object answer. A failure that matches
// no tool's marker is not absent, it is unknown.
//
// Known limitation, recorded rather than papered over: "not found" is
// jls/jexec's own wording and is not anchored to the object name, so a
// hypothetical future tool whose stderr carries the same phrase and is
// routed through jexec would be misread as an absent jail. This is
// strictly narrower than the pre-ADR-0117 behaviour, which matched
// that substring against every error this package produced; and both
// call sites treat a false positive as a loud, actionable failure
// (JailExists reporting absent makes jail(8) itself refuse the
// resulting create, and ObserveNet reporting an absent interface
// makes EnsureAddressing demand a restart) rather than as silence.
var absentObjectMarkers = map[string][]string{
	"ifconfig": {"does not exist"},
	"jls":      {"not found"},
	"jexec":    {"not found"},
}

// ErrJailNotFound is wrapped by any error that reports a *definite*
// "jail(8) has no such jail" answer - as opposed to a failure to find
// out. Callers that need to tell those two apart (the VNET reconciler
// deciding between "restart this jail" and "could not check") match it
// with errors.Is. Every other failure is unknown.
var ErrJailNotFound = errors.New("jail not found")

// shellNames are the shells whose own "the command isn't installed"
// wording has to be recognized so it is never mistaken for the tool
// below it reporting an absent object.
var shellNames = []string{"sh", "bash", "zsh", "ksh", "dash", "csh", "tcsh"}

// commandMissing reports whether err is the *binary* failing to run,
// rather than the binary running and reporting something about the
// system. This distinction is load-bearing: Go's own exec reports a
// missing binary as `exec: "jls": executable file not found in $PATH`
// and a shell would say `sh: jls: not found` - and a naive substring
// match on "not found" would read that as "jls ran and told me the
// jail does not exist", turning a broken node into a confident
// "this jail is gone" that Apiary would then act on.
func commandMissing(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	if strings.Contains(msg, "command not found") || strings.Contains(msg, "executable file not found") {
		return true
	}
	// A shell prompt anywhere in the message means this text did not
	// come from the tool this package asked to run, so it cannot be that
	// tool's absent-object answer - whatever command it happens to be
	// complaining about. The tool name is deliberately not matched
	// against: `sh: ifconfig: not found` is just as fatal to the
	// question being asked as `sh: jls: not found` is.
	for _, shell := range shellNames {
		for _, prefix := range []string{shell + ": ", "/bin/" + shell + ": "} {
			if strings.Contains(msg, prefix) {
				return true
			}
		}
	}
	return false
}

// notFound reports whether err is a definite "this does not exist"
// answer from tool, rather than a failure to find out. Callers must
// treat a false result as unknown, never as "absent" - and in
// particular must not treat a false *positive* as one either, which is
// why commandMissing is checked first and separately.
func notFound(err error, tool string) bool {
	if err == nil || commandMissing(err) {
		return false
	}
	msg := err.Error()
	for _, marker := range absentObjectMarkers[tool] {
		if strings.Contains(msg, marker) {
			return true
		}
	}
	return false
}

// run executes name with args through m.Runner, or through the real
// shell if m.Runner is nil.
func (m *Manager) run(ctx context.Context, name string, args ...string) (string, error) {
	if m.Runner == nil {
		return execCommandRunner{}.Run(ctx, name, args...)
	}
	return m.Runner.Run(ctx, name, args...)
}
