package raft

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"

	"github.com/hashicorp/go-msgpack/v2/codec"
	"github.com/hashicorp/raft"
	raftboltdb "github.com/hashicorp/raft-boltdb/v2"
	"go.etcd.io/bbolt"
)

// writeOfflineStore builds a BoltDB file laid out exactly as raft-boltdb
// lays one out (buckets "logs" and "conf"), so these tests exercise the
// real on-disk format rather than a convenient imitation of it.
func writeOfflineStore(t *testing.T, dataDir string, term uint64, entries map[uint64]raft.Log) {
	t.Helper()

	db, err := bbolt.Open(filepath.Join(dataDir, boltFileName), 0o600, nil)
	if err != nil {
		t.Fatalf("opening bolt db: %v", err)
	}
	defer db.Close()

	err = db.Update(func(tx *bbolt.Tx) error {
		logs, err := tx.CreateBucketIfNotExists([]byte("logs"))
		if err != nil {
			return err
		}
		conf, err := tx.CreateBucketIfNotExists([]byte("conf"))
		if err != nil {
			return err
		}
		if term != 0 {
			buf := make([]byte, 8)
			binary.BigEndian.PutUint64(buf, term)
			if err := conf.Put(keyCurrentTerm, buf); err != nil {
				return err
			}
		}
		for idx, entry := range entries {
			enc := encodeTestLog(t, entry)
			key := make([]byte, 8)
			binary.BigEndian.PutUint64(key, idx)
			if err := logs.Put(key, enc); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("seeding bolt db: %v", err)
	}
}

func configEntry(t *testing.T, index uint64, servers ...raft.Server) raft.Log {
	t.Helper()
	return raft.Log{
		Index: index,
		Term:  2,
		Type:  raft.LogConfiguration,
		Data:  raft.EncodeConfiguration(raft.Configuration{Servers: servers}),
	}
}

// encodeTestLog msgpack-encodes a log entry in the same shape
// raft-boltdb uses, via the library's own encoder, so fixtures are
// decoded by the production path rather than a hand-rolled imitation.
func encodeTestLog(t *testing.T, entry raft.Log) []byte {
	t.Helper()
	var buf bytes.Buffer
	enc := codec.NewEncoder(&buf, &codec.MsgpackHandle{})
	if err := enc.Encode(entry); err != nil {
		t.Fatalf("encoding log entry: %v", err)
	}
	return buf.Bytes()
}

func readFileBytes(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	return b
}

// writeTestSnapshot writes a snapshot in the real FileSnapshotStore
// layout: <dataDir>/snapshots/<term>-<index>-<msec>/meta.json, holding
// a fileSnapshotMeta-shaped envelope. A loose file or a wrong directory
// would simply be invisible to the reader, which is why this mirrors the
// layout exactly.
func writeTestSnapshot(t *testing.T, dataDir string, meta raft.SnapshotMeta) {
	t.Helper()

	name := fmt.Sprintf("%d-%d-%d", meta.Term, meta.Index, 1700000000000)
	dir := filepath.Join(dataDir, snapshotsDirName, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("creating snapshot dir: %v", err)
	}
	meta.ID = name

	envelope := struct {
		raft.SnapshotMeta
		CRC []byte
	}{SnapshotMeta: meta}

	blob, err := json.Marshal(envelope)
	if err != nil {
		t.Fatalf("encoding snapshot meta: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, snapshotMetaFile), blob, 0o644); err != nil {
		t.Fatalf("writing snapshot meta: %v", err)
	}
}

// listTree returns every path under root, relative, sorted - used to
// prove the read adds nothing to and removes nothing from the data dir.
func listTree(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return relErr
		}
		if rel == "." {
			return nil
		}
		out = append(out, fmt.Sprintf("%s (dir=%v size=%d)", rel, info.IsDir(), info.Size()))
		return nil
	})
	if err != nil {
		t.Fatalf("walking %s: %v", root, err)
	}
	sort.Strings(out)
	return out
}

