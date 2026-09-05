package cluster

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"sync/atomic"
	"time"

	"google.golang.org/protobuf/proto"

	internalpb "github.com/glenjbarber/apiary/api/internalpb"
	"github.com/glenjbarber/apiary/internal/bhyve"
	"github.com/glenjbarber/apiary/internal/cloudflare"
	"github.com/glenjbarber/apiary/internal/dhcpd"
	"github.com/glenjbarber/apiary/internal/jail"
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
//
// ListVMsLocal/ListNetworksLocal (not ListVMs/ListNetworks) are used
// deliberately: those are leader-only, and the reconciler runs on every
// node regardless of leadership - a real, previously-latent gap only
// found live the first time a genuine second raft node's own
// Reconciler.RunOnce needed to read the VM list (every earlier
// deployment had exactly one node, always the leader, so this never
// mattered) - see raftd.proto's doc comment on these two RPCs and
// ADR-0026.
type raftClient interface {
	ListVMsLocal(ctx context.Context) (*internalpb.ListVMsResponse, error)
	Apply(ctx context.Context, payload []byte, timeout time.Duration) (*internalpb.ApplyResponse, error)

	// ListNetworksLocal is only called when VLAN is set - see ensureNetwork.
	ListNetworksLocal(ctx context.Context) (*internalpb.ListNetworksResponse, error)

	// ListJailsLocal is fetched even when jail provisioning is disabled,
	// so assigned records can report errors and tombstones can be processed.
	ListJailsLocal(ctx context.Context) (*internalpb.ListJailsResponse, error)

	// Status is only called when HAST is set - see
	// Reconciler.resolvePeerAddresses, which reuses raft's own cluster
	// membership addresses as hast.conf's Node.Remote.
	Status(ctx context.Context) (*internalpb.StatusResponse, error)
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
	ApplyNAT(ctx context.Context, anchor, subnet, uplink string) error
}

