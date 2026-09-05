package manager

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"time"

	"google.golang.org/protobuf/proto"

	internalpb "github.com/glenjbarber/apiary/api/internalpb"
	rpcpb "github.com/glenjbarber/apiary/api/rpc"
	"github.com/glenjbarber/apiary/internal/assumptions"
	"github.com/glenjbarber/apiary/internal/cluster"
	"github.com/glenjbarber/apiary/internal/hoststats"
	"github.com/glenjbarber/apiary/internal/isostore"
	"github.com/glenjbarber/apiary/internal/nodeconfig"
)

// defaultApplyTimeout is used when a request doesn't specify one.
const defaultApplyTimeout = 10 * time.Second

// isoManager is the subset of *isostore.Manager the server needs,
// defined locally so tests can supply a fake - the same reasoning
// internal/cluster's own local interfaces follow.
type isoManager interface {
	Save(name string, r io.Reader, expectedSHA256 string) (*isostore.Info, error)
	List() ([]isostore.Info, error)
	Delete(name string) error

	// Path resolves name to its local file path, for PushISOTo
	// (ADR-0040) to re-open and stream an already-stored file to
	// another node - already exists on *isostore.Manager, just not
	// exposed through this local interface until now.
	Path(name string) (path string, ok bool, err error)
}

// VNCLookup is the subset of *bhyve.Manager the server needs for
// GetVMConsole, defined locally for the same reason as isoManager.
type VNCLookup interface {
	VNCPort(name string) (port int, ok bool, err error)
}

// SerialLogLookup is the subset of *bhyve.Manager the server needs for
// GetVMSerialLog, defined locally for the same reason as isoManager.
type SerialLogLookup interface {
	SerialLogPath(name string) (path string, ok bool, err error)
}

// VLANStatus is the subset of *vlan.Manager the server needs for
// ListNetworks's per-node bridge status, defined locally for the same
// reason as isoManager.
type VLANStatus interface {
	InterfaceStatus(ctx context.Context, name string) (exists, up bool, err error)
}

// PeerForwarder is the subset of *PeerReporter the server needs to
// forward an operation rejected by this node's own raftd (not the
// leader) to the current leader's own managerd instead - originally
// just the leader-only reads (ADR-0035), now also the external write
// RPCs a caller might aim at any node (ADR-0036's follow-up) -
// mirrors internal/cluster's own peerReporter interface, which serves
// the same forwarding role for the reconciler's internal phase-report
// writes (ADR-0029).
type PeerForwarder interface {
	ListVMs(ctx context.Context, addr string) (*rpcpb.ListVMsResponse, error)
	GetVM(ctx context.Context, addr, id string) (*rpcpb.GetVMResponse, error)
	ListJails(ctx context.Context, addr string) (*rpcpb.ListJailsResponse, error)
	GetJail(ctx context.Context, addr, id string) (*rpcpb.GetJailResponse, error)
	ListNetworks(ctx context.Context, addr string) (*rpcpb.ListNetworksResponse, error)
	ListISOs(ctx context.Context, addr string) (*rpcpb.ListISOsResponse, error)

	CreateVM(ctx context.Context, addr string, req *rpcpb.CreateVMRequest) (*rpcpb.CreateVMResponse, error)
	UpdateVM(ctx context.Context, addr string, req *rpcpb.UpdateVMRequest) (*rpcpb.UpdateVMResponse, error)
	DeleteVM(ctx context.Context, addr string, req *rpcpb.DeleteVMRequest) (*rpcpb.DeleteVMResponse, error)
	CreateJail(ctx context.Context, addr string, req *rpcpb.CreateJailRequest) (*rpcpb.CreateJailResponse, error)
	UpdateJail(ctx context.Context, addr string, req *rpcpb.UpdateJailRequest) (*rpcpb.UpdateJailResponse, error)
	DeleteJail(ctx context.Context, addr string, req *rpcpb.DeleteJailRequest) (*rpcpb.DeleteJailResponse, error)
	CreateNetwork(ctx context.Context, addr string, req *rpcpb.CreateNetworkRequest) (*rpcpb.CreateNetworkResponse, error)
	DeleteNetwork(ctx context.Context, addr string, req *rpcpb.DeleteNetworkRequest) (*rpcpb.DeleteNetworkResponse, error)
	CreateAPIKey(ctx context.Context, addr string, req *rpcpb.CreateAPIKeyRequest) (*rpcpb.CreateAPIKeyResponse, error)
	RevokeAPIKey(ctx context.Context, addr string, req *rpcpb.RevokeAPIKeyRequest) (*rpcpb.RevokeAPIKeyResponse, error)
	ListAPIKeys(ctx context.Context, addr string) (*rpcpb.ListAPIKeysResponse, error)

	ForcePurgeVM(ctx context.Context, addr string, req *rpcpb.ForcePurgeVMRequest) (*rpcpb.ForcePurgeVMResponse, error)
	MigrateVM(ctx context.Context, addr string, req *rpcpb.MigrateVMRequest) (*rpcpb.MigrateVMResponse, error)
	SetVMFirewallPaused(ctx context.Context, addr string, req *rpcpb.SetVMFirewallPausedRequest) (*rpcpb.SetVMFirewallPausedResponse, error)
	SetVMCloudflareExposure(ctx context.Context, addr string, req *rpcpb.SetVMCloudflareExposureRequest) (*rpcpb.SetVMCloudflareExposureResponse, error)
	ForcePurgeJail(ctx context.Context, addr string, req *rpcpb.ForcePurgeJailRequest) (*rpcpb.ForcePurgeJailResponse, error)
	MigrateJail(ctx context.Context, addr string, req *rpcpb.MigrateJailRequest) (*rpcpb.MigrateJailResponse, error)

	// UploadISO streams a local file to addr's own UploadISO RPC - used
	// by PushISOTo (the source node's side of on-demand image fetching,
	// ADR-0041) to push a file this node already has to whichever peer
	// asked for it.
	UploadISO(ctx context.Context, addr, name, expectedSHA256 string, r io.Reader) error

	// SimulateNodeFailure forwards the entire original request to addr's
	// own SimulateNodeFailure RPC - see ADR-0052, and this file's own
	// SimulateNodeFailure handler for why the whole request (not just
	// one failed sub-call) is forwarded on a leader-hint rejection.
	SimulateNodeFailure(ctx context.Context, addr string, req *rpcpb.SimulateNodeFailureRequest) (*rpcpb.SimulateNodeFailureResponse, error)
	SimulateNetworkFailure(ctx context.Context, addr string, req *rpcpb.SimulateNetworkFailureRequest) (*rpcpb.SimulateNetworkFailureResponse, error)
	TraceCellPath(ctx context.Context, addr string, req *rpcpb.TraceCellPathRequest) (*rpcpb.TraceCellPathResponse, error)

	// HostStats forwards to addr's own HostStats RPC - already used by
	// internal/frontend's own separate peer-client interface for the
	// cluster overview page (ADR-0036); exposed here too so
	// SimulateNodeFailure can reuse the exact same reachability signal
	// (a successful call means the peer is up) rather than inventing a
	// new ping.
	HostStats(ctx context.Context, addr string) (*rpcpb.HostStatsResponse, error)
	GetLocalNetworkBridgeStatus(ctx context.Context, addr, networkID string) (*rpcpb.GetLocalNetworkBridgeStatusResponse, error)
	ListAssumptionResults(ctx context.Context, addr string, req *rpcpb.ListAssumptionResultsRequest) (*rpcpb.ListAssumptionResultsResponse, error)
}

// reconcilerStats is the subset of *cluster.Reconciler the server needs
// for Evidence-Aware Health's (ADR-0056) "last successful reconciliation"
// signal, defined locally so tests can supply a fake - the same reasoning
// isoManager/VLANStatus already follow. nil on a node built without a
// Reconciler - HostStats reports the three reconcile fields as zero
// rather than panicking, matching every other nil-able dependency here.
type reconcilerStats interface {
	LastReconcileAttempt() (time.Time, bool)
	LastReconcileSuccess() (time.Time, bool)
	ReconcileInterval() time.Duration
}

// assumptionStore is the subset of *assumptions.Manager the server
// needs, defined locally so tests can supply a fake - the same
// reasoning isoManager/VLANStatus already follow. nil on a node that
// never got a store wired up - ListAssumptionResults reports a clear
// error rather than panicking, matching every other nil-able dependency
// here.
type assumptionStore interface {
	Load() ([]assumptions.Result, []assumptions.HistoryEntry, error)
	Degraded() (bool, string)
}

// defaultPeerManagerdPort mirrors internal/cluster's own constant of
// the same name and purpose - used when Server.peerManagerdPort is
// unset. Duplicated rather than shared across the package boundary,
// the same small-duplication tradeoff internal/cluster/peer.go already
// documents for this exact value.
const defaultPeerManagerdPort = "17700"

// Server implements the generated ManagerServiceServer interface, the
// server side of managerd's external RPC API.
type Server struct {
	rpcpb.UnimplementedManagerServiceServer

	raft   *RaftClient
	nodeID string
	isos   isoManager

	// vnc is nil on a node with no bhyve provisioning configured (see
	// cmd/managerd's own nil-able Reconciler.Bhyve) - GetVMConsole
	// reports Available=false rather than panicking in that case.
	vnc VNCLookup

	// serialLog is nil under the same condition as vnc, for the same
	// reason - GetVMSerialLog reports Available=false rather than
	// panicking.
	serialLog SerialLogLookup

	// vlan is nil on a node with no VLAN support configured (see
	// cmd/managerd's own nil-able Reconciler.VLAN) - ListNetworks
	// reports "unknown" bridge status rather than panicking in that case.
	vlan VLANStatus

	// statsGather defaults to hoststats.Gather in NewServer; overridable
	// in tests so HostStats's RPC-translation logic can be exercised
	// without shelling out to real system commands.
	statsGather func(context.Context) *hoststats.Snapshot

	// peers is nil on a node with no peer forwarding configured (see
	// cmd/managerd's own -peer-api-key) - a leader-only read rejected by
	// this node's own raftd then just returns the LeaderHint error as
	// before (ADR-0035), rather than forwarding.
	peers PeerForwarder

	// peerManagerdPort mirrors internal/cluster's own field of the same
	// name and purpose - empty uses defaultPeerManagerdPort.
	peerManagerdPort string

	// zfs is nil on a node with no ZFS Base configured - SetDatasetQuota
	// reports an error rather than panicking in that case. Physical,
	// per-node data like isos above - never routed through raft.
	zfs quotaSetter

	// nodeConfig is nil on a node that never got a node-config file path
	// wired up - GetNodeConfig/UpdateNodeConfig report an error rather
	// than panicking. See ADR-0049/internal/nodeconfig.
	nodeConfig nodeConfigStore

	// assumptions is nil on a node that never got an assumptions store
	// wired up - ListAssumptionResults reports an error rather than
	// panicking. Physical, per-node data like isos/nodeConfig above,
	// never routed through raft. See ADR-0055/internal/assumptions.
	assumptions assumptionStore

	// assumptionStaleAfter is the age past which ListAssumptionResults
	// collapses a snapshot entry's effective status to UNKNOWN,
	// regardless of its stored observed_status - see that handler's own
	// doc comment.
	assumptionStaleAfter time.Duration

	// reconciler is nil on a node built without a Reconciler - HostStats
	// reports zero for the three reconcile fields rather than panicking.
	// Physical, per-node data like assumptions/isos above, never routed
	// through raft. See ADR-0056.
	reconciler reconcilerStats
}

// quotaSetter is the subset of *zfs.Manager SetDatasetQuota needs,
// defined locally so it can be faked in tests without a real zfs(8)
// binary - the same reasoning isoManager/VLANStatus already follow.
type quotaSetter interface {
	SetProperty(ctx context.Context, name, prop, value string) error
}

