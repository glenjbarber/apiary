package frontend

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
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

	// IsColonyLeader marks the node this Comb's own raftd currently reports
	// as the Colony leader (ADR-0123). It comes from the same single anchor
	// Status call every row already shares, so it costs no extra RPC, and it
	// is deliberately only ever set on a row whose NodeID matches the
	// observed leader - when the leader is unknown or unobserved, no row is
	// marked rather than one row being guessed at.
	IsColonyLeader bool

	// Causes/CauseState/CauseHeadline are the "why is this Comb unhealthy"
	// surface: one entry per REAL problem this Comb has, each carrying the
	// error text Apiary actually holds, the resource it concerns (linked
	// only where a real page exists), when it was observed, and how stale
	// that observation is. CauseState is the single most severe state
	// across them - what the overview card's badge shows - and
	// CauseHeadline is that one cause in a single line, so the command
	// center answers "what is wrong with this Comb" without a click.
	//
	// All three are zero-valued when the only honest answer is "not
	// observed"; see combCauseNeverObserved's own doc comment - a Comb
	// with no evidence at all must never be summarized as a healthy one.
	Causes        []combCauseView
	CauseState    combCauseState
	CauseHeadline string
	// CauseBadgeClass/CauseBadgeLabel render CauseState. They are carried
	// on the view rather than chosen in the template so the overview card
	// and the evidence page cannot disagree about which state is green -
	// combCauseBadge stays the single authority.
	CauseBadgeClass string
	CauseBadgeLabel string
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
//
// index is the ONE cluster-wide resource-list read the caller already made
// for this request; it is nil only when a caller had none, in which case
// combCauses reports the per-resource causes as unreadable rather than
// quietly reporting "nothing is wrong with this Comb's resources" - a
// missing read must never read as a clean bill of health. now is passed in
// rather than read from the clock so one request's rows, causes, and ages
// are all computed against a single instant.
func (s *Server) clusterNodeEvidence(ctx context.Context, nodeID, localNodeID string, anchor *rpcpb.StatusResponse, index *combCauseIndex, now time.Time) clusterNodeView {
	hostStats, hostStatsErr := s.fetchHostStats(ctx, nodeID, localNodeID)
	// fetchHostStats silently falls back to this frontend's own colocated
	// client when s.peers is nil, so for a node that is NOT local its
	// "answer" is this frontend's own host data wearing another Comb's
	// name. nodeHealthSignals already refuses to treat that as evidence
	// (it leaves PeerDialAttempted false, so reachability reads Unknown);
	// the cause panel must refuse it for the same reason, or a Comb with
	// no observation would be shown this frontend's own reconcile history
	// as if it had been observed.
	observedHostStats := hostStats
	if hostStatsErr == nil && nodeID != localNodeID && s.peers == nil {
		observedHostStats = nil
	}
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
	// Only an actually-observed leader marks a row (ADR-0123). An unknown,
	// electing, or unobserved reading marks nothing, so the list can never
	// imply a leader that was not seen. Derived once from the same anchor
	// every row already shares, so it costs no extra RPC.
	leader := colonyLeaderFromStatus(anchor, nil, now)
	node.IsColonyLeader = leader.IsLeaderNode(nodeID)
	var causeStale bool
	node.Causes, node.CauseState, node.CauseHeadline, causeStale = combCauses(nodeID, localNodeID, anchor, observedHostStats, hostStatsErr, result.Observations, index, now)
	node.CauseBadgeClass, node.CauseBadgeLabel = combCauseBadge(node.CauseState, causeStale)
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
		s.render(w, "cluster_overview_page", s.withAuthFieldsFrom(r, pageData{Error: err.Error(), ActivePage: "stats"}, nil, err))
		return
	}

	localNodeID := statusResp.GetManagerNodeId()
	nodeIDs := statusResp.GetKnownNodeIds()
	if len(nodeIDs) == 0 && localNodeID != "" {
		nodeIDs = []string{localNodeID}
	}

	// One shared, fail-soft read of the cluster-wide resource lists feeds
	// every row's cause panel below. Fetching per Comb instead would cost
	// two extra leader-forwarded Raft reads per node and could show two
	// Combs contradictory "affected resource" lists from two different
	// moments in the same page load.
	now := time.Now()
	index := s.gatherCombCauseIndex(r.Context())

	nodes := make([]clusterNodeView, len(nodeIDs))
	var wg sync.WaitGroup
	for i, id := range nodeIDs {
		wg.Add(1)
		go func(i int, id string) {
			defer wg.Done()
			nodes[i] = s.clusterNodeEvidence(r.Context(), id, localNodeID, statusResp, index, now)
		}(i, id)
	}
	wg.Wait()

	sort.Slice(nodes, func(i, j int) bool { return nodes[i].NodeID < nodes[j].NodeID })

	s.render(w, "cluster_overview_page", s.withAuthFieldsFrom(r, pageData{
		ClusterNodes:                nodes,
		JoinRequests:                s.currentJoinRequests(r),
		ActivePage:                  "stats",
		JoinRequestError:            r.URL.Query().Get("join_request_error"),
		JoinRequestPreflightID:      r.URL.Query().Get("preflight_request_id"),
		JoinRequestPreflightVerdict: r.URL.Query().Get("preflight_verdict"),
		JoinRequestPreflightDetail:  r.URL.Query().Get("preflight_detail"),
	}, statusResp, nil))
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

	node := s.clusterNodeEvidence(r.Context(), id, statusResp.GetManagerNodeId(), statusResp, s.gatherCombCauseIndex(r.Context()), time.Now())
	s.render(w, "cluster_evidence_page", s.withAuthFieldsFrom(r, pageData{
		EvidenceNode: node,
		ActivePage:   "stats",
	}, statusResp, nil))
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
	s.render(w, "host_page", s.withAuthFieldsFrom(r, pageData{Error: errMsg, Stats: stats, ActivePage: "stats"}, statusResp, nil))
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

