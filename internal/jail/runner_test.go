package jail

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// TestNotFound confirms only a definite "this does not exist" answer
// from the tool itself counts as absent. Everything else - a
// permission error, a timeout, a missing binary - is a failure to find
// out, and a caller that mistook it for "absent" would report a
// missing jail or a missing interface as a fact when it had no
// evidence at all.
func TestNotFound(t *testing.T) {
	tests := []struct {
		tool string
		msg  string
		want bool
	}{
		// Each tool's own absent-object wording, scoped to that tool.
		{tool: "jls", msg: "jls: apiary-web-1: not found", want: true},
		{tool: "ifconfig", msg: "ifconfig: interface epair0b does not exist", want: true},
		{tool: "jexec", msg: "jexec: jail not found", want: true},
		// Failures to find out are never absent.
		{tool: "jls", msg: "jls: permission denied", want: false},
		{tool: "jexec", msg: "context deadline exceeded", want: false},
		{tool: "ifconfig", msg: "ifconfig: ioctl: Operation not permitted", want: false},
		// One tool's wording, reached through a different tool's
		// question, is not that tool's answer.
		{tool: "jls", msg: "ifconfig: interface epair0b does not exist", want: false},
		{tool: "ifconfig", msg: "jls: apiary-web-1: not found", want: false},
		// The binary failing to run is not the binary reporting on
		// the system. This is the case a loose "not found" substring
		// gets catastrophically wrong: it turns a broken node into a
		// confident "this jail does not exist", which Apiary would
		// then act on by creating the jail again.
		{tool: "jls", msg: "sh: jls: not found", want: false},
		{tool: "jls", msg: "/bin/sh: 1: jls: not found", want: false},
		{tool: "jls", msg: "zsh: command not found: jls", want: false},
		{tool: "jls", msg: `exec: "jls": executable file not found in $PATH`, want: false},
		{tool: "jexec", msg: "sh: jexec: not found", want: false},
		// ...but a genuinely different tool's missing-binary report
		// is not this one's problem either way.
		{tool: "jexec", msg: "sh: ifconfig: not found", want: false},
		{tool: "jexec", msg: "csh: jexec: not found", want: false},
	}
	for _, tt := range tests {
		t.Run(tt.tool+"/"+tt.msg, func(t *testing.T) {
			if got := notFound(errors.New(tt.msg), tt.tool); got != tt.want {
				t.Errorf("notFound(%q, %q) = %v, want %v", tt.msg, tt.tool, got, tt.want)
			}
		})
	}
	if notFound(nil, "jls") {
		t.Errorf("notFound(nil, ...) = true, want false: a nil error is not an absent-object report")
	}
}

// TestAbsentObjectMarkersAreToolScoped guards the map itself: every
// tool this package drives must have an entry, and no marker may be so
// short that it reads as absent on a message it was never written for.
// Uniqueness across tools is deliberately not asserted - jls(8) and
// jexec(8) genuinely do use the same wording for the same fact.
func TestAbsentObjectMarkersAreToolScoped(t *testing.T) {
	for _, tool := range []string{"ifconfig", "jls", "jexec"} {
		if len(absentObjectMarkers[tool]) == 0 {
			t.Errorf("absentObjectMarkers has no entry for %q", tool)
		}
	}
	for tool, markers := range absentObjectMarkers {
		for _, marker := range markers {
			if len(marker) < 8 {
				t.Errorf("absentObjectMarkers[%q] contains %q, too short to be specific to one tool's wording", tool, marker)
			}
		}
	}
}

// TestRunUsesInjectedRunner confirms the injection seam actually
// intercepts execution, so the rest of this package's tests are
// testing this package's logic rather than whatever ifconfig happens
// to be installed on the developer's laptop.
func TestRunUsesInjectedRunner(t *testing.T) {
	f := newFakeRunner().on("ifconfig epair0b", "epair0b: flags=8843<UP>")
	m := New("apiary-")
	m.Runner = f

	out, err := m.run(context.Background(), "ifconfig", "epair0b")
	if err != nil {
		t.Fatalf("run() error = %v", err)
	}
	if out != "epair0b: flags=8843<UP>" {
		t.Errorf("run() = %q", out)
	}
}

