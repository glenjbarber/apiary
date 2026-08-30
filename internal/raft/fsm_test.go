package raft

import (
	"bytes"
	"fmt"
	"io"
	"testing"

	"github.com/hashicorp/raft"
	"google.golang.org/protobuf/proto"

	internalpb "github.com/glenjbarber/apiary/api/internalpb"
)

// fakeSnapshotSink is a minimal in-memory raft.SnapshotSink for testing
// FSMSnapshot.Persist without a real raft.FileSnapshotStore.
type fakeSnapshotSink struct {
	bytes.Buffer
}

func (s *fakeSnapshotSink) ID() string    { return "test" }
func (s *fakeSnapshotSink) Cancel() error { return nil }
func (s *fakeSnapshotSink) Close() error  { return nil }

// mustMarshalCommand marshals cmd, failing the test on error.
func mustMarshalCommand(t *testing.T, cmd *internalpb.Command) []byte {
	t.Helper()
	data, err := proto.Marshal(cmd)
	if err != nil {
		t.Fatalf("marshaling command: %v", err)
	}
	return data
}

func createVMCmd(id, name string) *internalpb.Command {
	return &internalpb.Command{
		Op: &internalpb.Command_CreateVm{
			CreateVm: &internalpb.CreateVM{Vm: &internalpb.VMDefinition{Id: id, Name: name}},
		},
	}
}

func TestFSM_Apply_CreateVM(t *testing.T) {
	fsm := NewFSM()

	result := fsm.Apply(&raft.Log{Index: 1, Data: mustMarshalCommand(t, createVMCmd("vm-1", "web-1"))})

	applyResult, ok := result.(*FSMApplyResult)
	if !ok {
		t.Fatalf("Apply returned %T, want *FSMApplyResult", result)
	}
	if applyResult.Error != "" {
		t.Fatalf("Error = %q, want empty", applyResult.Error)
	}
	if applyResult.VM.GetId() != "vm-1" || applyResult.VM.GetName() != "web-1" {
		t.Errorf("VM = %+v, want id=vm-1 name=web-1", applyResult.VM)
	}
	if got := fsm.AppliedIndex(); got != 1 {
		t.Errorf("AppliedIndex() = %d, want 1", got)
	}

	vm, ok := fsm.VM("vm-1")
	if !ok || vm.GetName() != "web-1" {
		t.Errorf("VM(vm-1) = (%+v, %v), want web-1 present", vm, ok)
	}
}

func TestFSM_Apply_CreateVMDuplicateRejected(t *testing.T) {
	fsm := NewFSM()
	fsm.Apply(&raft.Log{Index: 1, Data: mustMarshalCommand(t, createVMCmd("vm-1", "web-1"))})

	result := fsm.Apply(&raft.Log{Index: 2, Data: mustMarshalCommand(t, createVMCmd("vm-1", "web-2"))})

	applyResult := result.(*FSMApplyResult)
	if applyResult.Error == "" {
		t.Fatalf("Error = empty, want a duplicate-id rejection")
	}
	// The index still advances even though the command was rejected: the
	// raft-level commit succeeded, only the application-level command did
	// not.
	if got := fsm.AppliedIndex(); got != 2 {
		t.Errorf("AppliedIndex() = %d, want 2", got)
	}
}

func TestFSM_Apply_DeleteVM(t *testing.T) {
	fsm := NewFSM()
	fsm.Apply(&raft.Log{Index: 1, Data: mustMarshalCommand(t, createVMCmd("vm-1", "web-1"))})

	deleteCmd := &internalpb.Command{
		Op: &internalpb.Command_DeleteVm{DeleteVm: &internalpb.DeleteVM{Id: "vm-1"}},
	}
	result := fsm.Apply(&raft.Log{Index: 2, Data: mustMarshalCommand(t, deleteCmd)})

	applyResult := result.(*FSMApplyResult)
	if applyResult.Error != "" {
		t.Fatalf("Error = %q, want empty", applyResult.Error)
	}
	if _, ok := fsm.VM("vm-1"); ok {
		t.Errorf("VM(vm-1) still present after DeleteVM")
	}
}

