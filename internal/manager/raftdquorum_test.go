package manager

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"google.golang.org/grpc"

	internalpb "github.com/glenjbarber/apiary/api/internalpb"
	rpcpb "github.com/glenjbarber/apiary/api/rpc"
	"github.com/glenjbarber/apiary/internal/guardrail"
	"github.com/glenjbarber/apiary/internal/invariant"
	raftnode "github.com/glenjbarber/apiary/internal/raft"
	"github.com/glenjbarber/apiary/internal/restartplan"
)

// Tests for the ADR-0125 quorum-safety wiring: the fact-gathering in
// internal/manager, and the report-merging that lets one preflight show
// both ADR-0103's and ADR-0125's reasons at once.
//
// The pure evaluation (restartplan.EvaluateQuorumSafety) has its own
// exhaustive table tests in that package. What is tested HERE is
// everything those tests cannot reach: that this package reads the right
// facts off a real raft status response, that it probes real addresses,
// that a target is excluded from its own probe, and that the two
// independent reports merge without one of them quietly winning.

// newQuorumTestRaftd starts a real single-node raft and returns both its
// socket path and the node itself, so a test can AddVoter to build the
// multi-voter membership the quorum arithmetic actually needs.
//
// It exists because a single-voter cluster cannot express the
// interesting cases: with one voter there are no "other voters" to probe
// and no way to distinguish "the rest can form a majority" from "there
// is no rest". Returning the node (rather than only the socket, as
// newRaftdUDSSocket does) is what makes AddVoter reachable from a test.
func newQuorumTestRaftd(t *testing.T) (socket string, node *raftnode.Node) {
	t.Helper()

	cfg := raftnode.Config{
		NodeID:   "raftd-1",
		DataDir:  t.TempDir(),
		BindAddr: freeLoopbackAddr(t),
	}
	node, err := raftnode.New(cfg)
	if err != nil {
		t.Fatalf("raftnode.New() error: %v", err)
	}
	if err := node.Bootstrap(); err != nil {
		t.Fatalf("Bootstrap() error: %v", err)
	}
	eventually(t, 5*time.Second, func() bool { return node.Status().IsLeader })

	socketDir, err := os.MkdirTemp("", "quorum-uds")
	if err != nil {
		t.Fatalf("MkdirTemp() error: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(socketDir) })
	socketPath := filepath.Join(socketDir, "raftd.sock")

	lis, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("Listen(unix) error: %v", err)
	}
	grpcServer := grpc.NewServer()
	internalpb.RegisterRaftInternalServer(grpcServer, raftnode.NewServer(node))
	go grpcServer.Serve(lis)
	t.Cleanup(func() {
		grpcServer.GracefulStop()
		node.Shutdown()
	})
	return socketPath, node
}

// addUnreachableVoter adds a second voter bound to an address nothing is
// listening on, so the probe gets a real, genuine connection-refused
// rather than a simulated one. A closed loopback port is the most
// honest stand-in for a downed Comb available in a unit test.
func addUnreachableVoter(t *testing.T, node *raftnode.Node) string {
	t.Helper()
	addr := freeLoopbackAddr(t)
	if err := node.AddVoter("raftd-2", addr, 0, 5*time.Second); err != nil {
		t.Fatalf("AddVoter() error: %v", err)
	}
	return addr
}

// addNonvoter adds a member that genuinely carries no vote, so the
// "a non-voter has no quorum standing" rule can be exercised against a
// real membership entry rather than against a node id that is merely
// absent from the cluster.
func addNonvoter(t *testing.T, node *raftnode.Node, id string) {
	t.Helper()
	eventually(t, 5*time.Second, func() bool { return node.Status().IsLeader })
	if err := node.AddNonvoter(id, freeLoopbackAddr(t), 0, 5*time.Second); err != nil {
		t.Fatalf("AddNonvoter(%s) error: %v", id, err)
	}
}

func newQuorumTestServer(t *testing.T, socket, nodeID string) *Server {
	t.Helper()
	raftClient, err := Dial(socket, "")
	if err != nil {
		t.Fatalf("Dial() error: %v", err)
	}
	t.Cleanup(func() { raftClient.Close() })
	return NewServer(raftClient, nodeID, nil, nil, nil, nil, nil, "", nil, nil, nil, 0, nil)
}

// TestEvaluateRaftdQuorumSafety_SoleVoterIsBlocked is the case that
// justifies the whole exercise: a one-voter cluster where this node is
// both the leader and the entire membership. Restarting it costs the
// cluster its quorum AND forces an election, so it must be blocked on
// both counts - and the findings must say which is which, since an
// operator overriding one is not overriding the other.
func TestEvaluateRaftdQuorumSafety_SoleVoterIsBlocked(t *testing.T) {
	socket, _ := newQuorumTestRaftd(t)
	s := newQuorumTestServer(t, socket, "raftd-1")

	report := s.evaluateRaftdQuorumSafety(context.Background(), raftdServiceName, "raftd-1", false)

	if report.Verdict != guardrail.Block {
		t.Fatalf("verdict = %q, want %q; restarting the only voter of a one-voter cluster must be blocked", report.Verdict, guardrail.Block)
	}
	rules := map[string]bool{}
	for _, f := range report.Findings {
		rules[f.Rule] = true
	}
	if !rules[restartplan.RuleQuorumSafety] {
		t.Errorf("missing a %s finding; rules present = %v", restartplan.RuleQuorumSafety, rules)
	}
	if !rules[restartplan.RuleLeaderRestart] {
		t.Errorf("missing a %s finding; rules present = %v", restartplan.RuleLeaderRestart, rules)
	}
}

// TestEvaluateRaftdQuorumSafety_DeadPeerBlocksQuorum is the realistic
// production shape: a healthy-looking local node whose cluster has
// already lost a peer. The probe must reach a genuine verdict about the
// missing voter, not a default.
func TestEvaluateRaftdQuorumSafety_DeadPeerBlocksQuorum(t *testing.T) {
	socket, node := newQuorumTestRaftd(t)
	addUnreachableVoter(t, node)
	s := newQuorumTestServer(t, socket, "raftd-1")

	report := s.evaluateRaftdQuorumSafety(context.Background(), raftdServiceName, "raftd-1", false)

	if report.Verdict != guardrail.Block {
		t.Fatalf("verdict = %q, want %q; 2 voters with 1 down, quorum is 2, so restarting the survivor loses quorum", report.Verdict, guardrail.Block)
	}
	// The finding must cite the arithmetic, so the refusal is checkable
	// by hand rather than merely asserted.
	quoted := false
	for _, f := range report.Findings {
		if f.Rule == restartplan.RuleQuorumSafety {
			for _, e := range f.Evidence {
				if e.Detail != "" {
					quoted = true
				}
			}
		}
	}
	if !quoted {
		t.Errorf("the quorum finding carried no evidence; the operator is told no and shown nothing")
	}
}

// TestEvaluateRaftdQuorumSafety_NonVoterIsAllowedBecauseItHasNoStake
// pins the escape hatch that keeps the guardrail from being
// unusable: a member that is not a voter can be restarted freely,
// because its restart cannot change what quorum remains.
//
// The target here is a genuine Nonvoter ENTRY in the membership. An
// earlier version of this test used a node id that was simply absent
// from the cluster, which is a different fact entirely - see
// TestEvaluateRaftdQuorumSafety_UnknownTargetIsNotBorrowedFromTheNonVoterExemption
// - and let the implementation conflate the two.
func TestEvaluateRaftdQuorumSafety_NonVoterIsAllowedBecauseItHasNoStake(t *testing.T) {
	socket, node := newQuorumTestRaftd(t)
	addNonvoter(t, node, "observer-1")
	// This node is the leader and the only voter; the observer is the
	// target, so its restart costs the cluster nothing.
	s := newQuorumTestServer(t, socket, "raftd-1")

	report := s.evaluateRaftdQuorumSafety(context.Background(), raftdServiceName, "observer-1", false)

	if report.Verdict != guardrail.Allow {
		t.Errorf("verdict = %q, want %q; a non-voter's restart carries no quorum risk (findings: %+v)", report.Verdict, guardrail.Allow, report.Findings)
	}
	// And the question must be recorded as asked, not skipped silently.
	if len(report.Caveats) == 0 {
		t.Errorf("no caveat recorded; an operator cannot tell 'checked, not a voter' from 'never checked'")
	}
}

// TestEvaluateRaftdQuorumSafety_UnknownTargetIsNotBorrowedFromTheNonVoterExemption
// is the fail-closed half of the rule above. "The target is a member
// with no vote" and "the target is not a member at all" both leave
// IsTargetVoter false, and EvaluateQuorumSafety reads that as Allow -
// correctly for the first, dangerously for the second, because an empty
// or misspelled node_id arriving over the forwarded ReserveRestartLease
// RPC is exactly the shape a caller can send. An unrecognised target
// must be Unknown, which Force cannot rescue.
func TestEvaluateRaftdQuorumSafety_UnknownTargetIsNotBorrowedFromTheNonVoterExemption(t *testing.T) {
	socket, _ := newQuorumTestRaftd(t)
	s := newQuorumTestServer(t, socket, "raftd-1")

	for _, target := range []string{"", "no-such-node", "raftd-1 "} {
		report := s.evaluateRaftdQuorumSafety(context.Background(), raftdServiceName, target, true)
		if report.Verdict != guardrail.Unknown {
			t.Errorf("target %q: verdict = %q, want %q; an unrecognised target has no established quorum standing, and force must not manufacture one", target, report.Verdict, guardrail.Unknown)
		}
	}
}

// TestEvaluateRaftdQuorumSafety_JudgesTheTargetNotTheAskingNode is the
// regression test for a defect found in live verification on a real
// four-voter cluster, not in review.
//
// A follower forwards ReserveRestartLease to the leader, so the leader
// is evaluating a node that is not itself. The implementation used to
// hardcode s.nodeID as the target and read leadership as "am I the
// leader", so on the enforcing path it answered "is it safe to restart
// the LEADER's raftd" to a follower asking about its own. Every
// follower-initiated raftd restart was then refused, citing the leader
// as the node being protected - safe, but wrong, and it made the
// feature unusable on every node except the current one.
//
// The two subtests below differ only in the target. Same membership,
// same node, same everything else.
func TestEvaluateRaftdQuorumSafety_JudgesTheTargetNotTheAskingNode(t *testing.T) {
	// A single-voter cluster stays led by its one member, which is what
	// makes "this node is the leader AND the target is something else"
	// expressible at all. A second, fake voter cannot be built here: a
	// plain listening socket completes TCP but never answers raft, so
	// adding one costs this node its leadership outright and there is
	// then no leader for the bug to be confused with.
	newCluster := func(t *testing.T) *Server {
		t.Helper()
		socket, node := newQuorumTestRaftd(t)
		addNonvoter(t, node, "observer-1")
		return newQuorumTestServer(t, socket, "raftd-1")
	}

	t.Run("non-voter target is allowed even though this node is the leader", func(t *testing.T) {
		s := newCluster(t)

		report := s.evaluateRaftdQuorumSafety(context.Background(), raftdServiceName, "observer-1", false)

		if report.Verdict != guardrail.Allow {
			t.Fatalf("verdict = %q, want %q; this node is the leader but observer-1 is the target, and a non-voter target carries no quorum risk (findings: %+v)", report.Verdict, guardrail.Allow, report.Findings)
		}
		for _, f := range report.Findings {
			if f.Rule == restartplan.RuleLeaderRestart {
				t.Errorf("a leader-restart finding was raised for target observer-1 while this node happens to be the leader; the rule must be about the target, not the asker")
			}
		}
	})

	t.Run("leader target is still blocked on the same cluster", func(t *testing.T) {
		s := newCluster(t)

		report := s.evaluateRaftdQuorumSafety(context.Background(), raftdServiceName, "raftd-1", false)

		if report.Verdict != guardrail.Block {
			t.Fatalf("verdict = %q, want %q; the target here IS the leader and must be blocked (findings: %+v)", report.Verdict, guardrail.Block, report.Findings)
		}
		found := false
		for _, f := range report.Findings {
			if f.Rule == restartplan.RuleLeaderRestart {
				found = true
			}
		}
		if !found {
			t.Errorf("no %s finding for a leader target; rules present = %+v", restartplan.RuleLeaderRestart, report.Findings)
		}
	})
}

// TestEvaluateRaftdQuorumSafety_JudgesANonAskerVoterTargetOnItsOwnMerits
// is the other half of the same fix. A voter target that is NOT the
// asking node must be judged by its own quorum standing - here, a
// two-voter cluster where the other voter is gone, so restarting this
// target drops the cluster below its majority. Reading the asker's
// standing instead would let this through whenever the asker happened
// to be somewhere safer.
func TestEvaluateRaftdQuorumSafety_JudgesANonAskerVoterTargetOnItsOwnMerits(t *testing.T) {
	socket, node := newQuorumTestRaftd(t)
	addUnreachableVoter(t, node)
	s := newQuorumTestServer(t, socket, "raftd-1")

	report := s.evaluateRaftdQuorumSafety(context.Background(), raftdServiceName, "raftd-2", false)

	if report.Verdict != guardrail.Block {
		t.Fatalf("verdict = %q, want %q; 2 voters with 1 down means quorum 2, so restarting either one loses it", report.Verdict, guardrail.Block)
	}
}

// TestEvaluateRaftdQuorumSafety_ForceCannotRescueAnUnknown is the single
// most important asymmetry in ADR-0125, and the easiest to get wrong.
// Force may acknowledge a real "this costs quorum" Block. It may never
// rescue an Unknown, because there is no established fact to
// acknowledge - only an absence of evidence.
func TestEvaluateRaftdQuorumSafety_ForceCannotRescueAnUnknown(t *testing.T) {
	s := NewServer(nil, "raftd-1", nil, nil, nil, nil, nil, "", nil, nil, nil, 0, nil)

	report := s.evaluateRaftdQuorumSafety(context.Background(), raftdServiceName, "raftd-1", true)

	if report.Verdict != guardrail.Unknown {
		t.Fatalf("verdict = %q, want %q; no raft client means membership is unknown, and force must not turn that into an allow", report.Verdict, guardrail.Unknown)
	}
	for _, c := range report.Caveats {
		if c.Detail == "" {
			t.Errorf("empty caveat detail")
		}
	}
	// An Unknown must never be dressed up as an override.
	if forceDowngradedBlock(report) {
		t.Errorf("forceDowngradedBlock reported an override on an Unknown; nothing was ever established to override")
	}
}

// TestMoreRestrictiveVerdict_UnknownIsNeverSofterThanAllow is the merge
// rule that keeps a fail-closed evaluation from being undone by a
// permissive one. The ranking is deliberate and is the thing a future
// edit to this function is most likely to break, so it is pinned
// pairwise.
func TestMoreRestrictiveVerdict_UnknownIsNeverSofterThanAllow(t *testing.T) {
	cases := []struct {
		name string
		a, b guardrail.Verdict
		want guardrail.Verdict
	}{
		{"allow and allow", guardrail.Allow, guardrail.Allow, guardrail.Allow},
		{"allow and block -> block", guardrail.Allow, guardrail.Block, guardrail.Block},
		{"block and allow -> block", guardrail.Block, guardrail.Allow, guardrail.Block},
		{"allow and unknown -> unknown", guardrail.Allow, guardrail.Unknown, guardrail.Unknown},
		{"unknown and allow -> unknown", guardrail.Unknown, guardrail.Allow, guardrail.Unknown},
		{"block and unknown -> unknown", guardrail.Block, guardrail.Unknown, guardrail.Unknown},
		{"unknown and block -> unknown", guardrail.Unknown, guardrail.Block, guardrail.Unknown},
		// An unrecognised verdict is somebody else's future addition.
		// Ranking it below Allow would be fail-open on a value this code
		// has never seen.
		{"allow and a verdict we do not know -> the unknown one", guardrail.Allow, guardrail.Verdict("something-new"), guardrail.Verdict("something-new")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := moreRestrictiveVerdict(tc.a, tc.b); got != tc.want {
				t.Errorf("moreRestrictiveVerdict(%q, %q) = %q, want %q", tc.a, tc.b, got, tc.want)
			}
		})
	}
}

