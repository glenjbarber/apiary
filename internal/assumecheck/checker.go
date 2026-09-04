// Package assumecheck orchestrates Apiary's Automated Assumption Checks
// v1 (ADR-0055): a periodic, per-node pass that produces
// internal/assumptions.Result values for three continuously
// re-evaluated, system-observed checks and persists them via
// internal/assumptions.Manager. A sibling to internal/cluster.Reconciler
// in style (plain-Go dependency injection, narrow local interfaces,
// nil-able fields), but not part of that package - it never touches
// ZFS/bhyve/jail provisioning.
package assumecheck

import (
	"context"
	"errors"
	"fmt"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/glenjbarber/apiary/api/internalpb"
	rpcpb "github.com/glenjbarber/apiary/api/rpc"
	"github.com/glenjbarber/apiary/internal/assumptions"
)

// peerCheckTimeout bounds a single peer RPC - matches
// internal/manager's existing reachabilityCheckTimeout precedent
// (ADR-0052).
const peerCheckTimeout = 3 * time.Second

// Stable, small reason-code vocabulary. Never used for human-readable
// prose (that's Detail's job) - these exist purely so a history entry's
// transition identity doesn't depend on free-text error strings, which
// change even when nothing meaningful does.
const (
	ReasonNone                   = ""
	ReasonConnectionRefused      = "connection_refused"
	ReasonAuthenticationRejected = "authentication_rejected"
	ReasonDeadlineExceeded       = "deadline_exceeded"
	ReasonBridgeDown             = "bridge_down"
	ReasonBridgeStatusUnknown    = "bridge_status_unknown"
	ReasonNoDefaultRoute         = "no_default_route"
	ReasonRouteCheckFailed       = "route_check_failed"
	ReasonUplinkNotConfigured    = "uplink_not_configured"
	ReasonUplinkMismatch         = "uplink_mismatch"
	ReasonPeersNotConfigured     = "peers_not_configured"
	ReasonAuthNotConfigured      = "auth_not_configured"
	ReasonBhyveNotConfigured     = "bhyve_not_configured"
	ReasonReplicaNodeUnknown     = "replica_node_unknown"
	ReasonObservationFailed      = "observation_failed" // generic fallback for an unclassified error
)

type raftReader interface {
	Status(ctx context.Context) (*internalpb.StatusResponse, error)
	// ListVMsLocal, NOT ListVMs - that's leader-only (ADR-0009) and this
	// checker runs on every node regardless of leadership, the same
	// reasoning internal/cluster/reconciler.go's own raftClient
	// interface already documents.
	ListVMsLocal(ctx context.Context) (*internalpb.ListVMsResponse, error)
}

type peerChecker interface {
	HostStats(ctx context.Context, addr string) (*rpcpb.HostStatsResponse, error)
	GetLocalNetworkBridgeStatus(ctx context.Context, addr, networkID string) (*rpcpb.GetLocalNetworkBridgeStatusResponse, error)
}

type routeChecker interface {
	DefaultRouteInterface(ctx context.Context) (iface string, hasRoute bool, err error)
}

// Checker runs one Automated Assumption Checks tick. All fields are
// required for a meaningful RunOnce except Peers (nil-able - see
// internal/manager.Server's own nil-able peers field for the same
// convention): a node with no peer forwarding configured still runs (b),
// reporting (a) and (c)'s peer-dependent halves as StatusUnknown rather
// than skipping the tick.
type Checker struct {
	// NodeID MUST be the raft node ID (raftStatus.GetNodeId(), the same
	// value cmd/managerd's own main() captures as raftNodeID) - NOT the
	// -node-id flag's value, which only ever labels Status/HostStats
	// responses and may differ. Using the wrong one would silently
	// exclude this node from its own peer list and match zero VMs for
	// assumption (c).
	NodeID string

	Raft             raftReader
	Peers            peerChecker // nil-able
	PeerManagerdAddr func(raftAddress string) string
	Route            routeChecker

	// Uplink MUST be the already-resolved value (reconciler.Uplink,
	// read after cmd/managerd's own nodeconfig-override application and
	// nat-uplink-falls-back-to-vlan-uplink logic) - never re-derived
	// independently, which would risk a second, driftable copy of the
	// same resolution logic and could report on NAT when this node
	// isn't actually running managed-network NAT at all.
	Uplink string

	// PeerTLSConfigured/PeerAuthKeyConfigured reflect THIS node's own
	// outgoing peer-call configuration (-peer-tls, -peer-api-key != "")
	// - they do NOT imply the remote peer enforces either, see
	// PEER_SECURITY_PATH_ACCEPTED's own doc comment.
	PeerTLSConfigured     bool
	PeerAuthKeyConfigured bool

	Store *assumptions.Manager

	HeartbeatInterval time.Duration
	RunDeadline       time.Duration
	HistoryLimit      int
	HistoryMaxAge     time.Duration
}

