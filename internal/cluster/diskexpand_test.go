package cluster

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	internalpb "github.com/glenjbarber/apiary/api/internalpb"
	"github.com/glenjbarber/apiary/internal/capacity"
)

// ADR-0153's disk expansion in the reconciler. What matters is the
// ORDERING: the copy completes first, the expansion runs only against
// the VM's OWN copy, and a failure at any step leaves no incomplete
// disk behind for the next tick to mistake for a good one.

// fakeTruncate stands in for truncate(1), so a test can observe the
// size the reconciler asked for without actually growing a file by
// gigabytes.
//
// The script must call the REAL truncate by ABSOLUTE path. It cannot
// call it by bare name: the caller puts binDir alone on PATH, so a
// bare "truncate" resolves to this very script, and the script calls
// itself, and each generation forks another. That is a fork bomb, and
// it exhausts the host's process table and memory within seconds.
//
// The real path is resolved HERE, before the caller replaces PATH, so
// the lookup sees the system PATH rather than the shadowing one.
func fakeTruncate(t *testing.T, binDir string) {
	t.Helper()
	realTruncate, err := exec.LookPath("truncate")
	if err != nil {
		t.Fatalf("looking up the real truncate(1) before shadowing PATH: %v", err)
	}
	script := "#!/bin/sh\n# $1 is -s, $2 is the size, $3 is the path\n" + realTruncate + " -s \"$2\" \"$3\"\n"
	if err := os.WriteFile(filepath.Join(binDir, "truncate"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
}

func TestReconciler_ExpandsTheVMsOwnCopyOfTheBaseImage(t *testing.T) {
	binDir := t.TempDir()
	fakeTruncate(t, binDir)
	t.Setenv("PATH", binDir)

	raft := &fakeRaftClient{
		resp: &internalpb.ListVMsResponse{Vms: []*internalpb.VMDefinition{{
			Id: "vm-1", NodeId: "node-a", BaseImageName: "base.raw", DiskSizeMb: 8,
		}}},
	}
	zfs := newFakeDatasetManager()
	mountpoint := t.TempDir()
	zfs.mountpointFor["vm-1"] = mountpoint
	zfs.existing["vm-1"] = true

	// The base image itself is 4 KiB, and its virtual size is known.
	imageDir := t.TempDir()
	imagePath := filepath.Join(imageDir, "base.raw")
	if err := os.WriteFile(imagePath, make([]byte, 4096), 0o600); err != nil {
		t.Fatal(err)
	}
	isos := &fakeISOResolver{
		paths:        map[string]string{"base.raw": imagePath},
		virtualSizes: map[string]int64{"base.raw": 4096},
	}

	r := &Reconciler{Raft: raft, ZFS: zfs, Bhyve: newFakeVMManager(), ISOs: isos, Cloudflare: &fakeCloudflareManager{}, LocalNodeID: "node-a"}
	if err := r.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce() error: %v", err)
	}

	// The VM's own disk is 8 MiB: expanded from the image's 4 KiB.
	diskPath := filepath.Join(mountpoint, diskImageName)
	info, err := os.Stat(diskPath)
	if err != nil {
		t.Fatalf("statting the VM's disk: %v", err)
	}
	if info.Size() != 8*1024*1024 {
		t.Errorf("disk size = %d, want %d", info.Size(), 8*1024*1024)
	}

	// And the UPLOADED image is untouched: it is never opened for
	// writing and never resized.
	imageInfo, err := os.Stat(imagePath)
	if err != nil {
		t.Fatal(err)
	}
	if imageInfo.Size() != 4096 {
		t.Errorf("uploaded image size = %d, want 4096: the upload must never be resized", imageInfo.Size())
	}
}

func TestReconciler_RefusesToExpandAnImageOfUnknownSize(t *testing.T) {
	raft := &fakeRaftClient{
		resp: &internalpb.ListVMsResponse{Vms: []*internalpb.VMDefinition{{
			Id: "vm-1", NodeId: "node-a", BaseImageName: "base.raw", DiskSizeMb: 8,
		}}},
	}
	zfs := newFakeDatasetManager()
	mountpoint := t.TempDir()
	zfs.mountpointFor["vm-1"] = mountpoint
	zfs.existing["vm-1"] = true

	imageDir := t.TempDir()
	imagePath := filepath.Join(imageDir, "base.raw")
	if err := os.WriteFile(imagePath, make([]byte, 4096), 0o600); err != nil {
		t.Fatal(err)
	}
	// The virtual size is UNKNOWN: the image was stored before
	// ADR-0153.
	isos := &fakeISOResolver{
		paths:        map[string]string{"base.raw": imagePath},
		virtualSizes: map[string]int64{},
	}

	r := &Reconciler{Raft: raft, ZFS: zfs, Bhyve: newFakeVMManager(), ISOs: isos, Cloudflare: &fakeCloudflareManager{}, LocalNodeID: "node-a"}
	err := r.RunOnce(context.Background())
	if err == nil {
		t.Fatal("an image of unknown size was used to size a VM's disk, want a refusal")
	}
	if !strings.Contains(err.Error(), "unknown") {
		t.Errorf("refusal %q does not say the size is unknown", err.Error())
	}
}

func TestReconciler_RemovesTheIncompleteDiskWhenTheExpansionFails(t *testing.T) {
	// A truncate that fails.
	binDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(binDir, "truncate"), []byte("#!/bin/sh\nexit 1\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir)

	raft := &fakeRaftClient{
		resp: &internalpb.ListVMsResponse{Vms: []*internalpb.VMDefinition{{
			Id: "vm-1", NodeId: "node-a", BaseImageName: "base.raw", DiskSizeMb: 8,
		}}},
	}
	zfs := newFakeDatasetManager()
	mountpoint := t.TempDir()
	zfs.mountpointFor["vm-1"] = mountpoint
	zfs.existing["vm-1"] = true

	imageDir := t.TempDir()
	imagePath := filepath.Join(imageDir, "base.raw")
	if err := os.WriteFile(imagePath, make([]byte, 4096), 0o600); err != nil {
		t.Fatal(err)
	}
	isos := &fakeISOResolver{
		paths:        map[string]string{"base.raw": imagePath},
		virtualSizes: map[string]int64{"base.raw": 4096},
	}

	r := &Reconciler{Raft: raft, ZFS: zfs, Bhyve: newFakeVMManager(), ISOs: isos, Cloudflare: &fakeCloudflareManager{}, LocalNodeID: "node-a"}
	if err := r.RunOnce(context.Background()); err == nil {
		t.Fatal("a failed expansion was reported as a success")
	}
	// The incomplete disk must be gone: the next tick would otherwise
	// find it and treat it as already created.
	if _, err := os.Stat(filepath.Join(mountpoint, diskImageName)); !os.IsNotExist(err) {
		t.Error("the incomplete disk survived a failed expansion; the next reconcile tick would mistake it for a good one")
	}
}

func TestReconciler_RefusesAnExpansionThatWouldEatTheFreeSpaceReserve(t *testing.T) {
	binDir := t.TempDir()
	fakeTruncate(t, binDir)
	t.Setenv("PATH", binDir)

	raft := &fakeRaftClient{
		resp: &internalpb.ListVMsResponse{Vms: []*internalpb.VMDefinition{{
			Id: "vm-1", NodeId: "node-a", BaseImageName: "base.raw", DiskSizeMb: 8,
		}}},
	}
	zfs := newFakeDatasetManager()
	mountpoint := t.TempDir()
	zfs.mountpointFor["vm-1"] = mountpoint
	zfs.existing["vm-1"] = true

	imageDir := t.TempDir()
	imagePath := filepath.Join(imageDir, "base.raw")
	if err := os.WriteFile(imagePath, make([]byte, 4096), 0o600); err != nil {
		t.Fatal(err)
	}
	isos := &fakeISOResolver{
		paths:        map[string]string{"base.raw": imagePath},
		virtualSizes: map[string]int64{"base.raw": 4096},
	}

	coordinator, err := capacity.New(2)
	if err != nil {
		t.Fatalf("capacity.New() error: %v", err)
	}
	r := &Reconciler{
		Raft: raft, ZFS: zfs, Bhyve: newFakeVMManager(), ISOs: isos,
		Cloudflare: &fakeCloudflareManager{}, LocalNodeID: "node-a",
		Capacity: coordinator,
		CapacityDomain: capacity.Domain{
			Name: "pool", CapacityBytes: 4096 * 1024 * 1024,
		},
		// Only a little free space, so the 1 GiB reserve cannot
		// survive the expansion.
		CapacityFreeSpace: func(string) (int64, error) { return 1024 * 1024 * 1024, nil },
	}

	runErr := r.RunOnce(context.Background())
	if runErr == nil {
		t.Fatal("an expansion that would eat the free-space reserve was accepted, want a refusal")
	}
	if !strings.Contains(runErr.Error(), "reserve") {
		t.Errorf("refusal %q does not name the reserve", runErr.Error())
	}
	if _, err := os.Stat(filepath.Join(mountpoint, diskImageName)); !os.IsNotExist(err) {
		t.Error("the incomplete disk survived a refused expansion")
	}
}

func TestReconciler_UsesThePerImageDefaultWhenItIsLargerThanTheOverride(t *testing.T) {
	binDir := t.TempDir()
	fakeTruncate(t, binDir)
	t.Setenv("PATH", binDir)

	raft := &fakeRaftClient{
		resp: &internalpb.ListVMsResponse{Vms: []*internalpb.VMDefinition{{
			Id: "vm-1", NodeId: "node-a", BaseImageName: "base.raw", DiskSizeMb: 8,
		}}},
	}
	zfs := newFakeDatasetManager()
	mountpoint := t.TempDir()
	zfs.mountpointFor["vm-1"] = mountpoint
	zfs.existing["vm-1"] = true

	imageDir := t.TempDir()
	imagePath := filepath.Join(imageDir, "base.raw")
	if err := os.WriteFile(imagePath, make([]byte, 4096), 0o600); err != nil {
		t.Fatal(err)
	}
	// The image's own default is 16 MiB, larger than the VM's 8 MiB
	// override: the image's default wins, because a disk can never be
	// smaller than the image it holds.
	isos := &fakeISOResolver{
		paths:        map[string]string{"base.raw": imagePath},
		virtualSizes: map[string]int64{"base.raw": 4096},
		defaultSizes: map[string]uint64{"base.raw": 16},
	}

	r := &Reconciler{Raft: raft, ZFS: zfs, Bhyve: newFakeVMManager(), ISOs: isos, Cloudflare: &fakeCloudflareManager{}, LocalNodeID: "node-a"}
	if err := r.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce() error: %v", err)
	}

	info, err := os.Stat(filepath.Join(mountpoint, diskImageName))
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() != 16*1024*1024 {
		t.Errorf("disk size = %d, want %d: the image's own default is the floor for a seeded disk", info.Size(), 16*1024*1024)
	}
}

func TestReconciler_RespectsTheColonyDiskSizeFloor(t *testing.T) {
	raft := &fakeRaftClient{
		resp: &internalpb.ListVMsResponse{Vms: []*internalpb.VMDefinition{{
			Id: "vm-1", NodeId: "node-a",
		}}},
		colonyDiskSize: &internalpb.ColonyDiskSize{FloorMb: 40960},
	}
	zfs := newFakeDatasetManager()
	mountpoint := t.TempDir()
	zfs.mountpointFor["vm-1"] = mountpoint
	zfs.existing["vm-1"] = true

	r := &Reconciler{Raft: raft, ZFS: zfs, Bhyve: newFakeVMManager(), Cloudflare: &fakeCloudflareManager{}, LocalNodeID: "node-a"}
	if err := r.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce() error: %v", err)
	}

	info, err := os.Stat(filepath.Join(mountpoint, diskImageName))
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() != 40960*1024*1024 {
		t.Errorf("disk size = %d, want %d: the Colony floor wins over the per-Comb default", info.Size(), 40960*1024*1024)
	}
}