// ---------------------------------------------------------------------------
// Comb evidence: why is this Comb unhealthy?
//
// Everything below derives causes from facts Apiary ALREADY holds. The hard
// rule this whole section exists to keep is that a missing fact renders as
// "not observed", never as a healthy answer and never as a made-up error. An
// operator staring at a grey "not observed" learns something true; an
// operator staring at a confident green badge backed by nothing (or by a
// placeholder string) learns something false, and that is the failure mode
// this feature is written to remove.
//
// It is the same discipline internal/cluster/simulate.go already applies to
// HAST replicas (an absent status is undeterminable, not "out of sync") and
// internal/health applies to reachability (a Confirmed-reachable voter is
// never credited for survival). The vocabulary follows theirs too:
// "unobserved" is a first-class verdict, not a synonym for healthy.
// ---------------------------------------------------------------------------

// combCauseState is the three-way answer to "what did we actually observe
// about this?", plus one honest fourth case that is genuinely a different
// fact rather than a flavour of the other three.
//
// The three required states are deliberately NOT collapsed into a bool:
// "never observed" carries no information about health at all, "observed
// healthy" is a positive observation, and "observed failed" is a negative
// one. Any two of them sharing a rendering is the bug this type exists to
// make impossible - see combCauseBadge.
type combCauseState string

const (
	// combCauseNeverObserved means no evidence was ever collected for
	// this cause. It is NOT healthy and must never render as green, and
	// it is not a failure either - "we could not check" is a third
	// answer, exactly as internal/cluster's RecoveryVerdictReplicaUnobserved
	// is neither RecoveryVerdictReplicaInSync nor
	// RecoveryVerdictReplicaOutOfSync.
	combCauseNeverObserved combCauseState = "never_observed"

	// combCauseObservedHealthy means a real, recent, SUCCESSFUL
	// observation. It is the only one of the three that may ever render
	// green, and only while it is genuinely fresh - a stale success is
	// re-rendered as a stale success, never as this.
	combCauseObservedHealthy combCauseState = "observed_healthy"

	// combCauseObservedFailed means a real, recent, FAILED observation.
	combCauseObservedFailed combCauseState = "observed_failed"

	// combCauseNotApplicable means the thing that would produce evidence
	// is not configured on this Comb at all (no Reconciler, so no
	// reconcile evidence exists and none is expected). This is kept
	// distinct from never_observed on purpose: a Comb with no Reconciler
	// has answered a question that was never put, while a Comb with a
	// Reconciler that has never ticked has not.
	combCauseNotApplicable combCauseState = "not_applicable"
)

// combCauseView is one problem (or one piece of missing evidence) on one
// Comb, in the shape templates read. It is deliberately flat: an operator
// scanning the panel needs the error text, the resource, and the age on one
// line, and a nested "evidence" sub-object would make the honest "we don't
// know" case require special-casing at every render site.
type combCauseView struct {
	// Source names what produced (or failed to produce) this evidence,
	// in the same snake_case token convention health.Observation.Source
	// and invariant.Evidence.Source already use, so the three panels read
	// as one vocabulary.
	Source string

	// State is the three-way observation outcome. Rendered as text and as
	// a data-cause-state attribute, so "distinct" is a property a test
	// can assert on directly rather than something inferred from a CSS
	// class name.
	State combCauseState

	// BadgeClass/BadgeLabel are the visual rendering of State, derived in
	// exactly one place (combCauseBadge) so no template can disagree with
	// the Go side about which state is green.
	BadgeClass string
	BadgeLabel string

	// Summary is the one-line "what and where" a command-center card can
	// show without opening the evidence page.
	Summary string

	// Detail is the REAL text Apiary holds - the reconciler's own recorded
	// error message, or the host-subsystem failure string, or an explicit
	// statement of what could not be read. It is never a placeholder: when
	// no error text exists, this says so in those words (see
	// combCauseNoErrorText).
	Detail string

	// ResourceKind/ResourceName/ResourceID identify the thing actually
	// wrong, when the cause is about one. ResourceLink is set ONLY for a
	// resource that has a real page to open (a VM or a jail today) - a
	// network or an image has no per-resource route in this codebase, so
	// those render as a plain named reference instead of a link that
	// would 404. See combResourceLink.
	ResourceKind string
	ResourceName string
	ResourceID   string
	ResourceLink string

	// RelatedRefs are resources this cause merely NAMES (the image a VM
	// boots from, the network its NIC belongs to) with no page to link
	// to, plus the reason no link exists. Rendered as text so an
	// operator can still act on the name.
	RelatedRefs []string

	// ObservedAt is when THIS evidence was gathered - deliberately not
	// when the underlying condition began, matching health.Observation's
	// own ObservedAt contract. WrittenAtUnknown says when even that
	// could not be established, which is the honest state for a
	// Raft-replicated phase_error read fresh but written at an unknown
	// time (state.proto carries no timestamp for it).
	ObservedAt       string
	ObservedAtKnown  bool
	WrittenAtUnknown string

	// Age is the human age of the observation, and AgeSeconds its exact
	// form for sorting/review. Stale says the observation is past this
	// Comb's own freshness limit - derived from the FreshnessLimit
	// internal/health already computed for this Comb, never from a second
	// copy of the multiplier rule.
	Age          string
	AgeSeconds   int64
	Stale        bool
	StaleNote    string
	ClockSkewed  bool
	HasFreshness bool
}

