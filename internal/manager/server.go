package manager

import (
	"context"
	"fmt"
	"io"
	"time"

	"google.golang.org/protobuf/proto"

	internalpb "github.com/glenjbarber/apiary/api/internalpb"
	rpcpb "github.com/glenjbarber/apiary/api/rpc"
	"github.com/glenjbarber/apiary/internal/hoststats"
	"github.com/glenjbarber/apiary/internal/isostore"
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
}

// VNCLookup is the subset of *bhyve.Manager the server needs for
// GetVMConsole, defined locally for the same reason as isoManager.
type VNCLookup interface {
	VNCPort(name string) (port int, ok bool, err error)
}

// VLANStatus is the subset of *vlan.Manager the server needs for
// ListNetworks's per-node bridge status, defined locally for the same
// reason as isoManager.
type VLANStatus interface {
	InterfaceStatus(ctx context.Context, name string) (exists, up bool, err error)
}

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

	// vlan is nil on a node with no VLAN support configured (see
	// cmd/managerd's own nil-able Reconciler.VLAN) - ListNetworks
	// reports "unknown" bridge status rather than panicking in that case.
	vlan VLANStatus

	// statsGather defaults to hoststats.Gather in NewServer; overridable
	// in tests so HostStats's RPC-translation logic can be exercised
	// without shelling out to real system commands.
	statsGather func(context.Context) *hoststats.Snapshot
}

var _ rpcpb.ManagerServiceServer = (*Server)(nil)

// NewServer returns a Server that answers external RPCs using raft to
// reach raftd, reporting nodeID as its own identity, isos to store
// installer images locally on this node, vnc (nil-able) to look up a
// running VM's VNC console port, and vlanMgr (nil-able) to report a
// network's bridge status on this node.
func NewServer(raft *RaftClient, nodeID string, isos isoManager, vnc VNCLookup, vlanMgr VLANStatus) *Server {
	return &Server{raft: raft, nodeID: nodeID, isos: isos, vnc: vnc, vlan: vlanMgr, statsGather: hoststats.Gather}
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

// CreateNetwork implements rpcpb.ManagerServiceServer.
func (s *Server) CreateNetwork(ctx context.Context, req *rpcpb.CreateNetworkRequest) (*rpcpb.CreateNetworkResponse, error) {
	cmd := &internalpb.Command{
		Op: &internalpb.Command_CreateNetwork{CreateNetwork: &internalpb.CreateNetwork{Network: toInternalNetwork(req.GetNetwork())}},
	}
	network, appErr, leaderHint := s.applyNetworkCommand(ctx, cmd, req.GetTimeoutMs())
	return &rpcpb.CreateNetworkResponse{Network: fromInternalNetwork(network), Error: appErr, LeaderHint: leaderHint}, nil
}

// DeleteNetwork implements rpcpb.ManagerServiceServer.
func (s *Server) DeleteNetwork(ctx context.Context, req *rpcpb.DeleteNetworkRequest) (*rpcpb.DeleteNetworkResponse, error) {
	cmd := &internalpb.Command{
		Op: &internalpb.Command_DeleteNetwork{DeleteNetwork: &internalpb.DeleteNetwork{Id: req.GetId()}},
	}
	network, appErr, leaderHint := s.applyNetworkCommand(ctx, cmd, req.GetTimeoutMs())
	return &rpcpb.DeleteNetworkResponse{Network: fromInternalNetwork(network), Error: appErr, LeaderHint: leaderHint}, nil
}

// ListNetworks implements rpcpb.ManagerServiceServer.
func (s *Server) ListNetworks(ctx context.Context, _ *rpcpb.ListNetworksRequest) (*rpcpb.ListNetworksResponse, error) {
	resp, err := s.raft.ListNetworks(ctx)
	if err != nil {
		return &rpcpb.ListNetworksResponse{Error: err.Error()}, nil
	}
	if resp.GetError() != "" {
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
			Id: id, Name: req.GetName(), HashedKey: hashed, CreatedUnix: time.Now().Unix(),
		}}},
	}
	key, appErr, leaderHint := s.applyAPIKeyCommand(ctx, cmd, req.GetTimeoutMs())
	if appErr != "" {
		return &rpcpb.CreateAPIKeyResponse{Error: appErr, LeaderHint: leaderHint}, nil
	}
	return &rpcpb.CreateAPIKeyResponse{Key: fromInternalAPIKey(key), RawKey: raw}, nil
}

