// Package coverage implements Resilience Coverage Map v1 (ADR-0062,
// CODEX.md's "Resilience Coverage Map"): enumerates meaningful failure
// scenarios already computable by other shipped mechanisms - the
// Dependency Graph Simulator (ADR-0052/0053) and the Operational
// Invariants catalog (ADR-0060) - and classifies each by whether a
// real mechanism exists to evaluate it, never by whether the resulting
// answer happens to be good news.
//
// Like internal/health/internal/recovery/internal/invariant/internal/
// whynot, this package is pure computation with no I/O: internal/
// frontend gathers the raw facts (entirely via RPCs/gathering helpers
// that already exist for other pages) and calls the Classify* functions
// below. This package imports internal/invariant and internal/recovery
// directly (an established pure-package-to-pure-package reuse pattern -
// internal/invariant already imports internal/recovery the same way)
// but never internal/cluster or internal/manager: both pull in heavy
// OS-exec dependencies no pure leaf package here has ever imported.
//
// This is diagnostic only - CODEX's own text says the map should be
// used to "schedule or recommend Continuous Recovery Proof and
// Synthetic Infrastructure Checks, subject to Flight Plans and
// collision detection," but all four of those are separate, unbuilt
// CODEX concepts with zero implementation anywhere in this codebase.
// That whole direction is out of scope for v1.
package coverage

import (
	"sort"
	"strconv"

	"github.com/glenjbarber/apiary/internal/invariant"
	"github.com/glenjbarber/apiary/internal/recovery"
)

// Status is CODEX's own five-word vocabulary for how trustworthy a
// scenario's evidence is. StatusPhysicallyRehearsed and StatusStale are
// permanently unreachable in v1 - no persisted test-execution ledger
// exists anywhere in this codebase to record that a real physical
// drill ever happened, or to date/age any given live check's own
// freshness beyond "gathered just now." Every scenario below is
// computed fresh, at request time, every single page load.
type Status string

const (
	StatusSimulated Status = "simulated"

	// StatusPhysicallyRehearsed is never produced in v1 - see the
	// package doc comment.
	StatusPhysicallyRehearsed Status = "physically_rehearsed"

	// StatusStale is never produced in v1 - see the package doc comment.
	StatusStale Status = "stale"

	// StatusUntested means no mechanism exists that could ever confirm
	// or deny this scenario - either because nothing evaluates it at
	// all, or because the mechanism that runs is structurally incapable
	// of ever resolving anything but Unknown (hast-dual-primary).
	StatusUntested Status = "untested"

	// StatusUnsafeOrImpossible is derived, never a manual judgment: an
	// already-computed live result confirms that a REAL, physical
	// rehearsal of this exact scenario would itself constitute the
	// failure being tested - reserved for hypothetical, not-yet-
	// happened losses (a specific voter or hive that has not actually
	// been lost yet), never for a scenario whose Result merely
	// describes an already-active, present-tense fault.
	StatusUnsafeOrImpossible Status = "unsafe_or_impossible"
)

// statusOrder fixes the five Status values' display/counting order -
// used so Report.Counts and the rendered page always show all five,
// including the permanently-zero ones, rather than silently omitting
// them.
var statusOrder = []Status{StatusUnsafeOrImpossible, StatusSimulated, StatusUntested, StatusPhysicallyRehearsed, StatusStale}

// Tier is a coarse, honestly-labeled consequence/reach grouping.
// Deliberately NOT a numeric weight: CODEX's own "treat coverage as
// evidence, not a vanity percentage" line applies just as much to an
// invented sort-key multiplier as to a displayed score - three tiers
// with no arithmetic inside them avoids implying false precision the
// underlying data doesn't support.
type Tier int

const (
	// TierSingleResource scopes to exactly one resource or network.
	TierSingleResource Tier = iota
	// TierMultiResource scopes to a hive or network whose blast radius
	// varies (how many cells it owns/backs, or has attached) but is
	// never reduced to an invented number.
	TierMultiResource
	// TierClusterWide scopes to the whole cluster's fate.
	TierClusterWide
)

