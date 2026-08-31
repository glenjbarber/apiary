package cluster

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	internalpb "github.com/glenjbarber/apiary/api/internalpb"
	"github.com/glenjbarber/apiary/internal/bhyve"
	"github.com/glenjbarber/apiary/internal/dhcpd"
	"github.com/glenjbarber/apiary/internal/hast"
	"github.com/glenjbarber/apiary/internal/pf"
)

// fakeRaftClient is a fake raftClient: ListVMs returns a fixed response,
// and Apply records every command submitted (phase updates, purges) so
// tests can assert on the reconciler's own status-reporting behavior.
type fakeRaftClient struct {
	resp *internalpb.ListVMsResponse
	err  error

	networksResp *internalpb.ListNetworksResponse
	networksErr  error

	statusResp *internalpb.StatusResponse
	statusErr  error

	applied  []*internalpb.Command
	applyErr error
}

func (f *fakeRaftClient) Status(context.Context) (*internalpb.StatusResponse, error) {
	if f.statusResp != nil || f.statusErr != nil {
		return f.statusResp, f.statusErr
	}
	return &internalpb.StatusResponse{}, nil
}

func (f *fakeRaftClient) ListVMs(context.Context) (*internalpb.ListVMsResponse, error) {
	return f.resp, f.err
}

func (f *fakeRaftClient) ListNetworks(context.Context) (*internalpb.ListNetworksResponse, error) {
	if f.networksResp != nil || f.networksErr != nil {
		return f.networksResp, f.networksErr
	}
	return &internalpb.ListNetworksResponse{}, nil
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

func (f *fakeDatasetManager) CreateZvol(_ context.Context, name string, _ uint64) error {
	if f.createErr != nil {
		return f.createErr
	}
	f.created = append(f.created, name)
	f.existing[name] = true
	return nil
}

func (f *fakeDatasetManager) FullPath(name string) (string, error) {
	return "zroot/apiary/" + name, nil
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

type fakeISOResolver struct {
	paths   map[string]string
	pathErr error

	// rawDisk marks names that are NOT genuine ISO9660 media (e.g. a
	// memstick image) - IsISO9660 returns false for these. Defaults to
	// treating every name as real ISO9660, matching every pre-existing
	// test's expectations.
	rawDisk map[string]bool
	isoErr  error
}

func (f *fakeISOResolver) Path(name string) (string, bool, error) {
	if f.pathErr != nil {
		return "", false, f.pathErr
	}
	path, ok := f.paths[name]
	return path, ok, nil
}

func (f *fakeISOResolver) IsISO9660(name string) (bool, error) {
	if f.isoErr != nil {
		return false, f.isoErr
	}
	return !f.rawDisk[name], nil
}

func TestReconciler_RunOnce_AttachesResolvedISOAndBridge(t *testing.T) {
	raft := &fakeRaftClient{resp: &internalpb.ListVMsResponse{
		Vms: []*internalpb.VMDefinition{{Id: "vm-1", NodeId: "node-a", IsoName: "debian.iso"}},
	}}
	zfs := newFakeDatasetManager()
	zfs.mountpointFor["vm-1"] = t.TempDir()
	vms := newFakeVMManager()
	isos := &fakeISOResolver{paths: map[string]string{"debian.iso": "/isos/debian.iso"}}

	r := &Reconciler{Raft: raft, ZFS: zfs, Bhyve: vms, ISOs: isos, Bridge: "bridge0", LocalNodeID: "node-a", BootROM: "/fw/UEFI.fd"}
	if err := r.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce() error: %v", err)
	}

	cfg := vms.lastCfg["vm-1"]
	if cfg.ISOPath != "/isos/debian.iso" {
		t.Errorf("CreateVM() cfg.ISOPath = %q, want /isos/debian.iso", cfg.ISOPath)
	}
	if cfg.Bridge != "bridge0" {
		t.Errorf("CreateVM() cfg.Bridge = %q, want bridge0", cfg.Bridge)
	}
}

func TestReconciler_RunOnce_MemstickImageAttachedAsDiskNotCD(t *testing.T) {
	raft := &fakeRaftClient{resp: &internalpb.ListVMsResponse{
		Vms: []*internalpb.VMDefinition{{Id: "vm-1", NodeId: "node-a", IsoName: "freebsd-memstick.img"}},
	}}
	zfs := newFakeDatasetManager()
	zfs.mountpointFor["vm-1"] = t.TempDir()
	vms := newFakeVMManager()
	isos := &fakeISOResolver{
		paths:   map[string]string{"freebsd-memstick.img": "/isos/freebsd-memstick.img"},
		rawDisk: map[string]bool{"freebsd-memstick.img": true},
	}

	r := &Reconciler{Raft: raft, ZFS: zfs, Bhyve: vms, ISOs: isos, LocalNodeID: "node-a", BootROM: "/fw/UEFI.fd"}
	if err := r.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce() error: %v", err)
	}

	cfg := vms.lastCfg["vm-1"]
	if cfg.InstallDiskPath != "/isos/freebsd-memstick.img" {
		t.Errorf("CreateVM() cfg.InstallDiskPath = %q, want /isos/freebsd-memstick.img", cfg.InstallDiskPath)
	}
	if cfg.ISOPath != "" {
		t.Errorf("CreateVM() cfg.ISOPath = %q, want empty - a memstick image must not go on ahci-cd", cfg.ISOPath)
	}
}

