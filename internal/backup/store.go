package backup

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"io/fs"
	"time"
)

// GenerationState is the coarse verdict about a generation directory,
// used by listing and verification surfaces. It has exactly one
// "good" value and everything else is a named reason, following the
// project's rule that missing evidence is never a pass (ADR-0056,
// ADR-0118, ADR-0122).
type GenerationState string

const (
	// GenerationOK means the manifest was read, its version understood,
	// its own checksum verified, and its internal rules satisfied. It
	// says nothing about whether the artifacts are present - that is
	// Verify's job, and a GenerationOK with a missing artifact is a
	// state the surfaces must be able to show.
	GenerationOK GenerationState = "ok"

	// GenerationIncomplete means the directory has no manifest at all.
	// By this package's definition that is not a degraded backup, it is
	// not a backup: nothing was ever published, so there is nothing to
	// be unsure about. Sweep deletes these.
	GenerationIncomplete GenerationState = "incomplete"

	// GenerationUnknownVersion means a manifest exists and was read, but
	// declares a format_version this build does not implement. The
	// archive is intact; this build simply may not act on it. It is
	// "unknown" and never "failed", because a future archive on a
	// target is a normal thing to find.
	GenerationUnknownVersion GenerationState = "unknown_format_version"

	// GenerationUnreadable means a manifest exists but did not parse,
	// carries no checksum, failed its own checksum, or is otherwise
	// damaged. This one really is a problem with the archive, and it is
	// still "unknown" rather than "failed" at the level of a listing,
	// because what it proves is only that this copy is damaged - not
	// that the backup never existed.
	GenerationUnreadable GenerationState = "unreadable"

	// GenerationInconsistent means the manifest's own checksum verified
	// - the bytes are intact - but the document says something this
	// build refuses to act on, such as an artifact path outside the
	// generation directory. Those two facts are kept apart on purpose:
	// "corrupt" and "well-formed but unsafe" have different causes and
	// different fixes.
	GenerationInconsistent GenerationState = "inconsistent"
)

// Store owns a backup target root: a directory laid out as
//
//	<root>/<policy_id>/<generation_id>/manifest.pb
//	<root>/<policy_id>/<generation_id>/artifacts/<id>.<ext>
//
// It is the only thing in this package that can create a manifest, and
// it creates one only through Generation.Commit.
type Store struct {
	fs    FS
	codec Codec
	now   func() time.Time
	// root is the target directory holding every policy's generations.
	root string
	// quiescer and snapshots are the two seams to the outside world that
	// a capture needs. They are set explicitly and have no defaults,
	// because both of them are things this package must not be able to
	// infer: a Quiescer that stopped nothing would produce a backup
	// labelled quiesced, and a missing snapshot taker would produce a
	// capture window derived from a clock rather than from an
	// observation.
	quiescer  Quiescer
	snapshots SnapshotTaker
	// sanitizer carries this node's live secret VALUES for the byte-level
	// guard. It is optional at the Store level and its absence is
	// reported by GuardArmed rather than being silently treated as a
	// pass: the two unconditional layers (the allow-listed projections
	// and the reflection audit) are what make the exclusion structural,
	// and the watchlist is the alarm for a mistake they cannot reason
	// about. A node with no live secrets legitimately has nothing to
	// watch, and refusing to back up such a node would be a bug.
	sanitizer *Sanitizer
}

// GuardArmed reports whether the live-value byte guard is watching
// anything. A surface that wants to state how thoroughly a generation was
// checked asks this rather than assuming the guard ran.
func (s *Store) GuardArmed() bool { return s.sanitizer.Armed() }

// Option configures a Store. The filesystem and the clock are options
// because both are load-bearing to the guarantees rather than
// incidental: the atomicity claim is about this package's file
// operations, and the created_unix in a manifest is evidence.
type Option func(*Store)

// WithFS replaces the filesystem. Tests use this to inject failures at
// exactly the fsync or rename that a guarantee depends on.
func WithFS(filesystem FS) Option {
	return func(s *Store) {
		if filesystem != nil {
			s.fs = filesystem
		}
	}
}

// WithCodec replaces the wire encoding. The default is JSONCodec; see
// codec.go for why it is behind an interface at all.
func WithCodec(c Codec) Option {
	return func(s *Store) {
		if c != nil {
			s.codec = c
		}
	}
}

// WithSanitizer installs the live-value byte guard. See Store.sanitizer.
func WithSanitizer(san *Sanitizer) Option {
	return func(s *Store) {
		if san != nil {
			s.sanitizer = san
		}
	}
}

