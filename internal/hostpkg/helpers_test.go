package hostpkg

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeRunner is a scripted Runner. Every call is recorded so a test can
// assert on the exact command set that was executed - which is how
// "this package never runs pkg update" is enforced by a test rather
// than by a promise in a comment.
type fakeRunner struct {
	mu    sync.Mutex
	calls []call
	// responses maps a command key to its scripted result. The key is
	// the command name plus its args, joined by a space.
	responses map[string]response
	// fallback is returned for any command with no scripted response,
	// simulating a host where the tool is simply absent.
	fallback response
}

type call struct {
	Name string
	Args []string
}

type response struct {
	stdout string
	stderr string
	err    error
}

func newFakeRunner() *fakeRunner {
	return &fakeRunner{responses: map[string]response{}}
}

func (f *fakeRunner) on(key string, stdout string) *fakeRunner {
	f.responses[key] = response{stdout: stdout}
	return f
}

// script installs a whole response set at construction time.
func (f *fakeRunner) script(responses map[string]response) *fakeRunner {
	f.responses = responses
	return f
}

func (f *fakeRunner) onErr(key, stderr string, err error) *fakeRunner {
	f.responses[key] = response{stderr: stderr, err: err}
	return f
}

// replace swaps the whole scripted response set, modelling a host whose
// state has genuinely changed since the last read. It takes the lock, so
// a Collector being read concurrently in a race test is safe.
func (f *fakeRunner) replace(responses map[string]response) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.responses = responses
}

func (f *fakeRunner) Run(_ context.Context, name string, args ...string) (string, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	key := strings.TrimSpace(name + " " + strings.Join(args, " "))
	f.calls = append(f.calls, call{Name: name, Args: args})
	if resp, ok := f.responses[key]; ok {
		return resp.stdout, resp.stderr, resp.err
	}
	return f.fallback.stdout, f.fallback.stderr, f.fallback.err
}

func (f *fakeRunner) commandNames() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	names := make([]string, 0, len(f.calls))
	for _, c := range f.calls {
		names = append(names, strings.TrimSpace(c.Name+" "+strings.Join(c.Args, " ")))
	}
	return names
}

func (f *fakeRunner) ranCommand(substr string) bool {
	for _, name := range f.commandNames() {
		if strings.Contains(name, substr) {
			return true
		}
	}
	return false
}

// fixedStat is a statter with a scripted mtime.
type fixedStat struct {
	mod time.Time
	err error
}

func (s fixedStat) ModTime(string) (time.Time, error) { return s.mod, s.err }

// fakeApplier is a scripted Applier standing in for the privileged
// boundary, so the entire apply path - including the post-condition read
// that decides the outcome - is exercised without root.
type fakeApplier struct {
	mu       sync.Mutex
	upgrades [][]string
	result   UpgradeResult
	err      error
}

func (a *fakeApplier) Upgrade(_ context.Context, packages []string) (UpgradeResult, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.upgrades = append(a.upgrades, append([]string(nil), packages...))
	res := a.result
	res.Command = append([]string{"pkg", "upgrade"}, packages...)
	return res, a.err
}

func (a *fakeApplier) calls() [][]string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([][]string(nil), a.upgrades...)
}

type fakePrivilege struct {
	root   bool
	detail string
}

func (p fakePrivilege) Describe() (bool, string) { return p.root, p.detail }

// baseTime is a fixed instant so every age in these tests is exact.
var baseTime = time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)

// Sentinel errors the fake runner returns, standing in for the real
// ones a host without pkg would produce.
var (
	errNotFound   = errors.New("exec: \"pkg\": executable file not found in $PATH")
	errNoSuchFile = errors.New("stat /var/db/pkg/local.sqlite: no such file or directory")
)

// freshCollector wires a Collector to a fake runner and a fresh
// catalogue, with every time source fixed.
func freshCollector(t *testing.T, runner *fakeRunner) *Collector {
	t.Helper()
	return NewCollector(Options{
		Runner:    runner,
		Privilege: fakePrivilege{root: true, detail: "effective uid is 0 (test)"},
		Now:       func() time.Time { return baseTime },
		stat:      fixedStat{mod: baseTime.Add(-24 * time.Hour)},
	})
}

// standardHost scripts the three commands a healthy pkgbase-free host
// answers: two outdated packages and a catalogue refreshed a day ago.
func standardHost() *fakeRunner {
	return newFakeRunner().script(standardHostResponses())
}

func standardHostResponses() map[string]response {
	return map[string]response{
		"uname -r":    {stdout: "15.0-RELEASE-p4\n"},
		"uname -v":    {stdout: "15.0-RELEASE-p4\n"},
		"pkg version": {stdout: "FreeBSD 15.0-RELEASE-p4 [repo FreeBSD]: up to date\n"},
		"pkg query -e %n|%v|%o %n-%v": {stdout: strings.Join([]string{
			"bash|5.2.26|shells/bash",
			"curl|8.9.1|www/curl",
			"nginx|1.26.0|www/nginx",
		}, "\n") + "\n"},
		"pkg outdated": {stdout: strings.Join([]string{
			"bash                        <   bash-5.2.15",
			"                                >   bash-5.2.26",
			"curl                        <   curl-8.9.1",
			"                                >   curl-8.14.1",
		}, "\n") + "\n"},
	}
}

// upgradedHostResponses is standardHostResponses after a successful
// upgrade of bash and curl. It is what a post-condition read must see
// before an apply may be called applied.
func upgradedHostResponses() map[string]response {
	return map[string]response{
		"uname -r":    {stdout: "15.0-RELEASE-p4\n"},
		"uname -v":    {stdout: "15.0-RELEASE-p4\n"},
		"pkg version": {stdout: "FreeBSD 15.0-RELEASE-p4 [repo FreeBSD]: up to date\n"},
		"pkg query -e %n|%v|%o %n-%v": {stdout: strings.Join([]string{
			"bash|5.2.26|shells/bash",
			"curl|8.14.1|www/curl",
			"nginx|1.26.0|www/nginx",
		}, "\n") + "\n"},
		"pkg outdated": {stdout: ""},
	}
}

func findPackage(inv Inventory, name string) (InstalledPackage, bool) {
	for _, p := range inv.Ports {
		if p.Name == name {
			return p, true
		}
	}
	return InstalledPackage{}, false
}

func assertUnknown(t *testing.T, inv Inventory, subject string) {
	t.Helper()
	for _, u := range inv.Unknown {
		if strings.Contains(u.Subject, subject) {
			if strings.TrimSpace(u.Reason) == "" {
				t.Errorf("unknown for %q has an empty reason; every unknown must be explainable", subject)
			}
			return
		}
	}
	t.Errorf("no unknown recorded for %q; unknowns = %+v", subject, inv.Unknown)
}