func TestReconciler_RunOnce_ImageFormatCheckFailureAbortsBeforeCreate(t *testing.T) {
	raft := &fakeRaftClient{resp: &internalpb.ListVMsResponse{
		Vms: []*internalpb.VMDefinition{{Id: "vm-1", NodeId: "node-a", IsoName: "debian.iso"}},
	}}
	zfs := newFakeDatasetManager()
	zfs.mountpointFor["vm-1"] = t.TempDir()
	vms := newFakeVMManager()
	isos := &fakeISOResolver{
		paths:  map[string]string{"debian.iso": "/isos/debian.iso"},
		isoErr: errors.New("read error"),
	}

	r := &Reconciler{Raft: raft, ZFS: zfs, Bhyve: vms, ISOs: isos, LocalNodeID: "node-a", BootROM: "/fw/UEFI.fd"}
	if err := r.RunOnce(context.Background()); err == nil {
		t.Fatalf("RunOnce() = nil error, want the image-format check failure surfaced")
	}
	if len(vms.created) != 0 {
		t.Errorf("created = %v, want none (format check should fail before CreateVM)", vms.created)
	}
}

func TestReconciler_RunOnce_UnresolvableISOFailsWithoutCreatingVM(t *testing.T) {
	raft := &fakeRaftClient{resp: &internalpb.ListVMsResponse{
		Vms: []*internalpb.VMDefinition{{Id: "vm-1", NodeId: "node-a", IsoName: "missing.iso"}},
	}}
	zfs := newFakeDatasetManager()
	zfs.mountpointFor["vm-1"] = t.TempDir()
	vms := newFakeVMManager()
	isos := &fakeISOResolver{paths: map[string]string{}}

	r := &Reconciler{Raft: raft, ZFS: zfs, Bhyve: vms, ISOs: isos, LocalNodeID: "node-a", BootROM: "/fw/UEFI.fd"}
	if err := r.RunOnce(context.Background()); err == nil {
		t.Fatalf("RunOnce() = nil error, want a not-found error for the unresolvable ISO")
	}
	if len(vms.created) != 0 {
		t.Errorf("created = %v, want none (ISO resolution should fail before CreateVM)", vms.created)
	}
}

func TestReconciler_RunOnce_ISONamedButNoStoreConfiguredIsError(t *testing.T) {
	raft := &fakeRaftClient{resp: &internalpb.ListVMsResponse{
		Vms: []*internalpb.VMDefinition{{Id: "vm-1", NodeId: "node-a", IsoName: "debian.iso"}},
	}}
	zfs := newFakeDatasetManager()
	zfs.mountpointFor["vm-1"] = t.TempDir()
	vms := newFakeVMManager()

	r := &Reconciler{Raft: raft, ZFS: zfs, Bhyve: vms, LocalNodeID: "node-a", BootROM: "/fw/UEFI.fd"} // ISOs left nil
	if err := r.RunOnce(context.Background()); err == nil {
		t.Fatalf("RunOnce() = nil error, want an error since no ISO store is configured")
	}
}

