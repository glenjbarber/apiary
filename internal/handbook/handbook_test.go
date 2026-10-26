package handbook

// The handbook is a set of CLAIMS about the software: that a command
// exists, that a path is where a file lives, that a service is called
// what the handbook says.
//
// Those claims rot. A command gets renamed, a path moves, a service is
// dropped - and the handbook keeps saying the old thing, confidently,
// to an operator who has no way to check it except by trying.
//
// So the claims are checked. The test reads the handbook and asserts
// that what it says about the software is true of the software.
//
// It is a test-only package, because the property belongs to no
// daemon: it is a property of a document that sits next to the code.

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// handbookPath is where the handbook lives, relative to this file.
//
// It is derived from the test's own location rather than hard-coded,
// so the test passes on a machine that did not write it.
var handbookPath = filepath.Join("..", "..", "docs", "HANDBOOK.md")

// readHandbook reads the handbook.
func readHandbook(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(handbookPath)
	if err != nil {
		t.Fatalf("reading the handbook: %v", err)
	}
	return string(data)
}

// TestHandbookExists is the most basic claim: the handbook is there.
func TestHandbookExists(t *testing.T) {
	if _, err := os.Stat(handbookPath); err != nil {
		t.Fatalf("the handbook is not at %s: %v", handbookPath, err)
	}
}

// TestEveryDocumentTheHandbookLinksToExists checks the handbook's
// internal links.
//
// A handbook that points at a document that is not there sends the
// reader to a dead end at exactly the moment they needed the answer.
func TestEveryDocumentTheHandbookLinksToExists(t *testing.T) {
	body := readHandbook(t)
	// Markdown links of the form [text](path), excluding URLs and
	// anchors.
	re := regexp.MustCompile(`\]\(([^)]+)\)`)
	seen := map[string]bool{}
	for _, m := range re.FindAllStringSubmatch(body, -1) {
		link := m[1]
		if strings.HasPrefix(link, "http://") || strings.HasPrefix(link, "https://") || strings.HasPrefix(link, "#") {
			continue
		}
		// A link into the repo, relative to docs/.
		path := filepath.Join("..", "..", "docs", link)
		if seen[path] {
			continue
		}
		seen[path] = true
		if _, err := os.Stat(path); err != nil {
			t.Errorf("the handbook links to %q, which does not exist: %v", link, err)
		}
	}
}

// TestEveryApiaryctlCommandTheHandbookNamesExists checks the command
// reference against the CLI's own usage text.
//
// The handbook's command table is the part an operator is most likely
// to act on directly, and a command that was renamed or removed would
// leave the table claiming it is still there.
func TestEveryApiaryctlCommandTheHandbookNamesExists(t *testing.T) {
	body := readHandbook(t)
	usage := apiaryctlUsage(t)

	// Every command the handbook's TABLE lists must be one the CLI
	// knows.
	//
	// The table's cells are backticked, and a Go raw string cannot
	// contain a backtick, so the pattern is assembled from a string
	// rather than written as a literal.
	//
	// The table is checked rather than the whole document, because the
	// document quotes plenty of words in backticks that are not
	// commands. The table is where the handbook asserts "this command
	// exists", and that is the claim worth checking.
	//
	// The scan is limited to the command reference section, because
	// the handbook has OTHER tables whose first cells are backticked
	// words that are not commands - the health vocabulary, for one.
	backtick := "`"
	table := regexp.MustCompile("(?m)^\\| " + backtick + "(apiaryctl )?([a-z-]+(?: [a-z-]+)?)" + backtick + " \\|")
	matched := 0
	for _, m := range table.FindAllStringSubmatch(commandReference(body), -1) {
		matched++
		command := m[2]
		if !strings.Contains(usage, "apiaryctl "+command) {
			t.Errorf("the handbook's command table names %q, which the CLI's own usage text does not list", "apiaryctl "+command)
		}
	}
	// A table that matched NOTHING would make the check above
	// vacuous, so the number of rows is asserted too.
	if matched < 8 {
		t.Errorf("the handbook's command table matched only %d rows; the check above would be vacuous", matched)
	}
}

// TestEveryServiceTheHandbookNamesIsShipped checks the service names
// against the rc.d scripts.
//
// A service that was renamed would leave the handbook telling an
// operator to run a command that does not exist.
func TestEveryServiceTheHandbookNamesIsShipped(t *testing.T) {
	body := readHandbook(t)
	re := regexp.MustCompile("service (apiary_[a-z_]+)")
	seen := map[string]bool{}
	for _, m := range re.FindAllStringSubmatch(body, -1) {
		service := m[1]
		if seen[service] {
			continue
		}
		seen[service] = true
		path := filepath.Join("..", "..", "etc", "rc.d", service)
		if _, err := os.Stat(path); err != nil {
			t.Errorf("the handbook names the service %q, which has no rc.d script at %s: %v", service, path, err)
		}
	}
}

// TestEveryEndpointTheHandbookNamesIsServed checks the REST endpoints
// against the REST shim's routes.
//
// The shim's routes are the authority on what it serves, and the
// handbook's table is a claim about them.
func TestEveryEndpointTheHandbookNamesIsServed(t *testing.T) {
	body := readHandbook(t)
	re := regexp.MustCompile("`(GET|POST) (/v1/[a-z/-]+)`")
	seen := map[string]bool{}
	for _, m := range re.FindAllStringSubmatch(body, -1) {
		endpoint := m[2]
		if seen[endpoint] {
			continue
		}
		seen[endpoint] = true
		if !restshimServes(endpoint) {
			t.Errorf("the handbook names the endpoint %q, which the REST shim does not serve", endpoint)
		}
	}
}

// TestTheHandbookLinksToTheRunbooks checks that the handbook points at
// the runbooks that exist, because those are where the detail lives.
func TestTheHandbookLinksToTheRunbooks(t *testing.T) {
	body := readHandbook(t)
	for _, runbook := range []string{"create-node.md", "add-node-to-colony.md"} {
		if !strings.Contains(body, runbook) {
			t.Errorf("the handbook does not point at %s", runbook)
		}
	}
}

// commandReference returns the command reference section of the
// handbook.
//
// The scan is limited to it because the handbook has other tables
// whose first cells are backticked words that are not commands - the
// health vocabulary, for one, and a scan that read them would
// complain about a vocabulary rather than about a command.
func commandReference(body string) string {
	const heading = "## Command reference"
	start := strings.Index(body, heading)
	if start < 0 {
		return body
	}
	rest := body[start+len(heading):]
	if end := strings.Index(rest, "\n## "); end >= 0 {
		return rest[:end]
	}
	return rest
}
