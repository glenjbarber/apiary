package cluster

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"google.golang.org/protobuf/proto"

	internalpb "github.com/glenjbarber/apiary/api/internalpb"
	"github.com/glenjbarber/apiary/internal/bhyve"
	"github.com/glenjbarber/apiary/internal/dhcpd"
	"github.com/glenjbarber/apiary/internal/pf"
)

// diskImageName is the file created inside each VM's dataset to back its
// boot disk.
const diskImageName = "disk.img"

// defaultCPUs/defaultMemoryMB are used when a VMDefinition doesn't
// specify resources (both fields are optional today).
const (
	defaultCPUs     = 1
	defaultMemoryMB = 512
)

// phaseApplyTimeout bounds each phase/purge Apply call the reconciler
// makes on its own initiative (as opposed to in direct response to an
// external RPC, which sets its own timeout). These are best-effort
// status updates - see applyPhase - so a short, fixed timeout is enough.
const phaseApplyTimeout = 5 * time.Second

// Phase string constants mirror api/internalpb's VMPhase enum, kept as
// plain strings on VMPlacement so this package's core types don't need
// to import the wire schema - see VMPlacement's doc comment.
const (
	PhaseCreating = "creating"
	PhaseReady    = "ready"
	PhaseDeleting = "deleting"
	PhaseError    = "error"
)

// raftClient is the subset of *manager.RaftClient the reconciler needs.
// Defined locally (rather than importing internal/manager's concrete
// type into the signature) so tests can supply a fake without any real
// raft/gRPC machinery; *manager.RaftClient satisfies this today without
// any changes on its side. Apply is used for the reconciler's own
// phase/purge status updates (see applyPhase/purgeVM), not just to read
// the VM list.
type raftClient interface {
	ListVMs(ctx context.Context) (*internalpb.ListVMsResponse, error)
	Apply(ctx context.Context, payload []byte, timeout time.Duration) (*internalpb.ApplyResponse, error)

	// ListNetworks is only called when VLAN is set - see ensureNetwork.
	ListNetworks(ctx context.Context) (*internalpb.ListNetworksResponse, error)
}

// datasetManager is the subset of *zfs.Manager the reconciler needs, for
// the same reason as raftClient. *zfs.Manager satisfies this today.
type datasetManager interface {
	DatasetExists(ctx context.Context, name string) (bool, error)
	CreateDataset(ctx context.Context, name string) error
	DestroyDataset(ctx context.Context, name string) error
	GetProperty(ctx context.Context, name, prop string) (string, error)
}

// vmManager is the subset of *bhyve.Manager the reconciler needs, for
// the same reason as raftClient. *bhyve.Manager satisfies this today.
type vmManager interface {
	VMExists(ctx context.Context, name string) (bool, error)
	CreateVM(ctx context.Context, name string, cfg bhyve.Config) error
	DestroyVM(ctx context.Context, name string) error
}

// isoResolver is the subset of *isostore.Manager the reconciler needs,
// for the same reason as raftClient. *isostore.Manager satisfies this
// today.
type isoResolver interface {
	Path(name string) (string, bool, error)

	// IsISO9660 distinguishes real ISO9660 media (attach via ahci-cd)
	// from anything else, most notably a FreeBSD memstick image (attach
	// via ahci-hd instead) - see bhyve.Config.InstallDiskPath.
	IsISO9660(name string) (bool, error)
}

// vlanManager is the subset of *vlan.Manager the reconciler needs, for
// the same reason as raftClient. *vlan.Manager satisfies this today.
type vlanManager interface {
	EnsureVLAN(ctx context.Context, vlanID uint32) (string, error)
	EnsureBridge(ctx context.Context, name string) error
	EnsureMember(ctx context.Context, bridge, iface string) error
	EnsureBridgeAddress(ctx context.Context, bridge, subnet string) error
}

