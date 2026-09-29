package forcerestart_test

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/glenjbarber/apiary/internal/forcerestart"
)

// This file proves what force-restart actually does, by running the
// real restart loop against a recording stand-in for the host.
//
// WHY IT RUNS THE CODE INSTEAD OF READING IT
//
// The assertion force-restart used to make was `service <name> status`,
// and on a Comb that call is not a measurement: every apiary_* rc.d
// script reports "not running", on every check, while all four daemons
// are demonstrably up and listening. sockstat disagrees with all of
// them. So the old confirmation could only ever reach its own failure
// branch - on the first service, every time. It restarted managerd,
// burned the full 15-second budget, printed the failure and exited
// non-zero having never touched raftd, leaving behind a
// pending-restart record for a restart that never happened: the exact
// half-restarted managerd/raftd pair the command exists to prevent,
// produced by the command itself.
//
// That defect is invisible to any test that asserts on source text,
// because the text was right - it was the host command that lied. A
// test checking "the code no longer says `service ... status`" would
// have passed against a checker that grepped sockstat output for
// ":17700" while sockstat printed a different column layout, and would
// equally have passed against a checker matching a substring of an
// unrelated port. So every case below executes the real Run loop, with
// the host commands replaced by a stand-in that records every call in
// order.
//
// The record is not a stand-in either. The real writeForcedRecord runs,
// through the real internal/restartplan store, against fixture config
// files in a temp directory. The bytes on disk asserted on here are the
// bytes a Comb would get.
//
// No root, no Comb, no live service, and no wall-clock wait: the fake
// host's Sleep is a no-op, so the 15 probes this test asserts on cost
// nothing while still being the real 15 the production loop runs.

// mode is what the fake sockstat reports.
type mode int

const (
	// modeAll: every daemon in the plan is listening.
	modeAll mode = iota
	// modeNone: nothing is listening at all.
	modeNone
	// modeManagerdOnly: managerd came back, raftd did not.
	modeManagerdOnly
	// modeRaftdOnly: raftd is listening, managerd is not. The case
	// that catches a checker watching "any apiary socket", or one with
	// the two ports swapped.
	modeRaftdOnly
	// modeDecoy: a row carrying the digits of managerd's port with the
	// `host:` delimiter dropped, so the digits are right and only the
	// delimiter is not. A substring check passes this; a real parse
	// must not.
	modeDecoy
	// modeBroken: sockstat itself errors. Every probe unanswerable,
	// which is a broken measurement rather than a negative one.
	modeBroken
)

// leadership is what the fake local raftd answers. It is a separate
// type from mode because it is a different question asked at a
// different time: mode drives the listener poll after a restart, and
// this one is asked once, before anything at all happens.
type leadership int

const (
	// leadFollower: this Comb is a follower, which is the only answer
	// that lets the run proceed. It is the default, because a fake that
	// has to be told it is a follower before every existing case means
	// every existing case is also asserting the leader check - and the
	// cases below that are about the leader check would then be the
	// only ones that are.
	leadFollower leadership = iota
	// leadLeader: this Comb is the Colony's leader.
	leadLeader
	// leadUnanswerable: raftd could not be asked at all.
	leadUnanswerable
)

// fakeHost is the recording Host. Every call appends one line to log,
// so "the record was written before the restart" and "raftd was never
// touched after managerd timed out" are both read off a single ordered
// artifact rather than inferred from exit status and prose.
type fakeHost struct {
	t    *testing.T
	log  *strings.Builder
	mode mode

	// leadership is what Host.Leadership answers.
	leadership leadership

	// listeningAfter, when non-nil, replaces mode: this function
	// answers each Listening(port) call, which is how a case models a
	// daemon that is slow to come up rather than never coming up.
	listeningAfter func(port int, probe int) (bool, error)
	probe          int

	// restartErr, when non-nil, fails the restart of this rc.d name.
	restartErr map[string]error
	// hostname is what the banner prints.
	hostname string
	// hostnameErr models a host whose name cannot be read.
	hostnameErr error
}

func newFakeHost(t *testing.T, m mode) *fakeHost {
	return &fakeHost{t: t, log: &strings.Builder{}, mode: m, hostname: "comb-under-test"}
}

