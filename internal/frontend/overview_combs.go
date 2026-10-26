package frontend

import (
	"context"
	"fmt"
	"sort"

	"github.com/glenjbarber/apiary/internal/health"
	"github.com/glenjbarber/apiary/internal/statedigest"
)

// The Comb rows of the Blend overview: the state vocabulary, the three
// meters, the cells, and the matrix's checks.

// overviewCellsByComb groups the Colony's cells by the Comb that owns
// them.
//
// It reads from the combCauseIndex the page already gathered, so the
// cell count on the status line and the cell chips on a card come from
// ONE read. A VM list that could not be read contributes no cells at
// all - it is not an empty list, and treating it as one would let a
// failed read look like a Colony with no workloads.
func (s *Server) overviewCellsByComb(ctx context.Context, index *combCauseIndex) map[string][]overviewCell {
	out := map[string][]overviewCell{}
	if index == nil {
		return out
	}
	for _, vm := range index.VMs {
		if vm.NodeID == "" {
			continue
		}
		out[vm.NodeID] = append(out[vm.NodeID], overviewCell{
			ID: vm.ID, Kind: "vm", Href: "/vms/" + vm.ID,
			Stopped: vm.DesiredState != "running",
		})
	}
	for _, jail := range index.Jails {
		if jail.NodeID == "" {
			continue
		}
		out[jail.NodeID] = append(out[jail.NodeID], overviewCell{
			ID: jail.ID, Kind: "jail", Href: "/jails/" + jail.ID,
			Stopped: jail.DesiredState != "running",
		})
	}
	for _, cells := range out {
		sort.Slice(cells, func(i, j int) bool {
			if cells[i].Kind != cells[j].Kind {
				return cells[i].Kind < cells[j].Kind
			}
			return cells[i].ID < cells[j].ID
		})
	}
	return out
}

// overviewCombViewFromNodes builds the whole Combs section from the
// gathered rows.
//
// It is a FUNCTION rather than inline code in the handler so that the
// page and a test that renders the template directly build the view
// model the same way - there is one implementation, and a test cannot
// pass against a shape the real page never renders.
func overviewCombViewFromNodes(nodes []clusterNodeView, leaderID string, cellsByComb map[string][]overviewCell, digestColony stateDigestColony) overviewCombView {
	combs := make([]overviewView, 0, len(nodes))
	for _, node := range nodes {
		combs = append(combs, overviewViewFor(node, leaderID, cellsByComb[node.NodeID], digestColony))
	}
	return overviewCombView{Combs: combs, DigestColony: digestColony, DigestLabel: digestColony.BadgeLabel}
}

// overviewViewFor builds one Comb's row.
//
// The STATE is derived from the same Evidence-Aware Health verdict the
// rest of the app already computes for this Comb, so the new overview
// cannot disagree with the Command Center or the per-Comb evidence page
// about whether a Comb is fine.
func overviewViewFor(node clusterNodeView, leaderID string, cells []overviewCell, digestColony stateDigestColony) overviewView {
	view := overviewView{
		NodeID:    node.NodeID,
		Href:      "/host/" + node.NodeID,
		IsLeader:  node.NodeID == leaderID && leaderID != "",
		CellCount: len(cells),
		Cells:     cells,
	}
	if view.IsLeader {
		view.Role = "Leader"
	} else {
		view.Role = "Follower"
	}

	// The Comb's own state. The seven-state vocabulary, derived from
	// the health verdict this page already computed.
	view.State, view.StateDetail = overviewStateFor(node)

	// The uptime and the OS release. NEITHER HAS AN EVIDENCE SOURCE
	// YET, and both say so rather than showing a guess.
	//
	// The uptime is not on any RPC in this codebase. The base system
	// CAN be read - HostPackages carries the running kernel's release
	// - but only for the Comb this frontend is colocated with, and
	// there is no peer-forwarded read for the others. Rendering this
	// Comb's release on every row would be a lie about every row but
	// one.
	view.UptimeKnown = false
	view.UptimeDetail = "Apiary does not read a host's uptime yet, so this is unknown rather than a guess"
	view.OSKnown = false
	view.OSDetail = "Apiary does not read another Comb's base system, so this is unknown rather than a guess"

	// The three meters. An unreadable reading is unknown, never 0%.
	//
	// AND A READING THAT WAS NEVER THIS COMB'S IS ALSO UNREADABLE.
	// When peer forwarding is not configured, a request for another
	// Comb's host data is answered by this frontend's own managerd,
	// so the answer is this Comb's data wearing another Comb's name.
	// The overview refuses it: the meters read unknown rather than
	// showing this Comb's CPU and memory on every row.
	view.Observed = node.Reachable && node.ObservationsAreItsOwn
	view.CPU = overviewMeterFor("CPU", node.CPUGauge, cpuWarnPct)
	view.Memory = overviewMeterFor("Memory", node.MemGauge, memWarnPct)
	view.ZFS = overviewZFSMeter(node)
	if !view.Observed {
		view.CPU = overviewMeter{Label: "CPU", Detail: "this Comb's readings could not be read from here: peer forwarding is not configured, so a request for another Comb's host data is answered by this Comb's own managerd"}
		view.Memory = overviewMeter{Label: "Memory", Detail: "this Comb's readings could not be read from here: peer forwarding is not configured, so a request for another Comb's host data is answered by this Comb's own managerd"}
		view.ZFS = overviewMeter{Label: "ZFS", Detail: "this Comb's readings could not be read from here: peer forwarding is not configured, so a request for another Comb's host data is answered by this Comb's own managerd"}
	}

	// This Comb's own FSM state digest verdict (ADR-0143). It is
	// stamped onto the row here, from the colony-wide comparison the
	// page computed over every row.
	view.Suffrage = overviewSuffrageFor(node)
	// The Comb's own cause line. It names the CAUSE, never the
	// verdict - the verdict is the state dot above it, and a headline
	// that restated it would print the same finding twice.
	view.CauseHeadline = node.CauseHeadline
	view.CauseBadge = node.CauseBadgeClass
	view.CauseLabel = node.CauseBadgeLabel
	view.CauseState = string(node.CauseState)
	view.DigestLabel = node.DigestBadgeLabel
	view.DigestDetail = node.DigestDetail
	view.DigestClass = node.DigestBadgeClass

	// The matrix's checks. Only the columns with a real evidence
	// source today are present - see overviewChecksFor.
	view.Checks = overviewChecksFor(node, digestColony)
	return view
}

