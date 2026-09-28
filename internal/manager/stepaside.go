package manager

import (
	"context"
	"fmt"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	internalpb "github.com/glenjbarber/apiary/api/internalpb"
	rpcpb "github.com/glenjbarber/apiary/api/rpc"
	"github.com/glenjbarber/apiary/internal/restartplan"
)

// This file is the managerd side of ADR-0145's confirmed leadership
// step-aside: the one place that knows how to make an arbitrary Comb in
// the Colony give up raft leadership, and report back whether it is
// confirmed safe to restart.
//
// WHY THIS FILE EXISTS AT ALL
//
// The mechanism itself (internal/raft.Node.StepAsideForRestart) and its
// raftd-side RPC (raftd.proto's StepAsideForRestartLocal) were both
// local-only by design, and both are still correct that way. The
// problem they cannot solve is locality: raftd's internal API is a Unix
// domain socket, so a managerd running on coordinator node X physically
// cannot invoke it on node Y. Without a cross-node reach, the
// controlled update workflow could only ever step aside the Comb it
// happened to be running on - which is precisely the case where the
// operator most needs to know, because it may be the leader.
//
// The reach added here has three parts, and each is deliberately
// narrow:
//
//   - manager.proto's StepAsideForRestart, a target-local managerd RPC
//     that performs the step on the node that receives it. It is never
//     forwarded to the leader: asking the leader to stand down on behalf
//     of a different node is asking the wrong machine.
//   - PeerReporter.StepAsideForRestart, which carries that RPC to a peer
//     using the restart-guardrail token, not the ordinary peer API key.
//   - restartStepAsider below, which picks between the local socket and
//     a peer hop, resolves the peer's address from raft membership, and
//     fails closed on every uncertainty.
//
// WHAT IS DELIBERATELY NOT HERE
//
// This file is a reach, not a coordinator. It does not sequence Combs,
// does not hold a colony-wide single-flight lock, does not persist
// progress, and does not survive the managerd it lives in being the
// process that gets restarted. Those are real, unsolved problems - see
// ADR-0145's own open questions, and internal/restartplan's integration
// notes - and none of them is made smaller by pretending this file
// solves them. What it does do is make the step-aside reachable from
// off-node, which is the prerequisite for all of them.

// stepAsideRaft is the subset of *RaftClient the local hop needs: a
// membership read to resolve a peer's address, and the local step-aside
// itself. Defined as an interface so the whole adapter is testable
// without a raft node, the same reasoning isoManager/VNCLookup and
// quotaSetter already follow elsewhere in this package.
type stepAsideRaft interface {
	Status(ctx context.Context) (*internalpb.StatusResponse, error)
	StepAsideForRestartLocal(ctx context.Context, timeoutMs uint64) (*internalpb.StepAsideForRestartResponse, error)
}

// stepAsideForwarder is the subset of PeerForwarder the remote hop
// needs. PeerForwarder embeds it, so a *PeerReporter satisfies it
// directly, and a test needs to supply only this one method rather than
// stubbing the full forwarding surface.
type stepAsideForwarder interface {
	StepAsideForRestart(ctx context.Context, addr string, req *rpcpb.StepAsideForRestartRequest) (*rpcpb.StepAsideForRestartResponse, error)
}

// DefaultStepAsideForwardTimeout is how long the manager-side step-aside
// waits, in total, for one Comb to confirm it stood down. It is
// comfortably above raftnode.DefaultStepAsideTimeout (15s) so a peer's
// own internal transfer gets to use its full default before the
// forwarding hop starts reporting timeouts of its own - the peer is the
// authority on whether the handover happened, and this deadline only
// bounds how long the coordinator is willing to wait for the answer.
const DefaultStepAsideForwardTimeout = 30 * time.Second

