package main

import (
	"crypto/sha256"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	internalpb "github.com/glenjbarber/apiary/api/internalpb"
	raftnode "github.com/glenjbarber/apiary/internal/raft"
)

// freeLoopbackAddr returns a loopback TCP address with a free port, for
// tests that need a concrete, distinct raft bind address - mirrors
// internal/raft's own unexported helper of the same name, which this
// package can't import (it's test-only in package raft).
func freeLoopbackAddr(t *testing.T) string {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("finding free port: %v", err)
	}
	addr := lis.Addr().String()
	lis.Close()
	return addr
}

// eventually polls cond until it returns true or timeout elapses, failing
// the test otherwise - mirrors internal/raft's own helper.
func eventually(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("condition not met within %s", timeout)
}

// writeTestArchive builds a valid ConfigArchive (correct checksum,
// current format_version) around a small FSMSnapshotState containing
// one VM, and writes it to a file under t.TempDir(). Returns the path.
func writeTestArchive(t *testing.T) string {
	t.Helper()

	state := &internalpb.FSMSnapshotState{
		Vms: map[string]*internalpb.VMDefinition{
			"vm-1": {Id: "vm-1", Name: "restored-vm"},
		},
	}
	stateBytes, err := proto.Marshal(state)
	if err != nil {
		t.Fatalf("marshaling test state: %v", err)
	}
	checksum := sha256.Sum256(stateBytes)
	archive := &internalpb.ConfigArchive{
		FormatVersion:    configArchiveFormatVersion,
		FsmSnapshotState: stateBytes,
		Checksum:         checksum[:],
	}
	archiveBytes, err := proto.Marshal(archive)
	if err != nil {
		t.Fatalf("marshaling test archive: %v", err)
	}

	path := filepath.Join(t.TempDir(), "archive.bin")
	if err := os.WriteFile(path, archiveBytes, 0o600); err != nil {
		t.Fatalf("writing test archive: %v", err)
	}
	return path
}

func TestResetDataDir_WrongPhraseDoesNothing(t *testing.T) {
	dir := t.TempDir()
	dataDir := filepath.Join(dir, "raftd")
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		t.Fatalf("setup: %v", err)
	}
	marker := filepath.Join(dataDir, "raft.db")
	if err := os.WriteFile(marker, []byte("state"), 0o600); err != nil {
		t.Fatalf("setup: %v", err)
	}

	if err := resetDataDir("not-the-phrase", dataDir); err == nil {
		t.Fatal("resetDataDir() with wrong phrase = nil error, want a rejection")
	}

	if _, err := os.Stat(marker); err != nil {
		t.Errorf("data dir was touched despite the wrong phrase: %v", err)
	}
}

func TestResetDataDir_CorrectPhraseMovesAsideAndRecreatesEmpty(t *testing.T) {
	dir := t.TempDir()
	dataDir := filepath.Join(dir, "raftd")
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		t.Fatalf("setup: %v", err)
	}
	marker := filepath.Join(dataDir, "raft.db")
	if err := os.WriteFile(marker, []byte("state"), 0o600); err != nil {
		t.Fatalf("setup: %v", err)
	}

	if err := resetDataDir(resetConfirmPhrase, dataDir); err != nil {
		t.Fatalf("resetDataDir() error: %v", err)
	}

	entries, err := os.ReadDir(dataDir)
	if err != nil {
		t.Fatalf("reading recreated data dir: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("recreated data dir is not empty: %v", entries)
	}

	// The old state must still exist somewhere (moved aside, not deleted).
	matches, err := filepath.Glob(dataDir + ".reset-backup-*")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	if len(matches) != 1 {
		t.Fatalf("backup dirs found = %v, want exactly 1", matches)
	}
	if _, err := os.Stat(filepath.Join(matches[0], "raft.db")); err != nil {
		t.Errorf("old raft.db not preserved in backup dir: %v", err)
	}
}