func TestFSM_Apply_DeleteVM_AssignedVMIsSoftDeleted(t *testing.T) {
	fsm := NewFSM()
	fsm.Apply(&raft.Log{Index: 1, Data: mustMarshalCommand(t, &internalpb.Command{
		Op: &internalpb.Command_CreateVm{CreateVm: &internalpb.CreateVM{
			Vm: &internalpb.VMDefinition{Id: "vm-1", NodeId: "node-a"},
		}},
	})})

	deleteCmd := &internalpb.Command{
		Op: &internalpb.Command_DeleteVm{DeleteVm: &internalpb.DeleteVM{Id: "vm-1"}},
	}
	result := fsm.Apply(&raft.Log{Index: 2, Data: mustMarshalCommand(t, deleteCmd)})

	applyResult := result.(*FSMApplyResult)
	if applyResult.Error != "" {
		t.Fatalf("Error = %q, want empty", applyResult.Error)
	}
	vm, ok := fsm.VM("vm-1")
	if !ok {
		t.Fatalf("VM(vm-1) not found, want it to still exist as a tombstone")
	}
	if vm.GetDesiredState() != internalpb.VMState_VM_STATE_DELETING {
		t.Errorf("DesiredState = %v, want VM_STATE_DELETING", vm.GetDesiredState())
	}
}

func TestFSM_Apply_UpdateVMPhase(t *testing.T) {
	fsm := NewFSM()
	fsm.Apply(&raft.Log{Index: 1, Data: mustMarshalCommand(t, createVMCmd("vm-1", "web-1"))})

	cmd := &internalpb.Command{
		Op: &internalpb.Command_UpdateVmPhase{UpdateVmPhase: &internalpb.UpdateVMPhase{
			Id: "vm-1", Phase: internalpb.VMPhase_VM_PHASE_READY,
		}},
	}
	result := fsm.Apply(&raft.Log{Index: 2, Data: mustMarshalCommand(t, cmd)})

	applyResult := result.(*FSMApplyResult)
	if applyResult.Error != "" {
		t.Fatalf("Error = %q, want empty", applyResult.Error)
	}
	vm, _ := fsm.VM("vm-1")
	if vm.GetPhase() != internalpb.VMPhase_VM_PHASE_READY {
		t.Errorf("Phase = %v, want VM_PHASE_READY", vm.GetPhase())
	}
}

func TestFSM_Apply_UpdateVMPhase_MissingIDIsError(t *testing.T) {
	fsm := NewFSM()

	cmd := &internalpb.Command{
		Op: &internalpb.Command_UpdateVmPhase{UpdateVmPhase: &internalpb.UpdateVMPhase{
			Id: "vm-1", Phase: internalpb.VMPhase_VM_PHASE_READY,
		}},
	}
	result := fsm.Apply(&raft.Log{Index: 1, Data: mustMarshalCommand(t, cmd)})

	if result.(*FSMApplyResult).Error == "" {
		t.Fatalf("Error = empty, want a missing-id rejection")
	}
}

func TestFSM_Apply_PurgeVM(t *testing.T) {
	fsm := NewFSM()
	fsm.Apply(&raft.Log{Index: 1, Data: mustMarshalCommand(t, createVMCmd("vm-1", "web-1"))})

	cmd := &internalpb.Command{Op: &internalpb.Command_PurgeVm{PurgeVm: &internalpb.PurgeVM{Id: "vm-1"}}}
	result := fsm.Apply(&raft.Log{Index: 2, Data: mustMarshalCommand(t, cmd)})

	if result.(*FSMApplyResult).Error != "" {
		t.Fatalf("Error = %q, want empty", result.(*FSMApplyResult).Error)
	}
	if _, ok := fsm.VM("vm-1"); ok {
		t.Errorf("VM(vm-1) still present after PurgeVM")
	}
}

func TestFSM_Apply_PurgeVM_IdempotentWhenAlreadyGone(t *testing.T) {
	fsm := NewFSM()

	cmd := &internalpb.Command{Op: &internalpb.Command_PurgeVm{PurgeVm: &internalpb.PurgeVM{Id: "vm-1"}}}
	result := fsm.Apply(&raft.Log{Index: 1, Data: mustMarshalCommand(t, cmd)})

	if result.(*FSMApplyResult).Error != "" {
		t.Errorf("Error = %q, want empty (purging an already-gone id is not an error)", result.(*FSMApplyResult).Error)
	}
}

func TestFSM_Apply_InvalidPayload(t *testing.T) {
	fsm := NewFSM()

	result := fsm.Apply(&raft.Log{Index: 1, Data: []byte("not a valid protobuf command")})

	applyResult := result.(*FSMApplyResult)
	if applyResult.Error == "" {
		t.Fatalf("Error = empty, want a decoding error for a malformed payload")
	}
}