// combCauseBadge maps one state (plus staleness) to its visual rendering.
// This is the ONLY place that decides which state may look green, and it is
// a function rather than template logic precisely so that no template can
// reach the green classes with an unobserved state.
//
// The rules, in one place so a reviewer can check them at a glance:
//   - never_observed is grey "unknown" and can never be ready/healthy/ok.
//     Silence is a state with a verdict, and its verdict is not health.
//   - observed_failed is red whether or not it is stale: a stale failure is
//     still a failure, and greying it out would let an old, unresolved
//     problem look like it had simply not been measured.
//   - observed_healthy is green ONLY while fresh. A stale success renders
//     with the dashed `stale` class and says "stale" in its label, so a
//     three-hour-old success is never presented as a current one.
//   - not_applicable is neither success nor failure: nothing was expected.
func combCauseBadge(state combCauseState, stale bool) (class, label string) {
	switch state {
	case combCauseNeverObserved:
		return "unknown", "not observed"
	case combCauseObservedFailed:
		if stale {
			return "error", "observed failed (stale)"
		}
		return "error", "observed failed"
	case combCauseObservedHealthy:
		if stale {
			return "stale", "observed healthy (stale)"
		}
		return "ready", "observed healthy"
	case combCauseNotApplicable:
		return "not-applicable", "not applicable"
	default:
		// An unrecognized state is an unobserved one, never a healthy
		// one. Defaulting this branch to green would turn a future
		// typo into exactly the false reassurance the type exists to
		// prevent.
		return "unknown", "not observed"
	}
}

// combCauseSeverity orders states for "the worst thing about this Comb",
// which is what a single card badge has to show. A confirmed failure is
// worse than an unknown, and an unknown is worse than a clean result;
// a stale clean result is worse than a fresh one, which is why staleness
// subtracts from a healthy state rather than being ignored.
func combCauseSeverity(state combCauseState, stale bool) int {
	var base int
	switch state {
	case combCauseObservedFailed:
		base = 3
	case combCauseNeverObserved:
		base = 2
	case combCauseNotApplicable:
		base = 1
	case combCauseObservedHealthy:
		base = 0
	default:
		base = 2
	}
	if stale && state == combCauseObservedHealthy {
		base--
	}
	return base
}

// formatObservationAge renders an observation's age the way an operator
// reads one, at a resolution that stays useful: seconds for something that
// just happened, minutes for a tick interval, hours and days for anything
// an operator would call "stale".
//
// A negative age (a node whose reported clock is ahead of this frontend's)
// is NOT clamped to a plausible-looking zero with no comment: the age is
// reported as unreadable, because a duration computed from two disagreeing
// clocks is not evidence of anything.
func formatObservationAge(d time.Duration) string {
	if d < 0 {
		return "unknown (this Comb's clock is ahead of this frontend's)"
	}
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds ago", int64(d/time.Second))
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int64(d/time.Minute))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh %dm ago", int64(d/time.Hour), int64(d%time.Hour/time.Minute))
	default:
		return fmt.Sprintf("%dd %dh ago", int64(d/(24*time.Hour)), int64(d%(24*time.Hour)/time.Hour))
	}
}

// combReconcileEvidence is the honest reading of one Comb's own reconcile
// history - the raw (interval, last attempt, last success) triple that
// HostStatsResponse already carries - reduced to the one state an operator
// can act on, with the arithmetic kept out of the template.
type combReconcileEvidence struct {
	State          combCauseState
	Detail         string
	ObservedAt     time.Time
	HasObservedAt  bool
	Stale          bool
	ClockSkewed    bool
	Age            time.Duration
	FreshnessLimit time.Duration
	HasFreshness   bool
}

