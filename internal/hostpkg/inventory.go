package hostpkg

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// UpdateStatus is the per-item answer to "is there a newer version?",
// and the type exists specifically so that "we could not tell" is a
// value of its own rather than a bool that defaults to false. The
// vocabulary matches internal/cluster/simulate.go's discipline exactly:
// a positive verdict requires a positive observation, and the absence
// of an observation is named rather than inferred.
type UpdateStatus string

const (
	// UpdateStatusCurrent means a current, readable package catalogue
	// positively reported no newer version. It is never produced from a
	// stale or unreadable catalogue - that case is
	// UpdateStatusUnknown, which is the whole point of this type.
	UpdateStatusCurrent UpdateStatus = "current"

	// UpdateStatusOutdated means pkg positively reported a newer version,
	// and the candidate version is carried alongside it.
	UpdateStatusOutdated UpdateStatus = "outdated"

	// UpdateStatusUnknown means the answer could not be determined. It
	// is NOT a synonym for current, NOT a synonym for failed, and never
	// a reason to skip an item: it means this build has no evidence, and
	// the reason is carried in the accompanying Detail/Unknown entry.
	UpdateStatusUnknown UpdateStatus = "unknown"
)

// CatalogueState is how much this build trusts the local pkg catalogue.
// It is observed independently of pkg's own "up to date" answer, and it
// gates that answer: pkg cannot know about an update that was never
// fetched, so an old catalogue makes every current-verdict this package
// would otherwise give unknowable. Refreshing a catalogue is `pkg
// update`, which this package never runs.
type CatalogueState string

const (
	// CatalogueFresh means the catalogue's mtime was read and is within
	// the configured maximum age.
	CatalogueFresh CatalogueState = "fresh"

	// CatalogueStale means the catalogue's mtime was read and is older
	// than the configured maximum age. Every update verdict from this
	// host is downgraded to unknown.
	CatalogueStale CatalogueState = "stale"

	// CatalogueUnknown means the catalogue's mtime could not be read at
	// all - the file is not where this build expects it (pkg configured
	// with a non-default SQLITE_PATH), or it is unreadable. Same
	// consequence as CatalogueStale: no verdict is made.
	CatalogueUnknown CatalogueState = "unknown"
)

// BasePackageName is the name pkg gives the FreeBSD base system itself
// on a pkgbase host. It is excluded from the Ports inventory on purpose:
// the brief is "base system" *and* "installed Ports packages", and
// listing the base twice under both headings invites an operator to
// schedule the same reboot twice.
const BasePackageName = "FreeBSD"

// DefaultCataloguePath is where pkg keeps its local catalogue database
// under its default configuration (SQLITE_PATH=/var/db/pkg). It is a
// var rather than a const purely so a test can point it at a temp file;
// a host with a non-default SQLITE_PATH must pass the real path in
// Options, and getting that wrong is safe - the stat fails and the
// answer becomes CatalogueUnknown, not a false "up to date".
var DefaultCataloguePath = "/var/db/pkg/local.sqlite"

// DefaultMaxCatalogueAge is the age past which a catalogue is treated
// as too old to support an "up to date" claim. Fourteen days is a
// deliberately conservative default: a weekly-ish `pkg update` is a
// common operator habit, and the cost of being wrong in the safe
// direction here is only an honest "unknown", never a missed update.
const DefaultMaxCatalogueAge = 14 * 24 * time.Hour

// Options configures a Collector. Every field is injectable so the
// package is exercisable without a FreeBSD host; the zero value is
// valid and means "real Runner, real clock, real stat, defaults".
type Options struct {
	// Runner executes the pkg/uname commands. Nil means NewExecRunner.
	Runner Runner

	// Privilege is used only by Apply, never by Collect. Nil means
	// NewEUIDPrivilege.
	Privilege Privilege

	// Now returns the current time. Nil means time.Now. An Inventory's
	// ObservedAt, its catalogue age and its whole verdict depend on
	// this, so a test needs to control it.
	Now func() time.Time

	// Stat reads a file's mtime. Nil means the real os.Stat.
	stat statter

	// CataloguePath overrides DefaultCataloguePath.
	CataloguePath string

	// MaxCatalogueAge overrides DefaultMaxCatalogueAge. Zero means the
	// default; a negative value means "never treat a catalogue as
	// fresh", which makes every verdict unknown and is a supported mode,
	// not a degenerate one.
	MaxCatalogueAge time.Duration
}

