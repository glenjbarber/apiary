package hostpkg

import (
	"reflect"
	"strings"
	"testing"
)

func TestParseInstalledPackages(t *testing.T) {
	// One line per package, three fields, in the order
	// `pkg query -e "%n|%v|%o" %n-%v` produces them. Origins contain a
	// slash, versions contain a patch suffix, and names contain hyphens -
	// all three of which would break a naive space-split.
	in := strings.Join([]string{
		"py39-setuptools|62.3.2| devel/py-setuptools",
		"FreeBSD|15.0-RELEASE-p4|FreeBSD",
		"bhyve-firmware|1.0_1|virtualization/bhyve-firmware",
		"bash|5.2.26|shells/bash",
	}, "\n")

	got, unparsed := parseInstalledPackages(in)
	if len(unparsed) != 0 {
		t.Fatalf("unexpected unparsed lines: %v", unparsed)
	}
	want := []InstalledPackage{
		{Name: "FreeBSD", Version: "15.0-RELEASE-p4", Origin: "FreeBSD"},
		{Name: "bash", Version: "5.2.26", Origin: "shells/bash"},
		{Name: "bhyve-firmware", Version: "1.0_1", Origin: "virtualization/bhyve-firmware"},
		{Name: "py39-setuptools", Version: "62.3.2", Origin: "devel/py-setuptools"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("parseInstalledPackages() = %+v, want %+v", got, want)
	}
}

// A line this build cannot read must come back as unparsed rather than
// being silently dropped or shifted into the wrong column. This is the
// property the whole package's honesty rests on.
func TestParseInstalledPackagesRefusesUnreadableLines(t *testing.T) {
	for name, line := range map[string]string{
		"two fields":     "bash|5.2.26",
		"four fields":    "bash|5.2.26|shells/bash|extra",
		"no separator":   "bash 5.2.26 shells/bash",
		"empty name":     "|5.2.26|shells/bash",
		"empty version":  "bash||shells/bash",
		"trailing prose": "pkg: could not find a package named bash",
	} {
		t.Run(name, func(t *testing.T) {
			got, unparsed := parseInstalledPackages(line)
			if len(got) != 0 {
				t.Errorf("parseInstalledPackages(%q) produced packages %+v, want none", line, got)
			}
			if len(unparsed) != 1 || unparsed[0] != line {
				t.Errorf("parseInstalledPackages(%q) unparsed = %v, want the line preserved verbatim", line, unparsed)
			}
		})
	}
}

func TestParseInstalledPackagesIgnoresBlankLines(t *testing.T) {
	got, unparsed := parseInstalledPackages("\n\nbash|5.2.26|shells/bash\n\n\n")
	if len(unparsed) != 0 {
		t.Fatalf("blank lines should be ignored, got unparsed %v", unparsed)
	}
	if len(got) != 1 || got[0].Name != "bash" {
		t.Errorf("got %+v, want one bash package", got)
	}
}

func TestParseBaseVersion(t *testing.T) {
	cases := map[string]UpdateStatus{
		"FreeBSD 15.0-RELEASE-p4 [repo FreeBSD]: up to date":              UpdateStatusCurrent,
		"FreeBSD 14.2-RELEASE: 14.2-RELEASE-p2 [repo FreeBSD] up to date": UpdateStatusCurrent,
		"FreeBSD 16.0-CURRENT: up to date":                                UpdateStatusCurrent,
		"FreeBSD 15.0-RELEASE-p4: has updates available":                  UpdateStatusOutdated,
		"FreeBSD 15.0-RELEASE-p4 [repo FreeBSD] has updates available":    UpdateStatusOutdated,
		// Anything this build does not recognise must be unknown, never
		// current. A future pkg that rewords this must degrade the
		// verdict, not silently improve it.
		"FreeBSD 15.0-RELEASE-p4 [repo FreeBSD]: checking...": UpdateStatusUnknown,
		"":      UpdateStatusUnknown,
		"??":    UpdateStatusUnknown,
		"stale": UpdateStatusUnknown,
	}
	for line, want := range cases {
		if got := parseBaseVersion(line); got != want {
			t.Errorf("parseBaseVersion(%q) = %q, want %q", line, got, want)
		}
	}
}

// "updates available" must be checked before "up to date" and must not
// be swallowed by it, in either order. These two phrasings do not
// overlap in practice, and the test pins that down rather than trusting
// it.
func TestParseBaseVersionAll(t *testing.T) {
	t.Run("every line current", func(t *testing.T) {
		status, detail, ok := parseBaseVersionAll("FreeBSD 15.0: up to date\nnginx: up to date\n")
		if !ok || status != UpdateStatusCurrent {
			t.Errorf("got (%q, %q, %t), want (current, detail, true)", status, detail, ok)
		}
		if detail == "" {
			t.Error("detail should carry the lines it classified")
		}
	})
	t.Run("one outdated wins", func(t *testing.T) {
		status, _, ok := parseBaseVersionAll("FreeBSD 15.0: up to date\nnginx: has updates available\n")
		if !ok || status != UpdateStatusOutdated {
			t.Errorf("got (%q, %t), want (outdated, true)", status, ok)
		}
	})
	t.Run("one unrecognized line makes the whole read unknown", func(t *testing.T) {
		status, detail, ok := parseBaseVersionAll("FreeBSD 15.0: up to date\nsomething new happened\n")
		if !ok || status != UpdateStatusUnknown {
			t.Errorf("got (%q, %t), want (unknown, true)", status, ok)
		}
		if detail != "something new happened" {
			t.Errorf("detail = %q, want the offending line named verbatim", detail)
		}
	})
	t.Run("no output at all", func(t *testing.T) {
		status, _, ok := parseBaseVersionAll("\n \n")
		if ok || status != UpdateStatusUnknown {
			t.Errorf("got (%q, %t), want (unknown, false)", status, ok)
		}
	})
}

func TestSplitSpecName(t *testing.T) {
	// Names may contain hyphens and versions may too, so the only safe
	// split is against a name already known from the inventory.
	if v, ok := splitSpecName("py39-setuptools", "py39-setuptools-62.3.2"); !ok || v != "62.3.2" {
		t.Errorf("splitSpecName = (%q, %t), want (62.3.2, true)", v, ok)
	}
	if v, ok := splitSpecName("FreeBSD", "FreeBSD-15.0-RELEASE-p4"); !ok || v != "15.0-RELEASE-p4" {
		t.Errorf("splitSpecName = (%q, %t), want (15.0-RELEASE-p4, true)", v, ok)
	}
	if _, ok := splitSpecName("bash", "zsh-5.9"); ok {
		t.Error("a spec for a different package must not be attributed to bash")
	}
	if _, ok := splitSpecName("bash", "bash"); ok {
		t.Error("a spec with no version separator must not be accepted")
	}
}

func TestParseOutdated(t *testing.T) {
	installed := []InstalledPackage{
		{Name: "bash", Version: "5.2.15"},
		{Name: "curl", Version: "8.9.1"},
		{Name: "nginx", Version: "1.26.0"},
	}
	in := strings.Join([]string{
		"bash                        <   bash-5.2.15",
		"                                >   bash-5.2.26",
		"curl                        <   curl-8.9.1",
		"                                >   curl-8.14.1",
	}, "\n")

	got, unparsed := parseOutdated(in, installed)
	if len(unparsed) != 0 {
		t.Fatalf("unexpected unparsed lines: %v", unparsed)
	}
	want := map[string]string{"bash": "5.2.26", "curl": "8.14.1"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("parseOutdated() = %v, want %v", got, want)
	}
}

// An alternative single-line layout is accepted, because pkg's column
// layout has changed between releases and this build should not be the
// thing that silently decides an update is unavailable.
func TestParseOutdatedSingleLineLayout(t *testing.T) {
	installed := []InstalledPackage{{Name: "bash", Version: "5.2.15"}}
	got, unparsed := parseOutdated("bash-5.2.15 < bash-5.2.26\n", installed)
	if len(unparsed) != 0 {
		t.Fatalf("unexpected unparsed lines: %v", unparsed)
	}
	if got["bash"] != "5.2.26" {
		t.Errorf("parseOutdated() = %v, want bash -> 5.2.26", got)
	}
}

func TestParseOutdatedRefusesUnreadableOutput(t *testing.T) {
	installed := []InstalledPackage{{Name: "bash", Version: "5.2.15"}}
	cases := map[string]string{
		"candidate for an uninstalled package": "zsh < zsh-5.9\n > zsh-5.10\n",
		"< line with no candidate":             "bash < bash-5.2.15\n",
		"> line with no installed line":        "> bash-5.2.26\n",
		"prose from a future pkg":              "bash is out of date, sorry\n",
		"spec that does not match the name":    "bash < bash\n > bash-5.2.26\n",
	}
	for name, in := range cases {
		t.Run(name, func(t *testing.T) {
			got, unparsed := parseOutdated(in, installed)
			if len(got) != 0 {
				t.Errorf("parseOutdated(%q) = %v, want no attributed updates", in, got)
			}
			if len(unparsed) == 0 {
				t.Errorf("parseOutdated(%q) reported nothing unparsed; unreadable output must be surfaced", in)
			}
		})
	}
}

func TestParseOutdatedEmptyOutputIsFullyUnderstood(t *testing.T) {
	installed := []InstalledPackage{{Name: "bash", Version: "5.2.26"}}
	got, unparsed := parseOutdated("", installed)
	if len(got) != 0 || len(unparsed) != 0 {
		t.Errorf("empty `pkg outdated` output = (%v, %v), want (empty, empty): no output is a fully understood answer of 'nothing is outdated'", got, unparsed)
	}
}

func TestValidPackageName(t *testing.T) {
	valid := []string{"bash", "py39-setuptools", "bhyve-firmware", "curl", "go123", "libx11", "a", "pkg.name", "gcc-14"}
	for _, name := range valid {
		if !validPackageName(name) {
			t.Errorf("validPackageName(%q) = false, want true", name)
		}
	}
	// The invalid set is the security-relevant one: anything that could
	// be read as a flag, a path, or a second command, plus the
	// traversal-ish cases.
	invalid := []string{"", "-rf", "--no-repo-update", "bash;rm -rf /", "bash&&whoami", "../etc/passwd", "/bin/sh", "a b", "a\tb", "a\nb", "a|b", ".", "..", "...", ".bash", "a:b", "a\\b", "a$(id)", "a`id`", "a>b"}
	for _, name := range invalid {
		if validPackageName(name) {
			t.Errorf("validPackageName(%q) = true, want false", name)
		}
	}
}
