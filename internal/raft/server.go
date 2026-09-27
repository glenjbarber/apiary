package raft

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"google.golang.org/protobuf/proto"

	internalpb "github.com/glenjbarber/apiary/api/internalpb"
)

// defaultApplyTimeout is used when an ApplyRequest doesn't specify one.
const defaultApplyTimeout = 10 * time.Second

// Server implements the generated RaftInternalServer interface, translating
// between Node/FSM types and the api/internal/raftd.proto messages.
type Server struct {
	internalpb.UnimplementedRaftInternalServer

	node *Node

	// stepAsideMu guards StepAsideForRestartLocal against a second
	// concurrent caller. See that method for why the refusal is
	// reported as a field rather than a gRPC error.
	stepAsideMu sync.Mutex
}

var _ internalpb.RaftInternalServer = (*Server)(nil)

// NewServer returns a Server backed by node.
func NewServer(node *Node) *Server {
	return &Server{node: node}
}

// Apply implements internalpb.RaftInternalServer.
func (s *Server) Apply(_ context.Context, req *internalpb.ApplyRequest) (*internalpb.ApplyResponse, error) {
	timeout := defaultApplyTimeout
	if req.GetTimeoutMs() > 0 {
		timeout = time.Duration(req.GetTimeoutMs()) * time.Millisecond
	}

	result, err := s.node.Apply(req.GetPayload(), timeout)
	if err != nil {
		resp := &internalpb.ApplyResponse{Error: err.Error()}
		if errors.Is(err, ErrNotLeader) {
			resp.LeaderHint = s.node.LeaderHint()
		}
		return resp, nil
	}

	// result.Error is an application-level rejection (e.g. duplicate or
	// missing VM id) - the raft-level Apply still succeeded (err == nil
	// above), so it's reported the same way as a not-leader error: as an
	// ApplyResponse field, not a gRPC error.
	if result.Error != "" {
		return &internalpb.ApplyResponse{Error: result.Error}, nil
	}

	// Exactly one of VM/Network is set on a successful apply, depending on
	// which kind of command was applied (see FSMApplyResult's doc
	// comment) - marshal whichever one it is.
	var payload proto.Message = result.VM
	if result.Network != nil {
		payload = result.Network
	}
	if result.ApiKey != nil {
		payload = result.ApiKey
	}
	if result.Jail != nil {
		payload = result.Jail
	}
	if result.PendingJoinRequest != nil {
		payload = result.PendingJoinRequest
	}
	if result.RestartLease != nil {
		payload = result.RestartLease
	}
	if result.RestartRecord != nil {
		payload = result.RestartRecord
	}
	resultBytes, err := proto.Marshal(payload)
	if err != nil {
		return &internalpb.ApplyResponse{Error: fmt.Sprintf("encoding result: %v", err)}, nil
	}
	return &internalpb.ApplyResponse{Result: resultBytes}, nil
}

// Status implements internalpb.RaftInternalServer.
func (s *Server) Status(_ context.Context, _ *internalpb.StatusRequest) (*internalpb.StatusResponse, error) {
	status := s.node.Status()

	servers := make([]*internalpb.ServerInfo, 0, len(status.Servers))
	for _, srv := range status.Servers {
		servers = append(servers, &internalpb.ServerInfo{
			Id:       srv.ID,
			Address:  srv.Address,
			Suffrage: srv.Suffrage,
		})
	}

	return &internalpb.StatusResponse{
		IsLeader:     status.IsLeader,
		LeaderId:     status.LeaderID,
		NodeId:       status.NodeID,
		LastLogIndex: status.LastLogIndex,
		AppliedIndex: status.AppliedIndex,
		RaftState:    status.RaftState,
		Servers:      servers,
		StateDigest:  status.StateDigest,
	}, nil
}

