package backup

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
)

// Codec is the manifest's wire encoding. Everything above this line -
// versioning, the unknown-version refusal, the self-checksum, the secret
// exclusion, atomic publication, the verification verdicts, the
// consistency label - is independent of how the bytes are produced, and
// must stay that way.
//
// It exists as an interface for exactly one reason: ADR-0131 specifies
// this schema as api/internalpb/backup.proto, and that message set does
// not exist in the repository. Adding a proto file and regenerating
// stubs is a change this package must not make, so the schema is
// expressed as Go structs with the ADR's own field names and encoded as
// self-describing JSON in the meantime. When backup.proto lands,
// implementing Encode/Decode against it is a change to
// JSONCodec's body and to nothing else - and the byte-level secrets test
// keeps meaning exactly the same thing, because it greps encoded bytes
// rather than any particular encoding.
type Codec interface {
	// Encode returns the manifest's wire bytes. A manifest's Checksum
	// field is ignored on the way in: it is computed by EncodeManifest,
	// which is the only supported way to produce bytes, so a caller
	// cannot hand-write a manifest whose checksum does not describe it.
	Encode(m *Manifest) ([]byte, error)
	// Decode parses wire bytes into a manifest without judging the
	// result. Validation, checksum verification and version judgement
	// are DecodeManifest's job, not Decode's, so that an unreadable
	// archive produces a report about the archive rather than an
	// opaque parse error.
	Decode(b []byte) (*Manifest, error)
	// Name identifies the encoding in an error message, so a manifest
	// that is not parseable says how it was being read.
	Name() string
}

// JSONCodec is the v1 encoding.
//
// Why JSON rather than hand-rolled binary or a single-length-prefixed
// blob:
//
//   - A manifest outlives the binary that wrote it, possibly by years.
//     A future field added to this schema has to be survivable by a
//     reader that predates it. Self-describing text gets that for free -
//     a reader ignores a key it does not recognise - and the version
//     check, not the parser, is what turns "ignores an unknown key" into
//     "refuses an unknown version".
//   - The manifest is also the document an operator reads when a backup
//     has to be understood by hand, off a dead host, with no Apiary
//     binary to hand. That is not a hypothetical: it is the case this
//     whole feature exists for.
//   - The size difference against a packed encoding is irrelevant next
//     to the artifacts, which are the multi-gigabyte part.
//
// The cost is real and is not hidden: JSON is not as compact as a proto
// encoding, and it is not as schema-enforcing. The version field plus
// Validate are what supply the enforcement a .proto file would have
// supplied for free.
type JSONCodec struct{}

var _ Codec = JSONCodec{}

// Name implements Codec.
func (JSONCodec) Name() string { return "json" }

// Encode implements Codec. It is a plain json.Marshal of the struct, and
// the result is deterministic: every field is a typed struct member (no
// maps anywhere in the schema), and encoding/json emits struct fields in
// declaration order. Determinism is a correctness requirement here, not
// a nicety - the self-checksum is computed over exactly these bytes, so
// a non-deterministic encoding would make a manifest unverifiable by a
// later binary that re-encoded it. TestCodecRoundTripIsByteStable asserts
// it.
//
// The checksum field is encoded as whatever the caller set, because the
// checksum is a MEMBER of the document: a reader has to find it to
// verify anything. The digest itself is computed over the document with
// the field absent, and both EncodeManifest and ReadManifest implement
// that same exclusion at their own call sites - the one place that
// decides what the digest covers.
func (JSONCodec) Encode(m *Manifest) ([]byte, error) {
	if m == nil {
		return nil, fmt.Errorf("backup: cannot encode a nil manifest")
	}
	b, err := json.Marshal(m)
	if err != nil {
		return nil, fmt.Errorf("backup: encoding manifest: %w", err)
	}
	return b, nil
}

