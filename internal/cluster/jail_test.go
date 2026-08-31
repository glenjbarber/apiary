package cluster

import (
	"context"
	"testing"
	"time"

	internalpb "github.com/glenjbarber/apiary/api/internalpb"
	"github.com/glenjbarber/apiary/internal/hast"
	"github.com/glenjbarber/apiary/internal/jail"
)

type fakeJailManager struct {
	running    map[string]bool
	created    []string
	destroyed  []string
	lastCfg    map[string]jail.Config
	existsErr  error
	createErr  error
	destroyErr error
}

func newFakeJailManager() *fakeJailManager {
	return &fakeJailManager{running: map[string]bool{}, lastCfg: map[string]jail.Config{}}
}

func (f *fakeJailManager) JailExists(_ context.Context, name string) (bool, error) {
	if f.existsErr != nil {
		return false, f.existsErr
	}
	return f.running[name], nil
}

func (f *fakeJailManager) CreateJail(_ context.Context, name string, cfg jail.Config) error {
	if f.createErr != nil {
		return f.createErr
	}
	f.created = append(f.created, name)
	f.lastCfg[name] = cfg
	f.running[name] = true
	return nil
}

func (f *fakeJailManager) RemoveJail(_ context.Context, name string) error {
	if f.destroyErr != nil {
		return f.destroyErr
	}
	f.destroyed = append(f.destroyed, name)
	delete(f.running, name)
	return nil
}

type fakeMountManager struct {
	formatted map[string]bool
	mounted   map[string]string // mountPoint -> devicePath

	formatErr  error
	mountErr   error
	unmountErr error
}

func newFakeMountManager() *fakeMountManager {
	return &fakeMountManager{formatted: map[string]bool{}, mounted: map[string]string{}}
}

func (f *fakeMountManager) FormatIfNeeded(_ context.Context, devicePath string) error {
	if f.formatErr != nil {
		return f.formatErr
	}
	f.formatted[devicePath] = true
	return nil
}

func (f *fakeMountManager) Mount(_ context.Context, devicePath, mountPoint string) error {
	if f.mountErr != nil {
		return f.mountErr
	}
	f.mounted[mountPoint] = devicePath
	return nil
}

func (f *fakeMountManager) Unmount(_ context.Context, mountPoint string) error {
	if f.unmountErr != nil {
		return f.unmountErr
	}
	delete(f.mounted, mountPoint)
	return nil
}

func TestReconciler_RunOnce_CreatesJailOnPlainDataset(t *testing.T) {
	raft := &fakeRaftClient{
		jailsResp: &internalpb.ListJailsResponse{
			Jails: []*internalpb.JailDefinition{{Id: "jail-1", Name: "web-1", Hostname: "web-1.local", NodeId: "node-a"}},
		},
	}
	zfs := newFakeDatasetManager()
	zfs.mountpointFor["jail-1"] = t.TempDir()
	jm := newFakeJailManager()

	r := &Reconciler{Raft: raft, ZFS: zfs, Jail: jm, LocalNodeID: "node-a"}
	if err := r.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce() error: %v", err)
	}

	if !zfs.existing["jail-1"] {
		t.Errorf("dataset jail-1 was not created")
	}
	cfg, ok := jm.lastCfg["jail-1"]
	if !ok {
		t.Fatalf("CreateJail was never called for jail-1")
	}
	if cfg.Hostname != "web-1.local" {
		t.Errorf("Hostname = %q, want web-1.local", cfg.Hostname)
	}
	if cfg.Path != zfs.mountpointFor["jail-1"] {
		t.Errorf("Path = %q, want dataset mountpoint %q", cfg.Path, zfs.mountpointFor["jail-1"])
	}
}

