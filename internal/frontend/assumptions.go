package frontend

import (
	"context"
	"net/http"
	"sort"
	"sync"
	"time"

	rpcpb "github.com/glenjbarber/apiary/api/rpc"
)

// assumptionResultView is the template-facing shape for one
// AssumptionResult (ADR-0055) - translated once here, the same
// convention convert.go/simulate.go already follow for their own
// response types.
type assumptionResultView struct {
	Kind           string
	SubjectKind    string
	SubjectID      string
	DependencyID   string
	Qualifier      string
	ObservedStatus string // raw, as last measured - diagnostic only
	Status         string // EFFECTIVE status - already safe to render as-is, see Server field's own doc comment
	ReasonCode     string
	Detail         string
	LastObservedAt string
	Stale          bool
}

// nodeAssumptionsView is one node's section on the "/assumptions" page.
// Results and StaleResults are the same LatestPerKey snapshot split by
// .Stale - a stale entry (e.g. a check keyed on an uplink or peer this
// node no longer has, its key simply never recomputed since) is real,
// honest data, not a bug, but rendering it inline with genuinely
// current results reads as duplicated current state. Grouped
// separately, with StaleResults collapsed by default in the template.
type nodeAssumptionsView struct {
	NodeID                string
	Error                 string
	StorageDegraded       bool
	StorageDegradedDetail string
	Results               []assumptionResultView
	StaleResults          []assumptionResultView
}

// AllGreen summarizes current effective observations only. Historical stale
// results remain available separately and do not override current evidence.
func (n nodeAssumptionsView) AllGreen() bool {
	if n.Error != "" || n.StorageDegraded || len(n.Results) == 0 {
		return false
	}
	for _, r := range n.Results {
		if r.Stale || (r.Status != "true" && r.Status != "not_applicable") {
			return false
		}
	}
	return true
}

func fromRPCAssumptionStatus(s rpcpb.AssumptionStatus) string {
	switch s {
	case rpcpb.AssumptionStatus_ASSUMPTION_STATUS_TRUE:
		return "true"
	case rpcpb.AssumptionStatus_ASSUMPTION_STATUS_FALSE:
		return "false"
	case rpcpb.AssumptionStatus_ASSUMPTION_STATUS_NOT_APPLICABLE:
		return "not_applicable"
	default:
		return "unknown"
	}
}

func fromRPCAssumptionKindLabel(k rpcpb.AssumptionKind) string {
	switch k {
	case rpcpb.AssumptionKind_ASSUMPTION_KIND_PEER_MANAGER_RPC_SUCCEEDED:
		return "peer manager RPC succeeded"
	case rpcpb.AssumptionKind_ASSUMPTION_KIND_PEER_SECURITY_PATH_ACCEPTED:
		return "peer security path accepted"
	case rpcpb.AssumptionKind_ASSUMPTION_KIND_NAT_UPLINK_DEFAULT_ROUTE:
		return "NAT uplink owns default route"
	case rpcpb.AssumptionKind_ASSUMPTION_KIND_REPLICA_BHYVE_CONFIGURED:
		return "replica has bhyve configured"
	case rpcpb.AssumptionKind_ASSUMPTION_KIND_REPLICA_NETWORK_BRIDGE_UP:
		return "replica's network bridge is up"
	default:
		return "unspecified"
	}
}

func fromRPCAssumptionSubjectKind(k rpcpb.AssumptionSubjectKind) string {
	switch k {
	case rpcpb.AssumptionSubjectKind_ASSUMPTION_SUBJECT_KIND_NODE:
		return "node"
	case rpcpb.AssumptionSubjectKind_ASSUMPTION_SUBJECT_KIND_VM:
		return "vm"
	case rpcpb.AssumptionSubjectKind_ASSUMPTION_SUBJECT_KIND_JAIL:
		return "jail"
	default:
		return ""
	}
}

func fromRPCAssumptionResult(r *rpcpb.AssumptionResult) assumptionResultView {
	k := r.GetKey()
	return assumptionResultView{
		Kind:           fromRPCAssumptionKindLabel(k.GetKind()),
		SubjectKind:    fromRPCAssumptionSubjectKind(k.GetSubjectKind()),
		SubjectID:      k.GetSubjectId(),
		DependencyID:   k.GetDependencyId(),
		Qualifier:      k.GetQualifier(),
		ObservedStatus: fromRPCAssumptionStatus(r.GetObservedStatus()),
		Status:         fromRPCAssumptionStatus(r.GetStatus()),
		ReasonCode:     r.GetReasonCode(),
		Detail:         r.GetDetail(),
		LastObservedAt: time.Unix(r.GetLastObservedAtUnix(), 0).Local().Format("2006-01-02 15:04:05 MST"),
		Stale:          r.GetStale(),
	}
}