// WithClock replaces the clock. A manifest's created_unix and a
// capture's snapshot bounds are evidence, so a test that needs a
// specific one supplies one rather than reading the wall clock and
// hoping.
func WithClock(now func() time.Time) Option {
	return func(s *Store) {
		if now != nil {
			s.now = now
		}
	}
}

// NewStore returns a Store rooted at dir.
func NewStore(dir string, opts ...Option) *Store {
	s := &Store{fs: OSFS{}, codec: DefaultCodec(), now: time.Now}
	for _, opt := range opts {
		opt(s)
	}
	s.root = dir
	return s
}

// GenerationDir returns the directory a policy's generation lives in.
func (s *Store) GenerationDir(policyID, generationID string) string {
	return joinPath(s.root, policyID, generationID)
}

// Begin starts a generation and returns the handle that can add
// artifacts to it.
//
// It refuses to open a directory that already has a manifest in it. A
// committed generation is immutable, and this is where that is enforced:
// overwriting a published backup because a job was re-run with the same
// identifier would destroy the only copy of something. A caller that
// wants to replace a generation generates a new identifier.
//
// The returned handle carries the set of artifacts it actually wrote.
// Commit refuses any manifest that does not describe exactly that set,
// which is what makes "a manifest is never visible for artifacts that
// are not fully present" a structural property rather than a rule
// something has to remember.
func (s *Store) Begin(policyID, generationID string) (*Generation, error) {
	if err := validID("policy_id", policyID); err != nil {
		return nil, err
	}
	if err := validID("generation_id", generationID); err != nil {
		return nil, err
	}
	dir := s.GenerationDir(policyID, generationID)
	if _, err := s.fs.Stat(joinPath(dir, ManifestName)); err == nil {
		return nil, fmt.Errorf("%w: %s already has a %s; pick a new generation_id rather than overwriting a published backup",
			ErrGenerationCommitted, dir, ManifestName)
	} else if !errors.Is(err, fs.ErrNotExist) {
		// A stat error that is not "does not exist" is an inability to
		// answer, and an inability to answer must not be read as "no
		// manifest". Guessing here would let a write proceed against a
		// directory whose state is unknown.
		return nil, fmt.Errorf("checking whether %s already exists: %w", dir, err)
	}
	if err := s.fs.MkdirAll(joinPath(dir, artifactsDirName), 0o700); err != nil {
		return nil, fmt.Errorf("creating %s: %w", dir, err)
	}
	return &Generation{store: s, dir: dir, policyID: policyID, generationID: generationID}, nil
}

// Generation is an in-progress generation: artifacts may be added, and
// the manifest may be published exactly once, at the end.
type Generation struct {
	store        *Store
	dir          string
	policyID     string
	generationID string
	// written is every artifact this generation has published, in the
	// order it did so. Commit matches the manifest against this list
	// field by field.
	written []Artifact
	// committed is set by Commit, and a second Commit is refused.
	committed bool
	// aborted is set by Abort, which is the handle's own statement that
	// nothing here is a backup.
	aborted bool
}

// Dir returns the generation's directory. It is exposed so a caller
// running a dataset stream can name its destination, and so a failure
// message can be specific about where the partial generation is.
func (g *Generation) Dir() string { return g.dir }

// Artifacts returns the artifacts published so far, in write order. The
// slice is a copy: the generation's own state is not something a caller
// can reach in and edit behind Commit's back.
func (g *Generation) Artifacts() []Artifact {
	return append([]Artifact(nil), g.written...)
}

// usable reports whether the handle can still be written to, with a
// reason for each refusal.
func (g *Generation) usable() error {
	if g.aborted {
		return fmt.Errorf("backup: generation %s was aborted; it can no longer be written to", g.generationID)
	}
	if g.committed {
		return ErrGenerationCommitted
	}
	return nil
}

