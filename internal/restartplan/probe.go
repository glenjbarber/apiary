package restartplan

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sort"
	"strings"
	"time"

	"github.com/glenjbarber/apiary/internal/cluster"
)

// Voter is one raft voter as the probe needs to see it: an identity and
// the raft_bind_address to dial. Deliberately a plain struct rather than
// internalpb.ServerInfo - this package is pure Go over already-gathered
// facts (internal/cluster's own convention, and the reason
// internal/cluster/simulate.go has no proto import), so the RPC handler
// does the proto translation at its own boundary.
type Voter struct {
	NodeID          string
	RaftBindAddress string
}

// DialFunc is the injectable network primitive: given a context and an
// address, either return nil (something answered) or an error describing
// why not. Nothing else about a probe is injectable, so a test that
// wants a different reachability story supplies a different DialFunc
// and nothing else.
//
// The production implementation is TCP dial to the peer's
// raft_bind_address: the same primitive, with the same intent, that
// internal/manager's own dialReachable uses for ADR-0097's join
// reachability check (a plain connect, not a raft handshake - it
// cannot catch every misconfiguration, but it does catch "the peer's
// raftd is not listening", which is the exact failure this project has
// been bitten by more than once).
type DialFunc func(ctx context.Context, address string) error

// TCPDial is the production DialFunc.
func TCPDial(ctx context.Context, address string) error {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", address)
	if err != nil {
		return err
	}
	return conn.Close()
}

// Probe is the injectable form of ADR-0125 §3's probeRaftdVoters: it
// returns one reachability result per voter it was given, and a
// ReadOK that reports whether the probe could be read at all.
type Prober interface {
	Probe(ctx context.Context, voters []Voter) ProbeReport
}

// ProbeReport is one probe's whole result. ReadOK is the distinction
// ADR-0125 §3 draws between "the probe itself could not run" (Unknown,
// fail closed) and "individual voters did not answer" (those voters
// count as down, the rest of the probe still stands).
type ProbeReport struct {
	ReadOK  bool
	Error   string
	Voters  []VoterReachability
	Elapsed time.Duration
}

// ProberFunc adapts a plain function to Prober.
type ProberFunc func(ctx context.Context, voters []Voter) ProbeReport

// Probe implements Prober.
func (f ProberFunc) Probe(ctx context.Context, voters []Voter) ProbeReport { return f(ctx, voters) }

// ProbeOptions are the tunables of ProbeVoters. The zero value of each
// field is not "no timeout" but DefaultProbeOptions' value, so a caller
// that sets none of them still gets ADR-0125 §7's documented budgets
// rather than an unbounded dial.
type ProbeOptions struct {
	// PerVoterTimeout bounds one dial. ADR-0125 §7: 3s, matching the
	// reachabilityCheckTimeout used elsewhere in this codebase - a
	// healthy LAN peer answers in well under 100ms, so 3s is
	// generous, not marginal.
	PerVoterTimeout time.Duration

	// OverallTimeout bounds the whole probe set, so a cluster with
	// many voters cannot turn a preflight into a multi-minute wait.
	// ADR-0125 §7: 10s, sized for the worst sequential case.
	OverallTimeout time.Duration

	// Dial is the network primitive (see DialFunc). nil means
	// TCPDial.
	Dial DialFunc
}

// DefaultProbeOptions is ADR-0125 §7's per-voter/full-probe budget.
func DefaultProbeOptions() ProbeOptions {
	return ProbeOptions{
		PerVoterTimeout: 3 * time.Second,
		OverallTimeout:  10 * time.Second,
		Dial:            TCPDial,
	}
}

// WithDefaults fills in any unset field from DefaultProbeOptions,
// leaving what the caller did set alone.
func (o ProbeOptions) WithDefaults() ProbeOptions {
	d := DefaultProbeOptions()
	if o.PerVoterTimeout <= 0 {
		o.PerVoterTimeout = d.PerVoterTimeout
	}
	if o.OverallTimeout <= 0 {
		o.OverallTimeout = d.OverallTimeout
	}
	if o.Dial == nil {
		o.Dial = d.Dial
	}
	return o
}

