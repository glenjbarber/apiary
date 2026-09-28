package manager

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	internalpb "github.com/glenjbarber/apiary/api/internalpb"
	rpcpb "github.com/glenjbarber/apiary/api/rpc"
)

// The managerd side of ADR-0145's colony-wide controlled-update
// single-flight: the RPC that reaches the FSM decision, the
// leader-forwarding that makes "M different coordinators" mean what it
// says, and the fence that binds a controlled update's per-Comb restart
// leases to the operation that is actually running.
//
// WHAT IS DECIDED HERE AND WHAT IS NOT
//
// Nothing is decided here. Every grant, every recorded step and every
// settle is a raft Apply, and raft's serialized log-apply order is the
// single-flight (internal/raft/colonyupdate.go carries the full
// argument). This file's entire job is to get an op to the node that
// can commit it, to fail closed when it cannot, and to convert the
// refusal into something an operator or a coordinator can act on.
//
// Two properties of that job are worth stating because they are the ones
// a reviewer will want to check:
//
//   - Forwarding is leader-only, and the leader is never supplied by a
//     caller. It is derived from this node's own raft status, exactly as
//     reserveRestartLease derives it. ADR-0145's own hard constraint is
//     that the operator must never need to know who the leader is, and a
//     caller-supplied leader would put that knowledge straight back on
//     the wire.
//   - Every failure mode is a REFUSAL, never a degradation. An
//     unreadable raft status, a not-leader node with no reachable hint, a
//     failed forward, a nil raft client: all of them return an error and
//     none of them falls back to some local check. A single-flight that
//     silently weakens when the network is unhappy is not a single-
//     flight.

// MutateColonyUpdate implements rpcpb.ManagerServiceServer: the
// external, forwarded form of ADR-0145's acquire/advance/release.
//
// Token-gated exactly like ReserveRestartLease, ConfirmRestartCompleted
// and StepAsideForRestart, and for the same reason recorded in
// internal/manager/auth.go: claiming the Colony's single controlled
// update is a step in a cluster-wide restart, and must not be reachable
// by any CreateAPIKey-issued credential however privileged.
func (s *Server) MutateColonyUpdate(ctx context.Context, req *rpcpb.MutateColonyUpdateRequest) (*rpcpb.MutateColonyUpdateResponse, error) {
	presented, _ := extractBearerToken(ctx)
	if !restartGuardrailTokenValid(presented, s.restartGuardrailToken) {
		return nil, status.Error(codes.PermissionDenied, "invalid or missing restart-guardrail token")
	}
	return s.mutateColonyUpdate(ctx, req)
}

// mutateColonyUpdate is the internal plumbing, carrying no token check
// of its own for the same reason reserveRestartLease carries none: the
// only two callers are the token-checked method above and, in time, a
// trusted in-process coordinator. It is exported to no one.
func (s *Server) mutateColonyUpdate(ctx context.Context, req *rpcpb.MutateColonyUpdateRequest) (*rpcpb.MutateColonyUpdateResponse, error) {
	cmd, err := s.colonyUpdateCommand(req)
	if err != nil {
		return &rpcpb.MutateColonyUpdateResponse{Error: err.Error()}, nil
	}

	raftStatus, err := s.raftStatus(ctx)
	if err != nil {
		// Fail closed, and say so. An unreadable raft status is not a
		// weaker answer, it is no answer - and the one thing this RPC
		// must never do is report "free" when it does not know.
		return &rpcpb.MutateColonyUpdateResponse{Error: "refusing the controlled update: this node's own raft state could not be read, so whether the Colony is running one is unknown: " + err.Error()}, nil
	}
	if !raftStatus.GetIsLeader() {
		if hint := currentLeaderRaftAddress(raftStatus); hint != "" && s.peers != nil {
			fwd, ferr := s.peers.MutateColonyUpdate(ctx, s.peerManagerdAddr(hint), req)
			if ferr == nil {
				return fwd, nil
			}
			return &rpcpb.MutateColonyUpdateResponse{Error: augmentForwardError("not leader", hint, ferr)}, nil
		}
		return &rpcpb.MutateColonyUpdateResponse{
			Error:      "refusing the controlled update: this node is not the raft leader and no reachable leader hint is known, so nobody can commit the decision",
			LeaderHint: currentLeaderRaftAddress(raftStatus),
		}, nil
	}

	payload, err := proto.Marshal(cmd)
	if err != nil {
		return &rpcpb.MutateColonyUpdateResponse{Error: err.Error()}, nil
	}
	applyResp, err := s.raft.Apply(ctx, payload, defaultApplyTimeout)
	if err != nil {
		return &rpcpb.MutateColonyUpdateResponse{Error: "refusing the controlled update: the raft apply did not complete, so whether the Colony is running one is unknown: " + err.Error()}, nil
	}
	if applyResp.GetError() != "" {
		return &rpcpb.MutateColonyUpdateResponse{Error: applyResp.GetError(), LeaderHint: applyResp.GetLeaderHint()}, nil
	}
	var rec internalpb.ColonyUpdate
	if err := proto.Unmarshal(applyResp.GetResult(), &rec); err != nil {
		return &rpcpb.MutateColonyUpdateResponse{Error: err.Error()}, nil
	}
	return &rpcpb.MutateColonyUpdateResponse{
		Accepted: true,
		Update:   fromInternalColonyUpdate(&rec),
		Takeover: rec.GetTakeover(),
	}, nil
}

