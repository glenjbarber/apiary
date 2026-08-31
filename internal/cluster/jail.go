// Jail orchestration (ADR-0026's second half): the same Reconciler
// that provisions VMs also provisions jails, keyed off JailDefinition
// (mirroring VMDefinition, deliberately minimal - see its own doc
// comment). A non-replicated jail's root is a plain ZFS dataset
// (already mounted by ZFS itself); a replicated jail's root is a
// HAST-replicated device that needs a real filesystem before it can be
// mounted - unlike a bhyve VM's disk, which uses the raw device
// directly - so it goes through internal/ufsmount's newfs/mount
// wrapping on top of the same primary/secondary HAST role machinery
// hast.go already provides for VMs.
package cluster

import (
	"context"
	"fmt"

	"google.golang.org/protobuf/proto"

	internalpb "github.com/glenjbarber/apiary/api/internalpb"
	"github.com/glenjbarber/apiary/internal/jail"
)

// jailManager is the subset of *jail.Manager the reconciler needs, for
// the same reason as every other manager interface in this package.
type jailManager interface {
	CreateJail(ctx context.Context, name string, cfg jail.Config) error
	RemoveJail(ctx context.Context, name string) error
	JailExists(ctx context.Context, name string) (bool, error)
}

// mountManager is the subset of *ufsmount.Manager the reconciler
// needs, for the same reason as jailManager. Only ever used on a
// replicated jail's root - a non-replicated jail's ZFS dataset is
// already mounted by ZFS itself.
type mountManager interface {
	FormatIfNeeded(ctx context.Context, devicePath string) error
	Mount(ctx context.Context, devicePath, mountPoint string) error
	Unmount(ctx context.Context, mountPoint string) error
}

// jailHASTResourceName mirrors vmHASTResourceName, for jails.
func jailHASTResourceName(jailID string) string {
	return "jail-" + jailID
}

// jailRootPath returns where a jail's root filesystem lives on this
// node - a subdirectory of the reconciler's own jail base, distinct
// from vm.ID's ZFS dataset name so a VM and a jail can never collide
// even if operators happen to reuse the same id across both (unlikely,
// but this project's ZFS Manager already scopes VM datasets by raw ID
// directly, so a jail needs its own namespace instead of relying on
// id uniqueness holding across two different kinds of resource).
func jailRootPath(jailBase, jailID string) string {
	return jailBase + "/" + jailID
}

// reconcileJail dispatches to teardownJail or ensureJail depending on
// jailPlacement's tombstone state, mirroring reconcileVM's phase-
// transition wrapping exactly (UpdateJailPhase instead of
// UpdateVMPhase).
func (r *Reconciler) reconcileJail(ctx context.Context, j JailPlacement, hastDevicePaths map[string]string) error {
	if j.Deleting {
		return r.teardownJail(ctx, j)
	}

	if j.Phase != PhaseReady && j.Phase != PhaseCreating {
		r.applyJailPhase(ctx, j.ID, PhaseCreating, "")
	}
	if err := r.ensureJail(ctx, j, hastDevicePaths); err != nil {
		r.applyJailPhase(ctx, j.ID, PhaseError, err.Error())
		return err
	}
	if j.Phase != PhaseReady {
		r.applyJailPhase(ctx, j.ID, PhaseReady, "")
	}
	return nil
}

