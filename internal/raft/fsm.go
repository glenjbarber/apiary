package raft

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"sort"
	"sync"
	"time"

	"github.com/hashicorp/raft"
	"google.golang.org/protobuf/proto"

	internalpb "github.com/glenjbarber/apiary/api/internalpb"
)

// FSMApplyResult is returned from Node.Apply, echoing back the FSM's
// response to an applied Command. Error is set (and VM/Network left nil)
// if the command was rejected at the application level (e.g. duplicate/
// missing id) - this is separate from raft-level failures (not leader,
// timeout), which Node.Apply reports as a Go error instead. Exactly one
// of VM/Network is set on success, depending on which kind of command
// was applied.
type FSMApplyResult struct {
	Index              uint64
	VM                 *internalpb.VMDefinition
	Network            *internalpb.NetworkDefinition
	ApiKey             *internalpb.ApiKey
	Jail               *internalpb.JailDefinition
	PendingJoinRequest *internalpb.PendingJoinRequest
	Error              string
}

// FSM applies typed Command messages (see api/internalpb/state.proto)
// against an in-memory map of VM definitions, keyed by ID, plus
// similarly keyed maps of network definitions and API keys. This is
// the real ephemeral-state schema: cluster membership itself is
// handled by raft's own configuration mechanism (AddVoter/
// RemoveServer), not by the FSM.
type FSM struct {
	mu                  sync.Mutex
	lastIndex           uint64
	vms                 map[string]*internalpb.VMDefinition
	networks            map[string]*internalpb.NetworkDefinition
	apiKeys             map[string]*internalpb.ApiKey
	jails               map[string]*internalpb.JailDefinition
	pendingJoinRequests map[string]*internalpb.PendingJoinRequest

	// authEnabled is set permanently, forever, the first time any
	// CreateAPIKey command ever succeeds - it never reverts to false
	// even if every key is later revoked. See AuthEnabled's own doc
	// comment for why this must be a separate, one-way flag rather than
	// just checking len(apiKeys) > 0.
	authEnabled bool
}

var _ raft.FSM = (*FSM)(nil)

// NewFSM returns an empty FSM.
func NewFSM() *FSM {
	return &FSM{
		vms:                 make(map[string]*internalpb.VMDefinition),
		networks:            make(map[string]*internalpb.NetworkDefinition),
		apiKeys:             make(map[string]*internalpb.ApiKey),
		jails:               make(map[string]*internalpb.JailDefinition),
		pendingJoinRequests: make(map[string]*internalpb.PendingJoinRequest),
	}
}

// Apply implements raft.FSM. log.Data must be a marshaled
// api/internalpb.Command; a malformed payload is treated as an
// application-level error (FSMApplyResult.Error), not a panic, since a
// bad payload should never be able to crash the state machine.
func (f *FSM) Apply(log *raft.Log) interface{} {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.lastIndex = log.Index

	var cmd internalpb.Command
	if err := proto.Unmarshal(log.Data, &cmd); err != nil {
		return &FSMApplyResult{Index: log.Index, Error: fmt.Sprintf("invalid command encoding: %v", err)}
	}

	switch op := cmd.GetOp().(type) {
	case *internalpb.Command_CreateVm:
		return f.applyCreateVM(log.Index, op.CreateVm.GetVm())
	case *internalpb.Command_UpdateVm:
		return f.applyUpdateVM(log.Index, op.UpdateVm.GetVm())
	case *internalpb.Command_DeleteVm:
		return f.applyDeleteVM(log.Index, op.DeleteVm.GetId())
	case *internalpb.Command_UpdateVmPhase:
		return f.applyUpdateVMPhase(log.Index, op.UpdateVmPhase)
	case *internalpb.Command_PurgeVm:
		return f.applyPurgeVM(log.Index, op.PurgeVm.GetId())
	case *internalpb.Command_SetVmFirewallPaused:
		return f.applySetVMFirewallPaused(log.Index, op.SetVmFirewallPaused)
	case *internalpb.Command_SetVmCloudflareExposure:
		return f.applySetVMCloudflareExposure(log.Index, op.SetVmCloudflareExposure)
	case *internalpb.Command_SetVmDesiredState:
		return f.applySetVMDesiredState(log.Index, op.SetVmDesiredState)
	case *internalpb.Command_SetVmFirewallRules:
		return f.applySetVMFirewallRules(log.Index, op.SetVmFirewallRules)
	case *internalpb.Command_CreateNetwork:
		return f.applyCreateNetwork(log.Index, op.CreateNetwork.GetNetwork())
	case *internalpb.Command_DeleteNetwork:
		return f.applyDeleteNetwork(log.Index, op.DeleteNetwork.GetId())
	case *internalpb.Command_SetNetworkName:
		return f.applySetNetworkName(log.Index, op.SetNetworkName)
	case *internalpb.Command_CreateApiKey:
		return f.applyCreateAPIKey(log.Index, op.CreateApiKey.GetKey())
	case *internalpb.Command_RevokeApiKey:
		return f.applyRevokeAPIKey(log.Index, op.RevokeApiKey.GetId())
	case *internalpb.Command_CreateJail:
		return f.applyCreateJail(log.Index, op.CreateJail.GetJail())
	case *internalpb.Command_UpdateJail:
		return f.applyUpdateJail(log.Index, op.UpdateJail.GetJail())
	case *internalpb.Command_DeleteJail:
		return f.applyDeleteJail(log.Index, op.DeleteJail.GetId())
	case *internalpb.Command_UpdateJailPhase:
		return f.applyUpdateJailPhase(log.Index, op.UpdateJailPhase)
	case *internalpb.Command_PurgeJail:
		return f.applyPurgeJail(log.Index, op.PurgeJail.GetId())
	case *internalpb.Command_SetJailDesiredState:
		return f.applySetJailDesiredState(log.Index, op.SetJailDesiredState)
	case *internalpb.Command_CreatePendingJoinRequest:
		return f.applyCreatePendingJoinRequest(log.Index, op.CreatePendingJoinRequest.GetRequest())
	case *internalpb.Command_ApprovePendingJoinRequest:
		return f.applyApprovePendingJoinRequest(log.Index, op.ApprovePendingJoinRequest.GetRequestId())
	case *internalpb.Command_RejectPendingJoinRequest:
		return f.applyRejectPendingJoinRequest(log.Index, op.RejectPendingJoinRequest.GetRequestId())
	default:
		return &FSMApplyResult{Index: log.Index, Error: "command has no op set"}
	}
}

