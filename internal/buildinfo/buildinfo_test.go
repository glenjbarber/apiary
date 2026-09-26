package buildinfo

import (
	"flag"
	"io"
	"regexp"
	"runtime/debug"
	"strings"
	"testing"
)

// buildIDToken extracts the build id as a reader (or versioncheck) would
// see it: the first whitespace-delimited token after build=.
var buildIDToken = regexp.MustCompile(`build=(\S+)`)

func TestUninjectedIsHonestAboutEmptyStamps(t *testing.T) {
	// The package-level vars are empty in a plain `go test` run, which
	// is exactly the no-ldflags case the package documents. A test
	// binary is not stamped, so this is the real default path rather
	// than a contrived one.
	if !Uninjected() {
		t.Fatal("Uninjected() = false for an unstamped test binary; the " +
			"no-stamp path is not being exercised")
	}

	got := String()
	if !strings.Contains(got, "build=unknown") {
		t.Errorf("String() = %q, want it to say build=unknown when unstamped", got)
	}
	// The critical property: an unstamped build must never render an
	// empty or plausible-looking build id that a reader could mistake
	// for a real value.
	if strings.Contains(got, "build= ") || strings.Contains(got, "build=\n") {
		t.Errorf("String() = %q, want no empty build= field", got)
	}
}

func TestStringRendersStampedBuild(t *testing.T) {
	origID, origTime, origCommit, origVersion := BuildID, BuildTime, GitCommit, Version
	t.Cleanup(func() {
		BuildID, BuildTime, GitCommit, Version = origID, origTime, origCommit, origVersion
	})

	BuildID = "abc123def456"
	BuildTime = "2026-09-26T12:00:00Z"
	GitCommit = "0123456789abcdef0123456789abcdef01234567"
	Version = "v1.2.3"

	if Uninjected() {
		t.Fatal("Uninjected() = true for a fully stamped build")
	}

	got := String()
	for _, want := range []string{
		"build=abc123def456",
		"version=v1.2.3",
		"commit=0123456789ab", // shortened, not the full 40 chars
		"built=2026-09-26T12:00:00Z",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("String() = %q, want it to contain %q", got, want)
		}
	}
	if strings.Contains(got, "0123456789abcdef0123456789abcdef01234567") {
		t.Errorf("String() = %q, want the commit shortened, not printed in full", got)
	}
}

func TestStringAlwaysNamesThePlatform(t *testing.T) {
	// go= must be present either way: a cross-compiled binary's target
	// platform is the single most useful fact when a FreeBSD-only
	// failure is being debugged.
	if !strings.Contains(String(), "go=") {
		t.Errorf("String() = %q, want it to name the platform", String())
	}
}

func withStamps(t *testing.T, id, buildTime, commit, version string) {
	origID, origTime, origCommit, origVersion := BuildID, BuildTime, GitCommit, Version
	t.Cleanup(func() {
		BuildID, BuildTime, GitCommit, Version = origID, origTime, origCommit, origVersion
	})
	BuildID, BuildTime, GitCommit, Version = id, buildTime, commit, version
}

func TestPartialStampNeverLooksComplete(t *testing.T) {
	// The failure this guards: an id injected but not the time - a
	// hand-rolled ldflags line, or a Makefile that dropped a field -
	// used to render as an omitted "built=", which reads the same as a
	// complete stamp to anyone skimming a startup log. Each field now
	// reports on its own.
	withStamps(t, "abc123def456", "", "", "")
	got := String()

	for _, want := range []string{"build=abc123def456", "built=unknown", "commit=unknown"} {
		if !strings.Contains(got, want) {
			t.Errorf("String() = %q, want it to contain %q", got, want)
		}
	}
	for _, blank := range []string{"built= ", "commit= ", "built=)", "commit=)"} {
		if strings.Contains(got, blank) {
			t.Errorf("String() = %q, want no empty %q field", got, blank)
		}
	}
}

func TestUnstampedNamesEveryFieldUnknown(t *testing.T) {
	// Unstamped is the case that must be loud rather than tidy: all
	// three identity fields say unknown, so a reader is told the whole
	// truth at once instead of inferring it from a missing id.
	withStamps(t, "", "", "", "")
	got := String()
	for _, want := range []string{"build=unknown", "commit=unknown", "built=unknown"} {
		if !strings.Contains(got, want) {
			t.Errorf("String() = %q, want it to contain %q", got, want)
		}
	}
}

func TestDirtyIsDerivedFromTheID(t *testing.T) {
	// Dirty is a property of the id, not a separately injected flag, so
	// the two can never disagree about the same binary.
	cases := map[string]bool{
		"abc123def456":             false,
		"abc123def456-dirty":       true,
		"release-1.0":              false,
		"":                         false,
		"dirty-but-not-marked":     false, // the token alone is not the marker
		"abc-dirty-but-not":        false, // only a TRAILING suffix marks it
		"abc123def456-dirty-dirty": true,
	}
	for id, want := range cases {
		withStamps(t, id, "2026-09-26T12:00:00Z", "abc123def4560123456789abcdef0123456789abcd", "")
		if got := Dirty(); got != want {
			t.Errorf("Dirty() with BuildID=%q = %v, want %v", id, got, want)
		}
	}
}

