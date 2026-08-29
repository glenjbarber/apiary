// Package restshim translates managerd's external gRPC API
// (api/rpc.ManagerService) into a JSON-over-HTTP REST API, per CLAUDE.md's
// "RPC-style first ... REST translation layer sits on top afterward"
// architecture. It is a client of ManagerService, the same way managerd
// itself is a client of raftd's internal protocol - restshim never talks
// to raftd directly.
package restshim

import rpcpb "github.com/glenjbarber/apiary/api/rpc"

// vm is the REST-facing JSON shape for a VM definition. Kept as its own
// type (rather than exposing api/rpc's generated struct/JSON tags
// directly) so the REST schema's JSON shape isn't hostage to whatever
// protobuf's default JSON mapping happens to produce - the same
// decoupling reasoning ADR-0002/ADR-0005 already applied between
// api/internalpb and api/rpc.
type vm struct {
	ID           string `json:"id"`
	Name         string `json:"name,omitempty"`
	VCPUs        uint32 `json:"vcpus,omitempty"`
	MemoryMB     uint64 `json:"memory_mb,omitempty"`
	NodeID       string `json:"node_id,omitempty"`
	DesiredState string `json:"desired_state,omitempty"`
}

// stateToRPC/stateFromRPC translate the REST API's plain string state
// ("running"/"stopped") to/from api/rpc's VMState enum. An empty or
// unrecognized string maps to VM_STATE_UNSPECIFIED, matching how an
// unset field behaves elsewhere in this project's proto schemas.
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

func toRPCVM(v vm) *rpcpb.VMDefinition {
	return &rpcpb.VMDefinition{
		Id:           v.ID,
		Name:         v.Name,
		Vcpus:        v.VCPUs,
		MemoryMb:     v.MemoryMB,
		NodeId:       v.NodeID,
		DesiredState: stateToRPC(v.DesiredState),
	}
}

func fromRPCVM(d *rpcpb.VMDefinition) vm {
	if d == nil {
		return vm{}
	}
	return vm{
		ID:           d.GetId(),
		Name:         d.GetName(),
		VCPUs:        d.GetVcpus(),
		MemoryMB:     d.GetMemoryMb(),
		NodeID:       d.GetNodeId(),
		DesiredState: stateFromRPC(d.GetDesiredState()),
	}
}
