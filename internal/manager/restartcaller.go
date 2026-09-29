package manager

import (
	"context"
	"fmt"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	rpcpb "github.com/glenjbarber/apiary/api/rpc"
	"github.com/glenjbarber/apiary/internal/restartplan"
)

// This file is the real caller ADR-0145 needed, and the reason it can
// exist at all is that every piece it composes already works on its own:
//
//   - internal/raft.StepAsideForRestart, reached cross-node through
//     restartStepAsider in stepaside.go.
//   - internal/restartplan.EvaluateQuorumSafety over facts this package
//     really reads (raftdquorum.go), the same function the enforced
//     lease path calls, so a preview and the enforcement cannot drift.
//   - internal/raft.FSM's raft-replicated restart lease, through
//     reserveRestartLease, which applies AcquireRestartLease on the
//     leader and forwards there when this node is not the leader.
//   - the pending-restart record on disk, byte-identical to what
//     RestartNodeService writes (restartplan_agreement_test.go asserts
//     it, in this package, both directions).
//   - the real `service apiary_raftd restart`, through the same narrow
//     rc.d controller RestartNodeService already uses.
//   - the restarted process's own confirmation, made by cmd/raftd's own
//     startup hook.
//
// Until now nothing called all of that together outside a unit test with
// fakes on every boundary, so the composition itself was untested
// territory. This file is that composition, with every effect bound to
// the real one.
//
// WHAT THIS IS NOT, and the file is explicit about it rather than
// leaving it to be inferred:
//
//   - It is not a Colony sweep. One call, one Comb, one service. The
//     ordering across Combs, and the decision about which Comb to touch
//     first, are not here.
//   - It is not colony-wide single-flight. What serializes a restart
//     across the Colony is the one real restart lease, and a lease held
//     for a different target by a different holder refuses this call -
//     but that is ADR-0103's existing primitive, not new consensus. See
//     managerdLeaseReserver's doc comment for exactly what that does and
//     does not close.
//   - It is not durable. If the managerd running this plan is itself
//     restarted or dies mid-call, the plan is over: there is no
//     update-operation record to resume from, and the pending-restart
//     record left on disk is the only trace. Restarting apiary_managerd
//     is refused outright (see ExecuteNodeRestartPlan below), so this
//     cannot restart the process it lives in - but the absent piece is
//     real and is ADR-0145's own first open question.
//   - It is not a UI. There is no REST route and no frontend control,
//     and the RPC is gated on the root-owned restart-guardrail token
//     rather than on an API key, so an ordinary Admin credential cannot
//     reach it.

// planConfirmBackoff and planConfirmAttempts size the wait for the
// restarted process to report itself back.
//
// They are deliberately LARGER than restartplan.DefaultConfirmOptions'
// 5 attempts 3s apart, and the reason is specific rather than cautious:
// that default is cmd/raftd's OWN budget for making the confirmation,
// and this call is waiting for a different thing - for raftd to stop,
// start again, and get as far as running that hook at all. Confirming
// the same instant raftd's own hook begins means the coordinator's
// budget can expire before the process it is waiting on has finished
// starting up, and the honest answer "unobserved" would then be a lie
// about a restart that in fact completed.
//
// Ten attempts 3s apart is 27s of waiting, which comfortably covers
// rc.subr's stop/start, raftd's own bind and join, and its hook's own
// 5x3s budget. Exceeding it is not a failure: the lease stays held
// until cmd/raftd's hook confirms it, and the durable record says the
// coordinator stopped watching rather than that anything went wrong.
const (
	planConfirmAttempts = 10
	planConfirmBackoff  = 3 * time.Second
)

