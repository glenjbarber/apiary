package raft

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"sort"
	"sync"

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
	Index   uint64
	VM      *internalpb.VMDefinition
	Network *internalpb.NetworkDefinition
	Error   string
}

// FSM applies typed Command messages (see api/internalpb/state.proto)
// against an in-memory map of VM definitions, keyed by ID, plus a
// similarly keyed map of network definitions. This is the real
// ephemeral-state schema: cluster membership itself is handled by
// raft's own configuration mechanism (AddVoter/RemoveServer), not by the
// FSM.
type FSM struct {
	mu        sync.Mutex
	lastIndex uint64
	vms       map[string]*internalpb.VMDefinition
	networks  map[string]*internalpb.NetworkDefinition
}

var _ raft.FSM = (*FSM)(nil)

// NewFSM returns an empty FSM.
func NewFSM() *FSM {
	return &FSM{
		vms:      make(map[string]*internalpb.VMDefinition),
		networks: make(map[string]*internalpb.NetworkDefinition),
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
	case *internalpb.Command_CreateNetwork:
		return f.applyCreateNetwork(log.Index, op.CreateNetwork.GetNetwork())
	case *internalpb.Command_DeleteNetwork:
		return f.applyDeleteNetwork(log.Index, op.DeleteNetwork.GetId())
	default:
		return &FSMApplyResult{Index: log.Index, Error: "command has no op set"}
	}
}

func (f *FSM) applyCreateVM(index uint64, vm *internalpb.VMDefinition) *FSMApplyResult {
	if vm.GetId() == "" {
		return &FSMApplyResult{Index: index, Error: "CreateVM: id must be set"}
	}
	if _, exists := f.vms[vm.GetId()]; exists {
		return &FSMApplyResult{Index: index, Error: fmt.Sprintf("CreateVM: id %q already exists", vm.GetId())}
	}

	if vm.GetNetworkId() != "" {
		network, ok := f.networks[vm.GetNetworkId()]
		if !ok {
			return &FSMApplyResult{Index: index, Error: fmt.Sprintf("CreateVM: network %q does not exist", vm.GetNetworkId())}
		}
		ip, err := f.allocateIP(network)
		if err != nil {
			return &FSMApplyResult{Index: index, Error: fmt.Sprintf("CreateVM: %v", err)}
		}
		vm = proto.Clone(vm).(*internalpb.VMDefinition)
		vm.IpAddress = ip
		vm.MacAddress = deriveMAC(vm.GetId())
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

// applyPurgeVM removes a VM definition outright. Idempotent: purging an
// id that's already gone is not an error, since the reconciler that
// submits this may retry after a partial failure (e.g. it purged
// successfully but never saw the response).
func (f *FSM) applyPurgeVM(index uint64, id string) *FSMApplyResult {
	vm := f.vms[id]
	delete(f.vms, id)
	return &FSMApplyResult{Index: index, VM: vm}
}

// applyCreateNetwork adds a new NetworkDefinition.
func (f *FSM) applyCreateNetwork(index uint64, network *internalpb.NetworkDefinition) *FSMApplyResult {
	if network.GetId() == "" {
		return &FSMApplyResult{Index: index, Error: "CreateNetwork: id must be set"}
	}
	if _, exists := f.networks[network.GetId()]; exists {
		return &FSMApplyResult{Index: index, Error: fmt.Sprintf("CreateNetwork: id %q already exists", network.GetId())}
	}
	if _, _, err := net.ParseCIDR(network.GetSubnet()); err != nil {
		return &FSMApplyResult{Index: index, Error: fmt.Sprintf("CreateNetwork: invalid subnet %q: %v", network.GetSubnet(), err)}
	}
	f.networks[network.GetId()] = network
	return &FSMApplyResult{Index: index, Network: network}
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
	f.mu.Lock()
	defer f.mu.Unlock()

	state := &internalpb.FSMSnapshotState{
		LastIndex: f.lastIndex,
		Vms:       make(map[string]*internalpb.VMDefinition, len(f.vms)),
		Networks:  make(map[string]*internalpb.NetworkDefinition, len(f.networks)),
	}
	for id, vm := range f.vms {
		state.Vms[id] = vm
	}
	for id, network := range f.networks {
		state.Networks[id] = network
	}
	return &fsmSnapshot{state: state}, nil
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
