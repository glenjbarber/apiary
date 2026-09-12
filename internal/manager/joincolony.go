// Package manager: the mutually-authorized Colony-join flow (ADR-0083),
// modeled on a device-pairing handshake. A joining Comb calls
// RequestJoinColony on one existing Colony member it names by address;
// that member raft-Applies a PendingJoinRequest so every current
// member's UI shows the same request regardless of which one first
// received the call. An Admin visually compares the returned code
// against the one shown on the joining Comb's own screen before calling
// ApproveJoinRequest, which is what actually calls AddVoter against
// this managerd's own local raftd.
package manager

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"math/big"
	"time"

	"google.golang.org/protobuf/proto"

	internalpb "github.com/glenjbarber/apiary/api/internalpb"
	rpcpb "github.com/glenjbarber/apiary/api/rpc"
)

// defaultJoinRequestTTL bounds how long a PendingJoinRequest stays
// actionable - checked lazily at read/apply time (see
// internal/raft/fsm.go's pendingJoinRequestExpired), not enforced by any
// background sweep.
const defaultJoinRequestTTL = 15 * time.Minute

// generateJoinRequestID returns a random, non-secret identifier for a
// new PendingJoinRequest - mirrors generateAPIKeyID's own shape
// (auth.go), a short hex id purely for correlating a joining Comb's own
// poll with the record an Admin approves/rejects.
func generateJoinRequestID() (string, error) {
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return "jreq-" + hex.EncodeToString(buf), nil
}

// generateJoinCode returns a 6-digit numeric code for visual
// correlation between the joining Comb's own screen and the Admin
// reviewing ListJoinRequests - not a secret (see ADR-0083's own
// disclosed trust model), so crypto/rand is used here purely to avoid
// a predictable sequence an operator might mistake for meaningful,
// not because the code needs to resist a determined attacker.
func generateJoinCode() (string, error) {
	n, err := rand.Int(rand.Reader, big.NewInt(1000000))
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%06d", n.Int64()), nil
}

// applyJoinRequestCommand mirrors applyNetworkCommand/applyJailCommand
// exactly (server.go/jail.go's own identical per-type Apply wrappers),
// unmarshaling raftd's ApplyResponse.Result as a PendingJoinRequest.
func (s *Server) applyJoinRequestCommand(ctx context.Context, cmd *internalpb.Command, timeoutMs uint32) (req *internalpb.PendingJoinRequest, appErr, leaderHint string) {
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

	req = &internalpb.PendingJoinRequest{}
	if err := proto.Unmarshal(resp.GetResult(), req); err != nil {
		return nil, err.Error(), ""
	}
	return req, "", ""
}

