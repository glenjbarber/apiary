package raft

import (
	"errors"
	"net"
	"testing"
	"time"
)

// freeLoopbackAddr returns a loopback TCP address with a free port, for
// tests that need a concrete, distinct raft bind address.
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

// eventually polls cond until it returns true or timeout elapses, failing
// the test otherwise.
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

func TestNode_BootstrapSingleNode(t *testing.T) {
	cfg := Config{
		NodeID:   "node-1",
		DataDir:  t.TempDir(),
		BindAddr: freeLoopbackAddr(t),
	}

	node, err := New(cfg)
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}
	t.Cleanup(func() { node.Shutdown() })

	if err := node.Bootstrap(); err != nil {
		t.Fatalf("Bootstrap() error: %v", err)
	}

	eventually(t, 5*time.Second, func() bool {
		return node.Status().IsLeader
	})

	status := node.Status()
	if status.NodeID != "node-1" {
		t.Errorf("NodeID = %q, want %q", status.NodeID, "node-1")
	}
}

func TestNode_ApplyAndRestart(t *testing.T) {
	dataDir := t.TempDir()
	addr := freeLoopbackAddr(t)

	cfg := Config{
		NodeID:   "node-1",
		DataDir:  dataDir,
		BindAddr: addr,
	}

	hadState, err := HasExistingState(cfg)
	if err != nil {
		t.Fatalf("HasExistingState() error: %v", err)
	}
	if hadState {
		t.Fatalf("HasExistingState() = true on fresh data dir")
	}

	node, err := New(cfg)
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}
	if err := node.Bootstrap(); err != nil {
		t.Fatalf("Bootstrap() error: %v", err)
	}
	eventually(t, 5*time.Second, func() bool { return node.Status().IsLeader })

	result, err := node.Apply(mustMarshalCommand(t, createVMCmd("vm-1", "persist-me")), 5*time.Second)
	if err != nil {
		t.Fatalf("Apply() error: %v", err)
	}
	if result.Error != "" {
		t.Fatalf("Apply() result error = %q, want empty", result.Error)
	}
	if result.VM.GetName() != "persist-me" {
		t.Fatalf("Apply() result VM name = %q, want %q", result.VM.GetName(), "persist-me")
	}

	if err := node.Shutdown(); err != nil {
		t.Fatalf("Shutdown() error: %v", err)
	}

	// Reopen against the same data dir and a fresh port (the OS may not
	// have released the old one yet).
	cfg.BindAddr = freeLoopbackAddr(t)

	hadState, err = HasExistingState(cfg)
	if err != nil {
		t.Fatalf("HasExistingState() error after restart: %v", err)
	}
	if !hadState {
		t.Fatalf("HasExistingState() = false after a prior bootstrap+apply")
	}

	node2, err := New(cfg)
	if err != nil {
		t.Fatalf("New() error on restart: %v", err)
	}
	t.Cleanup(func() { node2.Shutdown() })

	// No Bootstrap() call: HasExistingState is true, so this restart must
	// not re-bootstrap and must recover the prior single-node cluster on
	// its own.
	eventually(t, 5*time.Second, func() bool { return node2.Status().IsLeader })

	// FSM replay happens on a separate goroutine from the raft state
	// machine, so it can briefly lag behind the leader-state flip above;
	// poll rather than asserting immediately.
	eventually(t, 5*time.Second, func() bool { return node2.Status().AppliedIndex > 0 })
}

func TestNode_GetVMAndListVMs(t *testing.T) {
	cfg := Config{
		NodeID:   "node-1",
		DataDir:  t.TempDir(),
		BindAddr: freeLoopbackAddr(t),
	}
	node, err := New(cfg)
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}
	t.Cleanup(func() { node.Shutdown() })

	// Before this node is leader, reads must be refused just like Apply.
	if _, _, err := node.GetVM("vm-1"); !errors.Is(err, ErrNotLeader) {
		t.Errorf("GetVM() before leadership: err = %v, want ErrNotLeader", err)
	}
	if _, err := node.ListVMs(); !errors.Is(err, ErrNotLeader) {
		t.Errorf("ListVMs() before leadership: err = %v, want ErrNotLeader", err)
	}

	if err := node.Bootstrap(); err != nil {
		t.Fatalf("Bootstrap() error: %v", err)
	}
	eventually(t, 5*time.Second, func() bool { return node.Status().IsLeader })

	if _, found, err := node.GetVM("vm-1"); err != nil || found {
		t.Errorf("GetVM(vm-1) before create = (found=%v, err=%v), want (false, nil)", found, err)
	}

	if _, err := node.Apply(mustMarshalCommand(t, createVMCmd("vm-1", "web-1")), 5*time.Second); err != nil {
		t.Fatalf("Apply() error: %v", err)
	}

	vm, found, err := node.GetVM("vm-1")
	if err != nil {
		t.Fatalf("GetVM() error: %v", err)
	}
	if !found || vm.GetName() != "web-1" {
		t.Errorf("GetVM(vm-1) = (%+v, %v), want web-1 present", vm, found)
	}

	vms, err := node.ListVMs()
	if err != nil {
		t.Fatalf("ListVMs() error: %v", err)
	}
	if len(vms) != 1 || vms[0].GetName() != "web-1" {
		t.Errorf("ListVMs() = %v, want [web-1]", vms)
	}
}
