package main

import (
	"os"
	"path/filepath"
	"testing"
)

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
