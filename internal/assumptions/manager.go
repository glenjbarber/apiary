// Package assumptions persists the results of Apiary's Automated
// Assumption Checks (ADR-0055) as a plain JSON file on disk - physical,
// per-node observational data like internal/isostore/internal/hoststats,
// never replicated through raft (a peer-reachability or NAT-uplink
// observation is only ever meaningful to the node that made it).
//
// Persistence is split into two parts, on purpose:
//
//   - a current SNAPSHOT: one Result per Key, its LastObservedAt updated
//     on every tick that actually checked it, even when the value is
//     unchanged. Freshness/staleness must always be computed from this,
//     never from the history journal below.
//   - a history JOURNAL: HistoryEntry records written only on a
//     (ObservedStatus, ReasonCode) transition, or when a caller-supplied
//     heartbeat interval has elapsed since that key's last journal entry
//   - so "500 entries" reflects genuinely new evidence, not tick count.
//
// A file that fails to parse, or whose schema_version this build doesn't
// recognize, is preserved (renamed aside, never overwritten) rather than
// silently treated as empty - see Load.
package assumptions

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// DefaultPath is where the store lives by default on a pkg-installed
// FreeBSD system, alongside internal/isostore/internal/nodeconfig's own
// /var/db/apiary conventions.
const DefaultPath = "/var/db/apiary/assumptions.json"

// currentSchemaVersion gates Load against a future, incompatible file
// format - an unrecognized version is quarantined exactly like a
// genuinely corrupt file, never guessed at.
const currentSchemaVersion = 2

// Status is a four-state result, never collapsed to a boolean.
// NotApplicable is not uncertainty - see the doc comment on that
// constant.
type Status string

const (
	StatusTrue    Status = "true"
	StatusFalse   Status = "false"
	StatusUnknown Status = "unknown"

	// StatusNotApplicable means the check does not apply here at all
	// (e.g. no NAT uplink configured on this node) - a real, positive
	// fact about scope, not an unresolved measurement. Must never be
	// conflated with StatusUnknown by any producer or consumer.
	StatusNotApplicable Status = "not_applicable"
)

// Kind identifies which of the three Automated Assumption Checks (v1,
// ADR-0055) a Key belongs to - a small, stable vocabulary shared between
// internal/assumecheck (the sole producer) and internal/manager (which
// translates Key.Kind to/from the AssumptionKind proto enum).
const (
	KindPeerManagerRPCSucceeded  = "peer_manager_rpc_succeeded"
	KindPeerSecurityPathAccepted = "peer_security_path_accepted"
	KindNATUplinkDefaultRoute    = "nat_uplink_default_route"
	KindReplicaBhyveConfigured   = "replica_bhyve_configured"
	KindReplicaNetworkBridgeUp   = "replica_network_bridge_up"
)

// SubjectKind disambiguates SubjectID's namespace - a VM and a jail can
// share a literal id string, so Key alone (Kind + SubjectID) is not a
// sufficient identity without this.
type SubjectKind string

const (
	// SubjectKindNode means the local node itself is the implicit
	// subject (used by the peer-reachability and NAT-uplink checks,
	// which are about this node's own relationships, not about a VM or
	// jail).
	SubjectKindNode SubjectKind = "node"
	SubjectKindVM   SubjectKind = "vm"

	// SubjectKindJail is reserved for a future check - v1's checks never
	// populate this (assumption (c) is VM-only, see internal/assumecheck).
	SubjectKindJail SubjectKind = "jail"
)

// Key identifies one continuously-tracked assumption. Every field besides
// Kind may legitimately be empty - Key is a struct specifically so
// "empty" is never confused with "absent."
type Key struct {
	Kind             string      `json:"kind"`
	SubjectKind      SubjectKind `json:"subject_kind,omitempty"`
	SubjectID        string      `json:"subject_id,omitempty"`
	DependencyID     string      `json:"dependency_id,omitempty"`
	Qualifier        string      `json:"qualifier,omitempty"`
	ObservedByNodeID string      `json:"observed_by_node_id,omitempty"`
}