func (h *fakeHost) RestartService(rcName string) error {
	fmt.Fprintf(h.log, "service %s restart\n", rcName)
	return h.restartErr[rcName]
}

func (h *fakeHost) Listening(port int) (bool, error) {
	fmt.Fprintf(h.log, "sockstat -4 -l\n")
	if h.listeningAfter != nil {
		out, err := h.listeningAfter(port, h.probe)
		h.probe++
		return out, err
	}
	h.probe++
	switch h.mode {
	case modeAll:
		return true, nil
	case modeNone, modeDecoy, modeBroken:
		return false, nil
	case modeManagerdOnly:
		return port == 17700, nil
	case modeRaftdOnly:
		return port == 17600, nil
	}
	h.t.Fatalf("fakeHost: unhandled mode %d", h.mode)
	return false, nil
}

func (h *fakeHost) Sleep(d time.Duration) {
	fmt.Fprintf(h.log, "sleep %s\n", d)
}

// Sleep is a no-op by construction - the budget is asserted by probe
// count, so the failing cases do not spend fifteen seconds each.

func (h *fakeHost) Hostname() (string, error) { return h.hostname, h.hostnameErr }

// Leadership answers the leader check, and logs the call so the
// ordering assertions can see that it happened before the first
// restart rather than after it.
func (h *fakeHost) Leadership() (forcerestart.Leadership, error) {
	fmt.Fprintf(h.log, "raftd status\n")
	switch h.leadership {
	case leadFollower:
		return forcerestart.Leadership{IsLeader: false, NodeID: "comb-under-test", RaftState: "Follower"}, nil
	case leadLeader:
		return forcerestart.Leadership{IsLeader: true, NodeID: "comb-under-test", RaftState: "Leader"}, nil
	case leadUnanswerable:
		return forcerestart.Leadership{}, fmt.Errorf("asking raftd for its status: rpc error: code = Unavailable desc = connection error: desc = %q", "transport: Error while dialing dial unix /var/run/apiary/raftd.sock: connect: no such file or directory")
	}
	h.t.Fatalf("fakeHost: unhandled leadership %d", h.leadership)
	return forcerestart.Leadership{}, nil
}

// sockstatErrors is the mode in which the measurement itself is broken.
// It is a separate function rather than a field so the failing case
// reads as a failure of the host command, which is what it is.
// lines returns the recorded host calls, which is the ordered log every
// ordering assertion below reads.
func (h *fakeHost) lines() []string {
	out := []string{}
	for _, l := range strings.Split(h.log.String(), "\n") {
		if l != "" {
			out = append(out, l)
		}
	}
	return out
}

// restarts returns the rc.d names that were restarted, in order.
func (h *fakeHost) restarts() []string {
	var out []string
	for _, l := range h.lines() {
		if strings.HasPrefix(l, "service ") && strings.HasSuffix(l, " restart") {
			out = append(out, strings.TrimSuffix(strings.TrimPrefix(l, "service "), " restart"))
		}
	}
	return out
}

func (h *fakeHost) probeCount() int {
	n := 0
	for _, l := range h.lines() {
		if l == "sockstat -4 -l" {
			n++
		}
	}
	return n
}

// run executes one force-restart against a recording host and a set of
// fixture config files, and returns everything a case needs to judge
// it. The Record paths are all inside a per-case temp directory, so
// the real store writes real files without needing root or /var/db.
func run(t *testing.T, plan []forcerestart.Service, h *fakeHost) (forcerestart.Result, string, string) {
	t.Helper()
	dir := t.TempDir()
	guardrail := filepath.Join(dir, "guardrail")
	common := filepath.Join(dir, "common.json")
	// A node id that is obviously a fixture, so a record naming anything
	// else is visibly wrong rather than subtly wrong.
	if err := os.WriteFile(common, []byte(`{"node_id":"comb-under-test"}`+"\n"), 0o600); err != nil {
		t.Fatalf("writing fixture common.json: %v", err)
	}
	var out strings.Builder
	res, err := forcerestart.Run(forcerestart.Options{
		Plan: plan,
		Host: h,
		Record: forcerestart.RecordPaths{
			Dir:          guardrail,
			RaftdJSON:    filepath.Join(dir, "raftd.json"),
			ManagerdJSON: filepath.Join(dir, "managerd.json"),
			CommonJSON:   common,
			Hostname:     "comb-under-test",
		},
		Out: &out,
	})
	// The error is returned through Result.Err, and every case judges
	// it from there so no case can pass by ignoring it.
	if (err != nil) != (res.Err != nil) {
		t.Fatalf("Run returned err=%v but Result.Err=%v; the two must agree", err, res.Err)
	}
	if err != nil && err != res.Err {
		t.Fatalf("Run returned an error that is not Result.Err: %v", err)
	}
	return res, out.String(), guardrail
}

