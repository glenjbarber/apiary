package manager

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	internalpb "github.com/glenjbarber/apiary/api/internalpb"
	rpcpb "github.com/glenjbarber/apiary/api/rpc"
	raftnode "github.com/glenjbarber/apiary/internal/raft"
)

// freeLoopbackAddr returns a loopback TCP address with a free port.
// Duplicated from internal/raft's test helper of the same name; not
// worth extracting a shared package for two small functions yet.
func freeLoopbackAddr(t *testing.T) string {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("finding free port: %v", err)
	}
	addr := lis.Addr().String()
	lis.Close()
	return addr
}

// eventually polls cond until it returns true or timeout elapses.
func eventually(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("condition not met within %s", timeout)
}

// newRaftdUDSSocket starts a real single-node raftd stack (raft.Node +
// gRPC server) on a temp Unix domain socket, and returns the socket path.
// Cleanup is registered on t.
func newRaftdUDSSocket(t *testing.T) string {
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

	// Short path in os.TempDir(), not t.TempDir(), to stay under the
	// Unix socket path length limit.
	socketDir, err := os.MkdirTemp("", "managerd-test-uds")
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

	return socketPath
}

// newManagerdRPCClient starts a real managerd Server (backed by a real
// RaftClient dialing raftdSocket) on a loopback TCP listener, and returns
// a real ManagerService client connected to it.
func newManagerdRPCClient(t *testing.T, raftdSocket string) rpcpb.ManagerServiceClient {
	t.Helper()

	raftClient, err := Dial(raftdSocket)
	if err != nil {
		t.Fatalf("Dial() error: %v", err)
	}
	t.Cleanup(func() { raftClient.Close() })

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen(tcp) error: %v", err)
	}

	grpcServer := grpc.NewServer()
	rpcpb.RegisterManagerServiceServer(grpcServer, NewServer(raftClient, "manager-1"))
	go grpcServer.Serve(lis)
	t.Cleanup(grpcServer.GracefulStop)

	conn, err := grpc.NewClient(
		lis.Addr().String(),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("grpc.NewClient() error: %v", err)
	}
	t.Cleanup(func() { conn.Close() })

	return rpcpb.NewManagerServiceClient(conn)
}

func TestIntegration_StatusRoundTripsThroughRaftd(t *testing.T) {
	raftdSocket := newRaftdUDSSocket(t)
	client := newManagerdRPCClient(t, raftdSocket)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	resp, err := client.Status(ctx, &rpcpb.StatusRequest{})
	if err != nil {
		t.Fatalf("Status() error: %v", err)
	}

	if resp.GetManagerNodeId() != "manager-1" {
		t.Errorf("ManagerNodeId = %q, want %q", resp.GetManagerNodeId(), "manager-1")
	}
	if !resp.GetRaftReachable() {
		t.Fatalf("RaftReachable = false, want true (raft_error=%q)", resp.GetRaftError())
	}
	if !resp.GetRaftIsLeader() {
		t.Errorf("RaftIsLeader = false, want true")
	}
	if resp.GetRaftNodeId() != "raftd-1" {
		t.Errorf("RaftNodeId = %q, want %q", resp.GetRaftNodeId(), "raftd-1")
	}
}

func TestIntegration_StatusReportsUnreachableRaftd(t *testing.T) {
	// Point at a socket path nothing is listening on.
	socketDir, err := os.MkdirTemp("", "managerd-test-dead-uds")
	if err != nil {
		t.Fatalf("MkdirTemp() error: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(socketDir) })
	deadSocket := filepath.Join(socketDir, "raftd.sock")

	client := newManagerdRPCClient(t, deadSocket)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	resp, err := client.Status(ctx, &rpcpb.StatusRequest{})
	if err != nil {
		t.Fatalf("Status() error: %v", err)
	}

	if resp.GetRaftReachable() {
		t.Fatalf("RaftReachable = true, want false against a dead raftd socket")
	}
	if resp.GetRaftError() == "" {
		t.Errorf("RaftError is empty, want a populated error message")
	}
}
