package forcerestart

import (
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/glenjbarber/apiary/internal/raftdconfig"
)

// PollProbes is how many times the listener is checked before a restart
// is declared unconfirmed, and PollInterval is the gap between them.
// Their product is the 15-second budget quoted in every diagnostic.
const (
	PollProbes   = 15
	PollInterval = time.Second
)

// Options configures one force-restart run. The zero value is a
// production configuration on this project: an empty Plan is
// DefaultPlan, an empty Host is the real host, an empty Record is the
// real record written to the real paths, and Out defaults to stderr
// because everything this prints is either a warning or a diagnostic.
type Options struct {
	// Plan is the ordered restart plan. Defaults to DefaultPlan.
	Plan []Service

	// Host is the machine this run acts on. Defaults to hostCommand{}.
	Host Host

	// Record locates the pending-restart record. Defaults to the paths
	// the daemons themselves use.
	Record RecordPaths

	// Out receives the banner, the restart plan, the record warnings,
	// the per-service progress lines and, on failure, the diagnostic.
	Out io.Writer
}

func (o Options) withDefaults() Options {
	if len(o.Plan) == 0 {
		o.Plan = DefaultPlan
	}
	if o.Host == nil {
		o.Host = hostCommand{}
	}
	if o.Out == nil {
		o.Out = os.Stderr
	}
	return o
}

// Result reports what a run actually did, so a caller can tell the
// difference between "stopped at managerd" and "stopped at raftd"
// without parsing the output.
type Result struct {
	// Restarted is every service whose restart command was issued, in
	// order, including the one that then failed to confirm. A service
	// that was restarted and not confirmed is a Comb running a mix of
	// the old and the new build, and it is the single most important
	// thing to know after a failed run, so it is reported explicitly
	// rather than inferred.
	Restarted []string

	// Confirmed is every service whose listener was seen afterwards.
	Confirmed []string

	// Failed is the service whose listener never appeared, empty if the
	// run completed.
	Failed string

	// Err is the reason the run stopped. Non-nil means the run did not
	// complete the plan.
	Err error
}

// Completed reports whether the whole plan was restarted and confirmed.
func (r Result) Completed() bool { return r.Err == nil }

// StoppedBeforeAnyRestart reports whether this run issued no restart
// command at all - that is, whether the Comb is exactly as it was.
//
// It is the distinction an operator's next action turns on, and it is
// read off Restarted rather than off Err, because Err does not say it:
// ErrNoKnownPort, ErrIsLeader and ErrLeaderUnknown are all refusals
// that touched nothing, while a restart command that itself failed is
// also in Restarted, and that one did touch something. A caller that
// wanted the opposite answer - a Comb left half-restarted - is asking
// the same question and reading the same field.
func (r Result) StoppedBeforeAnyRestart() bool { return len(r.Restarted) == 0 }