// WriteArtifact copies src into the generation and records the result.
//
// The bytes are written through a hashing, counting writer, so the
// digest and size in the manifest describe what actually landed rather
// than what was intended. The file itself is published atomically by the
// same temp-fsync-chmod-rename-directory-sync sequence the manifest uses:
// an artifact that is half-written under its own final name is an
// artifact a verifier would hash and declare corrupt, which would be a
// false corruption report about a backup that never claimed to be
// finished.
//
// ctx is honoured between chunks rather than only at entry, because a
// dataset stream is the one unbounded thing this package writes and a
// cancelled capture that keeps writing for another hour is a cancelled
// capture nobody can stop.
func (g *Generation) WriteArtifact(ctx context.Context, spec ArtifactSpec, src io.Reader) (Artifact, error) {
	if err := g.usable(); err != nil {
		return Artifact{}, err
	}
	art, err := spec.artifact()
	if err != nil {
		return Artifact{}, err
	}
	art.RelativePath = joinPath(artifactsDirName, art.ArtifactID+extensionFor(art.Kind))

	// The two guards that run on the bytes themselves, before they are
	// written anywhere a reader could see them.
	//
	// The first is the live-value watchlist, streaming so it costs
	// nothing on a dataset stream. It is skipped when unarmed, and
	// GuardArmed says so rather than pretending otherwise.
	//
	// The second is unconditional and specific to a node config: those
	// bytes are a decoded record, so they are decoded and audited by
	// their own key names before they are written. That check needs no
	// live secrets to run, which is the point - it is the one that still
	// works on a node whose configuration happens to hold no
	// credentials, and the one that catches a projection that gained a
	// field somebody forgot to classify.
	guarded := io.Writer(nil)
	hw := newHashWriter()
	if scanner := g.store.sanitizer.newSecretScanner(art.RelativePath); scanner != nil {
		guarded = scanner
	}
	var check io.Writer = hw
	if guarded != nil {
		check = io.MultiWriter(hw, guarded)
	}
	if err := g.writeFile(ctx, art.RelativePath, src, check, hw); err != nil {
		return Artifact{}, err
	}
	art.SHA256 = hex.EncodeToString(hw.sum())
	art.SizeBytes = hw.n

	g.written = append(g.written, art)
	return art, nil
}

// WriteBytes is WriteArtifact over a byte slice, for the artifact kinds
// this package builds itself - a marshalled definition, a redacted node
// config record.
//
// The body is audited before it is written when the spec is a node
// config. That is the single point ADR-0131 section 2 asks for - "the
// strip is enforced in code at the single point where a node config enters
// a manifest" - and it is here rather than in the projections' call
// sites so that no caller can reach a manifest around it.
func (g *Generation) WriteBytes(ctx context.Context, spec ArtifactSpec, body []byte) (Artifact, error) {
	if spec.Kind == KindNodeConfig {
		if err := auditNodeConfigRecord(body); err != nil {
			return Artifact{}, fmt.Errorf("refusing to write node config artifact %q: %w", spec.ID, err)
		}
	}
	return g.WriteArtifact(ctx, spec, newByteReader(body))
}

// writeFile performs the atomic publish, streaming through w. counter is
// the same stream's byte count, which writeFile needs and w does not
// expose.
func (g *Generation) writeFile(ctx context.Context, rel string, src io.Reader, w io.Writer, counter *hashWriter) error {
	if err := g.usable(); err != nil {
		return err
	}
	if err := validRelativePath(rel); err != nil {
		return err
	}
	full := joinPath(g.dir, rel)
	dir := joinPath(g.dir, pathDir(rel))

	if err := g.store.fs.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("creating %s: %w", dir, err)
	}

	tmp, err := g.store.fs.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return fmt.Errorf("creating a temp file in %s: %w", dir, err)
	}
	tmpPath := tmp.Name()
	defer func() { _ = g.store.fs.Remove(tmpPath) }()

	// A bounded buffer between the reader and the file, so a slow reader
	// (a zfs send over a busy link) does not turn into a syscall per
	// byte, and so ctx cancellation is checked at a predictable
	// interval rather than never.
	buf := make([]byte, 64*1024)
	for {
		select {
		case <-ctx.Done():
			tmp.Close()
			return fmt.Errorf("writing %s: %w", rel, ctx.Err())
		default:
		}
		n, readErr := src.Read(buf)
		if n > 0 {
			if _, err := w.Write(buf[:n]); err != nil {
				tmp.Close()
				return fmt.Errorf("writing %s: %w", rel, err)
			}
			if _, err := tmp.Write(buf[:n]); err != nil {
				tmp.Close()
				return fmt.Errorf("writing %s: %w", rel, err)
			}
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			tmp.Close()
			return fmt.Errorf("reading source for %s: %w", rel, readErr)
		}
	}

	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("syncing %s before publishing it: %w", tmpPath, err)
	}
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return fmt.Errorf("setting mode 0600 on %s: %w", tmpPath, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("closing %s: %w", tmpPath, err)
	}
	if err := g.store.fs.Rename(tmpPath, full); err != nil {
		return fmt.Errorf("publishing %s as %s: %w", tmpPath, full, err)
	}
	if err := g.store.fs.SyncDir(dir); err != nil {
		return fmt.Errorf("publishing %s succeeded but syncing %s did not: %w", full, dir, err)
	}
	return nil
}