type hostStatsCall struct {
	resp *rpcpb.HostStatsResponse
	err  error
}

type bridgeCacheKey struct{ addr, networkID string }

type bridgeCall struct {
	resp *rpcpb.GetLocalNetworkBridgeStatusResponse
	err  error
}

// RunOnce attempts assumptions (a), (b), and (c) independently - a
// failure gathering one never discards results already gathered from
// another - and makes exactly one Store.Append call at the end with
// whatever was produced. The whole tick is bounded by RunDeadline so one
// unresponsive peer can't stall subsequent ticks; HostStats/
// GetLocalNetworkBridgeStatus calls are cached per tick by address (and,
// for bridge checks, by (address, network id)) so N VMs sharing one
// replica target/network produce one call each, not N.
func (c *Checker) RunOnce(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, c.RunDeadline)
	defer cancel()

	now := time.Now()
	var results []assumptions.Result
	var errs []error

	hostStatsCache := make(map[string]hostStatsCall)
	getHostStats := func(addr string) (*rpcpb.HostStatsResponse, error) {
		if v, ok := hostStatsCache[addr]; ok {
			return v.resp, v.err
		}
		cctx, cancel := context.WithTimeout(ctx, peerCheckTimeout)
		defer cancel()
		resp, err := c.Peers.HostStats(cctx, addr)
		hostStatsCache[addr] = hostStatsCall{resp, err}
		return resp, err
	}

	bridgeCache := make(map[bridgeCacheKey]bridgeCall)
	getBridgeStatus := func(addr, networkID string) (*rpcpb.GetLocalNetworkBridgeStatusResponse, error) {
		key := bridgeCacheKey{addr, networkID}
		if v, ok := bridgeCache[key]; ok {
			return v.resp, v.err
		}
		cctx, cancel := context.WithTimeout(ctx, peerCheckTimeout)
		defer cancel()
		resp, err := c.Peers.GetLocalNetworkBridgeStatus(cctx, addr, networkID)
		bridgeCache[key] = bridgeCall{resp, err}
		return resp, err
	}

	raftStatus, statusErr := c.Raft.Status(ctx)
	var nodeAddrByID map[string]string
	if statusErr != nil {
		errs = append(errs, fmt.Errorf("assumecheck: raft status: %w", statusErr))
	} else {
		nodeAddrByID = make(map[string]string, len(raftStatus.GetServers()))
		for _, srv := range raftStatus.GetServers() {
			nodeAddrByID[srv.GetId()] = c.PeerManagerdAddr(srv.GetAddress())
		}
		results = append(results, c.checkPeers(raftStatus, now, getHostStats)...)
	}

	results = append(results, c.checkNATUplink(ctx, now))

	replicaResults, err := c.checkReplicaTargets(ctx, now, nodeAddrByID, getHostStats, getBridgeStatus)
	if err != nil {
		errs = append(errs, err)
	} else {
		results = append(results, replicaResults...)
	}

	if len(results) > 0 {
		if err := c.Store.Append(results, c.HeartbeatInterval, c.HistoryLimit, c.HistoryMaxAge); err != nil {
			errs = append(errs, fmt.Errorf("assumecheck: persisting results: %w", err))
		}
	}

	return errors.Join(errs...)
}