// Run executes one force-restart: the whole-plan preflight, the leader
// check, the banner, then each service in turn - record, restart,
// confirm by listener - stopping at the first service that does not
// come back.
//
// It returns a Result rather than only an error, and it returns a
// non-nil Err whenever the plan was not carried out in full, including
// for the refusal cases that touched nothing at all. A caller that
// ignores the error and reads the Result can still tell what happened.
func Run(opts Options) (Result, error) {
	opts = opts.withDefaults()
	res := Result{}

	// THE PREFLIGHT, BEFORE ANYTHING IS RESTARTED.
	//
	// The whole plan is checked for a known listener port before the
	// first restart. A service with no port here would otherwise be
	// discovered halfway through, with the first daemon already
	// restarted and nothing on the Comb explaining why. A third daemon
	// added to the plan tomorrow is refused on the spot, having touched
	// nothing - which is the only useful moment to be told about it.
	for _, s := range opts.Plan {
		if s.Port <= 0 {
			fmt.Fprintf(opts.Out, "force-restart: no known listener port for %s,\n", s.RCName())
			fmt.Fprintf(opts.Out, "  and force-restart will not guess one. NOTHING has been\n")
			fmt.Fprintf(opts.Out, "  restarted. Add the daemon's fixed port to the plan (ADR-0108,\n")
			fmt.Fprintf(opts.Out, "  internal/frontend/fixedport.go) and reinstall apiaryctl.\n")
			// res.Err is set alongside the return so that a caller
			// reading only the Result sees the same refusal it would
			// see from the error. Every other exit from this function
			// already keeps the two in step, and one that did not
			// would hand a caller an empty Result for a run that
			// refused.
			res.Err = &ErrNoKnownPort{Service: s.RCName()}
			return res, res.Err
		}
	}

	hostname, err := opts.Host.Hostname()
	if err != nil {
		// Not fatal: the hostname is only in the banner. Say so and
		// carry on, because refusing to restart anything over a
		// cosmetic detail is the wrong trade.
		fmt.Fprintf(opts.Out, "  could not read this host's name (%v); continuing.\n", err)
		hostname = "this Comb"
	}

	// THE LEADER CHECK, STILL BEFORE ANYTHING IS RESTARTED.
	//
	// It comes after the port preflight and before the banner, and both
	// placements are load-bearing. After the port preflight, because a
	// plan this build cannot carry out at all is a more fundamental
	// thing to be told about than a leader it would not have restarted
	// on. Before the banner, because the banner is "about to restart",
	// and printing that and then refusing would be a message that says
	// the opposite of what happened.
	//
	// Every exit from here sets res.Err alongside the return, for the
	// reason the port preflight does: a caller reading only the Result
	// has to see the same refusal it would see from the error.
	lead, err := opts.Host.Leadership()
	switch {
	case err != nil:
		leaderUnknown(opts.Out, err)
		res.Err = &ErrLeaderUnknown{Err: err}
		return res, res.Err
	case lead.IsLeader:
		refuseLeader(opts.Out, hostname, lead)
		res.Err = &ErrIsLeader{NodeID: lead.NodeID, RaftState: lead.RaftState}
		return res, res.Err
	}
	// Printed on the way through, not only on refusal. An operator who
	// sees which answer this Comb gave has a transcript that says the
	// check ran, and the next time they run this on a different Comb
	// the two transcripts are comparable.
	fmt.Fprintf(opts.Out, "leader check: this Comb is not the leader")
	if lead.RaftState != "" {
		fmt.Fprintf(opts.Out, " (raft state %s", lead.RaftState)
		if lead.NodeID != "" {
			fmt.Fprintf(opts.Out, ", node %s", lead.NodeID)
		}
		fmt.Fprintf(opts.Out, ")")
	}
	fmt.Fprintf(opts.Out, ".\n")

	banner(opts.Out, hostname, opts.Plan)
	fmt.Fprintf(opts.Out, "  restart plan, each confirmed by its own listener port:\n")
	fmt.Fprintf(opts.Out, "    %s  (service:port, in this order)\n", renderPlan(opts.Plan))

	for _, s := range opts.Plan {
		if err := restartOne(opts, s, &res); err != nil {
			return res, err
		}
	}

	closing(opts.Out)
	return res, nil
}

