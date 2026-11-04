package extnodes

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// DefaultPath is where the enrollment store lives.
//
// It sits beside the join authorization file, which is the other
// root-owned, operator-authored artifact an enrollment depends on. Both
// are secrets-adjacent: this file says WHICH external hosts an operator
// has admitted, and the join authorization file says which host may be
// admitted next.
const DefaultPath = "/usr/local/etc/apiary/external-nodes.json"

// Store holds the enrolled external nodes.
//
// It is LOCAL state, not raft-replicated state, and the reasoning is
// the discovery store's: a provider credential is a per-host secret and
// does not belong in replicated state, and an enrolled external host is
// not a Colony fact in the way a member is.
//
// It is, however, a store of OPERATOR DECISIONS, and it is persisted
// rather than held in memory, because an enrollment that vanished on a
// restart would silently un-admit a host that other Combs were still
// reporting on. That is exactly the kind of drift this store exists to
// make visible.
//
// The file is written atomically, and it is written with mode 0600
// because it records which external hosts are trusted.
type Store struct {
	mu sync.RWMutex

	path string

	// enrollments is keyed by the composite identity.
	enrollments map[Key]*Enrollment

	// statuses holds the current standing of each key, which is
	// separate from the enrollment itself: an enrollment persists
	// across a host's temporary absence.
	statuses map[Key]Status
}

// NewStore returns an empty store that is not backed by a file. It is
// what a managerd uses when no store path is configured, and it is what
// the tests use.
func NewStore() *Store {
	return &Store{
		enrollments: map[Key]*Enrollment{},
		statuses:    map[Key]Status{},
	}
}

// Load reads the store from path.
//
// A MISSING file is not an error: it is a store that has never been
// written, which is the same as an empty store. A file that exists but
// cannot be parsed IS an error, because silently treating an unreadable
// store as empty would make every enrolled host look unenrolled.
func Load(path string) (*Store, error) {
	store := NewStore()
	store.path = path

	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return store, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading external-node store %s: %w", path, err)
	}

	var doc storeDocument
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("parsing external-node store %s: %w", path, err)
	}

	for i := range doc.Enrollments {
		e := &doc.Enrollments[i]
		store.enrollments[e.Key] = e
		store.statuses[e.Key] = StatusEnrolled
	}
	for encoded, status := range doc.Statuses {
		key, ok := parseKey(encoded)
		if !ok {
			// A status for a key that cannot be parsed is a
			// record this build does not understand. It is
			// ignored rather than guessed at: guessing would
			// attach a standing to the wrong host.
			continue
		}
		if _, ok := store.enrollments[key]; ok {
			store.statuses[key] = status
		}
	}
	return store, nil
}

// parseKey reverses Key.String.
//
// The encoding is "provider/source/name". A provider or a node name
// containing a slash would make the encoding ambiguous, so the parse
// splits on the FIRST and LAST slash and takes the middle as the source
// id. A provider or name containing a slash is unusual but not
// impossible, and splitting on the outer slashes is the only reading
// that round-trips.
func parseKey(encoded string) (Key, bool) {
	first := strings.Index(encoded, "/")
	last := strings.LastIndex(encoded, "/")
	if first < 0 || last <= first {
		return Key{}, false
	}
	return Key{
		Provider: encoded[:first],
		SourceID: encoded[first+1 : last],
		Name:     encoded[last+1:],
	}, true
}

// storeDocument is the on-disk shape.
type storeDocument struct {
	// Version is the document format version, so a future format
	// change can be refused rather than misread.
	Version int `json:"version"`

	// Enrollments are the enrolled hosts.
	Enrollments []Enrollment `json:"enrollments"`

	// Statuses carries the per-key standing, for keys whose status is
	// not simply "enrolled".
	Statuses map[string]Status `json:"statuses,omitempty"`
}

// storeVersion is the current document format version.
const storeVersion = 1

// Enrollments returns the enrolled hosts, sorted by their composite
// identity so that a caller rendering them gets a stable order.
func (s *Store) Enrollments() []Enrollment {
	s.mu.RLock()
	defer s.mu.RUnlock()

	out := make([]Enrollment, 0, len(s.enrollments))
	for _, e := range s.enrollments {
		out = append(out, *e)
	}
	sort.Slice(out, func(i, j int) bool {
		return lessKey(out[i].Key, out[j].Key)
	})
	return out
}

// Get returns the enrollment for a key.
func (s *Store) Get(key Key) (Enrollment, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	e, ok := s.enrollments[key]
	if !ok {
		return Enrollment{}, false
	}
	return *e, true
}

// Status returns a key's current standing.
//
// A key that was never enrolled has no standing, which is different
// from every standing here: "not enrolled" is not "enrolled but
// absent".
func (s *Store) Status(key Key) Status {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.statuses[key]
}