func TestFSM_SnapshotRestore(t *testing.T) {
	fsm := NewFSM()
	fsm.Apply(&raft.Log{Index: 5, Data: mustMarshalCommand(t, createVMCmd("vm-1", "web-1"))})
	fsm.Apply(&raft.Log{Index: 6, Data: mustMarshalCommand(t, createVMCmd("vm-2", "web-2"))})

	snap, err := fsm.Snapshot()
	if err != nil {
		t.Fatalf("Snapshot() error: %v", err)
	}

	sink := &fakeSnapshotSink{}
	if err := snap.Persist(sink); err != nil {
		t.Fatalf("Persist() error: %v", err)
	}

	restored := NewFSM()
	if err := restored.Restore(io.NopCloser(bytes.NewReader(sink.Bytes()))); err != nil {
		t.Fatalf("Restore() error: %v", err)
	}

	if got := restored.AppliedIndex(); got != 6 {
		t.Errorf("restored AppliedIndex() = %d, want 6", got)
	}
	if vm, ok := restored.VM("vm-1"); !ok || vm.GetName() != "web-1" {
		t.Errorf("restored VM(vm-1) = (%+v, %v), want web-1 present", vm, ok)
	}
	if vm, ok := restored.VM("vm-2"); !ok || vm.GetName() != "web-2" {
		t.Errorf("restored VM(vm-2) = (%+v, %v), want web-2 present", vm, ok)
	}
}

func TestFSM_ListVMs(t *testing.T) {
	fsm := NewFSM()

	if got := fsm.ListVMs(); len(got) != 0 {
		t.Errorf("ListVMs() on empty FSM = %v, want empty", got)
	}

	fsm.Apply(&raft.Log{Index: 1, Data: mustMarshalCommand(t, createVMCmd("vm-1", "web-1"))})
	fsm.Apply(&raft.Log{Index: 2, Data: mustMarshalCommand(t, createVMCmd("vm-2", "web-2"))})

	got := fsm.ListVMs()
	if len(got) != 2 {
		t.Fatalf("ListVMs() returned %d entries, want 2", len(got))
	}
	names := map[string]bool{got[0].GetName(): true, got[1].GetName(): true}
	if !names["web-1"] || !names["web-2"] {
		t.Errorf("ListVMs() names = %v, want web-1 and web-2", names)
	}
}

func createNetworkCmd(id, name, subnet string) *internalpb.Command {
	return &internalpb.Command{
		Op: &internalpb.Command_CreateNetwork{
			CreateNetwork: &internalpb.CreateNetwork{Network: &internalpb.NetworkDefinition{Id: id, Name: name, Subnet: subnet}},
		},
	}
}

func createVMOnNetworkCmd(id, networkID string) *internalpb.Command {
	return &internalpb.Command{
		Op: &internalpb.Command_CreateVm{
			CreateVm: &internalpb.CreateVM{Vm: &internalpb.VMDefinition{Id: id, NetworkId: networkID}},
		},
	}
}

func TestFSM_Apply_CreateNetwork(t *testing.T) {
	fsm := NewFSM()

	result := fsm.Apply(&raft.Log{Index: 1, Data: mustMarshalCommand(t, createNetworkCmd("net-1", "prod", "10.60.0.0/24"))})

	applyResult := result.(*FSMApplyResult)
	if applyResult.Error != "" {
		t.Fatalf("Error = %q, want empty", applyResult.Error)
	}
	if applyResult.Network.GetId() != "net-1" || applyResult.Network.GetSubnet() != "10.60.0.0/24" {
		t.Errorf("Network = %+v, want id=net-1 subnet=10.60.0.0/24", applyResult.Network)
	}

	network, ok := fsm.Network("net-1")
	if !ok || network.GetName() != "prod" {
		t.Errorf("Network(net-1) = (%+v, %v), want prod present", network, ok)
	}
}

func TestFSM_Apply_CreateNetworkDuplicateRejected(t *testing.T) {
	fsm := NewFSM()
	fsm.Apply(&raft.Log{Index: 1, Data: mustMarshalCommand(t, createNetworkCmd("net-1", "prod", "10.60.0.0/24"))})

	result := fsm.Apply(&raft.Log{Index: 2, Data: mustMarshalCommand(t, createNetworkCmd("net-1", "dup", "10.61.0.0/24"))})

	if result.(*FSMApplyResult).Error == "" {
		t.Fatalf("Error = empty, want a duplicate-id rejection")
	}
}

func TestFSM_Apply_CreateNetworkInvalidSubnetRejected(t *testing.T) {
	fsm := NewFSM()

	result := fsm.Apply(&raft.Log{Index: 1, Data: mustMarshalCommand(t, createNetworkCmd("net-1", "prod", "not-a-cidr"))})

	if result.(*FSMApplyResult).Error == "" {
		t.Fatalf("Error = empty, want an invalid-subnet rejection")
	}
}

