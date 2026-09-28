// ADR-0147 Part 4's Colony join window, as managerd sees it: the
// configured ceiling, the three RPCs, and the two gates that make the
// window mean something.
//
// The window's state lives in raft (internal/raft/colonyjoinwindow.go)
// and this file is the policy layer above it: what a caller may ask
// for, what the reply says about which number won, and - the part that
// actually changes behaviour - the two places a join is refused
// because no window is live.
//
// The two gates are deliberately both present. Gating only request
// creation would leave a request created inside a window approvable at
// any later moment, which makes the request the long-lived capability
// and the window the theatre. See each gate's own comment for what the
// caller is told, because "refusing" with no next step is how an
// operator ends up looking for a problem that is not the one they have.

package manager

import (
	"context"
	"fmt"
	"time"

	"google.golang.org/protobuf/proto"

	internalpb "github.com/glenjbarber/apiary/api/internalpb"
	rpcpb "github.com/glenjbarber/apiary/api/rpc"
	"github.com/glenjbarber/apiary/internal/nodeconfig"
	"github.com/glenjbarber/apiary/internal/raft"
)

// DefaultColonyJoinWindowSeconds re-exports the config package's own
// constant, so callers that already hold a *manager.Server do not have
// to import internal/nodeconfig to name the number they are being
// held to. One definition, two names.
const DefaultColonyJoinWindowSeconds = nodeconfig.DefaultColonyJoinWindowSeconds

// colonyJoinWindowSeconds is the effective ceiling for OpenColonyJoinWindow
// on THIS managerd, read from config. The leader's value is the one
// that decides, because the leader computes the deadline - see
// OpenColonyJoinWindow's own comment.
func (s *Server) colonyJoinWindowSeconds() int64 {
	if s.colonyJoinWindowSecondsSet > 0 {
		return s.colonyJoinWindowSecondsSet
	}
	return DefaultColonyJoinWindowSeconds
}

// liveColonyJoinWindow reads this member's own replicated copy of the
// window and reports whether it is live at this node's own clock.
//
// A read error is NOT a live window. Treating "I could not find out" as
// "yes" would make the gate fail open exactly when raftd is unhealthy,
// which is the opposite of what a fail-closed control is for; treating
// it as closed is safe because the cost is a refused join an operator
// retries, and a join is never urgent.
func (s *Server) liveColonyJoinWindow(ctx context.Context) (*internalpb.ColonyJoinWindow, bool) {
	if s.raft == nil {
		return nil, false
	}
	resp, err := s.raft.GetColonyJoinWindowLocal(ctx)
	if err != nil || resp.GetWindow() == nil {
		return nil, false
	}
	w := resp.GetWindow()
	return w, resp.GetLive()
}

// colonyWindowClosedRefusal is the one sentence every "there is no
// window" refusal ends with. It is a named constant rather than three
// similar strings because the next step is the part operators actually
// need, and an unauthenticated caller with no next step is a support
// problem (the same reasoning RequestJoinColony's own guardrail
// refusals already follow).
const colonyWindowClosedRefusal = "this Colony is not currently accepting new members: open the join window on a member (an Admin, on any member, 15 minutes by default) and have the joining Comb start again"

// callerAPIKeyID returns the caller's API key ID, or "" when auth is
// disabled or no key was presented. It is recorded on the window so
// "who opened this Colony to new members" is replicated state rather
// than a claim one managerd makes in memory.
//
// Resolving the ID here rather than threading it through the
// interceptor's context is deliberate: the same ValidateAPIKeyHash the
// auth check already performs is the authority, and re-deriving it from
// the same bearer token means the recorded identity and the identity
// that was authorised can never disagree.
func (s *Server) callerAPIKeyID(ctx context.Context) string {
	key, _ := extractBearerToken(ctx)
	if key == "" || s.raft == nil {
		return ""
	}
	resp, err := s.raft.ValidateAPIKeyHash(ctx, hashAPIKey(key))
	if err != nil || !resp.GetValid() {
		return ""
	}
	return resp.GetKeyId()
}

// applyColonyJoinWindowCommand mirrors applyJoinRequestCommand: marshal,
// Apply through raftd, and surface raftd's own application error (which
// is where every refusal in colonyjoinwindow.go actually happens) plus
// the leader hint, so a follower can forward on exactly the same signal
// every other Apply-backed write in this package uses.
func (s *Server) applyColonyJoinWindowCommand(ctx context.Context, cmd *internalpb.Command, timeoutMs uint32) (window *internalpb.ColonyJoinWindow, appErr, leaderHint string) {
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
	window = &internalpb.ColonyJoinWindow{}
	if err := proto.Unmarshal(resp.GetResult(), window); err != nil {
		return nil, err.Error(), ""
	}
	return window, "", ""
}