// TestMergeGuardrailReports_KeepsBothRuleFamilies matters because the
// frontend keys its operator-facing copy off Rule. A merge that kept
// only the winning report's findings would show "quorum would be lost"
// while silently dropping an active-lease block, and the operator would
// see a reason that is not the reason they are actually blocked.
func TestMergeGuardrailReports_KeepsBothRuleFamilies(t *testing.T) {
	cooldown := guardrail.Report{
		Intent:  "restart-apiary_raftd",
		Verdict: guardrail.Block,
		Findings: []guardrail.Finding{
			{Rule: "restart-lease-held", Detail: "another node holds an unconfirmed lease"},
		},
	}
	quorum := guardrail.Report{
		Intent:  "restart-apiary_raftd",
		Verdict: guardrail.Allow,
		Caveats: []invariant.Evidence{
			{Source: restartplan.RuleQuorumSafety, Detail: "quorum is fine"},
		},
	}

	got := mergeGuardrailReports(cooldown, quorum)

	if got.Verdict != guardrail.Block {
		t.Errorf("verdict = %q, want %q; a block from either check stands", got.Verdict, guardrail.Block)
	}
	if len(got.Findings) != 1 || got.Findings[0].Rule != "restart-lease-held" {
		t.Errorf("findings = %+v, want the lease finding preserved", got.Findings)
	}
	if len(got.Caveats) != 1 {
		t.Errorf("caveats = %+v, want the quorum caveat preserved", got.Caveats)
	}
}