// coordinatorResultDirName is the sub-directory of
// restartplan.DefaultResultDir this handler's own durable plan record
// goes in, and the reason it is not DefaultResultDir itself is worth
// being exact about.
//
// Two processes write restart results and they are not the same record.
// cmd/raftd's startup hook writes what the RESTARTED process itself
// established - "I am back, and I confirmed my own lease" - under
// restartplan.DefaultResultDir, because that is its own wiring in
// cmd/raftd/main.go. This handler writes what the COORDINATING managerd
// established - the whole plan's outcome, its step-aside trace, every
// step it reached - and it must not land on the same path. One file
// would mean the two writers racing for it, each silently overwriting
// the other's evidence, which is precisely how a restart ends up
// recorded as confirmed by a process that never came back.
//
// A subdirectory is the smallest thing that separates them, and it keeps
// both under the same root so an operator reading
// /var/db/apiary/restart-plan sees both halves of the story side by side.
const coordinatorResultDirName = "coordinator"

// ExecuteNodeRestartPlan runs ADR-0125's whole sequence for one named
// Comb, with every effect bound to the real one. See the file comment
// above for what it composes and what it deliberately does not attempt.
func (s *Server) ExecuteNodeRestartPlan(ctx context.Context, req *rpcpb.ExecuteNodeRestartPlanRequest) (*rpcpb.ExecuteNodeRestartPlanResponse, error) {
	presented, _ := extractBearerToken(ctx)
	if !restartGuardrailTokenValid(presented, s.restartGuardrailToken) {
		// A gRPC status, exactly as StepAsideForRestart does it, and for
		// the same reason: "you may not ask this" is a different fact
		// from "I tried and could not do it", and a caller that cannot
		// tell them apart will read a misconfigured token as a refusal
		// by a node.
		return nil, status.Error(codes.PermissionDenied, "invalid or missing restart-guardrail token")
	}

	service := req.GetService()
	if service == "" {
		service = raftdServiceName
	}
	// ADR-0142's refusal, kept exactly where it is and for exactly the
	// reason it gives. The lease path, the quorum preflight and the
	// pending record all work for managerd today; what does not work is
	// orchestrating the death of the process doing the orchestrating.
	// ADR-0146 built that - a peer-issued restart, a detached child and a
	// replacement that confirms itself - and it is reached through
	// RequestManagerdRestart/IssueManagerdRestart in handoff.go, never
	// through here. The reason this path still refuses is the structural
	// one rather than an omission: everything below runs inside the
	// managerd being restarted, and no amount of durable state changes
	// which process would have to perform the start. Re-implementing the
	// refusal here rather than delegating to managerdSelfRestartRefused
	// would be a second copy that could drift from the first.
	if managerdSelfRestartRefused(service) {
		return &rpcpb.ExecuteNodeRestartPlanResponse{Error: managerdSelfRestartRefusal}, nil
	}
	// Everything else in the inventory is a service this plan does not
	// know how to reason about - there is no quorum argument for frontend
	// or restshimd, and no pending-record/confirmation path that is not
	// raftd's. Refusing by name is honest; silently treating it as
	// raftd would be neither.
	if !restartableService(service) || !isRaftdGuardrailed(service) {
		return &rpcpb.ExecuteNodeRestartPlanResponse{Error: fmt.Sprintf(
			"refusing to run a restart plan for %q: the only service this plan covers is %q, whose restart can cost the Colony its quorum", service, raftdServiceName)}, nil
	}

	// EXECUTION IS TARGET-LOCAL. The pending record is read back by the
	// restarted raftd on its own next startup, and the restart command
	// runs on the machine whose raftd is stopping; neither is visible
	// from here. So a request naming another Comb is forwarded to that
	// Comb's own managerd and this node only carries the intent.
	target := req.GetNodeId()
	if target == "" {
		target = s.nodeID
	}
	if target != s.nodeID {
		return s.forwardNodeRestartPlan(ctx, target, req)
	}

	// Two preconditions, checked before anything runs, so a
	// misconfigured node strands no lease and writes no misleading
	// record. Both are refused with a named reason rather than left to
	// surface as a nil dereference or as an outcome that implies an
	// attempt was made.
	if s.services == nil {
		return &rpcpb.ExecuteNodeRestartPlanResponse{Error: "refusing to run a restart plan: this node has no rc.d service controller configured, so no restart command could be issued - nothing was attempted and no lease was reserved"}, nil
	}
	if s.stepAsideClient() == nil {
		// Deliberately NOT the Engine's own nil-StepAsider posture,
		// which skips the step and leaves a leader-target to be caught
		// by the leader-restart guardrail behind an operator
		// acknowledgment. That degradation is right for a caller that
		// merely wants the old behaviour, and wrong here: this entry
		// point exists to promise a real, confirmed step-aside, and
		// quietly running without one would turn a stated guarantee into
		// an unstated fallback.
		return &rpcpb.ExecuteNodeRestartPlanResponse{Error: "refusing to run a restart plan: this node has no raft client configured, so no Comb's leadership state can be read or changed from here - nothing was attempted and no lease was reserved"}, nil
	}

	engine := s.newNodeRestartPlanEngine(service, target, req.GetForce())
	res, runErr := engine.Run(ctx)
	resp := &rpcpb.ExecuteNodeRestartPlanResponse{Result: toRPCNodeRestartPlanResult(res)}
	if runErr != nil {
		// Engine.Run's only error is a record it could not persist. The
		// result is still returned, because "it happened and we could
		// not write it down" is a fact the caller needs as much as the
		// run itself.
		resp.Error = runErr.Error()
	}
	return resp, nil
}

