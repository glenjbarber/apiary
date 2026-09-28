// Package buildgate is the per-Comb, per-step confirmation gate for
// ADR-0145: did the build that was just installed actually take effect?
//
// It answers one question, for one Comb, about all four daemons on that
// Comb, and it answers it as a verdict per daemon rather than as a
// boolean. The reason is a specific failure this project has already hit
// twice: an install and a restart are different events, a binary copied
// over a running executable leaves the new bytes on disk and the old ones
// resident in the process, and nothing about that state is visible from
// the filesystem. On 2026-09-27 `make update` left frontend and restshimd
// on the new build and managerd and raftd on the previous one across
// brood and drone, and a checksum of the file on disk reported success
// throughout.
//
// The gate therefore compares two INDEPENDENT readings:
//
//   - what the running process printed on its own startup line, which is
//     a claim made by the process about itself; and
//   - what the binary sitting beside it reports under -version, which
//     describes the bytes on disk without starting anything.
//
// When they agree, the step took effect. When they disagree, the install
// landed and the restart did not. Neither answer can be reached from the
// other, which is the whole reason this is a package and not a shell
// pipeline.
//
// # Where it runs, and the trust boundary that implies
//
// THIS PACKAGE RUNS LOCALLY, ON THE COMB IT IS DESCRIBING, AND OPENS NO
// NETWORK CONNECTION AT ALL. It reads two directories - the libexec
// directory holding the installed binaries and the log directory holding
// their startup lines - and executes the binaries it finds there with
// exactly one argument. There is no dial, no RPC, no remote read, and no
// code path that could be made remote by configuration, because
// `Paths` is two directory names and nothing else.
//
// That is a deliberate design choice rather than an incidental one, and
// it is worth stating the alternative that was rejected. A controller
// running on one Comb could instead read each other Comb's logs over the
// wire and run the comparison centrally. That was rejected for three
// reasons, in order of how much they matter:
//
//  1. It makes the absence of evidence indistinguishable from the
//     evidence of a mismatch. If the log never arrives, the central
//     comparison has two ids, one of which is missing. Whatever it
//     concludes has to be a guess about which case it is in, and a guess
//     in that direction fails OPEN. Reading locally means "the log file
//     is not there" is a fact the gate establishes for itself, on the
//     host that owns the log, before any value of any variable is
//     compared.
//
//  2. It moves the evidence. Log lines and binaries would cross the
//     colony, and a copy in transit is a second thing whose provenance
//     has to be trusted - so the design would gain a trust question
//     exactly where it is trying to lose one. Reading locally leaves
//     every byte of evidence on the Comb that produced it.
//
//  3. The logs are root-owned on a host where a deploy is a privileged
//     act. A central reader needs the privilege of every Comb to read
//     every Comb's logs, which is a strictly larger grant than one local
//     process needs to read one Comb's.
//
// The trust boundary this package actually draws: it trusts the local
// filesystem as read by the process invoking it, and it trusts each
// daemon's own claim about itself as recorded in that daemon's own
// startup log line. It establishes nothing beyond that. In particular it
// does NOT establish that the process which wrote the line is still
// running - see the Liveness limitation on Confirm, which is real and is
// not papered over.
//
// The consequence for callers: the thing that eventually carries a
// Report from one Comb to a coordinator is an RPC, and it is NOT built.
// This package is deliberately shaped so that adding it means shipping
// this package's Report, not shipping evidence: Report already separates
// the two ids, the verdict and the reason, so a transport moves claims
// rather than logs.
package buildgate

import (
	"errors"
	"io/fs"
	"path/filepath"
)

// Services are the four daemons installed on a Comb, and every one of them
// is checked. raftd is here on purpose: `make install` puts a new binary
// there for it, but `make update` deliberately does not restart the
// process (raftd lives in the Makefile's FORCE_RESTART_SRCS, not
// UPDATE_RESTART_SRCS), so it is the one most likely to be running an
// older build than the binary sitting next to it.
//
// frontend and restshimd are here for the same reason from the other
// direction: they are the two `make update` DOES restart, so a Comb
// half way through a sweep can have those two on the new build and the
// other two on the old, and a gate that only watched managerd and raftd
// would call that Comb confirmed.
var Services = []string{"raftd", "managerd", "frontend", "restshimd"}

// Default paths, taken from the Makefile's install and log targets.
const (
	// DefaultLibexecDir is where the Makefile installs the daemons.
	DefaultLibexecDir = "/usr/local/libexec/apiary/"

	// DefaultLogDir is where the daemons' startup lines are written.
	DefaultLogDir = "/var/log/apiary/"
)

