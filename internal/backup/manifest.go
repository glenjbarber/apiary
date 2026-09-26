package backup

import (
	"fmt"
	"path"
	"sort"
	"strings"
	"time"
)

// ManifestName is the filename a manifest is published under inside its
// generation directory. It is fixed and reserved: artifacts may not use
// it (ErrManifestShadowed), and its presence is the sole definition of
// "this generation is a backup" - see the package doc.
//
// The .pb suffix records intent, not a promise: ADR-0131 specifies the
// schema as api/internalpb/backup.proto, which does not exist yet and
// whose generation is out of this package's scope. See codec.go and the
// "Required proto change" section of the introducing commit.
const ManifestName = "manifest.pb"

// artifactsDirName is the single directory every payload artifact lives
// under, relative to the generation directory. Keeping payloads in one
// named subdirectory is what makes "no manifest, no backup" and "only
// Sweep removes garbage" mechanically true: a listing of the generation
// directory has exactly two kinds of entry, the manifest, and the
// artifacts directory.
const artifactsDirName = "artifacts"

// Consistency is the guarantee a generation makes about the instant its
// artifacts were captured. There are exactly two values, and there is
// deliberately no zero value that means "unstated": an unlabelled
// manifest is refused (ErrInvalidManifest), because a default here
// would be the silent downgrade ADR-0131 section 3 exists to prevent.
type Consistency string

const (
	// ConsistencySnapshot means every artifact was captured from a
	// dataset snapshot taken while the workload kept running. It is
	// honest in exactly one direction: the bytes are a real,
	// point-in-time, filesystem-consistent image of each dataset. It is
	// NOT application consistent - a guest's page cache and transaction
	// log are not part of a dataset snapshot, so a database inside the
	// captured dataset may be mid-fsync. Nothing derived from this
	// generation may describe it as quiesced, application consistent, or
	// "as it was running".
	ConsistencySnapshot Consistency = "snapshot"

	// ConsistencyQuiesced means the workload was stopped, the stop was
	// observed through a lookup rather than assumed from a call that
	// merely returned no error, and only then were the snapshots taken.
	// A generation may only be built with this label if it also carries
	// Quiesce evidence; Validate refuses the combination without it.
	ConsistencyQuiesced Consistency = "quiesced"
)

// Valid reports whether c is one of the two labels this package
// implements. The empty string is not valid, deliberately.
func (c Consistency) Valid() bool {
	return c == ConsistencySnapshot || c == ConsistencyQuiesced
}

// String makes Consistency printable in an error message without a
// caller having to remember it is a string type.
func (c Consistency) String() string { return string(c) }

// Guarantees is the sentence a UI should be able to render verbatim
// from a label alone, with no interpretation on the caller's part. It
// exists because the failure this guards against is a summary sentence
// that quietly says "consistent" for a snapshot-consistent generation.
// Only the two labels this package implements are named; an unlabelled
// Consistency gets an explicit refusal, not a hedge.
func (c Consistency) Guarantees() string {
	switch c {
	case ConsistencySnapshot:
		return "filesystem consistent, not application consistent (no downtime)"
	case ConsistencyQuiesced:
		return "quiesced: the workload was observed stopped before the snapshots were taken"
	default:
		return "unlabelled - this generation states no consistency guarantee and must not be read as claiming one"
	}
}

// Quiesce is the positive evidence that a ConsistencyQuiesced
// generation is entitled to its label. It is a struct rather than a
// bare timestamp because a timestamp alone records when something
// happened without recording what was actually checked, and the check
// is the whole claim: "the Cell was observed stopped" is only true if
// a lookup said so.
type Quiesce struct {
	// CellID is the Cell that was observed stopped.
	CellID string `json:"cell_id,omitempty"`
	// ObservedAtUnix is when the observation was made, not when the
	// caller believed it would be.
	ObservedAtUnix int64 `json:"observed_at_unix,omitempty"`
	// Method names the observation that produced the verdict, verbatim,
	// so an operator reading a manifest months later can tell the
	// difference between "a GetJail call reported not running" and
	// "we assumed a stop RPC that returned no error meant stopped".
	Method string `json:"method,omitempty"`
}

