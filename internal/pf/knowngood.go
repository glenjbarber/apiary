package pf

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Last-known-good: the exact ruleset this node last loaded into an
// anchor, kept on this node's own disk.
//
// Why it exists at all, in one line: `Manager.Apply` used to write a
// ruleset and keep no record of it, so when pf's running state
// disagreed with what Apiary thought it had loaded - a pfctl load that
// only partly took, a hand edit to an anchor, a reboot that dropped
// something - there was no baseline to notice the disagreement against
// and nothing to fall back to. This file is the baseline.
//
// Where it lives is a deliberate, and non-negotiable, choice: node-
// local disk, never Raft. This is the same line `state.proto`'s own
// header draws - replicated state is small JSON-shaped *facts* (VMs,
// networks, intents), while physical state (ZFS datasets, HAST
// bytes, and what one kernel's packet filter is actually enforcing) is
// per-node observation and never goes through raft. Putting a pf
// ruleset in the log would mean a permanent, physically divergent
// value in a replicated state machine, and would make every node's
// load-failure a Colony-wide commit. ADR-0129 makes the same call for
// its host-scope `known-good.rules`, and the two are deliberately
// separate: that file is the rollback *source* for a staged host
// policy, this one is the *evidence* baseline for every anchor this
// package loads, host or guest.
//
// Format, in full:
//
//	apiary-pf-known-good v1
//	anchor apiary/vm-<id>
//	recorded 2026-09-26T19:20:00Z
//	sha256 <hex digest of the body below>
//	<body, one rule per line, possibly empty>
//
// The checksum is what makes "corrupt" a state rather than a guess: a
// truncated write, a half-typed `cat >`, or a disk that gave up
// mid-sector is detected rather than loaded back as fact. Write is
// temp file -> fsync -> rename -> fsync of the containing directory,
// the standard atomic-replace dance, because a ruleset file that
// exists but is missing its last rule is worse than no file at all.
const (
	knownGoodMagic   = "apiary-pf-known-good v1"
	knownGoodDirMode = 0o700
	knownGoodMode    = 0o600
)

// KnownGoodState is the four-way answer to "does this node have a
// record of what it last loaded into this anchor?".
//
// Each value is a distinct fact. None of them means "nothing to do",
// and in particular `Corrupt` must never be collapsed into `Absent`:
// an absent record means we have never successfully loaded this anchor
// (or have flushed it, which is the same thing), while a corrupt record
// means we believed we had a baseline and cannot read it, so the
// running ruleset is unexplained.
type KnownGoodState string

const (
	// KnownGoodLoaded: a well-formed, non-empty record exists. This is
	// the only state that provides a comparison baseline.
	KnownGoodLoaded KnownGoodState = "loaded"

	// KnownGoodEmpty: a well-formed record exists, and the ruleset it
	// holds is empty - the last successful load loaded zero rules,
	// which in this package's convention means "everything allowed"
	// (ADR-0022's fail-open default). This is NOT the same as
	// KnownGoodAbsent: an empty record is a positive observation of a
	// successful load, and comparing the running ruleset against it is
	// meaningful (an anchor that has since gained a rule is drift).
	KnownGoodEmpty KnownGoodState = "empty"

	// KnownGoodAbsent: no record exists. Either this anchor was never
	// successfully loaded by this package, or it was flushed
	// (Flush removes the record, because a flushed anchor's
	// last-known-good ruleset *is* the empty one and re-recording that
	// would be indistinguishable from never having loaded anything).
	KnownGoodAbsent KnownGoodState = "absent"

	// KnownGoodCorrupt: a record exists but does not parse or does not
	// match its own checksum. Reported as a state with a reason, not as
	// a successful load and not as a hard error, so that no caller can
	// accidentally treat it as either.
	KnownGoodCorrupt KnownGoodState = "corrupt"
)

// KnownGood is one last-known-good record, or the reason there isn't
// one.
type KnownGood struct {
	Anchor     string // the anchor this record is about
	State      KnownGoodState
	Body       string    // the rendered ruleset, valid only in KnownGoodLoaded/KnownGoodEmpty
	RecordedAt time.Time // when the load happened; zero in every other state
	Detail     string    // why the record is corrupt/empty; "" otherwise
}

