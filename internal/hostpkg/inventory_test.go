package hostpkg

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestCollectStandardHost(t *testing.T) {
	c := freshCollector(t, standardHost())
	inv, err := c.Collect(context.Background())
	if err != nil {
		t.Fatalf("Collect() error: %v", err)
	}

	if !inv.Base.Observed || inv.Base.OSRelease != "15.0-RELEASE-p4" {
		t.Errorf("base = %+v, want an observed 15.0-RELEASE-p4", inv.Base)
	}
	if inv.Base.UpdateStatus != UpdateStatusCurrent {
		t.Errorf("base UpdateStatus = %q, want %q", inv.Base.UpdateStatus, UpdateStatusCurrent)
	}
	if inv.Catalogue.State != CatalogueFresh {
		t.Errorf("catalogue state = %q, want %q", inv.Catalogue.State, CatalogueFresh)
	}
	if len(inv.Unknown) != 0 {
		t.Errorf("a fully readable host reported unknowns: %+v", inv.Unknown)
	}

	// Three packages, sorted by name, with the two outdated ones
	// carrying their candidate versions and the third positively
	// current.
	if len(inv.Ports) != 3 {
		t.Fatalf("got %d packages, want 3: %+v", len(inv.Ports), inv.Ports)
	}
	if inv.Ports[0].Name != "bash" || inv.Ports[2].Name != "nginx" {
		t.Errorf("packages are not sorted by name: %+v", inv.Ports)
	}
	bash, _ := findPackage(inv, "bash")
	if bash.UpdateStatus != UpdateStatusOutdated || bash.Candidate != "5.2.26" {
		t.Errorf("bash = %+v, want outdated with candidate 5.2.26", bash)
	}
	curl, _ := findPackage(inv, "curl")
	if curl.UpdateStatus != UpdateStatusOutdated || curl.Candidate != "8.14.1" {
		t.Errorf("curl = %+v, want outdated with candidate 8.14.1", curl)
	}
	nginx, _ := findPackage(inv, "nginx")
	if nginx.UpdateStatus != UpdateStatusCurrent || nginx.Candidate != "" {
		t.Errorf("nginx = %+v, want current with no candidate", nginx)
	}

	if got := inv.OutdatedCount(); got != 2 {
		t.Errorf("OutdatedCount() = %d, want 2", got)
	}
	// The headline is updates_available, not unknown: nothing was
	// unestablished on this host.
	if got := inv.Headline(); got != HeadlineUpdatesAvailable {
		t.Errorf("Headline() = %q, want %q", got, HeadlineUpdatesAvailable)
	}
}

// A catalogue that has not been refreshed in a month is the single most
// dangerous case in this feature: pkg will still answer "up to date",
// and believing it is how a security update gets missed. This test is
// the reason the package exists.
func TestCollectStaleCatalogueIsNeverUpToDate(t *testing.T) {
	runner := newFakeRunner().
		on("uname -r", "15.0-RELEASE-p4\n").
		on("uname -v", "15.0-RELEASE-p4\n").
		on("pkg version", "FreeBSD 15.0-RELEASE-p4 [repo FreeBSD]: up to date\n").
		on("pkg query -e %n|%v|%o %n-%v", "bash|5.2.26|shells/bash\n").
		// pkg says everything is current. That answer is worthless
		// without a catalogue, and must not be repeated as one.
		on("pkg outdated", "")

	c := NewCollector(Options{
		Runner:          runner,
		Privilege:       fakePrivilege{root: false, detail: "effective uid is 501"},
		Now:             func() time.Time { return baseTime },
		stat:            fixedStat{mod: baseTime.Add(-90 * 24 * time.Hour)},
		MaxCatalogueAge: DefaultMaxCatalogueAge,
	})
	inv, err := c.Collect(context.Background())
	if err != nil {
		t.Fatalf("Collect() error: %v", err)
	}

	if inv.Catalogue.State != CatalogueStale {
		t.Fatalf("catalogue state = %q, want %q", inv.Catalogue.State, CatalogueStale)
	}
	bash, ok := findPackage(inv, "bash")
	if !ok {
		t.Fatal("bash missing from inventory")
	}
	if bash.UpdateStatus != UpdateStatusUnknown {
		t.Errorf("bash UpdateStatus = %q, want %q: pkg's own answer is meaningless on a stale catalogue", bash.UpdateStatus, UpdateStatusUnknown)
	}
	if bash.Detail == "" || !strings.Contains(bash.Detail, "pkg update") {
		t.Errorf("bash Detail = %q, want it to name `pkg update` as the only way to refresh the catalogue", bash.Detail)
	}
	if inv.Base.UpdateStatus != UpdateStatusUnknown {
		t.Errorf("base UpdateStatus = %q, want %q", inv.Base.UpdateStatus, UpdateStatusUnknown)
	}
	if got := inv.Headline(); got != HeadlineUnknown {
		t.Errorf("Headline() = %q, want %q - a stale catalogue must never read as up to date", got, HeadlineUnknown)
	}
	assertUnknown(t, inv, "package catalogue")
}

