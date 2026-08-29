package raft

import (
	"context"
	"errors"
	"time"

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

	return &internalpb.ApplyResponse{Result: result.Payload}, nil
}

// Status implements internalpb.RaftInternalServer.
func (s *Server) Status(_ context.Context, _ *internalpb.StatusRequest) (*internalpb.StatusResponse, error) {
	status := s.node.Status()
	return &internalpb.StatusResponse{
		IsLeader:     status.IsLeader,
		LeaderId:     status.LeaderID,
		NodeId:       status.NodeID,
		LastLogIndex: status.LastLogIndex,
		AppliedIndex: status.AppliedIndex,
		RaftState:    status.RaftState,
	}, nil
}
