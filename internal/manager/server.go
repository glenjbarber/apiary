package manager

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	internalpb "github.com/glenjbarber/apiary/api/internalpb"
	rpcpb "github.com/glenjbarber/apiary/api/rpc"
	"github.com/glenjbarber/apiary/internal/assumptionregister"
	"github.com/glenjbarber/apiary/internal/assumptions"
	"github.com/glenjbarber/apiary/internal/cluster"
	"github.com/glenjbarber/apiary/internal/frontendconfig"
	"github.com/glenjbarber/apiary/internal/guardrail"
	"github.com/glenjbarber/apiary/internal/hast"
	"github.com/glenjbarber/apiary/internal/health"
	"github.com/glenjbarber/apiary/internal/hoststats"
	"github.com/glenjbarber/apiary/internal/isostore"
	"github.com/glenjbarber/apiary/internal/netif"
	"github.com/glenjbarber/apiary/internal/nodeconfig"
	"github.com/glenjbarber/apiary/internal/origincert"
	"github.com/glenjbarber/apiary/internal/raftdconfig"
	"github.com/glenjbarber/apiary/internal/restshimdconfig"
)

// pamAuthenticator abstracts a username/password check, defined
// locally rather than importing internal/pam's own type of the same
// shape - importing internal/pam here would drag its cgo requirement
// (github.com/msteinert/pam/v2) into every consumer of this package,
// including cmd/frontend (which imports internal/manager for
// manager.Role/NewPeerReporter and must NOT require cgo - ADR-0087).
// cgo is a per-package build requirement, not a per-symbol one, so
// only cmd/managerd - which constructs the real pam.PAMAuthenticator
// and passes it in via SetPAMAuthenticator - ever needs to import
// internal/pam at all.
type pamAuthenticator interface {
	Authenticate(username, password string) (bool, error)
}

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

// hastStatusReader is deliberately the read-only slice needed for node-local
// HAST freshness evidence. It does not expose HAST role/config mutation.
type hastStatusReader interface {
	Status(ctx context.Context, name string) (*hast.Status, error)
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
	GetVMConsole(ctx context.Context, addr, id string) (*rpcpb.GetVMConsoleResponse, error)
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
	SetNetworkName(ctx context.Context, addr string, req *rpcpb.SetNetworkNameRequest) (*rpcpb.SetNetworkNameResponse, error)
	CreateAPIKey(ctx context.Context, addr string, req *rpcpb.CreateAPIKeyRequest) (*rpcpb.CreateAPIKeyResponse, error)
	RevokeAPIKey(ctx context.Context, addr string, req *rpcpb.RevokeAPIKeyRequest) (*rpcpb.RevokeAPIKeyResponse, error)
	ListAPIKeys(ctx context.Context, addr string) (*rpcpb.ListAPIKeysResponse, error)

	ForcePurgeVM(ctx context.Context, addr string, req *rpcpb.ForcePurgeVMRequest) (*rpcpb.ForcePurgeVMResponse, error)
	MigrateVM(ctx context.Context, addr string, req *rpcpb.MigrateVMRequest) (*rpcpb.MigrateVMResponse, error)
	SetVMFirewallPaused(ctx context.Context, addr string, req *rpcpb.SetVMFirewallPausedRequest) (*rpcpb.SetVMFirewallPausedResponse, error)
	SetVMFirewallRules(ctx context.Context, addr string, req *rpcpb.SetVMFirewallRulesRequest) (*rpcpb.SetVMFirewallRulesResponse, error)
	SetVMCloudflareExposure(ctx context.Context, addr string, req *rpcpb.SetVMCloudflareExposureRequest) (*rpcpb.SetVMCloudflareExposureResponse, error)
	SetVMDesiredState(ctx context.Context, addr string, req *rpcpb.SetVMDesiredStateRequest) (*rpcpb.SetVMDesiredStateResponse, error)
	ForcePurgeJail(ctx context.Context, addr string, req *rpcpb.ForcePurgeJailRequest) (*rpcpb.ForcePurgeJailResponse, error)
	MigrateJail(ctx context.Context, addr string, req *rpcpb.MigrateJailRequest) (*rpcpb.MigrateJailResponse, error)
	SetJailDesiredState(ctx context.Context, addr string, req *rpcpb.SetJailDesiredStateRequest) (*rpcpb.SetJailDesiredStateResponse, error)
	SetJailHostname(ctx context.Context, addr string, req *rpcpb.SetJailHostnameRequest) (*rpcpb.SetJailHostnameResponse, error)

	// UploadISO streams a local file to addr's own UploadISO RPC - used
	// by PushISOTo (the source node's side of on-demand image fetching,
	// ADR-0041) to push a file this node already has to whichever peer
	// asked for it.
	UploadISO(ctx context.Context, addr, name, expectedSHA256 string, r io.Reader) error

	// PushJailTemplate streams a local `zfs send` to addr's own
	// ReceiveJailTemplate RPC - the jail base-template equivalent of
	// UploadISO above, used by PushJailTemplateTo (ADR-0089).
	PushJailTemplate(ctx context.Context, addr, name string, r io.Reader) error

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

	// Status reaches a specific peer's own Status RPC, answering only
	// for that peer and never forwarded to a leader. ClusterHealth
	// (ADR-0122) needs it to learn each remote Comb's own self-reported
	// raft reachability and applied/last-log index, which HostStats
	// deliberately does not carry.
	Status(ctx context.Context, addr string) (*rpcpb.StatusResponse, error)

	GetLocalHASTResourceStatus(ctx context.Context, addr, resourceName string) (*rpcpb.GetLocalHASTResourceStatusResponse, error)
	GetLocalNetworkBridgeStatus(ctx context.Context, addr, networkID string) (*rpcpb.GetLocalNetworkBridgeStatusResponse, error)
	ListAssumptionResults(ctx context.Context, addr string, req *rpcpb.ListAssumptionResultsRequest) (*rpcpb.ListAssumptionResultsResponse, error)
	PurgeStaleAssumptionResults(ctx context.Context, addr string, req *rpcpb.PurgeStaleAssumptionResultsRequest) (*rpcpb.PurgeStaleAssumptionResultsResponse, error)

	// RequestJoinColony/ApproveJoinRequest/RejectJoinRequest forward on a
	// leader-hint rejection, mirroring every other Apply-backed write
	// above (ADR-0083). RequestJoinColony forwards the joining Comb's
	// original, unauthenticated request through to whichever peer is
	// actually leader - this forwarding call itself uses this node's own
	// configured -peer-api-key, not the (nonexistent) credential of the
	// original unauthenticated caller.
	RequestJoinColony(ctx context.Context, addr string, req *rpcpb.RequestJoinColonyRequest) (*rpcpb.RequestJoinColonyResponse, error)
	ApproveJoinRequest(ctx context.Context, addr string, req *rpcpb.ApproveJoinRequestRequest) (*rpcpb.ApproveJoinRequestResponse, error)
	RejectJoinRequest(ctx context.Context, addr string, req *rpcpb.RejectJoinRequestRequest) (*rpcpb.RejectJoinRequestResponse, error)

	// CancelJoinRequest/PurgeJoinRequest mirror RequestJoinColony/
	// ApproveJoinRequest/RejectJoinRequest above exactly.
	CancelJoinRequest(ctx context.Context, addr string, req *rpcpb.CancelJoinRequestRequest) (*rpcpb.CancelJoinRequestResponse, error)
	PurgeJoinRequest(ctx context.Context, addr string, req *rpcpb.PurgeJoinRequestRequest) (*rpcpb.PurgeJoinRequestResponse, error)

	// GetJoinRequestStatus (ADR-0092) forwards a status poll to addr -
	// used when RequestJoinColony's own target_address sent the request
	// itself to a different Colony member than the one being asked.
	GetJoinRequestStatus(ctx context.Context, addr, requestID string) (*rpcpb.GetJoinRequestStatusResponse, error)

	// RequestJoinColonyUnauthenticated/GetJoinRequestStatusUnauthenticated/
	// CancelJoinRequestUnauthenticated (ADR-0096) are used only for
	// RequestJoinColony/GetJoinRequestStatus/CancelJoinRequest's own
	// target_address (ADR-0092) - a caller-supplied address from an RPC
	// that is deliberately never authenticated (see internal/manager/
	// auth.go), so it must never be dialed with this node's own shared
	// -peer-api-key attached. Distinct from RequestJoinColony/
	// GetJoinRequestStatus/CancelJoinRequest above, which stay
	// authenticated for their own (trusted, internally-derived leader-
	// hint) use.
	RequestJoinColonyUnauthenticated(ctx context.Context, addr string, req *rpcpb.RequestJoinColonyRequest) (*rpcpb.RequestJoinColonyResponse, error)
	GetJoinRequestStatusUnauthenticated(ctx context.Context, addr, requestID string) (*rpcpb.GetJoinRequestStatusResponse, error)
	CancelJoinRequestUnauthenticated(ctx context.Context, addr string, req *rpcpb.CancelJoinRequestRequest) (*rpcpb.CancelJoinRequestResponse, error)

	// PreflightApproveJoinRequest forwards on a leader-hint rejection,
	// mirroring ApproveJoinRequest above exactly (ADR-0103).
	PreflightApproveJoinRequest(ctx context.Context, addr string, req *rpcpb.PreflightApproveJoinRequestRequest) (*rpcpb.PreflightApproveJoinRequestResponse, error)

	// UpdateVoterAddress forwards on a leader-hint rejection, mirroring
	// ApproveJoinRequest above exactly (ADR-0106).
	UpdateVoterAddress(ctx context.Context, addr string, req *rpcpb.UpdateVoterAddressRequest) (*rpcpb.UpdateVoterAddressResponse, error)

	// ReserveRestartLease/ConfirmRestartCompleted forward on a leader-hint
	// rejection like every other Apply-backed write above, but
	// authenticate with this node's own restart-guardrail token instead
	// of -peer-api-key - see ADR-0103 and internal/manager/auth.go's own
	// restartGuardrailTokenValid doc comment for why these two RPCs
	// deliberately sit outside the normal Viewer/Admin role hierarchy
	// every other forwarded RPC here uses.
	ReserveRestartLease(ctx context.Context, addr string, req *rpcpb.ReserveRestartLeaseRequest) (*rpcpb.ReserveRestartLeaseResponse, error)
	ConfirmRestartCompleted(ctx context.Context, addr string, req *rpcpb.ConfirmRestartCompletedRequest) (*rpcpb.ConfirmRestartCompletedResponse, error)
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

	// CloudflareConfigured backs HostStatsResponse.cloudflare_configured
	// (ADR-0063) - reusing this already-nil-able dependency rather than
	// adding a new NewServer parameter just for one more boolean signal.
	CloudflareConfigured() bool

	// NetworkArtifactStatus backs GetNetworkTeardownStatus (ADR-0081) -
	// this node's own local artifact-cleanup record for one network id.
	NetworkArtifactStatus(networkID string) (present bool, bridge string, ownBridge, ownVLAN, outboundNAT bool, err error)
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
	PurgeStale(staleAfter time.Duration, now time.Time) (int, error)
}

type assumptionRegisterStore interface {
	List() ([]assumptionregister.Claim, error)
	Save(assumptionregister.Claim, time.Time) error
	Delete(string) error
}

// nodeServiceController keeps the Machine page's host-service controls
// narrow and testable. It deliberately exposes only Apiary's fixed rc.d
// service names, never arbitrary commands or service names supplied by a
// browser request.
type nodeServiceController interface {
	List(context.Context) ([]*rpcpb.NodeService, error)
	Restart(context.Context, string) error
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

	// hastStatus is this node's local hastctl reader. Replica freshness is
	// derived by independently querying each configured replica endpoint.
	hastStatus hastStatusReader

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

	// frontendConfig/restshimdConfig/raftdConfig (ADR-0102) let this
	// managerd read/write the three sibling daemons' own config files,
	// co-located on this same host - Get*Config/Update*Config report an
	// error rather than panicking when nil, the same opt-in pattern as
	// nodeConfig above. Wired via SetFrontendConfig/SetRestshimdConfig/
	// SetRaftdConfig after construction (not a NewServer parameter) to
	// avoid touching NewServer's already-long positional signature -
	// the same reasoning SetAssumptionRegister/SetOriginCAIssuer below
	// already established for this exact situation.
	frontendConfig  frontendConfigStore
	restshimdConfig restshimdConfigStore
	raftdConfig     raftdConfigStore

	// raftdConversionConfig (ADR-0105) is a separate, write-capable view
	// of the same underlying raftd.json GetRaftdConfig reads through
	// raftdConfig above - kept as its own field/interface rather than
	// adding Save to raftdConfigStore, since raftdConfigStore's own doc
	// comment is deliberate that no general write path exists there
	// (the 2026-09-15 audit finding that a plain per-host save form is
	// unsafe for internal_token/Raft TLS material). ConvertStandaloneToJoiner
	// is the only caller, and it only ever rewrites raft_bind/await_join.
	raftdConversionConfig raftdConversionConfigStore

	// raftdConversion (ADR-0105) is the narrowly-scoped stop/reset/start
	// controller ConvertStandaloneToJoiner uses - see
	// raftdConversionController's own doc comment (raftdservice.go) for
	// why this is separate from services/nodeServiceController above
	// (raftd is deliberately excluded from that controller's restart
	// allowlist, being consensus-critical).
	raftdConversion raftdConversionController

	// listNetworkInterfaces reports the current host-local interface
	// inventory for the Machine Configuration page. It is separate from
	// nodeConfig because discovery is live and is never persisted.
	listNetworkInterfaces func() ([]netif.Interface, error)

	// assumptions is nil on a node that never got an assumptions store
	// wired up - ListAssumptionResults reports an error rather than
	// panicking. Physical, per-node data like isos/nodeConfig above,
	// never routed through raft. See ADR-0055/internal/assumptions.
	assumptions assumptionStore
	register    assumptionRegisterStore

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

	// services is this Hive's narrowly-scoped rc.d service controller. It is
	// intentionally local rather than a peer-forwarded or raft operation.
	services nodeServiceController
	originCA origincert.Issuer

	// authPAM is nil on a node with no -pam-service configured -
	// AuthenticatePassword reports an explicit "not configured" error
	// rather than panicking, and Status's pam_configured field reflects
	// this. See ADR-0087: PAM authentication lives here now, not in
	// cmd/frontend, so frontend itself needs no cgo/native build.
	authPAM pamAuthenticator

	// pamLockouts enforces AuthenticatePassword's own lockout (ADR-0096)
	// - always initialized (never nil), since it's harmless to consult
	// even on a node with authPAM unset (AuthenticatePassword returns
	// before ever touching it in that case). See pamLockoutTracker's own
	// doc comment for why this can't simply rely on
	// internal/frontend's identical tracker.
	pamLockouts *pamLockoutTracker

	// knownPeerAddresses (ADR-0097), when non-empty, is the sole
	// allowlist RequestJoinColony/GetJoinRequestStatus/CancelJoinRequest
	// check target_address against before dialing it - see
	// checkTargetAddressAllowed's own doc comment (joincolony.go). Nil
	// (the default, no -known-peer-addresses configured) preserves
	// ADR-0092's original behavior: any caller-supplied target_address
	// is dialed, the accepted-as-of-ADR-0096 residual risk.
	knownPeerAddresses map[string]bool

	// reachabilityCheck is swappable in tests (real TCP dials aren't
	// deterministic/fast enough for unit tests) - production always uses
	// the package-level dialReachable. See ApproveJoinRequest's own
	// pre-AddVoter check (ADR-0097).
	reachabilityCheck func(ctx context.Context, addr string) error

	// restartGuardrailToken gates ReserveRestartLease/ConfirmRestartCompleted
	// (ADR-0103) - loaded once from a root-owned local file at managerd
	// startup (SetRestartGuardrailToken), never exposed through any RPC.
	// Empty (the default, no file provisioned) means these two RPCs can
	// never succeed for a forwarded call - see restartGuardrailTokenValid's
	// own doc comment for why this fails closed rather than open.
	restartGuardrailToken string

	// restartConfirm persists the pending-restart-confirmation record
	// RestartNodeService writes before restarting apiary_managerd, read
	// back by cmd/managerd's own startup path to self-confirm (ADR-0103) -
	// nil on a node with no restart-confirmation state directory wired up.
	restartConfirm *RestartConfirmStore
}

// SetPAMAuthenticator wires PAM login support after construction (ADR-
// 0087), the same setter-for-optional-dependency pattern as
// SetOriginCAIssuer above - production wires a real pam.PAMAuthenticator
// here from cmd/managerd's own -pam-service flag; tests may supply a
// fake implementing the same small interface.
func (s *Server) SetPAMAuthenticator(auth pamAuthenticator) { s.authPAM = auth }

// SetKnownPeerAddresses configures the target_address allowlist
// (ADR-0097) from cmd/managerd's own -known-peer-addresses flag - a
// nil/empty slice clears it back to ADR-0092's original
// accept-any-target_address behavior.
func (s *Server) SetKnownPeerAddresses(addrs []string) {
	if len(addrs) == 0 {
		s.knownPeerAddresses = nil
		return
	}
	m := make(map[string]bool, len(addrs))
	for _, a := range addrs {
		m[a] = true
	}
	s.knownPeerAddresses = m
}

// quotaSetter is the subset of *zfs.Manager SetDatasetQuota and the
// orphaned-HAST-resource RPCs need, defined locally so it can be faked
// in tests without a real zfs(8) binary - the same reasoning
// isoManager/VLANStatus already follow. ListDatasets/DatasetExists/
// DestroyDataset were added for ListOrphanedHASTResources/
// CleanupOrphanedHASTResource (see ADR-0026's own "not yet addressed"
// note); Send/Receive/ListTemplateNames were added for the jail
// base-template peer-fetch RPCs (ADR-0089) - all kept on this same
// interface/field rather than a new one since it's always backed by
// the identical *zfs.Manager instance in cmd/managerd, despite the
// name now covering more than quotas.
type quotaSetter interface {
	SetProperty(ctx context.Context, name, prop, value string) error
	ListDatasets(ctx context.Context) ([]string, error)
	DatasetExists(ctx context.Context, name string) (bool, error)
	DestroyDataset(ctx context.Context, name string) error
	GetProperty(ctx context.Context, name, prop string) (string, error)

	Send(ctx context.Context, snapshot string) (io.ReadCloser, error)
	Receive(ctx context.Context, destName string, r io.Reader) error
	ListTemplateNames(ctx context.Context) ([]string, error)

	// CreateSnapshot/RollbackSnapshot/DestroySnapshot/ListSnapshots back
	// ADR-0090's VM checkpoint/rollback RPCs below.
	CreateSnapshot(ctx context.Context, name string) error
	RollbackSnapshot(ctx context.Context, name string) error
	DestroySnapshot(ctx context.Context, name string) error
	ListSnapshots(ctx context.Context, datasetName string) ([]string, error)
}

