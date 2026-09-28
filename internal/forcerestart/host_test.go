package forcerestart_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/glenjbarber/apiary/internal/forcerestart"
)

// The cases in forcerestart_test.go replace the Host entirely, which
// proves the restart loop but says nothing about the two things the
// loop delegates to it: that it runs the right commands, and that it
// reads their output correctly. Those are what this file covers, and it
// covers them against the production implementation with stand-in
// binaries on PATH rather than against a fake interface - so the exec,
// the argument list, the exit-status handling and the parsing are all
// exercised for real.
//
// No root, no Comb, no live daemon. The stand-ins write to a temp file
// and print canned sockstat output.

// stubs installs a directory of stand-in commands at the front of PATH
// and returns the log they all append to. Every stand-in records its
// own name and arguments before doing anything, so "which host command
// was actually run, with what" is answered by the artifact rather than
// by the code that was supposed to run it.
func stubs(t *testing.T, scripts map[string]string) (logFile string) {
	t.Helper()
	dir := t.TempDir()
	logFile = filepath.Join(dir, "calls.log")

	for name, body := range scripts {
		path := filepath.Join(dir, name)
		// $CALL_LOG is passed through the environment rather than
		// baked into the script, so the same stand-in text serves
		// every case without being rewritten.
		script := "#!/bin/sh\n" +
			"printf '%s %s\\n' \"" + name + "\" \"$*\" >> \"$CALL_LOG\"\n" +
			body
		if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
			t.Fatalf("writing stand-in %s: %v", name, err)
		}
	}
	t.Setenv("CALL_LOG", logFile)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return logFile
}

func calls(t *testing.T, logFile string) []string {
	t.Helper()
	body, err := os.ReadFile(logFile)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatalf("reading the call log: %v", err)
	}
	var out []string
	for _, l := range strings.Split(string(body), "\n") {
		if l != "" {
			out = append(out, l)
		}
	}
	return out
}

func TestHost_UsesTheRealCommandsAndNeverAsksForStatus(t *testing.T) {
	logFile := stubs(t, map[string]string{
		"service":  "exit 0\n",
		"sockstat": "exit 0\n",
	})
	h := forcerestart.NewHost()

	if err := h.RestartService("apiary_managerd"); err != nil {
		t.Fatalf("RestartService: %v", err)
	}
	if _, err := h.Listening(17700); err != nil {
		t.Fatalf("Listening: %v", err)
	}

	got := strings.Join(calls(t, logFile), "\n")
	want := "service apiary_managerd restart\nsockstat -4 -l"
	if got != want {
		t.Errorf("host commands issued:\n got %q\nwant %q", got, want)
	}
	// The old confirmation was `service <name> status`, and on a Comb
	// that answers "not running" for every apiary daemon that is up and
	// listening. The Host interface has no method that can ask for it;
	// this asserts the observable consequence as well, so a future
	// method added for some other reason cannot quietly reintroduce the
	// false measurement.
	if strings.Contains(got, "status") {
		t.Errorf("a service status call was issued: %q", got)
	}
	// -4 and -l are not decoration: without -l the output includes every
	// outbound connection on the host, and without -4 it includes IPv6
	// rows whose local addresses parse differently.
	if !strings.Contains(got, "sockstat -4 -l") {
		t.Errorf("sockstat was not run as -4 -l: %q", got)
	}
}

func TestHost_ListeningReadsRealListenerRows(t *testing.T) {
	for _, tc := range []struct {
		name   string
		output string
		want   bool
	}{
		{
			name: "both daemons up",
			output: `USER     COMMAND    PID   FD PROTO  LOCAL ADDRESS         FOREIGN ADDRESS
root     managerd   4242  7  tcp4   127.0.0.1:17700      *:*
root     raftd      4243  7  tcp4   127.0.0.1:17600      *:*
`,
			want: true,
		},
		{
			name:   "only raftd up, so managerd's port is not",
			output: "root     raftd      4243  7  tcp4   127.0.0.1:17600      *:*\n",
			want:   false,
		},
		{
			name:   "a decoy row with the delimiter dropped",
			output: "root     odd       4244  4  tcp4   127.0.0.1.17700     *:*\n",
			want:   false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stubs(t, map[string]string{
				"sockstat": "cat <<'ROWS'\n" + tc.output + "ROWS\nexit 0\n",
			})
			got, err := forcerestart.NewHost().Listening(17700)
			if err != nil {
				t.Fatalf("Listening: %v", err)
			}
			if got != tc.want {
				t.Errorf("Listening(17700) = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestHost_ABrokenSockstatIsAnErrorNotAnAnswer(t *testing.T) {
	// The distinction that decides whether a failure is visible. If a
	// sockstat that cannot be run reported "not listening" as a plain
	// false, the run would time out on every service and look like a
	// daemons-are-broken incident; if it reported "listening", it would
	// confirm a restart it never observed. Both are wrong, and the
	// caller needs the third answer.
	stubs(t, map[string]string{
		"sockstat": "echo 'sockstat: /dev/console: Permission denied' >&2\nexit 1\n",
	})
	up, err := forcerestart.NewHost().Listening(17700)
	if err == nil {
		t.Fatalf("a failing sockstat returned no error, so a broken measurement is indistinguishable from a negative one")
	}
	if up {
		t.Error("a failing sockstat reported the port listening")
	}
	if !strings.Contains(err.Error(), "sockstat") {
		t.Errorf("the error does not name the command that failed: %v", err)
	}
}

func TestHost_AMissingSockstatIsAnError(t *testing.T) {
	// An empty PATH stand-in directory: LookPath fails outright, which
	// is a different failure from sockstat running and exiting
	// non-zero, and both have to stay errors.
	t.Setenv("PATH", t.TempDir())
	if _, err := forcerestart.NewHost().Listening(17700); err == nil {
		t.Fatal("a sockstat that does not exist returned no error")
	}
}

func TestHost_ARestartThatFailsSurfacesItsOutput(t *testing.T) {
	// service(8) writes the reason to stdout and stderr together and
	// exits non-zero. The operator standing in front of the Comb needs
	// that text: "could not start apiary_raftd: /var/log/apiary/raftd.log
	// is not writable" is the whole diagnosis, and discarding it would
	// leave them with an exit status and nothing else.
	stubs(t, map[string]string{
		"service": "echo 'apiary_raftd: /var/log/apiary/raftd.log is not writable' >&2\nexit 1\n",
	})
	err := forcerestart.NewHost().RestartService("apiary_raftd")
	if err == nil {
		t.Fatal("a failing service(8) returned no error")
	}
	for _, want := range []string{
		"apiary_raftd",
		"restart",
		"/var/log/apiary/raftd.log is not writable",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error is missing %q: %v", want, err)
		}
	}
}