// validResourceID reports whether id is safe to interpolate as a bare
// token into a generated configuration file - dnsmasq.conf
// (internal/dhcpd, as a lease hostname) and hast.conf (internal/hast, as
// a resource name) both build config text directly from a VM/jail ID
// with no escaping, on the assumption that an ID is a short, plain
// token. Before this check, that assumption was enforced nowhere: only
// non-emptiness and uniqueness were required here, so any Operator
// could embed a newline in a VM ID and inject an arbitrary dnsmasq/hastd
// directive - dnsmasq's dhcp-script= in particular runs as root on every
// lease event. Mirrors internal/jail's own qualifiedName allowlist
// (alphanumerics, '-', '_'), the one place in this codebase that
// already got this right. Applied only at Create (not Update), since an
// ID is otherwise immutable once assigned.
func validResourceID(id string) bool {
	if id == "" || len(id) > 64 {
		return false
	}
	for _, r := range id {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_') {
			return false
		}
	}
	return true
}

func (f *FSM) applyCreateVM(index uint64, vm *internalpb.VMDefinition) *FSMApplyResult {
	if vm.GetId() == "" {
		return &FSMApplyResult{Index: index, Error: "CreateVM: id must be set"}
	}
	if !validResourceID(vm.GetId()) {
		return &FSMApplyResult{Index: index, Error: fmt.Sprintf("CreateVM: invalid id %q: only alphanumerics, '-', and '_' are allowed (max 64 chars)", vm.GetId())}
	}
	if _, exists := f.vms[vm.GetId()]; exists {
		return &FSMApplyResult{Index: index, Error: fmt.Sprintf("CreateVM: id %q already exists", vm.GetId())}
	}

	vm = proto.Clone(vm).(*internalpb.VMDefinition)
	// MacAddress is derived for every VM, not just ones naming a
	// NetworkDefinition - a flat-bridge VM (no network_id) previously
	// got whatever random MAC bhyve's own virtio-net device generated,
	// making it impossible for an operator to set up a static DHCP
	// reservation on their own router ahead of time. deriveMAC is a
	// pure function of the VM's own id, so this costs nothing and is
	// always safe to compute regardless of networking mode.
	vm.MacAddress = deriveMAC(vm.GetId())

	if vm.GetNetworkId() != "" {
		network, ok := f.networks[vm.GetNetworkId()]
		if !ok {
			return &FSMApplyResult{Index: index, Error: fmt.Sprintf("CreateVM: network %q does not exist", vm.GetNetworkId())}
		}
		ip, err := f.allocateIP(network)
		if err != nil {
			return &FSMApplyResult{Index: index, Error: fmt.Sprintf("CreateVM: %v", err)}
		}
		vm.IpAddress = ip
	}

	f.vms[vm.GetId()] = vm
	return &FSMApplyResult{Index: index, VM: vm}
}

// allocateIP picks the lowest host address in network's subnet not
// already assigned to another VM on this network, skipping the network
// address, the broadcast address, and ".1" (reserved as the bridge's
// own gateway address - see internal/vlan). Deterministic given the
// FSM's already-committed state, so every raft replica computes the
// same result independently - safe under raft's serialized log without
// needing a separate allocation round-trip.
func (f *FSM) allocateIP(network *internalpb.NetworkDefinition) (string, error) {
	_, ipnet, err := net.ParseCIDR(network.GetSubnet())
	if err != nil {
		return "", fmt.Errorf("network %q has an invalid subnet %q: %w", network.GetId(), network.GetSubnet(), err)
	}

	used := make(map[string]bool)
	for _, vm := range f.vms {
		if vm.GetNetworkId() == network.GetId() && vm.GetIpAddress() != "" {
			used[vm.GetIpAddress()] = true
		}
	}

	base := ipnet.IP.To4()
	if base == nil {
		return "", fmt.Errorf("network %q's subnet %q is not IPv4", network.GetId(), network.GetSubnet())
	}
	ones, bits := ipnet.Mask.Size()
	hostBits := bits - ones
	numAddrs := uint32(1) << uint(hostBits)

	baseInt := binary.BigEndian.Uint32(base)
	for host := uint32(1); host < numAddrs-1; host++ { // skip .0 (network) and the last (broadcast)
		if host == 1 {
			continue // reserved for the bridge's own gateway address
		}
		var candidate [4]byte
		binary.BigEndian.PutUint32(candidate[:], baseInt+host)
		ip := net.IP(candidate[:]).String()
		if !used[ip] {
			return ip, nil
		}
	}
	return "", fmt.Errorf("network %q (%s) has no free addresses", network.GetId(), network.GetSubnet())
}

