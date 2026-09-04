package main

import (
	"crypto/sha256"
	"os"
	"path/filepath"
	"testing"

	"google.golang.org/protobuf/proto"

	internalpb "github.com/glenjbarber/apiary/api/internalpb"
	raftnode "github.com/glenjbarber/apiary/internal/raft"
)

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