// restartOne restarts one service and confirms it, or stops the run.
func restartOne(opts Options, s Service, res *Result) error {
	fmt.Fprintf(opts.Out, "restarting %s (waiting for port %d) ...\n", s.RCName(), s.Port)

	// BEFORE the restart, always. A record written after a restart that
	// never came back teaches the cooldown nothing, and leaves the
	// pending file to be read by the *next* startup - which would then
	// confirm a restart that is already over.
	writeForcedRecord(s, opts.Record, opts.Out)

	if err := opts.Host.RestartService(s.RCName()); err != nil {
		res.Restarted = append(res.Restarted, s.RCName())
		res.Failed = s.RCName()
		res.Err = err
		fmt.Fprintf(opts.Out, "  could not restart %s: %v\n", s.RCName(), err)
		fmt.Fprintf(opts.Out, "  Stopping here rather than restarting the rest of the plan.\n")
		alreadyRestarted(opts.Out, res)
		return err
	}
	res.Restarted = append(res.Restarted, s.RCName())

	// A bounded poll, not one probe and not an open loop. Probes counts
	// attempts; the sleep is between them, so the budget is one second
	// short of 15 in the worst case, which is the safe direction for a
	// confirmation.
	broken := 0
	for probe := 0; probe < PollProbes; probe++ {
		up, err := opts.Host.Listening(s.Port)
		switch {
		case err != nil:
			// A sockstat that cannot be run is a broken measurement,
			// not a negative one. Counted and kept polling rather than
			// failing immediately: the daemon may still come up, and the
			// distinction is reported if it does not.
			broken++
		case up:
			res.Confirmed = append(res.Confirmed, s.RCName())
			fmt.Fprintf(opts.Out, "  %s is listening on port %d\n", s.RCName(), s.Port)
			return nil
		}
		if probe < PollProbes-1 {
			opts.Host.Sleep(PollInterval)
		}
	}

	res.Failed = s.RCName()
	res.Err = &UnconfirmedError{Service: s.RCName(), Port: s.Port, BrokenProbes: broken}
	timeout(opts.Out, s, res, broken)
	return res.Err
}

// alreadyRestarted prints the list an operator reads to work out which
// build this Comb is now half-running.
//
// It is printed by whole lines and always, on every failure path,
// because the alternative is an operator who stopped the run being
// unsure whether this Comb is running the new managerd and the old
// raftd, or the other way round, or neither.
func alreadyRestarted(out io.Writer, res *Result) {
	fmt.Fprintf(out, "  ALREADY RESTARTED on this Comb, this run:\n")
	if len(res.Restarted) == 0 {
		fmt.Fprintf(out, "    (nothing)\n")
		return
	}
	fmt.Fprintf(out, "    %s\n", strings.Join(res.Restarted, " "))
}

func join(items []string) string {
	out := ""
	for i, s := range items {
		if i > 0 {
			out += " "
		}
		out += s
	}
	return out
}

// UnconfirmedError reports a service that was restarted and then never
// opened its listener within the budget. BrokenProbes is how many of
// the probes could not be answered at all, as opposed to answered with
// "not listening" - a number worth seeing, because an operator staring
// at a timeout needs to know whether the daemon is not coming up or
// the measurement is not working.
type UnconfirmedError struct {
	Service      string
	Port         int
	BrokenProbes int
}

func (e *UnconfirmedError) Error() string {
	return fmt.Sprintf("%s did not open its listener port %d within %s", e.Service, e.Port, budget())
}

func budget() string { return fmt.Sprintf("%ds", int(PollProbes*PollInterval/time.Second)) }

// timeout prints the failure diagnostic. Every word of it is chosen:
// the operator is standing at a possibly half-restarted Comb and this
// is the only thing telling them what state it is in.
func timeout(out io.Writer, s Service, res *Result, broken int) {
	fmt.Fprintf(out, "%s did not open its listener port %d within %s.\n", s.RCName(), s.Port, budget())
	fmt.Fprintf(out, "  That is a sockstat(1) port check, not a 'service\n")
	fmt.Fprintf(out, "  status' check - 'status' is not trustworthy on this\n")
	fmt.Fprintf(out, "  host, which is why the port is checked instead.\n")
	if broken > 0 {
		fmt.Fprintf(out, "  %d of the %d probes could not be answered at all: sockstat\n", broken, PollProbes)
		fmt.Fprintf(out, "  itself failed. Treat this as a broken measurement as much\n")
		fmt.Fprintf(out, "  as a daemon that did not come up.\n")
	}
	fmt.Fprintf(out, "  Stopping here rather than restarting the next\n")
	fmt.Fprintf(out, "  service: a half-restarted managerd/raftd pair is\n")
	fmt.Fprintf(out, "  exactly the state that strands a restart lease.\n")
	alreadyRestarted(out, res)
	fmt.Fprintf(out, "  NOT restarted: every service after %s in\n", s.RCName())
	fmt.Fprintf(out, "  the plan above, so this Comb is now running a mix\n")
	fmt.Fprintf(out, "  of the old and the new build. Finish or roll back\n")
	fmt.Fprintf(out, "  before touching the next Comb.\n")
	fmt.Fprintf(out, "  See /var/log/apiary/%s.log\n", s.Name)
}

