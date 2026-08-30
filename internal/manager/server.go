package manager

import (
	"context"
	"fmt"
	"io"
	"time"

	"google.golang.org/protobuf/proto"

	internalpb "github.com/glenjbarber/apiary/api/internalpb"
	rpcpb "github.com/glenjbarber/apiary/api/rpc"
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

// Server implements the generated ManagerServiceServer interface, the
// server side of managerd's external RPC API.
type Server struct {
	rpcpb.UnimplementedManagerServiceServer

	raft   *RaftClient
	nodeID string
	isos   isoManager
}

var _ rpcpb.ManagerServiceServer = (*Server)(nil)

// NewServer returns a Server that answers external RPCs using raft to
// reach raftd, reporting nodeID as its own identity, and isos to store
// installer images locally on this node.
func NewServer(raft *RaftClient, nodeID string, isos isoManager) *Server {
	return &Server{raft: raft, nodeID: nodeID, isos: isos}
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
