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

// TestGitignore_CoversApiaryctl closes a gap the merge that introduced
// apiaryctl left open. `make build` writes its binaries straight into
// the repo root, where they share a name with their cmd/<name>
// directory, so the four pre-existing ones are listed in .gitignore by
// path. apiaryctl was not, and the failure is silent and permanent: the
// binary sits there untracked, a `git add -A` in the root commits a
// 30MB executable, and nothing anywhere reports it. Found by running
// scripts/check-version.sh, which builds everything, and then reading
// git status.
//
// Listed by path like the others rather than by a pattern, because the
// general patterns above do not catch it and the reason they do not is
// the reason the whole list exists: these files have no extension.
func TestGitignore_CoversApiaryctl(t *testing.T) {
	body, err := os.ReadFile(filepath.Join(repoRoot(t), ".gitignore"))
	if err != nil {
		t.Fatalf("reading .gitignore: %v", err)
	}
	if !regexp.MustCompile(`(?m)^/apiaryctl\s*$`).Match(body) {
		t.Errorf(".gitignore does not list /apiaryctl.\n" +
			"`make build` writes it into the repo root, and an untracked binary " +
			"there is picked up by `git add -A` and committed with no warning " +
			"anywhere. The other four installed binaries are listed for the same " +
			"reason; apiaryctl was missed when it was added.")
	}
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

// targetRecipe returns the recipe lines of the named make target - the
// tab-prefixed lines that follow its `name:` header, up to the next
// header, comment or blank line.
//
// Scoping matters here in both directions, and getting it wrong is what
// the earlier version of this file got wrong twice. A check over the
// whole file cannot distinguish a command from a comment about the
// command, because a comment is text in the file exactly as a recipe
// line is. A check over a recipe slice cannot see a target that was
// added elsewhere. So the negatives are whole-file and line-shaped, and
// the positives are scoped to the one target that is allowed to
// mention the command at all.
func targetRecipe(t *testing.T, makefile, target string) []string {
	t.Helper()
	lines := strings.Split(makefile, "\n")
	var recipe []string
	inRecipe := false
	for _, line := range lines {
		if !inRecipe {
			if line == target+":" || strings.HasPrefix(line, target+":") && !strings.HasPrefix(line, "\t") {
				inRecipe = true
			}
			continue
		}
		if !strings.HasPrefix(line, "\t") {
			break
		}
		recipe = append(recipe, line)
	}
	if recipe == nil {
		t.Fatalf("no %s target in the Makefile", target)
	}
	return recipe
}

// TestMakefile_HasNoForceRestartTarget is the regression for deleting
// the make target, and it is the strong form of the rule the move off
// the Makefile was made for.
//
// The wrapper it replaces was wrong not because it ran the wrong thing
// but because it existed. A target named `force-restart` on a machine
// with no Makefile is a name an operator can read in make output, in a
// build log, or out of muscle memory, and cannot type where they are
// standing. Within one merge of it being introduced it was quoted as a
// fallback in the SHARED.md, in managerd's own advice, and on the
// Colony update page, each time as the thing to reach for when the
// installed command was not yet deployed. A shim is a thing to quote;
// its absence is not. So the target and FORCE_RESTART_SRCS are deleted,
// and this test is what stops either coming back.
func TestMakefile_HasNoForceRestartTarget(t *testing.T) {
	makefile := readMakefile(t)

	// Whole-file, line-anchored negatives. Anchoring to the start of a
	// line matters: an unanchored `force-restart:` is also matched by a
	// sentence of prose that happens to end a clause with it, and a
	// test that fails on a comment is a test whose next fix is to weaken
	// the comment.
	for _, forbidden := range []string{
		"FORCE_RESTART_SRCS",
		"^force-restart:",
		`^\.PHONY:.*force-restart`,
		"^FORCE_RESTART",
	} {
		// These are literal-ish patterns written out above, so a
		// compile failure would be a bug in this list rather than in
		// the Makefile, and MustCompile is the right response to it.
		if m := regexp.MustCompile("(?m)" + forbidden).FindString(makefile); m != "" {
			t.Errorf("the Makefile still has %q at the start of a line.\n"+
				"force-restart is an operation on a running Comb, so it is the installed "+
				"apiaryctl at /usr/local/libexec/apiary/apiaryctl. A make target runs only "+
				"where there is a checkout, and a Comb has none, so the target is a name "+
				"that reads like an option and cannot be typed where it is needed. See "+
				"ADR-0136 and ADR-0141.", m)
		}
	}
}

// TestUpdate_RestartsOnlyTheNonVotingDaemons is the other half, and it
// is the half that caught the mutation this file's first version of
// this test let through. Deleting the force-restart target is only
// safe while nothing else in the Makefile can reach the command, and
// `update` is the one place that restarts services automatically: a
// line added to its recipe that runs the command would put managerd
// and raftd back in a sweep run on every Comb, which is the entire
// failure ADR-0141 exists to make unavailable.
func TestUpdate_RestartsOnlyTheNonVotingDaemons(t *testing.T) {
	makefile := readMakefile(t)
	recipe := targetRecipe(t, makefile, "update")

	// The list the restart loop iterates is the load-bearing half of
	// `update`, and it is a variable rather than a line of the recipe,
	// so checking the recipe alone cannot see a daemon added to it.
	// ADR-0141's Verification section pins this with `bmake -n update`;
	// this is the same pin as an assertion that runs in CI.
	got := makeVar(t, makefile, "UPDATE_RESTART_SRCS")
	want := []string{"frontend", "restshimd"}
	if len(got) != len(want) {
		t.Errorf("UPDATE_RESTART_SRCS = %v, want exactly %v. update runs on every Comb in a "+
			"deploy sweep; a daemon in this list is a daemon whose restart is no longer a "+
			"deliberate per-Comb act (ADR-0141).", got, want)
	}
	for _, w := range want {
		if !contains(got, w) {
			t.Errorf("UPDATE_RESTART_SRCS = %v, want it to include %s", got, w)
		}
	}

	named := false
	for _, line := range recipe {
		trimmed := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "@"))
		if trimmed == "" {
			continue
		}
		// Echoing the command is the point of the closing message: the
		// operator has just installed binaries without restarting the
		// two daemons that hold cluster state, and has to be told what
		// to type instead. So `echo` is allowed and only echo.
		if strings.HasPrefix(trimmed, "echo ") {
			if strings.Contains(trimmed, "apiaryctl force-restart") {
				named = true
			}
			continue
		}
		if strings.Contains(trimmed, "force-restart") {
			t.Errorf("the update target runs %q.\n"+
				"update is the target a deploy runs across every Comb without thinking, so "+
				"nothing in it may restart a quorum voter. Restarting managerd and raftd is "+
				"a deliberate per-Comb act, and the whole point of this command being a "+
				"separate installed binary is that an operator names it themselves.\n"+
				"--- update recipe ---\n%s", trimmed, strings.Join(recipe, "\n"))
		}
	}
	if !named {
		t.Error("the update target's closing message no longer names apiaryctl force-restart.\n" +
			"With the make target gone, that message is the only place the Makefile tells an " +
			"operator how to restart managerd and raftd, and it is printed at exactly the " +
			"moment they have just deployed and found those two stale.")
	}
}
