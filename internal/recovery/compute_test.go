package recovery

import (
	"strings"
	"testing"
)

// survivingQuorum classifies as QuorumSurvives for a non-leader target -
// used by tests below that don't care about quorum wording themselves,
// only about the steps built on top of it.
var survivingQuorum = QuorumFact{TotalVoters: 3, RemainingVoters: 2, RemainingReachable: 2, RemainingUnknown: 0, QuorumSize: 2}

var lostQuorum = QuorumFact{TotalVoters: 5, RemainingVoters: 4, RemainingReachable: 1, RemainingUnknown: 0, QuorumSize: 3}

func stepTitled(steps []Step, substr string) *Step {
	for i := range steps {
		if strings.Contains(steps[i].Title, substr) {
			return &steps[i]
		}
	}
	return nil
}

func TestBuildHandbook_QuorumStepAlwaysFirst(t *testing.T) {
	hb := BuildHandbook(Inputs{TargetNodeID: "node-a", Quorum: survivingQuorum})
	if len(hb.Steps) == 0 || hb.Steps[0].Order != 1 {
		t.Fatalf("Steps = %+v, want a first step with Order 1", hb.Steps)
	}
	if hb.QuorumVerdict != QuorumSurvives {
		t.Errorf("QuorumVerdict = %v, want Survives", hb.QuorumVerdict)
	}
}

func TestBuildHandbook_ReplicaConfiguredProducesIrreversibleMigrationStep(t *testing.T) {
	hb := BuildHandbook(Inputs{
		TargetNodeID: "node-a",
		Quorum:       survivingQuorum,
		OwnedResources: []ResourceFact{
			{ID: "abc123", Name: "web-1", Kind: "vm", ReplicaNodeID: "node-b", ReplicaConfigured: true},
		},
	})
	step := stepTitled(hb.Steps, "web-1")
	if step == nil {
		t.Fatalf("no step found for web-1, got: %+v", hb.Steps)
	}
	if !step.Irreversible {
		t.Error("migration step Irreversible = false, want true")
	}
	if step.StopCondition == "" {
		t.Error("migration step StopCondition is empty, want a real stop condition")
	}
	if !strings.Contains(step.Detail, "MigrateVM") {
		t.Errorf("migration step Detail = %q, want it to reference MigrateVM", step.Detail)
	}
	if !strings.Contains(step.Detail, "never a direct `hastctl role primary`") {
		t.Errorf("migration step Detail = %q, must explicitly forbid a raw hastctl role primary call", step.Detail)
	}
	if !strings.Contains(step.Detail, "vm-abc123") {
		t.Errorf("migration step Detail = %q, want the real HAST resource name vm-abc123, not the display name", step.Detail)
	}
	if !strings.Contains(step.Detail, "power-off") {
		t.Errorf("migration step Detail = %q, want enumerated fencing evidence (power-off)", step.Detail)
	}
	if !strings.Contains(step.Detail, "failed ping") {
		t.Errorf("migration step Detail = %q, want unreachable-from-report explicitly named as insufficient", step.Detail)
	}
}

func TestBuildHandbook_ReplicaConfiguredFalseProducesUnprotectedStep(t *testing.T) {
	hb := BuildHandbook(Inputs{
		TargetNodeID: "node-a",
		Quorum:       survivingQuorum,
		OwnedResources: []ResourceFact{
			{ID: "abc123", Name: "web-1", Kind: "vm", ReplicaConfigured: false},
		},
	})
	step := stepTitled(hb.Steps, "web-1")
	if step == nil {
		t.Fatalf("no step found for web-1, got: %+v", hb.Steps)
	}
	if step.Irreversible {
		t.Error("unprotected step Irreversible = true, want false - nothing to undo")
	}
	if strings.Contains(step.Detail, "MigrateVM") {
		t.Errorf("unprotected step Detail = %q, must not reference MigrateVM", step.Detail)
	}
}

func TestBuildHandbook_MigrationStepRefusesUnlessQuorumSurvives(t *testing.T) {
	hb := BuildHandbook(Inputs{
		TargetNodeID: "node-a",
		Quorum:       lostQuorum,
		OwnedResources: []ResourceFact{
			{ID: "abc123", Name: "web-1", Kind: "vm", ReplicaNodeID: "node-b", ReplicaConfigured: true},
		},
	})
	if hb.QuorumVerdict != QuorumLost {
		t.Fatalf("QuorumVerdict = %v, want Lost", hb.QuorumVerdict)
	}
	step := stepTitled(hb.Steps, "web-1")
	if step == nil {
		t.Fatalf("no step found for web-1, got: %+v", hb.Steps)
	}
	if !strings.Contains(step.Detail, "LOST") {
		t.Errorf("migration step Detail = %q, want it to quote the LOST quorum verdict", step.Detail)
	}
	if !strings.Contains(step.StopCondition, "SURVIVES") {
		t.Errorf("migration step StopCondition = %q, want it to require SURVIVES", step.StopCondition)
	}
}

