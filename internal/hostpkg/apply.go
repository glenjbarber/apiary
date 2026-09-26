package hostpkg

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"
)

// ApplyOutcome is what an apply actually achieved. The vocabulary is
// the same discipline as UpdateStatus and as internal/cluster's verdict
// types: unknown is a first-class outcome and is never quietly widened
// into success.
type ApplyOutcome string

const (
	// ApplyRefused means nothing was run. The plan was rejected before
	// any command could be issued.
	ApplyRefused ApplyOutcome = "refused"

	// ApplyApplied means every package the plan named was positively
	// re-read after the command and confirmed no longer outdated. This
	// is defined by a post-condition observation, never by the command's
	// own exit status - see Apply's doc comment.
	ApplyApplied ApplyOutcome = "applied"

	// ApplyFailed means the command ran and at least one package was
	// positively re-read as still outdated afterwards.
	ApplyFailed ApplyOutcome = "failed"

	// ApplyUnknown means the command ran but the outcome could not be
	// established - the post-update read failed, or a package's status
	// came back unknown. This is the honest verdict for "we don't
	// know", and it is never reported as success.
	ApplyUnknown ApplyOutcome = "unknown"

	// ApplyDiverged means every package is confirmed no longer outdated,
	// but at least one is not at the version the plan named. That is
	// not an unknown - the post-condition read did establish what is
	// installed - and it is not a success either, because Apiary is not
	// able to say the host received the upgrade that was previewed. pkg
	// can do this legitimately (a held package, a distfile change, a
	// locally patched port), which is exactly why it needs its own
	// verdict and its own human attention rather than a green badge.
	ApplyDiverged ApplyOutcome = "diverged"
)

// Applier is the ONE route to root in this package. It exists as an
// interface for two reasons: so Apply can be fully unit-tested without
// root, and so the privileged surface is a single named seam that a
// future reviewer can audit on its own.
//
// There is deliberately no method here that takes a caller-supplied
// command, name, or argument list. A generic "run this as root" would
// be a general-purpose root command channel wearing a package-update
// costume, and that is exactly the blast radius this package declines
// to have.
type Applier interface {
	// Upgrade installs exactly the named packages, and nothing else.
	// Implementations must not refresh the catalogue, must not remove
	// anything, and must not act on a package the caller did not name.
	Upgrade(ctx context.Context, packages []string) (UpgradeResult, error)
}

// UpgradeResult is the raw outcome of one privileged upgrade command.
// It is recorded verbatim so an operator can audit exactly what ran,
// and it is deliberately kept separate from ApplyOutcome: the exit
// status is evidence, not a verdict.
type UpgradeResult struct {
	// Command is the exact argv that was executed, first element
	// included. It is recorded so the action is reproducible and
	// reviewable after the fact.
	Command []string

	ExitCode int
	Stdout   string
	Stderr   string
}

// RunnerApplier is the real Applier: it shells out to pkg as whatever
// user the calling process is. It does not attempt to become root - the
// caller must already be root, which Apply checks via Privilege before
// this type is ever used.
type RunnerApplier struct {
	runner Runner
	// pkgPath is the pkg binary to invoke. Zero means "pkg", resolved
	// through PATH.
	pkgPath string

	// extraArgs are inserted immediately after the binary. Zero value is
	// the documented default. It exists so a host with a pkg whose flag
	// spelling differs can be accommodated without editing this
	// package - but it is a fixed slice, never derived from request
	// data, so it cannot become an injection point.
	extraArgs []string
}

// NewRunnerApplier returns the real, privileged Applier.
func NewRunnerApplier() *RunnerApplier {
	return &RunnerApplier{runner: NewExecRunner(), pkgPath: "pkg", extraArgs: defaultUpgradeFlags}
}

