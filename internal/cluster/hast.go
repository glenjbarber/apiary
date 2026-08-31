// HAST-backed replication (ADR-0026): real-time block replication of a
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
	"os"
	"path/filepath"
	"sort"
	"time"

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
// never mounting or using the result - see ADR-0026).
type hastRole struct {
	resourceName string // e.g. "vm-<id>" or "jail-<id>"
	peerNodeID   string // the *other* node in this resource
	sizeMB       uint64
	isPrimary    bool
}

// hastBackingFileName is the file created inside each HAST resource's
// own dataset to back it - see ensureHASTProvider's doc comment for why
// a plain file, not a zvol.
const hastBackingFileName = "backing.img"

// defaultHASTRestartSettleDelay is how long reconcileHASTRoles waits
// after a real hastd restart before attempting any role change,
// unless overridden by Reconciler.HASTRestartSettleDelay - see its
// call site's doc comment for why this needs to be an upfront wait,
// not just a per-attempt retry gap.
const defaultHASTRestartSettleDelay = 3 * time.Second

// hastRestartSettleDelay returns Reconciler.HASTRestartSettleDelay,
// defaulting to defaultHASTRestartSettleDelay if unset - tests override
// this to 0 so they don't pay the real-world settle cost the live
// hastd race actually needs.
func (r *Reconciler) hastRestartSettleDelay() time.Duration {
	if r.HASTRestartSettleDelay == 0 {
		return defaultHASTRestartSettleDelay
	}
	return r.HASTRestartSettleDelay
}

func hastDevicePath(resourceName string) string {
	return "/dev/hast/" + resourceName
}

func hastProviderDatasetName(resourceName string) string {
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
// roles. Returns the primary-role device path for each resource name
// that's primary here (ensureVM/ensureJail use this as their actual
// disk instead of a dataset-backed file).
//
// Order matters and is deliberate, caught live the hard way: each
// role's local provider is created FIRST, for every role, before
// hast.conf is (re)written and hastd restarted. Doing it the other way
// around - as an earlier version of this code did - restarts hastd
// with a resource already in its config whose backing provider doesn't
// exist yet; hastd's worker for that resource immediately fails to
// open the missing path and exits, then only retries on its own
// internal ~30s interval. A CreateResource/SetRole call made shortly
// after (once the provider finally does exist) can then hit that
// already-dead worker and fail with a "not connected" error from
// hastd - and, worse, hastctl itself exits 0 even when it prints that
// error, so nothing in this package's own error handling would have
// caught it. Creating every provider before touching hast.conf/hastd
// avoids the race entirely.
//
// A nil/empty roles is not a no-op: it still (re)writes an empty
// hast.conf if this node previously had resources that are now all
// gone, so completed teardowns actually drop out of the config instead
// of lingering.
func (r *Reconciler) reconcileHASTRoles(ctx context.Context, roles []hastRole) (map[string]string, error) {
	sort.Slice(roles, func(i, j int) bool { return roles[i].resourceName < roles[j].resourceName })

	localPaths := make(map[string]string, len(roles))
	for _, rl := range roles {
		path, err := r.ensureHASTProvider(ctx, rl)
		if err != nil {
			return nil, fmt.Errorf("provisioning provider for %s: %w", rl.resourceName, err)
		}
		localPaths[rl.resourceName] = path
	}

	peers, err := r.resolvePeerAddresses(ctx)
	if err != nil {
		return nil, fmt.Errorf("resolving peer addresses: %w", err)
	}

	localName := r.LocalNodeID
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
		resources = append(resources, hast.Resource{
			Name: rl.resourceName,
			Nodes: []hast.Node{
				{Name: localName, Local: localPaths[rl.resourceName], Remote: peerAddr},
				{Name: rl.peerNodeID, Local: localPaths[rl.resourceName], Remote: localAddr},
			},
		})
	}

	rendered, err := hast.RenderConfig(resources)
	if err != nil {
		return nil, fmt.Errorf("rendering hast.conf: %w", err)
	}
	if !r.hastConfigWritten || rendered != r.lastHASTConfig {
		if err := r.HAST.WriteConfig(resources); err != nil {
			return nil, fmt.Errorf("writing hast.conf: %w", err)
		}
		// RestartService always runs on a real config change, even down
		// to zero resources - NOT gated on len(resources) > 0 (a real,
		// live-caught bug: the original guard skipped restarting hastd
		// entirely when the last resource on a node was removed, since
		// "nothing left to restart for" seemed reasonable at the time.
		// In fact the opposite is true - hastd's still-running worker
		// for that now-removed resource keeps its backing file open
		// under the STALE config, so the reconciler's own very next
		// step (destroying that resource's now-unreferenced ZFS
		// dataset, in reclaimHASTRole/teardownVM/teardownJail) fails
		// forever with "cannot unmount ...: pool or dataset is busy" -
		// confirmed live tearing down a replicated jail with no other
		// HAST resource active on the node at the time. An empty
		// rendered config is a valid, empty hast.conf (RenderConfig's
		// own doc comment), and "service hastd onerestart" against it
		// just leaves hastd running with no active resources - safe,
		// and exactly what releases the stale worker's hold on the
		// file being destroyed).
		if err := r.HAST.RestartService(ctx); err != nil {
			return nil, fmt.Errorf("restarting hastd: %w", err)
		}
		// hastd's own worker processes need a few seconds to
		// (re)spawn and open their providers after a restart -
		// confirmed live: a role change attempted immediately (or
		// retried in quick 1s succession starting immediately)
		// after restart keeps hitting a worker that hasn't finished
		// starting yet and fails with "Error 57" (ENOTCONN)
		// repeatedly, while the exact same attempt made once after
		// this wait succeeds cleanly. A single upfront wait, not a
		// tighter retry loop starting at t=0, is what actually
		// avoids the race (see ADR-0026).
		time.Sleep(r.hastRestartSettleDelay())
		r.lastHASTConfig = rendered
		r.hastConfigWritten = true
	}

	devicePaths := make(map[string]string)
	for _, rl := range roles {
		if err := r.ensureHASTResourceAndRole(ctx, rl); err != nil {
			return devicePaths, fmt.Errorf("provisioning %s: %w", rl.resourceName, err)
		}
		if rl.isPrimary {
			devicePaths[rl.resourceName] = hastDevicePath(rl.resourceName)
		}
	}
	return devicePaths, nil
}