// dhcpManager is the subset of *dhcpd.Manager the reconciler needs, for
// the same reason as raftClient. *dhcpd.Manager satisfies this today.
type dhcpManager interface {
	WriteAndReload(ctx context.Context, scopes []dhcpd.NetworkScope) error
}

// pfManager is the subset of *pf.Manager the reconciler needs, for the
// same reason as raftClient. *pf.Manager satisfies this today.
type pfManager interface {
	Apply(ctx context.Context, anchor string, rules []pf.Rule) error
	Flush(ctx context.Context, anchor string) error
}

// Reconciler provisions local ZFS storage - and, if Bhyve is set, a
// running bhyve VM backed by that storage - for VMs assigned to this
// node, based on VMDefinition.node_id in raft's ephemeral state. It also
// tears both back down for a VM marked Deleting (see ADR-0016),
// reporting its progress back into raft's ephemeral state via Phase so
// external callers (e.g. the web UI) can show real reconciliation
// status rather than just the caller's original desired_state.
type Reconciler struct {
	Raft        raftClient
	ZFS         datasetManager
	LocalNodeID string

	// Bhyve is optional: nil disables VM provisioning entirely, leaving
	// this node doing dataset-only provisioning. This lets a node
	// without hardware-assisted virtualization (most of them, today -
	// see ADR-0010) run the reconciler safely, rather than failing every
	// tick trying to call bhyve(8) on hardware that can't run it.
	Bhyve vmManager

	// BootROM is the UEFI firmware path passed to every VM Bhyve
	// creates. Required if Bhyve is set.
	BootROM string

	// DiskSizeMB sizes the sparse disk image created for each VM's boot
	// disk. Defaults to 10240 (10GiB) if zero.
	DiskSizeMB uint64

	// Bridge, if set, is passed through to every VM Bhyve creates,
	// attaching it to this existing bridge(4) interface. Empty disables
	// networking - see internal/bhyve's own Config.Bridge doc comment.
	Bridge string

	// ISOs resolves a VM's ISOName to a local file path, for VMs that
	// name an installer image to boot from. Optional: nil (or an unset
	// ISOName) means no CD-ROM is attached, the same opt-in pattern as
	// Bhyve/Bridge above.
	ISOs isoResolver

	// VLAN, DHCP, and PF are optional (nil-able, same opt-in pattern as
	// Bhyve/ISOs above): together they realize a VM's NetworkID/
	// FirewallRules. VLAN provisions the vlan/bridge interfaces a
	// NetworkDefinition implies; DHCP serves real leases for VMs'
	// FSM-assigned IPs; PF loads a VM's FirewallRules into its own
	// pf(8) anchor. A VM naming a network on a node with VLAN unset (or
	// a network that can't be resolved) fails reconciliation with a
	// clear error, the same way an unresolvable ISO already does. See
	// ADR-0022.
	VLAN vlanManager
	DHCP dhcpManager
	PF   pfManager

	// lastDHCPConfig is the last dnsmasq config body actually written,
	// so reconcileDHCP only calls DHCP.WriteAndReload (which restarts
	// the dnsmasq service - see internal/dhcpd's own doc comment on why
	// that isn't a lighter-weight reload) when something has actually
	// changed, not every tick.
	lastDHCPConfig string
}