// deriveMAC computes a stable, locally-administered unicast MAC address
// from id - no separate allocation bookkeeping needed (unlike IP
// addresses, which must come from a specific finite subnet), and it's
// stable across reconciler ticks/FSM restarts since it's a pure
// function of the VM's own id.
func deriveMAC(id string) string {
	sum := sha256.Sum256([]byte(id))
	// First octet: clear the multicast bit (bit 0) and set the
	// locally-administered bit (bit 1), per IEEE 802 - marks this as a
	// locally-assigned unicast address, never colliding with a real
	// hardware-assigned MAC.
	b0 := (sum[0] &^ 0x01) | 0x02
	return fmt.Sprintf("%02x:%02x:%02x:%02x:%02x:%02x", b0, sum[1], sum[2], sum[3], sum[4], sum[5])
}

func (f *FSM) applyUpdateVM(index uint64, vm *internalpb.VMDefinition) *FSMApplyResult {
	if _, exists := f.vms[vm.GetId()]; !exists {
		return &FSMApplyResult{Index: index, Error: fmt.Sprintf("UpdateVM: id %q does not exist", vm.GetId())}
	}
	f.vms[vm.GetId()] = vm
	return &FSMApplyResult{Index: index, VM: vm}
}

// applyDeleteVM marks vm for deletion rather than removing it outright,
// unless it was never assigned to a node - with no node_id, no
// reconciler will ever pick it up to tear down real resources (there are
// none) or to purge the tombstone, so removing it immediately is both
// safe and necessary. Otherwise it's soft-deleted (VM_STATE_DELETING);
// the owning node's reconciler tears down its real resources and then
// submits PurgeVM to finish the job. Deleting an already-deleting VM is
// not an error - it's the same request landing twice.
func (f *FSM) applyDeleteVM(index uint64, id string) *FSMApplyResult {
	vm, exists := f.vms[id]
	if !exists {
		return &FSMApplyResult{Index: index, Error: fmt.Sprintf("DeleteVM: id %q does not exist", id)}
	}
	if vm.GetNodeId() == "" {
		delete(f.vms, id)
		return &FSMApplyResult{Index: index, VM: vm}
	}
	updated := proto.Clone(vm).(*internalpb.VMDefinition)
	updated.DesiredState = internalpb.VMState_VM_STATE_DELETING
	f.vms[id] = updated
	return &FSMApplyResult{Index: index, VM: updated}
}

// applyUpdateVMPhase records reconciliation progress against an existing
// VM. It never touches desired_state. A missing id is reported as an
// error but is not a bug - it can happen if a stale reconcile attempt's
// phase update loses a race against that same VM being purged.
func (f *FSM) applyUpdateVMPhase(index uint64, upd *internalpb.UpdateVMPhase) *FSMApplyResult {
	vm, exists := f.vms[upd.GetId()]
	if !exists {
		return &FSMApplyResult{Index: index, Error: fmt.Sprintf("UpdateVMPhase: id %q does not exist", upd.GetId())}
	}
	updated := proto.Clone(vm).(*internalpb.VMDefinition)
	updated.Phase = upd.GetPhase()
	updated.PhaseError = upd.GetPhaseError()
	f.vms[upd.GetId()] = updated
	return &FSMApplyResult{Index: index, VM: updated}
}

// applySetVMFirewallPaused toggles firewall_paused on an existing VM,
// touching no other field - deliberately narrow, unlike applyUpdateVM's
// full-replace semantics, since a caller here is only ever intending to
// change this one flag (see ADR-0049).
func (f *FSM) applySetVMFirewallPaused(index uint64, req *internalpb.SetVMFirewallPaused) *FSMApplyResult {
	vm, exists := f.vms[req.GetId()]
	if !exists {
		return &FSMApplyResult{Index: index, Error: fmt.Sprintf("SetVMFirewallPaused: id %q does not exist", req.GetId())}
	}
	updated := proto.Clone(vm).(*internalpb.VMDefinition)
	updated.FirewallPaused = req.GetPaused()
	f.vms[req.GetId()] = updated
	return &FSMApplyResult{Index: index, VM: updated}
}

// applySetVMCloudflareExposure sets cloudflare_hostname/cloudflare_port
// on an existing VM, touching no other field - the same narrow,
// deliberately-not-UpdateVM shape as applySetVMFirewallPaused (see
// ADR-0063). Cross-field validation (hostname requires network_id) is
// the RPC handler's job, not the FSM's - matching this command's own
// division of labor with every other narrow Set* command.
func (f *FSM) applySetVMCloudflareExposure(index uint64, req *internalpb.SetVMCloudflareExposure) *FSMApplyResult {
	vm, exists := f.vms[req.GetId()]
	if !exists {
		return &FSMApplyResult{Index: index, Error: fmt.Sprintf("SetVMCloudflareExposure: id %q does not exist", req.GetId())}
	}
	// Hostname is rendered verbatim into cloudflared's generated YAML
	// config (internal/cloudflare.RenderConfig) with no escaping - see
	// validResourceID's rationale above for the same class of bug this
	// closes (a newline here would inject an arbitrary ingress rule).
	if req.GetHostname() != "" && !validHostname(req.GetHostname()) {
		return &FSMApplyResult{Index: index, Error: fmt.Sprintf("SetVMCloudflareExposure: invalid hostname %q: only alphanumerics, '-', and '.' are allowed (max 255 chars)", req.GetHostname())}
	}
	updated := proto.Clone(vm).(*internalpb.VMDefinition)
	updated.CloudflareHostname = req.GetHostname()
	updated.CloudflarePort = req.GetPort()
	f.vms[req.GetId()] = updated
	return &FSMApplyResult{Index: index, VM: updated}
}