func TestReconciler_RunOnce_SkipsJailAlreadyRunning(t *testing.T) {
	raft := &fakeRaftClient{
		jailsResp: &internalpb.ListJailsResponse{
			Jails: []*internalpb.JailDefinition{{Id: "jail-1", NodeId: "node-a"}},
		},
	}
	zfs := newFakeDatasetManager()
	zfs.existing["jail-1"] = true
	zfs.mountpointFor["jail-1"] = t.TempDir()
	jm := newFakeJailManager()
	jm.running["jail-1"] = true

	r := &Reconciler{Raft: raft, ZFS: zfs, Jail: jm, LocalNodeID: "node-a"}
	if err := r.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce() error: %v", err)
	}

	if len(jm.created) != 0 {
		t.Errorf("CreateJail called = %v, want none - jail already running", jm.created)
	}
}

func TestReconciler_RunOnce_JailAssignedButNoJailSupportIsError(t *testing.T) {
	raft := &fakeRaftClient{
		jailsResp: &internalpb.ListJailsResponse{
			Jails: []*internalpb.JailDefinition{{Id: "jail-1", NodeId: "node-a"}},
		},
	}

	// Jail is nil below, so RunOnce never even fetches jails - a node
	// with no jail support configured simply doesn't act on them, the
	// same opt-in pattern as Bhyve.
	r := &Reconciler{Raft: raft, ZFS: newFakeDatasetManager(), LocalNodeID: "node-a"}
	if err := r.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce() error: %v, want nil (jail support not configured, so nothing to do)", err)
	}
}

func TestReconciler_RunOnce_DeletingJailTearsDownAndPurges(t *testing.T) {
	raft := &fakeRaftClient{
		jailsResp: &internalpb.ListJailsResponse{
			Jails: []*internalpb.JailDefinition{{Id: "jail-1", NodeId: "node-a", DesiredState: internalpb.JailState_JAIL_STATE_DELETING}},
		},
	}
	zfs := newFakeDatasetManager()
	zfs.existing["jail-1"] = true
	jm := newFakeJailManager()
	jm.running["jail-1"] = true

	r := &Reconciler{Raft: raft, ZFS: zfs, Jail: jm, LocalNodeID: "node-a"}
	if err := r.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce() error: %v", err)
	}

	if len(jm.destroyed) != 1 || jm.destroyed[0] != "jail-1" {
		t.Errorf("destroyed jails = %v, want [jail-1]", jm.destroyed)
	}
	if len(zfs.destroyed) != 1 || zfs.destroyed[0] != "jail-1" {
		t.Errorf("destroyed datasets = %v, want [jail-1]", zfs.destroyed)
	}
	if got := raft.purgedJailIDs(); len(got) != 1 || got[0] != "jail-1" {
		t.Errorf("purged ids = %v, want [jail-1]", got)
	}
}

// TestReconciler_RunOnce_DeletingJailPropagatesRejectedPurge mirrors
// TestReconciler_RunOnce_DeletingVMPropagatesRejectedPurge exactly -
// see its own doc comment (ADR-0028) for the real bug this guards
// against: a rejected Apply (e.g. "not the leader") must not be
// silently treated as a successful purge.
func TestReconciler_RunOnce_DeletingJailPropagatesRejectedPurge(t *testing.T) {
	raft := &fakeRaftClient{
		jailsResp: &internalpb.ListJailsResponse{
			Jails: []*internalpb.JailDefinition{{Id: "jail-1", NodeId: "node-a", DesiredState: internalpb.JailState_JAIL_STATE_DELETING}},
		},
		applyRespErr: "raft: this node is not the leader",
	}
	zfs2 := newFakeDatasetManager()
	zfs2.existing["jail-1"] = true
	jm2 := newFakeJailManager()
	jm2.running["jail-1"] = true

	r2 := &Reconciler{Raft: raft, ZFS: zfs2, Jail: jm2, LocalNodeID: "node-a"}
	if err := r2.RunOnce(context.Background()); err == nil {
		t.Fatalf("RunOnce() = nil error, want the rejected purge surfaced as an error")
	}
}

