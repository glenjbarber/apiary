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
		// One read per Comb, covering all four daemons: the gate already
		// has the whole set, and reading each daemon separately would be
		// four chances to answer from four different instants.
		report := buildgate.Confirm(comb, buildgate.DefaultPaths())
		for _, s := range report.Services {
			v := versioncheckOf(s)
			if *quiet && v == agree {
				continue
			}
			fmt.Printf("    %-10s %-38s %s\n", s.Name, string(v), detail(v, s))
			if v == differ {
				rc = 1
			}
		}
	}
	os.Exit(rc)
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

Exit status is 1 if any service is running a different build than the
one on disk, 0 otherwise - including when the answer is unknown, so an
inconclusive check never fails a deploy by accident.

%s
`, os.Args[0])
}
