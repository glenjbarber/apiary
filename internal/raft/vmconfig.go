package raft

import (
	"fmt"

	"google.golang.org/protobuf/proto"

	internalpb "github.com/glenjbarber/apiary/api/internalpb"
)

// Limits for SetVMConfig (issue #111). They are enforced here, in the
// apply function, so every replica refuses the same values in the same
// order - see the rationale in colonydisksize.go, which applies
// identically to any value that must be agreed across the Colony.
const (
	// VMConfigMinVCPUs/VMConfigMaxVCPUs bound a guest's vCPU count. The
	// maximum is the per-guest vCPU ceiling of vmm(4) on amd64 (VM_MAXCPU);
	// bhyve refuses a larger -c at launch, so an edit above it could only
	// ever fail later, asynchronously, inside the reconciler.
	VMConfigMinVCPUs = 1
	VMConfigMaxVCPUs = 16

	// VMConfigMinMemoryMB is the smallest guest RAM accepted. Below this a
	// FreeBSD guest does not reliably finish booting, so the edit is
	// refused rather than producing a VM that never comes up. There is no
	// static maximum: the upper bound is the owning node's physical memory,
	// which only the manager can see (see internal/manager's capacity
	// check), so the FSM does not guess at it.
	VMConfigMinMemoryMB = 128
)

// applySetVMConfig changes vcpus and memory_mb on an existing VM and no
// other field. Identity, disks (disk_size_mb, base image, clone source,
// HAST replica), network (network_id, ip_address, mac_address), ISO,
// placement (node_id) and firewall/Cloudflare settings are carried over
// from the stored record untouched. Like every narrow Set* command it
// never replaces the whole record, so it cannot race a reconciler phase
// update or a lifecycle change into a lost write.
//
// The change takes effect only when the reconciler next launches the
// VM's bhyve process (ensureVM's CreateVM path). A running VM is never
// resized here, and a running VM's ensureVM takes the early-return path
// that skips launch entirely, so this command cannot restart a guest.
func (f *FSM) applySetVMConfig(index uint64, req *internalpb.SetVMConfig) *FSMApplyResult {
	vm, exists := f.vms[req.GetId()]
	if !exists {
		return &FSMApplyResult{Index: index, Error: fmt.Sprintf("SetVMConfig: id %q does not exist", req.GetId())}
	}
	if vm.GetDesiredState() == internalpb.VMState_VM_STATE_DELETING {
		return &FSMApplyResult{Index: index, Error: fmt.Sprintf("SetVMConfig: VM %q is marked for deletion", req.GetId())}
	}
	if v := req.GetVcpus(); v < VMConfigMinVCPUs || v > VMConfigMaxVCPUs {
		return &FSMApplyResult{Index: index, Error: fmt.Sprintf("SetVMConfig: vcpus %d is outside the supported range %d-%d", v, VMConfigMinVCPUs, VMConfigMaxVCPUs)}
	}
	if m := req.GetMemoryMb(); m < VMConfigMinMemoryMB {
		return &FSMApplyResult{Index: index, Error: fmt.Sprintf("SetVMConfig: memory_mb %d is below the minimum of %d MB", m, VMConfigMinMemoryMB)}
	}
	updated := proto.Clone(vm).(*internalpb.VMDefinition)
	updated.Vcpus = req.GetVcpus()
	updated.MemoryMb = req.GetMemoryMb()
	f.vms[req.GetId()] = updated
	return &FSMApplyResult{Index: index, VM: updated}
}
