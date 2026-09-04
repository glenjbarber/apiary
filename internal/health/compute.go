package health

import (
	"fmt"
	"time"
)

// ComputeNodeHealth derives a NodeHealth verdict from s. The precedence
// order below is fixed and every branch is a distinct, separately-tested
// rule - none is a catch-all that silently absorbs a case it wasn't
// designed for. Every branch attaches the Observation(s) that led to its
// verdict; reconciliation Observations are additionally appended
// whenever step 8 is reached, regardless of which of its own sub-cases
// fires, so a reader always sees the full evidence picture, not just the
// single deciding fact.
func ComputeNodeHealth(s NodeSignals, now time.Time) NodeHealth {
	var obs []Observation

	// Step 1: membership itself could not be read at all. This applies
	// identically to every node being evaluated this request, since all
	// of them share the same one anchor membership read - not just a
	// gap in this one node's own row.
	if !s.MembershipObserved {
		obs = append(obs, Observation{
			Source:     "raft_membership",
			ObservedAt: s.MembershipObservedAt,
			Value:      "unobserved",
			Detail:     "raft membership could not be read - the local anchor's own raft is unreachable",
		})
		return NodeHealth{
			NodeID:       s.NodeID,
			Status:       StatusUnknown,
			Explanation:  "raft membership could not be read - the local anchor's own raft is unreachable",
			Observations: obs,
		}
	}

	membershipDetail := "not a raft member"
	if s.IsRaftMember {
		membershipDetail = fmt.Sprintf("raft member, suffrage=%s", s.Suffrage)
	}
	obs = append(obs, Observation{
		Source:     "raft_membership",
		ObservedAt: s.MembershipObservedAt,
		Value:      string(s.Suffrage),
		Detail:     membershipDetail,
	})

	reachDetail := string(s.PeerReachability)
	obs = append(obs, Observation{
		Source:     "peer_reachability",
		ObservedAt: now,
		Value:      string(s.PeerReachability),
		Detail:     "could this node's managerd be dialed and called just now: " + reachDetail,
	})

	// Step 2: CODEX.md's own named example, verbatim - raft membership
	// arithmetic says this is a voter, live observation says it cannot
	// be reached right now.
	if s.IsRaftMember && s.Suffrage == SuffrageVoter && s.PeerReachability == ReachabilityUnreachable {
		explanation := "raft counts this node as a voting member, but it cannot actually be reached right now"
		return NodeHealth{NodeID: s.NodeID, Status: StatusContradictory, Explanation: explanation, Observations: obs}
	}

	// Step 3: raft's own uncertainty about this node's suffrage must
	// never be promoted to Healthy, regardless of reachability.
	if s.IsRaftMember && s.Suffrage == SuffrageUnknown {
		explanation := "raft reports this member's own suffrage as unknown"
		return NodeHealth{NodeID: s.NodeID, Status: StatusUnknown, Explanation: explanation, Observations: obs}
	}

	// Step 4: a confirmed negative is never bucketed with "couldn't
	// check" (step 5) - a definite failure is worse than an unverifiable
	// one, and the two must not look the same to a reader.
	if s.PeerReachability == ReachabilityUnreachable {
		explanation := "this node could not be reached"
		return NodeHealth{NodeID: s.NodeID, Status: StatusDegraded, Explanation: explanation, Observations: obs}
	}

	// Step 5: reachability itself could not be determined (e.g. no peer
	// forwarding configured on the checking node).
	if s.PeerReachability == ReachabilityUnknown {
		explanation := "this node's reachability could not be determined"
		return NodeHealth{NodeID: s.NodeID, Status: StatusUnknown, Explanation: explanation, Observations: obs}
	}

	// From here PeerReachability == Reachable.
	heartbeatDetail := "not observed"
	if s.HeartbeatObserved {
		if s.HeartbeatOK {
			heartbeatDetail = "this node's own raft is reachable"
		} else {
			heartbeatDetail = "this node's own raft is NOT reachable, per its own report"
		}
	}
	obs = append(obs, Observation{
		Source:     "manager_heartbeat",
		ObservedAt: now,
		Value:      fmt.Sprintf("observed=%v ok=%v", s.HeartbeatObserved, s.HeartbeatOK),
		Detail:     heartbeatDetail,
	})

	// Step 6: reachable, but that node's own payload says its local
	// raftd is down - a direct contradiction between "we can talk to
	// this managerd" and "this managerd's own raft companion is dead."
	if s.HeartbeatObserved && !s.HeartbeatOK {
		explanation := "this node is reachable, but its own report says its local raft is unreachable"
		return NodeHealth{NodeID: s.NodeID, Status: StatusContradictory, Explanation: explanation, Observations: obs}
	}

	// Step 7: defensive - reachable per steps 4/5 filtering, but no
	// usable heartbeat payload came back at all.
	if !s.HeartbeatObserved {
		explanation := "this node is reachable, but no usable status payload was returned"
		return NodeHealth{NodeID: s.NodeID, Status: StatusUnknown, Explanation: explanation, Observations: obs}
	}

	if s.AppliedIndexObserved {
		obs = append(obs, Observation{
			Source:     "raft_applied_index",
			ObservedAt: s.IndicesObservedAt,
			Value:      fmt.Sprintf("applied=%d last_log=%d", s.AppliedIndex, s.LastLogIndex),
			Detail:     "last_log_index includes uncommitted entries - not used to derive this verdict, cited for reference only",
		})
	}

	// Step 8: reconciliation, reached only once raft signals are clean.
	return computeReconciliationHealth(s, now, obs)
}