// defaultUpgradeFlags is the fixed flag set for every upgrade this
// package performs.
//
//   - "-y" answers pkg's own confirmation prompt, because the operator
//     has already confirmed deliberately at a higher level (exact
//     phrase plus an inspected plan). Leaving it out would hang a
//     daemon that has no terminal.
//   - "--no-repo-update" is the important one: it stops `pkg upgrade`
//     from running `pkg update` internally. Refreshing the catalogue
//     is a state-changing, network-touching operation, and this package
//     has already established that the catalogue's age - not a silent
//     refresh performed by a privileged command - is what the operator
//     needs to be shown. (Flag spelling is unverified against a live
//     pkg; see the package's report notes. The argv is built by a
//     single pure function, pkgUpgradeCommand, so a correction is a
//     one-line, unit-tested change.)
var defaultUpgradeFlags = []string{"upgrade", "-y", "--no-repo-update"}

// pkgUpgradeCommand is the pure builder for the one command vector this
// package is ever allowed to execute. It is exported through
// RunnerApplier.CommandFor so the exact argv can be asserted in a test
// without root and without a host.
func (a *RunnerApplier) pkgUpgradeCommand(packages []string) ([]string, error) {
	if len(packages) == 0 {
		return nil, ErrNothingToDo
	}
	for _, name := range packages {
		if !validPackageName(name) {
			return nil, fmt.Errorf("hostpkg: refusing to build a command naming %q, which is not a valid package name", name)
		}
	}
	path := a.pkgPath
	if path == "" {
		path = "pkg"
	}
	flags := a.extraArgs
	if flags == nil {
		flags = defaultUpgradeFlags
	}
	argv := make([]string, 0, 1+len(flags)+len(packages))
	argv = append(argv, path)
	argv = append(argv, flags...)
	argv = append(argv, packages...)
	return argv, nil
}

// CommandFor exposes pkgUpgradeCommand for testing and documentation.
func (a *RunnerApplier) CommandFor(packages []string) ([]string, error) {
	return a.pkgUpgradeCommand(packages)
}

// Upgrade runs the single permitted command. The package list has been
// validated again here even though Apply already validated it: this is
// the boundary where root is actually reached, so it re-checks rather
// than trusting its caller.
func (a *RunnerApplier) Upgrade(ctx context.Context, packages []string) (UpgradeResult, error) {
	argv, err := a.pkgUpgradeCommand(packages)
	if err != nil {
		return UpgradeResult{}, err
	}
	runner := a.runner
	if runner == nil {
		runner = NewExecRunner()
	}
	stdout, stderr, runErr := runner.Run(ctx, argv[0], argv[1:]...)
	result := UpgradeResult{Command: argv, Stdout: stdout, Stderr: stderr}
	if runErr != nil {
		result.ExitCode = exitCodeOf(runErr)
		return result, runErr
	}
	return result, nil
}

// Record is the append-only account of one apply attempt. It is what
// "it must record what it did" means concretely: every attempt produces
// exactly one Record, including the refusals, and no Record is ever
// removed or rewritten.
type Record struct {
	// Seq is a monotonically increasing per-Collector sequence number,
	// so two records from the same host can never be confused.
	Seq int

	// Outcome is the verdict. See ApplyOutcome - in particular
	// ApplyUnknown is a real, reportable result, not an error to be
	// hidden.
	Outcome ApplyOutcome

	// PlanFingerprint is the plan that was submitted.
	PlanFingerprint string

	// PlanSummary is the plan's own one-line summary at submit time.
	PlanSummary string

	// Phrase is the confirmation phrase that was supplied. It is
	// recorded because "the operator typed the wrong thing" is a
	// materially different event from "the operator confirmed and it
	// went wrong", and an audit needs to tell them apart.
	Phrase string

	// Privilege is the root check's own detail, verbatim.
	Privilege string

	// Gate is the plan's safety gate at submit time.
	Gate Gate

	// StartedAt/FinishedAt bound the attempt.
	StartedAt  time.Time
	FinishedAt time.Time

	// Packages is the exact set of package names that were, or would
	// have been, upgraded. Empty on a refusal before the set was
	// derived.
	Packages []string

	// Command is the argv actually executed, or nil when nothing ran.
	Command []string

	// ExitCode is the privileged command's exit status, or -1 when no
	// command ran.
	ExitCode int

	// StdoutTail/StderrTail are bounded tails of the command's output.
	// Bounded because a package build log can be arbitrarily long and a
	// record is meant to be kept, not to become a log archive.
	StdoutTail string
	StderrTail string

	// Evidence is the post-update read that produced Outcome. It is the
	// actual observation, kept so an operator can see the fact rather
	// than only this build's conclusion from it - the same contract
	// internal/cluster's ReplicaSyncEvidence provides.
	Evidence *PostEvidence

	// Detail is the human-readable reason for Outcome, always non-empty
	// for a non-applied outcome.
	Detail string
}