// Paths locates the evidence on ONE Comb. It is two directories and
// nothing else, which is what makes the locality claim above a property
// of the type rather than a promise in a comment: there is nowhere to put
// a host, a port, or a peer.
//
// The zero value is not usable, because a gate that silently defaulted to
// reading /dev/null and reported UNOBSERVED for everything would be a
// gate that always stops - safe, but useless in a way that looks like a
// working one. Use DefaultPaths, or set both fields deliberately.
type Paths struct {
	// LibexecDir holds the installed binaries, one per service name.
	LibexecDir string

	// LogDir holds <service>.log for each service name.
	LogDir string
}

// DefaultPaths returns the production locations, which is what a caller
// on a real Comb wants and what a test overrides.
func DefaultPaths() Paths {
	return Paths{LibexecDir: DefaultLibexecDir, LogDir: DefaultLogDir}
}

// binary is the installed executable for a service.
func (p Paths) binary(service string) string {
	return filepath.Join(p.LibexecDir, service)
}

// log is the startup log for a service.
func (p Paths) log(service string) string {
	return filepath.Join(p.LogDir, service+".log")
}

// Status is one daemon's answer. Each is a distinct fact, and the
// distinction between the third and the fourth is the one this gate
// exists to keep: "I could not read the evidence" is not "the evidence
// says it did not take effect", and a caller that cannot tell them apart
// will eventually report the first as the second.
type Status string

const (
	// TookEffect means the running process is the build on disk. This is
	// the only status that permits a caller to call the step confirmed.
	TookEffect Status = "took-effect"

	// DirtyIDMatch means the two ids match and both carry the -dirty
	// marker, so they agree about the SOURCE and say nothing reliable
	// about the bytes: two dirty builds can share one id and differ.
	// This is a separate status rather than a TookEffect with a caveat
	// because a caller that switches on Status would otherwise confirm
	// the step, and "the commit is the same" is a weaker claim than
	// "the new build is running".
	DirtyIDMatch Status = "dirty-id-match"

	// RunningStale means the process is running a different build from
	// the one on disk: the install landed and the restart did not. This
	// is the mixed state ADR-0145's per-step gate exists to catch.
	RunningStale Status = "running-stale"

	// Unobserved means the running build could not be determined. The
	// evidence was absent, unreadable, or predates build stamping, and
	// the Reason says which. It is NOT success and NOT a mismatch: a
	// daemon running a build older than stamping is neither known-good
	// nor known-bad, and reporting it as either is a guess.
	Unobserved Status = "unobserved"

	// NotRunning means there is no process at all: the log that would
	// carry a startup line does not exist. It is distinct from
	// Unobserved because "the service is down" is an actionable fact in
	// its own right, and a caller triaging a stalled sweep needs to tell
	// it from "the log was there and I could not parse it".
	NotRunning Status = "not-running"
)

// Confirms reports whether a status is a server-confirmed "the step took
// effect".
//
// Only TookEffect does. DirtyIDMatch deliberately does not: the two ids
// agree, but a -dirty id does not describe its bytes, so agreeing with
// it is agreeing about source rather than about what is running.
// Everything else is a stop, including Unobserved - a gate that cannot
// read its evidence must not report success, and that is the entire
// reason this method exists rather than a bare Status comparison at every
// call site.
func (s Status) Confirms() bool { return s == TookEffect }

// Reason narrows WHY a service is not TookEffect. It never changes the
// Status: two different causes of Unobserved are the same answer for a
// caller deciding whether to proceed, and keeping them apart is a
// diagnostic, not a decision.
type Reason string