// RunOnce fetches the current VM list and, for each VM assigned to
// LocalNodeID, either provisions it (dataset, and if Bhyve is
// configured, a running VM) or - if it's marked Deleting - tears both
// back down and purges its record. It returns an error without touching
// anything if the VM list can't be fetched - reconciling against a
// partial or failed fetch is exactly the kind of mistake Plan's design
// note warns about avoiding. A failure partway through one VM is
// reported but does not stop the remaining VMs in this tick from being
// attempted.
func (r *Reconciler) RunOnce(ctx context.Context) error {
	resp, err := r.Raft.ListVMs(ctx)
	if err != nil {
		return fmt.Errorf("cluster: listing VMs: %w", err)
	}
	if resp.GetError() != "" {
		return fmt.Errorf("cluster: listing VMs: %s", resp.GetError())
	}

	desired := make([]VMPlacement, 0, len(resp.GetVms()))
	for _, vm := range resp.GetVms() {
		rules := make([]FirewallRule, 0, len(vm.GetFirewallRules()))
		for _, rule := range vm.GetFirewallRules() {
			rules = append(rules, FirewallRule{
				Direction: rule.GetDirection(),
				Action:    rule.GetAction(),
				Protocol:  rule.GetProtocol(),
				PortRange: rule.GetPortRange(),
			})
		}
		desired = append(desired, VMPlacement{
			ID:            vm.GetId(),
			NodeID:        vm.GetNodeId(),
			Vcpus:         vm.GetVcpus(),
			MemoryMB:      vm.GetMemoryMb(),
			Deleting:      vm.GetDesiredState() == internalpb.VMState_VM_STATE_DELETING,
			Phase:         phaseToString(vm.GetPhase()),
			ISOName:       vm.GetIsoName(),
			NetworkID:     vm.GetNetworkId(),
			IPAddress:     vm.GetIpAddress(),
			MACAddress:    vm.GetMacAddress(),
			FirewallRules: rules,
		})
	}

	// Networks are only fetched (and network reconciliation only
	// attempted) when VLAN is configured on this node - a node with no
	// VLAN support can't provision the vlan/bridge interfaces a network
	// implies anyway, so there's nothing useful this lookup could do
	// there (see ensureVM's own error for a VM naming a network in that
	// case).
	var networks map[string]*internalpb.NetworkDefinition
	if r.VLAN != nil {
		networks, err = r.fetchNetworks(ctx)
		if err != nil {
			return fmt.Errorf("cluster: listing networks: %w", err)
		}
	}

	planned := Plan(desired, r.LocalNodeID)

	var firstErr error
	for _, vm := range planned {
		if err := r.reconcileVM(ctx, vm, networks); err != nil && firstErr == nil {
			firstErr = fmt.Errorf("cluster: reconciling VM %s: %w", vm.ID, err)
		}
	}

	// Reclaim any local resources left over under a VM ID that's no
	// longer assigned to this node (e.g. it was reassigned elsewhere) -
	// see PlanReclaim's own doc comment for why this is safe to infer,
	// unlike inferring teardown from a VM disappearing from the list
	// entirely.
	for _, id := range PlanReclaim(desired, r.LocalNodeID) {
		if err := r.reclaimStaleVM(ctx, id); err != nil && firstErr == nil {
			firstErr = fmt.Errorf("cluster: reclaiming stale VM %s: %w", id, err)
		}
	}

	if r.DHCP != nil {
		if err := r.reconcileDHCP(ctx, planned, networks); err != nil && firstErr == nil {
			firstErr = fmt.Errorf("cluster: reconciling DHCP: %w", err)
		}
	}

	return firstErr
}

// fetchNetworks reads every current network definition from raft,
// keyed by id for ensureVM/reconcileDHCP's lookups.
func (r *Reconciler) fetchNetworks(ctx context.Context) (map[string]*internalpb.NetworkDefinition, error) {
	resp, err := r.Raft.ListNetworks(ctx)
	if err != nil {
		return nil, err
	}
	if resp.GetError() != "" {
		return nil, fmt.Errorf("%s", resp.GetError())
	}
	networks := make(map[string]*internalpb.NetworkDefinition, len(resp.GetNetworks()))
	for _, n := range resp.GetNetworks() {
		networks[n.GetId()] = n
	}
	return networks, nil
}

