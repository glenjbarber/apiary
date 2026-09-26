// Package backup implements the non-UI core of Apiary's manifest-backed
// backup and restore (docs/adr/0131-backup-system.md): a versioned
// manifest, opaque artifacts addressed by relative path, and the three
// rules that make a manifest worth believing.
//
// # The three rules this package exists to enforce
//
//  1. A manifest is written LAST, and only once every artifact it names
//     is fully present, durable, and renamed into place. A directory
//     with no manifest is not a backup; it is garbage, and Sweep
//     removes it. This is enforced structurally, not by convention:
//     Store.Begin returns a Generation on which the only way to produce
//     a manifest is Generation.Commit, Commit refuses any manifest whose
//     artifact set is not exactly the set Generation actually wrote
//     (with matching digests and sizes), and Commit performs the write
//     as temp file -> fsync -> rename -> parent-directory fsync. A
//     partially-populated generation therefore has no state in which it
//     can be mistaken for a backup.
//
//  2. Secrets are structurally excluded. A live credential in this
//     codebase - nodeconfig.Config.PeerAPIKey, nodeconfig.Config.
//     RaftdToken, frontendconfig.Config.ManagerAPIKey,
//     raftdconfig.Config.InternalToken - is a value that must never
//     reach a backup in any encoding. That is enforced three ways, and
//     all three are tested: the manifest is built from allow-listed
//     projections into types defined here that have no field capable of
//     holding a secret (secrets.go); a deny list of secret-bearing field
//     names is walked by reflection over every value on its way into a
//     manifest, and a non-zero secret-named field is a hard error; and
//     the live secret VALUES are carried as a watchlist that the encoded
//     bytes are grepped against immediately before Commit. The watchlist
//     is a runtime backstop for a mistake the projections should already
//     have made impossible - it is not the primary mechanism, and the
//     tests assert all three independently.
//
//  3. A backup states the weakest guarantee it can actually support. A
//     ConsistencySnapshot generation is filesystem consistent and
//     explicitly not application consistent; a ConsistencyQuiesced
//     generation carries positive evidence that the Cell was observed
//     stopped, and cannot be constructed without it. There is no
//     "default" consistency: an unlabelled manifest is refused at write
//     time, because defaulting would be exactly the silent downgrade the
//     ADR forbids. Capture never falls back from QUIESCED to SNAPSHOT
//     when a Cell will not stop - it fails, and says why.
//
// # Opaque artifacts
//
// An artifact is a byte stream this package writes, sizes, digests and
// locates, and otherwise does not interpret. A dataset stream, a base
// image, a marshalled definition - none of them is parsed here. The
// manifest describes them (id, kind, size, SHA-256, relative path) so a
// reader can decide whether a generation is intact without needing the
// system that produced it, and that is the whole of the relationship.
//
// # Verification has three answers, not two
//
// verified, checksum_mismatch, and missing. A missing artifact is not a
// corrupt one: conflating them trains operators to ignore the alarm that
// matters, so they are distinct verdicts with distinct reasons, and an
// artifact whose absence cannot even be confirmed (a directory where a
// file should be, a permission error) is reported as missing-with-a-
// reason rather than as a mismatch. See verify.go.
//
// Nothing here hashes an artifact to claim it is restorable. A checksum
// proves the bytes that landed are the bytes that were produced; it
// proves nothing about whether they can be sent back anywhere, and the
// ADR's restore-drill tier is exactly the gap this package leaves open.
// It is left open on purpose, and reported as open.
//
// # What this package does not do
//
//   - It does not touch raft state. Restoring the Colony's control plane
//     is raftd -export/-restore against internalpb.ConfigArchive
//     (ADR-0051, ADR-0124), which is a separate, offline,
//     operator-initiated path this package neither performs nor
//     duplicates. There is deliberately no code here that writes a raft
//     log or snapshot file, and no interface that could be pointed at one.
//   - It does not schedule, lease, replicate, retain or prune by policy.
//     Those are the leader's and the FSM's concerns; this package is the
//     capture/verify/restore core they would drive.
//   - It does not encrypt anything. v1's answer to "the archive might
//     contain a credential" is exclusion, not encryption (ADR Rejected
//     alternative 5); guest data inside a dataset stream is not
//     encrypted by this system and this package does not pretend
//     otherwise.
//   - It does not run any host command. Every command this package
//     ultimately depends on - the dataset stream producer, the extractor
//   - arrives as an interface, so nothing here needs a live host, a
//     dataset, or root. Materialise reuses internal/jailarchive's
//     Extractor rather than writing a second unpacker.
//
// # Wire format, and the proto change this package is working around
//
// The manifest is an on-disk archive format that outlives the binary
// that wrote it, so it is versioned from the first release
// (FormatVersion) and self-checksummed, exactly as internalpb's
// ConfigArchive already is. ADR-0131 specifies that schema as a new
// api/internalpb/backup.proto, and adding a proto plus generated stubs
// is outside this package's scope: the schema is therefore expressed
// here as Go structs with JSON field names taken verbatim from the
// ADR's proto sketch, and encoding lives behind the Codec interface so
// that swapping JSON for proto.Marshal(BackupManifest) touches exactly
// one file. Everything above the codec - versioning, the unknown-version
// refusal, the self-checksum, the secret exclusion, atomicity, the
// verdicts, the consistency label - is independent of the encoding and
// does not move when the encoding does. See codec.go, and the
// "Required proto change" section of the commit message for the exact
// message set to add.
package backup

import (
	"errors"
	"fmt"
)