// overviewSuffrageFor returns a Comb's raft membership.
//
// It comes from the SAME membership list the page's anchor Status
// response carried - the one ADR-0144's Comb tree already read - so
// this costs no extra RPC. A Comb whose suffrage was not observed
// gets an empty string, which renders as nothing at all rather than
// as a guessed "Voter".
func overviewSuffrageFor(node clusterNodeView) string {
	if node.Suffrage == "" {
		return ""
	}
	return node.Suffrage
}

// overviewStateFor maps one Comb's observed evidence onto the seven
// state vocabulary.
//
// The precedence is deliberate and is the handoff's own:
//
//   - contradictory wins outright. Two evidence sources disagreeing is
//     the most serious thing that can be true, and it must not be
//     flattened into "critical" - the two are different facts.
//   - critical: the Comb is observed and bad.
//   - warn: the Comb is observed and needs attention.
//   - stale: the observation is older than its freshness window.
//   - unknown: the check did not complete.
//
// A Comb that could not be dialed is UNKNOWN, not critical: the handoff
// says "Unknown (hollow grey) when the Colony itself has no valid
// observation", and an unreachable Comb has no valid observation. The
// "Needs attention" list is where an unreachable Comb is reported -
// the chip's job is to say whether the evidence is sound, and silence
// is not sound.
func overviewStateFor(node clusterNodeView) (string, string) {
	if !node.Reachable {
		return statusUnknown, "this Comb could not be reached: " + node.Error
	}
	switch node.HealthStatus {
	case health.StatusContradictory:
		return statusContradictory, node.HealthExplanation
	case health.StatusDegraded:
		return statusCritical, node.HealthExplanation
	case health.StatusStale:
		return statusStale, node.HealthExplanation
	case health.StatusHealthy:
		// A healthy verdict backed by a cause that failed is not
		// healthy. The cause panel is the more specific evidence.
		if node.CauseState == combCauseObservedFailed {
			return statusCritical, node.CauseHeadline
		}
		// A healthy raft-and-reconcile verdict, but a cause that was
		// NEVER OBSERVED, is not the same as a Comb everything was
		// checked on. "This Comb is healthy" and "something about
		// this Comb could not be checked" are different facts, and
		// the handoff's rule is that the second one is never
		// rendered as the first.
		//
		// The Comb's own cause line already says "not observed" in
		// words; this keeps the state dot honest too.
		if node.CauseState == combCauseNeverObserved {
			return statusUnknown, node.CauseHeadline
		}
		return statusOK, node.HealthExplanation
	default:
		return statusUnknown, node.HealthExplanation
	}
}

// overviewMeterFor builds one meter from a gauge.
//
// The gauge already knows whether it has a reading (gaugeFromUsedTotal
// and gaugeFromLoadAverage both return the "unknown" state rather than
// 0% for a missing total), so the meter inherits that answer rather
// than re-deriving it.
func overviewMeterFor(label string, gauge gaugeView, threshold float64) overviewMeter {
	meter := overviewMeter{Label: label, Detail: gauge.Detail}
	if gauge.State == "unknown" {
		meter.Known = false
		if meter.Detail == "" {
			meter.Detail = "this reading could not be observed"
		}
		return meter
	}
	meter.Known = true
	meter.Percent = gauge.Percent
	meter.Over = float64(gauge.Percent) > threshold
	meter.Warn = meter.Over
	return meter
}

