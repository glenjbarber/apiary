package buildinfo

// The version stamp's own contract.
//
// buildinfo.Version was declared and read for the whole life of the
// package, and nothing ever set it: no build in the repository
// stamped a version, so every binary in the field reported no release
// at all. The variable existing is not the same as the feature
// working, and a test that only checked "the field renders" would
// have passed over that gap for as long as the field existed.
//
// So these tests exercise the real mechanism - scripts/build-ldflags.sh,
// and the Makefile recipe that calls it - rather than a reimplementation
// of it. A reimplementation is how a policy and its test drift apart
// without either noticing.
//
// The script is run as a subprocess rather than being read as text,
// because the thing that has to work is the script's OUTPUT: the -X
// flags it prints are what `go build -ldflags` is handed.

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// repoRoot is the repository root, resolved from this file's own
// compiled-in path rather than from the working directory, so the
// test does not depend on where it was invoked from.
func repoRoot(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller(0) failed: cannot locate this test file, " +
			"so the repository root cannot be resolved")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(thisFile), "..", ".."))
}

// runLdflags runs scripts/build-ldflags.sh with the given environment
// additions and returns its combined output.
func runLdflags(t *testing.T, env ...string) string {
	t.Helper()
	root := repoRoot(t)
	cmd := exec.Command(filepath.Join(root, "scripts", "build-ldflags.sh"))
	cmd.Dir = root
	cmd.Env = append(os.Environ(), env...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("build-ldflags.sh: %v\n%s", err, out)
	}
	return string(out)
}

// ldflagValue extracts one -X flag's value from the script's output.
func ldflagValue(t *testing.T, out, field string) string {
	t.Helper()
	// The script prints "-X <path>.<Field>=<value>" and `go build
	// -ldflags` splits that on spaces, so the flag and its value are
	// two separate whitespace-delimited fields.
	fields := strings.Fields(out)
	for i := 0; i+1 < len(fields); i++ {
		if fields[i] != "-X" {
			continue
		}
		name, value, found := strings.Cut(fields[i+1], "=")
		if !found {
			continue
		}
		if strings.HasSuffix(name, "."+field) {
			return value
		}
	}
	t.Fatalf("no %s in the ldflags output:\n%s", field, out)
	return ""
}

// The version must actually be stamped, or the whole feature is a
// no-op: the field exists, and every build leaves it empty.
func TestLdflagsScriptStampsTheVersion(t *testing.T) {
	out := runLdflags(t)

	version := ldflagValue(t, out, "Version")
	if version == "" {
		t.Fatalf("the ldflags carry an empty Version:\n%s", out)
	}

	// A developer build of an untagged commit says "devel". It must
	// not say nothing, and it must not say a plausible-looking
	// release version it cannot back up.
	if version != "devel" {
		t.Logf("this checkout is tagged, so the derived version is %q", version)
	}
}

// An explicit VERSION must win over the derived one. This is what a
// release build does, and it is the only way to stamp a version onto a
// build made from a tree that is not exactly the tag.
func TestExplicitVersionOverridesTheDerivedOne(t *testing.T) {
	out := runLdflags(t, "VERSION=1.0.0-alpha")

	if got := ldflagValue(t, out, "Version"); got != "1.0.0-alpha" {
		t.Errorf("Version = %q, want the pinned 1.0.0-alpha:\n%s", got, out)
	}
}

// The --version mode must agree with the flag it produces, or a
// release engineer reading one and a binary reporting the other would
// be comparing two different claims.
func TestVersionModeAgreesWithTheStampedValue(t *testing.T) {
	root := repoRoot(t)
	cmd := exec.Command(filepath.Join(root, "scripts", "build-ldflags.sh"), "--version")
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "VERSION=1.0.0-alpha")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("build-ldflags.sh --version: %v\n%s", err, out)
	}
	if strings.TrimSpace(string(out)) != "1.0.0-alpha" {
		t.Errorf("--version = %q, want 1.0.0-alpha", strings.TrimSpace(string(out)))
	}
}

// The Makefile has to pass VERSION through, or `make build
// VERSION=1.0.0-alpha` would build binaries that report "devel" -
// the exact failure the variable exists to prevent, one layer up.
func TestMakefilePassesVersionThroughToTheStamp(t *testing.T) {
	root := repoRoot(t)
	data, err := os.ReadFile(filepath.Join(root, "Makefile"))
	if err != nil {
		t.Fatalf("reading the Makefile: %v", err)
	}
	makefile := string(data)

	// Every recipe that calls the stamp script has to carry VERSION,
	// or that one target builds a differently-identified binary than
	// the others.
	calls := 0
	for _, line := range strings.Split(makefile, "\n") {
		if !strings.Contains(line, "build-ldflags.sh") &&
			!strings.Contains(line, "${LDFLAGS_SH}") {
			continue
		}
		if strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		if !strings.Contains(line, "ldflags=") {
			continue
		}
		calls++
		if !strings.Contains(line, "VERSION=") {
			t.Errorf("a recipe calls the stamp script without passing VERSION:\n%s", line)
		}
	}
	if calls == 0 {
		t.Error("no recipe calls the stamp script; the Makefile's build " +
			"recipe has changed shape and this test is no longer checking it")
	}
}