// checkPeers produces both PEER_MANAGER_RPC_SUCCEEDED and
// PEER_SECURITY_PATH_ACCEPTED results for every raft server other than
// this node.
func (c *Checker) checkPeers(raftStatus *internalpb.StatusResponse, now time.Time, getHostStats func(string) (*rpcpb.HostStatsResponse, error)) []assumptions.Result {
	var results []assumptions.Result
	for _, srv := range raftStatus.GetServers() {
		if srv.GetId() == c.NodeID {
			continue
		}
		addr := c.PeerManagerdAddr(srv.GetAddress())
		peersConfigured := c.Peers != nil

		var callErr error
		if peersConfigured {
			_, callErr = getHostStats(addr)
		}

		rpcKey := assumptions.Key{
			Kind: assumptions.KindPeerManagerRPCSucceeded, SubjectKind: assumptions.SubjectKindNode,
			DependencyID: srv.GetId(), ObservedByNodeID: c.NodeID,
		}
		rpcStatus, rpcReason, rpcDetail := classifyPeerRPC(peersConfigured, callErr)
		results = append(results, assumptions.Result{
			Key: rpcKey, ObservedStatus: rpcStatus, ReasonCode: rpcReason,
			Detail: assumptions.ClampDetail(rpcDetail), LastObservedAt: now,
		})

		authKey := assumptions.Key{
			Kind: assumptions.KindPeerSecurityPathAccepted, SubjectKind: assumptions.SubjectKindNode,
			DependencyID: srv.GetId(), ObservedByNodeID: c.NodeID,
		}
		authStatus, authReason, authDetail := c.classifyAuth(peersConfigured, callErr)
		results = append(results, assumptions.Result{
			Key: authKey, ObservedStatus: authStatus, ReasonCode: authReason,
			Detail: assumptions.ClampDetail(authDetail), LastObservedAt: now,
		})
	}
	return results
}

func classifyPeerRPC(peersConfigured bool, callErr error) (assumptions.Status, string, string) {
	if !peersConfigured {
		return assumptions.StatusUnknown, ReasonPeersNotConfigured, "no peer forwarding configured on this node"
	}
	if callErr == nil {
		return assumptions.StatusTrue, ReasonNone, ""
	}
	switch status.Code(callErr) {
	case codes.Unauthenticated, codes.PermissionDenied:
		return assumptions.StatusFalse, ReasonAuthenticationRejected, callErr.Error()
	case codes.DeadlineExceeded:
		return assumptions.StatusFalse, ReasonDeadlineExceeded, callErr.Error()
	case codes.Unavailable:
		return assumptions.StatusFalse, ReasonConnectionRefused, callErr.Error()
	default:
		return assumptions.StatusFalse, ReasonObservationFailed, callErr.Error()
	}
}

// classifyAuth reports PEER_SECURITY_PATH_ACCEPTED - see that Key's own
// doc comment (api/rpc/manager.proto) for exactly what this does and
// does not prove. Applicability is evaluated first and purely from
// local config, independent of connectivity: whether a security path is
// even configured is knowable without dialing anything.
func (c *Checker) classifyAuth(peersConfigured bool, callErr error) (assumptions.Status, string, string) {
	if !c.PeerTLSConfigured && !c.PeerAuthKeyConfigured {
		return assumptions.StatusNotApplicable, ReasonAuthNotConfigured, "no TLS or API key configured for peer calls on this node"
	}
	if !peersConfigured {
		return assumptions.StatusUnknown, ReasonPeersNotConfigured, "no peer forwarding configured on this node"
	}
	if callErr == nil {
		path := "plaintext"
		if c.PeerTLSConfigured {
			path = "TLS"
		}
		return assumptions.StatusTrue, ReasonNone, fmt.Sprintf(
			"call accepted over the configured %s security path; this does not confirm the peer enforces it", path)
	}
	switch status.Code(callErr) {
	case codes.Unauthenticated, codes.PermissionDenied:
		return assumptions.StatusFalse, ReasonAuthenticationRejected, callErr.Error()
	default:
		return assumptions.StatusUnknown, ReasonObservationFailed,
			"call failed before the security path could be evaluated: " + callErr.Error()
	}
}

