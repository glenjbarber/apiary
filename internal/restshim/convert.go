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

	// ISOName/BaseImageName are caller-set on create - see ADR-0017/
	// ADR-0031. IPAddress/MACAddress are read-only, populated by the FSM
	// only when NetworkID is set (see VMDefinition's own doc comments).
	ISOName       string `json:"iso_name,omitempty"`
	NetworkID     string `json:"network_id,omitempty"`
	IPAddress     string `json:"ip_address,omitempty"`
	MACAddress    string `json:"mac_address,omitempty"`
	BaseImageName string `json:"base_image_name,omitempty"`

	// CloneFromSnapshot mirrors ADR-0095 - "<source_vm_id>@<snapshot_name>",
	// caller-set on create only.
	CloneFromSnapshot string `json:"clone_from_snapshot,omitempty"`
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
		Id:                v.ID,
		Name:              v.Name,
		Vcpus:             v.VCPUs,
		MemoryMb:          v.MemoryMB,
		NodeId:            v.NodeID,
		DesiredState:      stateToRPC(v.DesiredState),
		ReplicaNodeId:     v.ReplicaNodeID,
		IsoName:           v.ISOName,
		NetworkId:         v.NetworkID,
		BaseImageName:     v.BaseImageName,
		CloneFromSnapshot: v.CloneFromSnapshot,
	}
}

func fromRPCVM(d *rpcpb.VMDefinition) vm {
	if d == nil {
		return vm{}
	}
	return vm{
		ID:                d.GetId(),
		Name:              d.GetName(),
		VCPUs:             d.GetVcpus(),
		MemoryMB:          d.GetMemoryMb(),
		NodeID:            d.GetNodeId(),
		DesiredState:      stateFromRPC(d.GetDesiredState()),
		ReplicaNodeID:     d.GetReplicaNodeId(),
		ISOName:           d.GetIsoName(),
		NetworkID:         d.GetNetworkId(),
		IPAddress:         d.GetIpAddress(),
		MACAddress:        d.GetMacAddress(),
		BaseImageName:     d.GetBaseImageName(),
		CloneFromSnapshot: d.GetCloneFromSnapshot(),
	}
}

// jail is the REST-facing JSON shape for a jail definition, mirroring
// vm's own shape and reasoning - deliberately minimal like
// JailDefinition itself (see ADR-0027).
type jail struct {
	ID              string `json:"id"`
	Name            string `json:"name,omitempty"`
	Hostname        string `json:"hostname,omitempty"`
	NodeID          string `json:"node_id,omitempty"`
	ReplicaNodeID   string `json:"replica_node_id,omitempty"`
	DesiredState    string `json:"desired_state,omitempty"`
	BaseTemplate    string `json:"base_template,omitempty"`
	BaseArchiveName string `json:"base_archive_name,omitempty"`
	NetworkID       string `json:"network_id,omitempty"`
	IPAddress       string `json:"ip_address,omitempty"`
	VNET            bool   `json:"vnet,omitempty"`
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
		Id:              j.ID,
		Name:            j.Name,
		Hostname:        j.Hostname,
		NodeId:          j.NodeID,
		ReplicaNodeId:   j.ReplicaNodeID,
		DesiredState:    jailStateToRPC(j.DesiredState),
		BaseTemplate:    j.BaseTemplate,
		BaseArchiveName: j.BaseArchiveName,
		NetworkId:       j.NetworkID,
		Vnet:            j.VNET,
	}
}

func fromRPCJail(d *rpcpb.JailDefinition) jail {
	if d == nil {
		return jail{}
	}
	return jail{
		ID:              d.GetId(),
		Name:            d.GetName(),
		Hostname:        d.GetHostname(),
		NodeID:          d.GetNodeId(),
		ReplicaNodeID:   d.GetReplicaNodeId(),
		DesiredState:    jailStateFromRPC(d.GetDesiredState()),
		BaseTemplate:    d.GetBaseTemplate(),
		BaseArchiveName: d.GetBaseArchiveName(),
		NetworkID:       d.GetNetworkId(),
		IPAddress:       d.GetIpAddress(),
		VNET:            d.GetVnet(),
	}
}

// network is the REST-facing JSON shape for a NetworkDefinition,
// mirroring vm/jail's own shape and reasoning.
type network struct {
	ID              string `json:"id"`
	Name            string `json:"name,omitempty"`
	VLANID          uint32 `json:"vlan_id,omitempty"`
	Subnet          string `json:"subnet"`
	BridgeName      string `json:"bridge_name,omitempty"`
	ExternalGateway string `json:"external_gateway,omitempty"`
	BridgeStatus    string `json:"bridge_status,omitempty"`
}

func toRPCNetwork(n network) *rpcpb.NetworkDefinition {
	return &rpcpb.NetworkDefinition{
		Id:              n.ID,
		Name:            n.Name,
		VlanId:          n.VLANID,
		Subnet:          n.Subnet,
		BridgeName:      n.BridgeName,
		ExternalGateway: n.ExternalGateway,
	}
}

func fromRPCNetwork(d *rpcpb.NetworkDefinition) network {
	if d == nil {
		return network{}
	}
	return network{
		ID:              d.GetId(),
		Name:            d.GetName(),
		VLANID:          d.GetVlanId(),
		Subnet:          d.GetSubnet(),
		BridgeName:      d.GetBridgeName(),
		ExternalGateway: d.GetExternalGateway(),
		BridgeStatus:    d.GetBridgeStatus(),
	}
}