// refuseLeader is what an operator sees when the Comb they ran this on
// is the one Comb the command will not restart.
//
// It is the longest message in this file, and that is the point of the
// refusal: the operator has just been told no by a command they were
// relying on, during something that is going wrong, and "no" with no
// reason and no next step is the message that gets one attempt at
// working around it. So it says what is protected, what state the Comb
// is in, and what to do instead - and it does not promise a control
// that does not exist.
// THE FOUR-SPACE INDENT
//
// Both refusals below indent an interpolated fact - a Comb's names, or
// the error a dial produced - by four spaces, while their prose wraps
// at two. The convention exists because those two kinds of line cannot
// both be wrapped: a hostname and a node id can be long, and a gRPC
// error is quoted verbatim because shortening it loses the part that
// identifies what went wrong. Prose at two spaces wraps to 76 columns
// and is checked for it; a fact at four spaces is as long as it is,
// and is set apart so the eye can tell which is which. A refusal that
// let a 200-column error run into its own paragraph is a refusal whose
// first line is the only one anybody reads.

func refuseLeader(out io.Writer, hostname string, lead Leadership) {
	fmt.Fprintf(out, "force-restart: refusing. This Comb is the Colony's current leader.\n")
	fmt.Fprintf(out, "    Comb `%s`", hostname)
	if lead.NodeID != "" {
		fmt.Fprintf(out, ", raft node `%s`", lead.NodeID)
	}
	if lead.RaftState != "" {
		fmt.Fprintf(out, ", raft state %s", lead.RaftState)
	}
	fmt.Fprintf(out, "\n")
	fmt.Fprintf(out, "\n")
	fmt.Fprintf(out, "  Raft applies every write in the Colony on the leader, and on a\n")
	fmt.Fprintf(out, "  multi-Comb Colony a leader that goes down without handing over can\n")
	fmt.Fprintf(out, "  cost the Colony its quorum. That is the one restart this command\n")
	fmt.Fprintf(out, "  will not perform, and it will not perform it on the strength of\n")
	fmt.Fprintf(out, "  an operator being sure.\n")
	fmt.Fprintf(out, "  NOTHING has been restarted. This Comb is running exactly the\n")
	fmt.Fprintf(out, "  build it was running a moment ago, and no pending-restart\n")
	fmt.Fprintf(out, "  record was written for it.\n")
	fmt.Fprintf(out, "\n")
	fmt.Fprintf(out, "  The leader is a position, not a machine. Restart the Combs that\n")
	fmt.Fprintf(out, "  are followers; leadership moves, and this command's answer\n")
	fmt.Fprintf(out, "  changes with it, so re-running it on this Comb later can give a\n")
	fmt.Fprintf(out, "  different answer. Raftd's own control on the Machine page reserves\n")
	fmt.Fprintf(out, "  a real cluster-wide lease and runs the quorum preflight this\n")
	fmt.Fprintf(out, "  command deliberately does not.\n")
	fmt.Fprintf(out, "\n")
	fmt.Fprintf(out, "  If this Comb has to be restarted now and nothing else can move\n")
	fmt.Fprintf(out, "  leadership off it, that is a decision to make with the Colony's\n")
	fmt.Fprintf(out, "  quorum in view - and there is no flag on this command to make it\n")
	fmt.Fprintf(out, "  for you, deliberately.\n")
}

