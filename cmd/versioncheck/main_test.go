package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A log line as a real daemon writes it, both the pre-stamping shape
// and the post-stamping shape.
const stampedLine = `2026/09/26 16:00:21 raftd: build=9c43262d358a ` +
	`commit=9c43262d358a built=2026-09-26T11:52:03-04:00 go=freebsd/amd64 ` +
	`listening on /var/run/apiary/raftd.sock (node-id=brood.lab3.home.arpa, raft-tls=false)`

const unstampedLine = `2026/09/26 02:03:23 raftd: listening on ` +
	`/var/run/apiary/raftd.sock (node-id=brood.lab3.home.arpa, raft-tls=false)`

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
	// The critical negative: a pre-stamping line must NOT yield a
	// build id. Returning "" here is what lets the caller say "unknown"
	// instead of inventing agreement.
	if m := buildLine.FindStringSubmatch(unstampedLine); m != nil {
		t.Errorf("no match wanted on an unstamped line, got %q", m[1])
	}
}

func TestRunningBuildTakesTheMostRecentLine(t *testing.T) {
	// A log with a restart in it: the answer must be the LAST line, or
	// a Comb restarted with a new build and the tool reports the old
	// one as still running.
	p := writeLog(t,
		"2026/09/26 02:03:23 raftd: listening on /var/run/apiary/raftd.sock (node-id=brood)",
		stampedLine,
	)
	got, err := runningBuild(p, "raftd")
	if err != nil {
		t.Fatal(err)
	}
	if want := "9c43262d358a"; got != want {
		t.Errorf("runningBuild() = %q, want the most recent %q", got, want)
	}
}

func TestRunningBuildEmptyOnPreStampingLog(t *testing.T) {
	p := writeLog(t, unstampedLine)
	got, err := runningBuild(p, "raftd")
	if err != nil {
		t.Fatalf("err = %v, want nil: a log that predates stamping is a readable fact", err)
	}
	if got != "" {
		t.Errorf("runningBuild() = %q, want \"\" so the caller reports unknown", got)
	}
}

func TestRunningBuildIgnoresOtherServices(t *testing.T) {
	// managerd's line must not be mistaken for raftd's. Both appear in
	// their own separate files in production, but a combined log is
	// exactly the kind of thing a future -log flag would produce.
	p := writeLog(t,
		"2026/09/26 16:00:21 managerd: build=managerd-build listening on 0.0.0.0:17700 (node-id=brood)",
		stampedLine,
	)
	got, err := runningBuild(p, "raftd")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(got, "managerd-build") {
		t.Errorf("runningBuild(raftd) = %q, picked up another service's line", got)
	}
}

func TestRunningBuildMissingFileIsAnError(t *testing.T) {
	// A missing log is "cannot tell", not "no build". The caller turns
	// this into verdict unknown, and conflating it with agree is the
	// failure this tool exists to avoid.
	if _, err := runningBuild(filepath.Join(t.TempDir(), "nope.log"), "raftd"); err == nil {
		t.Error("err = nil for a missing log, want an error so the verdict is unknown")
	}
}

func TestDetailNamesBothBuildsWhenTheyDiffer(t *testing.T) {
	// When the verdict is DIFFERENT, the reader must be able to see
	// which is which without re-running the tool.
	d := detail("raftd", differ)
	for _, want := range []string{"running", "on disk", "restart"} {
		if !strings.Contains(d, want) {
			t.Errorf("detail() = %q, want it to mention %q", d, want)
		}
	}
	if d := detail("raftd", agree); d != "" {
		t.Errorf("detail(agree) = %q, want empty", d)
	}
}

func TestVerdictsAreDistinctValues(t *testing.T) {
	// Guard against two verdicts collapsing to the same string, which
	// would make the report silently lie.
	all := map[verdict]bool{}
	for _, v := range []verdict{agree, differ, unknown, notRunning, unstamped, noStampLine, sameDirty} {
		if all[v] {
			t.Errorf("duplicate verdict %q", v)
		}
		all[v] = true
	}
}

func TestIsUndefinedFlagText(t *testing.T) {
	// The message a pre-stamping binary produces, written to stderr by
	// the flag package. Every Comb deployed before this change hits it,
	// and reporting it as a generic "unknown" would hide the fact that
	// the answer becomes knowable once the binary is rebuilt.
	if !isUndefinedFlagText("flag provided but not defined: -version\nUsage of ...") {
		t.Error("did not recognise the flag package's rejection")
	}
	if isUndefinedFlagText("permission denied") {
		t.Error("matched a non-flag message")
	}
}

