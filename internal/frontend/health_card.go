package frontend

// healthCardView and healthCardStat back the HealthCard component
// (docs/web-ui-redesign.md Section 1). It replaces the Command
// Center's former KPI strip - three fixed, always-identical labels
// ("Combs" / "Evidence-backed" / "Preflight") - with real counts, so
// the top of the dashboard answers "is anything wrong right now"
// without reading the topology cards below it.
//
// This file defines the view and the pure function that builds it from
// already-counted numbers. Section A deliberately stops here: pulling
// those numbers from a live ClusterNodeView/ClusterHealth response is
// Section C's job (the dashboard rework), once the same fetch that
// already builds the topology cards is the one place these counts come
// from - duplicating that fetch here would be a second, disagreeing
// source of the same facts.

// healthCardStat is one line of the card: one of the seven vocabulary
// states from the spec's Section 3.1, and how many Combs currently
// carry it. A zero count is shown, not hidden - "0 contradictory" is
// itself something an operator is trusting Apiary to have checked, not
// merely the absence of a warning.
type healthCardStat struct {
	State string
	Label string
	Count int
}

// healthCardView is the HealthCard's full rendered content.
type healthCardView struct {
	ReachableCombs int
	TotalCombs     int
	Stats          []healthCardStat

	// GuardrailHolds is the number of Combs currently holding an
	// unconfirmed restart-guardrail lease (ADR-0125) - a real,
	// actionable "something is mid-restart" fact, not a health verdict.
	GuardrailHolds int

	// PendingJoins is the number of not-yet-resolved join requests
	// (already fetched by the dashboard today as .JoinRequests) -
	// surfaced here because an Admin who never scrolls to the pending-
	// join-requests table below should still learn one exists.
	PendingJoins int
}

// healthCardStateOrder is Section 3.1's seven states, in the fixed
// display order every HealthCard uses - worst-first, so the line an
// operator most needs is never the one they have to scroll to.
var healthCardStateOrder = []struct{ state, label string }{
	{"critical", "Critical"},
	{"contradictory", "Contradictory"},
	{"warn", "Needs attention"},
	{"stale", "Stale"},
	{"unknown", "Unknown"},
	{"not-applicable", "Not applicable"},
	{"ok", "Healthy"},
}

// newHealthCardView builds a HealthCard from already-counted facts.
// counts is keyed by the same state names as healthCardStateOrder;
// a state absent from counts is rendered as zero rather than omitted -
// see healthCardStat's own doc comment for why.
func newHealthCardView(reachable, total int, counts map[string]int, guardrailHolds, pendingJoins int) healthCardView {
	stats := make([]healthCardStat, 0, len(healthCardStateOrder))
	for _, s := range healthCardStateOrder {
		stats = append(stats, healthCardStat{State: s.state, Label: s.label, Count: counts[s.state]})
	}
	return healthCardView{
		ReachableCombs: reachable,
		TotalCombs:     total,
		Stats:          stats,
		GuardrailHolds: guardrailHolds,
		PendingJoins:   pendingJoins,
	}
}