func readRecord(t *testing.T, dir, rcName string) (string, bool) {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(dir, "pending-restart-"+rcName+".json"))
	if os.IsNotExist(err) {
		return "", false
	}
	if err != nil {
		t.Fatalf("reading record for %s: %v", rcName, err)
	}
	return strings.TrimSpace(string(body)), true
}

// ---------------------------------------------------------------------------
// 1. The honest path, with `service status` lying the whole way.
//
// There is no way even to express that lie here: the Host interface has
// no method that asks a SERVICE for its status. The structural absence
// is the regression, and the log assertion below is a backstop against
// a future method being added that would reintroduce it. (It does now
// have a method that asks about leadership, which is a different
// question from a different source - see case 8.)
// ---------------------------------------------------------------------------

func TestForceRestart_HonestPath(t *testing.T) {
	h := newFakeHost(t, modeAll)
	res, out, guardrail := run(t, nil, h)

	if !res.Completed() {
		t.Fatalf("the honest path must complete, got %v (restarted %v)", res.Err, res.Restarted)
	}
	if got, want := strings.Join(h.restarts(), " "), "apiary_managerd apiary_raftd"; got != want {
		t.Errorf("restart order:\n got %q\nwant %q", got, want)
	}
	// managerd before raftd, and the reason is a real dependency, not
	// a preference: raftd's startup confirm path talks to managerd, and
	// leases have no TTL, so raftd-first strands a record.
	if len(h.restarts()) == 2 && h.restarts()[0] != "apiary_managerd" {
		t.Errorf("managerd must be restarted first, got %v", h.restarts())
	}

	for _, want := range []string{
		"apiary_managerd (waiting for port 17700)",
		"apiary_raftd (waiting for port 17600)",
		"apiary_managerd is listening on port 17700",
		"apiary_raftd is listening on port 17600",
		"restart plan, each confirmed by its own listener port:",
		"apiary_managerd:17700 apiary_raftd:17600  (service:port, in this order)",
		// The leader check passed, and the transcript says so. A run
		// that restarted anything without printing this either skipped
		// the check or checked after the fact, and both are the same
		// defect.
		"leader check: this Comb is not the leader (raft state Follower, node comb-under-test).",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output is missing %q\n---\n%s---", want, out)
		}
	}

	// One probe per service: both came up immediately, so the loop must
	// not have slept. A fixed number of probes regardless of success
	// would also be a defect, just a slower one.
	if got := h.probeCount(); got != 2 {
		t.Errorf("probes issued = %d, want 2 (one per service, no waiting)", got)
	}

	// No SERVICE status call was issued - and the interface still has
	// no way to make one, which is the durable form of this assertion.
	// The match is on `service <name> status` and not on the substring
	// " status", because the leader check logs a line of its own that
	// ends in the same two words, and an assertion loose enough to
	// catch that one would be too loose to catch the defect it exists
	// for. Leadership is a different measurement from a different
	// source: raftd's own Status RPC, not rc.d's pidfile check, which
	// lies on a live Comb.
	for _, l := range h.lines() {
		if strings.HasPrefix(l, "service ") && strings.HasSuffix(l, " status") {
			t.Errorf("a service status call was issued: %q; that measurement is known false on a Comb", l)
		}
	}

	// The records are real files written by the real store, with the
	// exact bytes a Comb would get. lease_id 0 is the point: it informs
	// the guardrail's cooldown and can never release a real lease.
	for _, rc := range []string{"apiary_managerd", "apiary_raftd"} {
		body, ok := readRecord(t, guardrail, rc)
		if !ok {
			t.Errorf("no pending-restart record was written for %s", rc)
			continue
		}
		want := fmt.Sprintf(`{"service":%q,"node_id":"comb-under-test","lease_id":0}`, rc)
		if body != want {
			t.Errorf("record for %s:\n got %s\nwant %s", rc, body, want)
		}
		info, err := os.Stat(filepath.Join(guardrail, "pending-restart-"+rc+".json"))
		if err == nil && info.Mode().Perm() != 0o600 {
			t.Errorf("record for %s has mode %v, want 0600: it names a voter and lives in /var/db", rc, info.Mode().Perm())
		}
	}

	// Recorded before restarted, for both services, interleaved with the
	// probe that confirmed each. This is the ordering that matters: a
	// record written after a restart that never came back teaches the
	// cooldown nothing and leaves the pending file for the *next*
	// startup, which would then confirm a restart already over.
	//
	// The whole log is asserted, not a prefix, because on the honest
	// path it is five calls and nothing else. A wait that slept between
	// a service that was already up would show up here as an extra
	// line, and that is a real defect: the wait costs a second per
	// restart whether or not anything needs waiting for. The leader
	// check is in this list for the same reason and not as a
	// footnote: it has to be the FIRST call, because a check made
	// after managerd is already down has stopped a run rather than
	// prevented one.
	wantLog := []string{
		"raftd status",
		"service apiary_managerd restart",
		"sockstat -4 -l",
		"service apiary_raftd restart",
		"sockstat -4 -l",
	}
	got := h.lines()
	if len(got) != len(wantLog) {
		t.Errorf("host calls = %v, want exactly %v", got, wantLog)
	} else {
		for i := range wantLog {
			if got[i] != wantLog[i] {
				t.Errorf("host call %d = %q, want %q", i, got[i], wantLog[i])
			}
		}
	}

	// The plan is announced before anything is restarted. The record
	// write is not in this log - it is a file, not a host command - so
	// the announcement is what makes an operator's terminal transcript
	// a record of the plan that was followed.
	if strings.Index(out, "restart plan") > strings.Index(out, "restarting apiary_managerd") {
		t.Errorf("the plan must be announced before the first restart\n---\n%s---", out)
	}
}

