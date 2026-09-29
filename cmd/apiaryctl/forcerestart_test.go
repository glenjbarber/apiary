package main

import (
	"os/exec"
	"strings"
	"testing"
)

// The refusal is implemented and tested in internal/forcerestart, where
// the restart loop and both preflights are executed for real. What is
// left untested by that is the one surface an operator reads BEFORE
// running the command, which is the only chance the command has to stop
// them on a follower-Comb at 2am.
//
// That is a text assertion, and this repository distrusts text
// assertions for anything behavioural - a checker that grepped
// sockstat output for ":17700" would have passed a broken parser. The
// distrust is about BEHAVIOUR, and this is not behaviour: there is no
// code path here that can be executed to find out what an operator
// would be told, because the text is the deliverable. The behaviour it
// describes is pinned by the cases in internal/forcerestart, and these
// are here so that the description cannot quietly stop matching the
// thing it describes.
//
// The cases build and run the real binary rather than matching against
// the usage constant, so they cover the dispatch too: a subcommand
// that stopped being wired into main() would fail here even though the
// string it prints is still present in the source.

// buildAPIaryctl builds the package under test into a temp directory
// and returns the path to the binary.
func buildAPIaryctl(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	bin := dir + "/apiaryctl"
	cmd := exec.Command("go", "build", "-o", bin, ".")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("building apiaryctl: %v\n%s", err, out)
	}
	return bin
}

func apiaryctlOutput(t *testing.T, args ...string) string {
	t.Helper()
	out, err := exec.Command(buildAPIaryctl(t), args...).CombinedOutput()
	if err != nil {
		if _, ok := err.(*exec.ExitError); !ok {
			t.Fatalf("running apiaryctl %v: %v\n%s", args, err, out)
		}
		// A non-zero exit is expected for most of these: help goes to
		// stdout and exits 0, no-subcommand prints usage to stderr and
		// exits 2, and the root-only refusal exits 1. The text is what
		// is under test, so the status is not.
	}
	return string(out)
}

// `apiaryctl help` is the thing an operator types to find out what the
// command does, and it is the one place that can say "it refuses on the
// leader" before anyone is on a Comb with a problem.
func TestUsage_SaysForceRestartRefusesOnTheLeader(t *testing.T) {
	out := apiaryctlOutput(t, "help")
	for _, want := range []string{
		"apiaryctl force-restart",
		"Refuses to run on the Colony's current",
		"leadership it",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("`apiaryctl help` does not mention %q\n---\n%s---", want, out)
		}
	}
}

// The same text is on the no-subcommand path, which is what an operator
// who typed `apiaryctl` and pressed return sees. It is a separate exit
// and a separate stream, so it is a separate case: a usage string
// printed on one path and not the other is a plausible edit, and the
// one that matters is the one nobody typed a subcommand for.
func TestUsage_BareInvocationExplainsTheLeaderRefusal(t *testing.T) {
	out := apiaryctlOutput(t)
	for _, want := range []string{
		"apiaryctl force-restart",
		"Refuses to run on the Colony's current",
		// Not "an unanswered question": this string is hard-wrapped in
		// the source and a match spanning the wrap is a test that fails
		// on a rewrap, which is a refactor being reported as a defect.
		"unanswered question is not a permission",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("`apiaryctl` with no subcommand does not mention %q\n---\n%s---", want, out)
		}
	}
}

// The command is still dispatched, and it still takes no arguments.
// A subcommand that stopped being reachable is invisible to every test
// in internal/forcerestart, which tests the package rather than the
// wiring, and the refusal would then exist only for callers nobody has.
func TestForceRestart_IsDispatchedAndTakesNoArguments(t *testing.T) {
	bin := buildAPIaryctl(t)

	// An argument is a usage error, reported by the flag parser's own
	// subcommand rather than by main()'s switch, so this is the check
	// that the subcommand is wired in at all.
	out, err := exec.Command(bin, "force-restart", "comb-two").CombinedOutput()
	if err == nil {
		t.Fatalf("`apiaryctl force-restart comb-two` exited 0; it takes no arguments\n%s", out)
	}
	if !strings.Contains(string(out), "takes no arguments") {
		t.Errorf("`apiaryctl force-restart comb-two` did not report the argument error\n---\n%s---", out)
	}
}
