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
	"google.golang.org/grpc/metadata"

	internalpb "github.com/glenjbarber/apiary/api/internalpb"
	rpcpb "github.com/glenjbarber/apiary/api/rpc"
	"github.com/glenjbarber/apiary/internal/isostore"
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
	return newManagerdRPCClientWithVNC(t, raftdSocket, "manager-1", nil)
}

// fakeVNCLookup is a fake VNCLookup for GetVMConsole tests, without any
// real bhyve.Manager/RunDir involved.
type fakeVNCLookup struct {
	ports map[string]int
	err   error
}

func (f *fakeVNCLookup) VNCPort(name string) (int, bool, error) {
	if f.err != nil {
		return 0, false, f.err
	}
	port, ok := f.ports[name]
	return port, ok, nil
}

// newManagerdRPCClientWithVNC is newManagerdRPCClient, but lets a test
// supply nodeID and vnc explicitly - needed for GetVMConsole, which
// checks a VM's node_id against the serving Server's own nodeID.
func newManagerdRPCClientWithVNC(t *testing.T, raftdSocket, nodeID string, vnc VNCLookup) rpcpb.ManagerServiceClient {
	t.Helper()
	return newManagerdRPCClientFull(t, raftdSocket, nodeID, vnc, nil)
}

// fakeVLANStatus is a fake VLANStatus for ListNetworks bridge-status
// tests, without any real vlan.Manager/ifconfig involved.
type fakeVLANStatus struct {
	// up maps a bridge name to its up/down state. A name absent from
	// this map is treated as not existing on this node yet.
	up  map[string]bool
	err error
}

func (f *fakeVLANStatus) InterfaceStatus(_ context.Context, name string) (exists, up bool, err error) {
	if f.err != nil {
		return false, false, f.err
	}
	up, exists = f.up[name]
	return exists, up, nil
}

