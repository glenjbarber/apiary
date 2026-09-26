package restartplan

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/glenjbarber/apiary/internal/cluster"
)

// TestProbeVotersReachability covers the three shapes ADR-0125 §3 names
// for a probe: everyone answered, one peer did not, nobody answered. The
// property asserted in every case is the same - one result per input
// voter, in input order, and one peer's failure never aborts the rest.
func TestProbeVotersReachability(t *testing.T) {
	voters := []Voter{
		{NodeID: "comb-a", RaftBindAddress: "10.0.0.1:19999"},
		{NodeID: "comb-b", RaftBindAddress: "10.0.0.2:19999"},
		{NodeID: "comb-c", RaftBindAddress: "10.0.0.3:19999"},
	}

	cases := []struct {
		name           string
		down           []string
		wantReachable  []string
		wantDown       []string
		wantReadOK     bool
		wantDialedAll  bool
		wantProbeError bool
	}{
		{
			name:          "every peer answers",
			wantReachable: []string{"comb-a", "comb-b", "comb-c"},
			wantReadOK:    true, wantDialedAll: true,
		},
		{
			name:          "one peer refuses",
			down:          []string{"comb-b"},
			wantReachable: []string{"comb-a", "comb-c"},
			wantDown:      []string{"comb-b"},
			wantReadOK:    true, wantDialedAll: true, wantProbeError: true,
		},
		{
			name:       "no peer answers",
			down:       []string{"comb-a", "comb-b", "comb-c"},
			wantDown:   []string{"comb-a", "comb-b", "comb-c"},
			wantReadOK: true, wantDialedAll: true, wantProbeError: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Keyed by raft_bind_address, because that is what the
			// injected dial actually receives.
			down := map[string]bool{}
			for _, id := range tc.down {
				down[id+":19999"] = true
			}
			var dialed []string
			report := ProbeVoters(context.Background(), voters, ProbeOptions{
				Dial: func(_ context.Context, addr string) error {
					dialed = append(dialed, addr)
					if down[addr] {
						return errors.New("dial tcp " + addr + ": connect: connection refused")
					}
					return nil
				},
			})

			if report.ReadOK != tc.wantReadOK {
				t.Errorf("ReadOK = %v, want %v", report.ReadOK, tc.wantReadOK)
			}
			if len(report.Voters) != len(voters) {
				t.Fatalf("got %d results for %d voters; the result slice must align with the input", len(report.Voters), len(voters))
			}
			for i, got := range report.Voters {
				if got.NodeID != voters[i].NodeID {
					t.Errorf("result %d is for %q, want %q - order must match the input", i, got.NodeID, voters[i].NodeID)
				}
				wantUp := !down[got.NodeID]
				if wantUp && got.Reachability != cluster.ReachabilityReachable {
					t.Errorf("%s: Reachability = %q, want reachable", got.NodeID, got.Reachability)
				}
				if !wantUp {
					if got.Reachability != cluster.ReachabilityUnreachable {
						t.Errorf("%s: Reachability = %q, want unreachable", got.NodeID, got.Reachability)
					}
					if tc.wantProbeError && got.ProbeError == "" {
						t.Errorf("%s: unreachable with no ProbeError - the reason has to reach the operator", got.NodeID)
					}
					if !tc.wantProbeError && got.ProbeError != "" {
						t.Errorf("%s: ProbeError = %q on a reachable voter", got.NodeID, got.ProbeError)
					}
				}
			}
			if tc.wantDialedAll && len(dialed) != len(voters) {
				t.Errorf("dialed %v, want every voter dialed", dialed)
			}
		})
	}
}

// TestProbeVotersProbeItselfCannotRun is the fail-closed case, and the
// one that matters most: a probe that could not run must report ReadOK
// false, never "zero other voters, therefore nothing to worry about".
func TestProbeVotersProbeItselfCannotRun(t *testing.T) {
	t.Run("an already-cancelled context means the probe never ran", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		dialed := 0
		report := ProbeVoters(ctx, []Voter{{NodeID: "comb-a", RaftBindAddress: "10.0.0.1:19999"}}, ProbeOptions{
			Dial: func(context.Context, string) error {
				dialed++
				return nil
			},
		})
		if report.ReadOK {
			t.Errorf("ReadOK = true for a cancelled context; a probe that never ran must say so")
		}
		if report.Error == "" {
			t.Errorf("Error = %q, want the cancellation reason stated", report.Error)
		}
		if dialed != 0 {
			t.Errorf("dialed %d times under a cancelled context, want 0", dialed)
		}
		if len(report.Voters) != 1 {
			t.Errorf("got %d results, want 1 - the slice must still align with the input", len(report.Voters))
		}
		if report.Voters[0].Reachability != cluster.ReachabilityUnknown {
			t.Errorf("Reachability = %q, want unknown - not probed is not the same as unreachable", report.Voters[0].Reachability)
		}
	})

	t.Run("an empty voter list is a completed probe, not a failed one", func(t *testing.T) {
		report := ProbeVoters(context.Background(), nil, ProbeOptions{})
		if !report.ReadOK {
			t.Errorf("ReadOK = false for an empty voter list; a single-voter cluster legitimately has no other voters")
		}
		if report.Voters == nil {
			t.Errorf("Voters is nil; the contract is a non-nil slice, never nil")
		}
		if len(report.Voters) != 0 {
			t.Errorf("Voters = %v, want empty", report.Voters)
		}
		if got := DescribeProbe(report); got != "no other voters to probe" {
			t.Errorf("DescribeProbe = %q, want %q", got, "no other voters to probe")
		}
	})
}