func TestStampsDistinguishesPredatingBinary(t *testing.T) {
	// The real regression, found against a live brood deployment: a
	// pre-stamping binary exits non-zero AND prints the rejection to
	// stderr, and returning early on err hid the specific cause
	// behind a generic error, so every Comb read "unknown".
	dir := t.TempDir()
	// A shell script standing in for a pre-stamping binary: non-zero
	// exit plus the flag package's message on stderr.
	script := filepath.Join(dir, "fakeraftd")
	body := "#!/bin/sh\necho 'flag provided but not defined: -version' >&2\necho 'Usage of fakeraftd:' >&2\nexit 2\n"
	if err := os.WriteFile(script, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	_, err := stamps(script)
	if !errors.Is(err, errUndefinedFlag) {
		t.Errorf("stamps() err = %v, want errUndefinedFlag", err)
	}
}

func TestVerdictsRemainDistinct(t *testing.T) {
	// The added verdicts must not collide with the existing ones.
	seen := map[verdict]bool{}
	for _, v := range []verdict{
		agree, differ, unknown, notRunning, unstamped, noStampLine,
		missing, predatesFlag, sameDirty,
	} {
		if seen[v] {
			t.Errorf("duplicate verdict %q", v)
		}
		seen[v] = true
	}
}

type errStr string

func (e errStr) Error() string { return string(e) }

func TestCompare(t *testing.T) {
	// The whole decision table, because each row is a claim this tool
	// makes to an operator about what is actually running.
	cases := []struct {
		name      string
		disk, run string
		want      verdict
	}{
		{"clean agreement", "9c43262d358a", "9c43262d358a", agree},
		{"clean disagreement", "b8e74c328d9f", "9c43262d358a", differ},
		{"running predates stamping", "b8e74c328d9f", "", noStampLine},
		// The downgrade: matching -dirty ids agree about the commit and
		// say nothing reliable about the bytes, so they must not render
		// as a clean match.
		{"dirty agreement is downgraded", "9c43262d358a-dirty", "9c43262d358a-dirty", sameDirty},
		// ...but a dirty id that differs is still a plain mismatch.
		{"dirty disagreement is still a mismatch", "9c43262d358a-dirty", "b8e74c328d9f", differ},
		// A clean id on disk with a dirty one running is a mismatch,
		// not a downgrade: the sources genuinely differ.
		{"clean vs dirty is a mismatch", "9c43262d358a", "9c43262d358a-dirty", differ},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := compare(c.disk, c.run); got != c.want {
				t.Errorf("compare(%q, %q) = %q, want %q", c.disk, c.run, got, c.want)
			}
		})
	}
}

func TestDirtyAgreementSaysWhyItIsWeaker(t *testing.T) {
	// A downgraded verdict that does not say what is missing is just a
	// different string, not a more honest answer.
	d := detail("raftd", sameDirty)
	for _, want := range []string{"dirty", "sha256"} {
		if !strings.Contains(d, want) {
			t.Errorf("detail(sameDirty) = %q, want it to mention %q", d, want)
		}
	}
	if d := detail("raftd", agree); d != "" {
		t.Errorf("detail(agree) = %q, want empty", d)
	}
}

func TestStampsReturnsADirtyIDWhole(t *testing.T) {
	// The id must survive intact through the regex, suffix included. If
	// a -dirty id were truncated or mangled, the downgrade would never
	// trigger and two genuinely different dirty builds would report
	// agreement.
	dir := t.TempDir()
	script := filepath.Join(dir, "fakerd")
	body := "#!/bin/sh\necho 'raftd build=9c43262d358a-dirty commit=9c43262d358a built=2026-09-26T11:52:03-04:00 go=freebsd/amd64'\n"
	if err := os.WriteFile(script, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	got, err := stamps(script)
	if err != nil {
		t.Fatalf("stamps() err = %v", err)
	}
	if want := "9c43262d358a-dirty"; got != want {
		t.Errorf("stamps() = %q, want the full id %q including its -dirty suffix", got, want)
	}
	if compare(got, got) != sameDirty {
		t.Error("a matching -dirty id did not produce the downgraded verdict")
	}
}