func (o Options) withDefaults() Options {
	if o.Runner == nil {
		o.Runner = NewExecRunner()
	}
	if o.Privilege == nil {
		o.Privilege = NewEUIDPrivilege()
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.stat == nil {
		o.stat = osStatter{}
	}
	if o.CataloguePath == "" {
		o.CataloguePath = DefaultCataloguePath
	}
	if o.MaxCatalogueAge == 0 {
		o.MaxCatalogueAge = DefaultMaxCatalogueAge
	}
	return o
}

// Collector produces inventories. It is safe for concurrent use: it
// holds configuration and a bounded in-memory record history, and every
// read is a fresh, independent set of command executions.
//
// A Collector deliberately does NOT cache an inventory between calls.
// A cached answer on a page an operator leaves open for ten minutes is
// a stale answer presented as current, which is precisely the class of
// bug this package refuses to have.
type Collector struct {
	opts Options

	// applier is nil-able, matching internal/manager.Server's own
	// nil-able-settable-dependency convention. Nil means the real
	// RunnerApplier, which is the only thing that ever runs anything as
	// root. It is settable purely so tests can drive the whole apply
	// path - including the post-condition read that decides the outcome -
	// without root and without a FreeBSD host.
	applier Applier

	mu      sync.Mutex
	seq     int
	records []Record
}

// NewCollector returns a Collector configured by opts.
func NewCollector(opts Options) *Collector {
	return &Collector{opts: opts.withDefaults()}
}

// SetApplier wires the privileged boundary. Nil (the default) means the
// real pkg-backed Applier. Setting it to a fake is how the apply path is
// unit-tested; setting it to nil restores real behaviour.
//
// Deliberately a method rather than an Options field: a Collector is
// long-lived, and an Applier is the one dependency whose replacement
// should be an explicit, greppable act at the call site rather than a
// value buried in a configuration struct.
func (c *Collector) SetApplier(a Applier) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.applier = a
}

// currentApplier returns the configured Applier, defaulting to the real
// one.
func (c *Collector) currentApplier() Applier {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.applier == nil {
		return NewRunnerApplier()
	}
	return c.applier
}

// BaseSystem is what was observed about the FreeBSD base itself.
type BaseSystem struct {
	// OSRelease is `uname -r` - the release string of the kernel
	// actually running right now. This is the version a reboot would
	// replace, which is why it is the running kernel and not whatever
	// /etc/version claims.
	OSRelease string

	// OSVersion is `uname -v` - FreeBSD's own version string. Carried
	// for display; it is not used to derive any verdict.
	OSVersion string

	// PkgBasePackage is true when the base system is itself an
	// installed pkg package named "FreeBSD" (a pkgbase host), which
	// means `pkg upgrade` can update it. False means the base is
	// updated by some other mechanism (src, freebsd-update), which
	// this package does not model or touch.
	PkgBasePackage bool

	// UpdateStatus is the base's update verdict, gated on Catalogue
	// exactly as every Ports verdict is.
	UpdateStatus UpdateStatus

	// Detail is pkg version's own output, verbatim, so an operator can
	// see the raw statement instead of only this build's reading of it.
	Detail string

	// Observed is false when even `uname -r` could not be read. No
	// verdict in this struct may be read as evidence-backed when it is
	// false.
	Observed bool
}

// InstalledPackage is one installed package's identity and update
// verdict.
type InstalledPackage struct {
	Name    string
	Version string
	Origin  string

	// UpdateStatus is this package's own verdict.
	UpdateStatus UpdateStatus

	// Candidate is the newer version pkg reported. Non-empty only when
	// UpdateStatus is UpdateStatusOutdated - an unknown status has no
	// candidate, and this must never be defaulted to Version.
	Candidate string

	// Detail carries the reason behind a non-current verdict, or the
	// verbatim line for an unknown one.
	Detail string
}

