// ADR-0156 Stage 2: managerd's view of this Colony's epoch.
//
// The value lives in REPLICATED state, not in managerd's configuration.
// That distinction is the whole point of the feature, and it is the
// same distinction ADR-0157 draws for the Colony ID: two independently
// bootstrapped raftd instances each form their own single-node cluster,
// and each would answer "which generation of which Colony am I in" from
// its own config file with a different value - which is exactly the
// condition ADR-0156 records as verified and undetectable. Read from
// replicated state, every voter of ONE Colony answers with the SAME
// value, and a node that has been superseded answers with a different
// one, so the difference becomes visible instead of silent.
//
// The rules that MATTER - how the epoch increments, and when a
// stale-epoch node is refused at admission - live in the FSM's own
// apply function (internal/raft/colonyepoch.go), for the reason every
// other replicated-field precedent in this codebase gives: a handler
// check is advisory, while the apply function runs in the same order on
// every replica, from the same committed log. This file is the RPC
// surface and the peer check, and it deliberately does not re-decide
// anything the FSM decides.

package manager

import (
	"context"
	"fmt"
	"time"

	"google.golang.org/protobuf/proto"

	internalpb "github.com/glenjbarber/apiary/api/internalpb"
	rpcpb "github.com/glenjbarber/apiary/api/rpc"
)

// localColonyEpoch returns this Colony's generation, or 0 if this node
// has none yet.
//
// The zero value is a REAL state, not an error: it means this node has
// applied no SetColonyEpoch, which is true of every node built before
// ADR-0156 Stage 2 and of a node restored from a pre-Stage-2 snapshot.
//
// The comparison that uses this value treats the two cases differently
// on purpose, and the difference is the safety property:
//
//   - A peer that STATES an epoch is compared against this Colony's. If
//     this Colony has no epoch either, the claim is refused - a peer
//     cannot be accepted as a member of an unnamed generation by
//     asserting a number, because there is nothing on the other side to
//     compare it against, and the check would be theatre.
//   - A peer that states NOTHING is accepted unchanged, exactly as every
//     pre-Stage-2 peer was. Such a member inherits the Colony's epoch
//     from the Colony rather than asserting one.
//
// A raftd that cannot be reached returns the zero value too. That is
// deliberately NOT distinguished here: the caller that needs to know
// "this Colony has no epoch" from "I could not find out" is the status
// path, which reports them separately, and a peer call that was
// refused because a transient raftd read failed would be refused for
// the wrong reason.
func (s *Server) localColonyEpoch(ctx context.Context) uint64 {
	if s.raft == nil {
		return 0
	}
	status, err := s.raft.Status(ctx)
	if err != nil {
		return 0
	}
	return status.GetColonyEpoch()
}

// checkPeerEpoch refuses a peer that is behind this Colony's
// generation, or ahead of it.
//
// This is the RPC-side half of ADR-0156's "the refusal of stale-epoch
// nodes at admission and on every RPC". It runs on the RECEIVING side
// of a peer call: the caller states its own epoch, and the callee
// compares it against its own replicated epoch.
//
// The rules:
//
//   - A peer stating an epoch BELOW this Colony's epoch is REFUSED.
//     This is the stale-epoch peer: a member of an older generation of
//     this Colony, which must not act on this one.
//   - A peer stating an epoch ABOVE this Colony's epoch is REFUSED, and
//     the refusal says so explicitly - that case means the RECEIVER is
//     the stale one, and the operator needs to know which side to fix.
//   - A peer stating NO epoch is ACCEPTED. Every peer built before this
//     record sends no epoch, and refusing those would break every
//     existing deployment's inter-node traffic for no safety gain.
//
// WHY THE REFUSAL LIVES HERE AND NOT IN THE FSM: the FSM is where a
// REPLICATED state change is decided, identically on every replica.
// This check is about a message arriving from outside the log, which
// never becomes replicated state at all - so there is no log entry for
// the FSM to refuse, and no replica to keep in agreement. Putting it
// here is the only place it can live.
//
// The refusal names BOTH epochs, so an operator reading it knows which
// side is behind rather than having to go and look.
func (s *Server) checkPeerEpoch(ctx context.Context, stated uint64) error {
	local := s.localColonyEpoch(ctx)

	// An absent claim is accepted, for the reason the doc comment
	// gives. It is checked BEFORE the local epoch, because a Colony
	// with no epoch yet (every node built before this record) must
	// still accept its existing peers.
	if stated == 0 {
		return nil
	}

	if local == 0 {
		return fmt.Errorf(
			"refusing this peer call: it states Colony epoch %d but this Colony has no epoch recorded yet, so there is nothing to compare it against. This node has applied no SetColonyEpoch - it predates ADR-0156 Stage 2, or it was restored from a snapshot that predates it",
			stated)
	}

	if stated < local {
		return fmt.Errorf(
			"refusing this peer call: it states Colony epoch %d but this Colony is at epoch %d. The peer is behind this Colony's generation and must be rebuilt from the Colony's current state rather than act on it; a stale-epoch node is not a member and cannot be made one by restarting it",
			stated, local)
	}
	if stated > local {
		return fmt.Errorf(
			"refusing this peer call: it states Colony epoch %d but this Colony is at epoch %d. The peer is AHEAD of this Colony's generation, which means THIS node is the stale one - rebuild this node from the Colony's current state before it acts on peer traffic again",
			stated, local)
	}
	return nil
}