// FormatVersion is this manifest format's own schema version, starting
// at 1. It is deliberately independent of every other versioned format
// in the system - internalpb.FSMSnapshotState's, and
// internalpb.ConfigArchive's - following the same discipline
// ConfigArchive already establishes: a version belongs to the envelope
// that carries it, so that adding a field to a payload cannot be
// mistaken for a change to how the payload is wrapped.
//
// It exists from the first release, before there is anything to migrate.
// A format that acquires a version at the moment it first changes has
// already shipped a version 0 that some archive somewhere carries, and
// that archive will be read by a binary that has never heard of it.
const FormatVersion uint32 = 1

// ErrUnknownFormatVersion is reported when a manifest carries a
// format_version this build does not implement. It is never downgraded
// to a warning and never treated as "probably compatible": an archive
// written by a newer Apiary may have changed any field, and this build
// has no way to know which. The concrete failure is a *VersionError,
// which carries both numbers so a caller can report exactly what it
// found.
var ErrUnknownFormatVersion = errors.New("backup: manifest format_version is not understood")

// ErrMissingFormatVersion is reported for a manifest with no
// format_version at all. This is deliberately distinct from
// ErrUnknownFormatVersion: a zero version is a corrupt or
// hand-assembled document, not a future one, and telling an operator
// those two things apart matters - one is a bug in the writer, the
// other is a normal thing to meet when reading an archive off a stick
// that a newer build wrote.
var ErrMissingFormatVersion = errors.New("backup: manifest carries no format_version")

// ErrInvalidManifest is the umbrella for a manifest that is internally
// inconsistent - a duplicate artifact id, a path that escapes the
// generation directory, a reference artifact that claims to have a
// payload. Every specific reason is a distinct wrapped error, so a
// caller can match on the cause with errors.Is rather than on a string.
var ErrInvalidManifest = errors.New("backup: manifest is not internally consistent")

// ErrArtifactMissing is reported when an artifact named by a manifest
// is not present, or cannot be opened. It is separate from
// ErrChecksumMismatch on purpose; see verify.go.
var ErrArtifactMissing = errors.New("backup: artifact is missing or unreadable")

// ErrChecksumMismatch is reported when an artifact is present and
// readable but its bytes do not hash to the recorded digest. It means
// corruption, never absence.
var ErrChecksumMismatch = errors.New("backup: artifact checksum does not match the manifest")

// ErrSecretExposed is reported when a value that a manifest must never
// carry is found on its way in - either a deny-listed field name reached
// by reflection, or a live secret value found in the encoded bytes.
// Both are hard failures with no override; see secrets.go for why there
// is deliberately no "I am sure" flag.
var ErrSecretExposed = errors.New("backup: a value that must never reach a manifest was found on its way in")

// ErrConsistencyNotQuiesced is reported when a capture was asked for
// ConsistencyQuiesced and the Cell's stopped state could not be
// observed. The capture fails; it does not quietly produce a
// ConsistencySnapshot generation instead, because a manifest that claims
// a guarantee its producer never obtained is a lie no later reader can
// detect.
var ErrConsistencyNotQuiesced = errors.New("backup: could not observe the Cell stopped, so a quiesced backup was not produced")

// ErrSkewExceeded is reported when a quiesced generation's snapshots
// span longer than the policy's max_skew. A quiesced backup whose
// artifacts were captured across a long window is not the single instant
// it claims to be.
var ErrSkewExceeded = errors.New("backup: snapshot skew exceeds the policy's max_skew")

// ErrManifestShadowed is reported when an artifact asks for a relative
// path that would collide with the manifest's own filename. The manifest
// name is reserved: an artifact squatting it could overwrite the
// generation's index with payload bytes.
var ErrManifestShadowed = errors.New("backup: artifact path would shadow the manifest file")

// ErrGenerationCommitted is reported when a generation is written to
// after it was already committed. A committed generation is immutable;
// overwriting one would destroy a backup that may be the only copy of
// something.
var ErrGenerationCommitted = errors.New("backup: this generation is already committed and cannot be modified")

// ErrArtifactSetMismatch is reported when the manifest handed to Commit
// does not describe exactly the artifacts the generation wrote. This is
// the structural half of the manifest-written-last rule: the manifest
// cannot claim a payload that was never captured, and cannot omit one
// that was.
var ErrArtifactSetMismatch = errors.New("backup: manifest artifact set does not match what was written")

// VersionError is the concrete ErrUnknownFormatVersion. It carries both
// numbers so a caller can report what it actually found rather than
// re-deriving it from a message string.
type VersionError struct {
	// Found is the format_version the manifest declared.
	Found uint32
	// Supported is the set of versions this build implements, which for
	// a v1 reader is exactly {FormatVersion}.
	Supported uint32
}

func (e *VersionError) Error() string {
	return fmt.Sprintf("backup: manifest declares format_version %d and this build implements exactly %d - the manifest was rejected rather than interpreted, because a newer format may have changed any field, including the ones a restore depends on (the archive is left exactly as it is; read it with the Apiary build that wrote it, or check whether a newer build is required)",
		e.Found, e.Supported)
}

// Unwrap lets errors.Is(err, ErrUnknownFormatVersion) work on the
// concrete error without every caller having to type-assert.
func (e *VersionError) Unwrap() error { return ErrUnknownFormatVersion }

// shortDigest renders a digest for an error message: enough hex to
// identify which of two values differs, not the whole 64 characters,
// which would push the useful part of the message off the end.
func shortDigest(b []byte) string {
	const keep = 12
	if len(b) <= keep {
		return fmt.Sprintf("%x", b)
	}
	return fmt.Sprintf("%x...", b[:keep])
}