// restartStepAsider implements restartplan.StepAside against real
// managerd/raftd plumbing: the local node's own raftd socket for itself,
// a token-authenticated peer hop for everyone else.
type restartStepAsider struct {
	// localNodeID is this Comb's own id. A step-aside request naming it
	// is served locally, never over the network.
	localNodeID string

	// raft is the local raftd client. nil on a node with no raft client
	// configured, which every path below treats as a refusal.
	raft stepAsideRaft

	// peers forwards a step-aside to another Comb's managerd. nil on a
	// node with no peer forwarder configured; a remote target is then
	// unreachable and the step-aside refuses.
	peers stepAsideForwarder

	// managerdPort is the port substituted into a member's raft
	// transport address to reach that member's managerd - empty uses
	// defaultPeerManagerdPort, matching Server.peerManagerdAddr.
	managerdPort string

	// timeout bounds one whole step-aside attempt. zero uses
	// DefaultStepAsideForwardTimeout.
	timeout time.Duration
}

// newRestartStepAsider wires Server's own dependencies into the
// adapter. It returns nil when there is nothing to wire, and a nil
// StepAsider is safe in restartplan: the step is skipped and the
// leader-restart guardrail still blocks a leader-target restart behind
// an explicit operator acknowledgment. That is the correct degraded
// posture - a missing reach must fall back to the old conservative
// behavior, never to an unconfirmed restart.
func (s *Server) newRestartStepAsider() restartplan.StepAside {
	if s.stepAsideClient() == nil {
		return nil
	}
	return &restartStepAsider{
		localNodeID:  s.nodeID,
		raft:         s.stepAsideClient(),
		peers:        s.peers,
		managerdPort: s.peerManagerdPort,
	}
}

// stepAsideClient is the raftd client the step-aside uses: the real one
// in production, or the test seam Server.stepAsideRaft when a test set
// it. Both nil cases are meaningful and both are refusals - a nil
// interface here would otherwise become a nil-pointer dereference the
// first time a Comb was about to be restarted, which is the worst
// possible moment to discover it (see Server.raftStatus's own doc
// comment for the same reasoning on the guardrail path).
func (s *Server) stepAsideClient() stepAsideRaft {
	if s.stepAsideRaft != nil {
		return s.stepAsideRaft
	}
	if s.raft == nil {
		return nil
	}
	return s.raft
}

// stepAsideTimeout is the configured deadline, or the default.
func (a *restartStepAsider) stepAsideTimeout() time.Duration {
	if a.timeout > 0 {
		return a.timeout
	}
	return DefaultStepAsideForwardTimeout
}

// StepAsideForRestart makes nodeID step aside and reports whether it is
// confirmed safe to restart. It implements restartplan.StepAside.
//
// Every failure mode here returns SafeToRestart false, and the ones
// that are genuinely transport-level failures also return a non-nil
// error. Both matter: restartplan refuses the run on either, and its
// refusal is deliberately not overridable by Force, because an
// unconfirmed handover means the node may still be leader and no
// operator acknowledgment can turn that unknown into a known.
func (a *restartStepAsider) StepAsideForRestart(ctx context.Context, nodeID string) (restartplan.StepAsideOutcome, error) {
	if nodeID == "" {
		return restartplan.StepAsideOutcome{Detail: "no target node was named, so there is no node that can be asked to step aside"}, fmt.Errorf("refusing the step-aside: no target node id was given")
	}
	if a == nil || a.raft == nil {
		// Unreachable in a correctly configured deployment, and exactly
		// the case that must fail closed rather than dereference -
		// see Server.raftStatus's own doc comment for why that
		// distinction is load-bearing on the guardrail path.
		return restartplan.StepAsideOutcome{Detail: "this node has no raft client configured, so no Comb's leadership state can be read or changed from here"}, fmt.Errorf("refusing the step-aside on %s: no raft client configured", nodeID)
	}

	ctx, cancel := context.WithTimeout(ctx, a.stepAsideTimeout())
	defer cancel()

	if nodeID == a.localNodeID {
		return a.stepAsideLocal(ctx, nodeID)
	}
	return a.stepAsideRemote(ctx, nodeID)
}