// RevokeAPIKey implements rpcpb.ManagerServiceServer.
func (s *Server) RevokeAPIKey(ctx context.Context, req *rpcpb.RevokeAPIKeyRequest) (*rpcpb.RevokeAPIKeyResponse, error) {
	cmd := &internalpb.Command{
		Op: &internalpb.Command_RevokeApiKey{RevokeApiKey: &internalpb.RevokeAPIKey{Id: req.GetId()}},
	}
	_, appErr, leaderHint := s.applyAPIKeyCommand(ctx, cmd, req.GetTimeoutMs())
	return &rpcpb.RevokeAPIKeyResponse{Error: appErr, LeaderHint: leaderHint}, nil
}

// ListAPIKeys implements rpcpb.ManagerServiceServer.
func (s *Server) ListAPIKeys(ctx context.Context, _ *rpcpb.ListAPIKeysRequest) (*rpcpb.ListAPIKeysResponse, error) {
	resp, err := s.raft.ListAPIKeys(ctx)
	if err != nil {
		return &rpcpb.ListAPIKeysResponse{Error: err.Error()}, nil
	}
	if resp.GetError() != "" {
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

// CreateVM implements rpcpb.ManagerServiceServer.
func (s *Server) CreateVM(ctx context.Context, req *rpcpb.CreateVMRequest) (*rpcpb.CreateVMResponse, error) {
	cmd := &internalpb.Command{
		Op: &internalpb.Command_CreateVm{CreateVm: &internalpb.CreateVM{Vm: toInternalVM(req.GetVm())}},
	}
	vm, appErr, leaderHint := s.applyCommand(ctx, cmd, req.GetTimeoutMs())
	return &rpcpb.CreateVMResponse{Vm: fromInternalVM(vm), Error: appErr, LeaderHint: leaderHint}, nil
}

// UpdateVM implements rpcpb.ManagerServiceServer.
func (s *Server) UpdateVM(ctx context.Context, req *rpcpb.UpdateVMRequest) (*rpcpb.UpdateVMResponse, error) {
	cmd := &internalpb.Command{
		Op: &internalpb.Command_UpdateVm{UpdateVm: &internalpb.UpdateVM{Vm: toInternalVM(req.GetVm())}},
	}
	vm, appErr, leaderHint := s.applyCommand(ctx, cmd, req.GetTimeoutMs())
	return &rpcpb.UpdateVMResponse{Vm: fromInternalVM(vm), Error: appErr, LeaderHint: leaderHint}, nil
}

// DeleteVM implements rpcpb.ManagerServiceServer.
func (s *Server) DeleteVM(ctx context.Context, req *rpcpb.DeleteVMRequest) (*rpcpb.DeleteVMResponse, error) {
	cmd := &internalpb.Command{
		Op: &internalpb.Command_DeleteVm{DeleteVm: &internalpb.DeleteVM{Id: req.GetId()}},
	}
	vm, appErr, leaderHint := s.applyCommand(ctx, cmd, req.GetTimeoutMs())
	return &rpcpb.DeleteVMResponse{Vm: fromInternalVM(vm), Error: appErr, LeaderHint: leaderHint}, nil
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
// per-request auth check.
func (s *Server) ForcePurgeVM(ctx context.Context, req *rpcpb.ForcePurgeVMRequest) (*rpcpb.ForcePurgeVMResponse, error) {
	getResp, err := s.raft.GetVM(ctx, req.GetId())
	if err != nil {
		return &rpcpb.ForcePurgeVMResponse{Error: err.Error()}, nil
	}
	if getResp.GetError() != "" {
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
	return &rpcpb.ForcePurgeVMResponse{Vm: fromInternalVM(vm), Error: appErr, LeaderHint: leaderHint}, nil
}

// MigrateVM implements rpcpb.ManagerServiceServer. See its own proto
// doc comment for the full reasoning behind the "target_node_id must
// already be this VM's replica_node_id" requirement (ADR-0028) - in
// short, any other target would silently destroy the VM's real disk
// data (the old node's reconciler tears it down via ADR-0025's
// resource reclaim, and the new node has never seen the disk at all).
func (s *Server) MigrateVM(ctx context.Context, req *rpcpb.MigrateVMRequest) (*rpcpb.MigrateVMResponse, error) {
	if req.GetTargetNodeId() == "" {
		return &rpcpb.MigrateVMResponse{Error: "target_node_id must be set"}, nil
	}

	getResp, err := s.raft.GetVM(ctx, req.GetId())
	if err != nil {
		return &rpcpb.MigrateVMResponse{Error: err.Error()}, nil
	}
	if getResp.GetError() != "" {
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

// CreateJail implements rpcpb.ManagerServiceServer.
func (s *Server) CreateJail(ctx context.Context, req *rpcpb.CreateJailRequest) (*rpcpb.CreateJailResponse, error) {
	cmd := &internalpb.Command{
		Op: &internalpb.Command_CreateJail{CreateJail: &internalpb.CreateJail{Jail: toInternalJail(req.GetJail())}},
	}
	jail, appErr, leaderHint := s.applyJailCommand(ctx, cmd, req.GetTimeoutMs())
	return &rpcpb.CreateJailResponse{Jail: fromInternalJail(jail), Error: appErr, LeaderHint: leaderHint}, nil
}

// UpdateJail implements rpcpb.ManagerServiceServer.
func (s *Server) UpdateJail(ctx context.Context, req *rpcpb.UpdateJailRequest) (*rpcpb.UpdateJailResponse, error) {
	cmd := &internalpb.Command{
		Op: &internalpb.Command_UpdateJail{UpdateJail: &internalpb.UpdateJail{Jail: toInternalJail(req.GetJail())}},
	}
	jail, appErr, leaderHint := s.applyJailCommand(ctx, cmd, req.GetTimeoutMs())
	return &rpcpb.UpdateJailResponse{Jail: fromInternalJail(jail), Error: appErr, LeaderHint: leaderHint}, nil
}

// DeleteJail implements rpcpb.ManagerServiceServer.
func (s *Server) DeleteJail(ctx context.Context, req *rpcpb.DeleteJailRequest) (*rpcpb.DeleteJailResponse, error) {
	cmd := &internalpb.Command{
		Op: &internalpb.Command_DeleteJail{DeleteJail: &internalpb.DeleteJail{Id: req.GetId()}},
	}
	jail, appErr, leaderHint := s.applyJailCommand(ctx, cmd, req.GetTimeoutMs())
	return &rpcpb.DeleteJailResponse{Jail: fromInternalJail(jail), Error: appErr, LeaderHint: leaderHint}, nil
}

// GetJail implements rpcpb.ManagerServiceServer.
func (s *Server) GetJail(ctx context.Context, req *rpcpb.GetJailRequest) (*rpcpb.GetJailResponse, error) {
	resp, err := s.raft.GetJail(ctx, req.GetId())
	if err != nil {
		return &rpcpb.GetJailResponse{Error: err.Error()}, nil
	}
	if resp.GetError() != "" {
		return &rpcpb.GetJailResponse{Error: resp.GetError(), LeaderHint: resp.GetLeaderHint()}, nil
	}
	return &rpcpb.GetJailResponse{Jail: fromInternalJail(resp.GetJail()), Found: resp.GetFound()}, nil
}

// ForcePurgeJail mirrors ForcePurgeVM exactly - see its own doc
// comment for the full reasoning, which applies identically here.
func (s *Server) ForcePurgeJail(ctx context.Context, req *rpcpb.ForcePurgeJailRequest) (*rpcpb.ForcePurgeJailResponse, error) {
	getResp, err := s.raft.GetJail(ctx, req.GetId())
	if err != nil {
		return &rpcpb.ForcePurgeJailResponse{Error: err.Error()}, nil
	}
	if getResp.GetError() != "" {
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
	return &rpcpb.ForcePurgeJailResponse{Jail: fromInternalJail(jail), Error: appErr, LeaderHint: leaderHint}, nil
}

// MigrateJail mirrors MigrateVM exactly - see its own doc comment for
// the full reasoning, which applies identically here.
func (s *Server) MigrateJail(ctx context.Context, req *rpcpb.MigrateJailRequest) (*rpcpb.MigrateJailResponse, error) {
	if req.GetTargetNodeId() == "" {
		return &rpcpb.MigrateJailResponse{Error: "target_node_id must be set"}, nil
	}

	getResp, err := s.raft.GetJail(ctx, req.GetId())
	if err != nil {
		return &rpcpb.MigrateJailResponse{Error: err.Error()}, nil
	}
	if getResp.GetError() != "" {
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
	return &rpcpb.MigrateJailResponse{Jail: fromInternalJail(result), Error: appErr, LeaderHint: leaderHint}, nil
}

// ListJails implements rpcpb.ManagerServiceServer.
func (s *Server) ListJails(ctx context.Context, _ *rpcpb.ListJailsRequest) (*rpcpb.ListJailsResponse, error) {
	resp, err := s.raft.ListJails(ctx)
	if err != nil {
		return &rpcpb.ListJailsResponse{Error: err.Error()}, nil
	}
	if resp.GetError() != "" {
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
		return &rpcpb.ListVMsResponse{Error: resp.GetError(), LeaderHint: resp.GetLeaderHint()}, nil
	}

	vms := make([]*rpcpb.VMDefinition, 0, len(resp.GetVms()))
	for _, vm := range resp.GetVms() {
		vms = append(vms, fromInternalVM(vm))
	}
	return &rpcpb.ListVMsResponse{Vms: vms}, nil
}
