package manager

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	internalpb "github.com/glenjbarber/apiary/api/internalpb"
	"github.com/glenjbarber/apiary/internal/guardrail"
	"github.com/glenjbarber/apiary/internal/restartplan"
)

// restartGuardrailServices is the set of services whose restart goes
// through the action-preflight restart guardrail (ADR-0103) - the ones
// carrying a real cluster-wide risk, and therefore needing a
// cluster-wide Raft lease reserved before the restart happens.
//
//	apiary_managerd - ADR-0103's original case. Restarting it severs the
//	  very process handling the RestartNodeService call, so that process
//	  can never confirm its own restart's outcome; confirmation comes
//	  from the replacement process's own startup instead.
//
//	apiary_raftd - ADR-0125. Consensus-critical, and the only service in
//	  the inventory whose restart can cost the cluster its quorum. It
//	  additionally has to clear evaluateRaftdQuorumSafety below, a
//	  question apiary_managerd has no stake in.
//
// frontend and restshimd are deliberately absent. They are separate
// processes, so restarting one never severs the RPC carrying the call,
// and neither can cost quorum. That is the same reasoning ADR-0102 used
// to make them merely `restartable: true` in the inventory below.
var restartGuardrailServices = map[string]bool{
	"apiary_managerd": true,
	"apiary_raftd":    true,
}

// guardrailService reports whether name's restart must reserve a lease
// first. A name absent from the map is not guardrailed, which is safe
// only because restartGuardrailServices is a closed set this package
// owns rather than something a caller supplies.
func guardrailService(name string) bool { return restartGuardrailServices[name] }

// raftStatus reads local raft status, or reports an error instead of
// panicking when this Server has no raft client at all.
//
// Every guardrail decision in this file and in server.go's restart path
// begins with a status read, and every one of them must FAIL CLOSED: an
// unreadable status means the membership is unknown, which is exactly
// the condition that has to produce a refusal rather than a guess.
// Calling s.raft.Status directly made that impossible to honour in the
// one case where it matters most, because a nil s.raft is a nil-pointer
// dereference, not an error - and gRPC does not recover a panicking
// handler, so the process dies. A managerd that crashes on a raftd
// restart request is strictly worse than one that refuses it, and the
// crash is the more likely of the two to be mistaken for a working
// guardrail right up until the moment it isn't.
//
// A nil raft client is not reachable in a correctly configured
// deployment, which is exactly why it went unnoticed: the guardrail
// was unreachable too, so the path was never walked.
func (s *Server) raftStatus(ctx context.Context) (*internalpb.StatusResponse, error) {
	if s.raft == nil {
		return nil, errors.New("this node has no raft client configured, so cluster membership cannot be read and no restart guardrail can be evaluated")
	}
	return s.raft.Status(ctx)
}

// isRaftdGuardrailed reports whether name is the raftd service whose
// restart additionally has to clear the quorum-safety evaluation.
//
// It is deliberately not simply `name == restartplan.DefaultService`:
// DefaultService is this package's idea of "the service a raftd restart
// plan is about", and coupling the production gate to it would mean a
// change to a planning constant could silently switch a cluster-quorum
// check on or off. Naming the condition here keeps the two independent.
func isRaftdGuardrailed(name string) bool { return name == raftdServiceName }

// raftdServiceName is the rc.d service name for the Raft daemon. It is
// the same string as restartplan.DefaultService, restated as a constant
// this package owns so the guardrail's trigger does not move when a
// planning default does.
const raftdServiceName = "apiary_raftd"