// stepAsideLocal serves the step-aside for this node's own Comb, over
// the local raftd socket - the same call a co-located caller would make,
// with no network hop and no address resolution to go wrong.
func (a *restartStepAsider) stepAsideLocal(ctx context.Context, nodeID string) (restartplan.StepAsideOutcome, error) {
	resp, err := a.raft.StepAsideForRestartLocal(ctx, uint64(a.stepAsideTimeout()/time.Millisecond))
	if err != nil {
		return restartplan.StepAsideOutcome{
			Detail: fmt.Sprintf("could not reach this node's own raftd to step %s aside: %v", nodeID, err),
		}, fmt.Errorf("stepping %s aside over the local raftd socket: %w", nodeID, err)
	}
	// raftd's own handler always names itself; a response that does not
	// is either a different node's answer or a stub, and either way it
	// cannot speak for the node about to be restarted.
	if got := resp.GetNodeId(); got != "" && got != a.localNodeID {
		detail := fmt.Sprintf("the local raftd answered the step-aside naming itself %q rather than %q, so its answer cannot speak for the target", got, nodeID)
		return restartplan.StepAsideOutcome{SafeToRestart: false, Detail: detail}, fmt.Errorf("%s", detail)
	}
	return stepAsideOutcomeFromInternal(nodeID, resp), nil
}

// stepAsideRemote forwards the step-aside to the named Comb's own
// managerd, which performs it on that node's raftd.
//
// The address is resolved from this node's own raft membership, never
// from anything a caller supplied. That is the same posture
// PreflightApproveJoinRequest and the other forwarding RPCs take, and
// it is what keeps this from becoming a way to aim managerd's
// token-authenticated step-aside at an arbitrary host.
func (a *restartStepAsider) stepAsideRemote(ctx context.Context, nodeID string) (restartplan.StepAsideOutcome, error) {
	addr, err := a.managerdAddrFor(ctx, nodeID)
	if err != nil {
		return restartplan.StepAsideOutcome{
			Detail: fmt.Sprintf("cannot ask %s to step aside: %v", nodeID, err),
		}, err
	}
	if a.peers == nil {
		return restartplan.StepAsideOutcome{
			Detail: fmt.Sprintf("cannot ask %s to step aside: this node has no peer forwarder configured, so no other Comb is reachable", nodeID),
		}, fmt.Errorf("refusing the step-aside on %s: no peer forwarder configured", nodeID)
	}

	resp, err := a.peers.StepAsideForRestart(ctx, addr, &rpcpb.StepAsideForRestartRequest{
		TargetNodeId: nodeID,
		TimeoutMs:    uint64(a.stepAsideTimeout() / time.Millisecond),
	})
	if err != nil {
		return restartplan.StepAsideOutcome{
			Detail: fmt.Sprintf("could not reach %s at %s to ask it to step aside: %v", nodeID, addr, err),
		}, fmt.Errorf("forwarding the step-aside for %s to %s: %w", nodeID, addr, err)
	}
	if got := resp.GetNodeId(); got != nodeID {
		// The response names the node that answered. A mismatch means
		// the request was satisfied by a machine other than the one
		// about to be restarted, so its answer says nothing about the
		// target. Reporting it rather than using the answer anyway is
		// the difference between an honest refusal and a restart that
		// believed it had checked leadership it never checked.
		detail := fmt.Sprintf("the node that answered the step-aside for %s reports itself as %q, so its answer cannot speak for the target", nodeID, got)
		if got == "" {
			detail = fmt.Sprintf("the node that answered the step-aside for %s named no node id, so its answer cannot be attributed to the target", nodeID)
		}
		return restartplan.StepAsideOutcome{SafeToRestart: false, Detail: detail},
			fmt.Errorf("%s", detail)
	}
	return stepAsideOutcomeFromRPC(nodeID, resp), nil
}

