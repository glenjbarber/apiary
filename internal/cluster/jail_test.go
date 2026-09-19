package cluster

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
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

// writePlaceholderJailRoot drops a file into dir so ensureJailRoot's
// empty-root safety check (ADR-0098) sees a populated root, standing
// in for a real base.txz extraction - these tests aren't exercising
// root population, just everything else ensureJail does.
func writePlaceholderJailRoot(t *testing.T, dir string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "placeholder"), []byte("stand-in for a real FreeBSD userland"), 0o644); err != nil {
		t.Fatalf("writePlaceholderJailRoot: %v", err)
	}
}

func TestReconciler_RunOnce_CreatesJailOnPlainDataset(t *testing.T) {
	raft := &fakeRaftClient{
		jailsResp: &internalpb.ListJailsResponse{
			Jails: []*internalpb.JailDefinition{{Id: "jail-1", Name: "web-1", Hostname: "web-1.local", NodeId: "node-a"}},
		},
	}
	zfs := newFakeDatasetManager()
	root := t.TempDir()
	writePlaceholderJailRoot(t, root)
	zfs.mountpointFor["jail-1"] = root
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
	root := t.TempDir()
	writePlaceholderJailRoot(t, root)
	zfs.mountpointFor["jail-1"] = root
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

// TestReconciler_RunOnce_SkipsJailAlreadyRunningWithNoManagedDataset is
// ADR-0099's own regression test: a jail already running per jail(8),
// but with no ZFS dataset under Apiary's own zroot/apiary/<id> naming
// convention at all (a real, live-production case - a jail adopted
// into this Colony's raft state that predates Apiary ever provisioning
// it). Confirms the running check now runs before any dataset/root
// work, so this jail is left completely alone: no CreateDataset, no
// GetProperty, no CreateJail. Before this fix, DatasetExists would
// report false, ensureJail would try to create a blank dataset and
// then fail ADR-0098's own empty-root safety check against it - a
// loud, recurring reconciliation error for a jail that was never
// actually broken.
func TestReconciler_RunOnce_SkipsJailAlreadyRunningWithNoManagedDataset(t *testing.T) {
	raft := &fakeRaftClient{
		jailsResp: &internalpb.ListJailsResponse{
			Jails: []*internalpb.JailDefinition{{Id: "freebsd-sync1", NodeId: "node-a"}},
		},
	}
	zfs := newFakeDatasetManager()
	jm := newFakeJailManager()
	jm.running["freebsd-sync1"] = true

	r := &Reconciler{Raft: raft, ZFS: zfs, Jail: jm, LocalNodeID: "node-a"}
	if err := r.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce() error: %v", err)
	}

	if len(zfs.created) != 0 {
		t.Errorf("CreateDataset called = %v, want none - an already-running jail needs no dataset work at all", zfs.created)
	}
	if len(jm.created) != 0 {
		t.Errorf("CreateJail called = %v, want none - jail already running", jm.created)
	}
}

// fakeJailArchiveExtractor stands in for internal/jailarchive.Extractor,
// mirroring fakeISOResolver's own hand-off pattern.
type fakeJailArchiveExtractor struct {
	extracted  []string // "archivePath -> destDir" pairs, in call order
	extractErr error
	// noop, if true, records the call but writes nothing into destDir -
	// simulates a "successful" extraction of an archive that turns out
	// to contain nothing, to prove the safety check still fires even
	// after a real extraction step ran.
	noop bool
}

func (f *fakeJailArchiveExtractor) Extract(_ context.Context, archivePath, destDir string) error {
	f.extracted = append(f.extracted, archivePath+" -> "+destDir)
	if f.extractErr != nil {
		return f.extractErr
	}
	if f.noop {
		return nil
	}
	return os.WriteFile(filepath.Join(destDir, "bin-sh"), []byte("#!/bin/sh\n"), 0o755)
}

