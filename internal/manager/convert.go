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

// toInternalFirewallRules converts the external, wire FirewallRule
// shape to its internal counterpart - shared by toInternalVM (at
// create/update time) and SetVMFirewallRules (editing rules on an
// existing VM), so both paths convert identically.
func toInternalFirewallRules(rules []*rpcpb.FirewallRule) []*internalpb.FirewallRule {
	out := make([]*internalpb.FirewallRule, 0, len(rules))
	for _, r := range rules {
		out = append(out, &internalpb.FirewallRule{
			Direction: r.GetDirection(),
			Action:    r.GetAction(),
			Protocol:  r.GetProtocol(),
			PortRange: r.GetPortRange(),
			Priority:  r.GetPriority(),
		})
	}
	return out
}

// toInternalVM converts the external-facing VMDefinition into the internal
// protocol's type. Kept as a separate type from api/internalpb's (per
// ADR-0002/ADR-0005) so the external schema doesn't couple to the internal
// protocol's evolution - this is the translation layer that decoupling
// requires.
func toInternalVM(vm *rpcpb.VMDefinition) *internalpb.VMDefinition {
	rules := toInternalFirewallRules(vm.GetFirewallRules())
	return &internalpb.VMDefinition{
		Id:                vm.GetId(),
		Name:              vm.GetName(),
		Vcpus:             vm.GetVcpus(),
		MemoryMb:          vm.GetMemoryMb(),
		NodeId:            vm.GetNodeId(),
		DesiredState:      internalpb.VMState(vm.GetDesiredState()),
		IsoName:           vm.GetIsoName(),
		NetworkId:         vm.GetNetworkId(),
		ReplicaNodeId:     vm.GetReplicaNodeId(),
		BaseImageName:     vm.GetBaseImageName(),
		CloneFromSnapshot: vm.GetCloneFromSnapshot(),
		// DiskSizeMb (ADR-0148) is the per-VM override of the
		// Colony-wide floor, in MiB. Zero means "no override", and the
		// FSM refuses a non-zero value below the floor.
		DiskSizeMb: vm.GetDiskSizeMb(),
		// IpAddress/MacAddress are assigned by the FSM itself, never set
		// by an external caller.
		FirewallRules: rules,
		// Phase/PhaseError are the reconciler's own observed state, never
		// set by an external caller - CreateVM/UpdateVM requests never
		// carry them through. FirewallPaused is likewise excluded here -
		// only SetVMFirewallPaused's own dedicated command may set it.
		// CloudflareHostname/CloudflarePort are excluded for the identical
		// reason - only SetVMCloudflareExposure's own dedicated command
		// may set them (ADR-0063).
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
			Priority:  r.GetPriority(),
		})
	}
	return &rpcpb.VMDefinition{
		Id:                vm.GetId(),
		Name:              vm.GetName(),
		Vcpus:             vm.GetVcpus(),
		MemoryMb:          vm.GetMemoryMb(),
		NodeId:            vm.GetNodeId(),
		DesiredState:      rpcpb.VMState(vm.GetDesiredState()),
		Phase:             rpcpb.VMPhase(vm.GetPhase()),
		PhaseError:        vm.GetPhaseError(),
		IsoName:           vm.GetIsoName(),
		NetworkId:         vm.GetNetworkId(),
		IpAddress:         vm.GetIpAddress(),
		MacAddress:        vm.GetMacAddress(),
		ReplicaNodeId:     vm.GetReplicaNodeId(),
		BaseImageName:     vm.GetBaseImageName(),
		CloneFromSnapshot: vm.GetCloneFromSnapshot(),
		FirewallRules:     rules,
		// FirewallPaused is read-only from this direction too - only
		// SetVMFirewallPaused's own dedicated command may set it.
		FirewallPaused: vm.GetFirewallPaused(),
		// CloudflareHostname/CloudflarePort mirror FirewallPaused's own
		// read-only-here treatment - only SetVMCloudflareExposure's own
		// dedicated command may set them (ADR-0063).
		CloudflareHostname: vm.GetCloudflareHostname(),
		CloudflarePort:     vm.GetCloudflarePort(),
		// DiskSizeMb (ADR-0148) mirrors the internal field.
		DiskSizeMb: vm.GetDiskSizeMb(),
		// ResourceId (ADR-0152) is the VM's PERSISTED PHYSICAL identity,
		// and it IS carried back out: a caller (notably the Cluster API
		// provider) needs to know the physical name the VM's host
		// resources carry, because that name does NOT change when the
		// logical id is renamed. It is read-only in the OTHER direction
		// - toInternalVM never copies it in, so a caller cannot set a
		// VM's physical identity through CreateVM/UpdateVM.
		ResourceId: vm.GetResourceId(),
	}
}