// managerdAddrFor resolves a member's managerd address from this node's
// own view of raft membership, and hands the lookup itself to
// managerdAddrFromStatus below so the step-aside and
// ExecuteNodeRestartPlan's forward share one fail-closed implementation.
func (a *restartStepAsider) managerdAddrFor(ctx context.Context, nodeID string) (string, error) {
	st, err := a.raft.Status(ctx)
	if err != nil {
		return "", fmt.Errorf("cluster membership could not be read, so the target's address is unknown: %w", err)
	}
	return managerdAddrFromStatus(st, nodeID, a.managerdPort)
}

// managerdAddrFromStatus is the membership-to-address step on its own, so
// that both the step-aside's peer hop above and ExecuteNodeRestartPlan's
// forward (ADR-0145) resolve a member's managerd address the one way.
// Two copies of a fail-closed lookup are two places to get the fail-closed
// cases wrong, and these cases - a member with no transport address, a
// member nobody has heard of - are exactly the ones that must be
// refusals rather than a dial to a guessed host.
//
// An unreadable membership and a member this node has never heard of are
// reported differently because they mean different things operationally:
// the first is this node's raftd being unreachable, the second is a
// genuinely unknown Comb.
func managerdAddrFromStatus(st *internalpb.StatusResponse, nodeID, managerdPort string) (string, error) {
	for _, srv := range st.GetServers() {
		if srv.GetId() != nodeID {
			continue
		}
		if srv.GetAddress() == "" {
			return "", fmt.Errorf("cluster membership lists %s with no raft transport address, so its managerd address cannot be derived", nodeID)
		}
		return peerManagerdAddrOf(srv.GetAddress(), managerdPort), nil
	}
	return "", fmt.Errorf("cluster membership does not list %s as a member", nodeID)
}

// stepAsideOutcomeFromInternal maps a raftd-local response onto
// restartplan's outcome.
//
// The error field is authoritative and forces SafeToRestart false even
// if the wire said otherwise: raftd's own server already enforces that
// at its boundary, and re-checking it here means a future raftd build
// that got it wrong still cannot authorize a restart on this side.
func stepAsideOutcomeFromInternal(nodeID string, resp *internalpb.StepAsideForRestartResponse) restartplan.StepAsideOutcome {
	out := restartplan.StepAsideOutcome{
		SafeToRestart: resp.GetSafeToRestart(),
		WasLeader:     resp.GetWasLeader(),
		Transferred:   resp.GetTransferred(),
		NewLeaderID:   resp.GetNewLeaderId(),
		Detail:        resp.GetDetail(),
	}
	if msg := resp.GetError(); msg != "" {
		out.SafeToRestart = false
		out.Detail = stepAsideDetailWithError(nodeID, msg, out.Detail)
	}
	if out.Detail == "" {
		out.Detail = stepAsideDetailFallback(nodeID, out)
	}
	return out
}

// stepAsideOutcomeFromRPC maps a peer managerd's response onto
// restartplan's outcome, with the same error-is-authoritative rule.
func stepAsideOutcomeFromRPC(nodeID string, resp *rpcpb.StepAsideForRestartResponse) restartplan.StepAsideOutcome {
	out := restartplan.StepAsideOutcome{
		SafeToRestart: resp.GetSafeToRestart(),
		WasLeader:     resp.GetWasLeader(),
		Transferred:   resp.GetTransferred(),
		NewLeaderID:   resp.GetNewLeaderId(),
		Detail:        resp.GetDetail(),
	}
	if msg := resp.GetError(); msg != "" {
		out.SafeToRestart = false
		out.Detail = stepAsideDetailWithError(nodeID, msg, out.Detail)
	}
	if out.Detail == "" {
		out.Detail = stepAsideDetailFallback(nodeID, out)
	}
	return out
}

// stepAsideDetailWithError keeps both halves of a failed step-aside: the
// node's own explanation of what went wrong, and the fact that this
// answer is a refusal. Returning only the first would let it read like
// a successful step that merely had a footnote.
func stepAsideDetailWithError(nodeID, msg, detail string) string {
	if detail == "" {
		return fmt.Sprintf("%s did not confirm it is safe to restart: %s", nodeID, msg)
	}
	return fmt.Sprintf("%s did not confirm it is safe to restart: %s (%s)", nodeID, msg, detail)
}

