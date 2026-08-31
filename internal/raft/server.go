package raft

import (
	"context"
	"errors"
	"fmt"
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
	id, valid, authEnabled := s.node.ValidateAPIKeyHash(req.GetHashedKey())
	return &internalpb.ValidateAPIKeyHashResponse{Valid: valid, KeyId: id, AuthEnabled: authEnabled}, nil
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