// reconcileVM dispatches to teardownVM or ensureVM depending on vm's
// tombstone state, wrapping ensureVM with the phase transitions that
// make reconciliation progress observable: "creating" before the first
// attempt, "ready" once it succeeds, "error" (with the failure message)
// if it doesn't. These are best-effort writes - see applyPhase - so a
// phase-update failure never masks or replaces the underlying
// provisioning error.
func (r *Reconciler) reconcileVM(ctx context.Context, vm VMPlacement, networks map[string]*internalpb.NetworkDefinition) error {
	if vm.Deleting {
		return r.teardownVM(ctx, vm)
	}

	if vm.Phase != PhaseReady && vm.Phase != PhaseCreating {
		r.applyPhase(ctx, vm.ID, PhaseCreating, "")
	}
	if err := r.ensureVM(ctx, vm, networks); err != nil {
		r.applyPhase(ctx, vm.ID, PhaseError, err.Error())
		return err
	}
	if vm.Phase != PhaseReady {
		r.applyPhase(ctx, vm.ID, PhaseReady, "")
	}
	return nil
}

// teardownVM destroys vm's real local resources (bhyve VM, then its
// dataset - the disk image lives inside the dataset, so destroying the
// dataset takes it with it) and, once both are confirmed gone, purges
// the VM's record entirely. Each step checks existence first so a
// teardown that's already partially done (e.g. a previous tick got the
// bhyve VM but failed on the dataset) converges instead of erroring on
// "already gone."
func (r *Reconciler) teardownVM(ctx context.Context, vm VMPlacement) error {
	if vm.Phase != PhaseDeleting {
		r.applyPhase(ctx, vm.ID, PhaseDeleting, "")
	}

	if r.Bhyve != nil {
		running, err := r.Bhyve.VMExists(ctx, vm.ID)
		if err != nil {
			return fmt.Errorf("checking bhyve VM: %w", err)
		}
		if running {
			if err := r.Bhyve.DestroyVM(ctx, vm.ID); err != nil {
				return fmt.Errorf("destroying bhyve VM: %w", err)
			}
		}
	}

	exists, err := r.ZFS.DatasetExists(ctx, vm.ID)
	if err != nil {
		return fmt.Errorf("checking dataset: %w", err)
	}
	if exists {
		if err := r.ZFS.DestroyDataset(ctx, vm.ID); err != nil {
			return fmt.Errorf("destroying dataset: %w", err)
		}
	}

	if r.PF != nil {
		if err := r.PF.Flush(ctx, vmAnchor(vm.ID)); err != nil {
			return fmt.Errorf("flushing firewall rules: %w", err)
		}
	}

	cmd := &internalpb.Command{
		Op: &internalpb.Command_PurgeVm{PurgeVm: &internalpb.PurgeVM{Id: vm.ID}},
	}
	data, err := proto.Marshal(cmd)
	if err != nil {
		return fmt.Errorf("marshaling PurgeVM: %w", err)
	}
	if _, err := r.Raft.Apply(ctx, data, phaseApplyTimeout); err != nil {
		return fmt.Errorf("purging VM record: %w", err)
	}
	return nil
}

// reclaimStaleVM tears down any local dataset/bhyve VM left over under
// id from before it was reassigned to a different node (see
// PlanReclaim). Unlike teardownVM, it never touches id's raft record -
// that belongs to whichever node currently owns it now, not this one -
// and it does nothing at all if no local resources exist under this id,
// which is the common case: most VMs in a cluster never touched this
// node in the first place, so every tick's reclaim pass is a cheap
// existence-check no-op for them.
func (r *Reconciler) reclaimStaleVM(ctx context.Context, id string) error {
	if r.Bhyve != nil {
		exists, err := r.Bhyve.VMExists(ctx, id)
		if err != nil {
			return fmt.Errorf("checking stale bhyve VM: %w", err)
		}
		if exists {
			if err := r.Bhyve.DestroyVM(ctx, id); err != nil {
				return fmt.Errorf("destroying stale bhyve VM: %w", err)
			}
		}
	}

	exists, err := r.ZFS.DatasetExists(ctx, id)
	if err != nil {
		return fmt.Errorf("checking stale dataset: %w", err)
	}
	if exists {
		if err := r.ZFS.DestroyDataset(ctx, id); err != nil {
			return fmt.Errorf("destroying stale dataset: %w", err)
		}
	}

	if r.PF != nil {
		if err := r.PF.Flush(ctx, vmAnchor(id)); err != nil {
			return fmt.Errorf("flushing stale firewall rules: %w", err)
		}
	}

	return nil
}