// colonyUpdateCommand converts the wire oneof into the internal Command,
// refusing anything malformed before it can become a raft proposal.
//
// The checks are here rather than in the FSM only so a caller gets a
// precise answer about its own request; the FSM re-checks everything
// that is a SAFETY property, and this file checks nothing that only it
// can see. An empty oneof is refused rather than treated as a no-op: a
// silent success is exactly what a caller would then read as "the
// colony is free".
func (s *Server) colonyUpdateCommand(req *rpcpb.MutateColonyUpdateRequest) (*internalpb.Command, error) {
	if req == nil {
		return nil, fmt.Errorf("no controlled-update operation was given")
	}
	switch op := req.GetOp().(type) {
	case *rpcpb.MutateColonyUpdateRequest_Acquire:
		a := op.Acquire
		if a.GetOperationId() == "" {
			return nil, fmt.Errorf("no operation id was given, so there is no operation to run and nothing a later reader could name")
		}
		if s.colonyUpdateIncarnation == "" {
			// A Server with no incarnation cannot be distinguished from
			// any other process on this Comb, so a fence could not fence
			// it. Refuse rather than issue a fence that does not fence.
			return nil, fmt.Errorf("this managerd has no controlled-update incarnation, so a fence it issued could not distinguish it from its predecessor")
		}
		return &internalpb.Command{Op: &internalpb.Command_AcquireColonyUpdate{
			AcquireColonyUpdate: &internalpb.AcquireColonyUpdate{
				OperationId:       a.GetOperationId(),
				HolderNodeId:      s.nodeID,
				HolderIncarnation: s.colonyUpdateIncarnation,
				// Authored here, on the leader, and never accepted from
				// the request - the same determinism rule
				// applyAcquireRestartLease follows.
				RequestedAtUnix: time.Now().Unix(),
				Takeover:        a.GetTakeover(),
			},
		}}, nil

	case *rpcpb.MutateColonyUpdateRequest_Advance:
		a := op.Advance
		if err := checkColonyUpdateFence(a.GetFence()); err != nil {
			return nil, err
		}
		return &internalpb.Command{Op: &internalpb.Command_AdvanceColonyUpdate{
			AdvanceColonyUpdate: &internalpb.AdvanceColonyUpdate{
				Fence:        toInternalColonyUpdateFence(a.GetFence()),
				Step:         a.GetStep(),
				TargetNodeId: a.GetTargetNodeId(),
				Detail:       a.GetDetail(),
				StepRecord:   toInternalColonyUpdateStep(a.GetStepRecord()),
			},
		}}, nil

	case *rpcpb.MutateColonyUpdateRequest_Release:
		r := op.Release
		if err := checkColonyUpdateFence(r.GetFence()); err != nil {
			return nil, err
		}
		return &internalpb.Command{Op: &internalpb.Command_ReleaseColonyUpdate{
			ReleaseColonyUpdate: &internalpb.ReleaseColonyUpdate{
				Fence:           toInternalColonyUpdateFence(r.GetFence()),
				Outcome:         r.GetOutcome(),
				Detail:          r.GetDetail(),
				CompletedAtUnix: time.Now().Unix(),
			},
		}}, nil

	case *rpcpb.MutateColonyUpdateRequest_Handover:
		h := op.Handover
		// The OUTGOING fence is validated for completeness here for the
		// same reason advance and release are: an incomplete fence can
		// never match exactly, and letting one through to be refused by
		// the FSM would replace a precise local explanation with a
		// leader-forwarded round trip that still says nothing useful.
		if err := checkColonyUpdateFence(h.GetFromFence()); err != nil {
			return nil, err
		}
		return &internalpb.Command{Op: &internalpb.Command_HandoverColonyUpdate{
			HandoverColonyUpdate: &internalpb.HandoverColonyUpdate{
				FromFence:           toInternalColonyUpdateFence(h.GetFromFence()),
				ToNodeId:            h.GetToNodeId(),
				ToHolderIncarnation: h.GetToHolderIncarnation(),
				Reason:              h.GetReason(),
				RequestedAtUnix:     time.Now().Unix(),
			},
		}}, nil

	default:
		return nil, fmt.Errorf("the request carried no acquire, advance, release or handover, so nothing was asked for")
	}
}

