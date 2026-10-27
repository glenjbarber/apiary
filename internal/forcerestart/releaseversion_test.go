package forcerestart_test

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestTheLdflagsScriptStampsAReleaseVersion covers the release
// version's path from the environment to the link flag.
//
// `internal/buildinfo` has had a `Version` field, and every binary's
// -version output has printed it, since before there was a release to
// name - but nothing ever set it. A release build that cannot report
// which release it is is a release nobody can identify in a bug
// report, so the stamping has to actually work.
func TestTheLdflagsScriptStampsAReleaseVersion(t *testing.T) {
	script := filepath.Join(repoRoot(t), "scripts/build-ldflags.sh")

	// A build with a version stamps it.
	out := runScript(t, script, "1.0.0-alpha")
	if !strings.Contains(out, ".Version=1.0.0-alpha") {
		t.Errorf("the ldflags for a release build do not carry the version: %s", out)
	}

	// An ordinary build does not. A version that said "not a release"
	// by being absent is more honest than one that named the branch.
	out = runScript(t, script, "")
	if strings.Contains(out, ".Version=") {
		t.Errorf("an ordinary build stamps a version, so a binary cannot be told apart from a release: %s", out)
	}
}

// TestTheLdflagsScriptRefusesAVersionWithSpacesOrQuotes: the version
// ends up in a startup log line and a -version output that an
// operator reads and quotes in a bug report, so a value that would
// make either ambiguous is refused rather than passed through.
func TestTheLdflagsScriptRefusesAVersionWithSpacesOrQuotes(t *testing.T) {
	script := filepath.Join(repoRoot(t), "scripts/build-ldflags.sh")
	for _, bad := range []string{"1.0.0 alpha", `1.0.0"alpha`} {
		cmd := exec.Command("sh", script)
		cmd.Env = append(cmd.Environ(), "BUILD_VERSION="+bad)
		out, err := cmd.CombinedOutput()
		if err == nil {
			t.Errorf("BUILD_VERSION=%q was accepted; it must be refused", bad)
			continue
		}
		if !strings.Contains(string(out), "BUILD_VERSION") {
			t.Errorf("the refusal for BUILD_VERSION=%q does not name the variable: %s", bad, out)
		}
	}
}

// TestTheMakefilePassesTheVersionThrough: the script understands
// BUILD_VERSION, but a release build only gets it if the Makefile
// forwards it.
func TestTheMakefilePassesTheVersionThrough(t *testing.T) {
	makefile := readMakefile(t)
	if !strings.Contains(makefile, "BUILD_VERSION='$(BUILD_VERSION)'") {
		t.Error("the build recipe does not pass BUILD_VERSION to the ldflags script, so a release build would never be stamped")
	}
	if !strings.Contains(makefile, "BUILD_VERSION?=") {
		t.Error("the Makefile does not declare BUILD_VERSION, so `make build BUILD_VERSION=x` would be ignored")
	}
}

// runScript runs the ldflags script with a BUILD_VERSION and returns
// its output.
func runScript(t *testing.T, script, version string) string {
	t.Helper()
	cmd := exec.Command("sh", script)
	cmd.Env = append(cmd.Environ(), "BUILD_VERSION="+version)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("running %s: %v\n%s", script, err, out)
	}
	return string(out)
}