// nodeConfigStore is the subset of *nodeconfig.Manager GetNodeConfig/
// UpdateNodeConfig need, defined locally for the same fakeability
// reason as quotaSetter above.
type nodeConfigStore interface {
	Load() (nodeconfig.Config, error)
	Save(nodeconfig.Config) error
}

var _ rpcpb.ManagerServiceServer = (*Server)(nil)

// NewServer returns a Server that answers external RPCs using raft to
// reach raftd, reporting nodeID as its own identity, isos to store
// installer images locally on this node, vnc (nil-able) to look up a
// running VM's VNC console port, serialLog (nil-able) to look up a
// running VM's captured serial console log, vlanMgr (nil-able) to
// report a network's bridge status on this node, peers/
// peerManagerdPort (peers nil-able) to forward a leader-only read
// rejected by this node's own raftd to the current leader's own
// managerd instead (ADR-0035), zfsMgr (nil-able) to set a dataset
// quota locally, nodeConfig (nil-able) to read/write this node's own
// local settings file (ADR-0049), assumptionStoreMgr (nil-able) to read
// this node's persisted Automated Assumption Checks (ADR-0055),
// assumptionStaleAfter to compute their effective staleness, and
// reconciler (nil-able) for Evidence-Aware Health's (ADR-0056) reconcile
// signals. These are appended at the end (rather than interleaved with
// the params above) specifically to keep every existing positional
// NewServer(...) call site a mechanical one-line edit.
func NewServer(raft *RaftClient, nodeID string, isos isoManager, vnc VNCLookup, serialLog SerialLogLookup, vlanMgr VLANStatus, peers PeerForwarder, peerManagerdPort string, zfsMgr quotaSetter, nodeConfig nodeConfigStore, assumptionStoreMgr assumptionStore, assumptionStaleAfter time.Duration, reconciler reconcilerStats) *Server {
	return &Server{raft: raft, nodeID: nodeID, isos: isos, vnc: vnc, serialLog: serialLog, vlan: vlanMgr, statsGather: hoststats.Gather, peers: peers, peerManagerdPort: peerManagerdPort, zfs: zfsMgr, nodeConfig: nodeConfig, assumptions: assumptionStoreMgr, assumptionStaleAfter: assumptionStaleAfter, reconciler: reconciler}
}

// peerManagerdAddr turns a raft leader_hint (the leader's raft
// transport address, e.g. "10.50.0.14:17600") into that same node's
// managerd address, by keeping the host and substituting the
// configured/default managerd port - mirrors internal/cluster's own
// resolvePeerManagerdAddr exactly (see its doc comment for the full
// reasoning), duplicated across the package boundary for the same
// reason defaultPeerManagerdPort is.
func (s *Server) peerManagerdAddr(leaderHint string) string {
	port := s.peerManagerdPort
	if port == "" {
		port = defaultPeerManagerdPort
	}
	host, _, err := net.SplitHostPort(leaderHint)
	if err != nil {
		host = leaderHint
	}
	return net.JoinHostPort(host, port)
}

// Status implements rpcpb.ManagerServiceServer. If raftd is unreachable,
// it still returns a normal response with RaftReachable=false and
// RaftError set, rather than a gRPC error, so callers always get a
// diagnosable payload.
func (s *Server) Status(ctx context.Context, _ *rpcpb.StatusRequest) (*rpcpb.StatusResponse, error) {
	resp := &rpcpb.StatusResponse{ManagerNodeId: s.nodeID}

	raftStatus, err := s.raft.Status(ctx)
	if err != nil {
		resp.RaftReachable = false
		resp.RaftError = err.Error()
		return resp, nil
	}

	resp.RaftReachable = true
	resp.RaftIsLeader = raftStatus.GetIsLeader()
	resp.RaftLeaderId = raftStatus.GetLeaderId()
	resp.RaftNodeId = raftStatus.GetNodeId()
	resp.RaftLastLogIndex = raftStatus.GetLastLogIndex()
	resp.RaftAppliedIndex = raftStatus.GetAppliedIndex()
	resp.RaftState = raftStatus.GetRaftState()
	for _, server := range raftStatus.GetServers() {
		resp.KnownNodeIds = append(resp.KnownNodeIds, server.GetId())
		resp.Members = append(resp.Members, &rpcpb.RaftMember{
			NodeId: server.GetId(), Address: server.GetAddress(), Suffrage: server.GetSuffrage(),
		})
	}
	return resp, nil
}

// applyCommand marshals cmd, submits it via raft, and decodes the result.
// The three return strings (vm, appErr, leaderHint) mirror how the
// internal protocol itself reports outcomes: appErr covers both a
// connection-level failure reaching raftd and an application-level
// rejection (e.g. duplicate/missing VM id) - both are user-facing errors
// from an external caller's perspective, just surfaced as response
// fields rather than a gRPC error, consistent with Status above.
func (s *Server) applyCommand(ctx context.Context, cmd *internalpb.Command, timeoutMs uint32) (vm *internalpb.VMDefinition, appErr, leaderHint string) {
	timeout := defaultApplyTimeout
	if timeoutMs > 0 {
		timeout = time.Duration(timeoutMs) * time.Millisecond
	}

	payload, err := proto.Marshal(cmd)
	if err != nil {
		return nil, err.Error(), ""
	}

	resp, err := s.raft.Apply(ctx, payload, timeout)
	if err != nil {
		return nil, err.Error(), ""
	}
	if resp.GetError() != "" {
		return nil, resp.GetError(), resp.GetLeaderHint()
	}

	vm = &internalpb.VMDefinition{}
	if err := proto.Unmarshal(resp.GetResult(), vm); err != nil {
		return nil, err.Error(), ""
	}
	return vm, "", ""
}

// applyNetworkCommand mirrors applyCommand, for commands whose result is
// a NetworkDefinition instead of a VMDefinition (CreateNetwork/
// DeleteNetwork) - see internal/raft's FSMApplyResult, which likewise
// carries exactly one of VM/Network depending on the command applied.
func (s *Server) applyNetworkCommand(ctx context.Context, cmd *internalpb.Command, timeoutMs uint32) (network *internalpb.NetworkDefinition, appErr, leaderHint string) {
	timeout := defaultApplyTimeout
	if timeoutMs > 0 {
		timeout = time.Duration(timeoutMs) * time.Millisecond
	}

	payload, err := proto.Marshal(cmd)
	if err != nil {
		return nil, err.Error(), ""
	}

	resp, err := s.raft.Apply(ctx, payload, timeout)
	if err != nil {
		return nil, err.Error(), ""
	}
	if resp.GetError() != "" {
		return nil, resp.GetError(), resp.GetLeaderHint()
	}

	network = &internalpb.NetworkDefinition{}
	if err := proto.Unmarshal(resp.GetResult(), network); err != nil {
		return nil, err.Error(), ""
	}
	return network, "", ""
}

// CreateNetwork implements rpcpb.ManagerServiceServer. A rejection
// specifically for not being the leader is forwarded to the leader's
// own managerd when peer forwarding is configured (ADR-0036's
// follow-up to ADR-0035, extending forwarding from reads to writes) -
// otherwise a caller hitting a non-leader node could never create
// anything at all.
func (s *Server) CreateNetwork(ctx context.Context, req *rpcpb.CreateNetworkRequest) (*rpcpb.CreateNetworkResponse, error) {
	cmd := &internalpb.Command{
		Op: &internalpb.Command_CreateNetwork{CreateNetwork: &internalpb.CreateNetwork{Network: toInternalNetwork(req.GetNetwork())}},
	}
	network, appErr, leaderHint := s.applyNetworkCommand(ctx, cmd, req.GetTimeoutMs())
	if leaderHint != "" && s.peers != nil {
		if fwd, ferr := s.peers.CreateNetwork(ctx, s.peerManagerdAddr(leaderHint), req); ferr == nil {
			return fwd, nil
		}
	}
	return &rpcpb.CreateNetworkResponse{Network: fromInternalNetwork(network), Error: appErr, LeaderHint: leaderHint}, nil
}

// DeleteNetwork implements rpcpb.ManagerServiceServer. See CreateNetwork's
// doc comment for the forwarding rationale, identical here.
func (s *Server) DeleteNetwork(ctx context.Context, req *rpcpb.DeleteNetworkRequest) (*rpcpb.DeleteNetworkResponse, error) {
	cmd := &internalpb.Command{
		Op: &internalpb.Command_DeleteNetwork{DeleteNetwork: &internalpb.DeleteNetwork{Id: req.GetId()}},
	}
	network, appErr, leaderHint := s.applyNetworkCommand(ctx, cmd, req.GetTimeoutMs())
	if leaderHint != "" && s.peers != nil {
		if fwd, ferr := s.peers.DeleteNetwork(ctx, s.peerManagerdAddr(leaderHint), req); ferr == nil {
			return fwd, nil
		}
	}
	return &rpcpb.DeleteNetworkResponse{Network: fromInternalNetwork(network), Error: appErr, LeaderHint: leaderHint}, nil
}

// ListNetworks implements rpcpb.ManagerServiceServer.
func (s *Server) ListNetworks(ctx context.Context, _ *rpcpb.ListNetworksRequest) (*rpcpb.ListNetworksResponse, error) {
	resp, err := s.raft.ListNetworks(ctx)
	if err != nil {
		return &rpcpb.ListNetworksResponse{Error: err.Error()}, nil
	}
	if resp.GetError() != "" {
		if s.peers != nil && resp.GetLeaderHint() != "" {
			if fwd, ferr := s.peers.ListNetworks(ctx, s.peerManagerdAddr(resp.GetLeaderHint())); ferr == nil {
				return fwd, nil
			}
		}
		return &rpcpb.ListNetworksResponse{Error: resp.GetError(), LeaderHint: resp.GetLeaderHint()}, nil
	}
	networks := make([]*rpcpb.NetworkDefinition, 0, len(resp.GetNetworks()))
	for _, n := range resp.GetNetworks() {
		rn := fromInternalNetwork(n)
		rn.BridgeStatus = s.bridgeStatus(ctx, n)
		networks = append(networks, rn)
	}
	return &rpcpb.ListNetworksResponse{Networks: networks}, nil
}

// applyAPIKeyCommand mirrors applyNetworkCommand, for commands whose
// result is an ApiKey instead of a NetworkDefinition (CreateAPIKey/
// RevokeAPIKey).
func (s *Server) applyAPIKeyCommand(ctx context.Context, cmd *internalpb.Command, timeoutMs uint32) (key *internalpb.ApiKey, appErr, leaderHint string) {
	timeout := defaultApplyTimeout
	if timeoutMs > 0 {
		timeout = time.Duration(timeoutMs) * time.Millisecond
	}

	payload, err := proto.Marshal(cmd)
	if err != nil {
		return nil, err.Error(), ""
	}

	resp, err := s.raft.Apply(ctx, payload, timeout)
	if err != nil {
		return nil, err.Error(), ""
	}
	if resp.GetError() != "" {
		return nil, resp.GetError(), resp.GetLeaderHint()
	}

	key = &internalpb.ApiKey{}
	if err := proto.Unmarshal(resp.GetResult(), key); err != nil {
		return nil, err.Error(), ""
	}
	return key, "", ""
}