// --- Network/DHCP/firewall fakes ---

type fakeVLANManager struct {
	ensuredVLANs   []uint32
	ensuredBridges []string
	members        map[string][]string // bridge -> ifaces added
	addresses      map[string]string   // bridge -> subnet
	vlanErr        error
	bridgeErr      error
	memberErr      error
	addressErr     error
}

func newFakeVLANManager() *fakeVLANManager {
	return &fakeVLANManager{members: map[string][]string{}, addresses: map[string]string{}}
}

func (f *fakeVLANManager) EnsureVLAN(_ context.Context, vlanID uint32) (string, error) {
	if f.vlanErr != nil {
		return "", f.vlanErr
	}
	f.ensuredVLANs = append(f.ensuredVLANs, vlanID)
	if vlanID == 0 {
		return "uplink0", nil
	}
	return fmt.Sprintf("vlan%d", vlanID), nil
}

func (f *fakeVLANManager) EnsureBridge(_ context.Context, name string) error {
	if f.bridgeErr != nil {
		return f.bridgeErr
	}
	f.ensuredBridges = append(f.ensuredBridges, name)
	return nil
}

func (f *fakeVLANManager) EnsureMember(_ context.Context, bridge, iface string) error {
	if f.memberErr != nil {
		return f.memberErr
	}
	f.members[bridge] = append(f.members[bridge], iface)
	return nil
}

func (f *fakeVLANManager) EnsureBridgeAddress(_ context.Context, bridge, subnet string) error {
	if f.addressErr != nil {
		return f.addressErr
	}
	f.addresses[bridge] = subnet
	return nil
}

type fakeDHCPManager struct {
	lastScopes []dhcpd.NetworkScope
	calls      int
	err        error
}

func (f *fakeDHCPManager) WriteAndReload(_ context.Context, scopes []dhcpd.NetworkScope) error {
	if f.err != nil {
		return f.err
	}
	f.lastScopes = scopes
	f.calls++
	return nil
}

type fakePFManager struct {
	applied  map[string][]pf.Rule
	flushed  []string
	applyErr error
	flushErr error
}

func newFakePFManager() *fakePFManager {
	return &fakePFManager{applied: map[string][]pf.Rule{}}
}

func (f *fakePFManager) Apply(_ context.Context, anchor string, rules []pf.Rule) error {
	if f.applyErr != nil {
		return f.applyErr
	}
	f.applied[anchor] = rules
	return nil
}

func (f *fakePFManager) Flush(_ context.Context, anchor string) error {
	if f.flushErr != nil {
		return f.flushErr
	}
	f.flushed = append(f.flushed, anchor)
	return nil
}

func TestReconciler_RunOnce_CreatesVMOnNetworkWithAssignedIPAndMAC(t *testing.T) {
	raft := &fakeRaftClient{
		resp: &internalpb.ListVMsResponse{Vms: []*internalpb.VMDefinition{{
			Id: "vm-1", NodeId: "node-a", NetworkId: "net-1", IpAddress: "10.60.0.2", MacAddress: "02:aa:bb:cc:dd:ee",
		}}},
		networksResp: &internalpb.ListNetworksResponse{Networks: []*internalpb.NetworkDefinition{
			{Id: "net-1", VlanId: 100, Subnet: "10.60.0.0/24"},
		}},
	}
	zfs := newFakeDatasetManager()
	zfs.mountpointFor["vm-1"] = t.TempDir()
	vms := newFakeVMManager()
	vlan := newFakeVLANManager()

	r := &Reconciler{Raft: raft, ZFS: zfs, Bhyve: vms, VLAN: vlan, LocalNodeID: "node-a", BootROM: "/fw/UEFI.fd"}
	if err := r.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce() error: %v", err)
	}

	wantBridge := networkBridgeName(&internalpb.NetworkDefinition{Id: "net-1"})
	if len(wantBridge) > 15 {
		t.Fatalf("test setup bug: derived bridge name %q exceeds FreeBSD's 15-char interface name limit", wantBridge)
	}

	cfg := vms.lastCfg["vm-1"]
	if cfg.Bridge != wantBridge {
		t.Errorf("cfg.Bridge = %q, want %q (derived from network id)", cfg.Bridge, wantBridge)
	}
	if cfg.MACAddress != "02:aa:bb:cc:dd:ee" {
		t.Errorf("cfg.MACAddress = %q, want 02:aa:bb:cc:dd:ee", cfg.MACAddress)
	}
	if len(vlan.ensuredVLANs) != 1 || vlan.ensuredVLANs[0] != 100 {
		t.Errorf("ensuredVLANs = %v, want [100]", vlan.ensuredVLANs)
	}
	if vlan.addresses[wantBridge] != "10.60.0.0/24" {
		t.Errorf("bridge address = %q, want 10.60.0.0/24 assigned", vlan.addresses[wantBridge])
	}
	if members := vlan.members[wantBridge]; len(members) != 1 || members[0] != "vlan100" {
		t.Errorf("bridge members = %v, want [vlan100]", members)
	}
}

