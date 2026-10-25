package manager

import (
	"context"
	"fmt"
	"time"

	"google.golang.org/protobuf/proto"

	internalpb "github.com/glenjbarber/apiary/api/internalpb"
	rpcpb "github.com/glenjbarber/apiary/api/rpc"
)

// ADR-0148's Colony-wide VM disk-size floor, as managerd sees it.
//
// The floor's state lives in raft (internal/raft/colonydisksize.go) and
// the monotonicity check lives in the FSM's own apply function, so
// every replica refuses the same lowering in the same way. This file is
// only the RPC surface: it submits the command and reports the
// outcome.
//
// ADR-0153 depends on this RPC existing and working: the reconciler
// reads the floor on every Comb, and the per-VM and per-image sizes
// are only ever MAXIMA against it.

// SetColonyDiskSize implements rpcpb.ManagerServiceServer - raises
// ADR-0148's Colony-wide disk-size floor.
//
// The refusal itself lives in the FSM (a request below the current
// floor is refused BY NAME, naming both numbers, rather than clamped),
// so this handler's job is only to submit the command and report what
// the FSM decided.
//
// A rejection specifically for not being the leader is forwarded to
// the leader's own managerd, following the same pattern every other
// write RPC here uses.
func (s *Server) SetColonyDiskSize(ctx context.Context, req *rpcpb.SetColonyDiskSizeRequest) (*rpcpb.SetColonyDiskSizeResponse, error) {
	if req.GetFloorMb() == 0 {
		return &rpcpb.SetColonyDiskSizeResponse{Error: "floor_mb must be greater than zero; a floor of 0 is not a usable disk size"}, nil
	}

	cmd := &internalpb.Command{
		Op: &internalpb.Command_SetColonyDiskSize{
			SetColonyDiskSize: &internalpb.SetColonyDiskSize{
				FloorMb:  req.GetFloorMb(),
				SetBy:    s.nodeID,
				SetByKey: s.callerAPIKeyID(ctx),
				NowUnix:  time.Now().Unix(),
			},
		},
	}
	timeout := defaultApplyTimeout
	if req.GetTimeoutMs() > 0 {
		timeout = time.Duration(req.GetTimeoutMs()) * time.Millisecond
	}
	payload, err := proto.Marshal(cmd)
	if err != nil {
		return &rpcpb.SetColonyDiskSizeResponse{Error: err.Error()}, nil
	}
	resp, err := s.raft.Apply(ctx, payload, timeout)
	if err != nil {
		return &rpcpb.SetColonyDiskSizeResponse{Error: err.Error()}, nil
	}
	if resp.GetError() != "" {
		var ferr error
		if resp.GetLeaderHint() != "" && s.peers != nil {
			var fwd *rpcpb.SetColonyDiskSizeResponse
			if fwd, ferr = s.peers.SetColonyDiskSize(ctx, s.peerManagerdAddr(resp.GetLeaderHint()), req); ferr == nil {
				return fwd, nil
			}
		}
		return &rpcpb.SetColonyDiskSizeResponse{Error: augmentForwardError(resp.GetError(), resp.GetLeaderHint(), ferr), LeaderHint: resp.GetLeaderHint()}, nil
	}

	result := &internalpb.ColonyDiskSize{}
	if err := proto.Unmarshal(resp.GetResult(), result); err != nil {
		return &rpcpb.SetColonyDiskSizeResponse{Error: err.Error()}, nil
	}
	return &rpcpb.SetColonyDiskSizeResponse{ColonyDiskSize: fromInternalColonyDiskSize(result)}, nil
}

// GetColonyDiskSize implements rpcpb.ManagerServiceServer - reads
// ADR-0148's floor.
//
// It is deliberately NOT forwarded to the leader: the floor is
// replicated state, and every Comb reads it on every reconcile tick,
// so this node answers for its own copy rather than making a network
// hop per VM per tick.
func (s *Server) GetColonyDiskSize(ctx context.Context, _ *rpcpb.GetColonyDiskSizeRequest) (*rpcpb.GetColonyDiskSizeResponse, error) {
	if s.raft == nil {
		return &rpcpb.GetColonyDiskSizeResponse{Error: "this node has no raft client configured"}, nil
	}
	resp, err := s.raft.GetColonyDiskSizeLocal(ctx)
	if err != nil {
		return &rpcpb.GetColonyDiskSizeResponse{Error: err.Error()}, nil
	}
	return &rpcpb.GetColonyDiskSizeResponse{ColonyDiskSize: fromInternalColonyDiskSize(resp.GetColonyDiskSize())}, nil
}

// fromInternalColonyDiskSize converts the internal floor to its
// external counterpart.
//
// A nil internal record means this Colony has never had a floor set,
// which is NOT the same as a floor of zero - and the conversion
// preserves that distinction, because it is the only way the external
// surface can express "unset".
func fromInternalColonyDiskSize(s *internalpb.ColonyDiskSize) *rpcpb.ColonyDiskSize {
	if s == nil {
		return nil
	}
	return &rpcpb.ColonyDiskSize{
		FloorMb:   s.GetFloorMb(),
		SetBy:     s.GetSetBy(),
		SetByKey:  s.GetSetByKey(),
		SetAtUnix: s.GetSetAtUnix(),
	}
}

// SetColonyDiskSizeRequestValidationError describes why a floor
// request was refused before it reached raft. It exists so a caller
// can tell "you asked for something impossible" from "the Colony
// refused to lower its floor".
func SetColonyDiskSizeRequestValidationError(floorMB uint64) error {
	if floorMB == 0 {
		return fmt.Errorf("a Colony disk-size floor of 0 MB is not a usable disk size")
	}
	return nil
}