// newNodeRestartPlanEngine wires restartplan.Engine to this node's real
// plumbing. Every field is a live dependency of this Server; nothing
// here is a test double, and nothing is left nil to be discovered at
// the moment a Comb is about to be restarted.
func (s *Server) newNodeRestartPlanEngine(service, target string, force bool) *restartplan.Engine {
	return &restartplan.Engine{
		Service:    service,
		NodeID:     target,
		Force:      force,
		StepAsider: s.newRestartStepAsider(),
		Gatherer:   managerdQuorumGatherer{s: s},
		Leaser:     managerdLeaseReserver{s: s},
		Restarter:  rcServiceRestarter{services: s.services},
		Confirmer: selfReportedConfirmer{
			// restartplan.DefaultResultDir, NOT
			// s.coordinatorResultDir() below. cmd/raftd's startup hook
			// writes the restarted process's own account of itself
			// there (cmd/raftd/main.go), and that record - not this
			// process's - is the only thing that can testify to a
			// restart having happened. Reading the coordinator's own
			// directory instead would find THIS handler's previous run's
			// record, so a stale `confirmed` left by an earlier restart
			// would be the first thing a fresh plan saw. The lease-id
			// check in selfReportedConfirmer would usually catch that,
			// but "usually" is not a property worth having on the one
			// witness a restarted lease has.
			results: restartplan.NewResultStore(s.selfReportDir()),
		},
		Pending: restartplan.NewPendingStore(s.planPendingDir()),
		Results: restartplan.NewResultStore(s.coordinatorResultDir()),
		Confirm: s.planConfirm(),
	}
}

// planConfirm is the budget the confirmer gets, production defaults
// unless a caller overrode them (see Server.planConfirmAttempts).
func (s *Server) planConfirm() restartplan.ConfirmOptions {
	opts := restartplan.ConfirmOptions{Attempts: planConfirmAttempts, Backoff: planConfirmBackoff}
	if s.planConfirmAttempts > 0 {
		opts.Attempts = s.planConfirmAttempts
	}
	if s.planConfirmBackoff > 0 {
		opts.Backoff = s.planConfirmBackoff
	}
	return opts
}

// SetNodeRestartPlanDirs overrides the two directories
// ExecuteNodeRestartPlan reads and writes - the coordinator's own durable
// record and the restarted process's self-report - and its confirmation
// budget. It exists for tests, and for nothing else.
//
// Both halves are required together on purpose. A test that pointed both
// at one directory would have the coordinator's record and raftd's
// self-report landing on the same file, which is the exact collision
// coordinatorResultDirName exists to prevent; running the flow with that
// collision in place would test the broken arrangement rather than the
// real one.
func (s *Server) SetNodeRestartPlanDirs(coordinatorDir, selfReportDir string, confirmAttempts int, confirmBackoff time.Duration) {
	s.planCoordinatorDir = coordinatorDir
	s.planSelfReportDir = selfReportDir
	s.planConfirmAttempts = confirmAttempts
	s.planConfirmBackoff = confirmBackoff
}