// TestReconciler_RunOnce_ClonesJailFromBaseTemplate. The mountpoint gets
// a placeholder file (writePlaceholderJailRoot) even though a real `zfs
// clone` would populate it for real - the fake ZFS manager's own Clone
// only records the call, so without this the new unconditional empty-
// root check (ADR-0098) would fail this test for a reason unrelated to
// what it's actually testing.
func TestReconciler_RunOnce_ClonesJailFromBaseTemplate(t *testing.T) {
	raft := &fakeRaftClient{
		jailsResp: &internalpb.ListJailsResponse{
			Jails: []*internalpb.JailDefinition{{Id: "jail-1", NodeId: "node-a", BaseTemplate: "freebsd-14"}},
		},
	}
	zfs := newFakeDatasetManager()
	zfs.snapshots["templates/freebsd-14@apiary-template"] = true
	root := t.TempDir()
	writePlaceholderJailRoot(t, root)
	zfs.mountpointFor["jail-1"] = root
	jm := newFakeJailManager()

	r := &Reconciler{Raft: raft, ZFS: zfs, Jail: jm, LocalNodeID: "node-a"}
	if err := r.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce() error: %v", err)
	}

	if len(zfs.created) != 0 {
		t.Errorf("CreateDataset called = %v, want none - a templated jail is cloned, not created blank", zfs.created)
	}
	want := "templates/freebsd-14@apiary-template->jail-1"
	if len(zfs.cloned) != 1 || zfs.cloned[0] != want {
		t.Errorf("Clone calls = %v, want [%q]", zfs.cloned, want)
	}
	if !zfs.existing["jail-1"] {
		t.Errorf("dataset jail-1 was not created via clone")
	}
	if _, ok := jm.lastCfg["jail-1"]; !ok {
		t.Fatalf("CreateJail was never called for jail-1")
	}
}

// TestReconciler_RunOnce_BaseTemplateAndBaseArchiveTogetherIsError
// confirms ensureJail's own explicit rejection: these are two
// independent ways to populate a jail's root (ADR-0084 and ADR-0098),
// and naming both is refused outright rather than letting one silently
// win over the other.
func TestReconciler_RunOnce_BaseTemplateAndBaseArchiveTogetherIsError(t *testing.T) {
	raft := &fakeRaftClient{
		jailsResp: &internalpb.ListJailsResponse{
			Jails: []*internalpb.JailDefinition{{Id: "jail-1", NodeId: "node-a", BaseTemplate: "freebsd-14", BaseArchiveName: "base.txz"}},
		},
	}
	zfs := newFakeDatasetManager()
	zfs.snapshots["templates/freebsd-14@apiary-template"] = true

	r := &Reconciler{Raft: raft, ZFS: zfs, Jail: newFakeJailManager(), LocalNodeID: "node-a"}
	if err := r.RunOnce(context.Background()); err == nil || !strings.Contains(err.Error(), "not supported together") {
		t.Fatalf("RunOnce() error: %v, want explicit unsupported-combination error", err)
	}
	assertJailPhaseError(t, raft, "jail-1", "not supported together")
	if len(zfs.cloned) != 0 {
		t.Errorf("Clone calls = %v, want none", zfs.cloned)
	}
}

// TestReconciler_RunOnce_EmptyJailRootWithNoBaseArchiveIsError confirms
// the ADR-0098 safety check: a jail whose root is a completely empty
// dataset, and which names no base_archive_name, must fail
// reconciliation with a clear error rather than reach PhaseReady with
// jail(8) attached to nothing.
func TestReconciler_RunOnce_EmptyJailRootWithNoBaseArchiveIsError(t *testing.T) {
	raft := &fakeRaftClient{
		jailsResp: &internalpb.ListJailsResponse{
			Jails: []*internalpb.JailDefinition{{Id: "jail-1", NodeId: "node-a"}},
		},
	}
	zfs := newFakeDatasetManager()
	zfs.mountpointFor["jail-1"] = t.TempDir() // left empty - no placeholder written
	jm := newFakeJailManager()

	r := &Reconciler{Raft: raft, ZFS: zfs, Jail: jm, LocalNodeID: "node-a"}
	err := r.RunOnce(context.Background())
	if err == nil || !strings.Contains(err.Error(), "is empty") {
		t.Fatalf("RunOnce() error = %v, want an empty-root error", err)
	}
	if len(jm.created) != 0 {
		t.Errorf("CreateJail called = %v, want none - an empty root must never reach jail(8)", jm.created)
	}
}