// healthObservation is the REST-facing JSON shape for one raw piece of
// evidence behind a verdict. The five fields mirror
// rpcpb.HealthObservation exactly; they are restated here rather than
// relying on protobuf's generated JSON tags for the same reason the vm,
// jail, and network types above are.
type healthObservation struct {
	Source                string `json:"source"`
	ObservedUnix          int64  `json:"observed_unix"`
	FreshnessLimitSeconds uint32 `json:"freshness_limit_seconds"`
	Value                 string `json:"value,omitempty"`
	Detail                string `json:"detail,omitempty"`
}

// knownHealthStatuses are the health states this build of the shim
// recognizes, sent to the caller in every response so the obligation
// ADR-0122 documents can be discharged mechanically rather than from
// memory. See clusterHealth for why that matters.
var knownHealthStatuses = []string{"healthy", "degraded", "unknown", "stale", "contradictory"}

// nodeHealth is the REST-facing JSON shape for one Comb's verdict.
type nodeHealth struct {
	NodeID       string              `json:"node_id"`
	Status       string              `json:"status"`
	Explanation  string              `json:"explanation,omitempty"`
	Observations []healthObservation `json:"observations"`

	// Dialed is managerd's own wording, kept verbatim: it is true for the
	// node that answered the request, because that node's reachability is
	// established by answering rather than by a dial. It does not mean a
	// socket was opened.
	Dialed bool `json:"dialed"`

	// RaftStateDigest is this Comb's own FSM state digest (ADR-0143),
	// carried per row and uncompared - deciding whether digests agree is a
	// colony-wide question, and answering it here would bake a verdict
	// into a read several consumers make for unrelated reasons. An empty
	// or absent digest means it could not be read at all, and is never
	// evidence of agreement. Always read it together with
	// RaftAppliedIndex: a mismatch at equal indexes is a real
	// disagreement, a mismatch at different indexes may only be a sample
	// taken while the cluster was moving.
	RaftStateDigest string `json:"raft_state_digest"`

	// RaftAppliedIndex is emitted even when it is 0, because 0 is a real
	// value and a digest is meaningless without the index it was taken
	// at. 0 is also what an unreadable index looks like - the raft
	// applied-index observation beside it is the way to tell those apart.
	RaftAppliedIndex uint64 `json:"raft_applied_index"`
}

// clusterHealth is the REST-facing JSON shape for the colony-wide health
// read, a pass-through of managerd's ClusterHealthResponse (ADR-0122).
//
// Error is deliberately NOT an error status code here, and the caller
// must not treat its absence as a pass either. Unlike every other
// in-band error in this shim, it is not a rejection of the request: the
// RPC succeeded and the rows below it are real verdicts, merely capped at
// unknown because managerd could not read raft membership. Turning that
// into a 4xx would discard the very rows that make the failure legible,
// so it is carried in a 200 body and left for the caller to act on.
//
// KnownStatuses is the documentation ADR-0122's decision 3 puts on the
// wire. status stays a string rather than becoming an enum, so a caller
// reading a response from a newer managerd - or a newer shim - can meet a
// state it has never heard of, and the failure mode that string contract
// cannot prevent is a switch statement falling through to a default arm
// that reads as a pass. That obligation belongs with the response rather
// than silently in a client's head, so the states this build recognizes
// are sent with every response: check a row's status against this list,
// and treat anything not on it as not healthy. The rule is what
// membership arithmetic masquerading as availability proof looks like
// from the outside, which is the exact failure Evidence-Aware Health
// exists to prevent.
type clusterHealth struct {
	Error         string       `json:"error,omitempty"`
	LocalNodeID   string       `json:"local_node_id"`
	KnownStatuses []string     `json:"known_statuses"`
	Nodes         []nodeHealth `json:"nodes"`
}

// fromRPCClusterHealth renders managerd's verdict without reinterpreting
// any part of it. Every known Comb produces a row whatever its evidence,
// so a caller reading this can never mistake an absent entry for a
// healthy one; a Comb that could not be reached arrives with a
// non-healthy status and no observations rather than being dropped here.
// The empty slices are non-nil so they marshal as [] rather than null.
func fromRPCClusterHealth(r *rpcpb.ClusterHealthResponse) clusterHealth {
	out := clusterHealth{
		LocalNodeID:   r.GetLocalNodeId(),
		Error:         r.GetError(),
		KnownStatuses: knownHealthStatuses,
		Nodes:         make([]nodeHealth, 0, len(r.GetNodes())),
	}
	for _, n := range r.GetNodes() {
		row := nodeHealth{
			NodeID:           n.GetNodeId(),
			Status:           n.GetStatus(),
			Explanation:      n.GetExplanation(),
			Dialed:           n.GetDialed(),
			RaftStateDigest:  n.GetRaftStateDigest(),
			RaftAppliedIndex: n.GetRaftAppliedIndex(),
			Observations:     make([]healthObservation, 0, len(n.GetObservations())),
		}
		for _, o := range n.GetObservations() {
			row.Observations = append(row.Observations, healthObservation{
				Source:                o.GetSource(),
				ObservedUnix:          o.GetObservedUnix(),
				FreshnessLimitSeconds: o.GetFreshnessLimitSeconds(),
				Value:                 o.GetValue(),
				Detail:                o.GetDetail(),
			})
		}
		out.Nodes = append(out.Nodes, row)
	}
	return out
}
