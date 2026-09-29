// Package joinauth is ADR-0147 Part 3's authorization store: the
// root-owned file that records which Comb an operator has authorized to
// join this Colony, and under which TLS fingerprint.
//
// # Why a file and not a key
//
// The authority boundary is file OWNERSHIP, not a secret. An Admin-tier
// caller can already do a great deal through the manager API -
// UpdateNodeConfig/UpdateFrontendConfig/UpdateRestshimdConfig write
// files as root, RestartNodeService restarts services from a fixed
// allowlist, ConvertStandaloneToJoiner can reach `raftd -reset`. What
// such a caller cannot do is read or write an arbitrary root-owned
// file, or run an arbitrary command. That gap is the entire basis of
// Part 3, and it is why the mechanism needs no cryptography, no
// rotation story, and nothing to leak.
//
// # Why no RPC writes it
//
// Nothing in api/rpc names this file, and nothing in api/rpc ever
// will. The three Update*Config handlers write known configuration
// files and this is not one of them: there is no proto field that
// carries a path to it, so there is nothing for an Admin to aim at.
// internal/manager/joinauthorization_test.go enforces that
// mechanically by walking the ManagerService descriptor rather than
// trusting a review to keep noticing.
//
// # Single use is recorded elsewhere
//
// This file is the INPUT, not the record. Only the leader reads it, so
// a copy on another Comb may predate the approval that spent an entry
// and would otherwise authorize the same join twice. The record of use
// is replicated: PendingJoinRequest.authorization_id and
// .consumed_at_unix, written by the same raft command that approves.
// See internal/manager/joinauthorization.go.
//
// # What it deliberately does not stop
//
// An Admin can delete an entry, refuse to forward, or withhold the
// file entirely. That is denial, and denial is survivable and
// diagnosable. The asymmetry is the point: the operator can admit a
// Comb, and an Admin cannot quietly admit one.
package joinauth

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/glenjbarber/apiary/internal/jsonstrict"
)

// DefaultPath is where the store lives on a pkg-installed FreeBSD
// system: inside the same root-owned 0700 directory as every daemon's
// own config, and deliberately NOT one of those config files. It is
// created empty and 0600 by `apiaryctl install`, so a fresh Comb has
// the store before it needs it, and its absence on an un-migrated Comb
// is a refusal that names this exact path rather than a silent
// "nothing is authorized".
const DefaultPath = "/usr/local/etc/apiary/join-authorizations.json"

// DefaultTTL is how long a freshly created entry lasts. The owner
// answered ADR-0147's own question 1 with 24 hours: long enough for an
// operator to walk to the leader Comb and type one command, short
// enough that a forgotten entry does not sit in a file indefinitely
// authorizing whatever Comb later claims the identity it names.
const DefaultTTL = 24 * time.Hour

// Entry is one single-use authorization.
//
// NodeID and Fingerprint are matched EXACTLY against the pending join
// request. There is no wildcard and no prefix match: an entry that
// names a Comb must also name the exact certificate that Comb
// presented, so an entry written for one joining Comb cannot be
// re-pointed at another by editing the request rather than the file.
type Entry struct {
	// ID is a random, non-secret identifier ("auth-" plus 16 hex
	// characters). It is what gets recorded as
	// PendingJoinRequest.authorization_id, and it is what the
	// replicated record is looked up by, so it must be unique across
	// the file rather than merely unlikely to repeat.
	ID string `json:"id"`

	// NodeID is the joining Comb's raft identity, exactly as
	// RequestJoinColony recorded it on the pending request.
	NodeID string `json:"node_id"`

	// Fingerprint is that Comb's managerd TLS certificate fingerprint
	// in the "SHA256:AA:BB:..." shape internal/manager's own
	// tlsCertFingerprint produces. Compared case-insensitively after
	// normalization, because it is a value an operator reads off a
	// screen and retypes.
	Fingerprint string `json:"fingerprint"`

	// ExpiresAtUnix is the absolute deadline. Absolute, not relative,
	// for the same reason the join window's is: it has to mean the same
	// instant on every Comb that reads the file, and it has to survive
	// a copy of the file being made hours apart.
	ExpiresAtUnix int64 `json:"expires_at_unix"`
}

// Store is the whole file. A named field rather than a bare array so
// the format can grow (a "consumed" marker, a created-at stamp)
// without a future binary having to guess whether a bare array means
// the old shape or the new one.
type Store struct {
	Authorizations []Entry `json:"authorizations"`
}

// NewEntryID returns a fresh random identifier in this package's own
// "auth-" shape. It is not a secret and carries no entropy the entry's
// security depends on: the security of an entry is that creating one
// requires root, not that its id is unguessable.
func NewEntryID() (string, error) {
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return "auth-" + hex.EncodeToString(buf), nil
}