// applySetVMDesiredState changes only desired_state. Deletion is kept on
// DeleteVM's separate tombstone path so a lifecycle control cannot discard a
// Cell's storage or record.
func (f *FSM) applySetVMDesiredState(index uint64, req *internalpb.SetVMDesiredState) *FSMApplyResult {
	vm, exists := f.vms[req.GetId()]
	if !exists {
		return &FSMApplyResult{Index: index, Error: fmt.Sprintf("SetVMDesiredState: id %q does not exist", req.GetId())}
	}
	if vm.GetDesiredState() == internalpb.VMState_VM_STATE_DELETING {
		return &FSMApplyResult{Index: index, Error: fmt.Sprintf("SetVMDesiredState: VM %q is marked for deletion", req.GetId())}
	}
	switch req.GetDesiredState() {
	case internalpb.VMState_VM_STATE_STOPPED, internalpb.VMState_VM_STATE_RUNNING, internalpb.VMState_VM_STATE_RESTARTING:
	default:
		return &FSMApplyResult{Index: index, Error: "SetVMDesiredState: desired_state must be stopped, running, or restarting"}
	}
	updated := proto.Clone(vm).(*internalpb.VMDefinition)
	updated.DesiredState = req.GetDesiredState()
	f.vms[req.GetId()] = updated
	return &FSMApplyResult{Index: index, VM: updated}
}

// applySetVMFirewallRules replaces firewall_rules wholesale on an
// existing VM, touching no other field - the same narrow,
// deliberately-not-UpdateVM shape as applySetVMFirewallPaused above.
// No field-level validation here, matching applyCreateVM's own
// division of labor: nothing validates individual rule contents at
// creation time either, so this doesn't hold edits to a stricter
// standard than creation.
func (f *FSM) applySetVMFirewallRules(index uint64, req *internalpb.SetVMFirewallRules) *FSMApplyResult {
	vm, exists := f.vms[req.GetId()]
	if !exists {
		return &FSMApplyResult{Index: index, Error: fmt.Sprintf("SetVMFirewallRules: id %q does not exist", req.GetId())}
	}
	updated := proto.Clone(vm).(*internalpb.VMDefinition)
	updated.FirewallRules = req.GetFirewallRules()
	f.vms[req.GetId()] = updated
	return &FSMApplyResult{Index: index, VM: updated}
}

// applyPurgeVM removes a VM definition outright. Idempotent: purging an
// id that's already gone is not an error, since the reconciler that
// submits this may retry after a partial failure (e.g. it purged
// successfully but never saw the response).
func (f *FSM) applyPurgeVM(index uint64, id string) *FSMApplyResult {
	vm := f.vms[id]
	delete(f.vms, id)
	return &FSMApplyResult{Index: index, VM: vm}
}

// applyCreateJail adds a new JailDefinition, mirroring applyCreateVM
// (minus the network-allocation step, since jails have no equivalent
// yet - see JailDefinition's doc comment).
func (f *FSM) applyCreateJail(index uint64, jail *internalpb.JailDefinition) *FSMApplyResult {
	if jail.GetId() == "" {
		return &FSMApplyResult{Index: index, Error: "CreateJail: id must be set"}
	}
	if !validResourceID(jail.GetId()) {
		return &FSMApplyResult{Index: index, Error: fmt.Sprintf("CreateJail: invalid id %q: only alphanumerics, '-', and '_' are allowed (max 64 chars)", jail.GetId())}
	}
	if _, exists := f.jails[jail.GetId()]; exists {
		return &FSMApplyResult{Index: index, Error: fmt.Sprintf("CreateJail: id %q already exists", jail.GetId())}
	}
	f.jails[jail.GetId()] = jail
	return &FSMApplyResult{Index: index, Jail: jail}
}

func (f *FSM) applyUpdateJail(index uint64, jail *internalpb.JailDefinition) *FSMApplyResult {
	if _, exists := f.jails[jail.GetId()]; !exists {
		return &FSMApplyResult{Index: index, Error: fmt.Sprintf("UpdateJail: id %q does not exist", jail.GetId())}
	}
	f.jails[jail.GetId()] = jail
	return &FSMApplyResult{Index: index, Jail: jail}
}

// applyDeleteJail mirrors applyDeleteVM exactly: soft-delete
// (JAIL_STATE_DELETING) when a node_id is assigned (a reconciler needs
// to tear down real resources first), immediate removal otherwise.
func (f *FSM) applyDeleteJail(index uint64, id string) *FSMApplyResult {
	jail, exists := f.jails[id]
	if !exists {
		return &FSMApplyResult{Index: index, Error: fmt.Sprintf("DeleteJail: id %q does not exist", id)}
	}
	if jail.GetNodeId() == "" {
		delete(f.jails, id)
		return &FSMApplyResult{Index: index, Jail: jail}
	}
	updated := proto.Clone(jail).(*internalpb.JailDefinition)
	updated.DesiredState = internalpb.JailState_JAIL_STATE_DELETING
	f.jails[id] = updated
	return &FSMApplyResult{Index: index, Jail: updated}
}