// assessCombReconcileEvidence reduces one Comb's own reconcile timestamps
// to a single, honest state.
//
// Every branch here is a distinct fact with a distinct operator response,
// and none of them is a default that absorbs the rest:
//
//  1. No Reconciler configured (reconcile_interval_seconds == 0, the only
//     reliable "not applicable" signal - see internal/health's own comment
//     on that field). Nothing was asked, so nothing is claimed.
//  2. A recent success. The ONLY state that may render green.
//  3. A recent attempt with no recent success: ticks are running and the
//     most recent one did not complete cleanly. This is a real, recent,
//     failed observation.
//  4. A success older than the limit, but with no later attempt to
//     contradict it: the last thing this Comb actually reported was a
//     clean tick, it is just old. Reported as a STALE success, which is
//     still a real observed success - calling it a failure would invent a
//     failure that was never observed, which is the same inversion in the
//     other direction.
//  5. An attempt that came after the last success and is itself older than
//     the limit: the most recent tick this Comb ran did not succeed, but
//     nothing has been observed since, so it is a stale failure.
//  6. An attempt with no success ever recorded: the same, from the
//     start - it has never once completed cleanly.
//  7. Configured but never attempted. No evidence has ever been collected,
//     which is emphatically not a healthy verdict.
//
// freshnessLimit comes from the caller because internal/health already
// derived it for this exact Comb (3x that Comb's OWN reconcile interval -
// see health.reconcileFreshnessMultiplier). A second copy of that rule here
// would be free to drift from the one that decides the health badge, which
// is precisely the "two paths, two answers" failure ADR-0122 was written to
// end. When health never got far enough to derive one (it short-circuits
// earlier on reachability/heartbeat problems), the limit is simply absent
// and nothing here claims staleness.
func assessCombReconcileEvidence(reconcilerConfigured bool, everAttempted bool, lastAttempt time.Time, everSucceeded bool, lastSuccess time.Time, freshnessLimit time.Duration, hasFreshness bool, now time.Time) combReconcileEvidence {
	e := combReconcileEvidence{FreshnessLimit: freshnessLimit, HasFreshness: hasFreshness}

	if !reconcilerConfigured {
		e.State = combCauseNotApplicable
		e.Detail = "this Comb's managerd reports no Reconciler configured (reconcile_interval_seconds is 0), so no reconciliation was ever expected here - this is not a statement about the Comb's health"
		return e
	}

	// fresh reports whether an observation counts as recent, and whether
	// its age is even computable. observed=false means there is no
	// observation to age at all. hasFreshness=false means
	// internal/health never derived a limit for this Comb; reporting a
	// stale verdict against a limit we do not have would be inventing
	// the threshold, so such an observation is simply reported fresh and
	// its age is left to speak for itself.
	fresh := func(observed bool, at time.Time) (fresh bool, skewed bool) {
		if !observed {
			return false, false
		}
		age := now.Sub(at)
		if age < 0 {
			return true, true
		}
		if !hasFreshness {
			return true, false
		}
		return age <= freshnessLimit, false
	}

	successFresh, successSkewed := fresh(everSucceeded, lastSuccess)
	attemptFresh, attemptSkewed := fresh(everAttempted, lastAttempt)
	// A later attempt than the last success means the most recent tick
	// this Comb actually ran did not succeed - the strongest thing the two
	// timestamps together can tell us, and the one that must not be lost
	// to a "stale, so let's call it healthy" reading.
	attemptAfterSuccess := everAttempted && everSucceeded && lastAttempt.After(lastSuccess)

	switch {
	case everSucceeded && successFresh:
		e.State = combCauseObservedHealthy
		e.ObservedAt, e.HasObservedAt = lastSuccess, true
		e.Age, e.ClockSkewed = now.Sub(lastSuccess), successSkewed
		e.Detail = "this Comb's own reconciler completed a full tick with no error at this time"
	case everAttempted && attemptFresh:
		e.State = combCauseObservedFailed
		e.ObservedAt, e.HasObservedAt = lastAttempt, true
		e.Age, e.ClockSkewed = now.Sub(lastAttempt), attemptSkewed
		e.Detail = "this Comb's reconciler is ticking, but its most recent tick did not complete cleanly, and no later tick has succeeded - the tick's own error text is not carried on the wire (see the tick_reconcile_error_gap row in this panel for the exact field that would carry it)"
	case everSucceeded && !attemptAfterSuccess:
		// The last thing this Comb actually reported was a clean tick.
		// It is simply old, so it is reported as a STALE success: still
		// a real observation, and explicitly not a current one. Calling
		// it a failure would invent a failure nobody observed.
		e.State = combCauseObservedHealthy
		e.ObservedAt, e.HasObservedAt = lastSuccess, true
		e.Age, e.ClockSkewed = now.Sub(lastSuccess), successSkewed
		e.Stale = true
		e.Detail = fmt.Sprintf("the last successful reconcile tick this Comb reported was %s, past its own freshness limit - this is a stale success, not a current one, and no tick has been observed since to update it", formatObservationAge(e.Age))
	case everAttempted:
		// A real observation, just an old one. Rendering this as
		// healthy because "the last thing we heard was not a failure"
		// is the exact inversion this feature exists to prevent.
		e.State = combCauseObservedFailed
		e.ObservedAt, e.HasObservedAt = lastAttempt, true
		e.Age, e.ClockSkewed = now.Sub(lastAttempt), attemptSkewed
		e.Stale = true
		e.Detail = fmt.Sprintf("the last reconcile attempt this Comb reported was %s, past its own freshness limit - it has never completed cleanly since, and no tick has been observed since either, so whether it is still failing is genuinely unknown", formatObservationAge(e.Age))
	default:
		e.State = combCauseNeverObserved
		e.Detail = "this Comb has a Reconciler configured but has never been observed to run a tick - no reconciliation evidence exists yet, which is not the same as healthy"
	}
	return e
}

// reconcileFreshnessLimit pulls the per-Comb reconcile freshness limit back
// out of the Observations internal/health already produced for that Comb,
// rather than recomputing 3x its interval here. Returns found=false when
// health never derived one for this request (it stops at the first raft
// problem), in which case callers must claim no staleness at all.
func reconcileFreshnessLimit(observations []health.Observation) (time.Duration, bool) {
	for _, o := range observations {
		if o.Source == "reconciler_last_success" && o.FreshnessLimit > 0 {
			return o.FreshnessLimit, true
		}
	}
	return 0, false
}

// combCauseIndex is ONE page request's single read of the cluster-wide
// resource lists, shared by every Comb's cause panel.
//
// It exists so "which resource is broken on this Comb" costs two
// leader-forwarded Raft reads per page rather than two per Comb, and - more
// importantly - so every Comb on a page is described against the same
// snapshot. Per-Comb reads would let two rows disagree about the same VM
// within one page load, which is the same class of bug ADR-0122 exists to
// prevent for health verdicts.
type combCauseIndex struct {
	// VMs/Jails are the converted definitions. Empty with no error means
	// the cluster genuinely has none, which is a real answer.
	VMs   []vmView
	Jails []jailView

	// VMError/JailError are the fail-soft fetch errors ("" on success),
	// matching currentNetworks/currentClusterISOs' own convention: a
	// failed read yields nil plus a message, never a 500.
	VMError   string
	JailError string
}

