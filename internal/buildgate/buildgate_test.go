package buildgate

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// These cover the gate's decision surface, from both ends: Compare is the
// whole verdict table with no filesystem involved, and Confirm is the
// same table driven off real files and real executable scripts standing
// in for the four daemons.
//
// The fake binaries are shell scripts rather than compiled Go on purpose.
// A test double that is itself built from the same stamping machinery
// would agree with the gate by construction, and the property under test
// is that the gate reads what a PROCESS SAYS, not what a struct holds.

// A log line as a real daemon writes it, both the pre-stamping shape and
// the post-stamping shape.
const stampedLine = `2026/09/26 16:00:21 raftd: build=9c43262d358a ` +
	`commit=9c43262d358a built=2026-09-26T11:52:03-04:00 go=freebsd/amd64 ` +
	`listening on /var/run/apiary/raftd.sock (node-id=brood.lab3.home.arpa, raft-tls=false)`

const unstampedLine = `2026/09/26 02:03:23 raftd: listening on ` +
	`/var/run/apiary/raftd.sock (node-id=brood.lab3.home.arpa, raft-tls=false)`

// ---------------------------------------------------------------------------
// Compare: the verdict table, with no Comb involved.
// ---------------------------------------------------------------------------

func TestCompareIsTheWholeVerdictTable(t *testing.T) {
	// Every row is a claim this gate makes to a controller about what is
	// actually running on a Comb.
	cases := []struct {
		name          string
		onDisk, run   string
		want          Status
		wantConfirmed bool
	}{
		{"clean agreement", "9c43262d358a", "9c43262d358a", TookEffect, true},
		{"clean disagreement", "b8e74c328d9f", "9c43262d358a", RunningStale, false},
		// The downgrade: matching -dirty ids agree about the commit and
		// say nothing reliable about the bytes, so they must not confirm.
		{"dirty agreement does not confirm", "9c43262d358a-dirty", "9c43262d358a-dirty", DirtyIDMatch, false},
		// ...but a dirty id that differs is still a plain mismatch.
		{"dirty disagreement is still a mismatch", "9c43262d358a-dirty", "b8e74c328d9f", RunningStale, false},
		// A clean id on disk with a dirty one running is a mismatch, not
		// a downgrade: the sources genuinely differ.
		{"clean vs dirty is a mismatch", "9c43262d358a", "9c43262d358a-dirty", RunningStale, false},
		// The two no-evidence rows. A missing id on EITHER side is no
		// evidence, and neither may be reported as a difference.
		{"no running id is unobserved", "b8e74c328d9f", "", Unobserved, false},
		{"no on-disk id is unobserved", "", "9c43262d358a", Unobserved, false},
		{"neither id is unobserved", "", "", Unobserved, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := Compare(c.onDisk, c.run); got != c.want {
				t.Errorf("Compare(%q, %q) = %q, want %q", c.onDisk, c.run, got, c.want)
			}
		})
	}
}

// TestCompareNeverConfirmsWithoutTwoIds is the fail-open guard, stated
// directly. Every input that is not a clean two-id match must be refused,
// because a gate that cannot read its evidence must not report success.
func TestCompareNeverConfirmsWithoutTwoIds(t *testing.T) {
	inputs := [][2]string{
		{"9c43262d358a", "9c43262d358a-dirty"},
		{"9c43262d358a-dirty", "9c43262d358a"},
		{"9c43262d358a-dirty", "9c43262d358a-dirty"},
		{"9c43262d358a", ""},
		{"", "9c43262d358a"},
		{"", ""},
		{"9c43262d358a", "b8e74c328d9f"},
	}
	for _, in := range inputs {
		got := Compare(in[0], in[1])
		if got.Confirms() {
			t.Errorf("Compare(%q, %q) = %q, which confirms - the only confirming input is two identical clean ids",
				in[0], in[1], got)
		}
	}
	if !Compare("9c43262d358a", "9c43262d358a").Confirms() {
		t.Error("two identical clean ids do not confirm, so the gate can never pass")
	}
}

// TestStatusConfirmsIsTrueForExactlyOneStatus keeps Confirms a single
// source of truth. A caller switching on Status directly would bypass
// every guard the method carries, so the set has to stay this narrow.
func TestStatusConfirmsIsTrueForExactlyOneStatus(t *testing.T) {
	all := []Status{TookEffect, DirtyIDMatch, RunningStale, Unobserved, NotRunning}
	confirming := 0
	for _, s := range all {
		if s.Confirms() {
			confirming++
			if s != TookEffect {
				t.Errorf("%q confirms, want only %q to", s, TookEffect)
			}
		}
	}
	if confirming != 1 {
		t.Errorf("%d statuses confirm, want exactly 1", confirming)
	}
}

// TestStatusStringsAreDistinct guards against two verdicts collapsing to
// the same string, which would make a report silently lie.
func TestStatusStringsAreDistinct(t *testing.T) {
	all := map[Status]bool{}
	for _, s := range []Status{TookEffect, DirtyIDMatch, RunningStale, Unobserved, NotRunning} {
		if all[s] {
			t.Errorf("duplicate status %q", s)
		}
		all[s] = true
		if s == "" {
			t.Error("a status has an empty string, which is indistinguishable from unset")
		}
	}
}

