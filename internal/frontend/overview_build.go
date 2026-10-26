package frontend

import (
	"context"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	rpcpb "github.com/glenjbarber/apiary/api/rpc"
	"github.com/glenjbarber/apiary/internal/statedigest"
)

// Building the Blend overview (handoff section 7).
//
// EVERY NUMBER HERE COMES FROM AN OBSERVATION. Where a value could not
// be read, the field says so rather than substituting a zero. The
// gathering is deliberately fail-soft: a Comb that cannot be dialed
// appears in the Combs list as unknown, and the rest of the Colony
// still renders.
//
// THE GATHER IS DONE ONCE PER REQUEST and the result is shared by the
// full page and by every refreshing fragment, so the status line, the
// Needs-attention list, and the Combs section can never disagree with
// each other.

// overviewBundle is everything the overview renders, gathered once.
type overviewBundle struct {
	Status    overviewStatusLine
	Chip      overviewHealthChip
	Attention []overviewAttentionItem
	Combs     overviewCombView
	// ColonyName is the Colony's name for the top bar, short enough
	// to sit on one line. ColonyNameFull is the identifier it was
	// taken from, for the title attribute; see colonyTitle.
	ColonyName     string
	ColonyNameFull string
	// Warnings is the warning count, which the status line shows and
	// the chip deliberately does not (handoff 7.1).
	Warnings int
	// Error is a page-level failure. The page still renders - an
	// overview that says "the evidence could not be read" is more
	// useful than an error page.
	Error string
}

// gatherOverview gathers everything the overview renders.
//
// statusResp is the Status response the page already has; passing it in
// avoids a second RPC. A nil status is not an error here - it means
// "the Colony has no valid observation", which the chip renders as
// Unknown.
func (s *Server) gatherOverview(ctx context.Context, r *http.Request, statusResp *rpcpb.StatusResponse, statusErr error) overviewBundle {
	now := time.Now()
	bundle := overviewBundle{}

	if statusErr != nil {
		bundle.Error = statusErr.Error()
	}
	if statusResp != nil {
		bundle.ColonyName, bundle.ColonyNameFull = colonyTitle(statusResp.GetRaftColonyId(), statusResp.GetManagerNodeId())
	}

	localNodeID := ""
	if statusResp != nil {
		localNodeID = statusResp.GetManagerNodeId()
	}
	nodeIDs := []string{}
	if statusResp != nil {
		nodeIDs = append(nodeIDs, statusResp.GetKnownNodeIds()...)
	}
	if len(nodeIDs) == 0 && localNodeID != "" {
		nodeIDs = []string{localNodeID}
	}
	sort.Strings(nodeIDs)

	// The one cluster-wide resource read, shared by every Comb's row
	// and by the Cells count - two reads would put one Comb's cells
	// from before a change and another's from after it.
	index := s.gatherCombCauseIndex(ctx)

	// The per-Comb evidence, gathered concurrently because one
	// unreachable Comb must not hold up every other Comb's row.
	nodes := make([]clusterNodeView, len(nodeIDs))
	var wg sync.WaitGroup
	for i, id := range nodeIDs {
		wg.Add(1)
		go func(i int, id string) {
			defer wg.Done()
			nodes[i] = s.clusterNodeEvidence(ctx, id, localNodeID, statusResp, index, now)
		}(i, id)
	}
	wg.Wait()
	sort.Slice(nodes, func(i, j int) bool { return nodes[i].NodeID < nodes[j].NodeID })

	// The colony-wide FSM agreement verdict, computed once from the
	// rows above - the same way the Command Center does it, so the two
	// surfaces cannot disagree.
	observations := make([]statedigest.Observation, len(nodes))
	for i, node := range nodes {
		observations[i] = statedigest.Observation{
			NodeID: node.NodeID, Digest: node.StateDigest, AppliedIndex: node.AppliedIndex,
		}
	}
	digestViews, digestColony := stateDigestVerdicts(observations)
	for i := range nodes {
		view, found := digestViews[nodes[i].NodeID]
		if !found {
			view = stateDigestUnobservedView(statedigest.Observation{NodeID: nodes[i].NodeID}, len(nodes))
		}
		nodes[i].DigestState = view.State
		nodes[i].DigestBadgeClass = view.BadgeClass
		nodes[i].DigestBadgeLabel = view.Label
		nodes[i].DigestDetail = view.Detail
	}

	leader := colonyLeaderFromStatus(statusResp, statusErr, now)
	leaderID := ""
	if leader.Observed && leader.State == colonyLeaderKnown {
		leaderID = leader.NodeID
	}

	cellsByComb := s.overviewCellsByComb(ctx, index)

	// The Combs section.
	bundle.Combs = overviewCombViewFromNodes(nodes, leaderID, cellsByComb, digestColony)

	// The status line, the chip, and the Needs-attention list are all
	// derived from the SAME rows, so they cannot disagree.
	bundle.Warnings = s.overviewWarningCount(nodes)
	hastSummary := s.overviewHASTSummary(ctx, index, localNodeID)
	bundle.Status = s.overviewStatusLineFrom(nodes, leader, bundle.Combs, hastSummary, bundle.Warnings, now)
	bundle.Chip = overviewChipFrom(nodes, bundle.Warnings, statusResp != nil, statusErr)
	bundle.Attention = s.overviewAttentionFrom(r, s.overviewCanAdmin(r), nodes, bundle.Combs, index, leader, now)
	return bundle
}

// colonyTitle returns the top bar's Colony name and the full identifier
// behind it.
//
// A Colony has no human-chosen name today, only raft_colony_id, which
// is a UUID. Shown whole, a UUID is 36 characters of noise in the one
// place the operator looks first, so the bar shows "Colony" and the
// UUID's first group ("Colony e5130fb4"), and the title attribute
// carries the whole ID for anyone who needs to match it.
//
// A Colony with no identity yet is a real state, not an error
// (raft_colony_id is empty until a node has a Colony identity). The
// bar then names the Comb that is answering, so it is never blank.
func colonyTitle(colonyID, nodeID string) (name, full string) {
	if colonyID == "" {
		return nodeID, nodeID
	}
	short := colonyID
	if i := strings.IndexByte(short, '-'); i > 0 {
		short = short[:i]
	}
	if len(short) > 8 {
		short = short[:8]
	}
	return "Colony " + short, colonyID
}