// gatherCombCauseIndex reads the VM and jail lists once, fail-soft, exactly
// as currentNetworks does. The two reads are independent: one failing must
// not cost the other, because a Comb whose VMs could not be listed is a
// different (and narrower) claim than one whose jails could not be.
func (s *Server) gatherCombCauseIndex(ctx context.Context) *combCauseIndex {
	index := &combCauseIndex{}

	if resp, err := s.client.ListVMs(ctx, &rpcpb.ListVMsRequest{}); err != nil {
		index.VMError = err.Error()
	} else if resp.GetError() != "" {
		index.VMError = resp.GetError()
	} else {
		for _, vm := range resp.GetVms() {
			index.VMs = append(index.VMs, fromRPCVM(vm))
		}
	}

	if resp, err := s.client.ListJails(ctx, &rpcpb.ListJailsRequest{}); err != nil {
		index.JailError = err.Error()
	} else if resp.GetError() != "" {
		index.JailError = resp.GetError()
	} else {
		for _, j := range resp.GetJails() {
			index.Jails = append(index.Jails, fromRPCJail(j))
		}
	}

	return index
}

// combCauseNoErrorText is the wording used wherever Apiary can prove
// something is wrong but holds no error string for it. It exists as a named
// constant because the temptation to write "unknown error" or "N/A" here is
// exactly the silent fabrication this feature must not perform: the honest
// answer is that the field is not on the wire, and saying so is more useful
// to an operator than a plausible-looking string would be.
const combCauseNoErrorText = "no error text is recorded for this on the wire (see the reconcile_phase_gap row in this panel for the exact field that would carry it)"

// combResourceLink returns the page that actually exists for one resource,
// or "" when none does.
//
// This is deliberately a hard-coded, hand-checked mapping rather than a
// format string over the resource kind: only VMs and jails have per-resource
// routes in this codebase today. A network or an image has a LIST page
// (/networks, /images) but no per-item one, and inventing "#net-<id>" style
// anchors would produce links that resolve to the top of a list while
// looking exactly like the working links beside them. A link that resolves
// to the wrong thing is worse than no link, so those render as plain named
// references (combCauseView.RelatedRefs) instead.
func combResourceLink(kind, id string) string {
	if id == "" {
		return ""
	}
	switch kind {
	case "vm":
		return "/vms/" + id
	case "jail":
		return "/jails/" + id
	default:
		return ""
	}
}

// combCause assembles every honest cause for one Comb: what is wrong, the
// real text Apiary holds for it, the resource it concerns, when that
// evidence was gathered, and how stale it is.
//
// hostStats is the ALREADY-VERIFIED response for this Comb, or nil when none
// could be trusted (see clusterNodeEvidence's observedHostStats). A nil here
// is what turns every HostStats-derived cause into "not observed" instead
// of an empty list - the difference between "this Comb is fine" and "nothing
// is known about this Comb" is the whole feature.
func combCauses(nodeID, localNodeID string, anchor *rpcpb.StatusResponse, hostStats *rpcpb.HostStatsResponse, hostStatsErr error, observations []health.Observation, index *combCauseIndex, now time.Time) ([]combCauseView, combCauseState, string, bool) {
	freshnessLimit, hasFreshness := reconcileFreshnessLimit(observations)

	var causes []combCauseView

	// 1. Reachability. If the Comb could not be dialed there is no
	// observation of anything on it, and every other cause below would
	// be a guess. This one cause is therefore enough to make the whole
	// panel honest on its own.
	if hostStats == nil {
		detail := combCauseNoErrorText
		switch {
		case hostStatsErr != nil:
			detail = "this Comb's own managerd could not be reached, so nothing about it could be observed: " + hostStatsErr.Error()
		case nodeID != localNodeID:
			detail = "this frontend has no peer forwarding configured, so it cannot observe any Comb other than the one it is colocated with - nothing about this Comb was actually read"
		}
		causes = append(causes, newCombCause("manager_reachability", combCauseNeverObserved, "this Comb's managerd could not be read", detail, now))
	}

	// 2. Raft membership read from the shared anchor, not from a per-Comb
	// dial: membership is a cluster-wide-consistent replicated fact, and
	// internal/health already derived a verdict from it. A node that
	// simply is not a member is a real, worth-reporting finding, but only
	// when the anchor could actually read membership - an unread anchor
	// says nothing about any individual Comb.
	if anchor.GetRaftReachable() && !knownColonyMember(anchor, nodeID) {
		causes = append(causes, newCombCause("raft_membership", combCauseObservedFailed,
			"this Comb is not in the current raft membership",
			"the current raft membership does not contain this Comb, so it is not part of this Colony - it cannot be holding any of the Colony's state", now))
	}

	// 3. The Comb's own reconcile history, and 4. its host subsystems -
	// both require a real HostStats answer.
	reconcileFailed := hostStats == nil
	if hostStats != nil {
		reconcileConfigured := hostStats.GetReconcileIntervalSeconds() > 0
		rec := assessCombReconcileEvidence(
			reconcileConfigured,
			hostStats.GetLastReconcileAttemptUnix() > 0, time.Unix(hostStats.GetLastReconcileAttemptUnix(), 0),
			hostStats.GetLastReconcileSuccessUnix() > 0, time.Unix(hostStats.GetLastReconcileSuccessUnix(), 0),
			freshnessLimit, hasFreshness, now,
		)
		reconcileFailed = rec.State == combCauseObservedFailed
		causes = append(causes, newCombCause("reconciler_last_tick", rec.State, reconcileTickSummary(rec), rec.Detail, now))
		// A pointer, deliberately: newCombCause fixes the badge from the
		// state alone, and the staleness verdict only becomes known after
		// assessCombReconcileEvidence has run, so the badge is re-derived
		// here from the FINAL state rather than left describing a state
		// the row no longer has.
		last := &causes[len(causes)-1]
		last.ObservedAt, last.ObservedAtKnown = formatObservedAt(rec.ObservedAt, rec.HasObservedAt)
		last.Age, last.AgeSeconds = ageFields(rec.Age, rec.ClockSkewed)
		last.Stale, last.StaleNote = rec.Stale, staleNote(rec.Stale, freshnessLimit, hasFreshness)
		last.HasFreshness = hasFreshness
		last.BadgeClass, last.BadgeLabel = combCauseBadge(last.State, last.Stale)

		// hoststats.Snapshot.Errors is real text from a real failed
		// subsystem probe ("zfs: ...", "pf: ...", see
		// internal/hoststats.Gather). It is node-scoped though: there is
		// no resource attribution and no start time anywhere in
		// hoststats, which is stated in the detail rather than papered
		// over with an invented timestamp.
		for _, hostErr := range hostStats.GetErrors() {
			c := newCombCause("host_subsystem", combCauseObservedFailed, hostSubsystemSummary(hostErr), hostErr, now)
			c.RelatedRefs = []string{"node-scoped: hoststats records which subsystem failed to report, not which resource it concerns, and not when it started"}
			causes = append(causes, c)
		}
	}

	// 5. Per-resource causes from the one shared Raft read.
	causes = append(causes, combResourceCauses(nodeID, hostStats, index, freshnessLimit, hasFreshness, now)...)

	// 6. Everything we could not read. Listed explicitly so a missing
	// field is visible as a gap rather than as the absence of a problem.
	causes = append(causes, combEvidenceGaps(hostStats, reconcileFailed, index, observations, now)...)

	state, stale, headline := worstCombCause(causes)
	return causes, state, headline, stale
}