// ArtifactKind distinguishes what an artifact is for, without this
// package ever interpreting the bytes behind it. Reference is the one
// kind with no payload at all: it points at something re-fetchable and
// digest-verifiable, and copying the payload would waste the target.
type ArtifactKind string

const (
	// KindDataset is a byte stream captured from a dataset snapshot.
	// The extension this package writes for it is .stream, and that is
	// a statement about this package's own bookkeeping, not a claim
	// about the stream's format - the bytes are opaque here.
	KindDataset ArtifactKind = "dataset"

	// KindConfig is a marshalled workload definition plus the node's
	// rendered configuration for it. Recorded so a restore can state
	// what configuration the data was captured under. It is never
	// applied to a live control plane in v1.
	KindConfig ArtifactKind = "config"

	// KindNodeConfig is a redacted projection of one of this node's
	// local configuration files - see secrets.go, which is where the
	// whole reason this kind exists lives. A NodeConfig artifact holds
	// the non-secret fields and nothing else; the secrets are named in
	// the manifest's SecretGaps and never written down at all.
	KindNodeConfig ArtifactKind = "node_config"

	// KindImage is a stored installation image payload.
	KindImage ArtifactKind = "image"

	// KindReference names something that is re-fetchable and
	// re-verified against a recorded digest on demand. It has no
	// payload, so RelativePath must be empty and Size must be zero -
	// Validate enforces both, because a "reference" that quietly has a
	// file behind it is a reference nobody re-verified.
	KindReference ArtifactKind = "reference"
)

// extensionByKind is the filename suffix Store writes for each kind. It
// is a purely local naming convention; the content is opaque either
// way, and Restore reads the bytes through the kind recorded in the
// manifest, never through the suffix.
var extensionByKind = map[ArtifactKind]string{
	KindDataset:    ".stream",
	KindConfig:     ".json",
	KindNodeConfig: ".json",
	KindImage:      ".bin",
	KindReference:  "", // no payload, no file
}

// hasPayload reports whether a kind is expected to be backed by a file
// in the generation's artifacts directory.
func (k ArtifactKind) hasPayload() bool { return k != KindReference }

// DatasetArtifact describes one captured dataset stream. The stream
// itself is never parsed by this package; these are the facts a restore
// needs in order to decide whether it is even eligible to run, plus the
// two fields reserved for the recursive, multi-pool case ADR-0131 says
// v1 will not produce but the schema must not have to grow to accept.
type DatasetArtifact struct {
	// Dataset is the dataset name the stream came from.
	Dataset string `json:"dataset,omitempty"`
	// Snapshot is the snapshot name within that dataset.
	Snapshot string `json:"snapshot,omitempty"`
	// SourceGUID is the dataset's ZFS-style GUID at capture time. A
	// restore compares it against the live dataset before clobbering
	// anything; an empty GUID makes an in-place restore INELIGIBLE,
	// because a guard that cannot be evaluated is not a passing guard.
	SourceGUID string `json:"source_guid,omitempty"`
	// SnapshotTakenUnix is when the snapshot this stream was read from
	// was actually created, as reported by whoever created it. It is
	// what EarliestSnapshotUnix and LatestSnapshotUnix are computed
	// from, and it is kept per artifact so a reader can see the spread
	// rather than only its endpoints.
	SnapshotTakenUnix int64 `json:"snapshot_taken_unix,omitempty"`
	// Recursive and ParentDataset are reserved for v2 coordinated sets.
	// They are present, validated for consistency, and never set by this
	// package's capture engine - a v1 producer that set them would be
	// claiming a coordinated set it did not take.
	Recursive     bool   `json:"recursive,omitempty"`
	ParentDataset string `json:"parent_dataset,omitempty"`
}

