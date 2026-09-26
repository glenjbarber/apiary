package raft

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/hashicorp/raft"
	raftboltdb "github.com/hashicorp/raft-boltdb/v2"
	"go.etcd.io/bbolt"
)

// keyCurrentTerm mirrors hashicorp/raft's own unexported keyCurrentTerm
// (raft.go). It is duplicated rather than imported because raft does not
// export it, and this is the only stable-store key this file needs. The
// value is a fixed on-disk format identifier, not a raft policy string:
// if hashicorp/raft ever changes it, this read reports TermObserved=false
// and Err rather than silently reporting a wrong term. The identical
// trade is already made deliberately elsewhere in this codebase - see
// health.ParseSuffrage, which duplicates raft's wire suffrage strings for
// exactly this reason.
var keyCurrentTerm = []byte("CurrentTerm")

// On-disk layout identifiers, mirroring hashicorp/raft's own unexported
// constants in file_snapshot.go. Duplicated for the same reason
// keyCurrentTerm is - these are format identifiers this file must match,
// not raft policy. The snapshot payload itself is decoded into raft's
// exported SnapshotMeta, so only the names are local.
const (
	// snapshotsDirName is the subdirectory of the raft data dir that
	// FileSnapshotStore keeps its snapshots in.
	snapshotsDirName = "snapshots"
	// snapshotMetaFile is the per-snapshot metadata file name.
	snapshotMetaFile = "meta.json"
	// snapshotTmpSuffix marks a half-written snapshot directory that
	// must be ignored.
	snapshotTmpSuffix = ".tmp"
)

// offlineOpenTimeout bounds how long ReadOfflineStatus waits for the
// BoltDB file lock. raftd holds an exclusive flock on raft.db for its
// whole life, so a read-only open can only succeed while raftd is NOT
// running. Failing after a bounded wait - rather than blocking forever -
// is what makes this safe to run unconditionally from a monitoring
// context.
const offlineOpenTimeout = 2 * time.Second

// offlineMaxLogScan bounds how many log entries ReadOfflineStatus reads
// backwards while hunting for the newest configuration entry. A
// configuration entry is appended on every membership change, and a
// freshly bootstrapped cluster's very first log entry is itself a
// configuration, so the search normally terminates almost immediately.
// The bound exists so a pathological log cannot make a diagnostic tool
// unbounded; hitting it is reported, never silently truncated (see
// OfflineStatus.MembershipNote).
const offlineMaxLogScan = 10000

// Membership source values - which persisted record the reported
// membership was actually read from. Reported rather than assumed, so a
// reader can tell a fresh log entry from a snapshot-carried value.
const (
	// MembershipFromLog: the newest LogConfiguration entry in the raft
	// log was newer than any snapshot's configuration.
	MembershipFromLog = "log"
	// MembershipFromSnapshot: no newer log entry existed, so the
	// configuration carried by the latest snapshot was used. This is
	// normal after log compaction has discarded older entries.
	MembershipFromSnapshot = "snapshot"
	// MembershipUnknown: neither a log entry nor a snapshot yielded a
	// configuration. Not an error on its own - it is the honest
	// "no evidence" answer, and Members is empty.
	MembershipUnknown = "unknown"
)

// ErrOfflineRaftdRunning reports that raft.db could not be opened
// read-only because another process holds its lock. In practice that
// means raftd is running: it holds an exclusive lock on the BoltDB file
// for its entire life, and a consistent read of the log is only
// possible while it is stopped. Callers should stop raftd (or query the
// node over its live gRPC socket) instead of retrying.
var ErrOfflineRaftdRunning = errors.New("raft: raft.db is locked by another process - raftd is most likely running; stop it first, or read this node's status over its live gRPC socket")

// OfflineStatus is a read-only, one-shot view of what this node has
// actually persisted about itself. It is produced by ReadOfflineStatus,
// which never writes, never binds a transport, never contacts a peer,
// and never takes a raft vote - it is the NO-OP counterpart to New, for
// the case where raftd must not be started (or has been stopped) just to
// be asked what it knows.
//
// Every field's zero value is a real, representable state rather than a
// silent success: TermObserved false means the term could not be read,
// not that the term was zero. IndexesObserved false means the log bounds
// are unknown, not that the log is empty.
type OfflineStatus struct {
	// NodeID is the identity this node would use if started.
	NodeID string

	// DataDir and BoltPath are where the state was read from.
	DataDir  string
	BoltPath string

	// Term is the last current-term raft persisted for this node, and
	// TermObserved reports whether it was actually read.
	Term         uint64
	TermObserved bool
	// TermErr explains a TermObserved=false. A missing key is normal on
	// a never-started node and is not an error; anything else is.
	TermErr string

	// FirstIndex/LastIndex bound the retained log, and IndexesObserved
	// reports whether they were actually read. LastIndex is the highest
	// index raft has stored, which includes entries that are written but
	// not yet committed - it is not a commit index, and nothing here
	// should be read as one.
	FirstIndex      uint64
	LastIndex       uint64
	IndexesObserved bool

	// Members is the persisted membership, sorted by ID for stable
	// output. Empty with MembershipSource == MembershipUnknown.
	Members []ServerInfo

	// MembershipSource is which persisted record Members was read from
	// (see the MembershipFrom* constants) and MembershipIndex is the log
	// index that record was written at.
	MembershipSource string
	MembershipIndex  uint64

	// MembershipNote explains a MembershipSource of MembershipUnknown,
	// and is empty whenever membership was actually read.
	MembershipNote string
}