func toRPCColonyJoinWindow(w *internalpb.ColonyJoinWindow) *rpcpb.ColonyJoinWindow {
	if w == nil {
		return nil
	}
	return &rpcpb.ColonyJoinWindow{
		Enabled:       w.GetEnabled(),
		OpenedBy:      w.GetOpenedBy(),
		OpenedByKey:   w.GetOpenedByKey(),
		OpenedAtUnix:  w.GetOpenedAtUnix(),
		ExpiresAtUnix: w.GetExpiresAtUnix(),
	}
}

// OpenColonyJoinWindow implements rpcpb.ManagerServiceServer - ADR-0147
// Part 4's toggle. Admin-tier (it is a replicated state change) and
// forwardable: any member may be asked, the leader decides, and only
// the leader mutates.
//
// The requested duration is bounded by THIS member's configured value
// before anything is submitted, and bounded again by the LEADER's
// value inside the FSM. Two Combs configured differently is a
// legitimate state and the answer is deliberately not "they disagree,
// refuse" - the leader's number wins and the response says which
// number was used, via applied_duration_seconds. The pre-check here is
// not the enforcement; it is there so the common case (an operator on a
// member whose own file is the tighter one) is refused with a useful
// sentence instead of a round trip that the FSM would refuse anyway.
func (s *Server) OpenColonyJoinWindow(ctx context.Context, req *rpcpb.OpenColonyJoinWindowRequest) (*rpcpb.OpenColonyJoinWindowResponse, error) {
	ceiling := s.colonyJoinWindowSeconds()
	duration := req.GetDurationSeconds()
	if duration == 0 {
		duration = ceiling
	}
	if duration > ceiling {
		return &rpcpb.OpenColonyJoinWindowResponse{Error: fmt.Sprintf(
			"refusing a %d-second join window: this member's configured ceiling is %d seconds (colony_join_window_seconds in managerd.json); a window cannot be made longer than the configured value, and the leader's ceiling is what actually applies",
			duration, ceiling)}, nil
	}
	if duration <= 0 {
		return &rpcpb.OpenColonyJoinWindowResponse{Error: fmt.Sprintf(
			"duration_seconds must be positive, got %d", duration)}, nil
	}

	// Forward before applying, mirroring ApproveJoinRequest: only the
	// leader computes the deadline, so only the leader's answer is the
	// one that means anything.
	if status, statusErr := s.raft.Status(ctx); statusErr == nil && !status.GetIsLeader() {
		if hint := currentLeaderRaftAddress(status); hint != "" && s.peers != nil {
			if fwd, ferr := s.peers.OpenColonyJoinWindow(ctx, s.peerManagerdAddr(hint), req); ferr == nil {
				return fwd, nil
			}
		}
	}

	now := time.Now().Unix()
	window, appErr, leaderHint := s.applyColonyJoinWindowCommand(ctx, &internalpb.Command{
		Op: &internalpb.Command_OpenColonyJoinWindow{
			OpenColonyJoinWindow: &internalpb.OpenColonyJoinWindow{
				OpenedBy:           s.nodeID,
				OpenedByKey:        s.callerAPIKeyID(ctx),
				DurationSeconds:    duration,
				MaxDurationSeconds: ceiling,
				NowUnix:            now,
			},
		},
	}, req.GetTimeoutMs())
	if leaderHint != "" && s.peers != nil {
		if fwd, ferr := s.peers.OpenColonyJoinWindow(ctx, s.peerManagerdAddr(leaderHint), req); ferr == nil {
			return fwd, nil
		}
	}
	if appErr != "" {
		return &rpcpb.OpenColonyJoinWindowResponse{Error: appErr, LeaderHint: leaderHint}, nil
	}
	applied := window.GetExpiresAtUnix() - window.GetOpenedAtUnix()
	return &rpcpb.OpenColonyJoinWindowResponse{
		Window:                 toRPCColonyJoinWindow(window),
		AppliedDurationSeconds: applied,
	}, nil
}