// Scenario is one failure scenario's classification.
type Scenario struct {
	// Kind: "hive-failure" | "network-failure" | "network-connectivity"
	// | "cell-recoverability" | "hast-dual-primary" | "quorum-tolerance"
	Kind, Target, Label string
	Status              Status
	Tier                Tier

	// Result is "true" | "false" | "unknown" - a passthrough of the
	// underlying evaluation, or empty for network-failure (a
	// descriptive blast-radius scenario with no pass/fail verdict).
	Result      string
	Explanation string
}

// Report is the full, deterministic result of one page load.
type Report struct {
	// Scenarios is sorted by Tier (descending), then Status (per
	// statusOrder - unsafe_or_impossible first, most actionable), then
	// Kind, then Target - never by an invented numeric score.
	Scenarios []Scenario

	// Gaps is a small, fixed, hand-maintained list of named failure
	// scenario classes this codebase has literally no mechanism to
	// evaluate for ANY target (see ADR-0062) - never per-target rows,
	// since Apiary has no inventory of every physical uplink or every
	// possible correlated multi-node pair to enumerate targets against.
	// Rendered in a visually separate callout, never interleaved with
	// Scenarios.
	Gaps []string

	// Counts always has an entry for all five Status values, including
	// the permanently-zero StatusPhysicallyRehearsed/StatusStale - made
	// explicit rather than silently absent, since "we have zero
	// physically-rehearsed or stale evidence anywhere" is itself an
	// important, disclosed fact CODEX's own text asks this feature to
	// surface honestly.
	Counts map[Status]int
}

// KnownGaps is the fixed, disclosed list of failure scenario classes
// CODEX's Dependency Graph conceptually includes but this codebase has
// no mechanism to evaluate at all - see ADR-0052/ADR-0053 for the
// scoping citations and ADR-0062 for why each is listed here rather
// than silently omitted.
func KnownGaps() []string {
	return []string{
		"Uplink/physical-NIC loss has no failure-impact simulation, unlike node or network failure - internal/assumecheck already checks uplink configuration correctness live (surfaced on /assumptions), but nothing computes what would actually break if an uplink disappeared.",
		"Correlated or simultaneous multi-node failure is not modeled at all - the Dependency Graph Simulator (ADR-0052) only ever simulates one node disappearing at a time.",
		"No persisted test-execution ledger exists anywhere in this codebase - every scenario on this page is computed fresh at page-load time, never cached or dated from an earlier run. This is why 'physically rehearsed' and 'stale' evidence can never appear here in v1.",
	}
}

func tierOrder(t Tier) int { return int(t) }

func statusRank(s Status) int {
	for i, cand := range statusOrder {
		if cand == s {
			return i
		}
	}
	return len(statusOrder)
}

// BuildReport sorts scenarios deterministically, computes a zero-filled
// Counts breakdown across all five Status values, and attaches the
// fixed KnownGaps list.
func BuildReport(scenarios []Scenario) Report {
	sorted := make([]Scenario, len(scenarios))
	copy(sorted, scenarios)
	sort.SliceStable(sorted, func(i, j int) bool {
		if sorted[i].Tier != sorted[j].Tier {
			return tierOrder(sorted[i].Tier) > tierOrder(sorted[j].Tier)
		}
		if sorted[i].Status != sorted[j].Status {
			return statusRank(sorted[i].Status) < statusRank(sorted[j].Status)
		}
		if sorted[i].Kind != sorted[j].Kind {
			return sorted[i].Kind < sorted[j].Kind
		}
		return sorted[i].Target < sorted[j].Target
	})

	counts := make(map[Status]int, len(statusOrder))
	for _, s := range statusOrder {
		counts[s] = 0
	}
	for _, s := range sorted {
		counts[s.Status]++
	}

	return Report{Scenarios: sorted, Gaps: KnownGaps(), Counts: counts}
}

// ClassifyQuorumTolerance reframes the cluster-wide quorum-tolerance
// invariant as a coverage scenario. Result == false means some voter's
// SIMULATED (not-yet-happened) loss would break quorum - rehearsing
// "which voter is the weak link" for real when this is false would risk
// causing exactly that loss, so it resolves StatusUnsafeOrImpossible;
// otherwise a real mechanism ran and produced a confirmed answer, so it
// resolves StatusSimulated (True or Unknown alike).
func ClassifyQuorumTolerance(e invariant.Evaluation) Scenario {
	status := StatusSimulated
	if e.Result == invariant.ResultFalse {
		status = StatusUnsafeOrImpossible
	}
	return Scenario{
		Kind: "quorum-tolerance", Target: "cluster", Label: "Cluster loses one more raft voter",
		Status: status, Tier: TierClusterWide, Result: string(e.Result), Explanation: e.Explanation,
	}
}

