package manager

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"

	internalpb "github.com/glenjbarber/apiary/api/internalpb"
	rpcpb "github.com/glenjbarber/apiary/api/rpc"
	"github.com/glenjbarber/apiary/internal/joinauth"
	raftnode "github.com/glenjbarber/apiary/internal/raft"
)

func TestAugmentForwardError(t *testing.T) {
	if got := augmentForwardError("raft: this node is not the leader", "10.90.0.94:17600", nil); got != "raft: this node is not the leader" {
		t.Errorf("augmentForwardError with nil ferr = %q, want baseErr unchanged", got)
	}

	ferr := errors.New(`tls: failed to verify certificate: x509: certificate is valid for 127.0.0.1, not 10.90.0.94`)
	got := augmentForwardError("raft: this node is not the leader", "10.90.0.94:17600", ferr)
	if !strings.Contains(got, "raft: this node is not the leader") {
		t.Errorf("augmentForwardError() = %q, want the original error preserved", got)
	}
	if !strings.Contains(got, "10.90.0.94:17600") {
		t.Errorf("augmentForwardError() = %q, want the leader hint included", got)
	}
	if !strings.Contains(got, "certificate is valid for 127.0.0.1") {
		t.Errorf("augmentForwardError() = %q, want the real forwarding failure reason included, not swallowed", got)
	}
}

// fakeFailingPeerForwarder is a PeerForwarder whose CreateJail always
// fails with a canned error, simulating exactly the live TLS-
// verification failure this test file's own integration test
// reproduces with a real second raft node.
type fakeFailingPeerForwarder struct {
	PeerForwarder
	err error
}

func (f *fakeFailingPeerForwarder) CreateJail(context.Context, string, *rpcpb.CreateJailRequest) (*rpcpb.CreateJailResponse, error) {
	return nil, f.err
}

// newJoinedFollowerRaftdSocket starts a second real raftd (raft.Node +
// gRPC server, mirroring newRaftdUDSSocket exactly) that is NOT
// bootstrapped, joins it to the Colony already reachable via
// leaderClient as a genuine second voter (using the same
// RequestJoinColony/ApproveJoinRequest flow a real join uses), and
// returns its own socket path once the join is confirmed. The returned
// node is guaranteed to be a follower, never the leader, since a
// 2-voter cluster's original leader keeps its term on an uncontested
// AddVoter.
func newJoinedFollowerRaftdSocket(t *testing.T, leaderSocket string, nodeID string) string {
	t.Helper()

	// This helper approves a join, so it builds the leader's managerd
	// itself rather than taking a client: ADR-0147 Part 3 requires an
	// operator authorization - an entry in a root-owned file, spent from
	// two Combs with two keys - before an approval gets anywhere, and
	// that needs the leader's *Server as well as its client.
	leaderClient, leaderSrv := newManagerdRPCClientAndServer(t, leaderSocket, "manager-1")

	addr := freeLoopbackAddr(t)
	cfg := raftnode.Config{NodeID: nodeID, DataDir: t.TempDir(), BindAddr: addr, FastElectionTiming: true}
	node, err := raftnode.New(cfg)
	if err != nil {
		t.Fatalf("raftnode.New() error: %v", err)
	}

	socketDir, err := os.MkdirTemp("", "managerd-test-uds-follower")
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

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	reqResp, err := leaderClient.RequestJoinColony(ctx, withIntroduction(&rpcpb.RequestJoinColonyRequest{NodeId: nodeID, RaftBindAddress: addr, JoinerLogStateObserved: true}, t))
	if err != nil || reqResp.GetError() != "" {
		t.Fatalf("RequestJoinColony() = (%+v, %v)", reqResp, err)
	}
	approveResp, _ := operatorAuthorizedApprovalForTest(t, leaderClient, leaderSrv, reqResp.GetRequestId())
	if err != nil || approveResp.GetError() != "" {
		t.Fatalf("ApproveJoinRequest() = (%+v, %v)", approveResp, err)
	}

	eventually(t, 5*time.Second, func() bool { return !node.Status().IsLeader && node.Status().LeaderID != "" })
	return socketPath
}