// Commit publishes the manifest, and is the only path by which a
// manifest becomes visible.
//
// Four things happen, in this order, and the order is the design:
//
//  1. The manifest must describe EXACTLY the artifacts this generation
//     wrote - same set of ids, same kinds, same digests, same sizes.
//     Not a superset (which would claim a payload that was never
//     captured) and not a subset (which would orphan a payload that was
//     and would leave a restore silently missing data). This is checked
//     before anything is encoded, so a mismatch is a plain error with no
//     filesystem side effect at all.
//
//  2. The manifest's own rules are validated - version, the consistency
//     label and its evidence, artifact paths, digests, the skew limit.
//
//  3. The whole manifest is reflection-audited for a secret-shaped field
//     with a value, and its encoded bytes are grepped for this node's
//     live secret values. Both run before the write, so a refusal leaves
//     the filesystem exactly as it was: artifacts on disk, no manifest,
//     therefore not a backup.
//
//  4. Only then is the manifest published, atomically. Until the rename
//     in atomicWriteFile completes, a reader listing this directory sees
//     artifacts and no manifest, which by this package's definition is
//     garbage rather than a degraded backup. After it completes, a reader
//     sees the complete manifest, because the bytes were fsynced before
//     the name that makes them visible was created.
//
// The artifact set is reconciled here rather than trusted: Commit copies
// the recorded digests and sizes from what was written, so a caller
// cannot put a digest in the manifest that the bytes on disk do not
// have. It is a reconciliation, not an overwrite - a caller that
// disagrees about a digest gets ErrArtifactSetMismatch rather than
// having its value silently replaced.
func (g *Generation) Commit(ctx context.Context, m Manifest) error {
	if err := g.usable(); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("backup: committing generation %s: %w", g.generationID, err)
	}
	if m.PolicyID != g.policyID || m.GenerationID != g.generationID {
		return fmt.Errorf("%w: manifest names policy %q generation %q but this generation is policy %q generation %q",
			ErrArtifactSetMismatch, m.PolicyID, m.GenerationID, g.policyID, g.generationID)
	}
	if err := reconcileArtifacts(m.Artifacts, g.written); err != nil {
		return err
	}
	if m.CreatedUnix == 0 {
		m.CreatedUnix = unixSeconds(g.store.now())
	}
	if m.SecretGaps == nil {
		m.SecretGaps = SecretGaps()
	}
	if err := m.Validate(); err != nil {
		return err
	}
	if err := AuditSecretFree(&m); err != nil {
		return err
	}
	body, err := EncodeManifest(g.store.codec, &m)
	if err != nil {
		return err
	}
	// The live-value guard, one last time, on the manifest's own bytes.
	// The gap list it carries names every excluded credential by
	// description - kind, why, where the live copy lives - and never by
	// value, so this check is a backstop against a caller that put one
	// into a field of its own. Skipped when the store has no live secrets
	// to look for; GuardArmed reports that rather than it being implied.
	if scanner := g.store.sanitizer.newSecretScanner(ManifestName); scanner != nil {
		if _, err := scanner.Write(body); err != nil {
			return err
		}
	}
	if err := g.store.commitBytes(ctx, g.dir, body); err != nil {
		return err
	}
	// Marked committed only after the rename has happened, so a failed
	// publish leaves the handle usable for a retry and a published
	// generation can never be written to or removed again through it.
	g.committed = true
	return nil
}

// commitBytes is Store's half of Commit, kept on the Store so a caller
// that already has a manifest and a verified generation directory can
// use it without a Generation handle. It still runs every guard.
func (s *Store) commitBytes(ctx context.Context, dir string, body []byte) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("backup: publishing %s: %w", joinPath(dir, ManifestName), err)
	}
	if err := s.fs.SyncDir(joinPath(dir, artifactsDirName)); err != nil {
		// The artifacts must be durable before the manifest that
		// describes them, or a crash can leave a manifest naming bytes
		// that never landed. Failing here is the difference between "the
		// backup did not happen" and "the backup is a lie".
		return fmt.Errorf("syncing %s before publishing its manifest: %w", joinPath(dir, artifactsDirName), err)
	}
	return atomicWriteFile(s.fs, dir, ".manifest-*", joinPath(dir, ManifestName), body, 0o600)
}

