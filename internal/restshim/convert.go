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
	ID            string `json:"id"`
	Name          string `json:"name,omitempty"`
	VCPUs         uint32 `json:"vcpus,omitempty"`
	MemoryMB      uint64 `json:"memory_mb,omitempty"`
	NodeID        string `json:"node_id,omitempty"`
	DesiredState  string `json:"desired_state,omitempty"`
	ReplicaNodeID string `json:"replica_node_id,omitempty"`
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
		Id:            v.ID,
		Name:          v.Name,
		Vcpus:         v.VCPUs,
		MemoryMb:      v.MemoryMB,
		NodeId:        v.NodeID,
		DesiredState:  stateToRPC(v.DesiredState),
		ReplicaNodeId: v.ReplicaNodeID,
	}
}

func fromRPCVM(d *rpcpb.VMDefinition) vm {
	if d == nil {
		return vm{}
	}
	return vm{
		ID:            d.GetId(),
		Name:          d.GetName(),
		VCPUs:         d.GetVcpus(),
		MemoryMB:      d.GetMemoryMb(),
		NodeID:        d.GetNodeId(),
		DesiredState:  stateFromRPC(d.GetDesiredState()),
		ReplicaNodeID: d.GetReplicaNodeId(),
	}
}

// jail is the REST-facing JSON shape for a jail definition, mirroring
// vm's own shape and reasoning - deliberately minimal like
// JailDefinition itself (see ADR-0027).
type jail struct {
	ID            string `json:"id"`
	Name          string `json:"name,omitempty"`
	Hostname      string `json:"hostname,omitempty"`
	NodeID        string `json:"node_id,omitempty"`
	ReplicaNodeID string `json:"replica_node_id,omitempty"`
	DesiredState  string `json:"desired_state,omitempty"`
}

// jailStateToRPC/jailStateFromRPC mirror stateToRPC/stateFromRPC, for
// JailState instead of VMState.
func jailStateToRPC(s string) rpcpb.JailState {
	switch s {
	case "running":
		return rpcpb.JailState_JAIL_STATE_RUNNING
	case "stopped":
		return rpcpb.JailState_JAIL_STATE_STOPPED
	default:
		return rpcpb.JailState_JAIL_STATE_UNSPECIFIED
	}
}

func jailStateFromRPC(s rpcpb.JailState) string {
	switch s {
	case rpcpb.JailState_JAIL_STATE_RUNNING:
		return "running"
	case rpcpb.JailState_JAIL_STATE_STOPPED:
		return "stopped"
	default:
		return ""
	}
}

func toRPCJail(j jail) *rpcpb.JailDefinition {
	return &rpcpb.JailDefinition{
		Id:            j.ID,
		Name:          j.Name,
		Hostname:      j.Hostname,
		NodeId:        j.NodeID,
		ReplicaNodeId: j.ReplicaNodeID,
		DesiredState:  jailStateToRPC(j.DesiredState),
	}
}

func fromRPCJail(d *rpcpb.JailDefinition) jail {
	if d == nil {
		return jail{}
	}
	return jail{
		ID:            d.GetId(),
		Name:          d.GetName(),
		Hostname:      d.GetHostname(),
		NodeID:        d.GetNodeId(),
		ReplicaNodeID: d.GetReplicaNodeId(),
		DesiredState:  jailStateFromRPC(d.GetDesiredState()),
	}
}

// network is the REST-facing JSON shape for a NetworkDefinition,
// mirroring vm/jail's own shape and reasoning.
type network struct {
	ID           string `json:"id"`
	Name         string `json:"name,omitempty"`
	VLANID       uint32 `json:"vlan_id,omitempty"`
	Subnet       string `json:"subnet"`
	BridgeName   string `json:"bridge_name,omitempty"`
	BridgeStatus string `json:"bridge_status,omitempty"`
}

func toRPCNetwork(n network) *rpcpb.NetworkDefinition {
	return &rpcpb.NetworkDefinition{
		Id:         n.ID,
		Name:       n.Name,
		VlanId:     n.VLANID,
		Subnet:     n.Subnet,
		BridgeName: n.BridgeName,
	}
}

func fromRPCNetwork(d *rpcpb.NetworkDefinition) network {
	if d == nil {
		return network{}
	}
	return network{
		ID:           d.GetId(),
		Name:         d.GetName(),
		VLANID:       d.GetVlanId(),
		Subnet:       d.GetSubnet(),
		BridgeName:   d.GetBridgeName(),
		BridgeStatus: d.GetBridgeStatus(),
	}
}