// TestProbeVotersPerVoterTimeout checks the per-dial budget: a dial that
// hangs is cut off at PerVoterTimeout, and the voter it belonged to is
// reported unreachable rather than leaving the probe hanging forever.
func TestProbeVotersPerVoterTimeout(t *testing.T) {
	opts := DefaultProbeOptions()
	opts.PerVoterTimeout = 20 * time.Millisecond

	var sawDeadline bool
	start := time.Now()
	report := ProbeVoters(context.Background(),
		[]Voter{{NodeID: "comb-a", RaftBindAddress: "10.0.0.1:19999"}},
		ProbeOptions{
			PerVoterTimeout: opts.PerVoterTimeout,
			OverallTimeout:  time.Minute,
			Dial: func(ctx context.Context, _ string) error {
				// Stand in for a dial that honours only its context -
				// the shape of every real net.Dialer-based dial here.
				<-ctx.Done()
				sawDeadline = true
				return ctx.Err()
			},
		})

	if !sawDeadline {
		t.Errorf("the dial was never cut off; a hanging peer must be bounded by PerVoterTimeout")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("ProbeVoters took %s; the per-voter budget was not applied", elapsed)
	}
	if len(report.Voters) != 1 || report.Voters[0].Reachability != cluster.ReachabilityUnreachable {
		t.Errorf("Voters = %+v, want one unreachable entry", report.Voters)
	}
}

// TestProbeVotersOverallDeadline checks the whole-set budget. When it runs
// out, the probe reports ReadOK false AND marks every un-attempted voter
// unknown with a stated reason - the combination that makes this an
// honest "we could not read the cluster" rather than a quiet truncation
// that would look like those voters were simply absent.
func TestProbeVotersOverallDeadline(t *testing.T) {
	voters := []Voter{
		{NodeID: "comb-a", RaftBindAddress: "10.0.0.1:19999"},
		{NodeID: "comb-b", RaftBindAddress: "10.0.0.2:19999"},
		{NodeID: "comb-c", RaftBindAddress: "10.0.0.3:19999"},
	}
	var dials int
	report := ProbeVoters(context.Background(), voters, ProbeOptions{
		PerVoterTimeout: 10 * time.Millisecond,
		OverallTimeout:  25 * time.Millisecond,
		Dial: func(ctx context.Context, _ string) error {
			dials++
			<-ctx.Done()
			return ctx.Err()
		},
	})

	if report.ReadOK {
		t.Errorf("ReadOK = true although the overall deadline was hit mid-probe")
	}
	if !strings.Contains(report.Error, "deadline") {
		t.Errorf("Error = %q, want it to name the deadline", report.Error)
	}
	if len(report.Voters) != len(voters) {
		t.Fatalf("got %d results for %d voters; truncation must still align with the input", len(report.Voters), len(voters))
	}
	// Where the deadline lands depends on the machine, so the tail
	// assertions only apply when it landed before the last voter. The
	// ReadOK and length assertions above hold either way: the dial in
	// this test blocks until its context ends, so the overall budget is
	// always reached somewhere.
	t.Logf("the overall budget was reached after %d of %d dials", dials, len(voters))
	for i, got := range report.Voters {
		if got.NodeID != voters[i].NodeID {
			t.Errorf("result %d is for %q, want %q", i, got.NodeID, voters[i].NodeID)
		}
		if got.Reachability == cluster.ReachabilityReachable {
			t.Errorf("%s reported reachable although it was never dialed", got.NodeID)
		}
		if got.ProbeError == "" {
			t.Errorf("%s has no ProbeError; every non-result needs a stated reason", got.NodeID)
		}
	}
	if dials < len(voters) {
		last := report.Voters[len(report.Voters)-1]
		if last.ProbeError != "not probed: overall probe deadline reached" {
			t.Errorf("the truncated tail carries ProbeError %q, want the not-probed reason", last.ProbeError)
		}
	}
}

