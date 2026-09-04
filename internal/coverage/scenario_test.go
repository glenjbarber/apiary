package coverage

import (
	"testing"

	"github.com/glenjbarber/apiary/internal/invariant"
	"github.com/glenjbarber/apiary/internal/recovery"
)

func TestClassifyQuorumTolerance_UnsafeOrImpossibleOnlyWhenResultFalse(t *testing.T) {
	false_ := ClassifyQuorumTolerance(invariant.Evaluation{Result: invariant.ResultFalse})
	if false_.Status != StatusUnsafeOrImpossible {
		t.Errorf("Result=false: Status = %v, want StatusUnsafeOrImpossible", false_.Status)
	}
	for _, r := range []invariant.Result{invariant.ResultTrue, invariant.ResultUnknown} {
		got := ClassifyQuorumTolerance(invariant.Evaluation{Result: r})
		if got.Status != StatusSimulated {
			t.Errorf("Result=%v: Status = %v, want StatusSimulated", r, got.Status)
		}
	}
}

func TestClassifyCellRecoverability_NeverUnsafeOrImpossibleEvenWhenResultFalse(t *testing.T) {
	// A false cell-recoverability Result describes an ALREADY-ACTIVE
	// fault (a confirmed-incapable replica target), not a hypothetical
	// future loss - rehearsing it would be meaningless, not dangerous.
	for _, r := range []invariant.Result{invariant.ResultTrue, invariant.ResultFalse, invariant.ResultUnknown} {
		got := ClassifyCellRecoverability(invariant.Evaluation{Result: r, Scope: "vm-1"})
		if got.Status != StatusSimulated {
			t.Errorf("Result=%v: Status = %v, want StatusSimulated (never a testing hazard)", r, got.Status)
		}
	}
}

func TestClassifyNetworkConnectivity_NeverUnsafeOrImpossibleEvenWhenResultFalse(t *testing.T) {
	for _, r := range []invariant.Result{invariant.ResultTrue, invariant.ResultFalse, invariant.ResultUnknown} {
		got := ClassifyNetworkConnectivity(invariant.Evaluation{Result: r, Scope: "net-1"})
		if got.Status != StatusSimulated {
			t.Errorf("Result=%v: Status = %v, want StatusSimulated (never a testing hazard)", r, got.Status)
		}
	}
}

func TestClassifyHASTDualPrimary_AlwaysUntestedNeverSimulated(t *testing.T) {
	got := ClassifyHASTDualPrimary(invariant.Evaluation{Result: invariant.ResultUnknown, Scope: "vm-1"})
	if got.Status != StatusUntested {
		t.Fatalf("Status = %v, want StatusUntested (a mechanism that can never resolve is not real evidence)", got.Status)
	}
}

func TestClassifyHiveFailure_UnsafeOrImpossibleOnlyWhenQuorumLost(t *testing.T) {
	lost := ClassifyHiveFailure("node-a", "Hive node-a fails", recovery.QuorumLost, true, 2, 1)
	if lost.Status != StatusUnsafeOrImpossible {
		t.Errorf("Lost: Status = %v, want StatusUnsafeOrImpossible", lost.Status)
	}
	survives := ClassifyHiveFailure("node-a", "Hive node-a fails", recovery.QuorumSurvives, true, 2, 1)
	if survives.Status != StatusSimulated {
		t.Errorf("Survives: Status = %v, want StatusSimulated", survives.Status)
	}
	unknown := ClassifyHiveFailure("node-a", "Hive node-a fails", recovery.QuorumUnknown, true, 2, 1)
	if unknown.Status != StatusSimulated || unknown.Result != "unknown" {
		t.Errorf("Unknown: Status/Result = %v/%v, want StatusSimulated/unknown", unknown.Status, unknown.Result)
	}
}

func TestClassifyHiveFailure_InvalidNeverFabricatesUnsafeOrImpossible(t *testing.T) {
	// valid=false means the underlying QuorumFact was internally
	// inconsistent - must never be treated as a confirmed-bad finding,
	// even if the (meaningless) Verdict value happens to be QuorumLost.
	got := ClassifyHiveFailure("node-a", "Hive node-a fails", recovery.QuorumLost, false, 0, 0)
	if got.Status != StatusSimulated || got.Result != "unknown" {
		t.Fatalf("invalid fact: Status/Result = %v/%v, want StatusSimulated/unknown - never fabricate a finding", got.Status, got.Result)
	}
}

func TestClassifyNetworkFailure_AlwaysSimulated(t *testing.T) {
	got := ClassifyNetworkFailure("net-1", "Network net-1 fails", 5)
	if got.Status != StatusSimulated {
		t.Fatalf("Status = %v, want StatusSimulated - no unsafe-to-rehearse equivalent exists for a managed network's failure", got.Status)
	}
	if got.Tier != TierMultiResource {
		t.Errorf("Tier = %v, want TierMultiResource", got.Tier)
	}
}

func TestBuildReport_SortsByTierThenStatusThenKindThenTarget(t *testing.T) {
	scenarios := []Scenario{
		{Kind: "cell-recoverability", Target: "vm-2", Tier: TierSingleResource, Status: StatusSimulated},
		{Kind: "quorum-tolerance", Target: "cluster", Tier: TierClusterWide, Status: StatusSimulated},
		{Kind: "hive-failure", Target: "node-a", Tier: TierMultiResource, Status: StatusUnsafeOrImpossible},
		{Kind: "cell-recoverability", Target: "vm-1", Tier: TierSingleResource, Status: StatusSimulated},
		{Kind: "hast-dual-primary", Target: "vm-1", Tier: TierSingleResource, Status: StatusUntested},
	}
	report := BuildReport(scenarios)

	want := []string{
		"quorum-tolerance:cluster", // TierClusterWide first
		"hive-failure:node-a",      // TierMultiResource next
		"cell-recoverability:vm-1", // TierSingleResource, unsafe/simulated before untested, then target order
		"cell-recoverability:vm-2",
		"hast-dual-primary:vm-1",
	}
	if len(report.Scenarios) != len(want) {
		t.Fatalf("len(Scenarios) = %d, want %d", len(report.Scenarios), len(want))
	}
	for i, s := range report.Scenarios {
		got := s.Kind + ":" + s.Target
		if got != want[i] {
			t.Errorf("Scenarios[%d] = %q, want %q", i, got, want[i])
		}
	}
}

func TestBuildReport_CountsIncludeAllFiveStatusesZeroFilled(t *testing.T) {
	report := BuildReport([]Scenario{{Status: StatusSimulated}})
	for _, s := range []Status{StatusSimulated, StatusPhysicallyRehearsed, StatusStale, StatusUntested, StatusUnsafeOrImpossible} {
		if _, ok := report.Counts[s]; !ok {
			t.Errorf("Counts missing entry for %v", s)
		}
	}
	if report.Counts[StatusPhysicallyRehearsed] != 0 || report.Counts[StatusStale] != 0 {
		t.Errorf("Counts = %+v, want physically_rehearsed and stale to be explicitly zero", report.Counts)
	}
	if report.Counts[StatusSimulated] != 1 {
		t.Errorf("Counts[simulated] = %d, want 1", report.Counts[StatusSimulated])
	}
}

func TestBuildReport_GapsAlwaysAttached(t *testing.T) {
	report := BuildReport(nil)
	if len(report.Gaps) == 0 {
		t.Fatal("expected the fixed KnownGaps list to always be attached, even with zero scenarios")
	}
}