func TestNetworkBridgeName_FitsFreeBSDInterfaceNameLimit(t *testing.T) {
	// FreeBSD interface names are capped at IF_NAMESIZE (16 bytes
	// including the trailing NUL - 15 usable characters). A network id
	// of any length a caller might choose must still produce a bridge
	// name that fits, since the id itself isn't under this package's
	// control.
	for _, id := range []string{"a", "net-1", "a-very-long-network-identifier-chosen-by-a-user"} {
		name := networkBridgeName(&internalpb.NetworkDefinition{Id: id})
		if len(name) > 15 {
			t.Errorf("networkBridgeName(id=%q) = %q (%d chars), want <= 15", id, name, len(name))
		}
	}
}

func TestNetworkBridgeName_PrefersExplicitOverride(t *testing.T) {
	name := networkBridgeName(&internalpb.NetworkDefinition{Id: "net-1", BridgeName: "custom0"})
	if name != "custom0" {
		t.Errorf("networkBridgeName() = %q, want the explicit override custom0", name)
	}
}

func TestReconciler_RunOnce_VMOnUnknownNetworkFailsBeforeCreate(t *testing.T) {
	raft := &fakeRaftClient{
		resp:         &internalpb.ListVMsResponse{Vms: []*internalpb.VMDefinition{{Id: "vm-1", NodeId: "node-a", NetworkId: "missing"}}},
		networksResp: &internalpb.ListNetworksResponse{},
	}
	zfs := newFakeDatasetManager()
	zfs.mountpointFor["vm-1"] = t.TempDir()
	vms := newFakeVMManager()
	vlan := newFakeVLANManager()

	r := &Reconciler{Raft: raft, ZFS: zfs, Bhyve: vms, VLAN: vlan, LocalNodeID: "node-a", BootROM: "/fw/UEFI.fd"}
	if err := r.RunOnce(context.Background()); err == nil {
		t.Fatalf("RunOnce() = nil error, want a network-not-found error")
	}
	if len(vms.created) != 0 {
		t.Errorf("created = %v, want none", vms.created)
	}
}

func TestReconciler_RunOnce_VMOnNetworkWithoutVLANConfiguredIsError(t *testing.T) {
	raft := &fakeRaftClient{resp: &internalpb.ListVMsResponse{
		Vms: []*internalpb.VMDefinition{{Id: "vm-1", NodeId: "node-a", NetworkId: "net-1"}},
	}}
	zfs := newFakeDatasetManager()
	zfs.mountpointFor["vm-1"] = t.TempDir()
	vms := newFakeVMManager()

	r := &Reconciler{Raft: raft, ZFS: zfs, Bhyve: vms, LocalNodeID: "node-a", BootROM: "/fw/UEFI.fd"} // VLAN left nil
	if err := r.RunOnce(context.Background()); err == nil {
		t.Fatalf("RunOnce() = nil error, want an error since no VLAN support is configured")
	}
	if len(vms.created) != 0 {
		t.Errorf("created = %v, want none", vms.created)
	}
}