// An unreadable catalogue - a non-default SQLITE_PATH, a permissions
// problem - has the same consequence as a stale one, and must also never
// produce a current verdict.
func TestCollectUnreadableCatalogueIsUnknown(t *testing.T) {
	runner := standardHost()
	c := NewCollector(Options{
		Runner:    runner,
		Privilege: fakePrivilege{root: false, detail: "euid 501"},
		Now:       func() time.Time { return baseTime },
		stat:      fixedStat{err: errNoSuchFile},
	})
	inv, err := c.Collect(context.Background())
	if err != nil {
		t.Fatalf("Collect() error: %v", err)
	}
	if inv.Catalogue.State != CatalogueUnknown {
		t.Fatalf("catalogue state = %q, want %q", inv.Catalogue.State, CatalogueUnknown)
	}
	if inv.Headline() != HeadlineUnknown {
		t.Errorf("Headline() = %q, want %q", inv.Headline(), HeadlineUnknown)
	}
	// An outdated package is still positively reported as outdated even
	// on an unreadable catalogue: that evidence does not depend on the
	// catalogue being fresh, because a newer version is known to exist.
	bash, _ := findPackage(inv, "bash")
	if bash.UpdateStatus != UpdateStatusOutdated {
		t.Errorf("bash UpdateStatus = %q, want %q: a known-available update is evidence regardless of catalogue age",
			bash.UpdateStatus, UpdateStatusOutdated)
	}
	// The package with no reported update is unknown, not current.
	nginx, _ := findPackage(inv, "nginx")
	if nginx.UpdateStatus != UpdateStatusUnknown {
		t.Errorf("nginx UpdateStatus = %q, want %q", nginx.UpdateStatus, UpdateStatusUnknown)
	}
	// And the headline is unknown, not updates_available, even though two
	// updates ARE available: the conservative verdict outranks the
	// positive one so a partial picture can never read as a full one.
	if got := inv.Headline(); got != HeadlineUnknown {
		t.Errorf("Headline() = %q, want %q", got, HeadlineUnknown)
	}
}

// A catalogue whose mtime is in the future means the clock and the file
// disagree. That is not evidence of freshness.
func TestCollectFutureCatalogueIsUnknown(t *testing.T) {
	c := NewCollector(Options{
		Runner:    standardHost(),
		Privilege: fakePrivilege{root: false, detail: "euid 501"},
		Now:       func() time.Time { return baseTime },
		stat:      fixedStat{mod: baseTime.Add(time.Hour)},
	})
	inv, err := c.Collect(context.Background())
	if err != nil {
		t.Fatalf("Collect() error: %v", err)
	}
	if inv.Catalogue.State != CatalogueUnknown {
		t.Errorf("catalogue state = %q, want %q", inv.Catalogue.State, CatalogueUnknown)
	}
	if inv.Headline() != HeadlineUnknown {
		t.Errorf("Headline() = %q, want %q", inv.Headline(), HeadlineUnknown)
	}
}