func TestFSM_Apply_DeleteNetwork(t *testing.T) {
	fsm := NewFSM()
	fsm.Apply(&raft.Log{Index: 1, Data: mustMarshalCommand(t, createNetworkCmd("net-1", "prod", "10.60.0.0/24"))})

	deleteCmd := &internalpb.Command{Op: &internalpb.Command_DeleteNetwork{DeleteNetwork: &internalpb.DeleteNetwork{Id: "net-1"}}}
	result := fsm.Apply(&raft.Log{Index: 2, Data: mustMarshalCommand(t, deleteCmd)})

	if result.(*FSMApplyResult).Error != "" {
		t.Fatalf("Error = %q, want empty", result.(*FSMApplyResult).Error)
	}
	if _, ok := fsm.Network("net-1"); ok {
		t.Errorf("Network(net-1) still present after DeleteNetwork")
	}
}

func TestFSM_Apply_DeleteNetworkMissingIsError(t *testing.T) {
	fsm := NewFSM()

	deleteCmd := &internalpb.Command{Op: &internalpb.Command_DeleteNetwork{DeleteNetwork: &internalpb.DeleteNetwork{Id: "missing"}}}
	result := fsm.Apply(&raft.Log{Index: 1, Data: mustMarshalCommand(t, deleteCmd)})

	if result.(*FSMApplyResult).Error == "" {
		t.Fatalf("Error = empty, want a not-found rejection")
	}
}

func TestFSM_Apply_DeleteNetworkStillReferencedIsRejected(t *testing.T) {
	fsm := NewFSM()
	fsm.Apply(&raft.Log{Index: 1, Data: mustMarshalCommand(t, createNetworkCmd("net-1", "prod", "10.60.0.0/24"))})
	fsm.Apply(&raft.Log{Index: 2, Data: mustMarshalCommand(t, createVMOnNetworkCmd("vm-1", "net-1"))})

	deleteCmd := &internalpb.Command{Op: &internalpb.Command_DeleteNetwork{DeleteNetwork: &internalpb.DeleteNetwork{Id: "net-1"}}}
	result := fsm.Apply(&raft.Log{Index: 3, Data: mustMarshalCommand(t, deleteCmd)})

	if result.(*FSMApplyResult).Error == "" {
		t.Fatalf("Error = empty, want a still-referenced rejection")
	}
	if _, ok := fsm.Network("net-1"); !ok {
		t.Errorf("Network(net-1) removed despite the rejected delete")
	}
}

func TestFSM_Apply_CreateVMOnNetworkAssignsIPAndMAC(t *testing.T) {
	fsm := NewFSM()
	fsm.Apply(&raft.Log{Index: 1, Data: mustMarshalCommand(t, createNetworkCmd("net-1", "prod", "10.60.0.0/24"))})

	result := fsm.Apply(&raft.Log{Index: 2, Data: mustMarshalCommand(t, createVMOnNetworkCmd("vm-1", "net-1"))})

	applyResult := result.(*FSMApplyResult)
	if applyResult.Error != "" {
		t.Fatalf("Error = %q, want empty", applyResult.Error)
	}
	if applyResult.VM.GetIpAddress() != "10.60.0.2" {
		t.Errorf("IpAddress = %q, want 10.60.0.2 (skipping .0 network, .1 gateway)", applyResult.VM.GetIpAddress())
	}
	if applyResult.VM.GetMacAddress() == "" {
		t.Errorf("MacAddress = empty, want a derived address")
	}
}

func TestFSM_Apply_CreateVMOnNetworkIsDeterministic(t *testing.T) {
	fsm := NewFSM()
	fsm.Apply(&raft.Log{Index: 1, Data: mustMarshalCommand(t, createNetworkCmd("net-1", "prod", "10.60.0.0/24"))})
	r1 := fsm.Apply(&raft.Log{Index: 2, Data: mustMarshalCommand(t, createVMOnNetworkCmd("vm-1", "net-1"))}).(*FSMApplyResult)

	fsm2 := NewFSM()
	fsm2.Apply(&raft.Log{Index: 1, Data: mustMarshalCommand(t, createNetworkCmd("net-1", "prod", "10.60.0.0/24"))})
	r2 := fsm2.Apply(&raft.Log{Index: 2, Data: mustMarshalCommand(t, createVMOnNetworkCmd("vm-1", "net-1"))}).(*FSMApplyResult)

	if r1.VM.GetIpAddress() != r2.VM.GetIpAddress() || r1.VM.GetMacAddress() != r2.VM.GetMacAddress() {
		t.Errorf("two independent FSMs assigned different values for the same commands: %+v vs %+v - not safe under raft replication", r1.VM, r2.VM)
	}
}