// RequestJoinColony implements rpcpb.ManagerServiceServer - see this
// file's own doc comment and ADR-0083. Deliberately exempted from
// checkAuth entirely (internal/manager/auth.go) since the caller has no
// Colony API key yet by definition.
//
// target_address (ADR-0092) is checked first: when set, this call is
// the JOINING Comb's own managerd asking the operator-named existing
// Colony member to record the request on ITS OWN raft instead - this
// managerd never raft-Applies anything itself in that case, it's purely
// a dial-and-relay. Empty target_address preserves the original
// ADR-0083 behavior below (record directly on whichever managerd
// receives the call).
func (s *Server) RequestJoinColony(ctx context.Context, req *rpcpb.RequestJoinColonyRequest) (*rpcpb.RequestJoinColonyResponse, error) {
	if req.GetNodeId() == "" || req.GetRaftBindAddress() == "" {
		return &rpcpb.RequestJoinColonyResponse{Error: "node_id and raft_bind_address must both be set"}, nil
	}
	if target := req.GetTargetAddress(); target != "" {
		if s.peers == nil {
			return &rpcpb.RequestJoinColonyResponse{Error: "no peer forwarding is configured on this node; cannot reach the named Colony member"}, nil
		}
		// Unauthenticated dial (ADR-0096): target is caller-supplied and
		// this RPC is itself deliberately never authenticated, so it must
		// never be dialed with this node's own shared -peer-api-key
		// attached - see PeerForwarder's own doc comment.
		resp, err := s.peers.RequestJoinColonyUnauthenticated(ctx, target, &rpcpb.RequestJoinColonyRequest{
			NodeId: req.GetNodeId(), RaftBindAddress: req.GetRaftBindAddress(), TimeoutMs: req.GetTimeoutMs(),
		})
		if err != nil {
			return &rpcpb.RequestJoinColonyResponse{Error: fmt.Sprintf("reaching %s: %v", target, err)}, nil
		}
		return resp, nil
	}
	requestID, err := generateJoinRequestID()
	if err != nil {
		return &rpcpb.RequestJoinColonyResponse{Error: fmt.Sprintf("generating request id: %v", err)}, nil
	}
	code, err := generateJoinCode()
	if err != nil {
		return &rpcpb.RequestJoinColonyResponse{Error: fmt.Sprintf("generating verification code: %v", err)}, nil
	}

	now := time.Now()
	cmd := &internalpb.Command{
		Op: &internalpb.Command_CreatePendingJoinRequest{
			CreatePendingJoinRequest: &internalpb.CreatePendingJoinRequest{
				Request: &internalpb.PendingJoinRequest{
					RequestId:       requestID,
					NodeId:          req.GetNodeId(),
					RaftBindAddress: req.GetRaftBindAddress(),
					Code:            code,
					RequestedAtUnix: now.Unix(),
					ExpiresAtUnix:   now.Add(defaultJoinRequestTTL).Unix(),
					Status:          internalpb.JoinRequestStatus_JOIN_REQUEST_STATUS_PENDING,
				},
			},
		},
	}
	result, appErr, leaderHint := s.applyJoinRequestCommand(ctx, cmd, req.GetTimeoutMs())
	if leaderHint != "" && s.peers != nil {
		if fwd, ferr := s.peers.RequestJoinColony(ctx, s.peerManagerdAddr(leaderHint), req); ferr == nil {
			return fwd, nil
		}
	}
	if appErr != "" {
		return &rpcpb.RequestJoinColonyResponse{Error: appErr, LeaderHint: leaderHint}, nil
	}
	return &rpcpb.RequestJoinColonyResponse{RequestId: result.GetRequestId(), Code: result.GetCode()}, nil
}

// GetJoinRequestStatus implements rpcpb.ManagerServiceServer - a local,
// non-leader-restricted read (ADR-0083: a joining Comb polls whichever
// specific existing member it originally contacted), also exempted
// from checkAuth entirely for the same reason RequestJoinColony is.
//
// target_address (ADR-0092) mirrors RequestJoinColony's own field: when
// set, this poll is forwarded to that address instead of reading this
// node's own local raft - needed because RequestJoinColony's own
// target_address means the request itself lives on a REMOTE Colony
// member, not here.
func (s *Server) GetJoinRequestStatus(ctx context.Context, req *rpcpb.GetJoinRequestStatusRequest) (*rpcpb.GetJoinRequestStatusResponse, error) {
	if target := req.GetTargetAddress(); target != "" {
		if s.peers == nil {
			return &rpcpb.GetJoinRequestStatusResponse{Error: "no peer forwarding is configured on this node; cannot reach the named Colony member"}, nil
		}
		// Unauthenticated dial (ADR-0096) - see RequestJoinColony's own
		// identical comment above.
		resp, err := s.peers.GetJoinRequestStatusUnauthenticated(ctx, target, req.GetRequestId())
		if err != nil {
			return &rpcpb.GetJoinRequestStatusResponse{Error: fmt.Sprintf("reaching %s: %v", target, err)}, nil
		}
		return resp, nil
	}
	resp, err := s.raft.GetPendingJoinRequestLocal(ctx, req.GetRequestId())
	if err != nil {
		return &rpcpb.GetJoinRequestStatusResponse{Error: err.Error()}, nil
	}
	if resp.GetError() != "" {
		return &rpcpb.GetJoinRequestStatusResponse{Error: resp.GetError()}, nil
	}
	return &rpcpb.GetJoinRequestStatusResponse{Request: fromInternalPendingJoinRequest(resp.GetRequest())}, nil
}