// checkNATUplink produces the single NAT_UPLINK_DEFAULT_ROUTE result for
// this node.
func (c *Checker) checkNATUplink(ctx context.Context, now time.Time) assumptions.Result {
	key := assumptions.Key{
		Kind: assumptions.KindNATUplinkDefaultRoute, SubjectKind: assumptions.SubjectKindNode,
		DependencyID: c.Uplink, ObservedByNodeID: c.NodeID,
	}
	if c.Uplink == "" {
		return assumptions.Result{
			Key: key, ObservedStatus: assumptions.StatusNotApplicable, ReasonCode: ReasonUplinkNotConfigured,
			Detail: "this node is not running managed-network NAT", LastObservedAt: now,
		}
	}

	iface, hasRoute, err := c.Route.DefaultRouteInterface(ctx)
	switch {
	case err != nil:
		return assumptions.Result{
			Key: key, ObservedStatus: assumptions.StatusUnknown, ReasonCode: ReasonRouteCheckFailed,
			Detail: assumptions.ClampDetail(err.Error()), LastObservedAt: now,
		}
	case !hasRoute:
		return assumptions.Result{
			Key: key, ObservedStatus: assumptions.StatusFalse, ReasonCode: ReasonNoDefaultRoute,
			Detail: "this node has no default route at all", LastObservedAt: now,
		}
	case iface == c.Uplink:
		return assumptions.Result{Key: key, ObservedStatus: assumptions.StatusTrue, LastObservedAt: now}
	default:
		return assumptions.Result{
			Key: key, ObservedStatus: assumptions.StatusFalse, ReasonCode: ReasonUplinkMismatch,
			Detail:         fmt.Sprintf("default route is via %q, not the configured uplink %q", iface, c.Uplink),
			LastObservedAt: now,
		}
	}
}

