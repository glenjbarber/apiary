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

// ClassifyQuorum answers "does raft still have quorum without the
// simulated target" as a real three-state result, never a bare bool:
//
//   - QuorumLost: even crediting every unknown-reachability voter as
//     reachable, a majority still cannot be reached.
//   - QuorumUnknown: the confirmed-reachable count alone falls short, but
//     the deficit could close if the unknown-reachability voters turn out
//     reachable - a genuinely different, less severe finding than Lost.
//   - QuorumSurvives: the confirmed-reachable count alone already meets
//     quorum.
//
// When isCurrentLeader, any non-Lost raw verdict is downgraded to
// Unknown: the underlying reachability data was gathered via calls
// answered by the current leader (ListVMs/ListJails are leader-only,
// ADR-0035, and SimulateNodeFailure's own reachability probes are run
// from whichever node answers the RPC - always the leader once
// forwarded). That proves the leader can reach each remaining voter, but
// nothing about whether the remaining voters can reach EACH OTHER - the
// actual precondition for a new election once the leader itself is gone.
// A pure count-based Lost verdict is never downgraded, since that's a
// voter-count fact independent of reachability, not something leader-loss
// makes any less true.
//
// Callers must check ValidQuorumFact first - this function does not
// re-validate its input.
func ClassifyQuorum(f QuorumFact, isCurrentLeader bool) QuorumVerdict {
	var raw QuorumVerdict
	switch {
	case f.RemainingReachable >= f.QuorumSize:
		raw = QuorumSurvives
	case f.RemainingReachable+f.RemainingUnknown >= f.QuorumSize:
		raw = QuorumUnknown
	default:
		raw = QuorumLost
	}

	if isCurrentLeader && raw != QuorumLost {
		return QuorumUnknown
	}
	return raw
}
