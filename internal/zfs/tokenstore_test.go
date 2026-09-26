package zfs

// The target-local resume token store. ADR-0130 is explicit that the
// token is the TARGET node's own source of truth, never required from
// raft and never the source's to supply — so this is a local file with
// local permissions, and every way it can fail has to be told apart
// from "there is nothing stored".

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFileTokenStoreRoundTrip(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "replication")
	store := NewFileTokenStore(dir)

	// A policy with nothing stored is a confirmed absence, not an
	// error: a caller that forgot to check must not be able to mistake
	// it for "the receive finished cleanly", and must not crash either.
	rec, err := store.Load("policy-1")
	if err != nil {
		t.Fatalf("Load on an empty store = %v, want no error", err)
	}
	if rec.State != ResumeTokenNone || rec.Token != "" {
		t.Errorf("empty store returned %+v", rec)
	}

	const token = "1-9f8e7d6c5b4a3210"
	if err := store.Save("policy-1", token); err != nil {
		t.Fatalf("Save: %v", err)
	}
	rec, err = store.Load("policy-1")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if rec.State != ResumeTokenLive || rec.Token != token {
		t.Errorf("Load = %+v, want the saved token live", rec)
	}
	if rec.PolicyID != "policy-1" {
		t.Errorf("PolicyID = %q", rec.PolicyID)
	}
	if rec.SavedUnix <= 0 {
		t.Error("SavedUnix was not recorded")
	}

	// A token is write authority over a half-received dataset, so the
	// file is 0600 in a 0700 directory — the same cost internal/
	// raftdconfig already pays for its internal token.
	di, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat dir: %v", err)
	}
	if perm := di.Mode().Perm(); perm != 0o700 {
		t.Errorf("token directory mode = %o, want 700", perm)
	}
	fi, err := os.Stat(filepath.Join(dir, "policy-1.token"))
	if err != nil {
		t.Fatalf("stat file: %v", err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("token file mode = %o, want 600", perm)
	}

	// No temp files left behind by the write-through-rename.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp") {
			t.Errorf("Save left a temporary file behind: %s", e.Name())
		}
	}

	// Delete is idempotent, so a cleanup path can run twice.
	if err := store.Delete("policy-1"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if err := store.Delete("policy-1"); err != nil {
		t.Fatalf("second Delete = %v, want no error", err)
	}
	rec, _ = store.Load("policy-1")
	if rec.State != ResumeTokenNone {
		t.Errorf("after Delete, Load = %+v", rec)
	}
}

func TestFileTokenStoreReplacesRatherThanAppends(t *testing.T) {
	dir := t.TempDir()
	store := NewFileTokenStore(dir)
	if err := store.Save("policy-1", "1-aaaaaaaaaaaaaaaa"); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if err := store.Save("policy-1", "1-bbbbbbbbbbbbbbbb"); err != nil {
		t.Fatalf("Save: %v", err)
	}
	rec, err := store.Load("policy-1")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if rec.Token != "1-bbbbbbbbbbbbbbbb" {
		t.Errorf("Token = %q; a second Save must replace, not concatenate", rec.Token)
	}
}

func TestFileTokenStoreTrimsAndValidatesWhatItPersists(t *testing.T) {
	store := NewFileTokenStore(t.TempDir())
	if err := store.Save("policy-1", "  1-trimmed  "); err != nil {
		t.Fatalf("Save with padding: %v", err)
	}
	rec, _ := store.Load("policy-1")
	if rec.Token != "1-trimmed" {
		t.Errorf("Token = %q, want the trimmed value", rec.Token)
	}
	// An empty or implausible token would be handed to `zfs send -t`
	// by a later run, so neither may be written at all.
	for _, bad := range []string{"", "   ", "\n\t ", "null", "none", "-", "unknown", "two\nlines", strings.Repeat("x", 5000)} {
		if err := store.Save("policy-2", bad); err == nil {
			t.Errorf("Save(%q) returned no error", bad)
		}
	}
	// And nothing was created for the refused writes.
	if _, err := os.Stat(filepath.Join(store.Dir, "policy-2.token")); !os.IsNotExist(err) {
		t.Error("a refused Save created a file anyway")
	}
}