// nodeConfigStore is the subset of *nodeconfig.Manager GetNodeConfig/
// UpdateNodeConfig need, defined locally for the same fakeability
// reason as quotaSetter above.
type nodeConfigStore interface {
	Load() (nodeconfig.Config, error)
	Save(nodeconfig.Config) error
}

type nodeConfigHistoryStore interface {
	History() ([]nodeconfig.ConfigChange, error)
	SaveWithHistory(nodeconfig.Config, nodeconfig.ChangeOrigin, string, string) error
	RecordCurrent(string, nodeconfig.ChangeOrigin, string, string) error
}

// frontendConfigStore/restshimdConfigStore/raftdConfigStore (ADR-0102)
// are the subset of *frontendconfig.Manager/*restshimdconfig.Manager/
// *raftdconfig.Manager the new Get*Config/Update*Config handlers need,
// defined locally for the same fakeability reason as nodeConfigStore
// above.
type frontendConfigStore interface {
	Load() (frontendconfig.Config, error)
	Save(frontendconfig.Config) error
}

type restshimdConfigStore interface {
	Load() (restshimdconfig.Config, error)
	Save(restshimdconfig.Config) error
}

// raftdConfigStore is deliberately Load-only - see GetRaftdConfig's own
// doc comment for why there is no UpdateRaftdConfig (and so no Save
// call) at all.
type raftdConfigStore interface {
	Load() (raftdconfig.Config, error)
}

// raftdConversionConfigStore is ConvertStandaloneToJoiner's own
// Load-and-Save view of raftd.json (ADR-0105) - see raftdConversionConfig's
// own field doc comment above for why this is a separate interface from
// raftdConfigStore rather than adding Save there.
type raftdConversionConfigStore interface {
	Load() (raftdconfig.Config, error)
	Save(raftdconfig.Config) error
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
	return &Server{raft: raft, nodeID: nodeID, isos: isos, vnc: vnc, serialLog: serialLog, vlan: vlanMgr, statsGather: hoststats.Gather, peers: peers, peerManagerdPort: peerManagerdPort, zfs: zfsMgr, nodeConfig: nodeConfig, listNetworkInterfaces: netif.List, assumptions: assumptionStoreMgr, assumptionStaleAfter: assumptionStaleAfter, reconciler: reconciler, services: rcServiceController{}, pamLockouts: newPAMLockoutTracker(), reachabilityCheck: dialReachable, raftdConversion: rcRaftdConversionAdapter{}}
}

// SetNetworkInterfaceLister overrides host interface discovery for tests.
// Production servers use netif.List automatically.
func (s *Server) SetNetworkInterfaceLister(lister func() ([]netif.Interface, error)) {
	s.listNetworkInterfaces = lister
}

// SetHASTStatusReader wires local, read-only HAST status after construction.
func (s *Server) SetHASTStatusReader(reader hastStatusReader) { s.hastStatus = reader }

// SetAssumptionRegister wires the local, operator-authored register after
// construction. It exists as a setter to keep the established NewServer
// signature stable across managerd and its many focused tests.
func (s *Server) SetAssumptionRegister(register assumptionRegisterStore) {
	s.register = register
}

// SetOriginCAIssuer enables explicit local Origin CA issuance. Production
// wires Cloudflare's client here; tests may supply a non-networking issuer.
func (s *Server) SetOriginCAIssuer(issuer origincert.Issuer) { s.originCA = issuer }

// SetFrontendConfig/SetRestshimdConfig/SetRaftdConfig (ADR-0102) wire
// this managerd's ability to read/write the three sibling daemons' own
// config files, co-located on this same host - nil (the default)
// leaves Get*Config/Update*Config reporting an error rather than
// panicking, the same opt-in pattern nodeConfig itself already
// follows. Setters, not NewServer parameters, for the same reason
// SetAssumptionRegister/SetOriginCAIssuer above are setters: keeping
// every existing positional NewServer(...) call site untouched.
func (s *Server) SetFrontendConfig(store frontendConfigStore)   { s.frontendConfig = store }
func (s *Server) SetRestshimdConfig(store restshimdConfigStore) { s.restshimdConfig = store }
func (s *Server) SetRaftdConfig(store raftdConfigStore)         { s.raftdConfig = store }
func (s *Server) SetRaftdConversionConfig(store raftdConversionConfigStore) {
	s.raftdConversionConfig = store
}
func (s *Server) SetRaftdConversion(ctrl raftdConversionController) { s.raftdConversion = ctrl }

// SetRestartGuardrailToken wires the action-preflight restart guardrail's
// dedicated credential (ADR-0103) - loaded once by cmd/managerd from a
// root-owned local file, never an RPC-settable value. Empty (the
// default, no file provisioned) means ReserveRestartLease/
// ConfirmRestartCompleted can never succeed for a forwarded call.
func (s *Server) SetRestartGuardrailToken(token string) { s.restartGuardrailToken = token }

// SetRestartConfirmStore wires the small local state file
// RestartNodeService writes before restarting apiary_managerd, read back
// by cmd/managerd's own startup path to self-confirm (ADR-0103). Nil
// (the default) leaves RestartNodeService unable to persist the pending
// record - callers should always wire this alongside restartGuardrailToken.
func (s *Server) SetRestartConfirmStore(store *RestartConfirmStore) { s.restartConfirm = store }

func originCertificateInfo(entry origincert.InventoryEntry) *rpcpb.OriginCertificateInfo {
	return &rpcpb.OriginCertificateInfo{Name: entry.Name, Service: entry.Service,
		Hostnames: append([]string(nil), entry.Hostnames...), Id: entry.ID,
		ExpiresAtUnix: entry.ExpiresAt.Unix(), CertPath: entry.CertPath,
		KeyPath: entry.KeyPath, UpdatedAtUnix: entry.UpdatedAt.Unix(),
		AutoRenew: entry.AutoRenew}
}

func (s *Server) ListOriginCertificates(_ context.Context, _ *rpcpb.ListOriginCertificatesRequest) (*rpcpb.ListOriginCertificatesResponse, error) {
	if s.nodeConfig == nil {
		return &rpcpb.ListOriginCertificatesResponse{Error: "this node has no node-config store configured"}, nil
	}
	cfg, err := s.nodeConfig.Load()
	if err != nil {
		return &rpcpb.ListOriginCertificatesResponse{Error: err.Error()}, nil
	}
	if cfg.OriginCADirectory == "" {
		return &rpcpb.ListOriginCertificatesResponse{}, nil
	}
	entries, err := origincert.LoadInventory(cfg.OriginCADirectory)
	if err != nil {
		return &rpcpb.ListOriginCertificatesResponse{Error: err.Error()}, nil
	}
	resp := &rpcpb.ListOriginCertificatesResponse{}
	for _, entry := range entries {
		resp.Certificates = append(resp.Certificates, originCertificateInfo(entry))
	}
	return resp, nil
}

func (s *Server) IssueOriginCertificate(ctx context.Context, req *rpcpb.IssueOriginCertificateRequest) (*rpcpb.IssueOriginCertificateResponse, error) {
	if s.nodeConfig == nil || s.originCA == nil {
		return &rpcpb.IssueOriginCertificateResponse{Error: "Origin CA issuance is not configured on this Comb"}, nil
	}
	cfg, err := s.nodeConfig.Load()
	if err != nil {
		return &rpcpb.IssueOriginCertificateResponse{Error: err.Error()}, nil
	}
	if cfg.OriginCATokenFile == "" || cfg.OriginCADirectory == "" {
		return &rpcpb.IssueOriginCertificateResponse{Error: "configure the dedicated Origin CA token file and certificate directory first"}, nil
	}
	if cfg.TLSCert != filepath.Join(cfg.OriginCADirectory, req.GetName()+".crt") || cfg.TLSKey != filepath.Join(cfg.OriginCADirectory, req.GetName()+".key") {
		return &rpcpb.IssueOriginCertificateResponse{Error: "managerd TLS certificate and key paths must match this Origin CA certificate name"}, nil
	}
	token, err := os.ReadFile(cfg.OriginCATokenFile)
	if err != nil {
		return &rpcpb.IssueOriginCertificateResponse{Error: "reading Origin CA token file: " + err.Error()}, nil
	}
	entry, err := origincert.Issue(ctx, s.originCA, origincert.IssueRequest{Directory: cfg.OriginCADirectory, Name: req.GetName(), Service: "apiary_managerd", Hostnames: req.GetHostnames(), ValidityDays: int(req.GetValidityDays()), Token: strings.TrimSpace(string(token)), AutoRenew: req.GetAutoRenew()})
	if err != nil {
		return &rpcpb.IssueOriginCertificateResponse{Error: err.Error()}, nil
	}
	if err := s.services.Restart(ctx, "apiary_managerd"); err != nil {
		return &rpcpb.IssueOriginCertificateResponse{Certificate: originCertificateInfo(entry), Error: "certificate installed but managerd restart failed: " + err.Error()}, nil
	}
	return &rpcpb.IssueOriginCertificateResponse{Certificate: originCertificateInfo(entry), RestartScheduled: true}, nil
}

func toRPCAssumptionClaim(claim assumptionregister.Claim) *rpcpb.AssumptionClaim {
	return &rpcpb.AssumptionClaim{
		Id: claim.ID, Statement: claim.Statement, Owner: claim.Owner,
		Scope: claim.Scope, Evidence: claim.Evidence,
		VerificationMethod: claim.VerificationMethod,
		ExpiresAtUnix:      claim.ExpiresAt.Unix(), CreatedAtUnix: claim.CreatedAt.Unix(),
		UpdatedAtUnix: claim.UpdatedAt.Unix(),
	}
}

func fromRPCAssumptionClaim(claim *rpcpb.AssumptionClaim) assumptionregister.Claim {
	return assumptionregister.Claim{
		ID: claim.GetId(), Statement: claim.GetStatement(), Owner: claim.GetOwner(),
		Scope: claim.GetScope(), Evidence: claim.GetEvidence(),
		VerificationMethod: claim.GetVerificationMethod(),
		ExpiresAt:          time.Unix(claim.GetExpiresAtUnix(), 0),
	}
}

// relevantRegisterClaims returns unexpired local claims scoped to either the
// whole Colony or one Hive. It does not parse arbitrary prose scopes or treat
// an operator claim as evidence used by health or recovery calculations.
func (s *Server) relevantRegisterClaims(nodeID string, now time.Time) []*rpcpb.AssumptionClaim {
	if s.register == nil {
		return nil
	}
	claims, err := s.register.List()
	if err != nil {
		return nil
	}
	wantHive := "hive:" + nodeID
	var out []*rpcpb.AssumptionClaim
	for _, claim := range claims {
		if !claim.ExpiresAt.After(now) {
			continue
		}
		if claim.Scope == "colony" || claim.Scope == wantHive {
			out = append(out, toRPCAssumptionClaim(claim))
		}
	}
	return out
}

func (s *Server) ListAssumptionClaims(context.Context, *rpcpb.ListAssumptionClaimsRequest) (*rpcpb.ListAssumptionClaimsResponse, error) {
	if s.register == nil {
		return &rpcpb.ListAssumptionClaimsResponse{Error: "no assumption register configured on this node"}, nil
	}
	claims, err := s.register.List()
	if err != nil {
		return &rpcpb.ListAssumptionClaimsResponse{Error: err.Error()}, nil
	}
	resp := &rpcpb.ListAssumptionClaimsResponse{}
	for _, claim := range claims {
		resp.Claims = append(resp.Claims, toRPCAssumptionClaim(claim))
	}
	return resp, nil
}

func (s *Server) SaveAssumptionClaim(_ context.Context, req *rpcpb.SaveAssumptionClaimRequest) (*rpcpb.SaveAssumptionClaimResponse, error) {
	if s.register == nil {
		return &rpcpb.SaveAssumptionClaimResponse{Error: "no assumption register configured on this node"}, nil
	}
	if req.GetClaim() == nil {
		return &rpcpb.SaveAssumptionClaimResponse{Error: "claim is required"}, nil
	}
	if err := s.register.Save(fromRPCAssumptionClaim(req.GetClaim()), time.Now()); err != nil {
		return &rpcpb.SaveAssumptionClaimResponse{Error: err.Error()}, nil
	}
	return &rpcpb.SaveAssumptionClaimResponse{}, nil
}