const (
	// ReasonNone accompanies TookEffect.
	ReasonNone Reason = ""

	// ReasonDirtyBuild: both ids carry -dirty. See DirtyIDMatch.
	ReasonDirtyBuild Reason = "both build ids are -dirty, so they describe source and not bytes"

	// ReasonLogMissing: the log file does not exist, so the daemon has
	// never logged a startup line from where this gate can see it.
	ReasonLogMissing Reason = "the daemon's log file does not exist"

	// ReasonLogUnreadable: the log exists and could not be read. A
	// permission failure is the common case when the gate is run
	// unprivileged, and it says nothing about whether the restart
	// happened.
	ReasonLogUnreadable Reason = "the daemon's log file could not be read"

	// ReasonLogPredatesStamping: the log was read and carries startup
	// lines, but none of them carries a build id. The process is
	// running a build that predates build stamping entirely, so it
	// cannot answer the question however many times it is asked.
	ReasonLogPredatesStamping Reason = "the running build predates build stamping, so it states no build id"

	// ReasonBinaryMissing: the binary is not installed here.
	ReasonBinaryMissing Reason = "the daemon is not installed in the libexec directory"

	// ReasonBinaryUnreadable: the binary exists and could not be run, or
	// ran and could not be understood. On a Comb where the config is
	// root-only this is the common case when the gate runs
	// unprivileged.
	ReasonBinaryUnreadable Reason = "the daemon's binary could not be read for a build id"

	// ReasonBinaryPredatesVersionFlag: the binary has no -version flag.
	// Every Comb deployed before build stamping landed is in this state,
	// and it is a fact about the binary rather than about the process.
	ReasonBinaryPredatesVersionFlag Reason = "the binary has no -version flag; it predates build stamping"

	// ReasonBinaryUnstamped: the binary answered -version and printed no
	// build id, which is a stamped build with no id injected - built
	// without -ldflags.
	ReasonBinaryUnstamped Reason = "the binary printed no build id; it was built without -ldflags"
)

// Service is one daemon's complete answer. It carries the evidence
// alongside the conclusion, so a reader never has to re-derive the verdict
// from the ids to know which is which.
type Service struct {
	// Name is the service: one of Services.
	Name string

	// Status is the verdict. See Confirms.
	Status Status

	// OnDisk is the build id the installed binary reports about itself,
	// or "" when no id could be read. Empty is never implied agreement.
	OnDisk string

	// Running is the build id the running process printed when it
	// started, or "" when none could be read.
	Running string

	// Reason narrows a non-TookEffect Status. See Reason.
	Reason Reason

	// Detail is the operator-readable sentence behind the verdict,
	// always non-empty. It carries both ids where both were read,
	// because "which is which" is the first thing a reader needs and
	// the second thing they should have to go and look up.
	Detail string

	// EvidenceErr is the underlying read failure where there was one, for
	// a caller that wants the error rather than the sentence. It is nil
	// whenever the status does not come from a failed read.
	EvidenceErr error
}

// Confirms reports whether THIS daemon took the new build. It is the
// per-service half of Report.Confirmed, and it exists so a caller
// iterating Report.Services never has to compare Status against a literal
// and so cannot accidentally confirm a status that Confirms refuses.
func (s Service) Confirms() bool { return s.Status.Confirms() }

// Report is one Comb's answer, covering every service in Services.
//
// It is a value with no methods that can change it and no clock inside
// it: everything a caller might want to decide is already decided here,
// and re-deciding it at a call site is how "unknown" quietly becomes
// "same build".
type Report struct {
	// NodeID is the Comb this report describes. It is carried, not
	// derived: a report about brood that gets filed under drone is a
	// misroute, and a misroute here reads as a successful gate on a
	// Comb that was never touched.
	NodeID string

	// Paths is the evidence location the report was read from, so a
	// report can be traced to the directories it answered about.
	Paths Paths

	// Services holds one entry per service in Services, in that order.
	Services []Service
}

// Service returns one service's answer by name.
func (r Report) Service(name string) (Service, bool) {
	for _, service := range r.Services {
		if service.Name == name {
			return service, true
		}
	}
	return Service{}, false
}

// Confirmed reports whether EVERY service on this Comb is TookEffect.
//
// It is the answer to "did this Comb take the new build", and it is
// deliberately strict: a single Unobserved, NotRunning, RunningStale or
// DirtyIDMatch makes the whole Comb unconfirmed. A gate that averaged
// would confirm a Comb with three restarted daemons and one that never
// started, which is the exact state a sweep is supposed to end without.
func (r Report) Confirmed() bool {
	if len(r.Services) == 0 {
		// No evidence at all is not confirmation. Reachable only if a
		// caller built a Report by hand with no services in it, and
		// handled here because "an empty set of answers" is precisely
		// the shape that fails open.
		return false
	}
	for _, service := range r.Services {
		if !service.Status.Confirms() {
			return false
		}
	}
	return true
}

// FirstStop returns the first service that did not take effect, in
// Services order, so a caller can report one specific daemon and stop.
//
// Returning the FIRST rather than the worst is a reporting choice, not a
// severity judgement: during a sweep the operator needs the first thing
// to look at, and Services is ordered raftd-first for exactly that
// reason. Every non-TookEffect service is in Report.Services regardless,
// so nothing is hidden by this returning one of them.
func (r Report) FirstStop() (Service, bool) {
	for _, service := range r.Services {
		if !service.Status.Confirms() {
			return service, true
		}
	}
	return Service{}, false
}

