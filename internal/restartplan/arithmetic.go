package restartplan

import (
	"fmt"
	"sort"

	"github.com/glenjbarber/apiary/internal/cluster"
)

// QuorumImpact is ADR-0125's quorum arithmetic, returned in this
// package's own type so callers get a documented, renderable summary -
// the arithmetic string is part of what an operator is shown, not an
// afterthought.
//
// The counts themselves come straight from
// cluster.ComputeQuorumImpact, ADR-0052's function, which is the single
// implementation of this arithmetic in the codebase: this type adds a
// rendering and a typed field or two, never a second opinion. In
// particular Survives is computed from RemainingReachable only - a voter
// whose reachability was never established is counted as not-up, never
// credited as survival.
type QuorumImpact struct {
	TargetIsVoter      bool
	TotalVoters        uint32
	RemainingVoters    uint32
	RemainingReachable uint32
	RemainingUnknown   uint32
	QuorumSize         uint32
	Survives           bool
	Note               string
	ObservedAtUnix     int64

	// Unreachable lists the other voters that were dialed and did not
	// answer, sorted. A caller that wants to show which specific
	// voters drove the verdict has them here without re-deriving
	// anything.
	Unreachable []string
}

// ComputeQuorum answers "does the cluster still have quorum once
// targetNodeID's raftd is restarted" from a QuorumFact, by translating
// the fact into the exact plain-Go shape
// cluster.ComputeQuorumImpact already consumes and calling it. No
// reachability of the target itself is required: the target is the node
// being restarted, so it contributes to TotalVoters and nothing else.
//
// When fact.ProbeReadOK is false the returned impact is deliberately
// meaningless (zero-valued, Survives false) - there is no evidence to
// compute from, and a zero-valued impact is the most obviously
// unusable thing this function can return. EvaluateQuorumSafety never
// calls it in that state, and its doc comment says why.
func ComputeQuorum(fact QuorumFact) QuorumImpact {
	if !fact.ProbeReadOK {
		return QuorumImpact{}
	}

	// cluster.ComputeQuorumImpact wants the target itself in the
	// server list, with its suffrage, so that TotalVoters is correct;
	// its own doc comment explains that a target absent from the list
	// is reported as "not part of the current raft configuration".
	servers := make([]cluster.ServerSuffrage, 0, len(fact.OtherVoters)+1)
	suffrage := "Nonvoter"
	if fact.IsTargetVoter {
		suffrage = "Voter"
	}
	servers = append(servers, cluster.ServerSuffrage{
		ID:           fact.TargetNodeID,
		Suffrage:     suffrage,
		Reachability: cluster.ReachabilityUnknown, // ignored for the target itself
	})
	for _, v := range fact.OtherVoters {
		reach := v.Reachability
		if reach == "" {
			// A voter with no reachability recorded at all is
			// unknown, never reachable. Defaulting it to Reachable
			// would be the single most dangerous line in this
			// package.
			reach = cluster.ReachabilityUnknown
		}
		servers = append(servers, cluster.ServerSuffrage{ID: v.NodeID, Suffrage: "Voter", Reachability: reach})
	}

	impact := cluster.ComputeQuorumImpact(servers, fact.TargetNodeID)
	out := QuorumImpact{
		TargetIsVoter:      impact.TargetIsVoter,
		TotalVoters:        impact.TotalVoters,
		RemainingVoters:    impact.RemainingVoters,
		RemainingReachable: impact.RemainingReachable,
		RemainingUnknown:   impact.RemainingUnknown,
		QuorumSize:         impact.QuorumSize,
		Survives:           impact.Survives,
		Note:               impact.Note,
	}
	if !fact.ObservedAt.IsZero() {
		out.ObservedAtUnix = fact.ObservedAt.Unix()
	}
	for _, v := range impact.Voters {
		if v.Reachability == cluster.ReachabilityUnreachable {
			out.Unreachable = append(out.Unreachable, v.ID)
		}
	}
	sort.Strings(out.Unreachable)
	return out
}

// Arithmetic renders the counts behind a verdict in one line, in the
// same "total / quorum / reachable" order the ADR states them, so a
// finding's evidence is checkable by hand rather than merely asserted.
func (i QuorumImpact) Arithmetic() string {
	return fmt.Sprintf("%d voter(s) configured, quorum is %d, %d of the remaining %d voter(s) confirmed reachable right now",
		i.TotalVoters, i.QuorumSize, i.RemainingReachable, i.RemainingVoters)
}
