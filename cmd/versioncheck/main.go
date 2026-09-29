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
// The comparison itself now lives in internal/buildgate, because
// ADR-0145's controlled update needs the same answers as a per-step
// confirmation gate and a second copy of this table would be free to
// drift. This command is the operator-facing view of it, and its output
// is unchanged: same flags, same columns, same verdict strings, same
// detail sentences, same exit status. The one thing it does not carry
// over is the gate's stricter reading of a -dirty pair, which is
// deliberate and is explained at versioncheckOf below.
//
// SCOPE: this checks the host it runs on. The Comb names on the command
// line are labels for the report, not remote targets - it reads this
// host's /usr/local/libexec/apiary and /var/log/apiary and nothing
// else. Run it on each Comb; do not read "same build" for brood as
// applying to drone.
//
// -advice is a second rendering of the same read, one state word and one
// sentence per daemon, written for the end of a deploy: `make update`
// restarts two of the four daemons and installs all four (ADR-0141), and
// the state it leaves behind is the state this tool exists to detect. It
// is a narrower REPORT of one reading, not a narrower question - the
// services, the evidence, the verdicts and the exit status are the same
// either way. See adviceState for why it is not a second outcome
// vocabulary.
//
// Usage:
//
//	versioncheck [flags] comb comb ...
//	versioncheck -list
package main

import (
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/glenjbarber/apiary/internal/buildgate"
	"github.com/glenjbarber/apiary/internal/buildinfo"
)

// services is the gate's own list, so the two cannot disagree about
// which daemons matter.
var services = buildgate.Services

// verdict is this command's own report vocabulary. It is wider than
// buildgate.Status on purpose: a status says whether a step is
// confirmed, and a human report also has to say WHY not, which is a
// diagnostic. The two are mapped explicitly at versioncheckOf rather
// than by deriving one from the other, so adding a status to the gate
// cannot silently repaint this tool's output.
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

	// sameDirty is agreement that has been honestly downgraded. The two
	// ids match, which normally means one commit and therefore one set
	// of bytes - but a -dirty id means the binary is not the commit, and
	// two dirty builds can share an id while differing. Reporting that
	// as a plain "same build" would be a stronger claim than the
	// evidence supports, which is the one thing this tool must not do.
	sameDirty verdict = "same source (dirty build - bytes unverified)"
)

// versioncheckOf maps one gate answer onto this command's report. The
// only interesting case is DirtyIDMatch.
//
// The gate calls a matching pair of -dirty ids a NON-confirmation, on the
// grounds that a -dirty id describes source and not bytes. This command
// reports it as agreement-that-is-downgraded and still exits zero, which
// is what it has always done and what an operator reading a one-shot
// report expects: the two builds do agree about the commit. The
// difference is real, it is a difference of QUESTION - "did the new
// build take effect" versus "do these two agree" - and it is stated here
// rather than being left for a reader to infer from two sources that
// disagree.
func versioncheckOf(service buildgate.Service) verdict {
	switch service.Status {
	case buildgate.TookEffect:
		return agree
	case buildgate.DirtyIDMatch:
		return sameDirty
	case buildgate.RunningStale:
		return differ
	case buildgate.NotRunning:
		return notRunning
	case buildgate.Unobserved:
		// Three genuinely different situations, which must not be
		// collapsed into one "unknown", because they have different
		// fixes and this report is what an operator reads to choose one.
		switch service.Reason {
		case buildgate.ReasonBinaryPredatesVersionFlag:
			return predatesFlag
		case buildgate.ReasonBinaryUnstamped:
			return unstamped
		case buildgate.ReasonBinaryMissing:
			return missing
		}
		return unknown
	}
	return unknown
}