// stepAsideDetailFallback guarantees Detail is never empty, so a
// response that carried only booleans still produces prose an operator
// can read in a refused restart's evidence.
func stepAsideDetailFallback(nodeID string, out restartplan.StepAsideOutcome) string {
	if !out.WasLeader {
		return fmt.Sprintf("%s was not the raft leader, so no leadership handover was needed", nodeID)
	}
	if out.Transferred {
		return fmt.Sprintf("%s handed over leadership to %s", nodeID, out.NewLeaderID)
	}
	return fmt.Sprintf("%s held raft leadership and reported no confirmed handover", nodeID)
}

// StepAsideForRestart implements rpcpb.ManagerServiceServer: the
// managerd-side reach for this node's own raftd step-aside (ADR-0145).
//
// It shares restartStepAsider's local path rather than reimplementing
// it, so a same-node coordinator and a remote coordinator asking this
// node get the same answer by construction. There is no leader-forward
// here either: this handler always acts on the node that received the
// call, and a request naming a different Comb is refused rather than
// quietly served, so a misrouted call is visible instead of stepping
// aside the wrong machine.
func (s *Server) StepAsideForRestart(ctx context.Context, req *rpcpb.StepAsideForRestartRequest) (*rpcpb.StepAsideForRestartResponse, error) {
	presented, _ := extractBearerToken(ctx)
	if !restartGuardrailTokenValid(presented, s.restartGuardrailToken) {
		// A gRPC status, unlike the response error field below, and the
		// distinction is deliberate: "you are not authorized to ask
		// this" is not "I tried and could not stand down", and a
		// coordinator that could not tell those apart could mistake a
		// misconfigured token for a node that refused to hand over.
		return nil, status.Error(codes.PermissionDenied, "invalid or missing restart-guardrail token")
	}

	resp := &rpcpb.StepAsideForRestartResponse{NodeId: s.nodeID}

	if target := req.GetTargetNodeId(); target != "" && target != s.nodeID {
		resp.SafeToRestart = false
		resp.Error = fmt.Sprintf("refusing the step-aside: this request asks for %q to step aside, but this node is %q and a step-aside is only ever performed on the node that received it - send it to %q's own managerd", target, s.nodeID, target)
		resp.Detail = "the step-aside was refused because it was addressed to the wrong Comb"
		return resp, nil
	}

	asider := &restartStepAsider{
		localNodeID: s.nodeID,
		raft:        s.stepAsideClient(),
	}
	// An explicit timeout from the caller bounds this call end to end,
	// which is the caller's prerogative: a coordinator driving a sweep
	// knows how long it is prepared to wait on one Comb.
	if ms := req.GetTimeoutMs(); ms > 0 {
		asider.timeout = time.Duration(ms) * time.Millisecond
	}

	outcome, err := asider.StepAsideForRestart(ctx, s.nodeID)
	resp.SafeToRestart = outcome.SafeToRestart
	resp.WasLeader = outcome.WasLeader
	resp.Transferred = outcome.Transferred
	resp.NewLeaderId = outcome.NewLeaderID
	resp.Detail = outcome.Detail
	if err != nil {
		// Kept in the response field, not promoted to a gRPC status:
		// the caller needs to read safe_to_restart, the evidence, and
		// the reason together, and a transport-level error would carry
		// none of the other two.
		resp.SafeToRestart = false
		resp.Error = err.Error()
	}
	if !resp.SafeToRestart && resp.Error == "" {
		// A refusal with an empty error is the one shape this response
		// must never take: safe_to_restart false with a clean error
		// reads downstream as "the step ran and the answer was no",
		// which is a materially different event from "the step could
		// not be confirmed" and hides a node that may still be leading
		// behind a routine-looking no.
		resp.Error = "the node did not confirm it is safe to restart"
		if resp.Detail != "" {
			resp.Error += ": " + resp.Detail
		}
	}
	return resp, nil
}