// Stops returns every service that did not take effect, in Services
// order. It is FirstStop for a caller that wants the whole picture; the
// two are separate so neither has to guess which the other meant.
func (r Report) Stops() []Service {
	var stops []Service
	for _, service := range r.Services {
		if !service.Status.Confirms() {
			stops = append(stops, service)
		}
	}
	return stops
}

// summary renders the one-line form used inside Detail strings. It exists
// so a sentence never has to decide for itself whether an id is missing:
// a blank in the middle of "(running , on disk 9c43)" reads as a value.
func summary(running, onDisk string) string {
	return "(running " + orNone(running) + ", on disk " + orNone(onDisk) + ")"
}

func orNone(id string) string {
	if id == "" {
		return "none recorded"
	}
	return id
}

// Confirm is the entry point: it reads the evidence on THIS Comb and
// returns one verdict per service, with no network access of any kind.
//
// The Comb names in the ADR-0145 sweep are labels for a report, not
// remote targets. A caller wanting a verdict about another Comb asks that
// Comb; Confirm cannot be pointed at one, because Paths has nowhere to put
// a host.
//
// # What a caller MUST do with each result
//
// This is the contract, and it is the reason the method exists in this
// form rather than returning a bool:
//
//   - TookEffect: the step is confirmed for that daemon. Nothing else
//     follows from it automatically - confirmation of one daemon is not
//     confirmation of the Comb, and Confirmed() is the Comb-level
//     answer.
//   - DirtyIDMatch: STOP. The ids agree and the build is dirty, so the
//     commit does not describe the bytes. A caller that wants byte-level
//     proof must compare the artifact's sha256 instead; nothing here can
//     supply it.
//   - RunningStale: STOP. The install landed and the restart did not.
//     This is the mixed state, and it is a fault to be fixed, not a state
//     to wait out.
//   - Unobserved: STOP. No evidence is not permission. The single most
//     important line in this comment is that Unobserved and RunningStale
//     are BOTH stops: they are not the same stop, and neither is
//     permission, but a caller that treats one as a soft warning and the
//     other as an error has still built a gate that can pass a Comb it
//     never actually read.
//   - NotRunning: STOP, and a different one. There is no process, so
//     there is nothing to have taken effect.
//
// A caller that cannot act on a stop must not proceed. "Not safe" is a
// valid answer and, during a sweep, it is the only safe one: the next
// Comb's step rests on this one having been confirmed.
//
// # Limitations, stated rather than hidden
//
// Liveness. A log line is a record of a process that STARTED, not of one
// that is still running. Confirm does not check that the pid behind the
// most recent startup line is alive, so a daemon that started on the new
// build and has since died is reported TookEffect. This is not a
// shortcut taken for convenience - it is versioncheck's existing
// behaviour, kept deliberately so the CLI's verdicts do not change - but
// it is a real gap and a caller deciding whether a Comb is SERVING must
// ask a question this package does not answer.
//
// Log rotation and truncation. The most recent matching line in the file
// is taken as the current process's. A rotated or truncated log whose
// remaining content predates stamping is reported ReasonLogPredatesStamping
// rather than as a stale process, which is the honest answer for a log
// that no longer contains the evidence, but it is an answer about the
// LOG and not about the process.
//
// Single point in time. Each report is one sample. A Comb can be
// restarted between two of the readings in a single Confirm call, so a
// report is evidence about an instant, not a guarantee about the
// following second. Re-read rather than cache.
//
// Bytes. Nothing here compares artifact bytes. A clean build id names a
// commit, and buildinfo's own documentation is explicit that the same
// commit can produce different bytes behind the same id on a different
// toolchain or platform. For a byte-level answer, hash the artifact.
func Confirm(nodeID string, paths Paths) Report {
	report := Report{NodeID: nodeID, Paths: paths, Services: make([]Service, 0, len(Services))}
	for _, name := range Services {
		report.Services = append(report.Services, check(name, paths))
	}
	return report
}

