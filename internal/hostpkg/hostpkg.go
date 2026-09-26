// Package hostpkg answers, per Comb, the two questions a FreeBSD
// operator actually asks about packages: what is installed right now,
// and is anything out of date. It covers the FreeBSD base system and
// the installed Ports packages, and it deliberately keeps "we could
// not tell" as a first-class, separately-typed answer that can never
// be rendered as "up to date".
//
// # Why the honesty constraint dominates the design
//
// pkg's own "is there an update?" answer is only as good as the local
// package catalogue, and refreshing that catalogue is `pkg update` -
// which this package never runs, on any host, ever (see "What this
// package will not do"). A host whose catalogue has not been refreshed
// in a month will still answer "up to date", because pkg genuinely
// does not know better. Reporting that verbatim as "up to date" would
// be a confident, wrong, and operationally dangerous answer, so this
// package refuses to: every update-available verdict is gated on an
// independently-observed catalogue freshness (see Catalogue), and a
// stale or unreadable catalogue downgrades the whole answer to
// UpdateStatusUnknown with the reason recorded in Inventory.Unknown.
//
// The verdict vocabulary deliberately matches internal/cluster's own
// (internal/cluster/simulate.go): a positive statement is only made
// from a positive observation, and every non-positive state is named
// rather than folded into "fine". internal/cluster's
// RecoveryVerdictReplicaUnobserved is the direct precedent - "we could
// not check" is neither "checked and healthy" nor "checked and broken",
// and it has its own rendering and its own explanation.
//
// # Privilege: the blast radius, stated plainly
//
// Reading needs no privilege at all. `pkg query -e`, `pkg outdated`,
// `pkg version`, `uname` and `sysctl` are all readable by any local
// user, so Collect, PlanFrom and Inventory.Fingerprint work fine as an
// unprivileged process, and the read half of this feature is therefore
// safe to expose broadly.
//
// Writing does need root, and this package's answer to that is narrow
// on purpose:
//
//   - Root is reached through exactly one interface, Applier, and the
//     only implementation in this package (pkgUpgradeApplier) runs one
//     hard-coded command vector that it builds itself. Nothing in this
//     package ever passes operator-supplied text to a shell, ever
//     accepts an argument list from a caller, or ever constructs a
//     command name from data.
//   - The only packages that can appear in that vector are ones the
//     inventory directly observed as installed, and each name is
//     additionally checked against a conservative allowlist pattern.
//   - There is deliberately no generic "run this command as root"
//     escape hatch here, and no network-reachable surface is wired to
//     Applier by this package. Whether managerd should expose Apply at
//     all - and to whom - is a privilege-model decision that belongs
//     to internal/manager and the operator, not to this file. See the
//     package's "Integration still required" notes in the commit that
//     introduced it.
//
// # What this package will not do
//
//   - It never runs `pkg update` or `pkg upgrade` implicitly, and never
//     runs `pkg upgrade` at all without an explicit Apply call
//     carrying an exact confirmation phrase.
//   - It has no scheduler, no timer, no goroutine and no periodic
//     refresh. There is no unattended-upgrade path in v1, by design:
//     a silent package update on a host running HAST or ZFS can break
//     a voter, and a "helpful" unattended upgrade is exactly the kind
//     of thing that does that.
//   - It never removes a package. ActionRemove exists in the plan
//     vocabulary so an operator can see that a removal was considered,
//     but Apply refuses a plan containing one (ErrRemovalUnsupported).
//
// # Testability
//
// Every FreeBSD command is reached through Runner, and every file read
// through statter, so the whole package - inventory, plan and apply -
// is exercised by ordinary unit tests on macOS. Nothing here needs a
// live FreeBSD host, and nothing here was ever run against one. The
// command output shapes the parsers accept are documented per parser;
// they are unverified against a live pkg, and a parse failure always
// degrades to unknown rather than to a guess.
//
// # Integration still required (deliberately not built here)
//
// This package is a library. Nothing reaches it yet, on purpose.
//
//   - Reading: a GetHostPackages RPC in api/rpc/manager.proto plus a
//     handler in internal/manager/server.go, both of which are outside
//     this change's ownership. The frontend page that consumes such a
//     source already exists (internal/frontend/hostpkg_page.go) and
//     renders an honest "unavailable" panel until it is wired.
//   - Writing: whether managerd should expose Apply at all, and to
//     whom, is a privilege-model decision. Two things have to be settled
//     before it can be: which role may trigger a root package upgrade
//     (the frontend's Admin tier is the obvious candidate, matching
//     every other host-wide write), and whether a confirmation has to
//     survive a managerd restart the way internal/manager's own
//     pending-restart lease does. This package's in-memory Record
//     history deliberately does not answer that.
//
// Records are held in memory for the life of the process. Choosing a
// path, format and retention policy for them on the operator's disk is
// the same kind of decision, and is left to whoever wires Apply up.
package hostpkg

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