// ListJoinRequests implements rpcpb.ManagerServiceServer - Admin-only
// (internal/manager/auth.go), a local, non-leader-restricted read of
// every currently-Pending, not-yet-expired request across the whole
// Colony (raft-replicated, so this is the same list regardless of
// which member's UI it's viewed from).
func (s *Server) ListJoinRequests(ctx context.Context, _ *rpcpb.ListJoinRequestsRequest) (*rpcpb.ListJoinRequestsResponse, error) {
	resp, err := s.raft.ListPendingJoinRequestsLocal(ctx)
	if err != nil {
		return &rpcpb.ListJoinRequestsResponse{Error: err.Error()}, nil
	}
	requests := make([]*rpcpb.PendingJoinRequest, 0, len(resp.GetRequests()))
	for _, r := range resp.GetRequests() {
		requests = append(requests, fromInternalPendingJoinRequest(r))
	}
	return &rpcpb.ListJoinRequestsResponse{Requests: requests}, nil
}

// ApproveJoinRequest implements rpcpb.ManagerServiceServer - Admin-only.
// The handler that actually runs on the leader calls AddVoter against
// this managerd's own local raftd (RaftClient.AddVoter) before marking
// the request Approved - if AddVoter itself fails, the request is left
// Pending (not marked Approved) so the Admin can retry, rather than
// silently recording success for a membership change that didn't
// actually happen.
func (s *Server) ApproveJoinRequest(ctx context.Context, req *rpcpb.ApproveJoinRequestRequest) (*rpcpb.ApproveJoinRequestResponse, error) {
	getResp, err := s.raft.GetPendingJoinRequestLocal(ctx, req.GetRequestId())
	if err != nil {
		return &rpcpb.ApproveJoinRequestResponse{Error: err.Error()}, nil
	}
	if getResp.GetError() != "" {
		return &rpcpb.ApproveJoinRequestResponse{Error: getResp.GetError()}, nil
	}
	pending := getResp.GetRequest()

	timeout := defaultApplyTimeout
	if req.GetTimeoutMs() > 0 {
		timeout = time.Duration(req.GetTimeoutMs()) * time.Millisecond
	}
	addResp, err := s.raft.AddVoter(ctx, pending.GetNodeId(), pending.GetRaftBindAddress(), 0, timeout)
	if err != nil {
		return &rpcpb.ApproveJoinRequestResponse{Error: fmt.Sprintf("adding %q as a raft voter: %v", pending.GetNodeId(), err)}, nil
	}
	if addResp.GetError() != "" {
		if addResp.GetLeaderHint() != "" && s.peers != nil {
			if fwd, ferr := s.peers.ApproveJoinRequest(ctx, s.peerManagerdAddr(addResp.GetLeaderHint()), req); ferr == nil {
				return fwd, nil
			}
		}
		return &rpcpb.ApproveJoinRequestResponse{Error: addResp.GetError(), LeaderHint: addResp.GetLeaderHint()}, nil
	}

	cmd := &internalpb.Command{
		Op: &internalpb.Command_ApprovePendingJoinRequest{
			ApprovePendingJoinRequest: &internalpb.ApprovePendingJoinRequest{RequestId: req.GetRequestId()},
		},
	}
	result, appErr, leaderHint := s.applyJoinRequestCommand(ctx, cmd, req.GetTimeoutMs())
	if leaderHint != "" && s.peers != nil {
		if fwd, ferr := s.peers.ApproveJoinRequest(ctx, s.peerManagerdAddr(leaderHint), req); ferr == nil {
			return fwd, nil
		}
	}
	if appErr != "" {
		return &rpcpb.ApproveJoinRequestResponse{Error: appErr, LeaderHint: leaderHint}, nil
	}
	return &rpcpb.ApproveJoinRequestResponse{Request: fromInternalPendingJoinRequest(result)}, nil
}

// RejectJoinRequest implements rpcpb.ManagerServiceServer - Admin-only,
// the symmetric decline action. Never touches raft membership at all -
// just marks the record Rejected.
func (s *Server) RejectJoinRequest(ctx context.Context, req *rpcpb.RejectJoinRequestRequest) (*rpcpb.RejectJoinRequestResponse, error) {
	cmd := &internalpb.Command{
		Op: &internalpb.Command_RejectPendingJoinRequest{
			RejectPendingJoinRequest: &internalpb.RejectPendingJoinRequest{RequestId: req.GetRequestId()},
		},
	}
	result, appErr, leaderHint := s.applyJoinRequestCommand(ctx, cmd, req.GetTimeoutMs())
	if leaderHint != "" && s.peers != nil {
		if fwd, ferr := s.peers.RejectJoinRequest(ctx, s.peerManagerdAddr(leaderHint), req); ferr == nil {
			return fwd, nil
		}
	}
	if appErr != "" {
		return &rpcpb.RejectJoinRequestResponse{Error: appErr, LeaderHint: leaderHint}, nil
	}
	return &rpcpb.RejectJoinRequestResponse{Request: fromInternalPendingJoinRequest(result)}, nil
}