// A negative MaxCatalogueAge is a supported mode, not a degenerate one:
// it means "never trust a catalogue", and it must produce a total,
// explainable unknown rather than a crash or a silent current.
func TestCollectNeverTrustCatalogueMode(t *testing.T) {
	c := NewCollector(Options{
		Runner:          standardHost(),
		Privilege:       fakePrivilege{root: false, detail: "euid 501"},
		Now:             func() time.Time { return baseTime },
		stat:            fixedStat{mod: baseTime},
		MaxCatalogueAge: -1,
	})
	inv, err := c.Collect(context.Background())
	if err != nil {
		t.Fatalf("Collect() error: %v", err)
	}
	if inv.Catalogue.State != CatalogueStale {
		t.Errorf("catalogue state = %q, want %q", inv.Catalogue.State, CatalogueStale)
	}
	nginx, _ := findPackage(inv, "nginx")
	if nginx.UpdateStatus != UpdateStatusUnknown {
		t.Errorf("nginx UpdateStatus = %q, want %q", nginx.UpdateStatus, UpdateStatusUnknown)
	}
}

// A host with no pkg at all - a developer's macOS machine, or a FreeBSD
// install where pkg is genuinely missing - must produce a complete,
// explained unknown. Not an error, and emphatically not "up to date".
func TestCollectOnHostWithoutPkg(t *testing.T) {
	runner := newFakeRunner().
		on("uname -r", "24.5.0\n").
		on("uname -v", "Darwin Kernel Version 24.5.0\n").
		onErr("pkg version", "sh: pkg: command not found", errNotFound).
		onErr("pkg query -e %n|%v|%o %n-%v", "sh: pkg: command not found", errNotFound).
		onErr("pkg outdated", "sh: pkg: command not found", errNotFound)

	c := NewCollector(Options{
		Runner:    runner,
		Privilege: fakePrivilege{root: false, detail: "euid 501"},
		Now:       func() time.Time { return baseTime },
		stat:      fixedStat{err: errNoSuchFile},
	})
	inv, err := c.Collect(context.Background())
	if err != nil {
		t.Fatalf("Collect() on a host with no pkg must not error, got: %v", err)
	}
	if !inv.Base.Observed || inv.Base.OSRelease != "24.5.0" {
		t.Errorf("base = %+v, want the running kernel still observed", inv.Base)
	}
	if inv.Base.UpdateStatus != UpdateStatusUnknown {
		t.Errorf("base UpdateStatus = %q, want %q", inv.Base.UpdateStatus, UpdateStatusUnknown)
	}
	if inv.Base.PkgBasePackage {
		t.Error("PkgBasePackage should be false with no pkg at all")
	}
	if len(inv.Ports) != 0 {
		t.Errorf("Ports = %+v, want empty", inv.Ports)
	}
	if len(inv.Unknown) == 0 {
		t.Fatal("a host with no pkg must record at least one unknown")
	}
	if got := inv.Headline(); got != HeadlineUnknown {
		t.Errorf("Headline() = %q, want %q", got, HeadlineUnknown)
	}
	// Every unknown must carry the command's own message, so an operator
	// can tell "no pkg installed" from "pkg is broken".
	found := false
	for _, u := range inv.Unknown {
		if strings.Contains(u.Reason, "command not found") {
			found = true
		}
	}
	if !found {
		t.Errorf("no unknown carried the command's own error text: %+v", inv.Unknown)
	}
}

// Unreadable `pkg outdated` output means no package can be called
// current. This is the parser-drift case, and it must fail closed.
func TestCollectUnreadableOutdatedOutputFailsClosed(t *testing.T) {
	runner := newFakeRunner().
		on("uname -r", "15.0-RELEASE-p4\n").
		on("uname -v", "15.0-RELEASE-p4\n").
		on("pkg version", "FreeBSD 15.0-RELEASE-p4 [repo FreeBSD]: up to date\n").
		on("pkg query -e %n|%v|%o %n-%v", "bash|5.2.26|shells/bash\nnginx|1.26.0|www/nginx\n").
		// Output from a pkg this build has never seen.
		on("pkg outdated", "bash is out of date, would like to tell you about it\n")

	c := NewCollector(Options{
		Runner:    runner,
		Privilege: fakePrivilege{root: false, detail: "euid 501"},
		Now:       func() time.Time { return baseTime },
		stat:      fixedStat{mod: baseTime.Add(-time.Hour)},
	})
	inv, err := c.Collect(context.Background())
	if err != nil {
		t.Fatalf("Collect() error: %v", err)
	}
	for _, name := range []string{"bash", "nginx"} {
		p, ok := findPackage(inv, name)
		if !ok {
			t.Fatalf("%s missing", name)
		}
		if p.UpdateStatus != UpdateStatusUnknown {
			t.Errorf("%s UpdateStatus = %q, want %q when pkg outdated's output is unreadable", name, p.UpdateStatus, UpdateStatusUnknown)
		}
	}
	if inv.Headline() != HeadlineUnknown {
		t.Errorf("Headline() = %q, want %q", inv.Headline(), HeadlineUnknown)
	}
	assertUnknown(t, inv, "update availability")
}