// leaderUnknown is the refusal for a Comb that could not say whether it
// is the leader.
//
// It is a separate message from refuseLeader rather than the same one
// with a hedge in it, because the operator's next action is completely
// different: this one is a broken or absent raftd to go and look at,
// not a leadership problem to wait out. What both messages share is the
// refusal itself, and that is stated here as a decision rather than
// inherited: an unanswered question is not permission.
func leaderUnknown(out io.Writer, err error) {
	fmt.Fprintf(out, "force-restart: refusing. Could not tell whether this Comb is the\n")
	fmt.Fprintf(out, "  Colony's current leader.\n")
	fmt.Fprintf(out, "    %v\n", err)
	fmt.Fprintf(out, "\n")
	fmt.Fprintf(out, "  That is not the same answer as \"not the leader\", and this\n")
	fmt.Fprintf(out, "  command will not treat it as one. The check is the only thing\n")
	fmt.Fprintf(out, "  standing between an operator and a leader restart, so a check that\n")
	fmt.Fprintf(out, "  could not be answered refuses.\n")
	fmt.Fprintf(out, "  NOTHING has been restarted.\n")
	fmt.Fprintf(out, "\n")
	// The config path goes on its own line rather than inside the
	// sentence, and at the four-space indent: it is a path, and a path
	// in the middle of a wrapped paragraph is how a 76-column
	// diagnostic becomes an 88-column one the first time someone
	// installs to a different prefix.
	fmt.Fprintf(out, "  Check that apiary_raftd is running, and that the socket in\n")
	fmt.Fprintf(out, "    %s\n", raftdconfig.DefaultPath)
	fmt.Fprintf(out, "  is this Comb's own. If raftd is not running at all,\n")
	fmt.Fprintf(out, "  `service apiary_raftd start` first, and wait for it to answer\n")
	fmt.Fprintf(out, "  before coming back here.\n")
}

// banner is the warning printed before anything happens, and it is
// printed in full every time. The whole reason force-restart exists is
// that it does not do what a coordinated restart does, and an operator
// who has not read that should not have to have read it beforehand.
func banner(out io.Writer, hostname string, plan []Service) {
	fmt.Fprintf(out, "force-restart: about to restart %s on `%s`\n", renderPlan(plan), hostname)
	fmt.Fprintf(out, "  by handing 'service' a restart directly. This acquires NO\n")
	fmt.Fprintf(out, "  restart lease, runs NO quorum preflight, and coordinates\n")
	fmt.Fprintf(out, "  with NO other Comb. Restarting raftd on more than one Comb at\n")
	fmt.Fprintf(out, "  once can cost the cluster its quorum - which is why this command\n")
	fmt.Fprintf(out, "  refuses to run on the current leader at all, and why the leader\n")
	fmt.Fprintf(out, "  was checked before this banner rather than asked about afterwards.\n")
	fmt.Fprintf(out, "  For a coordinated restart use the Machine page's per-service\n")
	fmt.Fprintf(out, "  control, which reserves a real cluster-wide lease.\n")
	fmt.Fprintf(out, "\n")
	fmt.Fprintf(out, "  It does still leave a record: each service gets the same\n")
	fmt.Fprintf(out, "  pending-restart note a leased restart would, with lease_id 0,\n")
	fmt.Fprintf(out, "  so the guardrail's 600s cooldown learns a restart happened\n")
	fmt.Fprintf(out, "  here and blocks a second one on another Comb. It never takes\n")
	fmt.Fprintf(out, "  a lease and never releases one.\n")
	fmt.Fprintf(out, "\n")
}

func closing(out io.Writer) {
	fmt.Fprintf(out, "\n")
	fmt.Fprintf(out, "force-restart: done.\n")
	fmt.Fprintf(out, "  Confirm the running build, not the one on disk:\n")
	fmt.Fprintf(out, "    grep build= /var/log/apiary/managerd.log /var/log/apiary/raftd.log\n")
	fmt.Fprintf(out, "      | tail -2\n")
	fmt.Fprintf(out, "  and check the Machine page's colony view before moving on to\n")
	fmt.Fprintf(out, "  the next Comb.\n")
}