func TestResetDataDir_MissingDataDirStillRecreatesEmpty(t *testing.T) {
	dir := t.TempDir()
	dataDir := filepath.Join(dir, "does-not-exist-yet")

	if err := resetDataDir(resetConfirmPhrase, dataDir); err != nil {
		t.Fatalf("resetDataDir() error: %v", err)
	}

	info, err := os.Stat(dataDir)
	if err != nil || !info.IsDir() {
		t.Errorf("data dir was not created: %v", err)
	}
}

func TestRestoreDataDir_WrongPhraseDoesNothing(t *testing.T) {
	cfg := raftnode.Config{NodeID: "node-1", DataDir: filepath.Join(t.TempDir(), "raftd"), BindAddr: "127.0.0.1:0"}
	archive := writeTestArchive(t)

	if err := restoreDataDir("not-the-phrase", archive, cfg); err == nil {
		t.Fatal("restoreDataDir() with wrong phrase = nil error, want a rejection")
	}

	if _, err := os.Stat(cfg.DataDir); !os.IsNotExist(err) {
		t.Errorf("data dir was touched despite the wrong phrase: err=%v", err)
	}
}

func TestRestoreDataDir_RejectsNonEmptyDataDir(t *testing.T) {
	dataDir := filepath.Join(t.TempDir(), "raftd")
	cfg := raftnode.Config{NodeID: "node-1", DataDir: dataDir, BindAddr: "127.0.0.1:0"}
	node, err := raftnode.New(cfg)
	if err != nil {
		t.Fatalf("raftnode.New() error: %v", err)
	}
	if err := node.Bootstrap(); err != nil {
		t.Fatalf("Bootstrap() error: %v", err)
	}
	if err := node.Shutdown(); err != nil {
		t.Fatalf("Shutdown() error: %v", err)
	}

	if err := restoreDataDir(restoreConfirmPhrase, writeTestArchive(t), cfg); err == nil {
		t.Fatal("restoreDataDir() into an already-bootstrapped data dir succeeded, want an error")
	}
}

func TestRestoreDataDir_ChecksumMismatchRejected(t *testing.T) {
	state := &internalpb.FSMSnapshotState{Vms: map[string]*internalpb.VMDefinition{"vm-1": {Id: "vm-1"}}}
	stateBytes, err := proto.Marshal(state)
	if err != nil {
		t.Fatalf("marshaling test state: %v", err)
	}
	archive := &internalpb.ConfigArchive{
		FormatVersion:    configArchiveFormatVersion,
		FsmSnapshotState: stateBytes,
		Checksum:         []byte("not-the-real-checksum"),
	}
	archiveBytes, err := proto.Marshal(archive)
	if err != nil {
		t.Fatalf("marshaling test archive: %v", err)
	}
	path := filepath.Join(t.TempDir(), "archive.bin")
	if err := os.WriteFile(path, archiveBytes, 0o600); err != nil {
		t.Fatalf("writing test archive: %v", err)
	}

	cfg := raftnode.Config{NodeID: "node-1", DataDir: filepath.Join(t.TempDir(), "raftd"), BindAddr: "127.0.0.1:0"}
	if err := restoreDataDir(restoreConfirmPhrase, path, cfg); err == nil {
		t.Fatal("restoreDataDir() with a bad checksum succeeded, want a rejection")
	}
}

func TestDryRunRestore_ValidArchiveMakesNoChanges(t *testing.T) {
	archive := writeTestArchive(t)

	if err := dryRunRestore(archive); err != nil {
		t.Fatalf("dryRunRestore() error: %v", err)
	}

	// dryRunRestore takes no -data-dir at all, so the only thing to
	// confirm is that the archive file itself is untouched.
	if _, err := os.Stat(archive); err != nil {
		t.Errorf("archive file missing after dry run: %v", err)
	}
}