// string is an internal, stable encoding used only for map-keying within
// this package - never persisted or exposed.
func (k Key) string() string {
	return strings.Join([]string{
		k.Kind, string(k.SubjectKind), k.SubjectID, k.DependencyID, k.Qualifier, k.ObservedByNodeID,
	}, "\x1f")
}

// Result is the CURRENT-SNAPSHOT record for one Key - see the package
// doc comment. LastObservedAt is genuine per-tick freshness, never
// throttled by a heartbeat interval.
type Result struct {
	Key            Key       `json:"key"`
	ObservedStatus Status    `json:"observed_status"`
	ReasonCode     string    `json:"reason_code,omitempty"`
	Detail         string    `json:"detail,omitempty"`
	LastObservedAt time.Time `json:"last_observed_at"`
}

// HistoryEntry is a JOURNAL record - see the package doc comment.
// RecordedAt is that entry's own time, distinct from (and typically
// older than) the snapshot's LastObservedAt for the same Key.
type HistoryEntry struct {
	Key            Key       `json:"key"`
	ObservedStatus Status    `json:"observed_status"`
	ReasonCode     string    `json:"reason_code,omitempty"`
	Detail         string    `json:"detail,omitempty"`
	RecordedAt     time.Time `json:"recorded_at"`
}

// MaxDetailLen bounds a persisted Detail string - an unbounded transport
// or TLS error string must never turn this file into an unbounded
// diagnostic dump.
const MaxDetailLen = 500

// credentialMarkerRe matches a common credential-bearing prefix
// ("Bearer ", "Authorization:", "api-key=", "token=") followed by its
// value, so ClampDetail can redact the value without needing a full
// secrets-scanning library.
var credentialMarkerRe = regexp.MustCompile(`(?i)(bearer\s+|authorization\s*[:=]\s*(?:bearer\s+)?|api[-_]?key\s*[:=]\s*|token\s*[:=]\s*)\S+`)

// ClampDetail sanitizes free text before it's persisted: strips control
// characters, redacts anything that looks like a credential value, then
// truncates to MaxDetailLen. Never used to determine transition
// identity - see internal/assumecheck's ReasonCode for that.
func ClampDetail(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r == '\n' || r == '\t' || (r >= 0x20 && r != 0x7f) {
			b.WriteRune(r)
		}
	}
	clean := credentialMarkerRe.ReplaceAllString(b.String(), "${1}[REDACTED]")
	if len(clean) > MaxDetailLen {
		clean = clean[:MaxDetailLen] + "...[truncated]"
	}
	return clean
}

// Manager reads/writes the assumptions store. Safe for concurrent use -
// Append is called by a background checker loop on every tick,
// concurrently with Load calls from ListAssumptionResults RPCs, unlike
// internal/nodeconfig's rare, human-triggered Save.
type Manager struct {
	// Path is where the store file is read from/written to. Defaults to
	// DefaultPath if empty.
	Path string

	mu sync.RWMutex

	// storageWarning/storageWarningDetail are recomputed on EVERY Load -
	// not a sticky, one-time latch - so the warning self-clears once an
	// operator removes a quarantined file, with no separate
	// acknowledgement step needed.
	storageWarning       bool
	storageWarningDetail string
}

func (m *Manager) path() string {
	if m.Path == "" {
		return DefaultPath
	}
	return m.Path
}

type fileEnvelope struct {
	SchemaVersion int            `json:"schema_version"`
	Snapshot      []Result       `json:"snapshot"`
	History       []HistoryEntry `json:"history"`
}

// Load returns the current snapshot and the history journal separately.
// A missing file returns nil, nil, nil - not a warning, matching a fresh
// install. A file that fails to parse, or whose schema_version this
// build doesn't recognize, is renamed aside (0600, a timestamp suffix)
// rather than silently replaced; Load then returns an empty, fresh
// state. Every call also globs the store's directory for previously
// quarantined files and a permissions check on the live file, folding
// either into the warning Degraded reports - so a quarantine event from
// an earlier process lifetime, or an insecurely-permissioned file, stays
// visible across a restart with no new corruption.
func (m *Manager) Load() ([]Result, []HistoryEntry, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.loadLocked()
}

