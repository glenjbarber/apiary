package manager

import (
	internalpb "github.com/glenjbarber/apiary/api/internalpb"
	rpcpb "github.com/glenjbarber/apiary/api/rpc"
)

// toInternalVM converts the external-facing VMDefinition into the internal
// protocol's type. Kept as a separate type from api/internalpb's (per
// ADR-0002/ADR-0005) so the external schema doesn't couple to the internal
// protocol's evolution - this is the translation layer that decoupling
// requires.
func toInternalVM(vm *rpcpb.VMDefinition) *internalpb.VMDefinition {
	return &internalpb.VMDefinition{
		Id:           vm.GetId(),
		Name:         vm.GetName(),
		Vcpus:        vm.GetVcpus(),
		MemoryMb:     vm.GetMemoryMb(),
		NodeId:       vm.GetNodeId(),
		DesiredState: internalpb.VMState(vm.GetDesiredState()),
		IsoName:      vm.GetIsoName(),
		// Phase/PhaseError are the reconciler's own observed state, never
		// set by an external caller - CreateVM/UpdateVM requests never
		// carry them through.
	}
}

// fromInternalVM converts an internal VMDefinition back to the external
// schema's type.
func fromInternalVM(vm *internalpb.VMDefinition) *rpcpb.VMDefinition {
	if vm == nil {
		return nil
	}
	return &rpcpb.VMDefinition{
		Id:           vm.GetId(),
		Name:         vm.GetName(),
		Vcpus:        vm.GetVcpus(),
		MemoryMb:     vm.GetMemoryMb(),
		NodeId:       vm.GetNodeId(),
		DesiredState: rpcpb.VMState(vm.GetDesiredState()),
		Phase:        rpcpb.VMPhase(vm.GetPhase()),
		PhaseError:   vm.GetPhaseError(),
		IsoName:      vm.GetIsoName(),
	}
}