// checkReplicaTargets produces REPLICA_BHYVE_CONFIGURED and (when the VM
// has a network_id) REPLICA_NETWORK_BRIDGE_UP results for every VM this
// node owns with a replica_node_id set. VMs only - JailDefinition has no
// bhyve concept and no network_id field in v1, so a jail with
// replica_node_id set produces no results here at all (see ADR-0055).
func (c *Checker) checkReplicaTargets(
	ctx context.Context, now time.Time, nodeAddrByID map[string]string,
	getHostStats func(string) (*rpcpb.HostStatsResponse, error),
	getBridgeStatus func(string, string) (*rpcpb.GetLocalNetworkBridgeStatusResponse, error),
) ([]assumptions.Result, error) {
	if nodeAddrByID == nil {
		// raft Status already failed this tick and was recorded once -
		// nothing meaningful can be checked without node addresses.
		return nil, nil
	}

	resp, err := c.Raft.ListVMsLocal(ctx)
	if err != nil {
		return nil, fmt.Errorf("assumecheck: listing VMs: %w", err)
	}
	if resp.GetError() != "" {
		return nil, fmt.Errorf("assumecheck: listing VMs: %s", resp.GetError())
	}

	var results []assumptions.Result
	for _, vm := range resp.GetVms() {
		if vm.GetNodeId() != c.NodeID || vm.GetReplicaNodeId() == "" {
			continue
		}

		bhyveKey := assumptions.Key{
			Kind: assumptions.KindReplicaBhyveConfigured, SubjectKind: assumptions.SubjectKindVM,
			SubjectID: vm.GetId(), DependencyID: vm.GetReplicaNodeId(), ObservedByNodeID: c.NodeID,
		}
		var networkKey *assumptions.Key
		if vm.GetNetworkId() != "" {
			k := assumptions.Key{
				Kind: assumptions.KindReplicaNetworkBridgeUp, SubjectKind: assumptions.SubjectKindVM,
				SubjectID: vm.GetId(), DependencyID: vm.GetReplicaNodeId(), Qualifier: vm.GetNetworkId(),
				ObservedByNodeID: c.NodeID,
			}
			networkKey = &k
		}

		addr, known := nodeAddrByID[vm.GetReplicaNodeId()]
		switch {
		case !known:
			results = append(results, assumptions.Result{
				Key: bhyveKey, ObservedStatus: assumptions.StatusUnknown, ReasonCode: ReasonReplicaNodeUnknown,
				Detail: "replica node not found in current raft membership", LastObservedAt: now,
			})
			if networkKey != nil {
				results = append(results, assumptions.Result{
					Key: *networkKey, ObservedStatus: assumptions.StatusUnknown, ReasonCode: ReasonReplicaNodeUnknown,
					Detail: "replica node not found in current raft membership", LastObservedAt: now,
				})
			}
		case c.Peers == nil:
			results = append(results, assumptions.Result{
				Key: bhyveKey, ObservedStatus: assumptions.StatusUnknown, ReasonCode: ReasonPeersNotConfigured,
				Detail: "no peer forwarding configured on this node", LastObservedAt: now,
			})
			if networkKey != nil {
				results = append(results, assumptions.Result{
					Key: *networkKey, ObservedStatus: assumptions.StatusUnknown, ReasonCode: ReasonPeersNotConfigured,
					Detail: "no peer forwarding configured on this node", LastObservedAt: now,
				})
			}
		default:
			hsResp, hsErr := getHostStats(addr)
			bStatus, bReason, bDetail := classifyBhyveConfigured(hsResp, hsErr)
			results = append(results, assumptions.Result{
				Key: bhyveKey, ObservedStatus: bStatus, ReasonCode: bReason,
				Detail: assumptions.ClampDetail(bDetail), LastObservedAt: now,
			})

			if networkKey != nil {
				brResp, brErr := getBridgeStatus(addr, vm.GetNetworkId())
				nStatus, nReason, nDetail := classifyBridge(brResp, brErr)
				results = append(results, assumptions.Result{
					Key: *networkKey, ObservedStatus: nStatus, ReasonCode: nReason,
					Detail: assumptions.ClampDetail(nDetail), LastObservedAt: now,
				})
			}
		}
	}
	return results, nil
}

func classifyBhyveConfigured(resp *rpcpb.HostStatsResponse, callErr error) (assumptions.Status, string, string) {
	if callErr != nil {
		return assumptions.StatusUnknown, ReasonObservationFailed, callErr.Error()
	}
	if resp.GetBhyveConfigured() {
		return assumptions.StatusTrue, ReasonNone, ""
	}
	return assumptions.StatusFalse, ReasonBhyveNotConfigured, "replica node's managerd was not started with -bhyve-bootrom"
}

// classifyBridge maps GetLocalNetworkBridgeStatusResponse honestly: an
// RPC-level error or an unrecognized network id (which may just be
// replication lag, not a genuinely absent bridge) is UNKNOWN, never
// FALSE - only an explicit "down" bridge_status is ever a definitive
// false.
func classifyBridge(resp *rpcpb.GetLocalNetworkBridgeStatusResponse, callErr error) (assumptions.Status, string, string) {
	if callErr != nil {
		return assumptions.StatusUnknown, ReasonObservationFailed, callErr.Error()
	}
	if resp.GetError() != "" {
		return assumptions.StatusUnknown, ReasonBridgeStatusUnknown, resp.GetError()
	}
	switch resp.GetBridgeStatus() {
	case "up":
		return assumptions.StatusTrue, ReasonNone, ""
	case "down":
		return assumptions.StatusFalse, ReasonBridgeDown, "bridge exists but is not up"
	default:
		return assumptions.StatusUnknown, ReasonBridgeStatusUnknown,
			"underlying signal can't distinguish \"no VLAN support there\" from \"not yet created\""
	}
}