// PostEvidence is the post-apply re-read that decided the outcome.
type PostEvidence struct {
	Observed      bool
	StillOutdated []string
	Unknown       []string

	// Divergent names packages that are confirmed no longer outdated
	// but are not at the version the plan named, as "name: installed X,
	// planned Y". It is the raw evidence behind ApplyDiverged.
	Divergent []string

	Detail string
}

// outputTailLimit bounds how much command output a Record keeps. 4 KiB
// is enough to see an error and a summary, and small enough that a
// record can be held in memory for the life of a process without care.
const outputTailLimit = 4096

// outputTail trims s to outputTailLimit, marking that it was trimmed -
// a silently truncated log reads exactly like a complete one.
func outputTail(s string) string {
	if len(s) <= outputTailLimit {
		return s
	}
	return s[:outputTailLimit] + fmt.Sprintf("\n[truncated: %d more bytes]", len(s)-outputTailLimit)
}

// exitCodeOf extracts a process exit status from an *exec.ExitError
// without importing os/exec here. A non-exit error has no status, which
// is reported as -1 rather than as 0 - a command that could not be
// started has not succeeded.
func exitCodeOf(err error) int {
	type exitcoder interface{ ExitCode() int }
	if ec, ok := err.(exitcoder); ok {
		return ec.ExitCode()
	}
	return -1
}

// ApplyRequest is everything Apply needs. It is a struct rather than a
// long parameter list so that adding a required field later is a
// compile error at every call site rather than a silently-defaulted
// argument.
type ApplyRequest struct {
	// Plan is the preview the operator inspected. It must be a plan
	// produced by PlanFrom from a real Inventory: a zero Plan is
	// refused with ErrNotAnInventory.
	Plan Plan

	// Phrase must equal Plan.RequiredConfirmPhrase() exactly.
	Phrase string
}