// planPendingDir is where the pending-restart record goes, and it has to
// be the SAME directory RestartNodeService and cmd/raftd use - see
// managerdAddrFromStatus's sibling reasoning about two writers to one
// file, and restartplan_agreement_test.go for the byte-level check.
//
// s.restartConfirm is the store RestartNodeService already writes
// through, so its directory is the authoritative one when it is wired;
// restartplan.DefaultStateDir is the fallback and is what cmd/raftd
// reads when this managerd has no store of its own configured.
func (s *Server) planPendingDir() string {
	if s.restartConfirm != nil && s.restartConfirm.Dir != "" {
		return s.restartConfirm.Dir
	}
	return restartplan.DefaultStateDir
}

// coordinatorResultDir is the root this handler's own durable record goes
// under - deliberately not restartplan.DefaultResultDir, which is
// cmd/raftd's; see coordinatorResultDirName's own comment.
func (s *Server) coordinatorResultDir() string {
	if s.planCoordinatorDir != "" {
		return s.planCoordinatorDir
	}
	return restartplan.DefaultResultDir + "/" + coordinatorResultDirName
}

// selfReportDir is where the restarted process's own account of itself
// is expected: cmd/raftd's own result directory, never this handler's.
// The two are different files written by different processes, and only
// the restarted process's can testify to a restart.
func (s *Server) selfReportDir() string {
	if s.planSelfReportDir != "" {
		return s.planSelfReportDir
	}
	return restartplan.DefaultResultDir
}

// forwardNodeRestartPlan carries a plan request for another Comb to that
// Comb's own managerd, where it actually runs.
//
// It resolves the address from this node's own raft membership and never
// from anything the caller supplied, which is what keeps a
// token-authenticated restart from becoming a way to aim managerd at an
// arbitrary host - the same posture restartStepAsider's remote hop takes,
// over the same shared lookup. It is NOT leader-forwarded: the leader has
// no special business restarting another Comb's raftd, and the execution
// has to happen on the target for the pending record and the confirmation
// to mean anything.
//
// The response is passed back as it arrived, because the far end is the
// only node whose answer is about the node being restarted. Its
// node_id is not re-checked here the way the step-aside's is, and that is
// a deliberate difference: this response carries the far end's own
// durable record of a run it performed, with its own node id inside it,
// rather than a bare yes/no about some other node's state. A caller that
// needs the identity checks the result's own node_id, which is why
// NodeRestartPlanResult carries one.
func (s *Server) forwardNodeRestartPlan(ctx context.Context, target string, req *rpcpb.ExecuteNodeRestartPlanRequest) (*rpcpb.ExecuteNodeRestartPlanResponse, error) {
	if s.peers == nil {
		return &rpcpb.ExecuteNodeRestartPlanResponse{Error: fmt.Sprintf(
			"cannot run a restart plan for %s: this node has no peer forwarder configured, so no other Comb is reachable", target)}, nil
	}
	status, err := s.raftStatus(ctx)
	if err != nil {
		return &rpcpb.ExecuteNodeRestartPlanResponse{Error: fmt.Sprintf(
			"cannot run a restart plan for %s: cluster membership could not be read, so the target's address is unknown: %v", target, err)}, nil
	}
	addr, err := managerdAddrFromStatus(status, target, s.peerManagerdPort)
	if err != nil {
		return &rpcpb.ExecuteNodeRestartPlanResponse{Error: fmt.Sprintf(
			"cannot run a restart plan for %s: %v", target, err)}, nil
	}
	fwd, ferr := s.peers.ExecuteNodeRestartPlan(ctx, addr, req)
	if ferr != nil {
		return &rpcpb.ExecuteNodeRestartPlanResponse{Error: fmt.Sprintf(
			"forwarding the restart plan for %s to its own managerd at %s failed: %v - nothing is known about whether that node did anything", target, addr, ferr)}, nil
	}
	return fwd, nil
}

