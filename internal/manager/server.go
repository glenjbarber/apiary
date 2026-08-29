package manager

import (
	"context"
	"time"

	"google.golang.org/protobuf/proto"

	internalpb "github.com/glenjbarber/apiary/api/internalpb"
	rpcpb "github.com/glenjbarber/apiary/api/rpc"
)

// defaultApplyTimeout is used when a request doesn't specify one.
const defaultApplyTimeout = 10 * time.Second

// Server implements the generated ManagerServiceServer interface, the
// server side of managerd's external RPC API.
type Server struct {
	rpcpb.UnimplementedManagerServiceServer

	raft   *RaftClient
	nodeID string
}

var _ rpcpb.ManagerServiceServer = (*Server)(nil)

// NewServer returns a Server that answers external RPCs using raft to
// reach raftd, reporting nodeID as its own identity.
func NewServer(raft *RaftClient, nodeID string) *Server {
	return &Server{raft: raft, nodeID: nodeID}
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