// Apply performs the plan, once, explicitly, and records what happened.
//
// # The success criterion
//
// Apply's outcome is decided by a fresh inventory read AFTER the
// command, not by the command's exit status. An exit status of 0 is
// necessary but not sufficient, and this is deliberate: pkg can exit 0
// having done nothing (a package it considered already current), can
// exit 0 having installed a version the plan did not predict, and can
// exit non-zero having already applied most of the upgrade. Only a
// post-condition read can establish what is actually true, and only
// that read can honestly say "applied".
//
// When the post-condition read cannot be made - the host's pkg is now
// broken, the command is still running past the deadline, the read
// failed - the outcome is ApplyUnknown, never ApplyApplied and never
// ApplyFailed. A caller that cannot distinguish those two is exactly the
// caller that will eventually report a half-finished upgrade as a
// success.
//
// # The refusal order
//
// The confirmation phrase is checked first, before privilege, staleness,
// or emptiness - matching ADR-0113's confirm_phrase-first precedent in
// internal/manager. A mistyped phrase must never produce a
// half-action, and the cheapest possible response to a mistyped phrase
// is to do nothing at all.
func (c *Collector) Apply(ctx context.Context, req ApplyRequest) Record {
	started := c.opts.Now()
	rec := Record{
		Seq:             c.nextSeq(),
		Outcome:         ApplyRefused,
		PlanFingerprint: req.Plan.InventoryFingerprint,
		PlanSummary:     req.Plan.Summary(),
		Phrase:          req.Phrase,
		Gate:            req.Plan.Safety.Gate,
		StartedAt:       started,
		ExitCode:        -1,
	}

	// 1. The phrase, before anything else.
	if want := req.Plan.RequiredConfirmPhrase(); req.Phrase != want {
		rec.Detail = fmt.Sprintf("confirmation phrase %q does not match the required phrase %q - nothing was run", req.Phrase, want)
		return c.finish(rec)
	}

	// 2. A plan this package can vouch for.
	if req.Plan.InventoryFingerprint == "" {
		rec.Detail = ErrNotAnInventory.Error() + " - nothing was run"
		return c.finish(rec)
	}

	// 3. Root.
	root, privilegeDetail := c.opts.Privilege.Describe()
	rec.Privilege = privilegeDetail
	if !root {
		rec.Detail = fmt.Sprintf("%v: %s - nothing was run", ErrNotPrivileged, privilegeDetail)
		return c.finish(rec)
	}

	// 4. v1's own boundary: no removals.
	if req.Plan.HasRemoval() {
		rec.Detail = ErrRemovalUnsupported.Error() + " - the plan contains a removal, so nothing was run"
		return c.finish(rec)
	}

	// 5. Something to do.
	packages := req.Plan.Actionable()
	if len(packages) == 0 && req.Plan.Base != BaseActionUpdate {
		rec.Detail = ErrNothingToDo.Error() + " - nothing was run"
		return c.finish(rec)
	}
	rec.Packages = append([]string(nil), packages...)

	// 6. The plan must still match reality.
	live, err := c.Collect(ctx)
	if err != nil {
		rec.Detail = fmt.Sprintf("the live inventory could not be read (%v), so it could not be confirmed the plan still applies - nothing was run", err)
		return c.finish(rec)
	}
	if live.Fingerprint() != req.Plan.InventoryFingerprint {
		rec.Detail = fmt.Sprintf("%v: the previewed inventory has changed since it was generated (%s then, %s now) - regenerate the plan and inspect it again, nothing was run",
			ErrPlanStale, short(req.Plan.InventoryFingerprint), short(live.Fingerprint()))
		return c.finish(rec)
	}

	// 7. Actually do it. v1's scope is Ports packages only: a base
	// system update is a reboot, not a package install, and modelling
	// the reboot - its timing, its quorum consequences, whether this
	// host is even the one that should be rebooted - is a different and
	// considerably more dangerous feature. It is surfaced in the plan
	// and refused here rather than half-implemented.
	if req.Plan.Base == BaseActionUpdate {
		rec.Detail = "this version does not perform base system updates - the plan requires a reboot on this Comb, which must be scheduled deliberately outside this workflow - no package command was run"
		return c.finish(rec)
	}

	applier := c.currentApplier()
	result, upgradeErr := applier.Upgrade(ctx, packages)
	rec.Command = result.Command
	rec.ExitCode = result.ExitCode
	rec.StdoutTail = outputTail(result.Stdout)
	rec.StderrTail = outputTail(result.Stderr)

	// 8. The post-condition read, which - not the exit status - decides.
	evidence, postErr := c.postEvidence(ctx, req.Plan.Changes)
	rec.Evidence = evidence
	if postErr != nil {
		rec.Outcome = ApplyUnknown
		rec.Detail = fmt.Sprintf("the upgrade command was issued but its outcome could not be established: %v; this is 'could not determine', not success and not failure - inspect the host by hand before assuming anything", postErr)
		return c.finish(rec)
	}
	switch {
	case !evidence.Observed:
		rec.Outcome = ApplyUnknown
		rec.Detail = "the upgrade command was issued but the host's package state could not be re-read afterwards, so the result is unknown - not success and not failure"
	case len(evidence.Unknown) > 0:
		rec.Outcome = ApplyUnknown
		rec.Detail = fmt.Sprintf("after the upgrade, %d package(s) could not be confirmed: %s - the result is unknown, not success",
			len(evidence.Unknown), strings.Join(evidence.Unknown, ", "))
	case len(evidence.StillOutdated) > 0 && upgradeErr != nil:
		rec.Outcome = ApplyFailed
		rec.Detail = fmt.Sprintf("the upgrade command failed (exit %d) and %d package(s) are still outdated: %s",
			result.ExitCode, len(evidence.StillOutdated), strings.Join(evidence.StillOutdated, ", "))
	case len(evidence.StillOutdated) > 0:
		// Exit status 0 with packages still outdated: pkg reported
		// success and did not do the work. This is a real, separately
		// interesting failure and must not be reported as success.
		rec.Outcome = ApplyFailed
		rec.Detail = fmt.Sprintf("the upgrade command reported success (exit %d) but %d package(s) are still outdated afterwards: %s - pkg did not do what was asked",
			result.ExitCode, len(evidence.StillOutdated), strings.Join(evidence.StillOutdated, ", "))
	case len(evidence.Divergent) > 0:
		// Every package is current, but not at the version the operator
		// previewed. Known, and not the intended outcome - so it gets
		// its own verdict rather than a green one.
		rec.Outcome = ApplyDiverged
		rec.Detail = fmt.Sprintf("every package is current, but %d landed at a version this plan did not name: %s - Apiary cannot report this as the upgrade that was previewed; check the host by hand",
			len(evidence.Divergent), strings.Join(evidence.Divergent, "; "))
	default:
		rec.Outcome = ApplyApplied
		rec.Detail = fmt.Sprintf("all %d package(s) re-read after the upgrade and confirmed installed at the version the plan named", len(packages))
	}
	return c.finish(rec)
}