// newManagerdRPCClientFull is newManagerdRPCClient, but lets a test
// supply nodeID, vnc, and vlanMgr explicitly.
func newManagerdRPCClientFull(t *testing.T, raftdSocket, nodeID string, vnc VNCLookup, vlanMgr VLANStatus) rpcpb.ManagerServiceClient {
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

	srv := NewServer(raftClient, nodeID, isostore.New(t.TempDir()), vnc, vlanMgr)
	// Wired unconditionally, mirroring cmd/managerd/main.go exactly - this
	// is a no-op for every pre-existing test here (none of them ever
	// create an API key, so checkAuth's "zero keys = open" branch always
	// applies) and is what the ADR-0023 auth tests below rely on.
	grpcServer := grpc.NewServer(
		grpc.UnaryInterceptor(srv.AuthUnaryInterceptor),
		grpc.StreamInterceptor(srv.AuthStreamInterceptor),
	)
	rpcpb.RegisterManagerServiceServer(grpcServer, srv)
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

func TestIntegration_GetVMConsole_RunningLocallyWithVNC(t *testing.T) {
	raftdSocket := newRaftdUDSSocket(t)
	vnc := &fakeVNCLookup{ports: map[string]int{"vm-1": 5901}}
	client := newManagerdRPCClientWithVNC(t, raftdSocket, "node-a", vnc)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if _, err := client.CreateVM(ctx, &rpcpb.CreateVMRequest{
		Vm: &rpcpb.VMDefinition{Id: "vm-1", Name: "web-1", NodeId: "node-a"},
	}); err != nil {
		t.Fatalf("CreateVM() error: %v", err)
	}

	resp, err := client.GetVMConsole(ctx, &rpcpb.GetVMConsoleRequest{Id: "vm-1"})
	if err != nil {
		t.Fatalf("GetVMConsole() error: %v", err)
	}
	if resp.GetError() != "" {
		t.Fatalf("GetVMConsole() returned error: %s", resp.GetError())
	}
	if !resp.GetAvailable() || resp.GetHost() != "127.0.0.1" || resp.GetPort() != 5901 {
		t.Errorf("GetVMConsole() = %+v, want available on 127.0.0.1:5901", resp)
	}
}

func TestIntegration_GetVMConsole_NotYetProvisioned(t *testing.T) {
	raftdSocket := newRaftdUDSSocket(t)
	vnc := &fakeVNCLookup{ports: map[string]int{}}
	client := newManagerdRPCClientWithVNC(t, raftdSocket, "node-a", vnc)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if _, err := client.CreateVM(ctx, &rpcpb.CreateVMRequest{
		Vm: &rpcpb.VMDefinition{Id: "vm-1", Name: "web-1", NodeId: "node-a"},
	}); err != nil {
		t.Fatalf("CreateVM() error: %v", err)
	}

	resp, err := client.GetVMConsole(ctx, &rpcpb.GetVMConsoleRequest{Id: "vm-1"})
	if err != nil {
		t.Fatalf("GetVMConsole() error: %v", err)
	}
	if resp.GetError() != "" {
		t.Fatalf("GetVMConsole() returned error: %s", resp.GetError())
	}
	if resp.GetAvailable() {
		t.Errorf("GetVMConsole() = %+v, want Available=false (no VNC port recorded yet)", resp)
	}
}

func TestIntegration_GetVMConsole_WrongNodeReportsHint(t *testing.T) {
	raftdSocket := newRaftdUDSSocket(t)
	client := newManagerdRPCClientWithVNC(t, raftdSocket, "node-a", &fakeVNCLookup{})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if _, err := client.CreateVM(ctx, &rpcpb.CreateVMRequest{
		Vm: &rpcpb.VMDefinition{Id: "vm-1", Name: "web-1", NodeId: "node-b"},
	}); err != nil {
		t.Fatalf("CreateVM() error: %v", err)
	}

	resp, err := client.GetVMConsole(ctx, &rpcpb.GetVMConsoleRequest{Id: "vm-1"})
	if err != nil {
		t.Fatalf("GetVMConsole() error: %v", err)
	}
	if resp.GetError() == "" {
		t.Fatalf("GetVMConsole() returned no error, want a hint that vm-1 is on node-b, not node-a")
	}
}

func TestIntegration_GetVMConsole_NoVNCConfigured(t *testing.T) {
	raftdSocket := newRaftdUDSSocket(t)
	client := newManagerdRPCClientWithVNC(t, raftdSocket, "node-a", nil)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if _, err := client.CreateVM(ctx, &rpcpb.CreateVMRequest{
		Vm: &rpcpb.VMDefinition{Id: "vm-1", Name: "web-1", NodeId: "node-a"},
	}); err != nil {
		t.Fatalf("CreateVM() error: %v", err)
	}

	resp, err := client.GetVMConsole(ctx, &rpcpb.GetVMConsoleRequest{Id: "vm-1"})
	if err != nil {
		t.Fatalf("GetVMConsole() error: %v", err)
	}
	if resp.GetError() == "" {
		t.Fatalf("GetVMConsole() returned no error, want one reporting no VNC support on this node")
	}
}

func TestIntegration_GetVMConsole_UnknownVM(t *testing.T) {
	raftdSocket := newRaftdUDSSocket(t)
	client := newManagerdRPCClientWithVNC(t, raftdSocket, "node-a", &fakeVNCLookup{})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	resp, err := client.GetVMConsole(ctx, &rpcpb.GetVMConsoleRequest{Id: "does-not-exist"})
	if err != nil {
		t.Fatalf("GetVMConsole() error: %v", err)
	}
	if resp.GetError() == "" {
		t.Fatalf("GetVMConsole() returned no error, want a not-found error")
	}
}

func TestIntegration_CreateListDeleteNetwork(t *testing.T) {
	raftdSocket := newRaftdUDSSocket(t)
	client := newManagerdRPCClient(t, raftdSocket)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	createResp, err := client.CreateNetwork(ctx, &rpcpb.CreateNetworkRequest{
		Network: &rpcpb.NetworkDefinition{Id: "net-1", Name: "prod", VlanId: 100, Subnet: "10.60.0.0/24"},
	})
	if err != nil {
		t.Fatalf("CreateNetwork() error: %v", err)
	}
	if createResp.GetError() != "" {
		t.Fatalf("CreateNetwork() returned error: %s", createResp.GetError())
	}
	if createResp.GetNetwork().GetId() != "net-1" {
		t.Errorf("CreateNetwork() network = %+v, want id=net-1", createResp.GetNetwork())
	}

	listResp, err := client.ListNetworks(ctx, &rpcpb.ListNetworksRequest{})
	if err != nil {
		t.Fatalf("ListNetworks() error: %v", err)
	}
	if len(listResp.GetNetworks()) != 1 || listResp.GetNetworks()[0].GetId() != "net-1" {
		t.Fatalf("ListNetworks() = %+v, want one network net-1", listResp.GetNetworks())
	}

	deleteResp, err := client.DeleteNetwork(ctx, &rpcpb.DeleteNetworkRequest{Id: "net-1"})
	if err != nil {
		t.Fatalf("DeleteNetwork() error: %v", err)
	}
	if deleteResp.GetError() != "" {
		t.Fatalf("DeleteNetwork() returned error: %s", deleteResp.GetError())
	}

	listResp2, err := client.ListNetworks(ctx, &rpcpb.ListNetworksRequest{})
	if err != nil {
		t.Fatalf("ListNetworks() (after delete) error: %v", err)
	}
	if len(listResp2.GetNetworks()) != 0 {
		t.Errorf("ListNetworks() (after delete) = %+v, want none", listResp2.GetNetworks())
	}
}

func TestIntegration_DeleteNetworkStillReferencedByVMIsRejected(t *testing.T) {
	raftdSocket := newRaftdUDSSocket(t)
	client := newManagerdRPCClient(t, raftdSocket)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if _, err := client.CreateNetwork(ctx, &rpcpb.CreateNetworkRequest{
		Network: &rpcpb.NetworkDefinition{Id: "net-1", Name: "prod", Subnet: "10.60.0.0/24"},
	}); err != nil {
		t.Fatalf("CreateNetwork() error: %v", err)
	}
	if _, err := client.CreateVM(ctx, &rpcpb.CreateVMRequest{
		Vm: &rpcpb.VMDefinition{Id: "vm-1", NetworkId: "net-1"},
	}); err != nil {
		t.Fatalf("CreateVM() error: %v", err)
	}

	deleteResp, err := client.DeleteNetwork(ctx, &rpcpb.DeleteNetworkRequest{Id: "net-1"})
	if err != nil {
		t.Fatalf("DeleteNetwork() error: %v", err)
	}
	if deleteResp.GetError() == "" {
		t.Fatalf("DeleteNetwork() returned no error, want a still-referenced rejection")
	}
}

func TestIntegration_CreateVMOnNetworkGetsIPAndMACThroughFullStack(t *testing.T) {
	raftdSocket := newRaftdUDSSocket(t)
	client := newManagerdRPCClient(t, raftdSocket)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if _, err := client.CreateNetwork(ctx, &rpcpb.CreateNetworkRequest{
		Network: &rpcpb.NetworkDefinition{Id: "net-1", Name: "prod", Subnet: "10.60.0.0/24"},
	}); err != nil {
		t.Fatalf("CreateNetwork() error: %v", err)
	}

	createResp, err := client.CreateVM(ctx, &rpcpb.CreateVMRequest{
		Vm: &rpcpb.VMDefinition{Id: "vm-1", NetworkId: "net-1"},
	})
	if err != nil {
		t.Fatalf("CreateVM() error: %v", err)
	}
	if createResp.GetError() != "" {
		t.Fatalf("CreateVM() returned error: %s", createResp.GetError())
	}
	if createResp.GetVm().GetIpAddress() != "10.60.0.2" {
		t.Errorf("CreateVM() vm.IpAddress = %q, want 10.60.0.2", createResp.GetVm().GetIpAddress())
	}
	if createResp.GetVm().GetMacAddress() == "" {
		t.Errorf("CreateVM() vm.MacAddress = empty, want a derived address")
	}
}

func TestIntegration_ListNetworks_BridgeStatusUnknownWithoutVLAN(t *testing.T) {
	raftdSocket := newRaftdUDSSocket(t)
	client := newManagerdRPCClient(t, raftdSocket) // no VLANStatus configured

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if _, err := client.CreateNetwork(ctx, &rpcpb.CreateNetworkRequest{
		Network: &rpcpb.NetworkDefinition{Id: "net-1", Subnet: "10.60.0.0/24"},
	}); err != nil {
		t.Fatalf("CreateNetwork() error: %v", err)
	}

	listResp, err := client.ListNetworks(ctx, &rpcpb.ListNetworksRequest{})
	if err != nil {
		t.Fatalf("ListNetworks() error: %v", err)
	}
	if got := listResp.GetNetworks()[0].GetBridgeStatus(); got != "unknown" {
		t.Errorf("BridgeStatus = %q, want unknown (no VLAN support configured on this node)", got)
	}
}

func TestIntegration_ListNetworks_BridgeStatusUpOrDown(t *testing.T) {
	raftdSocket := newRaftdUDSSocket(t)
	bridge := resolveBridgeName(&internalpb.NetworkDefinition{Id: "net-1"})
	vlan := &fakeVLANStatus{up: map[string]bool{bridge: true}}
	client := newManagerdRPCClientFull(t, raftdSocket, "node-a", nil, vlan)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if _, err := client.CreateNetwork(ctx, &rpcpb.CreateNetworkRequest{
		Network: &rpcpb.NetworkDefinition{Id: "net-1", Subnet: "10.60.0.0/24"},
	}); err != nil {
		t.Fatalf("CreateNetwork() error: %v", err)
	}
	if _, err := client.CreateNetwork(ctx, &rpcpb.CreateNetworkRequest{
		Network: &rpcpb.NetworkDefinition{Id: "net-2", Subnet: "10.61.0.0/24"},
	}); err != nil {
		t.Fatalf("CreateNetwork() (net-2) error: %v", err)
	}

	listResp, err := client.ListNetworks(ctx, &rpcpb.ListNetworksRequest{})
	if err != nil {
		t.Fatalf("ListNetworks() error: %v", err)
	}
	statuses := map[string]string{}
	for _, n := range listResp.GetNetworks() {
		statuses[n.GetId()] = n.GetBridgeStatus()
	}
	if statuses["net-1"] != "up" {
		t.Errorf("net-1 BridgeStatus = %q, want up", statuses["net-1"])
	}
	if statuses["net-2"] != "unknown" {
		t.Errorf("net-2 BridgeStatus = %q, want unknown (bridge doesn't exist on this node)", statuses["net-2"])
	}
}

func TestIntegration_ZeroAPIKeys_EverythingIsUnauthenticated(t *testing.T) {
	raftdSocket := newRaftdUDSSocket(t)
	client := newManagerdRPCClient(t, raftdSocket)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// No Authorization metadata attached anywhere in this test - every
	// call should still succeed, since no API key exists yet.
	if _, err := client.ListVMs(ctx, &rpcpb.ListVMsRequest{}); err != nil {
		t.Errorf("ListVMs() error = %v, want nil with zero API keys", err)
	}
	if _, err := client.Status(ctx, &rpcpb.StatusRequest{}); err != nil {
		t.Errorf("Status() error = %v, want nil with zero API keys", err)
	}
}

func TestIntegration_CreateAPIKey_RawKeyWorksThenIsRejectedIfWrong(t *testing.T) {
	raftdSocket := newRaftdUDSSocket(t)
	client := newManagerdRPCClient(t, raftdSocket)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	createResp, err := client.CreateAPIKey(ctx, &rpcpb.CreateAPIKeyRequest{Name: "terraform"})
	if err != nil {
		t.Fatalf("CreateAPIKey() error: %v", err)
	}
	if createResp.GetError() != "" {
		t.Fatalf("CreateAPIKey() returned error: %s", createResp.GetError())
	}
	if createResp.GetRawKey() == "" {
		t.Fatalf("CreateAPIKey() RawKey is empty, want a real generated key")
	}
	if createResp.GetKey().GetName() != "terraform" {
		t.Errorf("CreateAPIKey() key.Name = %q, want terraform", createResp.GetKey().GetName())
	}

	// Now that a key exists, an unauthenticated call must be rejected...
	if _, err := client.ListVMs(ctx, &rpcpb.ListVMsRequest{}); err == nil {
		t.Fatalf("ListVMs() with no Authorization metadata = nil error, want Unauthenticated now that a key exists")
	}

	// ...a wrong key must be rejected...
	wrongCtx := metadata.NewOutgoingContext(ctx, metadata.Pairs("authorization", "Bearer apk_wrong"))
	if _, err := client.ListVMs(wrongCtx, &rpcpb.ListVMsRequest{}); err == nil {
		t.Fatalf("ListVMs() with a wrong key = nil error, want Unauthenticated")
	}

	// ...but the real, just-issued raw key must work.
	goodCtx := metadata.NewOutgoingContext(ctx, metadata.Pairs("authorization", "Bearer "+createResp.GetRawKey()))
	if _, err := client.ListVMs(goodCtx, &rpcpb.ListVMsRequest{}); err != nil {
		t.Errorf("ListVMs() with the real key error = %v, want nil", err)
	}
}

func TestIntegration_RevokeAPIKey_StopsWorkingImmediately(t *testing.T) {
	raftdSocket := newRaftdUDSSocket(t)
	client := newManagerdRPCClient(t, raftdSocket)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	createResp, err := client.CreateAPIKey(ctx, &rpcpb.CreateAPIKeyRequest{Name: "ci"})
	if err != nil {
		t.Fatalf("CreateAPIKey() error: %v", err)
	}
	authedCtx := metadata.NewOutgoingContext(ctx, metadata.Pairs("authorization", "Bearer "+createResp.GetRawKey()))

	if _, err := client.ListVMs(authedCtx, &rpcpb.ListVMsRequest{}); err != nil {
		t.Fatalf("ListVMs() before revoke error = %v, want nil", err)
	}

	// RevokeAPIKey itself needs a valid key too, once one exists.
	revokeResp, err := client.RevokeAPIKey(authedCtx, &rpcpb.RevokeAPIKeyRequest{Id: createResp.GetKey().GetId()})
	if err != nil {
		t.Fatalf("RevokeAPIKey() error: %v", err)
	}
	if revokeResp.GetError() != "" {
		t.Fatalf("RevokeAPIKey() returned error: %s", revokeResp.GetError())
	}

	if _, err := client.ListVMs(authedCtx, &rpcpb.ListVMsRequest{}); err == nil {
		t.Fatalf("ListVMs() with the now-revoked key = nil error, want Unauthenticated")
	}
}

func TestIntegration_ListAPIKeys_NeverReturnsKeyMaterial(t *testing.T) {
	raftdSocket := newRaftdUDSSocket(t)
	client := newManagerdRPCClient(t, raftdSocket)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	createResp, err := client.CreateAPIKey(ctx, &rpcpb.CreateAPIKeyRequest{Name: "terraform"})
	if err != nil {
		t.Fatalf("CreateAPIKey() error: %v", err)
	}
	authedCtx := metadata.NewOutgoingContext(ctx, metadata.Pairs("authorization", "Bearer "+createResp.GetRawKey()))

	listResp, err := client.ListAPIKeys(authedCtx, &rpcpb.ListAPIKeysRequest{})
	if err != nil {
		t.Fatalf("ListAPIKeys() error: %v", err)
	}
	if len(listResp.GetKeys()) != 1 || listResp.GetKeys()[0].GetName() != "terraform" {
		t.Fatalf("ListAPIKeys() = %+v, want one key named terraform", listResp.GetKeys())
	}
	// APIKeyInfo has no field for the raw key or its hash at all, but
	// double-check the actual raw key value doesn't show up anywhere in
	// the response by accident (e.g. via a future field added carelessly).
	for _, k := range listResp.GetKeys() {
		if k.String() == createResp.GetRawKey() {
			t.Fatalf("ListAPIKeys() leaked the raw key value")
		}
	}
}