func TestReadOfflineStatus_MissingDataDirFailsWithoutCreatingIt(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "absent")

	if _, err := ReadOfflineStatus(Config{DataDir: dir}); err == nil {
		t.Fatal("expected an error for a data dir that does not exist")
	}

	// A read-only diagnostic must not leave a plausible-looking empty
	// raft state behind for the next real start.
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("ReadOfflineStatus created the data dir: stat err = %v", err)
	}
}

func TestReadOfflineStatus_EmptyDirHasNoEvidenceNotEmptyMembership(t *testing.T) {
	dir := t.TempDir()

	status, err := ReadOfflineStatus(Config{NodeID: "n1", DataDir: dir})
	if err == nil {
		// No raft.db at all is also a legitimate, non-fatal answer, but
		// only if the caller is told; either way membership must be
		// reported as unknown rather than silently empty.
		t.Fatalf("expected an error with no bolt file present, got status %+v", status)
	}
}

func TestReadOfflineStatus_FreshStoreReportsUnknown(t *testing.T) {
	dir := t.TempDir()
	writeOfflineStore(t, dir, 0, nil)

	status, err := ReadOfflineStatus(Config{NodeID: "n1", DataDir: dir})
	if err != nil {
		t.Fatalf("ReadOfflineStatus: %v", err)
	}

	if status.MembershipSource != MembershipUnknown {
		t.Errorf("MembershipSource = %q, want %q", status.MembershipSource, MembershipUnknown)
	}
	if len(status.Members) != 0 {
		t.Errorf("Members = %+v, want empty", status.Members)
	}
	if status.MembershipNote == "" {
		t.Error("MembershipNote is empty; an unknown verdict must explain itself")
	}
	if status.TermObserved {
		t.Error("TermObserved = true on a store with no persisted term")
	}
	if status.TermErr == "" {
		t.Error("TermErr is empty; a missing term must be explained, not silently zero")
	}
}

func TestReadOfflineStatus_ReadsMembershipAndTermFromLog(t *testing.T) {
	dir := t.TempDir()
	writeOfflineStore(t, dir, 7, map[uint64]raft.Log{
		1: configEntry(t, 1,
			raft.Server{ID: "n1", Address: "10.0.0.1:17600", Suffrage: raft.Voter},
			raft.Server{ID: "n2", Address: "10.0.0.2:17600", Suffrage: raft.Voter},
		),
		2: {Index: 2, Term: 2, Type: raft.LogNoop},
		3: {Index: 3, Term: 2, Type: raft.LogBarrier},
	})

	status, err := ReadOfflineStatus(Config{NodeID: "n1", DataDir: dir})
	if err != nil {
		t.Fatalf("ReadOfflineStatus: %v", err)
	}

	if !status.TermObserved || status.Term != 7 {
		t.Errorf("term = %d observed=%v, want 7/true", status.Term, status.TermObserved)
	}
	if status.MembershipSource != MembershipFromLog {
		t.Errorf("MembershipSource = %q, want %q", status.MembershipSource, MembershipFromLog)
	}
	if status.MembershipIndex != 1 {
		t.Errorf("MembershipIndex = %d, want 1", status.MembershipIndex)
	}
	if !status.IndexesObserved {
		t.Error("IndexesObserved = false, want true")
	}
	if len(status.Members) != 2 {
		t.Fatalf("Members = %+v, want 2", status.Members)
	}
	// Sorted by ID, and n1 sorts before n2.
	if status.Members[0].ID != "n1" || status.Members[1].ID != "n2" {
		t.Errorf("members not sorted by ID: %+v", status.Members)
	}
	if status.Members[0].Suffrage != "Voter" {
		t.Errorf("suffrage = %q, want %q", status.Members[0].Suffrage, "Voter")
	}
	if status.Members[0].Address != "10.0.0.1:17600" {
		t.Errorf("address = %q, want 10.0.0.1:17600", status.Members[0].Address)
	}
}