func (s *Server) DeleteAssumptionClaim(_ context.Context, req *rpcpb.DeleteAssumptionClaimRequest) (*rpcpb.DeleteAssumptionClaimResponse, error) {
	if s.register == nil {
		return &rpcpb.DeleteAssumptionClaimResponse{Error: "no assumption register configured on this node"}, nil
	}
	if err := s.register.Delete(req.GetId()); err != nil {
		return &rpcpb.DeleteAssumptionClaimResponse{Error: err.Error()}, nil
	}
	return &rpcpb.DeleteAssumptionClaimResponse{}, nil
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

// augmentForwardError appends ferr's own text to baseErr when a forward
// attempt to leaderHint was actually made and failed (ferr != nil) -
// every leader-forwarding RPC below uses this instead of returning
// baseErr bare, so a real forwarding failure (TLS handshake, peer auth,
// network) is never silently indistinguishable from "this node simply
// doesn't know who the leader is." That indistinguishability was a real,
// live-diagnosed bug (see SHARED.md's 2026-09-21 buzz/brood incident):
// LeaderHint was correctly populated and a forward was attempted, but
// its TLS-verification failure was discarded entirely, leaving only
// the generic "raft: this node is not the leader" - which took an
// extensive live SSH investigation with a custom-built diagnostic tool
// to trace back to its real cause. ferr == nil (no forward attempted,
// or the forward itself never ran) returns baseErr completely
// unchanged - this must never alter the existing "no peers configured"
// or "no leader known" error text.
func augmentForwardError(baseErr, leaderHint string, ferr error) string {
	if ferr == nil {
		return baseErr
	}
	return fmt.Sprintf("%s (forwarding to leader hint %q also failed: %v)", baseErr, leaderHint, ferr)
}

// Status implements rpcpb.ManagerServiceServer. If raftd is unreachable,
// it still returns a normal response with RaftReachable=false and
// RaftError set, rather than a gRPC error, so callers always get a
// diagnosable payload.
func (s *Server) Status(ctx context.Context, _ *rpcpb.StatusRequest) (*rpcpb.StatusResponse, error) {
	resp := &rpcpb.StatusResponse{ManagerNodeId: s.nodeID, PamConfigured: s.authPAM != nil}

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

// AuthenticatePassword checks username/password against this node's
// own PAM stack (ADR-0087). Exempted from checkAuth entirely (see
// AuthUnaryInterceptor) - a caller here has no API key yet by
// definition, and the real PAM check is the actual security boundary.
//
// pamLockouts (ADR-0096) enforces its own lockout directly here, not
// only in internal/frontend's handleLogin: this RPC's own
// unauthenticated exemption means any network client reaching
// managerd's gRPC port can call it directly, bypassing frontend (and
// its own, separate lockout) entirely. Without a check here too, that
// path let an attacker brute-force a real PAM/UNIX account with no
// rate limit at all - the actual trust boundary for this credential
// check is this handler, not frontend's HTTP layer in front of it.
func (s *Server) AuthenticatePassword(_ context.Context, req *rpcpb.AuthenticatePasswordRequest) (*rpcpb.AuthenticatePasswordResponse, error) {
	if s.authPAM == nil {
		return &rpcpb.AuthenticatePasswordResponse{Error: "this node has no PAM login configured"}, nil
	}
	user := req.GetUsername()
	if locked, remaining := s.pamLockouts.Locked(user); locked {
		return &rpcpb.AuthenticatePasswordResponse{Error: fmt.Sprintf("account temporarily locked after repeated failed attempts; try again in %s", remaining.Round(time.Second))}, nil
	}
	ok, err := s.authPAM.Authenticate(user, req.GetPassword())
	if err != nil {
		return &rpcpb.AuthenticatePasswordResponse{Error: err.Error()}, nil
	}
	if !ok {
		s.pamLockouts.RecordFailure(user)
		return &rpcpb.AuthenticatePasswordResponse{Ok: false}, nil
	}
	s.pamLockouts.RecordSuccess(user)
	return &rpcpb.AuthenticatePasswordResponse{Ok: true}, nil
}

// GetLocalNodeHealth implements rpcpb.ManagerServiceServer. It exposes the
// existing Evidence-Aware Health v1 calculation (ADR-0056) as a reusable,
// explicitly local read. It does not forward to a leader: local raft and
// reconciler observations from another Hive would be false attribution.
func (s *Server) GetLocalNodeHealth(ctx context.Context, _ *rpcpb.GetLocalNodeHealthRequest) (*rpcpb.GetLocalNodeHealthResponse, error) {
	now := time.Now()
	status, err := s.Status(ctx, &rpcpb.StatusRequest{})
	if err != nil {
		return nil, err
	}
	stats, err := s.HostStats(ctx, &rpcpb.HostStatsRequest{})
	if err != nil {
		return nil, err
	}

	// One Inputs, one SignalsFrom, one ComputeNodeHealth - the same
	// derivation the cluster-wide ClusterHealth handler below runs, so
	// this local answer and that cluster-wide one can never be
	// computed by two different rule sets.
	inputs := health.Inputs{
		NodeID:                   s.nodeID,
		IsLocal:                  true,
		PeerForwardingConfigured: s.peers != nil,
		MembershipObservedAt:     now,
		HeartbeatObserved:        true,
		HeartbeatOK:              status.GetRaftReachable(),
		MembershipObserved:       status.GetRaftReachable(),
		AppliedIndexObserved:     status.GetRaftReachable(),
		AppliedIndex:             status.GetRaftAppliedIndex(),
		LastLogIndex:             status.GetRaftLastLogIndex(),
		IndicesObservedAt:        now,
		ReconcileIntervalSeconds: stats.GetReconcileIntervalSeconds(),
		ReconcileObservedAt:      now,
	}
	if status.GetRaftReachable() {
		for _, member := range status.GetMembers() {
			if member.GetNodeId() == s.nodeID {
				inputs.MemberFound = true
				inputs.Suffrage = health.ParseSuffrage(member.GetSuffrage())
				break
			}
		}
	}
	if unix := stats.GetLastReconcileAttemptUnix(); unix > 0 {
		inputs.ReconcileEverAttempted = true
		inputs.LastReconcileAttempt = time.Unix(unix, 0)
	}
	if unix := stats.GetLastReconcileSuccessUnix(); unix > 0 {
		inputs.ReconcileEverSucceeded = true
		inputs.LastReconcileSuccess = time.Unix(unix, 0)
	}

	result := health.ComputeNodeHealth(health.SignalsFrom(inputs), now)
	response := &rpcpb.GetLocalNodeHealthResponse{
		NodeId:      result.NodeID,
		Status:      string(result.Status),
		Explanation: result.Explanation,
	}
	for _, observation := range result.Observations {
		var observedUnix int64
		if !observation.ObservedAt.IsZero() {
			observedUnix = observation.ObservedAt.Unix()
		}
		response.Observations = append(response.Observations, &rpcpb.HealthObservation{
			Source:                observation.Source,
			ObservedUnix:          observedUnix,
			FreshnessLimitSeconds: uint32(observation.FreshnessLimit / time.Second),
			Value:                 observation.Value,
			Detail:                observation.Detail,
		})
	}
	response.RelevantClaims = s.relevantRegisterClaims(s.nodeID, now)
	return response, nil
}

// ClusterHealth implements rpcpb.ManagerServiceServer (ADR-0122): the
// cluster-wide Evidence-Aware Health verdict, computed here so a
// non-HTML consumer gets the same answer the web UI shows instead of
// reimplementing internal/health's decision chain against the raw wire
// fields - the drift ADR-0056's own Consequences section warned about.
//
// It is deliberately NOT folded into Status or HostStats: those answer
// for one node and are called on paths that must not acquire a
// cluster-wide fan-out cost (ADR-0056's own scoping decision, kept
// intact here).
//
// Like every multi-source read in this service it is not an atomic
// snapshot - each node's Status and HostStats are separate sequential
// reads, so one node's verdict can reflect a slightly earlier moment
// than another's.
func (s *Server) ClusterHealth(ctx context.Context, _ *rpcpb.ClusterHealthRequest) (*rpcpb.ClusterHealthResponse, error) {
	now := time.Now()
	anchor, err := s.Status(ctx, &rpcpb.StatusRequest{})
	if err != nil {
		return nil, err
	}

	membershipObserved := anchor.GetRaftReachable()
	nodeIDs := anchor.GetKnownNodeIds()
	if len(nodeIDs) == 0 && s.nodeID != "" {
		nodeIDs = []string{s.nodeID}
	}

	response := &rpcpb.ClusterHealthResponse{LocalNodeId: s.nodeID}
	if !membershipObserved {
		// Every node's verdict is capped at unknown without membership,
		// so report the single shared cause once rather than repeating
		// it unexplained on every row.
		response.Error = "raft membership could not be read from this node - every Comb's verdict is capped at unknown"
	}

	// Raft membership is a cluster-wide-consistent replicated fact, so
	// it is read exactly once above and reused for every node below.
	// RaftMember.address is the member's own raft transport address;
	// peerManagerdAddr strips its port and applies this deployment's
	// managerd port, the same conversion every other peer call uses.
	memberByNode := make(map[string]*rpcpb.RaftMember, len(anchor.GetMembers()))
	for _, member := range anchor.GetMembers() {
		memberByNode[member.GetNodeId()] = member
	}

	type nodeResult struct {
		verdict health.NodeHealth
		dialed  bool
	}
	results := make([]nodeResult, len(nodeIDs))
	var wg sync.WaitGroup
	for i, nodeID := range nodeIDs {
		wg.Add(1)
		go func(i int, nodeID string) {
			defer wg.Done()
			dialed, verdict := s.clusterNodeHealth(ctx, nodeID, s.nodeID, anchor, membershipObserved, memberByNode, now)
			results[i] = nodeResult{verdict: verdict, dialed: dialed}
		}(i, nodeID)
	}
	wg.Wait()

	sort.Slice(results, func(i, j int) bool { return results[i].verdict.NodeID < results[j].verdict.NodeID })
	for _, result := range results {
		response.Nodes = append(response.Nodes, &rpcpb.ClusterNodeHealth{
			NodeId:       result.verdict.NodeID,
			Status:       string(result.verdict.Status),
			Explanation:  result.verdict.Explanation,
			Observations: toRPCHealthObservations(result.verdict.Observations),
			Dialed:       result.dialed,
		})
	}
	return response, nil
}

// clusterNodeHealth gathers one node's evidence and computes its verdict.
// It also reports whether the node was actually dialed, so a caller can
// distinguish "established by a real call" from "true by definition,
// because it answered this very request." All I/O is bounded by the
// existing reachabilityCheckTimeout.
func (s *Server) clusterNodeHealth(
	ctx context.Context,
	nodeID, localNodeID string,
	anchor *rpcpb.StatusResponse,
	membershipObserved bool,
	memberByNode map[string]*rpcpb.RaftMember,
	now time.Time,
) (bool, health.NodeHealth) {
	inputs := health.Inputs{
		NodeID:                   nodeID,
		IsLocal:                  nodeID == localNodeID,
		PeerForwardingConfigured: s.peers != nil,
		MembershipObserved:       membershipObserved,
		MembershipObservedAt:     now,
	}

	if member, found := memberByNode[nodeID]; found && membershipObserved {
		inputs.MemberFound = true
		inputs.Suffrage = health.ParseSuffrage(member.GetSuffrage())
	}

	var stats *rpcpb.HostStatsResponse
	dialed := false
	if inputs.IsLocal {
		// No dial is needed or wanted: this node is answering now, and
		// self-dialing would be a pointless extra hop.
		stats, _ = s.HostStats(ctx, &rpcpb.HostStatsRequest{})
	} else if s.peers != nil {
		if member, known := memberByNode[nodeID]; known && member.GetAddress() != "" {
			addr := s.peerManagerdAddr(member.GetAddress())
			dialed = true
			inputs.PeerDialAttempted = true

			checkCtx, cancel := context.WithTimeout(ctx, reachabilityCheckTimeout)
			peerStats, statsErr := s.peers.HostStats(checkCtx, addr)
			if statsErr == nil {
				inputs.PeerDialSucceeded = true
				stats = peerStats
			}
			// A separate, independent probe for the peer's own raft
			// self-report and log position - deliberately NOT inferred
			// from whether HostStats answered, which says nothing at
			// all about that peer's own raftd.
			if peerStatus, statusErr := s.peers.Status(checkCtx, addr); statusErr == nil {
				inputs.HeartbeatObserved = true
				inputs.HeartbeatOK = peerStatus.GetRaftReachable()
				if peerStatus.GetRaftReachable() {
					inputs.AppliedIndexObserved = true
					inputs.AppliedIndex = peerStatus.GetRaftAppliedIndex()
					inputs.LastLogIndex = peerStatus.GetRaftLastLogIndex()
					inputs.IndicesObservedAt = now
				}
			}
			cancel()
		}
	}

	if inputs.IsLocal {
		// The anchor Status call already answered for this node, so its
		// own self-report and log position are already in hand - no
		// redundant self-dial.
		inputs.HeartbeatObserved = true
		inputs.HeartbeatOK = anchor.GetRaftReachable()
		inputs.AppliedIndexObserved = anchor.GetRaftReachable()
		inputs.AppliedIndex = anchor.GetRaftAppliedIndex()
		inputs.LastLogIndex = anchor.GetRaftLastLogIndex()
		inputs.IndicesObservedAt = now
	}

	if stats != nil {
		inputs.ReconcileObservedAt = now
		inputs.ReconcileIntervalSeconds = stats.GetReconcileIntervalSeconds()
		if unix := stats.GetLastReconcileAttemptUnix(); unix > 0 {
			inputs.ReconcileEverAttempted = true
			inputs.LastReconcileAttempt = time.Unix(unix, 0)
		}
		if unix := stats.GetLastReconcileSuccessUnix(); unix > 0 {
			inputs.ReconcileEverSucceeded = true
			inputs.LastReconcileSuccess = time.Unix(unix, 0)
		}
	}

	return dialed, health.ComputeNodeHealth(health.SignalsFrom(inputs), now)
}

// toRPCHealthObservations converts computed observations for the wire.
func toRPCHealthObservations(observations []health.Observation) []*rpcpb.HealthObservation {
	out := make([]*rpcpb.HealthObservation, 0, len(observations))
	for _, observation := range observations {
		var observedUnix int64
		if !observation.ObservedAt.IsZero() {
			observedUnix = observation.ObservedAt.Unix()
		}
		out = append(out, &rpcpb.HealthObservation{
			Source:                observation.Source,
			ObservedUnix:          observedUnix,
			FreshnessLimitSeconds: uint32(observation.FreshnessLimit / time.Second),
			Value:                 observation.Value,
			Detail:                observation.Detail,
		})
	}
	return out
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
	var ferr error
	if leaderHint != "" && s.peers != nil {
		var fwd *rpcpb.CreateNetworkResponse
		if fwd, ferr = s.peers.CreateNetwork(ctx, s.peerManagerdAddr(leaderHint), req); ferr == nil {
			return fwd, nil
		}
	}
	return &rpcpb.CreateNetworkResponse{Network: fromInternalNetwork(network), Error: augmentForwardError(appErr, leaderHint, ferr), LeaderHint: leaderHint}, nil
}

// DeleteNetwork implements rpcpb.ManagerServiceServer. See CreateNetwork's
// doc comment for the forwarding rationale, identical here.
func (s *Server) DeleteNetwork(ctx context.Context, req *rpcpb.DeleteNetworkRequest) (*rpcpb.DeleteNetworkResponse, error) {
	cmd := &internalpb.Command{
		Op: &internalpb.Command_DeleteNetwork{DeleteNetwork: &internalpb.DeleteNetwork{Id: req.GetId()}},
	}
	network, appErr, leaderHint := s.applyNetworkCommand(ctx, cmd, req.GetTimeoutMs())
	var ferr error
	if leaderHint != "" && s.peers != nil {
		var fwd *rpcpb.DeleteNetworkResponse
		if fwd, ferr = s.peers.DeleteNetwork(ctx, s.peerManagerdAddr(leaderHint), req); ferr == nil {
			return fwd, nil
		}
	}
	return &rpcpb.DeleteNetworkResponse{Network: fromInternalNetwork(network), Error: augmentForwardError(appErr, leaderHint, ferr), LeaderHint: leaderHint}, nil
}

// SetNetworkName implements rpcpb.ManagerServiceServer - renames a
// network, the only field ADR-0071/ADR-0080 allow changing on an
// existing definition without the full replace-after-teardown workflow.
func (s *Server) SetNetworkName(ctx context.Context, req *rpcpb.SetNetworkNameRequest) (*rpcpb.SetNetworkNameResponse, error) {
	cmd := &internalpb.Command{
		Op: &internalpb.Command_SetNetworkName{SetNetworkName: &internalpb.SetNetworkName{Id: req.GetId(), Name: req.GetName()}},
	}
	network, appErr, leaderHint := s.applyNetworkCommand(ctx, cmd, req.GetTimeoutMs())
	var ferr error
	if leaderHint != "" && s.peers != nil {
		var fwd *rpcpb.SetNetworkNameResponse
		if fwd, ferr = s.peers.SetNetworkName(ctx, s.peerManagerdAddr(leaderHint), req); ferr == nil {
			return fwd, nil
		}
	}
	return &rpcpb.SetNetworkNameResponse{Network: fromInternalNetwork(network), Error: augmentForwardError(appErr, leaderHint, ferr), LeaderHint: leaderHint}, nil
}

// GetNetworkTeardownStatus implements rpcpb.ManagerServiceServer - a
// physical, per-node, local-only report (never routed through raft),
// mirroring ListOrphanedHASTResources's own posture exactly. Backs the
// guided network-replacement workflow (ADR-0071/ADR-0081): the caller
// is expected to call this once per known node and treat any error
// (including "no Reconciler configured") the same as present=true -
// unknown must block a recreate, never be mistaken for evidence that
// cleanup succeeded.
func (s *Server) GetNetworkTeardownStatus(_ context.Context, req *rpcpb.GetNetworkTeardownStatusRequest) (*rpcpb.GetNetworkTeardownStatusResponse, error) {
	if s.reconciler == nil {
		return &rpcpb.GetNetworkTeardownStatusResponse{Error: "this node has no reconciler configured"}, nil
	}
	present, bridge, ownBridge, ownVLAN, outboundNAT, err := s.reconciler.NetworkArtifactStatus(req.GetNetworkId())
	if err != nil {
		return &rpcpb.GetNetworkTeardownStatusResponse{Error: err.Error()}, nil
	}
	return &rpcpb.GetNetworkTeardownStatusResponse{
		Present: present, Bridge: bridge, OwnBridge: ownBridge, OwnVlan: ownVLAN, OutboundNat: outboundNAT,
	}, nil
}

// ListNetworks implements rpcpb.ManagerServiceServer.
func (s *Server) ListNetworks(ctx context.Context, _ *rpcpb.ListNetworksRequest) (*rpcpb.ListNetworksResponse, error) {
	resp, err := s.raft.ListNetworks(ctx)
	if err != nil {
		return &rpcpb.ListNetworksResponse{Error: err.Error()}, nil
	}
	if resp.GetError() != "" {
		var ferr error
		if s.peers != nil && resp.GetLeaderHint() != "" {
			var fwd *rpcpb.ListNetworksResponse
			if fwd, ferr = s.peers.ListNetworks(ctx, s.peerManagerdAddr(resp.GetLeaderHint())); ferr == nil {
				return fwd, nil
			}
		}
		return &rpcpb.ListNetworksResponse{Error: augmentForwardError(resp.GetError(), resp.GetLeaderHint(), ferr), LeaderHint: resp.GetLeaderHint()}, nil
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
		var ferr error
		if leaderHint != "" && s.peers != nil {
			// Forwarded: the leader generates and stores its own fresh
			// raw/hashed pair - the raw/hashed values generated above are
			// simply discarded, never sent anywhere.
			var fwd *rpcpb.CreateAPIKeyResponse
			if fwd, ferr = s.peers.CreateAPIKey(ctx, s.peerManagerdAddr(leaderHint), req); ferr == nil {
				return fwd, nil
			}
		}
		return &rpcpb.CreateAPIKeyResponse{Error: augmentForwardError(appErr, leaderHint, ferr), LeaderHint: leaderHint}, nil
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
	var ferr error
	if leaderHint != "" && s.peers != nil {
		var fwd *rpcpb.RevokeAPIKeyResponse
		if fwd, ferr = s.peers.RevokeAPIKey(ctx, s.peerManagerdAddr(leaderHint), req); ferr == nil {
			return fwd, nil
		}
	}
	return &rpcpb.RevokeAPIKeyResponse{Error: augmentForwardError(appErr, leaderHint, ferr), LeaderHint: leaderHint}, nil
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
		var ferr error
		if s.peers != nil && resp.GetLeaderHint() != "" {
			var fwd *rpcpb.ListAPIKeysResponse
			if fwd, ferr = s.peers.ListAPIKeys(ctx, s.peerManagerdAddr(resp.GetLeaderHint())); ferr == nil {
				return fwd, nil
			}
		}
		return &rpcpb.ListAPIKeysResponse{Error: augmentForwardError(resp.GetError(), resp.GetLeaderHint(), ferr), LeaderHint: resp.GetLeaderHint()}, nil
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

// GetLocalHASTResourceStatus reports only this node's fresh hastctl view.
// The strict resource-name validation prevents caller-controlled paths from
// reaching hastctl; callers must query owner and replica independently.
func (s *Server) GetLocalHASTResourceStatus(ctx context.Context, req *rpcpb.GetLocalHASTResourceStatusRequest) (*rpcpb.GetLocalHASTResourceStatusResponse, error) {
	name := req.GetResourceName()
	if !validHASTResourceName(name) {
		return &rpcpb.GetLocalHASTResourceStatusResponse{Error: "resource_name must be vm-<id> or jail-<id>, with an alphanumeric, '-' or '_' id of 1-64 characters"}, nil
	}
	if s.hastStatus == nil {
		return &rpcpb.GetLocalHASTResourceStatusResponse{Error: "HAST status is not configured on this node"}, nil
	}
	observed, err := s.hastStatus.Status(ctx, name)
	if err != nil {
		return &rpcpb.GetLocalHASTResourceStatusResponse{Error: err.Error(), ResourceName: name, ObservedAtUnix: time.Now().Unix()}, nil
	}
	if observed == nil {
		return &rpcpb.GetLocalHASTResourceStatusResponse{Error: "HAST status returned no observation", ResourceName: name, ObservedAtUnix: time.Now().Unix()}, nil
	}
	return &rpcpb.GetLocalHASTResourceStatusResponse{
		ResourceName: name, Role: observed.Role, ResourceStatus: observed.ResourceStatus,
		Replication: observed.Replication, Dirty: observed.Dirty,
		ExtentSize: observed.ExtentSize, ObservedAtUnix: time.Now().Unix(),
	}, nil
}

func validHASTResourceName(name string) bool {
	var id string
	switch {
	case strings.HasPrefix(name, "vm-"):
		id = strings.TrimPrefix(name, "vm-")
	case strings.HasPrefix(name, "jail-"):
		id = strings.TrimPrefix(name, "jail-")
	default:
		return false
	}
	if len(id) == 0 || len(id) > 64 {
		return false
	}
	for _, c := range id {
		if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '-' || c == '_') {
			return false
		}
	}
	return true
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

// PurgeStaleAssumptionResults implements rpcpb.ManagerServiceServer.
// Like ListAssumptionResults, this only ever touches THIS node's own
// store - never routed through raft, never leader-forwarded. Uses the
// same s.assumptionStaleAfter threshold ListAssumptionResults already
// uses to compute the `stale` flag callers see, so this can never
// remove an entry that wasn't already visibly labeled stale.
func (s *Server) PurgeStaleAssumptionResults(_ context.Context, _ *rpcpb.PurgeStaleAssumptionResultsRequest) (*rpcpb.PurgeStaleAssumptionResultsResponse, error) {
	if s.assumptions == nil {
		return &rpcpb.PurgeStaleAssumptionResultsResponse{Error: "no assumptions store configured on this node"}, nil
	}
	removed, err := s.assumptions.PurgeStale(s.assumptionStaleAfter, time.Now())
	if err != nil {
		return &rpcpb.PurgeStaleAssumptionResultsResponse{Error: err.Error()}, nil
	}
	return &rpcpb.PurgeStaleAssumptionResultsResponse{RemovedCount: uint32(removed)}, nil
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
	var ferr error
	if leaderHint != "" && s.peers != nil {
		var fwd *rpcpb.CreateVMResponse
		if fwd, ferr = s.peers.CreateVM(ctx, s.peerManagerdAddr(leaderHint), req); ferr == nil {
			return fwd, nil
		}
	}
	return &rpcpb.CreateVMResponse{Vm: fromInternalVM(vm), Error: augmentForwardError(appErr, leaderHint, ferr), LeaderHint: leaderHint}, nil
}

// UpdateVM implements rpcpb.ManagerServiceServer. See CreateNetwork's
// doc comment for the forwarding rationale, identical here.
func (s *Server) UpdateVM(ctx context.Context, req *rpcpb.UpdateVMRequest) (*rpcpb.UpdateVMResponse, error) {
	cmd := &internalpb.Command{
		Op: &internalpb.Command_UpdateVm{UpdateVm: &internalpb.UpdateVM{Vm: toInternalVM(req.GetVm())}},
	}
	vm, appErr, leaderHint := s.applyCommand(ctx, cmd, req.GetTimeoutMs())
	var ferr error
	if leaderHint != "" && s.peers != nil {
		var fwd *rpcpb.UpdateVMResponse
		if fwd, ferr = s.peers.UpdateVM(ctx, s.peerManagerdAddr(leaderHint), req); ferr == nil {
			return fwd, nil
		}
	}
	return &rpcpb.UpdateVMResponse{Vm: fromInternalVM(vm), Error: augmentForwardError(appErr, leaderHint, ferr), LeaderHint: leaderHint}, nil
}

// DeleteVM implements rpcpb.ManagerServiceServer. See CreateNetwork's
// doc comment for the forwarding rationale, identical here.
func (s *Server) DeleteVM(ctx context.Context, req *rpcpb.DeleteVMRequest) (*rpcpb.DeleteVMResponse, error) {
	cmd := &internalpb.Command{
		Op: &internalpb.Command_DeleteVm{DeleteVm: &internalpb.DeleteVM{Id: req.GetId()}},
	}
	vm, appErr, leaderHint := s.applyCommand(ctx, cmd, req.GetTimeoutMs())
	var ferr error
	if leaderHint != "" && s.peers != nil {
		var fwd *rpcpb.DeleteVMResponse
		if fwd, ferr = s.peers.DeleteVM(ctx, s.peerManagerdAddr(leaderHint), req); ferr == nil {
			return fwd, nil
		}
	}
	return &rpcpb.DeleteVMResponse{Vm: fromInternalVM(vm), Error: augmentForwardError(appErr, leaderHint, ferr), LeaderHint: leaderHint}, nil
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
	var ferr error
	if leaderHint != "" && s.peers != nil {
		var fwd *rpcpb.SetVMFirewallPausedResponse
		if fwd, ferr = s.peers.SetVMFirewallPaused(ctx, s.peerManagerdAddr(leaderHint), req); ferr == nil {
			return fwd, nil
		}
	}
	return &rpcpb.SetVMFirewallPausedResponse{Vm: fromInternalVM(vm), Error: augmentForwardError(appErr, leaderHint, ferr), LeaderHint: leaderHint}, nil
}

// SetVMFirewallRules implements rpcpb.ManagerServiceServer - lets an
// operator edit a VM's firewall rules after creation, previously only
// settable via CreateVM. Same narrow, atomic-apply shape as
// SetVMFirewallPaused above, for the identical reason (ADR-0049).
func (s *Server) SetVMFirewallRules(ctx context.Context, req *rpcpb.SetVMFirewallRulesRequest) (*rpcpb.SetVMFirewallRulesResponse, error) {
	cmd := &internalpb.Command{
		Op: &internalpb.Command_SetVmFirewallRules{SetVmFirewallRules: &internalpb.SetVMFirewallRules{
			Id: req.GetId(), FirewallRules: toInternalFirewallRules(req.GetFirewallRules()),
		}},
	}
	vm, appErr, leaderHint := s.applyCommand(ctx, cmd, req.GetTimeoutMs())
	var ferr error
	if leaderHint != "" && s.peers != nil {
		var fwd *rpcpb.SetVMFirewallRulesResponse
		if fwd, ferr = s.peers.SetVMFirewallRules(ctx, s.peerManagerdAddr(leaderHint), req); ferr == nil {
			return fwd, nil
		}
	}
	return &rpcpb.SetVMFirewallRulesResponse{Vm: fromInternalVM(vm), Error: augmentForwardError(appErr, leaderHint, ferr), LeaderHint: leaderHint}, nil
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
			var ferr error
			if s.peers != nil && getResp.GetLeaderHint() != "" {
				var fwd *rpcpb.SetVMCloudflareExposureResponse
				if fwd, ferr = s.peers.SetVMCloudflareExposure(ctx, s.peerManagerdAddr(getResp.GetLeaderHint()), req); ferr == nil {
					return fwd, nil
				}
			}
			return &rpcpb.SetVMCloudflareExposureResponse{Error: augmentForwardError(getResp.GetError(), getResp.GetLeaderHint(), ferr), LeaderHint: getResp.GetLeaderHint()}, nil
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
	var ferr error
	if leaderHint != "" && s.peers != nil {
		var fwd *rpcpb.SetVMCloudflareExposureResponse
		if fwd, ferr = s.peers.SetVMCloudflareExposure(ctx, s.peerManagerdAddr(leaderHint), req); ferr == nil {
			return fwd, nil
		}
	}
	return &rpcpb.SetVMCloudflareExposureResponse{Vm: fromInternalVM(vm), Error: augmentForwardError(appErr, leaderHint, ferr), LeaderHint: leaderHint}, nil
}

// SetVMDesiredState changes only a VM lifecycle target. Unlike UpdateVM it is
// an atomic FSM operation, so a Stop, Start, or Restart request cannot erase
// unrelated configuration from a concurrent full-record edit.
func (s *Server) SetVMDesiredState(ctx context.Context, req *rpcpb.SetVMDesiredStateRequest) (*rpcpb.SetVMDesiredStateResponse, error) {
	cmd := &internalpb.Command{Op: &internalpb.Command_SetVmDesiredState{SetVmDesiredState: &internalpb.SetVMDesiredState{
		Id: req.GetId(), DesiredState: internalpb.VMState(req.GetDesiredState()),
	}}}
	vm, appErr, leaderHint := s.applyCommand(ctx, cmd, req.GetTimeoutMs())
	var ferr error
	if leaderHint != "" && s.peers != nil {
		var fwd *rpcpb.SetVMDesiredStateResponse
		if fwd, ferr = s.peers.SetVMDesiredState(ctx, s.peerManagerdAddr(leaderHint), req); ferr == nil {
			return fwd, nil
		}
	}
	return &rpcpb.SetVMDesiredStateResponse{Vm: fromInternalVM(vm), Error: augmentForwardError(appErr, leaderHint, ferr), LeaderHint: leaderHint}, nil
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
		var ferr error
		if s.peers != nil && getResp.GetLeaderHint() != "" {
			var fwd *rpcpb.ForcePurgeVMResponse
			if fwd, ferr = s.peers.ForcePurgeVM(ctx, s.peerManagerdAddr(getResp.GetLeaderHint()), req); ferr == nil {
				return fwd, nil
			}
		}
		return &rpcpb.ForcePurgeVMResponse{Error: augmentForwardError(getResp.GetError(), getResp.GetLeaderHint(), ferr), LeaderHint: getResp.GetLeaderHint()}, nil
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
	var ferr error
	if leaderHint != "" && s.peers != nil {
		var fwd *rpcpb.ForcePurgeVMResponse
		if fwd, ferr = s.peers.ForcePurgeVM(ctx, s.peerManagerdAddr(leaderHint), req); ferr == nil {
			return fwd, nil
		}
	}
	return &rpcpb.ForcePurgeVMResponse{Vm: fromInternalVM(vm), Error: augmentForwardError(appErr, leaderHint, ferr), LeaderHint: leaderHint}, nil
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
		var ferr error
		if s.peers != nil && getResp.GetLeaderHint() != "" {
			var fwd *rpcpb.MigrateVMResponse
			if fwd, ferr = s.peers.MigrateVM(ctx, s.peerManagerdAddr(getResp.GetLeaderHint()), req); ferr == nil {
				return fwd, nil
			}
		}
		return &rpcpb.MigrateVMResponse{Error: augmentForwardError(getResp.GetError(), getResp.GetLeaderHint(), ferr), LeaderHint: getResp.GetLeaderHint()}, nil
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
	var ferr error
	if leaderHint != "" && s.peers != nil {
		var fwd *rpcpb.MigrateVMResponse
		if fwd, ferr = s.peers.MigrateVM(ctx, s.peerManagerdAddr(leaderHint), req); ferr == nil {
			return fwd, nil
		}
	}
	return &rpcpb.MigrateVMResponse{Vm: fromInternalVM(result), Error: augmentForwardError(appErr, leaderHint, ferr), LeaderHint: leaderHint}, nil
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
		var ferr error
		if s.peers != nil && resp.GetLeaderHint() != "" {
			var fwd *rpcpb.GetVMResponse
			if fwd, ferr = s.peers.GetVM(ctx, s.peerManagerdAddr(resp.GetLeaderHint()), req.GetId()); ferr == nil {
				return fwd, nil
			}
		}
		return &rpcpb.GetVMResponse{Error: augmentForwardError(resp.GetError(), resp.GetLeaderHint(), ferr), LeaderHint: resp.GetLeaderHint()}, nil
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

// ListJailTemplateNames implements rpcpb.ManagerServiceServer (ADR-
// 0089) - the jail base-template equivalent of ListISOs, backing the
// reconciler's own "does this peer have it" query when a jail names a
// base_template this node's own ZFS doesn't have yet (ADR-0084).
func (s *Server) ListJailTemplateNames(ctx context.Context, _ *rpcpb.ListJailTemplateNamesRequest) (*rpcpb.ListJailTemplateNamesResponse, error) {
	if s.zfs == nil {
		return &rpcpb.ListJailTemplateNamesResponse{}, nil
	}
	names, err := s.zfs.ListTemplateNames(ctx)
	if err != nil {
		return &rpcpb.ListJailTemplateNamesResponse{Error: err.Error()}, nil
	}
	return &rpcpb.ListJailTemplateNamesResponse{Names: names}, nil
}

// PushJailTemplateTo implements rpcpb.ManagerServiceServer (ADR-0089) -
// the jail base-template equivalent of PushISOTo: this node (which
// already has name) zfs-sends it to target_node_id via a real
// ReceiveJailTemplate client stream. Peer-only, mirrors PushISOTo's
// own reasoning exactly - never meant for direct human/API-client use.
func (s *Server) PushJailTemplateTo(ctx context.Context, req *rpcpb.PushJailTemplateToRequest) (*rpcpb.PushJailTemplateToResponse, error) {
	if req.GetName() == "" || req.GetTargetNodeId() == "" {
		return &rpcpb.PushJailTemplateToResponse{Error: "name and target_node_id are required"}, nil
	}
	if s.zfs == nil {
		return &rpcpb.PushJailTemplateToResponse{Error: "this node has no ZFS support configured"}, nil
	}
	if s.peers == nil {
		return &rpcpb.PushJailTemplateToResponse{Error: "peer forwarding is not configured on this node"}, nil
	}

	rc, err := s.zfs.Send(ctx, "templates/"+req.GetName()+"@apiary-template")
	if err != nil {
		return &rpcpb.PushJailTemplateToResponse{Error: fmt.Sprintf("reading local template %q: %v", req.GetName(), err)}, nil
	}
	defer rc.Close()

	targetAddr, err := s.nodeManagerdAddr(ctx, req.GetTargetNodeId())
	if err != nil {
		return &rpcpb.PushJailTemplateToResponse{Error: err.Error()}, nil
	}

	if err := s.peers.PushJailTemplate(ctx, targetAddr, req.GetName(), rc); err != nil {
		return &rpcpb.PushJailTemplateToResponse{Error: fmt.Sprintf("pushing to %q: %v", req.GetTargetNodeId(), err)}, nil
	}
	return &rpcpb.PushJailTemplateToResponse{}, nil
}

// ReceiveJailTemplate implements rpcpb.ManagerServiceServer (ADR-0089)
// - the jail base-template equivalent of UploadISO. The first stream
// message must carry metadata (the template name); every message
// after that carries a chunk of the `zfs send` stream's bytes, piped
// directly into `zfs receive` as they arrive - never buffered in
// memory, mirroring UploadISO's own io.Pipe-based handler exactly.
func (s *Server) ReceiveJailTemplate(stream rpcpb.ManagerService_ReceiveJailTemplateServer) error {
	first, err := stream.Recv()
	if err != nil {
		return fmt.Errorf("manager: ReceiveJailTemplate: receiving metadata: %w", err)
	}
	meta := first.GetMetadata()
	if meta == nil {
		return fmt.Errorf("manager: ReceiveJailTemplate: first message must be metadata")
	}
	if s.zfs == nil {
		return stream.SendAndClose(&rpcpb.ReceiveJailTemplateResponse{Error: "this node has no ZFS support configured"})
	}

	pr, pw := io.Pipe()
	recvDone := make(chan error, 1)
	go func() {
		err := s.zfs.Receive(stream.Context(), "templates/"+meta.GetName(), pr)
		pr.CloseWithError(err)
		recvDone <- err
	}()

	for {
		req, err := stream.Recv()
		if err == io.EOF {
			pw.Close()
			break
		}
		if err != nil {
			pw.CloseWithError(err)
			<-recvDone
			return fmt.Errorf("manager: ReceiveJailTemplate: receiving chunk: %w", err)
		}
		if _, err := pw.Write(req.GetChunk()); err != nil {
			break // Receive's goroutine already failed; its error is reported below
		}
	}

	if err := <-recvDone; err != nil {
		return stream.SendAndClose(&rpcpb.ReceiveJailTemplateResponse{Error: err.Error()})
	}
	return stream.SendAndClose(&rpcpb.ReceiveJailTemplateResponse{Name: meta.GetName()})
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
	var cloudflareConfigured bool
	if s.reconciler != nil {
		if t, ok := s.reconciler.LastReconcileSuccess(); ok {
			lastReconcileSuccessUnix = t.Unix()
		}
		if t, ok := s.reconciler.LastReconcileAttempt(); ok {
			lastReconcileAttemptUnix = t.Unix()
		}
		reconcileIntervalSeconds = uint32(s.reconciler.ReconcileInterval() / time.Second)
		cloudflareConfigured = s.reconciler.CloudflareConfigured()
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
		CloudflareConfigured:     cloudflareConfigured,
	}, nil
}

// GetVMConsole implements rpcpb.ManagerServiceServer. Leader-only VM
// lookup failures are forwarded to the current leader; the leader then
// answers only when the VM is actually running on that node.
func (s *Server) GetVMConsole(ctx context.Context, req *rpcpb.GetVMConsoleRequest) (*rpcpb.GetVMConsoleResponse, error) {
	resp, err := s.raft.GetVM(ctx, req.GetId())
	if err != nil {
		return &rpcpb.GetVMConsoleResponse{Error: err.Error()}, nil
	}
	if resp.GetError() != "" {
		if s.peers != nil && resp.GetLeaderHint() != "" {
			var fwd *rpcpb.GetVMResponse
			var ferr error
			fwd, ferr = s.peers.GetVM(ctx, s.peerManagerdAddr(resp.GetLeaderHint()), req.GetId())
			if ferr == nil && fwd.GetError() == "" && fwd.GetFound() && fwd.GetVm().GetNodeId() == s.nodeID {
				resp = &internalpb.GetVMResponse{Vm: toInternalVM(fwd.GetVm()), Found: true}
			} else {
				return &rpcpb.GetVMConsoleResponse{Error: augmentForwardError(resp.GetError(), resp.GetLeaderHint(), ferr)}, nil
			}
		}
		if resp.GetError() != "" {
			return &rpcpb.GetVMConsoleResponse{Error: resp.GetError()}, nil
		}
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

// ProxyVMConsole relays an authenticated peer/frontend stream to this Hive's
// loopback-only VNC listener. It intentionally validates through
// GetVMConsole here instead of accepting a host or port from the caller: only
// this managerd may choose the local endpoint, and only for a VM it owns.
func (s *Server) ProxyVMConsole(stream rpcpb.ManagerService_ProxyVMConsoleServer) error {
	first, err := stream.Recv()
	if err != nil {
		return err
	}
	if first.GetOpen() == nil || first.GetOpen().GetId() == "" {
		return fmt.Errorf("first console tunnel frame must open a VM")
	}
	console, err := s.GetVMConsole(stream.Context(), &rpcpb.GetVMConsoleRequest{Id: first.GetOpen().GetId()})
	if err != nil {
		return err
	}
	if console.GetError() != "" {
		return fmt.Errorf("opening VM console: %s", console.GetError())
	}
	if !console.GetAvailable() {
		return fmt.Errorf("VM console is not available")
	}
	tcpConn, err := net.DialTimeout("tcp", fmt.Sprintf("%s:%d", console.GetHost(), console.GetPort()), 5*time.Second)
	if err != nil {
		return fmt.Errorf("dialing VM console: %w", err)
	}
	defer tcpConn.Close()

	fromClient := make(chan error, 1)
	go func() {
		defer tcpConn.Close()
		for {
			frame, recvErr := stream.Recv()
			if recvErr != nil {
				fromClient <- recvErr
				return
			}
			if len(frame.GetData()) == 0 {
				fromClient <- fmt.Errorf("console tunnel accepts only data after open")
				return
			}
			if _, writeErr := tcpConn.Write(frame.GetData()); writeErr != nil {
				fromClient <- writeErr
				return
			}
		}
	}()

	buf := make([]byte, 32*1024)
	for {
		n, readErr := tcpConn.Read(buf)
		if n > 0 {
			if sendErr := stream.Send(&rpcpb.VMConsoleTunnelFrame{Payload: &rpcpb.VMConsoleTunnelFrame_Data{Data: buf[:n]}}); sendErr != nil {
				return sendErr
			}
		}
		if readErr != nil {
			if readErr == io.EOF {
				return nil
			}
			return readErr
		}
		select {
		case recvErr := <-fromClient:
			if recvErr == io.EOF {
				return nil
			}
			return recvErr
		default:
		}
	}
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
// node - see GetVMSerialLogResponse's doc comment. Mirrors
// GetVMConsole's own leader-forwarding exactly (a real bug, caught
// live: this RPC never got the fix GetVMConsole did - see "Allow local
// VM console on follower"/"Forward VM console lookups to leader" -
// so a follower node's own GetVM lookup here previously surfaced a raw
// "raft: this node is not the leader" error instead of forwarding to
// the leader to resolve the VM's owner first).
func (s *Server) GetVMSerialLog(ctx context.Context, req *rpcpb.GetVMSerialLogRequest) (*rpcpb.GetVMSerialLogResponse, error) {
	resp, err := s.raft.GetVM(ctx, req.GetId())
	if err != nil {
		return &rpcpb.GetVMSerialLogResponse{Error: err.Error()}, nil
	}
	if resp.GetError() != "" {
		if s.peers != nil && resp.GetLeaderHint() != "" {
			var fwd *rpcpb.GetVMResponse
			var ferr error
			fwd, ferr = s.peers.GetVM(ctx, s.peerManagerdAddr(resp.GetLeaderHint()), req.GetId())
			if ferr == nil && fwd.GetError() == "" && fwd.GetFound() && fwd.GetVm().GetNodeId() == s.nodeID {
				resp = &internalpb.GetVMResponse{Vm: toInternalVM(fwd.GetVm()), Found: true}
			} else {
				return &rpcpb.GetVMSerialLogResponse{Error: augmentForwardError(resp.GetError(), resp.GetLeaderHint(), ferr)}, nil
			}
		}
		if resp.GetError() != "" {
			return &rpcpb.GetVMSerialLogResponse{Error: resp.GetError()}, nil
		}
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
	resp := &rpcpb.GetNodeConfigResponse{
		RpcAddr:       effectiveRPCAddr(cfg.RPCAddr),
		Uplink:        cfg.Uplink,
		NatUplink:     cfg.NATUplink,
		DhcpDnsServer: cfg.DNSServer,
		JailEnabled:   cfg.JailEnabled,

		ZfsBase:       cfg.ZFSBase,
		BhyvePrefix:   cfg.BhyvePrefix,
		IsoDir:        cfg.ISODir,
		JailPrefix:    cfg.JailPrefix,
		JailMountBase: cfg.JailMountBase,

		ReconcileInterval:           durationString(cfg.ReconcileInterval),
		AssumptionCheckInterval:     durationString(cfg.AssumptionCheckInterval),
		AssumptionHeartbeatInterval: durationString(cfg.AssumptionHeartbeatInterval),
		AssumptionStaleAfter:        durationString(cfg.AssumptionStaleAfter),
		AssumptionRunDeadline:       durationString(cfg.AssumptionRunDeadline),
		AssumptionHistoryMaxAge:     durationString(cfg.AssumptionHistoryMaxAge),
		AssumptionHistoryLimit:      int32(cfg.AssumptionHistoryLimit),

		BhyveBootrom:   cfg.BhyveBootROM,
		BhyveBridge:    cfg.BhyveBridge,
		DiskSizeMb:     cfg.DiskSizeMB,
		JailDiskSizeMb: cfg.JailDiskSizeMB,

		HastEnabled: cfg.HASTEnabled,
		PeerTls:     cfg.PeerTLS,

		PeerManagerdPort:   cfg.PeerManagerdPort,
		PeerTlsHostnameMap: cfg.PeerTLSHostnameMap,
		PeerTlsCa:          cfg.PeerTLSCA,
		KnownPeerAddresses: cfg.KnownPeerAddresses,

		// Secrets are never returned - only whether one is set. See
		// UpdateNodeConfigRequest's own doc comment for how to set or
		// clear one.
		PeerApiKeySet: cfg.PeerAPIKey != "",
		RaftdTokenSet: cfg.RaftdToken != "",

		TlsCert:    cfg.TLSCert,
		TlsKey:     cfg.TLSKey,
		PamService: cfg.PAMService,

		CloudflareTokenFile:             cfg.CloudflareTokenFile,
		CloudflareZoneId:                cfg.CloudflareZoneID,
		CloudflareTunnelId:              cfg.CloudflareTunnelID,
		CloudflareTunnelCredentialsFile: cfg.CloudflareTunnelCredentialsFile,
		OriginCaTokenFile:               cfg.OriginCATokenFile,
		OriginCaDirectory:               cfg.OriginCADirectory,
		OriginCaRenewalCheckInterval:    durationString(cfg.OriginCARenewalCheckInterval),
	}
	if historyStore, ok := s.nodeConfig.(nodeConfigHistoryStore); ok {
		changes, err := historyStore.History()
		if err != nil {
			return &rpcpb.GetNodeConfigResponse{Error: "reading configuration history: " + err.Error()}, nil
		}
		for _, change := range changes {
			resp.ConfigChanges = append(resp.ConfigChanges, &rpcpb.ConfigChange{
				Field: change.Field, Previous: change.Previous, Current: change.Current,
				Origin: string(change.Origin), Rationale: change.Rationale,
				Evidence: change.Evidence, ChangedAtUnix: change.ChangedAt.Unix(), Attested: change.Attested,
			})
		}
	}
	if s.listNetworkInterfaces != nil {
		if interfaces, err := s.listNetworkInterfaces(); err == nil {
			resp.AvailableInterfaces = make([]*rpcpb.NetworkInterface, 0, len(interfaces))
			for _, iface := range interfaces {
				resp.AvailableInterfaces = append(resp.AvailableInterfaces, &rpcpb.NetworkInterface{
					Name: iface.Name, Up: iface.Up, Addresses: append([]string(nil), iface.Addresses...),
				})
			}
		} else {
			resp.InterfaceInventoryError = err.Error()
		}
	}
	return resp, nil
}

// effectiveRPCAddr mirrors cmd/managerd's startup default so a newly
// installed host reports the endpoint it will actually use, rather than an
// empty persisted value that only means "use the default".
func effectiveRPCAddr(value string) string {
	if value == "" {
		return "127.0.0.1:17700"
	}
	return value
}

// UpdateManagerdBindAddress changes only managerd's own external RPC bind
// address. It deliberately does not restart managerd: an endpoint change can
// sever the connection carrying this response, and managerd's restart
// guardrail remains the single, explicit action that makes the saved value
// live.
func (s *Server) UpdateManagerdBindAddress(_ context.Context, req *rpcpb.UpdateManagerdBindAddressRequest) (*rpcpb.UpdateManagerdBindAddressResponse, error) {
	if s.nodeConfig == nil {
		return &rpcpb.UpdateManagerdBindAddressResponse{Error: "this node has no node-config store configured"}, nil
	}
	addr := strings.TrimSpace(req.GetRpcAddr())
	if err := s.validateLocalBindAddress(addr); err != nil {
		return &rpcpb.UpdateManagerdBindAddressResponse{Error: err.Error()}, nil
	}
	current, err := s.nodeConfig.Load()
	if err != nil {
		return &rpcpb.UpdateManagerdBindAddressResponse{Error: err.Error()}, nil
	}
	current.RPCAddr = addr
	origin := nodeconfig.ChangeOrigin(req.GetChangeOrigin())
	if origin == "" {
		origin = nodeconfig.OriginOperator
	}
	if err := nodeconfig.ValidateOrigin(origin); err != nil {
		return &rpcpb.UpdateManagerdBindAddressResponse{Error: err.Error()}, nil
	}
	if len(req.GetChangeRationale()) > 2048 || len(req.GetChangeEvidence()) > 2048 {
		return &rpcpb.UpdateManagerdBindAddressResponse{Error: "change rationale and evidence must be at most 2048 characters"}, nil
	}
	if historyStore, ok := s.nodeConfig.(nodeConfigHistoryStore); ok {
		if err := historyStore.SaveWithHistory(current, origin, req.GetChangeRationale(), req.GetChangeEvidence()); err != nil {
			return &rpcpb.UpdateManagerdBindAddressResponse{Error: err.Error()}, nil
		}
	} else if err := s.nodeConfig.Save(current); err != nil {
		return &rpcpb.UpdateManagerdBindAddressResponse{Error: err.Error()}, nil
	}
	return &rpcpb.UpdateManagerdBindAddressResponse{RestartRequired: true}, nil
}

// validateLocalBindAddress rejects a syntactically valid but remote endpoint
// before it reaches managerd.json. The browser offers the same host-local
// addresses from GetNodeConfig's interface inventory, but RPC callers are not
// trusted to have used that UI. Wildcard and loopback are retained for the
// established single-node/default configurations.
func (s *Server) validateLocalBindAddress(value string) error {
	host, port, err := net.SplitHostPort(value)
	if err != nil || host == "" {
		return fmt.Errorf("rpc_addr must be an address and port, such as 10.90.0.12:17700")
	}
	if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
		return fmt.Errorf("rpc_addr has invalid port %q", port)
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return fmt.Errorf("rpc_addr host %q must be a numeric address assigned to this Comb", host)
	}
	if ip.IsUnspecified() || ip.IsLoopback() {
		return nil
	}
	if s.listNetworkInterfaces == nil {
		return fmt.Errorf("cannot validate rpc_addr against this Comb's interface inventory")
	}
	interfaces, err := s.listNetworkInterfaces()
	if err != nil {
		return fmt.Errorf("cannot validate rpc_addr against this Comb's interface inventory: %w", err)
	}
	for _, iface := range interfaces {
		for _, raw := range iface.Addresses {
			candidate, _, err := net.ParseCIDR(raw)
			if err == nil && candidate.Equal(ip) {
				return nil
			}
		}
	}
	return fmt.Errorf("rpc_addr host %q is not assigned to this Comb", host)
}

// durationString formats a time.Duration for GetNodeConfigResponse -
// the zero value renders as "" (never set/using the startup flag),
// not Go's own "0s", so the Machine page's input can stay blank rather
// than showing a misleadingly specific value nobody actually saved.
func durationString(d time.Duration) string {
	if d == 0 {
		return ""
	}
	return d.String()
}

// UpdateNodeConfig implements rpcpb.ManagerServiceServer - persists new
// local settings, replacing the file in full (matching
// nodeconfig.Manager.Save's own doc comment) rather than merging, so a
// caller intending to change only one field must send both (the web UI
// does this by always resending every current value alongside whatever
// the operator actually changed - see internal/frontend/machine.go).
// Takes effect on this node's next managerd restart, not live - see
// ADR-0049/ADR-0070.
//
// Duration fields arrive as plain strings (e.g. "30s") and are parsed
// here, before ever reaching nodeconfig.Save, so a malformed value is
// rejected immediately with a clear error rather than being persisted
// and only failing the next time managerd actually starts.
//
// peer_api_key/raftd_token are write-only (see
// UpdateNodeConfigRequest's own doc comment): an empty value here
// leaves the currently-saved secret untouched by reloading it from the
// existing config first, rather than accidentally clearing it just
// because the web form's own field was left blank on a save that was
// only touching some other setting.
func (s *Server) UpdateNodeConfig(_ context.Context, req *rpcpb.UpdateNodeConfigRequest) (*rpcpb.UpdateNodeConfigResponse, error) {
	if s.nodeConfig == nil {
		return &rpcpb.UpdateNodeConfigResponse{Error: "this node has no node-config store configured"}, nil
	}
	current, err := s.nodeConfig.Load()
	if err != nil {
		return &rpcpb.UpdateNodeConfigResponse{Error: err.Error()}, nil
	}

	reconcileInterval, err := parseOptionalDuration("reconcile_interval", req.GetReconcileInterval())
	if err != nil {
		return &rpcpb.UpdateNodeConfigResponse{Error: err.Error()}, nil
	}
	assumptionCheckInterval, err := parseOptionalDuration("assumption_check_interval", req.GetAssumptionCheckInterval())
	if err != nil {
		return &rpcpb.UpdateNodeConfigResponse{Error: err.Error()}, nil
	}
	assumptionHeartbeatInterval, err := parseOptionalDuration("assumption_heartbeat_interval", req.GetAssumptionHeartbeatInterval())
	if err != nil {
		return &rpcpb.UpdateNodeConfigResponse{Error: err.Error()}, nil
	}
	assumptionStaleAfter, err := parseOptionalDuration("assumption_stale_after", req.GetAssumptionStaleAfter())
	if err != nil {
		return &rpcpb.UpdateNodeConfigResponse{Error: err.Error()}, nil
	}
	assumptionRunDeadline, err := parseOptionalDuration("assumption_run_deadline", req.GetAssumptionRunDeadline())
	if err != nil {
		return &rpcpb.UpdateNodeConfigResponse{Error: err.Error()}, nil
	}
	assumptionHistoryMaxAge, err := parseOptionalDuration("assumption_history_max_age", req.GetAssumptionHistoryMaxAge())
	if err != nil {
		return &rpcpb.UpdateNodeConfigResponse{Error: err.Error()}, nil
	}
	originCARenewalCheckInterval, err := parseOptionalDuration("origin_ca_renewal_check_interval", req.GetOriginCaRenewalCheckInterval())
	if err != nil {
		return &rpcpb.UpdateNodeConfigResponse{Error: err.Error()}, nil
	}

	peerAPIKey := current.PeerAPIKey
	if req.GetClearPeerApiKey() {
		peerAPIKey = ""
	} else if req.GetPeerApiKey() != "" {
		peerAPIKey = req.GetPeerApiKey()
	}
	raftdToken := current.RaftdToken
	if req.GetClearRaftdToken() {
		raftdToken = ""
	} else if req.GetRaftdToken() != "" {
		raftdToken = req.GetRaftdToken()
	}

	cfg := nodeconfig.Config{
		// NodeID/RPCAddr/RaftdSocket (ADR-0100) are deliberately never
		// part of UpdateNodeConfigRequest - see nodeconfig's own
		// package doc comment for why they stay file-only, hand-edit
		// only. Because they have no proto field to read from req,
		// they must be explicitly carried over from current here, or
		// every UpdateNodeConfig call - not just ones touching these
		// fields - would silently zero them out, since cfg below is
		// otherwise built fresh from req rather than merged onto
		// current.
		NodeID:      current.NodeID,
		RPCAddr:     current.RPCAddr,
		RaftdSocket: current.RaftdSocket,

		Uplink:        req.GetUplink(),
		NATUplink:     req.GetNatUplink(),
		DNSServer:     req.GetDhcpDnsServer(),
		JailEnabled:   req.JailEnabled,
		ZFSBase:       req.GetZfsBase(),
		BhyvePrefix:   req.GetBhyvePrefix(),
		ISODir:        req.GetIsoDir(),
		JailPrefix:    req.GetJailPrefix(),
		JailMountBase: req.GetJailMountBase(),

		ReconcileInterval:           reconcileInterval,
		AssumptionCheckInterval:     assumptionCheckInterval,
		AssumptionHeartbeatInterval: assumptionHeartbeatInterval,
		AssumptionStaleAfter:        assumptionStaleAfter,
		AssumptionRunDeadline:       assumptionRunDeadline,
		AssumptionHistoryMaxAge:     assumptionHistoryMaxAge,
		AssumptionHistoryLimit:      int(req.GetAssumptionHistoryLimit()),

		BhyveBootROM:   req.GetBhyveBootrom(),
		BhyveBridge:    req.GetBhyveBridge(),
		DiskSizeMB:     req.GetDiskSizeMb(),
		JailDiskSizeMB: req.GetJailDiskSizeMb(),

		HASTEnabled: req.HastEnabled,
		PeerTLS:     req.PeerTls,

		PeerAPIKey:         peerAPIKey,
		PeerManagerdPort:   req.GetPeerManagerdPort(),
		PeerTLSHostnameMap: req.GetPeerTlsHostnameMap(),
		PeerTLSCA:          req.GetPeerTlsCa(),
		KnownPeerAddresses: req.GetKnownPeerAddresses(),

		TLSCert:    req.GetTlsCert(),
		TLSKey:     req.GetTlsKey(),
		PAMService: req.GetPamService(),

		CloudflareTokenFile:             req.GetCloudflareTokenFile(),
		CloudflareZoneID:                req.GetCloudflareZoneId(),
		CloudflareTunnelID:              req.GetCloudflareTunnelId(),
		CloudflareTunnelCredentialsFile: req.GetCloudflareTunnelCredentialsFile(),
		OriginCATokenFile:               req.GetOriginCaTokenFile(),
		OriginCADirectory:               req.GetOriginCaDirectory(),
		OriginCARenewalCheckInterval:    originCARenewalCheckInterval,

		RaftdToken: raftdToken,
	}
	// pam_service requires tls_cert/tls_key (ADR-0087): a login
	// password must not travel to this RPC over a plaintext channel.
	// Checked here too, not just at managerd startup, so a bad
	// combination is rejected immediately rather than only failing the
	// next time managerd restarts.
	if cfg.PAMService != "" && (cfg.TLSCert == "" || cfg.TLSKey == "") {
		return &rpcpb.UpdateNodeConfigResponse{Error: "pam_service requires tls_cert/tls_key to also be set - a login password must not travel to this RPC over a plaintext channel"}, nil
	}
	origin := nodeconfig.ChangeOrigin(req.GetChangeOrigin())
	if origin == "" {
		origin = nodeconfig.OriginOperator
	}
	if err := nodeconfig.ValidateOrigin(origin); err != nil {
		return &rpcpb.UpdateNodeConfigResponse{Error: err.Error()}, nil
	}
	if len(req.GetChangeRationale()) > 2048 || len(req.GetChangeEvidence()) > 2048 {
		return &rpcpb.UpdateNodeConfigResponse{Error: "change rationale and evidence must be at most 2048 characters"}, nil
	}
	if req.GetAnnotateField() != "" {
		historyStore, ok := s.nodeConfig.(nodeConfigHistoryStore)
		if !ok {
			return &rpcpb.UpdateNodeConfigResponse{Error: "this node does not support configuration history attestations"}, nil
		}
		if err := historyStore.RecordCurrent(req.GetAnnotateField(), origin, req.GetChangeRationale(), req.GetChangeEvidence()); err != nil {
			return &rpcpb.UpdateNodeConfigResponse{Error: err.Error()}, nil
		}
		return &rpcpb.UpdateNodeConfigResponse{}, nil
	}
	if historyStore, ok := s.nodeConfig.(nodeConfigHistoryStore); ok {
		if err := historyStore.SaveWithHistory(cfg, origin, req.GetChangeRationale(), req.GetChangeEvidence()); err != nil {
			return &rpcpb.UpdateNodeConfigResponse{Error: err.Error()}, nil
		}
	} else if err := s.nodeConfig.Save(cfg); err != nil {
		return &rpcpb.UpdateNodeConfigResponse{Error: err.Error()}, nil
	}
	return &rpcpb.UpdateNodeConfigResponse{}, nil
}

// parseOptionalDuration parses value with time.ParseDuration unless
// it's empty (meaning "leave this at its zero value / not set"),
// naming field in any error so a malformed Machine Configuration form
// field is easy to identify.
func parseOptionalDuration(field, value string) (time.Duration, error) {
	if value == "" {
		return 0, nil
	}
	d, err := time.ParseDuration(value)
	if err != nil {
		return 0, fmt.Errorf("invalid %s %q: %w", field, value, err)
	}
	return d, nil
}

// GetFrontendConfig implements rpcpb.ManagerServiceServer - reports
// the co-located frontend's own local settings (ADR-0102), never
// routed through raft. See internal/frontendconfig.
func (s *Server) GetFrontendConfig(_ context.Context, _ *rpcpb.GetFrontendConfigRequest) (*rpcpb.GetFrontendConfigResponse, error) {
	if s.frontendConfig == nil {
		return &rpcpb.GetFrontendConfigResponse{Error: "this node has no frontend-config store configured"}, nil
	}
	cfg, err := s.frontendConfig.Load()
	if err != nil {
		return &rpcpb.GetFrontendConfigResponse{Error: err.Error()}, nil
	}
	return &rpcpb.GetFrontendConfigResponse{
		ManagerAddr:          cfg.ManagerAddr,
		HttpAddr:             cfg.HTTPAddr,
		ManagerTls:           cfg.ManagerTLS,
		ManagerTlsCa:         cfg.ManagerTLSCA,
		ManagerTlsServerName: cfg.ManagerTLSServerName,
		TlsCert:              cfg.TLSCert,
		TlsKey:               cfg.TLSKey,
		PeerTls:              cfg.PeerTLS,
		PeerTlsCa:            cfg.PeerTLSCA,
		PeerHostnameSuffix:   cfg.PeerHostnameSuffix,
		PeerManagerPort:      cfg.PeerManagerPort,
		// manager_api_key is never returned - only whether one is set.
		// See UpdateFrontendConfigRequest's own doc comment for how to
		// set or clear one.
		ManagerApiKeySet: cfg.ManagerAPIKey != "",
	}, nil
}

// UpdateFrontendConfig implements rpcpb.ManagerServiceServer -
// persists new settings for the co-located frontend, replacing the
// file in full (matching frontendconfig.Manager.Save's own doc
// comment) rather than merging - same posture as UpdateNodeConfig.
// Since every frontendconfig.Config field is fully RPC-editable,
// there's nothing to carry over from current except the secret. On
// success, schedules a restart of frontend to apply the change - see
// UpdateFrontendConfigResponse's own doc comment in the proto.
func (s *Server) UpdateFrontendConfig(_ context.Context, req *rpcpb.UpdateFrontendConfigRequest) (*rpcpb.UpdateFrontendConfigResponse, error) {
	if s.frontendConfig == nil {
		return &rpcpb.UpdateFrontendConfigResponse{Error: "this node has no frontend-config store configured"}, nil
	}
	current, err := s.frontendConfig.Load()
	if err != nil {
		return &rpcpb.UpdateFrontendConfigResponse{Error: err.Error()}, nil
	}
	apiKey := current.ManagerAPIKey
	if req.GetClearManagerApiKey() {
		apiKey = ""
	} else if req.GetManagerApiKey() != "" {
		apiKey = req.GetManagerApiKey()
	}
	cfg := frontendconfig.Config{
		ManagerAddr:          req.GetManagerAddr(),
		HTTPAddr:             req.GetHttpAddr(),
		ManagerTLS:           req.GetManagerTls(),
		ManagerTLSCA:         req.GetManagerTlsCa(),
		ManagerTLSServerName: req.GetManagerTlsServerName(),
		TLSCert:              req.GetTlsCert(),
		TLSKey:               req.GetTlsKey(),
		PeerTLS:              req.GetPeerTls(),
		PeerTLSCA:            req.GetPeerTlsCa(),
		PeerHostnameSuffix:   req.GetPeerHostnameSuffix(),
		PeerManagerPort:      req.GetPeerManagerPort(),
		ManagerAPIKey:        apiKey,
	}
	if err := s.frontendConfig.Save(cfg); err != nil {
		return &rpcpb.UpdateFrontendConfigResponse{Error: err.Error()}, nil
	}
	s.scheduleServiceRestart("apiary_frontend")
	return &rpcpb.UpdateFrontendConfigResponse{Scheduled: true}, nil
}

// GetRestshimdConfig implements rpcpb.ManagerServiceServer - mirrors
// GetFrontendConfig for the co-located restshimd (ADR-0102). No secret
// fields to redact - restshimdconfig.Config has none.
func (s *Server) GetRestshimdConfig(_ context.Context, _ *rpcpb.GetRestshimdConfigRequest) (*rpcpb.GetRestshimdConfigResponse, error) {
	if s.restshimdConfig == nil {
		return &rpcpb.GetRestshimdConfigResponse{Error: "this node has no restshimd-config store configured"}, nil
	}
	cfg, err := s.restshimdConfig.Load()
	if err != nil {
		return &rpcpb.GetRestshimdConfigResponse{Error: err.Error()}, nil
	}
	return &rpcpb.GetRestshimdConfigResponse{
		ManagerAddr:          cfg.ManagerAddr,
		HttpAddr:             cfg.HTTPAddr,
		ManagerTls:           cfg.ManagerTLS,
		ManagerTlsCa:         cfg.ManagerTLSCA,
		ManagerTlsServerName: cfg.ManagerTLSServerName,
		TlsCert:              cfg.TLSCert,
		TlsKey:               cfg.TLSKey,
	}, nil
}

// UpdateRestshimdConfig implements rpcpb.ManagerServiceServer - mirrors
// UpdateFrontendConfig for the co-located restshimd (ADR-0102), minus
// any secret handling (restshimdconfig.Config has none) - the simplest
// of the six new handlers. Schedules a restshimd restart on success.
func (s *Server) UpdateRestshimdConfig(_ context.Context, req *rpcpb.UpdateRestshimdConfigRequest) (*rpcpb.UpdateRestshimdConfigResponse, error) {
	if s.restshimdConfig == nil {
		return &rpcpb.UpdateRestshimdConfigResponse{Error: "this node has no restshimd-config store configured"}, nil
	}
	cfg := restshimdconfig.Config{
		ManagerAddr:          req.GetManagerAddr(),
		HTTPAddr:             req.GetHttpAddr(),
		ManagerTLS:           req.GetManagerTls(),
		ManagerTLSCA:         req.GetManagerTlsCa(),
		ManagerTLSServerName: req.GetManagerTlsServerName(),
		TLSCert:              req.GetTlsCert(),
		TLSKey:               req.GetTlsKey(),
	}
	if err := s.restshimdConfig.Save(cfg); err != nil {
		return &rpcpb.UpdateRestshimdConfigResponse{Error: err.Error()}, nil
	}
	s.scheduleServiceRestart("apiary_restshimd")
	return &rpcpb.UpdateRestshimdConfigResponse{Scheduled: true}, nil
}

// GetRaftdConfig implements rpcpb.ManagerServiceServer - read-only
// display of the co-located raftd's own settings (ADR-0102). There is
// deliberately no UpdateRaftdConfig: a 2026-09-15 audit found that
// per-host editing of internal_token or raft TLS material is unsafe
// for a consensus-critical daemon without a real coordinated rotation
// workflow (internal_token must match managerd's own separately-
// configured raftd_token for RaftInternal auth to keep working;
// changing raft TLS material on one voter and restarting it can
// isolate that voter and lose quorum) - both out of scope here. raftd's
// config stays entirely hand-edit-the-file-and-restart-only; this RPC
// exists purely so the Machine page can show current values for
// context.
func (s *Server) GetRaftdConfig(_ context.Context, _ *rpcpb.GetRaftdConfigRequest) (*rpcpb.GetRaftdConfigResponse, error) {
	if s.raftdConfig == nil {
		return &rpcpb.GetRaftdConfigResponse{Error: "this node has no raftd-config store configured"}, nil
	}
	cfg, err := s.raftdConfig.Load()
	if err != nil {
		return &rpcpb.GetRaftdConfigResponse{Error: err.Error()}, nil
	}
	return &rpcpb.GetRaftdConfigResponse{
		DataDir:  cfg.DataDir,
		Socket:   cfg.Socket,
		RaftBind: cfg.RaftBind,

		RaftTlsCert: cfg.RaftTLSCert,
		RaftTlsKey:  cfg.RaftTLSKey,
		RaftTlsCa:   cfg.RaftTLSCA,

		// internal_token is never returned - only whether one is set.
		InternalTokenSet: cfg.InternalToken != "",
	}, nil
}

// scheduleServiceRestart is UpdateFrontendConfig/UpdateRestshimdConfig's
// shared internal restart-to-apply mechanism (ADR-0102) - the same
// underlying s.services.Restart call and 250ms flush-first delay
// RestartNodeService's own handler already uses, called directly
// rather than through the RestartNodeService RPC itself since name is
// always one of the two hardcoded, already-known-restartable values
// here, never caller-supplied. A no-op if this node has no service
// controller configured, matching RestartNodeService's own posture.
func (s *Server) scheduleServiceRestart(name string) {
	if s.services == nil {
		return
	}
	go func() {
		time.Sleep(250 * time.Millisecond)
		if err := s.services.Restart(context.Background(), name); err != nil {
			fmt.Fprintf(os.Stderr, "apiary: restarting %s: %v\n", name, err)
		}
	}()
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

// CreateVMSnapshot implements rpcpb.ManagerServiceServer (ADR-0090) -
// takes a ZFS snapshot of a VM's own dataset (which holds its
// disk.img), the same "physical, per-node, never routed through raft"
// posture as SetDatasetQuota above.
func (s *Server) CreateVMSnapshot(ctx context.Context, req *rpcpb.CreateVMSnapshotRequest) (*rpcpb.CreateVMSnapshotResponse, error) {
	if s.zfs == nil {
		return &rpcpb.CreateVMSnapshotResponse{Error: "this node has no ZFS support configured"}, nil
	}
	if req.GetSnapshotName() == "" {
		return &rpcpb.CreateVMSnapshotResponse{Error: "snapshot_name must be set"}, nil
	}
	if err := s.zfs.CreateSnapshot(ctx, req.GetId()+"@"+req.GetSnapshotName()); err != nil {
		return &rpcpb.CreateVMSnapshotResponse{Error: err.Error()}, nil
	}
	return &rpcpb.CreateVMSnapshotResponse{}, nil
}

// ListVMSnapshots implements rpcpb.ManagerServiceServer (ADR-0090).
func (s *Server) ListVMSnapshots(ctx context.Context, req *rpcpb.ListVMSnapshotsRequest) (*rpcpb.ListVMSnapshotsResponse, error) {
	if s.zfs == nil {
		return &rpcpb.ListVMSnapshotsResponse{}, nil
	}
	names, err := s.zfs.ListSnapshots(ctx, req.GetId())
	if err != nil {
		return &rpcpb.ListVMSnapshotsResponse{Error: err.Error()}, nil
	}
	return &rpcpb.ListVMSnapshotsResponse{SnapshotNames: names}, nil
}

// RestoreVMSnapshot implements rpcpb.ManagerServiceServer (ADR-0090) -
// rolls a VM's dataset back to a named snapshot. Refuses while the VM
// is currently desired to be running: rolling back a live disk.img out
// from under a running bhyve process risks silent guest-visible
// corruption, since ZFS has no notion of coordinating with a process
// that already has the file open. This check is best-effort (no raft
// client configured, or a failed/leader-forwarded GetVM lookup, both
// just skip it rather than block the restore) - the real, disclosed
// safety boundary here is the UI's own copy telling the operator to
// stop the VM first, not this check, see ADR-0090.
func (s *Server) RestoreVMSnapshot(ctx context.Context, req *rpcpb.RestoreVMSnapshotRequest) (*rpcpb.RestoreVMSnapshotResponse, error) {
	if s.zfs == nil {
		return &rpcpb.RestoreVMSnapshotResponse{Error: "this node has no ZFS support configured"}, nil
	}
	if s.raft != nil {
		if vmResp, err := s.GetVM(ctx, &rpcpb.GetVMRequest{Id: req.GetId()}); err == nil && vmResp.GetFound() && vmResp.GetVm().GetDesiredState() == rpcpb.VMState_VM_STATE_RUNNING {
			return &rpcpb.RestoreVMSnapshotResponse{Error: fmt.Sprintf("VM %q must be stopped before restoring a snapshot", req.GetId())}, nil
		}
	}
	if err := s.zfs.RollbackSnapshot(ctx, req.GetId()+"@"+req.GetSnapshotName()); err != nil {
		return &rpcpb.RestoreVMSnapshotResponse{Error: err.Error()}, nil
	}
	return &rpcpb.RestoreVMSnapshotResponse{}, nil
}

// DeleteVMSnapshot implements rpcpb.ManagerServiceServer (ADR-0090).
func (s *Server) DeleteVMSnapshot(ctx context.Context, req *rpcpb.DeleteVMSnapshotRequest) (*rpcpb.DeleteVMSnapshotResponse, error) {
	if s.zfs == nil {
		return &rpcpb.DeleteVMSnapshotResponse{Error: "this node has no ZFS support configured"}, nil
	}
	if err := s.zfs.DestroySnapshot(ctx, req.GetId()+"@"+req.GetSnapshotName()); err != nil {
		return &rpcpb.DeleteVMSnapshotResponse{Error: err.Error()}, nil
	}
	return &rpcpb.DeleteVMSnapshotResponse{}, nil
}

// hastOrphanDatasetPrefix/hastOrphanResourceType parse a top-level ZFS
// dataset name back into a HAST resource's type and bare id, mirroring
// internal/cluster/hast.go's hastProviderDatasetName("vm-"+id) and
// jailHASTResourceName ("jail-"+id) naming exactly - kept as a local,
// duplicated constant rather than an exported cross-package helper
// since it's a two-line string convention, not shared logic.
func hastOrphanResourceType(datasetName string) (kind, id string, ok bool) {
	switch {
	case strings.HasPrefix(datasetName, "hast-vm-"):
		return "vm", strings.TrimPrefix(datasetName, "hast-vm-"), true
	case strings.HasPrefix(datasetName, "hast-jail-"):
		return "jail", strings.TrimPrefix(datasetName, "hast-jail-"), true
	default:
		return "", "", false
	}
}

// hastOrphanDatasetName is hastOrphanResourceType's inverse. Validates id
// itself before ever building the dataset name string - not just trusting
// zfs.Manager.path()'s own generic per-segment traversal guard further
// downstream (this project's own ADR-0067 doctrine: validate at the point a
// value is first accepted, don't rely on a later layer to catch what should
// never have been constructed in the first place). A "/" in id would let a
// caller-supplied resource_id build a dataset path deeper than this
// package's own flat hast-vm-*/hast-jail-* naming convention ever produces -
// not reachable today (nothing in this codebase creates a nested dataset
// under Base, and path() already rejects a literal ".." segment), but this
// closes the gap outright rather than depending on that invariant holding.
func hastOrphanDatasetName(kind, id string) (string, error) {
	if id == "" {
		return "", fmt.Errorf("resource_id must not be empty")
	}
	if strings.ContainsAny(id, "/\\") {
		return "", fmt.Errorf("resource_id %q must not contain a path separator", id)
	}
	switch kind {
	case "vm":
		return "hast-vm-" + id, nil
	case "jail":
		return "hast-jail-" + id, nil
	default:
		return "", fmt.Errorf("resource_type must be \"vm\" or \"jail\", got %q", kind)
	}
}

// hastResourceActiveOnThisNode reports whether this node is still the
// owner or replica of the given VM/jail id - i.e. whether its HAST
// provider dataset is genuinely still in use, not orphaned. Re-run at
// CleanupOrphanedHASTResource time (not just trusted from an earlier
// List call) so a resource that became active again in between - a new
// VM/jail created with the same id, however unlikely - is never
// destroyed out from under it.
func (s *Server) hastResourceActiveOnThisNode(ctx context.Context, kind, id string) (bool, error) {
	switch kind {
	case "vm":
		resp, err := s.raft.ListVMsLocal(ctx)
		if err != nil {
			return false, err
		}
		for _, vm := range resp.GetVms() {
			if vm.GetId() == id && (vm.GetNodeId() == s.nodeID || vm.GetReplicaNodeId() == s.nodeID) {
				return true, nil
			}
		}
		return false, nil
	case "jail":
		resp, err := s.raft.ListJailsLocal(ctx)
		if err != nil {
			return false, err
		}
		for _, j := range resp.GetJails() {
			if j.GetId() == id && (j.GetNodeId() == s.nodeID || j.GetReplicaNodeId() == s.nodeID) {
				return true, nil
			}
		}
		return false, nil
	default:
		return false, fmt.Errorf("resource_type must be \"vm\" or \"jail\", got %q", kind)
	}
}

// ListOrphanedHASTResources implements rpcpb.ManagerServiceServer - see
// its own doc comment on ManagerService for the full gap this closes
// (ADR-0026). Local-only, never routed through raft: this node's own
// hast-vm-*/hast-jail-* provider datasets are physical, per-node state,
// exactly like ISOs/HostStats.
func (s *Server) ListOrphanedHASTResources(ctx context.Context, _ *rpcpb.ListOrphanedHASTResourcesRequest) (*rpcpb.ListOrphanedHASTResourcesResponse, error) {
	if s.zfs == nil {
		return &rpcpb.ListOrphanedHASTResourcesResponse{Error: "this node has no ZFS support configured"}, nil
	}
	datasets, err := s.zfs.ListDatasets(ctx)
	if err != nil {
		return &rpcpb.ListOrphanedHASTResourcesResponse{Error: err.Error()}, nil
	}

	vmResp, err := s.raft.ListVMsLocal(ctx)
	if err != nil {
		return &rpcpb.ListOrphanedHASTResourcesResponse{Error: err.Error()}, nil
	}
	jailResp, err := s.raft.ListJailsLocal(ctx)
	if err != nil {
		return &rpcpb.ListOrphanedHASTResourcesResponse{Error: err.Error()}, nil
	}
	activeVM := make(map[string]bool, len(vmResp.GetVms()))
	for _, vm := range vmResp.GetVms() {
		if vm.GetNodeId() == s.nodeID || vm.GetReplicaNodeId() == s.nodeID {
			activeVM[vm.GetId()] = true
		}
	}
	activeJail := make(map[string]bool, len(jailResp.GetJails()))
	for _, j := range jailResp.GetJails() {
		if j.GetNodeId() == s.nodeID || j.GetReplicaNodeId() == s.nodeID {
			activeJail[j.GetId()] = true
		}
	}

	var orphans []*rpcpb.OrphanedHASTResource
	for _, ds := range datasets {
		// ListDatasets is recursive; a hast-*  dataset created by this
		// codebase is always a direct, childless leaf, so anything with
		// a "/" here belongs to some other dataset's descendant, not a
		// HAST provider root itself.
		if strings.Contains(ds, "/") {
			continue
		}
		kind, id, ok := hastOrphanResourceType(ds)
		if !ok {
			continue
		}
		if (kind == "vm" && activeVM[id]) || (kind == "jail" && activeJail[id]) {
			continue
		}
		usedSize, _ := s.zfs.GetProperty(ctx, ds, "used") // best-effort context only
		orphans = append(orphans, &rpcpb.OrphanedHASTResource{
			ResourceId:   id,
			ResourceType: kind,
			DatasetName:  ds,
			UsedSize:     usedSize,
		})
	}
	return &rpcpb.ListOrphanedHASTResourcesResponse{Resources: orphans}, nil
}

// CleanupOrphanedHASTResource implements rpcpb.ManagerServiceServer -
// the explicit, human-triggered destructive half of the pair (see
// ListOrphanedHASTResources's doc comment on ManagerService). Re-checks
// orphan status itself rather than trusting the caller's own prior
// List call, the same defense ForcePurgeVM/ForcePurgeJail apply against
// state moving between an operator's two separate actions.
func (s *Server) CleanupOrphanedHASTResource(ctx context.Context, req *rpcpb.CleanupOrphanedHASTResourceRequest) (*rpcpb.CleanupOrphanedHASTResourceResponse, error) {
	if s.zfs == nil {
		return &rpcpb.CleanupOrphanedHASTResourceResponse{Error: "this node has no ZFS support configured"}, nil
	}
	id := req.GetResourceId()
	kind := req.GetResourceType()
	if id == "" {
		return &rpcpb.CleanupOrphanedHASTResourceResponse{Error: "resource_id must be set"}, nil
	}
	datasetName, err := hastOrphanDatasetName(kind, id)
	if err != nil {
		return &rpcpb.CleanupOrphanedHASTResourceResponse{Error: err.Error()}, nil
	}
	active, err := s.hastResourceActiveOnThisNode(ctx, kind, id)
	if err != nil {
		return &rpcpb.CleanupOrphanedHASTResourceResponse{Error: err.Error()}, nil
	}
	if active {
		return &rpcpb.CleanupOrphanedHASTResourceResponse{
			Error: fmt.Sprintf("%s %q is no longer orphaned - a record on this node now references it again; refusing to destroy its data", kind, id),
		}, nil
	}
	exists, err := s.zfs.DatasetExists(ctx, datasetName)
	if err != nil {
		return &rpcpb.CleanupOrphanedHASTResourceResponse{Error: err.Error()}, nil
	}
	if !exists {
		return &rpcpb.CleanupOrphanedHASTResourceResponse{Error: fmt.Sprintf("no such orphaned resource: dataset %q does not exist", datasetName)}, nil
	}
	if err := s.zfs.DestroyDataset(ctx, datasetName); err != nil {
		return &rpcpb.CleanupOrphanedHASTResourceResponse{Error: err.Error()}, nil
	}
	return &rpcpb.CleanupOrphanedHASTResourceResponse{}, nil
}

// ListNodeServices reports this Hive's own fixed Apiary rc.d service
// inventory. It is intentionally not a cluster view: each Hive knows its
// own rc.conf and process state, and a peer must be queried directly for its
// own result.
func (s *Server) ListNodeServices(ctx context.Context, _ *rpcpb.ListNodeServicesRequest) (*rpcpb.ListNodeServicesResponse, error) {
	if s.services == nil {
		return &rpcpb.ListNodeServicesResponse{Error: "this node has no service controller configured"}, nil
	}
	services, err := s.services.List(ctx)
	if err != nil {
		return &rpcpb.ListNodeServicesResponse{Error: err.Error()}, nil
	}
	return &rpcpb.ListNodeServicesResponse{Services: services}, nil
}

// restartGuardrailService is the one service the action-preflight
// restart guardrail (ADR-0103) applies to - the real cross-node quorum
// risk the guardrail exists for. frontend/restshimd restarts never kill
// the process handling the RestartNodeService call itself (they're
// separate processes), so neither needs the guardrail's lease/confirm
// machinery at all.
const restartGuardrailService = "apiary_managerd"

// restartLeaseCooldownSeconds is the minimum time between confirmed
// apiary_managerd restarts on any Raft voter - "do not restart both
// managers within ten minutes," the roadmap's own example verbatim.
const restartLeaseCooldownSeconds = 600

// restartCommandTimeout bounds the `service apiary_managerd restart`
// call itself - comfortably shorter than nothing (there is no lease TTL
// to race against any more, ADR-0103's revision note #17), but still
// bounded so a hung command doesn't block this goroutine forever.
const restartCommandTimeout = 60 * time.Second

// RestartNodeService restarts one allowlisted service on this Hive. Every
// restart is scheduled after this RPC returns: restarting either managerd or
// frontend can otherwise sever the gRPC or HTTP connection carrying the
// confirmation back to the operator.
//
// For apiary_managerd only, this now goes through the action-preflight
// restart guardrail (ADR-0103): a cluster-wide Raft lease must be
// reserved first, and - critically - this process can never reliably
// observe or confirm the restart's own outcome, because the restart
// replaces the very process handling this call. Confirmation instead
// happens from the restarted node's own next startup (cmd/managerd's own
// startup path) - see RestartConfirmStore's doc comment.
func (s *Server) RestartNodeService(ctx context.Context, req *rpcpb.RestartNodeServiceRequest) (*rpcpb.RestartNodeServiceResponse, error) {
	if s.services == nil {
		return &rpcpb.RestartNodeServiceResponse{Error: "this node has no service controller configured"}, nil
	}
	name := req.GetName()
	if !restartableService(name) {
		return &rpcpb.RestartNodeServiceResponse{Error: fmt.Sprintf("service %q cannot be restarted from Apiary", name)}, nil
	}

	var guardrailOverridden bool
	var leaseID uint64
	if name == restartGuardrailService {
		reserveResp, err := s.reserveRestartLease(ctx, &rpcpb.ReserveRestartLeaseRequest{
			Service: name,
			NodeId:  s.nodeID,
			Force:   req.GetForce(),
		})
		if err != nil {
			return &rpcpb.RestartNodeServiceResponse{Error: err.Error()}, nil
		}
		// Fail closed on both a real block and any raft-level failure to
		// even determine one - an evaluation that couldn't determine the
		// real state must never be silently treated as safe.
		if reserveResp.GetError() != "" || !reserveResp.GetGranted() {
			errMsg := reserveResp.GetError()
			if errMsg == "" {
				errMsg = "refusing to restart: the restart-lease reservation was not granted"
			}
			return &rpcpb.RestartNodeServiceResponse{Error: errMsg}, nil
		}
		guardrailOverridden = reserveResp.GetGuardrailOverridden()
		leaseID = reserveResp.GetLeaseId()

		if s.restartConfirm != nil {
			// Written BEFORE issuing the restart command below, not
			// after - a crash immediately after issuing the command
			// still leaves a discoverable, correctly-blocking trace
			// (ADR-0103).
			if err := s.restartConfirm.Save(PendingRestart{Service: name, NodeID: s.nodeID, LeaseID: leaseID}); err != nil {
				fmt.Fprintf(os.Stderr, "apiary: saving pending restart confirmation for %s: %v\n", name, err)
			}
		}
	}

	go func() {
		// Let gRPC and the frontend's HTTP handler flush the confirmation
		// before either target service is restarted.
		time.Sleep(250 * time.Millisecond)
		restartCtx, cancel := context.WithTimeout(context.Background(), restartCommandTimeout)
		defer cancel()
		if err := s.services.Restart(restartCtx, name); err != nil {
			fmt.Fprintf(os.Stderr, "apiary: restarting %s: %v\n", name, err)
		}
		// No further action here for restartGuardrailService: this
		// process cannot reliably confirm its own replacement's outcome
		// (ADR-0103) - confirmation happens from the NEW process's own
		// startup, which reads back the pending-restart record this
		// handler already wrote above.
	}()
	return &rpcpb.RestartNodeServiceResponse{Scheduled: true, GuardrailOverridden: guardrailOverridden}, nil
}

// toRPCGuardrailFindings converts internal/guardrail.Finding values into
// their flat, wire-safe rpcpb.GuardrailFinding form - mirroring
// AssumptionClaim.evidence's own flat-string convention rather than a
// nested message.
func toRPCGuardrailFindings(findings []guardrail.Finding) []*rpcpb.GuardrailFinding {
	out := make([]*rpcpb.GuardrailFinding, 0, len(findings))
	for _, f := range findings {
		evidence := make([]string, 0, len(f.Evidence))
		for _, e := range f.Evidence {
			evidence = append(evidence, e.Source+": "+e.Detail)
		}
		out = append(out, &rpcpb.GuardrailFinding{Rule: f.Rule, Detail: f.Detail, Evidence: evidence})
	}
	return out
}

// voterNodeIDs returns the node ids of every currently-Voter server in
// status, per ADR-0103's own "only actual Raft voters count" rule.
func voterNodeIDs(status *internalpb.StatusResponse) []string {
	var voters []string
	for _, srv := range status.GetServers() {
		if srv.GetSuffrage() == "Voter" {
			voters = append(voters, srv.GetId())
		}
	}
	return voters
}

// PreflightRestartNodeService previews the action-preflight restart
// guardrail (ADR-0103) for apiary_managerd - Viewer-tier, read-only, no
// live dial to a caller-influenced address (only a local raft-internal
// state read, unlike PreflightApproveJoinRequest). Always Allow for
// every other service, which carries no quorum stake.
func (s *Server) PreflightRestartNodeService(ctx context.Context, req *rpcpb.PreflightRestartNodeServiceRequest) (*rpcpb.PreflightRestartNodeServiceResponse, error) {
	name := req.GetName()
	if name != restartGuardrailService {
		return &rpcpb.PreflightRestartNodeServiceResponse{Verdict: string(guardrail.Allow)}, nil
	}

	fact := guardrail.RestartCooldownFact{TargetService: name}
	raftStatus, err := s.raft.Status(ctx)
	if err != nil {
		// Can't determine voter status either - assume this node could
		// be a voter so the read failure below is actually consulted,
		// rather than silently short-circuiting to Allow.
		fact.IsLocalNodeVoter = true
		fact.ReadOK = false
	} else {
		fact.IsLocalNodeVoter = false
		for _, v := range voterNodeIDs(raftStatus) {
			if v == s.nodeID {
				fact.IsLocalNodeVoter = true
				break
			}
		}
		leaseResp, leaseErr := s.raft.GetRestartLeaseStateLocal(ctx, name)
		fact.ReadOK = leaseErr == nil
		if leaseErr == nil {
			if lease := leaseResp.GetLease(); lease != nil {
				fact.ActiveLease = &guardrail.LeaseInfo{HolderNodeID: lease.GetHolderNodeId(), RequestedAtUnix: lease.GetRequestedAtUnix()}
			}
			if record := leaseResp.GetRecord(); record != nil && fact.ActiveLease == nil {
				elapsed := time.Now().Unix() - record.GetCompletedAtUnix()
				if elapsed < restartLeaseCooldownSeconds {
					fact.RecentRestart = &guardrail.RestartInfo{NodeID: record.GetNodeId(), CompletedAtUnix: record.GetCompletedAtUnix()}
				}
			}
		}
	}

	report := guardrail.EvaluateConcurrentManagerRestart(fact)
	return &rpcpb.PreflightRestartNodeServiceResponse{
		Verdict:  string(report.Verdict),
		Findings: toRPCGuardrailFindings(report.Findings),
	}, nil
}

// ReserveRestartLease is the external RPC form, gRPC-reachable by a
// peer's own forwarded call - gated by restartGuardrailTokenValid against
// a root-owned local file loaded once at startup, deliberately NOT the
// normal Viewer/Admin role hierarchy (see internal/manager/auth.go's own
// reserveRestartLeaseMethod doc comment). RestartNodeService, in the same
// process, calls reserveRestartLease directly instead of this method -
// its own caller's incoming context carries whatever credential the
// *operator* presented (an Admin API key, or none), never this dedicated
// token, so gating the same-process path here would make every local
// restart fail closed for the wrong reason.
func (s *Server) ReserveRestartLease(ctx context.Context, req *rpcpb.ReserveRestartLeaseRequest) (*rpcpb.ReserveRestartLeaseResponse, error) {
	presented, _ := extractBearerToken(ctx)
	if !restartGuardrailTokenValid(presented, s.restartGuardrailToken) {
		return nil, status.Error(codes.PermissionDenied, "invalid or missing restart-guardrail token")
	}
	return s.reserveRestartLease(ctx, req)
}

// reserveRestartLease is internal plumbing behind RestartNodeService's own
// guardrail (ADR-0103) - never called by an operator or exposed in any
// UI control, and carries no token check of its own: the only two
// callers are ReserveRestartLease above (already checked the token) and
// RestartNodeService (the same trusted process calling itself directly,
// never over the wire).
func (s *Server) reserveRestartLease(ctx context.Context, req *rpcpb.ReserveRestartLeaseRequest) (*rpcpb.ReserveRestartLeaseResponse, error) {
	raftStatus, err := s.raft.Status(ctx)
	if err != nil {
		return &rpcpb.ReserveRestartLeaseResponse{Error: err.Error()}, nil
	}
	if !raftStatus.GetIsLeader() {
		if hint := currentLeaderRaftAddress(raftStatus); hint != "" && s.peers != nil {
			fwd, ferr := s.peers.ReserveRestartLease(ctx, s.peerManagerdAddr(hint), req)
			if ferr == nil {
				return fwd, nil
			}
			// A hint was known and a forward was actually attempted here -
			// the "no reachable leader hint" message below would be false
			// in this branch, so this reports the real (forwarding)
			// failure instead of reusing that unrelated text.
			return &rpcpb.ReserveRestartLeaseResponse{Error: augmentForwardError("not leader", hint, ferr)}, nil
		}
		return &rpcpb.ReserveRestartLeaseResponse{Error: "not leader and no reachable leader hint for the restart-guardrail lease"}, nil
	}

	// The leader authors both the voter snapshot and the timestamp
	// itself, immediately before submitting - never accepted from the
	// request, so a stale follower view or cross-node clock skew can
	// never influence the decision (ADR-0103).
	cmd := &internalpb.Command{
		Op: &internalpb.Command_AcquireRestartLease{
			AcquireRestartLease: &internalpb.AcquireRestartLease{
				Service:         req.GetService(),
				NodeId:          req.GetNodeId(),
				RequestedAtUnix: time.Now().Unix(),
				VoterNodeIds:    voterNodeIDs(raftStatus),
				CooldownSeconds: restartLeaseCooldownSeconds,
				Force:           req.GetForce(),
			},
		},
	}
	payload, err := proto.Marshal(cmd)
	if err != nil {
		return &rpcpb.ReserveRestartLeaseResponse{Error: err.Error()}, nil
	}
	applyResp, err := s.raft.Apply(ctx, payload, defaultApplyTimeout)
	if err != nil {
		return &rpcpb.ReserveRestartLeaseResponse{Error: err.Error()}, nil
	}
	if applyResp.GetError() != "" {
		return &rpcpb.ReserveRestartLeaseResponse{Error: applyResp.GetError(), LeaderHint: applyResp.GetLeaderHint()}, nil
	}
	var lease internalpb.RestartLease
	if err := proto.Unmarshal(applyResp.GetResult(), &lease); err != nil {
		return &rpcpb.ReserveRestartLeaseResponse{Error: err.Error()}, nil
	}
	return &rpcpb.ReserveRestartLeaseResponse{
		Granted:             true,
		GuardrailOverridden: lease.GetForce(),
		LeaseId:             lease.GetLeaseId(),
	}, nil
}

// ConfirmRestartCompleted is the external RPC form, gRPC-reachable by a
// peer's own forwarded call - same token-gating posture as
// ReserveRestartLease above, for the identical reason.
func (s *Server) ConfirmRestartCompleted(ctx context.Context, req *rpcpb.ConfirmRestartCompletedRequest) (*rpcpb.ConfirmRestartCompletedResponse, error) {
	presented, _ := extractBearerToken(ctx)
	if !restartGuardrailTokenValid(presented, s.restartGuardrailToken) {
		return nil, status.Error(codes.PermissionDenied, "invalid or missing restart-guardrail token")
	}
	return s.confirmRestartCompleted(ctx, req)
}

// ConfirmRestartCompletedLocal is for cmd/managerd's own startup-
// confirmation path (ADR-0103) ONLY - the restarted node's own next
// startup calling on its own behalf, the same trusted process as this
// Server, never over the wire. Skips the token check entirely, the same
// same-process-trusts-itself reasoning reserveRestartLease has relative
// to ReserveRestartLease - cmd/managerd has no way to present this
// Server's own dedicated token back to itself (nor should it need to).
func (s *Server) ConfirmRestartCompletedLocal(ctx context.Context, service, nodeID string, leaseID uint64) (*rpcpb.ConfirmRestartCompletedResponse, error) {
	return s.confirmRestartCompleted(ctx, &rpcpb.ConfirmRestartCompletedRequest{Service: service, NodeId: nodeID, LeaseId: leaseID})
}

// confirmRestartCompleted is submitted by the restarted node's own next
// startup, never by the process that requested the restart (ADR-0103,
// revision note #16) - carries no token check of its own, mirroring
// reserveRestartLease's identical split above.
func (s *Server) confirmRestartCompleted(ctx context.Context, req *rpcpb.ConfirmRestartCompletedRequest) (*rpcpb.ConfirmRestartCompletedResponse, error) {
	raftStatus, err := s.raft.Status(ctx)
	if err != nil {
		return &rpcpb.ConfirmRestartCompletedResponse{Error: err.Error()}, nil
	}
	if !raftStatus.GetIsLeader() {
		if hint := currentLeaderRaftAddress(raftStatus); hint != "" && s.peers != nil {
			fwd, ferr := s.peers.ConfirmRestartCompleted(ctx, s.peerManagerdAddr(hint), req)
			if ferr == nil {
				return fwd, nil
			}
			// See reserveRestartLease's identical comment above.
			return &rpcpb.ConfirmRestartCompletedResponse{Error: augmentForwardError("not leader", hint, ferr)}, nil
		}
		return &rpcpb.ConfirmRestartCompletedResponse{Error: "not leader and no reachable leader hint for the restart-guardrail confirmation"}, nil
	}

	cmd := &internalpb.Command{
		Op: &internalpb.Command_RecordRestartCompleted{
			RecordRestartCompleted: &internalpb.RecordRestartCompleted{
				Service:         req.GetService(),
				NodeId:          req.GetNodeId(),
				CompletedAtUnix: time.Now().Unix(),
				LeaseId:         req.GetLeaseId(),
			},
		},
	}
	payload, err := proto.Marshal(cmd)
	if err != nil {
		return &rpcpb.ConfirmRestartCompletedResponse{Error: err.Error()}, nil
	}
	applyResp, err := s.raft.Apply(ctx, payload, defaultApplyTimeout)
	if err != nil {
		return &rpcpb.ConfirmRestartCompletedResponse{Error: err.Error()}, nil
	}
	if applyResp.GetError() != "" {
		return &rpcpb.ConfirmRestartCompletedResponse{Error: applyResp.GetError(), LeaderHint: applyResp.GetLeaderHint()}, nil
	}
	return &rpcpb.ConfirmRestartCompletedResponse{}, nil
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
	var ferr error
	if leaderHint != "" && s.peers != nil {
		var fwd *rpcpb.CreateJailResponse
		if fwd, ferr = s.peers.CreateJail(ctx, s.peerManagerdAddr(leaderHint), req); ferr == nil {
			return fwd, nil
		}
	}
	return &rpcpb.CreateJailResponse{Jail: fromInternalJail(jail), Error: augmentForwardError(appErr, leaderHint, ferr), LeaderHint: leaderHint}, nil
}

// UpdateJail implements rpcpb.ManagerServiceServer. See CreateNetwork's
// doc comment for the forwarding rationale, identical here.
func (s *Server) UpdateJail(ctx context.Context, req *rpcpb.UpdateJailRequest) (*rpcpb.UpdateJailResponse, error) {
	cmd := &internalpb.Command{
		Op: &internalpb.Command_UpdateJail{UpdateJail: &internalpb.UpdateJail{Jail: toInternalJail(req.GetJail())}},
	}
	jail, appErr, leaderHint := s.applyJailCommand(ctx, cmd, req.GetTimeoutMs())
	var ferr error
	if leaderHint != "" && s.peers != nil {
		var fwd *rpcpb.UpdateJailResponse
		if fwd, ferr = s.peers.UpdateJail(ctx, s.peerManagerdAddr(leaderHint), req); ferr == nil {
			return fwd, nil
		}
	}
	return &rpcpb.UpdateJailResponse{Jail: fromInternalJail(jail), Error: augmentForwardError(appErr, leaderHint, ferr), LeaderHint: leaderHint}, nil
}

// DeleteJail implements rpcpb.ManagerServiceServer. See CreateNetwork's
// doc comment for the forwarding rationale, identical here.
func (s *Server) DeleteJail(ctx context.Context, req *rpcpb.DeleteJailRequest) (*rpcpb.DeleteJailResponse, error) {
	cmd := &internalpb.Command{
		Op: &internalpb.Command_DeleteJail{DeleteJail: &internalpb.DeleteJail{Id: req.GetId()}},
	}
	jail, appErr, leaderHint := s.applyJailCommand(ctx, cmd, req.GetTimeoutMs())
	var ferr error
	if leaderHint != "" && s.peers != nil {
		var fwd *rpcpb.DeleteJailResponse
		if fwd, ferr = s.peers.DeleteJail(ctx, s.peerManagerdAddr(leaderHint), req); ferr == nil {
			return fwd, nil
		}
	}
	return &rpcpb.DeleteJailResponse{Jail: fromInternalJail(jail), Error: augmentForwardError(appErr, leaderHint, ferr), LeaderHint: leaderHint}, nil
}

// SetJailDesiredState mirrors SetVMDesiredState for jails.
func (s *Server) SetJailDesiredState(ctx context.Context, req *rpcpb.SetJailDesiredStateRequest) (*rpcpb.SetJailDesiredStateResponse, error) {
	cmd := &internalpb.Command{Op: &internalpb.Command_SetJailDesiredState{SetJailDesiredState: &internalpb.SetJailDesiredState{
		Id: req.GetId(), DesiredState: internalpb.JailState(req.GetDesiredState()),
	}}}
	jail, appErr, leaderHint := s.applyJailCommand(ctx, cmd, req.GetTimeoutMs())
	var ferr error
	if leaderHint != "" && s.peers != nil {
		var fwd *rpcpb.SetJailDesiredStateResponse
		if fwd, ferr = s.peers.SetJailDesiredState(ctx, s.peerManagerdAddr(leaderHint), req); ferr == nil {
			return fwd, nil
		}
	}
	return &rpcpb.SetJailDesiredStateResponse{Jail: fromInternalJail(jail), Error: augmentForwardError(appErr, leaderHint, ferr), LeaderHint: leaderHint}, nil
}

// SetJailHostname implements rpcpb.ManagerServiceServer - lets an
// operator rename a jail's hostname after creation, previously only
// settable via CreateJail. Same narrow, atomic-apply shape as
// SetVMFirewallPaused, for the identical reason (a full UpdateJail
// replace is never routed through the web UI).
func (s *Server) SetJailHostname(ctx context.Context, req *rpcpb.SetJailHostnameRequest) (*rpcpb.SetJailHostnameResponse, error) {
	cmd := &internalpb.Command{Op: &internalpb.Command_SetJailHostname{SetJailHostname: &internalpb.SetJailHostname{
		Id: req.GetId(), Hostname: req.GetHostname(),
	}}}
	jail, appErr, leaderHint := s.applyJailCommand(ctx, cmd, req.GetTimeoutMs())
	var ferr error
	if leaderHint != "" && s.peers != nil {
		var fwd *rpcpb.SetJailHostnameResponse
		if fwd, ferr = s.peers.SetJailHostname(ctx, s.peerManagerdAddr(leaderHint), req); ferr == nil {
			return fwd, nil
		}
	}
	return &rpcpb.SetJailHostnameResponse{Jail: fromInternalJail(jail), Error: augmentForwardError(appErr, leaderHint, ferr), LeaderHint: leaderHint}, nil
}

// GetJail implements rpcpb.ManagerServiceServer.
func (s *Server) GetJail(ctx context.Context, req *rpcpb.GetJailRequest) (*rpcpb.GetJailResponse, error) {
	resp, err := s.raft.GetJail(ctx, req.GetId())
	if err != nil {
		return &rpcpb.GetJailResponse{Error: err.Error()}, nil
	}
	if resp.GetError() != "" {
		var ferr error
		if s.peers != nil && resp.GetLeaderHint() != "" {
			var fwd *rpcpb.GetJailResponse
			if fwd, ferr = s.peers.GetJail(ctx, s.peerManagerdAddr(resp.GetLeaderHint()), req.GetId()); ferr == nil {
				return fwd, nil
			}
		}
		return &rpcpb.GetJailResponse{Error: augmentForwardError(resp.GetError(), resp.GetLeaderHint(), ferr), LeaderHint: resp.GetLeaderHint()}, nil
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
		var ferr error
		if s.peers != nil && getResp.GetLeaderHint() != "" {
			var fwd *rpcpb.ForcePurgeJailResponse
			if fwd, ferr = s.peers.ForcePurgeJail(ctx, s.peerManagerdAddr(getResp.GetLeaderHint()), req); ferr == nil {
				return fwd, nil
			}
		}
		return &rpcpb.ForcePurgeJailResponse{Error: augmentForwardError(getResp.GetError(), getResp.GetLeaderHint(), ferr), LeaderHint: getResp.GetLeaderHint()}, nil
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
	var ferr error
	if leaderHint != "" && s.peers != nil {
		var fwd *rpcpb.ForcePurgeJailResponse
		if fwd, ferr = s.peers.ForcePurgeJail(ctx, s.peerManagerdAddr(leaderHint), req); ferr == nil {
			return fwd, nil
		}
	}
	return &rpcpb.ForcePurgeJailResponse{Jail: fromInternalJail(jail), Error: augmentForwardError(appErr, leaderHint, ferr), LeaderHint: leaderHint}, nil
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
		var ferr error
		if s.peers != nil && getResp.GetLeaderHint() != "" {
			var fwd *rpcpb.MigrateJailResponse
			if fwd, ferr = s.peers.MigrateJail(ctx, s.peerManagerdAddr(getResp.GetLeaderHint()), req); ferr == nil {
				return fwd, nil
			}
		}
		return &rpcpb.MigrateJailResponse{Error: augmentForwardError(getResp.GetError(), getResp.GetLeaderHint(), ferr), LeaderHint: getResp.GetLeaderHint()}, nil
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
	var ferr error
	if leaderHint != "" && s.peers != nil {
		var fwd *rpcpb.MigrateJailResponse
		if fwd, ferr = s.peers.MigrateJail(ctx, s.peerManagerdAddr(leaderHint), req); ferr == nil {
			return fwd, nil
		}
	}
	return &rpcpb.MigrateJailResponse{Jail: fromInternalJail(result), Error: augmentForwardError(appErr, leaderHint, ferr), LeaderHint: leaderHint}, nil
}

// ListJails implements rpcpb.ManagerServiceServer.
func (s *Server) ListJails(ctx context.Context, _ *rpcpb.ListJailsRequest) (*rpcpb.ListJailsResponse, error) {
	resp, err := s.raft.ListJails(ctx)
	if err != nil {
		return &rpcpb.ListJailsResponse{Error: err.Error()}, nil
	}
	if resp.GetError() != "" {
		var ferr error
		if s.peers != nil && resp.GetLeaderHint() != "" {
			var fwd *rpcpb.ListJailsResponse
			if fwd, ferr = s.peers.ListJails(ctx, s.peerManagerdAddr(resp.GetLeaderHint())); ferr == nil {
				return fwd, nil
			}
		}
		return &rpcpb.ListJailsResponse{Error: augmentForwardError(resp.GetError(), resp.GetLeaderHint(), ferr), LeaderHint: resp.GetLeaderHint()}, nil
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
		var ferr error
		if s.peers != nil && resp.GetLeaderHint() != "" {
			var fwd *rpcpb.ListVMsResponse
			if fwd, ferr = s.peers.ListVMs(ctx, s.peerManagerdAddr(resp.GetLeaderHint())); ferr == nil {
				return fwd, nil
			}
		}
		return &rpcpb.ListVMsResponse{Error: augmentForwardError(resp.GetError(), resp.GetLeaderHint(), ferr), LeaderHint: resp.GetLeaderHint()}, nil
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
		var ferr error
		if s.peers != nil && vmsResp.GetLeaderHint() != "" {
			var fwd *rpcpb.SimulateNodeFailureResponse
			if fwd, ferr = s.peers.SimulateNodeFailure(ctx, s.peerManagerdAddr(vmsResp.GetLeaderHint()), req); ferr == nil {
				return fwd, nil
			}
		}
		return &rpcpb.SimulateNodeFailureResponse{Error: augmentForwardError(vmsResp.GetError(), vmsResp.GetLeaderHint(), ferr), LeaderHint: vmsResp.GetLeaderHint()}, nil
	}

	jailsResp, err := s.raft.ListJails(ctx)
	if err != nil {
		return &rpcpb.SimulateNodeFailureResponse{Error: err.Error()}, nil
	}
	if jailsResp.GetError() != "" {
		var ferr error
		if s.peers != nil && jailsResp.GetLeaderHint() != "" {
			var fwd *rpcpb.SimulateNodeFailureResponse
			if fwd, ferr = s.peers.SimulateNodeFailure(ctx, s.peerManagerdAddr(jailsResp.GetLeaderHint()), req); ferr == nil {
				return fwd, nil
			}
		}
		return &rpcpb.SimulateNodeFailureResponse{Error: augmentForwardError(jailsResp.GetError(), jailsResp.GetLeaderHint(), ferr), LeaderHint: jailsResp.GetLeaderHint()}, nil
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
	requirements := make([]cluster.ImageRequirement, 0, len(vmsResp.GetVms())*2+len(jailsResp.GetJails()))
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
	for _, j := range jailsResp.GetJails() {
		if j.GetBaseArchiveName() != "" {
			requirements = append(requirements, cluster.ImageRequirement{
				ResourceID: j.GetId(), ResourceName: j.GetName(), ImageName: j.GetBaseArchiveName(), Role: cluster.ImageRoleBaseArchive,
			})
		}
	}
	report.ImageAvailability = cluster.ComputeImageAvailability(requirements, s.imageInventoryObservations(ctx, raftStatus.GetServers(), targetID, localNodeID), targetID)
	return &rpcpb.SimulateNodeFailureResponse{
		Quorum:                 toRPCQuorumImpact(report.Quorum),
		OwnedResources:         toRPCOwnedResourceImpacts(report.OwnedResources),
		ReplicaBackedResources: toRPCReplicaBackedImpacts(report.ReplicaBackedResources),
		ImageAvailability:      toRPCImageAvailability(report.ImageAvailability),
		RelevantClaims:         s.relevantRegisterClaims(targetID, time.Now()),
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
		var ferr error
		if s.peers != nil && networksResp.GetLeaderHint() != "" {
			var fwd *rpcpb.SimulateNetworkFailureResponse
			if fwd, ferr = s.peers.SimulateNetworkFailure(ctx, s.peerManagerdAddr(networksResp.GetLeaderHint()), req); ferr == nil {
				return fwd, nil
			}
		}
		return &rpcpb.SimulateNetworkFailureResponse{Error: augmentForwardError(networksResp.GetError(), networksResp.GetLeaderHint(), ferr), LeaderHint: networksResp.GetLeaderHint()}, nil
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
		var ferr error
		if s.peers != nil && vmsResp.GetLeaderHint() != "" {
			var fwd *rpcpb.SimulateNetworkFailureResponse
			if fwd, ferr = s.peers.SimulateNetworkFailure(ctx, s.peerManagerdAddr(vmsResp.GetLeaderHint()), req); ferr == nil {
				return fwd, nil
			}
		}
		return &rpcpb.SimulateNetworkFailureResponse{Error: augmentForwardError(vmsResp.GetError(), vmsResp.GetLeaderHint(), ferr), LeaderHint: vmsResp.GetLeaderHint()}, nil
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
	voters := make([]*rpcpb.VoterReachability, 0, len(q.Voters))
	for _, v := range q.Voters {
		voters = append(voters, &rpcpb.VoterReachability{
			NodeId:       v.ID,
			Reachability: string(v.Reachability),
		})
	}
	return &rpcpb.QuorumImpact{
		TargetIsVoter:            q.TargetIsVoter,
		TotalVoters:              q.TotalVoters,
		RemainingVoters:          q.RemainingVoters,
		RemainingReachableVoters: q.RemainingReachable,
		RemainingUnknownVoters:   q.RemainingUnknown,
		QuorumSize:               q.QuorumSize,
		Survives:                 q.Survives,
		Note:                     q.Note,
		Voters:                   voters,
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
		switch impact.Role {
		case cluster.ImageRoleBaseImage:
			role = rpcpb.ImageRole_IMAGE_ROLE_BASE_IMAGE
		case cluster.ImageRoleBaseArchive:
			role = rpcpb.ImageRole_IMAGE_ROLE_BASE_ARCHIVE
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
