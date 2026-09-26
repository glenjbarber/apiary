package backup

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"strings"
	"time"
)

// ArtifactVerdict is what verifying one artifact concluded. There are
// three, and the separation between the last two is the whole point of
// this file.
//
// "The artifact is gone" and "the artifact is damaged" send an operator
// to completely different places. A missing artifact means the copy is
// incomplete - a truncated capture, a failed sync, something that
// deleted it. A damaged artifact means the bytes are there and are
// wrong - bit rot, a bad write, a lying producer. An operator who is told
// "corrupt" for a file that is not there will go looking for corruption,
// and an operator who is told "missing" for a file that is there and
// wrong will re-run the backup and possibly not check whether the target
// is failing.
//
// Folding them into one "not verified" answer is the mistake this
// vocabulary exists to prevent, and it is the same discipline as
// internal/health's and internal/cluster's separation of "not healthy"
// from "we could not tell" (ADR-0056, ADR-0118).
type ArtifactVerdict string

const (
	// ArtifactVerified means the artifact is present, readable, and its
	// bytes hash to the digest the manifest recorded. It says nothing
	// about whether the artifact can be restored anywhere - see the
	// package doc on why that tier is left open and reported as open.
	ArtifactVerified ArtifactVerdict = "verified"

	// ArtifactChecksumMismatch means the artifact is present and
	// readable, and its bytes are not the bytes the manifest describes.
	// This is corruption, and it is never reported for a file that is
	// absent.
	ArtifactChecksumMismatch ArtifactVerdict = "checksum_mismatch"

	// ArtifactMissing means the artifact is not present, or is present
	// but cannot be read - a directory where a file should be, a
	// permission error, an I/O error. The reason field says which,
	// because the operator's next step differs, but the verdict is the
	// same: absence of evidence that the artifact is intact, never
	// evidence that it is damaged.
	ArtifactMissing ArtifactVerdict = "missing"
)

// Terminal reports whether a verdict means the artifact is definitely not
// intact, as opposed to definitely being intact. A caller building an
// overall generation verdict uses this to avoid inventing a fourth
// combined state.
func (v ArtifactVerdict) Terminal() bool {
	return v == ArtifactChecksumMismatch || v == ArtifactMissing
}

// Verified reports whether the artifact was positively confirmed intact.
func (v ArtifactVerdict) Verified() bool { return v == ArtifactVerified }

// ArtifactCheck is the verification result for one artifact.
type ArtifactCheck struct {
	ArtifactID string
	Kind       ArtifactKind
	Verdict    ArtifactVerdict
	// Detail is the human-readable reason, always populated for a
	// non-verified verdict and usually empty for a verified one. It
	// names what was actually observed: the size the file has, the two
	// digests, which of several ways a read failed.
	Detail string

	ExpectedSHA256 string
	ObservedSHA256 string
	ExpectedBytes  uint64
	ObservedBytes  uint64
	// Path is where the artifact was looked for. Empty for a reference.
	Path string
}

// Verification is the result of verifying a whole generation. Every
// field is a fact; the overall state is a verdict, and the two are kept
// apart so a caller cannot accidentally render "3 of 5 artifacts are
// fine" as "the backup is fine".
type Verification struct {
	PolicyID     string
	GenerationID string
	Dir          string

	// State is the manifest-level verdict. It is GenerationOK when the
	// manifest was read and understood; when it is not, no artifact was
	// hashed at all and Results is empty, because a verifier that
	// reported per-artifact results for a manifest it could not read
	// would be reporting on a description rather than on data.
	State GenerationState
	// Reason is the human-readable version of State.
	Reason string
	// FormatVersion is the version the manifest declared, verbatim.
	FormatVersion uint32
	// ChecksumVerified records that the manifest's own checksum was
	// recomputed and matched.
	ChecksumVerified bool
	// Understood is false for an unknown version, a damaged manifest, or
	// an internally inconsistent one.
	Understood bool

	// Consistency is the label the manifest carries, reproduced here so
	// a surface cannot accidentally describe a snapshot-consistent
	// generation as quiesced by omission.
	Consistency Consistency
	// ClaimsQuiesced is the manifest's own claim, computed by
	// Manifest.Claims, never by the verifier's opinion.
	ClaimsQuiesced bool

	Results []ArtifactCheck
	// NotApplicable lists the artifacts verification deliberately did not
	// check, with a reason. A reference artifact is one of these, and an
	// artifact the pass's byte budget ran out on is another. They are
	// reported separately and never counted as verified, because hashing
	// nothing is not verification and a reference nobody re-fetched has
	// not been checked by anybody.
	NotApplicable []NotApplicableArtifact

	Verified   int
	Mismatched int
	Missing    int
	// Skipped counts the payload artifacts this pass could not afford to
	// hash. It is the coverage gap, and it is why Ok requires it to be
	// zero: "2 of 3 verified" with the third skipped is not a clean bill
	// of health, and a boolean that said otherwise is the green-without-
	// evidence failure this package's whole evidence discipline is about.
	Skipped int

	// Restorable is deliberately absent. This package does not claim a
	// generation is restorable on the strength of a checksum, and a
	// struct with no field for that claim is a field nobody can fill in
	// by accident. The restore-drill tier is ADR-0131's v2 and is
	// reported as absent, not as assumed.
	VerifiedAtUnix int64
}