func TestReconciler_RunOnce_AppliesFirewallRulesOnCreate(t *testing.T) {
	raft := &fakeRaftClient{resp: &internalpb.ListVMsResponse{Vms: []*internalpb.VMDefinition{{
		Id: "vm-1", NodeId: "node-a",
		FirewallRules: []*internalpb.FirewallRule{{Direction: "in", Action: "block", Protocol: "tcp", PortRange: "22"}},
	}}}}
	zfs := newFakeDatasetManager()
	zfs.mountpointFor["vm-1"] = t.TempDir()
	vms := newFakeVMManager()
	pfMgr := newFakePFManager()

	r := &Reconciler{Raft: raft, ZFS: zfs, Bhyve: vms, PF: pfMgr, LocalNodeID: "node-a", BootROM: "/fw/UEFI.fd"}
	if err := r.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce() error: %v", err)
	}

	rules := pfMgr.applied["apiary/vm-vm-1"]
	if len(rules) != 1 || rules[0].PortRange != "22" {
		t.Errorf("pf rules applied to apiary/vm-vm-1 = %v, want one rule with PortRange=22", rules)
	}
}

func TestReconciler_RunOnce_ReappliesFirewallRulesForAlreadyRunningVM(t *testing.T) {
	raft := &fakeRaftClient{resp: &internalpb.ListVMsResponse{Vms: []*internalpb.VMDefinition{{
		Id: "vm-1", NodeId: "node-a", Phase: internalpb.VMPhase_VM_PHASE_READY,
		FirewallRules: []*internalpb.FirewallRule{{Direction: "out", Action: "pass", Protocol: "udp", PortRange: "53"}},
	}}}}
	zfs := newFakeDatasetManager()
	zfs.mountpointFor["vm-1"] = t.TempDir()
	vms := newFakeVMManager()
	vms.running["vm-1"] = true // already running
	pfMgr := newFakePFManager()

	r := &Reconciler{Raft: raft, ZFS: zfs, Bhyve: vms, PF: pfMgr, LocalNodeID: "node-a", BootROM: "/fw/UEFI.fd"}
	if err := r.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce() error: %v", err)
	}
	if len(vms.created) != 0 {
		t.Errorf("created = %v, want none (VM already running)", vms.created)
	}
	if rules := pfMgr.applied["apiary/vm-vm-1"]; len(rules) != 1 {
		t.Errorf("pf rules applied to apiary/vm-vm-1 = %v, want one rule even though the VM was already running", rules)
	}
}

func TestReconciler_RunOnce_FlushesFirewallOnTeardown(t *testing.T) {
	raft := &fakeRaftClient{resp: &internalpb.ListVMsResponse{Vms: []*internalpb.VMDefinition{{
		Id: "vm-1", NodeId: "node-a", DesiredState: internalpb.VMState_VM_STATE_DELETING,
	}}}}
	zfs := newFakeDatasetManager()
	zfs.existing["vm-1"] = true
	vms := newFakeVMManager()
	pfMgr := newFakePFManager()

	r := &Reconciler{Raft: raft, ZFS: zfs, Bhyve: vms, PF: pfMgr, LocalNodeID: "node-a", BootROM: "/fw/UEFI.fd"}
	if err := r.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce() error: %v", err)
	}
	if len(pfMgr.flushed) != 1 || pfMgr.flushed[0] != "apiary/vm-vm-1" {
		t.Errorf("flushed = %v, want [apiary/vm-vm-1]", pfMgr.flushed)
	}
}

