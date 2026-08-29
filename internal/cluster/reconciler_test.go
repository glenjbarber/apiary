package cluster

import (
	"context"
	"errors"
	"testing"

	internalpb "github.com/glenjbarber/apiary/api/internalpb"
	"github.com/glenjbarber/apiary/internal/bhyve"
)

type fakeVMLister struct {
	resp *internalpb.ListVMsResponse
	err  error
}

func (f *fakeVMLister) ListVMs(context.Context) (*internalpb.ListVMsResponse, error) {
	return f.resp, f.err
}

type fakeDatasetManager struct {
	existing map[string]bool
	created  []string

	existsErr     error
	createErr     error
	getPropErr    error
	mountpointFor map[string]string
}

func newFakeDatasetManager() *fakeDatasetManager {
	return &fakeDatasetManager{existing: map[string]bool{}, mountpointFor: map[string]string{}}
}

func (f *fakeDatasetManager) DatasetExists(_ context.Context, name string) (bool, error) {
	if f.existsErr != nil {
		return false, f.existsErr
	}
	return f.existing[name], nil
}

func (f *fakeDatasetManager) CreateDataset(_ context.Context, name string) error {
	if f.createErr != nil {
		return f.createErr
	}
	f.created = append(f.created, name)
	f.existing[name] = true
	return nil
}

func (f *fakeDatasetManager) GetProperty(_ context.Context, name, _ string) (string, error) {
	if f.getPropErr != nil {
		return "", f.getPropErr
	}
	return f.mountpointFor[name], nil
}

type fakeVMManager struct {
	running   map[string]bool
	created   []string
	lastCfg   map[string]bhyve.Config
	existsErr error
	createErr error
}

func newFakeVMManager() *fakeVMManager {
	return &fakeVMManager{running: map[string]bool{}, lastCfg: map[string]bhyve.Config{}}
}

func (f *fakeVMManager) VMExists(_ context.Context, name string) (bool, error) {
	if f.existsErr != nil {
		return false, f.existsErr
	}
	return f.running[name], nil
}

func (f *fakeVMManager) CreateVM(_ context.Context, name string, cfg bhyve.Config) error {
	if f.createErr != nil {
		return f.createErr
	}
	f.created = append(f.created, name)
	f.lastCfg[name] = cfg
	f.running[name] = true
	return nil
}

func TestReconciler_RunOnce_CreatesMissingDatasets(t *testing.T) {
	raft := &fakeVMLister{resp: &internalpb.ListVMsResponse{
		Vms: []*internalpb.VMDefinition{
			{Id: "vm-1", NodeId: "node-a"},
			{Id: "vm-2", NodeId: "node-b"},
		},
	}}
	zfs := newFakeDatasetManager()

	r := &Reconciler{Raft: raft, ZFS: zfs, LocalNodeID: "node-a"}
	if err := r.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce() error: %v", err)
	}

	if len(zfs.created) != 1 || zfs.created[0] != "vm-1" {
		t.Errorf("created = %v, want [vm-1]", zfs.created)
	}
}

func TestReconciler_RunOnce_SkipsExistingDataset(t *testing.T) {
	raft := &fakeVMLister{resp: &internalpb.ListVMsResponse{
		Vms: []*internalpb.VMDefinition{{Id: "vm-1", NodeId: "node-a"}},
	}}
	zfs := newFakeDatasetManager()
	zfs.existing["vm-1"] = true

	r := &Reconciler{Raft: raft, ZFS: zfs, LocalNodeID: "node-a"}
	if err := r.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce() error: %v", err)
	}
	if len(zfs.created) != 0 {
		t.Errorf("created = %v, want nothing (dataset already existed)", zfs.created)
	}
}

func TestReconciler_RunOnce_FailsWithoutProvisioningOnListVMsError(t *testing.T) {
	raft := &fakeVMLister{err: errors.New("raftd unreachable")}
	zfs := newFakeDatasetManager()

	r := &Reconciler{Raft: raft, ZFS: zfs, LocalNodeID: "node-a"}
	if err := r.RunOnce(context.Background()); err == nil {
		t.Fatalf("RunOnce() = nil error, want an error when ListVMs fails")
	}
	if len(zfs.created) != 0 {
		t.Errorf("created = %v, want nothing provisioned on a failed fetch", zfs.created)
	}
}

func TestReconciler_RunOnce_FailsWithoutProvisioningOnApplicationError(t *testing.T) {
	// A non-leader raftd returns a normal response with .Error set, not a
	// transport error - RunOnce must treat that the same as a hard
	// failure, not an empty VM list.
	raft := &fakeVMLister{resp: &internalpb.ListVMsResponse{Error: "not the leader"}}
	zfs := newFakeDatasetManager()

	r := &Reconciler{Raft: raft, ZFS: zfs, LocalNodeID: "node-a"}
	if err := r.RunOnce(context.Background()); err == nil {
		t.Fatalf("RunOnce() = nil error, want an error when raftd reports a failure")
	}
	if len(zfs.created) != 0 {
		t.Errorf("created = %v, want nothing provisioned when raftd reports a failure", zfs.created)
	}
}

