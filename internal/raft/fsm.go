package raft

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/hashicorp/raft"
	"google.golang.org/protobuf/proto"

	internalpb "github.com/glenjbarber/apiary/api/internalpb"
	"github.com/glenjbarber/apiary/internal/statedigest"
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
	RestartLease       *internalpb.RestartLease
	RestartRecord      *internalpb.RestartRecord
	ColonyUpdate       *internalpb.ColonyUpdate
	ColonyJoinWindow   *internalpb.ColonyJoinWindow
	TrustedPeer        *internalpb.TrustedPeer
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

	// restartLeases/restartRecords back the action-preflight restart
	// guardrail (ADR-0103) - see RestartLease/RestartRecord's own doc
	// comments in api/internalpb/state.proto.
	restartLeases  map[string]*internalpb.RestartLease
	restartRecords map[string]*internalpb.RestartRecord

	// colonyUpdates is ADR-0145's colony-wide controlled-update
	// single-flight AND its durable operation history, in one map keyed
	// by operation id. At most one entry has active = true; settled
	// entries are retained, bounded by maxSettledColonyUpdates - see
	// internal/raft/colonyupdate.go for why the lock and the progress
	// record are deliberately the same object rather than two things
	// that could disagree.
	colonyUpdates map[string]*internalpb.ColonyUpdate

	// colonyJoinWindow is ADR-0147 Part 4's single Colony-wide join
	// window. nil until one is first opened, which is the same thing as
	// "closed" to every caller - see internal/raft/colonyjoinwindow.go,
	// which owns the semantics and the reasoning.
	colonyJoinWindow *internalpb.ColonyJoinWindow

	// trustedPeers is ADR-0147 Part 4's replicated peer trust store,
	// keyed by TrustedPeer.node_id. Replicated for the same reason
	// colonyJoinWindow above is: a pin held in one managerd's memory is
	// a pin that vanishes when that Comb steps aside, which would make
	// "which certificate does this Colony trust" change with leadership
	// for no reason an operator did anything. See internal/raft/peers.go
	// for the pin lifecycle and why it is kept separate from raft
	// membership.
	trustedPeers map[string]*internalpb.TrustedPeer

	// authEnabled is set permanently, forever, the first time any
	// CreateAPIKey command ever succeeds - it never reverts to false
	// even if every key is later revoked. See AuthEnabled's own doc
	// comment for why this must be a separate, one-way flag rather than
	// just checking len(apiKeys) > 0.
	authEnabled bool

	// stateDigest caches the canonical digest of everything above, for
	// ADR-0143's cross-voter comparison. It is recomputed on every Apply
	// and on Restore rather than computed per Status call, because
	// Status is on the health-check and colony-view path and
	// serialising the whole state machine on every call would turn a
	// diagnostic into a load problem. Read it only through StateDigest.
	stateDigest string
}

var _ raft.FSM = (*FSM)(nil)

// NewFSM returns an empty FSM.
func NewFSM() *FSM {
	f := &FSM{
		vms:                 make(map[string]*internalpb.VMDefinition),
		networks:            make(map[string]*internalpb.NetworkDefinition),
		apiKeys:             make(map[string]*internalpb.ApiKey),
		jails:               make(map[string]*internalpb.JailDefinition),
		pendingJoinRequests: make(map[string]*internalpb.PendingJoinRequest),
		restartLeases:       make(map[string]*internalpb.RestartLease),
		restartRecords:      make(map[string]*internalpb.RestartRecord),
		colonyUpdates:       make(map[string]*internalpb.ColonyUpdate),
		trustedPeers:        make(map[string]*internalpb.TrustedPeer),
	}
	// Seed the digest of the empty state. No lock is taken because the
	// FSM has not been handed to anyone yet; recomputeStateDigestLocked
	// is called from Apply and Restore with the lock held, which is the
	// only other place it may run.
	f.recomputeStateDigestLocked()
	return f
}