func TestReconciler_RunOnce_ReconcilesDHCPLeasesForNetworkedVMs(t *testing.T) {
	raft := &fakeRaftClient{
		resp: &internalpb.ListVMsResponse{Vms: []*internalpb.VMDefinition{{
			Id: "vm-1", NodeId: "node-a", NetworkId: "net-1", IpAddress: "10.60.0.2", MacAddress: "02:aa:bb:cc:dd:ee",
		}}},
		networksResp: &internalpb.ListNetworksResponse{Networks: []*internalpb.NetworkDefinition{
			{Id: "net-1", Subnet: "10.60.0.0/24"},
		}},
	}
	zfs := newFakeDatasetManager()
	zfs.mountpointFor["vm-1"] = t.TempDir()
	vms := newFakeVMManager()
	vlan := newFakeVLANManager()
	dhcp := &fakeDHCPManager{}

	r := &Reconciler{Raft: raft, ZFS: zfs, Bhyve: vms, VLAN: vlan, DHCP: dhcp, LocalNodeID: "node-a", BootROM: "/fw/UEFI.fd"}
	if err := r.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce() error: %v", err)
	}

	if dhcp.calls != 1 {
		t.Fatalf("dhcp.calls = %d, want 1", dhcp.calls)
	}
	if len(dhcp.lastScopes) != 1 || len(dhcp.lastScopes[0].Leases) != 1 {
		t.Fatalf("lastScopes = %+v, want one scope with one lease", dhcp.lastScopes)
	}
	lease := dhcp.lastScopes[0].Leases[0]
	if lease.IP != "10.60.0.2" || lease.MAC != "02:aa:bb:cc:dd:ee" {
		t.Errorf("lease = %+v, want IP=10.60.0.2 MAC=02:aa:bb:cc:dd:ee", lease)
	}

	// A second tick with nothing changed must not restart dnsmasq again.
	if err := r.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce() (2nd tick) error: %v", err)
	}
	if dhcp.calls != 1 {
		t.Errorf("dhcp.calls after unchanged 2nd tick = %d, want still 1", dhcp.calls)
	}
}