// Catalogue is the independent observation of how old this host's pkg
// catalogue is. It exists because pkg's own answer cannot be trusted
// without it - see the package doc comment.
type Catalogue struct {
	Path       string
	State      CatalogueState
	ModifiedAt time.Time
	Age        time.Duration
	Detail     string
}

// Unknown records one thing this inventory could not determine, and why.
// It is the inventory's own "here is my evidence gap" list: an operator
// reading the page can see exactly which questions went unanswered
// instead of having to infer it from a suspiciously tidy table.
type Unknown struct {
	// Subject names what could not be determined, e.g.
	// "base system", "package catalogue", "pkg query output",
	// "pkg outdated output", or a specific package name.
	Subject string

	// Reason is the specific evidence gap, including any command's own
	// error text verbatim. It is never empty.
	Reason string
}

// Headline is the single top-level answer for a host's package state.
type Headline string

const (
	// HeadlineUpToDate means every base and package verdict was
	// positively observed as current. This is the only path to this
	// value: it is unreachable while any Unknown entry exists.
	HeadlineUpToDate Headline = "up_to_date"

	// HeadlineUpdatesAvailable means at least one update is positively
	// observed as available, and nothing at all is unknown.
	HeadlineUpdatesAvailable Headline = "updates_available"

	// HeadlineUnknown means at least one question could not be
	// determined. This deliberately outranks HeadlineUpdatesAvailable:
	// "something needs updating and I cannot tell you what else" is
	// one honest answer, and splitting it into a positive-looking
	// headline plus a footnote is how a stale catalogue becomes a
	// missed security update.
	HeadlineUnknown Headline = "unknown"
)

// Inventory is one point-in-time read of a single Comb's package state.
// It is a value, not a live view: it is a fact about a moment, and
// ObservedAt is when that moment was.
type Inventory struct {
	// ObservedAt is when these commands ran.
	ObservedAt time.Time

	// Base is the observed FreeBSD base system.
	Base BaseSystem

	// Ports is every installed package except the base package itself,
	// sorted by name. Never nil, so a template can range over it and
	// get zero rows rather than an error.
	Ports []InstalledPackage

	// Catalogue is the independent catalogue-freshness observation.
	Catalogue Catalogue

	// Unknown lists every evidence gap that shaped the verdicts above.
	Unknown []Unknown

	// RawBaseVersionOutput is `pkg version`'s verbatim stdout, retained
	// so an operator can see exactly what pkg said when this build's
	// reading of it is unclear.
	RawBaseVersionOutput string

	// RawOutdatedOutput is `pkg outdated`'s verbatim stdout, for the
	// same reason.
	RawOutdatedOutput string
}

// Headline folds the whole inventory into one answer. See the Headline
// constants for why unknown outranks "updates available".
func (i Inventory) Headline() Headline {
	if len(i.Unknown) > 0 {
		return HeadlineUnknown
	}
	if i.Base.UpdateStatus == UpdateStatusOutdated {
		return HeadlineUpdatesAvailable
	}
	for _, p := range i.Ports {
		if p.UpdateStatus == UpdateStatusOutdated {
			return HeadlineUpdatesAvailable
		}
	}
	return HeadlineUpToDate
}

// OutdatedCount is the number of positively-observed outdated packages.
// It is a count of evidence, not a completeness claim: it says nothing
// about how many are unknown.
func (i Inventory) OutdatedCount() int {
	n := 0
	for _, p := range i.Ports {
		if p.UpdateStatus == UpdateStatusOutdated {
			n++
		}
	}
	return n
}

// UnknownCount is the number of evidence gaps recorded in Unknown.
func (i Inventory) UnknownCount() int { return len(i.Unknown) }

