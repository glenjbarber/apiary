package jailnet

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
)

// Runner executes one external command and returns its trimmed stdout.
// It is the same contract, and deliberately the same interface shape,
// as jail.CommandRunner: this package drives ifconfig(8) on the *host*
// side of the pair (vlan.Manager is not injectable, and the bridge
// membership question this package has to answer - "is the host side
// actually a member of the bridge we think it is" - is one vlan.Manager
// does not expose at all), so it shells out itself rather than routing
// through another package's private copy.
type Runner interface {
	Run(ctx context.Context, name string, args ...string) (string, error)
}

// execRunner is the production Runner. It is deliberately this
// package's own tiny copy of the "run a command, fold its stderr into
// the error" wrapper rather than a call into jail.CommandRunner or
// vlan's private one: internal/hast, internal/bhyve, internal/vlan,
// internal/jail and this package each keep their own, a pattern
// internal/vlan's own doc comment calls out as intentional, and none of
// them are exported.
type execRunner struct{}

func (execRunner) Run(ctx context.Context, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return "", fmt.Errorf("%s %s: %s", name, strings.Join(args, " "), msg)
	}
	return strings.TrimSpace(stdout.String()), nil
}

// HostState is what `ifconfig <name>` on the host actually said about
// one interface, at the moment it was asked.
//
// Every field is a definite answer or the zero value of a question that
// was not answered; the distinction is carried by the error return, not
// by a field, because "exists: false" and "could not look" must never be
// the same value in any struct a caller can accidentally compare.
type HostState struct {
	// Exists is false only when ifconfig gave a definite "no such
	// interface" answer.
	Exists bool

	// Up is the interface's own UP flag. Only meaningful when Exists.
	Up bool

	// MemberOf is the bridge this interface is a member of, or "" if it
	// is in no bridge. Only meaningful when Exists.
	MemberOf string
}

// ObserveHost reads one interface's real state from the host's ifconfig.
//
// The three-valued discipline is the whole point of this function, so
// it is worth stating precisely:
//
//   - ifconfig exits non-zero with "does not exist" on stderr for an
//     interface that is not there. That is a definite answer:
//     HostState{Exists: false} and a nil error.
//   - Every other failure - a permission error, a timeout, a missing
//     binary, a context cancellation - is a failure to find out, and
//     returns the zero HostState with a non-nil error. A caller that
//     cannot look must not conclude "gone", because the repair for
//     "gone" is to destroy and re-provision, and firing that on a
//     momentary ifconfig hiccup would tear down a working jail's
//     interface over nothing.
func (r *Reconciler) ObserveHost(ctx context.Context, iface string) (HostState, error) {
	out, err := r.run(ctx, "ifconfig", iface)
	if err != nil {
		if isAbsent(err) {
			return HostState{Exists: false}, nil
		}
		return HostState{}, fmt.Errorf("%w: observing host interface %s: %s", errStateUnknown, iface, err)
	}
	return HostState{
		Exists:   true,
		Up:       ifconfigIsUp(out),
		MemberOf: ifconfigMemberOf(out),
	}, nil
}

// ifconfigIsUp reports whether the interface's first line carries an
// exact "UP" flag inside its <...> list, e.g.
//
//	epair0a: flags=8843<UP,BROADCAST,RUNNING,SIMPLEX,MULTICAST> metric 0 mtu 1500
//
// Matching the whole flag rather than a substring matters: "UP" is a
// substring of nothing else FreeBSD defines today, but "the interface's
// status line mentions up" is not the same claim as "the UP flag is
// set", and the second is the one that decides whether a jail's
// interface can pass traffic.
func ifconfigIsUp(out string) bool {
	first, _, _ := strings.Cut(out, "\n")
	start := strings.Index(first, "<")
	end := strings.Index(first, ">")
	if start < 0 || end < start {
		return false
	}
	for _, flag := range strings.Split(first[start+1:end], ",") {
		if flag == "UP" {
			return true
		}
	}
	return false
}