// newCombCause builds one cause with State, badge, and observation time
// already derived, so no call site can forget the badge mapping - the
// mapping is the safety property, and it only holds if it is not optional.
//
// summary is the SHORT "what and where" a command-center card can show in
// one line; detail carries the full sentence for the evidence page. They are
// separate fields precisely because a card that repeats the whole detail
// stops being scannable, and because a one-line summary is what an operator
// actually reads first.
func newCombCause(source string, state combCauseState, summary, detail string, now time.Time) combCauseView {
	class, label := combCauseBadge(state, false)
	return combCauseView{
		Source:          source,
		State:           state,
		BadgeClass:      class,
		BadgeLabel:      label,
		Detail:          detail,
		Summary:         source + ": " + summary,
		ObservedAt:      now.Format("2006-01-02 15:04:05 MST"),
		ObservedAtKnown: true,
		Age:             "just now",
	}
}

// formatObservedAt renders an observation time, keeping "never observed" as
// its own state instead of a zero timestamp that would format as the
// obviously-wrong year 1.
func formatObservedAt(at time.Time, known bool) (string, bool) {
	if !known {
		return "", false
	}
	return at.Format("2006-01-02 15:04:05 MST"), true
}

// ageFields renders an age plus its exact seconds. A negative age keeps its
// skew flag rather than being clamped to a clean-looking zero.
func ageFields(age time.Duration, skewed bool) (string, int64) {
	if skewed || age < 0 {
		return formatObservationAge(age), 0
	}
	return formatObservationAge(age), int64(age / time.Second)
}

// staleNote explains a staleness mark in the operator's own terms, and
// stays empty when nothing is stale. hasFreshness=false means no limit was
// derivable, in which case nothing is ever marked stale - there is no
// threshold to be past, so claiming one would be inventing the rule.
func staleNote(stale bool, limit time.Duration, hasFreshness bool) string {
	if !stale || !hasFreshness {
		return ""
	}
	return fmt.Sprintf("past this Comb's own %s freshness limit (3x its own reconcile interval) - treat this as history, not as a current reading", limit.Round(time.Second))
}