func TestReconciler_RunOnce_ReclaimsResourcesForVMReassignedElsewhere(t *testing.T) {
	// vm-1 used to be on node-a (this node) and has since been
	// reassigned to node-b - the record still exists, it's just no
	// longer this node's responsibility. node-a still has leftover
	// local resources under vm-1's id from before the reassignment.
	raft := &fakeRaftClient{resp: &internalpb.ListVMsResponse{
		Vms: []*internalpb.VMDefinition{{Id: "vm-1", NodeId: "node-b"}},
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
	// The record itself belongs to node-b now - node-a must not purge it.
	if got := raft.purgedIDs(); len(got) != 0 {
		t.Errorf("purgedIDs = %v, want none - a reassigned VM's record is not this node's to remove", got)
	}
}

func TestReconciler_RunOnce_ReclaimIsNoOpWhenNothingLocalExists(t *testing.T) {
	// The common case: vm-1 is assigned to node-b and has never touched
	// node-a at all. Reclaim must do nothing (no destroy calls), not
	// error.
	raft := &fakeRaftClient{resp: &internalpb.ListVMsResponse{
		Vms: []*internalpb.VMDefinition{{Id: "vm-1", NodeId: "node-b"}},
	}}
	zfs := newFakeDatasetManager()
	vms := newFakeVMManager()

	r := &Reconciler{Raft: raft, ZFS: zfs, Bhyve: vms, LocalNodeID: "node-a", BootROM: "/fw/UEFI.fd"}
	if err := r.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce() error: %v", err)
	}

	if len(vms.destroyed) != 0 || len(zfs.destroyed) != 0 {
		t.Errorf("destroyed bhyve=%v zfs=%v, want none", vms.destroyed, zfs.destroyed)
	}
}

type fakeHASTManager struct {
	writtenConfigs []([]hast.Resource)
	created        []string
	roleSet        map[string]hast.Role
	restarts       int

	// statusKnown tracks resources this fake considers "already
	// created" (Status succeeds) - mirrors real hastctl requiring
	// CreateResource before Status/SetRole succeed.
	statusKnown map[string]bool

	createErr  error
	roleErr    error
	restartErr error
}

func newFakeHASTManager() *fakeHASTManager {
	return &fakeHASTManager{roleSet: map[string]hast.Role{}, statusKnown: map[string]bool{}}
}

func (f *fakeHASTManager) WriteConfig(resources []hast.Resource) error {
	f.writtenConfigs = append(f.writtenConfigs, resources)
	return nil
}

func (f *fakeHASTManager) CreateResource(_ context.Context, name string) error {
	if f.createErr != nil {
		return f.createErr
	}
	f.created = append(f.created, name)
	f.statusKnown[name] = true
	return nil
}

func (f *fakeHASTManager) SetRole(_ context.Context, name string, role hast.Role) error {
	if f.roleErr != nil {
		return f.roleErr
	}
	f.roleSet[name] = role
	return nil
}

func (f *fakeHASTManager) Status(_ context.Context, name string) (*hast.Status, error) {
	if !f.statusKnown[name] {
		return nil, fmt.Errorf("fakeHASTManager: resource %q not created", name)
	}
	return &hast.Status{Role: string(f.roleSet[name])}, nil
}

func (f *fakeHASTManager) RestartService(context.Context) error {
	if f.restartErr != nil {
		return f.restartErr
	}
	f.restarts++
	return nil
}

// statusResponseWithPeers builds a fake raft StatusResponse listing
// localID/peerID with plausible raft-transport addresses - enough for
// resolvePeerAddresses to succeed.
func statusResponseWithPeers(localID, localAddr, peerID, peerAddr string) *internalpb.StatusResponse {
	return &internalpb.StatusResponse{
		Servers: []*internalpb.ServerInfo{
			{Id: localID, Address: localAddr},
			{Id: peerID, Address: peerAddr},
		},
	}
}

func TestReconciler_RunOnce_ProvisionsHASTPrimaryForReplicatedVM(t *testing.T) {
	raft := &fakeRaftClient{
		resp: &internalpb.ListVMsResponse{
			Vms: []*internalpb.VMDefinition{{Id: "vm-1", NodeId: "node-a", ReplicaNodeId: "node-b"}},
		},
		statusResp: statusResponseWithPeers("node-a", "10.0.0.1:17600", "node-b", "10.0.0.2:17600"),
	}
	zfs := newFakeDatasetManager()
	vms := newFakeVMManager()
	h := newFakeHASTManager()

	r := &Reconciler{Raft: raft, ZFS: zfs, Bhyve: vms, HAST: h, LocalNodeID: "node-a", BootROM: "/fw/UEFI.fd"}
	if err := r.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce() error: %v", err)
	}

	if len(zfs.created) != 1 || zfs.created[0] != "hast-vm-vm-1" {
		t.Errorf("created zvols = %v, want [hast-vm-vm-1]", zfs.created)
	}
	if role := h.roleSet["vm-vm-1"]; role != hast.RolePrimary {
		t.Errorf("role for vm-vm-1 = %q, want primary", role)
	}
	if len(h.writtenConfigs) == 0 {
		t.Fatalf("WriteConfig was never called")
	}
	last := h.writtenConfigs[len(h.writtenConfigs)-1]
	if len(last) != 1 || last[0].Name != "vm-vm-1" {
		t.Fatalf("written resources = %+v, want one named vm-vm-1", last)
	}
	if h.restarts != 1 {
		t.Errorf("restarts = %d, want 1", h.restarts)
	}

	cfg, ok := vms.lastCfg["vm-1"]
	if !ok {
		t.Fatalf("bhyve CreateVM was never called for vm-1")
	}
	if cfg.DiskPath != "/dev/hast/vm-vm-1" {
		t.Errorf("bhyve DiskPath = %q, want /dev/hast/vm-vm-1", cfg.DiskPath)
	}

	// A replicated VM gets no plain per-VM dataset - the HAST device is
	// its whole disk, there's nothing useful for a dataset to hold.
	if zfs.existing["vm-1"] {
		t.Errorf("plain dataset vm-1 was created for a replicated VM, want none")
	}
}