// knownGoodStore is the on-disk half of the last-known-good model: one
// directory, one file per anchor, named after the anchor so two anchors
// can never overwrite each other's baseline. Anchor names already form
// a hierarchy ("apiary/vm-<id>"), and mirroring that hierarchy on disk
// keeps the mapping obvious: /var/db/apiary/pf/apiary/vm-<id>.rules.
// It looks redundant and is - deliberately. The cost is a nested
// directory, and the benefit is that no sanitising scheme of our own
// invention stands between an anchor name and its file, where a
// collision or a path escape could quietly give one anchor another's
// baseline.
type knownGoodStore struct{ dir string }

func (s knownGoodStore) path(anchor string) (string, error) {
	if err := validateAnchor(anchor); err != nil {
		return "", err
	}
	return filepath.Join(s.dir, filepath.FromSlash(anchor)+".rules"), nil
}

// validateAnchor rejects an anchor name that could escape the store's
// directory or name a file it should not. Anchor names are built by
// this project (cluster's vmAnchor/natAnchor, jailnet's Anchor), so
// this is defence in depth rather than a defence against a known
// attacker - but the cost is nil and the failure mode without it (a
// record for "../etc/pf.conf") is not.
func validateAnchor(anchor string) error {
	switch {
	case anchor == "":
		return fmt.Errorf("pf: empty anchor name")
	case strings.HasPrefix(anchor, "/"):
		return fmt.Errorf("pf: anchor %q must be relative", anchor)
	case strings.ContainsAny(anchor, "\n\r\x00"):
		return fmt.Errorf("pf: anchor %q contains a control character", anchor)
	}
	for _, part := range strings.Split(anchor, "/") {
		switch part {
		case "":
			return fmt.Errorf("pf: anchor %q has an empty path element", anchor)
		case ".", "..":
			return fmt.Errorf("pf: anchor %q has a %q path element, which could escape the last-known-good directory", anchor, part)
		}
	}
	return nil
}

