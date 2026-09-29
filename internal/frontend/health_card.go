package frontend

import (
	"fmt"
	"strings"

	"github.com/glenjbarber/apiary/internal/health"
)

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

	// Colony is the three-way state the reachable box above is outlined
	// with. See colonyVerdictFor's own doc comment for the precedence
	// and for why it is not a health calculation of its own.
	Colony healthCardColonyVerdict
}

// healthCardColonyState is the single verdict the "Combs reachable" box
// is outlined with: the Colony's state, in the same green/yellow/red
// treatment the topology card's Colony leader already uses
// (docs/web-ui-redesign.md Section C). Three states, not the seven the
// Stats grid below it shows, because a box outline answers one question -
// "is anything wrong right now" - and a border cannot carry seven.
type healthCardColonyState string

const (
	// colonyStateAllClear: every known Comb is reachable and nothing is
	// outstanding.
	colonyStateAllClear healthCardColonyState = "ok"
	// colonyStateAttention: an outstanding, non-outage issue. Every
	// Comb is up; something about the Colony is still unsettled.
	colonyStateAttention healthCardColonyState = "warn"
	// colonyStateCombDown: at least one known Comb is not reachable.
	colonyStateCombDown healthCardColonyState = "critical"
)

// healthCardColonyVerdict is the outline's state plus the words that say
// the same thing. The leader-Comb outline is deliberately not the only
// cue for what it marks (see cluster_overview.html's .is-leader comment),
// and this project's own rule is that a health verdict is never carried by
// color alone - so the box carries Label in text and Detail in the title.
//
// BadgeClass is carried rather than chosen in the template for the reason
// CauseBadgeClass is: the box and any future second surface must not be
// able to disagree about which state is green.
type healthCardColonyVerdict struct {
	State      healthCardColonyState
	BadgeClass string
	Label      string
	Detail     string
}

// colonyVerdictFor applies the precedence the reachable box is outlined
// by, using only facts the card already carries:
//
//	red     a machine is down (a known Comb that is not reachable)
//	yellow  an outstanding, non-outage issue (anything else outstanding)
//	green   everything is good
//
// Reachability wins unconditionally, with no severity ordering among the
// rest. A Comb that is down is the fact that matters most, and a down
// Comb's own health reading is usually `unknown` as well, so ranking the
// non-reachability facts would only choose which true statement to bury.
//
// Yellow is the honest floor rather than green's: `unknown` and `stale`
// land there, because this project's own rule is that silence is a real
// state with its own verdict and "this Comb was never observed" is not
// the claim "this Comb is healthy". A Colony with no known Combs at all
// is yellow for the same reason - nothing was checked, so nothing was
// found good. A green outline therefore always means every known Comb
// answered with affirmative evidence, which is what makes it worth
// reading at a glance.
//
// This is not a second health calculation. Every input is the same
// counted number the Stats grid above already renders, plus the two
// card-level facts it already shows; nothing here fetches anything.
func colonyVerdictFor(reachable, total int, counts map[string]int, guardrailHolds, pendingJoins int) healthCardColonyVerdict {
	if down := total - reachable; down > 0 {
		return healthCardColonyVerdict{
			State:      colonyStateCombDown,
			BadgeClass: "critical",
			Label:      fmt.Sprintf("%d down", down),
			Detail: fmt.Sprintf("Colony state: %s of %d known Combs did not answer. A Comb that is not reachable is the one fact this outline reserves red for.",
				healthCardCombs(down), total),
		}
	}

	// not-applicable is excluded: it means a resource has no reconciler
	// configured, which is an honest "not checked" rather than a fault,
	// and healthCardVocabFor's own comment already records that a Comb's
	// own health never produces it. Subtracting it from the total rather
	// than summing the other six states keeps an unforeseen key (an empty
	// HealthStatus, say) counted as an issue instead of silently dropped.
	notHealthy := total - counts["ok"] - counts["not-applicable"]
	if notHealthy < 0 {
		notHealthy = 0
	}

	var outstanding []string
	if total == 0 {
		outstanding = append(outstanding, "no Comb was observed, so nothing was checked")
	}
	if notHealthy > 0 {
		outstanding = append(outstanding, fmt.Sprintf("%d of %d Combs report a health verdict other than healthy", notHealthy, total))
	}
	if guardrailHolds > 0 {
		outstanding = append(outstanding, fmt.Sprintf("%d unconfirmed restart guardrail hold%s", guardrailHolds, healthCardPlural(guardrailHolds)))
	}
	if pendingJoins > 0 {
		outstanding = append(outstanding, fmt.Sprintf("%d pending join request%s", pendingJoins, healthCardPlural(pendingJoins)))
	}
	if len(outstanding) == 0 {
		return healthCardColonyVerdict{
			State:      colonyStateAllClear,
			BadgeClass: "ok",
			Label:      "All clear",
			Detail: fmt.Sprintf("Colony state: all %s reachable, every one of them reporting healthy, and nothing outstanding.",
				healthCardCombs(total)),
		}
	}
	return healthCardColonyVerdict{
		State:      colonyStateAttention,
		BadgeClass: "warn",
		Label:      "Needs attention",
		Detail: fmt.Sprintf("Colony state: no Comb is down, but %s. Red is reserved for a Comb that does not answer; this is the outstanding-issue outline.",
			strings.Join(outstanding, "; ")),
	}
}

// healthCardCombs renders n as "1 Comb" or "3 Combs" for the verdict's
// own text.
func healthCardCombs(n int) string {
	return fmt.Sprintf("%d Comb%s", n, healthCardPlural(n))
}

// healthCardPlural returns the suffix an English count of n needs.
func healthCardPlural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
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
		Colony:         colonyVerdictFor(reachable, total, counts, guardrailHolds, pendingJoins),
	}
}

// healthCardVocabFor maps internal/health's own Status values onto the
// HealthCard vocabulary. health.Status has five values, not seven - it
// has no "critical" tier of its own (only "degraded") and no
// "not-applicable" (a Comb's own health is always evaluated; that state
// exists for a resource with no reconciler configured, not for a Comb).
// Those two vocabulary slots are therefore never incremented by a Comb's
// HealthStatus and always render as zero - an honest reflection of what
// this signal can say, not a gap.
func healthCardVocabFor(s health.Status) string {
	if s == health.StatusHealthy {
		return "ok"
	}
	if s == health.StatusDegraded {
		return "warn"
	}
	return string(s) // "unknown" / "stale" / "contradictory" already match
}

// newHealthCardViewFromNodes builds a HealthCard from the same
// []clusterNodeView the Command Center's topology cards already render
// (docs/web-ui-redesign.md Section C) - not a second fetch, so the two
// can never disagree about which Combs are reachable or how many.
// guardrailHolds is passed through rather than computed here, since no
// existing RPC gives this function anywhere to get a real count from -
// see pageData.HealthCard's own comment.
func newHealthCardViewFromNodes(nodes []clusterNodeView, guardrailHolds, pendingJoins int) healthCardView {
	reachable := 0
	counts := make(map[string]int, len(healthCardStateOrder))
	for _, n := range nodes {
		if n.Reachable {
			reachable++
		}
		counts[healthCardVocabFor(n.HealthStatus)]++
	}
	return newHealthCardView(reachable, len(nodes), counts, guardrailHolds, pendingJoins)
}
