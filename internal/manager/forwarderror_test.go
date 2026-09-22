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
func newJoinedFollowerRaftdSocket(t *testing.T, leaderClient rpcpb.ManagerServiceClient, nodeID string) string {
	t.Helper()

	addr := freeLoopbackAddr(t)
	cfg := raftnode.Config{NodeID: nodeID, DataDir: t.TempDir(), BindAddr: addr}
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
	reqResp, err := leaderClient.RequestJoinColony(ctx, &rpcpb.RequestJoinColonyRequest{NodeId: nodeID, RaftBindAddress: addr})
	if err != nil || reqResp.GetError() != "" {
		t.Fatalf("RequestJoinColony() = (%+v, %v)", reqResp, err)
	}
	approveResp, err := leaderClient.ApproveJoinRequest(ctx, &rpcpb.ApproveJoinRequestRequest{RequestId: reqResp.GetRequestId(), ConfirmPhrase: "yes-trust-new-comb"})
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
	leaderClient := newManagerdRPCClient(t, leaderSocket)

	followerSocket := newJoinedFollowerRaftdSocket(t, leaderClient, "follower-1")
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