// CreateAPIKey implements rpcpb.ManagerServiceServer. This is the only
// place a raw key ever exists outside a caller's own hands - it's
// generated here, hashed before being submitted through raft, and
// returned in the response exactly once; the hash is all that's ever
// stored (see ADR-0023).
func (s *Server) CreateAPIKey(ctx context.Context, req *rpcpb.CreateAPIKeyRequest) (*rpcpb.CreateAPIKeyResponse, error) {
	raw, hashed, err := generateAPIKey()
	if err != nil {
		return &rpcpb.CreateAPIKeyResponse{Error: fmt.Sprintf("generating API key: %v", err)}, nil
	}

	id, err := generateAPIKeyID()
	if err != nil {
		return &rpcpb.CreateAPIKeyResponse{Error: fmt.Sprintf("generating API key id: %v", err)}, nil
	}

	cmd := &internalpb.Command{
		Op: &internalpb.Command_CreateApiKey{CreateApiKey: &internalpb.CreateAPIKey{Key: &internalpb.ApiKey{
			Id: id, Name: req.GetName(), HashedKey: hashed, CreatedUnix: time.Now().Unix(), Role: req.GetRole(),
		}}},
	}
	key, appErr, leaderHint := s.applyAPIKeyCommand(ctx, cmd, req.GetTimeoutMs())
	if appErr != "" {
		if leaderHint != "" && s.peers != nil {
			// Forwarded: the leader generates and stores its own fresh
			// raw/hashed pair - the raw/hashed values generated above are
			// simply discarded, never sent anywhere.
			if fwd, ferr := s.peers.CreateAPIKey(ctx, s.peerManagerdAddr(leaderHint), req); ferr == nil {
				return fwd, nil
			}
		}
		return &rpcpb.CreateAPIKeyResponse{Error: appErr, LeaderHint: leaderHint}, nil
	}
	return &rpcpb.CreateAPIKeyResponse{Key: fromInternalAPIKey(key), RawKey: raw}, nil
}

// RevokeAPIKey implements rpcpb.ManagerServiceServer. See CreateNetwork's
// doc comment for the forwarding rationale, identical here.
func (s *Server) RevokeAPIKey(ctx context.Context, req *rpcpb.RevokeAPIKeyRequest) (*rpcpb.RevokeAPIKeyResponse, error) {
	cmd := &internalpb.Command{
		Op: &internalpb.Command_RevokeApiKey{RevokeApiKey: &internalpb.RevokeAPIKey{Id: req.GetId()}},
	}
	_, appErr, leaderHint := s.applyAPIKeyCommand(ctx, cmd, req.GetTimeoutMs())
	if leaderHint != "" && s.peers != nil {
		if fwd, ferr := s.peers.RevokeAPIKey(ctx, s.peerManagerdAddr(leaderHint), req); ferr == nil {
			return fwd, nil
		}
	}
	return &rpcpb.RevokeAPIKeyResponse{Error: appErr, LeaderHint: leaderHint}, nil
}

// ListAPIKeys implements rpcpb.ManagerServiceServer. See ListNetworks's
// doc comment for the forwarding rationale - ListAPIKeys was missed
// from ADR-0035's original set of forwarded reads (only VM/Jail/
// Network reads were covered there); ADR-0037's follow-up closes that
// gap.
func (s *Server) ListAPIKeys(ctx context.Context, _ *rpcpb.ListAPIKeysRequest) (*rpcpb.ListAPIKeysResponse, error) {
	resp, err := s.raft.ListAPIKeys(ctx)
	if err != nil {
		return &rpcpb.ListAPIKeysResponse{Error: err.Error()}, nil
	}
	if resp.GetError() != "" {
		if s.peers != nil && resp.GetLeaderHint() != "" {
			if fwd, ferr := s.peers.ListAPIKeys(ctx, s.peerManagerdAddr(resp.GetLeaderHint())); ferr == nil {
				return fwd, nil
			}
		}
		return &rpcpb.ListAPIKeysResponse{Error: resp.GetError(), LeaderHint: resp.GetLeaderHint()}, nil
	}
	keys := make([]*rpcpb.APIKeyInfo, 0, len(resp.GetKeys()))
	for _, k := range resp.GetKeys() {
		keys = append(keys, fromInternalAPIKey(k))
	}
	return &rpcpb.ListAPIKeysResponse{Keys: keys}, nil
}

// bridgeStatus reports network's bridge interface status on this node -
// "up", "down", or "unknown" if this node has no VLAN support
// configured, or the bridge doesn't exist here yet (e.g. no VM on this
// network has been reconciled on this node).
func (s *Server) bridgeStatus(ctx context.Context, n *internalpb.NetworkDefinition) string {
	if s.vlan == nil {
		return "unknown"
	}
	exists, up, err := s.vlan.InterfaceStatus(ctx, resolveBridgeName(n))
	if err != nil || !exists {
		return "unknown"
	}
	if up {
		return "up"
	}
	return "down"
}

// GetLocalNetworkBridgeStatus implements rpcpb.ManagerServiceServer. It
// reports THIS node's own local bridge status for one network, built
// from RaftClient.ListNetworksLocal (the already-replicated network
// config, read locally - never leader-restricted) plus the existing
// local bridgeStatus helper. Deliberately NOT ListNetworks: that RPC is
// leader-only and forwards to the current leader on a non-leader node,
// which would silently report the LEADER's bridge state mislabeled as
// this node's own - see ADR-0055 for the live bug this exists to avoid.
// Never forwards, by construction - there is no leader concept here at
// all.
func (s *Server) GetLocalNetworkBridgeStatus(ctx context.Context, req *rpcpb.GetLocalNetworkBridgeStatusRequest) (*rpcpb.GetLocalNetworkBridgeStatusResponse, error) {
	resp, err := s.raft.ListNetworksLocal(ctx)
	if err != nil {
		return &rpcpb.GetLocalNetworkBridgeStatusResponse{Error: err.Error()}, nil
	}
	if resp.GetError() != "" {
		return &rpcpb.GetLocalNetworkBridgeStatusResponse{Error: resp.GetError()}, nil
	}
	for _, n := range resp.GetNetworks() {
		if n.GetId() == req.GetNetworkId() {
			return &rpcpb.GetLocalNetworkBridgeStatusResponse{BridgeStatus: s.bridgeStatus(ctx, n)}, nil
		}
	}
	// Not found locally - this may reflect replication lag rather than
	// a genuinely absent network (see ADR-0055); the caller (typically
	// internal/assumecheck) must map this to an unknown/unverified
	// outcome, never a definitive false.
	return &rpcpb.GetLocalNetworkBridgeStatusResponse{
		Error: fmt.Sprintf("network_id %q not found on this node's local FSM view", req.GetNetworkId()),
	}, nil
}

// ListAssumptionResults implements rpcpb.ManagerServiceServer. Like
// HostStats/GetVMConsole, this only answers for THIS node's own store -
// physical, per-node observational data, never routed through raft and
// never leader-forwarded (see ADR-0055). status is the EFFECTIVE value a
// consumer should trust: it collapses ANY stored observed_status -
// including NOT_APPLICABLE - to UNKNOWN once the entry's
// last_observed_at exceeds assumptionStaleAfter, since applicability
// itself can silently change if the checker stops running. This is
// computed fresh on every call, never persisted this way.
func (s *Server) ListAssumptionResults(ctx context.Context, req *rpcpb.ListAssumptionResultsRequest) (*rpcpb.ListAssumptionResultsResponse, error) {
	if s.assumptions == nil {
		return &rpcpb.ListAssumptionResultsResponse{Error: "no assumptions store configured on this node"}, nil
	}

	snapshot, history, err := s.assumptions.Load()
	if err != nil {
		return &rpcpb.ListAssumptionResultsResponse{Error: err.Error()}, nil
	}

	now := time.Now()
	latest := make([]*rpcpb.AssumptionResult, 0, len(snapshot))
	for _, r := range assumptions.LatestPerKey(snapshot) {
		latest = append(latest, toRPCAssumptionResult(r, now, s.assumptionStaleAfter))
	}

	var historyOut []*rpcpb.AssumptionHistoryEntry
	if filter := req.GetFilter(); filter != nil {
		wantKey := fromRPCAssumptionKey(filter)
		for _, h := range history {
			if h.Key == wantKey {
				historyOut = append(historyOut, toRPCAssumptionHistoryEntry(h))
			}
		}
	}

	degraded, degradedDetail := s.assumptions.Degraded()
	return &rpcpb.ListAssumptionResultsResponse{
		Latest: latest, History: historyOut,
		StorageDegraded: degraded, StorageDegradedDetail: degradedDetail,
	}, nil
}

// CreateVM implements rpcpb.ManagerServiceServer. See CreateNetwork's
// doc comment for the forwarding rationale, identical here - this is
// the exact RPC whose non-leader rejection ("raft: this node is not
// the leader") was visibly surfacing to a real user in the web UI's
// create-VM form before this forwarding existed.
func (s *Server) CreateVM(ctx context.Context, req *rpcpb.CreateVMRequest) (*rpcpb.CreateVMResponse, error) {
	cmd := &internalpb.Command{
		Op: &internalpb.Command_CreateVm{CreateVm: &internalpb.CreateVM{Vm: toInternalVM(req.GetVm())}},
	}
	vm, appErr, leaderHint := s.applyCommand(ctx, cmd, req.GetTimeoutMs())
	if leaderHint != "" && s.peers != nil {
		if fwd, ferr := s.peers.CreateVM(ctx, s.peerManagerdAddr(leaderHint), req); ferr == nil {
			return fwd, nil
		}
	}
	return &rpcpb.CreateVMResponse{Vm: fromInternalVM(vm), Error: appErr, LeaderHint: leaderHint}, nil
}

// UpdateVM implements rpcpb.ManagerServiceServer. See CreateNetwork's
// doc comment for the forwarding rationale, identical here.
func (s *Server) UpdateVM(ctx context.Context, req *rpcpb.UpdateVMRequest) (*rpcpb.UpdateVMResponse, error) {
	cmd := &internalpb.Command{
		Op: &internalpb.Command_UpdateVm{UpdateVm: &internalpb.UpdateVM{Vm: toInternalVM(req.GetVm())}},
	}
	vm, appErr, leaderHint := s.applyCommand(ctx, cmd, req.GetTimeoutMs())
	if leaderHint != "" && s.peers != nil {
		if fwd, ferr := s.peers.UpdateVM(ctx, s.peerManagerdAddr(leaderHint), req); ferr == nil {
			return fwd, nil
		}
	}
	return &rpcpb.UpdateVMResponse{Vm: fromInternalVM(vm), Error: appErr, LeaderHint: leaderHint}, nil
}

// DeleteVM implements rpcpb.ManagerServiceServer. See CreateNetwork's
// doc comment for the forwarding rationale, identical here.
func (s *Server) DeleteVM(ctx context.Context, req *rpcpb.DeleteVMRequest) (*rpcpb.DeleteVMResponse, error) {
	cmd := &internalpb.Command{
		Op: &internalpb.Command_DeleteVm{DeleteVm: &internalpb.DeleteVM{Id: req.GetId()}},
	}
	vm, appErr, leaderHint := s.applyCommand(ctx, cmd, req.GetTimeoutMs())
	if leaderHint != "" && s.peers != nil {
		if fwd, ferr := s.peers.DeleteVM(ctx, s.peerManagerdAddr(leaderHint), req); ferr == nil {
			return fwd, nil
		}
	}
	return &rpcpb.DeleteVMResponse{Vm: fromInternalVM(vm), Error: appErr, LeaderHint: leaderHint}, nil
}

// SetVMFirewallPaused implements rpcpb.ManagerServiceServer. See
// CreateNetwork's own doc comment for the general
// apply-then-forward-on-leader-hint pattern every write RPC here
// follows. Deliberately submits the narrow SetVMFirewallPaused command
// rather than reading, cloning, and resubmitting via UpdateVm (as
// MigrateVM does) - see ADR-0049: a dedicated FSM-level command applies
// atomically inside the FSM's own single Apply call, with no
// read-modify-write race against a concurrent UpdateVM changing some
// other field in between.
func (s *Server) SetVMFirewallPaused(ctx context.Context, req *rpcpb.SetVMFirewallPausedRequest) (*rpcpb.SetVMFirewallPausedResponse, error) {
	cmd := &internalpb.Command{
		Op: &internalpb.Command_SetVmFirewallPaused{SetVmFirewallPaused: &internalpb.SetVMFirewallPaused{
			Id: req.GetId(), Paused: req.GetPaused(),
		}},
	}
	vm, appErr, leaderHint := s.applyCommand(ctx, cmd, req.GetTimeoutMs())
	if leaderHint != "" && s.peers != nil {
		if fwd, ferr := s.peers.SetVMFirewallPaused(ctx, s.peerManagerdAddr(leaderHint), req); ferr == nil {
			return fwd, nil
		}
	}
	return &rpcpb.SetVMFirewallPausedResponse{Vm: fromInternalVM(vm), Error: appErr, LeaderHint: leaderHint}, nil
}