// postEvidence re-reads the host and reports what is still true. It is
// the only thing permitted to decide Apply's outcome.
//
// It compares against each change's TargetVersion, not merely against
// "is this package still outdated". Those are different questions, and
// answering only the second one would let Apiary report a successful
// upgrade to a version the operator never previewed.
func (c *Collector) postEvidence(ctx context.Context, changes []Change) (*PostEvidence, error) {
	live, err := c.Collect(ctx)
	if err != nil {
		return &PostEvidence{Observed: false, Detail: err.Error()}, err
	}
	wanted := make(map[string]string, len(changes))
	for _, change := range changes {
		wanted[change.Package.Name] = change.TargetVersion
	}
	ev := &PostEvidence{Observed: true}
	seen := make(map[string]bool, len(changes))
	for _, p := range live.Ports {
		target, ok := wanted[p.Name]
		if !ok {
			continue
		}
		seen[p.Name] = true
		switch p.UpdateStatus {
		case UpdateStatusOutdated:
			ev.StillOutdated = append(ev.StillOutdated, p.Name)
		case UpdateStatusUnknown:
			ev.Unknown = append(ev.Unknown, p.Name)
		default:
			// Confirmed current. That is still not necessarily the
			// version the plan named, so check it before letting this
			// count as the intended upgrade.
			if target != "" && p.Version != target {
				ev.Divergent = append(ev.Divergent, fmt.Sprintf("%s: installed %s, planned %s", p.Name, p.Version, target))
			}
		}
	}
	// A package that has vanished from the installed list is not
	// evidence of success either - this package never removes anything,
	// so its disappearance means the read is not describing the same
	// host, which is itself an unknown rather than a pass.
	var missing []string
	for name := range wanted {
		if !seen[name] {
			missing = append(missing, name)
		}
	}
	sort.Strings(ev.StillOutdated)
	sort.Strings(ev.Unknown)
	sort.Strings(ev.Divergent)
	sort.Strings(missing)
	ev.Unknown = append(ev.Unknown, missing...)
	ev.Detail = fmt.Sprintf("re-read %d package(s): %d still outdated, %d at an unexpected version, %d unconfirmed",
		len(wanted), len(ev.StillOutdated), len(ev.Divergent), len(ev.Unknown))
	return ev, nil
}

// nextSeq hands out the next record sequence number.
func (c *Collector) nextSeq() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.seq++
	return c.seq
}

// finish stamps the record's end time and appends it to this
// Collector's history. Every attempt is recorded, including every
// refusal: an audit that only records successes cannot answer "did
// anyone try to do this".
func (c *Collector) finish(rec Record) Record {
	rec.FinishedAt = c.opts.Now()
	c.mu.Lock()
	c.records = append(c.records, rec)
	c.mu.Unlock()
	return rec
}

// Records returns a copy of this Collector's apply history, oldest
// first. It is in-memory and bounded by the life of the process: this
// package deliberately does not choose a persistence format or path on
// the operator's behalf. A caller that needs records to outlive the
// process should copy them where it chooses - see the report's
// integration notes.
func (c *Collector) Records() []Record {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]Record(nil), c.records...)
}

// short trims a fingerprint for display in a sentence.
func short(fingerprint string) string {
	if len(fingerprint) <= 12 {
		return fingerprint
	}
	return fingerprint[:12]
}
