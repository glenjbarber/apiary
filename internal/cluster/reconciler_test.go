package cluster

import (
	"context"
	"errors"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	internalpb "github.com/glenjbarber/apiary/api/internalpb"
	"github.com/glenjbarber/apiary/internal/bhyve"
)

// fakeRaftClient is a fake raftClient: ListVMs returns a fixed response,
// and Apply records every command submitted (phase updates, purges) so
// tests can assert on the reconciler's own status-reporting behavior.
type fakeRaftClient struct {
	resp *internalpb.ListVMsResponse
	err  error

	applied  []*internalpb.Command
	applyErr error
}

func (f *fakeRaftClient) ListVMs(context.Context) (*internalpb.ListVMsResponse, error) {
	return f.resp, f.err
}

func (f *fakeRaftClient) Apply(_ context.Context, payload []byte, _ time.Duration) (*internalpb.ApplyResponse, error) {
	if f.applyErr != nil {
		return nil, f.applyErr
	}
	var cmd internalpb.Command
	if err := proto.Unmarshal(payload, &cmd); err != nil {
		return nil, err
	}
	f.applied = append(f.applied, &cmd)
	return &internalpb.ApplyResponse{}, nil
}

// phaseUpdatesFor returns every phase this fake recorded being applied
// to id, in submission order.
func (f *fakeRaftClient) phaseUpdatesFor(id string) []internalpb.VMPhase {
	var phases []internalpb.VMPhase
	for _, cmd := range f.applied {
		if upd := cmd.GetUpdateVmPhase(); upd != nil && upd.GetId() == id {
			phases = append(phases, upd.GetPhase())
		}
	}
	return phases
}

func (f *fakeRaftClient) purgedIDs() []string {
	var ids []string
	for _, cmd := range f.applied {
		if p := cmd.GetPurgeVm(); p != nil {
			ids = append(ids, p.GetId())
		}
	}
	return ids
}