// nodeAssumptions fetches nodeID's ListAssumptionResults: directly
// through s.client if nodeID is this frontend's own colocated node (or
// peers isn't configured at all), otherwise by dialing that node's
// managerd directly via s.peers - the same locality pattern
// nodeHostStats (cluster_overview.go, ADR-0036) already established,
// since this is node-local physical data with no leader/forwarding
// concept to lean on.
func (s *Server) nodeAssumptions(ctx context.Context, nodeID, localNodeID string) nodeAssumptionsView {
	var resp *rpcpb.ListAssumptionResultsResponse
	var err error
	if s.peers == nil || nodeID == localNodeID {
		resp, err = s.client.ListAssumptionResults(ctx, &rpcpb.ListAssumptionResultsRequest{})
	} else {
		resp, err = s.peers.ListAssumptionResults(ctx, s.peerAddr(nodeID), &rpcpb.ListAssumptionResultsRequest{})
	}
	if err != nil {
		return nodeAssumptionsView{NodeID: nodeID, Error: err.Error()}
	}
	if resp.GetError() != "" {
		return nodeAssumptionsView{NodeID: nodeID, Error: resp.GetError()}
	}

	sortResults := func(results []assumptionResultView) {
		sort.Slice(results, func(i, j int) bool {
			if results[i].Kind != results[j].Kind {
				return results[i].Kind < results[j].Kind
			}
			if results[i].SubjectID != results[j].SubjectID {
				return results[i].SubjectID < results[j].SubjectID
			}
			return results[i].DependencyID < results[j].DependencyID
		})
	}

	var current, stale []assumptionResultView
	for _, r := range resp.GetLatest() {
		v := fromRPCAssumptionResult(r)
		if v.Stale {
			stale = append(stale, v)
		} else {
			current = append(current, v)
		}
	}
	sortResults(current)
	sortResults(stale)

	return nodeAssumptionsView{
		NodeID: nodeID, Results: current, StaleResults: stale,
		StorageDegraded: resp.GetStorageDegraded(), StorageDegradedDetail: resp.GetStorageDegradedDetail(),
	}
}

// handleAssumptionsPage serves the Automated Assumption Checks page
// ("/assumptions", ADR-0055): one section per known node, fetched
// concurrently (mirrors handleClusterOverviewPage's own fan-out) since
// one unreachable node shouldn't hold up every other node's section.
// Complementary to the separate, operator-authored Assumption Register
// (assumption_register.go) - this page shows only the current
// (`latest`) automated view, no history drill-down.
func (s *Server) handleAssumptionsPage(w http.ResponseWriter, r *http.Request) {
	statusResp, err := s.client.Status(r.Context(), &rpcpb.StatusRequest{})
	if err != nil {
		s.render(w, "assumptions_page", s.withAuthFields(r, pageData{Error: err.Error(), ActivePage: "assumptions"}))
		return
	}

	localNodeID := statusResp.GetManagerNodeId()
	nodeIDs := statusResp.GetKnownNodeIds()
	if len(nodeIDs) == 0 && localNodeID != "" {
		nodeIDs = []string{localNodeID}
	}

	nodes := make([]nodeAssumptionsView, len(nodeIDs))
	var wg sync.WaitGroup
	for i, id := range nodeIDs {
		wg.Add(1)
		go func(i int, id string) {
			defer wg.Done()
			nodes[i] = s.nodeAssumptions(r.Context(), id, localNodeID)
		}(i, id)
	}
	wg.Wait()

	sort.Slice(nodes, func(i, j int) bool { return nodes[i].NodeID < nodes[j].NodeID })

	s.render(w, "assumptions_page", s.withAuthFieldsFrom(r, pageData{AssumptionNodes: nodes, ActivePage: "assumptions"}, statusResp, nil))
}

// handlePurgeStaleAssumptionResults clears one node's own stale
// current-snapshot entries (ADR-0055's "superseded/stale result(s)"
// group the page already displays), then re-renders the page exactly
// as handleAssumptionsPage does - node_id names which node's section
// to act on, since this is per-node local data with no cluster-wide
// concept, the same locality nodeAssumptions already follows for
// reading it.
func (s *Server) handlePurgeStaleAssumptionResults(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		s.render(w, "assumptions_page", s.withAuthFields(r, pageData{Error: err.Error(), ActivePage: "assumptions"}))
		return
	}
	nodeID := r.FormValue("node_id")

	statusResp, err := s.client.Status(r.Context(), &rpcpb.StatusRequest{})
	if err != nil {
		s.render(w, "assumptions_page", s.withAuthFields(r, pageData{Error: err.Error(), ActivePage: "assumptions"}))
		return
	}
	localNodeID := statusResp.GetManagerNodeId()

	var resp *rpcpb.PurgeStaleAssumptionResultsResponse
	var rpcErr error
	if s.peers == nil || nodeID == localNodeID {
		resp, rpcErr = s.client.PurgeStaleAssumptionResults(r.Context(), &rpcpb.PurgeStaleAssumptionResultsRequest{})
	} else {
		resp, rpcErr = s.peers.PurgeStaleAssumptionResults(r.Context(), s.peerAddr(nodeID), &rpcpb.PurgeStaleAssumptionResultsRequest{})
	}
	var purgeErr string
	if rpcErr != nil {
		purgeErr = rpcErr.Error()
	} else if resp.GetError() != "" {
		purgeErr = resp.GetError()
	}

	nodeIDs := statusResp.GetKnownNodeIds()
	if len(nodeIDs) == 0 && localNodeID != "" {
		nodeIDs = []string{localNodeID}
	}
	nodes := make([]nodeAssumptionsView, len(nodeIDs))
	var wg sync.WaitGroup
	for i, id := range nodeIDs {
		wg.Add(1)
		go func(i int, id string) {
			defer wg.Done()
			nodes[i] = s.nodeAssumptions(r.Context(), id, localNodeID)
		}(i, id)
	}
	wg.Wait()
	sort.Slice(nodes, func(i, j int) bool { return nodes[i].NodeID < nodes[j].NodeID })

	s.render(w, "assumptions_page", s.withAuthFieldsFrom(r, pageData{AssumptionNodes: nodes, ActivePage: "assumptions", Error: purgeErr}, statusResp, nil))
}