// Enroll records an enrollment.
//
// It is IDEMPOTENT with respect to the same authorization pair: enrolling
// the same key twice with the same two authorizations is not an error and
// does not create a second record. That matters because ADR-0191 requires
// enrollment to be a safe thing to retry.
//
// It is NOT idempotent with respect to a DIFFERENT fingerprint under a
// matched key. That is a CONFLICT, and the stored record is left
// untouched, because it means the node was re-keyed or two different
// things now share a name. Failing closed here is the point: a record
// that cannot be placed unambiguously is worse than a missing record,
// because a missing record is visibly missing.
func (s *Store) Enroll(e Enrollment) (Enrollment, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if e.Key.Provider == "" {
		return Enrollment{}, errors.New("refusing to enroll: the provider is empty, and an enrollment with no provider cannot be told apart from any other")
	}
	if e.Key.Name == "" {
		return Enrollment{}, errors.New("refusing to enroll: the node name is empty, and an enrollment with no name identifies nothing")
	}
	if len(e.Authorizations) < 2 {
		return Enrollment{}, fmt.Errorf("refusing to enroll %s: %d authorizing act(s) recorded, and two are required - two distinct API keys, presented to two distinct Colony members", e.Key, len(e.Authorizations))
	}
	first, second := e.Authorizations[0], e.Authorizations[1]
	if first.KeyID == "" || second.KeyID == "" {
		return Enrollment{}, fmt.Errorf("refusing to enroll %s: one of the two authorizing acts carried no API key", e.Key)
	}
	if first.KeyID == second.KeyID {
		return Enrollment{}, fmt.Errorf("refusing to enroll %s: both authorizing acts used the SAME API key (%q). Two DISTINCT keys are required, on two DISTINCT Colony members, so that one credential cannot admit a host on its own", e.Key, first.KeyID)
	}
	if first.NodeID == second.NodeID {
		return Enrollment{}, fmt.Errorf("refusing to enroll %s: both authorizing acts were presented to the SAME Colony member (%s). Two keys on one member is still one operator on one member; the second authorization has to be presented to a different member of the Colony", e.Key, first.NodeID)
	}
	if e.EnrolledAt.IsZero() {
		e.EnrolledAt = time.Now()
	}
	if e.EnrolledBy == "" {
		e.EnrolledBy = "external-discovery"
	}

	known, exists := s.enrollments[e.Key]
	if exists {
		// A fingerprint change under a matched key is a conflict.
		if known.Fingerprint != "" && e.Fingerprint != "" && known.Fingerprint != e.Fingerprint {
			s.statuses[e.Key] = StatusConflict
			return *known, fmt.Errorf("refusing to enroll %s: the fingerprint changed from %q to %q under an identity that already matched. The host may have been re-keyed, or two different hosts may now share a name; the existing enrollment is left exactly as it was", e.Key, known.Fingerprint, e.Fingerprint)
		}
		// The ordinary case: the same host, enrolled again. Keep the
		// original enrollment time and authorizations - the first
		// admission is the fact of record - and refresh the
		// volatile fields.
		known.Status = e.Status
		if e.Fingerprint != "" {
			known.Fingerprint = e.Fingerprint
		}
		s.statuses[e.Key] = StatusEnrolled
		return *known, nil
	}

	record := e
	s.enrollments[e.Key] = &record
	s.statuses[e.Key] = StatusEnrolled
	return record, nil
}

// SetStatus sets a key's standing.
//
// It is used by reconciliation to mark a host absent, and by an
// operator action to unenroll one. A key that was never enrolled cannot
// be given a standing: there is nothing to stand.
func (s *Store) SetStatus(key Key, status Status) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.enrollments[key]; !ok {
		return fmt.Errorf("refusing to set the standing of %s: it is not enrolled, and there is nothing to stand", key)
	}
	s.statuses[key] = status
	return nil
}

// Remove deletes an enrollment.
//
// Removal is an EXPLICIT operator action and never a side-effect of a
// scan. A host that vanished from a provider's listing is marked
// absent, not removed: it is very often a host that is down, and
// deleting the record on that evidence destroys the history that makes
// the absence diagnosable.
func (s *Store) Remove(key Key) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.enrollments[key]; !ok {
		return fmt.Errorf("refusing to remove %s: it is not enrolled", key)
	}
	delete(s.enrollments, key)
	delete(s.statuses, key)
	return nil
}

// Save writes the store to its path atomically.
//
// The file is written with mode 0600 because it records which external
// hosts are trusted. The write goes to a temporary file in the same
// directory and is renamed into place, so a crash mid-write cannot
// leave a half-written store that would read as "no enrollments".
func (s *Store) Save() error {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if s.path == "" {
		return errors.New("refusing to save the external-node store: it has no path")
	}

	doc := storeDocument{
		Version:     storeVersion,
		Enrollments: make([]Enrollment, 0, len(s.enrollments)),
	}
	for _, e := range s.enrollments {
		doc.Enrollments = append(doc.Enrollments, *e)
	}
	sort.Slice(doc.Enrollments, func(i, j int) bool {
		return lessKey(doc.Enrollments[i].Key, doc.Enrollments[j].Key)
	})

	statuses := map[string]Status{}
	for key, status := range s.statuses {
		if status != StatusEnrolled {
			statuses[key.String()] = status
		}
	}
	if len(statuses) > 0 {
		doc.Statuses = statuses
	}

	body, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding the external-node store: %w", err)
	}
	body = append(body, '\n')

	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("creating %s: %w", dir, err)
	}

	tmp, err := os.CreateTemp(dir, ".external-nodes-*.tmp")
	if err != nil {
		return fmt.Errorf("creating a temporary file in %s: %w", dir, err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)

	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return fmt.Errorf("setting the mode on %s: %w", tmpName, err)
	}
	if _, err := tmp.Write(body); err != nil {
		tmp.Close()
		return fmt.Errorf("writing %s: %w", tmpName, err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("flushing %s: %w", tmpName, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("closing %s: %w", tmpName, err)
	}
	if err := os.Rename(tmpName, s.path); err != nil {
		return fmt.Errorf("renaming %s into place at %s: %w", tmpName, s.path, err)
	}
	return nil
}

// Path returns the store's file path, empty for an in-memory store.
func (s *Store) Path() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.path
}

// lessKey orders keys by their composite identity.
func lessKey(a, b Key) bool {
	if a.Provider != b.Provider {
		return a.Provider < b.Provider
	}
	if a.SourceID != b.SourceID {
		return a.SourceID < b.SourceID
	}
	return a.Name < b.Name
}
