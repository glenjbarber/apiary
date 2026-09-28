package manager

import (
	"strings"
	"testing"
)

// TestRestartableService_Allowlist confirms the allowlist is exactly the
// four Apiary rc.d services and nothing else - the property that keeps
// RestartNodeService from ever being talked into running an arbitrary
// `service X restart` by a name off the wire.
//
// ADR-0102 made restshimd restartable (stateless, non-consensus,
// UpdateRestshimdConfig needs to trigger it after a config save) while
// leaving raftd excluded as consensus-critical. ADR-0125 then reversed
// that half of the judgment: raftd became restartable precisely because
// the quorum-safety guardrail now exists to make exposing it safe, so a
// restart of it reserves a cluster-wide lease and is refused outright
// when the remaining voters would not form a majority. What did NOT
// change is the other half - UpdateRaftdConfig still never auto-restarts
// raftd on a config write, and "restartable on request" must never drift
// into "restarted as a side effect of saving a value".
func TestRestartableService_Allowlist(t *testing.T) {
	cases := []struct {
		name string
		want bool
	}{
		{"apiary_managerd", false},
		{"apiary_frontend", true},
		{"apiary_restshimd", true},
		{"apiary_raftd", true},
		{"apiary_unknown", false},
		// Near-misses that must stay false: a name that merely contains
		// a guardrailed service, and a plausible rc.d name from another
		// project, are both outside the closed set.
		{"apiary_managerd_extra", false},
		{"sshd", false},
	}
	for _, c := range cases {
		if got := restartableService(c.name); got != c.want {
			t.Errorf("restartableService(%q) = %v, want %v", c.name, got, c.want)
		}
	}
}

// TestManagerdSelfRestartRefused covers the predicate that guards the
// refusal, including the near-miss that must not trip it: a service
// whose name merely contains "apiary_managerd" is a different service
// and must reach the normal allowlist check instead.
func TestManagerdSelfRestartRefused(t *testing.T) {
	if !managerdSelfRestartRefused("apiary_managerd") {
		t.Error("apiary_managerd must be refused a self-restart")
	}
	for _, name := range []string{"apiary_raftd", "apiary_frontend", "apiary_restshimd", "apiary_managerd_extra", ""} {
		if managerdSelfRestartRefused(name) {
			t.Errorf("managerdSelfRestartRefused(%q) = true, want false", name)
		}
	}
	// The message must name a way forward. An operator reading only
	// this string is the entire audience for it.
	if !strings.Contains(managerdSelfRestartRefusal, "apiaryctl force-restart") {
		t.Error("the refusal message must point the operator at apiaryctl force-restart")
	}
	if !strings.Contains(managerdSelfRestartRefusal, "service apiary_managerd restart") {
		t.Error("the refusal message must also offer the managerd-only alternative")
	}
	// Naming the command is not the same as being able to run it. This
	// message is read in a browser, possibly on a laptop, by someone who
	// then has to type the next thing on a Comb - and a Comb has no
	// source checkout on it. The command force-restart used to live in
	// did need one, and naming the checkout was the honest fix at the
	// time; the honest fix now is that the command is installed
	// (ADR-0136) and this string must not send anyone looking for a
	// Makefile. Every phrase below was true of this string once and is a
	// defect now.
	//
	// A substring ban is a blunt instrument and the message has to be
	// written around it: "neither needs a source checkout" tripped this
	// check, and was the right thing to trip it, because an operator
	// reading "needs a source checkout" skims past "no". The positive
	// assertion below is the one that carries the meaning; the ban is
	// only there to stop the old wording creeping back.
	for _, forbidden := range []string{"make force-restart", "source checkout", "Makefile", "record-forced-restart"} {
		if strings.Contains(managerdSelfRestartRefusal, forbidden) {
			t.Errorf("the refusal message contains %q, which reads as pointing the operator at a checkout the Comb does not have: %q", forbidden, managerdSelfRestartRefusal)
		}
	}
	// Said positively, because this is the fact that makes the next
	// command the operator types actually work.
	for _, want := range []string{"installed on every Comb", "root shell"} {
		if !strings.Contains(managerdSelfRestartRefusal, want) {
			t.Errorf("the refusal message must say %q, so the operator knows the next command works on this Comb; got %q", want, managerdSelfRestartRefusal)
		}
	}
	// And the wider command has to be the second one named. Restarting
	// managerd and raftd is a bigger act than the operator asked for.
	plain := strings.Index(managerdSelfRestartRefusal, "service apiary_managerd restart")
	forced := strings.Index(managerdSelfRestartRefusal, "apiaryctl force-restart")
	if plain < 0 || forced < 0 || plain > forced {
		t.Errorf("the managerd-only alternative must come first, got plain=%d forced=%d in %q", plain, forced, managerdSelfRestartRefusal)
	}
}