// The -advice rendering: one line per daemon, for the tail of an update.
//
// `make update` installs all four daemons and restarts two of them, so
// it ends with a question it cannot answer about itself: which of the
// four is actually running the bytes that were just installed. The
// Makefile used to answer it from what it knew it had done, which is
// intent rather than evidence, and ADR-0145 records what that cost on
// 2026-09-27 - four Combs where the closing message was true about
// what had been done and said nothing about what was running.
//
// These two functions are the second rendering. They are deliberately
// not a second table: adviceState is a switch over the gate's own Status
// with one catch-all, and every state word is derived from a status
// rather than from the report vocabulary above. A new status nobody has
// given a word therefore lands on "unknown", which is the safe landing
// and NOT the same landing as TookEffect.

// adviceState is the one word -advice prints for one daemon.
//
// The catch-all is the only fallback and it is deliberately "unknown":
// buildgate.Unobserved means no evidence, and a status this file has
// no word for is no better established than that. Neither may render
// as "current", which is the single claim in this rendering that an
// operator could act on by doing nothing.
func adviceState(service buildgate.Service) string {
	switch service.Status {
	case buildgate.TookEffect:
		return "current"
	case buildgate.DirtyIDMatch:
		return "unverified"
	case buildgate.RunningStale:
		return "stale"
	case buildgate.NotRunning:
		return "not running"
	}
	return "unknown"
}

// adviceSentence is what the state word means, in one line, carrying the
// evidence the word was made from.
//
// Every row that is not TookEffect names what it is not. A daemon whose
// build could not be read is the case a deploy report is most likely to
// get quietly wrong, because it sits underneath two rows that are
// genuinely fine and reads as the absence of a problem: "no news" is
// not "good news", and the sentence says so in the same words every
// time rather than leaving it to the reader.
func adviceSentence(service buildgate.Service) string {
	switch service.Status {
	case buildgate.TookEffect:
		return "running the build on disk (" + orNone(service.Running) + ")"
	case buildgate.DirtyIDMatch:
		return "running " + orNone(service.Running) + " and the binary on disk says " + orNone(service.OnDisk) +
			", and a -dirty id names the source and not the bytes, so this is agreement about the commit" +
			" only; sha256 is what compares the artifacts"
	case buildgate.RunningStale:
		return "installed but not restarted - running " + orNone(service.Running) +
			", on disk " + orNone(service.OnDisk)
	case buildgate.NotRunning:
		return noEvidence(service) + ", so there is no process here for a new build to have reached"
	}
	return noEvidence(service) + " - neither confirmed nor failed"
}

// noEvidence renders why a daemon could not be read, so an unobserved
// row is never a bare "unknown" with nothing behind it: an absent
// binary, an unreadable log and a pre-stamping build all want a
// different thing done about them.
//
// The gate always sets a reason on these rows, and the empty case is
// handled anyway because a hand-built Service is one line away and a
// sentence that opens with a semicolon is the kind of thing that ships.
func noEvidence(service buildgate.Service) string {
	if service.Reason == buildgate.ReasonNone {
		return "no evidence was collected for this daemon"
	}
	return string(service.Reason)
}

// orNone is buildgate's, unexported there, and needed here because an
// advice line prints both ids: a blank between the words reads as a
// value rather than as a missing one.
func orNone(id string) string {
	if id == "" {
		return "no id read"
	}
	return id
}