// checkColonyUpdateFence refuses a fence that is missing any of its four
// parts, before it can be proposed.
//
// All four are required, and none of the four is decorative. See
// api/internalpb/state.proto's ColonyUpdateFence for the full argument;
// what matters here is that a partially-populated fence is refused
// rather than passed along, because the FSM's exact-match test would
// reject it anyway and the caller's only clue would be a raft log index.
func checkColonyUpdateFence(f *rpcpb.ColonyUpdateFence) error {
	if f == nil {
		return fmt.Errorf("refusing the controlled update: no fence was presented, and every advance and release must present the one it was granted")
	}
	var missing []string
	if f.GetOperationId() == "" {
		missing = append(missing, "operation_id")
	}
	if f.GetHolderNodeId() == "" {
		missing = append(missing, "holder_node_id")
	}
	if f.GetFenceToken() == 0 {
		missing = append(missing, "fence_token")
	}
	if f.GetHolderIncarnation() == "" {
		missing = append(missing, "holder_incarnation")
	}
	if len(missing) > 0 {
		return fmt.Errorf("refusing the controlled update: the fence is incomplete, missing %s - a fence that names only part of the holder cannot be matched exactly, and a partial match is not a match",
			strings.Join(missing, ", "))
	}
	return nil
}

// ColonyUpdateIncarnation is this managerd process's own identity in
// ADR-0145's fence: a fresh random value at every startup, never
// derived from anything durable.
//
// It has to be per PROCESS rather than per node or per host, and that
// is the whole reason the fence has four parts. A Comb whose managerd
// is replaced has the same node_id before and after, so a fence keyed on
// node_id alone would still be satisfied by the process it replaced -
// which is precisely the resurrected-old-coordinator case. The
// predecessor's token stays valid forever, because nothing ever revokes
// it; what invalidates it is that a NEW grant records a strictly higher
// fence token, and the old one can never match again.
func ColonyUpdateIncarnation() string {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		// crypto/rand failing is not a condition this codebase has
		// anywhere else, and it must not be papered over with a
		// predictable fallback: a predictable incarnation would make the
		// fence forgeable by anyone who can guess it. Refusing to
		// acquire is the safe direction, and mutateColonyUpdate's
		// empty-incarnation check turns that into a clean refusal.
		return ""
	}
	return hex.EncodeToString(buf[:])
}

// toInternalColonyUpdateFence / fromInternalColonyUpdateFence are exact
// field-for-field translations. There is deliberately no defaulting and
// no normalisation: a translation that quietly filled a field in would
// be a place where the wire and the FSM could disagree about what a
// fence says, and the fence is the thing that must not be ambiguous.
func toInternalColonyUpdateFence(f *rpcpb.ColonyUpdateFence) *internalpb.ColonyUpdateFence {
	if f == nil {
		return nil
	}
	return &internalpb.ColonyUpdateFence{
		OperationId:       f.GetOperationId(),
		HolderNodeId:      f.GetHolderNodeId(),
		FenceToken:        f.GetFenceToken(),
		HolderIncarnation: f.GetHolderIncarnation(),
	}
}