// overviewZFSMeter builds the ZFS pool meter from a Comb's pools.
//
// The reading is the WORST pool's capacity, not an average: an average
// of a 10%-full and a 99%-full pool reads as a comfortable 55%, which
// is exactly the "easy to see what is wrong" failure the design is
// against.
//
// A Comb with NO pools is NOT 0%. It has no ZFS pool to report on,
// which is a different fact from a pool that is empty, so the meter
// reads unknown with a reason.
func overviewZFSMeter(node clusterNodeView) overviewMeter {
	meter := overviewMeter{Label: "ZFS"}
	if !node.Reachable {
		meter.Detail = "this Comb could not be reached"
		return meter
	}
	if !node.ZFSKnown {
		meter.Detail = "this Comb reports no ZFS pool, so there is no pool usage to report"
		return meter
	}
	meter.Known = true
	meter.Percent = node.ZFSPct
	meter.Over = float64(node.ZFSPct) > zfsWarnPct
	meter.Warn = meter.Over
	meter.Detail = fmt.Sprintf("pool %s is %d%% used", node.ZFSPool, node.ZFSPct)
	return meter
}

// overviewChecksFor builds the matrix's columns for one Comb.
//
// THE HANDOFF ASKED FOR TEN COLUMNS AND NINE OF THEM HAVE NO EVIDENCE
// SOURCE TODAY. It said: "Ship only the columns that have a real
// evidence source today, and list the dropped ones in the PR."
//
// The columns that ship are the four Apiary can actually observe for
// any Comb:
//
//	Reachable - could this Comb be dialed just now
//	Raft      - is this Comb's own raft answering
//	Memory    - the memory reading, against its threshold
//	ZFS       - the pool reading, against its threshold
//
// The six dropped columns (HAST, PF, Network, TLS, Time, Reconciler)
// are per-Comb facts that either have no Colony-wide RPC at all
// (TLS, Time), exist only for the Comb this frontend is colocated
// with (PF), or are per-RESOURCE rather than per-Comb (HAST). A
// column of empty dots would be noise that looks like evidence.
func overviewChecksFor(node clusterNodeView, digestColony stateDigestColony) []overviewCheck {
	checks := []overviewCheck{}

	reachable := overviewCheck{Name: "Reachable", State: statusOK, Detail: "this Comb answered a direct call just now"}
	if !node.Reachable {
		reachable.State = statusCritical
		reachable.Detail = "this Comb could not be reached: " + node.Error
	}
	checks = append(checks, reachable)

	raft := overviewCheck{Name: "Raft", State: statusOK, Detail: "this Comb's own raft answered"}
	switch {
	case node.DigestState == string(statedigest.Match):
		raft.State = statusOK
		raft.Detail = "this Comb's FSM state digest agrees with the other voters'"
	case node.DigestState == string(statedigest.Mismatch):
		raft.State = statusContradictory
		raft.Detail = node.DigestDetail
	case node.DigestState == string(statedigest.Unsettled):
		raft.State = statusWarn
		raft.Detail = node.DigestDetail
	default:
		raft.State = statusUnknown
		raft.Detail = "this Comb's FSM state digest could not be compared: " + node.DigestDetail
	}
	checks = append(checks, raft)

	memory := overviewCheck{Name: "Memory", State: statusOK, Detail: "memory is within its threshold"}
	switch {
	case !node.Reachable:
		memory.State = statusUnknown
		memory.Detail = "this Comb could not be reached, so its memory could not be read"
	case node.MemGauge.State == "unknown":
		memory.State = statusUnknown
		memory.Detail = node.MemGauge.Detail
	case node.MemGauge.State == "critical":
		memory.State = statusCritical
		memory.Detail = node.MemGauge.Detail
	case node.MemGauge.State == "warn":
		memory.State = statusWarn
		memory.Detail = node.MemGauge.Detail
	}
	checks = append(checks, memory)

	zfs := overviewCheck{Name: "ZFS", State: statusOK, Detail: "pool usage is within its threshold"}
	switch {
	case !node.Reachable:
		zfs.State = statusUnknown
		zfs.Detail = "this Comb could not be reached, so its pools could not be read"
	default:
		meter := overviewZFSMeter(node)
		switch {
		case !meter.Known:
			zfs.State = statusUnknown
			zfs.Detail = meter.Detail
		case meter.Warn:
			zfs.State = statusWarn
			zfs.Detail = meter.Detail
		}
	}
	checks = append(checks, zfs)

	return checks
}

// overviewCellIsRunning reports whether a cell is running, for the
// card's struck-through stopped cells.
func overviewCellIsRunning(cell overviewCell) bool { return !cell.Stopped }