// ProbeVoters dials every given voter's raft_bind_address and reports
// the result, one entry per input voter, in input order.
//
// Three properties are load-bearing and each is tested:
//
//   - The returned slice is never nil and always the same length as the
//     input. A caller that zips a probe result against its own voter
//     list must never be handed a short slice and silently treat the
//     missing tail as "not probed".
//   - One voter's dial failure never aborts the rest. A peer that
//     refuses a connection is information about that peer, not about
//     the probe.
//   - A probe that could not run at all (context already cancelled, the
//     overall budget expiring, no usable addresses) reports ReadOK false
//     rather than an empty or quietly-truncated set that would read as
//     "no other voters, therefore safe". An overall-budget expiry part
//     way through is reported the same way, and the voters it cut off
//     are unprobed rather than unreachable.
//
// No retry per voter: ADR-0125 §3 is explicit that a single dial attempt
// is the signal, and an operator who fixed the network re-runs
// preflight.
func ProbeVoters(ctx context.Context, voters []Voter, opts ProbeOptions) ProbeReport {
	opts = opts.WithDefaults()
	start := time.Now()

	report := ProbeReport{ReadOK: true, Voters: make([]VoterReachability, 0, len(voters))}

	overallCtx, cancelOverall := context.WithTimeout(ctx, opts.OverallTimeout)
	defer cancelOverall()

	for _, v := range voters {
		entry := VoterReachability{NodeID: v.NodeID, RaftBindAddress: v.RaftBindAddress}
		if v.RaftBindAddress == "" {
			// No address to dial is not evidence the peer is up, and
			// not evidence the probe ran properly either: it is one
			// unreadable fact, so it lands as unknown with the reason
			// stated rather than as a silent "unreachable".
			entry.Reachability = cluster.ReachabilityUnknown
			entry.ProbeError = "no raft_bind_address known for this voter"
			report.Voters = append(report.Voters, entry)
			continue
		}
		if err := overallCtx.Err(); err != nil {
			// The budget ran out before this voter was even
			// attempted: that is the probe failing to read, not a
			// verdict about the remaining voters. The current voter
			// is included in the not-probed tail - dropping it would
			// hand the caller a shorter slice than it passed in, and
			// the missing entry would read exactly like "this voter is
			// not in the cluster".
			report.ReadOK = false
			report.Error = "probe deadline reached before all voters were dialed: " + err.Error()
			for _, rest := range votersFrom(voters, v.NodeID) {
				report.Voters = append(report.Voters, VoterReachability{
					NodeID: rest.NodeID, RaftBindAddress: rest.RaftBindAddress,
					Reachability: cluster.ReachabilityUnknown,
					ProbeError:   "not probed: overall probe deadline reached",
				})
			}
			report.Elapsed = time.Since(start)
			return report
		}

		dialCtx, cancel := context.WithTimeout(overallCtx, opts.PerVoterTimeout)
		err := opts.Dial(dialCtx, v.RaftBindAddress)
		cancel()
		if err == nil {
			entry.Reachability = cluster.ReachabilityReachable
			report.Voters = append(report.Voters, entry)
			continue
		}
		if errors.Is(err, ErrNoDial) {
			// A build (or a deliberately disabled probe) with no network
			// primitive has not learned anything about this peer, let
			// alone about the cluster. Reporting "unreachable" would
			// manufacture a quorum verdict out of our own missing
			// wiring, so the whole probe is marked unreadable instead and
			// every voter is left unprobed.
			report.ReadOK = false
			report.Error = "no dial function is available, so no peer reachability could be established: " + err.Error()
			for _, rest := range votersFrom(voters, v.NodeID) {
				report.Voters = append(report.Voters, VoterReachability{
					NodeID: rest.NodeID, RaftBindAddress: rest.RaftBindAddress,
					Reachability: cluster.ReachabilityUnknown,
					ProbeError:   "not probed: no dial function is available",
				})
			}
			report.Elapsed = time.Since(start)
			return report
		}
		if overallCtx.Err() != nil {
			// The dial failed because the *overall* budget ran out
			// mid-dial, not because the peer refused. Recording that as
			// "unreachable" would blame a healthy peer for this probe's
			// own impatience, and would then feed a fabricated "down"
			// into the quorum arithmetic - the exact kind of quiet wrong
			// answer this package exists to avoid. The whole probe is
			// therefore marked unreadable, and this voter and the rest
			// are reported as unprobed.
			report.ReadOK = false
			report.Error = "probe deadline reached while dialing " + v.RaftBindAddress + ": " + err.Error()
			for _, rest := range votersFrom(voters, v.NodeID) {
				report.Voters = append(report.Voters, VoterReachability{
					NodeID: rest.NodeID, RaftBindAddress: rest.RaftBindAddress,
					Reachability: cluster.ReachabilityUnknown,
					ProbeError:   "not probed: overall probe deadline reached",
				})
			}
			report.Elapsed = time.Since(start)
			return report
		}
		// A genuine per-voter failure: this peer, on its own terms.
		entry.Reachability = cluster.ReachabilityUnreachable
		entry.ProbeError = err.Error()
		report.Voters = append(report.Voters, entry)
	}

	if err := ctx.Err(); err != nil {
		// The caller's own context ended: this probe was cut short,
		// so it is not a complete reading of the cluster.
		report.ReadOK = false
		if report.Error == "" {
			report.Error = "probe cancelled: " + err.Error()
		}
	}
	report.Elapsed = time.Since(start)
	return report
}