// NotApplicableArtifact is an artifact verification declined to judge,
// and why. BudgetSkipped separates the two reasons, because only one of
// them is the verifier's own doing and could be changed by asking for a
// bigger budget.
type NotApplicableArtifact struct {
	ArtifactID string
	Kind       ArtifactKind
	Reason     string
	// BudgetSkipped is true when the pass ran out of hash budget, false
	// when there was nothing to hash in the first place.
	BudgetSkipped bool
}

// Ok reports whether every artifact that could be checked was verified
// AND at least one was checked. A generation with no checkable
// artifacts is not "verified" - nothing was established, and a summary
// that said "0 of 0 artifacts verified, all good" would be the exact
// green-without-evidence the project rule forbids.
func (v Verification) Ok() bool {
	return v.Understood && v.State == GenerationOK && v.Verified > 0 &&
		v.Mismatched == 0 && v.Missing == 0 && v.Skipped == 0 &&
		len(v.Results) == v.Verified
}

// Summary renders the three counts as one sentence, for a log line or a
// UI subtitle. It names the count of things that could not be checked
// separately from the count that failed, because "0 missing, 2
// unchecked" and "0 missing" are not the same message.
func (v Verification) Summary() string {
	base := fmt.Sprintf("%d verified, %d checksum mismatch, %d missing", v.Verified, v.Mismatched, v.Missing)
	if v.Skipped > 0 {
		base += fmt.Sprintf(", %d not hashed (this pass ran out of byte budget)", v.Skipped)
	}
	if refs := len(v.NotApplicable) - v.Skipped; refs > 0 {
		base += fmt.Sprintf(", %d with no payload to check", refs)
	}
	if !v.Understood {
		base += fmt.Sprintf("; no artifact was checked because the manifest itself is %s", v.State)
	}
	return base
}

// VerifyOptions configures a verification pass.
type VerifyOptions struct {
	// FS overrides the filesystem, for the same reason Store takes one.
	FS FS
	// Codec overrides the manifest encoding, for reading an archive
	// written by a different build.
	Codec Codec
	// Now supplies the verification timestamp.
	Now func() time.Time
	// HashBudgetBytes, when non-zero, is a byte budget for the whole
	// pass: whole artifacts are hashed in manifest order until the budget
	// is exhausted, and the rest are reported in NotApplicable with a
	// reason saying they were not checked and why.
	//
	// It is a budget over whole artifacts rather than a prefix of each
	// one, and that is a correctness decision rather than a performance
	// one. A manifest records one digest per artifact, so a prefix can
	// never be compared against it - a prefix check would have to invent a
	// second digest to compare against, and a verifier that invents a
	// digest is not verifying anything. ADR-0131 tier 2 says the same
	// thing in words: re-hash "a sampled subset in full".
	//
	// Artifacts past the budget are NOT counted as verified. They appear
	// in NotApplicable, which Summary reports separately, so a daily sweep
	// over multi-gigabyte generations says how much it did not check
	// rather than implying it checked everything.
	//
	// Zero means hash every byte of every artifact: the full check.
	HashBudgetBytes int64
	// SkipHash verifies presence and size only. It exists so a listing
	// can answer "is the shape of this generation intact" cheaply, and
	// any result it produces says in its own Detail that it is not a
	// verification of the payload's integrity.
	SkipHash bool
}