// ---------------------------------------------------------------------------
// 2. Nothing is listening. Fail on managerd, and do not go on to restart
//    raftd into a node whose managerd is down.
// ---------------------------------------------------------------------------

func TestForceRestart_StopsBeforeTheNextServiceOnTimeout(t *testing.T) {
	h := newFakeHost(t, modeNone)
	res, out, guardrail := run(t, nil, h)

	if res.Completed() {
		t.Fatal("a run where nothing ever listens must not report completion")
	}
	if _, ok := res.Err.(*forcerestart.UnconfirmedError); !ok {
		t.Fatalf("Err = %T (%v), want *forcerestart.UnconfirmedError", res.Err, res.Err)
	}
	if got, want := strings.Join(h.restarts(), " "), "apiary_managerd"; got != want {
		t.Errorf("restarted %q, want only %q: raftd must not be restarted into a Comb whose managerd is down", got, want)
	}
	if got := h.probeCount(); got != forcerestart.PollProbes {
		t.Errorf("probes = %d, want %d: the wait is a bounded poll, not one probe and not an open loop", got, forcerestart.PollProbes)
	}

	for _, want := range []string{
		"apiary_managerd did not open its listener port 17700 within 15s.",
		"Stopping here rather than restarting the next",
		"mix",
		"ALREADY RESTARTED on this Comb, this run:",
		"    apiary_managerd",
		"NOT restarted: every service after apiary_managerd",
		"See /var/log/apiary/managerd.log",
		// The diagnostic has to say why the port is the measurement,
		// because the next operator to look at this will reach for
		// `service status` and be told the same false thing it always
		// says.
		"not a 'service",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output is missing %q\n---\n%s---", want, out)
		}
	}

	// The service that was restarted and not confirmed still gets its
	// record: the restart *was* issued, and the cooldown has to learn
	// that. The service that was never restarted must not, because a
	// record for a restart that never happened is the failure this
	// whole record mechanism once had.
	if _, ok := readRecord(t, guardrail, "apiary_managerd"); !ok {
		t.Error("no record for the managerd restart that was actually issued")
	}
	if _, ok := readRecord(t, guardrail, "apiary_raftd"); ok {
		t.Error("a record was written for a raftd restart that never happened")
	}
}