// ifconfigMemberOf reports which bridge(4) interface out says this
// interface is a member of, or "" if it is in none.
//
// A bridge member's own ifconfig output carries the membership on a
// "member:" line, e.g. the shape FreeBSD has used for many releases:
//
//	member: bridge1 flags=3<LEARNING,DISCOVER>                              ifmaxaddr 0 port 8 priority 128 path cost 20000 proto rstp
//
// and a plain (unbridged) interface has no such line at all, which is
// why "" is a real, useful answer here rather than a failure: an epair
// host side that has silently fallen out of its bridge is precisely the
// drift this package exists to catch.
//
// The line is matched on its first token being exactly "member:" and on
// it carrying a "flags=" token, deliberately without requiring any
// particular indentation. ifconfig's bridge status block is the only
// place that keyword appears at all, so the keyword is a stronger
// discriminator than the indentation is, and FreeBSD has moved that
// block around between releases - an indentation-sensitive parser here
// would read a real membership as no membership, and this package would
// then "repair" a correctly-attached interface on every single tick.
// Unverifiable on a development host, and the failure mode of getting
// it wrong is a permanent, silent misreport, so it is parsed as
// liberally as the keyword safely allows.
//
// The member lines are counted rather than returned as they are found,
// because a bridge's own `ifconfig bridge1` output is a *list* of its
// members laid out with the same "member:" keyword as a member's own
// membership claim - and the two are only distinguishable by how many
// of them there are. A plain interface can be a member of at most one
// bridge, so it can never have two "member:" lines; a bridge lists one
// per member. Reading a bridge's first member as the bridge's own
// membership would make this function report every bridge on the node
// as being attached to its first member, which is the kind of wrong
// answer that turns into a "repair" on every single tick.
//
// The bridge's member list also never includes the bridge itself, so
// comparing the names against iface would not catch it - the
// multiplicity is the only signal available.
func ifconfigMemberOf(out string) string {
	var members []string
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 3 || fields[0] != "member:" {
			continue
		}
		if !anyHasPrefix(fields[1:], "flags=") {
			continue
		}
		// The name may or may not carry a trailing colon depending on
		// the release; neither form is a different bridge.
		members = append(members, strings.TrimSuffix(fields[1], ":"))
	}
	if len(members) != 1 {
		// None: not in a bridge. More than one: this is a bridge's own
		// member list, which says nothing about where it is attached.
		return ""
	}
	return members[0]
}

func anyHasPrefix(fields []string, want string) bool {
	for _, f := range fields {
		if strings.HasPrefix(f, want) {
			return true
		}
	}
	return false
}

// absentMarkers are the stderr substrings ifconfig(8) itself uses to say
// an interface does not exist. Deliberately the same single marker
// internal/vlan's own ifaceExists already matches on, so the two
// packages cannot drift into disagreeing about what "absent" means.
var absentMarkers = []string{"does not exist"}

// binaryMissingMarkers are the ways a shell or the Go runtime report
// that the command itself could not be run. They are checked first and
// separately because a missing ifconfig(8) on a node is not an absent
// interface, and conflating the two would let Apiary destroy and
// re-provision every jail's interface on a node whose ifconfig is
// simply not installed.
var binaryMissingMarkers = []string{
	"command not found",
	"executable file not found",
	"no such file or directory",
}

var shellNames = []string{"sh", "bash", "zsh", "ksh", "dash", "csh", "tcsh"}

// isAbsent reports whether err is a definite "ifconfig ran and said
// there is no such interface" answer. Anything else - including a
// failure to run ifconfig at all - is false, and a false here means
// unknown, never "absent".
func isAbsent(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	for _, marker := range binaryMissingMarkers {
		if strings.Contains(msg, marker) {
			return false
		}
	}
	for _, shell := range shellNames {
		for _, prefix := range []string{shell + ": ", "/bin/" + shell + ": "} {
			if strings.Contains(msg, prefix) {
				return false
			}
		}
	}
	for _, marker := range absentMarkers {
		if strings.Contains(msg, marker) {
			return true
		}
	}
	return false
}