// CloseColonyJoinWindow implements rpcpb.ManagerServiceServer - closing
// early, because an operator who changes their mind should not have to
// wait the window out. Closing one that is not open is a no-op rather
// than an error, so the UI's "close now" is safe to press on a window
// that expired a second earlier.
func (s *Server) CloseColonyJoinWindow(ctx context.Context, req *rpcpb.CloseColonyJoinWindowRequest) (*rpcpb.CloseColonyJoinWindowResponse, error) {
	if status, statusErr := s.raft.Status(ctx); statusErr == nil && !status.GetIsLeader() {
		if hint := currentLeaderRaftAddress(status); hint != "" && s.peers != nil {
			if fwd, ferr := s.peers.CloseColonyJoinWindow(ctx, s.peerManagerdAddr(hint), req); ferr == nil {
				return fwd, nil
			}
		}
	}

	window, appErr, leaderHint := s.applyColonyJoinWindowCommand(ctx, &internalpb.Command{
		Op: &internalpb.Command_CloseColonyJoinWindow{
			CloseColonyJoinWindow: &internalpb.CloseColonyJoinWindow{
				ClosedBy:    s.nodeID,
				ClosedByKey: s.callerAPIKeyID(ctx),
				NowUnix:     time.Now().Unix(),
			},
		},
	}, req.GetTimeoutMs())
	if leaderHint != "" && s.peers != nil {
		if fwd, ferr := s.peers.CloseColonyJoinWindow(ctx, s.peerManagerdAddr(leaderHint), req); ferr == nil {
			return fwd, nil
		}
	}
	if appErr != "" {
		return &rpcpb.CloseColonyJoinWindowResponse{Error: appErr, LeaderHint: leaderHint}, nil
	}
	return &rpcpb.CloseColonyJoinWindowResponse{Window: toRPCColonyJoinWindow(window)}, nil
}

// GetColonyJoinWindow implements rpcpb.ManagerServiceServer - ADR-0147
// Part 4's unauthenticated read, and the only pre-join trust anchor in
// the system.
//
// It is exempt from checkAuth because a Comb that has not joined has
// no Colony API key by definition, exactly as RequestJoinColony is. What
// makes that acceptable is that this RPC can only read: it returns the
// window and the Colony's own node IDs, it changes nothing, and it
// refuses outright once the window is not live - so the period during
// which an unauthenticated caller learns anything at all is a period an
// operator opened on purpose.
//
// The publication fields (colony_name, managerd_fingerprints) belong to
// the trust-store half of Part 4 and are filled in there; what is here
// is the window itself plus the two node IDs, both of which are
// already replicated.
func (s *Server) GetColonyJoinWindow(ctx context.Context, req *rpcpb.GetColonyJoinWindowRequest) (*rpcpb.GetColonyJoinWindowResponse, error) {
	if target := req.GetTargetAddress(); target != "" {
		if s.peers == nil {
			return &rpcpb.GetColonyJoinWindowResponse{Error: "no peer forwarding is configured on this node; cannot reach the named Colony member"}, nil
		}
		if err := s.checkTargetAddressAllowed(target); err != nil {
			return &rpcpb.GetColonyJoinWindowResponse{Error: err.Error()}, nil
		}
		fctx, cancel := context.WithTimeout(ctx, defaultUnauthenticatedForwardTimeout)
		defer cancel()
		resp, err := s.peers.GetColonyJoinWindowUnauthenticated(fctx, target, &rpcpb.GetColonyJoinWindowRequest{TimeoutMs: req.GetTimeoutMs()})
		if err != nil {
			return &rpcpb.GetColonyJoinWindowResponse{Error: fmt.Sprintf("reaching %s: %v", target, err)}, nil
		}
		return resp, nil
	}

	window, live := s.liveColonyJoinWindow(ctx)
	if !live {
		return &rpcpb.GetColonyJoinWindowResponse{Error: colonyWindowClosedRefusal}, nil
	}
	return &rpcpb.GetColonyJoinWindowResponse{
		Window:       toRPCColonyJoinWindow(window),
		ColonyNodeId: s.nodeID,
	}, nil
}

// requireLiveColonyJoinWindow is the approval-time gate, and the one
// that is checked FIRST in ApproveJoinRequest - before the PINs, before
// the authorization file, before anything else. It is the cheapest
// check available and the one an operator most needs explained, and it
// is a live check on the leader rather than a value read when a form
// was rendered: a window that expires while an approval form sits in a
// browser must not be approvable on the strength of when the form was
// drawn.
//
// It also refuses a request created under a previous window, which is
// what makes a reopen invalidate everything created before it.
func (s *Server) requireLiveColonyJoinWindow(ctx context.Context, requestID string) error {
	window, live := s.liveColonyJoinWindow(ctx)
	if !live {
		return fmt.Errorf("%s", colonyWindowClosedRefusal)
	}
	resp, err := s.raft.GetPendingJoinRequestLocal(ctx, requestID)
	if err != nil {
		return fmt.Errorf("reading join request %q: %v", requestID, err)
	}
	if resp.GetError() != "" {
		return fmt.Errorf("%s", resp.GetError())
	}
	if !raft.ColonyJoinWindowRequestCurrent(window, resp.GetRequest()) {
		return fmt.Errorf(
			"refusing to approve: join request %q was created under an earlier join window, and a reopen invalidates every request made under the previous one; %s",
			requestID, colonyWindowClosedRefusal)
	}
	return nil
}
