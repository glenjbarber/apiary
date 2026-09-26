package restartplan

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// DefaultStateDir is where both the pending-restart records and the
// outcome records live. It matches internal/manager's own
// NewRestartConfirmStore("/var/db/apiary/guardrail") directory
// (cmd/managerd/main.go), because the pending-restart file is written by
// whoever issues the restart and read back by the restarted process on
// its next startup - two processes that must agree on the path, not two
// independent conventions that happen to look alike.
const DefaultStateDir = "/var/db/apiary/guardrail"

// DefaultResultDir is where this package's own outcome records live.
// Separate from DefaultStateDir on purpose: the pending-restart file is
// a blocking guardrail record with a single writer and a single reader,
// while the outcome file is an append-shaped history an operator reads
// after the fact. Neither is raft state - both are node-local files
// that can be lost and re-derived, which is why neither is treated as
// authoritative anywhere in this package.
const DefaultResultDir = "/var/db/apiary/restart-plan"

// PendingRestart is the on-disk record of a restart lease this node's
// own process is about to act on, written BEFORE the restart command is
// issued and read back by the restarted process on its next startup
// (ADR-0103's pattern, ADR-0125 §2 closing the gap that made it
// impossible for raftd).
//
// The JSON field names and the file name it produces are byte-for-byte
// identical to internal/manager.PendingRestart and
// internal/manager.RestartConfirmStore, which is the writer in
// production. cmd/raftd's own test asserts that equivalence against the
// real manager type rather than trusting this comment, because a silent
// drift here would strand a raft-replicated lease with nothing able to
// release it.
type PendingRestart struct {
	Service string `json:"service"`
	NodeID  string `json:"node_id"`
	LeaseID uint64 `json:"lease_id"`
}

// PendingStore reads, writes and clears the pending-restart record for
// one service: the local, node-private trace of a restart lease this
// node's own process is acting on. Its JSON field names, file name and
// write sequence are kept byte-for-byte identical to
// internal/manager's PendingRestart/RestartConfirmStore, which is the
// writer in production - see Save's doc comment.
type PendingStore struct {
	Dir string
}

// NewPendingStore returns a PendingStore rooted at dir, defaulting to
// DefaultStateDir.
func NewPendingStore(dir string) *PendingStore {
	if dir == "" {
		dir = DefaultStateDir
	}
	return &PendingStore{Dir: dir}
}

func (p *PendingStore) path(service string) string {
	return filepath.Join(p.Dir, "pending-restart-"+service+".json")
}