// AddVoter implements internalpb.RaftInternalServer.
func (s *Server) AddVoter(_ context.Context, req *internalpb.AddVoterRequest) (*internalpb.AddVoterResponse, error) {
	timeout := defaultApplyTimeout
	if req.GetTimeoutMs() > 0 {
		timeout = time.Duration(req.GetTimeoutMs()) * time.Millisecond
	}

	err := s.node.AddVoter(req.GetId(), req.GetAddress(), req.GetPrevIndex(), timeout)
	if err != nil {
		resp := &internalpb.AddVoterResponse{Error: err.Error()}
		if errors.Is(err, ErrNotLeader) {
			resp.LeaderHint = s.node.LeaderHint()
		}
		return resp, nil
	}
	return &internalpb.AddVoterResponse{}, nil
}

// RemoveServer implements internalpb.RaftInternalServer.
func (s *Server) RemoveServer(_ context.Context, req *internalpb.RemoveServerRequest) (*internalpb.RemoveServerResponse, error) {
	timeout := defaultApplyTimeout
	if req.GetTimeoutMs() > 0 {
		timeout = time.Duration(req.GetTimeoutMs()) * time.Millisecond
	}

	err := s.node.RemoveServer(req.GetId(), req.GetPrevIndex(), timeout)
	if err != nil {
		resp := &internalpb.RemoveServerResponse{Error: err.Error()}
		if errors.Is(err, ErrNotLeader) {
			resp.LeaderHint = s.node.LeaderHint()
		}
		return resp, nil
	}
	return &internalpb.RemoveServerResponse{}, nil
}

// GetVM implements internalpb.RaftInternalServer.
func (s *Server) GetVM(_ context.Context, req *internalpb.GetVMRequest) (*internalpb.GetVMResponse, error) {
	vm, found, err := s.node.GetVM(req.GetId())
	if err != nil {
		resp := &internalpb.GetVMResponse{Error: err.Error()}
		if errors.Is(err, ErrNotLeader) {
			resp.LeaderHint = s.node.LeaderHint()
		}
		return resp, nil
	}
	return &internalpb.GetVMResponse{Vm: vm, Found: found}, nil
}

// ListVMs implements internalpb.RaftInternalServer.
func (s *Server) ListVMs(_ context.Context, _ *internalpb.ListVMsRequest) (*internalpb.ListVMsResponse, error) {
	vms, err := s.node.ListVMs()
	if err != nil {
		resp := &internalpb.ListVMsResponse{Error: err.Error()}
		if errors.Is(err, ErrNotLeader) {
			resp.LeaderHint = s.node.LeaderHint()
		}
		return resp, nil
	}
	return &internalpb.ListVMsResponse{Vms: vms}, nil
}

// GetNetwork implements internalpb.RaftInternalServer.
func (s *Server) GetNetwork(_ context.Context, req *internalpb.GetNetworkRequest) (*internalpb.GetNetworkResponse, error) {
	network, found, err := s.node.GetNetwork(req.GetId())
	if err != nil {
		resp := &internalpb.GetNetworkResponse{Error: err.Error()}
		if errors.Is(err, ErrNotLeader) {
			resp.LeaderHint = s.node.LeaderHint()
		}
		return resp, nil
	}
	return &internalpb.GetNetworkResponse{Network: network, Found: found}, nil
}

// ListNetworks implements internalpb.RaftInternalServer.
func (s *Server) ListNetworks(_ context.Context, _ *internalpb.ListNetworksRequest) (*internalpb.ListNetworksResponse, error) {
	networks, err := s.node.ListNetworks()
	if err != nil {
		resp := &internalpb.ListNetworksResponse{Error: err.Error()}
		if errors.Is(err, ErrNotLeader) {
			resp.LeaderHint = s.node.LeaderHint()
		}
		return resp, nil
	}
	return &internalpb.ListNetworksResponse{Networks: networks}, nil
}

// ListVMsLocal/ListNetworksLocal implement internalpb.RaftInternalServer -
// deliberately non-leader-restricted, see raftd.proto's doc comment.
func (s *Server) ListVMsLocal(_ context.Context, _ *internalpb.ListVMsRequest) (*internalpb.ListVMsResponse, error) {
	return &internalpb.ListVMsResponse{Vms: s.node.ListVMsLocal()}, nil
}

