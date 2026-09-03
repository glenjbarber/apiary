// Package frontend implements the HTMX web UI's server-side handlers: it
// renders HTML (full pages and HTMX partial-swap fragments) from data
// fetched via a rpcpb.ManagerServiceClient - the same client interface
// internal/restshim uses, since the frontend is just another
// ManagerService client, never talking to raftd directly.
package frontend

import (
	"fmt"
	"sort"
	"strings"
	"time"

	rpcpb "github.com/glenjbarber/apiary/api/rpc"
)

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

	// Phase is the reconciler's own observed progress ("pending",
	// "creating", "ready", "deleting", "error") - this is what the VM
	// table's State column shows, since desired_state alone (what a
	// caller asked for) never reflected whether that had actually
	// happened yet.
	Phase      string
	PhaseError string

	// ISOName is the installer image (if any) this VM was created with -
	// empty means it booted from its disk alone.
	ISOName string

	// NetworkID/IPAddress/MACAddress are set when this VM is attached
	// to a NetworkDefinition (ADR-0022) - IPAddress/MACAddress are
	// assigned automatically, never entered by a caller. All three are
	// empty for a VM using the flat single-bridge behavior from before
	// network management existed.
	NetworkID  string
	IPAddress  string
	MACAddress string

	// ReplicaNodeID, if set, names the node this VM's disk is
	// HAST-replicated to (ADR-0026) - data redundancy, not failover.
	ReplicaNodeID string
	BaseImageName string
	FirewallRules []firewallRuleView
}

type firewallRuleView struct {
	Direction string
	Action    string
	Protocol  string
	PortRange string
}

// networkView is the template-facing shape for a NetworkDefinition.
type networkView struct {
	ID         string
	Name       string
	VLANID     uint32
	Subnet     string
	BridgeName string

	// BridgeStatus is "up", "down", or "unknown" (this node has no VLAN
	// support configured, or the bridge doesn't exist here yet) -
	// physical, per-node state from ListNetworks, never set on create.
	BridgeStatus string
}

func fromRPCNetwork(n *rpcpb.NetworkDefinition) networkView {
	return networkView{
		ID:           n.GetId(),
		Name:         n.GetName(),
		VLANID:       n.GetVlanId(),
		Subnet:       n.GetSubnet(),
		BridgeName:   n.GetBridgeName(),
		BridgeStatus: n.GetBridgeStatus(),
	}
}

// jailView is the template-facing shape for a JailDefinition, mirroring
// vmView's shape but minimal like its own RPC type - see ADR-0026.
type jailView struct {
	ID           string
	Name         string
	Hostname     string
	NodeID       string
	DesiredState string

	// Phase is the reconciler's own observed progress, mirroring
	// vmView.Phase exactly.
	Phase      string
	PhaseError string

	// ReplicaNodeID, if set, names the node this jail's root filesystem
	// is HAST-replicated to (ADR-0026) - data redundancy, not failover.
	ReplicaNodeID string
}

func fromRPCJail(j *rpcpb.JailDefinition) jailView {
	if j == nil {
		return jailView{}
	}
	return jailView{
		ID:            j.GetId(),
		Name:          j.GetName(),
		Hostname:      j.GetHostname(),
		NodeID:        j.GetNodeId(),
		DesiredState:  jailStateFromRPC(j.GetDesiredState()),
		Phase:         jailPhaseFromRPC(j.GetPhase()),
		PhaseError:    j.GetPhaseError(),
		ReplicaNodeID: j.GetReplicaNodeId(),
	}
}

// jailStateFromRPC/jailPhaseFromRPC mirror stateFromRPC/phaseFromRPC
// exactly, for JailState/JailPhase instead of VMState/VMPhase.
func jailStateFromRPC(s rpcpb.JailState) string {
	switch s {
	case rpcpb.JailState_JAIL_STATE_RUNNING:
		return "running"
	case rpcpb.JailState_JAIL_STATE_STOPPED:
		return "stopped"
	case rpcpb.JailState_JAIL_STATE_DELETING:
		return "deleting"
	default:
		return ""
	}
}