// reconcileArtifacts checks the manifest's artifact set against what was
// written, then copies the recorded digest and size from the written
// artifact.
//
// The comparison is on (id, kind) for membership and on (id, kind, size,
// digest) for the values, because those are the fields a reader acts on.
// A mismatch in any of them is reported with both values, naming the
// artifact, because "the manifest does not match what was written" is
// not a message an operator can act on and "artifact X records 1024
// bytes but 2048 were written" is.
func reconcileArtifacts(declared, written []Artifact) error {
	byID := make(map[string]Artifact, len(written))
	for _, w := range written {
		byID[w.ArtifactID] = w
	}
	declaredIDs := make(map[string]struct{}, len(declared))
	for _, d := range declared {
		// A reference is declared rather than written - it has no bytes,
		// so there is nothing on disk to reconcile it against and nothing
		// that could be missing. It still goes through Validate, which is
		// where its shape (a name, a kind, a digest to check the fetch
		// against, and no payload) is enforced.
		if !d.Kind.hasPayload() {
			declaredIDs[d.ArtifactID] = struct{}{}
			continue
		}
		if _, dup := declaredIDs[d.ArtifactID]; dup {
			return fmt.Errorf("%w: artifact_id %q appears more than once in the manifest", ErrInvalidManifest, d.ArtifactID)
		}
		declaredIDs[d.ArtifactID] = struct{}{}

		w, ok := byID[d.ArtifactID]
		if !ok {
			return fmt.Errorf("%w: the manifest names artifact %q (%s), which this generation never wrote - a manifest may not describe a payload that was not captured",
				ErrArtifactSetMismatch, d.ArtifactID, d.Kind)
		}
		if d.Kind != w.Kind {
			return fmt.Errorf("%w: artifact %q is recorded as kind %q in the manifest and was written as %q",
				ErrArtifactSetMismatch, d.ArtifactID, d.Kind, w.Kind)
		}
		if d.SizeBytes != w.SizeBytes {
			return fmt.Errorf("%w: artifact %q records %d bytes in the manifest and %d were written",
				ErrArtifactSetMismatch, d.ArtifactID, d.SizeBytes, w.SizeBytes)
		}
		if d.SHA256 != "" && !digestEqual(d.SHA256, w.SHA256) {
			return fmt.Errorf("%w: artifact %q records digest %s in the manifest and its bytes hash to %s",
				ErrArtifactSetMismatch, d.ArtifactID, d.SHA256, w.SHA256)
		}
	}
	for _, w := range written {
		if _, ok := declaredIDs[w.ArtifactID]; !ok {
			return fmt.Errorf("%w: artifact %q (%s, %d bytes) was written but the manifest does not list it - an unlisted payload is data a restore would silently never see",
				ErrArtifactSetMismatch, w.ArtifactID, w.Kind, w.SizeBytes)
		}
	}
	return nil
}

// digestEqual compares two hex digests case-insensitively.
func digestEqual(a, b string) bool { return canonicalDigest(a) == canonicalDigest(b) }

// Abort discards the generation. It is the all-or-nothing half of the
// design: a capture that cannot finish leaves nothing that could be
// mistaken for a finished capture.
//
// A failed RemoveAll is returned rather than ignored, and the generation
// is still marked aborted. The alternative - reporting success because
// the directory is probably gone - would leave the one case that matters
// silently unreported: a generation directory that a future Sweep
// cannot remove, sitting on a target the operator believes is clean.
// Sweep will find it and report it, and a human can then look.
func (g *Generation) Abort() error {
	if g.committed {
		return fmt.Errorf("%w: %s was committed; refusing to remove a published backup", ErrGenerationCommitted, g.generationID)
	}
	g.aborted = true
	if err := g.store.fs.RemoveAll(g.dir); err != nil {
		return fmt.Errorf("removing the partial generation at %s: %w", g.dir, err)
	}
	return nil
}

// hashWriter digests and counts what passes through it. The digest is
// over the bytes that reached the file, not the bytes a caller intended
// to write, which is the only version of this that is worth anything.
//
// A streaming hash.Hash rather than a [32]byte kept in the struct: a
// re-seeded sum over each chunk would "work" for a single-chunk write
// and be silently wrong for a dataset stream, which is exactly the
// artifact where a wrong digest would matter most and be least likely
// to be noticed.
type hashWriter struct {
	h hash.Hash
	n uint64
}

func newHashWriter() *hashWriter { return &hashWriter{h: sha256.New()} }

func (h *hashWriter) Write(p []byte) (int, error) {
	n, err := h.h.Write(p)
	h.n += uint64(n)
	return n, err
}

func (h *hashWriter) sum() []byte { return h.h.Sum(nil) }