// managerdQuorumGatherer is ADR-0125 §3's "go and find out", reading
// this node's real raftd and dialling the real peers.
//
// It composes two existing real checks into the one fact
// restartplan.EvaluateQuorumSafety wants:
//
//   - raftdQuorumFact (raftdquorum.go), which is the same read-and-dial
//     the enforced lease path uses on the leader. Sharing it is the
//     mechanism behind ADR-0125's "the preview and the enforcement
//     cannot drift apart": there is one implementation, not two that
//     happen to agree today.
//   - evaluateRestartCooldown, ADR-0103's own concurrent-restart and
//     cooldown report, attached as QuorumFact.Existing so a lease
//     somebody else is already holding appears in THIS evaluation,
//     before anything is attempted, rather than only as a surprise at
//     the lease step.
type managerdQuorumGatherer struct{ s *Server }

// GatherQuorumFact implements restartplan.FactGatherer.
func (g managerdQuorumGatherer) GatherQuorumFact(ctx context.Context, service, nodeID string) restartplan.QuorumFact {
	fact := g.s.raftdQuorumFact(ctx, service, nodeID, false)
	// force is deliberately not read here. The Engine applies the
	// operator's acknowledgment to the gathered fact itself, after this
	// returns, and a gatherer that also applied it would create two
	// places that think they own it.
	fact.Existing = g.s.evaluateRestartCooldown(ctx, service)
	return fact
}

// managerdLeaseReserver is the real, raft-replicated serialization point:
// ADR-0103's no-TTL cluster-wide restart lease, reserved through the same
// reserveRestartLease every other guarded restart uses, which applies
// AcquireRestartLease on the leader and forwards there when this node is
// not the leader.
//
// It adds one thing, and the addition is the point:
//
// AN ALREADY-HELD LEASE REFUSES THIS CALL WHATEVER force SAYS. The
// gathered fact above usually catches that first, as a `blocked` at the
// evaluate step naming the holder (ADR-0103's `concurrent-manager-restart`
// rule). It is re-checked here, immediately before the apply, because:
//
//  1. A held lease is not a KNOWN cost an operator can acknowledge. Force
//     means "yes, this will trigger an election" - a fact, visible in
//     advance. A lease held for another Comb is the opposite: it is
//     evidence that somebody else's restart is genuinely in flight and
//     has not confirmed, which is precisely the state ADR-0145's "one at
//     a time" rule exists to prevent two clients from creating. Paying
//     that risk forward is not something an operator should be able to
//     do by ticking a box, and it is not something this entry point does.
//  2. restartplan's applyForce downgrades this package's OWN findings
//     (the quorum and leader rules) and deliberately leaves a foreign
//     finding alone. When force downgrades a quorum Block to Allow while
//     a lease is also held, the evaluation's verdict is Allow and the
//     `concurrent-manager-restart` finding survives in its evidence - a
//     report that reads "allowed" while carrying a reason it is not.
//     That combination is honest as a record and useless as a gate, so
//     the gate lives here, where it cannot be softened.
//
// The read-then-reserve window is real and is not closed here. Closing it
// needs a raft-replicated operation-ownership record that names the
// whole Colony update rather than one service's lease, which is
// ADR-0145's own single-flight work and is being built separately. In
// the window, an un-forced call is still refused by the FSM's own apply -
// that is the authoritative gate and it is never bypassed - and only a
// forced call can be granted a lease that appeared in the microseconds
// between the read and the apply. This is stated rather than hidden.
type managerdLeaseReserver struct{ s *Server }