func (s *Server) ListNetworksLocal(_ context.Context, _ *internalpb.ListNetworksRequest) (*internalpb.ListNetworksResponse, error) {
	return &internalpb.ListNetworksResponse{Networks: s.node.ListNetworksLocal()}, nil
}

// GetPendingJoinRequestLocal/ListPendingJoinRequestsLocal implement
// internalpb.RaftInternalServer, mirroring ListNetworksLocal exactly -
// no leader check, no LeaderHint (see ADR-0083, and node.go's own doc
// comment on these two Node methods).
func (s *Server) GetPendingJoinRequestLocal(_ context.Context, req *internalpb.GetPendingJoinRequestRequest) (*internalpb.GetPendingJoinRequestResponse, error) {
	pendingReq, found := s.node.GetPendingJoinRequestLocal(req.GetRequestId())
	if !found {
		return &internalpb.GetPendingJoinRequestResponse{Error: fmt.Sprintf("join request %q not found", req.GetRequestId())}, nil
	}
	return &internalpb.GetPendingJoinRequestResponse{Request: pendingReq}, nil
}

func (s *Server) ListPendingJoinRequestsLocal(_ context.Context, _ *internalpb.ListPendingJoinRequestsRequest) (*internalpb.ListPendingJoinRequestsResponse, error) {
	return &internalpb.ListPendingJoinRequestsResponse{Requests: s.node.ListPendingJoinRequestsLocal()}, nil
}

// GetRestartLeaseStateLocal implements internalpb.RaftInternalServer
// (ADR-0103).
func (s *Server) GetRestartLeaseStateLocal(_ context.Context, req *internalpb.GetRestartLeaseStateRequest) (*internalpb.GetRestartLeaseStateResponse, error) {
	lease, record := s.node.RestartLeaseStateLocal(req.GetService())
	return &internalpb.GetRestartLeaseStateResponse{Lease: lease, Record: record}, nil
}

// StepAsideForRestartLocal implements internalpb.RaftInternalServer
// (ADR-0145). It is the wire form of Node.StepAsideForRestart, and the
// whole answer is safe_to_restart: everything else is evidence for
// whoever reads detail.
//
// Failures are reported in the response's error field rather than as a
// gRPC status, matching every other RPC in this service and for a
// concrete reason. A caller that only sees a transport error cannot tell
// "this node was the leader and could not hand over, do not restart it"
// from "the socket is down" - and those two demand opposite responses.
// Here both arrive together, in one answer, with the evidence attached.
func (s *Server) StepAsideForRestartLocal(ctx context.Context, req *internalpb.StepAsideForRestartRequest) (*internalpb.StepAsideForRestartResponse, error) {
	// One step-aside per node at a time. A second caller has to be told
	// the node is busy rather than left to collide with the first inside
	// the raft library, where the failure surfaces as a leadership
	// transfer already in progress - which reads like a cluster fault
	// rather than what it actually is, two coordinators asking at once.
	//
	// This is a per-node latch, not the colony-wide single-flight of
	// ADR-0145; that guarantee belongs to the update coordinator and is
	// not implemented yet. All this prevents is two callers knocking on
	// the same door.
	if !s.stepAsideMu.TryLock() {
		status := s.node.Status()
		return &internalpb.StepAsideForRestartResponse{
			NodeId:    status.NodeID,
			WasLeader: status.IsLeader,
			Detail: fmt.Sprintf(
				"%s already has a step-aside in flight, so this request was refused without touching raft state; "+
					"ask again once the first finishes", status.NodeID),
			Error: "step-aside already in progress on this node",
		}, nil
	}
	defer s.stepAsideMu.Unlock()

	timeout := DefaultStepAsideTimeout
	if req.GetTimeoutMs() > 0 {
		timeout = time.Duration(req.GetTimeoutMs()) * time.Millisecond
	}

	result, err := s.node.StepAsideForRestart(ctx, timeout)

	resp := &internalpb.StepAsideForRestartResponse{
		NodeId:        s.node.Status().NodeID,
		WasLeader:     result.WasLeader,
		Transferred:   result.Transferred,
		NewLeaderId:   result.NewLeaderID,
		SafeToRestart: result.SafeToRestart,
		Detail:        result.Detail,
	}
	if err != nil {
		resp.Error = err.Error()
		// Node.StepAsideForRestart already answers false on every error
		// path. Assert it again at the wire boundary: a response that
		// said "safe" and carried an error would be a licence to kill a
		// node that is still leading, and no future change to the node
		// method should be able to produce one.
		resp.SafeToRestart = false
	}
	return resp, nil
}