func (m *Manager) loadLocked() ([]Result, []HistoryEntry, error) {
	path := m.path()
	var warnings []string

	if info, err := os.Stat(path); err == nil {
		if info.Mode().Perm()&^0o600 != 0 {
			warnings = append(warnings, fmt.Sprintf(
				"assumptions file %s has permissions %#o, wider than the expected 0600", path, info.Mode().Perm()))
		}
	}

	if matches, _ := filepath.Glob(path + ".corrupt-*"); len(matches) > 0 {
		sort.Strings(matches)
		warnings = append(warnings, fmt.Sprintf(
			"%d quarantined corrupt assumptions file(s) present (most recent: %s) - review and remove after investigating",
			len(matches), matches[len(matches)-1]))
	}

	setWarnings := func() {
		m.storageWarning = len(warnings) > 0
		m.storageWarningDetail = strings.Join(warnings, "; ")
	}

	body, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		setWarnings()
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}

	var env fileEnvelope
	parseErr := json.Unmarshal(body, &env)
	if parseErr != nil || env.SchemaVersion != currentSchemaVersion {
		reason := "unparseable content"
		if parseErr == nil {
			reason = fmt.Sprintf("unrecognized schema_version %d (this build understands %d)", env.SchemaVersion, currentSchemaVersion)
		}
		quarantined, qErr := m.quarantine(path)
		if qErr != nil {
			return nil, nil, fmt.Errorf("assumptions: quarantining corrupt file: %w", qErr)
		}
		warnings = append(warnings, fmt.Sprintf(
			"assumptions file was corrupt (%s) - preserved as %s, starting fresh rather than overwriting it", reason, quarantined))
		setWarnings()
		return nil, nil, nil
	}

	setWarnings()
	return env.Snapshot, env.History, nil
}

func (m *Manager) quarantine(path string) (string, error) {
	dest := fmt.Sprintf("%s.corrupt-%d", path, time.Now().UnixNano())
	if err := os.Rename(path, dest); err != nil {
		return "", err
	}
	_ = os.Chmod(dest, 0o600)
	return dest, nil
}

// Degraded reports whether a storage warning is currently active (a
// quarantined file exists, or the live file has unsafe permissions) and
// why - reflects the most recent Load/Append call.
func (m *Manager) Degraded() (bool, string) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.storageWarning, m.storageWarningDetail
}

// Append is called once per checker tick with that tick's freshly
// computed Results (at most one per Key). Every Key present in tick gets
// its snapshot entry unconditionally overwritten - fresh LastObservedAt,
// always, even when the value is unchanged. A HistoryEntry is
// additionally appended for a Key only when (ObservedStatus, ReasonCode)
// changed from the prior snapshot value for that Key (a transition,
// always recorded regardless of Detail churn), or when heartbeatInterval
// has elapsed since that Key's last HistoryEntry. History is then pruned
// per-Key to historyLimit most-recent entries AND to maxAge - both
// bounds apply. Written atomically: a temp file in the same directory,
// fsync'd, renamed over the real path, then the parent directory itself
// fsync'd - durable against a crash mid-write, not just torn-read-safe.
func (m *Manager) Append(tick []Result, heartbeatInterval time.Duration, historyLimit int, maxAge time.Duration) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	snapshot, history, err := m.loadLocked()
	if err != nil {
		return err
	}

	snapIndex := make(map[string]int, len(snapshot))
	for i, r := range snapshot {
		snapIndex[r.Key.string()] = i
	}
	lastHistoryIdx := make(map[string]int)
	for i, h := range history {
		lastHistoryIdx[h.Key.string()] = i
	}

	for _, r := range tick {
		ks := r.Key.string()
		prevIdx, existed := snapIndex[ks]
		var prev Result
		if existed {
			prev = snapshot[prevIdx]
			snapshot[prevIdx] = r
		} else {
			snapshot = append(snapshot, r)
			snapIndex[ks] = len(snapshot) - 1
		}

		transitioned := !existed || prev.ObservedStatus != r.ObservedStatus || prev.ReasonCode != r.ReasonCode

		heartbeatDue := true
		if hIdx, ok := lastHistoryIdx[ks]; ok {
			heartbeatDue = r.LastObservedAt.Sub(history[hIdx].RecordedAt) >= heartbeatInterval
		}

		if transitioned || heartbeatDue {
			history = append(history, HistoryEntry{
				Key: r.Key, ObservedStatus: r.ObservedStatus, ReasonCode: r.ReasonCode,
				Detail: r.Detail, RecordedAt: r.LastObservedAt,
			})
			lastHistoryIdx[ks] = len(history) - 1
		}
	}

	history = pruneHistory(history, historyLimit, maxAge, time.Now())

	return m.writeLocked(snapshot, history)
}

