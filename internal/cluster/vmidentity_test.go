package cluster

import (
	"context"
	"strings"
	"testing"

	internalpb "github.com/glenjbarber/apiary/api/internalpb"
)

// ADR-0152's reconciler half: a VM whose LOGICAL id was renamed must
// keep the host resources its PHYSICAL id named, and must never have a
// second set provisioned under the new logical id.
//
// The tests below drive the reconciler with a VM whose id and
// resource_id DIFFER, which is exactly the state a renamed VM is in,
// and assert that every physical call site addresses the physical name.

// renamedVM returns a VM as the FSM holds it after a rename: the
// logical id is the new one, the resource_id is the old one.
func renamedVM(logicalID, physicalID, nodeID string) *internalpb.VMDefinition {
	return &internalpb.VMDefinition{
		Id:           logicalID,
		ResourceId:   physicalID,
		Name:         "renamed",
		NodeId:       nodeID,
		DesiredState: internalpb.VMState_VM_STATE_RUNNING,
	}
}

func TestReconciler_RunOnce_UsesThePhysicalIdentityForHostResources(t *testing.T) {
	raft := &fakeRaftClient{
		resp: &internalpb.ListVMsResponse{Vms: []*internalpb.VMDefinition{
			renamedVM("uid-scoped-id", "legacy-id", "node-a"),
		}},
	}
	zfs := newFakeDatasetManager()
	zfs.mountpointFor["legacy-id"] = t.TempDir()
	vms := newFakeVMManager()
	pf := newFakePFManager()

	r := &Reconciler{Raft: raft, ZFS: zfs, Bhyve: vms, PF: pf, Cloudflare: &fakeCloudflareManager{}, LocalNodeID: "node-a", BootROM: "/fw/UEFI.fd"}
	if err := r.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce() error: %v", err)
	}

	// The dataset is named for the PHYSICAL identity, never the new
	// logical one: a rename must not provision a second dataset.
	if len(zfs.created) != 1 || zfs.created[0] != "legacy-id" {
		t.Errorf("created datasets = %v, want exactly [legacy-id]: the physical identity must be used", zfs.created)
	}
	// And the bhyve VM too.
	if len(vms.created) != 1 || vms.created[0] != "legacy-id" {
		t.Errorf("created bhyve VMs = %v, want exactly [legacy-id]", vms.created)
	}
	// The pf anchor is physical too, so the existing rules stay
	// attached to the VM rather than being stranded.
	if _, ok := pf.applied["apiary/vm-legacy-id"]; !ok {
		t.Errorf("pf anchors applied = %v, want the physical anchor apiary/vm-legacy-id", keysOf(pf.applied))
	}
	if _, ok := pf.applied["apiary/vm-uid-scoped-id"]; ok {
		t.Errorf("pf anchors applied = %v, want NO anchor under the new logical id", keysOf(pf.applied))
	}
}

// TestReconciler_RunOnce_DoesNotDestroyTheLegacyPhysicalResources is the
// reclaim half: a renamed VM's legacy-named resources are the EXPECTED
// resources, not orphans, so the reclaim pass must leave them alone.
func TestReconciler_RunOnce_DoesNotDestroyTheLegacyPhysicalResources(t *testing.T) {
	raft := &fakeRaftClient{
		resp: &internalpb.ListVMsResponse{Vms: []*internalpb.VMDefinition{
			renamedVM("uid-scoped-id", "legacy-id", "node-a"),
		}},
	}
	zfs := newFakeDatasetManager()
	// The legacy-named dataset already exists on this node, because the
	// VM was created under that name before it was renamed.
	zfs.existing["legacy-id"] = true
	vms := newFakeVMManager()
	vms.running["legacy-id"] = true

	r := &Reconciler{Raft: raft, ZFS: zfs, Bhyve: vms, Cloudflare: &fakeCloudflareManager{}, LocalNodeID: "node-a", BootROM: "/fw/UEFI.fd"}
	if err := r.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce() error: %v", err)
	}

	if len(zfs.destroyed) != 0 {
		t.Errorf("destroyed datasets = %v, want none: a renamed VM's own dataset is the expected one, not an orphan", zfs.destroyed)
	}
	if len(vms.destroyed) != 0 {
		t.Errorf("destroyed bhyve VMs = %v, want none", vms.destroyed)
	}
}