// Decode implements Codec.
func (JSONCodec) Decode(b []byte) (*Manifest, error) {
	var m Manifest
	dec := json.NewDecoder(bytes.NewReader(b))
	// Unknown keys are ignored, which is the forward-compatibility
	// property the format version check then qualifies: a newer
	// manifest parses, and is then refused for being newer.
	if err := dec.Decode(&m); err != nil {
		return nil, err
	}
	// A second Decode must hit EOF, so that a file containing two
	// concatenated JSON documents is rejected rather than silently
	// half-read. Manifest files are single documents; two of them in
	// one file is corruption worth naming.
	if dec.More() {
		return nil, fmt.Errorf("manifest contains more than one JSON document")
	}
	return &m, nil
}

// DefaultCodec is what Store uses when no codec is supplied.
func DefaultCodec() Codec { return JSONCodec{} }

// EncodeManifest serialises a manifest and stamps it with a checksum
// over its own body. It is the only supported way to produce manifest
// bytes, and the checksum is computed here rather than by the caller so
// that "the digest describes the bytes" is guaranteed by construction
// rather than by discipline.
//
// The checksum covers the body with the checksum field absent, mirroring
// internalpb.ConfigArchive's own arrangement (its checksum covers the
// embedded payload, not the envelope). So the file that lands on disk
// DOES contain a checksum field - a reader has to be able to find it -
// while the digest is computed over the same document with that one
// field removed. ReadManifest applies the identical exclusion, so the
// two halves cannot drift: a change to what the digest covers has to be
// made in one function or the round trip stops verifying.
func EncodeManifest(codec Codec, m *Manifest) ([]byte, error) {
	body, err := encodeBody(codec, m)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(body)
	stamped := *m
	stamped.Checksum = hex.EncodeToString(sum[:])
	return codec.Encode(&stamped)
}

// encodeBody is the one definition of "the bytes a manifest's checksum
// covers": the manifest, encoded, with the checksum field itself
// removed. Both EncodeManifest and ReadManifest go through it, which is
// the only reason the two can be trusted to agree.
func encodeBody(codec Codec, m *Manifest) ([]byte, error) {
	probe := *m
	probe.Checksum = ""
	return codec.Encode(&probe)
}

// ReadInfo is everything a caller learns about a manifest document
// before any of it is trusted. It exists because the interesting cases
// are the ones that are not errors from the caller's point of view - a
// newer archive on a target is a normal thing to find, and the surface
// that lists backups has to be able to say so without the whole listing
// failing.
type ReadInfo struct {
	// Path is where the document was read from.
	Path string
	// FormatVersion is the version the document declared, verbatim,
	// including a version this build does not implement. It is recorded
	// even on the failure path precisely so a UI can print "written by a
	// newer Apiary" instead of "corrupt".
	FormatVersion uint32
	// Understood is false when FormatVersion is not this build's, or
	// when the document failed its own checksum, or when it failed
	// Validate. Every one of those is a refusal to act on the contents,
	// never a partial interpretation of them.
	Understood bool
	// ChecksumVerified records whether the document's self-checksum was
	// recomputed and matched. It is a separate field rather than folded
	// into Understood so a caller can distinguish "the bytes are intact
	// and the format is new" from "the bytes are damaged", which are
	// very different problems with very different fixes.
	ChecksumVerified bool
	// State is the coarse verdict the listing and verification surfaces
	// render. See GenerationState.
	State GenerationState
	// Reason is the human-readable version of State, always populated.
	Reason string
}