// SetVMCloudflareExposure implements rpcpb.ManagerServiceServer - see
// ADR-0063. Unlike SetVMFirewallPaused, a non-empty hostname needs a
// cross-field check (network_id must already be set) that depends on
// the VM's CURRENT record, so this fetches first (the same GetVM-then-
// validate-then-submit shape MigrateVM uses) rather than submitting the
// narrow command blind. Clearing exposure (hostname == "") skips the
// fetch entirely - there's nothing to validate against for turning
// exposure off.
func (s *Server) SetVMCloudflareExposure(ctx context.Context, req *rpcpb.SetVMCloudflareExposureRequest) (*rpcpb.SetVMCloudflareExposureResponse, error) {
	if req.GetHostname() != "" {
		getResp, err := s.raft.GetVM(ctx, req.GetId())
		if err != nil {
			return &rpcpb.SetVMCloudflareExposureResponse{Error: err.Error()}, nil
		}
		if getResp.GetError() != "" {
			if s.peers != nil && getResp.GetLeaderHint() != "" {
				if fwd, ferr := s.peers.SetVMCloudflareExposure(ctx, s.peerManagerdAddr(getResp.GetLeaderHint()), req); ferr == nil {
					return fwd, nil
				}
			}
			return &rpcpb.SetVMCloudflareExposureResponse{Error: getResp.GetError(), LeaderHint: getResp.GetLeaderHint()}, nil
		}
		if !getResp.GetFound() {
			return &rpcpb.SetVMCloudflareExposureResponse{Error: fmt.Sprintf("VM %q not found", req.GetId())}, nil
		}
		if getResp.GetVm().GetNetworkId() == "" {
			return &rpcpb.SetVMCloudflareExposureResponse{Error: fmt.Sprintf(
				"VM %q has no network_id set - a flat-bridge VM's IP is never tracked in raft state, so there is no address for a Cloudflare Tunnel to proxy to. Set network_id via UpdateVM first.",
				req.GetId(),
			)}, nil
		}
	}

	cmd := &internalpb.Command{
		Op: &internalpb.Command_SetVmCloudflareExposure{SetVmCloudflareExposure: &internalpb.SetVMCloudflareExposure{
			Id: req.GetId(), Hostname: req.GetHostname(), Port: req.GetPort(),
		}},
	}
	vm, appErr, leaderHint := s.applyCommand(ctx, cmd, req.GetTimeoutMs())
	if leaderHint != "" && s.peers != nil {
		if fwd, ferr := s.peers.SetVMCloudflareExposure(ctx, s.peerManagerdAddr(leaderHint), req); ferr == nil {
			return fwd, nil
		}
	}
	return &rpcpb.SetVMCloudflareExposureResponse{Vm: fromInternalVM(vm), Error: appErr, LeaderHint: leaderHint}, nil
}

// ForcePurgeVM implements rpcpb.ManagerServiceServer. It's an escape
// hatch for a VM tombstoned by DeleteVM whose owning node will never
// come back to reconcile it away (see the RPC's own proto doc comment
// and CLAUDE.md's resource-reclaim gap) - it only succeeds against a
// VM already in VM_STATE_DELETING, so a live VM can't be force-purged
// by mistake and silently orphan resources on a node that's actually
// still up. GetVM here uses the same leader-only read every other
// lookup in this RPC does - there's no reason to special-case it the
// way ADR-0023's ValidateAPIKeyHash does, since this isn't a
// per-request auth check. Unlike CreateVM/UpdateVM/DeleteVM, this RPC's
// own preliminary read goes through RaftClient.GetVM directly (not the
// exported, forwarding-enabled GetVM RPC handler), so the forward has
// to happen right here rather than falling out of GetVM's own
// forwarding - and it forwards the entire original request, not just
// the read, since a peer needs to redo this RPC's own validation too
// (ADR-0037's follow-up, closing the gap that ADR itself named).
func (s *Server) ForcePurgeVM(ctx context.Context, req *rpcpb.ForcePurgeVMRequest) (*rpcpb.ForcePurgeVMResponse, error) {
	getResp, err := s.raft.GetVM(ctx, req.GetId())
	if err != nil {
		return &rpcpb.ForcePurgeVMResponse{Error: err.Error()}, nil
	}
	if getResp.GetError() != "" {
		if s.peers != nil && getResp.GetLeaderHint() != "" {
			if fwd, ferr := s.peers.ForcePurgeVM(ctx, s.peerManagerdAddr(getResp.GetLeaderHint()), req); ferr == nil {
				return fwd, nil
			}
		}
		return &rpcpb.ForcePurgeVMResponse{Error: getResp.GetError(), LeaderHint: getResp.GetLeaderHint()}, nil
	}
	if !getResp.GetFound() {
		return &rpcpb.ForcePurgeVMResponse{Error: fmt.Sprintf("VM %q not found", req.GetId())}, nil
	}
	if getResp.GetVm().GetDesiredState() != internalpb.VMState_VM_STATE_DELETING {
		return &rpcpb.ForcePurgeVMResponse{Error: fmt.Sprintf("VM %q is not marked for deletion - call DeleteVM first", req.GetId())}, nil
	}

	cmd := &internalpb.Command{
		Op: &internalpb.Command_PurgeVm{PurgeVm: &internalpb.PurgeVM{Id: req.GetId()}},
	}
	vm, appErr, leaderHint := s.applyCommand(ctx, cmd, req.GetTimeoutMs())
	if leaderHint != "" && s.peers != nil {
		if fwd, ferr := s.peers.ForcePurgeVM(ctx, s.peerManagerdAddr(leaderHint), req); ferr == nil {
			return fwd, nil
		}
	}
	return &rpcpb.ForcePurgeVMResponse{Vm: fromInternalVM(vm), Error: appErr, LeaderHint: leaderHint}, nil
}

// MigrateVM implements rpcpb.ManagerServiceServer. See its own proto
// doc comment for the full reasoning behind the "target_node_id must
// already be this VM's replica_node_id" requirement (ADR-0028) - in
// short, any other target would silently destroy the VM's real disk
// data (the old node's reconciler tears it down via ADR-0025's
// resource reclaim, and the new node has never seen the disk at all).
// See ForcePurgeVM's doc comment for why forwarding has to happen at
// this RPC's own call sites rather than through GetVM's forwarding
// (ADR-0037's follow-up).
func (s *Server) MigrateVM(ctx context.Context, req *rpcpb.MigrateVMRequest) (*rpcpb.MigrateVMResponse, error) {
	if req.GetTargetNodeId() == "" {
		return &rpcpb.MigrateVMResponse{Error: "target_node_id must be set"}, nil
	}

	getResp, err := s.raft.GetVM(ctx, req.GetId())
	if err != nil {
		return &rpcpb.MigrateVMResponse{Error: err.Error()}, nil
	}
	if getResp.GetError() != "" {
		if s.peers != nil && getResp.GetLeaderHint() != "" {
			if fwd, ferr := s.peers.MigrateVM(ctx, s.peerManagerdAddr(getResp.GetLeaderHint()), req); ferr == nil {
				return fwd, nil
			}
		}
		return &rpcpb.MigrateVMResponse{Error: getResp.GetError(), LeaderHint: getResp.GetLeaderHint()}, nil
	}
	if !getResp.GetFound() {
		return &rpcpb.MigrateVMResponse{Error: fmt.Sprintf("VM %q not found", req.GetId())}, nil
	}
	vm := getResp.GetVm()
	if vm.GetDesiredState() == internalpb.VMState_VM_STATE_DELETING {
		return &rpcpb.MigrateVMResponse{Error: fmt.Sprintf("VM %q is marked for deletion, cannot migrate", req.GetId())}, nil
	}
	if req.GetTargetNodeId() == vm.GetNodeId() {
		return &rpcpb.MigrateVMResponse{Error: fmt.Sprintf("VM %q is already assigned to node %q", req.GetId(), req.GetTargetNodeId())}, nil
	}
	if vm.GetReplicaNodeId() != req.GetTargetNodeId() {
		return &rpcpb.MigrateVMResponse{Error: fmt.Sprintf(
			"MigrateVM requires target_node_id (%q) to already be this VM's replica_node_id (currently %q) - a synced HAST secondary. "+
				"Set replica_node_id via UpdateVM first, confirm hastctl reports status: complete on the target, then migrate.",
			req.GetTargetNodeId(), vm.GetReplicaNodeId(),
		)}, nil
	}

	updated := proto.Clone(vm).(*internalpb.VMDefinition)
	updated.NodeId = req.GetTargetNodeId()
	updated.ReplicaNodeId = vm.GetNodeId()

	cmd := &internalpb.Command{
		Op: &internalpb.Command_UpdateVm{UpdateVm: &internalpb.UpdateVM{Vm: updated}},
	}
	result, appErr, leaderHint := s.applyCommand(ctx, cmd, req.GetTimeoutMs())
	if leaderHint != "" && s.peers != nil {
		if fwd, ferr := s.peers.MigrateVM(ctx, s.peerManagerdAddr(leaderHint), req); ferr == nil {
			return fwd, nil
		}
	}
	return &rpcpb.MigrateVMResponse{Vm: fromInternalVM(result), Error: appErr, LeaderHint: leaderHint}, nil
}

// ReportVMPhase implements rpcpb.ManagerServiceServer. See its own
// proto doc comment (ADR-0029): a peer-to-peer RPC letting a
// reconciler that owns a VM but whose own raftd isn't the current
// leader still get its phase update applied, by calling the leader
// node's managerd instead of failing locally. This node's own raftd
// must itself be the leader for this to succeed - if leadership has
// moved on again since the caller resolved this address, the normal
// error/leader_hint response tells it to re-resolve and retry once
// more, the same way any other rejected Apply does.
func (s *Server) ReportVMPhase(ctx context.Context, req *rpcpb.ReportVMPhaseRequest) (*rpcpb.ReportVMPhaseResponse, error) {
	cmd := &internalpb.Command{
		Op: &internalpb.Command_UpdateVmPhase{UpdateVmPhase: &internalpb.UpdateVMPhase{
			Id:         req.GetId(),
			Phase:      internalpb.VMPhase(req.GetPhase()),
			PhaseError: req.GetPhaseError(),
		}},
	}
	payload, err := proto.Marshal(cmd)
	if err != nil {
		return &rpcpb.ReportVMPhaseResponse{Error: err.Error()}, nil
	}
	resp, err := s.raft.Apply(ctx, payload, defaultApplyTimeout)
	if err != nil {
		return &rpcpb.ReportVMPhaseResponse{Error: err.Error()}, nil
	}
	return &rpcpb.ReportVMPhaseResponse{Error: resp.GetError(), LeaderHint: resp.GetLeaderHint()}, nil
}

// ReportVMTeardownComplete implements rpcpb.ManagerServiceServer,
// mirroring ReportVMPhase for the final PurgeVM step of teardownVM
// instead of a phase update.
func (s *Server) ReportVMTeardownComplete(ctx context.Context, req *rpcpb.ReportVMTeardownCompleteRequest) (*rpcpb.ReportVMTeardownCompleteResponse, error) {
	cmd := &internalpb.Command{
		Op: &internalpb.Command_PurgeVm{PurgeVm: &internalpb.PurgeVM{Id: req.GetId()}},
	}
	payload, err := proto.Marshal(cmd)
	if err != nil {
		return &rpcpb.ReportVMTeardownCompleteResponse{Error: err.Error()}, nil
	}
	resp, err := s.raft.Apply(ctx, payload, defaultApplyTimeout)
	if err != nil {
		return &rpcpb.ReportVMTeardownCompleteResponse{Error: err.Error()}, nil
	}
	return &rpcpb.ReportVMTeardownCompleteResponse{Error: resp.GetError(), LeaderHint: resp.GetLeaderHint()}, nil
}

