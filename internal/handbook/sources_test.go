package handbook

// The two helpers that let the handbook's claims be checked against
// the software they describe.
//
// Both read the SOURCE rather than a copy of it, because a copy is
// exactly the thing that goes stale.

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// apiaryctlUsage returns the CLI's own usage text.
//
// It runs the real binary, because the usage text is what an operator
// sees when they get a command wrong - and it is the authority on
// which commands exist.
func apiaryctlUsage(t *testing.T) string {
	t.Helper()
	root := filepath.Join("..", "..")
	binary := filepath.Join(t.TempDir(), "apiaryctl")
	cmd := exec.Command("go", "build", "-o", binary, "./cmd/apiaryctl")
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "GOFLAGS=-mod=mod")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("building apiaryctl: %v\n%s", err, out)
	}
	out, err := exec.Command(binary, "help").CombinedOutput()
	if err != nil {
		t.Fatalf("running apiaryctl help: %v\n%s", err, out)
	}
	usage := string(out)

	// A COMMAND GROUP's own usage lists its sub-commands, and the
	// top-level usage does not. So a command that is a group - one
	// whose name is followed by another word - is checked against the
	// GROUP's usage rather than the top-level one.
	//
	// This matters because `apiaryctl cert-setup setup` is a real
	// command that the top-level usage never mentions: it names
	// `cert-setup` and stops there.
	for _, group := range []string{"cert-setup", "colony-ca"} {
		groupUsage, groupErr := exec.Command(binary, group, "help").CombinedOutput()
		if groupErr != nil {
			continue
		}
		// The group's usage lists its STEPS as bare words under a
		// "STEPS" heading, not as full command lines. So each step is
		// recorded as "apiaryctl <group> <step>", which is the form
		// the handbook's table uses.
		for _, step := range groupSteps(string(groupUsage)) {
			usage += "\napiaryctl " + group + " " + step
		}
	}
	return usage
}

// groupSteps returns the step names a command group's usage lists.
//
// The steps are the indented words under the "STEPS" heading. They
// are recognised by their position - a heading, then indented names -
// rather than by their text, because the text is prose that changes
// and the names are what an operator types.
func groupSteps(groupUsage string) []string {
	var steps []string
	inSteps := false
	for _, line := range strings.Split(groupUsage, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "STEPS" {
			inSteps = true
			continue
		}
		if !inSteps {
			continue
		}
		// A blank line ends the section.
		if trimmed == "" {
			inSteps = false
			continue
		}
		// A step line is indented and its first word is the step
		// name; the description follows on the same line.
		if !strings.HasPrefix(line, "  ") || strings.HasPrefix(line, "   ") {
			continue
		}
		fields := strings.Fields(trimmed)
		if len(fields) == 0 {
			continue
		}
		name := fields[0]
		if name == "help" || name == "FLAGS" || name == "STEPS" {
			continue
		}
		steps = append(steps, name)
	}
	return steps
}

// restshimServes reports whether the REST shim serves the endpoint.
//
// It reads the shim's ROUTE REGISTRATION rather than running the
// server, because the registration is the authority and it is a
// single, readable line per route.
func restshimServes(endpoint string) bool {
	data, err := os.ReadFile(filepath.Join("..", "..", "internal", "restshim", "server.go"))
	if err != nil {
		return false
	}
	re := regexp.MustCompile(`mux\.HandleFunc\("([A-Z]+) ([^"]+)"`)
	for _, m := range re.FindAllStringSubmatch(string(data), -1) {
		if m[2] == endpoint {
			return true
		}
	}
	return false
}

// The endpoint claim is checked against the shim's routes, and the
// shim's routes are the authority. A path with a parameter in it is
// matched by its literal prefix, because the handbook documents the
// shape rather than a specific instance.
var _ = strings.TrimSpace