// GetJail implements internalpb.RaftInternalServer.
func (s *Server) GetJail(_ context.Context, req *internalpb.GetJailRequest) (*internalpb.GetJailResponse, error) {
	jail, found, err := s.node.GetJail(req.GetId())
	if err != nil {
		resp := &internalpb.GetJailResponse{Error: err.Error()}
		if errors.Is(err, ErrNotLeader) {
			resp.LeaderHint = s.node.LeaderHint()
		}
		return resp, nil
	}
	return &internalpb.GetJailResponse{Jail: jail, Found: found}, nil
}

// ListJails implements internalpb.RaftInternalServer.
func (s *Server) ListJails(_ context.Context, _ *internalpb.ListJailsRequest) (*internalpb.ListJailsResponse, error) {
	jails, err := s.node.ListJails()
	if err != nil {
		resp := &internalpb.ListJailsResponse{Error: err.Error()}
		if errors.Is(err, ErrNotLeader) {
			resp.LeaderHint = s.node.LeaderHint()
		}
		return resp, nil
	}
	return &internalpb.ListJailsResponse{Jails: jails}, nil
}

// ListJailsLocal implements internalpb.RaftInternalServer - deliberately
// non-leader-restricted, see raftd.proto's doc comment.
func (s *Server) ListJailsLocal(_ context.Context, _ *internalpb.ListJailsRequest) (*internalpb.ListJailsResponse, error) {
	return &internalpb.ListJailsResponse{Jails: s.node.ListJailsLocal()}, nil
}

// ValidateAPIKeyHash implements internalpb.RaftInternalServer. Unlike
// every other read RPC here, this never returns ErrNotLeader - see
// Node.ValidateAPIKeyHash's doc comment for why.
func (s *Server) ValidateAPIKeyHash(_ context.Context, req *internalpb.ValidateAPIKeyHashRequest) (*internalpb.ValidateAPIKeyHashResponse, error) {
	id, role, valid, authEnabled := s.node.ValidateAPIKeyHash(req.GetHashedKey())
	return &internalpb.ValidateAPIKeyHashResponse{Valid: valid, KeyId: id, AuthEnabled: authEnabled, Role: role}, nil
}

// ListAPIKeys implements internalpb.RaftInternalServer.
func (s *Server) ListAPIKeys(_ context.Context, _ *internalpb.ListAPIKeysRequest) (*internalpb.ListAPIKeysResponse, error) {
	keys, err := s.node.ListAPIKeys()
	if err != nil {
		resp := &internalpb.ListAPIKeysResponse{Error: err.Error()}
		if errors.Is(err, ErrNotLeader) {
			resp.LeaderHint = s.node.LeaderHint()
		}
		return resp, nil
	}
	return &internalpb.ListAPIKeysResponse{Keys: keys}, nil
}

func (s *Server) ExportState(_ context.Context, _ *internalpb.ExportStateRequest) (*internalpb.ExportStateResponse, error) {
	state, err := s.node.ExportState()
	if err != nil {
		resp := &internalpb.ExportStateResponse{Error: err.Error()}
		if errors.Is(err, ErrNotLeader) {
			resp.LeaderHint = s.node.LeaderHint()
		}
		return resp, nil
	}
	data, err := proto.Marshal(state)
	if err != nil {
		return &internalpb.ExportStateResponse{Error: err.Error()}, nil
	}
	return &internalpb.ExportStateResponse{
		FsmSnapshotState: data,
		AppliedIndex:     state.GetLastIndex(),
		NodeId:           s.node.Status().NodeID,
	}, nil
}