// ReportJailPhase mirrors ReportVMPhase exactly, for jails.
func (s *Server) ReportJailPhase(ctx context.Context, req *rpcpb.ReportJailPhaseRequest) (*rpcpb.ReportJailPhaseResponse, error) {
	cmd := &internalpb.Command{
		Op: &internalpb.Command_UpdateJailPhase{UpdateJailPhase: &internalpb.UpdateJailPhase{
			Id:         req.GetId(),
			Phase:      internalpb.JailPhase(req.GetPhase()),
			PhaseError: req.GetPhaseError(),
		}},
	}
	payload, err := proto.Marshal(cmd)
	if err != nil {
		return &rpcpb.ReportJailPhaseResponse{Error: err.Error()}, nil
	}
	resp, err := s.raft.Apply(ctx, payload, defaultApplyTimeout)
	if err != nil {
		return &rpcpb.ReportJailPhaseResponse{Error: err.Error()}, nil
	}
	return &rpcpb.ReportJailPhaseResponse{Error: resp.GetError(), LeaderHint: resp.GetLeaderHint()}, nil
}

// ReportJailTeardownComplete mirrors ReportVMTeardownComplete exactly,
// for jails.
func (s *Server) ReportJailTeardownComplete(ctx context.Context, req *rpcpb.ReportJailTeardownCompleteRequest) (*rpcpb.ReportJailTeardownCompleteResponse, error) {
	cmd := &internalpb.Command{
		Op: &internalpb.Command_PurgeJail{PurgeJail: &internalpb.PurgeJail{Id: req.GetId()}},
	}
	payload, err := proto.Marshal(cmd)
	if err != nil {
		return &rpcpb.ReportJailTeardownCompleteResponse{Error: err.Error()}, nil
	}
	resp, err := s.raft.Apply(ctx, payload, defaultApplyTimeout)
	if err != nil {
		return &rpcpb.ReportJailTeardownCompleteResponse{Error: err.Error()}, nil
	}
	return &rpcpb.ReportJailTeardownCompleteResponse{Error: resp.GetError(), LeaderHint: resp.GetLeaderHint()}, nil
}

// GetVM implements rpcpb.ManagerServiceServer.
func (s *Server) GetVM(ctx context.Context, req *rpcpb.GetVMRequest) (*rpcpb.GetVMResponse, error) {
	resp, err := s.raft.GetVM(ctx, req.GetId())
	if err != nil {
		return &rpcpb.GetVMResponse{Error: err.Error()}, nil
	}
	if resp.GetError() != "" {
		if s.peers != nil && resp.GetLeaderHint() != "" {
			if fwd, ferr := s.peers.GetVM(ctx, s.peerManagerdAddr(resp.GetLeaderHint()), req.GetId()); ferr == nil {
				return fwd, nil
			}
		}
		return &rpcpb.GetVMResponse{Error: resp.GetError(), LeaderHint: resp.GetLeaderHint()}, nil
	}
	return &rpcpb.GetVMResponse{Vm: fromInternalVM(resp.GetVm()), Found: resp.GetFound()}, nil
}

// UploadISO implements rpcpb.ManagerServiceServer. The client's first
// message must carry metadata (name + expected hash); every message
// after that carries a chunk of the file's bytes. Chunks are piped
// directly into isostore.Save as they arrive - the whole upload is
// never buffered in memory, and Save's own hash verification runs
// concurrently with receiving the stream rather than after it.
func (s *Server) UploadISO(stream rpcpb.ManagerService_UploadISOServer) error {
	first, err := stream.Recv()
	if err != nil {
		return fmt.Errorf("manager: UploadISO: receiving metadata: %w", err)
	}
	meta := first.GetMetadata()
	if meta == nil {
		return fmt.Errorf("manager: UploadISO: first message must be metadata")
	}

	pr, pw := io.Pipe()
	type result struct {
		info *isostore.Info
		err  error
	}
	saveDone := make(chan result, 1)
	go func() {
		info, err := s.isos.Save(meta.GetName(), pr, meta.GetExpectedSha256())
		pr.CloseWithError(err)
		saveDone <- result{info, err}
	}()

	for {
		req, err := stream.Recv()
		if err == io.EOF {
			pw.Close()
			break
		}
		if err != nil {
			pw.CloseWithError(err)
			<-saveDone
			return fmt.Errorf("manager: UploadISO: receiving chunk: %w", err)
		}
		if _, err := pw.Write(req.GetChunk()); err != nil {
			break // Save's goroutine already failed; its error is reported below
		}
	}

	res := <-saveDone
	if res.err != nil {
		return stream.SendAndClose(&rpcpb.UploadISOResponse{Error: res.err.Error()})
	}
	return stream.SendAndClose(&rpcpb.UploadISOResponse{
		Name:      res.info.Name,
		SizeBytes: uint64(res.info.SizeBytes),
		Sha256:    res.info.SHA256,
	})
}

// ListISOs implements rpcpb.ManagerServiceServer.
func (s *Server) ListISOs(_ context.Context, _ *rpcpb.ListISOsRequest) (*rpcpb.ListISOsResponse, error) {
	infos, err := s.isos.List()
	if err != nil {
		return &rpcpb.ListISOsResponse{Error: err.Error()}, nil
	}
	isos := make([]*rpcpb.ISOInfo, 0, len(infos))
	for _, info := range infos {
		isos = append(isos, &rpcpb.ISOInfo{Name: info.Name, SizeBytes: uint64(info.SizeBytes), Sha256: info.SHA256})
	}
	return &rpcpb.ListISOsResponse{Isos: isos}, nil
}

// DeleteISO implements rpcpb.ManagerServiceServer.
func (s *Server) DeleteISO(_ context.Context, req *rpcpb.DeleteISORequest) (*rpcpb.DeleteISOResponse, error) {
	if err := s.isos.Delete(req.GetName()); err != nil {
		return &rpcpb.DeleteISOResponse{Error: err.Error()}, nil
	}
	return &rpcpb.DeleteISOResponse{}, nil
}

// nodeManagerdAddr resolves nodeID to that node's own managerd address,
// via the current raft server list (ADR-0040) - the same {Id, Address}
// roster Status already surfaces as KnownNodeIds, just also consulted
// for the address half here. Returns an error if nodeID isn't a known
// raft member at all.
func (s *Server) nodeManagerdAddr(ctx context.Context, nodeID string) (string, error) {
	status, err := s.raft.Status(ctx)
	if err != nil {
		return "", fmt.Errorf("querying raft status: %w", err)
	}
	for _, srv := range status.GetServers() {
		if srv.GetId() == nodeID {
			return s.peerManagerdAddr(srv.GetAddress()), nil
		}
	}
	return "", fmt.Errorf("unknown node %q", nodeID)
}

// PushISOTo implements rpcpb.ManagerServiceServer (ADR-0041) - the
// peer-to-peer half of on-demand image fetching: this node (which
// already has name) pushes it to target_node_id via a real UploadISO
// client stream, verifying against this node's own already-known hash
// rather than trusting anything the caller supplied - the source is the
// only side that can actually vouch for the file's integrity here.
// Called by a peer's reconciler (via RequestISOPush) after it's already
// confirmed via ListISONames that this node has the file a VM/jail it's
// provisioning names but doesn't have locally yet.
func (s *Server) PushISOTo(ctx context.Context, req *rpcpb.PushISOToRequest) (*rpcpb.PushISOToResponse, error) {
	if req.GetName() == "" || req.GetTargetNodeId() == "" {
		return &rpcpb.PushISOToResponse{Error: "name and target_node_id are required"}, nil
	}
	if s.peers == nil {
		return &rpcpb.PushISOToResponse{Error: "peer forwarding is not configured on this node"}, nil
	}

	infos, err := s.isos.List()
	if err != nil {
		return &rpcpb.PushISOToResponse{Error: fmt.Sprintf("listing local ISOs: %v", err)}, nil
	}
	var sha256Hash string
	found := false
	for _, info := range infos {
		if info.Name == req.GetName() {
			sha256Hash = info.SHA256
			found = true
			break
		}
	}
	if !found {
		return &rpcpb.PushISOToResponse{Error: fmt.Sprintf("%q is not present on this node", req.GetName())}, nil
	}

	path, ok, err := s.isos.Path(req.GetName())
	if err != nil {
		return &rpcpb.PushISOToResponse{Error: fmt.Sprintf("resolving local path: %v", err)}, nil
	}
	if !ok {
		return &rpcpb.PushISOToResponse{Error: fmt.Sprintf("%q is not present on this node", req.GetName())}, nil
	}
	file, err := os.Open(path)
	if err != nil {
		return &rpcpb.PushISOToResponse{Error: fmt.Sprintf("opening local file: %v", err)}, nil
	}
	defer file.Close()

	targetAddr, err := s.nodeManagerdAddr(ctx, req.GetTargetNodeId())
	if err != nil {
		return &rpcpb.PushISOToResponse{Error: err.Error()}, nil
	}

	if err := s.peers.UploadISO(ctx, targetAddr, req.GetName(), sha256Hash, file); err != nil {
		return &rpcpb.PushISOToResponse{Error: fmt.Sprintf("pushing to %q: %v", req.GetTargetNodeId(), err)}, nil
	}
	return &rpcpb.PushISOToResponse{}, nil
}

// HostStats implements rpcpb.ManagerServiceServer. Every subsystem in
// the underlying hoststats.Snapshot is gathered best-effort - a
// failure in one (recorded in Errors) never blanks out the rest.
func (s *Server) HostStats(ctx context.Context, _ *rpcpb.HostStatsRequest) (*rpcpb.HostStatsResponse, error) {
	snap := s.statsGather(ctx)

	pools := make([]*rpcpb.PoolStats, 0, len(snap.Pools))
	for _, p := range snap.Pools {
		pools = append(pools, &rpcpb.PoolStats{
			Name: p.Name, SizeBytes: p.SizeBytes, AllocBytes: p.AllocBytes,
			FreeBytes: p.FreeBytes, CapacityPct: p.CapacityPct, Health: p.Health,
		})
	}

	disks := make([]*rpcpb.DiskStats, 0, len(snap.Disks))
	for _, d := range snap.Disks {
		disks = append(disks, &rpcpb.DiskStats{
			Name: d.Name, Model: d.Model, Serial: d.Serial, Healthy: d.Healthy, Error: d.Error,
		})
	}

	net := make([]*rpcpb.NetIfaceStats, 0, len(snap.Net))
	for _, n := range snap.Net {
		net = append(net, &rpcpb.NetIfaceStats{Name: n.Name, RxBytes: n.RxBytes, TxBytes: n.TxBytes, Up: n.Up})
	}

	// The three reconcile fields stay at their zero values when
	// s.reconciler is nil - internal/health treats that as "no
	// Reconciler configured on this node," never as "reconciling and
	// failing" (see ADR-0056).
	var lastReconcileSuccessUnix, lastReconcileAttemptUnix int64
	var reconcileIntervalSeconds uint32
	if s.reconciler != nil {
		if t, ok := s.reconciler.LastReconcileSuccess(); ok {
			lastReconcileSuccessUnix = t.Unix()
		}
		if t, ok := s.reconciler.LastReconcileAttempt(); ok {
			lastReconcileAttemptUnix = t.Unix()
		}
		reconcileIntervalSeconds = uint32(s.reconciler.ReconcileInterval() / time.Second)
	}

	return &rpcpb.HostStatsResponse{
		NodeId: s.nodeID,
		Cpu: &rpcpb.CPUStats{
			Cores: int32(snap.CPU.Cores), LoadAvg_1: snap.CPU.LoadAvg1,
			LoadAvg_5: snap.CPU.LoadAvg5, LoadAvg_15: snap.CPU.LoadAvg15,
		},
		Mem:   &rpcpb.MemStats{TotalBytes: snap.Mem.TotalBytes, FreeBytes: snap.Mem.FreeBytes},
		Pools: pools,
		Disks: disks,
		Net:   net,
		Pf: &rpcpb.PFStats{
			Enabled: snap.PF.Enabled, CurrentStates: snap.PF.CurrentStates, Matches: snap.PF.Matches,
		},
		Errors: snap.Errors,
		// BhyveConfigured proves this node's managerd was started with
		// -bhyve-bootrom set, NOT that bhyve is currently usable - see
		// HostStatsResponse.bhyve_configured's own doc comment.
		BhyveConfigured: s.vnc != nil,

		LastReconcileSuccessUnix: lastReconcileSuccessUnix,
		LastReconcileAttemptUnix: lastReconcileAttemptUnix,
		ReconcileIntervalSeconds: reconcileIntervalSeconds,
	}, nil
}