// ensureJail ensures j's root filesystem exists - a plain ZFS dataset,
// or, if ReplicaNodeID is set, a HAST-replicated device formatted and
// mounted at this node's own jail root path (see hastDevicePaths/
// ADR-0026) - then that its jail(8) process is running.
func (r *Reconciler) ensureJail(ctx context.Context, j JailPlacement, hastDevicePaths map[string]string) error {
	if r.Jail == nil {
		return fmt.Errorf("jail %q is assigned to this node but no jail support is configured on this node", j.ID)
	}

	var rootPath string
	if j.ReplicaNodeID == "" {
		exists, err := r.ZFS.DatasetExists(ctx, j.ID)
		if err != nil {
			return fmt.Errorf("checking dataset: %w", err)
		}
		if !exists {
			if err := r.ZFS.CreateDataset(ctx, j.ID); err != nil {
				return fmt.Errorf("creating dataset: %w", err)
			}
		}
		mountpoint, err := r.ZFS.GetProperty(ctx, j.ID, "mountpoint")
		if err != nil {
			return fmt.Errorf("getting dataset mountpoint: %w", err)
		}
		rootPath = mountpoint
	} else {
		if r.HAST == nil {
			return fmt.Errorf("jail names replica node %q but no HAST support is configured on this node", j.ReplicaNodeID)
		}
		if r.Mount == nil {
			return fmt.Errorf("jail names replica node %q but no filesystem-mount support is configured on this node", j.ReplicaNodeID)
		}
		devicePath, ok := hastDevicePaths[jailHASTResourceName(j.ID)]
		if !ok {
			return fmt.Errorf("HAST device for replicated jail %q was not provisioned this tick", j.ID)
		}
		if err := r.Mount.FormatIfNeeded(ctx, devicePath); err != nil {
			return fmt.Errorf("formatting HAST device: %w", err)
		}
		rootPath = jailRootPath(r.jailBase(), j.ID)
		if err := r.Mount.Mount(ctx, devicePath, rootPath); err != nil {
			return fmt.Errorf("mounting HAST device: %w", err)
		}
	}

	running, err := r.Jail.JailExists(ctx, j.ID)
	if err != nil {
		return fmt.Errorf("checking jail: %w", err)
	}
	if running {
		return nil
	}

	if err := r.Jail.CreateJail(ctx, j.ID, jail.Config{Path: rootPath, Hostname: j.Hostname}); err != nil {
		return fmt.Errorf("creating jail: %w", err)
	}
	return nil
}

// teardownJail mirrors teardownVM exactly: destroy the real jail(8)
// process, then its root filesystem (unmount+reclaim the HAST role for
// a replicated jail, or destroy the dataset otherwise), then purge the
// record once both are confirmed gone.
func (r *Reconciler) teardownJail(ctx context.Context, j JailPlacement) error {
	if j.Phase != PhaseDeleting {
		r.applyJailPhase(ctx, j.ID, PhaseDeleting, "")
	}

	if r.Jail != nil {
		running, err := r.Jail.JailExists(ctx, j.ID)
		if err != nil {
			return fmt.Errorf("checking jail: %w", err)
		}
		if running {
			if err := r.Jail.RemoveJail(ctx, j.ID); err != nil {
				return fmt.Errorf("removing jail: %w", err)
			}
		}
	}

	if j.ReplicaNodeID != "" {
		if r.Mount != nil {
			if err := r.Mount.Unmount(ctx, jailRootPath(r.jailBase(), j.ID)); err != nil {
				return fmt.Errorf("unmounting jail root: %w", err)
			}
		}
		if err := r.reclaimHASTRole(ctx, jailHASTResourceName(j.ID)); err != nil {
			return fmt.Errorf("reclaiming HAST resource: %w", err)
		}
	} else {
		exists, err := r.ZFS.DatasetExists(ctx, j.ID)
		if err != nil {
			return fmt.Errorf("checking dataset: %w", err)
		}
		if exists {
			if err := r.ZFS.DestroyDataset(ctx, j.ID); err != nil {
				return fmt.Errorf("destroying dataset: %w", err)
			}
		}
	}

	return r.purgeJail(ctx, j.ID)
}