// stampPeerEpoch sets the colony_epoch field on a request that is
// about to be forwarded to a peer, to this node's own replicated epoch.
//
// WHY THE CALLER STATES ITS OWN EPOCH: the callee can only know what
// the CALLER believes about the caller's own generation, and that is
// precisely the question a stale-epoch check has to ask. A peer that
// was partitioned away across a generation change still believes it is
// current - only a comparison against what the callee knows reveals
// the disagreement.
//
// The field is set ONLY when it is currently empty, for the same
// reason ApproveJoinRequest's approving_node_id is stamped only when
// empty: a forwarded request must still say what the ORIGINAL
// sender said, and a second forward must not overwrite the first
// sender's epoch with this node's own.
//
// An epoch of 0 is left as 0. That is the "no claim" value, and it is
// what a node with no epoch yet sends - which the callee accepts
// unchanged, for the reason checkPeerEpoch's own doc comment gives.
func (s *Server) stampPeerEpoch(ctx context.Context, req *rpcpb.ApproveJoinRequestRequest) {
	if req.GetColonyEpoch() != 0 {
		return
	}
	req.ColonyEpoch = s.localColonyEpoch(ctx)
}

func (s *Server) stampPeerEpochStatus(ctx context.Context, req *rpcpb.GetJoinRequestStatusRequest) {
	if req.GetColonyEpoch() != 0 {
		return
	}
	req.ColonyEpoch = s.localColonyEpoch(ctx)
}

func (s *Server) stampPeerEpochReject(ctx context.Context, req *rpcpb.RejectJoinRequestRequest) {
	if req.GetColonyEpoch() != 0 {
		return
	}
	req.ColonyEpoch = s.localColonyEpoch(ctx)
}

func (s *Server) stampPeerEpochCancel(ctx context.Context, req *rpcpb.CancelJoinRequestRequest) {
	if req.GetColonyEpoch() != 0 {
		return
	}
	req.ColonyEpoch = s.localColonyEpoch(ctx)
}

// AdvanceColonyEpoch implements rpcpb.ManagerServiceServer - moves this
// Colony to the next generation.
//
// The refusal itself lives in the FSM (an advance that is not exactly
// one step, or that names a Colony with no epoch, is refused BY NAME,
// naming both epochs, rather than clamped), so this handler's job is
// only to submit the command and report what the FSM decided.
//
// A rejection specifically for not being the leader is forwarded to the
// leader's own managerd, following the same pattern every other write
// RPC here uses.
func (s *Server) AdvanceColonyEpoch(ctx context.Context, req *rpcpb.AdvanceColonyEpochRequest) (*rpcpb.AdvanceColonyEpochResponse, error) {
	if s.raft == nil {
		return &rpcpb.AdvanceColonyEpochResponse{Error: "this node has no raft client configured"}, nil
	}

	// The current epoch is read here rather than being taken from the
	// caller, because the only valid target is the current epoch plus
	// one and the caller has no business naming it. The FSM re-checks
	// the same thing against the committed log, so a read that raced a
	// concurrent advance is refused there rather than silently
	// applying a wrong step.
	current := s.localColonyEpoch(ctx)
	if current == 0 {
		return &rpcpb.AdvanceColonyEpochResponse{Error: "this Colony has no epoch recorded yet, so there is nothing to advance; the epoch is established by the leader when it first takes leadership"}, nil
	}

	cmd := &internalpb.Command{
		Op: &internalpb.Command_AdvanceColonyEpoch{
			AdvanceColonyEpoch: &internalpb.AdvanceColonyEpoch{
				Epoch:     current + 1,
				SetBy:     s.nodeID,
				SetAtUnix: time.Now().Unix(),
			},
		},
	}
	timeout := defaultApplyTimeout
	if req.GetTimeoutMs() > 0 {
		timeout = time.Duration(req.GetTimeoutMs()) * time.Millisecond
	}
	payload, err := proto.Marshal(cmd)
	if err != nil {
		return &rpcpb.AdvanceColonyEpochResponse{Error: err.Error()}, nil
	}
	resp, err := s.raft.Apply(ctx, payload, timeout)
	if err != nil {
		return &rpcpb.AdvanceColonyEpochResponse{Error: err.Error()}, nil
	}
	if resp.GetError() != "" {
		var ferr error
		if resp.GetLeaderHint() != "" && s.peers != nil {
			var fwd *rpcpb.AdvanceColonyEpochResponse
			if fwd, ferr = s.peers.AdvanceColonyEpoch(ctx, s.peerManagerdAddr(resp.GetLeaderHint()), req); ferr == nil {
				return fwd, nil
			}
		}
		return &rpcpb.AdvanceColonyEpochResponse{Error: augmentForwardError(resp.GetError(), resp.GetLeaderHint(), ferr), LeaderHint: resp.GetLeaderHint()}, nil
	}

	result := &internalpb.ColonyEpoch{}
	if err := proto.Unmarshal(resp.GetResult(), result); err != nil {
		return &rpcpb.AdvanceColonyEpochResponse{Error: err.Error()}, nil
	}
	return &rpcpb.AdvanceColonyEpochResponse{Epoch: result.GetEpoch()}, nil
}