func jailPhaseFromRPC(p rpcpb.JailPhase) string {
	switch p {
	case rpcpb.JailPhase_JAIL_PHASE_CREATING:
		return "creating"
	case rpcpb.JailPhase_JAIL_PHASE_READY:
		return "ready"
	case rpcpb.JailPhase_JAIL_PHASE_DELETING:
		return "deleting"
	case rpcpb.JailPhase_JAIL_PHASE_ERROR:
		return "error"
	default:
		return "pending"
	}
}

// apiKeyView is the template-facing shape for an APIKeyInfo - metadata
// only, never the raw key or its hash (see ADR-0023). Created is
// pre-formatted here (rather than in the template, which has no time
// helpers registered) from the RPC's raw Unix timestamp.
type apiKeyView struct {
	ID      string
	Name    string
	Role    string
	Created string
}

func fromRPCAPIKey(k *rpcpb.APIKeyInfo) apiKeyView {
	return apiKeyView{
		ID:      k.GetId(),
		Name:    k.GetName(),
		Role:    k.GetRole(),
		Created: time.Unix(k.GetCreatedUnix(), 0).Format("2006-01-02 15:04:05"),
	}
}

// isoView is the template-facing shape for a stored installer image.
type isoView struct {
	Name      string
	SizeBytes uint64
	SHA256    string
}

func fromRPCISO(i *rpcpb.ISOInfo) isoView {
	return isoView{Name: i.GetName(), SizeBytes: i.GetSizeBytes(), SHA256: i.GetSha256()}
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
	case rpcpb.VMState_VM_STATE_DELETING:
		return "deleting"
	default:
		return ""
	}
}

// phaseFromRPC renders VM_PHASE_UNSPECIFIED as "pending" - a VM that's
// never been reconciled by any node yet (no node picked it up, or it
// hasn't ticked since creation) is meaningfully different from one the
// reconciler is actively working on.
func phaseFromRPC(p rpcpb.VMPhase) string {
	switch p {
	case rpcpb.VMPhase_VM_PHASE_CREATING:
		return "creating"
	case rpcpb.VMPhase_VM_PHASE_READY:
		return "ready"
	case rpcpb.VMPhase_VM_PHASE_DELETING:
		return "deleting"
	case rpcpb.VMPhase_VM_PHASE_ERROR:
		return "error"
	default:
		return "pending"
	}
}

// sortVMs sorts vms in place by sortBy ("id", "node", or "state" -
// state meaning Phase, the real-time column; anything else falls back
// to "id"), case-insensitively, ascending unless dir is "desc". Ties
// within the requested key fall back to ID, so the order stays stable
// and predictable across repeated calls (e.g. every polling tick)
// rather than shuffling equal-Phase rows relative to each other.
func sortVMs(vms []vmView, sortBy, dir string) {
	key := func(v vmView) string {
		switch sortBy {
		case "node":
			return strings.ToLower(v.NodeID)
		case "state":
			return strings.ToLower(v.Phase)
		default:
			return strings.ToLower(v.ID)
		}
	}
	sort.SliceStable(vms, func(i, j int) bool {
		a, b := key(vms[i]), key(vms[j])
		if a != b {
			if dir == "desc" {
				return a > b
			}
			return a < b
		}
		if dir == "desc" {
			return strings.ToLower(vms[i].ID) > strings.ToLower(vms[j].ID)
		}
		return strings.ToLower(vms[i].ID) < strings.ToLower(vms[j].ID)
	})
}

func fromRPCVM(d *rpcpb.VMDefinition) vmView {
	if d == nil {
		return vmView{}
	}
	v := vmView{
		ID:            d.GetId(),
		Name:          d.GetName(),
		VCPUs:         d.GetVcpus(),
		MemoryMB:      d.GetMemoryMb(),
		NodeID:        d.GetNodeId(),
		DesiredState:  stateFromRPC(d.GetDesiredState()),
		Phase:         phaseFromRPC(d.GetPhase()),
		PhaseError:    d.GetPhaseError(),
		ISOName:       d.GetIsoName(),
		NetworkID:     d.GetNetworkId(),
		IPAddress:     d.GetIpAddress(),
		MACAddress:    d.GetMacAddress(),
		ReplicaNodeID: d.GetReplicaNodeId(),
		BaseImageName: d.GetBaseImageName(),
	}
	for _, rule := range d.GetFirewallRules() {
		v.FirewallRules = append(v.FirewallRules, firewallRuleView{
			Direction: rule.GetDirection(), Action: rule.GetAction(),
			Protocol: rule.GetProtocol(), PortRange: rule.GetPortRange(),
		})
	}
	return v
}