// applyUpdateJailPhase mirrors applyUpdateVMPhase exactly.
func (f *FSM) applyUpdateJailPhase(index uint64, upd *internalpb.UpdateJailPhase) *FSMApplyResult {
	jail, exists := f.jails[upd.GetId()]
	if !exists {
		return &FSMApplyResult{Index: index, Error: fmt.Sprintf("UpdateJailPhase: id %q does not exist", upd.GetId())}
	}
	updated := proto.Clone(jail).(*internalpb.JailDefinition)
	updated.Phase = upd.GetPhase()
	updated.PhaseError = upd.GetPhaseError()
	f.jails[upd.GetId()] = updated
	return &FSMApplyResult{Index: index, Jail: updated}
}

// applySetJailDesiredState mirrors applySetVMDesiredState and preserves all
// jail configuration and storage intent while changing only lifecycle state.
func (f *FSM) applySetJailDesiredState(index uint64, req *internalpb.SetJailDesiredState) *FSMApplyResult {
	jail, exists := f.jails[req.GetId()]
	if !exists {
		return &FSMApplyResult{Index: index, Error: fmt.Sprintf("SetJailDesiredState: id %q does not exist", req.GetId())}
	}
	if jail.GetDesiredState() == internalpb.JailState_JAIL_STATE_DELETING {
		return &FSMApplyResult{Index: index, Error: fmt.Sprintf("SetJailDesiredState: jail %q is marked for deletion", req.GetId())}
	}
	switch req.GetDesiredState() {
	case internalpb.JailState_JAIL_STATE_STOPPED, internalpb.JailState_JAIL_STATE_RUNNING, internalpb.JailState_JAIL_STATE_RESTARTING:
	default:
		return &FSMApplyResult{Index: index, Error: "SetJailDesiredState: desired_state must be stopped, running, or restarting"}
	}
	updated := proto.Clone(jail).(*internalpb.JailDefinition)
	updated.DesiredState = req.GetDesiredState()
	f.jails[req.GetId()] = updated
	return &FSMApplyResult{Index: index, Jail: updated}
}

// applyPurgeJail mirrors applyPurgeVM exactly: idempotent, not an
// error if id is already gone.
func (f *FSM) applyPurgeJail(index uint64, id string) *FSMApplyResult {
	jail := f.jails[id]
	delete(f.jails, id)
	return &FSMApplyResult{Index: index, Jail: jail}
}

// applyCreatePendingJoinRequest records a new Colony-join request
// (ADR-0083). Rejected on a request_id collision (treated as a real
// error, not silently overwritten - the same posture CreateVM/CreateJail
// already take on duplicate ids) and on an obviously-malformed request
// (empty node_id/raft_bind_address/code), so a bad RequestJoinColony
// call fails loudly at the FSM layer, not just the RPC layer - every
// raft follower applying this same log entry must reach the identical
// decision.
func (f *FSM) applyCreatePendingJoinRequest(index uint64, req *internalpb.PendingJoinRequest) *FSMApplyResult {
	if req.GetRequestId() == "" || req.GetNodeId() == "" || req.GetRaftBindAddress() == "" || req.GetCode() == "" {
		return &FSMApplyResult{Index: index, Error: "CreatePendingJoinRequest: request_id, node_id, raft_bind_address, and code must all be set"}
	}
	if _, exists := f.pendingJoinRequests[req.GetRequestId()]; exists {
		return &FSMApplyResult{Index: index, Error: fmt.Sprintf("CreatePendingJoinRequest: request_id %q already exists", req.GetRequestId())}
	}
	f.pendingJoinRequests[req.GetRequestId()] = req
	return &FSMApplyResult{Index: index, PendingJoinRequest: req}
}

// applyApprovePendingJoinRequest and applyRejectPendingJoinRequest both
// require the request to currently be Pending and not yet expired - an
// Admin approving/rejecting a request that some other Admin (on a
// different Colony member's UI, since this is raft-replicated) already
// resolved, or that expired in the meantime, is a real error, not a
// silent no-op, so a stale browser tab's second click surfaces clearly
// rather than double-applying.
func (f *FSM) applyApprovePendingJoinRequest(index uint64, requestID string) *FSMApplyResult {
	return f.applyResolvePendingJoinRequest(index, requestID, internalpb.JoinRequestStatus_JOIN_REQUEST_STATUS_APPROVED, "ApprovePendingJoinRequest")
}

func (f *FSM) applyRejectPendingJoinRequest(index uint64, requestID string) *FSMApplyResult {
	return f.applyResolvePendingJoinRequest(index, requestID, internalpb.JoinRequestStatus_JOIN_REQUEST_STATUS_REJECTED, "RejectPendingJoinRequest")
}

func (f *FSM) applyResolvePendingJoinRequest(index uint64, requestID string, status internalpb.JoinRequestStatus, opName string) *FSMApplyResult {
	req, exists := f.pendingJoinRequests[requestID]
	if !exists {
		return &FSMApplyResult{Index: index, Error: fmt.Sprintf("%s: request_id %q does not exist", opName, requestID)}
	}
	if req.GetStatus() != internalpb.JoinRequestStatus_JOIN_REQUEST_STATUS_PENDING {
		return &FSMApplyResult{Index: index, Error: fmt.Sprintf("%s: request_id %q is already %s", opName, requestID, req.GetStatus())}
	}
	if pendingJoinRequestExpired(req) {
		return &FSMApplyResult{Index: index, Error: fmt.Sprintf("%s: request_id %q has expired", opName, requestID)}
	}
	updated := proto.Clone(req).(*internalpb.PendingJoinRequest)
	updated.Status = status
	f.pendingJoinRequests[requestID] = updated
	return &FSMApplyResult{Index: index, PendingJoinRequest: updated}
}

