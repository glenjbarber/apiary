package frontend

import (
	"context"
	"io"
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

	// PurgeStaleAssumptionResults lets the "/assumptions" page's own
	// clear-stale action (ADR-0106) reach any known node, not only the
	// one this frontend is colocated with - same shape as
	// ListAssumptionResults above.
	PurgeStaleAssumptionResults(ctx context.Context, addr string, req *rpcpb.PurgeStaleAssumptionResultsRequest) (*rpcpb.PurgeStaleAssumptionResultsResponse, error)

	// Status lets Evidence-Aware Health (ADR-0056) learn a peer's own
	// raft applied/last-log index and its own raft_reachable heartbeat -
	// the same "always answers locally, dial addr directly" shape as
	// HostStats above, not leader-forwarding (Status has no leader
	// concept to route through).
	Status(ctx context.Context, addr string) (*rpcpb.StatusResponse, error)

	// GetLocalNetworkBridgeStatus lets Operational Invariants (ADR-0060)
	// learn a specific node's own local bridge state for a network it
	// hosts a resource on - the same "always answers locally, dial addr
	// directly" shape as HostStats/Status above. Deliberately never
	// ListNetworks's own bridge_status field, which is populated by
	// whichever node answers that leader-only, forwarding RPC and would
	// silently mislabel the LEADER's bridge state as this node's own -
	// see GetLocalNetworkBridgeStatus's own doc comment in
	// internal/manager/server.go for the ADR-0055 bug this avoids.
	GetLocalNetworkBridgeStatus(ctx context.Context, addr, networkID string) (*rpcpb.GetLocalNetworkBridgeStatusResponse, error)

	// GetVMSerialLog reaches the Hive that owns a VM. Serial capture is
	// deliberately local to the bhyve host, so the frontend must not ask a
	// non-owning managerd and mistake its ownership hint for a missing log.
	GetVMSerialLog(ctx context.Context, addr, id string) (*rpcpb.GetVMSerialLogResponse, error)
	GetVMConsole(ctx context.Context, addr, id string) (*rpcpb.GetVMConsoleResponse, error)
	OpenVMConsole(ctx context.Context, addr, id string) (io.ReadWriteCloser, error)
	GetNodeConfig(ctx context.Context, addr string) (*rpcpb.GetNodeConfigResponse, error)

	// GetNetworkTeardownStatus lets the guided network-replacement
	// workflow (ADR-0071/ADR-0081) learn a specific node's own local
	// artifact-cleanup status for a deleted network id - the same
	// "always answers locally, dial addr directly" shape as
	// GetLocalNetworkBridgeStatus above.
	GetNetworkTeardownStatus(ctx context.Context, addr, networkID string) (*rpcpb.GetNetworkTeardownStatusResponse, error)

	// CreateVMSnapshot/ListVMSnapshots/RestoreVMSnapshot/DeleteVMSnapshot
	// (ADR-0090) reach the Hive that owns a VM - a snapshot is real
	// local ZFS state, the same "must ask the owning Comb directly"
	// reasoning as GetVMSerialLog above.
	CreateVMSnapshot(ctx context.Context, addr, id, snapshotName string) (*rpcpb.CreateVMSnapshotResponse, error)
	ListVMSnapshots(ctx context.Context, addr, id string) (*rpcpb.ListVMSnapshotsResponse, error)
	RestoreVMSnapshot(ctx context.Context, addr, id, snapshotName string) (*rpcpb.RestoreVMSnapshotResponse, error)
	DeleteVMSnapshot(ctx context.Context, addr, id, snapshotName string) (*rpcpb.DeleteVMSnapshotResponse, error)

	// GetJoinRequestStatus (ADR-0092) polls a join request that lives on
	// a different Colony member than the one this frontend is colocated
	// with - the normal case now that RequestJoinColony's own
	// target_address sends the request there directly, rather than
	// recording it on this node's own (usually unrelated) local raft.
	GetJoinRequestStatus(ctx context.Context, addr, requestID string) (*rpcpb.GetJoinRequestStatusResponse, error)
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
// nodeHealthSignals gathers this node's fresh raw facts and hands them
// to health.SignalsFrom, which owns the actual derivation rules (ADR-0122
// moved them out of this file and into internal/health, so this UI path
// and managerd's ClusterHealth RPC cannot compute the same verdict by
// two different rule sets). This function still owns all of the I/O -
// the frontend is a separate process from managerd and gathers its own
// evidence, exactly as ADR-0056 designed.
func (s *Server) nodeHealthSignals(ctx context.Context, nodeID, localNodeID string, anchor *rpcpb.StatusResponse, hostStats *rpcpb.HostStatsResponse, hostStatsErr error, now time.Time) health.NodeSignals {
	inputs := health.Inputs{
		NodeID:                   nodeID,
		IsLocal:                  nodeID == localNodeID,
		PeerForwardingConfigured: s.peers != nil,
		MembershipObserved:       anchor.GetRaftReachable(),
		MembershipObservedAt:     now,
	}

	if inputs.MembershipObserved {
		for _, m := range anchor.GetMembers() {
			if m.GetNodeId() == nodeID {
				inputs.MemberFound = true
				inputs.Suffrage = health.ParseSuffrage(m.GetSuffrage())
				break
			}
		}
	}

	// fetchHostStats above silently falls back to this node's own local
	// client when s.peers is nil, so in that case its result says nothing
	// trustworthy about the actual remote node - attempt nothing and let
	// reachability read as unknown, which is what it is.
	if !inputs.IsLocal && s.peers != nil {
		inputs.PeerDialAttempted = true
		inputs.PeerDialSucceeded = hostStatsErr == nil
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
		inputs.HeartbeatObserved = true
		inputs.HeartbeatOK = peerStatus.GetRaftReachable()
		if peerStatus.GetRaftReachable() {
			inputs.AppliedIndexObserved = true
			inputs.AppliedIndex = peerStatus.GetRaftAppliedIndex()
			inputs.LastLogIndex = peerStatus.GetRaftLastLogIndex()
			inputs.IndicesObservedAt = now
		}
	}

	if hostStatsErr == nil && hostStats != nil {
		inputs.ReconcileObservedAt = now
		inputs.ReconcileIntervalSeconds = hostStats.GetReconcileIntervalSeconds()
		// reconcile_interval_seconds == 0 is the only reliable "no
		// Reconciler configured on this node" signal - a 0 timestamp
		// alone can't distinguish that from "configured but no tick yet"
		// (see HostStatsResponse's own doc comment). SignalsFrom derives
		// ReconcilerConfigured from exactly this field.
		if unix := hostStats.GetLastReconcileAttemptUnix(); unix > 0 {
			inputs.ReconcileEverAttempted = true
			inputs.LastReconcileAttempt = time.Unix(unix, 0)
		}
		if unix := hostStats.GetLastReconcileSuccessUnix(); unix > 0 {
			inputs.ReconcileEverSucceeded = true
			inputs.LastReconcileSuccess = time.Unix(unix, 0)
		}
	}

	return health.SignalsFrom(inputs)
}

// clusterNodeEvidence gathers the same host and Evidence-Aware Health facts
// used by the overview row, for a single node. Keeping this calculation in
// one place prevents the overview and the dedicated evidence page from
// presenting different answers for the same request.
func (s *Server) clusterNodeEvidence(ctx context.Context, nodeID, localNodeID string, anchor *rpcpb.StatusResponse) clusterNodeView {
	now := time.Now()
	hostStats, hostStatsErr := s.fetchHostStats(ctx, nodeID, localNodeID)
	var stats statsView
	var errMsg string
	if hostStatsErr != nil {
		errMsg = hostStatsErr.Error()
	} else {
		stats = fromRPCStats(hostStats)
	}
	node := summarizeClusterNode(nodeID, stats, errMsg)

	signals := s.nodeHealthSignals(ctx, nodeID, localNodeID, anchor, hostStats, hostStatsErr, now)
	result := health.ComputeNodeHealth(signals, now)
	node.HealthStatus = result.Status
	node.HealthExplanation = result.Explanation
	node.HealthObservations = result.Observations
	return node
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
			nodes[i] = s.clusterNodeEvidence(r.Context(), id, localNodeID, statusResp)
		}(i, id)
	}
	wg.Wait()

	sort.Slice(nodes, func(i, j int) bool { return nodes[i].NodeID < nodes[j].NodeID })

	s.render(w, "cluster_overview_page", s.withAuthFields(r, pageData{
		ClusterNodes:                nodes,
		JoinRequests:                s.currentJoinRequests(r),
		ActivePage:                  "stats",
		JoinRequestError:            r.URL.Query().Get("join_request_error"),
		JoinRequestPreflightID:      r.URL.Query().Get("preflight_request_id"),
		JoinRequestPreflightVerdict: r.URL.Query().Get("preflight_verdict"),
		JoinRequestPreflightDetail:  r.URL.Query().Get("preflight_detail"),
	}))
}