// ---------------------------------------------------------------------------
// 3. Only managerd came back. raftd fails on its own port, and the
//    message must say what this Comb is now half-way into.
// ---------------------------------------------------------------------------

func TestForceRestart_NamesEveryRestartAlreadyIssued(t *testing.T) {
	h := newFakeHost(t, modeManagerdOnly)
	res, out, _ := run(t, nil, h)

	if res.Completed() {
		t.Fatal("raftd never listened, so the run must not report completion")
	}
	if got, want := strings.Join(h.restarts(), " "), "apiary_managerd apiary_raftd"; got != want {
		t.Errorf("restarted %q, want %q: both were issued, and both belong in the diagnostic", got, want)
	}
	if !strings.Contains(out, "apiary_raftd did not open its listener port 17600 within 15s.") {
		t.Errorf("the failure must name raftd and its own port\n---\n%s---", out)
	}

	// The already-restarted list is compared as a whole block, not by
	// substring, and that is deliberate. The restart plan printed before
	// the run also contains an indented "apiary_managerd", so a
	// substring assertion here would be satisfied by the plan and would
	// survive losing the list entirely - which is the exact defect this
	// check exists to catch. The operator's only way to learn which
	// build this Comb is now running is this line.
	lines := strings.Split(out, "\n")
	idx := -1
	for i, l := range lines {
		if strings.TrimSpace(l) == "ALREADY RESTARTED on this Comb, this run:" {
			idx = i
			break
		}
	}
	if idx < 0 {
		t.Fatalf("no already-restarted block in the output\n---\n%s---", out)
	}
	if idx+1 >= len(lines) {
		t.Fatalf("the already-restarted block has no contents\n---\n%s---", out)
	}
	if got, want := strings.TrimSpace(lines[idx+1]), "apiary_managerd apiary_raftd"; got != want {
		t.Errorf("already-restarted list = %q, want %q: it must name every restart issued, not just the first", got, want)
	}
	if got, want := strings.Join(res.Restarted, " "), "apiary_managerd apiary_raftd"; got != want {
		t.Errorf("Result.Restarted = %q, want %q", got, want)
	}
}

// ---------------------------------------------------------------------------
// 4. raftd's listener must not satisfy managerd's wait, and vice versa.
//    A checker watching "any apiary socket", or one with the two ports
//    transposed, sails straight through this.
// ---------------------------------------------------------------------------

func TestForceRestart_EachServiceIsConfirmedOnItsOwnPort(t *testing.T) {
	// Only raftd is up. managerd's wait must time out on 17700 and
	// raftd must never be restarted, because a plan that confirmed
	// managerd on raftd's port would restart raftd next.
	h := newFakeHost(t, modeRaftdOnly)
	res, out, _ := run(t, nil, h)
	if res.Completed() {
		t.Error("managerd was never listening; the run must fail even though raftd was")
	}
	if got, want := strings.Join(h.restarts(), " "), "apiary_managerd"; got != want {
		t.Errorf("restarted %q, want %q", got, want)
	}
	if !strings.Contains(out, "apiary_managerd did not open its listener port 17700 within 15s.") {
		t.Errorf("managerd's wait accepted raftd's listener\n---\n%s---", out)
	}

	// And the other direction, which is the transposed-ports case: only
	// managerd up, so raftd's wait must time out on 17600 rather than
	// being satisfied by the 17700 that is up.
	h2 := newFakeHost(t, modeManagerdOnly)
	res2, out2, _ := run(t, nil, h2)
	if res2.Completed() {
		t.Error("raftd was never listening; the run must fail")
	}
	if !strings.Contains(out2, "apiary_raftd did not open its listener port 17600 within 15s.") {
		t.Errorf("raftd's wait accepted managerd's listener\n---\n%s---", out2)
	}
}

// ---------------------------------------------------------------------------
// 5. A sockstat that cannot be run is a broken measurement, not a
//    negative one. It must not read as success.
// ---------------------------------------------------------------------------