// ConfigArtifact is a workload's definition plus the configuration the
// node actually rendered for it. definition and rendered_config are
// stored as opaque bytes with the kind recorded alongside, so this
// package never has to know what either encoding is, and can therefore
// never be broken by a change to either schema.
type ConfigArtifact struct {
	// CellKind is "vm" or "jail" - the workload kind, recorded so a
	// reader can interpret the bytes without a second lookup.
	CellKind string `json:"cell_kind,omitempty"`
	// CellID is the workload's identifier.
	CellID string `json:"cell_id,omitempty"`
	// Definition is the marshalled definition, opaque to this package.
	Definition []byte `json:"definition,omitempty"`
	// RenderedConfig is the node-local rendered configuration,
	// likewise opaque here.
	RenderedConfig []byte `json:"rendered_config,omitempty"`
}

// ImageArtifact describes a stored installation image. Both digests are
// recorded: the digest the catalog expected, and the digest actually
// observed at capture. Recording only one of them would hide exactly
// the case worth noticing, which is a payload that did not match what
// the catalog claimed.
type ImageArtifact struct {
	Name              string `json:"name,omitempty"`
	Kind              string `json:"kind,omitempty"`
	ExpectedSHA256Hex string `json:"expected_sha256_hex,omitempty"`
	ObservedSHA256Hex string `json:"observed_sha256_hex,omitempty"`
	SizeBytes         uint64 `json:"bytes,omitempty"`
}

// ReferenceArtifact names something re-fetchable, with no payload. The
// recorded digest is what makes it verifiable without a copy: the fetch
// path re-hashes whatever it retrieves and compares, which is what
// internal/isostore's and internal/freebsdimg's own save-time checks
// already do.
type ReferenceArtifact struct {
	Name              string `json:"name,omitempty"`
	Kind              string `json:"kind,omitempty"`
	ExpectedSHA256Hex string `json:"expected_sha256_hex,omitempty"`
}

// Artifact is one item in a generation. Exactly one of the typed
// payloads is set, chosen by Kind; storing all five in a Go struct with
// omitempty rather than a protobuf oneof is deliberate and is explained
// in codec.go alongside the encoding choice.
type Artifact struct {
	// ArtifactID identifies the artifact within its generation and
	// forms its filename. It must be unique, and must be a plain
	// single path element - no separators, no dots-only names, nothing
	// that could escape the artifacts directory.
	ArtifactID string `json:"artifact_id,omitempty"`
	// Kind selects which of the payloads below is authoritative.
	Kind ArtifactKind `json:"kind,omitempty"`
	// RelativePath is the artifact's location relative to the
	// generation directory, e.g. "artifacts/<id>.stream". Empty for a
	// reference, which has no payload.
	RelativePath string `json:"relative_path,omitempty"`
	// SHA256 is the digest of the bytes as they landed on disk, computed
	// while they were written. It is hex, lowercase. A payload artifact
	// must have one; a reference must not.
	SHA256 string `json:"sha256,omitempty"`
	// SizeBytes is the number of payload bytes. A payload artifact must
	// have one recorded even when it is legitimately zero, so a zero
	// here is validated against the actual file size rather than
	// assumed.
	SizeBytes uint64 `json:"bytes,omitempty"`

	// Exactly one of these is set, per Kind.
	Dataset   *DatasetArtifact   `json:"dataset,omitempty"`
	Config    *ConfigArtifact    `json:"config,omitempty"`
	Image     *ImageArtifact     `json:"image,omitempty"`
	Reference *ReferenceArtifact `json:"reference,omitempty"`
}

// SecretGap records one thing a restore operator will have to obtain
// out of band, and where the live copy of it lives. Gaps are generated
// from the deny list in secrets.go rather than written by hand, which is
// what makes "the manifest never mentions a secret" and "the manifest
// always says which secrets are missing" the same fact instead of two
// things that can drift apart.
type SecretGap struct {
	// Kind identifies the excluded field.
	Kind string `json:"kind,omitempty"`
	// Why is the one-line reason it is excluded.
	Why string `json:"why,omitempty"`
	// LiveCopyLocation is where the real value is today, so an operator
	// holding a manifest knows where to go.
	LiveCopyLocation string `json:"live_copy_location,omitempty"`
	// RestoreProcedure is what a human has to do to get a working
	// credential back. Silence here is how a restored node comes up
	// mysteriously unauthenticated.
	RestoreProcedure string `json:"restore_procedure,omitempty"`
}

