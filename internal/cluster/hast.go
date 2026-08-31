// HAST-backed replication (ADR-0025): real-time block replication of a
// VM's disk (and, once jail orchestration lands, a jail's root
// filesystem) to a second node, for data redundancy - not automatic
// failover. Every node reconciles only its own local role (primary or
// secondary) from shared raft state; there is no cross-node RPC or
// remote exec anywhere in this file, matching every other reconciler
// in this package.
package cluster

import (
	"context"
	"fmt"
	"net"
	"sort"

	"github.com/glenjbarber/apiary/internal/hast"
)

// hastManager is the subset of *hast.Manager the reconciler needs, for
// the same reason as every other manager interface in this package.
type hastManager interface {
	WriteConfig(resources []hast.Resource) error
	CreateResource(ctx context.Context, name string) error
	SetRole(ctx context.Context, name string, role hast.Role) error
	Status(ctx context.Context, name string) (*hast.Status, error)
	RestartService(ctx context.Context) error
}

// hastRole describes one local HAST role this node needs to fulfill -
// either the primary (this node owns the VM/jail and replicates its
// disk out) or the secondary (this node just silently replicates,
// never mounting or using the result - see ADR-0025).
type hastRole struct {
	resourceName string // e.g. "vm-<id>" or "jail-<id>"
	peerNodeID   string // the *other* node in this resource
	sizeMB       uint64
	isPrimary    bool
}

func hastDevicePath(resourceName string) string {
	return "/dev/hast/" + resourceName
}

func hastZvolName(resourceName string) string {
	return "hast-" + resourceName
}

// resolvePeerAddresses returns every known raft cluster member's
// host-only address (the raft transport's own address, stripped of its
// port), keyed by node ID. Reused directly as hast.conf's Node.Remote:
// every project node's hast.conf Node.Name already equals its raft node
// ID/hostname (raftd's -node-id defaults to os.Hostname(), confirmed
// live on all four project machines), so no separate node-address
// config is needed - raft already tracks this for its own transport.
func (r *Reconciler) resolvePeerAddresses(ctx context.Context) (map[string]string, error) {
	resp, err := r.Raft.Status(ctx)
	if err != nil {
		return nil, err
	}
	addrs := make(map[string]string, len(resp.GetServers()))
	for _, s := range resp.GetServers() {
		host, _, err := net.SplitHostPort(s.GetAddress())
		if err != nil {
			host = s.GetAddress() // not host:port - use as given
		}
		addrs[s.GetId()] = host
	}
	return addrs, nil
}

// reconcileHASTRoles provisions every local HAST resource named in
// roles (zvol, hastctl create, role), after first writing the FULL
// rendered hast.conf covering every resource in roles and restarting
// hastd if that changed since the last tick (mirrors
// Reconciler.lastDHCPConfig's exact diffing pattern) - hastctl create
// requires the resource already be defined in hast.conf, so the
// aggregate config write must land first, before any per-resource
// hastctl call. Returns the primary-role device path for each resource
// name that's primary here (ensureVM/ensureJail use this as their
// actual disk instead of a dataset-backed file).
//
// A nil/empty roles is not a no-op: it still (re)writes an empty
// hast.conf if this node previously had resources that are now all
// gone, so completed teardowns actually drop out of the config instead
// of lingering.
func (r *Reconciler) reconcileHASTRoles(ctx context.Context, roles []hastRole) (map[string]string, error) {
	peers, err := r.resolvePeerAddresses(ctx)
	if err != nil {
		return nil, fmt.Errorf("resolving peer addresses: %w", err)
	}

	localName := r.LocalNodeID
	sort.Slice(roles, func(i, j int) bool { return roles[i].resourceName < roles[j].resourceName })

	resources := make([]hast.Resource, 0, len(roles))
	for _, rl := range roles {
		peerAddr, ok := peers[rl.peerNodeID]
		if !ok {
			return nil, fmt.Errorf("resolving address for peer node %q (resource %s): not found in raft cluster membership", rl.peerNodeID, rl.resourceName)
		}
		localAddr, ok := peers[localName]
		if !ok {
			return nil, fmt.Errorf("resolving this node's own address (%q) in raft cluster membership", localName)
		}
		devicePath, err := r.hastZvolDevicePath(rl.resourceName)
		if err != nil {
			return nil, fmt.Errorf("resolving zvol path for %s: %w", rl.resourceName, err)
		}
		resources = append(resources, hast.Resource{
			Name: rl.resourceName,
			Nodes: []hast.Node{
				{Name: localName, Local: devicePath, Remote: peerAddr},
				{Name: rl.peerNodeID, Local: devicePath, Remote: localAddr},
			},
		})
	}

	rendered, err := hast.RenderConfig(resources)
	if err != nil {
		return nil, fmt.Errorf("rendering hast.conf: %w", err)
	}
	if rendered != r.lastHASTConfig {
		if err := r.HAST.WriteConfig(resources); err != nil {
			return nil, fmt.Errorf("writing hast.conf: %w", err)
		}
		if len(resources) > 0 {
			if err := r.HAST.RestartService(ctx); err != nil {
				return nil, fmt.Errorf("restarting hastd: %w", err)
			}
		}
		r.lastHASTConfig = rendered
	}

	devicePaths := make(map[string]string)
	for _, rl := range roles {
		if err := r.ensureHASTZvolAndRole(ctx, rl); err != nil {
			return devicePaths, fmt.Errorf("provisioning %s: %w", rl.resourceName, err)
		}
		if rl.isPrimary {
			devicePaths[rl.resourceName] = hastDevicePath(rl.resourceName)
		}
	}
	return devicePaths, nil
}