// applyPhase submits a best-effort UpdateVMPhase command reporting this
// reconciler's own progress on id. Failures are deliberately swallowed:
// this is status reporting, not the operation itself - a phase update
// that loses a race (e.g. against the VM being purged) or hits a
// transient raft error shouldn't turn into a reconciliation failure on
// its own. The next tick's phase comparison will simply try again.
func (r *Reconciler) applyPhase(ctx context.Context, id, phase, phaseError string) {
	cmd := &internalpb.Command{
		Op: &internalpb.Command_UpdateVmPhase{UpdateVmPhase: &internalpb.UpdateVMPhase{
			Id:         id,
			Phase:      phaseFromString(phase),
			PhaseError: phaseError,
		}},
	}
	data, err := proto.Marshal(cmd)
	if err != nil {
		return
	}
	_, _ = r.Raft.Apply(ctx, data, phaseApplyTimeout)
}

func phaseToString(p internalpb.VMPhase) string {
	switch p {
	case internalpb.VMPhase_VM_PHASE_CREATING:
		return PhaseCreating
	case internalpb.VMPhase_VM_PHASE_READY:
		return PhaseReady
	case internalpb.VMPhase_VM_PHASE_DELETING:
		return PhaseDeleting
	case internalpb.VMPhase_VM_PHASE_ERROR:
		return PhaseError
	default:
		return ""
	}
}

func phaseFromString(p string) internalpb.VMPhase {
	switch p {
	case PhaseCreating:
		return internalpb.VMPhase_VM_PHASE_CREATING
	case PhaseReady:
		return internalpb.VMPhase_VM_PHASE_READY
	case PhaseDeleting:
		return internalpb.VMPhase_VM_PHASE_DELETING
	case PhaseError:
		return internalpb.VMPhase_VM_PHASE_ERROR
	default:
		return internalpb.VMPhase_VM_PHASE_UNSPECIFIED
	}
}