// handleClusterEvidencePage serves the evidence-backed health details for
// one known Comb ("/host/{id}/evidence"). It reuses the same fresh facts and
// verdict calculation as the overview page, but gives the operator a stable
// page to inspect and link to directly.
func (s *Server) handleClusterEvidencePage(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	statusResp, err := s.client.Status(r.Context(), &rpcpb.StatusRequest{})
	if err != nil {
		http.Error(w, "could not verify current Colony membership", http.StatusServiceUnavailable)
		return
	}
	if !knownColonyMember(statusResp, id) {
		http.NotFound(w, r)
		return
	}

	node := s.clusterNodeEvidence(r.Context(), id, statusResp.GetManagerNodeId(), statusResp)
	s.render(w, "cluster_evidence_page", s.withAuthFields(r, pageData{
		EvidenceNode: node,
		ActivePage:   "stats",
	}))
}

// handleHostPage serves the verbose per-node stats page
// ("/host/{id}") - the same detail the old single-node "/" page used
// to show, now addressable per node.
func (s *Server) handleHostPage(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	statusResp, err := s.client.Status(r.Context(), &rpcpb.StatusRequest{})
	if err != nil {
		http.Error(w, "could not verify current Colony membership", http.StatusServiceUnavailable)
		return
	}
	if !knownColonyMember(statusResp, id) {
		http.NotFound(w, r)
		return
	}

	stats, errMsg := s.nodeHostStats(r.Context(), id, statusResp.GetManagerNodeId())
	if stats.NodeID == "" {
		stats.NodeID = id
	}
	s.render(w, "host_page", s.withAuthFields(r, pageData{Error: errMsg, Stats: stats, ActivePage: "stats"}))
}

// knownColonyMember reports whether id is in the current raft membership
// returned by this frontend's colocated managerd. It is deliberately checked
// before handleHostPage derives a peer address: a URL path value is
// attacker-controlled, while KnownNodeIds is obtained from raftd's current
// server configuration. This keeps the frontend's peer credential from being
// sent to an arbitrary endpoint through /host/{id}.
func knownColonyMember(status *rpcpb.StatusResponse, id string) bool {
	if id != "" && id == status.GetManagerNodeId() {
		return true
	}
	for _, knownID := range status.GetKnownNodeIds() {
		if knownID == id {
			return true
		}
	}
	return false
}