// Fingerprint is a stable digest of everything in this inventory that a
// plan depends on. Apply recomputes it from a fresh read and refuses to
// act when it no longer matches, so an operator can never apply a
// preview of a state that has since changed. It deliberately excludes
// ObservedAt and every human-readable Detail string, so a re-read at a
// different moment of an unchanged host still matches.
func (i Inventory) Fingerprint() string {
	var b strings.Builder
	fmt.Fprintf(&b, "osrelease=%s\x00", i.Base.OSRelease)
	fmt.Fprintf(&b, "base=%s\x00", i.Base.UpdateStatus)
	fmt.Fprintf(&b, "pkgbase=%t\x00", i.Base.PkgBasePackage)
	fmt.Fprintf(&b, "catalogue=%s\x00", i.Catalogue.State)
	for _, p := range i.Ports {
		fmt.Fprintf(&b, "pkg=%s|%s|%s|%s\x00", p.Name, p.Version, p.UpdateStatus, p.Candidate)
	}
	return digest(b.String())
}

// Collect performs one complete, read-only inventory of the local host.
//
// It is deliberately total: it returns a usable Inventory describing
// everything it could not determine rather than an error, because "this
// host's package state is unknown" is a real answer an operator needs
// to see, not a failure to hide. In the current implementation it
// therefore never returns a non-nil error - every subsystem failure is
// an Unknown entry, including a missing pkg entirely - but the error is
// kept in the signature so that a future subsystem with a genuinely
// unrecoverable setup problem does not force every caller to change.
//
// Collect never runs `pkg update`, and never runs `pkg upgrade`.
func (c *Collector) Collect(ctx context.Context) (Inventory, error) {
	now := c.opts.Now()
	inv := Inventory{ObservedAt: now}

	// Catalogue first: it gates every update verdict below, and doing it
	// first means each downstream "unknown" can name the catalogue as
	// its own contributing reason rather than the two being reported as
	// unrelated failures.
	inv.Catalogue = c.observeCatalogue(now)

	inv.Base = c.observeBase(ctx)
	installed, unparsedQuery := c.observeInstalled(ctx)

	// The base package, when pkgbase is in use, is the base system's
	// own record - it is removed from the Ports list so the two
	// headings never duplicate the same package.
	for _, p := range installed {
		if p.Name == BasePackageName {
			inv.Base.PkgBasePackage = true
		}
	}
	ports := make([]InstalledPackage, 0, len(installed))
	for _, p := range installed {
		if p.Name != BasePackageName {
			ports = append(ports, p)
		}
	}
	sort.Slice(ports, func(i, j int) bool { return ports[i].Name < ports[j].Name })
	inv.Ports = ports

	if len(unparsedQuery) > 0 {
		inv.Unknown = append(inv.Unknown, Unknown{
			Subject: "installed package list",
			Reason: fmt.Sprintf("`pkg query` produced %d line(s) this build cannot read: %s",
				len(unparsedQuery), strings.Join(quoteAll(unparsedQuery), "; ")),
		})
	}

	// The installed list is read exactly once and threaded into the
	// outdated join, so both halves of the inventory are resolved
	// against a single consistent picture. A host genuinely changing
	// underneath the read is caught by the fingerprint staleness check
	// in Apply, not by re-reading here and hoping the two agree.
	outdated := c.observeOutdated(ctx, installed)
	inv.RawOutdatedOutput = outdated.Raw
	if outdated.Err != "" {
		inv.Unknown = append(inv.Unknown, Unknown{
			Subject: "update availability",
			Reason:  "`pkg outdated` could not be read: " + outdated.Err,
		})
	}
	if len(outdated.Unparsed) > 0 {
		inv.Unknown = append(inv.Unknown, Unknown{
			Subject: "update availability",
			Reason: fmt.Sprintf("`pkg outdated` produced %d line(s) this build cannot read, so no package can be called current: %s",
				len(outdated.Unparsed), strings.Join(quoteAll(outdated.Unparsed), "; ")),
		})
	}

	catalogueUsable := inv.Catalogue.State == CatalogueFresh
	staleReason := ""
	if !catalogueUsable {
		switch inv.Catalogue.State {
		case CatalogueStale:
			if c.opts.MaxCatalogueAge < 0 {
				staleReason = fmt.Sprintf("this host's package catalogue was last refreshed %s ago, and this deployment is configured never to trust a catalogue's age; refreshing it means `pkg update`, which this page never runs",
					roundAge(inv.Catalogue.Age))
			} else {
				staleReason = fmt.Sprintf("this host's package catalogue was last refreshed %s ago (limit %s), and refreshing it means `pkg update`, which this page never runs",
					roundAge(inv.Catalogue.Age), roundAge(c.opts.MaxCatalogueAge))
			}
		default:
			staleReason = "this host's package catalogue freshness could not be established (" + inv.Catalogue.Detail + ")"
		}
	}

	inv.Ports = resolveStatuses(ports, outdated.Parsed, outdated.Err != "", len(outdated.Unparsed) > 0, catalogueUsable, staleReason)

	if !catalogueUsable {
		inv.Base.UpdateStatus = UpdateStatusUnknown
		inv.Base.Detail = staleReason
		// The catalogue gap is recorded once, as its own subject, rather
		// than repeated per package. The base's and each package's own
		// Detail field already name the same reason where it applies, so
		// this entry is the single place an operator looks to find out
		// that the whole inventory is gated on one unreadable fact.
		inv.Unknown = append(inv.Unknown, Unknown{Subject: "package catalogue", Reason: staleReason})
	}

	return inv, nil
}

