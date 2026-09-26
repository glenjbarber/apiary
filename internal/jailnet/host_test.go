package jailnet

import (
	"errors"
	"fmt"
	"testing"
)

// TestIfconfigIsUp confirms the UP flag is read as a whole flag, from
// the interface's own first line. A single hop's state is the whole
// difference between a jail that passes traffic and one that does not,
// so a false negative here is a permanent silent misreport.
func TestIfconfigIsUp(t *testing.T) {
	tests := []struct {
		name string
		out  string
		want bool
	}{
		{
			name: "up",
			out:  "epair0a: flags=8843<UP,BROADCAST,RUNNING,SIMPLEX,MULTICAST> metric 0 mtu 1500\n\tstatus: active\n",
			want: true,
		},
		{
			name: "down",
			out:  "epair0a: flags=8843<BROADCAST,RUNNING,SIMPLEX,MULTICAST> metric 0 mtu 1500\n\tstatus: no carrier\n",
			want: false,
		},
		{
			// "status: active" is not the UP flag. Reading it as one
			// would call a downed interface healthy.
			name: "active status without the UP flag is still down",
			out:  "epair0a: flags=8843<BROADCAST,SIMPLEX,MULTICAST> metric 0 mtu 1500\n\tstatus: active\n",
			want: false,
		},
		{
			name: "no flag list at all",
			out:  "epair0a: metric 0 mtu 1500\n",
			want: false,
		},
		{
			name: "empty output",
			out:  "",
			want: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ifconfigIsUp(tt.out); got != tt.want {
				t.Errorf("ifconfigIsUp() = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestIfconfigMemberOf covers the bridge-membership line across the
// layouts FreeBSD has used, plus the many unindented continuation lines
// ifconfig emits that must not be mistaken for one. This parser decides
// whether the reconciler believes an interface is attached, so a false
// negative here means re-joining a correctly-attached interface on
// every single tick.
func TestIfconfigMemberOf(t *testing.T) {
	tests := []struct {
		name string
		out  string
		want string
	}{
		{
			name: "the documented bridge member layout",
			out: `epair0a: flags=8843<UP,BROADCAST,RUNNING,SIMPLEX,MULTICAST> metric 0 mtu 1500
	ether 02:1a:2b:3c:4d:5e
	media: Ethernet autoselect (1000baseT <full-duplex>)
	status: active
	member: bridge1 flags=3<LEARNING,DISCOVER>                              ifmaxaddr 0 port 8 priority 128 path cost 20000 proto rstp
		input filter not set`,
			want: "bridge1",
		},
		{
			name: "indented member line, as newer ifconfig renders it",
			out: `epair0a: flags=8843<UP> metric 0 mtu 1500
	member: bridge1 flags=143<LEARNING,DISCOVER,AUTOEDGE,AUTOPTP>
		ifmaxaddr 0 port 1 priority 128 path cost 20000 proto rstp`,
			want: "bridge1",
		},
		{
			name: "bridge name carrying a trailing colon",
			out:  "epair0a: flags=8843<UP>\n\tmember: bridge2: flags=3<LEARNING>\n",
			want: "bridge2",
		},
		{
			name: "an unbridged interface has no membership at all",
			out: `epair0a: flags=8843<UP,BROADCAST,RUNNING,SIMPLEX,MULTICAST> metric 0 mtu 1500
	ether 02:1a:2b:3c:4d:5e
	media: Ethernet autoselect (1000baseT <full-duplex>)
	status: active
	nd6 options=29<PERFORMNUD,IFDISABLED,AUTO_LINKLOCAL>`,
			want: "",
		},
		{
			// The bridge's own ifconfig lists its members, and a
			// misattributed "member: " line is precisely how a bridge
			// would be mistaken for one of its own members.
			name: "the bridge's own member list is not a membership claim",
			out: `bridge1: flags=8843<UP,BROADCAST,RUNNING,SIMPLEX,MULTICAST> metric 0 mtu 1500
	ether 02:1a:2b:3c:4d:5f
	member: epair0a flags=3<LEARNING,DISCOVER>
	member: vtnet0 flags=3<LEARNING,DISCOVER>`,
			want: "",
		},
		{
			name: "a member line with no flags clause is not a membership claim",
			out:  "epair0a: flags=8843<UP>\n\tmember: bridge1\n",
			want: "",
		},
		{
			name: "empty output",
			out:  "",
			want: "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ifconfigMemberOf(tt.out); got != tt.want {
				t.Errorf("ifconfigMemberOf() = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestIsAbsent confirms the absent/unknown discrimination at the
// ifconfig level. Only ifconfig's own "does not exist" is a definite
// answer; every other failure is a failure to find out, and being wrong
// in that direction means tearing down a working jail's interface over a
// transient hiccup.
func TestIsAbsent(t *testing.T) {
	tests := []struct {
		msg  string
		want bool
	}{
		{msg: "ifconfig: interface epair0a does not exist", want: true},
		{msg: "ifconfig: ioctl (SIOCGIFFLAGS): Operation not permitted", want: false},
		{msg: "sh: ifconfig: not found", want: false},
		{msg: "zsh: command not found: ifconfig", want: false},
		{msg: `exec: "ifconfig": executable file not found in $PATH`, want: false},
		{msg: "fork/exec /sbin/ifconfig: no such file or directory", want: false},
		{msg: "csh: ifconfig: not found", want: false},
		{msg: "/bin/sh: 1: ifconfig: not found", want: false},
		{msg: "context deadline exceeded", want: false},
		{msg: "interface not found", want: false},
	}
	for _, tt := range tests {
		t.Run(tt.msg, func(t *testing.T) {
			if got := isAbsent(errors.New(tt.msg)); got != tt.want {
				t.Errorf("isAbsent(%q) = %v, want %v", tt.msg, got, tt.want)
			}
		})
	}
	if isAbsent(nil) {
		t.Error("isAbsent(nil) = true, want false: a nil error is not an absent-object report")
	}
}

// TestObserveHost_PresentAndAbsent is the end-to-end shape of a host
// observation: definite answers for both outcomes, and an error for
// everything in between.
func TestObserveHost_PresentAndAbsent(t *testing.T) {
	t.Run("present", func(t *testing.T) {
		runner := newFakeRunner().withState("epair0a", true, "bridge1")
		r := &Reconciler{Runner: runner}
		state, err := r.ObserveHost(t.Context(), "epair0a")
		if err != nil {
			t.Fatalf("ObserveHost() error = %v", err)
		}
		if !state.Exists || !state.Up || state.MemberOf != "bridge1" {
			t.Errorf("ObserveHost() = %+v, want exists/up/bridge1", state)
		}
	})

	t.Run("definitely absent", func(t *testing.T) {
		runner := newFakeRunner().without("epair0a")
		r := &Reconciler{Runner: runner}
		state, err := r.ObserveHost(t.Context(), "epair0a")
		if err != nil {
			t.Fatalf("ObserveHost() error = %v, want nil for a definite absent interface", err)
		}
		if state.Exists {
			t.Errorf("ObserveHost() = %+v, want Exists=false", state)
		}
		if state.Up || state.MemberOf != "" {
			t.Errorf("ObserveHost() = %+v, want the zero value: an absent interface has no other state", state)
		}
	})

	t.Run("could not look", func(t *testing.T) {
		runner := newFakeRunner().withState("epair0a", true, "bridge1").failAll("ifconfig: ioctl: Operation not permitted")
		r := &Reconciler{Runner: runner}
		state, err := r.ObserveHost(t.Context(), "epair0a")
		if err == nil {
			t.Fatalf("ObserveHost() = %+v, want an error", state)
		}
		if !errors.Is(err, errStateUnknown) {
			t.Errorf("error = %v, want it to wrap errStateUnknown so a caller can tell unknown from wrong without string matching", err)
		}
		if state.Exists {
			t.Errorf("ObserveHost() = %+v, want the zero value: nothing was observed", state)
		}
	})
}

// TestResultVerdictUnknownInvariant is a belt-and-braces check on the
// package's central promise, stated once over every verdict: a caller
// must be able to read Observed and Verdict interchangeably, and
// VerdictUnknown must never be reachable with Observed set.
func TestResultVerdictUnknownInvariant(t *testing.T) {
	for _, v := range []Verdict{VerdictInSync, VerdictRepaired, VerdictDrifted, VerdictNotRunning, VerdictUnknown} {
		res := Result{Verdict: v, Observed: v != VerdictUnknown}
		if res.Observed == res.Unknown() {
			t.Errorf("Verdict %q: Observed=%v Unknown()=%v, want exactly one of the two to hold", v, res.Observed, res.Unknown())
		}
	}
	// And the spelled-out values, because these strings are what a UI
	// and an operator will read; changing one is a visible change.
	want := map[Verdict]string{
		VerdictInSync:     "in_sync",
		VerdictRepaired:   "repaired",
		VerdictDrifted:    "drifted",
		VerdictUnknown:    "unknown",
		VerdictNotRunning: "not_running",
	}
	for v, w := range want {
		if string(v) != w {
			t.Errorf("Verdict %q, want the value %q to be stable: it is read by operators", v, w)
		}
	}
}

// TestFindingsAreStableValues is the same kind of assertion for the
// finding strings.
func TestFindingsAreStableValues(t *testing.T) {
	want := map[Finding]string{
		FindingRestartRequired:       "restart_required",
		FindingEpairMissing:          "epair_missing",
		FindingEpairRecordCorrupt:    "epair_record_corrupt",
		FindingHostSideDown:          "host_side_down",
		FindingBridgeMembershipWrong: "bridge_membership_wrong",
		FindingAddressWrong:          "address_wrong",
		FindingForeignAddress:        "foreign_address",
		FindingRouteWrong:            "route_wrong",
	}
	for f, w := range want {
		if string(f) != w {
			t.Errorf("Finding %q, want the value %q to be stable", f, w)
		}
	}
	if got := fmt.Sprint(FindingRestartRequired); got != "restart_required" {
		t.Errorf("Sprint(FindingRestartRequired) = %q, want %q: a finding is logged with plain %%v, so it has to render as itself", got, "restart_required")
	}
}