func TestReadOfflineStatus_PrefersNewestConfigurationEntry(t *testing.T) {
	dir := t.TempDir()
	// A later configuration entry adds a member; the read must report
	// the newer set, not the first one it encounters scanning back from
	// the end of the log.
	writeOfflineStore(t, dir, 3, map[uint64]raft.Log{
		1: configEntry(t, 1, raft.Server{ID: "n1", Address: "10.0.0.1:17600", Suffrage: raft.Voter}),
		2: {Index: 2, Term: 2, Type: raft.LogNoop},
		3: configEntry(t, 3,
			raft.Server{ID: "n1", Address: "10.0.0.1:17600", Suffrage: raft.Voter},
			raft.Server{ID: "n2", Address: "10.0.0.2:17600", Suffrage: raft.Nonvoter},
			raft.Server{ID: "n3", Address: "10.0.0.3:17600", Suffrage: raft.Staging},
		),
		4: {Index: 4, Term: 3, Type: raft.LogBarrier},
	})

	status, err := ReadOfflineStatus(Config{NodeID: "n1", DataDir: dir})
	if err != nil {
		t.Fatalf("ReadOfflineStatus: %v", err)
	}

	if status.MembershipIndex != 3 {
		t.Errorf("MembershipIndex = %d, want 3 (the newest config entry)", status.MembershipIndex)
	}
	if len(status.Members) != 3 {
		t.Fatalf("Members = %+v, want 3", status.Members)
	}
	byID := map[string]string{}
	for _, m := range status.Members {
		byID[m.ID] = m.Suffrage
	}
	for id, want := range map[string]string{"n1": "Voter", "n2": "Nonvoter", "n3": "Staging"} {
		if byID[id] != want {
			t.Errorf("%s suffrage = %q, want %q", id, byID[id], want)
		}
	}
}

func TestReadOfflineStatus_LockedStoreReportsRaftdRunning(t *testing.T) {
	dir := t.TempDir()
	// Hold an exclusive write lock the way a running raftd does.
	db, err := bbolt.Open(filepath.Join(dir, boltFileName), 0o600, &bbolt.Options{Timeout: 0})
	if err != nil {
		t.Fatalf("opening bolt db: %v", err)
	}
	defer db.Close()

	_, err = ReadOfflineStatus(Config{NodeID: "n1", DataDir: dir})
	if err == nil {
		t.Fatal("expected an error while another process holds the lock")
	}
	if !errors.Is(err, ErrOfflineRaftdRunning) {
		t.Errorf("err = %v, want ErrOfflineRaftdRunning", err)
	}
}

func TestReadOfflineStatus_CompactedLogFallsBackToSnapshot(t *testing.T) {
	dir := t.TempDir()
	// A log whose only retained entry is a no-op, plus a snapshot
	// carrying the configuration. The snapshot must be used rather than
	// reporting unknown membership.
	writeOfflineStore(t, dir, 5, map[uint64]raft.Log{
		9: {Index: 9, Term: 5, Type: raft.LogNoop},
	})
	writeTestSnapshot(t, dir, raft.SnapshotMeta{
		Version:            1,
		Index:              8,
		Term:               5,
		ConfigurationIndex: 4,
		Configuration: raft.Configuration{Servers: []raft.Server{
			{ID: "n1", Address: "10.0.0.1:17600", Suffrage: raft.Voter},
		}},
	})

	status, err := ReadOfflineStatus(Config{NodeID: "n1", DataDir: dir})
	if err != nil {
		t.Fatalf("ReadOfflineStatus: %v", err)
	}
	if status.MembershipSource != MembershipFromSnapshot {
		t.Errorf("MembershipSource = %q, want %q", status.MembershipSource, MembershipFromSnapshot)
	}
	if len(status.Members) != 1 || status.Members[0].ID != "n1" {
		t.Errorf("Members = %+v, want the snapshot's single member", status.Members)
	}
}