func TestReconciler_RunOnce_PropagatesCreateDatasetError(t *testing.T) {
	raft := &fakeVMLister{resp: &internalpb.ListVMsResponse{
		Vms: []*internalpb.VMDefinition{{Id: "vm-1", NodeId: "node-a"}},
	}}
	zfs := newFakeDatasetManager()
	zfs.createErr = errors.New("disk full")

	r := &Reconciler{Raft: raft, ZFS: zfs, LocalNodeID: "node-a"}
	if err := r.RunOnce(context.Background()); err == nil {
		t.Fatalf("RunOnce() = nil error, want the underlying CreateDataset error surfaced")
	}
}

func TestReconciler_RunOnce_WithoutBhyveOnlyProvisionsDataset(t *testing.T) {
	raft := &fakeVMLister{resp: &internalpb.ListVMsResponse{
		Vms: []*internalpb.VMDefinition{{Id: "vm-1", NodeId: "node-a"}},
	}}
	zfs := newFakeDatasetManager()

	r := &Reconciler{Raft: raft, ZFS: zfs, LocalNodeID: "node-a"} // Bhyve left nil
	if err := r.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce() error: %v", err)
	}
	if len(zfs.created) != 1 {
		t.Errorf("created = %v, want [vm-1]", zfs.created)
	}
}

func TestReconciler_RunOnce_CreatesBhyveVMWithResourcesFromVMDefinition(t *testing.T) {
	raft := &fakeVMLister{resp: &internalpb.ListVMsResponse{
		Vms: []*internalpb.VMDefinition{{Id: "vm-1", NodeId: "node-a", Vcpus: 4, MemoryMb: 2048}},
	}}
	zfs := newFakeDatasetManager()
	zfs.mountpointFor["vm-1"] = t.TempDir()
	vms := newFakeVMManager()

	r := &Reconciler{Raft: raft, ZFS: zfs, Bhyve: vms, LocalNodeID: "node-a", BootROM: "/fw/UEFI.fd"}
	if err := r.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce() error: %v", err)
	}

	if len(vms.created) != 1 || vms.created[0] != "vm-1" {
		t.Fatalf("created = %v, want [vm-1]", vms.created)
	}
	cfg := vms.lastCfg["vm-1"]
	if cfg.CPUs != 4 || cfg.MemoryMB != 2048 {
		t.Errorf("CreateVM() cfg = %+v, want CPUs=4 MemoryMB=2048", cfg)
	}
	if cfg.BootROM != "/fw/UEFI.fd" {
		t.Errorf("CreateVM() cfg.BootROM = %q, want /fw/UEFI.fd", cfg.BootROM)
	}
	if cfg.DiskPath == "" {
		t.Errorf("CreateVM() cfg.DiskPath is empty, want a disk image under the dataset's mountpoint")
	}
}

func TestReconciler_RunOnce_UsesDefaultResourcesWhenUnset(t *testing.T) {
	raft := &fakeVMLister{resp: &internalpb.ListVMsResponse{
		Vms: []*internalpb.VMDefinition{{Id: "vm-1", NodeId: "node-a"}}, // no Vcpus/MemoryMb
	}}
	zfs := newFakeDatasetManager()
	zfs.mountpointFor["vm-1"] = t.TempDir()
	vms := newFakeVMManager()

	r := &Reconciler{Raft: raft, ZFS: zfs, Bhyve: vms, LocalNodeID: "node-a", BootROM: "/fw/UEFI.fd"}
	if err := r.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce() error: %v", err)
	}

	cfg := vms.lastCfg["vm-1"]
	if cfg.CPUs != defaultCPUs || cfg.MemoryMB != defaultMemoryMB {
		t.Errorf("CreateVM() cfg = %+v, want defaults CPUs=%d MemoryMB=%d", cfg, defaultCPUs, defaultMemoryMB)
	}
}

func TestReconciler_RunOnce_SkipsBhyveVMAlreadyRunning(t *testing.T) {
	raft := &fakeVMLister{resp: &internalpb.ListVMsResponse{
		Vms: []*internalpb.VMDefinition{{Id: "vm-1", NodeId: "node-a"}},
	}}
	zfs := newFakeDatasetManager()
	zfs.existing["vm-1"] = true
	vms := newFakeVMManager()
	vms.running["vm-1"] = true

	r := &Reconciler{Raft: raft, ZFS: zfs, Bhyve: vms, LocalNodeID: "node-a", BootROM: "/fw/UEFI.fd"}
	if err := r.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce() error: %v", err)
	}
	if len(vms.created) != 0 {
		t.Errorf("created = %v, want nothing (VM already running)", vms.created)
	}
}