// TestIntegration_CreateJail_ForwardingFailureSurfacedInError is
// ADR-0106/SHARED.md's own regression test for the 2026-09-21 buzz/
// brood live incident: a follower with a correctly-known leader hint,
// whose forward to that leader fails (here simulated; live, it was a
// TLS hostname-verification failure), must surface the real forwarding
// failure - not a bare "raft: this node is not the leader" with no
// trace that a forward was even attempted. Uses two genuine raft
// nodes (a real leader, a real joined-and-synced follower) so the
// LeaderHint under test is exactly what a real cluster would report,
// not a stubbed value.
func TestIntegration_CreateJail_ForwardingFailureSurfacedInError(t *testing.T) {
	leaderSocket := newRaftdUDSSocket(t)
	leaderClient, _ := newManagerdRPCClientAndServer(t, leaderSocket, "manager-1")

	openColonyJoinWindowForTest(t, leaderClient)
	followerSocket := newJoinedFollowerRaftdSocket(t, leaderSocket, "follower-1")
	_, followerSrv := newManagerdRPCClientAndServer(t, followerSocket, "follower-1")

	simulatedErr := errors.New(`tls: failed to verify certificate: x509: certificate is valid for 127.0.0.1, not 10.90.0.94`)
	followerSrv.peers = &fakeFailingPeerForwarder{err: simulatedErr}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	resp, err := followerSrv.CreateJail(ctx, &rpcpb.CreateJailRequest{
		Jail: &rpcpb.JailDefinition{Id: "diag-test-jail", Name: "diag-test-jail"},
	})
	if err != nil {
		t.Fatalf("CreateJail() error: %v", err)
	}

	if resp.GetLeaderHint() == "" {
		t.Fatal("LeaderHint = empty, want the real leader's address - the forwarding attempt this test exercises never happens without one")
	}
	if !strings.Contains(resp.GetError(), "raft: this node is not the leader") {
		t.Errorf("Error = %q, want the original raft error preserved", resp.GetError())
	}
	if !strings.Contains(resp.GetError(), "certificate is valid for 127.0.0.1") {
		t.Errorf("Error = %q, want the real forwarding failure reason surfaced, not silently swallowed (this is the exact live 2026-09-21 buzz/brood bug)", resp.GetError())
	}
}

// fakeFailingApproveJoinPeerForwarder is the same device for
// ApproveJoinRequest: its ApproveJoinRequest always fails with a canned
// error, simulating the same live TLS-verification failure the tests
// above reproduce for CreateJail and RequestJoinColony.
type fakeFailingApproveJoinPeerForwarder struct {
	PeerForwarder
	err error
}

func (f *fakeFailingApproveJoinPeerForwarder) ApproveJoinRequest(context.Context, string, *rpcpb.ApproveJoinRequestRequest) (*rpcpb.ApproveJoinRequestResponse, error) {
	return nil, f.err
}