func TestFileTokenStoreRefusesUnsafePolicyIDs(t *testing.T) {
	dir := t.TempDir()
	store := NewFileTokenStore(dir)
	// The id becomes a path component under a root-owned directory, so
	// a traversal here is a filesystem escape rather than a cosmetic
	// problem. Every entry is refused before any path is built.
	for _, bad := range []string{
		"",
		"..",
		"../escape",
		"a/b",
		"a\\b",
		".hidden",
		".hidden/../x",
		"with space",
		"nul\x00byte",
		"percent%2f",
		strings.Repeat("x", 129),
	} {
		if err := store.Save(bad, "1-token"); err == nil {
			t.Errorf("Save(%q) returned no error", bad)
		}
		if _, err := store.Load(bad); err == nil {
			t.Errorf("Load(%q) returned no error", bad)
		}
		if err := store.Delete(bad); err == nil {
			t.Errorf("Delete(%q) returned no error", bad)
		}
	}
	// The directory is still empty: nothing escaped and nothing landed.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("the token directory is not empty: %v", entries)
	}
	// The ordinary shapes are accepted.
	for _, good := range []string{"policy-1", "policy_1", "a.b", "A1", "0"} {
		if err := store.Save(good, "1-token"); err != nil {
			t.Errorf("Save(%q) = %v, want nil", good, err)
		}
	}
}

func TestFileTokenStoreUnreadableIsUnknownNotEmpty(t *testing.T) {
	// A store that exists but cannot be read may well hold a token
	// this process simply could not see. Treating it as empty would
	// silently drop the resume path and restart a full receive on top
	// of a half-populated dataset.
	dir := t.TempDir()
	store := NewFileTokenStore(dir)

	// A directory where the file belongs: present, unreadable as a file.
	if err := os.MkdirAll(filepath.Join(dir, "policy-dir.token"), 0o700); err != nil {
		t.Fatalf("prep: %v", err)
	}
	rec, err := store.Load("policy-dir")
	if err == nil {
		t.Fatal("Load on an unreadable store returned no error")
	}
	if !IsUnobserved(err) {
		t.Errorf("err = %v, want an *UnobservedError", err)
	}
	if rec.State != ResumeTokenUnknown {
		t.Errorf("State = %q, want %q", rec.State, ResumeTokenUnknown)
	}

	// An empty file is corruption, not a confirmed absence: something
	// wrote the file and the value did not survive.
	if err := os.WriteFile(filepath.Join(dir, "policy-empty.token"), []byte("   \n"), 0o600); err != nil {
		t.Fatalf("prep: %v", err)
	}
	rec, err = store.Load("policy-empty")
	if !IsUnobserved(err) {
		t.Errorf("err = %v, want an *UnobservedError for an empty token file", err)
	}
	if rec.State != ResumeTokenUnknown {
		t.Errorf("State = %q, want %q", rec.State, ResumeTokenUnknown)
	}
}

func TestFileTokenStoreRecordsTheFilesOwnTime(t *testing.T) {
	// The saved time is the file's mtime, not a timestamp this process
	// remembers. That is what keeps it correct for a token written by
	// an earlier run, a restored backup, or another tool, and it is why
	// no injectable clock exists on this type: an injected one would
	// make the recorded time a statement about this process rather
	// than about the file.
	store := NewFileTokenStore(t.TempDir())
	if err := store.Save("policy-1", "1-token"); err != nil {
		t.Fatalf("Save: %v", err)
	}
	path := filepath.Join(store.Dir, "policy-1.token")
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	rec, err := store.Load("policy-1")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if rec.SavedUnix != fi.ModTime().Unix() {
		t.Errorf("SavedUnix = %d, want the file's own mtime %d", rec.SavedUnix, fi.ModTime().Unix())
	}
	if rec.SavedUnix <= 0 {
		t.Error("SavedUnix was not recorded at all")
	}
}

func TestDefaultTokenDirIsTheDocumentedPath(t *testing.T) {
	// The path is an operational fact: it is where an operator looks
	// for a token, and it matches the ADR's own text.
	if DefaultTokenDir != "/var/db/apiary/replication" {
		t.Errorf("DefaultTokenDir = %q", DefaultTokenDir)
	}
	if got := NewFileTokenStore(""); got.Dir != DefaultTokenDir {
		t.Errorf("NewFileTokenStore(\"\") = %q, want %q", got.Dir, DefaultTokenDir)
	}
}

func TestTokenStoreInterfaceIsSatisfiedByTheFileStore(t *testing.T) {
	var store TokenStore = NewFileTokenStore(t.TempDir())
	if err := store.Save("policy-1", "1-token"); err != nil {
		t.Fatalf("Save: %v", err)
	}
	rec, err := store.Load("policy-1")
	if err != nil || rec.State != ResumeTokenLive || rec.Token != "1-token" {
		t.Errorf("Load = %+v, %v", rec, err)
	}
	if err := store.Delete("policy-1"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
}