// describeGuardrailBlock renders a non-Allow report as the single
// operator-facing sentence a ReserveRestartLease response carries.
//
// Findings are joined rather than summarised: a Block can rest on more
// than one rule (quorum arithmetic AND leader-restart, say), and
// dropping all but the first would understate the reason for a refusal
// the operator is being asked to override.
func describeGuardrailBlock(report guardrail.Report) string {
	parts := make([]string, 0, len(report.Findings))
	for _, f := range report.Findings {
		parts = append(parts, f.Detail)
	}
	switch {
	case len(parts) == 0:
		// A report that blocked without saying why is itself a bug
		// worth surfacing verbatim rather than papering over with a
		// generic sentence that would read like a real reason.
		return fmt.Sprintf("refusing to restart: the quorum-safety guardrail returned %q with no finding to explain it", report.Verdict)
	case len(parts) == 1:
		return "refusing to restart: " + parts[0]
	default:
		return "refusing to restart: " + strings.Join(parts, "; ")
	}
}

// forceDowngradedBlock reports whether an Allow verdict arrived only
// because Force downgraded a Block into caveats - so the caller can
// report the override to the operator instead of letting an
// acknowledged-but-dangerous restart look like a clean one.
//
// The test is a Block finding having become a caveat, not the mere
// presence of caveats: EvaluateQuorumSafety also emits caveats for
// entirely benign reasons (a non-voter target, an unknown-but-not-
// fatal reachability), and those must not be mistaken for an override.
func forceDowngradedBlock(report guardrail.Report) bool {
	for _, c := range report.Caveats {
		if strings.HasPrefix(c.Detail, "operator acknowledged this and restarted anyway: ") {
			return true
		}
	}
	return false
}