// Verify re-reads a generation's manifest, re-checks the manifest's own
// checksum, re-stats every artifact, and re-hashes them.
//
// The order is the same one ReadManifest uses and the same one raftd's
// loadConfigArchive uses: manifest first, artifacts second. Hashing
// artifacts named by a manifest this build does not understand would be
// verifying a description against bytes, which establishes nothing.
func Verify(ctx context.Context, dir string, opts VerifyOptions) (Verification, error) {
	filesystem := opts.FS
	if filesystem == nil {
		filesystem = OSFS{}
	}
	codec := opts.Codec
	if codec == nil {
		codec = DefaultCodec()
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}

	v := Verification{Dir: dir, VerifiedAtUnix: unixSeconds(now())}
	policyID, generationID := splitGenerationDir(dir)
	v.PolicyID, v.GenerationID = policyID, generationID

	mpath := joinPath(dir, ManifestName)
	body, readErr := filesystem.ReadFile(mpath)
	if readErr != nil {
		// Classified here rather than handed to ReadManifest as nil
		// bytes, because "the file is not there" and "the file is there
		// and does not parse" are different states with different fixes,
		// and collapsing them into one parse failure would report a
		// generation that was never published as a damaged one.
		state, reason := GenerationUnreadable, fmt.Sprintf("%s could not be read: %v", mpath, readErr)
		if errors.Is(readErr, fs.ErrNotExist) {
			state = GenerationIncomplete
			reason = "no " + ManifestName + " was ever published in " + dir + ", so this directory is not a backup: the artifacts, if any, are a partial capture and nothing was ever promised about them"
		}
		v.State, v.Reason = state, reason
		return v, fmt.Errorf("backup: %s: %w", mpath, readErr)
	}

	m, info, err := ReadManifest(codec, mpath, body)
	v.State = info.State
	v.Reason = info.Reason
	v.FormatVersion = info.FormatVersion
	v.ChecksumVerified = info.ChecksumVerified
	v.Understood = info.Understood
	if err != nil {
		// No artifact is hashed. A verification that reports per-artifact
		// verdicts for a manifest it could not read is verifying a
		// description, and its counts would read as evidence about data.
		return v, err
	}
	v.Consistency = m.Consistency
	v.ClaimsQuiesced = m.Claims(ConsistencyQuiesced)

	// The byte budget, spent in manifest order on whole artifacts. It is
	// deliberately "remaining" and not "what is left of a total that may
	// hit zero": a budget of exactly zero must stop checking, and a
	// counter that has reached zero looks identical to "no budget was
	// set" if the two are the same variable.
	remaining := opts.HashBudgetBytes
	budgetActive := remaining > 0
	for _, a := range m.Artifacts {
		if err := ctx.Err(); err != nil {
			return v, contextErr(fmt.Sprintf("verifying generation %s", generationID), ctx)
		}
		if !a.Kind.hasPayload() {
			// Not verified, not failed, and deliberately not counted in
			// Results. Listed separately with a reason so a surface can
			// say "this generation has 2 references, which were not
			// checked" instead of quietly omitting them.
			v.NotApplicable = append(v.NotApplicable, NotApplicableArtifact{
				ArtifactID: a.ArtifactID,
				Kind:       a.Kind,
				Reason:     fmt.Sprintf("a %s artifact has no payload; it records a digest that the fetch path re-checks when it retrieves the thing, and nothing here has retrieved anything", a.Kind),
			})
			continue
		}
		if budgetActive && int64(a.SizeBytes) > remaining {
			// Not enough budget left to hash this one whole, and hashing
			// part of it could not be compared against the recorded digest
			// anyway. It is reported as unchecked, never as verified.
			v.NotApplicable = append(v.NotApplicable, NotApplicableArtifact{
				ArtifactID:    a.ArtifactID,
				Kind:          a.Kind,
				BudgetSkipped: true,
				Reason: fmt.Sprintf("not hashed: this pass had %d byte(s) of budget left and this artifact is %d, and a partial hash cannot be compared against the digest the manifest records. It is NOT verified and NOT failed",
					remaining, a.SizeBytes),
			})
			v.Skipped++
			continue
		}
		if budgetActive {
			remaining -= int64(a.SizeBytes)
		}
		check := verifyArtifact(filesystem, dir, a, opts)
		v.Results = append(v.Results, check)
		switch check.Verdict {
		case ArtifactVerified:
			v.Verified++
		case ArtifactChecksumMismatch:
			v.Mismatched++
		case ArtifactMissing:
			v.Missing++
		}
	}
	return v, nil
}