// Apply implements raft.FSM. log.Data must be a marshaled
// api/internalpb.Command; a malformed payload is treated as an
// application-level error (FSMApplyResult.Error), not a panic, since a
// bad payload should never be able to crash the state machine.
func (f *FSM) Apply(log *raft.Log) interface{} {
	f.mu.Lock()
	defer f.mu.Unlock()

	// Registered after the unlock above, so it runs BEFORE it: the
	// digest is recomputed under the same lock that just applied the
	// command, and no reader can observe a state whose digest predates
	// it. lastIndex is deliberately not part of the digest, so setting
	// it first does not perturb the result.
	defer f.recomputeStateDigestLocked()

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
	case *internalpb.Command_SetJailHostname:
		return f.applySetJailHostname(log.Index, op.SetJailHostname)
	case *internalpb.Command_CreatePendingJoinRequest:
		return f.applyCreatePendingJoinRequest(log.Index, op.CreatePendingJoinRequest.GetRequest())
	case *internalpb.Command_RecordJoinApproval:
		return f.applyRecordJoinApproval(log.Index, op.RecordJoinApproval)
	case *internalpb.Command_ApprovePendingJoinRequest:
		return f.applyApprovePendingJoinRequest(log.Index, op.ApprovePendingJoinRequest)
	case *internalpb.Command_RejectPendingJoinRequest:
		return f.applyRejectPendingJoinRequest(log.Index, op.RejectPendingJoinRequest.GetRequestId())
	case *internalpb.Command_CancelPendingJoinRequest:
		return f.applyCancelPendingJoinRequest(log.Index, op.CancelPendingJoinRequest.GetRequestId())
	case *internalpb.Command_PurgeJoinRequest:
		return f.applyPurgeJoinRequest(log.Index, op.PurgeJoinRequest.GetRequestId())
	case *internalpb.Command_AcquireRestartLease:
		return f.applyAcquireRestartLease(log.Index, op.AcquireRestartLease)
	case *internalpb.Command_RecordRestartCompleted:
		return f.applyRecordRestartCompleted(log.Index, op.RecordRestartCompleted)
	case *internalpb.Command_AcquireColonyUpdate:
		return f.applyAcquireColonyUpdate(log.Index, op.AcquireColonyUpdate)
	case *internalpb.Command_AdvanceColonyUpdate:
		return f.applyAdvanceColonyUpdate(log.Index, op.AdvanceColonyUpdate)
	case *internalpb.Command_ReleaseColonyUpdate:
		return f.applyReleaseColonyUpdate(log.Index, op.ReleaseColonyUpdate)
	case *internalpb.Command_HandoverColonyUpdate:
		return f.applyHandoverColonyUpdate(log.Index, op.HandoverColonyUpdate)
	case *internalpb.Command_OpenColonyJoinWindow:
		return f.applyOpenColonyJoinWindow(log.Index, op.OpenColonyJoinWindow)
	case *internalpb.Command_CloseColonyJoinWindow:
		return f.applyCloseColonyJoinWindow(log.Index, op.CloseColonyJoinWindow)
	case *internalpb.Command_PinTrustedPeer:
		return f.applyPinTrustedPeer(log.Index, op.PinTrustedPeer.GetPeer())
	case *internalpb.Command_SetTrustedPeerVoter:
		return f.applySetTrustedPeerVoter(log.Index, op.SetTrustedPeerVoter)
	case *internalpb.Command_UnpinTrustedPeer:
		return f.applyUnpinTrustedPeer(log.Index, op.UnpinTrustedPeer.GetNodeId())
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

// validSnapshotRef reports whether ref is safe to interpolate as a
// zfs(8) "dataset@snapshot" argument - both halves use validResourceID's
// own character class, so a value that passes here can never contain a
// "/" (dataset-path traversal) or a second "@" (snapshot-delimiter
// confusion). internal/zfs.Manager's own path()/snapshotPath() already
// reject those independently when CloneFromSnapshot/BaseTemplate
// actually get used (defense in depth, the same posture ADR-0067
// established for every renderer) - this closes the FSM-boundary gap
// those two fields (ADR-0095/ADR-0084) were never given, unlike every
// other interpolated field validResourceID/validHostname cover (ADR-0096).
func validSnapshotRef(ref string) bool {
	dataset, snap, ok := strings.Cut(ref, "@")
	return ok && validResourceID(dataset) && validResourceID(snap)
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
	if vm.GetCloneFromSnapshot() != "" && !validSnapshotRef(vm.GetCloneFromSnapshot()) {
		return &FSMApplyResult{Index: index, Error: fmt.Sprintf("CreateVM: invalid clone_from_snapshot %q: must be \"sourceid@snapshotname\" using only alphanumerics, '-', and '_'", vm.GetCloneFromSnapshot())}
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
		// uplink_bridged networks are served by the physical LAN's own
		// DHCP, not Apiary's - allocating (and thus reserving) a raft IP
		// here would just be bookkeeping Apiary can never enforce or
		// know is accurate. See ADR-0101.
		if !network.GetUplinkBridged() {
			ip, err := f.allocateIP(network)
			if err != nil {
				return &FSMApplyResult{Index: index, Error: fmt.Sprintf("CreateVM: %v", err)}
			}
			vm.IpAddress = ip
		}
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
	// A jail (ADR-0117) can name the same NetworkDefinition a VM does,
	// so its allocated addresses must be excluded here too - otherwise
	// a jail and a VM on the same network could collide on IP.
	for _, jail := range f.jails {
		if jail.GetNetworkId() == network.GetId() && jail.GetIpAddress() != "" {
			used[jail.GetIpAddress()] = true
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
	if vm.GetCloneFromSnapshot() != "" && !validSnapshotRef(vm.GetCloneFromSnapshot()) {
		return &FSMApplyResult{Index: index, Error: fmt.Sprintf("UpdateVM: invalid clone_from_snapshot %q: must be \"sourceid@snapshotname\" using only alphanumerics, '-', and '_'", vm.GetCloneFromSnapshot())}
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
//
// A VM that still exists must already be a DELETING tombstone. Every
// legitimate caller (the owning node's teardown, and the Admin-only
// ForcePurgeVM escape hatch) purges a resource that DeleteVM already
// marked; refusing anything else here means a purge can never remove the
// definition of a live VM, whichever RPC role submitted it.
func (f *FSM) applyPurgeVM(index uint64, id string) *FSMApplyResult {
	vm, exists := f.vms[id]
	if !exists {
		return &FSMApplyResult{Index: index}
	}
	if vm.GetDesiredState() != internalpb.VMState_VM_STATE_DELETING {
		return &FSMApplyResult{Index: index, Error: fmt.Sprintf("PurgeVM: VM %q is not marked for deletion - call DeleteVM first", id)}
	}
	delete(f.vms, id)
	return &FSMApplyResult{Index: index, VM: vm}
}

// applyCreateJail adds a new JailDefinition, mirroring applyCreateVM -
// including, since ADR-0117, the same network_id-driven IP allocation
// step (jail.GetVnet() is this jail's opt-in into dedicated VNET
// networking; see JailDefinition's own doc comment).
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
	if jail.GetHostname() != "" {
		if !validHostname(jail.GetHostname()) {
			return &FSMApplyResult{Index: index, Error: fmt.Sprintf("CreateJail: invalid hostname %q: only alphanumerics, '-', and '.' are allowed (max 255 chars)", jail.GetHostname())}
		}
		if conflict := f.jailHostnameConflict(jail.GetHostname(), ""); conflict != "" {
			return &FSMApplyResult{Index: index, Error: fmt.Sprintf("CreateJail: hostname %q is already used by jail %q", jail.GetHostname(), conflict)}
		}
	}
	// base_template is interpolated into a ZFS dataset/snapshot path
	// (internal/zfs.Manager.Clone, via internal/cluster's
	// jailTemplateSnapshot) - see validSnapshotRef's own doc comment
	// (ADR-0096) for why this needs the same FSM-boundary check as
	// clone_from_snapshot, even though it's a bare name rather than a
	// "dataset@snapshot" pair.
	if jail.GetBaseTemplate() != "" && !validResourceID(jail.GetBaseTemplate()) {
		return &FSMApplyResult{Index: index, Error: fmt.Sprintf("CreateJail: invalid base_template %q: only alphanumerics, '-', and '_' are allowed (max 64 chars)", jail.GetBaseTemplate())}
	}
	if jail.GetVnet() && jail.GetNetworkId() == "" {
		return &FSMApplyResult{Index: index, Error: "CreateJail: vnet requires network_id to be set"}
	}

	jail = proto.Clone(jail).(*internalpb.JailDefinition)
	if jail.GetNetworkId() != "" {
		network, ok := f.networks[jail.GetNetworkId()]
		if !ok {
			return &FSMApplyResult{Index: index, Error: fmt.Sprintf("CreateJail: network %q does not exist", jail.GetNetworkId())}
		}
		// Mirrors applyCreateVM: an uplink_bridged network is served by
		// the physical LAN's own DHCP, not Apiary's, so there is nothing
		// for the FSM itself to allocate here.
		if !network.GetUplinkBridged() {
			ip, err := f.allocateIP(network)
			if err != nil {
				return &FSMApplyResult{Index: index, Error: fmt.Sprintf("CreateJail: %v", err)}
			}
			jail.IpAddress = ip
		}
	}

	f.jails[jail.GetId()] = jail
	return &FSMApplyResult{Index: index, Jail: jail}
}

func (f *FSM) applyUpdateJail(index uint64, jail *internalpb.JailDefinition) *FSMApplyResult {
	if _, exists := f.jails[jail.GetId()]; !exists {
		return &FSMApplyResult{Index: index, Error: fmt.Sprintf("UpdateJail: id %q does not exist", jail.GetId())}
	}
	if jail.GetBaseTemplate() != "" && !validResourceID(jail.GetBaseTemplate()) {
		return &FSMApplyResult{Index: index, Error: fmt.Sprintf("UpdateJail: invalid base_template %q: only alphanumerics, '-', and '_' are allowed (max 64 chars)", jail.GetBaseTemplate())}
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

// applySetJailHostname sets JailDefinition.hostname on an existing
// jail, touching no other field - deliberately narrow, unlike
// applyUpdateJail's full-replace semantics, mirroring
// applySetVMFirewallPaused's own reasoning exactly.
func (f *FSM) applySetJailHostname(index uint64, req *internalpb.SetJailHostname) *FSMApplyResult {
	jail, exists := f.jails[req.GetId()]
	if !exists {
		return &FSMApplyResult{Index: index, Error: fmt.Sprintf("SetJailHostname: id %q does not exist", req.GetId())}
	}
	if req.GetHostname() != "" {
		if !validHostname(req.GetHostname()) {
			return &FSMApplyResult{Index: index, Error: fmt.Sprintf("SetJailHostname: invalid hostname %q: only alphanumerics, '-', and '.' are allowed (max 255 chars)", req.GetHostname())}
		}
		if conflict := f.jailHostnameConflict(req.GetHostname(), req.GetId()); conflict != "" {
			return &FSMApplyResult{Index: index, Error: fmt.Sprintf("SetJailHostname: hostname %q is already used by jail %q", req.GetHostname(), conflict)}
		}
	}
	updated := proto.Clone(jail).(*internalpb.JailDefinition)
	updated.Hostname = req.GetHostname()
	f.jails[req.GetId()] = updated
	return &FSMApplyResult{Index: index, Jail: updated}
}

// applyPurgeJail mirrors applyPurgeVM exactly: idempotent, not an
// error if id is already gone, and refused unless the jail is already a
// DELETING tombstone.
func (f *FSM) applyPurgeJail(index uint64, id string) *FSMApplyResult {
	jail, exists := f.jails[id]
	if !exists {
		return &FSMApplyResult{Index: index}
	}
	if jail.GetDesiredState() != internalpb.JailState_JAIL_STATE_DELETING {
		return &FSMApplyResult{Index: index, Error: fmt.Sprintf("PurgeJail: jail %q is not marked for deletion - call DeleteJail first", id)}
	}
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
	if err := ValidateJoinRequestFields(req.GetNodeId(), req.GetRaftBindAddress(), req.GetTlsCertFingerprint()); err != nil {
		return &FSMApplyResult{Index: index, Error: "CreatePendingJoinRequest: " + err.Error()}
	}
	if len(req.GetCode()) > maxJoinCodeLen || !printableASCII(req.GetCode()) || len(req.GetRequestId()) > maxJoinNodeIDLen || !printableASCII(req.GetRequestId()) {
		return &FSMApplyResult{Index: index, Error: "CreatePendingJoinRequest: request_id or code is malformed"}
	}
	if _, exists := f.pendingJoinRequests[req.GetRequestId()]; exists {
		return &FSMApplyResult{Index: index, Error: fmt.Sprintf("CreatePendingJoinRequest: request_id %q already exists", req.GetRequestId())}
	}
	// Bound the state an unauthenticated caller can make every voter keep.
	// Eviction is decided from the new request's own requested_at_unix (part
	// of the replicated log entry), never from the wall clock, so every
	// replica evicts exactly the same records when it applies this entry.
	for id, old := range f.pendingJoinRequests {
		if old.GetExpiresAtUnix()+joinRequestRetentionAfterExpiry < req.GetRequestedAtUnix() {
			delete(f.pendingJoinRequests, id)
		}
	}
	if len(f.pendingJoinRequests) >= MaxJoinRequests {
		return &FSMApplyResult{Index: index, Error: fmt.Sprintf("CreatePendingJoinRequest: %d join requests are already recorded; an Admin must purge stale ones before more can be accepted", len(f.pendingJoinRequests))}
	}
	f.pendingJoinRequests[req.GetRequestId()] = req
	return &FSMApplyResult{Index: index, PendingJoinRequest: req}
}

// applyApprovePendingJoinRequest and applyRejectPendingJoinRequest both
// require the request to currently be Pending - an Admin approving/
// rejecting a request that some other Admin (on a different Colony
// member's UI, since this is raft-replicated) already resolved is a real
// error, not a silent no-op, so a stale browser tab's second click
// surfaces clearly rather than double-applying. Expiry is enforced by
// internal/manager before the command is submitted, not here.
// applyRecordJoinApproval records ONE authorizing act against a
// still-pending request (ADR-0147 Part 3's two-person rule) and resolves
// nothing. It fills the first empty approval slot and fails once both
// are full.
//
// Two slots rather than one, and a failure rather than an overwrite, is
// the design. One slot would let a third call silently replace a record
// an operator may still be reading; a list would grow without bound on a
// value only ever read two deep. The FSM's serialized apply order is
// what makes "the first approval" a total order every replica agrees
// on, so a leadership change cannot change which act came first.
//
// The request must be PENDING: recording an authorizing act against a
// request that has already resolved would be writing into a closed
// record. Expiry is not consulted here and at_unix arrives on the
// command, for the determinism reason applyResolvePendingJoinRequest
// below gives in full.
func (f *FSM) applyRecordJoinApproval(index uint64, cmd *internalpb.RecordJoinApproval) *FSMApplyResult {
	req, exists := f.pendingJoinRequests[cmd.GetRequestId()]
	if !exists {
		return &FSMApplyResult{Index: index, Error: fmt.Sprintf("RecordJoinApproval: request_id %q does not exist", cmd.GetRequestId())}
	}
	if req.GetStatus() != internalpb.JoinRequestStatus_JOIN_REQUEST_STATUS_PENDING {
		return &FSMApplyResult{Index: index, Error: fmt.Sprintf("RecordJoinApproval: request_id %q is already %s", cmd.GetRequestId(), req.GetStatus())}
	}
	// An absent identity is refused rather than recorded as an empty
	// string. Two of these is exactly what the two-person rule counts,
	// and an empty key id is precisely the "nobody authenticated" case
	// - managerd with no API keys at all - which the rule must not be
	// satisfiable by.
	if cmd.GetKeyId() == "" || cmd.GetNodeId() == "" {
		return &FSMApplyResult{Index: index, Error: "RecordJoinApproval: key_id and node_id must both be set - a managerd with no API keys cannot satisfy the two-authorization rule, which is deliberate rather than an oversight"}
	}
	if !printableASCII(cmd.GetKeyId()) || !printableASCII(cmd.GetNodeId()) {
		return &FSMApplyResult{Index: index, Error: "RecordJoinApproval: key_id or node_id is malformed"}
	}
	updated := proto.Clone(req).(*internalpb.PendingJoinRequest)
	attestation := &internalpb.JoinApprovalAttestation{
		KeyId:  cmd.GetKeyId(),
		NodeId: cmd.GetNodeId(),
		AtUnix: cmd.GetAtUnix(),
	}
	first := updated.GetApproval_1()
	switch {
	case first == nil:
		updated.Approval_1 = attestation

	case updated.GetApproval_2() == nil:
		updated.Approval_2 = attestation

	case AuthorizationsDistinct(first, updated.GetApproval_2()):
		// The rule is already satisfied, and a further act is IGNORED
		// rather than recorded or refused.
		//
		// Ignored rather than recorded: the pair is the Colony's record
		// of who admitted this Comb, and overwriting either half of it
		// with a later click would rewrite that history.
		//
		// Ignored rather than refused, and that is the part worth being
		// careful about. A request can hold two distinct authorizations
		// and still be refused by a LATER check - a duplicate node_id, a
		// reachability dial that failed. An operator who fixes that and
		// approves again must get past this gate, or the gate has made
		// its own success unrecoverable: every retry would stop here
		// with a message about a rule the operator had already satisfied,
		// pointing at nothing they can change. A rule that cannot be
		// re-entered after a correctable failure is a trap.
		return &FSMApplyResult{Index: index, PendingJoinRequest: req}

	case AuthorizationsDistinct(first, attestation):
		// The two recorded acts can never satisfy the two-person rule -
		// one key twice, or two keys on one Comb - so the second slot is
		// REPLACED rather than the request being left unapprovable. The
		// first act is never touched: it is the one that made the pair
		// unusable, and discarding it would let the same Comb keep
		// rolling the record forward.
		//
		// This is the only overwrite in the FSM, and it overwrites only
		// a value that has no approving power to lose.
		updated.Approval_2 = attestation

	default:
		// Both recorded acts AND this one agree with each other, so
		// there is nothing left to replace: the caller is repeating the
		// first authorization and no arrangement of slots would ever be
		// satisfiable from here. Named rather than silently dropped,
		// because "refused with no reason" is how an operator ends up
		// concluding the Colony is broken.
		return &FSMApplyResult{Index: index, Error: fmt.Sprintf(
			"RecordJoinApproval: request_id %q is already recorded twice over by key %q on %q, and this call is that same key on that same Comb - no arrangement of the two slots can satisfy ADR-0147 Part 3 from here. Approve it from a different Comb with a different API key, or reject this request and have the joining Comb request again",
			cmd.GetRequestId(), first.GetKeyId(), first.GetNodeId())}
	}
	f.pendingJoinRequests[cmd.GetRequestId()] = updated
	return &FSMApplyResult{Index: index, PendingJoinRequest: updated}
}

// AuthorizationsDistinct is ADR-0147 Part 3's two-person rule as a
// single predicate: two different API keys, presented to two different
// Combs.
//
// Exported, and used by internal/manager through this function rather
// than by reimplementing it there, so the rule has exactly ONE
// definition in the codebase. It has to: the FSM enforces it (and must
// reach the identical decision on every replica) while the manager
// layer reports on it, and two copies would be two chances for the
// manager to refuse a pair the FSM considers valid, or the reverse.
// A nil argument is never distinct from anything, because a missing
// attestation is the absence of an authorization rather than a
// different one.
func AuthorizationsDistinct(a, b *internalpb.JoinApprovalAttestation) bool {
	if a == nil || b == nil {
		return false
	}
	return a.GetKeyId() != b.GetKeyId() && a.GetNodeId() != b.GetNodeId()
}

// The authorization fields arrive on the APPROVE command rather than
// being written by a second command afterwards, because the record of
// use and the approval it authorizes have to be one atomic fact: a log
// entry that approved without recording which entry was spent would
// leave single use resting entirely on a local file, which is the exact
// failure the replicated record exists to prevent.
func (f *FSM) applyApprovePendingJoinRequest(index uint64, cmd *internalpb.ApprovePendingJoinRequest) *FSMApplyResult {
	return f.applyResolvePendingJoinRequest(index, cmd.GetRequestId(), internalpb.JoinRequestStatus_JOIN_REQUEST_STATUS_APPROVED, "ApprovePendingJoinRequest",
		func(updated *internalpb.PendingJoinRequest) {
			updated.AuthorizationId = cmd.GetAuthorizationId()
			updated.ConsumedAtUnix = cmd.GetConsumedAtUnix()
		})
}

func (f *FSM) applyRejectPendingJoinRequest(index uint64, requestID string) *FSMApplyResult {
	return f.applyResolvePendingJoinRequest(index, requestID, internalpb.JoinRequestStatus_JOIN_REQUEST_STATUS_REJECTED, "RejectPendingJoinRequest", nil)
}

// applyCancelPendingJoinRequest is the requesting Comb's own withdrawal
// of its still-pending request - same validation as Approve/Reject
// (must currently be Pending, not expired), just a different terminal
// status. Deliberately not restricted to "the caller who created it" -
// request_id itself already carries that same knowledge-is-the-
// credential trust model GetJoinRequestStatus already established
// (ADR-0083), so no new authorization concept is introduced here.
func (f *FSM) applyCancelPendingJoinRequest(index uint64, requestID string) *FSMApplyResult {
	return f.applyResolvePendingJoinRequest(index, requestID, internalpb.JoinRequestStatus_JOIN_REQUEST_STATUS_CANCELLED, "CancelPendingJoinRequest", nil)
}

// stamp is how the approve path adds ADR-0147 Part 3's record of use to
// the same atomic state change that flips the status; nil for the three
// resolutions that carry nothing extra.
func (f *FSM) applyResolvePendingJoinRequest(index uint64, requestID string, status internalpb.JoinRequestStatus, opName string, stamp func(*internalpb.PendingJoinRequest)) *FSMApplyResult {
	req, exists := f.pendingJoinRequests[requestID]
	if !exists {
		return &FSMApplyResult{Index: index, Error: fmt.Sprintf("%s: request_id %q does not exist", opName, requestID)}
	}
	if req.GetStatus() != internalpb.JoinRequestStatus_JOIN_REQUEST_STATUS_PENDING {
		return &FSMApplyResult{Index: index, Error: fmt.Sprintf("%s: request_id %q is already %s", opName, requestID, req.GetStatus())}
	}
	// Expiry is deliberately NOT checked here. This runs on every replica and
	// again whenever the log is replayed (restart, snapshot catch-up), so
	// consulting the wall clock would let a replay after the TTL reject an
	// entry that originally succeeded and leave replicas disagreeing about
	// the request's status. internal/manager checks expiry against the clock
	// before submitting the command instead.
	updated := proto.Clone(req).(*internalpb.PendingJoinRequest)
	updated.Status = status
	if stamp != nil {
		stamp(updated)
	}
	f.pendingJoinRequests[requestID] = updated
	// ADR-0147 Part 4's pin lifecycle, inside the same log entry that
	// settles the request rather than as a command a caller has to
	// remember to send. The ordering matters: the pin is written when
	// the certificate is accepted, which is BEFORE this point, so a
	// store whose cleanup were a separate call would be a store an
	// interrupted or half-completed approval could leave a dead
	// certificate in forever. Doing it here means "approved" and "the
	// pin is now a member pin" are one replicated fact, and "rejected"
	// and "the pin is gone" are one replicated fact.
	//
	// Approve promotes; Reject and Cancel drop, because a request that
	// reached a terminal state without becoming a voter must not leave
	// a standing trust anchor behind. See peers.go.
	if status == internalpb.JoinRequestStatus_JOIN_REQUEST_STATUS_APPROVED {
		f.promotePin(updated.GetNodeId())
	} else {
		f.dropUnpromotedPin(updated.GetNodeId())
	}
	return &FSMApplyResult{Index: index, PendingJoinRequest: updated}
}

// AuthorizationUse reports which request already SPENT a given
// root-owned authorization entry, and when (ADR-0147 Part 3).
//
// This is the replicated half of single use, and it exists because the
// file cannot be the record of it. Only the leader reads the
// authorization file, so after a leadership change the new leader holds
// a file that may still list an entry as available when a previous
// leader already spent it. This scan is over replicated state and so
// answers the same way on every Comb and in every election.
//
// The scan is a linear walk of a bounded map (MaxJoinRequests, 100),
// reached only on the approval path immediately before a call that
// commits a raft membership change. One limit is stated rather than
// worked around: a request that was PURGED after its approval leaves the
// map, and its entry becomes spendable again. The alternative is a
// second replicated map whose entire purpose is to remember a bounded
// amount of history forever, which is a worse trade than an explicit
// Admin action - PurgeJoinRequest - being able to forget.
func (f *FSM) AuthorizationUse(authorizationID string) (requestID string, consumedAtUnix int64, found bool) {
	if authorizationID == "" {
		return "", 0, false
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for id, req := range f.pendingJoinRequests {
		if req.GetAuthorizationId() == authorizationID {
			return id, req.GetConsumedAtUnix(), true
		}
	}
	return "", 0, false
}

// applyPurgeJoinRequest mirrors applyPurgeJail exactly: idempotent,
// not an error if requestID is already gone. Unlike
// applyResolvePendingJoinRequest above, this works regardless of the
// record's current status (Pending, Approved, Rejected, Cancelled, or
// expired) - an existing Colony Admin's way to actually clean up a
// stale or erroneous entry, since Approve/Reject/Cancel deliberately
// never delete anything.
func (f *FSM) applyPurgeJoinRequest(index uint64, requestID string) *FSMApplyResult {
	req := f.pendingJoinRequests[requestID]
	delete(f.pendingJoinRequests, requestID)
	// A purge is an Admin cleaning up, so it is the last chance to drop
	// a pin for a request that never became a voter - including a
	// request that simply expired without ever being resolved, which is
	// the shape most of these records actually have. A pin that IS a
	// voter pin is deliberately kept: the request is a stale record, the
	// member is not, and dropping a live member's trust anchor because
	// someone tidied up an old row would be a far worse outcome than an
	// entry outliving its request.
	if req != nil {
		f.dropUnpromotedPin(req.GetNodeId())
	}
	return &FSMApplyResult{Index: index, PendingJoinRequest: req}
}

// applyAcquireRestartLease is the sole enforcement point for the
// action-preflight restart guardrail (ADR-0103): raft's own serialized
// log-apply order is what makes "at most one node holds a lease for a
// given service at a time" an actual guarantee, not a per-node check
// racing against another node's identical check (the exact flaw an
// earlier, purely local-timestamp design of this guardrail had). Every
// field on req is authored by the current leader immediately before
// submission (ManagerService.ReserveRestartLease's own doc comment) -
// this function never calls time.Now() or re-derives voter membership
// itself, since every raft replica must reach the identical decision
// from the identical command.
//
// ADR-0145's fence rides on this same command, deliberately, and this is
// the load-bearing part of "a coordinator that has lost ownership can no
// longer issue restarts". A ColonyUpdate record says who is running a
// controlled update; without this check a displaced coordinator would
// keep that record - correctly, it was the truth when it read it - and
// could still acquire every per-Comb restart lease its sweep had queued,
// which is the exact double-update the single-flight exists to prevent.
// Checking it HERE, in the FSM, is what makes the fence survive process
// death: the only thing that can invalidate a token is a committed
// apply, and nothing local to any managerd can.
func (f *FSM) applyAcquireRestartLease(index uint64, req *internalpb.AcquireRestartLease) *FSMApplyResult {
	service := req.GetService()

	// A fence, when present, must match the active operation EXACTLY.
	// An empty fence is unconstrained and falls through - see
	// AcquireRestartLease.colony_update_fence's own doc comment for why
	// requiring one would break every existing operator-driven
	// RestartNodeService caller.
	//
	// Note what force does and does not do here. A fence mismatch is
	// NOT overridable, deliberately and for the same reason restartplan
	// refuses to let Force rescue a failed step-aside: force
	// acknowledges a KNOWN cost, and "you are not the coordinator
	// anymore" is not a cost, it is the absence of permission. The
	// consequence is that a takeover genuinely terminates the displaced
	// coordinator rather than merely outranking it, which is what
	// makes an explicit takeover safe enough to exist at all.
	if fence := req.GetColonyUpdateFence(); fence.GetOperationId() != "" {
		active := activeColonyUpdate(f.colonyUpdates)
		if !fenceMatchesActive(fence, active) {
			return &FSMApplyResult{Index: index, Error: fenceRefusal(
				fmt.Sprintf("acquire the restart lease for %q on %q", service, req.GetNodeId()), fence, active)}
		}
	}

	// An existing lease blocks unconditionally, regardless of age -
	// deliberately no expiry check here. A time-based auto-clear was
	// considered and rejected (see RestartLease's own doc comment):
	// letting a lease lapse while the underlying restart's outcome is
	// still genuinely unknown would reopen the exact concurrent-restart
	// window this guardrail exists to close.
	existingLease := f.restartLeases[service]
	activeLease := existingLease != nil

	// A prior RestartRecord only counts toward the cooldown if its
	// holder is a currently-known Raft voter - a non-voter's own
	// manager restart carries no quorum risk and must never block a
	// voter's restart.
	var recentVoterRestart bool
	if record := f.restartRecords[service]; record != nil && voterListContains(req.GetVoterNodeIds(), record.GetNodeId()) {
		// A negative elapsed value (from clock skew across a leader
		// election) already blocks here, conservatively - see
		// ADR-0103's own Consequences for why this is deliberate, not
		// an unhandled edge case.
		elapsed := req.GetRequestedAtUnix() - record.GetCompletedAtUnix()
		recentVoterRestart = elapsed < req.GetCooldownSeconds()
	}

	blocked := activeLease || recentVoterRestart
	if blocked && !req.GetForce() {
		switch {
		case activeLease:
			return &FSMApplyResult{Index: index, Error: fmt.Sprintf(
				"AcquireRestartLease: %q already has an unconfirmed restart lease held by node %q (requested at %d) - refusing to grant a second one until it is confirmed complete or explicitly overridden with force",
				service, existingLease.GetHolderNodeId(), existingLease.GetRequestedAtUnix(),
			)}
		default:
			record := f.restartRecords[service]
			return &FSMApplyResult{Index: index, Error: fmt.Sprintf(
				"AcquireRestartLease: %q was restarted by voter %q %ds ago, inside the %ds cooldown - refusing to grant a concurrent restart lease",
				service, record.GetNodeId(), req.GetRequestedAtUnix()-record.GetCompletedAtUnix(), req.GetCooldownSeconds(),
			)}
		}
	}

	lease := &internalpb.RestartLease{
		LeaseId:         index,
		Service:         service,
		HolderNodeId:    req.GetNodeId(),
		RequestedAtUnix: req.GetRequestedAtUnix(),
		Force:           blocked && req.GetForce(),
	}
	f.restartLeases[service] = lease
	return &FSMApplyResult{Index: index, RestartLease: lease}
}

// applyRecordRestartCompleted is submitted by the restarted node's own
// next startup, never by the process that requested the restart (see
// ADR-0103) - completed_at_unix is authored by whichever node applies
// this (the current leader), the same determinism reasoning as
// applyAcquireRestartLease.
func (f *FSM) applyRecordRestartCompleted(index uint64, req *internalpb.RecordRestartCompleted) *FSMApplyResult {
	service := req.GetService()

	// The record is written unconditionally - a real restart really did
	// complete, regardless of whether the lease below still matches.
	record := &internalpb.RestartRecord{
		Service:         service,
		NodeId:          req.GetNodeId(),
		CompletedAtUnix: req.GetCompletedAtUnix(),
	}
	f.restartRecords[service] = record

	// The lease is released ONLY on an exact lease_id AND holder_node_id
	// match - a stale or out-of-order confirmation must never release a
	// different, currently-active lease for the same service/node pair
	// (see RestartLease's own doc comment).
	if lease := f.restartLeases[service]; lease != nil &&
		lease.GetLeaseId() == req.GetLeaseId() &&
		lease.GetHolderNodeId() == req.GetNodeId() {
		delete(f.restartLeases, service)
	}

	return &FSMApplyResult{Index: index, RestartRecord: record}
}

// applyAcquireColonyUpdate is the single-flight itself (ADR-0145): at
// most one controlled update may exist in the Colony at a time.
//
// The exclusivity is raft's own serialized log-apply order and nothing
// else - there is no lock anywhere in this file, and that is the entire
// point. Two coordinators on two Combs, each having checked "is anything
// running?" against its own possibly-stale view, still cannot both be
// granted, because their two commands occupy two different positions in
// one totally ordered log and this function is called once per position.
// That is the same property ADR-0103 bought for the per-service restart
// lease, and the same reasoning that killed the purely local-timestamp
// design recorded there.
//
// Like applyAcquireRestartLease, every time-bearing field is authored by
// the current leader immediately before submission and never read from
// the request, so every replica reaches the identical decision from the
// identical command.
func (f *FSM) applyAcquireColonyUpdate(index uint64, req *internalpb.AcquireColonyUpdate) *FSMApplyResult {
	if req.GetOperationId() == "" {
		return &FSMApplyResult{Index: index, Error: "AcquireColonyUpdate: no operation id was given, so there is no operation to run and nothing a later reader could name"}
	}
	if req.GetHolderNodeId() == "" {
		return &FSMApplyResult{Index: index, Error: "AcquireColonyUpdate: no holder node id was given, so the single-flight could not record who holds it"}
	}
	if req.GetHolderIncarnation() == "" {
		// Not defensive padding. Without it a replacement managerd on the
		// same Comb is indistinguishable from the predecessor it
		// replaced, and the predecessor would keep a valid fence - which
		// is precisely the resurrected-old-coordinator case the fence
		// exists to defeat.
		return &FSMApplyResult{Index: index, Error: "AcquireColonyUpdate: no holder incarnation was given, so a replacement managerd could not be told apart from the process it replaced"}
	}

	// Id reuse is a bug, not a re-acquire. A caller that wants to resume
	// an operation already in flight must present its fence to
	// Advance/Release, not to re-grant the id: re-granting would reset
	// the fence token and silently un-fence whatever the old process
	// still holds. A caller that wants to start a NEW operation must
	// name a new id, which is what makes "has this operation ever run?"
	// answerable from retained history.
	if prior, used := f.colonyUpdates[req.GetOperationId()]; used {
		settled := "still in progress"
		if !prior.GetActive() {
			settled = fmt.Sprintf("already settled (%q at %d)", prior.GetOutcome(), prior.GetSettledAtUnix())
		}
		return &FSMApplyResult{Index: index, Error: fmt.Sprintf(
			"AcquireColonyUpdate: operation id %q has already been used (granted at fence token %d, %s); "+
				"re-granting it would reset the fence and un-fence whatever its previous holder still holds, "+
				"so resume it with AdvanceColonyUpdate/ReleaseColonyUpdate instead, or name a new operation id",
			req.GetOperationId(), prior.GetFenceToken(), settled)}
	}

	active := activeColonyUpdate(f.colonyUpdates)
	takeover := false
	if active != nil {
		if !req.GetTakeover() {
			where := "no step recorded"
			if active.GetStep() != "" {
				where = "at step " + active.GetStep()
			}
			return &FSMApplyResult{Index: index, Error: fmt.Sprintf(
				"AcquireColonyUpdate: controlled update %q is already in progress, held by node %q (incarnation %q) at fence token %d, %s; "+
					"only one controlled update may exist in the Colony at a time - this request was refused, and a takeover must be an explicit, deliberate act by whoever takes responsibility",
				active.GetOperationId(), active.GetHolderNodeId(), active.GetHolderIncarnation(),
				active.GetFenceToken(), where)}
		}
		// Settle the displaced record as UNOBSERVED, not as failed and
		// not as anything that reads like a conclusion. That is the
		// honest verdict: the displaced holder may well still be running,
		// nobody has looked, and the whole point of the higher fence
		// token is that it does not need to be looked at - but "we did
		// not check" is exactly what this word means, and
		// internal/cluster's own convention insists it never be folded
		// into either "fine" or "broken".
		//
		// It MUST be settled rather than left active, and that is not
		// tidiness: two entries with active = true would break the
		// single-flight invariant this whole mechanism rests on, and
		// every later reader - including fenceMatchesActive - finds
		// "the" active record by scanning for the first one it meets.
		active.Active = false
		active.Outcome = colonyOutcomeUnobserved
		active.SettledAtUnix = req.GetRequestedAtUnix()
		active.UpdatedAtUnix = req.GetRequestedAtUnix()
		active.Detail = fmt.Sprintf(
			"displaced by controlled update %q, granted to %q (incarnation %q) at fence token %d; "+
				"this operation's fate is unobserved - its previous holder (%q, incarnation %q, fence token %d) is fenced out of both the update record and the restart-lease path, "+
				"but nobody has looked at what it had already done, and it is not resumable",
			req.GetOperationId(), req.GetHolderNodeId(), req.GetHolderIncarnation(), index,
			active.GetHolderNodeId(), active.GetHolderIncarnation(), active.GetFenceToken())
		takeover = true
	}

	rec := &internalpb.ColonyUpdate{
		OperationId:       req.GetOperationId(),
		HolderNodeId:      req.GetHolderNodeId(),
		HolderIncarnation: req.GetHolderIncarnation(),
		// The fence token is this command's own log index: unique and
		// monotonic for free, exactly as RestartLease.lease_id is
		// (ADR-0103), and strictly greater than every token issued
		// before it, which is what makes the takeover above fence the
		// displaced holder on every replica at the same instant.
		FenceToken:    index,
		GrantedAtUnix: req.GetRequestedAtUnix(),
		Active:        true,
		Takeover:      takeover,
		UpdatedAtUnix: req.GetRequestedAtUnix(),
	}
	f.colonyUpdates[rec.OperationId] = rec

	// Housekeeping, performed by the one command that legitimately
	// creates history rather than by a background sweeper - see
	// evictSettledColonyUpdates' own doc comment.
	evictSettledColonyUpdates(f.colonyUpdates, maxSettledColonyUpdates)

	return &FSMApplyResult{Index: index, ColonyUpdate: rec}
}

// applyAdvanceColonyUpdate records durable progress on the operation the
// caller still holds. It is the durable half of "what is the state of the
// controlled update?", and it is also how a replacement managerd's own
// step records become readable by every other Comb rather than only by
// the one that wrote them.
//
// Every field of the fence must match the active record exactly. A
// mismatch - an old token, a superseded incarnation, somebody else's
// operation - is refused BY NAME, naming what is actually held, and is
// not overridable by anything: there is no force field on this command,
// for the same reason there is none on the restart-lease fence.
func (f *FSM) applyAdvanceColonyUpdate(index uint64, req *internalpb.AdvanceColonyUpdate) *FSMApplyResult {
	active := activeColonyUpdate(f.colonyUpdates)
	fence := req.GetFence()
	if !fenceMatchesActive(fence, active) {
		return &FSMApplyResult{Index: index, Error: fenceRefusal("advance the controlled update", fence, active)}
	}

	record := req.GetStepRecord()
	if err := validateColonyUpdateStep(record); err != nil {
		return &FSMApplyResult{Index: index, Error: fmt.Sprintf("refusing to advance controlled update %q: %v", active.GetOperationId(), err)}
	}
	if record != nil && !appendColonyUpdateStep(active, record) {
		// appendColonyUpdateStep has already formed the precise refusal,
		// naming the position and what is already there.
		return &FSMApplyResult{Index: index, Error: fmt.Sprintf(
			"refusing to advance controlled update %q: %s",
			active.GetOperationId(), colonyUpdateStepRefusal(active, record))}
	}
	if req.GetStep() != "" {
		active.Step = req.GetStep()
	}
	if req.GetTargetNodeId() != "" {
		active.TargetNodeId = req.GetTargetNodeId()
	}
	if req.GetDetail() != "" {
		active.Detail = req.GetDetail()
	}
	return &FSMApplyResult{Index: index, ColonyUpdate: active}
}

// appendColonyUpdateStep adds rec to rec_owner's step history at the
// ordinal position rec itself names, and reports whether it did.
//
// Three cases, and the distinctions between them are the point:
//
//   - the next free position: appended, with recorded_at_unix STAMPED
//     from the leader's own clock rather than accepted from the caller,
//     for exactly the reason applyAcquireRestartLease stamps its own
//     requested_at_unix - every replica must reach the identical value.
//   - an occupied position carrying an IDENTICAL record: accepted as the
//     no-op it is. A coordinator that crashed after its apply committed
//     but before it read the response re-sends the same record on retry,
//     and refusing that would wedge the operation on a lost response
//     rather than on anything real.
//   - anything else: refused. A gap means a reader could be shown a
//     sequence with a hole it cannot explain; a differing record at an
//     occupied position means a superseded holder is trying to rewrite
//     history it no longer owns.
func appendColonyUpdateStep(rec *internalpb.ColonyUpdate, step *internalpb.ColonyUpdateStepRecord) bool {
	pos := int(step.GetIndex())
	existing := rec.GetSteps()
	switch {
	case pos == len(existing):
		// proto.Clone rather than a Go struct copy: a generated message
		// carries an internal mutex and state pointer, and copying one
		// by value is both a vet error and a real race waiting to happen.
		stamped, ok := proto.Clone(step).(*internalpb.ColonyUpdateStepRecord)
		if !ok {
			return false
		}
		stamped.RecordedAtUnix = rec.GetUpdatedAtUnix()
		rec.Steps = append(existing, stamped)
		return true
	case pos < len(existing):
		e := existing[pos]
		return e.GetStep() == step.GetStep() &&
			e.GetOutcome() == step.GetOutcome() &&
			e.GetDetail() == step.GetDetail() &&
			e.GetNodeId() == step.GetNodeId() &&
			equalStrings(e.GetEvidence(), step.GetEvidence())
	default:
		return false
	}
}

// colonyUpdateStepRefusal explains why a step record was not appended,
// naming the position and what is already there so the reader does not
// have to go and look. It only ever describes a REJECTED record, so the
// caller has already established that.
func colonyUpdateStepRefusal(rec *internalpb.ColonyUpdate, step *internalpb.ColonyUpdateStepRecord) string {
	pos := int(step.GetIndex())
	existing := rec.GetSteps()
	if pos > len(existing) {
		return fmt.Sprintf("step position %d skips ahead of the %d step record(s) already written; steps are recorded in order so a reader can never be shown a gap it cannot explain",
			pos, len(existing))
	}
	e := existing[pos]
	return fmt.Sprintf("step position %d is already recorded as %q (%q on %q) and this request says %q (%q on %q); a step record is immutable once written, so a holder that has been superseded cannot rewrite one",
		pos, e.GetStep(), e.GetOutcome(), e.GetNodeId(), step.GetStep(), step.GetOutcome(), step.GetNodeId())
}

// applyReleaseColonyUpdate settles the operation terminally.
//
// It MARKS the record settled rather than deleting it, and that is the
// reason the record answers the question ADR-0145 asked. A replacement
// managerd asking "what is the state of the controlled update?" has to
// be able to find the answer both while it runs and after it is over;
// "is anything running?" and "what happened to the one that was?" are
// different questions and the same object answers both.
//
// The exact-fence requirement is identical to AdvanceColonyUpdate's, and
// for the same reason: a displaced coordinator must not be able to
// settle the operation that displaced it, which would leave the colony
// believing an update finished when its replacement is still running.
func (f *FSM) applyReleaseColonyUpdate(index uint64, req *internalpb.ReleaseColonyUpdate) *FSMApplyResult {
	active := activeColonyUpdate(f.colonyUpdates)
	fence := req.GetFence()
	if !fenceMatchesActive(fence, active) {
		return &FSMApplyResult{Index: index, Error: fenceRefusal("settle the controlled update", fence, active)}
	}
	if !validColonyUpdateOutcome(req.GetOutcome()) {
		return &FSMApplyResult{Index: index, Error: fmt.Sprintf(
			"refusing to settle controlled update %q with outcome %q, which is not one of the recognised outcomes (%q, %q, %q, %q, %q); "+
				"\"the update finished\" is a claim like any other and is not the absence of one",
			active.GetOperationId(), req.GetOutcome(),
			colonyOutcomeConfirmed, colonyOutcomeFailed, colonyOutcomeBlocked,
			colonyOutcomeUnobserved, colonyOutcomeUnverified)}
	}
	if req.GetDetail() == "" {
		return &FSMApplyResult{Index: index, Error: fmt.Sprintf(
			"refusing to settle controlled update %q as %q with no detail - an unbacked verdict is not a record",
			active.GetOperationId(), req.GetOutcome())}
	}

	active.Active = false
	active.Outcome = req.GetOutcome()
	active.Detail = req.GetDetail()
	active.SettledAtUnix = req.GetCompletedAtUnix()
	active.UpdatedAtUnix = req.GetCompletedAtUnix()
	// Housekeeping again, for the same reason acquire does it: the cap
	// has to hold at every point a reader could look, not merely after
	// the next grant. Leaving the bound to be re-established by a
	// subsequent acquire would mean the state was over its cap for as
	// long as nobody started anything - which, on a quiet colony, is
	// forever.
	evictSettledColonyUpdates(f.colonyUpdates, maxSettledColonyUpdates)
	return &FSMApplyResult{Index: index, ColonyUpdate: active}
}

// applyHandoverColonyUpdate is the PLANNED, cooperative transfer of an
// operation that is already in progress (ADR-0146 rule 2). It is the
// normal way a colony-wide sweep finishes: the operation names every
// Comb, so eventually the only one left is the holder's own, and the
// holder cannot restart itself - ADR-0142 refuses that outright - so
// ownership moves to another Comb before that Comb is touched.
//
// It is a separate command from AcquireColonyUpdate's takeover flag, and
// the separation is the entire point. Takeover is a contested seizure of a
// live operation from a coordinator that has stopped answering; a handover
// is cooperation between two coordinators that both know what is
// happening. Routing the second through the first would set takeover=true
// on the common path, settle the outgoing record as "unobserved", and
// leave a healthy sweep indistinguishable in the durable record from a
// cluster fight. That destroys the flag's only purpose, which is to tell
// an operator that something went wrong.
//
// Three properties keep this a handover rather than a way around the
// fence:
//
//  1. The OUTGOING holder's exact fence must match the active record. A
//     coordinator that has already been displaced, or whose operation was
//     taken over, cannot hand over an operation it no longer holds. Same
//     fenceMatchesActive rule Advance and Release use, and the refusal is
//     the same named refusal.
//  2. The record is NOT settled. Operation id, step history, target and
//     progress all survive; only the holder changes. A handover that
//     created a new record would discard exactly the durable progress
//     this whole mechanism exists to preserve.
//  3. The fence token is minted higher, because it is this command's own
//     log index, exactly as on acquire. The incoming holder faces the
//     identical exact-match requirement as any other holder, and the
//     outgoing token is stale the instant this commits, on every replica.
//     A handover relocates an operation; it never relaxes what is required
//     to keep moving it.
//
// Note what is deliberately NOT here. There is no force, and no override
// of any kind: a handover is a thing two coordinators agree on, and if the
// outgoing one cannot be reached then what the operator needs is an
// explicit takeover, which is loud and is recorded as such. There is also
// no membership check on to_node_id. The FSM has no authoritative view of
// Colony membership - applyAcquireRestartLease takes its voter list from
// the caller for the same reason - so validating the target Comb is the
// resolving managerd's job, not this one's, and inventing a partial check
// here would be a check that could be wrong.
func (f *FSM) applyHandoverColonyUpdate(index uint64, req *internalpb.HandoverColonyUpdate) *FSMApplyResult {
	active := activeColonyUpdate(f.colonyUpdates)
	from := req.GetFromFence()
	if !fenceMatchesActive(from, active) {
		return &FSMApplyResult{Index: index, Error: fenceRefusal("hand over the controlled update", from, active)}
	}
	if err := validateColonyUpdateHandover(req); err != nil {
		return &FSMApplyResult{Index: index, Error: fmt.Sprintf(
			"refusing to hand over controlled update %q: %v", active.GetOperationId(), err)}
	}

	// A handover to the identity it already has would mint a higher fence
	// token and append a record, changing nothing. That is refused rather
	// than tolerated because the whole point of the handovers list is that
	// every entry in it is a real change of coordinator, and a list that
	// can fill with no-ops is a list nobody can read.
	if req.GetToNodeId() == active.GetHolderNodeId() &&
		req.GetToHolderIncarnation() == active.GetHolderIncarnation() {
		return &FSMApplyResult{Index: index, Error: fmt.Sprintf(
			"refusing to hand over controlled update %q to node %q (incarnation %q): that is the coordinator that already holds it, "+
				"so the handover would change nothing while still minting a higher fence token and recording a change of holder that did not happen",
			active.GetOperationId(), active.GetHolderNodeId(), active.GetHolderIncarnation())}
	}

	// Authored here, on the leader, and never taken from the request -
	// req.GetRequestedAtUnix() is deliberately not used for this, exactly
	// as applyAcquireColonyUpdate ignores its own for the record it
	// writes. Every replica must be able to agree on the record's contents
	// from the log alone.
	handedOver := &internalpb.ColonyUpdateHandover{
		FromNodeId:       active.GetHolderNodeId(),
		FromIncarnation:  active.GetHolderIncarnation(),
		FromFenceToken:   active.GetFenceToken(),
		ToNodeId:         req.GetToNodeId(),
		ToIncarnation:    req.GetToHolderIncarnation(),
		ToFenceToken:     index,
		Reason:           req.GetReason(),
		HandedOverAtUnix: req.GetRequestedAtUnix(),
	}

	// active.Takeover is deliberately left alone. It is sticky on purpose:
	// if this operation was ever seized rather than handed over, an
	// operator reading it later must still be able to see that, however
	// many cooperative handovers followed.
	active.HolderNodeId = req.GetToNodeId()
	active.HolderIncarnation = req.GetToHolderIncarnation()
	active.FenceToken = index
	active.UpdatedAtUnix = req.GetRequestedAtUnix()
	active.Handovers = append(active.Handovers, handedOver)
	// Detail is deliberately NOT rewritten. It is the current step's own
	// description, and overwriting it with a sentence about the handover
	// would erase the thing a reader is most likely to be looking for.
	// The handover is in Handovers, where a reader can find it by name.

	// No eviction: a handover creates no settled record, so it cannot
	// push the history past its cap.
	return &FSMApplyResult{Index: index, ColonyUpdate: active}
}

// equalStrings compares two string slices element-wise. A tiny helper
// rather than slices.Equal so the comparison's intent reads as a replay
// check at the call site.
func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// voterListContains reports whether nodeID appears in voters - a plain
// linear scan, since voters is always the small (single-digit) size of
// this codebase's own raft membership, never worth indexing.
func voterListContains(voters []string, nodeID string) bool {
	for _, v := range voters {
		if v == nodeID {
			return true
		}
	}
	return false
}

// pendingJoinRequestExpired reports whether req is past its TTL, and is
// called only from ListPendingJoinRequests below, to keep an expired
// request out of the Admin's actionable list without deleting it. It is
// on the READ path and deliberately not on the apply path: it reads the
// wall clock, and an apply that consulted it would let a replay after
// the TTL settle differently from the node that applied the entry live
// (finding A15 in docs/audits/2026-09-26-code-audit.md, and the reason
// applyResolvePendingJoinRequest no longer checks expiry itself). The
// TTL itself is enforced by internal/manager, which checks the clock
// before submitting the command, so what the command carries is the
// decision rather than a question for the FSM. Nothing anywhere runs a
// background sweep/purge goroutine over expired records.
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

// RestartLeaseState returns the current lease (nil if none held) and the
// most recent completed-restart record (nil if never confirmed) for
// service - backs GetRestartLeaseStateLocal (ADR-0103). A plain read of
// already-replicated FSM state, safe to answer from any node's own
// local copy without leader routing.
func (f *FSM) RestartLeaseState(service string) (*internalpb.RestartLease, *internalpb.RestartRecord) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.restartLeases[service], f.restartRecords[service]
}

// ColonyUpdateState returns a SNAPSHOT COPY of the one active
// controlled-update record (nil when nothing is running) and of the
// settled history, newest first - backing GetColonyUpdateStateLocal
// (ADR-0145).
//
// A plain read of already-replicated FSM state, on the same
// any-node-can-answer footing as RestartLeaseState above and for the
// same reason. What it does NOT do is decide anything: every grant,
// advance and settle goes through Apply, so a copy of this on a lagging
// follower can mislead a reader but can never grant a lock. That gap is
// the reason the wire response carries an explicit authoritative flag
// rather than presenting a follower's copy as though it were the
// leader's.
//
// The records are CLONED rather than handed out by pointer, and that is
// not fastidiousness - it is a correctness requirement that the race
// detector found the hard way. applyAdvanceColonyUpdate mutates the
// active record in place (cheap, and correct under f.mu), so a caller
// holding the FSM's own pointer would be reading fields the apply loop
// is concurrently writing. ADR-0103's RestartLeaseState does not clone
// because its records are always REPLACED rather than mutated, which
// makes a handed-out pointer stable for the life of the object.
//
// A read that returns a live pointer into state that is still being
// written is how "the record said step=issue-restart" becomes a
// half-updated struct, so this clones.
func (f *FSM) ColonyUpdateState() (active *internalpb.ColonyUpdate, history []*internalpb.ColonyUpdate) {
	f.mu.Lock()
	defer f.mu.Unlock()
	active = cloneColonyUpdate(activeColonyUpdate(f.colonyUpdates))
	settled := settledColonyUpdates(f.colonyUpdates)
	history = make([]*internalpb.ColonyUpdate, 0, len(settled))
	for _, rec := range settled {
		history = append(history, cloneColonyUpdate(rec))
	}
	return active, history
}

// ColonyUpdateByID returns a snapshot copy of one operation's record by
// id, settled or not. It is what lets a replacement managerd ask about
// the operation it was told about rather than only about whatever is
// running now. Cloned for the same reason as ColonyUpdateState above.
func (f *FSM) ColonyUpdateByID(operationID string) (*internalpb.ColonyUpdate, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	rec, ok := f.colonyUpdates[operationID]
	if !ok {
		return nil, false
	}
	return cloneColonyUpdate(rec), true
}

// cloneColonyUpdate copies a record, nil in and nil out. A shallow Go
// copy would be wrong twice over: generated messages carry an internal
// mutex, and the nested Steps slice would still alias.
func cloneColonyUpdate(rec *internalpb.ColonyUpdate) *internalpb.ColonyUpdate {
	if rec == nil {
		return nil
	}
	cloned, ok := proto.Clone(rec).(*internalpb.ColonyUpdate)
	if !ok {
		return nil
	}
	return cloned
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
	// uplink_bridged reuses the node's own pre-existing uplink bridge
	// directly (see ADR-0101) - it can't also be tagged with a VLAN, use
	// a real external gateway (mutually exclusive network modes), or
	// override the bridge name (there is no Apiary-owned bridge for this
	// mode to name).
	if network.GetUplinkBridged() {
		if network.GetVlanId() != 0 {
			return &FSMApplyResult{Index: index, Error: "CreateNetwork: uplink_bridged networks must not set vlan_id (they share the host's own untagged uplink segment)"}
		}
		if network.GetExternalGateway() != "" {
			return &FSMApplyResult{Index: index, Error: "CreateNetwork: uplink_bridged and external_gateway are mutually exclusive"}
		}
		if network.GetBridgeName() != "" {
			return &FSMApplyResult{Index: index, Error: "CreateNetwork: uplink_bridged networks reuse each node's own -bhyve-bridge and cannot set bridge_name"}
		}
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

// jailHostnameConflict returns the id of an existing jail (other than
// excludeID) whose hostname already equals hostname, or "" if none
// does. Comparison is case-insensitive - jail(8) hostnames are DNS
// names, and "Web01.lan"/"web01.lan" naming two different jails would
// be exactly the ambiguous-identity outcome ADR-0106 exists to prevent,
// even though validHostname itself (shared with the unrelated Cloudflare
// interpolation-safety check above) does not normalize case. An empty
// hostname never conflicts with another empty hostname: it means "not
// set," not a shared identity, matching this field's existing optional
// status (no CreateJail/SetJailHostname caller has ever been required
// to set it).
func (f *FSM) jailHostnameConflict(hostname, excludeID string) string {
	if hostname == "" {
		return ""
	}
	want := strings.ToLower(hostname)
	for id, jail := range f.jails {
		if id == excludeID {
			continue
		}
		if strings.ToLower(jail.GetHostname()) == want {
			return id
		}
	}
	return ""
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

// StateDigest returns this node's canonical digest of its own FSM state
// (ADR-0143), as lowercase hex. The value is cached and recomputed on
// every Apply, so this is a field read under the lock rather than a
// re-serialisation of the whole state machine.
//
// Two voters whose digests are equal hold identical state. Two voters
// whose digests differ do not - which says the state machines
// disagreed, and deliberately does not say which one is wrong. Pair the
// digest with AppliedIndex above: a mismatch at equal indexes is a real
// disagreement, while a mismatch at differing indexes may only be a
// sample taken while the cluster was still applying entries.
//
// Never returns an empty string. "No state" is a state with a real
// digest; an empty digest therefore means this method was not called at
// all, and a caller can use "" to mean unobserved without a separate
// flag.
func (f *FSM) StateDigest() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.stateDigest
}

// recomputeStateDigestLocked refreshes the cached digest from the FSM's
// current state. The caller must hold f.mu, except in NewFSM where the
// FSM is not yet shared.
//
// The cost is proportional to the total state, paid once per applied
// command rather than once per Status call. That is the deliberate
// trade ADR-0143 records: Status is polled by the health check and the
// colony view, and making every poll re-serialise the state machine
// would make a diagnostic a load problem on exactly the large colonies
// that most need it.
func (f *FSM) recomputeStateDigestLocked() {
	f.stateDigest = statedigest.Of(f.snapshotStateLocked())
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

	return f.snapshotStateLocked()
}

// snapshotStateLocked is SnapshotState's body without the locking, so the
// digest recompute can reach it without taking the lock a second time. The
// caller must hold f.mu. The returned message shares the FSM's value
// pointers rather than deep-copying them, which is what Snapshot and
// Restore have always relied on and is safe for both callers: each one
// either encodes the message immediately or hands it to raft's own
// Persist, and both happen under this lock.
func (f *FSM) snapshotStateLocked() *internalpb.FSMSnapshotState {
	state := &internalpb.FSMSnapshotState{
		LastIndex:           f.lastIndex,
		Vms:                 make(map[string]*internalpb.VMDefinition, len(f.vms)),
		Networks:            make(map[string]*internalpb.NetworkDefinition, len(f.networks)),
		ApiKeys:             make(map[string]*internalpb.ApiKey, len(f.apiKeys)),
		Jails:               make(map[string]*internalpb.JailDefinition, len(f.jails)),
		PendingJoinRequests: make(map[string]*internalpb.PendingJoinRequest, len(f.pendingJoinRequests)),
		RestartLeases:       make(map[string]*internalpb.RestartLease, len(f.restartLeases)),
		RestartRecords:      make(map[string]*internalpb.RestartRecord, len(f.restartRecords)),
		ColonyUpdates:       make(map[string]*internalpb.ColonyUpdate, len(f.colonyUpdates)),
		ColonyJoinWindow:    cloneColonyJoinWindow(f.colonyJoinWindow),
		AuthEnabled:         f.authEnabled,
		TrustedPeers:        make(map[string]*internalpb.TrustedPeer, len(f.trustedPeers)),
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
	for service, lease := range f.restartLeases {
		state.RestartLeases[service] = lease
	}
	for service, record := range f.restartRecords {
		state.RestartRecords[service] = record
	}
	for id, rec := range f.colonyUpdates {
		state.ColonyUpdates[id] = rec
	}
	for id, peer := range f.trustedPeers {
		state.TrustedPeers[id] = peer
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
	f.restartLeases = state.GetRestartLeases()
	if f.restartLeases == nil {
		f.restartLeases = make(map[string]*internalpb.RestartLease)
	}
	f.restartRecords = state.GetRestartRecords()
	if f.restartRecords == nil {
		f.restartRecords = make(map[string]*internalpb.RestartRecord)
	}
	f.colonyUpdates = state.GetColonyUpdates()
	if f.colonyUpdates == nil {
		f.colonyUpdates = make(map[string]*internalpb.ColonyUpdate)
	}
	f.colonyJoinWindow = cloneColonyJoinWindow(state.GetColonyJoinWindow())
	f.trustedPeers = state.GetTrustedPeers()
	if f.trustedPeers == nil {
		f.trustedPeers = make(map[string]*internalpb.TrustedPeer)
	}
	f.authEnabled = state.GetAuthEnabled()
	// Recompute under the same lock: a Status call arriving after this
	// returns must see a digest of the restored state, never one left
	// over from the pre-restore state.
	f.recomputeStateDigestLocked()
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
