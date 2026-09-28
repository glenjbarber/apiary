package forcerestart_test

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The behaviour in this package is correct, and it is still worth nothing
// on a Comb that does not have the binary. `apiaryctl force-restart` is an
// operation performed *because* the Comb is running, which is the whole
// reason it was moved out of the Makefile: a Comb has no checkout, so
// anything reached through `make` cannot be the operational path there.
// That argument has one load-bearing link - the `install` target has to
// put apiaryctl on the node - and removing it from that list is a
// one-line edit that breaks every Comb with no test noticing and no
// error anywhere.
//
// So the link is tested. These cases read the Makefile rather than
// running `make install`, because `make install` writes to
// /usr/local/libexec and would need root and a Comb, and because the
// failure being guarded against is precisely a disagreement between the
// source tree and what the install rule copies.

func repoRoot(t *testing.T) string {
	t.Helper()
	// This file is internal/forcerestart/install_test.go, so the module
	// root is two directories up.
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	return filepath.Dir(filepath.Dir(wd))
}

func readMakefile(t *testing.T) string {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(repoRoot(t), "Makefile"))
	if err != nil {
		t.Fatalf("reading the Makefile: %v", err)
	}
	return string(body)
}

// makeVar returns the value of a make variable written in the
// line-continuation form this Makefile uses for its lists:
//
//	NAME=	first \
//		second
func makeVar(t *testing.T, makefile, name string) []string {
	t.Helper()
	lines := strings.Split(makefile, "\n")
	for i, line := range lines {
		if !strings.HasPrefix(line, name+"=") {
			continue
		}
		values := splitList(strings.TrimSuffix(strings.TrimSpace(strings.TrimPrefix(line, name+"=")), "\\"))
		for _, cont := range lines[i+1:] {
			if !strings.HasPrefix(cont, "\t\t") {
				break
			}
			values = append(values, splitList(strings.TrimSuffix(strings.TrimSpace(cont), "\\"))...)
		}
		return values
	}
	t.Fatalf("no %s= in the Makefile", name)
	return nil
}

func splitList(s string) []string {
	var out []string
	for _, f := range strings.Fields(s) {
		out = append(out, f)
	}
	return out
}

func contains(list []string, want string) bool {
	for _, got := range list {
		if got == want {
			return true
		}
	}
	return false
}

func TestMakefile_BuildsApiaryctl(t *testing.T) {
	// Not installed without being built: `install` copies binaries out
	// of the working directory, and a name in INSTALL_SRCS that is not
	// in SRCS is a `cp: apiaryctl: No such file` partway through
	// installing the daemons.
	srcs := makeVar(t, readMakefile(t), "SRCS")
	if !contains(srcs, "apiaryctl") {
		t.Errorf("SRCS = %v, want it to include apiaryctl: `make build` would not produce it", srcs)
	}
}

func TestMakefile_InstallsApiaryctl(t *testing.T) {
	install := makeVar(t, readMakefile(t), "INSTALL_SRCS")
	if !contains(install, "apiaryctl") {
		t.Errorf("INSTALL_SRCS = %v, want it to include apiaryctl.\n"+
			"Without it `make install` leaves no apiaryctl on the node, and the one command "+
			"an operator can run on a Comb with no source checkout stops existing there - "+
			"with no error anywhere.", install)
	}

	// And the install rule has to actually loop over the whole list. A
	// variable that is defined correctly and then not the one the recipe
	// iterates is the same defect wearing a disguise, and it is the
	// shape the sample-config loop already took when apiaryctl arrived
	// with no .json.sample of its own.
	makefile := readMakefile(t)
	if !strings.Contains(makefile, "for S in ${INSTALL_SRCS} ; \\") {
		t.Error("the binary install rule no longer iterates ${INSTALL_SRCS}; " +
			"apiaryctl would be listed and never copied")
	}
}

