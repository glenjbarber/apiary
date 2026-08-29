package cluster

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"google.golang.org/protobuf/proto"

	internalpb "github.com/glenjbarber/apiary/api/internalpb"
	"github.com/glenjbarber/apiary/internal/bhyve"
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
		desired = append(desired, VMPlacement{
			ID:       vm.GetId(),
			NodeID:   vm.GetNodeId(),
			Vcpus:    vm.GetVcpus(),
			MemoryMB: vm.GetMemoryMb(),
			Deleting: vm.GetDesiredState() == internalpb.VMState_VM_STATE_DELETING,
			Phase:    phaseToString(vm.GetPhase()),
		})
	}

	var firstErr error
	for _, vm := range Plan(desired, r.LocalNodeID) {
		if err := r.reconcileVM(ctx, vm); err != nil && firstErr == nil {
			firstErr = fmt.Errorf("cluster: reconciling VM %s: %w", vm.ID, err)
		}
	}
	return firstErr
}

// reconcileVM dispatches to teardownVM or ensureVM depending on vm's
// tombstone state, wrapping ensureVM with the phase transitions that
// make reconciliation progress observable: "creating" before the first
// attempt, "ready" once it succeeds, "error" (with the failure message)
// if it doesn't. These are best-effort writes - see applyPhase - so a
// phase-update failure never masks or replaces the underlying
// provisioning error.
func (r *Reconciler) reconcileVM(ctx context.Context, vm VMPlacement) error {
	if vm.Deleting {
		return r.teardownVM(ctx, vm)
	}

	if vm.Phase != PhaseReady && vm.Phase != PhaseCreating {
		r.applyPhase(ctx, vm.ID, PhaseCreating, "")
	}
	if err := r.ensureVM(ctx, vm); err != nil {
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
func (r *Reconciler) ensureVM(ctx context.Context, vm VMPlacement) error {
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

	if err := r.Bhyve.CreateVM(ctx, vm.ID, bhyve.Config{
		CPUs:     cpus,
		MemoryMB: memoryMB,
		BootROM:  r.BootROM,
		DiskPath: diskPath,
	}); err != nil {
		return fmt.Errorf("creating bhyve VM: %w", err)
	}
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