// ensureVM ensures vm's dataset exists, then - if Bhyve is configured -
// that its disk image and bhyve VM exist too.
func (r *Reconciler) ensureVM(ctx context.Context, vm VMPlacement, networks map[string]*internalpb.NetworkDefinition) error {
	exists, err := r.ZFS.DatasetExists(ctx, vm.ID)
	if err != nil {
		return fmt.Errorf("checking dataset: %w", err)
	}
	if !exists {
		if err := r.ZFS.CreateDataset(ctx, vm.ID); err != nil {
			return fmt.Errorf("creating dataset: %w", err)
		}
	}

	if r.Bhyve == nil {
		return nil
	}

	running, err := r.Bhyve.VMExists(ctx, vm.ID)
	if err != nil {
		return fmt.Errorf("checking bhyve VM: %w", err)
	}
	if running {
		// The VM's own device config (disk/network/MAC) is fixed at
		// launch and can't be changed here, but firewall rules aren't a
		// bhyve device - they can (and should) be kept in sync on every
		// tick even for an already-running VM.
		if r.PF != nil {
			if err := r.PF.Apply(ctx, vmAnchor(vm.ID), toPFRules(vm.FirewallRules)); err != nil {
				return fmt.Errorf("applying firewall rules: %w", err)
			}
		}
		return nil
	}

	mountpoint, err := r.ZFS.GetProperty(ctx, vm.ID, "mountpoint")
	if err != nil {
		return fmt.Errorf("getting dataset mountpoint: %w", err)
	}

	diskPath, err := r.ensureDiskImage(mountpoint)
	if err != nil {
		return fmt.Errorf("preparing disk image: %w", err)
	}

	cpus := int(vm.Vcpus)
	if cpus == 0 {
		cpus = defaultCPUs
	}
	memoryMB := vm.MemoryMB
	if memoryMB == 0 {
		memoryMB = defaultMemoryMB
	}

	var isoPath, installDiskPath string
	if vm.ISOName != "" {
		if r.ISOs == nil {
			return fmt.Errorf("VM names ISO %q but no ISO store is configured on this node", vm.ISOName)
		}
		path, ok, err := r.ISOs.Path(vm.ISOName)
		if err != nil {
			return fmt.Errorf("resolving ISO %q: %w", vm.ISOName, err)
		}
		if !ok {
			return fmt.Errorf("ISO %q not found", vm.ISOName)
		}
		isGenuineISO, err := r.ISOs.IsISO9660(vm.ISOName)
		if err != nil {
			return fmt.Errorf("checking image format of %q: %w", vm.ISOName, err)
		}
		if isGenuineISO {
			isoPath = path
		} else {
			// Not a real ISO9660 filesystem - most likely a FreeBSD
			// memstick image, which is a raw bootable disk. Attaching it
			// via ahci-cd (like a real ISO) leaves firmware with no
			// ISO9660 filesystem to find, so it never boots.
			installDiskPath = path
		}
	}

	bridge := r.Bridge
	var macAddress string
	if vm.NetworkID != "" {
		if r.VLAN == nil {
			return fmt.Errorf("VM names network %q but no VLAN support is configured on this node", vm.NetworkID)
		}
		network, ok := networks[vm.NetworkID]
		if !ok {
			return fmt.Errorf("network %q not found", vm.NetworkID)
		}
		networkBridge, err := r.ensureNetwork(ctx, network)
		if err != nil {
			return fmt.Errorf("provisioning network %q: %w", vm.NetworkID, err)
		}
		bridge = networkBridge
		macAddress = vm.MACAddress
	}

	if err := r.Bhyve.CreateVM(ctx, vm.ID, bhyve.Config{
		CPUs:            cpus,
		MemoryMB:        memoryMB,
		BootROM:         r.BootROM,
		DiskPath:        diskPath,
		Bridge:          bridge,
		MACAddress:      macAddress,
		ISOPath:         isoPath,
		InstallDiskPath: installDiskPath,
		// Always on when Bhyve provisioning itself is enabled - a node
		// that can run bhyve VMs at all should always let their console
		// be viewed; there's no scenario where the tradeoff runs the
		// other way, so this isn't a separate opt-in flag.
		EnableVNC: true,
	}); err != nil {
		return fmt.Errorf("creating bhyve VM: %w", err)
	}

	if r.PF != nil {
		if err := r.PF.Apply(ctx, vmAnchor(vm.ID), toPFRules(vm.FirewallRules)); err != nil {
			return fmt.Errorf("applying firewall rules: %w", err)
		}
	}
	return nil
}

// ensureNetwork provisions the vlan(4)/bridge(4) interfaces network
// implies on this node (idempotent, called every tick like every other
// existence check in this file) and returns the bridge name to attach
// the VM's tap to.
func (r *Reconciler) ensureNetwork(ctx context.Context, network *internalpb.NetworkDefinition) (string, error) {
	iface, err := r.VLAN.EnsureVLAN(ctx, network.GetVlanId())
	if err != nil {
		return "", fmt.Errorf("ensuring vlan interface: %w", err)
	}
	bridge := networkBridgeName(network)
	if err := r.VLAN.EnsureBridge(ctx, bridge); err != nil {
		return "", fmt.Errorf("ensuring bridge: %w", err)
	}
	if err := r.VLAN.EnsureMember(ctx, bridge, iface); err != nil {
		return "", fmt.Errorf("adding %s to bridge: %w", iface, err)
	}
	if err := r.VLAN.EnsureBridgeAddress(ctx, bridge, network.GetSubnet()); err != nil {
		return "", fmt.Errorf("assigning gateway address: %w", err)
	}
	return bridge, nil
}

