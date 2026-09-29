package manager

// ADR-0146: how a controlled Colony update restarts the managerd that is
// coordinating it, without the process that performs the start being the
// process the stop kills.
//
// WHY THIS FILE EXISTS AT ALL
//
// ADR-0142 recorded a fault reproduced on two Combs rather than argued
// about: RestartNodeService orchestrated every restart from a goroutine
// INSIDE the calling managerd, so when the target was managerd itself,
// the stop half of `service apiary_managerd restart` killed the process
// the goroutine lived in and the start half never ran. The service
// stopped and did not come back. ADR-0142's conclusion is structural -
// no in-process orchestration of a process's own death can work, because
// the process that would perform the start is the process being stopped
// - and its chosen answer is to refuse apiary_managerd outright and point
// the operator at `apiaryctl force-restart`.
//
// That answer is right for an operator at a shell and wrong for a
// controlled update, because a colony update that cannot update the Comb
// running it stops one short every single time, and the operator is back
// to logging into nodes and knowing which one is leading.
//
// So the handoff is built here, in four parts, and each part is one of
// ADR-0146's rules:
//
//   - Rule 3, RequestManagerdRestart: a PEER managerd asks a Comb to
//     restart its own managerd. A managerd may not ask itself, and that
//     refusal is non-overridable and carries no force field.
//   - Rule 4, IssueManagerdRestart: the target writes a durable marker,
//     ACKs the peer, and only then arranges the restart in a child
//     process reparented out of managerd.
//   - Rule 5, confirmManagerdRestartHandoff: the REPLACEMENT process
//     confirms the step, on its own next startup, from its own evidence.
//   - Rule 6: an absent or unreadable marker is `unobserved`, and the
//     step stops. Never success, never silently skipped.
//
// WHAT IS DELIBERATELY NOT HERE, because the absence is load-bearing and
// not an oversight:
//
//   - No UI control, and no route to one. The Machine page's managerd row
//     is status-only and stays that way. ADR-0142's refusal stands
//     unchanged in RestartNodeService and in managerdSelfRestartRefused;
//     this file adds a different entry point for a different caller, not
//     a lever on the old one.
//   - No colony sweep. Deciding which Comb to touch, in what order, and
//     when to stop is ADR-0145's work and belongs to a coordinator that
//     holds the fence. Everything here is one named Comb, one step, one
//     operation.
//   - No claim that the detached child works. ADR-0146 rule 4 is explicit
//     that the mechanism is asserted rather than measured, and that it
//     must be reproduced on real Combs before a Colony is trusted with
//     it. This file is the mechanism being written down; it is not the
//     reproduction, and the SetDetachedRestarter seam below exists so that
//     reproduction can be done against a real rc.d without a test double
//     anywhere near the stop/start itself.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	rpcpb "github.com/glenjbarber/apiary/api/rpc"
	"github.com/glenjbarber/apiary/internal/buildgate"
)

// ManagerdHandoffFileName is the durable marker's own name inside the
// restart-guardrail state directory.
//
// It is NOT "pending-restart-apiary_managerd.json", and the difference is
// deliberate rather than cosmetic. That file is ADR-0103's
// RestartConfirmStore record, it is written by RestartNodeService, and
// cmd/managerd's existing confirmPendingRestartOnStartup is its single
// reader. Two processes writing one file is the exact collision
// coordinatorResultDirName's own comment describes in the sibling
// coordinator/result split, and the consequence there was two
// processes' evidence silently overwriting each other's. This path has
// one writer (the managerd about to die) and one reader (its own
// replacement) and nobody else, which is what lets the replacement own
// the lease confirmation for this restart end to end.
const ManagerdHandoffFileName = "managerd-restart-handoff.json"

// ManagerdHandoffRecord is the durable "restart scheduled" marker ADR-0146
// rule 4 requires the target to write BEFORE it ACKs the peer.
//
// It exists because the failure mode this whole feature addresses is
// silence. The process that is about to die cannot report its own death,
// and nothing else on that Comb knows a restart was ever intended except
// a file this process wrote and its own replacement will read. If the
// write is missing or unreadable when the replacement starts, the honest
// answer is that nobody learned anything - which is rule 6's `unobserved`
// and never a success.
type ManagerdHandoffRecord struct {
	// OperationID is the controlled update this restart is a step of.
	// The replacement reads its own copy of that operation's durable
	// record and appends its step outcome to it; without this field
	// there is nothing to append to.
	OperationID string `json:"operation_id"`

	// NodeID is this Comb. It is carried rather than assumed so a
	// marker copied onto the wrong host is visibly wrong instead of
	// quietly describing somebody else's restart.
	NodeID string `json:"node_id"`

	// Step is the step being entered, in internal/restartplan's own
	// vocabulary, so the step record the replacement appends is
	// comparable with every other step in the operation.
	Step string `json:"step"`

	// Service is the rc.d service being restarted. "apiary_managerd"
	// in every real case; named rather than implied so the replacement
	// is confirming a specific service and not a role.
	Service string `json:"service"`

	// LeaseID is the cluster-wide ADR-0103 restart lease reserved for
	// this restart. The replacement confirms exactly this lease through
	// ConfirmRestartCompletedLocal, and a zero is refused at write time
	// rather than confirmed against whatever happens to be held.
	LeaseID uint64 `json:"lease_id"`

	// ExpectedBuild is the build id the binary ON DISK reported when
	// the marker was written, read through internal/buildgate. It is
	// the identity the replacement must find itself running before it
	// may confirm anything: a copy-over-a-running-binary leaves the new
	// bytes on disk and the old ones resident, which is the exact
	// ambiguity this project has already been bitten by twice.
	//
	// Empty means no id could be read, and an empty ExpectedBuild is
	// NOT a match. The replacement reports the step `unobserved` and
	// stops rather than confirming against an absence.
	ExpectedBuild string `json:"expected_build"`

	// ScheduledAtUnix is the scheduling process's own reading. It is
	// diagnostic only: nothing in the confirmation path decides anything
	// from it, and no timeout anywhere is derived from it. A restart
	// lease that expired on a timer would be able to end an update
	// mid-sweep, which is why ADR-0103 and ADR-0145 have none.
	ScheduledAtUnix int64 `json:"scheduled_at_unix"`
}