// GetVMConsole implements rpcpb.ManagerServiceServer. It only ever
// answers for a VM actually running on this node - see
// GetVMConsoleResponse's doc comment (api/rpc/manager.proto) for the
// resulting v1 limitation on a true multi-node deployment.
func (s *Server) GetVMConsole(ctx context.Context, req *rpcpb.GetVMConsoleRequest) (*rpcpb.GetVMConsoleResponse, error) {
	resp, err := s.raft.GetVM(ctx, req.GetId())
	if err != nil {
		return &rpcpb.GetVMConsoleResponse{Error: err.Error()}, nil
	}
	if resp.GetError() != "" {
		return &rpcpb.GetVMConsoleResponse{Error: resp.GetError()}, nil
	}
	if !resp.GetFound() {
		return &rpcpb.GetVMConsoleResponse{Error: fmt.Sprintf("VM %q not found", req.GetId())}, nil
	}
	vm := resp.GetVm()
	if vm.GetNodeId() != s.nodeID {
		return &rpcpb.GetVMConsoleResponse{
			Error: fmt.Sprintf("VM %q is assigned to node %q; query that node's managerd directly for its console", req.GetId(), vm.GetNodeId()),
		}, nil
	}
	if s.vnc == nil {
		return &rpcpb.GetVMConsoleResponse{Error: "this node has no VNC-capable bhyve support configured"}, nil
	}
	port, ok, err := s.vnc.VNCPort(req.GetId())
	if err != nil {
		return &rpcpb.GetVMConsoleResponse{Error: err.Error()}, nil
	}
	if !ok {
		return &rpcpb.GetVMConsoleResponse{Available: false}, nil
	}
	// Host is loopback, not s.nodeID: GetVMConsole only ever answers for a
	// VM already confirmed to be on *this* node (the check above), and
	// the caller dialing it (internal/frontend's console proxy) is only
	// ever expected to be running on that same node too - see
	// GetVMConsoleResponse's doc comment. A node's own hostname isn't
	// guaranteed to resolve from itself (confirmed live: apiarium's own
	// managerd couldn't resolve "apiarium"), so loopback avoids a DNS
	// dependency this project has no other reason to require.
	return &rpcpb.GetVMConsoleResponse{Host: "127.0.0.1", Port: uint32(port), Available: true}, nil
}

// defaultSerialLogTailBytes/maxSerialLogTailBytes bound GetVMSerialLog's
// response regardless of what a caller requests - a plain synchronous
// RPC (not a stream) has no business returning an arbitrarily large
// payload, and a runaway VM's serial log has been observed growing to
// several megabytes within minutes on this project's own hardware.
const (
	defaultSerialLogTailBytes = 64 * 1024
	maxSerialLogTailBytes     = 1024 * 1024
)

// GetVMSerialLog implements rpcpb.ManagerServiceServer. Like
// GetVMConsole, it only ever answers for a VM actually running on this
// node - see GetVMSerialLogResponse's doc comment.
func (s *Server) GetVMSerialLog(ctx context.Context, req *rpcpb.GetVMSerialLogRequest) (*rpcpb.GetVMSerialLogResponse, error) {
	resp, err := s.raft.GetVM(ctx, req.GetId())
	if err != nil {
		return &rpcpb.GetVMSerialLogResponse{Error: err.Error()}, nil
	}
	if resp.GetError() != "" {
		return &rpcpb.GetVMSerialLogResponse{Error: resp.GetError()}, nil
	}
	if !resp.GetFound() {
		return &rpcpb.GetVMSerialLogResponse{Error: fmt.Sprintf("VM %q not found", req.GetId())}, nil
	}
	vm := resp.GetVm()
	if vm.GetNodeId() != s.nodeID {
		return &rpcpb.GetVMSerialLogResponse{
			Error: fmt.Sprintf("VM %q is assigned to node %q; query that node's managerd directly for its serial log", req.GetId(), vm.GetNodeId()),
		}, nil
	}
	if s.serialLog == nil {
		return &rpcpb.GetVMSerialLogResponse{Error: "this node has no bhyve support configured"}, nil
	}
	path, ok, err := s.serialLog.SerialLogPath(req.GetId())
	if err != nil {
		return &rpcpb.GetVMSerialLogResponse{Error: err.Error()}, nil
	}
	if !ok {
		return &rpcpb.GetVMSerialLogResponse{Available: false}, nil
	}

	maxBytes := int64(req.GetMaxBytes())
	if maxBytes <= 0 {
		maxBytes = defaultSerialLogTailBytes
	}
	if maxBytes > maxSerialLogTailBytes {
		maxBytes = maxSerialLogTailBytes
	}

	f, err := os.Open(path)
	if err != nil {
		return &rpcpb.GetVMSerialLogResponse{Error: err.Error()}, nil
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return &rpcpb.GetVMSerialLogResponse{Error: err.Error()}, nil
	}
	size := info.Size()
	truncated := size > maxBytes
	start := int64(0)
	if truncated {
		start = size - maxBytes
	}
	if _, err := f.Seek(start, io.SeekStart); err != nil {
		return &rpcpb.GetVMSerialLogResponse{Error: err.Error()}, nil
	}
	buf, err := io.ReadAll(f)
	if err != nil {
		return &rpcpb.GetVMSerialLogResponse{Error: err.Error()}, nil
	}
	return &rpcpb.GetVMSerialLogResponse{
		Content:   strings.ToValidUTF8(string(buf), "�"),
		Truncated: truncated,
		Available: true,
	}, nil
}

// GetNodeConfig implements rpcpb.ManagerServiceServer - reports this
// node's own local settings (see internal/nodeconfig), never routed
// through raft.
func (s *Server) GetNodeConfig(_ context.Context, _ *rpcpb.GetNodeConfigRequest) (*rpcpb.GetNodeConfigResponse, error) {
	if s.nodeConfig == nil {
		return &rpcpb.GetNodeConfigResponse{Error: "this node has no node-config store configured"}, nil
	}
	cfg, err := s.nodeConfig.Load()
	if err != nil {
		return &rpcpb.GetNodeConfigResponse{Error: err.Error()}, nil
	}
	return &rpcpb.GetNodeConfigResponse{Uplink: cfg.Uplink, NatUplink: cfg.NATUplink}, nil
}

// UpdateNodeConfig implements rpcpb.ManagerServiceServer - persists new
// local settings, replacing the file in full (matching
// nodeconfig.Manager.Save's own doc comment) rather than merging, so a
// caller intending to change only one field must send both. Takes
// effect on this node's next managerd restart, not live - see
// ADR-0049.
func (s *Server) UpdateNodeConfig(_ context.Context, req *rpcpb.UpdateNodeConfigRequest) (*rpcpb.UpdateNodeConfigResponse, error) {
	if s.nodeConfig == nil {
		return &rpcpb.UpdateNodeConfigResponse{Error: "this node has no node-config store configured"}, nil
	}
	err := s.nodeConfig.Save(nodeconfig.Config{Uplink: req.GetUplink(), NATUplink: req.GetNatUplink()})
	if err != nil {
		return &rpcpb.UpdateNodeConfigResponse{Error: err.Error()}, nil
	}
	return &rpcpb.UpdateNodeConfigResponse{}, nil
}

// SetDatasetQuota implements rpcpb.ManagerServiceServer - sets a ZFS
// quota on a dataset under this node's own configured Base scope,
// never routed through raft (physical, per-node storage). See
// ADR-0049 for why the Base dataset itself can't be targeted directly
// (zfs.Manager's own path() rejects an empty name).
func (s *Server) SetDatasetQuota(ctx context.Context, req *rpcpb.SetDatasetQuotaRequest) (*rpcpb.SetDatasetQuotaResponse, error) {
	if s.zfs == nil {
		return &rpcpb.SetDatasetQuotaResponse{Error: "this node has no ZFS support configured"}, nil
	}
	if err := s.zfs.SetProperty(ctx, req.GetDatasetName(), "quota", req.GetQuota()); err != nil {
		return &rpcpb.SetDatasetQuotaResponse{Error: err.Error()}, nil
	}
	return &rpcpb.SetDatasetQuotaResponse{}, nil
}

// applyJailCommand mirrors applyNetworkCommand, for commands whose
// result is a JailDefinition instead of a NetworkDefinition
// (CreateJail/UpdateJail/DeleteJail).
func (s *Server) applyJailCommand(ctx context.Context, cmd *internalpb.Command, timeoutMs uint32) (jail *internalpb.JailDefinition, appErr, leaderHint string) {
	timeout := defaultApplyTimeout
	if timeoutMs > 0 {
		timeout = time.Duration(timeoutMs) * time.Millisecond
	}

	payload, err := proto.Marshal(cmd)
	if err != nil {
		return nil, err.Error(), ""
	}

	resp, err := s.raft.Apply(ctx, payload, timeout)
	if err != nil {
		return nil, err.Error(), ""
	}
	if resp.GetError() != "" {
		return nil, resp.GetError(), resp.GetLeaderHint()
	}

	jail = &internalpb.JailDefinition{}
	if err := proto.Unmarshal(resp.GetResult(), jail); err != nil {
		return nil, err.Error(), ""
	}
	return jail, "", ""
}

// CreateJail implements rpcpb.ManagerServiceServer. See CreateNetwork's
// doc comment for the forwarding rationale, identical here.
func (s *Server) CreateJail(ctx context.Context, req *rpcpb.CreateJailRequest) (*rpcpb.CreateJailResponse, error) {
	cmd := &internalpb.Command{
		Op: &internalpb.Command_CreateJail{CreateJail: &internalpb.CreateJail{Jail: toInternalJail(req.GetJail())}},
	}
	jail, appErr, leaderHint := s.applyJailCommand(ctx, cmd, req.GetTimeoutMs())
	if leaderHint != "" && s.peers != nil {
		if fwd, ferr := s.peers.CreateJail(ctx, s.peerManagerdAddr(leaderHint), req); ferr == nil {
			return fwd, nil
		}
	}
	return &rpcpb.CreateJailResponse{Jail: fromInternalJail(jail), Error: appErr, LeaderHint: leaderHint}, nil
}

// UpdateJail implements rpcpb.ManagerServiceServer. See CreateNetwork's
// doc comment for the forwarding rationale, identical here.
func (s *Server) UpdateJail(ctx context.Context, req *rpcpb.UpdateJailRequest) (*rpcpb.UpdateJailResponse, error) {
	cmd := &internalpb.Command{
		Op: &internalpb.Command_UpdateJail{UpdateJail: &internalpb.UpdateJail{Jail: toInternalJail(req.GetJail())}},
	}
	jail, appErr, leaderHint := s.applyJailCommand(ctx, cmd, req.GetTimeoutMs())
	if leaderHint != "" && s.peers != nil {
		if fwd, ferr := s.peers.UpdateJail(ctx, s.peerManagerdAddr(leaderHint), req); ferr == nil {
			return fwd, nil
		}
	}
	return &rpcpb.UpdateJailResponse{Jail: fromInternalJail(jail), Error: appErr, LeaderHint: leaderHint}, nil
}

