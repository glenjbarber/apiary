package zfs

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// DefaultTokenDir is where a target node persists the resume token for
// an interrupted receive. ADR-0130 is explicit that the token is
// target-local state and the target's own source of truth: it is never
// required from raft, never the source's responsibility, and never an
// input the source can be tricked into supplying.
const DefaultTokenDir = "/var/db/apiary/replication"

// TokenStore persists a replication resume token per policy id, on the
// node that observed it.
//
// The store has no notion of "empty means current". Load returns a
// TokenRecord whose State is one of the four ResumeTokenState values,
// so a caller that forgot to check cannot accidentally treat "no
// stored token" as "the receive finished cleanly" — that conflation is
// the failure this whole file exists to prevent.
type TokenStore interface {
	// Load returns the record stored for policyID. A policy with no
	// stored record is NOT an error: it returns a record with
	// State ResumeTokenNone and no token.
	Load(policyID string) (TokenRecord, error)

	// Save persists token for policyID, replacing any previous value.
	Save(policyID, token string) error

	// Delete removes any stored record. Deleting a record that is not
	// there is not an error.
	Delete(policyID string) error
}

// TokenRecord is the local view of one policy's resume-token state.
type TokenRecord struct {
	PolicyID string
	// State is ResumeTokenLive when Token is set, ResumeTokenNone when
	// the store is known to hold nothing, and ResumeTokenStale when the
	// store held a token that a live observation has since contradicted.
	State ResumeTokenState
	// Token is the persisted token, set only when State is
	// ResumeTokenLive.
	Token string
	// SavedUnix is when the token was persisted, for evidence text.
	SavedUnix int64
}

// ErrTokenPolicyID is returned for a policy id that could not be used
// as a filename. Rejecting it matters: the id becomes a path component
// under a root-owned directory, so a traversal here would be a
// filesystem escape.
var ErrTokenPolicyID = errors.New("zfs: invalid replication policy id")

// validateTokenPolicyID restricts a policy id to the shape an Apiary
// id actually has, so it can never escape the token directory.
func validateTokenPolicyID(policyID string) error {
	if policyID == "" || len(policyID) > 128 {
		return fmt.Errorf("%w: %q", ErrTokenPolicyID, policyID)
	}
	for _, r := range policyID {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '-', r == '_', r == '.':
		default:
			return fmt.Errorf("%w: %q contains %q, which is not allowed", ErrTokenPolicyID, policyID, r)
		}
	}
	if strings.HasPrefix(policyID, ".") {
		return fmt.Errorf("%w: %q", ErrTokenPolicyID, policyID)
	}
	return nil
}

// FileTokenStore is the production TokenStore: one file per policy,
// `<dir>/<policy-id>.token`, mode 0600 in a 0700 directory, matching
// the root-only secret-file handling internal/raftdconfig already does
// for its internal token.
//
// A token is opaque zfs(8) state describing an in-flight receive, not a
// secret key, but it is still write authority over a half-received
// dataset: anyone holding it can complete a receive onto that
// destination. 0600 in a 0700 directory is the cost of that.
type FileTokenStore struct {
	Dir string
}

// NewFileTokenStore returns a store rooted at dir, or at
// DefaultTokenDir when dir is empty.
func NewFileTokenStore(dir string) *FileTokenStore {
	if dir == "" {
		dir = DefaultTokenDir
	}
	return &FileTokenStore{Dir: dir}
}

func (s *FileTokenStore) path(policyID string) (string, error) {
	if err := validateTokenPolicyID(policyID); err != nil {
		return "", err
	}
	return filepath.Join(s.Dir, policyID+".token"), nil
}