// TestDescribeGuardrailBlock_ReportsEveryReason guards against a Block
// that rests on two rules being summarised down to one. An operator
// being asked to acknowledge a dangerous restart must see all of it.
func TestDescribeGuardrailBlock_ReportsEveryReason(t *testing.T) {
	report := guardrail.Report{
		Verdict: guardrail.Block,
		Findings: []guardrail.Finding{
			{Rule: restartplan.RuleQuorumSafety, Detail: "quorum would be lost"},
			{Rule: restartplan.RuleLeaderRestart, Detail: "this node is the leader"},
		},
	}
	msg := describeGuardrailBlock(report)
	if !contains(msg, "quorum would be lost") || !contains(msg, "this node is the leader") {
		t.Errorf("message = %q, want both findings present", msg)
	}

	// A block with no explanation is a bug in whatever produced it, and
	// must not be smoothed over into a generic sentence that reads like
	// a real reason. The test is that it admits the absence, not that it
	// avoids any particular word.
	bare := describeGuardrailBlock(guardrail.Report{Verdict: guardrail.Block})
	if !contains(bare, "no finding to explain it") {
		t.Errorf("an unexplained block rendered as %q; it should say it had no finding to give", bare)
	}
}

func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) && (haystack == needle || indexOf(haystack, needle) >= 0)
}