// ManagerdHandoffStore persists exactly one ManagerdHandoffRecord, on
// this Comb, for this managerd's own replacement to read back.
//
// It follows internal/deadman's single-purpose atomic-write convention
// and RestartConfirmStore's, for the same reasons: the writer may be
// killed at any instant after the rename, so the write has to be
// all-or-nothing, and the record is a capability-shaped thing that
// belongs 0600 in a root-owned directory.
type ManagerdHandoffStore struct {
	Dir string
}

// NewManagerdHandoffStore returns a store rooted at dir.
func NewManagerdHandoffStore(dir string) *ManagerdHandoffStore {
	return &ManagerdHandoffStore{Dir: dir}
}

func (h *ManagerdHandoffStore) path() string {
	return filepath.Join(h.Dir, ManagerdHandoffFileName)
}

// Save writes r atomically (temp file, chmod 0600, rename), replacing any
// stale prior marker.
//
// A prior marker surviving into a new scheduled restart would be a real
// hazard rather than a cosmetic one: the replacement would find a marker
// naming the PREVIOUS operation and confirm the wrong step, so
// overwriting is correct and the previous marker's own reader (a
// replacement that already ran) is long gone.
func (h *ManagerdHandoffStore) Save(r ManagerdHandoffRecord) error {
	if err := os.MkdirAll(h.Dir, 0o700); err != nil {
		return fmt.Errorf("managerd handoff: creating state directory: %w", err)
	}
	body, err := json.Marshal(r)
	if err != nil {
		return fmt.Errorf("managerd handoff: encoding record: %w", err)
	}
	tmp, err := os.CreateTemp(h.Dir, ".managerd-restart-handoff-*.tmp")
	if err != nil {
		return fmt.Errorf("managerd handoff: creating temp file: %w", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath) // no-op once successfully renamed
	if _, err := tmp.Write(body); err != nil {
		tmp.Close()
		return fmt.Errorf("managerd handoff: writing temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("managerd handoff: closing temp file: %w", err)
	}
	if err := os.Chmod(tmpPath, 0o600); err != nil {
		return fmt.Errorf("managerd handoff: setting permissions: %w", err)
	}
	if err := os.Rename(tmpPath, h.path()); err != nil {
		return fmt.Errorf("managerd handoff: finalizing write: %w", err)
	}
	return nil
}

// Load reads the marker, if any.
//
// A missing file is not an error and returns found false, because an
// ordinary startup of a managerd that was not scheduled for a controlled
// restart is the overwhelmingly common case and must not be logged as
// anything. A file that EXISTS but cannot be parsed is a different thing
// entirely and is returned as an error: a restart was scheduled here and
// its record is unreadable, which is rule 6's case.
func (h *ManagerdHandoffStore) Load() (ManagerdHandoffRecord, bool, error) {
	data, err := os.ReadFile(h.path())
	if err != nil {
		if os.IsNotExist(err) {
			return ManagerdHandoffRecord{}, false, nil
		}
		return ManagerdHandoffRecord{}, false, fmt.Errorf("managerd handoff: reading record: %w", err)
	}
	var r ManagerdHandoffRecord
	if err := json.Unmarshal(data, &r); err != nil {
		return ManagerdHandoffRecord{}, true, fmt.Errorf("managerd handoff: parsing record at %s: %w", h.path(), err)
	}
	return r, true, nil
}

// Clear removes the marker, and is called only after the replacement has
// recorded an outcome for it. Never before: a marker cleared by the
// process that is about to die is a marker the replacement can never
// find, which is the silence rule 6 exists to prevent.
func (h *ManagerdHandoffStore) Clear() error {
	if err := os.Remove(h.path()); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("managerd handoff: removing record: %w", err)
	}
	return nil
}

// DetachedRestarter arranges a service restart from a process that is NOT
// the managerd being restarted.
//
// The interface exists so the whole handler above it is testable without
// a real rc.d, and so ADR-0146 rule 4's own instruction - reproduce the
// fork on a real Comb before trusting it - can be carried out against the
// real thing by wiring this to setsidServiceRestarter on brood or drone
// with nothing else changed.
type DetachedRestarter interface {
	// ArrangeRestart makes sure service will be restarted, and returns
	// as soon as the arrangement is durable enough that the caller may
	// safely exit. It must not wait for the restart to happen: the
	// caller is the process being restarted, and waiting is how
	// ADR-0142's failure happens.
	ArrangeRestart(service string, delay time.Duration) error
}

// setsidServiceRestarter is the real DetachedRestarter: it execs a child
// in its own session that waits and then runs `service <name> restart`.
//
// EVERY part of the SysProcAttr and of the /bin/sh invocation is
// load-bearing, and each answers one of the three realistic failure modes
// ADR-0146 rule 4 names:
//
//   - Setsid puts the child in a new session and a new process group, so
//     it is not in managerd's process group. daemon(8), which supervises
//     managerd, signals the group it supervises; without a new session the
//     child dies with the parent before it reaches the start half. This
//     is the "killed with the process group" failure, closed.
//   - The child is started from context.Background() rather than the
//     request's context, and no Wait is ever called on it, so nothing
//     cancels it when the gRPC handler returns. This is the "inherits a
//     context that gets cancelled" failure, closed. A child that outlives
//     its parent by design is also why Wait is not called: the parent is
//     about to be killed, and a Wait would only ever be interrupted.
//   - The delay happens INSIDE the child, not as a sleep in the parent.
//     A parent that slept before forking would be killed during its own
//     sleep; a child that sleeps has no managerd left to be killed by.
//     The delay exists so the ACK to the peer has reached it before the
//     stop half begins, and it is bounded at both ends by the caller.
//
// `service <name> restart` is both halves in one command, and the process
// that issues it is this child, which is not managerd. That is the whole
// of ADR-0141's already-live-verified mechanism, reused rather than
// reinvented, and the ADR's own warning is carried here rather than
// hidden: forking it out of a live managerd at the exact moment managerd
// is being stopped has not been reproduced on a real Comb, and this
// struct is where that reproduction is run.
type setsidServiceRestarter struct {
	// shell and service are the two executables, fields rather than
	// literals so a test can point them at a recording script without
	// this file growing a build tag.
	shell   string
	service string

	// start is os/exec's Cmd.Start, injected for tests. nil uses
	// exec.Command(...).Start.
	start func(name string, args ...string) error
}

// DefaultDetachedRestarter is the production DetachedRestarter.
func DefaultDetachedRestarter() DetachedRestarter {
	return &setsidServiceRestarter{shell: "/bin/sh", service: "/usr/sbin/service"}
}

// Arrival bounds on the delay, and the reason it is a range rather than
// a constant is that both ends are real failure modes. Too short and the
// peer may never receive the ACK and will report a forwarding failure
// for a restart that did happen. Too long and an operator watching a
// Comb sees a scheduled restart that has not started, which is
// indistinguishable from the detached child having died - the one
// failure this mechanism cannot self-diagnose, precisely because the
// process that would notice is the one that is gone.
const (
	MinHandoffRestartDelay = 500 * time.Millisecond
	MaxHandoffRestartDelay = 30 * time.Second
)

// HandoffRestartDelay is the delay used when a caller does not choose
// one, and it is a named default rather than a literal at the call site
// so the value is one thing that can be asserted on.
const HandoffRestartDelay = 2 * time.Second

// clampHandoffDelay bounds a caller-supplied delay, and refuses nothing:
// a delay outside the range is corrected rather than rejected, because
// the delay is a courtesy to the ACK's timing and refusing a whole
// handoff over it would make the caller treat a cosmetic number as a
// permission boundary. Zero selects the default.
func clampHandoffDelay(ms uint64) time.Duration {
	if ms == 0 {
		return HandoffRestartDelay
	}
	d := time.Duration(ms) * time.Millisecond
	if d < MinHandoffRestartDelay {
		return MinHandoffRestartDelay
	}
	if d > MaxHandoffRestartDelay {
		return MaxHandoffRestartDelay
	}
	return d
}

// ArrangeRestart implements DetachedRestarter.
func (r *setsidServiceRestarter) ArrangeRestart(service string, delay time.Duration) error {
	if r.shell == "" || r.service == "" {
		return fmt.Errorf("refusing to arrange a %s restart: no restart command is configured on this node", service)
	}
	// The delay is expressed in whole seconds because it is handed to
	// sleep(1) inside the child, and sleep(1) takes an integer. A
	// sub-second request is rounded UP rather than down, because rounding
	// down would shorten the window the ACK needs.
	secs := int64((delay + time.Second - 1) / time.Second)
	if secs < 1 {
		secs = 1
	}
	script := fmt.Sprintf("sleep %d; exec %s %s restart", secs, r.service, service)

	start := r.start
	if start == nil {
		start = execStartSetsid
	}
	if err := start(r.shell, "-c", script); err != nil {
		return fmt.Errorf("arranging a %s restart from a detached child: %w", service, err)
	}
	return nil
}

// execStartSetsid is the real fork. It is a function rather than an
// inline exec.Command so the SysProcAttr above is stated once.
func execStartSetsid(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	// A new session, and therefore a new process group. Nothing else in
	// this repository sets SysProcAttr, so this is the only place in the
	// tree that can be trusted to have thought about it.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	// Start, never Wait. The child is meant to outlive this process, so
	// waiting for it would block a handler whose whole purpose is to
	// return before this process is killed. cmd.Process is left running
	// and reparented to init when managerd exits.
	if err := cmd.Start(); err != nil {
		return err
	}
	// The descriptors are closed on both sides deliberately: the child
	// writes to /dev/null rather than inheriting managerd's listener or
	// its stderr, and dropping the *os.Process reference here means
	// nothing in this process can later Wait on it by accident.
	cmd.Process.Release()
	return nil
}

// RequestManagerdRestart implements rpcpb.ManagerServiceServer: the
// coordinator-side half of ADR-0146 (rules 1 and 3).
//
// It is a reach, not a sweep. It reserves the real restart lease,
// re-derives rule 1's constraint from the fence the caller presented,
// and carries the intent to the Comb that is about to be restarted. It
// does not decide the order, does not choose the next Comb, and does not
// decide when the operation is finished.
func (s *Server) RequestManagerdRestart(ctx context.Context, req *rpcpb.RequestManagerdRestartRequest) (*rpcpb.RequestManagerdRestartResponse, error) {
	presented, _ := extractBearerToken(ctx)
	if !restartGuardrailTokenValid(presented, s.restartGuardrailToken) {
		// A gRPC status, exactly as StepAsideForRestart and
		// ExecuteNodeRestartPlan do it: "you may not ask this" is a
		// different fact from "I tried and could not do it", and a
		// coordinator that cannot tell them apart reads a misconfigured
		// token as a refusal by a node.
		return nil, status.Error(codes.PermissionDenied, "invalid or missing restart-guardrail token")
	}

	target := req.GetTargetNodeId()
	if target == "" {
		return &rpcpb.RequestManagerdRestartResponse{Error: "refusing to restart a managerd: no target Comb was named, so there is no Comb to ask"}, nil
	}
	fence := req.GetFence()
	if err := checkColonyUpdateFence(fence); err != nil {
		return &rpcpb.RequestManagerdRestartResponse{Error: err.Error()}, nil
	}
	if req.GetStep() == "" {
		return &rpcpb.RequestManagerdRestartResponse{Error: "refusing to restart a managerd: no step was named, so the replacement process would have no step to record an outcome against"}, nil
	}

	// RULE 1, enforced here and again on the target. A managerd may not
	// be the Comb that restarts its own managerd, and the fence is the
	// only thing that says who holds the operation. This is a refusal
	// rather than a silent correction: a coordinator that names the
	// holder's own Comb has a bug, and quietly handing the operation to
	// somebody else would hide it.
	if target == fence.GetHolderNodeId() {
		return &rpcpb.RequestManagerdRestartResponse{
			TargetNodeId: target,
			Error: fmt.Sprintf(
				"refusing to restart %s's managerd: that Comb is the holder of controlled update %q (fence token %d), and a managerd cannot restart itself - ADR-0142's refusal, which no force setting overrides because there is no force setting here to override it with. Hand the operation to another Comb with HandoverColonyUpdate first, which is the normal way a colony-wide sweep finishes, and then ask that Comb's managerd. If you only need this Comb's managerd restarted and no update is running, do it on the host from a root shell: `service apiary_managerd restart`, or `apiaryctl force-restart` for managerd and raftd together",
				target, fence.GetOperationId(), fence.GetFenceToken()),
		}, nil
	}

	if s.peers == nil {
		return &rpcpb.RequestManagerdRestartResponse{TargetNodeId: target, Error: fmt.Sprintf(
			"cannot restart %s's managerd: this node has no peer forwarder configured, so no other Comb is reachable", target)}, nil
	}
	raftStatus, err := s.raftStatus(ctx)
	if err != nil {
		return &rpcpb.RequestManagerdRestartResponse{TargetNodeId: target, Error: fmt.Sprintf(
			"cannot restart %s's managerd: cluster membership could not be read, so the target's address is unknown: %v", target, err)}, nil
	}
	addr, err := managerdAddrFromStatus(raftStatus, target, s.peerManagerdPort)
	if err != nil {
		return &rpcpb.RequestManagerdRestartResponse{TargetNodeId: target, Error: fmt.Sprintf(
			"cannot restart %s's managerd: %v", target, err)}, nil
	}

	// The lease is reserved BEFORE the target is asked, and never
	// released on a failure below.
	//
	// Reserved first because the restarted process confirms the lease
	// itself (ADR-0146 rule 5) and can only confirm a lease that exists;
	// a marker naming lease 0 would be a record the replacement could
	// not honestly confirm.
	//
	// Never released on failure because there is no "the restart never
	// happened" transition on a restart lease, and a forward that failed
	// does not tell us whether the target acted on the request before
	// the transport gave up. Releasing on that evidence would be exactly
	// the "somebody else might be restarting this right now" state
	// ADR-0103's lease exists to make impossible. It stays held and
	// blocking until the target confirms itself or an operator clears
	// it, which is that ADR's own deliberate posture.
	lease, err := s.reserveManagerdRestartLease(ctx, target)
	if err != nil {
		return &rpcpb.RequestManagerdRestartResponse{TargetNodeId: target, Error: err.Error()}, nil
	}

	issued, ferr := s.peers.IssueManagerdRestart(ctx, addr, &rpcpb.IssueManagerdRestartRequest{
		TargetNodeId: target,
		OperationId:  fence.GetOperationId(),
		Fence:        fence,
		Step:         req.GetStep(),
		LeaseId:      lease,
		DelayMs:      uint64(clampHandoffDelay(0) / time.Millisecond),
	})
	if ferr != nil {
		return &rpcpb.RequestManagerdRestartResponse{
			TargetNodeId: target,
			LeaseId:      lease,
			Error: fmt.Sprintf(
				"forwarding the managerd restart for %s to its own managerd at %s failed: %v - nothing is known about whether that node acted on it, and the restart lease %d for apiary_managerd on %s stays held and blocking until that node confirms itself or an operator clears it",
				target, addr, ferr, lease, target),
		}, nil
	}
	if msg := issued.GetError(); msg != "" || !issued.GetAccepted() {
		if msg == "" {
			msg = "the target accepted the request without scheduling a restart and without saying why"
		}
		return &rpcpb.RequestManagerdRestartResponse{
			TargetNodeId: target,
			LeaseId:      lease,
			Issued:       issued,
			Error:        fmt.Sprintf("restarting %s's managerd was refused: %s - the restart lease %d for apiary_managerd on %s stays held and blocking until that node confirms itself or an operator clears it", target, msg, lease, target),
		}, nil
	}
	return &rpcpb.RequestManagerdRestartResponse{
		Accepted:     true,
		TargetNodeId: target,
		LeaseId:      lease,
		Issued:       issued,
	}, nil
}

// reserveManagerdRestartLease takes the cluster-wide ADR-0103 restart
// lease for apiary_managerd on target, through the same leader-resolved
// reserveRestartLease every other guarded restart uses.
//
// Force is false and there is deliberately no way to pass true. A restart
// of the management daemon on a quorum-critical Comb is the most
// consequential restart this system performs, and a lease held by somebody
// else means an update is genuinely in flight - which is evidence, not a
// known cost, and evidence is not something an acknowledgment can
// convert into permission. This is managerdLeaseReserver's own argument,
// stated once more at the one place a caller could otherwise have reached
// around it.
func (s *Server) reserveManagerdRestartLease(ctx context.Context, target string) (uint64, error) {
	resp, err := s.reserveRestartLease(ctx, &rpcpb.ReserveRestartLeaseRequest{
		Service: managerdServiceName,
		NodeId:  target,
		Force:   false,
	})
	if err != nil {
		return 0, fmt.Errorf("reserving the restart lease for apiary_managerd on %s: %w", target, err)
	}
	if msg := resp.GetError(); msg != "" {
		return 0, fmt.Errorf("%s", msg)
	}
	if !resp.GetGranted() {
		return 0, fmt.Errorf("the restart lease for apiary_managerd on %s was neither granted nor refused, so no managerd restart was attempted", target)
	}
	if resp.GetLeaseId() == 0 {
		return 0, fmt.Errorf("the restart lease for apiary_managerd on %s was granted with id 0, which is never a real lease, so no managerd restart was attempted", target)
	}
	return resp.GetLeaseId(), nil
}

// IssueManagerdRestart implements rpcpb.ManagerServiceServer: the
// target-local half of ADR-0146 (rules 3 and 4).
//
// The order of what happens inside is the whole design, so it is stated
// here rather than left to the reader to reconstruct:
//
//  1. a durable handover, if this node holds the operation, so the
//     operation survives this process;
//  2. the durable marker, so the replacement knows what to confirm;
//  3. the ACK, by returning;
//  4. and only then the detached child that performs the restart.
//
// Arranging the restart before the marker is durable would leave a peer
// believing a restart was scheduled for an operation no replacement could
// ever find. Arranging it before the ACK would race the peer into
// reporting a forwarding failure for a restart that succeeded.
func (s *Server) IssueManagerdRestart(ctx context.Context, req *rpcpb.IssueManagerdRestartRequest) (*rpcpb.IssueManagerdRestartResponse, error) {
	presented, _ := extractBearerToken(ctx)
	if !restartGuardrailTokenValid(presented, s.restartGuardrailToken) {
		return nil, status.Error(codes.PermissionDenied, "invalid or missing restart-guardrail token")
	}

	resp := &rpcpb.IssueManagerdRestartResponse{NodeId: s.nodeID}
	refuse := func(format string, args ...any) (*rpcpb.IssueManagerdRestartResponse, error) {
		resp.Accepted = false
		resp.Error = fmt.Sprintf(format, args...)
		return resp, nil
	}

	// The misroute guard, identical in shape to StepAsideForRestart's:
	// this handler always acts on the node that received it, so a
	// request naming a different Comb is refused rather than honoured and
	// a misplaced call is visible instead of stopping the wrong machine.
	if target := req.GetTargetNodeId(); target != "" && target != s.nodeID {
		return refuse("refusing the managerd restart: this request asks to restart %q's managerd, but this node is %q and the restart is only ever arranged on the node that received it - send it to %q's own managerd", target, s.nodeID, target)
	}
	if req.GetOperationId() == "" {
		return refuse("refusing the managerd restart: no controlled-update operation was named, so there is no step to record an outcome against and nothing for a replacement to confirm")
	}
	fence := req.GetFence()
	if err := checkColonyUpdateFence(fence); err != nil {
		return refuse("%s", err.Error())
	}
	if fence.GetOperationId() != req.GetOperationId() {
		return refuse("refusing the managerd restart: the fence names controlled update %q but the request names %q, so the marker would describe a step of an operation the caller does not hold", fence.GetOperationId(), req.GetOperationId())
	}
	if req.GetStep() == "" {
		return refuse("refusing the managerd restart: no step was named, so the replacement process would have nothing to record an outcome against")
	}
	if req.GetLeaseId() == 0 {
		return refuse("refusing the managerd restart: no restart lease was presented, and the replacement process can only confirm a lease that exists - lease 0 is the emergency path and stays manual")
	}
	if s.handoff == nil {
		return refuse("refusing the managerd restart: this managerd has no durable handoff record store configured, so it could not leave anything for its own replacement to find - nothing was scheduled and nothing will be restarted")
	}
	if s.detachedRestarter == nil {
		// Deliberately NOT a degraded restart in this process. That
		// arrangement is ADR-0142's fault, reproduced on two Combs: the
		// stop half kills the process that was about to run the start
		// half, and the daemon does not come back. There is no version
		// of this that is a smaller outage.
		return refuse("refusing the managerd restart: this managerd has no detached restart mechanism configured, and performing the restart inside this process is the failure ADR-0142 recorded - the stop half would kill the process about to run the start half, and apiary_managerd would not come back. Nothing was scheduled and nothing will be restarted")
	}

	// STEP 1: the durable handover. This node is about to die, so if it
	// holds the operation the operation has to move first, or the sweep
	// dies with it.
	//
	// The coordinator-side check in RequestManagerdRestart should already
	// have made this impossible, and the re-check is not redundancy for
	// its own sake: that check is on the other side of a network hop,
	// and a check that only exists on one side of a hop is a check a
	// caller who reaches the other side directly can aim around.
	if fence.GetHolderNodeId() == s.nodeID {
		toNode, toIncarnation := req.GetHandoverToNodeId(), req.GetHandoverToIncarnation()
		switch {
		case toNode == "" || toIncarnation == "":
			return refuse("refusing the managerd restart: this Comb holds controlled update %q and is about to restart itself, so the operation must be handed to another managerd process first - no successor was named, and this handler will not invent one, because a handover to a guessed incarnation is a durable record of a change of coordinator that did not happen", fence.GetOperationId())
		case toNode == s.nodeID && toIncarnation == fence.GetHolderIncarnation():
			return refuse("refusing the managerd restart: this Comb holds controlled update %q and was told to hand it to its own current incarnation, which is not a change of coordinator at all - hand it to a different process or a different Comb", fence.GetOperationId())
		}
		handover, err := s.mutateColonyUpdate(ctx, &rpcpb.MutateColonyUpdateRequest{
			Op: &rpcpb.MutateColonyUpdateRequest_Handover{
				Handover: &rpcpb.HandoverColonyUpdate{
					FromFence:           fence,
					ToNodeId:            toNode,
					ToHolderIncarnation: toIncarnation,
					Reason:              fmt.Sprintf("this Comb is restarting its own managerd as step %q of controlled update %q, so the operation moves before this process exits", req.GetStep(), req.GetOperationId()),
				},
			},
		})
		if err != nil {
			return refuse("refusing the managerd restart: handing controlled update %q to %q failed: %v - nothing was scheduled and nothing will be restarted", req.GetOperationId(), toNode, err)
		}
		if msg := handover.GetError(); msg != "" {
			return refuse("refusing the managerd restart: handing controlled update %q to %q was refused: %s - nothing was scheduled and nothing will be restarted", req.GetOperationId(), toNode, msg)
		}
		resp.HandoverPerformed = true
	}

	// STEP 2: the durable marker, including the build identity the
	// replacement will have to match. buildgate.Confirm reads the binary
	// on disk and this process's own startup line on this Comb and opens
	// no network connection at all, which is the locality that makes its
	// "the log file is not there" a fact rather than a guess.
	report := buildgate.Confirm(s.nodeID, s.buildGatePaths())
	expected := ""
	if svc, ok := report.Service(buildGateManagerd); ok {
		expected = svc.OnDisk
	} else {
		expected = ""
	}
	marker := ManagerdHandoffRecord{
		OperationID:     req.GetOperationId(),
		NodeID:          s.nodeID,
		Step:            req.GetStep(),
		Service:         managerdServiceName,
		LeaseID:         req.GetLeaseId(),
		ExpectedBuild:   expected,
		ScheduledAtUnix: time.Now().Unix(),
	}
	if err := s.handoff.Save(marker); err != nil {
		// Refused rather than restarted. A restart whose marker could
		// not be written produces a Comb whose replacement cannot find
		// out that anything was ever intended, which is rule 6's exact
		// failure: silence that would have to be read as success.
		return refuse("refusing the managerd restart: the durable handoff record could not be written, so a replacement managerd would have no way to learn that this restart was scheduled - nothing will be restarted: %v", err)
	}

	// STEP 3 and 4 are separate on purpose. The child is started, and
	// only then does this handler return the ACK. The delay inside the
	// child is what makes that ordering safe in practice rather than
	// merely intended.
	delay := clampHandoffDelay(req.GetDelayMs())
	if err := s.detachedRestarter.ArrangeRestart(managerdServiceName, delay); err != nil {
		// The marker stays. A replacement that comes up and finds a
		// marker naming an operation whose restart was never arranged
		// will report the step as unobserved and stop, which is the
		// honest answer, and clearing the marker here would leave that
		// honest answer with nothing to read.
		resp.MarkerPath = s.handoff.path()
		return refuse("refusing the managerd restart: the durable handoff record was written to %s but the detached restart could not be arranged (%v) - this managerd is NOT restarting, and the marker has been left in place so a later startup or an operator can see that a restart was scheduled and never happened", resp.MarkerPath, err)
	}

	resp.Accepted = true
	resp.ExpectedBuild = expected
	resp.MarkerPath = s.handoff.path()
	resp.Detail = fmt.Sprintf(
		"scheduled apiary_managerd to restart in %s from a detached child process; the durable record at %s names controlled update %q step %q, restart lease %d, and the build the replacement must find itself running (%s); the replacement process confirms the step itself on its next startup",
		delay, resp.MarkerPath, req.GetOperationId(), req.GetStep(), req.GetLeaseId(), orNoneBuild(expected))
	return resp, nil
}

// buildGateManagerd is the buildgate.Services entry this handoff reads.
// The gate checks all four daemons because a whole-Comb sweep needs all
// four; this path confirms one step about one daemon, and reads the one
// that is about to be replaced. Named rather than written as a literal
// so a gate that renames the service is a compile error here.
const buildGateManagerd = "managerd"

// orNoneBuild renders an absent build id in words, because a blank in the
// middle of a sentence reads to an operator as a value that was measured
// and found empty rather than as a value that was never read.
func orNoneBuild(id string) string {
	if id == "" {
		return "no build id could be read from the installed binary"
	}
	return id
}

// HandoffConfirmation is what a replacement managerd worked out about
// the restart it is confirming, and it is a value rather than a bare
// error so the caller can log AND record the same thing.
//
// The outcome vocabulary is internal/restartplan's, verbatim, for the same
// reason ColonyUpdateStepRecord uses it: a step that was attempted and
// whose evidence was unusable is neither a success nor a failure, and
// collapsing those two is how a Comb that came back on the wrong build
// ends up reported as one that came back.
type HandoffConfirmation struct {
	// Found is false when there was no marker at all, which is an
	// ordinary startup of a managerd that was not scheduled for a
	// controlled restart and is not a finding.
	Found bool

	// OperationID, NodeID and Step echo the marker so a log line is
	// self-contained.
	OperationID string
	NodeID      string
	Step        string

	// Outcome is one of internal/restartplan's constants. Empty when
	// Found is false.
	Outcome string

	// Detail is never empty when Found is true, and is the sentence an
	// operator reads in the durable record.
	Detail string

	// Evidence are the observations behind Outcome, one per reading
	// actually made. A confirmed or failed outcome must cite at least
	// one; internal/raft's own validateColonyUpdateStep refuses a
	// durable record that does not, so this is the same rule rather
	// than a second, weaker one.
	Evidence []string

	// LeaseReleased reports whether the cluster-wide restart lease was
	// confirmed by this process. False means it is still held, which
	// still blocks every other guarded restart of apiary_managerd in
	// the Colony.
	LeaseReleased bool
}

// ConfirmManagerdRestartHandoff is the rule 5 and rule 6 half of
// ADR-0146, called by cmd/managerd on this process's own startup.
//
// It is exported because cmd/managerd is the only caller, exactly as
// PendingRestart and RestartConfirmStore are: the confirmation belongs to
// the process that was restarted, and nothing else may do it on its
// behalf. A managerd that called this on some other Comb's behalf would be
// the exact violation rule 5 exists to prevent.
//
// It never returns an error for a missing marker - an ordinary startup
// has no marker and must be silent - and it never reports success it did
// not establish. The three outcomes it can reach are:
//
//   - confirmed: the build gate matched the marker's expected build, the
//     restart lease was confirmed, and the step record was appended;
//   - failed: positive evidence the restart did not take effect, which is
//     the running build being something other than the one on disk, or
//     the on-disk binary not being there at all;
//   - unobserved: the evidence could not be read, or the operation could
//     not be found, or the expected build was never recorded. The step
//     stops, and nothing is rendered as a success.
//
// The error return means the confirmation could not be COMPLETED, which
// is narrower than the build verdict: it is set when the record could
// not be read, or when the step could not be written to the operation's
// durable history. In both cases the marker is left in place, because it
// is the only durable evidence the step exists, and this process is the
// replacement - there is no third attempt unless one is deliberately
// made. That makes the call safe to retry, and the retry writes a
// byte-identical step record, which the FSM accepts as the exact replay
// it is.
func (s *Server) ConfirmManagerdRestartHandoff(ctx context.Context) (HandoffConfirmation, error) {
	var out HandoffConfirmation
	if s.handoff == nil {
		return out, fmt.Errorf("this managerd has no durable handoff record store configured, so it cannot say whether a controlled-update restart was scheduled for it")
	}
	marker, found, err := s.handoff.Load()
	if err != nil {
		// RULE 6. A marker that EXISTS and cannot be read is the case
		// this whole rule is about: a restart was scheduled here and
		// nobody can find out what it was for. There is no operation
		// id to record against, so the honest outcome is an error to
		// the caller and a loud log, never a silent skip and never a
		// success. The marker is deliberately NOT cleared.
		return out, err
	}
	if !found {
		return out, nil
	}
	out = HandoffConfirmation{
		Found:       true,
		OperationID: marker.OperationID,
		NodeID:      marker.NodeID,
		Step:        marker.Step,
	}
	if marker.NodeID != s.nodeID {
		// A marker describing a different Comb is a marker that cannot
		// be acted on honestly: the lease, the operation and the build
		// all belong to somebody else. Recording anything against it
		// here would put this Comb's step history on another Comb's
		// operation.
		out.Outcome = restartOutcomeUnobserved
		out.Detail = fmt.Sprintf("a managerd restart handoff record for controlled update %q exists on this host, but it names Comb %q while this node is %q, so this process cannot confirm a step that belongs to another Comb's restart; the record has been left in place and the controlled update is not advanced", marker.OperationID, marker.NodeID, s.nodeID)
		out.Evidence = []string{fmt.Sprintf("marker names node_id %q, this node is %q", marker.NodeID, s.nodeID)}
		return out, nil
	}

	// The build-identity gate: running versus on-disk, on this Comb,
	// with no network involved. See internal/buildgate's own header for
	// why that locality is the property and not a detail.
	report := buildgate.Confirm(s.nodeID, s.buildGatePaths())
	outcome, detail, evidence := handoffBuildVerdict(marker, report)
	out.Outcome, out.Detail, out.Evidence = outcome, detail, evidence

	// The lease is confirmed REGARDLESS of the build verdict, and the
	// asymmetry is deliberate. The lease says a restart is in flight and
	// unconfirmed; this process IS the restarted process, so it is
	// precisely the witness ADR-0103 and ADR-0146 rule 5 require, and
	// whether the new build is the one that was on disk is a different
	// question with a different answer. Withholding the lease
	// confirmation because the build is wrong would leave the Colony
	// blocked on a restart that demonstrably happened; confirming the
	// step as successful because the restart happened would be the lie
	// rule 6 forbids. The two answers are separate and both are
	// recorded.
	if resp, err := s.ConfirmRestartCompletedLocal(ctx, marker.Service, marker.NodeID, marker.LeaseID); err != nil {
		out.Detail += fmt.Sprintf("; the restart lease %d for %s on %s could not be confirmed at all: %v, so it is still held and still blocks other guarded restarts of that service", marker.LeaseID, marker.Service, marker.NodeID, err)
	} else if msg := resp.GetError(); msg != "" {
		out.Detail += fmt.Sprintf("; the restart lease %d for %s on %s was not confirmed: %s, so it is still held and still blocks other guarded restarts of that service", marker.LeaseID, marker.Service, marker.NodeID, msg)
	} else {
		out.LeaseReleased = true
		out.Evidence = append(out.Evidence, fmt.Sprintf("restart lease %d for %s on %s was confirmed by the restarted process itself (ADR-0103 revision note #16: never by the process that asked for the restart)", marker.LeaseID, marker.Service, marker.NodeID))
	}

	// The step record, appended to the operation's own durable history.
	// It is attempted on EVERY outcome, including the unknown ones,
	// because "nobody could tell" is a fact an operator needs to read
	// back later and a step that is merely absent from the record reads
	// as a step nobody reached.
	//
	// The sentence about this step's own outcome is kept OUT of the
	// record on purpose. The record's detail is the build verdict and
	// the lease confirmation, both of which are byte-identical on a
	// retry; appending "and then recording it failed" would make the
	// second attempt's record differ from the first's at an already
	// occupied step position, which the FSM refuses as an attempt to
	// rewrite history. A lost response on the first attempt must leave
	// the retry able to complete, and it can only do that if what it
	// would write is the same thing both times.
	if err := s.recordHandoffStep(ctx, marker, out); err != nil {
		// The marker is deliberately NOT cleared on this path. It is
		// the only durable evidence that this step exists at all, and
		// clearing it because the raft round trip was not ready would
		// lose the step permanently - the replacement process is this
		// process, so there is no third attempt unless one is
		// deliberately made.
		out.Detail += fmt.Sprintf("; the step outcome could NOT be recorded against controlled update %q, so the operation's durable history does not know this step happened and the controlled update must not continue past it: %v", marker.OperationID, err)
		return out, err
	}

	// The marker is cleared only now: every reading that could be made
	// from it has been made, and written down somewhere that outlives
	// this process. Leaving it would make every subsequent startup of
	// this process re-report a step that was already recorded.
	if err := s.handoff.Clear(); err != nil {
		out.Detail += fmt.Sprintf("; the handoff record could not be removed, so the next startup of this process will report this step again: %v", err)
	}
	return out, nil
}

// handoffBuildVerdict turns the build gate's report plus the marker's
// expected build into an outcome, a sentence and the evidence behind
// both.
//
// Every branch names the two readings it compared, because "which is
// which" is the first thing a reader needs and the second thing they
// should have to look up - internal/buildgate's own summary() argument,
// applied here.
func handoffBuildVerdict(marker ManagerdHandoffRecord, report buildgate.Report) (outcome, detail string, evidence []string) {
	svc, ok := report.Service(buildGateManagerd)
	if !ok {
		return restartOutcomeUnobserved,
			fmt.Sprintf("the build gate read this Comb and produced no answer for %s, so nothing was established about which build is running; the step is unobserved and the controlled update must not continue on the assumption that it worked", buildGateManagerd),
			[]string{fmt.Sprintf("buildgate.Services is %v, which does not include %q", buildgate.Services, buildGateManagerd)}
	}
	readings := []string{
		fmt.Sprintf("managerd build id: running %s, on disk %s", orNoneBuild(svc.Running), orNoneBuild(svc.OnDisk)),
		fmt.Sprintf("build gate verdict for %s: %s (%s)", buildGateManagerd, svc.Status, svc.Detail),
	}
	if marker.ExpectedBuild == "" {
		// Not a mismatch. An absent expectation cannot be met, and
		// treating it as vacuously met is the single easiest way to
		// turn this gate into a rubber stamp.
		return restartOutcomeUnobserved,
			"the restart was scheduled without a recorded expected build identity, so the replacement process has nothing to confirm itself against; the step is unobserved rather than confirmed, and a build that was installed by hand or stamped with no -ldflags is the usual reason",
			append(readings, "the durable marker recorded no expected build id")
	}
	if svc.Status != buildgate.TookEffect {
		// Not running the build on disk. Positive evidence that the
		// install did not take effect through this restart, which is
		// a failure rather than an absence of evidence.
		return restartOutcomeFailed,
			fmt.Sprintf("apiary_managerd restarted but is not running the build sitting beside it (%s), so the install did not take effect through this restart: %s", orNoneBuild(svc.OnDisk), svc.Detail),
			append(readings, fmt.Sprintf("expected build %q was on disk before the restart", marker.ExpectedBuild))
	}
	if svc.Running != marker.ExpectedBuild {
		return restartOutcomeFailed,
			fmt.Sprintf("apiary_managerd is running build %s but the restart was scheduled against build %s, which is what was on disk when it was; the Comb came back on a build nobody asked for", orNoneBuild(svc.Running), marker.ExpectedBuild),
			append(readings, fmt.Sprintf("the marker expected %q and the running process reports %q", marker.ExpectedBuild, svc.Running))
	}
	return restartOutcomeConfirmed,
		fmt.Sprintf("apiary_managerd restarted and is running build %s, which is the build that was on disk when the restart was scheduled; the restart lease was confirmed by this process and the step is recorded against controlled update %q", marker.ExpectedBuild, marker.OperationID),
		append(readings, fmt.Sprintf("the running process and the durable marker agree on build %q", marker.ExpectedBuild))
}

// recordHandoffStep appends the replacement's verdict to the operation's
// own durable step history.
//
// The fence it presents is READ BACK from this node's own raftd rather
// than taken from the request or invented, and that deserves an
// explanation because it looks like it defeats the fencing. It does not:
// the fence here is not a claim of coordination, it is a claim of "this
// is the operation my durable marker names, as this node's raftd
// currently has it". The FSM re-checks the same four fields against the
// ACTIVE record by exact match, so a stale local read - a follower's copy
// that lags the leader by a commit - produces a precise refusal rather
// than a step record attached to the wrong operation. A replacement
// process has no fence of its own to present; it is not the coordinator,
// and ADR-0146 rule 5 asks it to record what it found rather than to
// act as one.
func (s *Server) recordHandoffStep(ctx context.Context, marker ManagerdHandoffRecord, out HandoffConfirmation) error {
	if s.raft == nil {
		return fmt.Errorf("this node has no raft client configured, so the operation's own record cannot be read and no step can be recorded")
	}
	state, err := s.raft.GetColonyUpdateStateLocal(ctx, marker.OperationID)
	if err != nil {
		return fmt.Errorf("reading controlled update %q from this node's own raftd: %w", marker.OperationID, err)
	}
	if msg := state.GetError(); msg != "" {
		return fmt.Errorf("reading controlled update %q: %s", marker.OperationID, msg)
	}
	active := state.GetActive()
	if active == nil {
		return fmt.Errorf("controlled update %q is not active, so there is nothing to record this step against - it has already been settled, or it was never granted on this node's view", marker.OperationID)
	}
	if active.GetOperationId() != marker.OperationID {
		return fmt.Errorf("this node's raftd reports controlled update %q as active, which is not the one this restart belonged to (%q), so the step was not recorded", active.GetOperationId(), marker.OperationID)
	}

	resp, err := s.mutateColonyUpdate(ctx, &rpcpb.MutateColonyUpdateRequest{
		Op: &rpcpb.MutateColonyUpdateRequest_Advance{
			Advance: &rpcpb.AdvanceColonyUpdate{
				Fence: &rpcpb.ColonyUpdateFence{
					OperationId:       active.GetOperationId(),
					HolderNodeId:      active.GetHolderNodeId(),
					FenceToken:        active.GetFenceToken(),
					HolderIncarnation: active.GetHolderIncarnation(),
				},
				Step:         marker.Step,
				TargetNodeId: marker.NodeID,
				Detail:       out.Detail,
				StepRecord: &rpcpb.ColonyUpdateStepRecord{
					// The ordinal position is the operation's own
					// step count, because the FSM appends only at
					// exactly the next free position and refuses a
					// gap. Replaying an already-recorded position
					// with an identical record is accepted, so a
					// retry of this whole path is safe.
					Index:    uint32(len(active.GetSteps())),
					Step:     marker.Step,
					Outcome:  out.Outcome,
					Detail:   out.Detail,
					Evidence: append([]string(nil), out.Evidence...),
					NodeId:   marker.NodeID,
				},
			},
		},
	})
	if err != nil {
		return fmt.Errorf("recording step %q of controlled update %q: %w", marker.Step, marker.OperationID, err)
	}
	if msg := resp.GetError(); msg != "" {
		return fmt.Errorf("recording step %q of controlled update %q: %s", marker.Step, marker.OperationID, msg)
	}
	return nil
}

// restartOutcome* restate internal/restartplan's Outcome constants for
// this file's use, the same way internal/raft/colonyupdate.go restates
// them. They are strings on the wire and in the durable record, so
// importing the type here would be a cast at every use rather than a
// guarantee; what the restatement buys is that a rename upstream cannot
// silently produce a value the FSM rejects.
const (
	restartOutcomeConfirmed  = "confirmed"
	restartOutcomeFailed     = "failed"
	restartOutcomeUnobserved = "unobserved"
)

// SetColonyUpdateHandoffStore wires the durable marker store and the
// detached restarter that IssueManagerdRestart and
// ConfirmManagerdRestartHandoff need. Setters, not NewServer parameters,
// for the same reason SetRestartConfirmStore is one: they are process
// wiring cmd/managerd owns, and a Server built in a test without them
// must be a Server that REFUSES a managerd restart rather than one that
// performs it in-process.
func (s *Server) SetColonyUpdateHandoffStore(store *ManagerdHandoffStore, restarter DetachedRestarter) {
	s.handoff = store
	s.detachedRestarter = restarter
}

// buildGatePaths is where this node's build evidence is read from, with
// the production default and a test override. The override exists so a
// test can exercise the whole gate - both readings, the comparison and
// the verdict - against a real pair of a log and a binary, which is the
// only way the "running versus on-disk" claim is tested rather than
// asserted.
func (s *Server) buildGatePaths() buildgate.Paths {
	if s.handoffBuildPaths.LibexecDir != "" && s.handoffBuildPaths.LogDir != "" {
		return s.handoffBuildPaths
	}
	return buildgate.DefaultPaths()
}

// SetColonyUpdateHandoffBuildPaths overrides where the build-identity
// gate reads, for tests. Both directories must be set; a partial
// override is ignored rather than half-applied, because a gate pointed
// at a real libexec directory and a test log directory would answer
// about a binary nobody is running.
func (s *Server) SetColonyUpdateHandoffBuildPaths(paths buildgate.Paths) {
	s.handoffBuildPaths = paths
}