func toInternalColonyUpdateStep(s *rpcpb.ColonyUpdateStepRecord) *internalpb.ColonyUpdateStepRecord {
	if s == nil {
		return nil
	}
	return &internalpb.ColonyUpdateStepRecord{
		Index:    s.GetIndex(),
		Step:     s.GetStep(),
		Outcome:  s.GetOutcome(),
		Detail:   s.GetDetail(),
		Evidence: append([]string(nil), s.GetEvidence()...),
		NodeId:   s.GetNodeId(),
	}
}

// fromInternalColonyUpdate renders an FSM record onto the wire.
func fromInternalColonyUpdate(rec *internalpb.ColonyUpdate) *rpcpb.ColonyUpdate {
	if rec == nil {
		return nil
	}
	steps := make([]*rpcpb.ColonyUpdateStepRecord, 0, len(rec.GetSteps()))
	for _, s := range rec.GetSteps() {
		steps = append(steps, &rpcpb.ColonyUpdateStepRecord{
			Index:    s.GetIndex(),
			Step:     s.GetStep(),
			Outcome:  s.GetOutcome(),
			Detail:   s.GetDetail(),
			Evidence: append([]string(nil), s.GetEvidence()...),
			NodeId:   s.GetNodeId(),
		})
	}
	// Handovers are carried whole, and a copy rather than an alias: the
	// record here comes from a snapshot the FSM will keep mutating in
	// place, so handing out its slices would be the same live-pointer bug
	// ColonyUpdateState already had to be fixed for.
	handovers := make([]*rpcpb.ColonyUpdateHandover, 0, len(rec.GetHandovers()))
	for _, h := range rec.GetHandovers() {
		handovers = append(handovers, &rpcpb.ColonyUpdateHandover{
			FromNodeId:      h.GetFromNodeId(),
			FromIncarnation: h.GetFromIncarnation(),
			FromFenceToken:  h.GetFromFenceToken(),
			ToNodeId:        h.GetToNodeId(),
			ToIncarnation:   h.GetToIncarnation(),
			ToFenceToken:    h.GetToFenceToken(),
			Reason:          h.GetReason(),
		})
	}
	return &rpcpb.ColonyUpdate{
		OperationId:       rec.GetOperationId(),
		HolderNodeId:      rec.GetHolderNodeId(),
		HolderIncarnation: rec.GetHolderIncarnation(),
		FenceToken:        rec.GetFenceToken(),
		GrantedAtUnix:     rec.GetGrantedAtUnix(),
		TargetNodeId:      rec.GetTargetNodeId(),
		Step:              rec.GetStep(),
		Active:            rec.GetActive(),
		Outcome:           rec.GetOutcome(),
		Detail:            rec.GetDetail(),
		SettledAtUnix:     rec.GetSettledAtUnix(),
		Takeover:          rec.GetTakeover(),
		Steps:             steps,
		UpdatedAtUnix:     rec.GetUpdatedAtUnix(),
		Handovers:         handovers,
	}
}

// colonyUpdateState is the read side, for a replacement managerd asking
// "what is the state of the controlled update?".
//
// It reads this node's OWN raftd copy and never forwards, which is
// deliberate and correct for the question actually being asked: the
// process that died was this one, on this Comb, so this Comb's raftd is
// the thing that has to answer. Forwarding to the leader would make the
// answer depend on a network hop that is exactly what may have just
// failed.
//
// authoritative is reported, never assumed. A follower's copy of an
// applied record can lag the leader's by one commit, and a reader that
// cannot tell the two apart would be reading a stale lock as a live one.
func (s *Server) colonyUpdateState(ctx context.Context, operationID string) (*internalpb.GetColonyUpdateStateResponse, error) {
	if s.raft == nil {
		// The nil-raft case ADR-0125 already found and fixed on the
		// guardrail path, where it used to panic rather than refuse. Same
		// posture here and for the same reason: "this build cannot tell
		// you" must never look like "there is nothing running".
		return nil, fmt.Errorf("refusing to report the controlled-update state: this node has no raft client configured, so nothing is known and nothing is free")
	}
	return s.raft.GetColonyUpdateStateLocal(ctx, operationID)
}