func TestForceRestart_SockstatFailureIsNotSuccess(t *testing.T) {
	h := newFakeHost(t, modeAll)
	// Every probe fails the same way a real broken sockstat would.
	h.listeningAfter = func(port int, probe int) (bool, error) {
		return false, fmt.Errorf("exec: \"sockstat\": executable file not found in $PATH")
	}
	res, out, _ := run(t, nil, h)

	if res.Completed() {
		t.Fatal("an unanswerable measurement must never confirm a restart")
	}
	var unconfirmed *forcerestart.UnconfirmedError
	if !errors.As(res.Err, &unconfirmed) {
		t.Fatalf("Err = %T (%v), want *forcerestart.UnconfirmedError", res.Err, res.Err)
	}
	if unconfirmed.BrokenProbes != forcerestart.PollProbes {
		t.Errorf("BrokenProbes = %d, want %d: every probe failed and the operator has to be told so",
			unconfirmed.BrokenProbes, forcerestart.PollProbes)
	}
	if !strings.Contains(out, "did not open its listener port 17700 within 15s.") {
		t.Errorf("a broken sockstat must be reported as a timeout, not as success\n---\n%s---", out)
	}
	if !strings.Contains(out, "could not be answered at all") {
		t.Errorf("the operator must be told the measurement was broken, not just that nothing was heard\n---\n%s---", out)
	}
}

// ---------------------------------------------------------------------------
// 6. A row carrying managerd's port without its `host:` delimiter is not
//    a match. This is what a "simplify the pattern" edit breaks
//    silently: the digits are right and only the delimiter is not.
//
//    The rows themselves are asserted in listener_test.go, and the exec
//    that feeds them to that parser is asserted in host_test.go against
//    a real sockstat(8) stand-in. What is left to assert here is that
//    the loop asks the question at all, in both directions - the
//    distinguishing case for a run is the one where a port is up and
//    must still not be accepted for the other service.
// ---------------------------------------------------------------------------

func TestForceRestart_ConfirmedPortsComeOnlyFromTheParser(t *testing.T) {
	// modeRaftdOnly: raftd's port is open and managerd's is not. If the
	// loop were checking ports by anything looser than the real
	// listener, this run would complete and the operator would walk away
	// believing both daemons were confirmed.
	h := newFakeHost(t, modeRaftdOnly)
	res, out, _ := run(t, nil, h)
	if res.Completed() {
		t.Fatal("only raftd is listening; a run that confirms managerd here is trusting something other than a listener")
	}
	if got := strings.Join(res.Confirmed, " "); got != "" {
		t.Errorf("Confirmed = %q, want nothing: no listener was ever confirmed", got)
	}
	if !strings.Contains(out, "apiary_managerd did not open its listener port 17700 within 15s.") {
		t.Errorf("output is missing the managerd timeout\n---\n%s---", out)
	}
}

// ---------------------------------------------------------------------------
// 7. A service with no known listener port is refused before anything at
//    all is restarted, not discovered halfway through the plan.
// ---------------------------------------------------------------------------

func TestForceRestart_UnknownPortIsRefusedBeforeAnythingIsRestarted(t *testing.T) {
	h := newFakeHost(t, modeAll)
	// A third daemon added to the plan tomorrow, with no port recorded.
	plan := append([]forcerestart.Service{
		{Name: "restshimd"},
	}, forcerestart.DefaultPlan...)
	res, out, guardrail := run(t, plan, h)

	if res.Completed() {
		t.Fatal("a plan entry with no known port must not be reported as completed")
	}
	var noPort *forcerestart.ErrNoKnownPort
	if !errors.As(res.Err, &noPort) {
		t.Fatalf("Err = %T (%v), want *forcerestart.ErrNoKnownPort", res.Err, res.Err)
	}
	if noPort.Service != "apiary_restshimd" {
		t.Errorf("the refusal names %q, want %q", noPort.Service, "apiary_restshimd")
	}
	if got := h.restarts(); len(got) != 0 {
		t.Errorf("restarted %v, want nothing: the refusal has to come before the first restart, "+
			"not halfway through with managerd already down", got)
	}
	// Stronger than "no restarts": the port preflight is the FIRST thing
	// Run does, so on this path the host is not touched at all. That
	// includes the leader check, which is the other preflight - a run
	// that asked raftd for its status before checking the plan is
	// asking this Comb a question it did not need to be asked.
	if got := h.lines(); len(got) != 0 {
		t.Errorf("host calls = %v, want none: a plan this build cannot carry out is refused before the Comb is contacted at all", got)
	}
	for _, want := range []string{
		"no known listener port for apiary_restshimd",
		"will not guess one",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output is missing %q\n---\n%s---", want, out)
		}
	}
	if _, ok := readRecord(t, guardrail, "apiary_managerd"); ok {
		t.Error("a record was written for a restart that was never issued")
	}
}