func TestReadOfflineStatus_LogConfigWinsOverOlderSnapshot(t *testing.T) {
	dir := t.TempDir()
	writeTestSnapshot(t, dir, raft.SnapshotMeta{
		Version:            1,
		Index:              4,
		Term:               4,
		ConfigurationIndex: 4,
		Configuration: raft.Configuration{Servers: []raft.Server{
			{ID: "n1", Address: "10.0.0.1:17600", Suffrage: raft.Voter},
		}},
	})
	writeOfflineStore(t, dir, 6, map[uint64]raft.Log{
		5: {Index: 5, Term: 5, Type: raft.LogNoop},
		6: configEntry(t, 6,
			raft.Server{ID: "n1", Address: "10.0.0.1:17600", Suffrage: raft.Voter},
			raft.Server{ID: "n2", Address: "10.0.0.2:17600", Suffrage: raft.Voter},
		),
	})

	status, err := ReadOfflineStatus(Config{NodeID: "n1", DataDir: dir})
	if err != nil {
		t.Fatalf("ReadOfflineStatus: %v", err)
	}
	if status.MembershipSource != MembershipFromLog || status.MembershipIndex != 6 {
		t.Errorf("got source=%q index=%d, want log/6", status.MembershipSource, status.MembershipIndex)
	}
	if len(status.Members) != 2 {
		t.Errorf("Members = %+v, want 2", status.Members)
	}
}

func TestReadOfflineStatus_EmptyLogAndNoSnapshotIsUnknown(t *testing.T) {
	dir := t.TempDir()
	writeOfflineStore(t, dir, 2, map[uint64]raft.Log{})

	status, err := ReadOfflineStatus(Config{NodeID: "n1", DataDir: dir})
	if err != nil {
		t.Fatalf("ReadOfflineStatus: %v", err)
	}
	if status.MembershipSource != MembershipUnknown {
		t.Errorf("MembershipSource = %q, want %q", status.MembershipSource, MembershipUnknown)
	}
	if status.MembershipNote == "" {
		t.Error("MembershipNote empty; unknown membership must be explained")
	}
}

// TestReadOfflineStatus_DoesNotMutateTheStore is the property that makes
// this safe to point at a production node: neither the log bytes nor the
// set of files on disk may change.
// TestReadOfflineStatus_NewestSnapshotWins guards the ordering choice
// directly: raft lists snapshots newest-first, so reading the wrong end
// of that slice would silently report stale membership after a series of
// membership changes.
func TestReadOfflineStatus_NewestSnapshotWins(t *testing.T) {
	dir := t.TempDir()
	writeOfflineStore(t, dir, 8, map[uint64]raft.Log{
		9: {Index: 9, Term: 8, Type: raft.LogNoop},
	})
	// Two snapshots, the older one written first.
	writeTestSnapshot(t, dir, raft.SnapshotMeta{
		Version: 1, Index: 2, Term: 2, ConfigurationIndex: 2,
		Configuration: raft.Configuration{Servers: []raft.Server{
			{ID: "old-only", Address: "10.0.0.9:17600", Suffrage: raft.Voter},
		}},
	})
	writeTestSnapshot(t, dir, raft.SnapshotMeta{
		Version: 1, Index: 7, Term: 7, ConfigurationIndex: 6,
		Configuration: raft.Configuration{Servers: []raft.Server{
			{ID: "n1", Address: "10.0.0.1:17600", Suffrage: raft.Voter},
			{ID: "n2", Address: "10.0.0.2:17600", Suffrage: raft.Voter},
		}},
	})

	status, err := ReadOfflineStatus(Config{NodeID: "n1", DataDir: dir})
	if err != nil {
		t.Fatalf("ReadOfflineStatus: %v", err)
	}
	if status.MembershipSource != MembershipFromSnapshot {
		t.Fatalf("MembershipSource = %q, want %q", status.MembershipSource, MembershipFromSnapshot)
	}
	if status.MembershipIndex != 6 {
		t.Errorf("MembershipIndex = %d, want 6 (the newer snapshot)", status.MembershipIndex)
	}
	if len(status.Members) != 2 {
		t.Fatalf("Members = %+v, want the newer snapshot's 2 members", status.Members)
	}
	for _, m := range status.Members {
		if m.ID == "old-only" {
			t.Error("reported the older snapshot's membership")
		}
	}
}

