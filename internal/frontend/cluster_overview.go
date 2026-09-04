package frontend

import (
	"context"
	"net/http"
	"sort"
	"sync"
	"time"

	rpcpb "github.com/glenjbarber/apiary/api/rpc"
	"github.com/glenjbarber/apiary/internal/health"
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

	// ListISOs lets the create-VM/create-jail forms build a cluster-wide
	// image picker (ADR-0041) reaching any node, not just the one this
	// frontend is colocated with - the same "forward a plain external
	// RPC to an arbitrary peer" shape HostStats already established,
	// kept on this same interface rather than a second one since both
	// are satisfied by the same *manager.PeerReporter value.
	ListISOs(ctx context.Context, addr string) (*rpcpb.ListISOsResponse, error)

	// ListAssumptionResults lets the "/assumptions" page (ADR-0055) fan
	// out to every known node, the same "forward a plain external RPC to
	// an arbitrary peer" shape as HostStats/ListISOs above.
	ListAssumptionResults(ctx context.Context, addr string, req *rpcpb.ListAssumptionResultsRequest) (*rpcpb.ListAssumptionResultsResponse, error)

	// Status lets Evidence-Aware Health (ADR-0056) learn a peer's own
	// raft applied/last-log index and its own raft_reachable heartbeat -
	// the same "always answers locally, dial addr directly" shape as
	// HostStats above, not leader-forwarding (Status has no leader
	// concept to route through).
	Status(ctx context.Context, addr string) (*rpcpb.StatusResponse, error)
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

	// HealthStatus/HealthExplanation/HealthObservations are Evidence-
	// Aware Health's (ADR-0056) computed verdict for this node - additive
	// alongside the fields above, which are left untouched.
	HealthStatus       health.Status
	HealthExplanation  string
	HealthObservations []health.Observation
}

// peerAddr turns a node ID into the address its managerd should be
// reachable at - see cmd/frontend's own -peer-hostname-suffix/
// -peer-manager-port flags.
func (s *Server) peerAddr(nodeID string) string {
	return nodeID + s.peerHostnameSuffix + ":" + s.peerManagerPort
}

// fetchHostStats dials nodeID's own managerd for a fresh HostStats
// response: directly through s.client if nodeID is this frontend's own
// colocated node (or peers isn't configured at all), otherwise by
// dialing that node's managerd directly via s.peers. Returns the raw
// response so a caller needing more than statsView's own converted
// fields (Evidence-Aware Health's reconcile signals, ADR-0056) doesn't
// need a second call.
func (s *Server) fetchHostStats(ctx context.Context, nodeID, localNodeID string) (*rpcpb.HostStatsResponse, error) {
	if s.peers == nil || nodeID == localNodeID {
		return s.client.HostStats(ctx, &rpcpb.HostStatsRequest{})
	}
	return s.peers.HostStats(ctx, s.peerAddr(nodeID))
}

