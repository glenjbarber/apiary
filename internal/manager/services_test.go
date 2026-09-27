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
	if !strings.Contains(managerdSelfRestartRefusal, "make force-restart") {
		t.Error("the refusal message must point the operator at make force-restart")
	}
	if !strings.Contains(managerdSelfRestartRefusal, "service apiary_managerd restart") {
		t.Error("the refusal message must also offer the managerd-only alternative")
	}
}