// toInternalNetwork/fromInternalNetwork mirror toInternalVM/fromInternalVM,
// for the same reason - decoupling the external schema from the internal
// protocol's evolution.
func toInternalNetwork(n *rpcpb.NetworkDefinition) *internalpb.NetworkDefinition {
	return &internalpb.NetworkDefinition{
		Id:              n.GetId(),
		Name:            n.GetName(),
		VlanId:          n.GetVlanId(),
		Subnet:          n.GetSubnet(),
		BridgeName:      n.GetBridgeName(),
		ExternalGateway: n.GetExternalGateway(),
		UplinkBridged:   n.GetUplinkBridged(),
	}
}

func fromInternalNetwork(n *internalpb.NetworkDefinition) *rpcpb.NetworkDefinition {
	if n == nil {
		return nil
	}
	return &rpcpb.NetworkDefinition{
		Id:              n.GetId(),
		Name:            n.GetName(),
		VlanId:          n.GetVlanId(),
		Subnet:          n.GetSubnet(),
		BridgeName:      n.GetBridgeName(),
		ExternalGateway: n.GetExternalGateway(),
		UplinkBridged:   n.GetUplinkBridged(),
	}
}

// fromInternalPendingJoinRequest converts a raft-replicated
// PendingJoinRequest to its external schema counterpart (ADR-0083) -
// same decoupling reasoning as every other toInternal*/fromInternal*
// pair here. JoinRequestStatus is defined identically (same names, same
// numeric values) in both api/internalpb and api/rpc, so a plain
// numeric cast is safe and exact, not just coincidentally compatible.
func fromInternalPendingJoinRequest(r *internalpb.PendingJoinRequest) *rpcpb.PendingJoinRequest {
	if r == nil {
		return nil
	}
	return &rpcpb.PendingJoinRequest{
		RequestId:              r.GetRequestId(),
		NodeId:                 r.GetNodeId(),
		RaftBindAddress:        r.GetRaftBindAddress(),
		Code:                   r.GetCode(),
		RequestedAtUnix:        r.GetRequestedAtUnix(),
		ExpiresAtUnix:          r.GetExpiresAtUnix(),
		Status:                 rpcpb.JoinRequestStatus(r.GetStatus()),
		TlsCertFingerprint:     r.GetTlsCertFingerprint(),
		JoinerLogStateObserved: r.GetJoinerLogStateObserved(),
		JoinerLastLogIndex:     r.GetJoinerLastLogIndex(),
		WindowOpenedAtUnix:     r.GetWindowOpenedAtUnix(),
		Approval_1:             fromInternalJoinApproval(r.GetApproval_1()),
		Approval_2:             fromInternalJoinApproval(r.GetApproval_2()),
		AuthorizationId:        r.GetAuthorizationId(),
		ConsumedAtUnix:         r.GetConsumedAtUnix(),
		// ADR-0147 Part 2. The SECOND PIN is deliberately absent here,
		// and there is no field for it: this message is what the
		// TARGET's own operator reads (ListJoinRequests, and the
		// request body of a poll response), and a value they can read
		// here is a value they can complete the handshake with alone,
		// which collapses a two-party flow into one. The PIN travels on
		// GetJoinRequestStatus's own response, to the holder of the
		// request_id AND the first code.
		Stage:                  rpcpb.JoinRequestStage(r.GetStage()),
		AdvertisedFingerprints: r.GetAdvertisedFingerprints(),
		FirstCodeAttempts:      r.GetFirstCodeAttempts(),
		SecondPinAttempts:      r.GetSecondPinAttempts(),
		SecondPinExpiresAtUnix: r.GetSecondPinExpiresAtUnix(),
		SecondPinReissues:      r.GetSecondPinReissues(),
		// ADR-0156 Stage 1: the Colony the joiner claimed, carried
		// through so the operator reviewing the request can see which
		// Colony the joiner said it was joining.
		ColonyId: r.GetColonyId(),
		// ADR-0156 Stage 2: the Colony GENERATION the joiner
		// claimed, carried through for the same reason. Zero means the
		// joiner made no claim, which is deliberately not the same as
		// a claim of epoch 0 - the audit trail distinguishes "claimed
		// epoch 1" from "claimed nothing".
		ColonyEpoch: r.GetColonyEpoch(),
	}
}