// evaluateRaftdQuorumSafety is ADR-0125 §3/§4's check wired to real
// facts: it reads this node's own raft status, TCP-dials every OTHER
// voter, and hands the result to restartplan.EvaluateQuorumSafety.
//
// Calling the same function the advisory preflight calls is the point,
// not an incidental reuse: ADR-0125 requires the preview an operator
// sees and the check that actually gates the restart to be incapable of
// drifting apart, and the only way to guarantee that is for there to be
// exactly one implementation they both call.
//
// targetNodeID is the node whose raftd is actually being judged, and it
// is a parameter rather than an implicit s.nodeID because the enforcing
// call site is NOT always running on the target. A follower forwards
// ReserveRestartLease to the leader, and the leader then evaluates the
// request - for the follower's raftd, not its own. An earlier revision
// hardcoded s.nodeID here on the reasoning that "neither RPC is a
// cluster-wide restart-node-X call"; live verification on a real
// four-voter cluster disproved that. The leader really is being asked
// about another node, and hardcoding s.nodeID made every
// follower-initiated raftd restart fail with a message naming the
// leader as the node being protected - safe, but wrong, and it blocked
// the feature on every node except the current one.
//
// There is deliberately no "empty means this node" fallback. Both local
// callers pass their own node id explicitly, so the fallback would only
// ever fire for a forwarded request that lost its node_id in transit -
// and silently answering such a request about the leader is precisely
// the confusion this parameter exists to remove. An empty target is
// reported below as a target that is not a member, which is Unknown.
//
// Caller posture differs by call site and is load-bearing:
//
//   - Enforcement (reserveRestartLease's leader branch) calls this on
//     the leader, where the membership read is authoritative, passing
//     the target from the request. This is the only call that can block
//     a restart.
//   - Advisory (PreflightRestartNodeService) may call it on a follower,
//     for this node. A follower's raft status can lag the leader's on
//     membership, so its answer is honest but not necessarily current -
//     which is acceptable for a preview, and is exactly why the
//     enforcing call is not made from a follower.
//
// Leadership is read as "is the target the node raft says is leader",
// not as "is this node the leader". Only the former is the question
// ADR-0125 asks, and on the enforcing path the two differ by
// construction.
func (s *Server) evaluateRaftdQuorumSafety(ctx context.Context, service, targetNodeID string, force bool) guardrail.Report {
	fact := restartplan.QuorumFact{
		Service:      service,
		TargetNodeID: targetNodeID,
		Force:        force,
		ObservedAt:   time.Now(),
	}

	status, err := s.raftStatus(ctx)
	if err != nil {
		// No status means no membership, which means we cannot even
		// establish whether this node is a voter - so we cannot reach
		// the "a non-voter carries no quorum risk" conclusion either.
		// EvaluateQuorumSafety turns this into Unknown (never Allow) and
		// Force does not rescue it, which is the fail-closed behaviour
		// ADR-0125 §8 requires.
		fact.ProbeReadOK = false
		fact.ProbeError = err.Error()
		return restartplan.EvaluateQuorumSafety(fact)
	}
	fact.ProbeReadOK = true

	// Leadership is a question about the TARGET, asked of the raft
	// status this node just read. An empty leader_id means nobody knows
	// who the leader is right now (an election is in progress, or this
	// node has not yet learned of one) - which is not the same as
	// "the target is not the leader", and must not be read as
	// permission. It is reported as an unreadable fact so the verdict
	// is Unknown, which Force cannot rescue.
	if status.GetLeaderId() == "" {
		fact.ProbeReadOK = false
		fact.ProbeError = "raft status reports no current leader, so it cannot be established whether the target is the leader"
		return restartplan.EvaluateQuorumSafety(fact)
	}
	fact.IsTargetLeader = status.GetLeaderId() == targetNodeID

	// Per ADR-0103's own "only actual Raft voters count" rule, and
	// because a non-voter's address is not something a quorum argument
	// is about: split the membership into the target and the peers that
	// actually have a stake.
	//
	// The target is looked up by its own id, which on the enforcing
	// path is a node other than this one.
	targetIsMember := false
	var others []restartplan.Voter
	for _, srv := range status.GetServers() {
		if srv.GetId() == targetNodeID {
			targetIsMember = true
			if srv.GetSuffrage() == "Voter" {
				fact.IsTargetVoter = true
			}
			continue
		}
		if srv.GetSuffrage() != "Voter" {
			continue
		}
		others = append(others, restartplan.Voter{
			NodeID:          srv.GetId(),
			RaftBindAddress: srv.GetAddress(),
		})
	}

	// A target that is not in the membership at all is NOT the same
	// fact as a target that is a member with no vote. EvaluateQuorumSafety
	// reads !IsTargetVoter as "a non-voter carries no quorum risk, Allow"
	// (ADR-0125 §4 rule 2), which is correct for a real non-voter and
	// dangerously wrong for a node id nobody has heard of - including an
	// empty or misspelled one arriving over the forwarded RPC. Rather
	// than let an unrecognised target borrow the non-voter's exemption,
	// this is reported as a fact that could not be established, so the
	// verdict is Unknown and Force cannot rescue it.
	if !targetIsMember {
		fact.ProbeReadOK = false
		fact.ProbeError = fmt.Sprintf("target node %q is not a member of the cluster as this node knows it, so its quorum standing cannot be established", targetNodeID)
		return restartplan.EvaluateQuorumSafety(fact)
	}

	// A single-voter cluster legitimately probes nobody. ProbeVoters
	// reports that as a readable probe over an empty set rather than a
	// failure, and ComputeQuorum then blocks the restart on its own
	// arithmetic - restarting the only voter does cost the cluster its
	// quorum, so that verdict is correct and not an artefact of the
	// empty probe.
	probeOpts := restartplan.DefaultProbeOptions()
	probeOpts.Dial = dialReachable
	probe := restartplan.ProbeVoters(ctx, others, probeOpts)
	if !probe.ReadOK {
		// The probe could not run (deadline, no dial primitive, an
		// already-cancelled context). Every voter is then unprobed
		// rather than down, and crediting them as up is the one answer
		// that must never be manufactured here.
		fact.ProbeReadOK = false
		fact.ProbeError = probe.Error
		return restartplan.EvaluateQuorumSafety(fact)
	}
	fact.OtherVoters = probe.Voters

	return restartplan.EvaluateQuorumSafety(fact)
}