// Load reads the stored record. A missing file is the confirmed
// "nothing stored" case and is reported as ResumeTokenNone with no
// error. A read that fails for any other reason is UNKNOWN, not
// absence: the store may well hold a token this process simply could
// not read, and treating an unreadable store as an empty one would
// silently drop the resume path and restart a full receive.
func (s *FileTokenStore) Load(policyID string) (TokenRecord, error) {
	path, err := s.path(policyID)
	if err != nil {
		return TokenRecord{}, err
	}
	rec := TokenRecord{PolicyID: policyID, State: ResumeTokenNone}

	info, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return rec, nil
		}
		return TokenRecord{PolicyID: policyID, State: ResumeTokenUnknown},
			&UnobservedError{Op: "stat " + path, Detail: "the local resume token store could not be read", Err: err}
	}
	// The saved time is the file's own mtime rather than anything this
	// process remembers, so it stays correct for a token written by an
	// earlier run, a restored backup, or another tool.
	rec.SavedUnix = info.ModTime().Unix()

	data, err := os.ReadFile(path)
	if err != nil {
		return TokenRecord{PolicyID: policyID, State: ResumeTokenUnknown, SavedUnix: rec.SavedUnix},
			&UnobservedError{Op: "read " + path, Detail: "the local resume token store exists but could not be read", Err: err}
	}
	token := strings.TrimSpace(string(data))
	if token == "" {
		// An empty file is a corrupt store, not a confirmed absence:
		// something wrote the file and the value did not survive.
		return TokenRecord{PolicyID: policyID, State: ResumeTokenUnknown, SavedUnix: rec.SavedUnix},
			&UnobservedError{Op: "read " + path, Detail: "the local resume token store holds an empty token, so the stored resume state is undetermined"}
	}
	rec.State = ResumeTokenLive
	rec.Token = token
	return rec, nil
}