// check reads one service's two independent readings and decides. The
// order is deliberate: the binary is read FIRST and its failures are
// classified, because without an on-disk id there is nothing to compare a
// running id against, and a service with an unreadable binary cannot be
// reported as anything but unobserved.
func check(name string, paths Paths) Service {
	disk, err := onDiskBuild(paths.binary(name))
	if err != nil {
		return unobservedBinary(name, disk, err, paths)
	}

	run, err := runningBuild(paths.log(name), name)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			// A log with no startup line at all means nothing has
			// logged a start from where this gate can see, which is a
			// different fact from "I could not read it" and from "it
			// is running something older". Reported as its own status
			// so a reader can tell "not deployed here" from "deployed
			// but stale".
			return Service{
				Name:        name,
				Status:      NotRunning,
				OnDisk:      disk,
				Reason:      ReasonLogMissing,
				Detail:      name + " has no log at " + paths.log(name) + ", so it is not running; " + summary("", disk),
				EvidenceErr: err,
			}
		}
		return Service{
			Name:        name,
			Status:      Unobserved,
			OnDisk:      disk,
			Reason:      ReasonLogUnreadable,
			Detail:      name + "'s log at " + paths.log(name) + " could not be read, so its running build is unknown; " + summary("", disk),
			EvidenceErr: err,
		}
	}

	status := Compare(disk, run)
	service := Service{
		Name:    name,
		Status:  status,
		OnDisk:  disk,
		Running: run,
	}
	switch status {
	case TookEffect:
		service.Detail = name + " is running the build on disk (" + run + ")"
	case DirtyIDMatch:
		service.Reason = ReasonDirtyBuild
		service.Detail = name + " reports the same commit as the binary on disk, but both ids are -dirty, " +
			"so the commit does not describe the bytes and two dirty builds can share one id; " + summary(run, disk) +
			" - compare sha256 to compare the artifacts"
	case RunningStale:
		service.Detail = name + " is running an older build than the binary on disk; " + summary(run, disk) +
			" - the process predates this file, so it was installed but not restarted"
	case Unobserved:
		service.Reason = ReasonLogPredatesStamping
		service.Detail = name + " is running, but the most recent startup line carries no build id, so it " +
			"predates build stamping and cannot say what it is; " + summary(run, disk)
	}
	return service
}

// unobservedBinary classifies a failure to read an on-disk build id. The
// three causes it separates are genuinely different facts and collapsing
// them into one "unknown" is what made every Comb in a live deployment
// read "unknown" before they were separated: a binary that predates
// -version cannot ever answer, a binary that is missing is a deployment
// gap, and a binary that ran and failed for some other reason says
// nothing about stamping either way.
//
// The not-exist test is errors.Is against fs.ErrNotExist rather than
// os.IsNotExist, and the difference is not cosmetic. os.IsNotExist
// inspects the concrete error type and does not unwrap, so a missing
// executable - whose error arrives from runCapture already wrapped -
// was classified as "could not be read" and the "not installed here"
// answer was unreachable. The same bug was in cmd/versioncheck before the
// extraction and was inherited with it; a Comb with a genuinely absent
// binary read "unknown" there, which is a thing to go and look at rather
// than the thing to go and install.
func unobservedBinary(name, disk string, err error, paths Paths) Service {
	service := Service{
		Name:        name,
		OnDisk:      disk,
		Status:      Unobserved,
		EvidenceErr: err,
	}
	switch {
	case errors.Is(err, errUndefinedFlag):
		service.Reason = ReasonBinaryPredatesVersionFlag
		service.Detail = name + "'s binary at " + paths.binary(name) + " has no -version flag; it was built " +
			"before build stamping existed, so it cannot report what it is"
	case errors.Is(err, errNoBuildID):
		service.Reason = ReasonBinaryUnstamped
		service.Detail = name + "'s binary at " + paths.binary(name) + " answered -version with no build id; " +
			"it was built without -ldflags"
	case errors.Is(err, fs.ErrNotExist):
		service.Reason = ReasonBinaryMissing
		service.Detail = name + " is not installed at " + paths.binary(name)
	default:
		service.Reason = ReasonBinaryUnreadable
		service.Detail = name + "'s binary at " + paths.binary(name) + " could not be read for a build id, so " +
			"nothing on this Comb can be compared against it"
	}
	return service
}

// Sentinel errors, so a caller distinguishes the cases by identity rather
// than by matching on message text at a second site.
var (
	// errUndefinedFlag: the binary rejected -version. The flag package
	// writes this to stderr, which exec merges into the captured output.
	errUndefinedFlag = errors.New("binary has no -version flag")

	// errNoBuildID: the binary answered and printed no build id.
	errNoBuildID = errors.New("no build id in -version output")
)