// Manifest is one backup generation's index. It is the only thing this
// package reads to know what a backup is; the artifacts it names are
// opaque bytes it never interprets.
//
// The JSON tags are taken verbatim from the ADR-0131 proto sketch so
// that generating api/internalpb/backup.proto later is a field-for-field
// mapping rather than a rename, and so that a manifest written by this
// package and one written by the future proto implementation are
// diffable against the same document.
type Manifest struct {
	// FormatVersion is this envelope's own schema version. Zero is
	// invalid, not "unversioned".
	FormatVersion uint32 `json:"format_version,omitempty"`
	// CreatedUnix is when this manifest was built.
	CreatedUnix int64 `json:"created_unix,omitempty"`
	// CombID is the node the data came from.
	CombID string `json:"comb_id,omitempty"`
	// ColonyID identifies the Colony the node belonged to at capture.
	ColonyID string `json:"colony_id,omitempty"`
	// AppliedIndex is the raft applied index the capture observed. It
	// is provenance: it records which control-plane state was current
	// when the data was taken, and nothing in this package treats it as
	// a correctness input, because this package never reads or writes
	// raft state.
	AppliedIndex uint64 `json:"applied_index,omitempty"`
	// PolicyID and GenerationID identify the generation. GenerationID
	// is also the generation directory's name, so a manifest found in
	// a directory that disagrees with it is rejected.
	PolicyID     string `json:"policy_id,omitempty"`
	GenerationID string `json:"generation_id,omitempty"`
	// Consistency is the label. Never empty - see Consistency.
	Consistency Consistency `json:"consistency,omitempty"`
	// Quiesce is the evidence backing a ConsistencyQuiesced label.
	// Required when Consistency is quiesced, and meaningless (but
	// tolerated) otherwise.
	Quiesce *Quiesce `json:"quiesce,omitempty"`
	// EarliestSnapshotUnix and LatestSnapshotUnix bound the window the
	// generation's snapshots were taken across. A set of datasets
	// snapshotted one after another is not atomic across the set, and
	// the width of that window is the honest answer to "is this one
	// instant".
	EarliestSnapshotUnix int64 `json:"earliest_snapshot_unix,omitempty"`
	LatestSnapshotUnix   int64 `json:"latest_snapshot_unix,omitempty"`
	// MaxSkewSeconds is the policy's limit. It is enforced for a
	// quiesced generation and recorded-but-not-enforced for a snapshot
	// one, because a snapshot generation never claimed the window was
	// tight.
	MaxSkewSeconds int64 `json:"max_skew_seconds,omitempty"`
	// Artifacts is the set, sorted by ArtifactID, with exactly one
	// generation's worth of payload in it.
	Artifacts []Artifact `json:"artifacts,omitempty"`
	// SecretGaps is generated from secrets.go's deny list and is
	// therefore the same on every manifest: what is deliberately not in
	// here, and where to get it.
	SecretGaps []SecretGap `json:"secret_gaps,omitempty"`
	// Checksum is SHA-256 over the encoded manifest body with this
	// field omitted - the same self-describing, checksummed envelope
	// discipline internalpb.ConfigArchive already uses, and for the same
	// reason: a portable archive sits outside the directory that
	// produced it, possibly for a long time, before anyone reads it.
	Checksum string `json:"checksum,omitempty"`
}

// skew returns the width of the capture window in seconds, and whether
// the manifest records a complete window at all.
func (m *Manifest) skew() (int64, bool) {
	if m.EarliestSnapshotUnix == 0 || m.LatestSnapshotUnix == 0 {
		return 0, false
	}
	return m.LatestSnapshotUnix - m.EarliestSnapshotUnix, true
}