func TestDryRunRestore_ChecksumMismatchRejected(t *testing.T) {
	state := &internalpb.FSMSnapshotState{Vms: map[string]*internalpb.VMDefinition{"vm-1": {Id: "vm-1"}}}
	stateBytes, err := proto.Marshal(state)
	if err != nil {
		t.Fatalf("marshaling test state: %v", err)
	}
	archive := &internalpb.ConfigArchive{
		FormatVersion:    configArchiveFormatVersion,
		FsmSnapshotState: stateBytes,
		Checksum:         []byte("not-the-real-checksum"),
	}
	archiveBytes, err := proto.Marshal(archive)
	if err != nil {
		t.Fatalf("marshaling test archive: %v", err)
	}
	path := filepath.Join(t.TempDir(), "archive.bin")
	if err := os.WriteFile(path, archiveBytes, 0o600); err != nil {
		t.Fatalf("writing test archive: %v", err)
	}

	if err := dryRunRestore(path); err == nil {
		t.Fatal("dryRunRestore() with a bad checksum succeeded, want a rejection")
	}
}

func TestRestoreDataDir_CorrectPhraseSeedsSnapshot(t *testing.T) {
	cfg := raftnode.Config{NodeID: "node-1", DataDir: filepath.Join(t.TempDir(), "raftd"), BindAddr: "127.0.0.1:0"}

	if err := restoreDataDir(restoreConfirmPhrase, writeTestArchive(t), cfg); err != nil {
		t.Fatalf("restoreDataDir() error: %v", err)
	}

	hadState, err := raftnode.HasExistingState(cfg)
	if err != nil {
		t.Fatalf("HasExistingState() error: %v", err)
	}
	if !hadState {
		t.Error("HasExistingState() = false after restoreDataDir, want true")
	}
}

func TestValidateAwaitJoinFlags_RejectsAwaitJoinWithJoin(t *testing.T) {
	if err := validateAwaitJoinFlags(true, "/var/run/apiary/raftd.sock"); err == nil {
		t.Fatal("validateAwaitJoinFlags(true, non-empty join) = nil error, want a rejection")
	}
}

func TestValidateAwaitJoinFlags_AllowsEitherAlone(t *testing.T) {
	if err := validateAwaitJoinFlags(true, ""); err != nil {
		t.Errorf("validateAwaitJoinFlags(true, \"\") error: %v", err)
	}
	if err := validateAwaitJoinFlags(false, "/var/run/apiary/raftd.sock"); err != nil {
		t.Errorf("validateAwaitJoinFlags(false, non-empty join) error: %v", err)
	}
	if err := validateAwaitJoinFlags(false, ""); err != nil {
		t.Errorf("validateAwaitJoinFlags(false, \"\") error: %v", err)
	}
}

// TestStartupJoinOrBootstrap_AwaitJoinDoesNotBootstrap confirms a fresh
// node started via startupJoinOrBootstrap with awaitJoin=true never forms
// any cluster of its own - no leader, no configuration - mirroring
// internal/raft/multinode_test.go's newUnbootstrappedNode/
// TestMultiNode_AddVoterFormsCluster shape, but exercised through
// cmd/raftd's own startup decision function (the code -await-join
// actually runs) rather than the bare library.
func TestStartupJoinOrBootstrap_AwaitJoinDoesNotBootstrap(t *testing.T) {
	cfg := raftnode.Config{
		NodeID:   "await-node",
		DataDir:  t.TempDir(),
		BindAddr: freeLoopbackAddr(t),
	}
	node, err := raftnode.New(cfg)
	if err != nil {
		t.Fatalf("raftnode.New() error: %v", err)
	}
	t.Cleanup(func() { node.Shutdown() })

	if err := startupJoinOrBootstrap(node, false, "", cfg.NodeID, cfg.BindAddr, "", true); err != nil {
		t.Fatalf("startupJoinOrBootstrap(awaitJoin=true) error: %v", err)
	}

	// Give any accidental self-bootstrap a moment to happen before
	// asserting it didn't.
	time.Sleep(200 * time.Millisecond)
	if node.Status().IsLeader {
		t.Error("node became leader of its own cluster despite -await-join")
	}
	if len(node.Status().Servers) != 0 {
		t.Errorf("node has %d servers in its configuration despite -await-join, want 0", len(node.Status().Servers))
	}
}