// pendingJoinRequestExpired checks expiry lazily, at read/apply time -
// this codebase's one existing precedent (the Assumption Register's own
// ExpiresAt handling) works the same way, and nothing anywhere in it
// runs a background sweep/purge goroutine.
func pendingJoinRequestExpired(req *internalpb.PendingJoinRequest) bool {
	return time.Now().Unix() >= req.GetExpiresAtUnix()
}

// PendingJoinRequest returns the current record for requestID (whatever
// its status), and whether it exists - deliberately not filtering out
// expired/resolved requests, unlike ListPendingJoinRequests below, since
// a joining Comb's own GetJoinRequestStatus poll needs to see a
// terminal Approved/Rejected outcome, or a genuine expiry, not just
// silence.
func (f *FSM) PendingJoinRequest(requestID string) (*internalpb.PendingJoinRequest, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	req, ok := f.pendingJoinRequests[requestID]
	return req, ok
}

// ListPendingJoinRequests returns every currently-Pending, not-yet-
// expired request, sorted by request_id for stable ordering - the list
// an Admin reviews to approve/reject. Resolved (Approved/Rejected) or
// expired requests are excluded here (though never deleted - see
// PendingJoinRequest above) since they're no longer actionable.
func (f *FSM) ListPendingJoinRequests() []*internalpb.PendingJoinRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	requests := make([]*internalpb.PendingJoinRequest, 0, len(f.pendingJoinRequests))
	for _, req := range f.pendingJoinRequests {
		if req.GetStatus() != internalpb.JoinRequestStatus_JOIN_REQUEST_STATUS_PENDING {
			continue
		}
		if pendingJoinRequestExpired(req) {
			continue
		}
		requests = append(requests, req)
	}
	sort.Slice(requests, func(i, j int) bool { return requests[i].GetRequestId() < requests[j].GetRequestId() })
	return requests
}

// Jail returns the current definition for id, and whether it exists.
func (f *FSM) Jail(id string) (*internalpb.JailDefinition, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	jail, ok := f.jails[id]
	return jail, ok
}

// ListJails returns every current jail definition.
func (f *FSM) ListJails() []*internalpb.JailDefinition {
	f.mu.Lock()
	defer f.mu.Unlock()
	jails := make([]*internalpb.JailDefinition, 0, len(f.jails))
	for _, j := range f.jails {
		jails = append(jails, j)
	}
	return jails
}

// applyCreateNetwork adds a new NetworkDefinition.
func (f *FSM) applyCreateNetwork(index uint64, network *internalpb.NetworkDefinition) *FSMApplyResult {
	if network.GetId() == "" {
		return &FSMApplyResult{Index: index, Error: "CreateNetwork: id must be set"}
	}
	if !validResourceID(network.GetId()) {
		return &FSMApplyResult{Index: index, Error: fmt.Sprintf("CreateNetwork: invalid id %q: only alphanumerics, '-', and '_' are allowed (max 64 chars)", network.GetId())}
	}
	if _, exists := f.networks[network.GetId()]; exists {
		return &FSMApplyResult{Index: index, Error: fmt.Sprintf("CreateNetwork: id %q already exists", network.GetId())}
	}
	if _, _, err := net.ParseCIDR(network.GetSubnet()); err != nil {
		return &FSMApplyResult{Index: index, Error: fmt.Sprintf("CreateNetwork: invalid subnet %q: %v", network.GetSubnet(), err)}
	}
	// bridge_name and external_gateway are both rendered verbatim into
	// generated dnsmasq.conf (internal/dhcpd.RenderConfig, as an
	// interface= line and a dhcp-option=...,3,<gateway> line
	// respectively) - validated here for the same reason validResourceID
	// exists: an unvalidated newline in either previously let an
	// Operator inject arbitrary dnsmasq directives. bridge_name doubles
	// as a real FreeBSD interface name (see ADR-0022's own 15-usable-
	// character discovery), so it gets the stricter interface-name check
	// rather than validResourceID's 64-char id allowance.
	if network.GetBridgeName() != "" && !validInterfaceName(network.GetBridgeName()) {
		return &FSMApplyResult{Index: index, Error: fmt.Sprintf("CreateNetwork: invalid bridge_name %q: must be a plain interface name (alphanumerics and '-', max 15 chars)", network.GetBridgeName())}
	}
	if network.GetExternalGateway() != "" && net.ParseIP(network.GetExternalGateway()) == nil {
		return &FSMApplyResult{Index: index, Error: fmt.Sprintf("CreateNetwork: invalid external_gateway %q: must be a plain IP address", network.GetExternalGateway())}
	}
	f.networks[network.GetId()] = network
	return &FSMApplyResult{Index: index, Network: network}
}

// validInterfaceName reports whether name is safe to both interpolate
// into generated configuration (dnsmasq.conf's interface=/pf's nat-to)
// and pass to ifconfig(8)/pfctl(8) as a literal FreeBSD interface name.
// FreeBSD interface names are null-padded into a 16-byte kernel buffer,
// leaving 15 usable characters - see ADR-0022's own real discovery of
// this limit, previously enforced only by luck for names Apiary
// generates itself (e.g. "apnet-<8 hex chars>"), never for an operator-
// supplied bridge_name or node-config uplink value.
func validInterfaceName(name string) bool {
	if name == "" || len(name) > 15 {
		return false
	}
	for _, r := range name {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-') {
			return false
		}
	}
	return true
}