// ---------------------------------------------------------------------------
// The evidence readers.
// ---------------------------------------------------------------------------

func writeLog(t *testing.T, lines ...string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "raftd.log")
	if err := os.WriteFile(p, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestBuildLineRegex(t *testing.T) {
	m := buildLine.FindStringSubmatch(stampedLine)
	if m == nil {
		t.Fatal("no match on a stamped line")
	}
	if want := "9c43262d358a"; m[1] != want {
		t.Errorf("build id = %q, want %q", m[1], want)
	}
	// The critical negative: a pre-stamping line must NOT yield a build
	// id. Returning "" here is what lets the caller say "unobserved"
	// instead of inventing agreement.
	if m := buildLine.FindStringSubmatch(unstampedLine); m != nil {
		t.Errorf("no match wanted on an unstamped line, got %q", m[1])
	}
}

func TestRunningBuildTakesTheMostRecentLine(t *testing.T) {
	// A log with a restart in it: the answer must be the LAST line, or a
	// Comb restarted with a new build and the gate reports the old one as
	// still running.
	//
	// BOTH lines carry a build id, deliberately. A first line without one
	// would leave the loop with a single write to last, and an
	// implementation that kept the FIRST line instead would produce the
	// same answer - so the earlier version of this test could not tell
	// the two apart, which is exactly what a mutation of this line
	// showed.
	older := `2026/09/26 02:03:23 raftd: build=a9879963600c ` +
		`commit=a9879963600c built=2026-09-26T09:14:00-04:00 go=freebsd/amd64 ` +
		`listening on /var/run/apiary/raftd.sock (node-id=brood)`
	p := writeLog(t, older, stampedLine)
	got, err := runningBuild(p, "raftd")
	if err != nil {
		t.Fatal(err)
	}
	if want := "9c43262d358a"; got != want {
		t.Errorf("runningBuild() = %q, want the most recent %q, not the first", got, want)
	}

	// The same log with the lines the other way round, to be sure the
	// answer tracks POSITION and not the value: both ids are stamped, so
	// a comparison that happened to prefer the newer id would pass the
	// case above by luck.
	reversed := writeLog(t, stampedLine, older)
	got, err = runningBuild(reversed, "raftd")
	if err != nil {
		t.Fatal(err)
	}
	if want := "a9879963600c"; got != want {
		t.Errorf("runningBuild() = %q, want the last line's %q - the answer follows position, not the value",
			got, want)
	}
}

func TestRunningBuildEmptyOnPreStampingLog(t *testing.T) {
	p := writeLog(t, unstampedLine)
	got, err := runningBuild(p, "raftd")
	if err != nil {
		t.Fatalf("err = %v, want nil: a log that predates stamping is a readable fact", err)
	}
	if got != "" {
		t.Errorf("runningBuild() = %q, want \"\" so the caller reports unobserved", got)
	}
}

func TestRunningBuildIgnoresOtherServices(t *testing.T) {
	// managerd's line must not be mistaken for raftd's. Both appear in
	// their own separate files in production, but a combined log is
	// exactly the kind of thing a future -log flag would produce.
	// The other service's line comes LAST. With it first, the raftd line
	// overwrote it and the filter was never exercised - an
	// implementation that ignored the service name entirely produced the
	// same answer, which is what a mutation of this filter showed.
	managerdLine := `2026/09/26 16:00:21 managerd: build=managerd-build ` +
		`listening on 0.0.0.0:17700 (node-id=brood)`
	p := writeLog(t, stampedLine, managerdLine)
	got, err := runningBuild(p, "raftd")
	if err != nil {
		t.Fatal(err)
	}
	if got != "9c43262d358a" {
		t.Errorf("runningBuild(raftd) = %q, want %q - the last line is managerd's and must not be read as raftd's",
			got, "9c43262d358a")
	}
	// And the mirror: asking for managerd out of the same combined log
	// must find managerd's line, not raftd's.
	got, err = runningBuild(p, "managerd")
	if err != nil {
		t.Fatal(err)
	}
	if got != "managerd-build" {
		t.Errorf("runningBuild(managerd) = %q, want %q", got, "managerd-build")
	}
}

func TestRunningBuildMissingFileIsAnError(t *testing.T) {
	// A missing log is "cannot tell", not "no build". The caller turns
	// this into NotRunning, and conflating it with TookEffect is the
	// failure this gate exists to avoid.
	_, err := runningBuild(filepath.Join(t.TempDir(), "nope.log"), "raftd")
	if err == nil {
		t.Fatal("err = nil for a missing log, want an error so the status is not-running")
	}
	if !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("err = %v, want a not-exist error so the caller can tell 'not running' from 'cannot read'", err)
	}
}