// DeleteJail implements rpcpb.ManagerServiceServer. See CreateNetwork's
// doc comment for the forwarding rationale, identical here.
func (s *Server) DeleteJail(ctx context.Context, req *rpcpb.DeleteJailRequest) (*rpcpb.DeleteJailResponse, error) {
	cmd := &internalpb.Command{
		Op: &internalpb.Command_DeleteJail{DeleteJail: &internalpb.DeleteJail{Id: req.GetId()}},
	}
	jail, appErr, leaderHint := s.applyJailCommand(ctx, cmd, req.GetTimeoutMs())
	if leaderHint != "" && s.peers != nil {
		if fwd, ferr := s.peers.DeleteJail(ctx, s.peerManagerdAddr(leaderHint), req); ferr == nil {
			return fwd, nil
		}
	}
	return &rpcpb.DeleteJailResponse{Jail: fromInternalJail(jail), Error: appErr, LeaderHint: leaderHint}, nil
}

// GetJail implements rpcpb.ManagerServiceServer.
func (s *Server) GetJail(ctx context.Context, req *rpcpb.GetJailRequest) (*rpcpb.GetJailResponse, error) {
	resp, err := s.raft.GetJail(ctx, req.GetId())
	if err != nil {
		return &rpcpb.GetJailResponse{Error: err.Error()}, nil
	}
	if resp.GetError() != "" {
		if s.peers != nil && resp.GetLeaderHint() != "" {
			if fwd, ferr := s.peers.GetJail(ctx, s.peerManagerdAddr(resp.GetLeaderHint()), req.GetId()); ferr == nil {
				return fwd, nil
			}
		}
		return &rpcpb.GetJailResponse{Error: resp.GetError(), LeaderHint: resp.GetLeaderHint()}, nil
	}
	return &rpcpb.GetJailResponse{Jail: fromInternalJail(resp.GetJail()), Found: resp.GetFound()}, nil
}

// ForcePurgeJail mirrors ForcePurgeVM exactly - see its own doc
// comment for the full reasoning, which applies identically here,
// including the forwarding rationale (ADR-0037's follow-up).
func (s *Server) ForcePurgeJail(ctx context.Context, req *rpcpb.ForcePurgeJailRequest) (*rpcpb.ForcePurgeJailResponse, error) {
	getResp, err := s.raft.GetJail(ctx, req.GetId())
	if err != nil {
		return &rpcpb.ForcePurgeJailResponse{Error: err.Error()}, nil
	}
	if getResp.GetError() != "" {
		if s.peers != nil && getResp.GetLeaderHint() != "" {
			if fwd, ferr := s.peers.ForcePurgeJail(ctx, s.peerManagerdAddr(getResp.GetLeaderHint()), req); ferr == nil {
				return fwd, nil
			}
		}
		return &rpcpb.ForcePurgeJailResponse{Error: getResp.GetError(), LeaderHint: getResp.GetLeaderHint()}, nil
	}
	if !getResp.GetFound() {
		return &rpcpb.ForcePurgeJailResponse{Error: fmt.Sprintf("jail %q not found", req.GetId())}, nil
	}
	if getResp.GetJail().GetDesiredState() != internalpb.JailState_JAIL_STATE_DELETING {
		return &rpcpb.ForcePurgeJailResponse{Error: fmt.Sprintf("jail %q is not marked for deletion - call DeleteJail first", req.GetId())}, nil
	}

	cmd := &internalpb.Command{
		Op: &internalpb.Command_PurgeJail{PurgeJail: &internalpb.PurgeJail{Id: req.GetId()}},
	}
	jail, appErr, leaderHint := s.applyJailCommand(ctx, cmd, req.GetTimeoutMs())
	if leaderHint != "" && s.peers != nil {
		if fwd, ferr := s.peers.ForcePurgeJail(ctx, s.peerManagerdAddr(leaderHint), req); ferr == nil {
			return fwd, nil
		}
	}
	return &rpcpb.ForcePurgeJailResponse{Jail: fromInternalJail(jail), Error: appErr, LeaderHint: leaderHint}, nil
}

// MigrateJail mirrors MigrateVM exactly - see its own doc comment for
// the full reasoning, which applies identically here, including the
// forwarding rationale (ADR-0037's follow-up).
func (s *Server) MigrateJail(ctx context.Context, req *rpcpb.MigrateJailRequest) (*rpcpb.MigrateJailResponse, error) {
	if req.GetTargetNodeId() == "" {
		return &rpcpb.MigrateJailResponse{Error: "target_node_id must be set"}, nil
	}

	getResp, err := s.raft.GetJail(ctx, req.GetId())
	if err != nil {
		return &rpcpb.MigrateJailResponse{Error: err.Error()}, nil
	}
	if getResp.GetError() != "" {
		if s.peers != nil && getResp.GetLeaderHint() != "" {
			if fwd, ferr := s.peers.MigrateJail(ctx, s.peerManagerdAddr(getResp.GetLeaderHint()), req); ferr == nil {
				return fwd, nil
			}
		}
		return &rpcpb.MigrateJailResponse{Error: getResp.GetError(), LeaderHint: getResp.GetLeaderHint()}, nil
	}
	if !getResp.GetFound() {
		return &rpcpb.MigrateJailResponse{Error: fmt.Sprintf("jail %q not found", req.GetId())}, nil
	}
	jail := getResp.GetJail()
	if jail.GetDesiredState() == internalpb.JailState_JAIL_STATE_DELETING {
		return &rpcpb.MigrateJailResponse{Error: fmt.Sprintf("jail %q is marked for deletion, cannot migrate", req.GetId())}, nil
	}
	if req.GetTargetNodeId() == jail.GetNodeId() {
		return &rpcpb.MigrateJailResponse{Error: fmt.Sprintf("jail %q is already assigned to node %q", req.GetId(), req.GetTargetNodeId())}, nil
	}
	if jail.GetReplicaNodeId() != req.GetTargetNodeId() {
		return &rpcpb.MigrateJailResponse{Error: fmt.Sprintf(
			"MigrateJail requires target_node_id (%q) to already be this jail's replica_node_id (currently %q) - a synced HAST secondary. "+
				"Set replica_node_id via UpdateJail first, confirm hastctl reports status: complete on the target, then migrate.",
			req.GetTargetNodeId(), jail.GetReplicaNodeId(),
		)}, nil
	}

	updated := proto.Clone(jail).(*internalpb.JailDefinition)
	updated.NodeId = req.GetTargetNodeId()
	updated.ReplicaNodeId = jail.GetNodeId()

	cmd := &internalpb.Command{
		Op: &internalpb.Command_UpdateJail{UpdateJail: &internalpb.UpdateJail{Jail: updated}},
	}
	result, appErr, leaderHint := s.applyJailCommand(ctx, cmd, req.GetTimeoutMs())
	if leaderHint != "" && s.peers != nil {
		if fwd, ferr := s.peers.MigrateJail(ctx, s.peerManagerdAddr(leaderHint), req); ferr == nil {
			return fwd, nil
		}
	}
	return &rpcpb.MigrateJailResponse{Jail: fromInternalJail(result), Error: appErr, LeaderHint: leaderHint}, nil
}

// ListJails implements rpcpb.ManagerServiceServer.
func (s *Server) ListJails(ctx context.Context, _ *rpcpb.ListJailsRequest) (*rpcpb.ListJailsResponse, error) {
	resp, err := s.raft.ListJails(ctx)
	if err != nil {
		return &rpcpb.ListJailsResponse{Error: err.Error()}, nil
	}
	if resp.GetError() != "" {
		if s.peers != nil && resp.GetLeaderHint() != "" {
			if fwd, ferr := s.peers.ListJails(ctx, s.peerManagerdAddr(resp.GetLeaderHint())); ferr == nil {
				return fwd, nil
			}
		}
		return &rpcpb.ListJailsResponse{Error: resp.GetError(), LeaderHint: resp.GetLeaderHint()}, nil
	}
	jails := make([]*rpcpb.JailDefinition, 0, len(resp.GetJails()))
	for _, j := range resp.GetJails() {
		jails = append(jails, fromInternalJail(j))
	}
	return &rpcpb.ListJailsResponse{Jails: jails}, nil
}

// ListVMs implements rpcpb.ManagerServiceServer.
func (s *Server) ListVMs(ctx context.Context, _ *rpcpb.ListVMsRequest) (*rpcpb.ListVMsResponse, error) {
	resp, err := s.raft.ListVMs(ctx)
	if err != nil {
		return &rpcpb.ListVMsResponse{Error: err.Error()}, nil
	}
	if resp.GetError() != "" {
		if s.peers != nil && resp.GetLeaderHint() != "" {
			if fwd, ferr := s.peers.ListVMs(ctx, s.peerManagerdAddr(resp.GetLeaderHint())); ferr == nil {
				return fwd, nil
			}
		}
		return &rpcpb.ListVMsResponse{Error: resp.GetError(), LeaderHint: resp.GetLeaderHint()}, nil
	}

	vms := make([]*rpcpb.VMDefinition, 0, len(resp.GetVms()))
	for _, vm := range resp.GetVms() {
		vms = append(vms, fromInternalVM(vm))
	}
	return &rpcpb.ListVMsResponse{Vms: vms}, nil
}

// reachabilityCheckTimeout bounds each individual peer reachability
// check SimulateNodeFailure performs - short enough that one
// unreachable peer doesn't make the whole simulation feel hung, long
// enough not to false-negative a merely slow-but-alive peer.
const reachabilityCheckTimeout = 3 * time.Second