// record writes body as anchor's last-known-good ruleset, atomically.
//
// It is called only after a load has actually succeeded, so the file
// can never claim a ruleset pf did not take. A record whose body is
// already on disk and byte-identical is not rewritten: Apply runs once
// per anchor per reconcile tick and there is no reason to fsync the
// same answer thousands of times a day.
func (s knownGoodStore) record(anchor, body string) error {
	path, err := s.path(anchor)
	if err != nil {
		return err
	}
	if existing, state, err := s.load(anchor); err == nil {
		if (state == KnownGoodLoaded || state == KnownGoodEmpty) && existing.Body == body {
			return nil
		}
	}

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, knownGoodDirMode); err != nil {
		return fmt.Errorf("creating %s: %w", dir, err)
	}

	digest := sha256.Sum256([]byte(body))
	content := fmt.Sprintf("%s\nanchor %s\nrecorded %s\nsha256 %s\n\n%s",
		knownGoodMagic, anchor, time.Now().UTC().Format(time.RFC3339), hex.EncodeToString(digest[:]), body)

	// Temp file in the *same* directory, so the rename below is a
	// same-filesystem atomic replace rather than a copy that can be
	// interrupted half done.
	tmp, err := os.CreateTemp(dir, ".known-good-*.tmp")
	if err != nil {
		return fmt.Errorf("creating a temp file in %s: %w", dir, err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op once the rename below succeeds

	if _, err := tmp.WriteString(content); err != nil {
		tmp.Close()
		return fmt.Errorf("writing %s: %w", tmpName, err)
	}
	// The data must be on the platter before the rename, or a crash
	// between the two leaves a complete-looking file full of zeroes.
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("fsyncing %s: %w", tmpName, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("closing %s: %w", tmpName, err)
	}
	if err := os.Chmod(tmpName, knownGoodMode); err != nil {
		return fmt.Errorf("chmod %s: %w", tmpName, err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("renaming %s to %s: %w", tmpName, path, err)
	}
	// And the directory entry must be durable too, or the rename can
	// be lost by a power cut even though the file's data survived.
	return syncDir(dir)
}

// forget removes anchor's record. Called by Flush: an anchor that has
// been emptied has a last-known-good ruleset of "nothing", and the
// honest encoding of that is the absence of a baseline rather than a
// file claiming an empty one - which is precisely the state a
// never-loaded anchor is in, and both of them mean "this anchor is not
// enforcing anything we know about".
func (s knownGoodStore) forget(anchor string) error {
	path, err := s.path(anchor)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// load reads anchor's record. A missing file is KnownGoodAbsent and a
// bad one is KnownGoodCorrupt, both with a nil error: they are states
// the caller must handle differently, not failures of the read. A
// non-nil error means the read itself failed in some other way (a
// permission problem, say) and the caller has no baseline either.
func (s knownGoodStore) load(anchor string) (KnownGood, KnownGoodState, error) {
	path, err := s.path(anchor)
	if err != nil {
		return KnownGood{}, KnownGoodAbsent, err
	}
	content, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return KnownGood{Anchor: anchor, State: KnownGoodAbsent, Detail: "no last-known-good record for this anchor on this node"}, KnownGoodAbsent, nil
		}
		return KnownGood{Anchor: anchor, State: KnownGoodAbsent, Detail: err.Error()}, KnownGoodAbsent, err
	}

	kg, state, err := parseKnownGood(anchor, string(content))
	return kg, state, err
}

// parseKnownGood is the pure half of the load: text in, a record and a
// state out. Split out from the file I/O so every corrupt shape can be
// tested without a filesystem, and so the format has exactly one
// definition.
func parseKnownGood(anchor, content string) (KnownGood, KnownGoodState, error) {
	corrupt := func(format string, args ...any) (KnownGood, KnownGoodState, error) {
		reason := fmt.Sprintf(format, args...)
		return KnownGood{Anchor: anchor, State: KnownGoodCorrupt, Detail: "last-known-good record is corrupt: " + reason}, KnownGoodCorrupt, nil
	}

	if !strings.HasPrefix(content, knownGoodMagic+"\n") {
		return corrupt("missing %q header", knownGoodMagic)
	}
	rest := content[len(knownGoodMagic)+1:]

	fields := make([]string, 0, 3)
	for len(fields) < 3 {
		line, after, ok := strings.Cut(rest, "\n")
		if !ok {
			return corrupt("header is truncated")
		}
		fields = append(fields, line)
		rest = after
	}
	var recordedAt time.Time
	var wantDigest string
	for i, field := range fields {
		key, value, ok := strings.Cut(field, " ")
		if !ok {
			return corrupt("header line %d (%q) is not a key/value pair", i+1, field)
		}
		switch key {
		case "anchor":
			if i != 0 {
				return corrupt("anchor line is out of order")
			}
			if value != anchor {
				return corrupt("record is for anchor %q, not %q", value, anchor)
			}
		case "recorded":
			if i != 1 {
				return corrupt("recorded line is out of order")
			}
			t, err := time.Parse(time.RFC3339, value)
			if err != nil {
				return corrupt("recorded timestamp %q is not RFC3339: %v", value, err)
			}
			recordedAt = t
		case "sha256":
			if i != 2 {
				return corrupt("sha256 line is out of order")
			}
			wantDigest = value
		default:
			return corrupt("unknown header key %q", key)
		}
	}
	if wantDigest == "" {
		return corrupt("no sha256 line")
	}
	// The blank line after the header is the separator between
	// metadata and ruleset; without it a truncated write could
	// silently turn header text into a rule.
	if rest == "" || rest[0] != '\n' {
		return corrupt("header is not followed by a blank line")
	}
	body := rest[1:]

	got := sha256.Sum256([]byte(body))
	if hex.EncodeToString(got[:]) != wantDigest {
		return corrupt("checksum mismatch: the file's body does not hash to its own recorded digest, so it was truncated or edited")
	}

	state := KnownGoodLoaded
	detail := ""
	if body == "" {
		state = KnownGoodEmpty
		detail = "the last successful load of this anchor was an empty ruleset (everything allowed)"
	}
	return KnownGood{Anchor: anchor, State: state, Body: body, RecordedAt: recordedAt, Detail: detail}, state, nil
}

// syncDir fsyncs a directory so a rename into it is durable.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("opening %s to fsync it: %w", dir, err)
	}
	defer d.Close()
	if err := d.Sync(); err != nil {
		return fmt.Errorf("fsyncing %s: %w", dir, err)
	}
	return nil
}