// Claims reports whether this manifest asserts the given guarantee.
//
// The asymmetry is the point. A ConsistencySnapshot generation never
// claims ConsistencyQuiesced, in any code path, because it did not stop
// anything. A ConsistencyQuiesced generation claims only what it carries
// evidence for: if the evidence is absent, Claims says false and
// Validate has already refused to let the manifest exist. A caller
// asking "may I treat this as a single instant?" gets a false for a
// snapshot generation rather than a hopeful true.
func (m *Manifest) Claims(level Consistency) bool {
	switch level {
	case ConsistencySnapshot:
		// A quiesced generation does satisfy the weaker claim; it is a
		// strictly stronger guarantee, not a different one.
		return m.Consistency == ConsistencySnapshot || m.Consistency == ConsistencyQuiesced
	case ConsistencyQuiesced:
		return m.Consistency == ConsistencyQuiesced && m.Quiesce != nil
	default:
		return false
	}
}

// TotalBytes sums the recorded payload sizes. It is derived, not
// stored, so it cannot disagree with the artifacts it summarises.
func (m *Manifest) TotalBytes() uint64 {
	var total uint64
	for _, a := range m.Artifacts {
		if a.Kind.hasPayload() {
			total += a.SizeBytes
		}
	}
	return total
}

// Artifact returns the named artifact.
func (m *Manifest) Artifact(id string) (Artifact, bool) {
	for _, a := range m.Artifacts {
		if a.ArtifactID == id {
			return a, true
		}
	}
	return Artifact{}, false
}