// TestRunningBuildSurvivesALineLongerThanTheScannerBuffer. A startup
// line carrying a long list runs past bufio's default 64 KiB, and a
// scanner that stopped there would truncate the log at exactly the point
// where the answer is - silently, because a short read is not an error.
func TestRunningBuildSurvivesALongLine(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "raftd.log")
	padding := strings.Repeat("x", 100*1024)
	line := "2026/09/26 16:00:21 raftd: build=9c43262d358a listening on /var/run/apiary/raftd.sock (pad=" + padding + ")"
	if err := os.WriteFile(p, []byte(line+"\n"+stampedLine+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := runningBuild(p, "raftd")
	if err != nil {
		t.Fatalf("err = %v, want nil: an over-long line must not abort the scan", err)
	}
	if want := "9c43262d358a"; got != want {
		t.Errorf("runningBuild() = %q, want %q - the last line was missed", got, want)
	}
}

func TestIsUndefinedFlagText(t *testing.T) {
	// The message a pre-stamping binary produces, written to stderr by
	// the flag package. Every Comb deployed before this change hits it,
	// and reporting it as a generic unobserved would hide the fact that
	// the answer becomes knowable once the binary is rebuilt.
	if !isUndefinedFlagText("flag provided but not defined: -version\nUsage of ...") {
		t.Error("did not recognise the flag package's rejection")
	}
	if isUndefinedFlagText("permission denied") {
		t.Error("matched a non-flag message")
	}
}

// TestOnDiskBuildDistinguishesPredatingBinary is the real regression,
// found against a live brood deployment: a pre-stamping binary exits
// non-zero AND prints the rejection to stderr, and returning early on err
// hid the specific cause behind a generic error, so every Comb read
// "unknown".
func TestOnDiskBuildDistinguishesPredatingBinary(t *testing.T) {
	dir := t.TempDir()
	// A shell script standing in for a pre-stamping binary: non-zero
	// exit plus the flag package's message on stderr.
	script := filepath.Join(dir, "fakeraftd")
	body := "#!/bin/sh\necho 'flag provided but not defined: -version' >&2\necho 'Usage of fakeraftd:' >&2\nexit 2\n"
	if err := os.WriteFile(script, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	_, err := onDiskBuild(script)
	if !errors.Is(err, errUndefinedFlag) {
		t.Errorf("onDiskBuild() err = %v, want errUndefinedFlag", err)
	}
}

// TestOnDiskBuildReturnsADirtyIDWhole. The id must survive intact through
// the regex, suffix included. If a -dirty id were truncated or mangled, the
// downgrade would never trigger and two genuinely different dirty builds
// would report agreement.
func TestOnDiskBuildReturnsADirtyIDWhole(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "fakerd")
	body := "#!/bin/sh\necho 'raftd build=9c43262d358a-dirty commit=9c43262d358a built=2026-09-26T11:52:03-04:00 go=freebsd/amd64'\n"
	if err := os.WriteFile(script, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	got, err := onDiskBuild(script)
	if err != nil {
		t.Fatalf("onDiskBuild() err = %v", err)
	}
	if want := "9c43262d358a-dirty"; got != want {
		t.Errorf("onDiskBuild() = %q, want the full id %q including its -dirty suffix", got, want)
	}
	if Compare(got, got) != DirtyIDMatch {
		t.Error("a matching -dirty id did not produce the downgraded status")
	}
}

// TestOnDiskBuildReportsAnUnstampedBinarySeparately. A binary that runs,
// exits cleanly and prints no build id is a stamped build with no id
// injected. It is a different fact from one that could not be run at all,
// and the two demand different fixes.
func TestOnDiskBuildReportsAnUnstampedBinarySeparately(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "fakerd")
	body := "#!/bin/sh\necho 'raftd: no flags here, just starting up'\n"
	if err := os.WriteFile(script, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	_, err := onDiskBuild(script)
	if !errors.Is(err, errNoBuildID) {
		t.Errorf("onDiskBuild() err = %v, want errNoBuildID", err)
	}
}

// ---------------------------------------------------------------------------
// Confirm: the whole gate, off real files.
// ---------------------------------------------------------------------------

// comb is a fake Comb: a libexec directory and a log directory, both in
// one temp dir, that a test writes daemons into.
type comb struct {
	paths  Paths
	script string
}

func newComb(t *testing.T) *comb {
	t.Helper()
	dir := t.TempDir()
	return &comb{
		paths:  Paths{LibexecDir: filepath.Join(dir, "libexec"), LogDir: filepath.Join(dir, "log")},
		script: "#!/bin/sh\n",
	}
}

// install writes a fake daemon that answers -version with buildID. An
// empty buildID writes a binary that prints nothing at all, standing in
// for one built without -ldflags.
func (c *comb) install(t *testing.T, name, buildID string) {
	t.Helper()
	if err := os.MkdirAll(c.paths.LibexecDir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := c.script
	if buildID != "" {
		body += "echo '" + name + " build=" + buildID + " commit=" + buildID +
			" built=2026-09-26T11:52:03-04:00 go=freebsd/amd64'\n"
	} else {
		body += "echo '" + name + ": starting up'\n"
	}
	if err := os.WriteFile(c.paths.binary(name), []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
}

// start writes a startup log line claiming buildID is running. An empty
// buildID writes a pre-stamping line with no build id at all.
func (c *comb) start(t *testing.T, name, buildID string) {
	t.Helper()
	if err := os.MkdirAll(c.paths.LogDir, 0o755); err != nil {
		t.Fatal(err)
	}
	line := "2026/09/26 16:00:21 " + name + ": listening on /var/run/apiary/" + name + ".sock (node-id=brood)"
	if buildID != "" {
		line = "2026/09/26 16:00:21 " + name + ": build=" + buildID +
			" commit=" + buildID + " built=2026-09-26T11:52:03-04:00 go=freebsd/amd64" +
			" listening on /var/run/apiary/" + name + ".sock (node-id=brood)"
	}
	if err := os.WriteFile(c.paths.log(name), []byte(line+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// deployAll puts every service on the same build and starts it, which is
// the one state in which a Comb may be called confirmed.
func (c *comb) deployAll(t *testing.T, buildID string) {
	t.Helper()
	for _, name := range Services {
		c.install(t, name, buildID)
		c.start(t, name, buildID)
	}
}

func TestConfirmOnAFullyDeployedComb(t *testing.T) {
	c := newComb(t)
	c.deployAll(t, "9c43262d358a")

	report := Confirm("brood.lab3.home.arpa", c.paths)

	if !report.Confirmed() {
		t.Fatalf("Confirmed() = false on a Comb where all four daemons run the on-disk build:\n%+v", report.Services)
	}
	if report.NodeID != "brood.lab3.home.arpa" {
		t.Errorf("NodeID = %q, want the Comb it was asked about", report.NodeID)
	}
	if report.Paths != c.paths {
		t.Errorf("Paths = %+v, want %+v so a report can be traced to what it read", report.Paths, c.paths)
	}
	if len(report.Services) != len(Services) {
		t.Fatalf("Services = %d, want %d - all four daemons matter", len(report.Services), len(Services))
	}
	for i, service := range report.Services {
		if service.Name != Services[i] {
			t.Errorf("Services[%d] = %q, want %q: the order is the reporting order and a caller depends on it",
				i, service.Name, Services[i])
		}
		if service.Status != TookEffect {
			t.Errorf("%s: Status = %q, want %q", service.Name, service.Status, TookEffect)
		}
		if service.Reason != ReasonNone {
			t.Errorf("%s: Reason = %q, want none", service.Name, service.Reason)
		}
		if service.Detail == "" {
			t.Errorf("%s: Detail is empty, want it always populated", service.Name)
		}
		if !service.Confirms() {
			t.Errorf("%s: Confirms() = false with Status %q, want true", service.Name, service.Status)
		}
	}
	if _, found := report.FirstStop(); found {
		t.Error("FirstStop() reported a stop on a fully confirmed Comb")
	}
	if got := report.Stops(); len(got) != 0 {
		t.Errorf("Stops() = %+v, want none", got)
	}
}

// TestConfirmCoversAllFourDaemons. raftd is the one most likely to be
// stale, because make update installs it without restarting it; frontend
// and restshimd are the two make update DOES restart, so a Comb half way
// through a sweep can have those two new and the other two old. A gate
// that watched only managerd and raftd would call that Comb confirmed.
func TestConfirmCoversAllFourDaemons(t *testing.T) {
	want := map[string]bool{"raftd": true, "managerd": true, "frontend": true, "restshimd": true}
	for _, name := range Services {
		delete(want, name)
	}
	for name := range want {
		t.Errorf("%s is not in Services, so the gate never checks it", name)
	}
	if len(Services) != 4 {
		t.Errorf("Services has %d entries, want 4", len(Services))
	}
}

// TestConfirmCatchesTheMixedState is the case the whole gate exists for,
// reproduced from what actually happened on 2026-09-27: make update
// installed the new build for everything and restarted only frontend and
// restshimd, so managerd and raftd were left running the previous build
// while every binary on disk was new.
func TestConfirmCatchesTheMixedState(t *testing.T) {
	c := newComb(t)
	const old, current = "a9879963600c", "107fdf6b414a"
	// Everything installed at the new build...
	for _, name := range Services {
		c.install(t, name, current)
	}
	// ...and only the two make update restarts came up on it.
	c.start(t, "frontend", current)
	c.start(t, "restshimd", current)
	c.start(t, "managerd", old)
	c.start(t, "raftd", old)

	report := Confirm("brood.lab3.home.arpa", c.paths)

	if report.Confirmed() {
		t.Fatal("Confirmed() = true on a mixed-state Comb: two daemons are running the previous build")
	}
	stop, found := report.FirstStop()
	if !found {
		t.Fatal("FirstStop() found nothing, want raftd - the first service in order and the one most likely stale")
	}
	if stop.Name != "raftd" {
		t.Errorf("FirstStop() = %q, want raftd", stop.Name)
	}
	if stop.Status != RunningStale {
		t.Errorf("raftd: Status = %q, want %q", stop.Status, RunningStale)
	}
	if stop.OnDisk != current || stop.Running != old {
		t.Errorf("raftd: OnDisk/Running = %q/%q, want %q/%q - both ids travel with the verdict so a reader need not re-derive it",
			stop.OnDisk, stop.Running, current, old)
	}
	// The evidence the operator would otherwise have to go and collect.
	for _, want := range []string{"running " + old, "on disk " + current, "restart"} {
		if !strings.Contains(stop.Detail, want) {
			t.Errorf("raftd: Detail = %q, want it to mention %q", stop.Detail, want)
		}
	}
	if len(report.Stops()) != 2 {
		t.Errorf("Stops() = %d services, want 2 (managerd and raftd)", len(report.Stops()))
	}
	// And the two that DID take effect are still reported as such, so a
	// caller can say which half of the sweep landed.
	frontend, _ := report.Service("frontend")
	if frontend.Status != TookEffect {
		t.Errorf("frontend: Status = %q, want %q", frontend.Status, TookEffect)
	}
}

// TestConfirmSeparatesNoEvidenceFromNoEffect is the central refusal. The
// two look alike from outside and demand opposite handling from a
// reader: one means the gate could not find out, the other means it found
// out that the restart did not happen. Collapsing them is how a gate ends
// up reporting success it never established.
func TestConfirmSeparatesNoEvidenceFromNoEffect(t *testing.T) {
	c := newComb(t)
	c.install(t, "raftd", "9c43262d358a")
	c.start(t, "raftd", "9c43262d358a")
	// managerd: installed at the new build, still running the old one.
	c.install(t, "managerd", "107fdf6b414a")
	c.start(t, "managerd", "a9879963600c")
	// frontend: running, but from a build that predates stamping, so it
	// cannot say what it is at all.
	c.install(t, "frontend", "107fdf6b414a")
	c.start(t, "frontend", "")
	// restshimd: never started, so there is no log.
	c.install(t, "restshimd", "107fdf6b414a")

	report := Confirm("drone.lab3.home.arpa", c.paths)

	// raftd is the control: the one service that DID take effect. The
	// other three are the three different ways it can fail to, and they
	// must be three different answers rather than one "not ok".
	want := map[string]struct {
		status   Status
		reason   Reason
		confirms bool
	}{
		"raftd":     {TookEffect, ReasonNone, true},
		"managerd":  {RunningStale, ReasonNone, false},
		"frontend":  {Unobserved, ReasonLogPredatesStamping, false},
		"restshimd": {NotRunning, ReasonLogMissing, false},
	}
	seen := map[Status]bool{}
	for name, expect := range want {
		service, found := report.Service(name)
		if !found {
			t.Fatalf("%s is missing from the report entirely", name)
		}
		if service.Status != expect.status {
			t.Errorf("%s: Status = %q, want %q", name, service.Status, expect.status)
		}
		if service.Reason != expect.reason {
			t.Errorf("%s: Reason = %q, want %q", name, service.Reason, expect.reason)
		}
		if service.Status.Confirms() != expect.confirms {
			t.Errorf("%s: Status %q confirms = %v, want %v", name, service.Status,
				service.Status.Confirms(), expect.confirms)
		}
		if seen[service.Status] {
			t.Errorf("%s: Status %q appears twice, want the three failure shapes to stay distinct",
				name, service.Status)
		}
		seen[service.Status] = true
	}
	if report.Confirmed() {
		t.Error("Confirmed() = true with one stale, one unobserved and one not-running daemon")
	}
	// The unobserved row is the one that must never be mistaken for the
	// stale row: it carries no running id, because none could be read.
	frontend, _ := report.Service("frontend")
	if frontend.Running != "" {
		t.Errorf("frontend: Running = %q, want empty - nothing could be read, and inventing an id here is the bug", frontend.Running)
	}
	if frontend.OnDisk != "107fdf6b414a" {
		t.Errorf("frontend: OnDisk = %q, want the id that WAS readable carried through", frontend.OnDisk)
	}
}

// TestConfirmUnobservedIsNotSuccessInAnyShape drives the same four
// services through every shape the evidence can be missing, and requires
// that none of them confirms. This is the property a caller relies on
// most and the one a fail-open implementation loses first.
func TestConfirmUnobservedIsNotSuccessInAnyShape(t *testing.T) {
	shapes := []struct {
		name       string
		setup      func(t *testing.T, c *comb, service string)
		want       Status
		wantReason Reason
	}{
		{"binary missing", func(t *testing.T, c *comb, service string) {
			c.start(t, service, "9c43262d358a")
		}, Unobserved, ReasonBinaryMissing},
		{"binary predates -version", func(t *testing.T, c *comb, service string) {
			c.script = "#!/bin/sh\necho 'flag provided but not defined: -version' >&2\nexit 2\n"
			c.install(t, service, "")
			c.script = "#!/bin/sh\n"
			c.start(t, service, "9c43262d358a")
		}, Unobserved, ReasonBinaryPredatesVersionFlag},
		{"binary unstamped", func(t *testing.T, c *comb, service string) {
			c.install(t, service, "")
			c.start(t, service, "9c43262d358a")
		}, Unobserved, ReasonBinaryUnstamped},
		{"binary not executable", func(t *testing.T, c *comb, service string) {
			c.install(t, service, "9c43262d358a")
			if err := os.Chmod(c.paths.binary(service), 0o600); err != nil {
				t.Fatal(err)
			}
			c.start(t, service, "9c43262d358a")
		}, Unobserved, ReasonBinaryUnreadable},
		{"log predates stamping", func(t *testing.T, c *comb, service string) {
			c.install(t, service, "9c43262d358a")
			c.start(t, service, "")
		}, Unobserved, ReasonLogPredatesStamping},
		{"log missing", func(t *testing.T, c *comb, service string) {
			c.install(t, service, "9c43262d358a")
		}, NotRunning, ReasonLogMissing},
	}

	for _, shape := range shapes {
		for _, service := range Services {
			t.Run(shape.name+"/"+service, func(t *testing.T) {
				c := newComb(t)
				// Every OTHER service is fully deployed, so the only
				// thing making the Comb unconfirmed is the shape under
				// test. Without this, a gate that reported
				// unconfirmed for unrelated reasons would pass.
				for _, other := range Services {
					if other != service {
						c.install(t, other, "9c43262d358a")
						c.start(t, other, "9c43262d358a")
					}
				}
				shape.setup(t, c, service)

				report := Confirm("brood.lab3.home.arpa", c.paths)
				got, found := report.Service(service)
				if !found {
					t.Fatalf("%s is missing from the report", service)
				}
				if got.Status != shape.want {
					t.Errorf("Status = %q, want %q", got.Status, shape.want)
				}
				// The exact reason, not merely "some reason". Two causes
				// of unobserved need different fixes - rebuild the binary
				// against a git checkout that is not read-only, versus
				// find the pid - and a test that accepted any non-empty
				// reason let a mutation that swapped one for another pass
				// untouched.
				if got.Reason != shape.wantReason {
					t.Errorf("Reason = %q, want %q", got.Reason, shape.wantReason)
				}
				if got.Status.Confirms() {
					t.Errorf("Status %q confirms on a Comb with no readable evidence", got.Status)
				}
				if report.Confirmed() {
					t.Errorf("Confirmed() = true with %s in state %q", service, got.Status)
				}
				if got.Reason == ReasonNone {
					t.Errorf("Reason is none, want the specific cause - a reader triaging a stalled sweep needs to " +
						"tell 'never started' from 'could not be read'")
				}
				if got.Detail == "" {
					t.Error("Detail is empty, want it always populated")
				}
			})
		}
	}
}

// TestConfirmRefusesADirtyComb is the strictness the CLI does not have and
// the gate does. versioncheck's exit status is 0 for a matching pair of
// -dirty ids, because "these two agree about the commit" is a true thing
// to report to an operator. A gate is asked a different question - did
// the new BUILD take effect - and a dirty id does not describe bytes, so
// it cannot answer that. Confirm and versioncheck therefore disagree
// here on purpose, and this test is what makes the disagreement
// deliberate rather than accidental.
func TestConfirmRefusesADirtyComb(t *testing.T) {
	c := newComb(t)
	c.deployAll(t, "9c43262d358a-dirty")

	report := Confirm("brood.lab3.home.arpa", c.paths)
	if report.Confirmed() {
		t.Fatal("Confirmed() = true on a -dirty build, want false: the ids agree about the commit and not about the bytes")
	}
	stop, found := report.FirstStop()
	if !found || stop.Status != DirtyIDMatch {
		t.Fatalf("FirstStop() = %+v, want a DirtyIDMatch", stop)
	}
	if stop.Reason != ReasonDirtyBuild {
		t.Errorf("Reason = %q, want %q", stop.Reason, ReasonDirtyBuild)
	}
	// The sentence must say what is missing, or it is just a different
	// string rather than a more honest answer.
	for _, want := range []string{"dirty", "sha256"} {
		if !strings.Contains(stop.Detail, want) {
			t.Errorf("Detail = %q, want it to mention %q", stop.Detail, want)
		}
	}
}

// TestConfirmNeverReadsTheNetwork is a property of the locality claim the
// package makes. Paths is two directory names and there is no way to put
// a host in it, so the claim is checked the only way it can be: by
// asserting that there is nothing in the exported surface which could
// carry one, and that a report produced with the production default paths
// names those paths rather than anything else.
//
// The substantive part of the claim - that no code path dials - is a
// property of the source, and the test that would prove it is a network
// sandbox, not something a unit test in this repository can provide.
func TestConfirmNeverReadsTheNetwork(t *testing.T) {
	p := Paths{}
	if p.LibexecDir != "" || p.LogDir != "" {
		t.Errorf("the zero Paths = %+v, want empty - a gate that defaulted to a readable location would be a "+
			"gate that silently answers about something", p)
	}
	d := DefaultPaths()
	if d.LibexecDir != DefaultLibexecDir || d.LogDir != DefaultLogDir {
		t.Errorf("DefaultPaths() = %+v, want the Makefile's own install and log directories", d)
	}
	// A report carries the paths it read, so a misroute is visible.
	report := Report{NodeID: "brood", Paths: d}
	if report.Paths != d {
		t.Errorf("Report.Paths = %+v, want %+v", report.Paths, d)
	}
}

// TestServiceConfirmsTracksItsStatus exists because the method is a
// one-line delegation and a delegation is exactly the kind of thing a
// test that only ever sees the happy path leaves unpinned. An earlier
// version of this file confirmed every service on a fully deployed Comb,
// where the correct answer and "always true" are the same value.
func TestServiceConfirmsTracksItsStatus(t *testing.T) {
	c := newComb(t)
	c.deployAll(t, "9c43262d358a")
	c.start(t, "managerd", "a9879963600c")

	report := Confirm("brood.lab3.home.arpa", c.paths)
	for _, service := range report.Services {
		if got, want := service.Confirms(), service.Status == TookEffect; got != want {
			t.Errorf("%s: Confirms() = %v with status %q, want %v", service.Name, got, service.Status, want)
		}
		if service.Confirms() != service.Status.Confirms() {
			t.Errorf("%s: Confirms() and Status.Confirms() disagree, want the method to be a faithful delegation",
				service.Name)
		}
	}
	// Named explicitly too, so the shape of the argument is obvious: a
	// Service with no status at all is not a confirmation.
	if (Service{}).Confirms() {
		t.Error("the zero Service confirms")
	}
	if (Service{Status: Unobserved}).Confirms() {
		t.Error("a Service with status unobserved confirms")
	}
	if (Service{Status: NotRunning}).Confirms() {
		t.Error("a Service with status not-running confirms")
	}
	if (Service{Status: RunningStale}).Confirms() {
		t.Error("a Service with status running-stale confirms")
	}
	if (Service{Status: DirtyIDMatch}).Confirms() {
		t.Error("a Service with status dirty-id-match confirms")
	}
	if !(Service{Status: TookEffect}).Confirms() {
		t.Error("a Service with status took-effect does not confirm")
	}
}

// TestReportConfirmedIsFalseForAnEmptyReport closes the fail-open hole an
// empty set of answers would leave. It is reachable by any caller that
// builds a Report by hand, and "no evidence at all" is precisely the
// shape that must not read as permission.
func TestReportConfirmedIsFalseForAnEmptyReport(t *testing.T) {
	var report Report
	if report.Confirmed() {
		t.Error("Confirmed() = true on a report with no services in it")
	}
	if _, found := report.FirstStop(); found {
		t.Error("FirstStop() reported a stop on an empty report; there is nothing to name")
	}
	if got := report.Stops(); got != nil {
		t.Errorf("Stops() = %+v, want nil", got)
	}
	if _, found := report.Service("raftd"); found {
		t.Error("Service() found raftd in an empty report")
	}
}

// TestReportServiceLookupIsByNameNotPosition. A caller asking about
// managerd must never be handed raftd's answer. A misroute here reads as
// a confirmed gate on a daemon nobody checked.
func TestReportServiceLookupIsByNameNotPosition(t *testing.T) {
	c := newComb(t)
	c.deployAll(t, "9c43262d358a")
	c.install(t, "managerd", "107fdf6b414a")
	c.start(t, "managerd", "a9879963600c")

	report := Confirm("brood.lab3.home.arpa", c.paths)

	managerd, found := report.Service("managerd")
	if !found {
		t.Fatal("Service(managerd) not found")
	}
	if managerd.Status != RunningStale {
		t.Errorf("Service(managerd).Status = %q, want %q - raftd's verdict was returned for managerd",
			managerd.Status, RunningStale)
	}
	if raftd, _ := report.Service("raftd"); raftd.Status != TookEffect {
		t.Errorf("Service(raftd).Status = %q, want %q", raftd.Status, TookEffect)
	}
	if _, found := report.Service("nosuchservice"); found {
		t.Error("Service(nosuchservice) found a service that was never checked")
	}
}

// TestReportCarriesEveryDetail says an unexplained verdict is not
// actionable. Every service in every shape gets a sentence, and a
// verdict with no id in it never renders a blank where a value would go.
func TestReportCarriesEveryDetail(t *testing.T) {
	c := newComb(t)
	c.install(t, "raftd", "9c43262d358a")
	c.start(t, "raftd", "9c43262d358a")
	c.install(t, "managerd", "107fdf6b414a")
	c.start(t, "managerd", "a9879963600c")
	c.install(t, "frontend", "107fdf6b414a")
	c.start(t, "frontend", "")
	c.install(t, "restshimd", "107fdf6b414a")

	report := Confirm("brood.lab3.home.arpa", c.paths)
	for _, service := range report.Services {
		if service.Detail == "" {
			t.Errorf("%s: Detail is empty", service.Name)
		}
		if strings.Contains(service.Detail, "running  ") || strings.Contains(service.Detail, "on disk  ") {
			t.Errorf("%s: Detail = %q, has a blank where a missing id is shown - a blank reads as a value", service.Name, service.Detail)
		}
		if strings.Contains(service.Detail, "none recorded") && service.Running != "" {
			t.Errorf("%s: Detail = %q, claims no running id while Running = %q", service.Name, service.Detail, service.Running)
		}
	}
}

// TestConfirmIsRepeatable pins the shape of the report: two calls on the
// same unchanged Comb produce the same verdicts in the same order, so a
// caller polling a step sees no flicker.
func TestConfirmIsRepeatable(t *testing.T) {
	c := newComb(t)
	c.deployAll(t, "9c43262d358a")
	c.start(t, "managerd", "a9879963600c")

	first := Confirm("brood.lab3.home.arpa", c.paths)
	second := Confirm("brood.lab3.home.arpa", c.paths)
	if len(first.Services) != len(second.Services) {
		t.Fatalf("Services lengths differ between calls: %d and %d", len(first.Services), len(second.Services))
	}
	for i := range first.Services {
		a, b := first.Services[i], second.Services[i]
		if a.Name != b.Name || a.Status != b.Status || a.OnDisk != b.OnDisk ||
			a.Running != b.Running || a.Reason != b.Reason {
			t.Errorf("Services[%d] differs between calls:\n %+v\n %+v", i, a, b)
		}
	}
	if first.Confirmed() != second.Confirmed() {
		t.Error("Confirmed() differs between two calls on an unchanged Comb")
	}
}

// TestEvidenceErrSurvivesWhereThereIsOne. The Reason is for a human and
// the error is for a caller that wants to distinguish, say, a permission
// failure from a missing file, so the underlying error has to travel with
// the verdict rather than being flattened into the sentence.
func TestEvidenceErrSurvivesWhereThereIsOne(t *testing.T) {
	c := newComb(t)
	c.install(t, "raftd", "9c43262d358a")
	// No log at all: an os.IsNotExist error, carried through.
	c.install(t, "managerd", "9c43262d358a")
	c.install(t, "frontend", "9c43262d358a")
	c.start(t, "frontend", "9c43262d358a")

	report := Confirm("brood.lab3.home.arpa", c.paths)

	raftd, _ := report.Service("raftd")
	if raftd.EvidenceErr == nil {
		t.Error("raftd: EvidenceErr is nil for a missing log, want the underlying not-exist error")
	} else if !errors.Is(raftd.EvidenceErr, fs.ErrNotExist) {
		t.Errorf("raftd: EvidenceErr = %v, want a not-exist error", raftd.EvidenceErr)
	}
	frontend, _ := report.Service("frontend")
	if frontend.EvidenceErr != nil {
		t.Errorf("frontend: EvidenceErr = %v, want nil - nothing failed to be read", frontend.EvidenceErr)
	}
}

// TestUnstampedIDIsNotMistakenForABuildID is the trap this package has to
// keep closed, and it survived the extraction into buildgate's code
// rather than the command's. An unstamped binary prints "build=unknown
// (not stamped; built without -ldflags)", buildLine matches it, and
// "unknown" would then be compared against every running id - making
// every unstamped Comb look like it held a different build from every
// other, which is both a false alarm and a failure to say the useful
// thing, which is that the binary was built without -ldflags.
func TestUnstampedIDIsNotMistakenForABuildID(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "fakerd")
	body := "#!/bin/sh\necho 'raftd build=unknown (not stamped; built without -ldflags) commit=unknown built=unknown go=freebsd/amd64'\n"
	if err := os.WriteFile(script, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	id, err := onDiskBuild(script)
	if !errors.Is(err, errNoBuildID) {
		t.Fatalf("onDiskBuild() err = %v, want errNoBuildID", err)
	}
	// The value read is still returned, so a caller that renders evidence
	// can show what the binary actually said rather than nothing.
	if id != unstampedID {
		t.Errorf("onDiskBuild() = %q, want %q carried through with the error", id, unstampedID)
	}

	// And end to end: an unstamped binary beside a stamped running
	// process is unobserved-with-a-reason, not a mismatch.
	c := newComb(t)
	c.install(t, "raftd", "9c43262d358a")
	c.start(t, "raftd", "9c43262d358a")
	if err := os.WriteFile(c.paths.binary("raftd"), []byte("#!/bin/sh\necho 'raftd build=unknown (not stamped; built without -ldflags)'\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	report := Confirm("brood.lab3.home.arpa", c.paths)
	raftd, found := report.Service("raftd")
	if !found {
		t.Fatal("raftd missing from the report")
	}
	if raftd.Status != Unobserved {
		t.Errorf("Status = %q, want %q", raftd.Status, Unobserved)
	}
	if raftd.Reason != ReasonBinaryUnstamped {
		t.Errorf("Reason = %q, want %q - the fix is 'build it with ldflags', not 'restart it'", raftd.Reason, ReasonBinaryUnstamped)
	}
	if report.Confirmed() {
		t.Error("Confirmed() = true with an unstamped binary on disk")
	}
}

// TestRunningUnstampedIDIsAStaleBuildNotAnUnknown. The mirror case, and
// the one that is easy to "fix" wrongly. A daemon that was itself built
// without -ldflags logs "build=unknown", so its running id reads as the
// literal "unknown" and genuinely differs from the stamped binary beside
// it. That IS RunningStale, and it is correct: the process resident in
// memory is not the binary on disk, which is precisely the mixed state.
// Turning it into Unobserved would report "I cannot tell" about a
// mismatch the gate can see.
func TestRunningUnstampedIDIsAStaleBuildNotAnUnknown(t *testing.T) {
	c := newComb(t)
	c.install(t, "raftd", "9c43262d358a")
	c.start(t, "raftd", unstampedID)

	report := Confirm("brood.lab3.home.arpa", c.paths)
	raftd, _ := report.Service("raftd")
	if raftd.Status != RunningStale {
		t.Errorf("Status = %q, want %q - the ids differ, and that is visible rather than unreadable", raftd.Status, RunningStale)
	}
	if report.Confirmed() {
		t.Error("Confirmed() = true with a running process that is not the on-disk build")
	}
}
