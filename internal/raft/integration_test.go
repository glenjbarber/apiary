package raft

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/proto"

	internalpb "github.com/glenjbarber/apiary/api/internalpb"
)

// TestIntegration_ApplyAndStatusOverUDS drives a real single-node raftd
// stack (Node + gRPC server) over an actual Unix domain socket, using a
// real generated gRPC client - the primary end-to-end verification for
// this slice's architecture (process split + UDS protocol), independent
// of grpcurl or any external tooling.
func TestIntegration_ApplyAndStatusOverUDS(t *testing.T) {
	cfg := Config{
		NodeID:   "node-1",
		DataDir:  t.TempDir(),
		BindAddr: freeLoopbackAddr(t),
	}

	node, err := New(cfg)
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}
	if err := node.Bootstrap(); err != nil {
		t.Fatalf("Bootstrap() error: %v", err)
	}
	eventually(t, 5*time.Second, func() bool { return node.Status().IsLeader })

	// Short path in os.TempDir() rather than t.TempDir(), to stay well
	// under the ~104-byte Unix socket path length limit.
	socketDir, err := os.MkdirTemp("", "raftd-uds")
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
	internalpb.RegisterRaftInternalServer(grpcServer, NewServer(node))
	go grpcServer.Serve(lis)
	t.Cleanup(func() {
		grpcServer.GracefulStop()
		node.Shutdown()
	})

	conn, err := grpc.NewClient(
		"unix://"+socketPath,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("grpc.NewClient() error: %v", err)
	}
	t.Cleanup(func() { conn.Close() })

	client := internalpb.NewRaftInternalClient(conn)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	statusResp, err := client.Status(ctx, &internalpb.StatusRequest{})
	if err != nil {
		t.Fatalf("Status() error: %v", err)
	}
	if !statusResp.GetIsLeader() {
		t.Fatalf("Status().IsLeader = false, want true")
	}

	applyResp, err := client.Apply(ctx, &internalpb.ApplyRequest{
		Payload: mustMarshalCommand(t, createVMCmd("vm-1", "over-the-wire")),
	})
	if err != nil {
		t.Fatalf("Apply() error: %v", err)
	}
	if applyResp.GetError() != "" {
		t.Fatalf("Apply() returned error: %s", applyResp.GetError())
	}
	var vm internalpb.VMDefinition
	if err := proto.Unmarshal(applyResp.GetResult(), &vm); err != nil {
		t.Fatalf("unmarshaling Apply() result: %v", err)
	}
	if vm.GetName() != "over-the-wire" {
		t.Fatalf("Apply() result VM name = %q, want %q", vm.GetName(), "over-the-wire")
	}

	statusResp, err = client.Status(ctx, &internalpb.StatusRequest{})
	if err != nil {
		t.Fatalf("Status() (2nd) error: %v", err)
	}
	if statusResp.GetAppliedIndex() == 0 {
		t.Fatalf("AppliedIndex = 0 after Apply, want > 0")
	}
}