// Validate is the write-side gate. Everything Commit and every capture
// must clear lives here, and each failure is a distinct wrapped error
// so a caller can tell an operator exactly which rule stopped them -
// a backup job that reports "the manifest was rejected" without saying
// which part is rejected sends the operator looking in the wrong place.
//
// Validate does not compute or check the checksum; encoding does that
// (see codec.go), because the checksum is a property of the bytes and
// a struct cannot know its own encoding.
func (m *Manifest) Validate() error {
	if m.FormatVersion == 0 {
		return fmt.Errorf("%w: format_version is 0, which is not a version this format has ever had", ErrMissingFormatVersion)
	}
	if m.FormatVersion != FormatVersion {
		return &VersionError{Found: m.FormatVersion, Supported: FormatVersion}
	}
	if m.GenerationID == "" {
		return fmt.Errorf("%w: generation_id is empty", ErrInvalidManifest)
	}
	if m.PolicyID == "" {
		return fmt.Errorf("%w: policy_id is empty", ErrInvalidManifest)
	}

	// The consistency label. Empty is refused rather than defaulted,
	// because "we did not record which guarantee this is" is not the
	// same claim as either guarantee and must never be rendered as one.
	if !m.Consistency.Valid() {
		return fmt.Errorf("%w: consistency is %q, which is not one of %q or %q - an unlabelled backup states no guarantee and is refused rather than guessed at",
			ErrInvalidManifest, m.Consistency, ConsistencySnapshot, ConsistencyQuiesced)
	}
	if m.Consistency == ConsistencyQuiesced {
		switch {
		case m.Quiesce == nil:
			return fmt.Errorf("%w: consistency is %q but the manifest carries no quiesce evidence, so the label is unbacked", ErrInvalidManifest, ConsistencyQuiesced)
		case m.Quiesce.CellID == "":
			return fmt.Errorf("%w: quiesce evidence names no Cell", ErrInvalidManifest)
		case m.Quiesce.ObservedAtUnix == 0:
			return fmt.Errorf("%w: quiesce evidence carries no observation time", ErrInvalidManifest)
		case m.Quiesce.Method == "":
			// The method is what distinguishes "observed stopped"
			// from "a call that returned no error was mistaken for
			// stopped", which is the failure this whole mode exists to
			// rule out. A blank method leaves that indistinguishable.
			return fmt.Errorf("%w: quiesce evidence names no observation method, so 'observed stopped' cannot be told apart from 'assumed stopped'", ErrInvalidManifest)
		}
	}
	if m.Consistency == ConsistencySnapshot && m.Quiesce != nil {
		// Not fatal - a producer may legitimately have quiesced and then
		// chosen the weaker label - but it is recorded, because a
		// snapshot-labelled manifest carrying quiesce evidence is
		// exactly the shape a reader should be suspicious of.
		m.Quiesce = nil
	}

	if m.EarliestSnapshotUnix > m.LatestSnapshotUnix {
		return fmt.Errorf("%w: earliest_snapshot_unix (%d) is after latest_snapshot_unix (%d)", ErrInvalidManifest, m.EarliestSnapshotUnix, m.LatestSnapshotUnix)
	}
	if m.LatestSnapshotUnix == 0 {
		return fmt.Errorf("%w: latest_snapshot_unix is 0, so the generation does not say when its data was captured", ErrInvalidManifest)
	}
	if skew, ok := m.skew(); ok && m.Consistency == ConsistencyQuiesced && m.MaxSkewSeconds > 0 && skew > m.MaxSkewSeconds {
		return fmt.Errorf("%w: the quiesced generation's snapshots span %ds, over the %ds limit - these are not one instant",
			ErrSkewExceeded, skew, m.MaxSkewSeconds)
	}

	seen := make(map[string]struct{}, len(m.Artifacts))
	for i, a := range m.Artifacts {
		if err := validateArtifact(a); err != nil {
			return fmt.Errorf("artifact %d (%q): %w", i, a.ArtifactID, err)
		}
		// Canonicalise rather than compare: the digest is the one field
		// whose two encodings would otherwise be byte-different
		// documents with identical meaning, and the self-checksum is
		// computed over bytes.
		if a.SHA256 != "" {
			a.SHA256 = canonicalDigest(a.SHA256)
			m.Artifacts[i].SHA256 = a.SHA256
		}
		if _, dup := seen[a.ArtifactID]; dup {
			return fmt.Errorf("%w: artifact_id %q appears more than once", ErrInvalidManifest, a.ArtifactID)
		}
		seen[a.ArtifactID] = struct{}{}
	}
	// Sorting here rather than requiring the caller to sort is
	// deliberate: the manifest is the generation's identity, and two
	// byte-different encodings of the same generation would break both
	// the self-checksum and any later comparison between archives.
	sort.Slice(m.Artifacts, func(i, j int) bool { return m.Artifacts[i].ArtifactID < m.Artifacts[j].ArtifactID })

	for i := range m.SecretGaps {
		if m.SecretGaps[i].Kind == "" {
			return fmt.Errorf("%w: secret gap %d has no kind", ErrInvalidManifest, i)
		}
	}
	return nil
}