// Save writes p atomically (temp file + chmod 0600 + rename),
// overwriting any stale prior record for the same service.
//
// internal/manager.RestartConfirmStore is the writer of this file in
// production today (RestartNodeService writes the record immediately
// before issuing the restart command). This Save exists so that Engine's
// state machine is complete on its own - a workflow that reserved a
// lease and issued a restart command without leaving a discoverable
// record is exactly the stranded-lease failure ADR-0103/0125 exist to
// prevent, and the state machine has to be able to do the right thing
// without a second writer standing in for it.
//
// Two writers to one file is a real risk, so it is made checkable rather
// than merely documented: TestPendingStoreMatchesManagerRestartConfirmStore
// writes through each type in turn and reads back through the other,
// asserting the file name, the JSON bytes and the permissions all agree.
// If internal/manager's format ever drifts, that test fails.
func (p *PendingStore) Save(restart PendingRestart) error {
	if err := os.MkdirAll(p.Dir, 0o700); err != nil {
		return fmt.Errorf("restartplan: creating pending-restart directory: %w", err)
	}
	body, err := json.Marshal(restart)
	if err != nil {
		return fmt.Errorf("restartplan: encoding pending restart: %w", err)
	}
	path := p.path(restart.Service)
	tmp, err := os.CreateTemp(p.Dir, ".pending-restart-*.tmp")
	if err != nil {
		return fmt.Errorf("restartplan: creating temp file: %w", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath) // no-op once successfully renamed
	if _, err := tmp.Write(body); err != nil {
		tmp.Close()
		return fmt.Errorf("restartplan: writing temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("restartplan: closing temp file: %w", err)
	}
	if err := os.Chmod(tmpPath, 0o600); err != nil {
		return fmt.Errorf("restartplan: setting permissions: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("restartplan: finalizing write: %w", err)
	}
	return nil
}

// Load reads the pending record for service. A missing file is not an
// error: it means either no restart is pending, or a prior one already
// confirmed and cleared itself.
func (p *PendingStore) Load(service string) (PendingRestart, bool, error) {
	data, err := os.ReadFile(p.path(service))
	if err != nil {
		if os.IsNotExist(err) {
			return PendingRestart{}, false, nil
		}
		return PendingRestart{}, false, fmt.Errorf("restartplan: reading pending restart: %w", err)
	}
	var pending PendingRestart
	if err := json.Unmarshal(data, &pending); err != nil {
		return PendingRestart{}, false, fmt.Errorf("restartplan: parsing pending restart: %w", err)
	}
	return pending, true, nil
}

// Clear removes the pending record for service, and is called only
// after a confirmation has actually succeeded - never on a failed or
// unknown attempt, which is what keeps a still-blocked lease still
// blocked instead of silently unblocking itself.
func (p *PendingStore) Clear(service string) error {
	if err := os.Remove(p.path(service)); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("restartplan: removing pending restart: %w", err)
	}
	return nil
}

// ResultStore persists one Result per (node, service) pair, atomically
// (temp file + chmod 0600 + rename), following the same convention
// internal/deadman and internal/manager's own store use.
//
// Save validates before it writes: a record whose verdict is not backed
// by the evidence that verdict implies is refused at the boundary rather
// than becoming a durable, quietly-wrong claim that some later reader
// would take at face value.
type ResultStore struct {
	Dir string
}

// NewResultStore returns a ResultStore rooted at dir, defaulting to
// DefaultResultDir.
func NewResultStore(dir string) *ResultStore {
	if dir == "" {
		dir = DefaultResultDir
	}
	return &ResultStore{Dir: dir}
}

func (r *ResultStore) path(nodeID, service string) string {
	return filepath.Join(r.Dir, "restart-result-"+safeFileComponent(nodeID)+"-"+safeFileComponent(service)+".json")
}

// Save validates and durably writes res for (nodeID, service).
func (r *ResultStore) Save(res Result) error {
	if err := res.Validate(); err != nil {
		return err
	}
	if err := os.MkdirAll(r.Dir, 0o700); err != nil {
		return fmt.Errorf("restartplan: creating result directory: %w", err)
	}
	body, err := json.Marshal(res)
	if err != nil {
		return fmt.Errorf("restartplan: encoding restart result: %w", err)
	}
	tmp, err := os.CreateTemp(r.Dir, ".restart-result-*.tmp")
	if err != nil {
		return fmt.Errorf("restartplan: creating temp file: %w", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath) // no-op once successfully renamed
	if _, err := tmp.Write(body); err != nil {
		tmp.Close()
		return fmt.Errorf("restartplan: writing temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("restartplan: closing temp file: %w", err)
	}
	if err := os.Chmod(tmpPath, 0o600); err != nil {
		return fmt.Errorf("restartplan: setting permissions: %w", err)
	}
	if err := os.Rename(tmpPath, r.path(res.NodeID, res.Service)); err != nil {
		return fmt.Errorf("restartplan: finalizing write: %w", err)
	}
	return nil
}

// Load reads back the record for (nodeID, service). found is false when
// no attempt has ever been recorded, which is not the same thing as an
// attempt that came back unknown.
func (r *ResultStore) Load(nodeID, service string) (res Result, found bool, err error) {
	data, err := os.ReadFile(r.path(nodeID, service))
	if err != nil {
		if os.IsNotExist(err) {
			return Result{}, false, nil
		}
		return Result{}, false, fmt.Errorf("restartplan: reading restart result: %w", err)
	}
	if err := json.Unmarshal(data, &res); err != nil {
		return Result{}, false, fmt.Errorf("restartplan: parsing restart result: %w", err)
	}
	return res, true, nil
}

// safeFileComponent keeps a node_id or service name usable as a
// filename component without ever letting a caller-supplied string
// escape the store's own directory. Node ids are hostnames in practice,
// but a record key comes from a caller, and a path built from one
// should not depend on that staying true.
func safeFileComponent(s string) string {
	if s == "" {
		return "_"
	}
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}
	out := b.String()
	if strings.HasPrefix(out, ".") {
		return "_" + strings.TrimPrefix(out, ".")
	}
	return out
}