// ClassifyHiveFailure reframes one hive's own hypothetical loss.
// unsafe_or_impossible only when valid is true and verdict is
// recovery.QuorumLost - the same "rehearsing this = causing it" hazard
// ClassifyQuorumTolerance names, at per-node granularity. An invalid
// QuorumFact (valid == false) must never be treated as confirmed bad -
// it resolves StatusSimulated with an "unknown" Result, never a
// fabricated unsafe_or_impossible finding.
func ClassifyHiveFailure(nodeID, label string, verdict recovery.QuorumVerdict, valid bool, ownedCount, replicaBackedCount int) Scenario {
	status := StatusSimulated
	result := "true"
	switch {
	case valid && verdict == recovery.QuorumLost:
		status = StatusUnsafeOrImpossible
		result = "false"
	case !valid || verdict == recovery.QuorumUnknown:
		result = "unknown"
	}
	explanation := "This hive owns " + strconv.Itoa(ownedCount) + " cell(s) and backs " + strconv.Itoa(replicaBackedCount) + " other cell(s) as a HAST replica."
	return Scenario{
		Kind: "hive-failure", Target: nodeID, Label: label,
		Status: status, Tier: TierMultiResource, Result: result, Explanation: explanation,
	}
}

// ClassifyNetworkFailure reframes SimulateNetworkFailure's own
// counterfactual blast-radius report. Always StatusSimulated: unlike
// raft quorum, a managed network's failure has no formal majority-loss
// failure mode that makes rehearsing it for real inherently dangerous -
// losing one managed network affects only the cells attached to it, a
// bounded, recoverable consequence, not an unrecoverable one.
func ClassifyNetworkFailure(networkID, label string, affectedCount int) Scenario {
	explanation := "Losing this network would affect " + strconv.Itoa(affectedCount) + " attached cell(s)."
	return Scenario{
		Kind: "network-failure", Target: networkID, Label: label,
		Status: StatusSimulated, Tier: TierMultiResource, Explanation: explanation,
	}
}

// ClassifyCellRecoverability and ClassifyNetworkConnectivity always
// resolve StatusSimulated regardless of Result: a False Result here
// describes an ALREADY-ACTIVE, present-tense fault (a replica target
// confirmed incapable right now; a bridge confirmed down right now),
// not a hypothetical future loss whose physical rehearsal would be
// dangerous. A confirmed-bad Result is exactly the evidence this
// feature exists to surface, not a testing hazard.
func ClassifyCellRecoverability(e invariant.Evaluation) Scenario {
	return Scenario{
		Kind: "cell-recoverability", Target: e.Scope, Label: "Cell " + e.Scope + " recovers if its owner is lost",
		Status: StatusSimulated, Tier: TierSingleResource, Result: string(e.Result), Explanation: e.Explanation,
	}
}

func ClassifyNetworkConnectivity(e invariant.Evaluation) Scenario {
	return Scenario{
		Kind: "network-connectivity", Target: e.Scope, Label: "Network " + e.Scope + " has a working route",
		Status: StatusSimulated, Tier: TierSingleResource, Result: string(e.Result), Explanation: e.Explanation,
	}
}

// ClassifyHASTDualPrimary always resolves StatusUntested, never
// StatusSimulated: invariant.EvaluateHASTDualPrimary is structurally
// incapable of ever resolving anything but Unknown - no code path
// exists that could ever confirm or deny it. Labeling that "Simulated"
// would imply a mechanism producing real evidence, exactly the false
// impression this feature exists to avoid creating.
func ClassifyHASTDualPrimary(e invariant.Evaluation) Scenario {
	return Scenario{
		Kind: "hast-dual-primary", Target: e.Scope, Label: "Resource " + e.Scope + " never has two writable HAST primaries",
		Status: StatusUntested, Tier: TierSingleResource, Result: string(e.Result), Explanation: e.Explanation,
	}
}
