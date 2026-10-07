// Package cellrecommend implements ADR-0150's cell-type recommendation
// rule evaluation: a small, deterministic function from a
// WorkloadDescription to a CellType, with the evidence behind the
// choice - not ML, nothing resembling model inference, the same spirit
// as internal/whynot's rule-based advisory logic (ADR-0061).
//
// This is pure computation with no I/O and no cluster-state lookups, so
// it is shared, unmodified, by both RecommendCellType (the standalone
// advisory RPC) and CreateCell (which calls it internally to resolve
// cell_type before building a VMDefinition/JailDefinition) - ADR-0150
// open question 2 is resolved by this package existing at all: there is
// exactly one copy of the selection logic, not two that could drift.
package cellrecommend

import (
	rpcpb "github.com/glenjbarber/apiary/api/rpc"
)

// Recommend evaluates workload against a fixed, ordered set of factors
// and returns a complete RecommendCellTypeResponse. Hard requirements
// (needs_custom_kernel_or_os, then needs_full_isolation) are checked
// first and are decisive - if either is set, the response carries
// CONFIDENCE_HIGH and no further factor can change the outcome. Every
// other factor is a soft signal that only contributes toward
// CONFIDENCE_MEDIUM/CONFIDENCE_HIGH when every signal present agrees, or
// leaves the recommendation at CONFIDENCE_LOW with alternative_cell_type
// set when signals disagree or none were present at all.
func Recommend(workload *rpcpb.WorkloadDescription) *rpcpb.RecommendCellTypeResponse {
	var reasons []*rpcpb.RecommendationReason

	// Hard requirements, checked first and decisive - see ADR-0150's own
	// "hard requirements first and decisive, then soft signals" ordering.
	if workload.GetNeedsCustomKernelOrOs() {
		reasons = append(reasons, &rpcpb.RecommendationReason{
			Factor: rpcpb.RecommendationFactor_FACTOR_CUSTOM_KERNEL_REQUIRED,
			Favors: rpcpb.CellType_CELL_TYPE_VM,
			Detail: "workload needs a custom kernel or non-FreeBSD OS, which a jail (sharing the host kernel) cannot provide",
		})
		return &rpcpb.RecommendCellTypeResponse{
			RecommendedCellType: rpcpb.CellType_CELL_TYPE_VM,
			Confidence:          rpcpb.RecommendationConfidence_CONFIDENCE_HIGH,
			Reasons:             reasons,
		}
	}
	if workload.GetNeedsFullIsolation() {
		reasons = append(reasons, &rpcpb.RecommendationReason{
			Factor: rpcpb.RecommendationFactor_FACTOR_ISOLATION_REQUIRED,
			Favors: rpcpb.CellType_CELL_TYPE_VM,
			Detail: "workload needs kernel-level isolation from the host, which a jail cannot provide",
		})
		return &rpcpb.RecommendCellTypeResponse{
			RecommendedCellType: rpcpb.CellType_CELL_TYPE_VM,
			Confidence:          rpcpb.RecommendationConfidence_CONFIDENCE_HIGH,
			Reasons:             reasons,
		}
	}

	// Soft signals. Each one that is present contributes one vote toward
	// CELL_TYPE_VM or CELL_TYPE_JAIL; the final recommendation is
	// whichever side has more votes, with confidence set by whether the
	// votes agree.
	vmVotes, jailVotes := 0, 0

	if workload.GetExpectsLiveMigrationOrSnapshot() {
		vmVotes++
		reasons = append(reasons, &rpcpb.RecommendationReason{
			Factor: rpcpb.RecommendationFactor_FACTOR_MIGRATION_SUPPORT,
			Favors: rpcpb.CellType_CELL_TYPE_VM,
			Detail: "VM snapshot/restore and migration are more mature than the jail equivalents",
		})
	}
	if workload.GetDensityPriority() {
		jailVotes++
		reasons = append(reasons, &rpcpb.RecommendationReason{
			Factor: rpcpb.RecommendationFactor_FACTOR_DENSITY_PRIORITY,
			Favors: rpcpb.CellType_CELL_TYPE_JAIL,
			Detail: "caller prioritizes many lightweight instances per node over per-instance isolation",
		})
	}
	// A resource estimate large enough to suggest a dedicated, heavier
	// workload leans VM; a small or unspecified estimate leans jail (a
	// jail's lower overhead is otherwise wasted on a workload too large
	// to benefit from density in the first place is NOT inferred here -
	// v1 only reads this as a density/overhead signal, nothing more).
	if workload.GetEstimatedVcpus() > 4 || workload.GetEstimatedMemoryMb() > 8192 {
		vmVotes++
		reasons = append(reasons, &rpcpb.RecommendationReason{
			Factor: rpcpb.RecommendationFactor_FACTOR_RESOURCE_PROFILE,
			Favors: rpcpb.CellType_CELL_TYPE_VM,
			Detail: "estimated resource profile is large enough that a dedicated VM's overhead is not the dominant cost",
		})
	} else if workload.GetEstimatedVcpus() > 0 || workload.GetEstimatedMemoryMb() > 0 {
		jailVotes++
		reasons = append(reasons, &rpcpb.RecommendationReason{
			Factor: rpcpb.RecommendationFactor_FACTOR_RESOURCE_PROFILE,
			Favors: rpcpb.CellType_CELL_TYPE_JAIL,
			Detail: "estimated resource profile is small enough that a jail's lower overhead is worth taking",
		})
	}

	switch {
	case vmVotes == 0 && jailVotes == 0:
		// No signal at all. Default to the lighter-weight kind (jail),
		// at the lowest confidence, with the VM noted as the real
		// alternative - never silently picking one with no stated reason.
		return &rpcpb.RecommendCellTypeResponse{
			RecommendedCellType: rpcpb.CellType_CELL_TYPE_JAIL,
			Confidence:          rpcpb.RecommendationConfidence_CONFIDENCE_LOW,
			AlternativeCellType: rpcpb.CellType_CELL_TYPE_VM,
			Reasons:             reasons,
		}
	case vmVotes > jailVotes:
		conf := rpcpb.RecommendationConfidence_CONFIDENCE_MEDIUM
		var alt rpcpb.CellType
		if jailVotes > 0 {
			conf = rpcpb.RecommendationConfidence_CONFIDENCE_LOW
			alt = rpcpb.CellType_CELL_TYPE_JAIL
		}
		return &rpcpb.RecommendCellTypeResponse{
			RecommendedCellType: rpcpb.CellType_CELL_TYPE_VM,
			Confidence:          conf,
			AlternativeCellType: alt,
			Reasons:             reasons,
		}
	case jailVotes > vmVotes:
		conf := rpcpb.RecommendationConfidence_CONFIDENCE_MEDIUM
		var alt rpcpb.CellType
		if vmVotes > 0 {
			conf = rpcpb.RecommendationConfidence_CONFIDENCE_LOW
			alt = rpcpb.CellType_CELL_TYPE_VM
		}
		return &rpcpb.RecommendCellTypeResponse{
			RecommendedCellType: rpcpb.CellType_CELL_TYPE_JAIL,
			Confidence:          conf,
			AlternativeCellType: alt,
			Reasons:             reasons,
		}
	default: // tie, vmVotes == jailVotes > 0
		return &rpcpb.RecommendCellTypeResponse{
			RecommendedCellType: rpcpb.CellType_CELL_TYPE_JAIL,
			Confidence:          rpcpb.RecommendationConfidence_CONFIDENCE_LOW,
			AlternativeCellType: rpcpb.CellType_CELL_TYPE_VM,
			Reasons:             reasons,
		}
	}
}