func TestReportWarnsWhenDirty(t *testing.T) {
	// Agreement between two -dirty ids is weaker than agreement between
	// two clean ones, and the report is where a human finds out.
	withStamps(t, "abc123def456-dirty", "2026-09-26T12:00:00Z", "abc123def4560123456789abcdef0123456789abcd", "")
	out := Report("raftd")
	if !strings.Contains(out, "DIRTY") {
		t.Errorf("Report() must warn that a dirty build's id does not describe its bytes:\n%s", out)
	}
	if !strings.Contains(out, "sha256") {
		t.Errorf("Report() should say how to compare artifacts instead:\n%s", out)
	}

	withStamps(t, "abc123def456", "2026-09-26T12:00:00Z", "abc123def4560123456789abcdef0123456789abcd", "")
	if clean := Report("raftd"); strings.Contains(clean, "DIRTY") {
		t.Errorf("Report() warned DIRTY for a clean build:\n%s", clean)
	}
}

func TestCommitStyleIDRendersWithoutAClock(t *testing.T) {
	// The shape the Makefile now produces. Pinned explicitly so a
	// future change to the id format shows up here rather than only in
	// a diff of a log line nobody reads.
	withStamps(t, "b8e74c328d9f", "2026-09-26T11:52:03-04:00", "b8e74c328d9f0123456789abcdef0123456789ab", "")
	got := String()
	for _, want := range []string{
		"build=b8e74c328d9f",
		"commit=b8e74c328d9f",
		"built=2026-09-26T11:52:03-04:00",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("String() = %q, want it to contain %q", got, want)
		}
	}
	// The property that makes the id comparable across machines: the
	// build= token is exactly the id and nothing is appended to it, so
	// nothing in it can vary with when the build ran. Asserted on the
	// parsed token rather than with Contains, because Contains on a
	// prefix would pass even if a timestamp had been re-appended.
	m := buildIDToken.FindStringSubmatch(got)
	if m == nil {
		t.Fatalf("String() = %q, want a build= field", got)
	}
	if m[1] != "b8e74c328d9f" {
		t.Errorf("build id token = %q, want exactly %q (a clock or counter in the id is the bug)", m[1], "b8e74c328d9f")
	}
}

func TestShortCommitEdgeCases(t *testing.T) {
	cases := map[string]string{
		"":                 "",
		"abc":              "abc",
		"0123456789ab":     "0123456789ab", // exactly 12, not truncated
		"0123456789abc":    "0123456789ab", // 13, truncated to 12
		"0123456789abcdef": "0123456789ab",
	}
	for in, want := range cases {
		if got := short(in); got != want {
			t.Errorf("short(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestReportIsSelfConsistent(t *testing.T) {
	origID := BuildID
	t.Cleanup(func() { BuildID = origID })
	BuildID = "deadbeef0000"

	out := Report("managerd")
	for _, want := range []string{"managerd", "build=deadbeef0000", "program:", "go:", "module:", "vcs:"} {
		if !strings.Contains(out, want) {
			t.Errorf("Report() missing %q:\n%s", want, out)
		}
	}
	// An empty module version must read as absent, not as a blank
	// field someone could mistake for a value.
	if !strings.Contains(out, "(none recorded)") {
		t.Errorf("Report() should mark unrecorded values explicitly:\n%s", out)
	}
}

func TestModuleInfoDoesNotPanicOnTestBinary(t *testing.T) {
	// Go's build info is present for a normal build; these must be
	// safe either way rather than assuming availability.
	_ = ModuleVersion()
	_ = VCS()
	_ = Toolchain()
	if Toolchain() == "" {
		t.Error("Toolchain() = \"\", want a Go version")
	}
}

func TestVCSMatchesReadBuildInfo(t *testing.T) {
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		t.Skip("no build info in this test binary")
	}
	var want string
	for _, s := range bi.Settings {
		if s.Key == "vcs.revision" {
			want = s.Value
		}
	}
	if got := VCS(); got != want {
		t.Errorf("VCS() = %q, want %q (must agree with debug.ReadBuildInfo)", got, want)
	}
}

func TestRegisterMustPrecedeParse(t *testing.T) {
	// The real bug this guards: registering -version AFTER
	// flag.Parse() makes the flag package reject it as undefined and
	// print usage, so `-version` silently does nothing. That is
	// exactly the failure the daemons hit until they were reordered,
	// and it is invisible in a test that only calls Report().
	origShow := showVersion
	t.Cleanup(func() { showVersion = origShow })

	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	RegisterVersionFlag(fs)

	if err := fs.Parse([]string{"-version"}); err != nil {
		t.Fatalf("Parse(-version) = %v; the flag was not registered before parsing", err)
	}
	if !VersionRequested() {
		t.Error("VersionRequested() = false after parsing -version")
	}
}

func TestVersionRequestedFalseWhenAbsent(t *testing.T) {
	origShow := showVersion
	t.Cleanup(func() { showVersion = origShow })

	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	RegisterVersionFlag(fs)
	if err := fs.Parse(nil); err != nil {
		t.Fatalf("Parse(nil) = %v", err)
	}
	if VersionRequested() {
		t.Error("VersionRequested() = true with no arguments")
	}
}