// TestProbeVotersMissingAddress covers a voter whose raft_bind_address is
// not known. It lands as unknown with a reason, not as a silent
// unreachable: not having an address is a gap in our own knowledge, not
// an observation about the peer.
func TestProbeVotersMissingAddress(t *testing.T) {
	report := ProbeVoters(context.Background(), []Voter{{NodeID: "comb-a"}}, ProbeOptions{
		Dial: func(context.Context, string) error {
			t.Errorf("a voter with no address must not be dialed")
			return nil
		},
	})
	if len(report.Voters) != 1 {
		t.Fatalf("Voters = %+v, want one entry", report.Voters)
	}
	got := report.Voters[0]
	if got.Reachability != cluster.ReachabilityUnknown {
		t.Errorf("Reachability = %q, want unknown", got.Reachability)
	}
	if !strings.Contains(got.ProbeError, "no raft_bind_address") {
		t.Errorf("ProbeError = %q, want it to explain the missing address", got.ProbeError)
	}
	if !report.ReadOK {
		t.Errorf("ReadOK = false; one unreadable fact is a fact, not a failed probe")
	}
}

// TestProbeOptionsWithDefaults pins the ADR-0125 §7 budgets, and the
// zero-value-is-a-budget-not-a-pause rule: a caller that sets nothing must
// still get 3s/10s, never an unbounded dial.
func TestProbeOptionsWithDefaults(t *testing.T) {
	got := ProbeOptions{}.WithDefaults()
	want := ProbeOptions{PerVoterTimeout: 3 * time.Second, OverallTimeout: 10 * time.Second, Dial: TCPDial}
	if got.PerVoterTimeout != want.PerVoterTimeout {
		t.Errorf("PerVoterTimeout = %v, want %v (ADR-0125 §7)", got.PerVoterTimeout, want.PerVoterTimeout)
	}
	if got.OverallTimeout != want.OverallTimeout {
		t.Errorf("OverallTimeout = %v, want %v (ADR-0125 §7)", got.OverallTimeout, want.OverallTimeout)
	}
	if got.Dial == nil {
		t.Errorf("Dial = nil, want TCPDial")
	}

	custom := ProbeOptions{PerVoterTimeout: time.Second, OverallTimeout: 2 * time.Second, Dial: TCPDial}.WithDefaults()
	if custom.PerVoterTimeout != time.Second || custom.OverallTimeout != 2*time.Second {
		t.Errorf("WithDefaults overwrote a value the caller did set: %+v", custom)
	}
}

// TestProbeVotersIsSafeUnderConcurrentUse guards the -race build: a
// probe is read by the frontend's preflight handler while other handlers
// probe other targets, and nothing in this package may keep shared
// mutable state between calls.
func TestProbeVotersIsSafeUnderConcurrentUse(t *testing.T) {
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			report := ProbeVoters(context.Background(),
				[]Voter{{NodeID: "comb-a", RaftBindAddress: "a:1"}, {NodeID: "comb-b", RaftBindAddress: "b:1"}},
				ProbeOptions{Dial: func(context.Context, string) error { return nil }})
			if len(report.Voters) != 2 {
				t.Errorf("concurrent probe %d returned %d results", i, len(report.Voters))
			}
		}(i)
	}
	wg.Wait()
}

// TestProbeHelpers covers VoterReachabilityMap, SortedUnreachable and
// DescribeProbe, all of which exist to save a caller from re-deriving
// what the probe already established.
func TestProbeHelpers(t *testing.T) {
	report := ProbeVoters(context.Background(), []Voter{
		{NodeID: "comb-z", RaftBindAddress: "z:1"},
		{NodeID: "comb-a", RaftBindAddress: "a:1"},
		{NodeID: "comb-m", RaftBindAddress: "m:1"},
	}, ProbeOptions{Dial: func(_ context.Context, addr string) error {
		if addr == "a:1" {
			return errors.New("connection refused")
		}
		return nil
	}})

	if got := SortedUnreachable(report.Voters); len(got) != 1 || got[0] != "comb-a" {
		t.Errorf("SortedUnreachable = %v, want [comb-a]", got)
	}
	byID := VoterReachabilityMap(report.Voters)
	if len(byID) != 3 {
		t.Errorf("VoterReachabilityMap has %d entries, want 3", len(byID))
	}
	if byID["comb-a"].Reachability != cluster.ReachabilityUnreachable {
		t.Errorf("comb-a mapped to %q, want unreachable", byID["comb-a"].Reachability)
	}
	if byID["comb-m"].Reachability != cluster.ReachabilityReachable {
		t.Errorf("comb-m mapped to %q, want reachable", byID["comb-m"].Reachability)
	}

	desc := DescribeProbe(report)
	for _, want := range []string{"probed 3 other voter(s)", "comb-z reachable", "comb-a unreachable", "connection refused", "comb-m reachable"} {
		if !strings.Contains(desc, want) {
			t.Errorf("DescribeProbe = %q, want it to contain %q", desc, want)
		}
	}
}