// Load reads the store at path.
//
// A MISSING file is an error naming the path, not an empty store. The
// difference matters: "no file" means this Comb was never installed, or
// the file was deleted, and telling an operator "no entries, so
// nothing is authorized" would send them looking for the wrong problem
// while the Colony is in fact unconfigured. The caller has to be able
// to tell those apart, so it is an error with a path in it.
//
// A MALFORMED file is likewise an error naming the path. It does not
// fall back to "no entries" for exactly the same reason, and for one
// more: a partially-written file that read as empty would authorize
// nothing, which is safe, but a partially-written file that read as
// SOME entries would authorize something nobody chose. Refusing both is
// the only reading that cannot be a coincidence.
//
// Unknown fields are refused rather than ignored, the same rule
// internal/hostinstall applies to every config file it reads: a
// hand-edited file carrying a field this binary does not know is a file
// this binary would silently drop the next time it wrote one.
func Load(path string) (Store, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return Store{}, fmt.Errorf("joinauth: %s does not exist - no Comb has been authorized to join on this one, and `apiaryctl install` creates this file empty", path)
		}
		return Store{}, fmt.Errorf("joinauth: reading %s: %w", path, err)
	}
	// A duplicated key is refused BEFORE decoding, and it is the check
	// that matters most here. This file is written by hand far more
	// often than it is written by apiaryctl, and encoding/json resolves
	// a duplicate by taking the last one - silently, with no error. An
	// operator who pastes an entry, changes their mind, and pastes
	// another gets a file that parses perfectly and authorizes the
	// SECOND identity, which is precisely the outcome a person editing
	// an authorization store is most likely to believe they prevented.
	// internal/jsonstrict exists for exactly this and is used by every
	// other hand-edited config in this codebase.
	if err := jsonstrict.RejectDuplicateKeys(data); err != nil {
		return Store{}, fmt.Errorf("joinauth: parsing %s: %w", path, err)
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var store Store
	if err := dec.Decode(&store); err != nil {
		return Store{}, fmt.Errorf("joinauth: parsing %s: %w", path, err)
	}
	// Decode stops at the end of the first JSON value and reports no
	// error for whatever follows it, so `{"authorizations": []} oops`
	// would otherwise load as an EMPTY store: a refusal that looked
	// like a success with nothing in it, on a file whose real contents
	// nobody can now read. Trailing content is refused instead.
	if dec.More() {
		return Store{}, fmt.Errorf("joinauth: parsing %s: there is more content after the end of the JSON object, so the file is not a single store and will not be read as one", path)
	}
	return store, nil
}

// loadForWrite is Load for the one caller that is about to CREATE the
// file: a missing store is an empty store, because the next thing that
// happens is a write that creates it.
//
// It is deliberately not Load. Load's missing-is-an-error rule exists
// for the reader - managerd deciding whether a join may be approved -
// where "no file" and "a file with nothing in it" are different
// diagnoses and an operator needs to be told which they have. For the
// writer they are the same thing, and refusing there would mean a Comb
// installed before this feature existed could never be authorized
// without first being re-installed, which is a worse answer than
// simply creating the file.
//
// A MALFORMED file is still refused here, by exactly the same parse.
// Creating a new store over one that exists but cannot be read would
// destroy an operator's deliberate edits to replace them with an empty
// file, and the message for that has to name the path.
func loadForWrite(path string) (Store, error) {
	store, err := Load(path)
	if err == nil {
		return store, nil
	}
	if _, statErr := os.Stat(path); statErr == nil || !os.IsNotExist(statErr) {
		return Store{}, err
	}
	return Store{}, nil
}