func main() {
	list := flag.Bool("list", false, "list the Combs this tool checks and exit")
	quiet := flag.Bool("quiet", false, "only report Combs whose verdict is not 'same build'")
	advice := flag.Bool("advice", false,
		"print one line per daemon - its observed state and what that state means - instead of the "+
			"full report. Same evidence, same services, same exit status; written for the end of a deploy")
	// versioncheck is built and stamped like every other binary the
	// Makefile produces, and check-stamped runs each of them with
	// -version before an install is allowed. Registering the flag is what
	// keeps this command from being the one binary in the tree that
	// cannot say what it is.
	buildinfo.RegisterVersionFlag(flag.CommandLine)
	flag.Usage = usage
	flag.Parse()

	if buildinfo.VersionRequested() {
		fmt.Print(buildinfo.Report("versioncheck"))
		return
	}

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
		// One read per Comb, covering all four daemons: the gate already
		// has the whole set, and reading each daemon separately would be
		// four chances to answer from four different instants.
		report := buildgate.Confirm(comb, buildgate.DefaultPaths())
		if *advice {
			printAdvice(comb, now, report, *quiet)
		} else {
			printReport(now, comb, report, *quiet)
		}
		// The exit status is the tool's own long-standing rule and is
		// NOT restated per rendering: 1 when any daemon is running a
		// different build from the one on disk, 0 otherwise, including
		// every "cannot tell". It is written here against the gate's
		// status rather than against the report's verdict word so that
		// -advice cannot quietly inherit a different meaning for the
		// same number - and `make update` needs the honest one, reading
		// 1 as "the report ran and found a mixed Comb", which on that
		// target is the designed outcome and not a fault.
		for _, s := range report.Services {
			if s.Status == buildgate.RunningStale {
				rc = 1
			}
		}
	}
	os.Exit(rc)
}

// printReport is the full form: a verdict from the report vocabulary, a
// detail column, and the ids the verdict was made from.
func printReport(now, comb string, report buildgate.Report, quiet bool) {
	fmt.Printf("%s  %s\n", now, comb)
	for _, s := range report.Services {
		v := versioncheckOf(s)
		if quiet && v == agree {
			continue
		}
		fmt.Printf("    %-10s %-38s %s\n", s.Name, string(v), detail(v, s))
	}
}

// printAdvice is the deploy form. Every daemon is printed, including the
// ones that took effect: this rendering is the tail of a message that
// says which daemons still need work, and a daemon with no line in it
// reads as a daemon nobody looked at.
func printAdvice(comb, now string, report buildgate.Report, quiet bool) {
	fmt.Printf("versioncheck %s %s\n", comb, now)
	for _, s := range report.Services {
		if quiet && s.Status == buildgate.TookEffect {
			continue
		}
		fmt.Printf("  %-10s %-11s %s\n", s.Name, adviceState(s), adviceSentence(s))
	}
}

// detail adds the evidence for anything that is not a clean match, so
// the reader does not have to go and re-derive it. The ids now travel
// with the verdict instead of being re-read here, which is the one
// substantive difference from the pre-extraction version: there used to
// be a second, independent read of both files to fill this column in,
// and it could disagree with the verdict printed beside it.
func detail(v verdict, service buildgate.Service) string {
	switch v {
	case differ:
		return fmt.Sprintf("(running %s, on disk %s - the process predates this file; restart it)",
			service.Running, service.OnDisk)
	case unstamped:
		return "(built without -ldflags; use make build)"
	case predatesFlag:
		return "(this binary has no -version flag - it was built before build stamping existed)"
	case sameDirty:
		return "(both say the same commit, but a -dirty build's id does not " +
			"describe its bytes - two of them can share this id and differ; " +
			"compare sha256 to be sure)"
	case notRunning:
		return ""
	}
	return ""
}

func usage() {
	fmt.Fprintf(os.Stderr, `versioncheck - is this Comb running the build that is on disk?

Reads each daemon's own startup log line and compares it against that
binary's -version output. Read-only: starts nothing, stops nothing.

This checks the host it is run on. The Comb names are labels for the
report, not remote targets: run it once per Comb.

  -advice   one line per daemon instead of the full report: its observed
            state, and what that state means. Same evidence, same services,
            same exit status. This is the form 'make update' prints, where
            "stale" is the work an operator still has to do and "unknown"
            is no evidence at all - neither confirmed nor failed.
  -quiet    with -advice, print only the daemons that are not current.
  -list     the daemons this checks, and exit.
  -version  what this binary is, like every other one in the tree.

Exit status is 1 if any service is running a different build than the
one on disk, 0 otherwise - including when the answer is unknown, so an
inconclusive check never fails a deploy by accident. In -advice mode
that 1 is the report working, not the report failing: a daemon left on
an older build is what 'make update' is designed to leave behind.

%s
`, os.Args[0])
}