func TestReconciler_RunOnce_ReclaimsJailResourcesForJailReassignedElsewhere(t *testing.T) {
	raft := &fakeRaftClient{
		jailsResp: &internalpb.ListJailsResponse{
			Jails: []*internalpb.JailDefinition{{Id: "jail-1", NodeId: "node-b"}},
		},
	}
	zfs := newFakeDatasetManager()
	zfs.existing["jail-1"] = true
	jm := newFakeJailManager()
	jm.running["jail-1"] = true

	r := &Reconciler{Raft: raft, ZFS: zfs, Jail: jm, LocalNodeID: "node-a"}
	if err := r.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce() error: %v", err)
	}

	if len(jm.destroyed) != 1 || jm.destroyed[0] != "jail-1" {
		t.Errorf("destroyed jails = %v, want [jail-1] (stale reclaim)", jm.destroyed)
	}
	if len(zfs.destroyed) != 1 || zfs.destroyed[0] != "jail-1" {
		t.Errorf("destroyed datasets = %v, want [jail-1] (stale reclaim)", zfs.destroyed)
	}
	// Reclaim never touches the raft record - it belongs to node-b now.
	if got := raft.purgedJailIDs(); len(got) != 0 {
		t.Errorf("purged ids = %v, want none - reclaim must not purge the record", got)
	}
}

func TestReconciler_RunOnce_ProvisionsHASTPrimaryForReplicatedJail(t *testing.T) {
	raft := &fakeRaftClient{
		jailsResp: &internalpb.ListJailsResponse{
			Jails: []*internalpb.JailDefinition{{Id: "jail-1", NodeId: "node-a", ReplicaNodeId: "node-b"}},
		},
		statusResp: statusResponseWithPeers("node-a", "10.0.0.1:17600", "node-b", "10.0.0.2:17600"),
	}
	zfs := newFakeDatasetManager()
	zfs.mountpointFor["hast-jail-jail-1"] = t.TempDir()
	jm := newFakeJailManager()
	mnt := newFakeMountManager()
	h := newFakeHASTManager()

	r := &Reconciler{
		Raft: raft, ZFS: zfs, Jail: jm, Mount: mnt, HAST: h,
		HASTRestartSettleDelay: time.Millisecond, LocalNodeID: "node-a", JailBase: "/apiary-jails",
	}
	if err := r.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce() error: %v", err)
	}

	if role := h.roleSet["jail-jail-1"]; role != hast.RolePrimary {
		t.Errorf("role for jail-jail-1 = %q, want primary", role)
	}
	if !mnt.formatted["/dev/hast/jail-jail-1"] {
		t.Errorf("HAST device was not formatted")
	}
	if mnt.mounted["/apiary-jails/jail-1"] != "/dev/hast/jail-jail-1" {
		t.Errorf("mounted = %+v, want /apiary-jails/jail-1 -> /dev/hast/jail-jail-1", mnt.mounted)
	}
	cfg, ok := jm.lastCfg["jail-1"]
	if !ok {
		t.Fatalf("CreateJail was never called for jail-1")
	}
	if cfg.Path != "/apiary-jails/jail-1" {
		t.Errorf("Path = %q, want /apiary-jails/jail-1", cfg.Path)
	}
	if zfs.existing["jail-1"] {
		t.Errorf("plain dataset jail-1 was created for a replicated jail, want none")
	}
}

func TestReconciler_RunOnce_ProvisionsHASTSecondaryForReplicaJailAssignment(t *testing.T) {
	raft := &fakeRaftClient{
		jailsResp: &internalpb.ListJailsResponse{
			Jails: []*internalpb.JailDefinition{{Id: "jail-1", NodeId: "node-b", ReplicaNodeId: "node-a"}},
		},
		statusResp: statusResponseWithPeers("node-a", "10.0.0.1:17600", "node-b", "10.0.0.2:17600"),
	}
	zfs := newFakeDatasetManager()
	zfs.mountpointFor["hast-jail-jail-1"] = t.TempDir()
	jm := newFakeJailManager()
	mnt := newFakeMountManager()
	h := newFakeHASTManager()

	r := &Reconciler{
		Raft: raft, ZFS: zfs, Jail: jm, Mount: mnt, HAST: h,
		HASTRestartSettleDelay: time.Millisecond, LocalNodeID: "node-a",
	}
	if err := r.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce() error: %v", err)
	}

	if role := h.roleSet["jail-jail-1"]; role != hast.RoleSecondary {
		t.Errorf("role for jail-jail-1 = %q, want secondary", role)
	}
	if len(jm.created) != 0 {
		t.Errorf("jails created = %v, want none - a replica never runs the jail", jm.created)
	}
	if len(mnt.mounted) != 0 {
		t.Errorf("mounted = %+v, want none - a secondary never mounts its replica", mnt.mounted)
	}
}