// ReserveRestartLease implements restartplan.Leaser.
func (l managerdLeaseReserver) ReserveRestartLease(ctx context.Context, service, nodeID string, force bool) (restartplan.Lease, error) {
	if holder, requestedAt, held, err := l.heldRestartLease(ctx, service); err != nil {
		// Fail closed. An unreadable lease state means the "is somebody
		// else already restarting something" question has no answer, and
		// an unanswered question is not permission.
		return restartplan.Lease{}, fmt.Errorf("the restart-lease state for %s could not be read, so whether another Comb is already restarting it is unknown: %w", service, err)
	} else if held {
		return restartplan.Lease{}, fmt.Errorf(
			"refusing to reserve a restart lease for %s on %s: an unconfirmed restart lease for %s is already held by %q (requested at %d), so a second Comb would restart concurrently with an unconfirmed one - this refusal is not overridable by force here, and the held lease stays blocked until that Comb confirms itself healthy or an operator clears it through ReserveRestartLease",
			service, nodeID, service, holder, requestedAt)
	}

	resp, err := l.s.reserveRestartLease(ctx, &rpcpb.ReserveRestartLeaseRequest{
		Service: service,
		NodeId:  nodeID,
		Force:   force,
	})
	if err != nil {
		return restartplan.Lease{}, err
	}
	if msg := resp.GetError(); msg != "" {
		return restartplan.Lease{}, fmt.Errorf("%s", msg)
	}
	if !resp.GetGranted() {
		// The response shape with neither a grant nor a reason is the
		// one this must not invent, and the engine's own lease step
		// treats lease id 0 as a refusal too - belt and braces, because
		// "granted with id 0" would read downstream as a real lease.
		return restartplan.Lease{}, fmt.Errorf("the restart-lease reservation for %s on %s was neither granted nor refused", service, nodeID)
	}
	return restartplan.Lease{ID: resp.GetLeaseId()}, nil
}

// heldRestartLease reads this node's own FSM copy of the restart-lease
// state and reports who is holding it, if anybody.
//
// A read of a follower is a read of that follower's own replicated
// state, which can lag the leader's. That is acceptable here because the
// FSM's apply is the authoritative gate and this read is a
// pre-application refusal; it is not acceptable as the only gate, and it
// is not the only gate.
func (l managerdLeaseReserver) heldRestartLease(ctx context.Context, service string) (holder string, requestedAt int64, held bool, err error) {
	if l.s.raft == nil {
		return "", 0, false, fmt.Errorf("this node has no raft client configured, so lease state cannot be read")
	}
	resp, err := l.s.raft.GetRestartLeaseStateLocal(ctx, service)
	if err != nil {
		return "", 0, false, err
	}
	if msg := resp.GetError(); msg != "" {
		return "", 0, false, fmt.Errorf("%s", msg)
	}
	lease := resp.GetLease()
	if lease == nil {
		return "", 0, false, nil
	}
	return lease.GetHolderNodeId(), lease.GetRequestedAtUnix(), true, nil
}

// rcServiceRestarter is the one platform-touching effect in the whole
// plan: the FreeBSD `service <name> restart` call, through the same
// narrow rc.d controller RestartNodeService uses.
//
// It is injected rather than called directly so internal/restartplan
// never shells out at import time, and so a test can make it fail on
// demand - the only honest way to exercise the failed-restart path, and
// the path where a non-zero exit must never be confused with "the node
// came back and we could not see it".
//
// A nil controller is refused by ExecuteNodeRestartPlan before an Engine
// is ever built, so reaching here with one means a caller wired this
// directly; the error names the problem rather than dereferencing nil.
type rcServiceRestarter struct {
	services nodeServiceController
}

// RestartService implements restartplan.Restarter.
func (r rcServiceRestarter) RestartService(ctx context.Context, service string) error {
	if r.services == nil {
		return fmt.Errorf("no rc.d service controller is configured, so `service %s restart` was never issued", service)
	}
	// Bounded by the same budget RestartNodeService gives its own
	// restart command. There is no lease TTL to race against any more
	// (ADR-0103's revision note #17), but an unbounded command would
	// still block this goroutine forever.
	restartCtx, cancel := context.WithTimeout(ctx, restartCommandTimeout)
	defer cancel()
	return r.services.Restart(restartCtx, service)
}

