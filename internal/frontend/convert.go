// Package frontend implements the HTMX web UI's server-side handlers: it
// renders HTML (full pages and HTMX partial-swap fragments) from data
// fetched via a rpcpb.ManagerServiceClient - the same client interface
// internal/restshim uses, since the frontend is just another
// ManagerService client, never talking to raftd directly.
package frontend

import rpcpb "github.com/glenjbarber/apiary/api/rpc"

// vmView is the template-facing shape for a VM. Kept as its own type
// (rather than exposing api/rpc's generated struct to templates
// directly) for the same decoupling reasons ADR-0002/ADR-0005/ADR-0011
// already established between layers.
type vmView struct {
	ID           string
	Name         string
	VCPUs        uint32
	MemoryMB     uint64
	NodeID       string
	DesiredState string
}

func stateToRPC(s string) rpcpb.VMState {
	switch s {
	case "running":
		return rpcpb.VMState_VM_STATE_RUNNING
	case "stopped":
		return rpcpb.VMState_VM_STATE_STOPPED
	default:
		return rpcpb.VMState_VM_STATE_UNSPECIFIED
	}
}

func stateFromRPC(s rpcpb.VMState) string {
	switch s {
	case rpcpb.VMState_VM_STATE_RUNNING:
		return "running"
	case rpcpb.VMState_VM_STATE_STOPPED:
		return "stopped"
	default:
		return ""
	}
}

func fromRPCVM(d *rpcpb.VMDefinition) vmView {
	if d == nil {
		return vmView{}
	}
	return vmView{
		ID:           d.GetId(),
		Name:         d.GetName(),
		VCPUs:        d.GetVcpus(),
		MemoryMB:     d.GetMemoryMb(),
		NodeID:       d.GetNodeId(),
		DesiredState: stateFromRPC(d.GetDesiredState()),
	}
}
