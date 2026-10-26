package frontend

import (
	"context"
	"net/http"
	"sort"
	"strconv"
	"sync"
	"time"
)

// The Storage > Replication page (handoff section 6).
//
// IT IS READ-ONLY AND IT IS PER-RESOURCE, NOT PER-COLONY. Apiary has
// no colony-wide HAST health reading: HAST's state lives on the two
// nodes that hold each pair, and the only way to learn it is to ask
// both of them. So this page enumerates the Colony's replica-backed
// resources and asks each pair.
//
// The evidence is EXACTLY the evidence the VM and jail detail pages
// already show (replicaFreshnessView, the same gather), so the page
// cannot disagree with the pages an operator is sent to from it.
//
// A RESOURCE WITH NO REPLICA IS NOT LISTED. "Unprotected" is a real
// state on the detail pages, but a Colony's worth of unreplicated
// cells is not a replication problem - it is a design choice, and
// listing every one of them would bury the pairs that ARE configured
// and the ones that are not syncing.

type replicaPageRow struct {
	// Kind is "vm" or "jail".
	Kind string
	// ID is the resource's ID.
	ID string
	// Name is the operator's name for it, if it has one.
	Name string
	// Owner and Replica are the two configured ends.
	Owner   string
	Replica string
	// Href is the resource's own page.
	Href string
	// Verdict is the freshness verdict: "complete-observed",
	// "degraded", "unknown", or "unprotected".
	Verdict string
	// VerdictState is the seven-state vocabulary value the verdict
	// maps onto, so the page draws from the same vocabulary as
	// everything else.
	VerdictState string
	// Explanation is the verdict's own sentence.
	Explanation string
	// Observations are the two nodes' verbatim HAST views.
	Observations []hastObservationView
}

// replicaPageView is the whole page's data.
type replicaPageView struct {
	Rows []replicaPageRow
	// Total is how many replica-backed resources exist.
	Total int
	// Unprotected is how many resources have NO replica configured.
	// It is counted and reported, because "how many of my cells have
	// no redundancy at all" is the question a storage page is asked.
	Unprotected int
	// Error is a page-level failure.
	Error string
	// GatheredAt is when the evidence was gathered.
	GatheredAt string
}

// handleReplicaPage serves the Storage > Replication page
// ("/replication").
//
// It is Viewer-readable: it reports the Colony's replication state,
// and seeing that state is not a privileged act. The handoff requires
// the page to be read-only, and it is - there is no mutating control
// anywhere on it.
func (s *Server) handleReplicaPage(w http.ResponseWriter, r *http.Request) {
	now := time.Now()
	view := s.gatherReplicaPage(r.Context(), now)
	s.render(w, "replica_page", s.withAuthFields(r, pageData{
		ActivePage:  "replication",
		ReplicaPage: view,
	}))
}

// gatherReplicaPage gathers every replica-backed resource's freshness.
//
// The two lists are read ONCE, and the HAST pairs are asked
// CONCURRENTLY and BOUNDED - the same bound every other gather in this
// package uses, so one page render cannot become a dial storm.
func (s *Server) gatherReplicaPage(ctx context.Context, now time.Time) replicaPageView {
	view := replicaPageView{GatheredAt: now.Format("2006-01-02 15:04:05 MST")}

	index := s.gatherCombCauseIndex(ctx)
	if index == nil {
		view.Error = "the resource lists could not be read, so no replication state is known here."
		return view
	}

	type target struct {
		kind, id, name, owner, replica string
	}
	var targets []target
	for _, vm := range index.VMs {
		if vm.ReplicaNodeID == "" {
			view.Unprotected++
			continue
		}
		targets = append(targets, target{"vm", vm.ID, vm.Name, vm.NodeID, vm.ReplicaNodeID})
	}
	for _, jail := range index.Jails {
		if jail.ReplicaNodeID == "" {
			view.Unprotected++
			continue
		}
		targets = append(targets, target{"jail", jail.ID, jail.Name, jail.NodeID, jail.ReplicaNodeID})
	}
	view.Total = len(targets)
	if len(targets) == 0 {
		return view
	}

	// Sorted and bounded, so two renders ask in the same order and one
	// page render cannot become a dial storm.
	sort.Slice(targets, func(i, j int) bool {
		if targets[i].kind != targets[j].kind {
			return targets[i].kind < targets[j].kind
		}
		return targets[i].id < targets[j].id
	})
	truncated := false
	if len(targets) > hastGatherLimit {
		targets = targets[:hastGatherLimit]
		truncated = true
	}

	freshness := make([]replicaFreshnessView, len(targets))
	var wg sync.WaitGroup
	for i, t := range targets {
		wg.Add(1)
		go func(i int, t target) {
			defer wg.Done()
			freshness[i] = s.replicaFreshness(ctx, t.kind, t.id, t.owner, t.replica)
		}(i, t)
	}
	wg.Wait()

	view.Rows = make([]replicaPageRow, 0, len(targets))
	for i, t := range targets {
		href := "/vms/" + t.id
		if t.kind == "jail" {
			href = "/jails/" + t.id
		}
		view.Rows = append(view.Rows, replicaPageRow{
			Kind: t.kind, ID: t.id, Name: t.name,
			Owner: t.owner, Replica: t.replica, Href: href,
			Verdict:      freshness[i].Verdict,
			VerdictState: replicaVerdictState(freshness[i].Verdict),
			Explanation:  freshness[i].Explanation,
			Observations: freshness[i].Observations,
		})
	}
	if truncated {
		view.Error = "Only the first " + strconv.Itoa(hastGatherLimit) + " replica-backed resources are shown; the rest were not asked about, so nothing is claimed about them."
	}
	return view
}

// replicaVerdictState maps a freshness verdict onto the seven-state
// vocabulary.
//
// "complete-observed" is ok. "degraded" is critical - the pair was
// observed and is bad. "unknown" is unknown - the check did not
// complete. "unprotected" is not-applicable - there is no pair to
// have a state at all, which is a different fact from a pair that
// failed.
func replicaVerdictState(verdict string) string {
	switch verdict {
	case "complete-observed":
		return statusOK
	case "degraded":
		return statusCritical
	case "unprotected":
		return statusNotApplicable
	default:
		return statusUnknown
	}
}
