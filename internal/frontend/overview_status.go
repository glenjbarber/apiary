package frontend

import (
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/glenjbarber/apiary/internal/health"
	"github.com/glenjbarber/apiary/internal/manager"
)

// The status line, the health chip, and the "Needs attention" list
// (handoff sections 7.1, 7.2, 7.3).
//
// All three are derived from ONE set of gathered rows, so they cannot
// disagree with each other or with the Combs section.

// overviewStatusLineFrom builds the one-line status summary.
//
// The handoff's items, in order:
//
//	Combs: reachable / known
//	Cells: running / total
//	Raft: leader name and term
//	HAST: in sync / total resources
//	ZFS: percent used across pools
//	Warnings: count, in amber with a warning dot. Omitted when zero.
//
// TWO OF THESE HAVE NO EVIDENCE SOURCE TODAY, AND BOTH SAY SO.
//
//   - "Raft: leader name and term" - the leader's NAME is observed
//     (the Status response carries it), but the raft TERM is not on
//     any RPC in this codebase. The line therefore shows the leader
//     and says the term is unknown, rather than showing a term
//     number it would have to invent.
//   - "HAST: in sync / total resources" - HAST sync state is
//     per-RESOURCE, not per-Colony, and reading it for every
//     replica-backed resource on every overview render would dial
//     every replica pair. The line reports the number of resources
//     that HAVE a replica configured and says their sync state is
//     unknown unless it was actually observed.
//
// A value the server cannot compute renders as "unknown" in the
// unknown style. Never guess, and never show a missing value as zero
// or ok.
func (s *Server) overviewStatusLineFrom(nodes []clusterNodeView, leader colonyLeaderView, combs overviewCombView, summary hastSyncSummary, warnings int, now time.Time) overviewStatusLine {
	line := overviewStatusLine{}

	// Combs: reachable / known.
	reachable, known := 0, len(nodes)
	for _, node := range nodes {
		if node.Reachable {
			reachable++
		}
	}
	line.Metrics = append(line.Metrics, overviewMetric{
		Label:  "Combs",
		Value:  fmt.Sprintf("%d/%d", reachable, known),
		Known:  known > 0,
		Detail: fmt.Sprintf("%d of %d known Combs answered a direct call just now", reachable, known),
	})

	// Cells: running / total.
	running, total := 0, 0
	for _, comb := range combs.Combs {
		for _, cell := range comb.Cells {
			total++
			if !cell.Stopped {
				running++
			}
		}
	}
	line.Metrics = append(line.Metrics, overviewMetric{
		Label:  "Cells",
		Value:  fmt.Sprintf("%d/%d", running, total),
		Known:  true,
		Detail: fmt.Sprintf("%d of %d cells are running", running, total),
	})

	// Raft: leader name, and the term if it is known.
	raft := overviewMetric{Label: "Raft"}
	switch {
	case !leader.Observed:
		raft.Value = "unknown"
		raft.Detail = "raft was not read, so no leader could be established: " + leader.Detail
	case leader.State == colonyLeaderKnown:
		raft.Known = true
		raft.Value = leader.NodeID
		// The raft term is not carried by any RPC in this codebase.
		// The line names the leader it DID observe and says plainly
		// that the term is not available, rather than showing a
		// number it would have to invent.
		raft.Detail = "leader " + leader.NodeID + " (raft term is not exposed by any RPC in this build, so it is not shown)"
	case leader.State == colonyLeaderNone:
		// An observed absence is a real reading: raft answered and
		// reports no leader. It is NOT unknown.
		raft.Known = true
		raft.Value = "no leader"
		raft.Detail = "raft was read and reports no current leader"
	default:
		raft.Value = "unknown"
		raft.Detail = "raft state is " + leader.State + ": " + leader.Detail
	}
	line.Metrics = append(line.Metrics, raft)

	// HAST: in sync / total resources. The total is the number of
	// resources that HAVE a replica configured; the "in sync" half is
	// only rendered when it was actually observed.
	hast := overviewMetric{Label: "HAST"}
	switch {
	case summary.Total == 0:
		// Nothing has a replica configured, so there is no
		// population the "in sync" half would be a fraction of.
		// That is an honest absence of the fact, not a reading.
		hast.Value = "unknown"
		hast.Detail = summary.Detail
	case !summary.Known:
		// Every resource's state could not be read. "0/N" would be
		// a claim that they are all out of sync, which is not what
		// was observed.
		hast.Value = "unknown"
		hast.Detail = summary.Detail
	default:
		hast.Value = fmt.Sprintf("%d/%d", summary.InSync, summary.Total)
		hast.Detail = summary.Detail
	}
	line.Metrics = append(line.Metrics, hast)

	// ZFS: percent used across pools. The worst pool across the
	// Colony, for the same reason the per-Comb meter uses the worst
	// pool.
	zfs := overviewMetric{Label: "ZFS"}
	zfsKnown := false
	zfsPct := 0
	zfsPool := ""
	for _, node := range nodes {
		if !node.Reachable || !node.ZFSKnown {
			continue
		}
		if !zfsKnown || node.ZFSPct > zfsPct {
			zfsKnown = true
			zfsPct = node.ZFSPct
			zfsPool = node.ZFSPool
		}
	}
	if zfsKnown {
		zfs.Known = true
		zfs.Value = fmt.Sprintf("%d%%", zfsPct)
		zfs.Detail = "the most-used pool across the Colony is " + zfsPool + " at " + fmt.Sprintf("%d%%", zfsPct)
	} else {
		zfs.Value = "unknown"
		zfs.Detail = "no Comb's ZFS pool usage could be read"
	}
	line.Metrics = append(line.Metrics, zfs)

	// Warnings: the count, in amber with a warning dot. OMITTED WHEN
	// ZERO - the handoff is explicit about this, and a "0 warnings"
	// item is noise that makes the real count harder to find.
	//
	// It is appended last so it sits at the end of the line, where a
	// count of things that need attention belongs.
	if warnings > 0 {
		line.Metrics = append(line.Metrics, overviewMetric{
			Label: "Warnings", Value: fmt.Sprintf("%d", warnings),
			Known: true, Warn: true,
			Detail: fmt.Sprintf("%d Comb(s) are observed and need attention", warnings),
		})
	}

	return line
}

