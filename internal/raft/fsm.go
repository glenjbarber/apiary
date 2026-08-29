package raft

import (
	"fmt"
	"io"
	"sync"

	"github.com/hashicorp/raft"
	"google.golang.org/protobuf/proto"

	internalpb "github.com/glenjbarber/apiary/api/internalpb"
)

// FSMApplyResult is returned from Node.Apply, echoing back the FSM's
// response to an applied Command. Error is set (and VM left nil) if the
// command was rejected at the application level (e.g. duplicate/missing
// VM id) - this is separate from raft-level failures (not leader,
// timeout), which Node.Apply reports as a Go error instead.
type FSMApplyResult struct {
	Index uint64
	VM    *internalpb.VMDefinition
	Error string
}

// FSM applies typed Command messages (see api/internalpb/state.proto)
// against an in-memory map of VM definitions, keyed by ID. This is the
// real ephemeral-state schema: cluster membership itself is handled by
// raft's own configuration mechanism (AddVoter/RemoveServer), not by the
// FSM, so what's modeled here is VM definitions and their node ownership
// assignment.
type FSM struct {
	mu        sync.Mutex
	lastIndex uint64
	vms       map[string]*internalpb.VMDefinition
}

var _ raft.FSM = (*FSM)(nil)

// NewFSM returns an empty FSM.
func NewFSM() *FSM {
	return &FSM{vms: make(map[string]*internalpb.VMDefinition)}
}

// Apply implements raft.FSM. log.Data must be a marshaled
// api/internalpb.Command; a malformed payload is treated as an
// application-level error (FSMApplyResult.Error), not a panic, since a
// bad payload should never be able to crash the state machine.
func (f *FSM) Apply(log *raft.Log) interface{} {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.lastIndex = log.Index

	var cmd internalpb.Command
	if err := proto.Unmarshal(log.Data, &cmd); err != nil {
		return &FSMApplyResult{Index: log.Index, Error: fmt.Sprintf("invalid command encoding: %v", err)}
	}

	switch op := cmd.GetOp().(type) {
	case *internalpb.Command_CreateVm:
		return f.applyCreateVM(log.Index, op.CreateVm.GetVm())
	case *internalpb.Command_UpdateVm:
		return f.applyUpdateVM(log.Index, op.UpdateVm.GetVm())
	case *internalpb.Command_DeleteVm:
		return f.applyDeleteVM(log.Index, op.DeleteVm.GetId())
	default:
		return &FSMApplyResult{Index: log.Index, Error: "command has no op set"}
	}
}

func (f *FSM) applyCreateVM(index uint64, vm *internalpb.VMDefinition) *FSMApplyResult {
	if vm.GetId() == "" {
		return &FSMApplyResult{Index: index, Error: "CreateVM: id must be set"}
	}
	if _, exists := f.vms[vm.GetId()]; exists {
		return &FSMApplyResult{Index: index, Error: fmt.Sprintf("CreateVM: id %q already exists", vm.GetId())}
	}
	f.vms[vm.GetId()] = vm
	return &FSMApplyResult{Index: index, VM: vm}
}

func (f *FSM) applyUpdateVM(index uint64, vm *internalpb.VMDefinition) *FSMApplyResult {
	if _, exists := f.vms[vm.GetId()]; !exists {
		return &FSMApplyResult{Index: index, Error: fmt.Sprintf("UpdateVM: id %q does not exist", vm.GetId())}
	}
	f.vms[vm.GetId()] = vm
	return &FSMApplyResult{Index: index, VM: vm}
}

func (f *FSM) applyDeleteVM(index uint64, id string) *FSMApplyResult {
	vm, exists := f.vms[id]
	if !exists {
		return &FSMApplyResult{Index: index, Error: fmt.Sprintf("DeleteVM: id %q does not exist", id)}
	}
	delete(f.vms, id)
	return &FSMApplyResult{Index: index, VM: vm}
}

// AppliedIndex returns the index of the most recently applied log entry.
func (f *FSM) AppliedIndex() uint64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastIndex
}

// VM returns the current definition for id, and whether it exists.
func (f *FSM) VM(id string) (*internalpb.VMDefinition, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	vm, ok := f.vms[id]
	return vm, ok
}

// ListVMs returns every current VM definition.
func (f *FSM) ListVMs() []*internalpb.VMDefinition {
	f.mu.Lock()
	defer f.mu.Unlock()
	vms := make([]*internalpb.VMDefinition, 0, len(f.vms))
	for _, vm := range f.vms {
		vms = append(vms, vm)
	}
	return vms
}

// Snapshot implements raft.FSM.
func (f *FSM) Snapshot() (raft.FSMSnapshot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	state := &internalpb.FSMSnapshotState{
		LastIndex: f.lastIndex,
		Vms:       make(map[string]*internalpb.VMDefinition, len(f.vms)),
	}
	for id, vm := range f.vms {
		state.Vms[id] = vm
	}
	return &fsmSnapshot{state: state}, nil
}

// Restore implements raft.FSM.
func (f *FSM) Restore(rc io.ReadCloser) error {
	defer rc.Close()

	data, err := io.ReadAll(rc)
	if err != nil {
		return err
	}

	var state internalpb.FSMSnapshotState
	if err := proto.Unmarshal(data, &state); err != nil {
		return err
	}

	f.mu.Lock()
	f.lastIndex = state.GetLastIndex()
	f.vms = state.GetVms()
	if f.vms == nil {
		f.vms = make(map[string]*internalpb.VMDefinition)
	}
	f.mu.Unlock()
	return nil
}

// fsmSnapshot implements raft.FSMSnapshot by marshaling
// internalpb.FSMSnapshotState.
type fsmSnapshot struct {
	state *internalpb.FSMSnapshotState
}

var _ raft.FSMSnapshot = (*fsmSnapshot)(nil)

func (s *fsmSnapshot) Persist(sink raft.SnapshotSink) error {
	data, err := proto.Marshal(s.state)
	if err != nil {
		sink.Cancel()
		return err
	}
	if _, err := sink.Write(data); err != nil {
		sink.Cancel()
		return err
	}
	return sink.Close()
}

func (s *fsmSnapshot) Release() {}