// validateArtifact enforces everything about one artifact that does not
// require reading its bytes.
func validateArtifact(a Artifact) error {
	if a.ArtifactID == "" {
		return fmt.Errorf("%w: artifact_id is empty", ErrInvalidManifest)
	}
	if err := validArtifactID(a.ArtifactID); err != nil {
		return err
	}
	if _, known := extensionByKind[a.Kind]; !known {
		return fmt.Errorf("%w: kind %q is not one of %v", ErrInvalidManifest, a.Kind, knownKinds())
	}

	// The path rules. A relative path that is absolute, or that climbs
	// out with "..", or that is not under artifacts/, is refused: a
	// manifest is a document this package will later use to open files,
	// and it may have been written by a different binary, copied by
	// hand, or come off removable media.
	if a.Kind.hasPayload() {
		if a.RelativePath == "" {
			return fmt.Errorf("%w: kind %q has a payload but no relative_path", ErrInvalidManifest, a.Kind)
		}
		if err := validRelativePath(a.RelativePath); err != nil {
			return err
		}
		if a.SHA256 == "" {
			return fmt.Errorf("%w: kind %q has a payload but no sha256, so it could never be verified", ErrInvalidManifest, a.Kind)
		}
		if err := validDigest(a.SHA256); err != nil {
			return err
		}
	} else {
		if a.RelativePath != "" {
			return fmt.Errorf("%w: kind %q has no payload by definition but names relative_path %q - a reference nobody re-fetched and re-verified is not a reference", ErrInvalidManifest, a.Kind, a.RelativePath)
		}
		if a.SHA256 != "" {
			return fmt.Errorf("%w: kind %q has no payload so it must not carry a digest; the digest belongs in the reference's expected_sha256_hex, where the fetch path checks it", ErrInvalidManifest, a.Kind)
		}
		if a.SizeBytes != 0 {
			return fmt.Errorf("%w: kind %q has no payload but records %d bytes", ErrInvalidManifest, a.Kind, a.SizeBytes)
		}
		if a.Reference == nil || a.Reference.Name == "" {
			return fmt.Errorf("%w: kind %q has no payload and no reference to fetch", ErrInvalidManifest, a.Kind)
		}
	}

	// Exactly the payload its kind calls for, and no others: a dataset
	// artifact with an image payload attached is a manifest that
	// disagrees with itself, and the disagreement is what a reader
	// would act on. A KindNodeConfig artifact is the one kind with no
	// typed payload at all - it is JSON written by secrets.go's
	// allow-listed projection and stored opaquely, precisely so that
	// the secret exclusion is a property of the type that was written
	// rather than of a filter applied to a struct that could hold one.
	rule, ok := payloadRules[a.Kind]
	if !ok {
		return fmt.Errorf("%w: kind %q has no payload rule", ErrInvalidManifest, a.Kind)
	}
	for _, p := range typedPayloadsOf(a) {
		if !p.set {
			if rule.want != "" && p.kind == rule.want {
				return fmt.Errorf("%w: kind %q requires its %s payload to be set", ErrInvalidManifest, a.Kind, p.kind)
			}
			continue
		}
		if rule.want == "" {
			return fmt.Errorf("%w: kind %q carries only opaque bytes and must not carry a %s payload", ErrInvalidManifest, a.Kind, p.kind)
		}
		if p.kind != rule.want {
			return fmt.Errorf("%w: kind %q must not carry a %s payload", ErrInvalidManifest, a.Kind, p.kind)
		}
	}
	return nil
}

// payloadRule is what a kind says about its typed payload: exactly one
// must be present, or none may be. KindNodeConfig is the only kind in the
// second category, and the distinction is load-bearing rather than
// cosmetic - it is why the node config's exclusion can be a property of
// the type that wrote it rather than a filter over a struct that could
// have held a secret.
type payloadRule struct {
	want ArtifactKind // "" means no typed payload at all
	none bool
}

// payloadRules is the per-kind statement of what must be set. It is a
// table rather than a chain of comparisons so that adding a kind is one
// row, and so that "every kind has a rule" is checkable -
// TestKnownKindsMatchesTheSchema does exactly that.
var payloadRules = map[ArtifactKind]payloadRule{
	KindDataset:    {want: KindDataset},
	KindConfig:     {want: KindConfig},
	KindImage:      {want: KindImage},
	KindReference:  {want: KindReference},
	KindNodeConfig: {none: true},
}

type payloadPresence struct {
	kind ArtifactKind
	set  bool
}

func typedPayloadsOf(a Artifact) []payloadPresence {
	return []payloadPresence{
		{KindDataset, a.Dataset != nil},
		{KindConfig, a.Config != nil},
		{KindImage, a.Image != nil},
		{KindReference, a.Reference != nil},
	}
}

func knownKinds() []string {
	out := make([]string, 0, len(extensionByKind))
	for k := range extensionByKind {
		out = append(out, string(k))
	}
	sort.Strings(out)
	return out
}