// overviewWarningCount counts the warnings the status line shows.
//
// A warning is a Comb whose state is WARN - observed and needing
// attention, not yet a failure. Critical and contradictory states are
// NOT warnings: they are counted by the health chip, and counting them
// here as well would make the two surfaces report the same thing twice
// with different numbers.
func (s *Server) overviewWarningCount(nodes []clusterNodeView) int {
	count := 0
	for _, node := range nodes {
		state, _ := overviewStateFor(node)
		if state == statusWarn {
			count++
		}
	}
	return count
}

// overviewChipFrom builds the overall health chip.
//
// The handoff's rules, exactly:
//
//   - "Healthy" (green) when nothing is critical, contradictory, or
//     unknown. WARNINGS DO NOT TURN THE CHIP AMBER - they are counted
//     in the status line instead.
//   - "Attention" (red) when anything is critical or contradictory.
//   - "Unknown" (hollow grey) when the Colony itself has no valid
//     observation.
//
// The precedence matters: the Colony's own observation comes FIRST. A
// Colony that could not be read is Unknown even if some Comb looks
// fine, because "everything is fine" is a claim about the whole.
func overviewChipFrom(nodes []clusterNodeView, warnings int, colonyObserved bool, statusErr error) overviewHealthChip {
	if !colonyObserved {
		detail := "the Colony itself has no valid observation, so nothing can be said about its health"
		if statusErr != nil {
			detail += ": " + statusErr.Error()
		}
		return overviewHealthChip{State: statusUnknown, Label: "Unknown", Detail: detail}
	}
	if len(nodes) == 0 {
		return overviewHealthChip{
			State:  statusUnknown,
			Label:  "Unknown",
			Detail: "no Comb was observed, so nothing was checked - this is not the same as a healthy Colony",
		}
	}

	critical, contradictory, unknown := 0, 0, 0
	for _, node := range nodes {
		switch state, _ := overviewStateFor(node); state {
		case statusCritical:
			critical++
		case statusContradictory:
			contradictory++
		case statusUnknown:
			unknown++
		}
	}
	switch {
	case contradictory > 0:
		return overviewHealthChip{
			State: statusCritical, Label: "Attention",
			Detail: fmt.Sprintf("%d Comb(s) report contradictory evidence - two sources disagree about the same Comb", contradictory),
		}
	case critical > 0:
		return overviewHealthChip{
			State: statusCritical, Label: "Attention",
			Detail: fmt.Sprintf("%d Comb(s) are observed and bad", critical),
		}
	case unknown > 0:
		return overviewHealthChip{
			State: statusUnknown, Label: "Unknown",
			Detail: fmt.Sprintf("%d Comb(s) could not be observed, so the Colony's health is not fully known", unknown),
		}
	default:
		detail := "nothing is critical, contradictory, or unknown"
		if warnings > 0 {
			detail += fmt.Sprintf("; %d warning(s) are counted in the status line", warnings)
		}
		return overviewHealthChip{State: statusOK, Label: "Healthy", Detail: detail}
	}
}

