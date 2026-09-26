package backup

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

// ArtifactSpec is what a caller asks to be captured. It is a struct
// rather than a long argument list so that adding a required field later
// is a compile error at every call site rather than a silently-defaulted
// argument - the same reasoning internal/hostpkg's ApplyRequest gives.
//
// Exactly one of the typed fields should be set, matching Kind. The
// typed field carries only the facts a restore needs to decide whether it
// is eligible to run; it never carries the bytes, which arrive
// separately and are opaque here.
type ArtifactSpec struct {
	// ID identifies the artifact within its generation and becomes its
	// filename. See validArtifactID for the character rules - an id is
	// turned into a path, so it is allowlisted.
	ID string
	// Kind is what this artifact is for, not what it contains.
	Kind ArtifactKind

	// Dataset describes a captured dataset stream.
	Dataset *DatasetArtifact
	// Config describes a workload's definition and rendered config.
	Config *ConfigArtifact
	// Image describes a stored image payload.
	Image *ImageArtifact
	// Reference names something re-fetchable and digest-verifiable,
	// with no payload.
	Reference *ReferenceArtifact
}

// artifact materialises the spec into a Manifest artifact, with the
// payload path still empty - Store.WriteArtifact fills that in once it
// knows the file was actually written.
func (s ArtifactSpec) artifact() (Artifact, error) {
	if err := validArtifactID(s.ID); err != nil {
		return Artifact{}, err
	}
	a := Artifact{ArtifactID: s.ID, Kind: s.Kind}
	switch s.Kind {
	case KindDataset:
		if s.Dataset == nil {
			return Artifact{}, fmt.Errorf("%w: artifact %q is a %s but names no dataset", ErrInvalidManifest, s.ID, s.Kind)
		}
		// recursive/parent_dataset are reserved for v2. A v1 producer
		// that set them would be claiming a coordinated set it did not
		// take, so they are refused rather than stored. The fields stay
		// in the schema so that the future that does take coordinated
		// sets does not need a format bump.
		if s.Dataset.Recursive {
			return Artifact{}, fmt.Errorf("%w: artifact %q sets dataset.recursive, which this version does not produce - a recursive coordinated set is a v2 capture and claiming one here would be false", ErrInvalidManifest, s.ID)
		}
		if s.Dataset.ParentDataset != "" {
			return Artifact{}, fmt.Errorf("%w: artifact %q sets dataset.parent_dataset, which this version does not produce", ErrInvalidManifest, s.ID)
		}
		if s.Dataset.Dataset == "" {
			return Artifact{}, fmt.Errorf("%w: artifact %q is a dataset stream but names no dataset", ErrInvalidManifest, s.ID)
		}
		a.Dataset = s.Dataset
	case KindConfig:
		if s.Config == nil {
			return Artifact{}, fmt.Errorf("%w: artifact %q is a config but names none", ErrInvalidManifest, s.ID)
		}
		a.Config = s.Config
	case KindImage:
		if s.Image == nil {
			return Artifact{}, fmt.Errorf("%w: artifact %q is an image but names none", ErrInvalidManifest, s.ID)
		}
		a.Image = s.Image
	case KindReference:
		if s.Reference == nil {
			return Artifact{}, fmt.Errorf("%w: artifact %q is a reference but names none", ErrInvalidManifest, s.ID)
		}
		if s.Reference.ExpectedSHA256Hex != "" {
			if err := validDigest(s.Reference.ExpectedSHA256Hex); err != nil {
				return Artifact{}, fmt.Errorf("artifact %q: %w", s.ID, err)
			}
			s.Reference.ExpectedSHA256Hex = canonicalDigest(s.Reference.ExpectedSHA256Hex)
		}
		a.Reference = s.Reference
	case KindNodeConfig:
		// No typed payload by design: a node config artifact is the
		// allow-listed projection's JSON, stored opaquely. The exclusion
		// of the four live credentials is a property of the type that
		// produced those bytes, not a filter applied here.
		if s.Dataset != nil || s.Config != nil || s.Image != nil || s.Reference != nil {
			return Artifact{}, fmt.Errorf("%w: artifact %q is a node config, which carries opaque bytes and no typed payload", ErrInvalidManifest, s.ID)
		}
	default:
		return Artifact{}, fmt.Errorf("%w: kind %q is not one of %v", ErrInvalidManifest, s.Kind, knownKinds())
	}
	return a, nil
}

// extensionFor returns the filename suffix for a kind. A reference has
// none, because it has no file.
func extensionFor(k ArtifactKind) string { return extensionByKind[k] }

