// Package cluster_test (external test package, not cluster's own
// internal package) - required so this file's internal/manager import
// doesn't create a cycle now that internal/manager's regular code
// imports internal/cluster (see ADR-0052's SimulateNodeFailure
// handler). An external test package is a separate compiled unit from
// cluster itself, so cluster_test -> manager -> cluster creates no
// cycle even though cluster's own package can never import manager.
package cluster_test

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"

	internalpb "github.com/glenjbarber/apiary/api/internalpb"
	"github.com/glenjbarber/apiary/internal/bhyve"
	"github.com/glenjbarber/apiary/internal/cluster"
	"github.com/glenjbarber/apiary/internal/manager"
	raftnode "github.com/glenjbarber/apiary/internal/raft"
	"github.com/glenjbarber/apiary/internal/zfs"
)

// This test exercises the full stack for real: a real raft.Node, a real
// RaftClient dialing it over a Unix domain socket, and a real zfs.Manager
// against an actual test pool - proving Reconciler provisions real ZFS
// datasets for VMs assigned to the local node. Requires a FreeBSD host
// with zfs(8) and a test pool available (see internal/zfs's own
// integration test doc comment); set APIARY_ZFS_TEST_POOL to override
// the default pool name.
func TestIntegration_ReconcilerProvisionsRealDataset(t *testing.T) {
	if _, err := exec.LookPath("zfs"); err != nil {
		t.Skip("zfs not available on this host; see internal/zfs's integration_test.go doc comment")
	}

	pool := os.Getenv("APIARY_ZFS_TEST_POOL")
	if pool == "" {
		pool = "apiarytest"
	}
	base := fmt.Sprintf("%s/it-cluster-%d", pool, time.Now().UnixNano())
	ctx := context.Background()
	if err := exec.CommandContext(ctx, "zfs", "create", "-p", base).Run(); err != nil {
		t.Fatalf("creating test base dataset %s: %v", base, err)
	}
	t.Cleanup(func() {
		exec.Command("zfs", "destroy", "-r", base).Run()
	})

	// Real raft.Node + raftd-side gRPC server on a temp UDS socket.
	nodeCfg := raftnode.Config{
		NodeID:   "node-a",
		DataDir:  t.TempDir(),
		BindAddr: freeLoopbackAddr(t),
	}
	node, err := raftnode.New(nodeCfg)
	if err != nil {
		t.Fatalf("raftnode.New() error: %v", err)
	}
	t.Cleanup(func() { node.Shutdown() })
	if err := node.Bootstrap(); err != nil {
		t.Fatalf("Bootstrap() error: %v", err)
	}
	eventually(t, 5*time.Second, func() bool { return node.Status().IsLeader })

	socketDir, err := os.MkdirTemp("", "cluster-test-uds")
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
	t.Cleanup(grpcServer.GracefulStop)

	raftClient, err := manager.Dial(socketPath, "")
	if err != nil {
		t.Fatalf("manager.Dial() error: %v", err)
	}
	t.Cleanup(func() { raftClient.Close() })

	// Seed two VM definitions: one assigned to this node, one to another.
	mustApplyCreateVM(t, node, "vm-mine", "node-a")
	mustApplyCreateVM(t, node, "vm-other", "node-b")

	zfsManager := zfs.New(base)
	r := &cluster.Reconciler{Raft: raftClient, ZFS: zfsManager, LocalNodeID: "node-a"}
	if err := r.RunOnce(ctx); err != nil {
		t.Fatalf("RunOnce() error: %v", err)
	}

	exists, err := zfsManager.DatasetExists(ctx, "vm-mine")
	if err != nil {
		t.Fatalf("DatasetExists(vm-mine) error: %v", err)
	}
	if !exists {
		t.Errorf("DatasetExists(vm-mine) = false, want true (Reconciler should have created it)")
	}

	exists, err = zfsManager.DatasetExists(ctx, "vm-other")
	if err != nil {
		t.Fatalf("DatasetExists(vm-other) error: %v", err)
	}
	if exists {
		t.Errorf("DatasetExists(vm-other) = true, want false (assigned to a different node)")
	}
}