// toInternalTrustedPeer mirrors an external rpcpb.TrustedPeer onto the
// internal one.
//
// It exists because the two are DIFFERENT PROTOBUF TYPES, not one type
// under two names. proto.Clone of an rpcpb.TrustedPeer yields an
// rpcpb.TrustedPeer, so asserting it as an internalpb.TrustedPeer
// panics at runtime - which is what PinPeerCertificate did, and
// therefore what every call to that RPC did.
//
// The conversion is explicit and field-by-field rather than a clone
// with an assertion, for two reasons. It cannot panic, and it cannot
// silently carry a field across that the two schemas spell
// differently. A new field added to one side and not the other is
// then a COMPILE error rather than a value that quietly means
// something else on the other side.
//
// The nested staged certificate is converted too: ADR-0170's overlap
// lives in the `next` field, and a conversion that dropped it would
// quietly lose a staged certificate on its way into the store.
func toInternalTrustedPeer(p *rpcpb.TrustedPeer) *internalpb.TrustedPeer {
	if p == nil {
		return nil
	}
	return &internalpb.TrustedPeer{
		NodeId:       p.GetNodeId(),
		CombName:     p.GetCombName(),
		Fingerprint:  p.GetFingerprint(),
		CertPem:      p.GetCertPem(),
		NotAfterUnix: p.GetNotAfterUnix(),
		PinnedAtUnix: p.GetPinnedAtUnix(),
		PinnedBy:     p.GetPinnedBy(),
		IsVoter:      p.GetIsVoter(),
		Next:         toInternalTrustedPeer(p.GetNext()),
		// The CA anchor and its digest travel with the pin, so a
		// CA-issued certificate reaches the store recorded under the
		// CA that signed it.
		CaPem:         p.GetCaPem(),
		CaFingerprint: p.GetCaFingerprint(),
	}
}

// toRPCCertificateReservation mirrors internalpb.CertificateReservation
// onto the external one (ADR-0171).
//
// It carries METADATA only: a FQDN, a fingerprint, and the joining
// Comb and request the preparation has been bound to. The prepared
// certificate's private key is not in the record and is not rendered
// by this function, because it is not anywhere in the store either.
func toRPCCertificateReservation(r *internalpb.CertificateReservation) *rpcpb.CertificateReservation {
	if r == nil {
		return nil
	}
	return &rpcpb.CertificateReservation{
		ReservationId:   r.GetReservationId(),
		Fqdn:            r.GetFqdn(),
		Fingerprint:     r.GetFingerprint(),
		NodeId:          r.GetNodeId(),
		RequestId:       r.GetRequestId(),
		CreatedAtUnix:   r.GetCreatedAtUnix(),
		ExpiresAtUnix:   r.GetExpiresAtUnix(),
		BoundAtUnix:     r.GetBoundAtUnix(),
		DeliveredAtUnix: r.GetDeliveredAtUnix(),
		PreparedBy:      r.GetPreparedBy(),
	}
}