func indexOf(haystack, needle string) int {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return i
		}
	}
	return -1
}

// TestPreflightRestartNodeService_RaftdIsGuardedEndToEnd walks the real
// RPC against a real raft, because the unit tests above would all still
// pass if the preflight simply forgot to call the new evaluation. This
// is the test that proves the wiring, not the arithmetic.
func TestPreflightRestartNodeService_RaftdIsGuardedEndToEnd(t *testing.T) {
	socket, _ := newQuorumTestRaftd(t)
	client, _ := newManagerdRPCClientAndServer(t, socket, "raftd-1")

	ctx := context.Background()

	resp, err := client.PreflightRestartNodeService(ctx, preflightReq(raftdServiceName, false))
	if err != nil {
		t.Fatalf("PreflightRestartNodeService() error: %v", err)
	}
	if resp.GetVerdict() != string(guardrail.Block) {
		t.Errorf("raftd preflight verdict = %q, want block; a sole-voter leader restart must be refused", resp.GetVerdict())
	}

	// frontend carries no quorum stake and is still plainly allowed -
	// the guardrail must not have become a blanket refusal.
	fresp, err := client.PreflightRestartNodeService(ctx, preflightReq("apiary_frontend", false))
	if err != nil {
		t.Fatalf("PreflightRestartNodeService(frontend) error: %v", err)
	}
	if fresp.GetVerdict() != string(guardrail.Allow) {
		t.Errorf("frontend preflight verdict = %q, want allow", fresp.GetVerdict())
	}
}