// TestMakefile_SampleLoopSkipsApiaryctl pins the other half of the same
// change. apiaryctl has no configuration file, so it has no
// etc/apiary/apiaryctl.json.sample, and the sample-copying loop cannot
// iterate INSTALL_SRCS any more or `make install` fails partway through
// with a missing-file error - after having already installed some of the
// daemons.
func TestMakefile_SampleLoopSkipsApiaryctl(t *testing.T) {
	makefile := readMakefile(t)
	samples := makeVar(t, makefile, "INSTALL_SAMPLE_SRCS")

	if contains(samples, "apiaryctl") {
		t.Errorf("INSTALL_SAMPLE_SRCS = %v, want it to exclude apiaryctl: it has no config file", samples)
	}
	if !strings.Contains(makefile, "for S in ${INSTALL_SAMPLE_SRCS} ; \\") {
		t.Error("the sample install rule does not iterate ${INSTALL_SAMPLE_SRCS}")
	}
	for _, daemon := range []string{"raftd", "managerd", "frontend", "restshimd"} {
		if !contains(samples, daemon) {
			t.Errorf("INSTALL_SAMPLE_SRCS = %v, want it to include %s: dropping a daemon from it "+
				"silently stops shipping that daemon's documented sample", samples, daemon)
		}
	}
}

// TestMakefile_ForceRestartIsOnlyAWrapper pins the shape the move was
// made for. The Makefile target must exec the installed command and hold
// no restart logic of its own, because the moment it holds logic again
// there are two implementations of a command that restarts a quorum
// voter, and the one an operator actually types is whichever they can
// reach.
//
// The check is on the recipe body only. The comment block above the
// target discusses what the command does and why, and that discussion is
// worth keeping; what must not come back is a `service` invocation, a
// `sockstat` pipeline, or a call to a script in this tree.
func TestMakefile_ForceRestartIsOnlyAWrapper(t *testing.T) {
	makefile := readMakefile(t)
	// Start past the target line itself: the slice has to begin on the
	// first recipe line, or the target name itself reads as a
	// non-tab-prefixed line and ends the recipe before it starts.
	header := "\nforce-restart:\n"
	start := strings.Index(makefile, header)
	if start < 0 {
		t.Fatal("no force-restart target in the Makefile")
	}
	recipe := makefile[start+len(header):]
	// The recipe ends at the next target definition, a blank line
	// followed by a non-tab line.
	if end := regexp.MustCompile(`(?m)^[^\t\n#]`).FindStringIndex(recipe); end != nil {
		recipe = recipe[:end[0]]
	}

	for _, forbidden := range []string{
		"service apiary_",
		"service apiary_$",
		"service apiary_$$",
		"sockstat",
		"scripts/",
		"record-forced-restart",
	} {
		if strings.Contains(recipe, forbidden) {
			t.Errorf("the force-restart recipe contains %q.\n"+
				"It must be a thin wrapper around the installed command. Any restart logic here "+
				"is a second implementation of a quorum-voter restart, free to drift from the one "+
				"an operator can reach on a Comb.\n--- recipe ---\n%s", forbidden, recipe)
		}
	}
	if !strings.Contains(recipe, "apiaryctl force-restart") {
		t.Errorf("the force-restart recipe does not run apiaryctl force-restart:\n%s", recipe)
	}
}

// TestApiaryctlIsSourceFree is the direct statement of the requirement
// this whole change exists for: the operational command must not need
// anything from a checkout. The package under test is what it runs, so
// this is checked on its imports.
//
// A dependency on internal/raft or api/internalpb would also break the
// import-boundary commitment ADR-0136 makes about apiaryctl, so one
// check covers two rules.
func TestApiaryctlIsSourceFree(t *testing.T) {
	forbidden := []string{
		// ADR-0136's import boundary: apiaryctl is not a path to Raft
		// writes, and it does not talk to raftd at all.
		"apiary/api/internalpb",
		"apiary/internal/raft",
		// Nothing from this tree is available on a Comb, so a dependency
		// on a file rather than on a Go package would be invisible to
		// the compiler and fatal at run time.
		"scripts/",
		"Makefile",
	}
	body, err := os.ReadFile(filepath.Join(repoRoot(t), "internal", "forcerestart", "forcerestart.go"))
	if err != nil {
		t.Fatalf("reading forcerestart.go: %v", err)
	}
	// Only the import block matters; a mention in a comment is not a
	// dependency.
	imports := string(body)
	if i := strings.Index(imports, ")"); i > 0 {
		imports = imports[:i]
	}
	for _, bad := range forbidden {
		if strings.Contains(imports, bad) {
			t.Errorf("internal/forcerestart imports something containing %q.\n"+
				"The command has to work on a Comb with no source on it, and ADR-0136's "+
				"import boundary keeps it away from Raft entirely.\n--- imports ---\n%s", bad, imports)
		}
	}
}