// Save writes the store to path atomically at mode 0600, replacing
// whatever was there before in full. The atomic-then-rename and the
// explicit chmod are internal/commonconfig.Save's own, repeated here
// rather than shared, for the same reason that package repeats
// internal/nodeconfig's: the mode argument os.WriteFile takes only
// applies on CREATION, so a plain write to an existing file leaves it
// at whatever mode it already had, and this file's whole authority
// claim is that it is not writable by anyone but root.
//
// The bytes are written sorted by id, so a hand-inspected file has a
// stable order and two Combs whose files hold the same entries produce
// the same bytes - which is what makes a diff between them meaningful
// to whoever is diagnosing a stale copy.
func (s Store) Save(path string) error {
	entries := make([]Entry, len(s.Authorizations))
	copy(entries, s.Authorizations)
	sort.Slice(entries, func(i, j int) bool { return entries[i].ID < entries[j].ID })
	body, err := json.MarshalIndent(Store{Authorizations: entries}, "", "  ")
	if err != nil {
		return fmt.Errorf("joinauth: marshalling %s: %w", path, err)
	}
	body = append(body, '\n')

	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".join-authorizations-*.tmp")
	if err != nil {
		return fmt.Errorf("joinauth: creating temp file: %w", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath) // no-op once renamed into place
	if _, err := tmp.Write(body); err != nil {
		tmp.Close()
		return fmt.Errorf("joinauth: writing temp file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("joinauth: syncing temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("joinauth: closing temp file: %w", err)
	}
	if err := os.Chmod(tmpPath, 0o600); err != nil {
		return fmt.Errorf("joinauth: setting permissions: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("joinauth: renaming into place: %w", err)
	}
	return nil
}

// Refusal explains why no entry in the store authorizes a join. It is
// a distinct type rather than a bare error string so the caller can
// tell the cases apart without matching on prose - the manager layer
// names different next steps for each, and matching on a string here
// would be exactly the drift internal/manager's own
// IsColonyWindowClosedRefusal exists to avoid.
type Refusal struct {
	// Reason is a stable, machine-readable token: "no-file",
	// "unreadable", "malformed", "no-entry", "fingerprint-mismatch",
	// "expired", "duplicate-id", or "no-slots-left".
	Reason string

	// Detail is the operator-facing sentence, already carrying the
	// values that identify the entry so the message can be pasted into
	// `apiaryctl join-authorize` unchanged.
	Detail string
}

func (r *Refusal) Error() string { return r.Detail }

// Find returns the entry that authorizes nodeID presenting fingerprint,
// and a *Refusal naming precisely what went wrong if none does.
//
// now is a parameter rather than a clock read inside, for the same
// reason the FSM takes every timestamp as a command input: the caller
// is the authority on what time it is, and a test that cannot fix the
// clock cannot test an expiry.
//
// The five refusals are deliberately distinct messages rather than one
// "not authorized", because each names a different next step and an
// operator who cannot tell them apart disables the check rather than
// doing the right thing:
//
//   - no-file / unreadable / malformed: the store itself is the
//     problem, and the answer is `apiaryctl install`.
//   - no-entry: nothing has been authorized for this node_id at all.
//   - fingerprint-mismatch: an entry exists for this node_id but names
//     a different certificate - either a stale entry from an earlier
//     attempt, or a different Comb claiming the identity. Both the
//     wanted and the offered fingerprints are named, because comparing
//     them is the operator's job.
//   - expired: the entry was created and has since aged out. Named with
//     its expiry so "create another" is obviously the answer.
func (s Store) Find(nodeID, fingerprint string, now time.Time) (Entry, *Refusal) {
	// The whole file is examined before anything is refused, and the
	// order of the questions matters: an EXACT match wins, then a
	// same-identity entry with a different certificate, then nothing.
	//
	// Scanning for the exact match FIRST is not tidiness. A store can
	// legitimately hold an entry for a Comb's previous certificate
	// alongside one for its current one, and a Comb that has been
	// re-imaged does exactly that. Refusing on the first
	// same-identity entry that does not match would report a
	// fingerprint mismatch for a join the operator has in fact already
	// authorized - pointing them at the stale entry and away from the
	// one that works.
	want := NormalizeFingerprint(fingerprint)
	for _, e := range s.Authorizations {
		if e.NodeID != nodeID || NormalizeFingerprint(e.Fingerprint) != want {
			continue
		}
		if now.Unix() >= e.ExpiresAtUnix {
			return Entry{}, &Refusal{
				Reason: "expired",
				Detail: fmt.Sprintf(
					"the authorization entry %q for node_id %q expired at %s; create another one",
					e.ID, nodeID, time.Unix(e.ExpiresAtUnix, 0).Format(time.RFC3339)),
			}
		}
		return e, nil
	}
	for _, e := range s.Authorizations {
		if e.NodeID != nodeID {
			continue
		}
		return Entry{}, &Refusal{
			Reason: "fingerprint-mismatch",
			Detail: fmt.Sprintf(
				"the authorization file holds entry %q for node_id %q naming certificate fingerprint %q, but this join request presented %q; the entry has to name the exact certificate, so it is not a match",
				e.ID, nodeID, e.Fingerprint, fingerprint),
		}
	}
	return Entry{}, &Refusal{
		Reason: "no-entry",
		Detail: fmt.Sprintf(
			"no authorization entry in the file names node_id %q with certificate fingerprint %q",
			nodeID, fingerprint),
	}
}

// NormalizeFingerprint puts a fingerprint in the one form comparisons
// are made in: "sha256:" followed by colon-separated UPPERCASE hex.
//
// The joining Comb emits "SHA256:" plus uppercase hex, but the value
// reaches a human through a web page and reaches this comparison
// through `apiaryctl join-authorize --fingerprint`, so it arrives in
// whatever case and prefix spelling the operator's terminal produced.
// Normalizing is what makes "the same certificate" a property of the
// certificate rather than of how it was typed.
func NormalizeFingerprint(s string) string {
	s = strings.TrimSpace(s)
	if len(s) < 7 || !strings.EqualFold(s[:7], "sha256:") {
		return strings.ToUpper(s)
	}
	return "SHA256:" + strings.ToUpper(s[7:])
}