// TestReadOfflineStatus_TmpSnapshotIgnored mirrors raft's own rule that a
// *.tmp directory is a half-written snapshot that must never be read.
func TestReadOfflineStatus_TmpSnapshotIgnored(t *testing.T) {
	dir := t.TempDir()
	writeOfflineStore(t, dir, 8, map[uint64]raft.Log{
		9: {Index: 9, Term: 8, Type: raft.LogNoop},
	})
	writeTestSnapshot(t, dir, raft.SnapshotMeta{
		Version: 1, Index: 2, Term: 2, ConfigurationIndex: 2,
		Configuration: raft.Configuration{Servers: []raft.Server{
			{ID: "committed", Address: "10.0.0.1:17600", Suffrage: raft.Voter},
		}},
	})
	// A newer but still half-written snapshot must be ignored.
	writeTestSnapshot(t, dir, raft.SnapshotMeta{
		Version: 1, Index: 8, Term: 8, ConfigurationIndex: 8,
		Configuration: raft.Configuration{Servers: []raft.Server{
			{ID: "half-written", Address: "10.0.0.8:17600", Suffrage: raft.Voter},
		}},
	})
	renameToTmp(t, dir)

	status, err := ReadOfflineStatus(Config{NodeID: "committed", DataDir: dir})
	if err != nil {
		t.Fatalf("ReadOfflineStatus: %v", err)
	}
	for _, m := range status.Members {
		if m.ID == "half-written" {
			t.Error("read a *.tmp half-written snapshot")
		}
	}
	if len(status.Members) != 1 {
		t.Errorf("Members = %+v, want only the committed snapshot's member", status.Members)
	}
}

// renameToTmp appends the tmp suffix to the newest snapshot directory.
func renameToTmp(t *testing.T, dataDir string) {
	t.Helper()
	root := filepath.Join(dataDir, snapshotsDirName)
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("reading snapshots dir: %v", err)
	}
	var newest string
	var newestTerm, newestIndex uint64
	for _, e := range entries {
		var meta raft.SnapshotMeta
		blob, err := os.ReadFile(filepath.Join(root, e.Name(), snapshotMetaFile))
		if err != nil {
			continue
		}
		if err := json.Unmarshal(blob, &meta); err != nil {
			continue
		}
		if meta.Term > newestTerm || (meta.Term == newestTerm && meta.Index > newestIndex) {
			newest, newestTerm, newestIndex = e.Name(), meta.Term, meta.Index
		}
	}
	if newest == "" {
		t.Fatal("no snapshot found to mark as tmp")
	}
	if err := os.Rename(filepath.Join(root, newest), filepath.Join(root, newest+snapshotTmpSuffix)); err != nil {
		t.Fatalf("renaming snapshot to tmp: %v", err)
	}
}