// TestReconciler_RunOnce_ReclaimsAStalePhysicalResourceThatNoLongerBelongs
// to any VM is the other half: the reclaim pass still works, comparing
// PHYSICAL ids against the current placements.
func TestReconciler_RunOnce_ReclaimsAStalePhysicalResourceThatNoLongerBelongsToAnyVM(t *testing.T) {
	raft := &fakeRaftClient{
		resp: &internalpb.ListVMsResponse{Vms: []*internalpb.VMDefinition{
			// The VM now lives on node-b, so node-a holds leftovers.
			renamedVM("uid-scoped-id", "legacy-id", "node-b"),
		}},
	}
	zfs := newFakeDatasetManager()
	zfs.existing["legacy-id"] = true
	vms := newFakeVMManager()
	vms.running["legacy-id"] = true

	r := &Reconciler{Raft: raft, ZFS: zfs, Bhyve: vms, Cloudflare: &fakeCloudflareManager{}, LocalNodeID: "node-a", BootROM: "/fw/UEFI.fd"}
	if err := r.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce() error: %v", err)
	}

	if len(zfs.destroyed) != 1 || zfs.destroyed[0] != "legacy-id" {
		t.Errorf("destroyed datasets = %v, want [legacy-id]: a physical resource no longer placed here is stale", zfs.destroyed)
	}
	if len(vms.destroyed) != 1 || vms.destroyed[0] != "legacy-id" {
		t.Errorf("destroyed bhyve VMs = %v, want [legacy-id]", vms.destroyed)
	}
}

// TestReconciler_RunOnce_TeardownOfARenamedVMUsesThePhysicalIdentity
// checks the destruction path: a renamed VM that is later deleted must
// have its LEGACY-named resources torn down, and its logical record
// purged.
func TestReconciler_RunOnce_TeardownOfARenamedVMUsesThePhysicalIdentity(t *testing.T) {
	deleting := renamedVM("uid-scoped-id", "legacy-id", "node-a")
	deleting.DesiredState = internalpb.VMState_VM_STATE_DELETING

	raft := &fakeRaftClient{
		resp: &internalpb.ListVMsResponse{Vms: []*internalpb.VMDefinition{deleting}},
	}
	zfs := newFakeDatasetManager()
	zfs.existing["legacy-id"] = true
	vms := newFakeVMManager()
	vms.running["legacy-id"] = true

	r := &Reconciler{Raft: raft, ZFS: zfs, Bhyve: vms, Cloudflare: &fakeCloudflareManager{}, LocalNodeID: "node-a", BootROM: "/fw/UEFI.fd"}
	if err := r.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce() error: %v", err)
	}

	if len(zfs.destroyed) != 1 || zfs.destroyed[0] != "legacy-id" {
		t.Errorf("destroyed datasets = %v, want [legacy-id]: a teardown must use the physical identity", zfs.destroyed)
	}
	if len(vms.destroyed) != 1 || vms.destroyed[0] != "legacy-id" {
		t.Errorf("destroyed bhyve VMs = %v, want [legacy-id]", vms.destroyed)
	}

	// The purge addresses the LOGICAL record, because that is the raft
	// key.
	var purged string
	for _, cmd := range raft.applied {
		if p := cmd.GetPurgeVm(); p != nil {
			purged = p.GetId()
		}
	}
	if purged != "uid-scoped-id" {
		t.Errorf("purged id = %q, want uid-scoped-id: the raft record is keyed by the logical id", purged)
	}
}

// TestReconciler_RunOnce_HASTResourcesUseThePhysicalIdentity checks the
// HAST half: the resource name is physical, on the primary path, the
// replica path and the cleanup path alike.
func TestReconciler_RunOnce_HASTResourcesUseThePhysicalIdentity(t *testing.T) {
	vm := renamedVM("uid-scoped-id", "legacy-id", "node-a")
	vm.ReplicaNodeId = "node-b"

	raft := &fakeRaftClient{
		resp: &internalpb.ListVMsResponse{Vms: []*internalpb.VMDefinition{vm}},
		statusResp: &internalpb.StatusResponse{Servers: []*internalpb.ServerInfo{
			{Id: "node-a", Address: "10.0.0.1:7000"},
			{Id: "node-b", Address: "10.0.0.2:7000"},
		}},
	}
	zfs := newFakeDatasetManager()
	zfs.mountpointFor["legacy-id"] = t.TempDir()
	zfs.mountpointFor["hast-vm-legacy-id"] = t.TempDir()
	hast := newFakeHASTManager()

	r := &Reconciler{Raft: raft, ZFS: zfs, HAST: hast, Cloudflare: &fakeCloudflareManager{}, LocalNodeID: "node-a", BootROM: "/fw/UEFI.fd"}
	if err := r.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce() error: %v", err)
	}

	// The HAST resource is named for the physical identity, so the
	// renamed VM keeps the ONE HAST resource it already had.
	if !strings.Contains(strings.Join(hast.created, ","), "vm-legacy-id") {
		t.Errorf("created HAST resources = %v, want one named vm-legacy-id", hast.created)
	}
	for _, name := range hast.created {
		if strings.Contains(name, "uid-scoped-id") {
			t.Errorf("created HAST resources = %v, want NO resource named after the new logical id: a rename must never create a second HAST resource", hast.created)
		}
	}
}