// Runner executes one real host command, returning stdout and stderr
// separately - the same shape internal/install.Runner already uses in
// this codebase, so a caller holding either interface can be adapted
// with a two-line wrapper and tests can fake one trivially.
type Runner interface {
	Run(ctx context.Context, name string, args ...string) (stdout, stderr string, err error)
}

// execRunner is the real, host-shelling Runner. It is deliberately the
// only place in this package that calls exec.
type execRunner struct{}

// NewExecRunner returns the Runner every real (non-test) caller should use.
func NewExecRunner() Runner { return execRunner{} }

func (execRunner) Run(ctx context.Context, name string, args ...string) (stdout, stderr string, err error) {
	cmd := exec.CommandContext(ctx, name, args...)
	var outBuf, errBuf bytes.Buffer
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf
	err = cmd.Run()
	return outBuf.String(), errBuf.String(), err
}

// statter is the one file read this package performs: the mtime of
// pkg's local catalogue database. Narrowed to exactly what is needed
// so a test can supply an age without touching the filesystem.
type statter interface {
	ModTime(path string) (t time.Time, err error)
}

type osStatter struct{}

func (osStatter) ModTime(path string) (time.Time, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return time.Time{}, err
	}
	return fi.ModTime(), nil
}

// Privilege reports whether the running process is able to perform an
// action that needs root. It exists so Apply's root requirement is an
// injectable, testable check rather than an os.Geteuid() buried in the
// middle of a code path that therefore could only be tested as root.
type Privilege interface {
	// Describe returns whether this process is root, plus a
	// human-readable reason naming what it actually observed. The reason
	// is recorded in the refusal so an operator is never told only
	// "permission denied" with no account of which check ran.
	Describe() (root bool, detail string)
}

type euidPrivilege struct{}

// NewEUIDPrivilege returns the Privilege implementation for real use.
// It inspects only the effective UID; it never attempts to change it.
func NewEUIDPrivilege() Privilege { return euidPrivilege{} }

func (euidPrivilege) Describe() (bool, string) {
	if os.Geteuid() == 0 {
		return true, "effective uid is 0"
	}
	return false, fmt.Sprintf("effective uid is %d, not 0", os.Geteuid())
}

// packageNamePattern is the conservative allowlist a package name must
// match before it is ever allowed anywhere near a command vector. It is
// intentionally narrower than what pkg actually permits (it rejects
// `+` in a leading position, any `/`, any `:` and any whitespace), so
// that even a bug in the inventory join cannot produce an argument that
// looks like a flag, a path, or a second command.
var packageNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9+._-]*$`)

// validPackageName reports whether name is safe to place in this
// package's own argv. Empty and near-empty names are rejected: a name
// that is only dots would be a path traversal attempt, not a package.
func validPackageName(name string) bool {
	return packageNamePattern.MatchString(name) && strings.Trim(name, ".") != ""
}

// Errors returned by this package. Every one of them is a refusal: no
// command was run, and nothing was changed on the host.
var (
	// ErrNotPrivileged means Apply was asked to write without root. No
	// package was touched.
	ErrNotPrivileged = fmt.Errorf("hostpkg: apply requires root")

	// ErrPhraseMismatch means the caller's confirmation phrase was
	// absent or wrong. Checked before anything else, matching ADR-0113's
	// confirm_phrase-first precedent in internal/manager, so a
	// mistyped phrase can never half-apply a plan.
	ErrPhraseMismatch = fmt.Errorf("hostpkg: confirmation phrase did not match")

	// ErrPlanStale means the live inventory no longer matches the
	// inventory the operator previewed. The plan is refused rather than
	// silently re-derived: an operator must see the changed plan, not
	// this one.
	ErrPlanStale = fmt.Errorf("hostpkg: the live inventory no longer matches the previewed plan")

	// ErrNothingToDo means the plan contains no upgrade or reinstall
	// action. Nothing was run.
	ErrNothingToDo = fmt.Errorf("hostpkg: the plan contains no upgrade or reinstall action")

	// ErrRemovalUnsupported is v1's explicit refusal to remove a
	// package. The plan vocabulary can describe a removal; this version
	// will not perform one.
	ErrRemovalUnsupported = fmt.Errorf("hostpkg: removing packages is not supported in this version")

	// ErrNotAnInventory means the supplied Plan was not produced by
	// PlanFrom (a zero value, most likely). Applying a plan this
	// package cannot vouch for is refused.
	ErrNotAnInventory = fmt.Errorf("hostpkg: the supplied plan has no inventory fingerprint")
)