// nodeHostStats fetches nodeID's HostStats and converts it to the
// template-facing statsView shape used by "/host/{id}".
func (s *Server) nodeHostStats(ctx context.Context, nodeID, localNodeID string) (statsView, string) {
	resp, err := s.fetchHostStats(ctx, nodeID, localNodeID)
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

// nodeHealthSignals gathers Evidence-Aware Health's (ADR-0056) raw
// NodeSignals for one node. Membership/suffrage always come from anchor
// - the ONE Status() call already fetched once at the top of
// handleClusterOverviewPage, shared across every node's signals - never
// a per-node membership read, since raft membership is a cluster-wide-
// consistent replicated fact (see internal/health's own doc comment).
// hostStats/hostStatsErr is whatever this same request already fetched
// for the row's basic stats, threaded through rather than re-fetched.
// For a non-local node with peer forwarding configured, one additional
// Status() call supplies that node's own applied/last-log index and
// heartbeat; for the local node these come from anchor itself, with no
// second self-dial.
func (s *Server) nodeHealthSignals(ctx context.Context, nodeID, localNodeID string, anchor *rpcpb.StatusResponse, hostStats *rpcpb.HostStatsResponse, hostStatsErr error, now time.Time) health.NodeSignals {
	sig := health.NodeSignals{NodeID: nodeID, MembershipObservedAt: now}

	sig.MembershipObserved = anchor.GetRaftReachable()
	if sig.MembershipObserved {
		for _, m := range anchor.GetMembers() {
			if m.GetNodeId() == nodeID {
				sig.IsRaftMember = true
				sig.Suffrage = health.ParseSuffrage(m.GetSuffrage())
				break
			}
		}
	}

	switch {
	case nodeID == localNodeID:
		sig.PeerReachability = health.ReachabilityReachable
	case s.peers == nil:
		// No peer forwarding configured at all - fetchHostStats above
		// silently fell back to this node's own local client (a
		// pre-existing quirk, not introduced here), so its result says
		// nothing trustworthy about the actual remote node.
		sig.PeerReachability = health.ReachabilityUnknown
	case hostStatsErr == nil:
		sig.PeerReachability = health.ReachabilityReachable
	default:
		sig.PeerReachability = health.ReachabilityUnreachable
	}

	var peerStatus *rpcpb.StatusResponse
	switch {
	case nodeID == localNodeID:
		peerStatus = anchor
	case s.peers != nil:
		if resp, err := s.peers.Status(ctx, s.peerAddr(nodeID)); err == nil {
			peerStatus = resp
		}
	}
	if peerStatus != nil {
		sig.HeartbeatObserved = true
		sig.HeartbeatOK = peerStatus.GetRaftReachable()
		if peerStatus.GetRaftReachable() {
			sig.AppliedIndexObserved = true
			sig.AppliedIndex = peerStatus.GetRaftAppliedIndex()
			sig.LastLogIndex = peerStatus.GetRaftLastLogIndex()
			sig.IndicesObservedAt = now
		}
	}

	if hostStatsErr == nil && hostStats != nil {
		sig.ReconcileObservedAt = now
		sig.ReconcileIntervalSeconds = hostStats.GetReconcileIntervalSeconds()
		// reconcile_interval_seconds == 0 is the only reliable "no
		// Reconciler configured on this node" signal - a 0 timestamp
		// alone can't distinguish that from "configured but no tick yet"
		// (see HostStatsResponse's own doc comment).
		sig.ReconcilerConfigured = sig.ReconcileIntervalSeconds > 0
		if unix := hostStats.GetLastReconcileAttemptUnix(); unix > 0 {
			sig.ReconcileEverAttempted = true
			sig.LastReconcileAttempt = time.Unix(unix, 0)
		}
		if unix := hostStats.GetLastReconcileSuccessUnix(); unix > 0 {
			sig.ReconcileEverSucceeded = true
			sig.LastReconcileSuccess = time.Unix(unix, 0)
		}
	}

	return sig
}

// handleClusterOverviewPage serves the default landing page ("/"): a
// basic-status row per known cluster node, fetched concurrently since
// one unreachable node shouldn't hold up every other node's row. Each
// row also carries an Evidence-Aware Health (ADR-0056) verdict computed
// from the same fetch, rather than trusting the basic Reachable/Error
// fields alone.
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
			now := time.Now()
			hostStats, hostStatsErr := s.fetchHostStats(r.Context(), id, localNodeID)
			var stats statsView
			var errMsg string
			if hostStatsErr != nil {
				errMsg = hostStatsErr.Error()
			} else {
				stats = fromRPCStats(hostStats)
			}
			node := summarizeClusterNode(id, stats, errMsg)

			signals := s.nodeHealthSignals(r.Context(), id, localNodeID, statusResp, hostStats, hostStatsErr, now)
			result := health.ComputeNodeHealth(signals, now)
			node.HealthStatus = result.Status
			node.HealthExplanation = result.Explanation
			node.HealthObservations = result.Observations

			nodes[i] = node
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