// overviewAttentionFrom builds the "Needs attention" list.
//
// SOURCES (handoff 7.3): stale or unhealthy replicas, combs over a
// resource threshold, certificates expiring soon, and pending join
// requests.
//
// ORDER: critical, contradictory, stale, warning, then informational
// (join requests).
//
// EVERY ITEM'S ACTION LINK GOES TO A PAGE OR FORM THAT ALREADY EXISTS.
// The handoff forbids new action endpoints in this phase, so there is
// no synthetic action here - each link is a real route.
func (s *Server) overviewAttentionFrom(r *http.Request, canAdmin bool, nodes []clusterNodeView, combs overviewCombView, index *combCauseIndex, leader colonyLeaderView, now time.Time) []overviewAttentionItem {
	type ranked struct {
		item  overviewAttentionItem
		rank  int
		order int
	}
	var items []ranked
	seq := 0
	add := func(rank int, item overviewAttentionItem) {
		seq++
		items = append(items, ranked{item: item, rank: rank, order: seq})
	}

	// Per-Comb items: the Comb's own state, and its resources over
	// their thresholds.
	for _, comb := range combs.Combs {
		switch comb.State {
		case statusCritical:
			add(0, overviewAttentionItem{
				State: statusCritical, Subject: comb.NodeID,
				Sentence: " is observed and bad",
				Detail:   comb.StateDetail,
				Action:   "Open", Href: comb.Href,
			})
		case statusContradictory:
			add(0, overviewAttentionItem{
				State: statusContradictory, Subject: comb.NodeID,
				Sentence: " has contradictory evidence",
				Detail:   comb.StateDetail,
				Action:   "Open", Href: comb.Href,
			})
		case statusStale:
			add(2, overviewAttentionItem{
				State: statusStale, Subject: comb.NodeID,
				Sentence: " is stale",
				Detail:   comb.StateDetail,
				Action:   "Open", Href: comb.Href,
			})
		case statusUnknown:
			add(3, overviewAttentionItem{
				State: statusUnknown, Subject: comb.NodeID,
				Sentence: " could not be observed",
				Detail:   comb.StateDetail,
				Action:   "Open", Href: comb.Href,
			})
		}

		// The resource thresholds. Each is reported on its own, with
		// the Comb named, because "86%" without the Comb it belongs
		// to is not actionable.
		for _, meter := range []overviewMeter{comb.CPU, comb.Memory, comb.ZFS} {
			if !meter.Warn {
				continue
			}
			add(3, overviewAttentionItem{
				State: statusWarn, Subject: comb.NodeID,
				Sentence: " " + meter.Label + " is at " + fmt.Sprintf("%d%%", meter.Percent),
				Detail:   meter.Detail,
				Action:   "Open", Href: comb.Href,
			})
		}
	}

	// Certificates expiring soon. This is the Comb's OWN inventory -
	// the same read the Certificates page makes - so the list names
	// the certificate and links to the page that can act on it.
	certRows, certErr := s.currentCertificates(r, now)
	if certErr == "" {
		for _, row := range certRows {
			if row.Status != "soon" {
				continue
			}
			add(3, overviewAttentionItem{
				State: statusWarn, Subject: row.Name,
				Sentence: " expires soon",
				Detail:   row.Remaining,
				Action:   "Review", Href: "/certificates",
			})
		}
	}

	// Pending join requests. These are INFORMATIONAL - they are the
	// last thing in the order, and they are the Admin's to act on.
	// The list is gathered only for an Admin session, because
	// ListJoinRequests is Admin-gated at the RPC layer and a
	// PermissionDenied there is expected, not a page error.
	if canAdmin {
		requests := s.currentJoinRequests(r)
		for _, req := range requests {
			add(4, overviewAttentionItem{
				State: statusNotApplicable, Subject: req.NodeID,
				Sentence: " requested to join the Colony",
				Detail:   req.RaftBindAddress,
				Action:   "Review", Href: "/#pending-join-requests",
			})
		}
	}

	sort.SliceStable(items, func(i, j int) bool {
		if items[i].rank != items[j].rank {
			return items[i].rank < items[j].rank
		}
		return items[i].order < items[j].order
	})

	out := make([]overviewAttentionItem, 0, len(items))
	for _, item := range items {
		out = append(out, item.item)
	}
	return out
}

// overviewAttentionSummary is a one-line description of the list's
// contents, used by the section heading.
func overviewAttentionSummary(items []overviewAttentionItem) string {
	if len(items) == 0 {
		return "Nothing needs attention."
	}
	counts := map[string]int{}
	for _, item := range items {
		counts[item.State]++
	}
	var parts []string
	for _, state := range statusOrder {
		if counts[state] > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", counts[state], strings.ToLower(statusLabel[state])))
		}
	}
	return strings.Join(parts, ", ")
}

// healthStatusIsKnown reports whether a health status is a real
// observation. It exists so this file does not reach into
// internal/health's own vocabulary more than it must.
func healthStatusIsKnown(status health.Status) bool {
	return status == health.StatusHealthy || status == health.StatusDegraded ||
		status == health.StatusStale || status == health.StatusContradictory
}

// overviewCanAdmin reports whether this request's session may see
// Admin-only material.
//
// When login is disabled every route is open, so the answer is yes -
// the same convention requireRole follows. When login is enabled, only
// a real Admin session qualifies.
func (s *Server) overviewCanAdmin(r *http.Request) bool {
	if s.auth == nil {
		return true
	}
	info, ok := s.currentSession(r)
	return ok && info.role.Satisfies(manager.RoleAdmin)
}
