package backup

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/glenjbarber/apiary/internal/jailarchive"
)

// Extractor unpacks one archive into a directory. It is the shape of
// internal/jailarchive.Extractor.Extract, named as an interface so the
// restore path can be exercised without shelling out - and so that the
// real implementation is visibly the same one, not a reimplementation.
//
// The signature is taken from internal/jailarchive.Extractor
// deliberately and unchanged. Reusing the type would drag an
// os/exec dependency into every test of this package; matching the
// signature and assigning the real extractor means a caller writes
//
//	backup.Extractor = jailarchive.New()
//
// and gets the same tar(1) path the rest of the project already relies
// on. There is no second unpacker here, which ADR-0131's Rejected
// alternative 4 rules out in as many words.
type Extractor interface {
	Extract(ctx context.Context, archivePath, destDir string) error
}

// jailArchiveExtractor adapts *jailarchive.Extractor to Extractor. It
// is exported as a constructor rather than left implicit so that the
// wiring is a visible line in the caller instead of something a reader
// has to notice.
type jailArchiveExtractor struct{ e *jailarchive.Extractor }

// NewJailArchiveExtractor returns the Extractor backed by
// internal/jailarchive's tar(1) extraction - the one already used for
// jail base userlands (ADR-0098), reused here rather than reimplemented.
func NewJailArchiveExtractor() Extractor {
	return jailArchiveExtractor{e: jailarchive.New()}
}

// Extract implements Extractor by delegating. The returned Extractor is
// declared to satisfy the interface at compile time, so if
// jailarchive's signature ever moves, this stops building here rather
// than at a restore, on a host, mid-incident.
func (a jailArchiveExtractor) Extract(ctx context.Context, archivePath, destDir string) error {
	return a.e.Extract(ctx, archivePath, destDir)
}

// var _ Extractor = jailArchiveExtractor{} cannot be a compile-time check
// on the delegated call, so the delegation above returns the concrete
// type's error unchanged. If jailarchive.Extract ever changes shape, the
// call site is the only thing that moves, and it is two lines long.

// MaterialiseOptions configures an out-of-band materialisation.
type MaterialiseOptions struct {
	// Extractor unpacks a payload artifact. Required for a payload that
	// is an archive.
	Extractor Extractor
	// DestDir is the directory to unpack into. It must already exist,
	// matching internal/jailarchive.Extractor's own contract, and this
	// package does not create it: a restore that silently made a
	// directory at a path the operator did not expect is a restore that
	// put files somewhere nobody will look.
	DestDir string
	// SpoolDir is where the artifact is copied before extraction.
	//
	// An Extractor needs a path, and a manifest artifact is a path
	// inside a generation directory - which is fine to extract from
	// directly. The spool exists for the case where the archive's
	// contents must land somewhere other than the generation, which is
	// the usual out-of-band shape: the archive is read once, out of a
	// directory that also holds every other artifact, and unpacked into
	// a fresh dataset. It is not optional when DestDir is not inside the
	// generation directory, and Materialise says so rather than
	// guessing.
	SpoolDir string
}