// TestReconciler_RunOnce_BaseArchiveExtractsIntoEmptyJailRoot confirms
// a jail naming base_archive_name gets it resolved via ISOs and
// extracted via JailArchives into its empty root before jail(8) is
// asked to attach to it.
func TestReconciler_RunOnce_BaseArchiveExtractsIntoEmptyJailRoot(t *testing.T) {
	raft := &fakeRaftClient{
		jailsResp: &internalpb.ListJailsResponse{
			Jails: []*internalpb.JailDefinition{{Id: "jail-1", NodeId: "node-a", BaseArchiveName: "base.txz"}},
		},
	}
	zfs := newFakeDatasetManager()
	root := t.TempDir()
	zfs.mountpointFor["jail-1"] = root
	jm := newFakeJailManager()
	isos := &fakeISOResolver{paths: map[string]string{"base.txz": "/isos/base.txz"}}
	archives := &fakeJailArchiveExtractor{}

	r := &Reconciler{Raft: raft, ZFS: zfs, Jail: jm, ISOs: isos, JailArchives: archives, LocalNodeID: "node-a"}
	if err := r.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce() error: %v", err)
	}

	if want := []string{"/isos/base.txz -> " + root}; len(archives.extracted) != 1 || archives.extracted[0] != want[0] {
		t.Errorf("extracted = %v, want %v", archives.extracted, want)
	}
	if _, ok := jm.lastCfg["jail-1"]; !ok {
		t.Fatalf("CreateJail was never called for jail-1")
	}
}

// TestReconciler_RunOnce_BaseTemplateMissingSnapshotIsError confirms
// that a missing base template still fails clearly when no peer
// forwarding is configured (ADR-0089's fallback to ADR-0084's
// original, pre-peer-fetch behavior).
func TestReconciler_RunOnce_BaseTemplateMissingSnapshotIsError(t *testing.T) {
	raft := &fakeRaftClient{
		jailsResp: &internalpb.ListJailsResponse{
			Jails: []*internalpb.JailDefinition{{Id: "jail-1", NodeId: "node-a", BaseTemplate: "freebsd-14"}},
		},
	}
	zfs := newFakeDatasetManager()
	jm := newFakeJailManager()

	r := &Reconciler{Raft: raft, ZFS: zfs, Jail: jm, LocalNodeID: "node-a"}
	if err := r.RunOnce(context.Background()); err == nil || !strings.Contains(err.Error(), "no peer forwarding is configured") {
		t.Fatalf("RunOnce() error: %v, want explicit missing-snapshot/no-peers error", err)
	}
	assertJailPhaseError(t, raft, "jail-1", "no peer forwarding is configured")
	if len(zfs.created) != 0 || len(zfs.cloned) != 0 {
		t.Fatal("no dataset should be created or cloned when the base template snapshot is missing")
	}
}