// TestStartupJoinOrBootstrap_AwaitJoinThenAddVoterFormsCluster confirms
// the other half: a node started with awaitJoin=true, once an external
// leader calls AddVoter against it, cleanly becomes a real follower -
// the actual mechanism ADR-0083's ApproveJoinRequest handler drives via
// RaftClient.AddVoter.
func TestStartupJoinOrBootstrap_AwaitJoinThenAddVoterFormsCluster(t *testing.T) {
	leaderCfg := raftnode.Config{
		NodeID:   "leader",
		DataDir:  t.TempDir(),
		BindAddr: freeLoopbackAddr(t),
	}
	leader, err := raftnode.New(leaderCfg)
	if err != nil {
		t.Fatalf("raftnode.New(leader) error: %v", err)
	}
	t.Cleanup(func() { leader.Shutdown() })
	if err := leader.Bootstrap(); err != nil {
		t.Fatalf("leader.Bootstrap() error: %v", err)
	}
	eventually(t, 5*time.Second, func() bool { return leader.Status().IsLeader })

	joinerCfg := raftnode.Config{
		NodeID:   "joiner",
		DataDir:  t.TempDir(),
		BindAddr: freeLoopbackAddr(t),
	}
	joiner, err := raftnode.New(joinerCfg)
	if err != nil {
		t.Fatalf("raftnode.New(joiner) error: %v", err)
	}
	t.Cleanup(func() { joiner.Shutdown() })

	// This is exactly what raftd's run() does when -await-join is set on
	// a fresh data dir: start the node's transport, then do nothing else
	// - no Bootstrap, no -join dial.
	if err := startupJoinOrBootstrap(joiner, false, "", joinerCfg.NodeID, joinerCfg.BindAddr, "", true); err != nil {
		t.Fatalf("startupJoinOrBootstrap(awaitJoin=true) error: %v", err)
	}

	if err := leader.AddVoter(joinerCfg.NodeID, joinerCfg.BindAddr, 0, 5*time.Second); err != nil {
		t.Fatalf("leader.AddVoter(joiner) error: %v", err)
	}

	eventually(t, 5*time.Second, func() bool {
		return len(leader.Status().Servers) == 2
	})
	eventually(t, 5*time.Second, func() bool {
		return joiner.Status().LeaderID == leaderCfg.NodeID
	})
}

func TestStartupJoinOrBootstrap_HadStateTakesPriorityOverAwaitJoin(t *testing.T) {
	cfg := raftnode.Config{
		NodeID:   "resumed-node",
		DataDir:  t.TempDir(),
		BindAddr: freeLoopbackAddr(t),
	}
	node, err := raftnode.New(cfg)
	if err != nil {
		t.Fatalf("raftnode.New() error: %v", err)
	}
	if err := node.Bootstrap(); err != nil {
		t.Fatalf("Bootstrap() error: %v", err)
	}
	eventually(t, 5*time.Second, func() bool { return node.Status().IsLeader })
	if err := node.Shutdown(); err != nil {
		t.Fatalf("Shutdown() error: %v", err)
	}

	hadState, err := raftnode.HasExistingState(cfg)
	if err != nil {
		t.Fatalf("HasExistingState() error: %v", err)
	}
	if !hadState {
		t.Fatal("HasExistingState() = false after Bootstrap+Shutdown, want true")
	}

	resumed, err := raftnode.New(cfg)
	if err != nil {
		t.Fatalf("raftnode.New() (resume) error: %v", err)
	}
	t.Cleanup(func() { resumed.Shutdown() })

	// awaitJoin=true is passed here exactly as run() would pass it on a
	// restart with -await-join left set in rc.conf - hadState must win,
	// resuming normally rather than sitting passively.
	if err := startupJoinOrBootstrap(resumed, hadState, "", cfg.NodeID, cfg.BindAddr, "", true); err != nil {
		t.Fatalf("startupJoinOrBootstrap() error: %v", err)
	}

	eventually(t, 5*time.Second, func() bool { return resumed.Status().IsLeader })
}
