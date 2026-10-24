package main

import (
	"crypto/sha256"
	"net"
	"os"
	"path/filepath"
	"strings"
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
	cfg := raftnode.Config{NodeID: "node-1", DataDir: filepath.Join(t.TempDir(), "raftd"), BindAddr: "127.0.0.1:0", FastElectionTiming: true}
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
	cfg := raftnode.Config{NodeID: "node-1", DataDir: dataDir, BindAddr: "127.0.0.1:0", FastElectionTiming: true}
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

	cfg := raftnode.Config{NodeID: "node-1", DataDir: filepath.Join(t.TempDir(), "raftd"), BindAddr: "127.0.0.1:0", FastElectionTiming: true}
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
	cfg := raftnode.Config{NodeID: "node-1", DataDir: filepath.Join(t.TempDir(), "raftd"), BindAddr: "127.0.0.1:0", FastElectionTiming: true}

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
		NodeID:             "await-node",
		DataDir:            t.TempDir(),
		BindAddr:           freeLoopbackAddr(t),
		FastElectionTiming: true,
	}
	node, err := raftnode.New(cfg)
	if err != nil {
		t.Fatalf("raftnode.New() error: %v", err)
	}
	t.Cleanup(func() { node.Shutdown() })

	if err := startupJoinOrBootstrap(node, false, "", cfg.NodeID, cfg.BindAddr, "", true, cfg.DataDir); err != nil {
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
		NodeID:             "leader",
		DataDir:            t.TempDir(),
		BindAddr:           freeLoopbackAddr(t),
		FastElectionTiming: true,
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
		NodeID:             "joiner",
		DataDir:            t.TempDir(),
		BindAddr:           freeLoopbackAddr(t),
		FastElectionTiming: true,
	}
	joiner, err := raftnode.New(joinerCfg)
	if err != nil {
		t.Fatalf("raftnode.New(joiner) error: %v", err)
	}
	t.Cleanup(func() { joiner.Shutdown() })

	// This is exactly what raftd's run() does when -await-join is set on
	// a fresh data dir: start the node's transport, then do nothing else
	// - no Bootstrap, no -join dial.
	if err := startupJoinOrBootstrap(joiner, false, "", joinerCfg.NodeID, joinerCfg.BindAddr, "", true, joinerCfg.DataDir); err != nil {
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
		NodeID:             "resumed-node",
		DataDir:            t.TempDir(),
		BindAddr:           freeLoopbackAddr(t),
		FastElectionTiming: true,
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
	if err := startupJoinOrBootstrap(resumed, hadState, "", cfg.NodeID, cfg.BindAddr, "", true, cfg.DataDir); err != nil {
		t.Fatalf("startupJoinOrBootstrap() error: %v", err)
	}

	eventually(t, 5*time.Second, func() bool { return resumed.Status().IsLeader })
}

func TestInternalTokenWarning(t *testing.T) {
	w := internalTokenWarning("", "/var/run/apiary/raftd.sock")
	if !strings.Contains(w, "internal_token is not set") || !strings.Contains(w, "/var/run/apiary/raftd.sock") {
		t.Errorf("empty token: warning = %q, want it to name the option and the socket", w)
	}
	if w := internalTokenWarning("s3cret", "/x"); w != "" {
		t.Errorf("a configured token must not warn, got %q", w)
	}
}

func TestLoopbackRaftBindWarning(t *testing.T) {
	w := loopbackRaftBindWarning("127.0.0.1:17600", false)
	if !strings.Contains(w, "127.0.0.1:17600") || !strings.Contains(w, "single_node") {
		t.Errorf("loopback, not single-node: warning = %q, want it to name the address and single_node", w)
	}
	if w := loopbackRaftBindWarning("127.0.0.1:17600", true); w != "" {
		t.Errorf("single_node must silence the loopback warning, got %q", w)
	}
	if w := loopbackRaftBindWarning("sting.lab3.home.arpa:17600", false); w != "" {
		t.Errorf("a routable address must not warn, got %q", w)
	}
	if w := loopbackRaftBindWarning("::1", false); w != "" {
		t.Errorf("an address with no port must not warn (SplitHostPort fails), got %q", w)
	}
}

func TestRaftTLSWarning(t *testing.T) {
	off := false
	on := true
	if w := raftTLSWarning(nil); w != "" {
		t.Errorf("absent raft_tls_enabled warned %q, want no warning (encryption is the default)", w)
	}
	if w := raftTLSWarning(&on); w != "" {
		t.Errorf("raft_tls_enabled: true warned %q, want no warning", w)
	}
	w := raftTLSWarning(&off)
	for _, want := range []string{"WARNING", "PLAINTEXT", "raft_tls_enabled", "raft_tls_cert", "raft_tls_key", "raft_tls_ca", "cluster state", "forwarded credentials", "ADR-0078"} {
		if !strings.Contains(w, want) {
			t.Errorf("raft_tls_enabled: false warning = %q, want it to mention %q", w, want)
		}
	}
}

// TestResolveRaftTLS covers issue #97's startup gate: raftd must refuse
// to start with no TLS fields and no raft_tls_enabled, must start with
// the full trio, and must start in plaintext only on an explicit false.
func TestResolveRaftTLS(t *testing.T) {
	cases := []struct {
		name            string
		on              bool
		explicit        bool
		cert, key, ca   string
		wantErr         bool
		wantCert, wantK string
		wantCA          string
	}{
		{name: "absent, no fields: refused", on: true, explicit: false, wantErr: true},
		{name: "absent, partial fields: refused", on: true, cert: "/c", key: "/k", wantErr: true},
		{name: "true, no fields: refused", on: true, explicit: true, wantErr: true},
		{name: "absent, full trio: TLS", on: true, cert: "/c", key: "/k", ca: "/ca", wantCert: "/c", wantK: "/k", wantCA: "/ca"},
		{name: "true, full trio: TLS", on: true, explicit: true, cert: "/c", key: "/k", ca: "/ca", wantCert: "/c", wantK: "/k", wantCA: "/ca"},
		{name: "false, no fields: plaintext", on: false, explicit: true},
		{name: "false, fields still set: refused", on: false, explicit: true, cert: "/c", key: "/k", ca: "/ca", wantErr: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cert, key, ca, err := resolveRaftTLS(c.on, c.cert, c.key, c.ca, c.explicit)
			if (err != nil) != c.wantErr {
				t.Fatalf("resolveRaftTLS() error = %v, wantErr %v", err, c.wantErr)
			}
			if err != nil {
				if !c.on {
					return
				}
				for _, want := range []string{"raft_tls_cert", "raft_tls_key", "raft_tls_ca"} {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("error %q does not name %s", err, want)
					}
				}
				if !strings.Contains(err.Error(), `"raft_tls_enabled": false`) {
					t.Errorf("error %q does not say raft_tls_enabled: false turns encryption off", err)
				}
				return
			}
			if cert != c.wantCert || key != c.wantK || ca != c.wantCA {
				t.Errorf("resolveRaftTLS() = %q,%q,%q, want %q,%q,%q", cert, key, ca, c.wantCert, c.wantK, c.wantCA)
			}
			if !c.on && (cert != "" || key != "" || ca != "") {
				t.Errorf("plaintext mode returned TLS material %q,%q,%q", cert, key, ca)
			}
		})
	}
}
