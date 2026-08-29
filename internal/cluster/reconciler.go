package cluster

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

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

// vmLister is the subset of *manager.RaftClient the reconciler needs.
// Defined locally (rather than importing internal/manager's concrete
// type into the signature) so tests can supply a fake without any real
// raft/gRPC machinery; *manager.RaftClient satisfies this today without
// any changes on its side.
type vmLister interface {
	ListVMs(ctx context.Context) (*internalpb.ListVMsResponse, error)
}

// datasetManager is the subset of *zfs.Manager the reconciler needs, for
// the same reason as vmLister. *zfs.Manager satisfies this today.
type datasetManager interface {
	DatasetExists(ctx context.Context, name string) (bool, error)
	CreateDataset(ctx context.Context, name string) error
	GetProperty(ctx context.Context, name, prop string) (string, error)
}

// vmManager is the subset of *bhyve.Manager the reconciler needs, for
// the same reason as vmLister. *bhyve.Manager satisfies this today.
type vmManager interface {
	VMExists(ctx context.Context, name string) (bool, error)
	CreateVM(ctx context.Context, name string, cfg bhyve.Config) error
}

// Reconciler provisions local ZFS storage - and, if Bhyve is set, a
// running bhyve VM backed by that storage - for VMs assigned to this
// node, based on VMDefinition.node_id in raft's ephemeral state. It only
// creates missing resources; see Plan's doc comment for why it never
// removes them.
type Reconciler struct {
	Raft        vmLister
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
// LocalNodeID, ensures its ZFS dataset exists and - if Bhyve is
// configured - that a bhyve VM backed by that dataset's disk image is
// running. It returns an error without provisioning anything if the VM
// list can't be fetched - reconciling against a partial or failed fetch
// is exactly the kind of mistake Plan's design note warns about
// avoiding. A failure partway through one VM's provisioning is reported
// but does not stop the remaining VMs in this tick from being
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
		})
	}

	var firstErr error
	for _, vm := range Plan(desired, r.LocalNodeID) {
		if err := r.ensureVM(ctx, vm); err != nil && firstErr == nil {
			firstErr = fmt.Errorf("cluster: provisioning VM %s: %w", vm.ID, err)
		}
	}
	return firstErr
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
