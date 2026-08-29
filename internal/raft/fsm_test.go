package raft

import (
	"bytes"
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