// validArtifactID requires a plain single path element. The id becomes
// a filename, and a filename is the one place where "a bit of operator
// supplied text" turns into a path; this is the allowlist that keeps
// that from being interesting.
func validArtifactID(id string) error {
	if id == "" {
		return fmt.Errorf("%w: artifact_id is empty", ErrInvalidManifest)
	}
	if id == "." || id == ".." {
		return fmt.Errorf("%w: artifact_id %q is not a usable filename", ErrInvalidManifest, id)
	}
	if strings.ContainsAny(id, `/\`) {
		return fmt.Errorf("%w: artifact_id %q contains a path separator", ErrInvalidManifest, id)
	}
	if strings.HasPrefix(id, ".") {
		return fmt.Errorf("%w: artifact_id %q starts with a dot, which would hide it from a plain directory listing", ErrInvalidManifest, id)
	}
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '-', r == '_':
		default:
			return fmt.Errorf("%w: artifact_id %q contains %q; only letters, digits, '-' and '_' are allowed so an id can never become a path", ErrInvalidManifest, id, r)
		}
	}
	if len(id) > 128 {
		return fmt.Errorf("%w: artifact_id is %d characters, over the 128 limit", ErrInvalidManifest, len(id))
	}
	return nil
}

// validRelativePath requires a path that stays inside the generation
// directory, under artifacts/, and is a plain normalised relative path.
// Every clause is here because a manifest is an instruction to open
// files and manifests get copied between hosts.
func validRelativePath(rel string) error {
	if rel == "" {
		return fmt.Errorf("%w: relative_path is empty", ErrInvalidManifest)
	}
	if path.IsAbs(rel) || strings.HasPrefix(rel, "/") || strings.HasPrefix(rel, `\`) {
		return fmt.Errorf("%w: relative_path %q is absolute", ErrInvalidManifest, rel)
	}
	if strings.Contains(rel, `\`) {
		return fmt.Errorf("%w: relative_path %q contains a backslash, which is a separator on some platforms and a filename character on others", ErrInvalidManifest, rel)
	}
	if path.Clean(rel) != rel {
		return fmt.Errorf("%w: relative_path %q is not in canonical form (did you mean %q?)", ErrInvalidManifest, rel, path.Clean(rel))
	}
	for _, elem := range strings.Split(rel, "/") {
		if elem == ".." {
			return fmt.Errorf("%w: relative_path %q climbs out of the generation directory", ErrInvalidManifest, rel)
		}
	}
	dir := path.Dir(rel)
	if dir != artifactsDirName {
		return fmt.Errorf("%w: relative_path %q is not under %q/", ErrInvalidManifest, rel, artifactsDirName)
	}
	if path.Base(rel) == ManifestName {
		return fmt.Errorf("%w: relative_path %q is the manifest's own name", ErrManifestShadowed, rel)
	}
	if strings.HasPrefix(path.Base(rel), ".") {
		return fmt.Errorf("%w: relative_path %q names a dotfile, which would be invisible to a plain listing", ErrInvalidManifest, rel)
	}
	return nil
}

// validDigest checks a hex SHA-256, case-insensitively, and returns the
// canonical lowercase form the manifest should carry. A digest that is
// the wrong length or not hex is a manifest that could never be
// verified, which is worse than one with no digest at all because it
// looks verified.
func validDigest(hexDigest string) error {
	if len(hexDigest) != 64 {
		return fmt.Errorf("%w: digest %q is %d characters; a SHA-256 is 64 hex characters", ErrInvalidManifest, hexDigest, len(hexDigest))
	}
	for _, r := range hexDigest {
		switch {
		case r >= '0' && r <= '9', r >= 'a' && r <= 'f', r >= 'A' && r <= 'F':
		default:
			return fmt.Errorf("%w: digest %q is not hexadecimal", ErrInvalidManifest, hexDigest)
		}
	}
	return nil
}

// canonicalDigest lowercases a digest that validDigest has already
// accepted.
func canonicalDigest(hexDigest string) string { return strings.ToLower(hexDigest) }

// unixSeconds converts a time to the manifest's wire unit. UTC is
// forced so two nodes in different zones compute the same number for
// the same instant.
func unixSeconds(t time.Time) int64 { return t.UTC().Unix() }
