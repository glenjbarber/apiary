package main

import (
	"errors"
	"flag"
	"fmt"
	"os"

	"github.com/glenjbarber/apiary/internal/forcerestart"
)

// runForceRestart is the whole of the force-restart subcommand, and
// almost all of it is a refusal.
//
// The refusals are the substance. This restarts the daemons that carry
// raft state and the operator API, on one node, with no lease, no
// quorum preflight and no coordination with the rest of the Colony. An
// operator who has the wrong Comb, the wrong privileges, or the wrong
// idea of which daemons are up can do real damage with it, so the
// three things that make it safe to hand to someone are that it insists
// on root, that it refuses outright on the Colony's current leader, and
// that it prints what it is about to do before it does it.
//
// The leader refusal is checked inside the package rather than here,
// against this Comb's own raftd, and there is no flag to get past it.
// That placement is the point: the property belongs to the restart, not
// to this one way of asking for it, and an option a caller can pass is
// a guard a caller can eventually forget to pass.
//
// The exit status is 1 for any failure and 0 only for a completed plan.
// There is no third value, and no "partly succeeded" exit that a script
// could mistake for success: a run that stopped halfway has left the
// Comb running a mix of two builds, which is the one state an operator
// must not be told is fine.
func runForceRestart(args []string) int {
	fs := flag.NewFlagSet("apiaryctl force-restart", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(os.Stderr, "apiaryctl force-restart: takes no arguments, got %q\n", fs.Arg(0))
		return 2
	}

	// Root only, and the check is the effective UID rather than $USER
	// or the environment. Everything this does is a service(8) restart
	// and a root-owned record under /var/db/apiary; neither is
	// something to discover permissions on the way to failing.
	//
	// Checked in this process rather than in the kernel at exec time so
	// that the message names this command and points at the Machine
	// page, instead of being a bare "permission denied" from a helper.
	if os.Geteuid() != 0 {
		fmt.Fprint(os.Stderr, `apiaryctl force-restart: must run as root.

It restarts managerd and raftd and writes the pending-restart records
the restart guardrail reads, both of which need root. Run it from a
root shell, or use sudo.

If you are trying to restart managerd alone, "service apiary_managerd
restart" does that and needs nothing but a root shell.
`)
		return 1
	}

	res, err := forcerestart.Run(forcerestart.Options{Out: os.Stderr})
	if err == nil {
		return 0
	}

	// A refusal that issued no restart command at all gets one line
	// here, and it is driven by the Result rather than by the error's
	// type. There are three of those refusals now - a plan entry with no
	// known port, this Comb being the leader, and this Comb's leadership
	// being unanswerable - and they differ in what the package had to
	// say, which it has already printed, while being identical in the
	// one thing that matters to the operator's next minute: nothing
	// here was touched. Naming them one at a time is how a fourth gets
	// added and reported as "stopped at ." with nothing restarted.
	if res.StoppedBeforeAnyRestart() {
		fmt.Fprintf(os.Stderr, "\napiaryctl force-restart: stopped before restarting anything.\n")
		return 1
	}

	// A timeout already printed its own diagnostic, including every
	// restart issued before the failure. Do not repeat it here; add
	// only what only this layer knows - that the run stopped, and what
	// the Comb is left running.
	fmt.Fprintf(os.Stderr, "\napiaryctl force-restart: stopped at %s.\n", res.Failed)
	fmt.Fprintf(os.Stderr, "  This Comb is now running a mix of the old and the new build.\n")
	fmt.Fprintf(os.Stderr, "  Finish or roll back before moving to the next Comb.\n")
	if !unwrapIsTimeout(err) {
		fmt.Fprintf(os.Stderr, "  The restart command itself failed: %v\n", err)
	}
	return 1
}

func unwrapIsTimeout(err error) bool {
	var unconfirmed *forcerestart.UnconfirmedError
	return errors.As(err, &unconfirmed)
}