// cloudflareManager is the subset of *cloudflare.Manager the reconciler
// needs, for the same reason as raftClient - lets tests inject a fake
// with no real outbound HTTP calls or daemon(8)/cloudflared processes.
// *cloudflare.Manager satisfies this today. See ADR-0063.
type cloudflareManager interface {
	ReconcileExposures(ctx context.Context, token, zoneID, tunnelTarget, tunnelID, credentialsFile string, desired []cloudflare.DesiredExposure) error
	StopIfRunning(ctx context.Context) error
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

	// Uplink is this node's own physical internet-facing interface
	// (e.g. "re0", "em0" - the same value passed to VLAN's own Uplink).
	// When set (and PF is set, and a network has no ExternalGateway),
	// ensureNetwork gives that network's own subnet outbound NAT
	// through this interface - see ADR-0048. This is what makes an
	// Apiary-managed network self-sufficient for real internet access
	// without depending on any external router or physical VLAN
	// trunking - empty disables it, leaving a network's VMs with only
	// local (this-node) connectivity, matching today's behavior before
	// this field existed.
	Uplink string

	// DNSServer, if set, is handed to every Apiary-managed network's DHCP
	// clients via option 6 (see dhcpd.NetworkScope.DNSServer's own doc
	// comment for why this is required, not optional-with-a-sane-
	// default: dnsmasq's own default behavior of advertising itself as
	// DNS server is actively wrong here, since port=0 disables its
	// resolver). Empty preserves the old (buggy) behavior for a
	// deployment that hasn't set this yet, rather than silently start
	// emitting an option with an empty address.
	DNSServer string

	// HAST is optional (nil-able, same opt-in pattern as everything
	// above): when set, a VM naming ReplicaNodeID gets its disk
	// HAST-replicated instead of the plain dataset-backed file - real
	// data redundancy, not automatic failover (see ADR-0026). A VM with
	// no ReplicaNodeID is completely unaffected by whether HAST is set.
	HAST hastManager

	// HASTRestartSettleDelay overrides how long reconcileHASTRoles waits
	// after a real hastd restart before attempting a role change
	// (defaults to defaultHASTRestartSettleDelay if zero). Tests set
	// this to a small non-zero value so they don't pay the real-world
	// settle cost the live hastd startup race actually needs.
	HASTRestartSettleDelay time.Duration

	// Cloudflare is optional (nil-able, same opt-in pattern as HAST/
	// Bhyve/etc. above): when set, VMs naming CloudflareHostname have
	// their public exposure reconciled into this node's own pre-
	// provisioned Cloudflare Tunnel - see ADR-0063. A VM naming
	// CloudflareHostname on a node with this unset is simply never
	// exposed - no error, the same "opt-in capability, quietly a no-op
	// when absent" pattern this codebase already applies elsewhere
	// (e.g. an ISO name on a node with no isostore configured). When
	// nil, reconcileCloudflareTunnel still calls StopIfRunning against
	// a Manager with default paths, so a leftover process from before
	// the feature was disabled gets cleaned up (ADR-0063 finding 8).
	Cloudflare cloudflareManager

	// CloudflareToken/CloudflareZoneID/CloudflareTunnelID/
	// CloudflareTunnelCredentialsFile configure the pre-provisioned
	// Tunnel this node's own cloudflared process authenticates as, and
	// the Cloudflare API token used to manage DNS records - see
	// cmd/managerd's own -cloudflare-* flags. All required together
	// when Cloudflare is set.
	CloudflareToken                 string
	CloudflareZoneID                string
	CloudflareTunnelID              string
	CloudflareTunnelCredentialsFile string

	// lastDHCPConfig is the last dnsmasq config body actually written,
	// so reconcileDHCP only calls DHCP.WriteAndReload (which restarts
	// the dnsmasq service - see internal/dhcpd's own doc comment on why
	// that isn't a lighter-weight reload) when something has actually
	// changed, not every tick.
	lastDHCPConfig string

	// lastHASTConfig mirrors lastDHCPConfig exactly, for hast.conf -
	// see reconcileHASTRoles.
	lastHASTConfig string

	// hastConfigWritten is false until this Reconciler has written a
	// HAST config at least once in this process's lifetime. Needed
	// because lastHASTConfig's zero value ("") is itself a valid
	// rendered config (zero resources) - without this flag, a freshly
	// restarted managerd whose current target happens to be "no HAST
	// resources on this node" would see rendered == lastHASTConfig
	// (both "") and skip WriteConfig/RestartService entirely, even
	// though the actual on-disk hast.conf/running hastd might still
	// reflect resources from before the restart. Caught live: a
	// managerd restart during a replicated jail's teardown left hastd
	// running with the jail's now-stale resource still loaded, forever
	// blocking the reconciler's own next step (destroying that
	// resource's ZFS dataset) with "pool or dataset is busy" - see
	// ADR-0027.
	hastConfigWritten bool

	// Jail is the lifecycle driver, including explicit deletion. A nil
	// driver cannot create or safely tear down a jail. A node with
	// Jail set but no HAST support can still run non-replicated jails -
	// only a jail naming ReplicaNodeID requires HAST/Mount too.
	Jail jailManager

	// JailProvisioningDisabled prevents creating jails and provisioning
	// their replicas, but retains the driver for explicit owner tombstones.
	// Disabling provisioning must not strand previously assigned records.
	JailProvisioningDisabled bool

	// Mount formats/mounts a HAST-replicated jail's root filesystem -
	// only ever consulted for a jail naming ReplicaNodeID (see
	// ensureJail). nil disables replicated-jail support even if Jail
	// and HAST are both set, the same opt-in pattern as everything else.
	Mount mountManager

	// JailBase is the parent directory replicated jails are mounted
	// under (defaults to "/apiary-jails" if empty - see jailBase()). A
	// non-replicated jail's root is its own ZFS dataset's mountpoint
	// instead, so this only matters when Mount is actually used.
	JailBase string

	// JailDiskSizeMB sizes a replicated jail's HAST-backed root
	// filesystem (defaults to 2048 if zero - see jailDiskSizeMB()). A
	// non-replicated jail's root has no separate size of its own; it's
	// whatever its ZFS dataset allows.
	JailDiskSizeMB uint64

	// Peers is optional (nil-able, same opt-in pattern as everything
	// above): when set, a raft write rejected because this node's own
	// raftd isn't the current leader is forwarded to the leader node's
	// managerd instead of silently failing (ADR-0029). nil preserves
	// the pre-ADR-0029 behavior exactly (local-only, visibly failing
	// via the ApplyResponse.Error check added alongside ADR-0028's own
	// fix) - useful for tests, and harmless on a single-node deployment
	// where the local node is always the leader anyway.
	Peers peerReporter

	// PeerManagerdPort overrides the port assumed for a peer's managerd
	// external API when forwarding via Peers (defaults to
	// defaultPeerManagerdPort - see resolvePeerManagerdAddr).
	PeerManagerdPort string

	// Interval is this node's own configured -reconcile-interval - set
	// once at construction, read by internal/manager's HostStats handler
	// to derive Evidence-Aware Health's (ADR-0056) per-node reconcile
	// freshness limit. Purely descriptive: RunOnce itself doesn't use
	// this to schedule anything - cmd/managerd's own ticker does that.
	Interval time.Duration

	// lastAttemptUnix/lastSuccessUnix record RunOnce's own attempt/
	// success history in UnixNano, for Evidence-Aware Health's "last
	// successful reconciliation" signal. atomic, not mutex-protected:
	// RunOnce is only ever called from cmd/managerd's own single ticker
	// loop, but LastReconcileAttempt/LastReconcileSuccess below are read
	// concurrently from RPC-handling goroutines. Zero means "never
	// observed" - see the accessors' own doc comments.
	lastAttemptUnix atomic.Int64
	lastSuccessUnix atomic.Int64
}

