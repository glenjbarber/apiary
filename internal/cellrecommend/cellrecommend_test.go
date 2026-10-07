package cellrecommend

import (
	"testing"

	rpcpb "github.com/glenjbarber/apiary/api/rpc"
)

func TestRecommend_CustomKernelForcesVMAtHighConfidence(t *testing.T) {
	resp := Recommend(&rpcpb.WorkloadDescription{NeedsCustomKernelOrOs: true, DensityPriority: true})
	if resp.GetRecommendedCellType() != rpcpb.CellType_CELL_TYPE_VM {
		t.Fatalf("RecommendedCellType = %v, want CELL_TYPE_VM", resp.GetRecommendedCellType())
	}
	if resp.GetConfidence() != rpcpb.RecommendationConfidence_CONFIDENCE_HIGH {
		t.Fatalf("Confidence = %v, want CONFIDENCE_HIGH (hard requirement is decisive)", resp.GetConfidence())
	}
	if resp.GetAlternativeCellType() != rpcpb.CellType_CELL_TYPE_UNSPECIFIED {
		t.Fatalf("AlternativeCellType = %v, want CELL_TYPE_UNSPECIFIED for a decisive hard requirement", resp.GetAlternativeCellType())
	}
	found := false
	for _, r := range resp.GetReasons() {
		if r.GetFactor() == rpcpb.RecommendationFactor_FACTOR_CUSTOM_KERNEL_REQUIRED && r.GetFavors() == rpcpb.CellType_CELL_TYPE_VM {
			found = true
		}
	}
	if !found {
		t.Errorf("Reasons = %+v, want a FACTOR_CUSTOM_KERNEL_REQUIRED entry favoring VM", resp.GetReasons())
	}
}

func TestRecommend_FullIsolationForcesVMAtHighConfidence(t *testing.T) {
	resp := Recommend(&rpcpb.WorkloadDescription{NeedsFullIsolation: true})
	if resp.GetRecommendedCellType() != rpcpb.CellType_CELL_TYPE_VM {
		t.Fatalf("RecommendedCellType = %v, want CELL_TYPE_VM", resp.GetRecommendedCellType())
	}
	if resp.GetConfidence() != rpcpb.RecommendationConfidence_CONFIDENCE_HIGH {
		t.Fatalf("Confidence = %v, want CONFIDENCE_HIGH", resp.GetConfidence())
	}
}

func TestRecommend_DensityPriorityAloneFavorsJail(t *testing.T) {
	resp := Recommend(&rpcpb.WorkloadDescription{DensityPriority: true})
	if resp.GetRecommendedCellType() != rpcpb.CellType_CELL_TYPE_JAIL {
		t.Fatalf("RecommendedCellType = %v, want CELL_TYPE_JAIL", resp.GetRecommendedCellType())
	}
}

func TestRecommend_MigrationSignalAloneFavorsVM(t *testing.T) {
	resp := Recommend(&rpcpb.WorkloadDescription{ExpectsLiveMigrationOrSnapshot: true})
	if resp.GetRecommendedCellType() != rpcpb.CellType_CELL_TYPE_VM {
		t.Fatalf("RecommendedCellType = %v, want CELL_TYPE_VM", resp.GetRecommendedCellType())
	}
}

func TestRecommend_NoSignalDefaultsToJailAtLowConfidenceWithVMAsAlternative(t *testing.T) {
	resp := Recommend(&rpcpb.WorkloadDescription{})
	if resp.GetRecommendedCellType() != rpcpb.CellType_CELL_TYPE_JAIL {
		t.Fatalf("RecommendedCellType = %v, want CELL_TYPE_JAIL (lighter-weight default)", resp.GetRecommendedCellType())
	}
	if resp.GetConfidence() != rpcpb.RecommendationConfidence_CONFIDENCE_LOW {
		t.Fatalf("Confidence = %v, want CONFIDENCE_LOW when no signal is present", resp.GetConfidence())
	}
	if resp.GetAlternativeCellType() != rpcpb.CellType_CELL_TYPE_VM {
		t.Fatalf("AlternativeCellType = %v, want CELL_TYPE_VM", resp.GetAlternativeCellType())
	}
	if len(resp.GetReasons()) != 0 {
		t.Errorf("Reasons = %+v, want none when no signal was present", resp.GetReasons())
	}
}

func TestRecommend_ConflictingSoftSignalsTieFavorsJailAtLowConfidence(t *testing.T) {
	// One vote each way: migration support (VM) vs. density priority (jail).
	resp := Recommend(&rpcpb.WorkloadDescription{ExpectsLiveMigrationOrSnapshot: true, DensityPriority: true})
	if resp.GetConfidence() != rpcpb.RecommendationConfidence_CONFIDENCE_LOW {
		t.Fatalf("Confidence = %v, want CONFIDENCE_LOW for a tie", resp.GetConfidence())
	}
	if resp.GetRecommendedCellType() == resp.GetAlternativeCellType() {
		t.Fatalf("RecommendedCellType (%v) and AlternativeCellType (%v) must differ on a tie", resp.GetRecommendedCellType(), resp.GetAlternativeCellType())
	}
}

func TestRecommend_LargeResourceEstimateFavorsVM(t *testing.T) {
	resp := Recommend(&rpcpb.WorkloadDescription{EstimatedVcpus: 16, EstimatedMemoryMb: 32768})
	if resp.GetRecommendedCellType() != rpcpb.CellType_CELL_TYPE_VM {
		t.Fatalf("RecommendedCellType = %v, want CELL_TYPE_VM for a large resource estimate", resp.GetRecommendedCellType())
	}
}

func TestRecommend_SmallResourceEstimateFavorsJail(t *testing.T) {
	resp := Recommend(&rpcpb.WorkloadDescription{EstimatedVcpus: 1, EstimatedMemoryMb: 256})
	if resp.GetRecommendedCellType() != rpcpb.CellType_CELL_TYPE_JAIL {
		t.Fatalf("RecommendedCellType = %v, want CELL_TYPE_JAIL for a small resource estimate", resp.GetRecommendedCellType())
	}
}

func TestRecommend_IsDeterministic(t *testing.T) {
	workload := &rpcpb.WorkloadDescription{DensityPriority: true, EstimatedVcpus: 2}
	first := Recommend(workload)
	second := Recommend(workload)
	if first.GetRecommendedCellType() != second.GetRecommendedCellType() || first.GetConfidence() != second.GetConfidence() {
		t.Fatalf("Recommend() was not deterministic for the same input: first=%+v second=%+v", first, second)
	}
}