// ensureHASTProvider provisions rl's local GEOM provider if it doesn't
// already exist, returning its path, and reports what already exists
// otherwise. This is a plain file inside a dedicated dataset
// (hastProviderDatasetName), not a zvol: confirmed live on apiarium
// (FreeBSD 16.0-CURRENT) that hastd cannot read/write its own metadata
// against a zvol-backed provider ("Unable to read metadata ... No such
// file or directory", role never actually takes effect) but works
// correctly against both a plain file and an md(4) vnode device - a
// plain file needs no extra device-allocation bookkeeping at all, so
// it's the simpler of the two working options. See ADR-0026.
//
// Deliberately separate from ensureHASTResourceAndRole (and called for
// every role before any of them touch hast.conf/hastd) - see
// reconcileHASTRoles's own doc comment for why the ordering matters.
func (r *Reconciler) ensureHASTProvider(ctx context.Context, rl hastRole) (string, error) {
	datasetName := hastProviderDatasetName(rl.resourceName)
	exists, err := r.ZFS.DatasetExists(ctx, datasetName)
	if err != nil {
		return "", fmt.Errorf("checking dataset: %w", err)
	}
	if !exists {
		if err := r.ZFS.CreateDataset(ctx, datasetName); err != nil {
			return "", fmt.Errorf("creating dataset: %w", err)
		}
	}

	mountpoint, err := r.ZFS.GetProperty(ctx, datasetName, "mountpoint")
	if err != nil {
		return "", fmt.Errorf("getting dataset mountpoint: %w", err)
	}
	path := filepath.Join(mountpoint, hastBackingFileName)

	if _, err := os.Stat(path); err == nil {
		return path, nil
	} else if !os.IsNotExist(err) {
		return "", err
	}

	f, err := os.Create(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	if err := f.Truncate(int64(rl.sizeMB) * 1024 * 1024); err != nil {
		return "", err
	}
	// hastd reads/writes its own metadata near the end of this file as
	// soon as its worker starts up - confirmed live that a Truncate
	// alone isn't enough for a read at that far offset to reliably
	// succeed moments later (repeated "Unable to read metadata ... No
	// such file or directory" even several seconds after creation,
	// despite the file's size already being correct per a plain ls).
	// An explicit Sync forces the extended size to be fully committed
	// before anything else touches the file.
	if err := f.Sync(); err != nil {
		return "", err
	}
	return path, nil
}

// ensureHASTResourceAndRole creates the hastctl resource (initializing
// its on-disk metadata) and sets the requested role. Only called after
// this role's provider already exists and the current hast.conf/hastd
// restart (if any) has landed - see reconcileHASTRoles.
//
// CreateResource is called unconditionally, every tick, rather than
// guarded by a "does it already exist" check - confirmed live that
// `hastctl list <name>` (this package's only status-reading primitive)
// succeeds and reports a normal "role: init" for a resource that's
// merely *defined in hast.conf and loaded by a running hastd*, whether
// or not `hastctl create` has ever actually been run against it. A
// Status-based "already created, skip CreateResource" guard therefore
// always sees success and never calls CreateResource at all - the
// resource's on-disk metadata block never gets initialized, and the
// following SetRole(primary) fails with "Unable to read metadata"
// forever, on every resource, every time (this was the actual root
// cause of every one of this feature's live HAST role-change failures,
// not the zvol-vs-file choice or restart timing that were fixed along
// the way to finding it). CreateResource is confirmed idempotent in
// practice (a second call against an already-created resource exits 0
// with no error), so there is no need for the guard at all. See
// ADR-0026.
func (r *Reconciler) ensureHASTResourceAndRole(ctx context.Context, rl hastRole) error {
	if err := r.HAST.CreateResource(ctx, rl.resourceName); err != nil {
		return fmt.Errorf("creating hast resource: %w", err)
	}

	role := hast.RoleSecondary
	if rl.isPrimary {
		role = hast.RolePrimary
	}

	// Exactly one attempt per tick, deliberately not a tight retry loop -
	// confirmed live that retrying SetRole rapidly (5 attempts, 1s apart)
	// right after a restart made things reliably *worse*, not better: a
	// resource that failed its first attempt kept failing every retry
	// and every subsequent tick, while an identical resource that had
	// never been attempted before succeeded first-try after the exact
	// same reconcileHASTRoles restart-settle wait. That points at each
	// failed attempt leaving hastd's per-resource on-disk state (written
	// into the provider file itself) worse than it started, rather than
	// the underlying issue being transient - retrying harder just
	// compounds it. A single attempt per tick matches every other
	// operation in this reconciler: try once, let convergence happen
	// (or a real failure surface) across ticks rather than in-process
	// retry storms. See ADR-0026.
	if err := r.HAST.SetRole(ctx, rl.resourceName, role); err != nil {
		return fmt.Errorf("setting role %s: %w", role, err)
	}

	// hastctl role can exit 0 with no error text even when the role
	// change didn't actually take (confirmed live: its worker process
	// can crash asynchronously after hastctl has already returned
	// success), so SetRole's own return value isn't sufficient
	// confirmation. Re-read this node's own status and treat a mismatch
	// as a real failure rather than reporting false success.
	status, err := r.HAST.Status(ctx, rl.resourceName)
	if err != nil {
		return fmt.Errorf("verifying role after SetRole: %w", err)
	}
	if status.Role != string(role) {
		return fmt.Errorf("role %s did not take effect for %s (hastd reports %q) - see its own logs for why", role, rl.resourceName, status.Role)
	}
	return nil
}

// reclaimHASTRole tears down resourceName's local dataset (and the
// backing file inside it) - dropping it from the next
// reconcileHASTRoles call's rendered resource list is what actually
// removes it from hast.conf (there is no clean standalone "delete
// resource" in hastctl; the resource simply stops being defined once
// its owning node no longer names it, and the next aggregate config
// write/restart reflects that). Idempotent: does nothing if the
// dataset doesn't exist, matching every other reclaim/teardown step in
// this package.
func (r *Reconciler) reclaimHASTRole(ctx context.Context, resourceName string) error {
	datasetName := hastProviderDatasetName(resourceName)
	exists, err := r.ZFS.DatasetExists(ctx, datasetName)
	if err != nil {
		return fmt.Errorf("checking stale dataset: %w", err)
	}
	if !exists {
		return nil
	}
	if err := r.ZFS.DestroyDataset(ctx, datasetName); err != nil {
		return fmt.Errorf("destroying stale dataset: %w", err)
	}
	return nil
}

// vmHASTResourceName returns the HAST resource name for a VM's disk -
// prefixed so it can never collide with a jail's own HAST resource
// name (added alongside jail orchestration) even if a VM and a jail
// happen to share an id.
func vmHASTResourceName(vmID string) string {
	return "vm-" + vmID
}