// resolveStatuses is the pure core of Collect's verdict assignment,
// separated out so the whole update-verdict rule is testable without a
// Runner at all. See the Inventory/UpdateStatus docs for why every
// fallback here is unknown.
func resolveStatuses(ports []InstalledPackage, outdated map[string]string, outdatedFailed, outdatedUnreadable, catalogueFresh bool, staleReason string) []InstalledPackage {
	out := make([]InstalledPackage, 0, len(ports))
	for _, p := range ports {
		resolved := p
		if candidate, ok := outdated[p.Name]; ok {
			resolved.UpdateStatus = UpdateStatusOutdated
			resolved.Candidate = candidate
			resolved.Detail = "pkg reports " + p.Name + "-" + p.Version + " installed, " + candidate + " available"
			out = append(out, resolved)
			continue
		}
		switch {
		case outdatedFailed:
			resolved.UpdateStatus = UpdateStatusUnknown
			resolved.Detail = "`pkg outdated` could not be read, so this package's update status is unobserved"
		case outdatedUnreadable:
			resolved.UpdateStatus = UpdateStatusUnknown
			resolved.Detail = "`pkg outdated` produced output this build cannot read, so this package's update status is unobserved"
		case !catalogueFresh:
			resolved.UpdateStatus = UpdateStatusUnknown
			resolved.Detail = staleReason
		default:
			resolved.UpdateStatus = UpdateStatusCurrent
			resolved.Detail = ""
		}
		out = append(out, resolved)
	}
	return out
}

// observeCatalogue reads the local catalogue's mtime. It is pure
// observation: no refresh, ever.
func (c *Collector) observeCatalogue(now time.Time) Catalogue {
	cat := Catalogue{Path: c.opts.CataloguePath}
	modified, err := c.opts.stat.ModTime(c.opts.CataloguePath)
	if err != nil {
		cat.State = CatalogueUnknown
		cat.Detail = fmt.Sprintf("could not read %s (%v) - if pkg uses a non-default SQLITE_PATH, this host's catalogue age is unobservable and no update verdict is made",
			c.opts.CataloguePath, err)
		return cat
	}
	cat.ModifiedAt = modified
	cat.Age = now.Sub(modified)
	if cat.Age < 0 {
		// A catalogue newer than the clock reading means the two time
		// sources disagree. That is not evidence of freshness, so it
		// is treated as unknown rather than silently clamped to zero.
		cat.State = CatalogueUnknown
		cat.Detail = fmt.Sprintf("%s has a modification time later than this reading of the clock (%s); the two disagree, so catalogue freshness is unobservable",
			c.opts.CataloguePath, now.Format(time.RFC3339))
		return cat
	}
	if c.opts.MaxCatalogueAge < 0 {
		cat.State = CatalogueStale
		cat.Detail = fmt.Sprintf("last refreshed %s ago, and this deployment never treats a catalogue as fresh", roundAge(cat.Age))
		return cat
	}
	if cat.Age > c.opts.MaxCatalogueAge {
		cat.State = CatalogueStale
		cat.Detail = fmt.Sprintf("last refreshed %s ago, limit is %s", roundAge(cat.Age), roundAge(c.opts.MaxCatalogueAge))
		return cat
	}
	cat.State = CatalogueFresh
	cat.Detail = fmt.Sprintf("last refreshed %s ago", roundAge(cat.Age))
	return cat
}