// TestReconciler_RunOnce_FetchesMissingJailTemplateFromPeerBeforeCloning
// confirms the ADR-0089 fix: a jail base template missing locally is
// fetched from the first peer reporting it, then cloned normally.
func TestReconciler_RunOnce_FetchesMissingJailTemplateFromPeerBeforeCloning(t *testing.T) {
	raft := &fakeRaftClient{
		jailsResp: &internalpb.ListJailsResponse{
			Jails: []*internalpb.JailDefinition{{Id: "jail-1", NodeId: "node-a", BaseTemplate: "freebsd-14"}},
		},
		statusResp: statusResponseWithPeers("node-a", "10.0.0.1:17600", "node-b", "10.0.0.2:17600"),
	}
	zfs := newFakeDatasetManager()
	root := t.TempDir()
	writePlaceholderJailRoot(t, root)
	zfs.mountpointFor["jail-1"] = root
	jm := newFakeJailManager()
	peers := &fakePeerReporter{
		jailTemplateNamesByAddr: map[string][]string{"10.0.0.2:17700": {"freebsd-14"}},
		onRequestTemplatePush: func(name string) {
			zfs.snapshots["templates/"+name+"@apiary-template"] = true
		},
	}

	r := &Reconciler{Raft: raft, ZFS: zfs, Jail: jm, Peers: peers, LocalNodeID: "node-a"}
	if err := r.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce() error: %v", err)
	}

	if len(peers.requestTemplatePushCalls) != 1 || peers.requestTemplatePushCalls[0] != "10.0.0.2:17700 freebsd-14 node-a" {
		t.Errorf("RequestJailTemplatePush calls = %v, want one call to 10.0.0.2:17700 for freebsd-14 targeting node-a", peers.requestTemplatePushCalls)
	}
	want := "templates/freebsd-14@apiary-template->jail-1"
	if len(zfs.cloned) != 1 || zfs.cloned[0] != want {
		t.Errorf("Clone calls = %v, want [%q]", zfs.cloned, want)
	}
}

// TestReconciler_RunOnce_JailTemplateNotFoundOnAnyPeerFailsWithoutCloning
// mirrors TestReconciler_RunOnce_ISONotFoundOnAnyPeerFailsWithoutCreatingVM,
// for jail templates.
func TestReconciler_RunOnce_JailTemplateNotFoundOnAnyPeerFailsWithoutCloning(t *testing.T) {
	raft := &fakeRaftClient{
		jailsResp: &internalpb.ListJailsResponse{
			Jails: []*internalpb.JailDefinition{{Id: "jail-1", NodeId: "node-a", BaseTemplate: "freebsd-14"}},
		},
		statusResp: statusResponseWithPeers("node-a", "10.0.0.1:17600", "node-b", "10.0.0.2:17600"),
	}
	zfs := newFakeDatasetManager()
	zfs.mountpointFor["jail-1"] = t.TempDir()
	jm := newFakeJailManager()
	peers := &fakePeerReporter{jailTemplateNamesByAddr: map[string][]string{"10.0.0.2:17700": {"other-template"}}}

	r := &Reconciler{Raft: raft, ZFS: zfs, Jail: jm, Peers: peers, LocalNodeID: "node-a"}
	if err := r.RunOnce(context.Background()); err == nil {
		t.Fatal("RunOnce() = nil error, want a clear failure when no peer has the template either")
	}
	if len(zfs.cloned) != 0 {
		t.Errorf("Clone calls = %v, want none", zfs.cloned)
	}
	if len(peers.requestTemplatePushCalls) != 0 {
		t.Errorf("RequestJailTemplatePush calls = %v, want none (no peer reported having the template)", peers.requestTemplatePushCalls)
	}
}

func TestReconciler_RunOnce_BaseTemplateWithReplicaNodeIsError(t *testing.T) {
	raft := &fakeRaftClient{
		jailsResp: &internalpb.ListJailsResponse{
			Jails: []*internalpb.JailDefinition{{Id: "jail-1", NodeId: "node-a", ReplicaNodeId: "node-b", BaseTemplate: "freebsd-14"}},
		},
		statusResp: statusResponseWithPeers("node-a", "10.0.0.1:17600", "node-b", "10.0.0.2:17600"),
	}
	zfs := newFakeDatasetManager()
	zfs.snapshots["templates/freebsd-14@apiary-template"] = true
	zfs.mountpointFor["hast-jail-jail-1"] = t.TempDir()

	r := &Reconciler{Raft: raft, ZFS: zfs, Jail: newFakeJailManager(), HAST: newFakeHASTManager(), Mount: newFakeMountManager(), LocalNodeID: "node-a"}
	if err := r.RunOnce(context.Background()); err == nil || !strings.Contains(err.Error(), "not supported together with replica_node_id") {
		t.Fatalf("RunOnce() error: %v, want explicit unsupported-combination error", err)
	}
	assertJailPhaseError(t, raft, "jail-1", "not supported together with replica_node_id")
}