// verifyArtifact checks one payload artifact. The order is: stat,
// then read, then hash. A file that is not there fails at the stat with
// ArtifactMissing, and never reaches the hash - which is what keeps
// "missing" and "mismatch" from bleeding into each other.
func verifyArtifact(filesystem FS, dir string, a Artifact, opts VerifyOptions) ArtifactCheck {
	check := ArtifactCheck{
		ArtifactID:     a.ArtifactID,
		Kind:           a.Kind,
		ExpectedSHA256: a.SHA256,
		ExpectedBytes:  a.SizeBytes,
	}
	full, err := artifactRelativePath(dir, a)
	if err != nil {
		check.Verdict = ArtifactMissing
		check.Detail = fmt.Sprintf("the manifest's recorded path for this artifact is not usable, so the artifact cannot be looked for at all: %v", err)
		return check
	}
	check.Path = full

	fi, err := filesystem.Stat(full)
	if err != nil {
		check.Verdict = ArtifactMissing
		if errors.Is(err, fs.ErrNotExist) {
			check.Detail = fmt.Sprintf("no file at %s, though the manifest records one of %d bytes with digest %s", full, a.SizeBytes, shortDigest(mustHex(a.SHA256)))
		} else {
			check.Detail = fmt.Sprintf("%s exists but could not be examined (%v), so whether it is intact is unknown - this is reported as missing rather than as corruption, because nothing was read to compare", full, err)
		}
		return check
	}
	if fi.IsDir() {
		check.Verdict = ArtifactMissing
		check.Detail = fmt.Sprintf("%s is a directory, not a file; the payload is not there to be verified", full)
		return check
	}
	check.ObservedBytes = uint64(fi.Size())

	// The size check is a mismatch, not a missing: the file is there.
	// Reporting a truncated file as "missing" would send an operator
	// looking for something that was deleted.
	if check.ObservedBytes != a.SizeBytes {
		check.Verdict = ArtifactChecksumMismatch
		check.Detail = fmt.Sprintf("%s is %d bytes and the manifest records %d - the payload is present but not the payload that was captured", full, check.ObservedBytes, a.SizeBytes)
		return check
	}

	if opts.SkipHash {
		check.Verdict = ArtifactVerified
		check.Detail = fmt.Sprintf("presence and size were checked; the bytes were not hashed, so this is NOT a verification of the payload's integrity (%d bytes present, size as recorded)", check.ObservedBytes)
		return check
	}

	f, err := filesystem.Open(full)
	if err != nil {
		check.Verdict = ArtifactMissing
		check.Detail = fmt.Sprintf("%s could not be opened (%v), so its contents are unknown - reported as missing, not as corruption", full, err)
		return check
	}
	defer f.Close()

	h := newHashWriter()
	if _, err := io.Copy(io.MultiWriter(h), f); err != nil {
		check.Verdict = ArtifactMissing
		check.Detail = fmt.Sprintf("%s could not be read to the end (%v), so its contents are unknown - reported as missing, not as corruption", full, err)
		return check
	}
	check.ObservedSHA256 = hex.EncodeToString(h.sum())

	if digestEqual(check.ObservedSHA256, check.ExpectedSHA256) {
		check.Verdict = ArtifactVerified
		check.Detail = fmt.Sprintf("all %d bytes hash to the recorded digest", check.ObservedBytes)
		return check
	}
	check.Verdict = ArtifactChecksumMismatch
	check.Detail = fmt.Sprintf("%s hashes to %s where the manifest records %s - the payload is present and its bytes are not the bytes that were captured",
		full, shortDigest(h.sum()), shortDigest(mustHex(a.SHA256)))
	return check
}

// splitGenerationDir recovers the policy and generation identifiers from
// a generation directory path, for a surface that has a path rather than
// the identifiers. It never returns an error: a path that does not have
// the expected shape yields empty identifiers and an empty Verification
// that still carries the directory, because the directory path is the
// one part that is definitely right.
func splitGenerationDir(dir string) (policyID, generationID string) {
	clean := pathClean(dir)
	parts := strings.Split(strings.Trim(clean, "/"), "/")
	if len(parts) >= 2 {
		generationID = parts[len(parts)-1]
		policyID = parts[len(parts)-2]
	}
	return policyID, generationID
}

// StoreVerify verifies a generation through the store, so the policy and
// generation identifiers come from arguments rather than from parsing a
// path.
func (s *Store) Verify(ctx context.Context, policyID, generationID string, opts VerifyOptions) (Verification, error) {
	if opts.FS == nil {
		opts.FS = s.fs
	}
	if opts.Codec == nil {
		opts.Codec = s.codec
	}
	if opts.Now == nil {
		opts.Now = s.now
	}
	dir := s.GenerationDir(policyID, generationID)
	v, err := Verify(ctx, dir, opts)
	v.PolicyID = policyID
	v.GenerationID = generationID
	return v, err
}

// hexBytes renders raw digest bytes as lowercase hex. It is a formatter,
// not a hasher: hashing the bytes a hash produced would be a second,
// pointless operation that produces a plausible-looking wrong answer, so
// the two are separate functions with separate names.
func hexBytes(b []byte) string { return hex.EncodeToString(b) }