// TestListJailsUnaffectedByInjection confirms the pre-existing
// ip4=inherit-era listing path still works through the new seam: this
// is the code every existing jail in every existing deployment runs
// through, and ADR-0117's whole compatibility claim rests on it being
// unchanged behaviorally.
func TestListJailsUnaffectedByInjection(t *testing.T) {
	f := newFakeRunner().on("jls -n name", "name=apiary-a\nname=apiary-b\nname=someone-elses")
	m := New("apiary-")
	m.Runner = f

	got, err := m.ListJails(context.Background())
	if err != nil {
		t.Fatalf("ListJails() error = %v", err)
	}
	// A jail without this Manager's prefix is Apiary's own to ignore,
	// and still is here - the scoping guarantee is the whole reason
	// this Manager is safe to point at a real host.
	if want := []string{"a", "b"}; strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("ListJails() = %v, want %v", got, want)
	}
}

func TestJailExists(t *testing.T) {
	tests := []struct {
		name    string
		handler func(*fakeRunner)
		want    bool
		wantErr bool
	}{
		{
			name:    "running",
			handler: func(f *fakeRunner) { f.on("jls -j apiary-web-1 -n name", "apiary-web-1") },
			want:    true,
		},
		{
			name:    "definitely absent",
			handler: func(f *fakeRunner) { f.onErr("jls -j apiary-web-1 -n name", "jls: apiary-web-1: not found") },
			want:    false,
		},
		{
			name:    "could not look is an error, not absent",
			handler: func(f *fakeRunner) { f.onErr("jls -j apiary-web-1 -n name", "jls: permission denied") },
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFakeRunner()
			tt.handler(f)
			m := New("apiary-")
			m.Runner = f

			got, err := m.JailExists(context.Background(), "web-1")
			if (err != nil) != tt.wantErr {
				t.Fatalf("JailExists() error = %v, wantErr %v", err, tt.wantErr)
			}
			if got != tt.want {
				t.Errorf("JailExists() = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestCreateJailVNETAddressesViaEnsureAddressing confirms that
// creation-time addressing goes through the same observe/compare/
// repair sequence the per-tick reconciler uses. Before this, a
// creation that added the address but failed on the route would
// retry forever against ifconfig's "File exists", because nothing
// ever looked before writing.
func TestCreateJailVNETAddressesViaEnsureAddressing(t *testing.T) {
	f := newFakeRunner().
		on("jail -c name=apiary-web-1 path=/r host.hostname=web-1 vnet vnet.interface=epair0b persist", "").
		// The address is already there from a previous partial
		// attempt, and the route is missing - the exact state that
		// used to fail forever.
		on("jexec apiary-web-1 ifconfig epair0b", sampleEpairIfconfig).
		on("jexec apiary-web-1 netstat -rn -f inet", "Routing tables\n").
		on("jexec apiary-web-1 route add default 10.0.1.1", "")
	m := New("apiary-")
	m.Runner = f

	err := m.CreateJail(context.Background(), "web-1", Config{
		Path:          "/r",
		Hostname:      "web-1",
		VNET:          true,
		VNETInterface: "epair0b",
		IPAddress:     "10.0.1.5",
		IPPrefixLen:   24,
		Gateway:       "10.0.1.1",
	})
	if err != nil {
		t.Fatalf("CreateJail() error = %v", err)
	}
	// The already-present address must not be re-added.
	if n := f.callsTo("jexec apiary-web-1 ifconfig epair0b inet 10.0.1.5/24 up"); n != 0 {
		t.Errorf("re-added an address that was already present %d time(s), want 0", n)
	}
	if n := f.callsTo("jexec apiary-web-1 route add default 10.0.1.1"); n != 1 {
		t.Errorf("added the missing default route %d time(s), want 1", n)
	}
}

// TestCreateJailVNETAddressingFailureIsFatal confirms a failed
// addressing step fails the whole create rather than leaving a running
// jail silently unaddressed: the jail exists, so the next reconcile
// tick will see it "running" and (before Stage 2) never retry.
func TestCreateJailVNETAddressingFailureIsFatal(t *testing.T) {
	f := newFakeRunner().
		on("jail -c name=apiary-web-1 path=/r host.hostname=web-1 vnet vnet.interface=epair0b persist", "").
		onErr("jexec apiary-web-1 ifconfig epair0b", "jexec: operation not permitted")
	m := New("apiary-")
	m.Runner = f

	err := m.CreateJail(context.Background(), "web-1", Config{
		Path:          "/r",
		Hostname:      "web-1",
		VNET:          true,
		VNETInterface: "epair0b",
		IPAddress:     "10.0.1.5",
		IPPrefixLen:   24,
		Gateway:       "10.0.1.1",
	})
	if err == nil {
		t.Fatalf("CreateJail() error = nil, want the addressing failure surfaced")
	}
	if !strings.Contains(fmt.Sprint(err), "not permitted") {
		t.Errorf("error = %v, want it to carry the underlying failure", err)
	}
}
