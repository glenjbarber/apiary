package buildgate

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strings"

	"github.com/glenjbarber/apiary/internal/buildinfo"
)

// The evidence readers, and the pure comparison they feed.
//
// This file is the reusable half of cmd/versioncheck, extracted
// unchanged in behaviour. It is pure with respect to the cluster and
// impure with respect to the filesystem, which is the one impurity worth
// having: every judgment about what a verdict MEANS is in Compare, a
// three-line function, and everything above it is I/O whose failures are
// classified rather than guessed at.

// buildLine matches the build identity a daemon prints on its startup
// log line, e.g. "build=b8e74c328d9f commit=b8e74c328d9f built=... go=freebsd/amd64".
var buildLine = regexp.MustCompile(`build=(\S+)`)

// unstampedID is what internal/buildinfo renders in place of a build id
// when a binary was linked without -ldflags. It is NOT a build id, and it
// is a trap here precisely because the regex happily matches it: an
// unstamped binary prints "build=unknown (not stamped; built without
// -ldflags -X ...)" and buildLine returns "unknown" as though it were
// one. Comparing that against a running id would report every unstamped
// Comb as holding a different build from every other, so the value is
// recognised and turned into errNoBuildID, which is the honest reading
// and the one the tool has always reported.
//
// A git short hash cannot be the literal string "unknown", so this
// cannot swallow a real build id.
const unstampedID = "unknown"

// Compare is the whole decision table: given the build id on disk and the
// build id the running process printed when it started, what can honestly
// be said?
//
// A running process that was itself built without -ldflags logs
// "build=unknown" for the same reason, and runningBuild returns that
// literal as its id. check does not special-case it: comparing it against
// a stamped on-disk id yields RunningStale, which is the correct answer
// and not a false alarm. The process resident in memory is genuinely not
// the binary on disk, which is exactly what RunningStale means - the
// install landed and the restart did not.
//
// Compare is exported because it is the part a controller reusing this logic
// needs, and because it is the part that can be tested without a Comb.
// Every row of it is a claim made to an operator about what is actually
// running, which is why each one is spelled out rather than folded into a
// boolean.
//
// An empty onDisk is reported as Unobserved, never as RunningStale: a
// missing id is not evidence of a difference.
//
// NotRunning is deliberately never returned. Whether a service is
// running is a fact about the log file's existence, which is established
// by the reader and not by comparing two ids - folding it in here would
// mean Compare could be handed a pair of ids that never coexist.
func Compare(onDisk, running string) Status {
	switch {
	case onDisk == "":
		return Unobserved
	case running == "":
		return Unobserved
	case running != onDisk:
		return RunningStale
	case buildinfo.IsDirtyID(onDisk):
		return DirtyIDMatch
	default:
		return TookEffect
	}
}

// onDiskBuild returns the build id a binary reports about itself, by
// running it with -version.
//
// The OUTPUT is the reliable signal, not the exit status, and the reason
// is worth recording because getting it wrong hid a real cause behind a
// generic "unknown" on a live deployment: a pre-stamping binary behaves
// two different ways depending on whether its author ever called
// flag.Parse. One rejects the flag ("flag provided but not defined") and
// exits 2; the other had no flag handling at all, ignores -version, and
// carries straight on to reading its config or binding its port. Both
// predate stamping, they fail differently, and neither prints a build id
// - so the ABSENCE of one in the output is what identifies them, and it
// has to be checked before the error is allowed to short-circuit the
// whole classification.
func onDiskBuild(bin string) (string, error) {
	out, err := runCapture(bin, "-version")
	if m := buildLine.FindStringSubmatch(out); m != nil {
		if m[1] == unstampedID {
			// The id is present in form and absent in fact. The value is
			// returned alongside the error so a caller that renders
			// evidence can still show what was read.
			return m[1], errNoBuildID
		}
		return m[1], nil
	}
	if isUndefinedFlagText(out) {
		return "", errUndefinedFlag
	}
	if err != nil {
		if isUndefinedFlagText(err.Error()) {
			return "", errUndefinedFlag
		}
		// It ran and produced no build id, but failed for some other
		// reason. On a Comb where the config is root-only that is the
		// common case when run unprivileged, and it says nothing about
		// stamping either way - so it stays unknown rather than being
		// guessed at.
		return "", err
	}
	// Ran, exited cleanly, no build id: a stamped build with no id
	// injected, i.e. built without -ldflags.
	return "", errNoBuildID
}

// isUndefinedFlagText is the output-side counterpart of errUndefinedFlag:
// the flag package writes its rejection to stderr, which exec merges into
// the captured output.
func isUndefinedFlagText(out string) bool {
	return strings.Contains(out, "flag provided but not defined") ||
		strings.Contains(out, "not defined: -version")
}

// runningBuild returns the build id a process printed when it started,
// read from its log. An empty string with nil error means the log line
// predates build stamping, which is a real and reportable state, not a
// failure to read.
func runningBuild(logPath, prog string) (string, error) {
	f, err := os.Open(logPath)
	if err != nil {
		return "", err
	}
	defer f.Close()
	return runningBuildFrom(f, prog)
}

// runningBuildFrom is runningBuild over an already-open reader.
//
// The split exists so the scanner's error can be provoked. A scanner that
// fails mid-file - a read error, or a line past its buffer - returns an
// empty id with a nil error if its error is dropped, and the gate then
// reports "the running build predates build stamping", which is a FALSE
// claim about a log it never finished reading. Distinguishing "I read the
// log and it says nothing" from "I could not read the log" is the whole
// point of the unobserved status, so that distinction needs to be
// reachable from a test rather than only in production.
func runningBuildFrom(r io.Reader, prog string) (string, error) {
	// Walk forwards and keep the last match: the most recent "listening"
	// line is the one that describes the process currently running.
	// Scanning the whole file rather than stopping at the first match is
	// the part that matters - a log with a restart in it must answer
	// with the LAST line, or a Comb restarted onto a new build reports
	// the old one as still running.
	last := ""
	sc := bufio.NewScanner(r)
	// A startup line with a long node list in it can exceed bufio's
	// default 64 KiB, and a scanner that stops there would silently
	// truncate the log at exactly the point where the answer is.
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Text()
		if !strings.Contains(line, prog+":") || !strings.Contains(line, "listening on") {
			continue
		}
		if m := buildLine.FindStringSubmatch(line); m != nil {
			last = m[1]
		}
	}
	return last, sc.Err()
}

// runCapture runs a binary and returns its combined output. A binary that
// cannot be executed at all is an error, not an empty string - "it would
// not run" and "it ran and said nothing" are different facts, and only one
// of them is about stamping.
func runCapture(bin string, args ...string) (string, error) {
	out, err := exec.Command(bin, args...).CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("running %s %s: %w", bin, strings.Join(args, " "), err)
	}
	return string(out), nil
}