// SimulateNodeFailure implements rpcpb.ManagerServiceServer - see
// ADR-0052 (the Dependency Graph Simulator's v1 slice). It combines
// three separate sequential reads (VM list, jail list, raft status)
// plus live per-remaining-voter reachability checks - this is NOT an
// atomic snapshot; a concurrent cluster change, or a peer becoming
// reachable/unreachable between checks, could be reflected in one part
// of the response and not another. Like ListVMs/ListJails, this only
// succeeds against the current leader; the ENTIRE original request
// (not just the failed sub-call) is forwarded on a leader-hint
// rejection so the report never mixes this node's own raft view with a
// different node's VM/jail list.
func (s *Server) SimulateNodeFailure(ctx context.Context, req *rpcpb.SimulateNodeFailureRequest) (*rpcpb.SimulateNodeFailureResponse, error) {
	targetID := req.GetNodeId()

	vmsResp, err := s.raft.ListVMs(ctx)
	if err != nil {
		return &rpcpb.SimulateNodeFailureResponse{Error: err.Error()}, nil
	}
	if vmsResp.GetError() != "" {
		if s.peers != nil && vmsResp.GetLeaderHint() != "" {
			if fwd, ferr := s.peers.SimulateNodeFailure(ctx, s.peerManagerdAddr(vmsResp.GetLeaderHint()), req); ferr == nil {
				return fwd, nil
			}
		}
		return &rpcpb.SimulateNodeFailureResponse{Error: vmsResp.GetError(), LeaderHint: vmsResp.GetLeaderHint()}, nil
	}

	jailsResp, err := s.raft.ListJails(ctx)
	if err != nil {
		return &rpcpb.SimulateNodeFailureResponse{Error: err.Error()}, nil
	}
	if jailsResp.GetError() != "" {
		if s.peers != nil && jailsResp.GetLeaderHint() != "" {
			if fwd, ferr := s.peers.SimulateNodeFailure(ctx, s.peerManagerdAddr(jailsResp.GetLeaderHint()), req); ferr == nil {
				return fwd, nil
			}
		}
		return &rpcpb.SimulateNodeFailureResponse{Error: jailsResp.GetError(), LeaderHint: jailsResp.GetLeaderHint()}, nil
	}

	raftStatus, err := s.raft.Status(ctx)
	if err != nil {
		return &rpcpb.SimulateNodeFailureResponse{Error: err.Error()}, nil
	}

	resources := make([]cluster.OwnedResourcePlacement, 0, len(vmsResp.GetVms())+len(jailsResp.GetJails()))
	for _, vm := range vmsResp.GetVms() {
		resources = append(resources, cluster.OwnedResourcePlacement{
			ID: vm.GetId(), Name: vm.GetName(), Kind: cluster.ResourceKindVM,
			NodeID: vm.GetNodeId(), ReplicaNodeID: vm.GetReplicaNodeId(),
		})
	}
	for _, jail := range jailsResp.GetJails() {
		resources = append(resources, cluster.OwnedResourcePlacement{
			ID: jail.GetId(), Name: jail.GetName(), Kind: cluster.ResourceKindJail,
			NodeID: jail.GetNodeId(), ReplicaNodeID: jail.GetReplicaNodeId(),
		})
	}

	// localNodeID is trivially reachable - this call is answering right
	// now. Every OTHER voter gets a real reachability check via the
	// same HostStats mechanism ADR-0036's cluster overview already
	// uses; if this node has no peer forwarding configured at all
	// (s.peers == nil), the whole picture is unverifiable, not
	// "assumed fine" - every other voter's reachability is Unknown.
	localNodeID := raftStatus.GetNodeId()
	servers := make([]cluster.ServerSuffrage, 0, len(raftStatus.GetServers()))
	for _, srv := range raftStatus.GetServers() {
		reachability := cluster.ReachabilityUnknown
		switch {
		case srv.GetId() == localNodeID:
			reachability = cluster.ReachabilityReachable
		case srv.GetId() == targetID:
			// The simulated target's own reachability is meaningless -
			// ComputeQuorumImpact never reads it.
		case s.peers != nil:
			checkCtx, cancel := context.WithTimeout(ctx, reachabilityCheckTimeout)
			_, herr := s.peers.HostStats(checkCtx, s.peerManagerdAddr(srv.GetAddress()))
			cancel()
			if herr == nil {
				reachability = cluster.ReachabilityReachable
			} else {
				reachability = cluster.ReachabilityUnreachable
			}
		}
		servers = append(servers, cluster.ServerSuffrage{ID: srv.GetId(), Suffrage: srv.GetSuffrage(), Reachability: reachability})
	}

	if !cluster.IsKnownTarget(servers, resources, targetID) {
		return &rpcpb.SimulateNodeFailureResponse{
			Error: fmt.Sprintf("node_id %q is not recognized: it does not appear in the raft configuration or as an owner/replica placement for any VM or jail", targetID),
		}, nil
	}

	report := cluster.SimulateNodeFailure(servers, resources, targetID)
	requirements := make([]cluster.ImageRequirement, 0, len(vmsResp.GetVms())*2)
	for _, vm := range vmsResp.GetVms() {
		if vm.GetIsoName() != "" {
			requirements = append(requirements, cluster.ImageRequirement{
				ResourceID: vm.GetId(), ResourceName: vm.GetName(), ImageName: vm.GetIsoName(), Role: cluster.ImageRoleISO,
			})
		}
		if vm.GetBaseImageName() != "" {
			requirements = append(requirements, cluster.ImageRequirement{
				ResourceID: vm.GetId(), ResourceName: vm.GetName(), ImageName: vm.GetBaseImageName(), Role: cluster.ImageRoleBaseImage,
			})
		}
	}
	report.ImageAvailability = cluster.ComputeImageAvailability(requirements, s.imageInventoryObservations(ctx, raftStatus.GetServers(), targetID, localNodeID), targetID)
	return &rpcpb.SimulateNodeFailureResponse{
		Quorum:                 toRPCQuorumImpact(report.Quorum),
		OwnedResources:         toRPCOwnedResourceImpacts(report.OwnedResources),
		ReplicaBackedResources: toRPCReplicaBackedImpacts(report.ReplicaBackedResources),
		ImageAvailability:      toRPCImageAvailability(report.ImageAvailability),
	}, nil
}

// imageInventoryObservations directly queries every remaining raft member's
// node-local image store. A failed or impossible query is recorded as unknown,
// never as an empty inventory.
func (s *Server) imageInventoryObservations(ctx context.Context, raftServers []*internalpb.ServerInfo, targetID, localNodeID string) []cluster.ImageInventoryObservation {
	observations := make([]cluster.ImageInventoryObservation, 0, len(raftServers))
	for _, server := range raftServers {
		if server.GetId() == targetID {
			continue
		}
		observation := cluster.ImageInventoryObservation{NodeID: server.GetId()}
		if server.GetId() == localNodeID {
			if s.isos != nil {
				infos, err := s.isos.List()
				if err == nil {
					observation.Observed = true
					for _, info := range infos {
						observation.Names = append(observation.Names, info.Name)
					}
				}
			}
		} else if s.peers != nil {
			checkCtx, cancel := context.WithTimeout(ctx, reachabilityCheckTimeout)
			resp, err := s.peers.ListISOs(checkCtx, s.peerManagerdAddr(server.GetAddress()))
			cancel()
			if err == nil && resp.GetError() == "" {
				observation.Observed = true
				for _, info := range resp.GetIsos() {
					observation.Names = append(observation.Names, info.GetName())
				}
			}
		}
		observations = append(observations, observation)
	}
	return observations
}

// SimulateNetworkFailure reports declared VM dependencies on one managed
// network. The network and VM lists are sequential leader-only reads, not an
// atomic snapshot. A leader hint forwards the entire request so one report
// never combines different nodes' FSM views.
func (s *Server) SimulateNetworkFailure(ctx context.Context, req *rpcpb.SimulateNetworkFailureRequest) (*rpcpb.SimulateNetworkFailureResponse, error) {
	networksResp, err := s.raft.ListNetworks(ctx)
	if err != nil {
		return &rpcpb.SimulateNetworkFailureResponse{Error: err.Error()}, nil
	}
	if networksResp.GetError() != "" {
		if s.peers != nil && networksResp.GetLeaderHint() != "" {
			if fwd, ferr := s.peers.SimulateNetworkFailure(ctx, s.peerManagerdAddr(networksResp.GetLeaderHint()), req); ferr == nil {
				return fwd, nil
			}
		}
		return &rpcpb.SimulateNetworkFailureResponse{Error: networksResp.GetError(), LeaderHint: networksResp.GetLeaderHint()}, nil
	}

	var target *internalpb.NetworkDefinition
	for _, network := range networksResp.GetNetworks() {
		if network.GetId() == req.GetNetworkId() {
			target = network
			break
		}
	}
	if target == nil {
		return &rpcpb.SimulateNetworkFailureResponse{Error: fmt.Sprintf("network_id %q is not recognized", req.GetNetworkId())}, nil
	}

	vmsResp, err := s.raft.ListVMs(ctx)
	if err != nil {
		return &rpcpb.SimulateNetworkFailureResponse{Error: err.Error()}, nil
	}
	if vmsResp.GetError() != "" {
		if s.peers != nil && vmsResp.GetLeaderHint() != "" {
			if fwd, ferr := s.peers.SimulateNetworkFailure(ctx, s.peerManagerdAddr(vmsResp.GetLeaderHint()), req); ferr == nil {
				return fwd, nil
			}
		}
		return &rpcpb.SimulateNetworkFailureResponse{Error: vmsResp.GetError(), LeaderHint: vmsResp.GetLeaderHint()}, nil
	}

	placements := make([]cluster.NetworkAttachedResourcePlacement, 0, len(vmsResp.GetVms()))
	for _, vm := range vmsResp.GetVms() {
		placements = append(placements, cluster.NetworkAttachedResourcePlacement{
			ID: vm.GetId(), Name: vm.GetName(), NodeID: vm.GetNodeId(), NetworkID: vm.GetNetworkId(),
		})
	}
	report := cluster.SimulateNetworkFailure(cluster.ManagedNetworkPlacement{
		ID: target.GetId(), Name: target.GetName(), VLANID: target.GetVlanId(), Subnet: target.GetSubnet(),
		BridgeName: target.GetBridgeName(), ExternalGateway: target.GetExternalGateway(),
	}, placements)
	impacts := make([]*rpcpb.NetworkFailureImpact, 0, len(report.AffectedResources))
	for _, impact := range report.AffectedResources {
		impacts = append(impacts, &rpcpb.NetworkFailureImpact{Id: impact.ID, Name: impact.Name, NodeId: impact.NodeID, Explanation: impact.Explanation})
	}
	return &rpcpb.SimulateNetworkFailureResponse{
		Network: fromInternalNetwork(target), AffectedResources: impacts, Note: report.Note,
	}, nil
}

func toRPCQuorumImpact(q cluster.QuorumImpact) *rpcpb.QuorumImpact {
	return &rpcpb.QuorumImpact{
		TargetIsVoter:            q.TargetIsVoter,
		TotalVoters:              q.TotalVoters,
		RemainingVoters:          q.RemainingVoters,
		RemainingReachableVoters: q.RemainingReachable,
		RemainingUnknownVoters:   q.RemainingUnknown,
		QuorumSize:               q.QuorumSize,
		Survives:                 q.Survives,
		Note:                     q.Note,
	}
}

func toRPCResourceKind(k cluster.ResourceKind) rpcpb.ResourceKind {
	if k == cluster.ResourceKindJail {
		return rpcpb.ResourceKind_RESOURCE_KIND_JAIL
	}
	return rpcpb.ResourceKind_RESOURCE_KIND_VM
}

func toRPCVerdict(v cluster.RecoveryVerdict) rpcpb.RecoveryVerdict {
	if v == cluster.RecoveryVerdictUnverifiedReplica {
		return rpcpb.RecoveryVerdict_RECOVERY_VERDICT_UNVERIFIED_REPLICA
	}
	return rpcpb.RecoveryVerdict_RECOVERY_VERDICT_UNPROTECTED
}

func toRPCOwnedResourceImpacts(impacts []cluster.OwnedResourceImpact) []*rpcpb.OwnedResourceImpact {
	out := make([]*rpcpb.OwnedResourceImpact, 0, len(impacts))
	for _, i := range impacts {
		out = append(out, &rpcpb.OwnedResourceImpact{
			Id: i.ID, Name: i.Name, Kind: toRPCResourceKind(i.Kind),
			ReplicaNodeId: i.ReplicaNodeID, Verdict: toRPCVerdict(i.Verdict), Explanation: i.Explanation,
		})
	}
	return out
}

func toRPCReplicaBackedImpacts(impacts []cluster.ReplicaBackedImpact) []*rpcpb.ReplicaBackedImpact {
	out := make([]*rpcpb.ReplicaBackedImpact, 0, len(impacts))
	for _, i := range impacts {
		out = append(out, &rpcpb.ReplicaBackedImpact{
			Id: i.ID, Name: i.Name, Kind: toRPCResourceKind(i.Kind),
			OwnerNodeId: i.OwnerNodeID, Explanation: i.Explanation,
		})
	}
	return out
}

func toRPCImageAvailability(impacts []cluster.ImageAvailabilityImpact) []*rpcpb.ImageAvailabilityImpact {
	out := make([]*rpcpb.ImageAvailabilityImpact, 0, len(impacts))
	for _, impact := range impacts {
		role := rpcpb.ImageRole_IMAGE_ROLE_ISO
		if impact.Role == cluster.ImageRoleBaseImage {
			role = rpcpb.ImageRole_IMAGE_ROLE_BASE_IMAGE
		}
		verdict := rpcpb.ImageAvailabilityVerdict_IMAGE_AVAILABILITY_VERDICT_UNKNOWN
		switch impact.Verdict {
		case cluster.ImageAvailabilityAvailable:
			verdict = rpcpb.ImageAvailabilityVerdict_IMAGE_AVAILABILITY_VERDICT_AVAILABLE
		case cluster.ImageAvailabilityUnavailable:
			verdict = rpcpb.ImageAvailabilityVerdict_IMAGE_AVAILABILITY_VERDICT_UNAVAILABLE
		}
		out = append(out, &rpcpb.ImageAvailabilityImpact{
			ResourceId: impact.ResourceID, ResourceName: impact.ResourceName,
			ImageName: impact.ImageName, Role: role, Verdict: verdict,
			SourceNodes: impact.SourceNodes, UnknownNodes: impact.UnknownNodes,
			Explanation: impact.Explanation,
		})
	}
	return out
}