// reclaimStaleJail mirrors reclaimStaleVM exactly, for jails instead
// of VMs.
func (r *Reconciler) reclaimStaleJail(ctx context.Context, id string, isCurrentReplica bool) error {
	if r.Jail != nil {
		exists, err := r.Jail.JailExists(ctx, id)
		if err != nil {
			return fmt.Errorf("checking stale jail: %w", err)
		}
		if exists {
			if err := r.Jail.RemoveJail(ctx, id); err != nil {
				return fmt.Errorf("removing stale jail: %w", err)
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

	if !isCurrentReplica {
		if r.Mount != nil {
			if err := r.Mount.Unmount(ctx, jailRootPath(r.jailBase(), id)); err != nil {
				return fmt.Errorf("unmounting stale jail root: %w", err)
			}
		}
		if err := r.reclaimHASTRole(ctx, jailHASTResourceName(id)); err != nil {
			return fmt.Errorf("reclaiming stale HAST resource: %w", err)
		}
	}

	return nil
}

// applyJailPhase mirrors applyPhase exactly, for jails instead of VMs.
func (r *Reconciler) applyJailPhase(ctx context.Context, id, phase, phaseError string) {
	cmd := &internalpb.Command{
		Op: &internalpb.Command_UpdateJailPhase{UpdateJailPhase: &internalpb.UpdateJailPhase{
			Id:         id,
			Phase:      jailPhaseFromString(phase),
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
	// Best-effort even via Peers (ADR-0029) - mirrors applyPhase's own
	// reasoning exactly.
	if r.Peers != nil && resp.GetLeaderHint() != "" {
		_ = r.Peers.ReportJailPhase(ctx, r.resolvePeerManagerdAddr(resp.GetLeaderHint()), id, phase, phaseError)
	}
}

// purgeJail submits a PurgeJail command, mirroring teardownVM's own
// PurgeVM submission exactly - including checking ApplyResponse.Error,
// not just the transport error, and forwarding to the leader's
// managerd via Peers when this node's own raftd rejects the write for
// not being the leader (see that call site's own comment - ADR-0028/
// ADR-0029).
func (r *Reconciler) purgeJail(ctx context.Context, id string) error {
	cmd := &internalpb.Command{
		Op: &internalpb.Command_PurgeJail{PurgeJail: &internalpb.PurgeJail{Id: id}},
	}
	data, err := proto.Marshal(cmd)
	if err != nil {
		return fmt.Errorf("marshaling PurgeJail: %w", err)
	}
	resp, err := r.Raft.Apply(ctx, data, phaseApplyTimeout)
	if err != nil {
		return fmt.Errorf("purging jail record: %w", err)
	}
	if resp.GetError() != "" {
		if r.Peers != nil && resp.GetLeaderHint() != "" {
			addr := r.resolvePeerManagerdAddr(resp.GetLeaderHint())
			if perr := r.Peers.ReportJailTeardownComplete(ctx, addr, id); perr != nil {
				return fmt.Errorf("purging jail record via peer %s: %w", addr, perr)
			}
			return nil
		}
		return fmt.Errorf("purging jail record: %s", resp.GetError())
	}
	return nil
}

func jailPhaseToString(p internalpb.JailPhase) string {
	switch p {
	case internalpb.JailPhase_JAIL_PHASE_CREATING:
		return PhaseCreating
	case internalpb.JailPhase_JAIL_PHASE_READY:
		return PhaseReady
	case internalpb.JailPhase_JAIL_PHASE_DELETING:
		return PhaseDeleting
	case internalpb.JailPhase_JAIL_PHASE_ERROR:
		return PhaseError
	default:
		return ""
	}
}

func jailPhaseFromString(p string) internalpb.JailPhase {
	switch p {
	case PhaseCreating:
		return internalpb.JailPhase_JAIL_PHASE_CREATING
	case PhaseReady:
		return internalpb.JailPhase_JAIL_PHASE_READY
	case PhaseDeleting:
		return internalpb.JailPhase_JAIL_PHASE_DELETING
	case PhaseError:
		return internalpb.JailPhase_JAIL_PHASE_ERROR
	default:
		return internalpb.JailPhase_JAIL_PHASE_UNSPECIFIED
	}
}

// jailDiskSizeMB returns Reconciler.JailDiskSizeMB, defaulting to 2048
// (2GiB) if unset - only ever consulted for a replicated jail's HAST
// role sizing (see RunOnce); a non-replicated jail's root has no
// separate size of its own, it's whatever its ZFS dataset allows.
func (r *Reconciler) jailDiskSizeMB() uint64 {
	if r.JailDiskSizeMB == 0 {
		return 2048
	}
	return r.JailDiskSizeMB
}

// jailBase returns Reconciler.JailBase, defaulting to "/apiary-jails"
// if unset - only ever consulted for a replicated jail's mount path
// (a non-replicated jail's root is its ZFS dataset's own mountpoint,
// which needs no separate base).
func (r *Reconciler) jailBase() string {
	if r.JailBase == "" {
		return "/apiary-jails"
	}
	return r.JailBase
}
