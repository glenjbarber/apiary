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

// fakeHost is the recording Host. Every call appends one line to log,
// so "the record was written before the restart" and "raftd was never
// touched after managerd timed out" are both read off a single ordered
// artifact rather than inferred from exit status and prose.
type fakeHost struct {
	t    *testing.T
	log  *strings.Builder
	mode mode

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
// no method that asks a service for its status. The structural absence
// is the regression, and the log assertion below is a backstop against
// a future method being added that would reintroduce it.
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

	// No status call was issued - and the interface has no way to make
	// one, which is the durable form of this assertion.
	for _, l := range h.lines() {
		if strings.Contains(l, " status") {
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
	// path it is four calls and nothing else. A wait that slept between
	// a service that was already up would show up here as an extra
	// line, and that is a real defect: the wait costs a second per
	// restart whether or not anything needs waiting for.
	wantLog := []string{
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