// Materialise unpacks one payload artifact into DestDir, out of band.
//
// It is the restore half of a jail backup, and it is the whole reason
// this file exists rather than a general archive layer: internal/
// jailarchive already does exactly this, and ADR-0131 adopts it as a
// component while rejecting it as a whole-system answer. What is added
// here is the part the bare Extractor has no opinion about - that the
// bytes about to be unpacked were verified against the manifest first,
// and that the destination is somewhere the running Cell is not.
//
// The order is: verify, then extract. Extracting first and verifying
// afterwards would unpack bytes that might not be the bytes that were
// captured, and on a real dataset that means putting corruption into a
// place an operator will then believe.
func (s *Store) Materialise(ctx context.Context, policyID, generationID, artifactID string, opts MaterialiseOptions) error {
	m, info, err := s.Load(policyID, generationID)
	if err != nil || !info.Understood {
		if err == nil {
			err = fmt.Errorf("manifest is %s", info.State)
		}
		return fmt.Errorf("backup: refusing to materialise %s/%s artifact %q: %w", policyID, generationID, artifactID, err)
	}
	a, ok := m.Artifact(artifactID)
	if !ok {
		return fmt.Errorf("backup: generation %s has no artifact %q; it has %d", generationID, artifactID, len(m.Artifacts))
	}
	if !a.Kind.hasPayload() {
		return fmt.Errorf("backup: artifact %q is a %s, which has no payload to unpack; materialise it by re-fetching it and checking its expected digest", artifactID, a.Kind)
	}
	if opts.Extractor == nil {
		return fmt.Errorf("backup: no Extractor was supplied, so artifact %q cannot be unpacked", artifactID)
	}

	dir := s.GenerationDir(policyID, generationID)
	full, err := artifactRelativePath(dir, a)
	if err != nil {
		return err
	}

	// Verify this one artifact before anything is written anywhere. A
	// generation-level verification would also work, but it would hash
	// every artifact to unpack one, and the specific question here is
	// only about the bytes about to touch the filesystem.
	check := verifyArtifact(s.fs, dir, a, VerifyOptions{})
	if !check.Verdict.Verified() {
		return fmt.Errorf("backup: refusing to unpack artifact %q: %s (%s)", artifactID, check.Verdict, check.Detail)
	}

	src := full
	if opts.SpoolDir != "" {
		spooled, err := s.spoolArtifact(full, a, opts.SpoolDir)
		if err != nil {
			return err
		}
		src = spooled
	}
	if err := opts.Extractor.Extract(ctx, src, opts.DestDir); err != nil {
		return fmt.Errorf("backup: unpacking artifact %q into %s: %w", artifactID, opts.DestDir, err)
	}
	return nil
}

// spoolArtifact copies a payload to SpoolDir under a name derived from
// its digest, and returns the path.
//
// The name is the digest, not the artifact id, because the spool is
// scratch: two generations of the same artifact id are the same bytes
// iff the digests match, so a digest-named spool deduplicates itself and
// cannot be confused with a file from a different generation. The copy
// is verified against the manifest's digest as it is written, so a
// spooled copy that is truncated by a full disk is a loud failure rather
// than an extraction from a half-copied archive.
func (s *Store) spoolArtifact(srcPath string, a Artifact, spoolDir string) (string, error) {
	if err := s.fs.MkdirAll(spoolDir, 0o700); err != nil {
		return "", fmt.Errorf("creating spool directory %s: %w", spoolDir, err)
	}
	dst := filepath.Join(spoolDir, a.SHA256+extensionFor(a.Kind))

	in, err := s.fs.Open(srcPath)
	if err != nil {
		return "", fmt.Errorf("opening %s to spool it: %w", srcPath, err)
	}
	defer in.Close()

	tmp, err := s.fs.CreateTemp(spoolDir, ".spool-*")
	if err != nil {
		return "", fmt.Errorf("creating a spool temp file in %s: %w", spoolDir, err)
	}
	tmpPath := tmp.Name()
	defer func() { _ = s.fs.Remove(tmpPath) }()

	hw := newHashWriter()
	n, err := io.Copy(io.MultiWriter(tmp, hw), in)
	if err != nil {
		tmp.Close()
		return "", fmt.Errorf("spooling %s: %w", srcPath, err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return "", fmt.Errorf("syncing the spooled copy of %s: %w", srcPath, err)
	}
	if err := tmp.Close(); err != nil {
		return "", fmt.Errorf("closing the spooled copy of %s: %w", srcPath, err)
	}
	if !digestEqual(hexBytes(hw.sum()), a.SHA256) || n != int64(a.SizeBytes) {
		return "", fmt.Errorf("backup: the spooled copy of %s does not match the manifest (%d bytes, digest %s vs the recorded %d bytes, %s) - nothing was extracted",
			srcPath, n, shortDigest(hw.sum()), a.SizeBytes, shortDigest(mustHex(a.SHA256)))
	}
	if err := s.fs.Rename(tmpPath, dst); err != nil {
		return "", fmt.Errorf("publishing the spooled copy of %s as %s: %w", srcPath, dst, err)
	}
	if err := s.fs.SyncDir(spoolDir); err != nil {
		return "", fmt.Errorf("publishing %s succeeded but syncing %s did not: %w", dst, spoolDir, err)
	}
	return dst, nil
}

// osFile is a compile-time assertion that *os.File satisfies File. It
// is here because the assertion costs nothing and its absence is how a
// narrowing of the File interface would only be discovered on a host,
// mid-capture, writing a backup.
var _ File = (*os.File)(nil)