// statsView is the template-facing shape for a HostStats snapshot -
// bytes are pre-formatted (FormattedBytes) as human-readable strings
// (e.g. "2.72 TB") since templates can't do that arithmetic themselves.
type statsView struct {
	NodeID string

	Cores     int32
	LoadAvg1  float64
	LoadAvg5  float64
	LoadAvg15 float64

	MemTotal   string
	MemFree    string
	MemUsedPct float64

	Pools []poolView
	Disks []diskView
	Net   []netIfaceView
	PF    pfView

	Errors []string
}

// pfView is the template-facing shape for pf(8)'s summary stats.
type pfView struct {
	Enabled       bool
	CurrentStates uint64
	Matches       uint64
}

type poolView struct {
	Name        string
	Size        string
	Alloc       string
	Free        string
	CapacityPct uint32
	Health      string
}

type diskView struct {
	Name    string
	Model   string
	Serial  string
	Healthy bool
	Error   string
}

type netIfaceView struct {
	Name string
	Rx   string
	Tx   string
	Up   bool
}

// formatBytes renders n as a human-readable size (e.g. "2.72 TB"),
// using 1024-based (IEC) units - matching how zfs/zpool already report
// sizes elsewhere in this project.
func formatBytes(n uint64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := uint64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	units := []string{"KB", "MB", "GB", "TB", "PB"}
	return fmt.Sprintf("%.2f %s", float64(n)/float64(div), units[exp])
}

func fromRPCStats(resp *rpcpb.HostStatsResponse) statsView {
	cpu := resp.GetCpu()
	mem := resp.GetMem()

	var usedPct float64
	if mem.GetTotalBytes() > 0 {
		used := mem.GetTotalBytes() - mem.GetFreeBytes()
		usedPct = float64(used) / float64(mem.GetTotalBytes()) * 100
	}

	pools := make([]poolView, 0, len(resp.GetPools()))
	for _, p := range resp.GetPools() {
		pools = append(pools, poolView{
			Name: p.GetName(), Size: formatBytes(p.GetSizeBytes()), Alloc: formatBytes(p.GetAllocBytes()),
			Free: formatBytes(p.GetFreeBytes()), CapacityPct: p.GetCapacityPct(), Health: p.GetHealth(),
		})
	}

	disks := make([]diskView, 0, len(resp.GetDisks()))
	for _, d := range resp.GetDisks() {
		disks = append(disks, diskView{
			Name: d.GetName(), Model: d.GetModel(), Serial: d.GetSerial(),
			Healthy: d.GetHealthy(), Error: d.GetError(),
		})
	}

	net := make([]netIfaceView, 0, len(resp.GetNet()))
	for _, n := range resp.GetNet() {
		net = append(net, netIfaceView{Name: n.GetName(), Rx: formatBytes(n.GetRxBytes()), Tx: formatBytes(n.GetTxBytes()), Up: n.GetUp()})
	}

	pf := resp.GetPf()

	return statsView{
		NodeID:     resp.GetNodeId(),
		Cores:      cpu.GetCores(),
		LoadAvg1:   cpu.GetLoadAvg_1(),
		LoadAvg5:   cpu.GetLoadAvg_5(),
		LoadAvg15:  cpu.GetLoadAvg_15(),
		MemTotal:   formatBytes(mem.GetTotalBytes()),
		MemFree:    formatBytes(mem.GetFreeBytes()),
		MemUsedPct: usedPct,
		Pools:      pools,
		Disks:      disks,
		Net:        net,
		PF: pfView{
			Enabled: pf.GetEnabled(), CurrentStates: pf.GetCurrentStates(), Matches: pf.GetMatches(),
		},
		Errors: resp.GetErrors(),
	}
}