// ReadManifest parses and judges a manifest document.
//
// The order is load-bearing and is the same order raftd's
// loadConfigArchive uses for ConfigArchive: parse, then version, then
// checksum. Each stage's failure is reported in the ReadInfo as well as
// the error, so the difference between "this archive is from the future",
// "this archive is damaged" and "this archive is internally inconsistent"
// survives all the way to whatever surface asked.
//
// A manifest that fails any stage is not partially returned. The zero
// Manifest accompanies a nil error only in the one case where a caller
// genuinely needs the bytes it did understand - an unknown version, where
// ReadInfo.FormatVersion and ReadInfo.Reason are the useful output - and
// even there the returned Manifest has Understood false and must not be
// acted on. Restore's eligibility check is the only consumer that reads
// a Manifest out of an un-understood ReadInfo, and it refuses.
func ReadManifest(codec Codec, path string, data []byte) (*Manifest, ReadInfo, error) {
	info := ReadInfo{Path: path}

	m, err := codec.Decode(data)
	if err != nil {
		info.State = GenerationUnreadable
		info.Reason = fmt.Sprintf("the file did not parse as a %s manifest: %v", codec.Name(), err)
		return nil, info, fmt.Errorf("backup: %s: %w", path, err)
	}
	info.FormatVersion = m.FormatVersion

	// Version before checksum, matching ConfigArchive: a reader that
	// does not understand the format cannot meaningfully decide what
	// its fields mean, and so cannot judge the checksum over them
	// either. It is also the cheaper and more informative failure.
	if m.FormatVersion == 0 {
		info.State = GenerationUnreadable
		info.Reason = "the manifest carries no format_version, so it is a corrupt or hand-assembled document rather than an older or newer format"
		return nil, info, fmt.Errorf("backup: %s: %w", path, ErrMissingFormatVersion)
	}
	if m.FormatVersion != FormatVersion {
		info.State = GenerationUnknownVersion
		info.Reason = (&VersionError{Found: m.FormatVersion, Supported: FormatVersion}).Error()
		// The parsed manifest is returned alongside the error, and only
		// for a caller that wants to report on it. It is explicitly not
		// trusted: Understood is false, and Restore refuses it.
		out := *m
		return &out, info, fmt.Errorf("backup: %s: %w", path, &VersionError{Found: m.FormatVersion, Supported: FormatVersion})
	}

	// The self-checksum. The digest is over the body with the checksum
	// field absent, so it is recomputed by re-encoding through the same
	// encodeBody helper the writer used - one definition of what the
	// digest covers, used by both sides.
	if m.Checksum == "" {
		info.State = GenerationUnreadable
		info.Reason = "the manifest carries no checksum, so a reader cannot tell a complete document from a truncated one"
		return nil, info, fmt.Errorf("backup: %s: manifest has no checksum; a manifest whose own integrity is unknown is refused", path)
	}
	if err := validDigest(m.Checksum); err != nil {
		info.State = GenerationUnreadable
		info.Reason = fmt.Sprintf("the manifest's checksum field is not a SHA-256 hex digest, so the document is damaged: %v", err)
		return nil, info, fmt.Errorf("backup: %s: %w", path, err)
	}
	body, err := encodeBody(codec, m)
	if err != nil {
		info.State = GenerationUnreadable
		info.Reason = fmt.Sprintf("the manifest could not be re-encoded for checksum verification: %v", err)
		return nil, info, fmt.Errorf("backup: %s: %w", path, err)
	}
	sum := sha256.Sum256(body)
	got := hex.EncodeToString(sum[:])
	if got != canonicalDigest(m.Checksum) {
		info.State = GenerationUnreadable
		info.Reason = fmt.Sprintf("the manifest failed its own checksum: it records %s and its contents hash to %s, so the file is corrupt, truncated, or was edited after it was written", shortDigest(mustHex(m.Checksum)), shortDigest(sum[:]))
		return nil, info, fmt.Errorf("backup: %s: %w", path, ErrChecksumMismatch)
	}
	info.ChecksumVerified = true

	// Finally the internal rules, including the artifact-path
	// allowlist. A manifest can be internally consistent in its
	// checksum and still be unsafe to act on - a path that climbs out of
	// the generation directory, an artifact with no digest - and acting
	// on it is exactly what this package exists to prevent.
	if err := m.Validate(); err != nil {
		info.State = GenerationInconsistent
		info.Reason = err.Error()
		return nil, info, fmt.Errorf("backup: %s: %w", path, err)
	}

	info.Understood = true
	info.State = GenerationOK
	info.Reason = fmt.Sprintf("format_version %d, %d artifact(s), consistency %s", m.FormatVersion, len(m.Artifacts), m.Consistency)
	return m, info, nil
}

// mustHex decodes a hex string that validDigest has already accepted, so
// a failure here is a programming error rather than a data condition.
func mustHex(s string) []byte {
	b, err := hex.DecodeString(s)
	if err != nil {
		return nil
	}
	return b
}
