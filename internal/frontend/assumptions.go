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
type nodeAssumptionsView struct {
	NodeID                string
	Error                 string
	StorageDegraded       bool
	StorageDegradedDetail string
	Results               []assumptionResultView
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

	results := make([]assumptionResultView, 0, len(resp.GetLatest()))
	for _, r := range resp.GetLatest() {
		results = append(results, fromRPCAssumptionResult(r))
	}
	sort.Slice(results, func(i, j int) bool {
		if results[i].Kind != results[j].Kind {
			return results[i].Kind < results[j].Kind
		}
		if results[i].SubjectID != results[j].SubjectID {
			return results[i].SubjectID < results[j].SubjectID
		}
		return results[i].DependencyID < results[j].DependencyID
	})

	return nodeAssumptionsView{
		NodeID: nodeID, Results: results,
		StorageDegraded: resp.GetStorageDegraded(), StorageDegradedDetail: resp.GetStorageDegradedDetail(),
	}
}

// handleAssumptionsPage serves the Automated Assumption Checks page
// ("/assumptions", ADR-0055): one section per known node, fetched
// concurrently (mirrors handleClusterOverviewPage's own fan-out) since
// one unreachable node shouldn't hold up every other node's section.
// This is a scoped precursor to CODEX.md's fuller Assumption Register -
// v1 shows only the current (`latest`) view, no history drill-down.
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

	s.render(w, "assumptions_page", s.withAuthFields(r, pageData{AssumptionNodes: nodes, ActivePage: "assumptions"}))
}