func TestReadOfflineStatus_DoesNotMutateTheStore(t *testing.T) {
	dir := t.TempDir()
	writeOfflineStore(t, dir, 4, map[uint64]raft.Log{
		1: configEntry(t, 1, raft.Server{ID: "n1", Address: "10.0.0.1:17600", Suffrage: raft.Voter}),
		2: {Index: 2, Term: 4, Type: raft.LogNoop},
	})
	writeTestSnapshot(t, dir, raft.SnapshotMeta{
		Version: 1, Index: 2, Term: 4, ConfigurationIndex: 1,
		Configuration: raft.Configuration{Servers: []raft.Server{
			{ID: "n1", Address: "10.0.0.1:17600", Suffrage: raft.Voter},
		}},
	})

	beforeBytes := readFileBytes(t, filepath.Join(dir, boltFileName))
	beforeTree := listTree(t, dir)

	if _, err := ReadOfflineStatus(Config{NodeID: "n1", DataDir: dir}); err != nil {
		t.Fatalf("ReadOfflineStatus: %v", err)
	}

	afterBytes := readFileBytes(t, filepath.Join(dir, boltFileName))
	if len(beforeBytes) != len(afterBytes) {
		t.Fatalf("bolt file size changed: %d -> %d", len(beforeBytes), len(afterBytes))
	}
	for i := range beforeBytes {
		if beforeBytes[i] != afterBytes[i] {
			t.Fatalf("bolt file content changed at byte %d", i)
		}
	}

	afterTree := listTree(t, dir)
	if !reflect.DeepEqual(beforeTree, afterTree) {
		t.Errorf("data directory contents changed:\n before: %v\n after:  %v", beforeTree, afterTree)
	}
}

// TestReadOfflineStatus_NoSnapshotsDirIsNotAnError guards the read-only
// property directly: constructing raft's FileSnapshotStore would
// MkdirAll that directory and run a create-then-delete permissions
// probe, so a read must not touch a data dir that has no snapshots.
func TestReadOfflineStatus_NoSnapshotsDirIsNotCreated(t *testing.T) {
	dir := t.TempDir()
	writeOfflineStore(t, dir, 1, map[uint64]raft.Log{
		1: configEntry(t, 1, raft.Server{ID: "n1", Address: "a", Suffrage: raft.Voter}),
	})

	before := listTree(t, dir)
	if _, err := ReadOfflineStatus(Config{NodeID: "n1", DataDir: dir}); err != nil {
		t.Fatalf("ReadOfflineStatus: %v", err)
	}
	after := listTree(t, dir)

	if !reflect.DeepEqual(before, after) {
		t.Errorf("read changed the data dir:\n before: %v\n after:  %v", before, after)
	}
	if _, err := os.Stat(filepath.Join(dir, snapshotsDirName)); !os.IsNotExist(err) {
		t.Errorf("snapshots dir was created by a read: stat err = %v", err)
	}
}

func TestSuffrageString_ReusedForOfflineReads(t *testing.T) {
	// The offline and live paths must report suffrage through one
	// vocabulary, so an unknown value cannot diverge between them.
	if got := suffrageString(raft.Staging); got != "Staging" {
		t.Errorf("suffrageString(Staging) = %q, want Staging", got)
	}
	if got := suffrageString(raft.ServerSuffrage(99)); got != "Unknown" {
		t.Errorf("suffrageString(99) = %q, want Unknown", got)
	}
}

func TestReadOfflineStatus_MissingBoltFileIsAnError(t *testing.T) {
	dir := t.TempDir()
	if _, err := ReadOfflineStatus(Config{NodeID: "n1", DataDir: dir}); err == nil {
		t.Fatal("expected an error when raft.db is absent")
	} else if !errors.Is(err, os.ErrNotExist) {
		t.Logf("error is %v (does not wrap os.ErrNotExist, acceptable)", err)
	}
}

func TestReadOfflineStatus_StoreOpenedByRaftBoltdbIsReadable(t *testing.T) {
	// Sanity check that a store written by the real raft-boltdb writer
	// is readable by the read-only path, which is the case that matters
	// in production.
	dir := t.TempDir()
	store, err := raftboltdb.New(raftboltdb.Options{Path: filepath.Join(dir, boltFileName)})
	if err != nil {
		t.Fatalf("raftboltdb.New: %v", err)
	}
	if err := store.SetUint64(keyCurrentTerm, 9); err != nil {
		t.Fatalf("SetUint64: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	status, err := ReadOfflineStatus(Config{NodeID: "n1", DataDir: dir})
	if err != nil {
		t.Fatalf("ReadOfflineStatus: %v", err)
	}
	if !status.TermObserved || status.Term != 9 {
		t.Errorf("term = %d observed=%v, want 9/true", status.Term, status.TermObserved)
	}
}