// An unreadable `pkg query` means the installed list itself is in
// doubt, and that is reported as its own subject.
func TestCollectUnreadableQueryOutput(t *testing.T) {
	runner := newFakeRunner().
		on("uname -r", "15.0-RELEASE-p4\n").
		on("uname -v", "15.0-RELEASE-p4\n").
		on("pkg version", "FreeBSD 15.0-RELEASE-p4 [repo FreeBSD]: up to date\n").
		on("pkg query -e %n|%v|%o %n-%v", "bash|5.2.26\n").
		on("pkg outdated", "")

	c := NewCollector(Options{
		Runner:    runner,
		Privilege: fakePrivilege{root: false, detail: "euid 501"},
		Now:       func() time.Time { return baseTime },
		stat:      fixedStat{mod: baseTime.Add(-time.Hour)},
	})
	inv, err := c.Collect(context.Background())
	if err != nil {
		t.Fatalf("Collect() error: %v", err)
	}
	if len(inv.Ports) != 0 {
		t.Errorf("Ports = %+v, want empty when the installed list is unreadable", inv.Ports)
	}
	assertUnknown(t, inv, "installed package list")
}

// A pkgbase host's base system is also an installed package. It is
// reported once, under the base heading, never twice.
func TestCollectPkgBaseIsNotListedTwice(t *testing.T) {
	runner := newFakeRunner().
		on("uname -r", "15.0-RELEASE-p4\n").
		on("uname -v", "15.0-RELEASE-p4\n").
		on("pkg version", "FreeBSD 15.0-RELEASE-p4 [repo FreeBSD]: up to date\n").
		on("pkg query -e %n|%v|%o %n-%v", strings.Join([]string{
			"FreeBSD|15.0-RELEASE-p4|FreeBSD",
			"bash|5.2.26|shells/bash",
		}, "\n")+"\n").
		on("pkg outdated", "")

	c := freshCollector(t, runner)
	inv, err := c.Collect(context.Background())
	if err != nil {
		t.Fatalf("Collect() error: %v", err)
	}
	if !inv.Base.PkgBasePackage {
		t.Error("PkgBasePackage = false, want true on a pkgbase host")
	}
	if _, dup := findPackage(inv, BasePackageName); dup {
		t.Errorf("the base package must not also appear in the Ports list: %+v", inv.Ports)
	}
	if len(inv.Ports) != 1 || inv.Ports[0].Name != "bash" {
		t.Errorf("Ports = %+v, want just bash", inv.Ports)
	}
}

// The whole read half of this feature must be free of `pkg update`.
// This is asserted against the recorded command set, not trusted.
func TestCollectNeverRunsPkgUpdate(t *testing.T) {
	runner := standardHost()
	if _, err := freshCollector(t, runner).Collect(context.Background()); err != nil {
		t.Fatalf("Collect() error: %v", err)
	}
	for _, cmd := range runner.commandNames() {
		if strings.HasPrefix(cmd, "pkg update") {
			t.Errorf("Collect ran %q; this package must never refresh a catalogue", cmd)
		}
		if strings.HasPrefix(cmd, "pkg upgrade") {
			t.Errorf("Collect ran %q; this package must never upgrade anything during a read", cmd)
		}
	}
}