// networkBridgeName returns network's configured bridge name, or a
// derived default if unset - see NetworkDefinition's own doc comment on
// BridgeName. The derived name is a short hash of the network's id, not
// "apiary-net-<id>" directly: FreeBSD interface names are capped at
// IF_NAMESIZE (16 bytes including the trailing NUL, so 15 usable
// characters) - confirmed live ("apiary-net-net-1", 16 characters,
// failed with "ioctl SIOCSIFNAME: File name too long") - and a
// network id of arbitrary caller-chosen length can't be embedded
// directly and still fit.
func networkBridgeName(network *internalpb.NetworkDefinition) string {
	if network.GetBridgeName() != "" {
		return network.GetBridgeName()
	}
	sum := sha256.Sum256([]byte(network.GetId()))
	return fmt.Sprintf("apnet-%x", sum[:4]) // "apnet-" (6) + 8 hex chars = 14
}

// vmAnchor returns the pf(8) anchor name for a VM's own firewall rules.
func vmAnchor(id string) string {
	return "apiary/vm-" + id
}

// toPFRules converts VMPlacement's plain FirewallRule slice into
// internal/pf's own Rule type.
func toPFRules(rules []FirewallRule) []pf.Rule {
	out := make([]pf.Rule, len(rules))
	for i, r := range rules {
		out[i] = pf.Rule{Direction: r.Direction, Action: r.Action, Protocol: r.Protocol, PortRange: r.PortRange}
	}
	return out
}

// reconcileDHCP aggregates every local, non-deleting VM's network
// assignment into per-network dnsmasq scopes and reloads DHCP if the
// result actually changed since the last tick (see Reconciler's
// lastDHCPConfig doc comment on why that check exists).
func (r *Reconciler) reconcileDHCP(ctx context.Context, planned []VMPlacement, networks map[string]*internalpb.NetworkDefinition) error {
	scopeByNetwork := make(map[string]*dhcpd.NetworkScope)
	for _, vm := range planned {
		if vm.Deleting || vm.NetworkID == "" || vm.IPAddress == "" {
			continue
		}
		network, ok := networks[vm.NetworkID]
		if !ok {
			continue // ensureVM already surfaces this as a per-VM error
		}
		scope, ok := scopeByNetwork[vm.NetworkID]
		if !ok {
			scope = &dhcpd.NetworkScope{Bridge: networkBridgeName(network), Subnet: network.GetSubnet()}
			scopeByNetwork[vm.NetworkID] = scope
		}
		scope.Leases = append(scope.Leases, dhcpd.Lease{MAC: vm.MACAddress, IP: vm.IPAddress, Hostname: vm.ID})
	}

	scopes := make([]dhcpd.NetworkScope, 0, len(scopeByNetwork))
	for _, s := range scopeByNetwork {
		scopes = append(scopes, *s)
	}
	sort.Slice(scopes, func(i, j int) bool { return scopes[i].Bridge < scopes[j].Bridge })

	rendered, err := dhcpd.RenderConfig(scopes)
	if err != nil {
		return fmt.Errorf("rendering dnsmasq config: %w", err)
	}
	if rendered == r.lastDHCPConfig {
		return nil
	}
	if err := r.DHCP.WriteAndReload(ctx, scopes); err != nil {
		return err
	}
	r.lastDHCPConfig = rendered
	return nil
}

// ensureDiskImage creates a sparse disk image inside mountpoint if one
// doesn't already exist, and returns its path.
func (r *Reconciler) ensureDiskImage(mountpoint string) (string, error) {
	path := filepath.Join(mountpoint, diskImageName)
	if _, err := os.Stat(path); err == nil {
		return path, nil
	} else if !os.IsNotExist(err) {
		return "", err
	}

	sizeMB := r.DiskSizeMB
	if sizeMB == 0 {
		sizeMB = 10240
	}

	f, err := os.Create(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	if err := f.Truncate(int64(sizeMB) * 1024 * 1024); err != nil {
		return "", err
	}
	return path, nil
}
