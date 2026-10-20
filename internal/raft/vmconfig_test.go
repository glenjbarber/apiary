package raft

import (
	"strings"
	"testing"

	"github.com/hashicorp/raft"
	"google.golang.org/protobuf/proto"

	internalpb "github.com/glenjbarber/apiary/api/internalpb"
)

// vmConfigFixture creates a VM carrying every field SetVMConfig must leave
// alone: identity, placement, disks, network addressing, ISO, firewall and
// Cloudflare settings, lifecycle state and phase.
func vmConfigFixture(t *testing.T, fsm *FSM) *internalpb.VMDefinition {
	t.Helper()
	res := fsm.Apply(&raft.Log{Index: 1, Data: mustMarshalCommand(t, &internalpb.Command{
		Op: &internalpb.Command_CreateVm{CreateVm: &internalpb.CreateVM{Vm: &internalpb.VMDefinition{
			Id:                 "vm-1",
			Name:               "web-1",
			Vcpus:              2,
			MemoryMb:           1024,
			NodeId:             "node-a",
			DesiredState:       internalpb.VMState_VM_STATE_RUNNING,
			Phase:              internalpb.VMPhase_VM_PHASE_READY,
			IsoName:            "installer.iso",
			BaseImageName:      "base.raw",
			DiskSizeMb:         20480,
			FirewallRules:      []*internalpb.FirewallRule{{Direction: "in", Action: "block", Priority: 1}},
			CloudflareHostname: "web.example.com",
			CloudflarePort:     8080,
		}}},
	})})
	if res.(*FSMApplyResult).Error != "" {
		t.Fatalf("CreateVM fixture: %s", res.(*FSMApplyResult).Error)
	}
	vm, ok := fsm.VM("vm-1")
	if !ok {
		t.Fatal("fixture VM missing after create")
	}
	return vm
}

func setVMConfig(t *testing.T, fsm *FSM, index uint64, id string, vcpus uint32, memoryMB uint64) string {
	t.Helper()
	res := fsm.Apply(&raft.Log{Index: index, Data: mustMarshalCommand(t, &internalpb.Command{
		Op: &internalpb.Command_SetVmConfig{SetVmConfig: &internalpb.SetVMConfig{Id: id, Vcpus: vcpus, MemoryMb: memoryMB}},
	})})
	return res.(*FSMApplyResult).Error
}

// TestFSM_Apply_SetVMConfig_PersistsOnlyComputeFields is the core
// atomicity and identity check: the new vCPU and RAM values are stored,
// and every other field of the record is byte-for-byte what it was.
func TestFSM_Apply_SetVMConfig_PersistsOnlyComputeFields(t *testing.T) {
	fsm := NewFSM()
	before := vmConfigFixture(t, fsm)

	if errMsg := setVMConfig(t, fsm, 2, "vm-1", 4, 2048); errMsg != "" {
		t.Fatalf("Error = %q, want empty", errMsg)
	}

	after, _ := fsm.VM("vm-1")
	if after.GetVcpus() != 4 || after.GetMemoryMb() != 2048 {
		t.Fatalf("vcpus/memory = %d/%d, want 4/2048", after.GetVcpus(), after.GetMemoryMb())
	}

	want := proto.Clone(before).(*internalpb.VMDefinition)
	want.Vcpus = 4
	want.MemoryMb = 2048
	if !proto.Equal(after, want) {
		t.Errorf("record changed beyond vcpus/memory_mb:\n got  %v\n want %v", after, want)
	}
}

// TestFSM_Apply_SetVMConfig_InvalidValuesRejectedAtomically checks that
// each refusal names the offending value and leaves the stored record
// completely unchanged - no partially applied edit.
func TestFSM_Apply_SetVMConfig_InvalidValuesRejectedAtomically(t *testing.T) {
	tests := []struct {
		name     string
		vcpus    uint32
		memoryMB uint64
		wantErr  string
	}{
		{name: "zero vcpus", vcpus: 0, memoryMB: 1024, wantErr: "vcpus 0 is outside the supported range 1-16"},
		{name: "too many vcpus", vcpus: 17, memoryMB: 1024, wantErr: "vcpus 17 is outside the supported range 1-16"},
		{name: "memory below floor", vcpus: 2, memoryMB: 64, wantErr: "memory_mb 64 is below the minimum of 128 MB"},
		{name: "zero memory", vcpus: 2, memoryMB: 0, wantErr: "memory_mb 0 is below the minimum of 128 MB"},
		{name: "valid vcpus but invalid memory", vcpus: 8, memoryMB: 1, wantErr: "memory_mb 1 is below"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fsm := NewFSM()
			before := vmConfigFixture(t, fsm)

			errMsg := setVMConfig(t, fsm, 2, "vm-1", tt.vcpus, tt.memoryMB)
			if !strings.Contains(errMsg, tt.wantErr) {
				t.Fatalf("Error = %q, want it to contain %q", errMsg, tt.wantErr)
			}
			after, _ := fsm.VM("vm-1")
			if !proto.Equal(after, before) {
				t.Errorf("rejected edit changed the record:\n got  %v\n want %v", after, before)
			}
		})
	}
}

func TestFSM_Apply_SetVMConfig_MissingVMIsError(t *testing.T) {
	fsm := NewFSM()
	errMsg := setVMConfig(t, fsm, 1, "no-such-vm", 2, 1024)
	if !strings.Contains(errMsg, `id "no-such-vm" does not exist`) {
		t.Fatalf("Error = %q, want a does-not-exist rejection", errMsg)
	}
	if _, ok := fsm.VM("no-such-vm"); ok {
		t.Error("SetVMConfig created a VM that did not exist")
	}
}

func TestFSM_Apply_SetVMConfig_DeletingVMIsRefused(t *testing.T) {
	fsm := NewFSM()
	vmConfigFixture(t, fsm)
	// Deletion of a VM with a node is a soft tombstone, so the record
	// survives in DELETING state for the owning node to tear down.
	fsm.Apply(&raft.Log{Index: 2, Data: mustMarshalCommand(t, &internalpb.Command{
		Op: &internalpb.Command_DeleteVm{DeleteVm: &internalpb.DeleteVM{Id: "vm-1"}},
	})})
	before, _ := fsm.VM("vm-1")

	errMsg := setVMConfig(t, fsm, 3, "vm-1", 4, 2048)
	if !strings.Contains(errMsg, "marked for deletion") {
		t.Fatalf("Error = %q, want a marked-for-deletion rejection", errMsg)
	}
	after, _ := fsm.VM("vm-1")
	if !proto.Equal(after, before) {
		t.Error("edit of a DELETING VM changed the record")
	}
}

// TestFSM_Apply_SetVMConfig_BoundaryValuesAccepted pins the inclusive
// ends of both ranges, so a future change to the constants is deliberate.
func TestFSM_Apply_SetVMConfig_BoundaryValuesAccepted(t *testing.T) {
	fsm := NewFSM()
	vmConfigFixture(t, fsm)

	if errMsg := setVMConfig(t, fsm, 2, "vm-1", VMConfigMinVCPUs, VMConfigMinMemoryMB); errMsg != "" {
		t.Fatalf("minimum values: Error = %q", errMsg)
	}
	if errMsg := setVMConfig(t, fsm, 3, "vm-1", VMConfigMaxVCPUs, VMConfigMinMemoryMB); errMsg != "" {
		t.Fatalf("maximum vcpus: Error = %q", errMsg)
	}
}