// combResourceCauses turns the shared VM/jail snapshot into per-resource
// causes for THIS Comb only.
//
// nodeID matching is not a filter for tidiness: a VM's phase_error was
// written by the Comb its node_id names, and showing it on any other
// Comb's page would attribute one node's real failure to another.
func combResourceCauses(nodeID string, hostStats *rpcpb.HostStatsResponse, index *combCauseIndex, freshnessLimit time.Duration, hasFreshness bool, now time.Time) []combCauseView {
	if index == nil {
		return nil
	}

	// A resource's phase_error carries no write timestamp anywhere
	// (api/internalpb/state.proto), so the best honest bound available is
	// the owning Comb's own last tick - the phase was written during some
	// tick at or before it. Recorded as an explicit caveat on each cause
	// rather than silently presented as the observation time, because
	// "we read it now" and "it happened then" are different claims.
	var lastTick time.Time
	haveLastTick := hostStats != nil && hostStats.GetLastReconcileAttemptUnix() > 0
	if haveLastTick {
		lastTick = time.Unix(hostStats.GetLastReconcileAttemptUnix(), 0)
	}

	var out []combCauseView
	appendResource := func(kind string, id, name, phase, phaseError, imageName, networkID string) {
		switch phase {
		case "error":
			detail := phaseError
			if strings.TrimSpace(detail) == "" {
				// The reconciler marked this resource as failed but
				// stored no message. Reporting the phase alone would be
				// true; inventing a message would not be. Say exactly
				// what is and is not known.
				detail = "the reconciler recorded phase=error for this resource but stored no error message: " + combCauseNoErrorText
			}
			c := newCombCause("reconcile_phase", combCauseObservedFailed, summaryResource(kind, name, id)+" is in phase=error", detail, now)
			c.ResourceKind, c.ResourceID, c.ResourceName = kind, id, name
			c.ResourceLink = combResourceLink(kind, id)
			c.RelatedRefs = relatedResourceRefs(imageName, networkID)
			// The Raft read that produced this is genuinely fresh, so
			// ObservedAt/age describe the read. What goes stale is the
			// WRITE, and that is what Stale tracks here.
			c.ObservedAt, c.ObservedAtKnown = now.Format("2006-01-02 15:04:05 MST"), true
			c.Age, c.AgeSeconds = "read from raft just now", 0
			switch {
			case !haveLastTick:
				c.WrittenAtUnknown = "this Comb has never been observed to run a reconcile tick, so not even an upper bound on when this error was written is known"
			default:
				c.WrittenAtUnknown = fmt.Sprintf("written during some reconcile tick at or before %s; the Raft field carries no per-error timestamp", formatObservedAtText(lastTick))
				if hasFreshness {
					if age := now.Sub(lastTick); age > freshnessLimit {
						c.Stale = true
						c.StaleNote = fmt.Sprintf("this Comb has not been observed to reconcile since %s, so this error may since have been resolved - no tick has confirmed or cleared it", formatObservationAge(age))
					} else {
						c.StaleNote = "the owning Comb reconciled within its own freshness limit, so this error is current as far as the evidence goes"
					}
				}
			}
			c.HasFreshness = hasFreshness
			out = append(out, c)
		case "creating", "deleting":
			// A resource parked mid-transition while its owner has not
			// ticked within the freshness limit is a real, derivable
			// problem: nothing has been observed to advance or fail it.
			// It is NOT a failure when the owner is ticking normally,
			// because that is simply what an in-flight transition looks
			// like, so those are skipped rather than rendered as noise.
			if !haveLastTick {
				return
			}
			if !hasFreshness || now.Sub(lastTick) <= freshnessLimit {
				return
			}
			age := now.Sub(lastTick)
			c := newCombCause("reconcile_transition", combCauseObservedFailed,
				fmt.Sprintf("%s is still in phase=%s with no recent reconcile tick", summaryResource(kind, name, id), phase),
				fmt.Sprintf("this resource is still in phase=%q and this Comb has not been observed to reconcile for %s, so nothing has advanced or failed it since - no error text has been recorded for it", phase, formatObservationAge(age)), now)
			c.ResourceKind, c.ResourceID, c.ResourceName = kind, id, name
			c.ResourceLink = combResourceLink(kind, id)
			c.RelatedRefs = relatedResourceRefs(imageName, networkID)
			c.WrittenAtUnknown = "this is not a recorded error: it is an absence of progress, derived from the phase and this Comb's last tick"
			c.Stale = true
			c.StaleNote = fmt.Sprintf("the backing observation is %s old - there is no recent tick to confirm this resource is still stuck", formatObservationAge(age))
			c.HasFreshness = true
			out = append(out, c)
		}
	}

	for _, vm := range index.VMs {
		if vm.NodeID != nodeID {
			continue
		}
		appendResource("vm", vm.ID, vm.Name, vm.Phase, vm.PhaseError, vm.ISOName, vm.NetworkID)
	}
	for _, j := range index.Jails {
		if j.NodeID != nodeID {
			continue
		}
		appendResource("jail", j.ID, j.Name, j.Phase, j.PhaseError, "", "")
	}
	return out
}

// relatedResourceRefs names the image/network a cause concerns WITHOUT
// linking them, because no per-resource page exists for either today
// (see combResourceLink). Showing the name still lets an operator act; a
// link to the top of a list page would not.
func relatedResourceRefs(imageName, networkID string) []string {
	var refs []string
	if imageName != "" {
		refs = append(refs, fmt.Sprintf("image %q (no per-image page exists yet, so this is text, not a link)", imageName))
	}
	if networkID != "" {
		refs = append(refs, fmt.Sprintf("network %q (no per-network page exists yet, so this is text, not a link)", networkID))
	}
	return refs
}

// summaryResource renders "kind name (id)" for a one-line summary, tolerating
// a resource with no name yet.
func summaryResource(kind, name, id string) string {
	kind = strings.ToUpper(kind[:1]) + kind[1:]
	if name == "" {
		return fmt.Sprintf("%s %s", kind, id)
	}
	return fmt.Sprintf("%s %s (%s)", kind, name, id)
}

func formatObservedAtText(t time.Time) string { return t.Format("2006-01-02 15:04:05 MST") }