func TestBuildHandbook_ReplicaBackedProducesNonIrreversibleInformationalStep(t *testing.T) {
	hb := BuildHandbook(Inputs{
		TargetNodeID: "node-a",
		Quorum:       survivingQuorum,
		ReplicaBacked: []ReplicaBackedFact{
			{ID: "xyz789", Name: "db-1", Kind: "vm", OwnerNodeID: "node-c"},
		},
	})
	step := stepTitled(hb.Steps, "db-1")
	if step == nil {
		t.Fatalf("no step found for db-1, got: %+v", hb.Steps)
	}
	if step.Irreversible {
		t.Error("replica-backed step Irreversible = true, want false")
	}
	if step.StopCondition != "" {
		t.Errorf("replica-backed step StopCondition = %q, want empty", step.StopCondition)
	}
}

func TestBuildHandbook_ImageUnknownAndUnavailableWordedDifferently(t *testing.T) {
	hb := BuildHandbook(Inputs{
		TargetNodeID: "node-a",
		Quorum:       survivingQuorum,
		Images: []ImageFact{
			{ResourceID: "r1", ResourceName: "web-1", ImageName: "ubuntu.raw", Verdict: ImageUnknown},
			{ResourceID: "r2", ResourceName: "web-2", ImageName: "debian.iso", Verdict: ImageUnavailable},
			{ResourceID: "r3", ResourceName: "web-3", ImageName: "fine.raw", Verdict: ImageAvailable},
		},
	})
	unknown := stepTitled(hb.Steps, "ubuntu.raw")
	unavailable := stepTitled(hb.Steps, "debian.iso")
	available := stepTitled(hb.Steps, "fine.raw")

	if unknown == nil || unavailable == nil {
		t.Fatalf("expected steps for both unknown and unavailable images, got: %+v", hb.Steps)
	}
	if available != nil {
		t.Errorf("expected no step for an ImageAvailable fact, got: %+v", available)
	}
	if unknown.Detail == unavailable.Detail {
		t.Error("unknown and unavailable image steps must never be worded the same way")
	}
	if !strings.Contains(unknown.Detail, "Verify") {
		t.Errorf("unknown image step Detail = %q, want it to say to verify a source", unknown.Detail)
	}
	if !strings.Contains(unavailable.Detail, "blocked") {
		t.Errorf("unavailable image step Detail = %q, want it to say rebuild is blocked", unavailable.Detail)
	}
}

func TestBuildHandbook_StepOrderIsStableAndDeterministic(t *testing.T) {
	in := Inputs{
		TargetNodeID: "node-a",
		Quorum:       survivingQuorum,
		OwnedResources: []ResourceFact{
			{ID: "a1", Name: "a", Kind: "vm", ReplicaNodeID: "node-b", ReplicaConfigured: true},
			{ID: "a2", Name: "b", Kind: "vm", ReplicaConfigured: false},
		},
		ReplicaBacked: []ReplicaBackedFact{{ID: "c1", Name: "c", Kind: "jail", OwnerNodeID: "node-d"}},
		Images:        []ImageFact{{ResourceID: "a1", ResourceName: "a", ImageName: "x.raw", Verdict: ImageUnavailable}},
	}

	hb1 := BuildHandbook(in)
	hb2 := BuildHandbook(in)

	if len(hb1.Steps) != len(hb2.Steps) {
		t.Fatalf("got different step counts across identical calls: %d vs %d", len(hb1.Steps), len(hb2.Steps))
	}
	for i := range hb1.Steps {
		if hb1.Steps[i] != hb2.Steps[i] {
			t.Errorf("step %d differs across identical calls: %+v vs %+v", i, hb1.Steps[i], hb2.Steps[i])
		}
		if hb1.Steps[i].Order != i+1 {
			t.Errorf("step %d has Order %d, want %d", i, hb1.Steps[i].Order, i+1)
		}
	}
}

func TestBuildHandbook_LeaderLossQuorumStepCarriesItsOwnCaveat(t *testing.T) {
	hb := BuildHandbook(Inputs{TargetNodeID: "node-a", IsCurrentLeader: true, Quorum: survivingQuorum})
	if hb.QuorumVerdict != QuorumUnknown {
		t.Fatalf("QuorumVerdict = %v, want Unknown (leader-loss downgrade)", hb.QuorumVerdict)
	}
	quorumStep := hb.Steps[0]
	if !strings.Contains(quorumStep.Detail, "leadership") {
		t.Errorf("quorum step Detail = %q, want it to explain the leader-loss caveat", quorumStep.Detail)
	}
	if quorumStep.StopCondition == "" {
		t.Error("quorum step StopCondition is empty for Unknown verdict, want a real stop condition")
	}
}
