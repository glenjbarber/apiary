package recovery

// ValidQuorumFact rejects a structurally-implausible QuorumFact before it
// can reach ClassifyQuorum as a convincing zero value. QuorumSize == 0 is
// the concrete, real trigger this guards against: internal/raft.Node.Status
// silently leaves Servers nil (with no error surfaced anywhere in the
// returned Status struct) if its own GetConfiguration() call errors - a
// pre-existing, already-disclosed gap (ADR-0056) - which would otherwise
// flow through as "0 total voters" and classify as a false QuorumLost,
// indistinguishable from a genuine finding. No functioning raft cluster
// ever has zero voters, so QuorumSize == 0 here means the upstream read
// failed silently, not that quorum was computed.
//
// The remaining checks are defensive consistency checks against a future
// proto/Go struct mismatch, not expected to fire against real data today:
// QuorumSize must equal a majority of TotalVoters; the reachable and
// unknown counts can never together exceed the remaining voter count;
// the remaining voter count can never exceed the total; and since exactly
// one target is ever removed, TotalVoters-RemainingVoters can only ever
// be 0 (the target wasn't a voter) or 1 (it was).
func ValidQuorumFact(f QuorumFact) bool {
	if f.QuorumSize == 0 {
		return false
	}
	if f.QuorumSize != f.TotalVoters/2+1 {
		return false
	}
	if f.RemainingReachable+f.RemainingUnknown > f.RemainingVoters {
		return false
	}
	if f.RemainingVoters > f.TotalVoters {
		return false
	}
	if diff := f.TotalVoters - f.RemainingVoters; diff != 0 && diff != 1 {
		return false
	}
	return true
}

// QuorumVantage names whose vantage point a QuorumFact's reachability
// counts were actually gathered from. It is an input, not something
// ClassifyQuorum can infer: the leader-loss downgrade below is only
// entitled to fire on data the leader itself produced, and a caller that
// dialed each voter itself has data no leader-vantage argument applies
// to. Leaving this out and letting ClassifyQuorum assume the leader's
// vantage is what made internal/frontend report "unknown" on a healthy
// multi-voter colony (see ClassifyQuorumFromVantage).
type QuorumVantage string

const (
	// VantageFromLeader means the reachability counts were gathered by
	// the current raft leader - via leader-only reads (ListVMs/
	// ListJails, ADR-0035) or an RPC that forwards to the leader
	// (SimulateNodeFailure, internal/migration's quorum gate). That
	// proves the leader can reach each remaining voter, but nothing
	// about whether the remaining voters can reach EACH OTHER - the
	// actual precondition for a new election once the leader is gone.
	VantageFromLeader QuorumVantage = "leader"

	// VantageFromNonLeader means the reachability counts were gathered
	// by a node other than the current leader - the caller dialling
	// each voter itself (internal/frontend's own bounded HostStats
	// fan-out does exactly this). No leader-vantage downgrade is
	// inferred from such data; the counts are taken at face value,
	// including the Unknown they produce on their own merits.
	VantageFromNonLeader QuorumVantage = "non-leader"
)

// downgradesToUnknown reports whether this vantage is entitled to
// downgrade a would-be Survives for a target that is the current leader.
// An unrecognized (e.g. zero) vantage is treated as leader-vantage: a
// caller that failed to say where its data came from gets the
// conservative reading, never a stronger verdict than it earned.
func (v QuorumVantage) downgradesToUnknown() bool {
	return v != VantageFromNonLeader
}

// ClassifyQuorumFromVantage is ClassifyQuorum with the vantage point
// made an explicit input, so that data gathered by a node other than
// the leader is not downgraded for a reason that only describes the
// leader's own vantage point.
//
// The downgrade fires only when ALL THREE hold: the target is the
// current leader, the data was gathered from that leader, and the raw
// verdict is not already Lost. A pure count-based Lost verdict is never
// downgraded either way, since that's a voter-count fact independent of
// reachability, not something leader-loss makes any less true.
//
// Callers must check ValidQuorumFact first - this function does not
// re-validate its input.
func ClassifyQuorumFromVantage(f QuorumFact, targetIsLeader bool, vantage QuorumVantage) QuorumVerdict {
	var raw QuorumVerdict
	switch {
	case f.RemainingReachable >= f.QuorumSize:
		raw = QuorumSurvives
	case f.RemainingReachable+f.RemainingUnknown >= f.QuorumSize:
		raw = QuorumUnknown
	default:
		raw = QuorumLost
	}

	if targetIsLeader && vantage.downgradesToUnknown() && raw != QuorumLost {
		return QuorumUnknown
	}
	return raw
}

// ClassifyQuorum is the leader-vantage shorthand for
// ClassifyQuorumFromVantage: it is correct for exactly those callers
// whose reachability data was gathered by the current leader -
// SimulateNodeFailure (leader-only reads, ADR-0035, and the whole
// request is forwarded to the leader on a leader-hint rejection, so its
// per-voter HostStats probes run from the leader) and internal/
// migration's GateOnQuorum. isCurrentLeader there means "the target is
// the current leader, and the data came from it".
//
// It is NOT the general entry point: a caller that gathered the data
// from its own vantage point (internal/frontend dials each voter
// directly) must call ClassifyQuorumFromVantage instead, or it will be
// handed the leader-vantage downgrade its data does not justify.
//
// Answers "does raft still have quorum without the simulated target" as
// a real three-state result, never a bare bool:
//
//   - QuorumLost: even crediting every unknown-reachability voter as
//     reachable, a majority still cannot be reached.
//   - QuorumUnknown: the confirmed-reachable count alone falls short, but
//     the deficit could close if the unknown-reachability voters turn out
//     reachable - a genuinely different, less severe finding than Lost.
//   - QuorumSurvives: the confirmed-reachable count alone already meets
//     quorum.
func ClassifyQuorum(f QuorumFact, isCurrentLeader bool) QuorumVerdict {
	vantage := VantageFromNonLeader
	if isCurrentLeader {
		vantage = VantageFromLeader
	}
	return ClassifyQuorumFromVantage(f, isCurrentLeader, vantage)
}