// Save writes token to `<dir>/<policy-id>.token`, creating the 0700
// directory if needed, and writes through a temporary file in the same
// directory so a crash mid-write cannot leave a truncated token that
// would later be fed to `zfs send -t`. The write is fsync'd for the
// same reason.
func (s *FileTokenStore) Save(policyID, token string) error {
	path, err := s.path(policyID)
	if err != nil {
		return err
	}
	token = strings.TrimSpace(token)
	if token == "" {
		return fmt.Errorf("zfs: refusing to persist an empty resume token for policy %q", policyID)
	}
	if !isPlausibleToken(token) {
		return fmt.Errorf("zfs: refusing to persist a resume token for policy %q that is not a plausible single-line token", policyID)
	}
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		return fmt.Errorf("zfs: creating resume token directory %s: %w", s.Dir, err)
	}
	tmp, err := os.CreateTemp(s.Dir, policyID+".token.tmp*")
	if err != nil {
		return fmt.Errorf("zfs: creating temp resume token file in %s: %w", s.Dir, err)
	}
	tmpName := tmp.Name()
	cleanup := func() {
		tmp.Close()
		os.Remove(tmpName)
	}
	if err := tmp.Chmod(0o600); err != nil {
		cleanup()
		return fmt.Errorf("zfs: setting mode on %s: %w", tmpName, err)
	}
	if _, err := tmp.WriteString(token + "\n"); err != nil {
		cleanup()
		return fmt.Errorf("zfs: writing %s: %w", tmpName, err)
	}
	if err := tmp.Sync(); err != nil {
		cleanup()
		return fmt.Errorf("zfs: syncing %s: %w", tmpName, err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("zfs: closing %s: %w", tmpName, err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("zfs: installing %s: %w", path, err)
	}
	return nil
}

// Delete removes the stored record. A record that is not there is not
// an error, so a cleanup path is idempotent.
//
// A directory at the record's path is refused rather than removed:
// "delete the stored token" and "remove a directory" are different
// operations, and os.Remove would quietly do the second while the
// caller believed it had done the first.
func (s *FileTokenStore) Delete(policyID string) error {
	path, err := s.path(policyID)
	if err != nil {
		return err
	}
	if info, err := os.Lstat(path); err == nil && info.IsDir() {
		return fmt.Errorf("zfs: refusing to remove %s: it is a directory, not a stored resume token", path)
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("zfs: removing resume token %s: %w", path, err)
	}
	return nil
}

// CombinedTokenObservation is the reconciled answer to "can this policy
// resume, and if not, why" — the local store's view crossed with the
// live destination's own report.
//
// The cross is not a sum. The live destination is authoritative about
// whether a receive is actually in progress, because only zfs(8) knows
// that; the local store is authoritative about what this node
// previously saw, which is what makes a "stale" state distinguishable
// from a "never existed" state. Neither is allowed to imply the other.
type CombinedTokenObservation struct {
	PolicyID string
	// State is the reconciled state.
	State ResumeTokenState
	// Token is the token to hand to `zfs send -t`, set only when State
	// is ResumeTokenLive.
	Token string
	// Live is the destination's own observation, verbatim.
	Live ResumeTokenObservation
	// Stored is the local store's record, verbatim.
	Stored TokenRecord
	// Detail is one operator-readable line explaining State.
	Detail string
}

// ObserveToken reconciles a live destination observation with a stored
// local record into one answer.
//
// The cases, and why each one is what it is:
//
//   - Live token, matching store -> ResumeTokenLive. The token is
//     usable and the store agrees.
//   - Live token, store empty or different -> ResumeTokenLive using the
//     LIVE token. zfs(8) is authoritative; a store that was empty
//     because the previous process died before persisting is a fact
//     about this node's bookkeeping, not about the dataset.
//   - No live token, store holds one -> ResumeTokenStale. The
//     destination says the receive is not in progress while this node
//     remembers one, so the token is dead (the destination was
//     destroyed, or the token expired) and the store should be cleared.
//   - No live token, store empty -> ResumeTokenNone. A confirmed clean
//     destination with nothing remembered.
//   - Either side unknown -> ResumeTokenUnknown. A contradiction that
//     cannot be resolved, and a genuinely unanswered question, both
//     stay unknown. The caller must not resume on unknown and must not
//     clear the store on unknown.
func ObserveToken(policyID string, live ResumeTokenObservation, stored TokenRecord) CombinedTokenObservation {
	combined := CombinedTokenObservation{
		PolicyID: policyID,
		Live:     live,
		Stored:   stored,
	}
	switch {
	case live.State == ResumeTokenUnknown || stored.State == ResumeTokenUnknown:
		combined.State = ResumeTokenUnknown
		combined.Detail = "the resume state of this destination could not be established, so it is unknown whether a receive is in progress: " + firstNonEmpty(live.Detail, storedTokenDetail(stored))
	case live.Live():
		combined.State = ResumeTokenLive
		combined.Token = live.Token
		if stored.State == ResumeTokenLive && stored.Token == live.Token {
			combined.Detail = "the destination reports a live receive_resume_token, matching the token this node stored"
		} else if stored.State == ResumeTokenLive {
			combined.Detail = "the destination reports a live receive_resume_token that differs from the token this node stored; the destination's own token wins"
		} else {
			combined.Detail = "the destination reports a live receive_resume_token that this node had not stored; the destination's own token wins"
		}
	case stored.State == ResumeTokenLive:
		combined.State = ResumeTokenStale
		combined.Token = stored.Token
		combined.Detail = fmt.Sprintf("this node stored a resume token but the destination now reports no receive in progress (%s): the token is stale and must not be used", firstNonEmpty(live.Detail, "zfs reported -"))
	default:
		combined.State = ResumeTokenNone
		combined.Detail = "the destination reports no receive in progress and this node has no stored token: there is nothing to resume"
	}
	return combined
}

// Usable reports whether Token holds a token that may be handed to
// `zfs send -t`. It is false for unknown, because "we could not find
// out" is not "there is nothing to find".
func (c CombinedTokenObservation) Usable() bool { return c.State == ResumeTokenLive }

// ObserveTokenFor is the Manager-side convenience: observe the
// destination's live token, load the local store, and reconcile the
// two. The store is consulted even when the live observation fails, so
// the operator still sees what this node remembered.
func (m *Manager) ObserveTokenFor(ctx context.Context, store TokenStore, policyID, destName string) (CombinedTokenObservation, error) {
	live, liveErr := m.ResumeToken(ctx, destName)

	stored := TokenRecord{PolicyID: policyID, State: ResumeTokenNone}
	var storeErr error
	if store != nil {
		stored, storeErr = store.Load(policyID)
	}
	combined := ObserveToken(policyID, live, stored)
	// Report the most meaningful of the two errors without collapsing
	// either: an unobserved answer stays an unobserved answer in
	// CombinedTokenObservation.State, whatever the returned error is.
	if liveErr != nil {
		return combined, liveErr
	}
	return combined, storeErr
}

func storedTokenDetail(stored TokenRecord) string {
	if stored.State == ResumeTokenLive {
		return "this node holds a stored token, whose validity could not be checked"
	}
	return "this node's store could not be read"
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