// validHostname reports whether s is safe to interpolate as a bare
// hostname into cloudflared's generated YAML config
// (internal/cloudflare.RenderConfig) - the same interpolation-safety
// concern validResourceID/validInterfaceName exist for, not a real DNS
// validity check (a hostname that fails real DNS resolution just fails
// later, harmlessly, when cloudflared can't route it).
func validHostname(s string) bool {
	if s == "" || len(s) > 255 {
		return false
	}
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '.') {
			return false
		}
	}
	return true
}

// applyDeleteNetwork removes a NetworkDefinition outright - no soft-
// delete tombstone, unlike DeleteVM, since a network has no physical
// resources of its own to reconcile away first (see NetworkDefinition's
// doc comment). Rejected if any VM still references it, or if it
// doesn't exist - no cascade/orphan-reclaim, matching the same caution
// already accepted for VM deletion (ADR-0016).
func (f *FSM) applyDeleteNetwork(index uint64, id string) *FSMApplyResult {
	network, exists := f.networks[id]
	if !exists {
		return &FSMApplyResult{Index: index, Error: fmt.Sprintf("DeleteNetwork: id %q does not exist", id)}
	}
	for _, vm := range f.vms {
		if vm.GetNetworkId() == id {
			return &FSMApplyResult{Index: index, Error: fmt.Sprintf("DeleteNetwork: still referenced by VM %q", vm.GetId())}
		}
	}
	delete(f.networks, id)
	return &FSMApplyResult{Index: index, Network: network}
}

// applySetNetworkName renames an existing network, touching no other
// field - the only mutation a NetworkDefinition supports after
// creation (ADR-0071/ADR-0080).
func (f *FSM) applySetNetworkName(index uint64, req *internalpb.SetNetworkName) *FSMApplyResult {
	network, exists := f.networks[req.GetId()]
	if !exists {
		return &FSMApplyResult{Index: index, Error: fmt.Sprintf("SetNetworkName: id %q does not exist", req.GetId())}
	}
	updated := proto.Clone(network).(*internalpb.NetworkDefinition)
	updated.Name = req.GetName()
	f.networks[req.GetId()] = updated
	return &FSMApplyResult{Index: index, Network: updated}
}

// Network returns the current definition for id, and whether it exists.
func (f *FSM) Network(id string) (*internalpb.NetworkDefinition, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	network, ok := f.networks[id]
	return network, ok
}

// ListNetworks returns every current network definition, sorted by id
// for stable ordering (this map, like vms, has no inherent order).
func (f *FSM) ListNetworks() []*internalpb.NetworkDefinition {
	f.mu.Lock()
	defer f.mu.Unlock()
	networks := make([]*internalpb.NetworkDefinition, 0, len(f.networks))
	for _, n := range f.networks {
		networks = append(networks, n)
	}
	sort.Slice(networks, func(i, j int) bool { return networks[i].GetId() < networks[j].GetId() })
	return networks
}

// applyCreateAPIKey adds a new ApiKey. managerd's CreateAPIKey RPC
// handler is what actually generates the raw key and computes
// key.HashedKey before submitting this - the FSM only stores what it's
// given, the same as every other Create* command.
func (f *FSM) applyCreateAPIKey(index uint64, key *internalpb.ApiKey) *FSMApplyResult {
	if key.GetId() == "" {
		return &FSMApplyResult{Index: index, Error: "CreateAPIKey: id must be set"}
	}
	if _, exists := f.apiKeys[key.GetId()]; exists {
		return &FSMApplyResult{Index: index, Error: fmt.Sprintf("CreateAPIKey: id %q already exists", key.GetId())}
	}
	if normalized := normalizeRole(key.GetRole()); normalized != key.GetRole() {
		key = proto.Clone(key).(*internalpb.ApiKey)
		key.Role = normalized
	}
	f.apiKeys[key.GetId()] = key
	f.authEnabled = true
	return &FSMApplyResult{Index: index, ApiKey: key}
}

// normalizeRole maps an empty or unrecognized role to "viewer" (least
// privilege, ADR-0030) - a key's role is never treated as "no
// restriction" just because a caller left it unset or misspelled it.
func normalizeRole(role string) string {
	switch role {
	case "admin", "operator", "viewer":
		return role
	default:
		return "viewer"
	}
}

// applyRevokeAPIKey removes an ApiKey outright - no soft-delete
// tombstone, the same reasoning as applyDeleteNetwork (a key has no
// physical resource to reconcile away first).
func (f *FSM) applyRevokeAPIKey(index uint64, id string) *FSMApplyResult {
	key, exists := f.apiKeys[id]
	if !exists {
		return &FSMApplyResult{Index: index, Error: fmt.Sprintf("RevokeAPIKey: id %q does not exist", id)}
	}
	delete(f.apiKeys, id)
	return &FSMApplyResult{Index: index, ApiKey: key}
}

// ValidateHash reports whether hash matches a currently valid (not
// revoked) API key's HashedKey, and that key's id if so. Unlike VM/
// Network/ListAPIKeys reads, callers of this (via Node.
// ValidateAPIKeyHash) do NOT require raft leadership - see that
// method's doc comment for why.
func (f *FSM) ValidateHash(hash string) (id, role string, ok bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, k := range f.apiKeys {
		if k.GetHashedKey() == hash {
			return k.GetId(), k.GetRole(), true
		}
	}
	return "", "", false
}

