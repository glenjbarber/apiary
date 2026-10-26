package frontend

import (
	"context"
	"sort"
	"strings"
	"sync"

	"github.com/glenjbarber/apiary/internal/invariant"
)

// invariantHASTSpec is one resource's HAST reading, reduced to the two
// things the status line needs: was it observed at all, and is it in
// sync.
type invariantHASTSpec struct {
	observed bool
	inSync   bool
}

// The HAST half of the status line (handoff section 7.2).
//
// "HAST: in sync / total resources" is the one status-line item whose
// evidence is not already in hand when the page renders: HAST sync
// state is per-RESOURCE and lives on the two nodes that hold the pair,
// so it has to be asked for.
//
// THE COST IS BOUNDED, NOT AVOIDED. Each replica-backed resource costs
// one query to its owner and one to its replica, both concurrent and
// both bounded by the same per-node timeout and fan-out limit the rest
// of this package uses. A Colony with no replica-backed resources costs
// nothing.
//
// A RESOURCE WITH NO REPLICA IS NOT COUNTED IN THE TOTAL. The total is
// "resources that have a replica configured", because that is the
// population the "in sync" half is a fraction OF. Counting an
// unreplicated resource in the denominator would make the line read
// "3/12 in sync" and imply that nine resources are failing to
// replicate, when in fact they were never meant to.
//
// A RESOURCE WHOSE SYNC STATE COULD NOT BE READ counts in the total but
// NOT in the numerator, and the detail says so. That is the honest
// reading: it is not in sync as far as the evidence goes, and the
// evidence is missing. It is never counted as in sync.

// hastSyncSummary is the HAST metric's data.
type hastSyncSummary struct {
	// Total is the number of resources that have a replica configured.
	Total int
	// InSync is how many of them were observed in sync.
	InSync int
	// Unobserved is how many could not be read. It is Total-InSync
	// minus the resources that were observed OUT of sync, so the
	// detail can name the difference.
	Unobserved int
	// OutOfSync is how many were observed out of sync.
	OutOfSync int
	// Detail explains the reading, including the reason for any
	// unobserved count.
	Detail string
	// Known says whether anything could be read at all.
	Known bool
}

// overviewHASTSummary gathers the HAST sync state for the Colony's
// replica-backed resources.
//
// localNodeID is this Comb's own id, needed to know which query is
// local and which is a peer dial.
func (s *Server) overviewHASTSummary(ctx context.Context, index *combCauseIndex, localNodeID string) hastSyncSummary {
	summary := hastSyncSummary{}
	if index == nil {
		summary.Detail = "the resource list could not be read, so no HAST state is known"
		return summary
	}

	resources := hastResourcesFromVMsJails(index.VMs, index.VMError, index.Jails, index.JailError)
	if len(resources) == 0 {
		summary.Detail = "no resource in this Colony has a HAST replica configured"
		return summary
	}
	// Same bound and same ordering as the invariants page's own HAST
	// gather - one page render must not become a dial storm.
	sorted := make([]hastResource, len(resources))
	copy(sorted, resources)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].ID < sorted[j].ID })
	if len(sorted) > hastGatherLimit {
		sorted = sorted[:hastGatherLimit]
	}

	summary.Total = len(sorted)
	specs := make([]invariantHASTSpec, len(sorted))
	var wg sync.WaitGroup
	for i, res := range sorted {
		wg.Add(1)
		go func(i int, res hastResource) {
			defer wg.Done()
			specs[i] = s.overviewHASTSpec(ctx, res, localNodeID)
		}(i, res)
	}
	wg.Wait()

	for _, spec := range specs {
		switch {
		case !spec.observed:
			summary.Unobserved++
		case spec.inSync:
			summary.InSync++
		default:
			summary.OutOfSync++
		}
	}
	summary.Known = summary.InSync+summary.OutOfSync > 0
	summary.Detail = overviewHASTDetail(summary)
	return summary
}

// overviewHASTDetail explains the HAST reading in words, naming the
// unobserved count separately from the out-of-sync one - they are
// different problems with different fixes.
func overviewHASTDetail(summary hastSyncSummary) string {
	switch {
	case summary.Total == 0:
		return "no resource in this Colony has a HAST replica configured"
	case summary.Unobserved == summary.Total:
		return "no resource's HAST sync state could be read, so none of them is known to be in sync"
	case summary.Unobserved > 0:
		return "some resources' HAST sync state could not be read, and those are not counted as in sync"
	default:
		return "each resource's HAST state was read from both of the nodes that hold it"
	}
}

// overviewHASTSpec asks both ends of one resource for their own HAST
// view and reduces the two answers to one verdict.
//
// It reuses the same question the invariants page asks
// (askLocalHASTStatus) and the same reading internal/invariant applies
// (a real role with status "complete" is in sync), so the status line
// and the Invariants page cannot disagree about whether a resource's
// replica is synchronized.
//
// A pair where only ONE end answered is NOT in sync and NOT observed
// out of sync - it is unobserved, because "the secondary said it was
// complete" alone cannot establish that the pair is healthy.
func (s *Server) overviewHASTSpec(ctx context.Context, res hastResource, localNodeID string) invariantHASTSpec {
	overallCtx, cancel := context.WithTimeout(ctx, hastGatherOverallTimeout())
	defer cancel()

	owner := s.askLocalHASTStatus(overallCtx, res.ID, res.OwnerNodeID, localNodeID)
	replica := s.askLocalHASTStatus(overallCtx, res.ID, res.ReplicaNodeID, localNodeID)

	spec := invariantHASTSpec{observed: owner.Observed && replica.Observed}
	if !spec.observed {
		return spec
	}
	// The rule is internal/invariant's OWN, not a copy of it: a
	// resource is in sync when BOTH ends report a real role and a
	// "complete" status. That is the same conjunction the
	// cell-recoverability invariant uses, so the status line and the
	// Invariants page cannot disagree about whether a resource's
	// replica is synchronized.
	//
	// The roles and statuses are read the same way internal/invariant
	// reads them - case-insensitively, and with an empty status
	// meaning "no status" rather than "complete".
	spec.inSync = hastEndInSync(owner) && hastEndInSync(replica)
	return spec
}

// hastEndInSync reports whether one end of a HAST pair reported a
// real role and a "complete" status.
//
// It mirrors internal/invariant's readHAST rule for the in-sync case.
// The two cannot drift apart silently: the role and status spellings
// are the same constants, and the comparison is case-insensitive on
// both sides for the same reason - a FreeBSD release that changes the
// capitalisation must not turn a synchronized pair into an unknown
// one.
func hastEndInSync(obs invariant.HASTObservation) bool {
	if !obs.Attempted || !obs.Observed {
		return false
	}
	role := strings.TrimSpace(obs.Role)
	status := strings.TrimSpace(obs.Status)
	if status == "" || strings.EqualFold(status, "unknown") {
		return false
	}
	if !strings.EqualFold(status, "complete") {
		return false
	}
	return strings.EqualFold(role, "primary") || strings.EqualFold(role, "secondary")
}
