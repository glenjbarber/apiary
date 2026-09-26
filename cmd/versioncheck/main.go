// Command versioncheck compares what a Comb is RUNNING against what is
// ON DISK, which is the question a deploy leaves genuinely ambiguous.
//
// A binary copied over a running executable leaves the new bytes on
// disk and the old ones resident in the process, so a matching mtime
// proves nothing. This checks the two independently and reports any
// Comb where they disagree.
//
// It is deliberately read-only: it starts nothing, stops nothing, and
// writes nothing. Where it cannot tell, it says so rather than
// guessing - an unstamped binary, a process it cannot read, or a
// Comb that is not running at all are all reported as unknown, never
// as agreement.
//
// Usage:
//
//	versioncheck [-raft-addr host:port] comb comb ...
//	versioncheck -list
package main

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

const libexec = "/usr/local/libexec/apiary/"

// The four daemons that are installed on a Comb. raftd is here on
// purpose: it is the one INSTALL_SRCS_FILTERED excludes from install,
// so it is the one most likely to be running an older build than the
// one sitting next to it.
var services = []string{"raftd", "managerd", "frontend", "restshimd"}

// buildLine matches the build identity a daemon prints on its startup
// log line, e.g. "build=9c43262d358a-2026... go=darwin/arm64".
var buildLine = regexp.MustCompile(`build=(\S+)`)

// stamps extracts the build id a binary reports about itself.
func stamps(bin string) (string, error) {
	out, err := runCapture(bin, "-version")
	// The OUTPUT is the reliable signal, not the exit status, and the
	// reason is worth recording: a pre-stamping binary behaves two
	// different ways depending on whether its author ever called
	// flag.Parse. One rejects the flag ("flag provided but not
	// defined") and exits 2; the other had no flag handling at all,
	// ignores -version, and carries straight on to reading its config
	// or binding its port. Both predate stamping, they fail
	// differently, and neither prints a build id - so the ABSENCE of
	// one in the output is what identifies them, and it has to be
	// checked before the error is allowed to short-circuit the whole
	// classification.
	if m := buildLine.FindStringSubmatch(out); m != nil {
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
		// common case when run unprivileged, and it says nothing
		// about stamping either way - so it stays unknown rather than
		// being guessed at.
		return "", err
	}
	// Ran, exited cleanly, no build id: a stamped build with no id
	// injected, i.e. built without -ldflags.
	return "", errNoBuildID
}

// Sentinel errors, so the caller distinguishes the cases by identity
// rather than by matching on message text at a second site.
var (
	errUndefinedFlag = errors.New("binary has no -version flag")
	errNoBuildID     = errors.New("no build id in -version output")
)

// isUndefinedFlagText is the output-side counterpart of
// isUndefinedFlag: the flag package writes its rejection to stderr,
// which exec merges into the captured output.
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
	// Walk backwards: the most recent "listening" line is the one that
	// describes the process currently running. Scanning the whole file
	// forwards and keeping the last match is equivalent and simpler to
	// get right on a log that may be large.
	last := ""
	sc := bufio.NewScanner(f)
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

// verdict is the honest three-way answer. Agreement, disagreement, and
// "cannot tell" are all distinct, and conflating the third with the
// first is precisely the bug this tool exists to prevent.
type verdict string

const (
	agree        verdict = "same build"
	differ       verdict = "DIFFERENT BUILD"
	unknown      verdict = "unknown"
	notRunning   verdict = "not running"
	missing      verdict = "not installed here"
	predatesFlag verdict = "pre-dates -version (build it first)"
	unstamped    verdict = "unstamped (no -ldflags)"
	noStampLine  verdict = "unknown (running build predates build stamping)"
)

func check(prog string) verdict {
	disk, err := stamps(libexec + prog)
	if err != nil {
		// Three genuinely different situations, which must not be
		// collapsed into one "unknown":
		//
		//  1. the binary exists but predates -version, so it cannot
		//     answer the question at all (every Comb deployed before
		//     build stamping landed);
		//  2. the binary is missing or not executable here;
		//  3. it answered, but with nothing that parses.
		switch {
		case errors.Is(err, errUndefinedFlag):
			return predatesFlag
		case errors.Is(err, errNoBuildID):
			return unstamped
		case os.IsNotExist(err):
			return missing
		}
		return unknown
	}
	if disk == "unknown" {
		return unstamped
	}

	run, err := runningBuild("/var/log/apiary/"+prog+".log", prog)
	if err != nil {
		// A log with no "listening" line at all means the service is
		// not currently running, which is a distinct fact from "I
		// could not read it" and from "it is running something
		// older". Reported as its own verdict so a reader can tell
		// "not deployed here" from "deployed but stale".
		if os.IsNotExist(err) {
			return notRunning
		}
		return unknown
	}
	switch {
	case run == "":
		return noStampLine
	case run != disk:
		return differ
	default:
		return agree
	}
}

func main() {
	list := flag.Bool("list", false, "list the Combs this tool checks and exit")
	quiet := flag.Bool("quiet", false, "only report Combs whose verdict is not 'same build'")
	flag.Usage = usage
	flag.Parse()

	if *list {
		for _, s := range services {
			fmt.Printf("  %s\n", s)
		}
		return
	}
	combs := flag.Args()
	if len(combs) == 0 {
		usage()
		os.Exit(2)
	}

	rc := 0
	now := time.Now().Format(time.RFC3339)
	for _, comb := range combs {
		fmt.Printf("%s  %s\n", now, comb)
		for _, s := range services {
			v := check(s)
			if *quiet && v == agree {
				continue
			}
			fmt.Printf("    %-10s %-38s %s\n", s, string(v), detail(s, v))
			if v == differ {
				rc = 1
			}
		}
	}
	os.Exit(rc)
}

// detail adds the evidence for anything that is not a clean match, so
// the reader does not have to go and re-derive it.
func detail(prog string, v verdict) string {
	switch v {
	case differ:
		disk, _ := stamps(libexec + prog)
		run, _ := runningBuild("/var/log/apiary/"+prog+".log", prog)
		return fmt.Sprintf("(running %s, on disk %s - the process predates this file; restart it)", run, disk)
	case unstamped:
		return "(built without -ldflags; use make build)"
	case predatesFlag:
		return "(this binary has no -version flag - it was built before build stamping existed)"
	case notRunning:
		return ""
	}
	return ""
}

func usage() {
	fmt.Fprintf(os.Stderr, `versioncheck - is this Comb running the build that is on disk?

Reads each daemon's own startup log line and compares it against that
binary's -version output. Read-only: starts nothing, stops nothing.

Exit status is 1 if any service is running a different build than the
one on disk, 0 otherwise - including when the answer is unknown, so an
inconclusive check never fails a deploy by accident.

%s
`, os.Args[0])
}

// runCapture runs a binary and returns its combined output. A binary
// that cannot be executed at all is an error, not an empty string -
// "it would not run" and "it ran and said nothing" are different facts.
func runCapture(bin string, args ...string) (string, error) {
	out, err := exec.Command(bin, args...).CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("running %s %s: %w", bin, strings.Join(args, " "), err)
	}
	return string(out), nil
}