// hastZvolDevicePath is the local GEOM provider path hastd wraps -
// always a zvol (see this file's own doc comment on why: neither
// project host has a spare raw disk/partition to dedicate). A zvol's
// device node includes its full Base-qualified path, unlike a plain
// dataset name.
func (r *Reconciler) hastZvolDevicePath(resourceName string) (string, error) {
	full, err := r.ZFS.FullPath(hastZvolName(resourceName))
	if err != nil {
		return "", err
	}
	return "/dev/zvol/" + full, nil
}

// ensureHASTZvolAndRole provisions rl's local zvol if it doesn't
// already exist, creates the hastctl resource if it hasn't been
// created yet on this node (checked via Status, since hastctl create
// on an already-created resource is not documented as idempotent), and
// sets the requested role. Existence-checked throughout, safe to call
// every tick.
func (r *Reconciler) ensureHASTZvolAndRole(ctx context.Context, rl hastRole) error {
	zvolName := hastZvolName(rl.resourceName)
	exists, err := r.ZFS.DatasetExists(ctx, zvolName)
	if err != nil {
		return fmt.Errorf("checking zvol: %w", err)
	}
	if !exists {
		if err := r.ZFS.CreateZvol(ctx, zvolName, rl.sizeMB); err != nil {
			return fmt.Errorf("creating zvol: %w", err)
		}
	}

	if _, err := r.HAST.Status(ctx, rl.resourceName); err != nil {
		// Not yet created on this node - hastctl create initializes
		// local on-disk metadata, required before any role change.
		if err := r.HAST.CreateResource(ctx, rl.resourceName); err != nil {
			return fmt.Errorf("creating hast resource: %w", err)
		}
	}

	role := hast.RoleSecondary
	if rl.isPrimary {
		role = hast.RolePrimary
	}
	if err := r.HAST.SetRole(ctx, rl.resourceName, role); err != nil {
		return fmt.Errorf("setting role %s: %w", role, err)
	}
	return nil
}

// reclaimHASTRole tears down resourceName's local zvol - dropping it
// from the next reconcileHASTRoles call's rendered resource list is
// what actually removes it from hast.conf (there is no clean
// standalone "delete resource" in hastctl; the resource simply stops
// being defined once its owning node no longer names it, and the next
// aggregate config write/restart reflects that). Idempotent: does
// nothing if the zvol doesn't exist, matching every other reclaim/
// teardown step in this package.
func (r *Reconciler) reclaimHASTRole(ctx context.Context, resourceName string) error {
	zvolName := hastZvolName(resourceName)
	exists, err := r.ZFS.DatasetExists(ctx, zvolName)
	if err != nil {
		return fmt.Errorf("checking stale zvol: %w", err)
	}
	if !exists {
		return nil
	}
	if err := r.ZFS.DestroyDataset(ctx, zvolName); err != nil {
		return fmt.Errorf("destroying stale zvol: %w", err)
	}
	return nil
}

// vmHASTResourceName returns the HAST resource/zvol-suffix name for a
// VM's disk - prefixed so it can never collide with a jail's own HAST
// resource name (added alongside jail orchestration) even if a VM and
// a jail happen to share an id.
func vmHASTResourceName(vmID string) string {
	return "vm-" + vmID
}
