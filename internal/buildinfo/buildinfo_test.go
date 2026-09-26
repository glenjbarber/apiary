package buildinfo

import (
	"flag"
	"io"
	"runtime/debug"
	"strings"
	"testing"
)

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
