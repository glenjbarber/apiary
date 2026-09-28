package hostinstall

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// makefileText reads the root Makefile through this file's own compiled-in
// path rather than through the working directory. `go test ./...` sets the
// working directory to the package's own directory, but a bare `go test` of a
// package list, an editor's runner or a hand-invoked test binary may not, and
// a test that silently found no Makefile would pass without testing anything.
func makefileText(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller(0) failed: cannot locate this test file, so the Makefile cannot be resolved")
	}
	root := filepath.Clean(filepath.Join(filepath.Dir(thisFile), "..", ".."))
	body, err := os.ReadFile(filepath.Join(root, "Makefile"))
	if err != nil {
		t.Fatalf("reading the Makefile: %v", err)
	}
	return string(body)
}

// target returns the lines of one Makefile target, including the target
// header itself, so an assertion about a recipe cannot be satisfied by the
// same text appearing in a comment somewhere else in the file.
func target(t *testing.T, makefile, name string) string {
	t.Helper()
	lines := strings.Split(makefile, "\n")
	for i, l := range lines {
		if strings.HasPrefix(l, name+":") || l == name {
			for j := i + 1; j < len(lines); j++ {
				if lines[j] != "" && !strings.HasPrefix(lines[j], "\t") && !strings.HasPrefix(lines[j], "#") {
					return strings.Join(lines[i:j], "\n")
				}
			}
			return strings.Join(lines[i:], "\n")
		}
	}
	t.Fatalf("no %q target in the Makefile", name)
	return ""
}

// setup-quick delegates the configuration to the installed binary. The
// Makefile is a source-checkout convenience; the Combs themselves have
// neither it nor a compiler, so the thing it delegates to is a thing that
// exists on a Comb.
func TestSetupQuickDelegatesConfigurationToTheInstalledBinary(t *testing.T) {
	mk := makefileText(t)
	recipe := target(t, mk, "setup-quick")

	if !strings.Contains(recipe, "/usr/local/libexec/apiary/apiaryctl install --apply") {
		t.Errorf("setup-quick does not call the installed apiaryctl with --apply:\n%s", recipe)
	}
	// It has to run AFTER `make install`, or the binary it is calling is
	// not on disk yet.
	installAt := strings.Index(recipe, "${MAKE} install")
	callAt := strings.Index(recipe, "apiaryctl install --apply")
	if installAt < 0 || callAt < 0 || callAt < installAt {
		t.Errorf("setup-quick calls apiaryctl before `make install` puts it on disk:\n%s", recipe)
	}
}

// The seven printf statements are gone. A recipe that writes a config file
// with a shell redirect is a second writer for the same file, and two
// writers disagree - one of them overwrites, which is the thing Part 1
// exists to stop.
func TestSetupQuickNoLongerWritesConfigFilesWithPrintf(t *testing.T) {
	mk := makefileText(t)
	recipe := target(t, mk, "setup-quick")

	for _, forbidden := range []string{
		"> /usr/local/etc/apiary/",
		"printf '{\\n",
		"chmod 600 /usr/local/etc/apiary",
	} {
		if strings.Contains(recipe, forbidden) {
			t.Errorf("setup-quick still contains %q; the configuration is written by apiaryctl now, and two writers for one file is the drift this replaced:\n%s", forbidden, recipe)
		}
	}
}

// Every variable an operator could set on the old recipe is still honoured.
// Removing the printfs must not quietly remove the ability to point a fresh
// host at its own layout.
func TestSetupQuickStillPassesEveryHostVariableThrough(t *testing.T) {
	mk := makefileText(t)
	recipe := target(t, mk, "setup-quick")

	for _, v := range []string{
		"NODE_RPC_ADDR", "NODE_HTTP_ADDR", "NODE_REST_ADDR", "NODE_TLS_DIR",
		"NODE_ZFS_POOL", "NODE_VLAN_UPLINK", "NODE_BHYVE_BRIDGE",
	} {
		if !strings.Contains(recipe, "${"+v+"}") && !strings.Contains(recipe, "${"+v+" ") {
			t.Errorf("setup-quick no longer passes %s through to apiaryctl install", v)
		}
	}
	// The bhyve firmware lookup stays, because it is a package question
	// that apiaryctl deliberately does not answer.
	if !strings.Contains(recipe, "BHYVE_UEFI.fd") {
		t.Error("setup-quick lost the bhyve UEFI firmware lookup")
	}
	if !strings.Contains(recipe, "could not locate a bhyve UEFI firmware") {
		t.Error("setup-quick lost the refusal when the firmware file cannot be found")
	}
}

// `setup` no longer issues the certificate, because `apiaryctl install`
// does, in Go, and two issuers for one path is one more thing to keep
// consistent. The target itself stays, for an operator who wants
// openssl's output instead.
func TestSetupNoLongerIssuesTheCertificateButSetupTLSSurvives(t *testing.T) {
	mk := makefileText(t)

	recipe := target(t, mk, "setup")
	if strings.Contains(recipe, "setup-tls") {
		t.Errorf("`setup` still depends on setup-tls, so the certificate has two issuers and whichever ran first wins:\n%s", recipe)
	}
	if !strings.Contains(recipe, "setup-pam") {
		t.Errorf("`setup` lost the PAM policy step, which nothing else provides:\n%s", recipe)
	}
	if !strings.Contains(recipe, "setup-dirs") {
		t.Errorf("`setup` lost the directory step:\n%s", recipe)
	}

	tls := target(t, mk, "setup-tls")
	if !strings.Contains(tls, "openssl req") {
		t.Errorf("the setup-tls target no longer generates anything, so it should be deleted rather than kept as a stub:\n%s", tls)
	}
	if !strings.Contains(tls, "only written if absent") {
		t.Errorf("setup-tls lost its own only-if-absent contract, which is what makes it safe beside an installer that also preserves:\n%s", tls)
	}
}

// The Makefile's own guidance about the wildcard and the LAN address is
// still there, because the reason `apiaryctl install` checks rpc_addr is
// that history, and an operator who reads only the Makefile should still
// learn it.
func TestTheRPCAddrGuidanceSurvives(t *testing.T) {
	mk := makefileText(t)
	if !strings.Contains(mk, "ADR-0139") {
		t.Error("the Makefile no longer mentions ADR-0139 anywhere")
	}
	if !strings.Contains(mk, "0.0.0.0:17700") {
		t.Error("the Makefile no longer records that the wildcard was the default that caused the incident")
	}
}