// LastReconcileAttempt reports the last time RunOnce was called at all
// (regardless of outcome), and whether any attempt has ever been
// observed. A node whose raft is fully down still "tries" every tick -
// that attempt is itself real, citable evidence, recorded before RunOnce
// does anything else.
func (r *Reconciler) LastReconcileAttempt() (time.Time, bool) {
	return unixNanoToTime(r.lastAttemptUnix.Load())
}

// LastReconcileSuccess reports the last time RunOnce completed with no
// error at all (its own firstErr was nil), and whether any success has
// ever been observed. This is a whole-tick fact, not a per-resource one -
// see RunOnce's own doc comment on firstErr.
func (r *Reconciler) LastReconcileSuccess() (time.Time, bool) {
	return unixNanoToTime(r.lastSuccessUnix.Load())
}

// ReconcileInterval returns Interval - a plain accessor so callers outside
// this package (internal/manager) don't need to read the field directly,
// matching this type's other accessor methods.
func (r *Reconciler) ReconcileInterval() time.Duration {
	return r.Interval
}

// CloudflareConfigured reports whether Cloudflare Tunnel exposure
// (ADR-0063) is enabled on this node at all - backs
// HostStatsResponse.cloudflare_configured, mirroring bhyve_configured's
// own "proves configured, not currently working" framing.
func (r *Reconciler) CloudflareConfigured() bool {
	return r.Cloudflare != nil
}

