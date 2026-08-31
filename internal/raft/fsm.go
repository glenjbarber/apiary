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
	ApiKey  *internalpb.ApiKey
	Jail    *internalpb.JailDefinition
	Error   string
}

// FSM applies typed Command messages (see api/internalpb/state.proto)
// against an in-memory map of VM definitions, keyed by ID, plus
// similarly keyed maps of network definitions and API keys. This is
// the real ephemeral-state schema: cluster membership itself is
// handled by raft's own configuration mechanism (AddVoter/
// RemoveServer), not by the FSM.
type FSM struct {
	mu        sync.Mutex
	lastIndex uint64
	vms       map[string]*internalpb.VMDefinition
	networks  map[string]*internalpb.NetworkDefinition
	apiKeys   map[string]*internalpb.ApiKey
	jails     map[string]*internalpb.JailDefinition

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
		vms:      make(map[string]*internalpb.VMDefinition),
		networks: make(map[string]*internalpb.NetworkDefinition),
		apiKeys:  make(map[string]*internalpb.ApiKey),
		jails:    make(map[string]*internalpb.JailDefinition),
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

// applyCreateJail adds a new JailDefinition, mirroring applyCreateVM
// (minus the network-allocation step, since jails have no equivalent
// yet - see JailDefinition's doc comment).
func (f *FSM) applyCreateJail(index uint64, jail *internalpb.JailDefinition) *FSMApplyResult {
	if jail.GetId() == "" {
		return &FSMApplyResult{Index: index, Error: "CreateJail: id must be set"}
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

// applyPurgeJail mirrors applyPurgeVM exactly: idempotent, not an
// error if id is already gone.
func (f *FSM) applyPurgeJail(index uint64, id string) *FSMApplyResult {
	jail := f.jails[id]
	delete(f.jails, id)
	return &FSMApplyResult{Index: index, Jail: jail}
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
	f.apiKeys[key.GetId()] = key
	f.authEnabled = true
	return &FSMApplyResult{Index: index, ApiKey: key}
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
func (f *FSM) ValidateHash(hash string) (id string, ok bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, k := range f.apiKeys {
		if k.GetHashedKey() == hash {
			return k.GetId(), true
		}
	}
	return "", false
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
	f.mu.Lock()
	defer f.mu.Unlock()

	state := &internalpb.FSMSnapshotState{
		LastIndex:   f.lastIndex,
		Vms:         make(map[string]*internalpb.VMDefinition, len(f.vms)),
		Networks:    make(map[string]*internalpb.NetworkDefinition, len(f.networks)),
		ApiKeys:     make(map[string]*internalpb.ApiKey, len(f.apiKeys)),
		Jails:       make(map[string]*internalpb.JailDefinition, len(f.jails)),
		AuthEnabled: f.authEnabled,
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
	f.apiKeys = state.GetApiKeys()
	if f.apiKeys == nil {
		f.apiKeys = make(map[string]*internalpb.ApiKey)
	}
	f.jails = state.GetJails()
	if f.jails == nil {
		f.jails = make(map[string]*internalpb.JailDefinition)
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