type fakeDatasetManager struct {
	existing  map[string]bool
	created   []string
	destroyed []string

	existsErr     error
	createErr     error
	destroyErr    error
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

func (f *fakeDatasetManager) DestroyDataset(_ context.Context, name string) error {
	if f.destroyErr != nil {
		return f.destroyErr
	}
	f.destroyed = append(f.destroyed, name)
	delete(f.existing, name)
	return nil
}

func (f *fakeDatasetManager) GetProperty(_ context.Context, name, _ string) (string, error) {
	if f.getPropErr != nil {
		return "", f.getPropErr
	}
	return f.mountpointFor[name], nil
}

type fakeVMManager struct {
	running    map[string]bool
	created    []string
	destroyed  []string
	lastCfg    map[string]bhyve.Config
	existsErr  error
	createErr  error
	destroyErr error
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

func (f *fakeVMManager) DestroyVM(_ context.Context, name string) error {
	if f.destroyErr != nil {
		return f.destroyErr
	}
	f.destroyed = append(f.destroyed, name)
	delete(f.running, name)
	return nil
}

func TestReconciler_RunOnce_CreatesMissingDatasets(t *testing.T) {
	raft := &fakeRaftClient{resp: &internalpb.ListVMsResponse{
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
	raft := &fakeRaftClient{resp: &internalpb.ListVMsResponse{
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
	raft := &fakeRaftClient{err: errors.New("raftd unreachable")}
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
	raft := &fakeRaftClient{resp: &internalpb.ListVMsResponse{Error: "not the leader"}}
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
	raft := &fakeRaftClient{resp: &internalpb.ListVMsResponse{
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
	raft := &fakeRaftClient{resp: &internalpb.ListVMsResponse{
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
	raft := &fakeRaftClient{resp: &internalpb.ListVMsResponse{
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
	raft := &fakeRaftClient{resp: &internalpb.ListVMsResponse{
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
	raft := &fakeRaftClient{resp: &internalpb.ListVMsResponse{
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

func TestReconciler_RunOnce_ReportsCreatingThenReadyPhase(t *testing.T) {
	raft := &fakeRaftClient{resp: &internalpb.ListVMsResponse{
		Vms: []*internalpb.VMDefinition{{Id: "vm-1", NodeId: "node-a"}},
	}}
	zfs := newFakeDatasetManager()

	r := &Reconciler{Raft: raft, ZFS: zfs, LocalNodeID: "node-a"}
	if err := r.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce() error: %v", err)
	}

	got := raft.phaseUpdatesFor("vm-1")
	want := []internalpb.VMPhase{internalpb.VMPhase_VM_PHASE_CREATING, internalpb.VMPhase_VM_PHASE_READY}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("phase updates = %v, want %v", got, want)
	}
}

func TestReconciler_RunOnce_SkipsRedundantPhaseUpdateWhenAlreadyReady(t *testing.T) {
	raft := &fakeRaftClient{resp: &internalpb.ListVMsResponse{
		Vms: []*internalpb.VMDefinition{{Id: "vm-1", NodeId: "node-a", Phase: internalpb.VMPhase_VM_PHASE_READY}},
	}}
	zfs := newFakeDatasetManager()
	zfs.existing["vm-1"] = true

	r := &Reconciler{Raft: raft, ZFS: zfs, LocalNodeID: "node-a"}
	if err := r.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce() error: %v", err)
	}

	if got := raft.phaseUpdatesFor("vm-1"); len(got) != 0 {
		t.Errorf("phase updates = %v, want none (already ready, dataset already exists)", got)
	}
}

func TestReconciler_RunOnce_ReportsErrorPhaseOnFailure(t *testing.T) {
	raft := &fakeRaftClient{resp: &internalpb.ListVMsResponse{
		Vms: []*internalpb.VMDefinition{{Id: "vm-1", NodeId: "node-a"}},
	}}
	zfs := newFakeDatasetManager()
	zfs.createErr = errors.New("disk full")

	r := &Reconciler{Raft: raft, ZFS: zfs, LocalNodeID: "node-a"}
	if err := r.RunOnce(context.Background()); err == nil {
		t.Fatalf("RunOnce() = nil error, want the underlying CreateDataset error surfaced")
	}

	got := raft.phaseUpdatesFor("vm-1")
	if len(got) != 2 || got[0] != internalpb.VMPhase_VM_PHASE_CREATING || got[1] != internalpb.VMPhase_VM_PHASE_ERROR {
		t.Errorf("phase updates = %v, want [CREATING, ERROR]", got)
	}
}

func TestReconciler_RunOnce_DeletingVMTearsDownDatasetAndPurges(t *testing.T) {
	raft := &fakeRaftClient{resp: &internalpb.ListVMsResponse{
		Vms: []*internalpb.VMDefinition{{Id: "vm-1", NodeId: "node-a", DesiredState: internalpb.VMState_VM_STATE_DELETING}},
	}}
	zfs := newFakeDatasetManager()
	zfs.existing["vm-1"] = true

	r := &Reconciler{Raft: raft, ZFS: zfs, LocalNodeID: "node-a"}
	if err := r.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce() error: %v", err)
	}

	if len(zfs.destroyed) != 1 || zfs.destroyed[0] != "vm-1" {
		t.Errorf("destroyed = %v, want [vm-1]", zfs.destroyed)
	}
	if got := raft.purgedIDs(); len(got) != 1 || got[0] != "vm-1" {
		t.Errorf("purgedIDs = %v, want [vm-1]", got)
	}
}

func TestReconciler_RunOnce_DeletingVMTearsDownBhyveVMFirst(t *testing.T) {
	raft := &fakeRaftClient{resp: &internalpb.ListVMsResponse{
		Vms: []*internalpb.VMDefinition{{Id: "vm-1", NodeId: "node-a", DesiredState: internalpb.VMState_VM_STATE_DELETING}},
	}}
	zfs := newFakeDatasetManager()
	zfs.existing["vm-1"] = true
	vms := newFakeVMManager()
	vms.running["vm-1"] = true

	r := &Reconciler{Raft: raft, ZFS: zfs, Bhyve: vms, LocalNodeID: "node-a", BootROM: "/fw/UEFI.fd"}
	if err := r.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce() error: %v", err)
	}

	if len(vms.destroyed) != 1 || vms.destroyed[0] != "vm-1" {
		t.Errorf("bhyve destroyed = %v, want [vm-1]", vms.destroyed)
	}
	if len(zfs.destroyed) != 1 || zfs.destroyed[0] != "vm-1" {
		t.Errorf("dataset destroyed = %v, want [vm-1]", zfs.destroyed)
	}
	if got := raft.purgedIDs(); len(got) != 1 || got[0] != "vm-1" {
		t.Errorf("purgedIDs = %v, want [vm-1]", got)
	}
}

func TestReconciler_RunOnce_DeletingVMWithNoResourcesStillPurges(t *testing.T) {
	// A VM that was tombstoned before it was ever actually reconciled
	// (no dataset, no bhyve VM) must still converge - teardown is a
	// no-op for resources that don't exist, but the purge still happens.
	raft := &fakeRaftClient{resp: &internalpb.ListVMsResponse{
		Vms: []*internalpb.VMDefinition{{Id: "vm-1", NodeId: "node-a", DesiredState: internalpb.VMState_VM_STATE_DELETING}},
	}}
	zfs := newFakeDatasetManager()

	r := &Reconciler{Raft: raft, ZFS: zfs, LocalNodeID: "node-a"}
	if err := r.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce() error: %v", err)
	}

	if len(zfs.destroyed) != 0 {
		t.Errorf("destroyed = %v, want none (nothing existed)", zfs.destroyed)
	}
	if got := raft.purgedIDs(); len(got) != 1 || got[0] != "vm-1" {
		t.Errorf("purgedIDs = %v, want [vm-1]", got)
	}
}

func TestReconciler_RunOnce_DeletingVMPropagatesTeardownError(t *testing.T) {
	raft := &fakeRaftClient{resp: &internalpb.ListVMsResponse{
		Vms: []*internalpb.VMDefinition{{Id: "vm-1", NodeId: "node-a", DesiredState: internalpb.VMState_VM_STATE_DELETING}},
	}}
	zfs := newFakeDatasetManager()
	zfs.existing["vm-1"] = true
	zfs.destroyErr = errors.New("dataset busy")

	r := &Reconciler{Raft: raft, ZFS: zfs, LocalNodeID: "node-a"}
	if err := r.RunOnce(context.Background()); err == nil {
		t.Fatalf("RunOnce() = nil error, want the underlying DestroyDataset error surfaced")
	}
	if got := raft.purgedIDs(); len(got) != 0 {
		t.Errorf("purgedIDs = %v, want none (teardown failed, must not purge)", got)
	}
}