func TestDescribeProbeReportsAnUnreadableProbe(t *testing.T) {
	got := DescribeProbe(ProbeReport{ReadOK: false, Error: "context deadline exceeded"})
	if !strings.Contains(got, "could not be read") || !strings.Contains(got, "context deadline exceeded") {
		t.Errorf("DescribeProbe = %q, want it to lead with the unreadable probe and its reason", got)
	}
}

func TestDescribeProbeReportsAnUnverifiedVoterDistinctly(t *testing.T) {
	got := DescribeProbe(ProbeReport{ReadOK: true, Voters: []VoterReachability{
		{NodeID: "comb-a", Reachability: cluster.ReachabilityUnknown, ProbeError: "never dialed"},
	}})
	if !strings.Contains(got, "unverified") {
		t.Errorf("DescribeProbe = %q, want an unverified voter not to be rendered as unreachable or fine", got)
	}
	if strings.Contains(got, "unreachable") {
		t.Errorf("DescribeProbe = %q, must not call an unprobed voter unreachable", got)
	}
}

// TestTCPDialAgainstALiveListener is the one test in this file that
// touches a real socket, and it needs no host and no FreeBSD: it stands
// up a loopback listener, dials it, and checks the primitive both
// succeeds against something listening and fails against a closed port.
func TestTCPDialAgainstALiveListener(t *testing.T) {
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("cannot listen on loopback here: %v", err)
	}
	defer lis.Close()
	go func() {
		for {
			conn, err := lis.Accept()
			if err != nil {
				return
			}
			conn.Close()
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := TCPDial(ctx, lis.Addr().String()); err != nil {
		t.Errorf("TCPDial against a live listener: %v", err)
	}

	// A port nothing is listening on: bind then immediately close, so the
	// port is almost certainly free.
	dead, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("cannot reserve a loopback port here: %v", err)
	}
	addr := dead.Addr().String()
	dead.Close()
	if err := TCPDial(ctx, addr); err == nil {
		t.Errorf("TCPDial against %s (nothing listening) returned nil; a closed port must be an error", addr)
	}
}

func TestTCPDialHonoursACancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	// 203.0.113.0/24 is TEST-NET-3 (RFC 5737) and is not routed, so this
	// cannot accidentally reach a real host even if the cancellation were
	// ignored.
	if err := TCPDial(ctx, "203.0.113.1:19999"); err == nil {
		t.Errorf("TCPDial under a cancelled context returned nil")
	}
}

func TestErrNoDialIsDistinctFromARealDialFailure(t *testing.T) {
	// The sentinel exists so a caller can tell "this build has no dial
	// primitive" from "the peer did not answer". It just has to be a
	// usable, matchable error value.
	wrapped := fmt.Errorf("probing peers: %w", ErrNoDial)
	if !errors.Is(wrapped, ErrNoDial) {
		t.Errorf("ErrNoDial is not matchable through a wrap")
	}
	if errors.Is(errors.New("connection refused"), ErrNoDial) {
		t.Errorf("a real dial failure must not be reported as a missing dial primitive")
	}
}

// TestProbeVotersWithNoDialPrimitiveIsUnreadable is what the sentinel is
// for: a probe that never had a way to dial has learned nothing about the
// cluster, and must say "could not read" rather than manufacture a quorum
// verdict out of its own missing wiring.
func TestProbeVotersWithNoDialPrimitiveIsUnreadable(t *testing.T) {
	report := ProbeVoters(context.Background(),
		[]Voter{{NodeID: "comb-a", RaftBindAddress: "a:1"}, {NodeID: "comb-b", RaftBindAddress: "b:1"}},
		ProbeOptions{Dial: func(context.Context, string) error { return ErrNoDial }})

	if report.ReadOK {
		t.Errorf("ReadOK = true for a probe with no dial primitive")
	}
	if !strings.Contains(report.Error, "no dial function") {
		t.Errorf("Error = %q, want it to name the missing primitive", report.Error)
	}
	if len(report.Voters) != 2 {
		t.Fatalf("got %d results for 2 voters; the slice must still align with the input", len(report.Voters))
	}
	for _, v := range report.Voters {
		if v.Reachability == cluster.ReachabilityReachable || v.Reachability == cluster.ReachabilityUnreachable {
			t.Errorf("%s reported as %q; nothing was dialed, so nothing is known", v.NodeID, v.Reachability)
		}
	}
	if !strings.Contains(DescribeProbe(report), "could not be read") {
		t.Errorf("DescribeProbe does not lead with the unreadable probe")
	}
}