// pruneHistory bounds history per-Key by both count and age - a noisy
// key can't crowd out another key's history, and old entries eventually
// leave regardless of volume.
func pruneHistory(history []HistoryEntry, limit int, maxAge time.Duration, now time.Time) []HistoryEntry {
	byKey := make(map[string][]HistoryEntry)
	var order []string
	for _, h := range history {
		ks := h.Key.string()
		if _, ok := byKey[ks]; !ok {
			order = append(order, ks)
		}
		byKey[ks] = append(byKey[ks], h)
	}

	var out []HistoryEntry
	for _, ks := range order {
		entries := byKey[ks]
		if maxAge > 0 {
			cutoff := now.Add(-maxAge)
			filtered := entries[:0:0]
			for _, e := range entries {
				if e.RecordedAt.After(cutoff) {
					filtered = append(filtered, e)
				}
			}
			entries = filtered
		}
		if limit > 0 && len(entries) > limit {
			entries = entries[len(entries)-limit:]
		}
		out = append(out, entries...)
	}

	sort.SliceStable(out, func(i, j int) bool { return out[i].RecordedAt.Before(out[j].RecordedAt) })
	return out
}

func (m *Manager) writeLocked(snapshot []Result, history []HistoryEntry) error {
	path := m.path()
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("assumptions: creating store directory: %w", err)
	}

	env := fileEnvelope{SchemaVersion: currentSchemaVersion, Snapshot: snapshot, History: history}
	body, err := json.MarshalIndent(env, "", "  ")
	if err != nil {
		return fmt.Errorf("assumptions: marshaling: %w", err)
	}

	tmp, err := os.CreateTemp(dir, ".assumptions-*.tmp")
	if err != nil {
		return fmt.Errorf("assumptions: creating temp file: %w", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath) // no-op once successfully renamed

	if _, err := tmp.Write(body); err != nil {
		tmp.Close()
		return fmt.Errorf("assumptions: writing temp file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("assumptions: syncing temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("assumptions: closing temp file: %w", err)
	}
	if err := os.Chmod(tmpPath, 0o600); err != nil {
		return fmt.Errorf("assumptions: setting permissions: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("assumptions: finalizing write: %w", err)
	}

	if dirFile, err := os.Open(dir); err == nil {
		_ = dirFile.Sync()
		_ = dirFile.Close()
	}

	return nil
}

// LatestPerKey returns the most recent Result per Key from a snapshot
// slice, deterministically sorted. Pure function - used by the RPC
// handler to build the `latest` response field. Since Result is already
// the snapshot's own one-per-key representation, this mainly exists to
// give callers a stable, sorted order rather than depending on the
// store's own file ordering.
func LatestPerKey(results []Result) []Result {
	byKey := make(map[string]Result, len(results))
	for _, r := range results {
		if existing, ok := byKey[r.Key.string()]; !ok || r.LastObservedAt.After(existing.LastObservedAt) {
			byKey[r.Key.string()] = r
		}
	}
	out := make([]Result, 0, len(byKey))
	for _, r := range byKey {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key.string() < out[j].Key.string() })
	return out
}