func TestFSM_Apply_CreateVMOnNetworkSkipsUsedIPs(t *testing.T) {
	fsm := NewFSM()
	fsm.Apply(&raft.Log{Index: 1, Data: mustMarshalCommand(t, createNetworkCmd("net-1", "prod", "10.60.0.0/30"))})
	// A /30 has exactly one usable non-gateway address (10.60.0.2) after
	// skipping .0 (network), .1 (gateway), and .3 (broadcast).
	first := fsm.Apply(&raft.Log{Index: 2, Data: mustMarshalCommand(t, createVMOnNetworkCmd("vm-1", "net-1"))}).(*FSMApplyResult)
	if first.Error != "" {
		t.Fatalf("first CreateVM Error = %q, want empty", first.Error)
	}

	second := fsm.Apply(&raft.Log{Index: 3, Data: mustMarshalCommand(t, createVMOnNetworkCmd("vm-2", "net-1"))})
	if second.(*FSMApplyResult).Error == "" {
		t.Fatalf("second CreateVM Error = empty, want an exhausted-network rejection")
	}
}

func TestFSM_Apply_CreateVMUnknownNetworkRejected(t *testing.T) {
	fsm := NewFSM()

	result := fsm.Apply(&raft.Log{Index: 1, Data: mustMarshalCommand(t, createVMOnNetworkCmd("vm-1", "missing-network"))})

	if result.(*FSMApplyResult).Error == "" {
		t.Fatalf("Error = empty, want an unknown-network rejection")
	}
}

func TestDeriveMAC_IsStableAndLocallyAdministered(t *testing.T) {
	a := deriveMAC("vm-1")
	b := deriveMAC("vm-1")
	c := deriveMAC("vm-2")

	if a != b {
		t.Errorf("deriveMAC(vm-1) = %q then %q, want stable across calls", a, b)
	}
	if a == c {
		t.Errorf("deriveMAC(vm-1) == deriveMAC(vm-2) = %q, want different ids to differ", a)
	}

	var firstOctet int
	if _, err := fmt.Sscanf(a[:2], "%x", &firstOctet); err != nil {
		t.Fatalf("parsing first octet of %q: %v", a, err)
	}
	if firstOctet&0x01 != 0 {
		t.Errorf("deriveMAC(vm-1) = %q, first octet has the multicast bit set", a)
	}
	if firstOctet&0x02 == 0 {
		t.Errorf("deriveMAC(vm-1) = %q, first octet is missing the locally-administered bit", a)
	}
}

func TestFSM_ListNetworks_SortedByID(t *testing.T) {
	fsm := NewFSM()
	fsm.Apply(&raft.Log{Index: 1, Data: mustMarshalCommand(t, createNetworkCmd("net-b", "b", "10.61.0.0/24"))})
	fsm.Apply(&raft.Log{Index: 2, Data: mustMarshalCommand(t, createNetworkCmd("net-a", "a", "10.60.0.0/24"))})

	got := fsm.ListNetworks()
	if len(got) != 2 || got[0].GetId() != "net-a" || got[1].GetId() != "net-b" {
		t.Errorf("ListNetworks() = %v, want [net-a, net-b] sorted", got)
	}
}

func TestFSM_SnapshotRestore_Networks(t *testing.T) {
	fsm := NewFSM()
	fsm.Apply(&raft.Log{Index: 1, Data: mustMarshalCommand(t, createNetworkCmd("net-1", "prod", "10.60.0.0/24"))})

	snap, err := fsm.Snapshot()
	if err != nil {
		t.Fatalf("Snapshot() error: %v", err)
	}
	sink := &fakeSnapshotSink{}
	if err := snap.(*fsmSnapshot).Persist(sink); err != nil {
		t.Fatalf("Persist() error: %v", err)
	}

	restored := NewFSM()
	if err := restored.Restore(io.NopCloser(bytes.NewReader(sink.Bytes()))); err != nil {
		t.Fatalf("Restore() error: %v", err)
	}

	network, ok := restored.Network("net-1")
	if !ok || network.GetSubnet() != "10.60.0.0/24" {
		t.Errorf("restored Network(net-1) = (%+v, %v), want present with subnet 10.60.0.0/24", network, ok)
	}
}