// TestReconciler_RunOnce_DiskPathLivesUnderThePhysicalDataset checks the
// disk file path: it is inside the physical dataset, so a rename never
// derives a new path from the logical id.
func TestReconciler_RunOnce_DiskPathLivesUnderThePhysicalDataset(t *testing.T) {
	raft := &fakeRaftClient{
		resp: &internalpb.ListVMsResponse{Vms: []*internalpb.VMDefinition{
			renamedVM("uid-scoped-id", "legacy-id", "node-a"),
		}},
	}
	zfs := newFakeDatasetManager()
	mountpoint := t.TempDir()
	zfs.mountpointFor["legacy-id"] = mountpoint
	zfs.existing["legacy-id"] = true
	vms := newFakeVMManager()

	r := &Reconciler{Raft: raft, ZFS: zfs, Bhyve: vms, Cloudflare: &fakeCloudflareManager{}, LocalNodeID: "node-a", BootROM: "/fw/UEFI.fd"}
	if err := r.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce() error: %v", err)
	}

	cfg, ok := vms.lastCfg["legacy-id"]
	if !ok {
		t.Fatalf("no bhyve config recorded for legacy-id; recorded names = %v", keysOf(vms.lastCfg))
	}
	if !strings.HasPrefix(cfg.DiskPath, mountpoint) {
		t.Errorf("disk path = %q, want it under the physical dataset's mountpoint %q", cfg.DiskPath, mountpoint)
	}
}

// TestReconciler_RunOnce_ResolvesASnapshotCloneSourceToItsPhysicalDataset
// checks the snapshot half: the clone source is a LOGICAL reference at
// the public boundary, and resolves to the PHYSICAL dataset at the ZFS
// operation.
func TestReconciler_RunOnce_ResolvesASnapshotCloneSourceToItsPhysicalDataset(t *testing.T) {
	raft := &fakeRaftClient{
		resp: &internalpb.ListVMsResponse{Vms: []*internalpb.VMDefinition{
			// The SOURCE VM, renamed: logical "src-uid", physical
			// "src-legacy".
			{Id: "src-uid", ResourceId: "src-legacy", Name: "src", NodeId: "node-a"},
			// The CLONING VM.
			{
				Id: "dst", ResourceId: "dst", Name: "dst", NodeId: "node-a",
				CloneFromSnapshot: "src-uid@nightly",
			},
		}},
	}
	zfs := newFakeDatasetManager()
	// The source snapshot exists locally under the SOURCE's PHYSICAL
	// name.
	zfs.snapshots["src-legacy@nightly"] = true

	r := &Reconciler{Raft: raft, ZFS: zfs, Cloudflare: &fakeCloudflareManager{}, LocalNodeID: "node-a"}
	if err := r.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce() error: %v", err)
	}

	if len(zfs.cloned) != 1 {
		t.Fatalf("clones = %v, want exactly one", zfs.cloned)
	}
	if zfs.cloned[0] != "src-legacy@nightly->dst" {
		t.Errorf("clone = %q, want the source resolved to its physical dataset: src-legacy@nightly->dst", zfs.cloned[0])
	}
}

// TestReconciler_RunOnce_DHCPLeaseHostnameUsesTheLogicalID checks the
// LOGICAL half of the table: the DHCP hostname and the Cloudflare
// display name follow the caller-facing id.
func TestReconciler_RunOnce_DHCPLeaseHostnameUsesTheLogicalID(t *testing.T) {
	raft := &fakeRaftClient{
		resp: &internalpb.ListVMsResponse{Vms: []*internalpb.VMDefinition{
			{
				Id: "uid-scoped-id", ResourceId: "legacy-id", Name: "renamed", NodeId: "node-a",
				NetworkId: "net-1", IpAddress: "10.60.0.2", MacAddress: "02:aa:bb:cc:dd:ee",
			},
		}},
		networksResp: &internalpb.ListNetworksResponse{Networks: []*internalpb.NetworkDefinition{
			{Id: "net-1", Name: "net-1", Subnet: "10.60.0.0/24", BridgeName: "bridge0"},
		}},
	}
	zfs := newFakeDatasetManager()
	dhcp := &fakeDHCPManager{}
	vlan := newFakeVLANManager()

	r := &Reconciler{Raft: raft, ZFS: zfs, DHCP: dhcp, VLAN: vlan, Cloudflare: &fakeCloudflareManager{}, LocalNodeID: "node-a"}
	if err := r.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce() error: %v", err)
	}

	found := false
	for _, scope := range dhcp.lastScopes {
		for _, lease := range scope.Leases {
			if lease.MAC == "02:aa:bb:cc:dd:ee" {
				found = true
				if lease.Hostname != "uid-scoped-id" {
					t.Errorf("DHCP lease hostname = %q, want uid-scoped-id: the lease hostname is the caller-facing identity", lease.Hostname)
				}
			}
		}
	}
	if !found {
		t.Error("no DHCP lease was written for the VM")
	}
}

// keysOf returns a map's keys, for a test's failure message.
func keysOf[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