func TestReconciler_RunOnce_ReplicatedJailWithoutHASTConfiguredIsError(t *testing.T) {
	raft := &fakeRaftClient{
		jailsResp: &internalpb.ListJailsResponse{
			Jails: []*internalpb.JailDefinition{{Id: "jail-1", NodeId: "node-a", ReplicaNodeId: "node-b"}},
		},
	}
	jm := newFakeJailManager()

	r := &Reconciler{Raft: raft, ZFS: newFakeDatasetManager(), Jail: jm, LocalNodeID: "node-a"}
	if err := r.RunOnce(context.Background()); err == nil {
		t.Fatalf("RunOnce() error = nil, want an error (no HAST support configured)")
	}
}

func TestReconciler_RunOnce_ReplicatedJailWithoutMountConfiguredIsError(t *testing.T) {
	raft := &fakeRaftClient{
		jailsResp: &internalpb.ListJailsResponse{
			Jails: []*internalpb.JailDefinition{{Id: "jail-1", NodeId: "node-a", ReplicaNodeId: "node-b"}},
		},
		statusResp: statusResponseWithPeers("node-a", "10.0.0.1:17600", "node-b", "10.0.0.2:17600"),
	}
	zfs := newFakeDatasetManager()
	zfs.mountpointFor["hast-jail-jail-1"] = t.TempDir()
	jm := newFakeJailManager()
	h := newFakeHASTManager()

	r := &Reconciler{
		Raft: raft, ZFS: zfs, Jail: jm, HAST: h,
		HASTRestartSettleDelay: time.Millisecond, LocalNodeID: "node-a",
	}
	if err := r.RunOnce(context.Background()); err == nil {
		t.Fatalf("RunOnce() error = nil, want an error (no Mount support configured)")
	}
}

func TestReconciler_RunOnce_DeletingReplicatedJailUnmountsAndReclaimsHASTNotDataset(t *testing.T) {
	raft := &fakeRaftClient{
		jailsResp: &internalpb.ListJailsResponse{
			Jails: []*internalpb.JailDefinition{{
				Id: "jail-1", NodeId: "node-a", ReplicaNodeId: "node-b",
				DesiredState: internalpb.JailState_JAIL_STATE_DELETING,
			}},
		},
	}
	zfs := newFakeDatasetManager()
	zfs.existing["hast-jail-jail-1"] = true
	jm := newFakeJailManager()
	jm.running["jail-1"] = true
	mnt := newFakeMountManager()
	mnt.mounted["/apiary-jails/jail-1"] = "/dev/hast/jail-jail-1"

	r := &Reconciler{Raft: raft, ZFS: zfs, Jail: jm, Mount: mnt, LocalNodeID: "node-a", JailBase: "/apiary-jails"}
	if err := r.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce() error: %v", err)
	}

	if _, stillMounted := mnt.mounted["/apiary-jails/jail-1"]; stillMounted {
		t.Errorf("jail root still mounted after teardown")
	}
	if zfs.existing["hast-jail-jail-1"] {
		t.Errorf("HAST provider dataset still exists after teardown")
	}
	if got := raft.purgedJailIDs(); len(got) != 1 || got[0] != "jail-1" {
		t.Errorf("purged ids = %v, want [jail-1]", got)
	}
}