// TestIntegration_ApproveJoinRequest_ForwardingFailureSurfacedInError is
// issues #77/#81's own reproduction: ApproveJoinRequest's leadership
// check (ADR-0103) forwards a follower's call to the leader - but when
// that forward itself fails, the OLD code fell through to local
// processing anyway, which then failed at the NEXT gate for an unrelated
// reason (here: the request_id does not exist locally, since this
// follower never recorded one - the FSM read that failure comes from is
// a completely different, unrelated fact from "the forward to the
// leader failed"). The operator was shown that unrelated failure and
// never learned the forward itself was the real problem.
//
// Two genuine raft nodes, so the LeaderHint and the leadership check
// under test are exactly what a real Colony reports - mirroring this
// file's own CreateJail/RequestJoinColony regression tests for the same
// class of bug.
func TestIntegration_ApproveJoinRequest_ForwardingFailureSurfacedInError(t *testing.T) {
	leaderSocket := newRaftdUDSSocket(t)
	leaderClient, leaderSrv := newManagerdRPCClientAndServer(t, leaderSocket, "manager-1")
	openColonyJoinWindowForTest(t, leaderClient)

	// A genuine second raft voter, added directly through RaftClient
	// rather than through the full RequestJoinColony/ApproveJoinRequest
	// dance newJoinedFollowerRaftdSocket otherwise uses (that helper
	// asserts its OWN inner approval is "awaiting a second authorization",
	// which a single-voter Colony's own two-person-rule exemption makes
	// untrue right up until this AddVoter happens - the exact chicken-
	// and-egg window that exemption does not yet handle correctly, a
	// separate, pre-existing issue outside this test's own scope). This
	// test is about a SEPARATE, later ApproveJoinRequest call on the
	// resulting follower, not about the join that made it a voter, so a
	// direct AddVoter is the right shortcut here.
	followerNodeID := "follower-1"
	followerAddr := freeLoopbackAddr(t)
	followerCfg := raftnode.Config{NodeID: followerNodeID, DataDir: t.TempDir(), BindAddr: followerAddr, FastElectionTiming: true}
	followerNode, err := raftnode.New(followerCfg)
	if err != nil {
		t.Fatalf("raftnode.New() error: %v", err)
	}
	t.Cleanup(func() { followerNode.Shutdown() })

	followerSocketDir, err := os.MkdirTemp("", "managerd-test-uds-follower")
	if err != nil {
		t.Fatalf("MkdirTemp() error: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(followerSocketDir) })
	followerSocket := filepath.Join(followerSocketDir, "raftd.sock")
	followerLis, err := net.Listen("unix", followerSocket)
	if err != nil {
		t.Fatalf("Listen(unix) error: %v", err)
	}
	followerGRPC := grpc.NewServer()
	internalpb.RegisterRaftInternalServer(followerGRPC, raftnode.NewServer(followerNode))
	go followerGRPC.Serve(followerLis)
	t.Cleanup(func() { followerGRPC.GracefulStop(); followerNode.Shutdown() })

	ctxAdd, cancelAdd := context.WithTimeout(context.Background(), 5*time.Second)
	addResp, err := leaderSrv.raft.AddVoter(ctxAdd, followerNodeID, followerAddr, 0, 5*time.Second)
	cancelAdd()
	if err != nil || addResp.GetError() != "" {
		t.Fatalf("AddVoter(%s) = (%+v, %v)", followerNodeID, addResp, err)
	}
	eventually(t, 5*time.Second, func() bool { return !followerNode.Status().IsLeader && followerNode.Status().LeaderID != "" })

	_, followerSrv := newManagerdRPCClientAndServer(t, followerSocket, followerNodeID)

	// A REAL pending request, replicated to both nodes via the same raft
	// log - so requireLiveColonyJoinWindow's own local read (which runs
	// BEFORE the leadership check under test, and is correctly answered
	// identically by either node since it reads replicated state) does
	// not itself refuse "not found" for an unrelated reason. Created
	// through the LEADER's own client (RequestJoinColony on the follower
	// would itself need to forward, which is not what this test is
	// about) and read back on the follower to confirm it replicated.
	ctxSeed, cancelSeed := context.WithTimeout(context.Background(), 5*time.Second)
	reqResp, err := leaderClient.RequestJoinColony(ctxSeed, withIntroduction(&rpcpb.RequestJoinColonyRequest{
		NodeId: "joiner-x", RaftBindAddress: "127.0.0.1:1", JoinerLogStateObserved: true,
	}, t))
	cancelSeed()
	if err != nil || reqResp.GetError() != "" {
		t.Fatalf("RequestJoinColony() = (%+v, %v)", reqResp, err)
	}
	eventually(t, 5*time.Second, func() bool {
		wctx, wcancel := context.WithTimeout(context.Background(), time.Second)
		defer wcancel()
		got, gerr := followerSrv.raft.GetPendingJoinRequestLocal(wctx, reqResp.GetRequestId())
		return gerr == nil && got.GetRequest() != nil
	})
	// Stage one, completed on the leader (VerifyJoinIntroduction is
	// itself leader-forwarded, and this managerd IS the leader) - so the
	// request under test is genuinely at CODE_VERIFIED with a real
	// second PIN, the same state a request reaches right before a real
	// Approve. Without this, EVERY path (old code's fall-through
	// included) is refused earlier still, at the "not yet verified"
	// stage check, and this test would prove nothing about the forward
	// failure at all.
	secondPin := completeJoinHandshakeForTest(t, leaderSrv, reqResp.GetRequestId())
	eventually(t, 5*time.Second, func() bool {
		wctx, wcancel := context.WithTimeout(context.Background(), time.Second)
		defer wcancel()
		got, gerr := followerSrv.raft.GetPendingJoinRequestLocal(wctx, reqResp.GetRequestId())
		return gerr == nil && got.GetRequest().GetStage() == internalpb.JoinRequestStage_JOIN_REQUEST_STAGE_CODE_VERIFIED
	})

	simulatedErr := errors.New(`tls: failed to verify certificate: x509: certificate is valid for 127.0.0.1, not 10.90.0.94`)
	followerSrv.peers = &fakeFailingApproveJoinPeerForwarder{err: simulatedErr}

	// issue #85: a follower that never evaluates the authorization-file
	// gate also never NEEDS an entry in its own copy of the file, which
	// is the structural fix for entries lingering on a Comb that never
	// actually consumed one - an operator who creates the entry only on
	// the real leader (as #82's now-corrected refusal tells them to) has
	// nothing left to clean up anywhere else. Asserted directly: this
	// follower's own authorization store is never even OPENED on this
	// path, success or failure.
	authReadCalls := 0
	followerSrv.joinAuthorizationRead = func() (joinauth.Store, error) {
		authReadCalls++
		return joinauth.Store{}, nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// The OLD code's fall-through reached a LATER local gate next (e.g.
	// the authorization-entry read, or AddVoter's own "not the leader")
	// and reported THAT, a true fact about the wrong question that never
	// mentioned the forward failure at all. The FIXED code must never
	// get that far: a follower whose forward fails reports THAT failure
	// and stops, before reading anything else past the window check.
	resp, err := followerSrv.ApproveJoinRequest(ctx, &rpcpb.ApproveJoinRequestRequest{
		RequestId: reqResp.GetRequestId(), ConfirmPhrase: approveJoinRequestConfirmPhrase, SecondPin: secondPin,
	})
	if authReadCalls != 0 {
		t.Errorf("this follower's own authorization store was read %d time(s); a follower that forwards (or fails to) must never consult its own copy at all (issue #85)", authReadCalls)
	}
	if err != nil {
		t.Fatalf("ApproveJoinRequest() error: %v", err)
	}
	if strings.Contains(resp.GetError(), "not the leader") && !strings.Contains(resp.GetError(), "certificate is valid for 127.0.0.1") {
		t.Errorf("Error = %q, fell through to a local gate after the forward failed instead of reporting the forward failure and stopping", resp.GetError())
	}
	if !strings.Contains(resp.GetError(), "certificate is valid for 127.0.0.1") {
		t.Errorf("Error = %q, want the real forwarding failure reason surfaced, not silently swallowed by a fall-through to local processing", resp.GetError())
	}
}

// fakeFailingJoinPeerForwarder is the same device for the Colony join
// RPCs: its RequestJoinColony always fails with a canned error,
// simulating the same live TLS-verification failure the test below
// reproduces with a real second raft node.
type fakeFailingJoinPeerForwarder struct {
	PeerForwarder
	err error
}

func (f *fakeFailingJoinPeerForwarder) RequestJoinColony(context.Context, string, *rpcpb.RequestJoinColonyRequest) (*rpcpb.RequestJoinColonyResponse, error) {
	return nil, f.err
}

// TestIntegration_RequestJoinColony_ForwardingFailureSurfacedInError is
// the Colony-join counterpart of the CreateJail test above.
// RequestJoinColony never received the same treatment, so a joining
// Comb reaching a follower that could not forward to the leader was
// told only that this node is not the leader: the forwarding failure
// was discarded, and recovering the real reason needed live diagnosis
// on the node. Two genuine raft nodes again, so the LeaderHint under
// test is what a real Colony reports.
func TestIntegration_RequestJoinColony_ForwardingFailureSurfacedInError(t *testing.T) {
	leaderSocket := newRaftdUDSSocket(t)
	leaderClient, _ := newManagerdRPCClientAndServer(t, leaderSocket, "manager-1")

	// Opened before the follower joins, so the follower receives the
	// window with the log it replicates on join. RequestJoinColony
	// refuses a closed Colony before it ever reaches the forward, and
	// GetColonyJoinWindow is exempt from checkAuth, so this reaches
	// the follower's own local read.
	openColonyJoinWindowForTest(t, leaderClient)
	followerSocket := newJoinedFollowerRaftdSocket(t, leaderSocket, "follower-1")
	followerClient, followerSrv := newManagerdRPCClientAndServer(t, followerSocket, "follower-1")
	eventually(t, 5*time.Second, func() bool {
		wctx, wcancel := context.WithTimeout(context.Background(), time.Second)
		defer wcancel()
		w, werr := followerClient.GetColonyJoinWindow(wctx, &rpcpb.GetColonyJoinWindowRequest{})
		return werr == nil && w.GetError() == ""
	})

	simulatedErr := errors.New(`tls: failed to verify certificate: x509: certificate is valid for 127.0.0.1, not 10.90.0.94`)
	followerSrv.peers = &fakeFailingJoinPeerForwarder{err: simulatedErr}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// ADR-0147 Part 2 requires a first code and the advertised
	// fingerprints, and refuses the call before it ever reaches the raft
	// apply where the leadership hint comes from. Without them this test
	// was measuring the refusal, not the forwarding failure.
	resp, err := followerClient.RequestJoinColony(ctx, withIntroduction(&rpcpb.RequestJoinColonyRequest{
		NodeId:                 "joiner-2",
		RaftBindAddress:        freeLoopbackAddr(t),
		JoinerLogStateObserved: true,
	}, t))
	if err != nil {
		t.Fatalf("RequestJoinColony() error: %v", err)
	}

	if resp.GetLeaderHint() == "" {
		t.Fatal("LeaderHint = empty, want the real leader's address - the forwarding attempt this test exercises never happens without one")
	}
	if !strings.Contains(resp.GetError(), "not the leader") {
		t.Errorf("Error = %q, want the original raft error preserved", resp.GetError())
	}
	if !strings.Contains(resp.GetError(), "certificate is valid for 127.0.0.1") {
		t.Errorf("Error = %q, want the real forwarding failure reason surfaced, not silently swallowed", resp.GetError())
	}
}
