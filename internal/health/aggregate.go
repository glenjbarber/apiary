package health

// AggregateStatus reduces a set of node verdicts to one verdict for the
// whole.
//
// It is the SAME rule the Command Center's health tree applies
// (internal/frontend's aggregateHealthState), moved here so that there
// is exactly one implementation of it.
//
// That matters because a monitoring report and the health page must not
// disagree about the Colony. Two implementations of the same aggregation
// would eventually drift, and a monitoring system that contradicts the
// UI it is supposed to mirror is worse than one that has no opinion.
//
// The rule, in the order it is applied:
//
//   - DEGRADED if any node is degraded or contradictory. A contradicted
//     node is a real, serious condition - the Combs disagree about
//     their own state - and it is reported as degraded rather than
//     given a state of its own, because the aggregation vocabulary is
//     the five-state one and "contradictory" means exactly that.
//
//   - STALE if any node is stale. A stale observation is not a healthy
//     one, and reporting the Colony as healthy because the worst thing
//     about it is merely old is exactly the failure this vocabulary
//     exists to prevent.
//
//   - UNKNOWN if any node is unknown, or if any node is UNAVAILABLE.
//     An unavailable node is folded into unknown here, deliberately: a
//     node that could not be reached has no verdict, and a Colony
//     containing one cannot be called healthy.
//
//   - HEALTHY otherwise.
//
// An empty set is UNKNOWN, never healthy. "No nodes" is not "all nodes
// fine", and a monitoring report that says HEALTHY about a Colony it
// could not see is the worst answer it could give.
//
// The five states are the same ones internal/health computes per node;
// the vocabulary is shared, and this function only reduces over it.
func AggregateStatus(statuses []Status) Status {
	if len(statuses) == 0 {
		return StatusUnknown
	}
	degraded, stale, unknown := false, false, false
	for _, status := range statuses {
		switch status {
		case StatusDegraded, StatusContradictory:
			degraded = true
		case StatusStale:
			stale = true
		case StatusUnknown:
			unknown = true
		case StatusHealthy:
			// Recognized and fine.
		default:
			// A status this build does not recognize - one a newer
			// managerd computed - is NOT healthy. It is unknown,
			// because a verdict this code cannot interpret is
			// indistinguishable from no verdict at all.
			//
			// Falling through to the healthy arm here is the exact
			// failure Evidence-Aware Health exists to prevent: a
			// switch statement's default arm reading as a pass.
			unknown = true
		}
	}
	switch {
	case degraded:
		return StatusDegraded
	case stale:
		return StatusStale
	case unknown:
		return StatusUnknown
	default:
		return StatusHealthy
	}
}