// ReadOfflineStatus reads this node's persisted raft state without
// starting a server, taking a vote, or contacting any peer. It is
// read-only end to end: the BoltDB file is opened with bbolt's
// read-only mode, which takes only a shared lock, and the snapshot store
// is only listed, never written.
//
// It fails with ErrOfflineRaftdRunning if raftd currently holds the
// file, because reading a log under a live writer is exactly the race
// this function exists to make impossible.
func ReadOfflineStatus(cfg Config) (OfflineStatus, error) {
	cfg, err := cfg.withDefaults()
	if err != nil {
		return OfflineStatus{}, err
	}

	status := OfflineStatus{
		NodeID:  cfg.NodeID,
		DataDir: cfg.DataDir,
	}

	// Never create the data directory: a read-only tool that silently
	// mkdir -p's would leave a plausible-looking empty raft state
	// behind for the next real start to trip over.
	info, err := os.Stat(cfg.DataDir)
	if err != nil {
		return OfflineStatus{}, fmt.Errorf("raft: reading offline status: %w", err)
	}
	if !info.IsDir() {
		return OfflineStatus{}, fmt.Errorf("raft: reading offline status: %s is not a directory", cfg.DataDir)
	}

	status.BoltPath = filepath.Join(cfg.DataDir, boltFileName)
	if _, err := os.Stat(status.BoltPath); err != nil {
		return OfflineStatus{}, fmt.Errorf("raft: reading offline status: %w", err)
	}

	store, err := raftboltdb.New(raftboltdb.Options{
		Path:        status.BoltPath,
		BoltOptions: &bbolt.Options{ReadOnly: true, Timeout: offlineOpenTimeout},
	})
	if err != nil {
		if errors.Is(err, bbolt.ErrTimeout) {
			return OfflineStatus{}, ErrOfflineRaftdRunning
		}
		return OfflineStatus{}, fmt.Errorf("raft: opening bolt store read-only: %w", err)
	}
	defer store.Close()

	readTerm(store, &status)
	readIndexBounds(store, &status)
	readMembership(cfg, store, &status)

	return status, nil
}

// readTerm fills in the persisted current term. A missing key is the
// normal state of a data directory that has never held a raft instance,
// so it is recorded as an observation about TermErr rather than raised
// as a failure - this tool must still work on a half-initialized node.
func readTerm(store *raftboltdb.BoltStore, status *OfflineStatus) {
	term, err := store.GetUint64(keyCurrentTerm)
	if err != nil {
		if errors.Is(err, raftboltdb.ErrKeyNotFound) {
			status.TermErr = "no current term persisted yet (this node has never held raft state)"
			return
		}
		status.TermErr = err.Error()
		return
	}
	status.Term = term
	status.TermObserved = true
}

// readIndexBounds reads the retained log's first and last index.
func readIndexBounds(store *raftboltdb.BoltStore, status *OfflineStatus) {
	first, err := store.FirstIndex()
	if err != nil {
		status.MembershipNote = fmt.Sprintf("log first index unreadable: %v", err)
		return
	}
	last, err := store.LastIndex()
	if err != nil {
		status.MembershipNote = fmt.Sprintf("log last index unreadable: %v", err)
		return
	}
	status.FirstIndex = first
	status.LastIndex = last
	status.IndexesObserved = true
}