// TestPreflightRestartNodeService_ForceIsThreadedThrough is the reason
// the Force field was added to the request. Without it a preflight
// always reports the un-forced verdict, so an operator who has already
// ticked "force" sees a Block that their actual restart would sail past
// - which reads as "force will not help" and is false.
func TestPreflightRestartNodeService_ForceIsThreadedThrough(t *testing.T) {
	socket, _ := newQuorumTestRaftd(t)
	client, _ := newManagerdRPCClientAndServer(t, socket, "raftd-1")
	ctx := context.Background()

	plain, err := client.PreflightRestartNodeService(ctx, preflightReq(raftdServiceName, false))
	if err != nil {
		t.Fatalf("unforced preflight error: %v", err)
	}
	forced, err := client.PreflightRestartNodeService(ctx, preflightReq(raftdServiceName, true))
	if err != nil {
		t.Fatalf("forced preflight error: %v", err)
	}

	if plain.GetVerdict() != string(guardrail.Block) {
		t.Fatalf("unforced verdict = %q, want block", plain.GetVerdict())
	}
	if forced.GetVerdict() != string(guardrail.Allow) {
		t.Errorf("forced verdict = %q, want allow; the preview must answer the question actually being asked", forced.GetVerdict())
	}
}

// TestPreflightRestartNodeService_ServiceThatCannotBeRestartedIsBlocked
// closes the preview/enforcement gap found in live verification.
//
// The preflight used to answer "allow" for anything outside the
// guardrail set, which included every service the inventory does not
// offer to restart and every name that is simply a typo. An operator
// previewing "sshd" was told the restart was safe; the real
// RestartNodeService then refused it with a bare error and no
// explanation of the disagreement. A preview that cannot be wrong is
// worth more than a permissive one.
func TestPreflightRestartNodeService_ServiceThatCannotBeRestartedIsBlocked(t *testing.T) {
	socket, _ := newQuorumTestRaftd(t)
	client, _ := newManagerdRPCClientAndServer(t, socket, "raftd-1")
	ctx := context.Background()

	for _, name := range []string{"sshd", "apiary_unknown", "apiary_raftdd", ""} {
		resp, err := client.PreflightRestartNodeService(ctx, preflightReq(name, false))
		if err != nil {
			t.Fatalf("PreflightRestartNodeService(%q) error: %v", name, err)
		}
		if resp.GetVerdict() != string(guardrail.Block) {
			t.Errorf("preflight(%q) verdict = %q, want block; a service that cannot be restarted is a definite refusal, not an unconstrained allow", name, resp.GetVerdict())
		}
		if resp.GetError() == "" {
			t.Errorf("preflight(%q) returned a block with no explanation", name)
		}
	}
}

// preflightReq builds a preflight request for name with force set, the
// two fields the raftd preflight path reads.
func preflightReq(name string, force bool) *rpcpb.PreflightRestartNodeServiceRequest {
	return &rpcpb.PreflightRestartNodeServiceRequest{Name: name, Force: force}
}