// selfReportedConfirmer waits for the RESTARTED PROCESS's own account of
// itself, rather than confirming on its behalf.
//
// This is the one place where it would be easy to get ADR-0103/0125 badly
// wrong, so it is worth being exact. ADR-0103's revision note #16 and
// ADR-0125 §2 both say the same thing: the confirmation must be made by
// the process that was restarted, on its own next startup, and never by
// the process that requested the restart. Calling ConfirmRestartCompleted
// from here would be a violation of that - it would be this managerd
// telling the cluster that raftd came back, which is the one witness
// that cannot testify to its own resurrection.
//
// So this type reads a file instead. cmd/raftd's startup hook
// (cmd/raftd/confirm.go) is the process that was restarted; it makes the
// real confirmation against the real FSM, and then it writes what it
// established as a durable result under restartplan.DefaultResultDir.
// This waits for exactly that record.
//
// Three checks make the answer honest rather than hopeful:
//
//   - The lease id must match. A stale `confirmed` record left by an
//     earlier restart on this node would otherwise be credited to this
//     one, which is precisely the "restart confirmed" that never
//     happened. Lease ids are raft log indexes, so they are unique and
//     never reused.
//   - The outcome must be `confirmed`. A record saying `unobserved` is a
//     record saying raftd came up and could not confirm its own lease -
//     real information, and emphatically not a confirmation.
//   - A missing record is a refusal with a stated reason, not a shrug,
//     so every attempt in the evidence says what it was waiting for.
//
// A consequence worth stating: if raftd's own hook cannot reach managerd
// (no restart-guardrail token provisioned on that node, say), this never
// succeeds, and the plan's honest answer is `unobserved` - the lease
// stays held and both records say so. That is the correct outcome, not a
// defect to paper over with a self-confirmation that would let a lease
// be released by a process that was never restarted.
type selfReportedConfirmer struct {
	results *restartplan.ResultStore
}

// ConfirmRestartCompleted implements restartplan.Confirmer.
func (c selfReportedConfirmer) ConfirmRestartCompleted(_ context.Context, service, nodeID string, leaseID uint64) error {
	if c.results == nil {
		return fmt.Errorf("no restart-result store is configured, so nothing this node restarted can report itself")
	}
	res, found, err := c.results.Load(nodeID, service)
	if err != nil {
		return fmt.Errorf("reading %s's own restart record for %s: %w", nodeID, service, err)
	}
	if !found {
		return fmt.Errorf("%s on %s has not written a restart record yet, so the restarted process has not reported itself back", service, nodeID)
	}
	if res.LeaseID != leaseID {
		return fmt.Errorf("the restart record on %s is for lease %d, not the lease %d this plan reserved, so it cannot speak for this restart", nodeID, res.LeaseID, leaseID)
	}
	if res.Outcome != restartplan.OutcomeConfirmed {
		return fmt.Errorf("%s on %s reported %q, not a confirmation: %s", service, nodeID, res.Outcome, res.Detail)
	}
	return nil
}

// toRPCNodeRestartPlanResult flattens a durable result onto the wire.
// The StepAside record is included when one exists and omitted when it
// does not, so a caller can tell "no step-asider was configured" from a
// step-aside that ran and found nothing to do - two different facts that
// a bare boolean would render identically.
func toRPCNodeRestartPlanResult(res restartplan.Result) *rpcpb.NodeRestartPlanResult {
	out := &rpcpb.NodeRestartPlanResult{
		Service:        res.Service,
		NodeId:         res.NodeID,
		LeaseId:        res.LeaseID,
		Attempt:        int32(res.Attempt),
		Outcome:        string(res.Outcome),
		Detail:         res.Detail,
		Evidence:       append([]string(nil), res.Evidence...),
		StartedAtUnix:  res.StartedAtUnix,
		FinishedAtUnix: res.FinishedAtUnix,
	}
	if r := res.StepAside; r != nil {
		out.StepAside = &rpcpb.NodeRestartPlanStepAside{
			Attempted:     r.Attempted,
			WasLeader:     r.WasLeader,
			Transferred:   r.Transferred,
			NewLeaderId:   r.NewLeaderID,
			SafeToRestart: r.SafeToRestart,
			Detail:        r.Detail,
		}
	}
	return out
}