// readMembership resolves the persisted membership, preferring the
// newest log configuration entry and falling back to the latest
// snapshot's configuration. That ordering mirrors raft's own recovery
// path, and picking by index rather than by "what is available first" is
// what keeps a post-compaction log from silently reporting stale
// membership.
func readMembership(cfg Config, store *raftboltdb.BoltStore, status *OfflineStatus) {
	var (
		snapConf     raft.Configuration
		snapIdx      uint64
		haveSnapshot bool
	)

	// Snapshot metadata is read directly rather than through raft's
	// FileSnapshotStore: constructing that store performs a MkdirAll and
	// a create-then-delete permissions probe, which would mean this
	// function is not read-only - the one property that makes it safe to
	// point at a production node.
	if latest, ok := readLatestSnapshotMeta(cfg.DataDir); ok {
		snapConf = latest.Configuration
		snapIdx = latest.ConfigurationIndex
		haveSnapshot = true
	}

	// Walk backwards from the newest retained entry. A configuration
	// entry is written on every membership change, so this normally
	// stops within a handful of entries.
	var (
		logConf    raft.Configuration
		logIdx     uint64
		haveLog    bool
		scanned    uint64
		scanCapped bool
	)
	if status.IndexesObserved {
		for idx := status.LastIndex; idx >= status.FirstIndex && idx > 0; idx-- {
			if scanned >= offlineMaxLogScan {
				scanCapped = true
				break
			}
			scanned++

			var entry raft.Log
			if err := store.GetLog(idx, &entry); err != nil {
				// Compaction leaves index gaps; a missing entry is
				// expected, not fatal.
				continue
			}
			if entry.Type != raft.LogConfiguration {
				continue
			}
			logConf = raft.DecodeConfiguration(entry.Data)
			logIdx = entry.Index
			haveLog = true
			break
		}
	}

	switch {
	case haveLog && (!haveSnapshot || logIdx >= snapIdx):
		status.Members = serversToInfo(logConf)
		status.MembershipSource = MembershipFromLog
		status.MembershipIndex = logIdx
	case haveSnapshot:
		status.Members = serversToInfo(snapConf)
		status.MembershipSource = MembershipFromSnapshot
		status.MembershipIndex = snapIdx
	default:
		status.MembershipSource = MembershipUnknown
		switch {
		case !status.IndexesObserved:
			// MembershipNote already explains an unreadable log.
		case scanCapped:
			status.MembershipNote = fmt.Sprintf(
				"scanned %d log entries back to index %d without finding a configuration entry and no snapshot carried one; membership is unknown, not empty",
				scanned, status.FirstIndex)
		case status.LastIndex == 0:
			status.MembershipNote = "log is empty (this node has never appended an entry), so no membership has been persisted yet"
		default:
			status.MembershipNote = fmt.Sprintf(
				"no configuration entry found between log indexes %d and %d and no snapshot carried one; membership is unknown, not empty",
				status.FirstIndex, status.LastIndex)
		}
	}

	sort.Slice(status.Members, func(i, j int) bool {
		return status.Members[i].ID < status.Members[j].ID
	})
}

// serversToInfo converts raft's configuration to the package's existing
// ServerInfo shape, so an offline read and a live read report members
// through exactly the same vocabulary.
func serversToInfo(conf raft.Configuration) []ServerInfo {
	out := make([]ServerInfo, 0, len(conf.Servers))
	for _, s := range conf.Servers {
		out = append(out, ServerInfo{
			ID:       string(s.ID),
			Address:  string(s.Address),
			Suffrage: suffrageString(s.Suffrage),
		})
	}
	return out
}

// readLatestSnapshotMeta returns the metadata of the newest snapshot
// under dataDir, found and ordered exactly the way raft's own
// FileSnapshotStore.find does: snapshot directories only (never loose
// files), never a *.tmp half-written one, only versions this raft can
// understand, newest first by term then index.
//
// The directory name, meta file name, and tmp suffix are raft's
// unexported constants, duplicated here for the same reason
// keyCurrentTerm is: they are on-disk format identifiers, not policy.
// The decoded type is raft's own exported SnapshotMeta, so the payload
// format stays raft's rather than a local reimplementation.
//
// A snapshot that cannot be read is skipped rather than fatal: a
// corrupt or foreign-version snapshot is exactly the situation where
// saying "membership unknown" beats refusing to answer at all.
func readLatestSnapshotMeta(dataDir string) (raft.SnapshotMeta, bool) {
	root := filepath.Join(dataDir, snapshotsDirName)

	entries, err := os.ReadDir(root)
	if err != nil {
		return raft.SnapshotMeta{}, false
	}

	var (
		best     raft.SnapshotMeta
		haveBest bool
	)
	for _, entry := range entries {
		if !entry.IsDir() || strings.HasSuffix(entry.Name(), snapshotTmpSuffix) {
			continue
		}
		blob, err := os.ReadFile(filepath.Join(root, entry.Name(), snapshotMetaFile))
		if err != nil {
			continue
		}
		var meta raft.SnapshotMeta
		if err := json.Unmarshal(blob, &meta); err != nil {
			continue
		}
		if meta.Version < raft.SnapshotVersionMin || meta.Version > raft.SnapshotVersionMax {
			continue
		}
		if !haveBest || snapshotMetaLess(best, meta) {
			best = meta
			haveBest = true
		}
	}
	return best, haveBest
}

// snapshotMetaLess reports whether a is older than b, matching raft's
// snapMetaSlice ordering (term first, then index).
func snapshotMetaLess(a, b raft.SnapshotMeta) bool {
	if a.Term != b.Term {
		return a.Term < b.Term
	}
	return a.Index < b.Index
}