func TestReconciler_RunOnce_BaseTemplateNeverReClonesExistingDataset(t *testing.T) {
	raft := &fakeRaftClient{
		jailsResp: &internalpb.ListJailsResponse{
			Jails: []*internalpb.JailDefinition{{Id: "jail-1", NodeId: "node-a", BaseTemplate: "freebsd-14"}},
		},
	}
	zfs := newFakeDatasetManager()
	zfs.existing["jail-1"] = true
	root := t.TempDir()
	writePlaceholderJailRoot(t, root)
	zfs.mountpointFor["jail-1"] = root
	jm := newFakeJailManager()

	r := &Reconciler{Raft: raft, ZFS: zfs, Jail: jm, LocalNodeID: "node-a"}
	if err := r.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce() error: %v", err)
	}

	if len(zfs.cloned) != 0 {
		t.Errorf("Clone calls = %v, want none - an already-existing dataset must never be re-cloned", zfs.cloned)
	}
}

// TestReconciler_RunOnce_BaseArchiveNotReExtractedOncePopulated
// confirms a jail root that's already populated is never re-extracted
// into on a later tick, mirroring ensureDiskImage's own "only act on
// create" behavior for VM base images.
func TestReconciler_RunOnce_BaseArchiveNotReExtractedOncePopulated(t *testing.T) {
	raft := &fakeRaftClient{
		jailsResp: &internalpb.ListJailsResponse{
			Jails: []*internalpb.JailDefinition{{Id: "jail-1", NodeId: "node-a", BaseArchiveName: "base.txz"}},
		},
	}
	zfs := newFakeDatasetManager()
	zfs.existing["jail-1"] = true
	root := t.TempDir()
	writePlaceholderJailRoot(t, root)
	zfs.mountpointFor["jail-1"] = root
	jm := newFakeJailManager()
	isos := &fakeISOResolver{paths: map[string]string{"base.txz": "/isos/base.txz"}}
	archives := &fakeJailArchiveExtractor{}

	r := &Reconciler{Raft: raft, ZFS: zfs, Jail: jm, ISOs: isos, JailArchives: archives, LocalNodeID: "node-a"}
	if err := r.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce() error: %v", err)
	}

	if len(archives.extracted) != 0 {
		t.Errorf("extracted = %v, want none - an already-populated root must never be re-extracted into", archives.extracted)
	}
}

// TestReconciler_RunOnce_BaseArchiveThatExtractsNothingIsStillAnError
// confirms the safety check runs again after extraction: an archive
// that "successfully" extracts but leaves the root empty must still
// fail, not silently reach PhaseReady.
func TestReconciler_RunOnce_BaseArchiveThatExtractsNothingIsStillAnError(t *testing.T) {
	raft := &fakeRaftClient{
		jailsResp: &internalpb.ListJailsResponse{
			Jails: []*internalpb.JailDefinition{{Id: "jail-1", NodeId: "node-a", BaseArchiveName: "empty.txz"}},
		},
	}
	zfs := newFakeDatasetManager()
	zfs.mountpointFor["jail-1"] = t.TempDir()
	jm := newFakeJailManager()
	isos := &fakeISOResolver{paths: map[string]string{"empty.txz": "/isos/empty.txz"}}
	archives := &fakeJailArchiveExtractor{noop: true}

	r := &Reconciler{Raft: raft, ZFS: zfs, Jail: jm, ISOs: isos, JailArchives: archives, LocalNodeID: "node-a"}
	err := r.RunOnce(context.Background())
	if err == nil || !strings.Contains(err.Error(), "is empty") {
		t.Fatalf("RunOnce() error = %v, want an empty-root error", err)
	}
	if len(jm.created) != 0 {
		t.Errorf("CreateJail called = %v, want none", jm.created)
	}
}

