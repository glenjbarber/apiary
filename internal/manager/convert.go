package manager

import (
	"crypto/sha256"
	"fmt"

	internalpb "github.com/glenjbarber/apiary/api/internalpb"
	rpcpb "github.com/glenjbarber/apiary/api/rpc"
)

// resolveBridgeName mirrors internal/cluster's networkBridgeName
// exactly (same hash formula) - duplicated rather than imported to
// avoid pulling internal/cluster's much heavier dependency graph
// (bhyve/dhcpd/pf/vlan) into managerd's RPC layer just for this one
// small pure function. See networkBridgeName's own doc comment for why
// a network id can't be embedded directly (FreeBSD's 15-usable-
// character interface name limit).
func resolveBridgeName(n *internalpb.NetworkDefinition) string {
	if n.GetBridgeName() != "" {
		return n.GetBridgeName()
	}
	sum := sha256.Sum256([]byte(n.GetId()))
	return fmt.Sprintf("apnet-%x", sum[:4])
}

// toInternalVM converts the external-facing VMDefinition into the internal
// protocol's type. Kept as a separate type from api/internalpb's (per
// ADR-0002/ADR-0005) so the external schema doesn't couple to the internal
// protocol's evolution - this is the translation layer that decoupling
// requires.
func toInternalVM(vm *rpcpb.VMDefinition) *internalpb.VMDefinition {
	rules := make([]*internalpb.FirewallRule, 0, len(vm.GetFirewallRules()))
	for _, r := range vm.GetFirewallRules() {
		rules = append(rules, &internalpb.FirewallRule{
			Direction: r.GetDirection(),
			Action:    r.GetAction(),
			Protocol:  r.GetProtocol(),
			PortRange: r.GetPortRange(),
		})
	}
	return &internalpb.VMDefinition{
		Id:            vm.GetId(),
		Name:          vm.GetName(),
		Vcpus:         vm.GetVcpus(),
		MemoryMb:      vm.GetMemoryMb(),
		NodeId:        vm.GetNodeId(),
		DesiredState:  internalpb.VMState(vm.GetDesiredState()),
		IsoName:       vm.GetIsoName(),
		NetworkId:     vm.GetNetworkId(),
		ReplicaNodeId: vm.GetReplicaNodeId(),
		BaseImageName: vm.GetBaseImageName(),
		// IpAddress/MacAddress are assigned by the FSM itself, never set
		// by an external caller.
		FirewallRules: rules,
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
	rules := make([]*rpcpb.FirewallRule, 0, len(vm.GetFirewallRules()))
	for _, r := range vm.GetFirewallRules() {
		rules = append(rules, &rpcpb.FirewallRule{
			Direction: r.GetDirection(),
			Action:    r.GetAction(),
			Protocol:  r.GetProtocol(),
			PortRange: r.GetPortRange(),
		})
	}
	return &rpcpb.VMDefinition{
		Id:            vm.GetId(),
		Name:          vm.GetName(),
		Vcpus:         vm.GetVcpus(),
		MemoryMb:      vm.GetMemoryMb(),
		NodeId:        vm.GetNodeId(),
		DesiredState:  rpcpb.VMState(vm.GetDesiredState()),
		Phase:         rpcpb.VMPhase(vm.GetPhase()),
		PhaseError:    vm.GetPhaseError(),
		IsoName:       vm.GetIsoName(),
		NetworkId:     vm.GetNetworkId(),
		IpAddress:     vm.GetIpAddress(),
		MacAddress:    vm.GetMacAddress(),
		ReplicaNodeId: vm.GetReplicaNodeId(),
		BaseImageName: vm.GetBaseImageName(),
		FirewallRules: rules,
	}
}

// toInternalNetwork/fromInternalNetwork mirror toInternalVM/fromInternalVM,
// for the same reason - decoupling the external schema from the internal
// protocol's evolution.
func toInternalNetwork(n *rpcpb.NetworkDefinition) *internalpb.NetworkDefinition {
	return &internalpb.NetworkDefinition{
		Id:         n.GetId(),
		Name:       n.GetName(),
		VlanId:     n.GetVlanId(),
		Subnet:     n.GetSubnet(),
		BridgeName: n.GetBridgeName(),
	}
}

func fromInternalNetwork(n *internalpb.NetworkDefinition) *rpcpb.NetworkDefinition {
	if n == nil {
		return nil
	}
	return &rpcpb.NetworkDefinition{
		Id:         n.GetId(),
		Name:       n.GetName(),
		VlanId:     n.GetVlanId(),
		Subnet:     n.GetSubnet(),
		BridgeName: n.GetBridgeName(),
	}
}

func toInternalJail(j *rpcpb.JailDefinition) *internalpb.JailDefinition {
	return &internalpb.JailDefinition{
		Id:            j.GetId(),
		Name:          j.GetName(),
		Hostname:      j.GetHostname(),
		NodeId:        j.GetNodeId(),
		ReplicaNodeId: j.GetReplicaNodeId(),
		DesiredState:  internalpb.JailState(j.GetDesiredState()),
		// Phase/PhaseError are the reconciler's own observed state, never
		// set by an external caller - CreateJail/UpdateJail requests
		// never carry them through.
	}
}

func fromInternalJail(j *internalpb.JailDefinition) *rpcpb.JailDefinition {
	if j == nil {
		return nil
	}
	return &rpcpb.JailDefinition{
		Id:            j.GetId(),
		Name:          j.GetName(),
		Hostname:      j.GetHostname(),
		NodeId:        j.GetNodeId(),
		ReplicaNodeId: j.GetReplicaNodeId(),
		DesiredState:  rpcpb.JailState(j.GetDesiredState()),
		Phase:         rpcpb.JailPhase(j.GetPhase()),
		PhaseError:    j.GetPhaseError(),
	}
}

// fromInternalAPIKey converts an internal ApiKey to the external,
// metadata-only APIKeyInfo type - HashedKey is deliberately never
// copied across this boundary (see APIKeyInfo's own doc comment), so a
// key's hash can never leak back out over ManagerService even if a
// future caller of this function forgets to strip it.
func fromInternalAPIKey(k *internalpb.ApiKey) *rpcpb.APIKeyInfo {
	if k == nil {
		return nil
	}
	return &rpcpb.APIKeyInfo{
		Id:          k.GetId(),
		Name:        k.GetName(),
		CreatedUnix: k.GetCreatedUnix(),
		Role:        k.GetRole(),
	}
}