// ---------------------------------------------------------------------------
// 8. THE LEADER REFUSAL.
//
// Restarting raftd on the Colony's leader is the one restart the Colony
// cannot absorb, and force-restart exists to be run by someone who is
// not reading anything first. So the command asks this Comb's own raftd
// who the leader is, and refuses on yes, before the first restart.
//
// Three cases, and the third is the one that would be easy to get
// wrong. "Is this Comb the leader?" and "I could not find out whether
// this Comb is the leader" are different answers, and only the second
// one is what a dead raftd, a wrong socket path in raftd.json and a
// mismatched internal token all look like from here. Treating any of
// them as "no" makes the check decorative: it is exactly the state an
// incident produces, and the one moment the check exists.
// ---------------------------------------------------------------------------

func TestForceRestart_RefusesOnTheLeader(t *testing.T) {
	h := newFakeHost(t, modeAll)
	h.leadership = leadLeader
	res, out, guardrail := run(t, nil, h)

	if res.Completed() {
		t.Fatal("this Comb is the leader; the run must not report completion")
	}
	var isLeader *forcerestart.ErrIsLeader
	if !errors.As(res.Err, &isLeader) {
		t.Fatalf("Err = %T (%v), want *forcerestart.ErrIsLeader", res.Err, res.Err)
	}
	if isLeader.NodeID != "comb-under-test" {
		t.Errorf("the refusal names node %q, want %q: an operator has to be told which voter said yes",
			isLeader.NodeID, "comb-under-test")
	}

	// Nothing was restarted, and - the part a "refused early" message
	// can get wrong - nothing was recorded either. A pending-restart
	// record for a restart that never happened teaches the guardrail's
	// cooldown that a voter is out of service when it is serving.
	if got := h.restarts(); len(got) != 0 {
		t.Errorf("restarted %v, want nothing: this is the whole refusal", got)
	}
	for _, rc := range []string{"apiary_managerd", "apiary_raftd"} {
		if _, ok := readRecord(t, guardrail, rc); ok {
			t.Errorf("a pending-restart record was written for %s on a run that restarted nothing", rc)
		}
	}
	if !res.StoppedBeforeAnyRestart() {
		t.Error("StoppedBeforeAnyRestart() = false, want true: this refusal touched nothing, and an operator's next action turns on that")
	}

	// The check has to have happened, and first. A run that restarts
	// managerd and then discovers it was the leader has already done the
	// one thing the check exists to prevent.
	if got, want := h.lines(), []string{"raftd status"}; len(got) != 1 || got[0] != want[0] {
		t.Errorf("host calls = %v, want exactly %v: the leader is asked about before anything is restarted, not after", got, want)
	}

	// The message has to carry its weight. "refusing" with no reason and
	// no next step is the message an operator works around, so the words
	// an operator acts on are asserted: what is protected, that the
	// Comb is untouched, that the leader is a position rather than a
	// machine, and that there is no flag to get past this.
	for _, want := range []string{
		"force-restart: refusing. This Comb is the Colony's current leader.",
		"Comb `comb-under-test`, raft node `comb-under-test`, raft state Leader",
		"quorum",
		"NOTHING has been restarted",
		"no pending-restart",
		"The leader is a position, not a machine",
		"there is no flag on this command",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output is missing %q\n---\n%s---", want, out)
		}
	}

	// The banner must NOT be here. It says "about to restart", and a
	// refusal printed under it is a message contradicting itself.
	if strings.Contains(out, "about to restart") {
		t.Errorf("the about-to-restart banner was printed on a refused run\n---\n%s---", out)
	}
	if strings.Contains(out, "restart plan") {
		t.Errorf("the restart plan was announced on a run that restarted nothing\n---\n%s---", out)
	}
	assertRefusalLinesFit(t, out)
}

