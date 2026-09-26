package restartplan

import (
	"context"
	"strings"
	"time"

	"github.com/glenjbarber/apiary/internal/cluster"
	"github.com/glenjbarber/apiary/internal/guardrail"
)

// --- shared test doubles -------------------------------------------------

// fakeClock is a Clock with no real time in it. Sleep records the
// requested durations and returns immediately, so a test can assert on
// the backoff a confirmation budget actually used without spending it,
// and can make a sleep fail on demand to exercise cancellation.
type fakeClock struct {
	now      time.Time
	sleeps   []time.Duration
	sleepErr error
}

func newFakeClock() *fakeClock {
	return &fakeClock{now: time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) Now() time.Time {
	c.now = c.now.Add(time.Second)
	return c.now
}

func (c *fakeClock) Sleep(ctx context.Context, d time.Duration) error {
	c.sleeps = append(c.sleeps, d)
	if c.sleepErr != nil {
		return c.sleepErr
	}
	return ctx.Err()
}

// reachableVoters builds a reachability slice from a list of node ids
// that answered, so a table's "which voters are up" reads as a set
// rather than as a parallel list of booleans that can silently disagree
// with the ids.
func reachableVoters(all []string, up ...string) []VoterReachability {
	isUp := make(map[string]bool, len(up))
	for _, id := range up {
		isUp[id] = true
	}
	out := make([]VoterReachability, 0, len(all))
	for _, id := range all {
		reach := cluster.ReachabilityUnreachable
		if isUp[id] {
			reach = cluster.ReachabilityReachable
		}
		out = append(out, VoterReachability{NodeID: id, RaftBindAddress: id + ":19999", Reachability: reach, ProbeError: "connection refused"})
	}
	return out
}

func unknownVoters(ids ...string) []VoterReachability {
	out := make([]VoterReachability, 0, len(ids))
	for _, id := range ids {
		out = append(out, VoterReachability{NodeID: id, RaftBindAddress: id + ":19999", Reachability: cluster.ReachabilityUnknown, ProbeError: "not probed"})
	}
	return out
}

func rulesOf(r guardrail.Report) []string {
	out := make([]string, 0, len(r.Findings))
	for _, f := range r.Findings {
		out = append(out, f.Rule)
	}
	return out
}

func hasRule(r guardrail.Report, rule string) bool {
	for _, got := range rulesOf(r) {
		if got == rule {
			return true
		}
	}
	return false
}

func caveatContaining(r guardrail.Report, sub string) bool {
	for _, c := range r.Caveats {
		if strings.Contains(c.Detail, sub) {
			return true
		}
	}
	return false
}

func findingFor(r guardrail.Report, rule string) (guardrail.Finding, bool) {
	for _, f := range r.Findings {
		if f.Rule == rule {
			return f, true
		}
	}
	return guardrail.Finding{}, false
}