// AuthEnabled reports whether API-key auth has ever been turned on -
// true forever from the moment the first CreateAPIKey command ever
// succeeds, even if every key is later revoked. This is deliberately
// NOT len(apiKeys) > 0: revoking the last remaining key must lock the
// cluster down (require a new key be created via some other already-
// authenticated path, or a raft snapshot restore), not silently reopen
// it - see ADR-0023 and its "no way back to open" consequence. Same
// no-leadership-required reasoning as ValidateHash.
func (f *FSM) AuthEnabled() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.authEnabled
}

// ListAPIKeys returns every current API key, sorted by id for stable
// ordering - used only for the admin-facing list view (via the
// leader-only Node.ListAPIKeys), never for per-request authentication.
func (f *FSM) ListAPIKeys() []*internalpb.ApiKey {
	f.mu.Lock()
	defer f.mu.Unlock()
	keys := make([]*internalpb.ApiKey, 0, len(f.apiKeys))
	for _, k := range f.apiKeys {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i].GetId() < keys[j].GetId() })
	return keys
}

// AppliedIndex returns the index of the most recently applied log entry.
func (f *FSM) AppliedIndex() uint64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastIndex
}

// VM returns the current definition for id, and whether it exists.
func (f *FSM) VM(id string) (*internalpb.VMDefinition, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	vm, ok := f.vms[id]
	return vm, ok
}

// ListVMs returns every current VM definition.
func (f *FSM) ListVMs() []*internalpb.VMDefinition {
	f.mu.Lock()
	defer f.mu.Unlock()
	vms := make([]*internalpb.VMDefinition, 0, len(f.vms))
	for _, vm := range f.vms {
		vms = append(vms, vm)
	}
	return vms
}

// Snapshot implements raft.FSM.
func (f *FSM) Snapshot() (raft.FSMSnapshot, error) {
	return &fsmSnapshot{state: f.SnapshotState()}, nil
}

// SnapshotState returns a deep-enough copy of the FSM's full ephemeral
// state (every VM/network/API-key/jail record, plus last_index and
// auth_enabled) as the same internalpb.FSMSnapshotState message
// Snapshot/Persist already use for raft's own periodic on-disk
// snapshots. Exported so raftd's -export CLI mode (via the
// ExportState RPC, docs/adr/0051-raftd-config-save-restore.md) can
// read this node's current, live state on demand - raft's own
// snapshot store only updates periodically (raft.DefaultConfig's
// SnapshotInterval/SnapshotThreshold), so it can't answer "what does
// this node have right now."
func (f *FSM) SnapshotState() *internalpb.FSMSnapshotState {
	f.mu.Lock()
	defer f.mu.Unlock()

	state := &internalpb.FSMSnapshotState{
		LastIndex:           f.lastIndex,
		Vms:                 make(map[string]*internalpb.VMDefinition, len(f.vms)),
		Networks:            make(map[string]*internalpb.NetworkDefinition, len(f.networks)),
		ApiKeys:             make(map[string]*internalpb.ApiKey, len(f.apiKeys)),
		Jails:               make(map[string]*internalpb.JailDefinition, len(f.jails)),
		PendingJoinRequests: make(map[string]*internalpb.PendingJoinRequest, len(f.pendingJoinRequests)),
		AuthEnabled:         f.authEnabled,
	}
	for id, vm := range f.vms {
		state.Vms[id] = vm
	}
	for id, network := range f.networks {
		state.Networks[id] = network
	}
	for id, key := range f.apiKeys {
		state.ApiKeys[id] = key
	}
	for id, jail := range f.jails {
		state.Jails[id] = jail
	}
	for id, req := range f.pendingJoinRequests {
		state.PendingJoinRequests[id] = req
	}
	return state
}

// Restore implements raft.FSM.
func (f *FSM) Restore(rc io.ReadCloser) error {
	defer rc.Close()

	data, err := io.ReadAll(rc)
	if err != nil {
		return err
	}

	var state internalpb.FSMSnapshotState
	if err := proto.Unmarshal(data, &state); err != nil {
		return err
	}

	f.mu.Lock()
	f.lastIndex = state.GetLastIndex()
	f.vms = state.GetVms()
	if f.vms == nil {
		f.vms = make(map[string]*internalpb.VMDefinition)
	}
	f.networks = state.GetNetworks()
	if f.networks == nil {
		f.networks = make(map[string]*internalpb.NetworkDefinition)
	}
	f.apiKeys = state.GetApiKeys()
	if f.apiKeys == nil {
		f.apiKeys = make(map[string]*internalpb.ApiKey)
	}
	f.jails = state.GetJails()
	if f.jails == nil {
		f.jails = make(map[string]*internalpb.JailDefinition)
	}
	f.pendingJoinRequests = state.GetPendingJoinRequests()
	if f.pendingJoinRequests == nil {
		f.pendingJoinRequests = make(map[string]*internalpb.PendingJoinRequest)
	}
	f.authEnabled = state.GetAuthEnabled()
	f.mu.Unlock()
	return nil
}

// fsmSnapshot implements raft.FSMSnapshot by marshaling
// internalpb.FSMSnapshotState.
type fsmSnapshot struct {
	state *internalpb.FSMSnapshotState
}

var _ raft.FSMSnapshot = (*fsmSnapshot)(nil)

func (s *fsmSnapshot) Persist(sink raft.SnapshotSink) error {
	data, err := proto.Marshal(s.state)
	if err != nil {
		sink.Cancel()
		return err
	}
	if _, err := sink.Write(data); err != nil {
		sink.Cancel()
		return err
	}
	return sink.Close()
}

func (s *fsmSnapshot) Release() {}