// observeBase reads the running base system. Failure to read it is
// recorded as an unknown, not returned as an error, for the same reason
// Collect as a whole does not error on unreadable subsystems.
func (c *Collector) observeBase(ctx context.Context) BaseSystem {
	var base BaseSystem
	release, releaseErrOut, releaseErr := c.opts.Runner.Run(ctx, "uname", "-r")
	version, _, versionErr := c.opts.Runner.Run(ctx, "uname", "-v")
	if releaseErr != nil {
		base.UpdateStatus = UpdateStatusUnknown
		base.Detail = "could not read the running kernel release (`uname -r`): " + firstNonEmpty(releaseErrOut, releaseErr)
		return base
	}
	base.Observed = true
	base.OSRelease = strings.TrimSpace(release)
	if versionErr == nil {
		base.OSVersion = strings.TrimSpace(version)
	}

	stdout, stderr, err := c.opts.Runner.Run(ctx, "pkg", "version")
	if err != nil {
		base.UpdateStatus = UpdateStatusUnknown
		base.Detail = "`pkg version` could not be run: " + firstNonEmpty(stderr, err)
		return base
	}
	status, detail, recognized := parseBaseVersionAll(stdout)
	if !recognized {
		base.UpdateStatus = UpdateStatusUnknown
		base.Detail = "`pkg version` produced no output this build can read"
		return base
	}
	base.UpdateStatus = status
	base.Detail = detail
	return base
}

// observeInstalled runs the one pkg query this package depends on for
// package identity.
func (c *Collector) observeInstalled(ctx context.Context) ([]InstalledPackage, []string) {
	stdout, stderr, err := c.opts.Runner.Run(ctx, "pkg", "query", "-e", "%n|%v|%o", "%n-%v")
	if err != nil {
		return nil, []string{"`pkg query` could not be run: " + firstNonEmpty(stderr, err)}
	}
	return parseInstalledPackages(stdout)
}

// outdatedObservation is the result of one `pkg outdated` read. Err and
// Unparsed are kept separate on purpose: a command that could not be run
// and a command whose output this build cannot parse are different
// problems with different fixes, and an operator needs to be told which
// one they are looking at.
type outdatedObservation struct {
	Parsed   map[string]string
	Unparsed []string
	Err      string
	Raw      string
}

// observeOutdated runs pkg's own answer to "what is out of date?" and
// joins it against the already-collected installed list.
func (c *Collector) observeOutdated(ctx context.Context, installed []InstalledPackage) outdatedObservation {
	stdout, stderr, err := c.opts.Runner.Run(ctx, "pkg", "outdated")
	if err != nil {
		return outdatedObservation{Raw: stdout, Err: firstNonEmpty(stderr, err)}
	}
	parsed, unparsed := parseOutdated(stdout, installed)
	return outdatedObservation{Parsed: parsed, Unparsed: unparsed, Raw: stdout}
}

// quoteAll renders strings as a quoted, comma-separated list for use in
// a human-readable reason string.
func quoteAll(items []string) []string {
	out := make([]string, 0, len(items))
	for _, item := range items {
		out = append(out, fmt.Sprintf("%q", item))
	}
	return out
}

// roundAge renders a duration at a granularity an operator can read
// ("3d4h", "17m"), never as a raw nanosecond count.
func roundAge(d time.Duration) string {
	if d < 0 {
		return "-" + roundAge(-d)
	}
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh%dm", int(d.Hours()), int(d.Minutes())%60)
	default:
		return fmt.Sprintf("%dd%dh", int(d.Hours())/24, int(d.Hours())%24)
	}
}

// firstNonEmpty returns stderr when non-blank, else err's own message -
// the same error-surfacing convention every other internal/* package's
// runCmd helper follows.
func firstNonEmpty(stderr string, err error) string {
	if s := strings.TrimSpace(stderr); s != "" {
		return s
	}
	return err.Error()
}