// The fingerprint must be stable across re-reads of an unchanged host
// and must change when anything a plan depends on changes. It is what
// stops an operator applying a preview of a state that has moved.
func TestInventoryFingerprint(t *testing.T) {
	c := freshCollector(t, standardHost())
	first, err := c.Collect(context.Background())
	if err != nil {
		t.Fatalf("Collect() error: %v", err)
	}
	second, err := c.Collect(context.Background())
	if err != nil {
		t.Fatalf("Collect() error: %v", err)
	}
	if first.Fingerprint() != second.Fingerprint() {
		t.Errorf("fingerprint changed between two identical reads: %s then %s", first.Fingerprint(), second.Fingerprint())
	}

	// A new candidate version for one package must move it.
	advanced := standardHost().on("pkg outdated", "bash                        <   bash-5.2.26\n > bash-5.2.37\n")
	third, err := freshCollector(t, advanced).Collect(context.Background())
	if err != nil {
		t.Fatalf("Collect() error: %v", err)
	}
	if third.Fingerprint() == first.Fingerprint() {
		t.Error("fingerprint did not change when a candidate version advanced")
	}
}

func TestResolveStatusesMatrix(t *testing.T) {
	ports := []InstalledPackage{
		{Name: "outdated", Version: "1"},
		{Name: "current", Version: "1"},
		{Name: "other", Version: "1"},
	}
	outdated := map[string]string{"outdated": "2", "other": "3"}

	cases := map[string]struct {
		outdatedFailed bool
		unreadable     bool
		catalogueFresh bool
		want           map[string]UpdateStatus
		wantCandidates map[string]string
	}{
		"everything readable": {
			catalogueFresh: true,
			want:           map[string]UpdateStatus{"outdated": UpdateStatusOutdated, "current": UpdateStatusCurrent, "other": UpdateStatusOutdated},
			wantCandidates: map[string]string{"outdated": "2", "other": "3"},
		},
		"pkg outdated failed": {
			outdatedFailed: true,
			want:           map[string]UpdateStatus{"outdated": UpdateStatusOutdated, "current": UpdateStatusUnknown, "other": UpdateStatusOutdated},
			wantCandidates: map[string]string{"outdated": "2", "other": "3"},
		},
		"pkg outdated unreadable": {
			unreadable:     true,
			want:           map[string]UpdateStatus{"outdated": UpdateStatusOutdated, "current": UpdateStatusUnknown, "other": UpdateStatusOutdated},
			wantCandidates: map[string]string{"outdated": "2", "other": "3"},
		},
		"stale catalogue": {
			want:           map[string]UpdateStatus{"outdated": UpdateStatusOutdated, "current": UpdateStatusUnknown, "other": UpdateStatusOutdated},
			wantCandidates: map[string]string{"outdated": "2", "other": "3"},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got := resolveStatuses(ports, outdated, tc.outdatedFailed, tc.unreadable, tc.catalogueFresh, "the catalogue is too old to trust")
			for _, p := range got {
				if want := tc.want[p.Name]; p.UpdateStatus != want {
					t.Errorf("%s UpdateStatus = %q, want %q", p.Name, p.UpdateStatus, want)
				}
				if p.Candidate != tc.wantCandidates[p.Name] {
					t.Errorf("%s Candidate = %q, want %q", p.Name, p.Candidate, tc.wantCandidates[p.Name])
				}
				// Every non-current package must explain itself.
				if p.UpdateStatus != UpdateStatusCurrent && p.Detail == "" {
					t.Errorf("%s is %q with no detail; a non-current verdict must always be explainable", p.Name, p.UpdateStatus)
				}
			}
		})
	}
}

func TestRoundAge(t *testing.T) {
	cases := map[time.Duration]string{
		30 * time.Second:             "30s",
		17 * time.Minute:             "17m",
		3*time.Hour + 4*time.Minute:  "3h4m",
		24 * time.Hour:               "1d0h",
		3*24*time.Hour + 4*time.Hour: "3d4h",
		-30 * time.Second:            "-30s",
	}
	for d, want := range cases {
		if got := roundAge(d); got != want {
			t.Errorf("roundAge(%s) = %q, want %q", d, got, want)
		}
	}
}