// toRPCTustedPeer mirrors internalpb.TrustedPeer onto the external
// message PinPeerCertificate/UnpinPeerCertificate carry.
func toRPCTustedPeer(p *internalpb.TrustedPeer) *rpcpb.TrustedPeer {
	if p == nil {
		return nil
	}
	return &rpcpb.TrustedPeer{
		NodeId:       p.GetNodeId(),
		CombName:     p.GetCombName(),
		Fingerprint:  p.GetFingerprint(),
		CertPem:      p.GetCertPem(),
		NotAfterUnix: p.GetNotAfterUnix(),
		PinnedAtUnix: p.GetPinnedAtUnix(),
		PinnedBy:     p.GetPinnedBy(),
		IsVoter:      p.GetIsVoter(),
		// next is the STAGED certificate during an ADR-0170 trust
		// transition. It is carried onto the wire so an operator can
		// SEE that a transition is in flight - a staged certificate
		// that no page renders is a staged certificate nobody knows
		// about.
		Next: toRPCTustedPeer(p.GetNext()),
		// The CA anchor is carried onto the wire so an operator can
		// SEE which CA a Comb is trusted through, and so the trust
		// page can say so rather than showing a leaf that says
		// nothing about what verifies it.
		CaPem:         p.GetCaPem(),
		CaFingerprint: p.GetCaFingerprint(),
	}
}

func toInternalJail(j *rpcpb.JailDefinition) *internalpb.JailDefinition {
	return &internalpb.JailDefinition{
		Id:              j.GetId(),
		Name:            j.GetName(),
		Hostname:        j.GetHostname(),
		NodeId:          j.GetNodeId(),
		ReplicaNodeId:   j.GetReplicaNodeId(),
		DesiredState:    internalpb.JailState(j.GetDesiredState()),
		BaseTemplate:    j.GetBaseTemplate(),
		BaseArchiveName: j.GetBaseArchiveName(),
		NetworkId:       j.GetNetworkId(),
		// RequestedIpAddress is the one address input a caller may give
		// (see JailDefinition's own comment). IpAddress is deliberately
		// not copied: it is the FSM's resolved value, and UpdateJail
		// carries the stored one forward in applyUpdateJail.
		RequestedIpAddress: j.GetRequestedIpAddress(),
		Vnet:               j.GetVnet(),
		FirewallRules:      toInternalFirewallRules(j.GetFirewallRules()),
		// Phase/PhaseError are the reconciler's own observed state, never
		// set by an external caller - CreateJail/UpdateJail requests
		// never carry them through.
	}
}

func fromInternalJail(j *internalpb.JailDefinition) *rpcpb.JailDefinition {
	if j == nil {
		return nil
	}
	rules := make([]*rpcpb.FirewallRule, 0, len(j.GetFirewallRules()))
	for _, r := range j.GetFirewallRules() {
		rules = append(rules, &rpcpb.FirewallRule{
			Direction: r.GetDirection(),
			Action:    r.GetAction(),
			Protocol:  r.GetProtocol(),
			PortRange: r.GetPortRange(),
		})
	}
	return &rpcpb.JailDefinition{
		Id:              j.GetId(),
		Name:            j.GetName(),
		Hostname:        j.GetHostname(),
		NodeId:          j.GetNodeId(),
		ReplicaNodeId:   j.GetReplicaNodeId(),
		DesiredState:    rpcpb.JailState(j.GetDesiredState()),
		Phase:           rpcpb.JailPhase(j.GetPhase()),
		PhaseError:      j.GetPhaseError(),
		BaseTemplate:    j.GetBaseTemplate(),
		BaseArchiveName: j.GetBaseArchiveName(),
		NetworkId:       j.GetNetworkId(),
		IpAddress:       j.GetIpAddress(),
		Vnet:            j.GetVnet(),
		// Non-nil even when empty, so the field marshals as [] rather
		// than null - the same convention the VM conversion above uses,
		// and the same reason: a jail with no rules is a definite
		// "unfiltered", not an absent value.
		FirewallRules: rules,
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