// combEvidenceGaps names, in the operator's own view, every cause this build
// CANNOT answer and why - the fields that do not exist yet on the wire.
//
// This exists because "the panel is empty" and "the panel is honest about
// what it does not know" must never be confused for each other. Each entry
// is a real, unfilled gap, not a failure: an operator reading it learns
// which RPC/field would have to change, and a reviewer reading it learns
// exactly what plumbing this feature was built around.
func combEvidenceGaps(hostStats *rpcpb.HostStatsResponse, reconcileFailed bool, index *combCauseIndex, observations []health.Observation, now time.Time) []combCauseView {
	var gaps []combCauseView

	// Reported whenever a failing tick has no explanation available, which
	// is always when the Comb could not be read at all AND whenever a tick
	// is genuinely failing. On a healthy Comb nothing is missing - no
	// error was expected - so the gap is not clutter there.
	if reconcileFailed {
		gaps = append(gaps, gapCause("tick_reconcile_error", now,
			"the reconciler's own tick-level error text (internal/cluster.Reconciler.RunOnce's firstErr, e.g. \"cluster: reconciling VM <id>: ...\") is returned to cmd/managerd and discarded - no field on HostStatsResponse carries it, so this Comb's exact failing tick cannot be named here. It would require a new field on HostStatsResponse (last_reconcile_error / last_reconcile_error_unix) written from RunOnce in cmd/managerd, or a new node-local GetLocalReconcileStatus RPC."))
	}

	if index != nil {
		if index.VMError != "" {
			gaps = append(gaps, gapCause("reconcile_phase", now,
				"the VM list could not be read, so no VM on this Comb could be shown as failed or stuck: "+index.VMError))
		}
		if index.JailError != "" {
			gaps = append(gaps, gapCause("reconcile_phase", now,
				"the jail list could not be read, so no jail on this Comb could be shown as failed or stuck: "+index.JailError))
		}
	}

	// The per-error write timestamp is a genuine gap on every Comb, not
	// only broken ones, and it is the reason reconcile_phase causes carry
	// a "written at or before" caveat above. Stating it once, here, keeps
	// that caveat from looking like an accident of formatting.
	if _, hasFreshness := reconcileFreshnessLimit(observations); !hasFreshness {
		gaps = append(gaps, gapCause("reconcile_phase", now,
			"no staleness threshold could be derived for this Comb: internal/health never computed a reconcile freshness limit (it stops at the first raft problem), so nothing here can be marked fresh or stale against one. Raft's VMDefinition.phase_error / JailDefinition.phase_error additionally carry no write timestamp of their own, so the only honest bound available is the owning Comb's last tick."))
	}

	return gaps
}

// gapCause is a "not observed" entry describing missing plumbing rather
// than a fault. It uses the same three-state vocabulary deliberately: a gap
// is never_observed, and the badge mapping above makes sure that can never
// render green.
func gapCause(source string, now time.Time, detail string) combCauseView {
	class, label := combCauseBadge(combCauseNeverObserved, false)
	return combCauseView{
		Source:     source + "_gap",
		State:      combCauseNeverObserved,
		BadgeClass: class,
		BadgeLabel: label,
		Summary:    source + " gap: Apiary cannot currently observe this",
		Detail:     detail,
		WrittenAtUnknown: "this row describes what Apiary cannot currently observe, not an observation of a fault - " +
			"so there is no evidence age to quote for it",
		// The page IS freshly checked - that is what makes the gap worth
		// reporting - so the time is real; it is the underlying evidence
		// that does not exist, not the check.
		ObservedAt:      now.Format("2006-01-02 15:04:05 MST"),
		ObservedAtKnown: true,
		Age:             "just now",
	}
}

// worstCombCause reduces the cause list to the single state a card badge can
// show, plus that cause's one-line summary. Ties break on the first cause
// in the list, which is built in a fixed order (reachability, membership,
// reconcile, subsystems, resources, gaps) so the most fundamental problem
// wins rather than whichever happened to be appended last.
func worstCombCause(causes []combCauseView) (state combCauseState, stale bool, headline string) {
	if len(causes) == 0 {
		// No causes at all means nothing was wrong AND nothing was
		// unread. Saying "healthy" here would be a claim this feature
		// has no evidence for, so the honest answer is the same one a
		// gap gets: not observed.
		return combCauseNeverObserved, false, "no cause was observed for this Comb, and no gap was reported either - read the observed signals above before treating that as healthy"
	}
	best := causes[0]
	bestSeverity := combCauseSeverity(best.State, best.Stale)
	for _, c := range causes[1:] {
		if s := combCauseSeverity(c.State, c.Stale); s > bestSeverity {
			best, bestSeverity = c, s
		}
	}
	// The worst cause's OWN staleness is returned with it, because the
	// overall badge is derived from (state, stale) together. Collapsing it
	// to state alone is how a 3-hour-old success ends up with a green
	// header badge above a correctly-greyed row.
	_, label := combCauseBadge(best.State, best.Stale)
	return best.State, best.Stale, fmt.Sprintf("%s - %s", label, best.Summary)
}

// reconcileTickSummary is the card-length description of a Comb's own
// reconcile evidence. It deliberately says WHICH of the three states it is,
// because a card that just said "reconciler_last_tick: ..." would leave an
// operator to guess whether silence meant health.
func reconcileTickSummary(e combReconcileEvidence) string {
	switch e.State {
	case combCauseObservedHealthy:
		if e.Stale {
			return fmt.Sprintf("last successful tick was %s, past this Comb's freshness limit", formatObservationAge(e.Age))
		}
		return "the last reconcile tick succeeded"
	case combCauseObservedFailed:
		if e.Stale {
			return fmt.Sprintf("the last observed reconcile attempt was %s, past this Comb's freshness limit", formatObservationAge(e.Age))
		}
		return "the last reconcile tick did not complete cleanly"
	case combCauseNeverObserved:
		return "no reconcile tick has ever been observed on this Comb"
	case combCauseNotApplicable:
		return "no Reconciler is configured on this Comb"
	default:
		return "reconcile evidence state could not be determined"
	}
}

// hostSubsystemSummary shortens one internal/hoststats.Errors entry to the
// subsystem that failed, which is the part that belongs on a card. The full
// string (including the underlying exec error) is carried in Detail.
func hostSubsystemSummary(hostErr string) string {
	subsystem, _, found := strings.Cut(hostErr, ":")
	if !found || strings.TrimSpace(subsystem) == "" {
		return "a host subsystem failed to report"
	}
	return "the " + strings.TrimSpace(subsystem) + " subsystem failed to report"
}