// TestReconciler_RunOnce_BaseArchiveWithNoISOStoreIsError confirms a
// jail naming base_archive_name on a node with no ISO store configured
// fails clearly, mirroring the equivalent VM base-image check.
func TestReconciler_RunOnce_BaseArchiveWithNoISOStoreIsError(t *testing.T) {
	raft := &fakeRaftClient{
		jailsResp: &internalpb.ListJailsResponse{
			Jails: []*internalpb.JailDefinition{{Id: "jail-1", NodeId: "node-a", BaseArchiveName: "base.txz"}},
		},
	}
	zfs := newFakeDatasetManager()
	zfs.mountpointFor["jail-1"] = t.TempDir()
	jm := newFakeJailManager()

	r := &Reconciler{Raft: raft, ZFS: zfs, Jail: jm, LocalNodeID: "node-a"}
	err := r.RunOnce(context.Background())
	if err == nil || !strings.Contains(err.Error(), "no ISO store is configured") {
		t.Fatalf("RunOnce() error = %v, want a no-ISO-store error", err)
	}
}

func TestReconciler_RunOnce_JailAssignedButNoJailSupportIsError(t *testing.T) {
	raft := &fakeRaftClient{
		jailsResp: &internalpb.ListJailsResponse{
			Jails: []*internalpb.JailDefinition{{Id: "jail-1", NodeId: "node-a"}},
		},
	}

	r := &Reconciler{Raft: raft, ZFS: newFakeDatasetManager(), LocalNodeID: "node-a"}
	if err := r.RunOnce(context.Background()); err == nil || !strings.Contains(err.Error(), "provisioning is disabled") {
		t.Fatalf("RunOnce() error: %v, want explicit disabled error", err)
	}
	assertJailPhaseError(t, raft, "jail-1", "provisioning is disabled")
	if len(raft.purgedJailIDs()) != 0 {
		t.Fatal("a non-deleting jail must not be purged")
	}
}

func assertJailPhaseError(t *testing.T, raft *fakeRaftClient, id, detail string) {
	t.Helper()
	for _, cmd := range raft.applied {
		if p := cmd.GetUpdateJailPhase(); p != nil && p.Id == id &&
			p.Phase == internalpb.JailPhase_JAIL_PHASE_ERROR && p.PhaseError != "" && strings.Contains(p.PhaseError, detail) {
			return
		}
	}
	t.Fatalf("missing jail phase error for %s containing %q", id, detail)
}

func TestReconciler_DisabledJailProvisioningDoesNotCreate(t *testing.T) {
	for _, replica := range []string{"", "node-b"} {
		t.Run("replica="+replica, func(t *testing.T) {
			raft := &fakeRaftClient{jailsResp: &internalpb.ListJailsResponse{Jails: []*internalpb.JailDefinition{
				{Id: "jail-1", NodeId: "node-a", ReplicaNodeId: replica, Phase: internalpb.JailPhase_JAIL_PHASE_READY},
				{Id: "remote", NodeId: "node-b", ReplicaNodeId: "node-a"},
			}}}
			zfs, jm := newFakeDatasetManager(), newFakeJailManager()
			r := &Reconciler{Raft: raft, ZFS: zfs, Jail: jm, HAST: newFakeHASTManager(),
				LocalNodeID: "node-a", JailProvisioningDisabled: true}
			if err := r.RunOnce(context.Background()); err == nil {
				t.Fatal("expected disabled error")
			}
			assertJailPhaseError(t, raft, "jail-1", "provisioning is disabled")
			if len(zfs.created)+len(jm.created)+len(zfs.destroyed)+len(jm.destroyed) != 0 {
				t.Fatal("disabled provisioning modified jail resources")
			}
		})
	}
}