// assertRefusalLinesFit checks the width of the prose in a pre-restart
// refusal. Lines indented by four spaces are exempt, by the convention
// stated above refuseLeader: those are interpolated facts and verbatim
// error text, which are as long as they are, and are set apart from
// the prose precisely so this rule can be applied to the prose alone.
// Everything else wraps at 76, the width used throughout
// internal/forcerestart and internal/manager, and asserted here rather
// than trusted - the leader refusal interpolates two names and the
// other one quotes a gRPC error, and both are the kind of line that
// grows without anyone editing a sentence.
func assertRefusalLinesFit(t *testing.T, out string) {
	t.Helper()
	for _, line := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
		if strings.HasPrefix(line, "    ") {
			continue
		}
		if n := len(line); n > 76 {
			t.Errorf("a line of prose in the refusal is %d columns, over the 76 this package wraps to: %q", n, line)
		}
	}
}

func TestForceRestart_RefusesWhenLeadershipCannotBeDetermined(t *testing.T) {
	h := newFakeHost(t, modeAll)
	h.leadership = leadUnanswerable
	res, out, guardrail := run(t, nil, h)

	if res.Completed() {
		t.Fatal("a Comb whose leadership was never established is not a Comb to restart raftd on")
	}
	var unknown *forcerestart.ErrLeaderUnknown
	if !errors.As(res.Err, &unknown) {
		t.Fatalf("Err = %T (%v), want *forcerestart.ErrLeaderUnknown", res.Err, res.Err)
	}
	// Unwrapping to the real cause is what lets a caller that cares
	// about grpc codes still see them; the refusal is a new fact, not a
	// replacement for the error that caused it.
	if unknown.Unwrap() == nil {
		t.Error("ErrLeaderUnknown.Unwrap() = nil, want the underlying error: the cause is what the operator has to go and fix")
	}

	if got := h.restarts(); len(got) != 0 {
		t.Errorf("restarted %v, want nothing", got)
	}
	if _, ok := readRecord(t, guardrail, "apiary_managerd"); ok {
		t.Error("a pending-restart record was written on a run that restarted nothing")
	}
	if !res.StoppedBeforeAnyRestart() {
		t.Error("StoppedBeforeAnyRestart() = false, want true")
	}

	for _, want := range []string{
		"Could not tell whether this Comb is the",
		"not the same answer as \"not the leader\"",
		"NOTHING has been restarted",
		"Check that apiary_raftd is running",
		"is this Comb's own",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output is missing %q\n---\n%s---", want, out)
		}
	}
	// Every paragraph in this file is hard-wrapped, and this one embeds
	// a path, so it is the one that can grow a line without anyone
	// editing a sentence. A diagnostic an operator reads on a terminal
	// is 76 columns by convention here, and the assertion is on that
	// rather than on taste. It covers the whole refusal, both this one
	// and the leader one, because they share a house style.
	assertRefusalLinesFit(t, out)
	// The two refusals are deliberately different messages, because
	// their next actions are: this one is a broken or absent raftd to go
	// and look at, not a leadership problem to wait out.
	if strings.Contains(out, "position, not a machine") {
		t.Errorf("an unanswerable leader check gave the operator the follower advice, which is advice for a different problem\n---\n%s---", out)
	}
}

func TestForceRestart_AsksOnceAndOnlyOnce(t *testing.T) {
	// A leader check that retried would be a leader check that takes
	// longer to say no, on a command whose whole value is that it
	// either says no immediately or does the work.
	//
	// The count is taken over a full successful run rather than a
	// refusal, because a refusal returns before a retry loop could have
	// had a second go: a second question placed inside the per-service
	// loop would only ever be visible here. One is the number, because
	// the answer can change between the start of a run and the restart
	// of raftd four seconds later, and re-reading it mid-plan would
	// either start a plan this Comb may no longer be allowed to finish
	// or abandon one it is already halfway through.
	h := newFakeHost(t, modeAll)
	res, _, _ := run(t, nil, h)
	if !res.Completed() {
		t.Fatalf("this case is about a completed run, got %v", res.Err)
	}
	asked := 0
	for _, l := range h.lines() {
		if l == "raftd status" {
			asked++
		}
	}
	if asked != 1 {
		t.Errorf("the leader check was asked %d times, want exactly 1", asked)
	}
}
