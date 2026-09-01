package frontend

import (
	"context"
	"net/http"
	"sort"
	"sync"

	rpcpb "github.com/glenjbarber/apiary/api/rpc"
)

// peerHostStatsClient is the subset of *manager.PeerReporter the
// server needs to fetch another node's HostStats directly, defined
// locally for the same reason as isoManager/VNCLookup/etc.
// HostStats always answers only for whoever receives the call - unlike
// ListVMs/GetVM/etc. (ADR-0035), there's no leader/forwarding concept
// to lean on, so reaching a node other than the one this frontend is
// colocated with means dialing that node's managerd directly.
type peerHostStatsClient interface {
	HostStats(ctx context.Context, addr string) (*rpcpb.HostStatsResponse, error)
}

// clusterNodeView is the template-facing shape for one row on the
// cluster overview page ("/") - a lightweight summary, deliberately
// much smaller than the verbose per-node statsView the "/host/{id}"
// page shows.
type clusterNodeView struct {
	NodeID     string
	Reachable  bool
	Error      string
	LoadAvg1   float64
	MemUsedPct float64
	PoolsOK    bool
	PFEnabled  bool
}

// peerAddr turns a node ID into the address its managerd should be
// reachable at - see cmd/frontend's own -peer-hostname-suffix/
// -peer-manager-port flags.
func (s *Server) peerAddr(nodeID string) string {
	return nodeID + s.peerHostnameSuffix + ":" + s.peerManagerPort
}

// nodeHostStats fetches nodeID's HostStats: directly through s.client
// if nodeID is this frontend's own colocated node (or peers isn't
// configured at all), otherwise by dialing that node's managerd
// directly via s.peers.
func (s *Server) nodeHostStats(ctx context.Context, nodeID, localNodeID string) (statsView, string) {
	if s.peers == nil || nodeID == localNodeID {
		resp, err := s.client.HostStats(ctx, &rpcpb.HostStatsRequest{})
		if err != nil {
			return statsView{}, err.Error()
		}
		return fromRPCStats(resp), ""
	}
	resp, err := s.peers.HostStats(ctx, s.peerAddr(nodeID))
	if err != nil {
		return statsView{}, err.Error()
	}
	return fromRPCStats(resp), ""
}

// summarizeClusterNode reduces a full statsView down to the cluster
// overview's basic-status row - PoolsOK is true only if every pool
// reports ONLINE (vacuously true with no pools at all, matching this
// project's own "no pools" empty-state convention elsewhere).
func summarizeClusterNode(nodeID string, stats statsView, fetchErr string) clusterNodeView {
	if fetchErr != "" {
		return clusterNodeView{NodeID: nodeID, Error: fetchErr}
	}
	poolsOK := true
	for _, p := range stats.Pools {
		if p.Health != "ONLINE" {
			poolsOK = false
			break
		}
	}
	return clusterNodeView{
		NodeID:     nodeID,
		Reachable:  true,
		LoadAvg1:   stats.LoadAvg1,
		MemUsedPct: stats.MemUsedPct,
		PoolsOK:    poolsOK,
		PFEnabled:  stats.PF.Enabled,
	}
}

// handleClusterOverviewPage serves the default landing page ("/"): a
// basic-status row per known cluster node, fetched concurrently since
// one unreachable node shouldn't hold up every other node's row.
func (s *Server) handleClusterOverviewPage(w http.ResponseWriter, r *http.Request) {
	statusResp, err := s.client.Status(r.Context(), &rpcpb.StatusRequest{})
	if err != nil {
		s.render(w, "cluster_overview_page", s.withAuthFields(r, pageData{Error: err.Error(), ActivePage: "stats"}))
		return
	}

	localNodeID := statusResp.GetManagerNodeId()
	nodeIDs := statusResp.GetKnownNodeIds()
	if len(nodeIDs) == 0 && localNodeID != "" {
		nodeIDs = []string{localNodeID}
	}

	nodes := make([]clusterNodeView, len(nodeIDs))
	var wg sync.WaitGroup
	for i, id := range nodeIDs {
		wg.Add(1)
		go func(i int, id string) {
			defer wg.Done()
			stats, errMsg := s.nodeHostStats(r.Context(), id, localNodeID)
			nodes[i] = summarizeClusterNode(id, stats, errMsg)
		}(i, id)
	}
	wg.Wait()

	sort.Slice(nodes, func(i, j int) bool { return nodes[i].NodeID < nodes[j].NodeID })

	s.render(w, "cluster_overview_page", s.withAuthFields(r, pageData{ClusterNodes: nodes, ActivePage: "stats"}))
}

// handleHostPage serves the verbose per-node stats page
// ("/host/{id}") - the same detail the old single-node "/" page used
// to show, now addressable per node.
func (s *Server) handleHostPage(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	localNodeID := ""
	if statusResp, err := s.client.Status(r.Context(), &rpcpb.StatusRequest{}); err == nil {
		localNodeID = statusResp.GetManagerNodeId()
	}

	stats, errMsg := s.nodeHostStats(r.Context(), id, localNodeID)
	if stats.NodeID == "" {
		stats.NodeID = id
	}
	s.render(w, "host_page", s.withAuthFields(r, pageData{Error: errMsg, Stats: stats, ActivePage: "stats"}))
}
