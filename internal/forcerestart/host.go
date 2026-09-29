package forcerestart

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// Host is the whole of force-restart's contact with the machine it runs
// on: three host commands and nothing else. It exists so that the
// restart loop, the ordering rules, the record-before-restart rule and
// the timeout behaviour can be executed for real in a test with no
// Comb, no root and no live daemon - see RecordingHost.
//
// The interface has no method that could ask whether a service is
// running. That is deliberate and is the reason this file exists in
// this shape: `service <name> status` reports "not running" for every
// apiary daemon on a Comb where all of them are up and listening, so a
// confirmation built on it can only ever reach its own failure branch -
// on the first service, every time. An earlier version of this
// behaviour lived in the Makefile and did exactly that: it restarted
// managerd, burned the full wait, printed a failure, and exited having
// never touched raftd, leaving behind a pending-restart record for a
// restart that never happened. The listener is the measurement, and
// leaving the ability to make the wrong measurement out of the
// interface is stronger than documenting that it should not be made.
// Leadership IS asked for, and that is not the same mistake. It is not
// `service status`: it is this Comb's own raftd answering Status for
// itself, which is a measurement raftd cannot get wrong about its own
// role the way a wrapper script can get a pidfile wrong. It is also the
// one question whose wrong answer is unrecoverable - a listener check
// that fails leaves a daemon that is not up, which the next attempt
// fixes, while a leader check that is quietly skipped does the
// restart. So the interface carries the question, and carries nothing
// that could answer it wrongly. See leadership.go.
type Host interface {
	// RestartService hands rc.d a restart and returns when it is done.
	// A non-nil error means the restart command itself failed; it says
	// nothing about whether the daemon came back, which is what
	// Listening is for.
	RestartService(rcName string) error

	// Listening reports whether something is listening on port, and
	// whether the question could be answered at all. An error is not
	// the same answer as false-for-silence, so they are distinct: a
	// sockstat that cannot be run is a broken measurement, and the
	// caller reports it as such instead of quietly treating it as
	// "not yet".
	Listening(port int) (bool, error)

	// Sleep waits between listener probes.
	Sleep(d time.Duration)

	// Hostname is this Comb's own name, used only in the operator-facing
	// banner.
	Hostname() (string, error)

	// Leadership reports whether this Comb is the Colony's current
	// leader, from this Comb's own raftd.
	//
	// It is asked once, before anything is restarted, and it refuses
	// the run on yes. An error is a refusal too and is not the same
	// answer as a no: an unanswerable question is an unestablished
	// Comb, and force-restart restarts raftd. See leadership.go for why
	// there is no retry and no override.
	Leadership() (Leadership, error)
}

// hostCommand is the production Host: for a restart, a listener and a
// name, it shells out to the same three commands an operator would
// type. For leadership it does not, because no such command exists -
// see the Leadership method below.
//
// It execs by bare name and relies on PATH, deliberately. The point of
// this command is that it works on a host with no source checkout, and
// a hardcoded absolute path to /usr/sbin/service would pin it to one
// FreeBSD layout's opinion about where tools live, which is a new way
// to fail on the machines this is for.
type hostCommand struct{}

// NewHost returns the real Host: service(8), sockstat(8), a sleep,
// this machine's name, and this Comb's own raftd. Exported so a test
// can drive the production implementation with stand-ins for those two
// commands on PATH, which exercises the exec and the parsing together
// rather than stubbing either one out.
func NewHost() Host { return hostCommand{} }

func (hostCommand) RestartService(rcName string) error {
	cmd := exec.Command("service", rcName, "restart")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("service %s restart: %w: %s", rcName, err, strings.TrimSpace(string(out)))
	}
	return nil
}

func (hostCommand) Listening(port int) (bool, error) {
	// -4 restricts to IPv4 and -l to listening sockets only. The exit
	// status is checked: a sockstat that fails is a broken measurement,
	// and a pipeline would have discarded that distinction and reported
	// the same "no listener" answer for both a working sockstat and a
	// missing one.
	out, err := exec.Command("sockstat", "-4", "-l").Output()
	if err != nil {
		return false, fmt.Errorf("sockstat -4 -l: %w", err)
	}
	return ListeningFrom(string(out), port), nil
}

func (hostCommand) Sleep(d time.Duration) { time.Sleep(d) }

func (hostCommand) Hostname() (string, error) { return os.Hostname() }

// Leadership asks this Comb's own raftd. Unlike the three commands
// above it is not a shell-out, and it is not on PATH: there is no
// host command that answers "is this Comb the leader", and the nearest
// things that do - rc.d's status line, a log file, the Machine page -
// are all either false on a live Comb (the status line) or derived
// from something other than raft's own state.
func (h hostCommand) Leadership() (Leadership, error) { return newLeadershipProbe().Leadership() }
