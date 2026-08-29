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

func TestIntegration_CreateUpdateDeleteVM(t *testing.T) {
	raftdSocket := newRaftdUDSSocket(t)
	client := newManagerdRPCClient(t, raftdSocket)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	createResp, err := client.CreateVM(ctx, &rpcpb.CreateVMRequest{
		Vm: &rpcpb.VMDefinition{Id: "vm-1", Name: "web-1", Vcpus: 2, MemoryMb: 1024},
	})
	if err != nil {
		t.Fatalf("CreateVM() error: %v", err)
	}
	if createResp.GetError() != "" {
		t.Fatalf("CreateVM() returned error: %s", createResp.GetError())
	}
	if createResp.GetVm().GetName() != "web-1" {
		t.Errorf("CreateVM() vm.Name = %q, want %q", createResp.GetVm().GetName(), "web-1")
	}

	// Creating the same id again must be rejected.
	dupResp, err := client.CreateVM(ctx, &rpcpb.CreateVMRequest{
		Vm: &rpcpb.VMDefinition{Id: "vm-1", Name: "web-1-dup"},
	})
	if err != nil {
		t.Fatalf("CreateVM() (dup) error: %v", err)
	}
	if dupResp.GetError() == "" {
		t.Fatalf("CreateVM() (dup) error = empty, want a duplicate-id rejection")
	}

	updateResp, err := client.UpdateVM(ctx, &rpcpb.UpdateVMRequest{
		Vm: &rpcpb.VMDefinition{Id: "vm-1", Name: "web-1-renamed", Vcpus: 4, MemoryMb: 2048, DesiredState: rpcpb.VMState_VM_STATE_RUNNING},
	})
	if err != nil {
		t.Fatalf("UpdateVM() error: %v", err)
	}
	if updateResp.GetError() != "" {
		t.Fatalf("UpdateVM() returned error: %s", updateResp.GetError())
	}
	if updateResp.GetVm().GetName() != "web-1-renamed" || updateResp.GetVm().GetVcpus() != 4 {
		t.Errorf("UpdateVM() vm = %+v, want name=web-1-renamed vcpus=4", updateResp.GetVm())
	}
	if updateResp.GetVm().GetDesiredState() != rpcpb.VMState_VM_STATE_RUNNING {
		t.Errorf("UpdateVM() vm.DesiredState = %v, want RUNNING", updateResp.GetVm().GetDesiredState())
	}

	deleteResp, err := client.DeleteVM(ctx, &rpcpb.DeleteVMRequest{Id: "vm-1"})
	if err != nil {
		t.Fatalf("DeleteVM() error: %v", err)
	}
	if deleteResp.GetError() != "" {
		t.Fatalf("DeleteVM() returned error: %s", deleteResp.GetError())
	}
	if deleteResp.GetVm().GetName() != "web-1-renamed" {
		t.Errorf("DeleteVM() echoed vm.Name = %q, want the deleted definition's name", deleteResp.GetVm().GetName())
	}

	// Deleting again must fail: it no longer exists.
	redeleteResp, err := client.DeleteVM(ctx, &rpcpb.DeleteVMRequest{Id: "vm-1"})
	if err != nil {
		t.Fatalf("DeleteVM() (2nd) error: %v", err)
	}
	if redeleteResp.GetError() == "" {
		t.Fatalf("DeleteVM() (2nd) error = empty, want a missing-id rejection")
	}
}

func TestIntegration_GetVMAndListVMs(t *testing.T) {
	raftdSocket := newRaftdUDSSocket(t)
	client := newManagerdRPCClient(t, raftdSocket)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	getBeforeResp, err := client.GetVM(ctx, &rpcpb.GetVMRequest{Id: "vm-1"})
	if err != nil {
		t.Fatalf("GetVM() (before create) error: %v", err)
	}
	if getBeforeResp.GetFound() {
		t.Fatalf("GetVM() (before create) Found = true, want false")
	}

	if _, err := client.CreateVM(ctx, &rpcpb.CreateVMRequest{
		Vm: &rpcpb.VMDefinition{Id: "vm-1", Name: "web-1", Vcpus: 2, MemoryMb: 1024},
	}); err != nil {
		t.Fatalf("CreateVM() error: %v", err)
	}
	if _, err := client.CreateVM(ctx, &rpcpb.CreateVMRequest{
		Vm: &rpcpb.VMDefinition{Id: "vm-2", Name: "web-2", Vcpus: 1, MemoryMb: 512},
	}); err != nil {
		t.Fatalf("CreateVM() (2nd) error: %v", err)
	}

	getResp, err := client.GetVM(ctx, &rpcpb.GetVMRequest{Id: "vm-1"})
	if err != nil {
		t.Fatalf("GetVM() error: %v", err)
	}
	if getResp.GetError() != "" {
		t.Fatalf("GetVM() returned error: %s", getResp.GetError())
	}
	if !getResp.GetFound() || getResp.GetVm().GetName() != "web-1" {
		t.Errorf("GetVM(vm-1) = (found=%v, vm=%+v), want found web-1", getResp.GetFound(), getResp.GetVm())
	}

	listResp, err := client.ListVMs(ctx, &rpcpb.ListVMsRequest{})
	if err != nil {
		t.Fatalf("ListVMs() error: %v", err)
	}
	if listResp.GetError() != "" {
		t.Fatalf("ListVMs() returned error: %s", listResp.GetError())
	}
	if len(listResp.GetVms()) != 2 {
		t.Fatalf("ListVMs() returned %d VMs, want 2", len(listResp.GetVms()))
	}
}