func TestReconciler_DisabledJailDeletion(t *testing.T) {
	for _, tc := range []struct {
		name                                 string
		existing, noDriver, replica, foreign bool
		jailErr, datasetErr                  error
		wantPurge                            bool
	}{
		{name: "never provisioned", wantPurge: true},
		{name: "previously provisioned", existing: true, wantPurge: true},
		{name: "no inspection driver", noDriver: true},
		{name: "jail inspection failure", jailErr: errors.New("jls unavailable")},
		{name: "dataset inspection failure", datasetErr: errors.New("zfs unavailable")},
		{name: "replication support missing", replica: true},
		{name: "other owner", foreign: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			j := &internalpb.JailDefinition{Id: "jail-1", NodeId: "node-a", DesiredState: internalpb.JailState_JAIL_STATE_DELETING}
			if tc.foreign {
				j.NodeId = "node-b"
			}
			if tc.replica {
				j.ReplicaNodeId = "node-b"
			}
			raft := &fakeRaftClient{jailsResp: &internalpb.ListJailsResponse{Jails: []*internalpb.JailDefinition{j}}}
			zfs, jm := newFakeDatasetManager(), newFakeJailManager()
			zfs.existing[j.Id], jm.running[j.Id] = tc.existing, tc.existing
			jm.existsErr, zfs.existsErr = tc.jailErr, tc.datasetErr
			r := &Reconciler{Raft: raft, ZFS: zfs, LocalNodeID: "node-a", JailProvisioningDisabled: true}
			if !tc.noDriver {
				r.Jail = jm
			}
			err := r.RunOnce(context.Background())
			if tc.wantPurge || tc.foreign {
				if err != nil {
					t.Fatalf("RunOnce: %v", err)
				}
			} else {
				if err == nil {
					t.Fatal("expected a fail-closed error")
				}
				assertJailPhaseError(t, raft, j.Id, "")
			}
			if got := len(raft.purgedJailIDs()) != 0; got != tc.wantPurge {
				t.Fatalf("purged=%v, want %v", got, tc.wantPurge)
			}
			if !tc.wantPurge && len(jm.destroyed)+len(zfs.destroyed) != 0 {
				t.Fatal("unverified resources were destroyed")
			}
			if tc.wantPurge && tc.existing && (len(jm.destroyed) != 1 || len(zfs.destroyed) != 1) {
				t.Fatal("explicit deletion did not remove existing resources")
			}
		})
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

	// The fake Mount never actually creates jailBase+"/jail-1" on disk
	// (unlike a real ufsmount.Mount, which mounts onto a real
	// directory) - pre-create it with a placeholder so the empty-root
	// safety check (ADR-0098) sees an already-populated root, since
	// this test isn't exercising root population.
	jailBase := t.TempDir()
	rootPath := filepath.Join(jailBase, "jail-1")
	if err := os.MkdirAll(rootPath, 0o755); err != nil {
		t.Fatalf("creating fake jail root: %v", err)
	}
	writePlaceholderJailRoot(t, rootPath)

	r := &Reconciler{
		Raft: raft, ZFS: zfs, Jail: jm, Mount: mnt, HAST: h,
		HASTRestartSettleDelay: time.Millisecond, LocalNodeID: "node-a", JailBase: jailBase,
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
	if mnt.mounted[rootPath] != "/dev/hast/jail-jail-1" {
		t.Errorf("mounted = %+v, want %s -> /dev/hast/jail-jail-1", mnt.mounted, rootPath)
	}
	cfg, ok := jm.lastCfg["jail-1"]
	if !ok {
		t.Fatalf("CreateJail was never called for jail-1")
	}
	if cfg.Path != rootPath {
		t.Errorf("Path = %q, want %q", cfg.Path, rootPath)
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