func unixNanoToTime(nano int64) (time.Time, bool) {
	if nano == 0 {
		return time.Time{}, false
	}
	return time.Unix(0, nano), true
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
func (r *Reconciler) RunOnce(ctx context.Context) (err error) {
	// Recorded unconditionally, before anything else - a node whose raft
	// is fully down still "tries" every tick, and that attempt is itself
	// real, citable evidence for Evidence-Aware Health (ADR-0056), not
	// something to skip recording just because the tick then fails
	// immediately.
	r.lastAttemptUnix.Store(time.Now().UnixNano())
	defer func() {
		if err == nil {
			r.lastSuccessUnix.Store(time.Now().UnixNano())
		}
	}()

	resp, listErr := r.Raft.ListVMsLocal(ctx)
	if listErr != nil {
		return fmt.Errorf("cluster: listing VMs: %w", listErr)
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
			ID:                 vm.GetId(),
			NodeID:             vm.GetNodeId(),
			Vcpus:              vm.GetVcpus(),
			MemoryMB:           vm.GetMemoryMb(),
			Deleting:           vm.GetDesiredState() == internalpb.VMState_VM_STATE_DELETING,
			Phase:              phaseToString(vm.GetPhase()),
			ISOName:            vm.GetIsoName(),
			NetworkID:          vm.GetNetworkId(),
			IPAddress:          vm.GetIpAddress(),
			MACAddress:         vm.GetMacAddress(),
			FirewallRules:      rules,
			ReplicaNodeID:      vm.GetReplicaNodeId(),
			BaseImageName:      vm.GetBaseImageName(),
			FirewallPaused:     vm.GetFirewallPaused(),
			CloudflareHostname: vm.GetCloudflareHostname(),
			CloudflarePort:     vm.GetCloudflarePort(),
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

	// Always observe jail intent. Disabled provisioning still needs to
	// report an error for assigned jails and process explicit tombstones.
	var desiredJails []JailPlacement
	{
		jailsResp, err := r.Raft.ListJailsLocal(ctx)
		if err != nil {
			return fmt.Errorf("cluster: listing jails: %w", err)
		}
		if jailsResp.GetError() != "" {
			return fmt.Errorf("cluster: listing jails: %s", jailsResp.GetError())
		}
		for _, j := range jailsResp.GetJails() {
			// timemachine is explicitly outside Apiary's lifecycle, even
			// if an old or mistaken replicated record names it.
			if jail.IsProtected(j.GetId()) {
				continue
			}
			desiredJails = append(desiredJails, JailPlacement{
				ID:            j.GetId(),
				Name:          j.GetName(),
				Hostname:      j.GetHostname(),
				NodeID:        j.GetNodeId(),
				Deleting:      j.GetDesiredState() == internalpb.JailState_JAIL_STATE_DELETING,
				Phase:         jailPhaseToString(j.GetPhase()),
				ReplicaNodeID: j.GetReplicaNodeId(),
			})
		}
	}

	planned := Plan(desired, r.LocalNodeID)
	replicas := PlanReplica(desired, r.LocalNodeID)
	plannedJails := PlanJail(desiredJails, r.LocalNodeID)
	jailReplicas := PlanJailReplica(desiredJails, r.LocalNodeID)

	var firstErr error

	// HAST provisioning happens before ensureVM/ensureJail below, since
	// a primary-role resource's disk/device path comes from here (see
	// reconcileHASTRoles). Skipped entirely if HAST isn't configured on
	// this node - a VM/jail naming ReplicaNodeID in that case surfaces
	// its own clear error from ensureVM/ensureJail instead. VM and jail
	// roles are combined into ONE call: hast.conf holds every resource
	// this node participates in at once, so building it from two
	// separate calls would have the second overwrite (and restart
	// hastd against) whatever the first just wrote.
	var hastDevicePaths map[string]string
	if r.HAST != nil {
		var roles []hastRole
		for _, vm := range planned {
			// A Deleting VM is being torn down (teardownVM's own
			// reclaimHASTRole call, below) this same tick, not ensured -
			// caught live: including it here raced its own teardown
			// within one RunOnce, ensuring a provider that then got
			// destroyed out from under it moments later (see ADR-0026).
			if vm.ReplicaNodeID != "" && !vm.Deleting {
				roles = append(roles, hastRole{resourceName: vmHASTResourceName(vm.ID), peerNodeID: vm.ReplicaNodeID, sizeMB: r.diskSizeMB(), isPrimary: true})
			}
		}
		for _, vm := range replicas {
			roles = append(roles, hastRole{resourceName: vmHASTResourceName(vm.ID), peerNodeID: vm.NodeID, sizeMB: r.diskSizeMB(), isPrimary: false})
		}
		for _, j := range plannedJails {
			if r.Jail != nil && !r.JailProvisioningDisabled && j.ReplicaNodeID != "" && !j.Deleting {
				roles = append(roles, hastRole{resourceName: jailHASTResourceName(j.ID), peerNodeID: j.ReplicaNodeID, sizeMB: r.jailDiskSizeMB(), isPrimary: true})
			}
		}
		for _, j := range jailReplicas {
			if r.Jail != nil && !r.JailProvisioningDisabled {
				roles = append(roles, hastRole{resourceName: jailHASTResourceName(j.ID), peerNodeID: j.NodeID, sizeMB: r.jailDiskSizeMB(), isPrimary: false})
			}
		}
		paths, err := r.reconcileHASTRoles(ctx, roles)
		hastDevicePaths = paths
		if err != nil && firstErr == nil {
			firstErr = fmt.Errorf("cluster: reconciling HAST roles: %w", err)
		}
	}

	for _, vm := range planned {
		if err := r.reconcileVM(ctx, vm, networks, hastDevicePaths); err != nil && firstErr == nil {
			firstErr = fmt.Errorf("cluster: reconciling VM %s: %w", vm.ID, err)
		}
	}

	for _, j := range plannedJails {
		if err := r.reconcileJail(ctx, j, hastDevicePaths); err != nil && firstErr == nil {
			firstErr = fmt.Errorf("cluster: reconciling jail %s: %w", j.ID, err)
		}
	}

	// Reclaim any local resources left over under a VM ID that's no
	// longer assigned to this node (e.g. it was reassigned elsewhere) -
	// see PlanReclaim's own doc comment for why this is safe to infer,
	// unlike inferring teardown from a VM disappearing from the list
	// entirely.
	//
	// replicaIDs marks VMs this node currently holds a legitimate HAST
	// secondary role for (from PlanReplica above) - PlanReclaim's own
	// candidate set only checks NodeID, so it naturally also includes
	// every VM this node is replicating for someone else (NodeID is
	// never this node's own for those). reclaimStaleVM must not treat
	// that legitimate secondary-role zvol as stale and destroy it -
	// caught live, the mirror image of the primary-side bug PlanReplicaReclaim's
	// own doc comment describes (see ADR-0026).
	replicaIDs := make(map[string]bool, len(replicas))
	for _, vm := range replicas {
		replicaIDs[vm.ID] = true
	}
	for _, id := range PlanReclaim(desired, r.LocalNodeID) {
		if err := r.reclaimStaleVM(ctx, id, replicaIDs[id]); err != nil && firstErr == nil {
			firstErr = fmt.Errorf("cluster: reclaiming stale VM %s: %w", id, err)
		}
	}

	// Reclaim a HAST secondary-role resource this node no longer needs
	// to hold (the VM was reassigned to a different replica, or is
	// gone) - mirrors PlanReclaim's reasoning exactly, against
	// ReplicaNodeID instead of NodeID. Safe even when r.HAST is nil:
	// reclaimHASTRole only ever destroys a zvol that's actually there.
	for _, id := range PlanReplicaReclaim(desired, r.LocalNodeID) {
		if err := r.reclaimHASTRole(ctx, vmHASTResourceName(id)); err != nil && firstErr == nil {
			firstErr = fmt.Errorf("cluster: reclaiming stale HAST replica %s: %w", id, err)
		}
	}

	// Jail reclaim passes mirror the VM ones exactly, against
	// desiredJails/plannedJails/jailReplicas instead.
	if r.Jail != nil && !r.JailProvisioningDisabled {
		jailReplicaIDs := make(map[string]bool, len(jailReplicas))
		for _, j := range jailReplicas {
			jailReplicaIDs[j.ID] = true
		}
		for _, id := range PlanJailReclaim(desiredJails, r.LocalNodeID) {
			if err := r.reclaimStaleJail(ctx, id, jailReplicaIDs[id]); err != nil && firstErr == nil {
				firstErr = fmt.Errorf("cluster: reclaiming stale jail %s: %w", id, err)
			}
		}
		for _, id := range PlanJailReplicaReclaim(desiredJails, r.LocalNodeID) {
			if err := r.reclaimHASTRole(ctx, jailHASTResourceName(id)); err != nil && firstErr == nil {
				firstErr = fmt.Errorf("cluster: reclaiming stale jail HAST replica %s: %w", id, err)
			}
		}
	}

	if r.DHCP != nil {
		if err := r.reconcileDHCP(ctx, planned, networks); err != nil && firstErr == nil {
			firstErr = fmt.Errorf("cluster: reconciling DHCP: %w", err)
		}
	}

	if err := r.reconcileCloudflareTunnel(ctx, planned); err != nil && firstErr == nil {
		firstErr = fmt.Errorf("cluster: reconciling cloudflare tunnel: %w", err)
	}

	return firstErr
}

// reconcileCloudflareTunnel reconciles every locally-owned VM's public
// exposure into this node's own pre-provisioned Cloudflare Tunnel (see
// ADR-0063), aggregating every exposed VM into one
// cloudflare.Manager.ReconcileExposures call - mirroring
// reconcileHASTRoles's own "aggregate every relevant resource into one
// call, not one per resource" shape, since cloudflared's own config
// holds every exposed Cell this node currently owns at once. planned is
// already filtered to this node's own owned, non-deleting VMs (see
// Plan) - a VM entering VM_STATE_DELETING drops out of this list (and
// so out of its desired exposure) even before its physical resources
// are torn down, which is the correct behavior: stop advertising a
// service that's about to be deleted, don't wait for full teardown.
func (r *Reconciler) reconcileCloudflareTunnel(ctx context.Context, planned []VMPlacement) error {
	if r.Cloudflare == nil {
		return (&cloudflare.Manager{}).StopIfRunning(ctx)
	}

	var desired []cloudflare.DesiredExposure
	for _, vm := range planned {
		if vm.CloudflareHostname == "" {
			continue
		}
		if vm.IPAddress == "" {
			// network_id/ip_address can drift independently of when
			// SetVMCloudflareExposure last ran (ADR-0063 finding 4) -
			// skip rather than build a broken ingress entry pointing
			// nowhere.
			continue
		}
		desired = append(desired, cloudflare.DesiredExposure{
			VMID:     vm.ID,
			Hostname: vm.CloudflareHostname,
			Address:  fmt.Sprintf("%s:%d", vm.IPAddress, vm.CloudflarePort),
		})
	}

	tunnelTarget := r.CloudflareTunnelID + ".cfargotunnel.com"
	return r.Cloudflare.ReconcileExposures(ctx, r.CloudflareToken, r.CloudflareZoneID, tunnelTarget, r.CloudflareTunnelID, r.CloudflareTunnelCredentialsFile, desired)
}

// fetchNetworks reads every current network definition from raft,
// keyed by id for ensureVM/reconcileDHCP's lookups.
func (r *Reconciler) fetchNetworks(ctx context.Context) (map[string]*internalpb.NetworkDefinition, error) {
	resp, err := r.Raft.ListNetworksLocal(ctx)
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
func (r *Reconciler) reconcileVM(ctx context.Context, vm VMPlacement, networks map[string]*internalpb.NetworkDefinition, hastDevicePaths map[string]string) error {
	if vm.Deleting {
		return r.teardownVM(ctx, vm)
	}

	if vm.Phase != PhaseReady && vm.Phase != PhaseCreating {
		r.applyPhase(ctx, vm.ID, PhaseCreating, "")
	}
	if err := r.ensureVM(ctx, vm, networks, hastDevicePaths); err != nil {
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

	if vm.ReplicaNodeID != "" {
		if err := r.reclaimHASTRole(ctx, vmHASTResourceName(vm.ID)); err != nil {
			return fmt.Errorf("reclaiming HAST resource: %w", err)
		}
	} else {
		exists, err := r.ZFS.DatasetExists(ctx, vm.ID)
		if err != nil {
			return fmt.Errorf("checking dataset: %w", err)
		}
		if exists {
			if err := r.ZFS.DestroyDataset(ctx, vm.ID); err != nil {
				return fmt.Errorf("destroying dataset: %w", err)
			}
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
	resp, err := r.Raft.Apply(ctx, data, phaseApplyTimeout)
	if err != nil {
		return fmt.Errorf("purging VM record: %w", err)
	}
	// A non-nil transport err above only covers the gRPC call to raftd
	// itself failing - a rejected Apply (e.g. this node's own raftd
	// isn't the current raft leader) comes back as a normal, non-error
	// ApplyResponse with Error set instead, and was silently ignored
	// here until caught live (ADR-0028): a jail migrated onto a node
	// that wasn't also the raft leader had its real jail(8)/dataset
	// torn down correctly, but its raft record never got purged.
	// Checking the error here surfaces a genuine rejection instead of
	// hiding it; Peers (ADR-0029), when configured, gives a "not
	// leader" rejection specifically a real path to still succeed, by
	// forwarding the same purge to the leader's own managerd instead.
	if resp.GetError() != "" {
		if r.Peers != nil && resp.GetLeaderHint() != "" {
			addr := r.resolvePeerManagerdAddr(resp.GetLeaderHint())
			if perr := r.Peers.ReportVMTeardownComplete(ctx, addr, vm.ID); perr != nil {
				return fmt.Errorf("purging VM record via peer %s: %w", addr, perr)
			}
			return nil
		}
		return fmt.Errorf("purging VM record: %s", resp.GetError())
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
func (r *Reconciler) reclaimStaleVM(ctx context.Context, id string, isCurrentReplica bool) error {
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

	// A reassigned VM that was HAST-replicated while owned here used a
	// zvol (vmHASTResourceName), not the plain dataset above - check for
	// that too, since PlanReclaim only gives an id, not whether it used
	// to be replicated. Skipped when this node is the VM's CURRENT
	// legitimate HAST secondary (isCurrentReplica) - that's the exact
	// same zvol this tick's PlanReplica pass just ensured, not a stale
	// leftover from a past reassignment (caught live - see ADR-0026).
	if !isCurrentReplica {
		if err := r.reclaimHASTRole(ctx, vmHASTResourceName(id)); err != nil {
			return fmt.Errorf("reclaiming stale HAST resource: %w", err)
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
	resp, err := r.Raft.Apply(ctx, data, phaseApplyTimeout)
	if err != nil || resp.GetError() == "" {
		return
	}
	// Still best-effort even with Peers configured (ADR-0029): a lost
	// phase-update race is genuinely not worth failing reconciliation
	// over (see this function's own doc comment), so a failed peer
	// forward is swallowed here too, same as a failed local Apply
	// always was.
	if r.Peers != nil && resp.GetLeaderHint() != "" {
		_ = r.Peers.ReportVMPhase(ctx, r.resolvePeerManagerdAddr(resp.GetLeaderHint()), id, phase, phaseError)
	}
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

// resolveLocalImagePath resolves name (an ISOName or BaseImageName) to
// a local path via r.ISOs, automatically fetching it from whichever
// known peer already has it (ADR-0041) if this node's own isostore
// doesn't have it yet, rather than failing outright the way a caller
// used to have to work around with a manual, browser-triggered copy
// (ADR-0040). r.ISOs must be non-nil - callers already check that
// before naming an image at all.
func (r *Reconciler) resolveLocalImagePath(ctx context.Context, name string) (string, error) {
	path, ok, err := r.ISOs.Path(name)
	if err != nil {
		return "", err
	}
	if ok {
		return path, nil
	}
	if err := r.fetchImageFromPeer(ctx, name); err != nil {
		return "", err
	}
	path, ok, err = r.ISOs.Path(name)
	if err != nil {
		return "", fmt.Errorf("resolving after fetch: %w", err)
	}
	if !ok {
		return "", fmt.Errorf("still not found locally after a reported-successful fetch from a peer")
	}
	return path, nil
}

// ensureVM ensures vm's disk exists - a plain dataset-backed file, or,
// if ReplicaNodeID is set, a HAST-replicated device instead (see
// hastDevicePaths/ADR-0026; no dataset is created in that case, there's
// nothing useful for it to hold) - then, if Bhyve is configured, that
// its bhyve VM exists too.
func (r *Reconciler) ensureVM(ctx context.Context, vm VMPlacement, networks map[string]*internalpb.NetworkDefinition, hastDevicePaths map[string]string) error {
	if vm.ReplicaNodeID == "" {
		exists, err := r.ZFS.DatasetExists(ctx, vm.ID)
		if err != nil {
			return fmt.Errorf("checking dataset: %w", err)
		}
		if !exists {
			if err := r.ZFS.CreateDataset(ctx, vm.ID); err != nil {
				return fmt.Errorf("creating dataset: %w", err)
			}
		}
	} else if r.HAST == nil {
		return fmt.Errorf("VM names replica node %q but no HAST support is configured on this node", vm.ReplicaNodeID)
	}

	if r.Bhyve == nil {
		return nil
	}

	// Network infrastructure (bridge/vlan interfaces, gateway address,
	// outbound NAT) is per-network, not per-VM device config - unlike a
	// VM's own tap/MAC (fixed at launch, see below), it needs to stay
	// correct on every tick even for a VM that's already running,
	// otherwise a network-level config change (e.g. Reconciler.Uplink)
	// never takes effect until every VM on that network happens to be
	// recreated. Confirmed live: this was a real gap - ADR-0048's
	// -nat-uplink fix silently never applied to an already-running VM
	// until this was moved ahead of the running-VM early return below.
	bridge := r.Bridge
	macAddress := vm.MACAddress
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
			if err := r.PF.Apply(ctx, vmAnchor(vm.ID), effectivePFRules(vm)); err != nil {
				return fmt.Errorf("applying firewall rules: %w", err)
			}
		}
		return nil
	}

	var diskPath string
	if vm.ReplicaNodeID != "" {
		path, ok := hastDevicePaths[vmHASTResourceName(vm.ID)]
		if !ok {
			return fmt.Errorf("HAST device for replicated VM %q was not provisioned this tick", vm.ID)
		}
		diskPath = path
	} else {
		mountpoint, err := r.ZFS.GetProperty(ctx, vm.ID, "mountpoint")
		if err != nil {
			return fmt.Errorf("getting dataset mountpoint: %w", err)
		}
		var baseImagePath string
		if vm.BaseImageName != "" {
			if r.ISOs == nil {
				return fmt.Errorf("VM names base image %q but no ISO store is configured on this node", vm.BaseImageName)
			}
			path, err := r.resolveLocalImagePath(ctx, vm.BaseImageName)
			if err != nil {
				return fmt.Errorf("resolving base image %q: %w", vm.BaseImageName, err)
			}
			baseImagePath = path
		}
		diskPath, err = r.ensureDiskImage(mountpoint, baseImagePath)
		if err != nil {
			return fmt.Errorf("preparing disk image: %w", err)
		}
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
		path, err := r.resolveLocalImagePath(ctx, vm.ISOName)
		if err != nil {
			return fmt.Errorf("resolving ISO %q: %w", vm.ISOName, err)
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
		// Same reasoning as EnableVNC above - a captured serial log costs
		// nothing a running VM doesn't already have spare capacity for,
		// and is often the only way to see what a guest actually printed
		// during boot (many cloud/server images redirect console output
		// to com1 instead of the VGA/EFI framebuffer VNC shows).
		EnableSerialLog: true,
	}); err != nil {
		return fmt.Errorf("creating bhyve VM: %w", err)
	}

	if r.PF != nil {
		if err := r.PF.Apply(ctx, vmAnchor(vm.ID), effectivePFRules(vm)); err != nil {
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
	// A network with ExternalGateway set already has a real router
	// answering for the subnet's gateway address on this L2 segment -
	// claiming it here too would conflict with that router (confirmed
	// live: this is what "silent tcpdump on the real router" actually
	// was - a duplicate-IP conflict, not a firewall/routing bug).
	if network.GetExternalGateway() == "" {
		if err := r.VLAN.EnsureBridgeAddress(ctx, bridge, network.GetSubnet()); err != nil {
			return "", fmt.Errorf("assigning gateway address: %w", err)
		}
		// Give this network real outbound internet access through this
		// node's own uplink - see ADR-0048. Only meaningful when this
		// node is itself the gateway (ExternalGateway unset, just above);
		// an externally-gatewayed network's own router is responsible
		// for its internet access instead.
		if r.PF != nil && r.Uplink != "" {
			if err := r.PF.ApplyNAT(ctx, natAnchor(network.GetId()), network.GetSubnet(), r.Uplink); err != nil {
				return "", fmt.Errorf("applying outbound NAT: %w", err)
			}
		}
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

// natAnchor returns the pf(8) anchor name for a network's own outbound
// NAT rule - a sibling of vmAnchor, one flat level under "apiary/" so
// it's covered by the same `anchor "apiary/*" all` reservation vmAnchor
// already relies on, with no separate host prerequisite needed.
func natAnchor(networkID string) string {
	return "apiary/net-" + networkID
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

// effectivePFRules returns vm's firewall rules to actually apply this
// tick - nil (everything allowed, see pf.Manager.Apply's own doc
// comment) if FirewallPaused is set, regardless of FirewallRules.
// FirewallRules itself is never touched by this - un-pausing just
// resumes normal enforcement on the next tick. See ADR-0049.
func effectivePFRules(vm VMPlacement) []pf.Rule {
	if vm.FirewallPaused {
		return nil
	}
	return toPFRules(vm.FirewallRules)
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
			scope = &dhcpd.NetworkScope{Bridge: networkBridgeName(network), Subnet: network.GetSubnet(), DNSServer: r.DNSServer, Gateway: network.GetExternalGateway()}
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

// diskSizeMB returns Reconciler.DiskSizeMB, defaulting to 10240
// (10GiB) if unset - shared by ensureDiskImage (the plain, dataset-
// backed path) and the HAST-replicated path's zvol sizing, so both
// paths size a VM's disk identically regardless of which one it uses.
func (r *Reconciler) diskSizeMB() uint64 {
	if r.DiskSizeMB == 0 {
		return 10240
	}
	return r.DiskSizeMB
}

// ensureDiskImage ensures the VM's disk file exists at mountpoint,
// creating it the first time. baseImagePath, if non-empty, seeds a
// freshly created file by copying that image's contents instead of
// creating a blank, truncated file - see ADR-0031. An already-existing
// disk file is never reseeded, matching every other "only act on
// create" resource in this reconciler (datasets, bhyve VMs).
func (r *Reconciler) ensureDiskImage(mountpoint, baseImagePath string) (string, error) {
	path := filepath.Join(mountpoint, diskImageName)
	if _, err := os.Stat(path); err == nil {
		return path, nil
	} else if !os.IsNotExist(err) {
		return "", err
	}

	if baseImagePath != "" {
		if err := copyFile(path, baseImagePath); err != nil {
			return "", fmt.Errorf("seeding disk from base image: %w", err)
		}
		return path, nil
	}

	sizeMB := r.diskSizeMB()

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

// copyFile copies src's contents into a newly created file at dst.
// dst is removed on any failure so a partial copy is never mistaken for
// a valid disk image on the next tick.
func copyFile(dst, src string) (err error) {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer func() {
		out.Close()
		if err != nil {
			os.Remove(dst)
		}
	}()

	if _, err = io.Copy(out, in); err != nil {
		return err
	}
	return out.Sync()
}