// This test exercises the full stack including bhyve: a real raft.Node,
// a real zfs.Manager, and a real bhyve.Manager, proving Reconciler
// provisions a dataset, a disk image inside it, and a running bhyve VM -
// all from one RunOnce call. Requires a host with both zfs(8) and
// hardware-assisted virtualization (see internal/bhyve's own
// integration test doc comment for the exact requirements); skips
// cleanly if either is unavailable. Set APIARY_ZFS_TEST_POOL and
// APIARY_BHYVE_TEST_BOOTROM to override defaults, same as the
// respective packages' own tests.
func TestIntegration_ReconcilerProvisionsRealBhyveVM(t *testing.T) {
	if _, err := exec.LookPath("zfs"); err != nil {
		t.Skip("zfs not available on this host")
	}
	if _, err := exec.LookPath("bhyve"); err != nil {
		t.Skip("bhyve not available on this host")
	}
	if out, err := exec.Command("kldstat").CombinedOutput(); err != nil || !strings.Contains(string(out), "vmm.ko") {
		t.Skip("vmm(4) not loaded (kldload vmm)")
	}
	bootrom := os.Getenv("APIARY_BHYVE_TEST_BOOTROM")
	if bootrom == "" {
		bootrom = "/usr/local/share/uefi-firmware/BHYVE_UEFI.fd"
	}
	if _, err := os.Stat(bootrom); err != nil {
		t.Skipf("boot ROM %s not present", bootrom)
	}

	pool := os.Getenv("APIARY_ZFS_TEST_POOL")
	if pool == "" {
		pool = "apiarytest"
	}
	base := fmt.Sprintf("%s/it-cluster-bhyve-%d", pool, time.Now().UnixNano())
	ctx := context.Background()
	if err := exec.CommandContext(ctx, "zfs", "create", "-p", base).Run(); err != nil {
		t.Fatalf("creating test base dataset %s: %v", base, err)
	}
	t.Cleanup(func() {
		exec.Command("zfs", "destroy", "-r", base).Run()
	})

	nodeCfg := raftnode.Config{NodeID: "node-a", DataDir: t.TempDir(), BindAddr: freeLoopbackAddr(t)}
	node, err := raftnode.New(nodeCfg)
	if err != nil {
		t.Fatalf("raftnode.New() error: %v", err)
	}
	t.Cleanup(func() { node.Shutdown() })
	if err := node.Bootstrap(); err != nil {
		t.Fatalf("Bootstrap() error: %v", err)
	}
	eventually(t, 5*time.Second, func() bool { return node.Status().IsLeader })

	socketDir, err := os.MkdirTemp("", "cluster-test-bhyve-uds")
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
	t.Cleanup(grpcServer.GracefulStop)

	raftClient, err := manager.Dial(socketPath, "")
	if err != nil {
		t.Fatalf("manager.Dial() error: %v", err)
	}
	t.Cleanup(func() { raftClient.Close() })

	mustApplyCreateVMWithResources(t, node, "vm-real", "node-a", 2, 256)

	zfsManager := zfs.New(base)
	bhyvePrefix := fmt.Sprintf("apiary-it-%d-", time.Now().UnixNano())
	bhyveManager := bhyve.New(bhyvePrefix)
	bhyveManager.RunDir = t.TempDir()
	t.Cleanup(func() { bhyveManager.DestroyVM(context.Background(), "vm-real") })

	r := &cluster.Reconciler{
		Raft:        raftClient,
		ZFS:         zfsManager,
		Bhyve:       bhyveManager,
		LocalNodeID: "node-a",
		BootROM:     bootrom,
		DiskSizeMB:  64,
	}
	if err := r.RunOnce(ctx); err != nil {
		t.Fatalf("RunOnce() error: %v", err)
	}

	exists, err := zfsManager.DatasetExists(ctx, "vm-real")
	if err != nil {
		t.Fatalf("DatasetExists() error: %v", err)
	}
	if !exists {
		t.Fatalf("DatasetExists() = false, want true")
	}

	mountpoint, err := zfsManager.GetProperty(ctx, "vm-real", "mountpoint")
	if err != nil {
		t.Fatalf("GetProperty(mountpoint) error: %v", err)
	}
	if _, err := os.Stat(filepath.Join(mountpoint, "disk.img")); err != nil {
		t.Errorf("disk image not created at %s: %v", filepath.Join(mountpoint, "disk.img"), err)
	}

	var running bool
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		running, err = bhyveManager.VMExists(ctx, "vm-real")
		if err != nil {
			t.Fatalf("VMExists() error: %v", err)
		}
		if running {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !running {
		t.Fatalf("VMExists() = false after RunOnce, want the bhyve VM to be running")
	}
}

// mustApplyCreateVMWithResources is mustApplyCreateVM plus explicit
// vcpus/memory, for tests that need to verify those are threaded
// through to bhyve.
func mustApplyCreateVMWithResources(t *testing.T, node *raftnode.Node, id, nodeID string, vcpus uint32, memoryMB uint64) {
	t.Helper()
	cmd := &internalpb.Command{
		Op: &internalpb.Command_CreateVm{
			CreateVm: &internalpb.CreateVM{Vm: &internalpb.VMDefinition{
				Id: id, NodeId: nodeID, Vcpus: vcpus, MemoryMb: memoryMB,
			}},
		},
	}
	data, err := proto.Marshal(cmd)
	if err != nil {
		t.Fatalf("marshaling CreateVM command: %v", err)
	}
	if _, err := node.Apply(data, 5*time.Second); err != nil {
		t.Fatalf("Apply(CreateVM %s) error: %v", id, err)
	}
}

// mustApplyCreateVM applies a CreateVM command directly against node,
// bypassing managerd since this test only needs the raft/FSM/zfs path.
func mustApplyCreateVM(t *testing.T, node *raftnode.Node, id, nodeID string) {
	t.Helper()
	cmd := &internalpb.Command{
		Op: &internalpb.Command_CreateVm{
			CreateVm: &internalpb.CreateVM{Vm: &internalpb.VMDefinition{Id: id, NodeId: nodeID}},
		},
	}
	data, err := proto.Marshal(cmd)
	if err != nil {
		t.Fatalf("marshaling CreateVM command: %v", err)
	}
	if _, err := node.Apply(data, 5*time.Second); err != nil {
		t.Fatalf("Apply(CreateVM %s) error: %v", id, err)
	}
}

// freeLoopbackAddr and eventually mirror internal/raft's test helpers of
// the same name - duplicated here rather than exported from internal/raft
// purely for test use, matching the small-duplication call internal/manager
// already made for the same helpers.
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