func TestReconciler_RunOnce_ProvisionsHASTSecondaryForReplicaAssignment(t *testing.T) {
	raft := &fakeRaftClient{
		resp: &internalpb.ListVMsResponse{
			Vms: []*internalpb.VMDefinition{{Id: "vm-1", NodeId: "node-b", ReplicaNodeId: "node-a"}},
		},
		statusResp: statusResponseWithPeers("node-a", "10.0.0.1:17600", "node-b", "10.0.0.2:17600"),
	}
	zfs := newFakeDatasetManager()
	vms := newFakeVMManager()
	h := newFakeHASTManager()

	// node-a is only named as the *replica*, never the owner - it must
	// never create a bhyve VM, only replicate.
	r := &Reconciler{Raft: raft, ZFS: zfs, Bhyve: vms, HAST: h, LocalNodeID: "node-a", BootROM: "/fw/UEFI.fd"}
	if err := r.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce() error: %v", err)
	}

	if role := h.roleSet["vm-vm-1"]; role != hast.RoleSecondary {
		t.Errorf("role for vm-vm-1 = %q, want secondary", role)
	}
	if len(vms.created) != 0 {
		t.Errorf("bhyve VMs created = %v, want none - a replica never runs the VM", vms.created)
	}
	if len(zfs.created) != 1 || zfs.created[0] != "hast-vm-vm-1" {
		t.Errorf("created zvols = %v, want [hast-vm-vm-1]", zfs.created)
	}
}

func TestReconciler_RunOnce_ReclaimsHASTSecondaryNoLongerAssigned(t *testing.T) {
	raft := &fakeRaftClient{
		resp: &internalpb.ListVMsResponse{
			// vm-1 is no longer replicated to node-a at all.
			Vms: []*internalpb.VMDefinition{{Id: "vm-1", NodeId: "node-b"}},
		},
	}
	zfs := newFakeDatasetManager()
	zfs.existing["hast-vm-vm-1"] = true // leftover from before reassignment

	r := &Reconciler{Raft: raft, ZFS: zfs, LocalNodeID: "node-a"}
	if err := r.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce() error: %v", err)
	}

	if len(zfs.destroyed) != 1 || zfs.destroyed[0] != "hast-vm-vm-1" {
		t.Errorf("destroyed = %v, want [hast-vm-vm-1]", zfs.destroyed)
	}
}

func TestReconciler_RunOnce_DeletingReplicatedVMReclaimsHASTNotDataset(t *testing.T) {
	raft := &fakeRaftClient{
		resp: &internalpb.ListVMsResponse{
			Vms: []*internalpb.VMDefinition{{Id: "vm-1", NodeId: "node-a", ReplicaNodeId: "node-b", DesiredState: internalpb.VMState_VM_STATE_DELETING}},
		},
	}
	zfs := newFakeDatasetManager()
	zfs.existing["hast-vm-vm-1"] = true

	r := &Reconciler{Raft: raft, ZFS: zfs, LocalNodeID: "node-a"}
	if err := r.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce() error: %v", err)
	}

	if len(zfs.destroyed) != 1 || zfs.destroyed[0] != "hast-vm-vm-1" {
		t.Errorf("destroyed = %v, want [hast-vm-vm-1]", zfs.destroyed)
	}
	if got := raft.purgedIDs(); len(got) != 1 || got[0] != "vm-1" {
		t.Errorf("purgedIDs = %v, want [vm-1]", got)
	}
}

func TestReconciler_RunOnce_ReplicatedVMWithoutHASTConfiguredIsError(t *testing.T) {
	raft := &fakeRaftClient{
		resp: &internalpb.ListVMsResponse{
			Vms: []*internalpb.VMDefinition{{Id: "vm-1", NodeId: "node-a", ReplicaNodeId: "node-b"}},
		},
	}
	zfs := newFakeDatasetManager()

	// HAST is deliberately left nil.
	r := &Reconciler{Raft: raft, ZFS: zfs, LocalNodeID: "node-a"}
	if err := r.RunOnce(context.Background()); err == nil {
		t.Fatalf("RunOnce() error = nil, want a clear error since HAST isn't configured")
	}
}

func TestReconciler_RunOnce_ReclaimPropagatesDestroyError(t *testing.T) {
	raft := &fakeRaftClient{resp: &internalpb.ListVMsResponse{
		Vms: []*internalpb.VMDefinition{{Id: "vm-1", NodeId: "node-b"}},
	}}
	zfs := newFakeDatasetManager()
	zfs.existing["vm-1"] = true
	zfs.destroyErr = errors.New("dataset busy")

	r := &Reconciler{Raft: raft, ZFS: zfs, LocalNodeID: "node-a"}
	if err := r.RunOnce(context.Background()); err == nil {
		t.Fatalf("RunOnce() error = nil, want the reclaim destroy error surfaced")
	}
}