// votersFrom returns the named voter and every voter after it,
// preserving order. It is inclusive of the named voter on purpose: the
// only caller uses it to report a not-probed tail that begins at a voter
// that itself never got dialed, and an exclusive slice would drop
// exactly the one entry the caller is trying to explain.
//
// A duplicated node id truncates the list - a cluster with two servers
// sharing an id is already broken beyond anything this package could
// interpret, and dropping entries would be a silent wrong answer where a
// shorter, obviously-degenerate one is not.
func votersFrom(voters []Voter, fromNodeID string) []Voter {
	seen := false
	var out []Voter
	for _, v := range voters {
		if !seen {
			if v.NodeID == fromNodeID {
				seen = true
			} else {
				continue
			}
		}
		out = append(out, v)
	}
	if !seen {
		return nil
	}
	return out
}

// VoterReachabilityMap indexes probe results by node id for callers
// that would otherwise re-scan a slice per lookup. Sorted output is not
// implied - a map has no order, and a caller that needs one should
// keep the slice.
func VoterReachabilityMap(vs []VoterReachability) map[string]VoterReachability {
	m := make(map[string]VoterReachability, len(vs))
	for _, v := range vs {
		m[v.NodeID] = v
	}
	return m
}

// SortedUnreachable returns the node ids of every voter that was dialed
// and did not answer, sorted - the shape an operator-facing message
// wants.
func SortedUnreachable(vs []VoterReachability) []string {
	var out []string
	for _, v := range vs {
		if v.Reachability == cluster.ReachabilityUnreachable {
			out = append(out, v.NodeID)
		}
	}
	sort.Strings(out)
	return out
}

// ErrNoDial is the sentinel a DialFunc returns to say "there is no
// network primitive here" - a build or a deliberately disabled probe
// rather than a peer that failed to answer.
//
// It is a distinct value, not a matter of wording, because the two mean
// opposite things: a real dial failure is an observation about one peer
// (unreachable, and the rest of the probe still stands), while ErrNoDial
// means nothing at all was learned, so ProbeVoters reports ReadOK false
// and the verdict becomes Unknown rather than a quorum arithmetic built
// on the absence of wiring.
var ErrNoDial = errors.New("restartplan: no dial function configured")

// DescribeProbe renders a probe report for a log line or a finding
// detail, always naming every voter it covered and never collapsing an
// unread result into "fine".
func DescribeProbe(report ProbeReport) string {
	if !report.ReadOK {
		return "probe could not be read: " + report.Error
	}
	if len(report.Voters) == 0 {
		return "no other voters to probe"
	}
	parts := make([]string, 0, len(report.Voters))
	for _, v := range report.Voters {
		switch v.Reachability {
		case cluster.ReachabilityReachable:
			parts = append(parts, v.NodeID+" reachable")
		case cluster.ReachabilityUnreachable:
			parts = append(parts, fmt.Sprintf("%s unreachable (%s)", v.NodeID, v.ProbeError))
		default:
			parts = append(parts, fmt.Sprintf("%s unverified (%s)", v.NodeID, v.ProbeError))
		}
	}
	return "probed " + fmt.Sprint(len(report.Voters)) + " other voter(s): " + strings.Join(parts, ", ")
}