func computeReconciliationHealth(s NodeSignals, now time.Time, obs []Observation) NodeHealth {
	if !s.ReconcilerConfigured {
		obs = append(obs, Observation{
			Source: "reconciler_last_success",
			Value:  "not_applicable",
			Detail: "this node has no Reconciler configured",
		})
		return NodeHealth{
			NodeID:       s.NodeID,
			Status:       StatusHealthy,
			Explanation:  "reachable, raft membership consistent, no reconciler configured on this node",
			Observations: obs,
		}
	}

	freshnessLimit := time.Duration(reconcileFreshnessMultiplier) * time.Duration(s.ReconcileIntervalSeconds) * time.Second

	if s.ReconcileEverSucceeded && now.Sub(s.LastReconcileSuccess) <= freshnessLimit {
		obs = append(obs, Observation{
			Source:         "reconciler_last_success",
			ObservedAt:     s.LastReconcileSuccess,
			FreshnessLimit: freshnessLimit,
			Value:          "succeeded",
			Detail:         "last reconcile tick fully succeeded",
		})
		return NodeHealth{
			NodeID:       s.NodeID,
			Status:       StatusHealthy,
			Explanation:  "reachable, raft membership consistent, reconciliation recently succeeded",
			Observations: obs,
		}
	}

	if s.ReconcileEverAttempted && now.Sub(s.LastReconcileAttempt) <= freshnessLimit {
		obs = append(obs, Observation{
			Source:         "reconciler_last_success",
			ObservedAt:     s.LastReconcileAttempt,
			FreshnessLimit: freshnessLimit,
			Value:          "attempting",
			Detail:         "reconciliation is being attempted but has not recently succeeded",
		})
		return NodeHealth{
			NodeID:       s.NodeID,
			Status:       StatusDegraded,
			Explanation:  "reconciliation is being attempted but has not recently succeeded",
			Observations: obs,
		}
	}

	obs = append(obs, Observation{
		Source:         "reconciler_last_success",
		ObservedAt:     s.ReconcileObservedAt,
		FreshnessLimit: freshnessLimit,
		Value:          "stale",
		Detail:         "no recent reconciliation attempt or success has been observed",
	})
	return NodeHealth{
		NodeID:       s.NodeID,
		Status:       StatusStale,
		Explanation:  "no recent reconciliation attempt or success has been observed",
		Observations: obs,
	}
}
