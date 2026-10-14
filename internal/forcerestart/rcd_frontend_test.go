package forcerestart_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// This project's test suite cannot literally exercise a FreeBSD rc.d
// script's restart behaviour on a Linux dev machine or in CI - there is
// no rc.subr, no daemon(8), no sockstat(1) to run it against. What can
// be tested from here, in the same spirit as the Makefile-text checks
// above, is that the script's own source still contains the fix for
// issue #76 (a restart racing an orphaned, untracked listener on
// frontend's port) and that the script is still syntactically valid
// POSIX sh, which is the one class of regression a plain text edit to
// a script nobody runs locally can introduce silently.
func readRCFrontend(t *testing.T) string {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(repoRoot(t), "etc", "rc.d", "apiary_frontend"))
	if err != nil {
		t.Fatalf("reading etc/rc.d/apiary_frontend: %v", err)
	}
	return string(body)
}

// TestRCFrontend_DetectsOrphanedPortBeforeStart pins issue #76's fix:
// `service apiary_frontend restart` used to find no pidfile-tracked
// process, declare the service not running, and then race straight
// into `daemon(8)` against a port a previous, untracked frontend
// process was still listening on - losing with "address already in
// use" because that old process was itself ignoring SIGTERM. The fix
// is a start_precmd hook that runs in exactly the gap rc.subr leaves
// open (no tracked process found) and checks the configured port
// directly with sockstat(1) before handing off to daemon(8).
func TestRCFrontend_DetectsOrphanedPortBeforeStart(t *testing.T) {
	body := readRCFrontend(t)

	if !strings.Contains(body, `start_precmd="apiary_frontend_orphanport"`) {
		t.Fatal("etc/rc.d/apiary_frontend no longer wires a start_precmd hook for orphaned-port " +
			"detection (issue #76) - a restart can go back to racing an untracked listener on " +
			"the configured port for the port instead of stopping or naming it")
	}
	if !strings.Contains(body, "apiary_frontend_orphanport()") {
		t.Fatal("etc/rc.d/apiary_frontend defines start_precmd=\"apiary_frontend_orphanport\" but not " +
			"the function itself")
	}
	if !strings.Contains(body, "sockstat") {
		t.Error("etc/rc.d/apiary_frontend's orphan-port check no longer uses sockstat(1) to find " +
			"what, if anything, is listening on the configured port")
	}
	if !strings.Contains(body, "kill -TERM") {
		t.Error("etc/rc.d/apiary_frontend's orphan-port check no longer sends SIGTERM to an " +
			"untracked process holding the port before giving up on it")
	}
	// The failure path has to name the PID, per issue #76's expected
	// behaviour ("fails clearly and names it") - a bare "address already
	// in use" from daemon(8) is exactly what operators were already
	// seeing and is not an improvement on its own.
	if !strings.Contains(body, "err 1 ") || !strings.Contains(body, "pid(s) ${pids}") {
		t.Error("etc/rc.d/apiary_frontend's orphan-port check no longer fails with a message " +
			"naming the PID(s) still holding the port")
	}
}

// TestRCFrontend_IsValidPOSIXShell catches a shell-syntax regression in
// a script this suite otherwise can't run: `sh -n` parses the script
// without executing it. This is the Linux system shell, not FreeBSD's,
// but POSIX sh syntax is syntax either way and this is still strictly
// more coverage than the zero a shell script normally gets from `go
// test`.
func TestRCFrontend_IsValidPOSIXShell(t *testing.T) {
	path := filepath.Join(repoRoot(t), "etc", "rc.d", "apiary_frontend")
	out, err := exec.Command("sh", "-n", path).CombinedOutput()
	if err != nil {
		t.Fatalf("sh -n etc/rc.d/apiary_frontend: %v\n%s", err, out)
	}
}