// NewReferenceSpec builds a reference artifact spec. It is a constructor
// rather than a bare struct literal so that the digest is canonicalised
// and checked in one place, and so a caller cannot accidentally create a
// reference with a payload-shaped field set.
func NewReferenceSpec(id, name, kind, expectedSHA256Hex string) ArtifactSpec {
	return ArtifactSpec{
		ID:        id,
		Kind:      KindReference,
		Reference: &ReferenceArtifact{Name: name, Kind: kind, ExpectedSHA256Hex: canonicalDigest(expectedSHA256Hex)},
	}
}

// newByteReader returns a reader over a byte slice, so a caller
// building a small artifact does not have to import bytes just to get an
// io.Reader.
func newByteReader(b []byte) io.Reader { return bytes.NewReader(b) }

// pathDir is filepath.Dir, kept as a named helper so the path handling
// in this file reads as intent.
func pathDir(p string) string { return filepath.Dir(p) }

// validID checks an identifier that becomes a path component - a policy
// id or a generation id. Same allowlist discipline as validArtifactID,
// and for the same reason: these come from configuration and from
// generated identifiers, and the one thing that must never happen is one
// of them escaping the target root.
func validID(what, id string) error {
	if id == "" {
		return fmt.Errorf("backup: %s is empty", what)
	}
	if len(id) > 128 {
		return fmt.Errorf("backup: %s is %d characters, over the 128 limit", what, len(id))
	}
	if id == "." || id == ".." {
		return fmt.Errorf("backup: %s %q is not a usable directory name", what, id)
	}
	if strings.ContainsAny(id, `/\`) {
		return fmt.Errorf("backup: %s %q contains a path separator", what, id)
	}
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '-', r == '_', r == '.':
		default:
			return fmt.Errorf("backup: %s %q contains %q; only letters, digits, '.', '-' and '_' are allowed so an id can never become a path", what, id, r)
		}
	}
	return nil
}

// GenerationSummary is one generation as a listing surface sees it.
//
// The rule throughout: every field is a fact or it is absent, and
// absence is never rendered as a value. A generation whose manifest could
// not be read reports State and Reason and nothing else, because a
// listing that filled in artifact_count for an archive it could not
// parse would be inventing evidence.
type GenerationSummary struct {
	PolicyID     string
	GenerationID string
	// Dir is the generation's directory, always known - it comes from
	// the path, not from the manifest.
	Dir string
	// ManifestPath is the path a manifest was looked for at.
	ManifestPath string

	State  GenerationState
	Reason string

	FormatVersion    uint32
	Consistency      Consistency
	CreatedUnix      int64
	ArtifactCount    int
	PayloadBytes     uint64
	SecretGapCount   int
	CombID           string
	AppliedIndex     uint64
	EarliestSnapshot int64
	LatestSnapshot   int64
}

// IsBackup reports whether this directory is a backup at all. It is
// false for GenerationIncomplete by design: no manifest means nothing
// was ever published, so the honest answer is "this is not a backup",
// not "this is a backup of unknown quality".
func (s GenerationSummary) IsBackup() bool { return s.State != GenerationIncomplete }

// List returns every generation directory under a policy, newest
// identifier last.
//
// It never returns an error for a generation it cannot read. A damaged
// or un-understandable archive is a fact about the target, and a listing
// that failed entirely because one archive was from the future would
// hide the other nineteen. Each entry carries its own state and reason
// instead, and a caller rendering the listing shows the reason next to
// the one generation that needs it.
//
// A policy with no directory at all is not an error either: nothing has
// been captured yet, which is a fact rather than a fault.
func (s *Store) List(policyID string) ([]GenerationSummary, error) {
	if err := validID("policy_id", policyID); err != nil {
		return nil, err
	}
	policyDir := joinPath(s.root, policyID)
	entries, err := s.fs.ReadDir(policyDir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("listing %s: %w", policyDir, err)
	}
	var out []GenerationSummary
	for _, e := range entries {
		if !e.IsDir() {
			// A stray file beside the generation directories. Reported
			// as incomplete rather than skipped: something wrote here
			// that this package did not, and an operator should be able
			// to see that.
			out = append(out, GenerationSummary{
				PolicyID:     policyID,
				GenerationID: e.Name(),
				Dir:          joinPath(policyDir, e.Name()),
				ManifestPath: joinPath(policyDir, e.Name(), ManifestName),
				State:        GenerationIncomplete,
				Reason:       fmt.Sprintf("%s is a file where a generation directory was expected; it is not a backup and nothing published a manifest into it", e.Name()),
			})
			continue
		}
		out = append(out, s.summarize(policyID, e.Name()))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].GenerationID < out[j].GenerationID })
	return out, nil
}

// summarize reads one generation directory and reports what it is.
// Every failure to read becomes a named state, never a skip and never a
// panic.
func (s *Store) summarize(policyID, generationID string) GenerationSummary {
	dir := s.GenerationDir(policyID, generationID)
	sum := GenerationSummary{
		PolicyID:     policyID,
		GenerationID: generationID,
		Dir:          dir,
		ManifestPath: joinPath(dir, ManifestName),
		State:        GenerationIncomplete,
	}
	data, err := s.fs.ReadFile(sum.ManifestPath)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			sum.Reason = "no " + ManifestName + " was ever published here, so this is a partial or abandoned generation rather than a backup"
			return sum
		}
		sum.State = GenerationUnreadable
		sum.Reason = fmt.Sprintf("%s could not be read: %v - whether it was ever a valid backup is unknown", sum.ManifestPath, err)
		return sum
	}
	m, info, err := ReadManifest(s.codec, sum.ManifestPath, data)
	sum.State = info.State
	sum.Reason = info.Reason
	sum.FormatVersion = info.FormatVersion
	if err != nil || m == nil {
		return sum
	}
	sum.Consistency = m.Consistency
	sum.CreatedUnix = m.CreatedUnix
	sum.ArtifactCount = len(m.Artifacts)
	sum.PayloadBytes = m.TotalBytes()
	sum.SecretGapCount = len(m.SecretGaps)
	sum.CombID = m.CombID
	sum.AppliedIndex = m.AppliedIndex
	sum.EarliestSnapshot = m.EarliestSnapshotUnix
	sum.LatestSnapshot = m.LatestSnapshotUnix
	return sum
}

// Load reads and judges a generation's manifest.
//
// The four returns are not a convenience. A caller restoring from an
// archive needs to know: what the manifest says (m), whether it is
// safe to act on (info.Understood), what the reader could establish
// about the file itself (info), and - when something went wrong - the
// reason in words rather than having to parse an error string (err).
//
// When info.Understood is false, m is either nil or explicitly
// untrusted, and Restore refuses it. That refusal is the whole of the
// unknown-version rule: not "try anyway", not "probably compatible".
func (s *Store) Load(policyID, generationID string) (*Manifest, ReadInfo, error) {
	if err := validID("policy_id", policyID); err != nil {
		return nil, ReadInfo{}, err
	}
	if err := validID("generation_id", generationID); err != nil {
		return nil, ReadInfo{}, err
	}
	dir := s.GenerationDir(policyID, generationID)
	mpath := joinPath(dir, ManifestName)
	data, err := s.fs.ReadFile(mpath)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			info := ReadInfo{
				Path:   mpath,
				State:  GenerationIncomplete,
				Reason: "no " + ManifestName + " was ever published in " + dir + ", so there is no backup here - the artifacts, if any, are a partial capture",
			}
			return nil, info, fmt.Errorf("backup: %s: %w", mpath, ErrArtifactMissing)
		}
		info := ReadInfo{Path: mpath, State: GenerationUnreadable, Reason: fmt.Sprintf("%s could not be read: %v", mpath, err)}
		return nil, info, fmt.Errorf("backup: %s: %w", mpath, err)
	}
	return ReadManifest(s.codec, mpath, data)
}

// LoadDir reads a generation directory that is not necessarily under a
// Store's root. It is how an archive that was copied off a dead host -
// which is the entire reason archives are portable and self-describing
// - gets read back, with the same rules and the same refusals.
func LoadDir(dir string, opts ...Option) (*Manifest, ReadInfo, error) {
	s := NewStore(dir, opts...)
	mpath := joinPath(dir, ManifestName)
	data, err := s.fs.ReadFile(mpath)
	if err != nil {
		state := GenerationUnreadable
		reason := fmt.Sprintf("%s could not be read: %v", mpath, err)
		if errors.Is(err, fs.ErrNotExist) {
			state = GenerationIncomplete
			reason = "no " + ManifestName + " in " + dir + ", so this directory is not a backup"
		}
		return nil, ReadInfo{Path: mpath, State: state, Reason: reason}, fmt.Errorf("backup: %s: %w", mpath, err)
	}
	return ReadManifest(s.codec, mpath, data)
}

// SweepResult reports what a sweep did. Removed lists the generation
// directories it deleted, and Refused lists the ones it deliberately did
// not.
//
// A sweep only removes a directory that has no manifest. It never
// removes one that does, whatever else it believes about it: a
// generation whose manifest is from the future, or damaged, or
// internally inconsistent, is still a generation somebody's only copy
// lives in, and deleting it because this build cannot read it is exactly
// the wrong response to "this build cannot read it".
type SweepResult struct {
	PolicyID string
	Removed  []string
	Refused  []RefusedGeneration
}

// RefusedGeneration is a directory a sweep left alone, and why.
type RefusedGeneration struct {
	GenerationID string
	State        GenerationState
	Reason       string
}

// Sweep removes generation directories under a policy that have no
// manifest: interrupted captures, jobs killed mid-stream, a crash
// between the last artifact and the manifest write. Those directories
// are the residue of all-or-nothing, and leaving them to accumulate is
// how a target silently fills up with data nothing will ever read.
//
// It is a separate call from Commit's Abort because a crash cannot call
// Abort. Abort is the polite path; Sweep is the one that runs after an
// unattended power loss and has to be equally careful about what it
// removes.
func (s *Store) Sweep(policyID string) (SweepResult, error) {
	res := SweepResult{PolicyID: policyID}
	if err := validID("policy_id", policyID); err != nil {
		return res, err
	}
	policyDir := joinPath(s.root, policyID)
	entries, err := s.fs.ReadDir(policyDir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return res, nil
		}
		return res, fmt.Errorf("listing %s: %w", policyDir, err)
	}
	for _, e := range entries {
		// Files are checked before the manifest is stat'ed, because a
		// stray file makes the stat fail with ENOTDIR rather than
		// ENOENT - and "there is a file here that is not a generation" is
		// a definite answer, whereas the ENOTDIR would be reported as
		// the much weaker "could not determine whether a manifest
		// exists".
		if !e.IsDir() {
			res.Refused = append(res.Refused, RefusedGeneration{
				GenerationID: e.Name(),
				State:        GenerationIncomplete,
				Reason:       fmt.Sprintf("%s is a file with no %s beside it; removing a file this package did not create is not its call to make", e.Name(), ManifestName),
			})
			continue
		}
		dir := joinPath(policyDir, e.Name())
		mpath := joinPath(dir, ManifestName)
		if _, err := s.fs.Stat(mpath); err == nil {
			// A manifest exists. Refuse, whatever this build thinks of
			// it, and say why.
			sum := s.summarize(policyID, e.Name())
			res.Refused = append(res.Refused, RefusedGeneration{
				GenerationID: e.Name(),
				State:        sum.State,
				Reason: fmt.Sprintf("a %s is present (%s), so this is a published backup and is not garbage; "+
					"an archive this build cannot read is still the only copy of something",
					ManifestName, sum.Reason),
			})
			continue
		} else if !errors.Is(err, fs.ErrNotExist) {
			res.Refused = append(res.Refused, RefusedGeneration{
				GenerationID: e.Name(),
				State:        GenerationUnreadable,
				Reason:       fmt.Sprintf("could not determine whether %s has a %s (%v), so nothing was removed - an inability to check is not permission to delete", mpath, ManifestName, err),
			})
			continue
		}
		if err := s.fs.RemoveAll(dir); err != nil {
			return res, fmt.Errorf("removing the partial generation at %s: %w", dir, err)
		}
		res.Removed = append(res.Removed, e.Name())
	}
	sort.Strings(res.Removed)
	sort.Slice(res.Refused, func(i, j int) bool { return res.Refused[i].GenerationID < res.Refused[j].GenerationID })
	return res, nil
}

// GenerationStateOfDir is a small helper for a surface that has a
// directory and wants a verdict without the whole summary. It exists so
// there is exactly one implementation of "look, and report what you
// found" - a second one would eventually be the one that skips the
// unreadable case.
func (s *Store) GenerationStateOfDir(policyID, generationID string) GenerationState {
	return s.summarize(policyID, generationID).State
}

// artifactRelativePath is a helper for the materialise and verify paths,
// which both need to turn a manifest artifact into a path inside a
// generation directory and both must refuse anything that is not one.
func artifactRelativePath(dir string, a Artifact) (string, error) {
	if err := validRelativePath(a.RelativePath); err != nil {
		return "", err
	}
	full := joinPath(dir, a.RelativePath)
	// Belt and braces: confirm the joined result is still inside dir.
	// validRelativePath already refuses "..", an absolute path, and a
	// path not under artifacts/, so this cannot fail - but a restore path
	// that opens files is the last place a redundant check is free.
	rel, err := filepath.Rel(dir, full)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("%w: artifact path %q resolves outside %s", ErrInvalidManifest, a.RelativePath, dir)
	}
	return full, nil
}

// nowUnix is the store's clock in the manifest's wire unit.
func (s *Store) nowUnix() int64 { return unixSeconds(s.now()) }

// contextErr turns a cancelled context into a wrapped error naming what
// was being attempted, so a cancellation never surfaces as a bare
// "context canceled" with no indication of which step stopped.
func contextErr(what string, ctx context.Context) error {
	return fmt.Errorf("backup: %s: %w", what, ctx.Err())
}

// pathClean is path.Clean, named so the callers in this package read as
// a statement about a path rather than a string operation.
func pathClean(p string) string { return path.Clean(p) }