// CancelJoinRequest implements rpcpb.ManagerServiceServer - the
// requesting Comb's own self-service withdrawal of its still-pending
// request. Deliberately exempted from checkAuth entirely
// (internal/manager/auth.go), the same reason RequestJoinColony/
// GetJoinRequestStatus already are: the caller is this request's own
// creator, which by definition has no Colony API key yet. Knowledge of
// request_id is the only credential this needs, matching
// GetJoinRequestStatus's own already-established scoping.
//
// target_address (ADR-0092) mirrors RequestJoinColony/
// GetJoinRequestStatus's own field: when set, this cancel is forwarded
// there instead of acting on this node's own local raft, since the
// request itself may live on a remote Colony member.
func (s *Server) CancelJoinRequest(ctx context.Context, req *rpcpb.CancelJoinRequestRequest) (*rpcpb.CancelJoinRequestResponse, error) {
	if target := req.GetTargetAddress(); target != "" {
		if s.peers == nil {
			return &rpcpb.CancelJoinRequestResponse{Error: "no peer forwarding is configured on this node; cannot reach the named Colony member"}, nil
		}
		// Unauthenticated dial (ADR-0096) - see RequestJoinColony's own
		// identical comment above.
		resp, err := s.peers.CancelJoinRequestUnauthenticated(ctx, target, &rpcpb.CancelJoinRequestRequest{RequestId: req.GetRequestId(), TimeoutMs: req.GetTimeoutMs()})
		if err != nil {
			return &rpcpb.CancelJoinRequestResponse{Error: fmt.Sprintf("reaching %s: %v", target, err)}, nil
		}
		return resp, nil
	}
	cmd := &internalpb.Command{
		Op: &internalpb.Command_CancelPendingJoinRequest{
			CancelPendingJoinRequest: &internalpb.CancelPendingJoinRequest{RequestId: req.GetRequestId()},
		},
	}
	result, appErr, leaderHint := s.applyJoinRequestCommand(ctx, cmd, req.GetTimeoutMs())
	if leaderHint != "" && s.peers != nil {
		if fwd, ferr := s.peers.CancelJoinRequest(ctx, s.peerManagerdAddr(leaderHint), req); ferr == nil {
			return fwd, nil
		}
	}
	if appErr != "" {
		return &rpcpb.CancelJoinRequestResponse{Error: appErr, LeaderHint: leaderHint}, nil
	}
	return &rpcpb.CancelJoinRequestResponse{Request: fromInternalPendingJoinRequest(result)}, nil
}

// PurgeJoinRequest implements rpcpb.ManagerServiceServer - Admin-only,
// same tier as Approve/Reject/List. Removes the record outright
// regardless of its current status, for cleaning up a stale or
// erroneous entry that Approve/Reject/Cancel would otherwise keep
// forever. Idempotent, mirroring PurgeVM/PurgeJail's own posture - not
// an error if request_id is already gone.
func (s *Server) PurgeJoinRequest(ctx context.Context, req *rpcpb.PurgeJoinRequestRequest) (*rpcpb.PurgeJoinRequestResponse, error) {
	cmd := &internalpb.Command{
		Op: &internalpb.Command_PurgeJoinRequest{
			PurgeJoinRequest: &internalpb.PurgeJoinRequest{RequestId: req.GetRequestId()},
		},
	}
	_, appErr, leaderHint := s.applyJoinRequestCommand(ctx, cmd, req.GetTimeoutMs())
	if leaderHint != "" && s.peers != nil {
		if fwd, ferr := s.peers.PurgeJoinRequest(ctx, s.peerManagerdAddr(leaderHint), req); ferr == nil {
			return fwd, nil
		}
	}
	if appErr != "" {
		return &rpcpb.PurgeJoinRequestResponse{Error: appErr, LeaderHint: leaderHint}, nil
	}
	return &rpcpb.PurgeJoinRequestResponse{}, nil
}
