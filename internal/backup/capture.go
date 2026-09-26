package backup

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"
)

// Source produces one artifact's bytes. It is the seam between this
// package and everything that actually knows how to get data off a
// running system: a dataset stream producer, a file reader, a marshalled
// definition.
//
// It receives an io.Writer and returns only an error, deliberately. If it
// also returned a digest or a size, the caller would have two sources of
// truth for the same facts, and the one this package verifies against
// would not be the one derived from the bytes. The digest is computed
// from what was written, here, in one place.
type Source interface {
	// Capture writes the artifact's bytes to w. A partial write followed
	// by an error is a failed capture, not a short artifact: the
	// all-or-nothing rule below turns any error into "discard the whole
	// generation", which is the only safe reading of a source that
	// stopped early without saying so.
	Capture(ctx context.Context, w io.Writer) error
	// Describe returns the spec for the artifact this source produces.
	// It is called once, before Capture, so the manifest's description
	// and the bytes come from the same source object.
	Describe() ArtifactSpec
}

// SourceFunc adapts a function to Source, for the common case of a
// source that is a closure over an already-open stream.
type SourceFunc struct {
	Spec ArtifactSpec
	Fn   func(ctx context.Context, w io.Writer) error
}

// Capture implements Source.
func (s SourceFunc) Capture(ctx context.Context, w io.Writer) error { return s.Fn(ctx, w) }

// Describe implements Source.
func (s SourceFunc) Describe() ArtifactSpec { return s.Spec }

// Quiescer stops a Cell and reports what it could actually observe about
// the stop.
//
// The distinction between "the stop call returned without error" and "a
// lookup reported the Cell is not running" is the entire reason
// ConsistencyQuiesced exists, and the reason this interface returns
// evidence rather than a bare error. A stop RPC that returns no error
// has told you the request was accepted; it has not told you the process
// released the dataset. internal/manager's RestoreVMSnapshot guard
// discloses its own version of this as best-effort, and ADR-0131
// upgrades it: an unverifiable guard is unknown, and unknown blocks.
//
// A Quiescer that cannot observe the stop must return an error. It must
// not return a zero StopEvidence with a nil error, and this package does
// not accept one: see requireObservation.
type Quiescer interface {
	// Quiesce stops cellID and returns the evidence. Returning an error
	// means the stop could not be observed, and the capture fails.
	Quiesce(ctx context.Context, cellID string) (StopEvidence, error)
}

// StopEvidence is what a Quiescer actually established about a stop.
type StopEvidence struct {
	// CellID is the Cell that was stopped.
	CellID string
	// Observed is true only when a lookup reported the Cell not
	// running. It is the field that distinguishes a quiesced backup from
	// an assumed one, and it is never inferred.
	Observed bool
	// Method names the observation, verbatim.
	Method string
	// AtUnix is when the observation was made.
	AtUnix int64
}

// requireObservation turns a Quiescer's answer into either usable
// evidence or a refusal.
//
// The refusal is ErrConsistencyNotQuiesced and it is never softened. The
// alternative - capturing the artifacts anyway and labelling the
// generation ConsistencySnapshot - produces a backup that is
// filesystem-consistent when the operator asked for a quiesced one, and
// nothing in the resulting archive would say so. That is the failure
// ADR-0131 section 3 names: "a backup labelled QUIESCED that was
// actually SNAPSHOT is a lie the operator cannot detect, and it is worse
// than a missing backup". The reverse lie - a snapshot generation that
// quietly claims to be quiesced - is refused even earlier, at Validate,
// because a label with no evidence is not a label.
func requireObservation(ev StopEvidence, cellID string, now func() time.Time) (*Quiesce, error) {
	if cellID == "" {
		return nil, fmt.Errorf("%w: no Cell was named, so there is nothing to quiesce and nothing that could be observed", ErrConsistencyNotQuiesced)
	}
	if !ev.Observed {
		method := ev.Method
		if method == "" {
			method = "no method"
		}
		return nil, fmt.Errorf("%w: the Quiescer for %q reported no positive observation of the Cell being stopped (%s). The capture was not downgraded to a %s generation, because a manifest that claims a guarantee its producer never obtained is a lie no later reader can detect; the artifacts that had been captured were discarded",
			ErrConsistencyNotQuiesced, cellID, method, ConsistencySnapshot)
	}
	if ev.CellID != "" && ev.CellID != cellID {
		return nil, fmt.Errorf("%w: asked to quiesce %q and got evidence for %q", ErrConsistencyNotQuiesced, cellID, ev.CellID)
	}
	if ev.Method == "" {
		return nil, fmt.Errorf("%w: %q was reported stopped but the observation names no method, so 'observed stopped' cannot be told apart from 'assumed stopped' - and assuming is the failure this mode exists to rule out. The capture was not downgraded to a %s generation",
			ErrConsistencyNotQuiesced, cellID, ConsistencySnapshot)
	}
	at := ev.AtUnix
	if at == 0 {
		at = unixSeconds(now())
	}
	return &Quiesce{CellID: cellID, ObservedAtUnix: at, Method: ev.Method}, nil
}

// SnapshotTaker is the seam to whatever creates the snapshot an artifact
// is streamed from. It returns the snapshot's observation time, which is
// what EarliestSnapshotUnix and LatestSnapshotUnix are built from, and
// which is the only honest way to answer "when was this data captured".
type SnapshotTaker interface {
	// Snapshot takes a snapshot for the artifact and returns the name it
	// was created under, plus when.
	Snapshot(ctx context.Context, spec ArtifactSpec) (name string, atUnix int64, err error)
}

// Request is one capture: a generation, its artifacts, and the guarantee
// it is supposed to provide.
type Request struct {
	PolicyID     string
	GenerationID string
	CombID       string
	ColonyID     string
	AppliedIndex uint64

	// Consistency is what the caller asked for. It is required and
	// never defaulted: see Consistency.
	Consistency Consistency
	// MaxSkewSeconds is the policy's limit on the width of the capture
	// window. It is enforced for a quiesced generation, whose whole claim
	// is that the artifacts are one instant. It is recorded but not
	// enforced for a snapshot generation, which never claimed a tight
	// window - a set of datasets snapshotted one after another is not
	// atomic across the set, and pretending otherwise would be the same
	// lie in a different field.
	MaxSkewSeconds int64

	// CellID is the Cell being captured, used for the quiesce evidence
	// and required when Consistency is quiesced.
	CellID string

	// Sources are the artifacts, in capture order.
	Sources []Source
}

// Result is what a capture actually did. It is returned on success and
// alongside an error on failure, because a failed capture that ran three
// of five sources is a fact the operator should have: it says the failure
// was at artifact 4, not that "something went wrong".
type Result struct {
	ManifestPath string
	Manifest     Manifest
	Generation   *Generation
	// CapturedBefore is how many artifacts were written before the
	// failure, on a failed capture only.
	CapturedBefore int
	// Aborted is true when the partial generation was removed. It is
	// false when removal failed, and then FailedRemoval carries why -
	// a partial generation left on disk is something an operator needs to
	// know about even though the backup itself correctly did not happen.
	Aborted       bool
	FailedRemoval string
}

// Capture runs a generation end to end.
//
// The shape of it is short enough to read in one go, and the order is the
// design:
//
//  1. Refuse an unlabelled or unknown consistency mode before any byte
//     is captured. There is no point streaming gigabytes in order to
//     discover the request was incoherent.
//
//  2. If the mode is quiesced, quiesce FIRST and require positive
//     evidence. A failure here returns before any artifact exists.
//
//  3. Write every artifact. Each is published atomically under its own
//     final name, and the generation handle records its digest and size.
//
//  4. Any failure at any point in 3: Abort, which removes the generation
//     directory. There is no best-effort backup, because a backup that
//     silently contains 4 of 5 datasets will be believed.
//
//  5. Only after every artifact is on disk, build the manifest, compute
//     the window, enforce the skew limit, and Commit. Commit is the only
//     thing that makes this generation a backup.
//
// A SnapshotTaker is optional. Without one, a capture takes the window
// from the store's clock at the moment each artifact is written, which is
// correct for the artifact kinds this package builds itself - a
// marshalled definition does not have a snapshot instant - and is
// explicitly wrong for a dataset stream. The Result says which it was, so
// a caller cannot mistake a clock-derived window for an observed one.
// The Result and the error are named on purpose: the deferred Abort runs
// after the return values are assigned, and it is the thing that knows
// whether the partial generation was removed. With unnamed results its
// bookkeeping would be written into a copy and thrown away, and a caller
// would be told nothing about a partial directory left on the target.
func (s *Store) Capture(ctx context.Context, req Request) (res Result, err error) {
	if !req.Consistency.Valid() {
		return res, fmt.Errorf("%w: consistency is %q; a capture must say whether it is %q or %q, and an unstated guarantee is refused rather than assumed",
			ErrInvalidManifest, req.Consistency, ConsistencySnapshot, ConsistencyQuiesced)
	}

	var quiesce *Quiesce
	if req.Consistency == ConsistencyQuiesced {
		// The Quiescer is supplied by the caller rather than stored on
		// the Store, because who quiesces is a managerd concern and
		// this package must not be able to stop anything by itself.
		if s.quiescer == nil {
			return res, fmt.Errorf("%w: this Store has no Quiescer configured, so a %s capture cannot be started - the capture is refused rather than downgraded to %q",
				ErrConsistencyNotQuiesced, ConsistencyQuiesced, ConsistencySnapshot)
		}
		ev, err := s.quiescer.Quiesce(ctx, req.CellID)
		if err != nil {
			return res, fmt.Errorf("%w: %v (the capture was not downgraded to a %s generation)",
				ErrConsistencyNotQuiesced, err, ConsistencySnapshot)
		}
		q, err := requireObservation(ev, req.CellID, s.now)
		if err != nil {
			return res, err
		}
		quiesce = q
	}

	gen, err := s.Begin(req.PolicyID, req.GenerationID)
	if err != nil {
		return res, err
	}
	res.Generation = gen
	// From here on every failure aborts. Deferred rather than repeated at
	// each return, so a new failure path cannot forget it - which is the
	// failure mode this whole file is guarding against.
	committed := false
	defer func() {
		if committed {
			return
		}
		if err := gen.Abort(); err != nil {
			res.FailedRemoval = err.Error()
		} else {
			res.Aborted = true
		}
	}()

	earliest, latest := int64(0), int64(0)
	for i, src := range req.Sources {
		if err := ctx.Err(); err != nil {
			return res, contextErr(fmt.Sprintf("capturing generation %s at artifact %d of %d", req.GenerationID, i+1, len(req.Sources)), ctx)
		}
		spec := src.Describe()
		var at int64
		if s.snapshots != nil {
			name, atUnix, err := s.snapshots.Snapshot(ctx, spec)
			if err != nil {
				return res, fmt.Errorf("taking the snapshot for artifact %d (%s): %w", i+1, spec.ID, err)
			}
			spec.Dataset = withSnapshot(spec.Dataset, name, atUnix)
			at = atUnix
		} else {
			at = s.nowUnix()
		}
		if at != 0 {
			if earliest == 0 || at < earliest {
				earliest = at
			}
			if at > latest {
				latest = at
			}
		}
		if err := captureOne(ctx, gen, spec, src); err != nil {
			return res, err
		}
		res.CapturedBefore = i + 1
	}
	if earliest == 0 {
		// Only reachable with no sources at all, which is legal: a policy
		// that matched no Cells produces an empty generation, and it is
		// honest about containing nothing. Its window is the single
		// instant it was taken, which is the truth - there is no spread
		// to record because there is no data.
		earliest = s.nowUnix()
		latest = earliest
	}

	m := Manifest{
		FormatVersion:        FormatVersion,
		CreatedUnix:          s.nowUnix(),
		CombID:               req.CombID,
		ColonyID:             req.ColonyID,
		AppliedIndex:         req.AppliedIndex,
		PolicyID:             req.PolicyID,
		GenerationID:         req.GenerationID,
		Consistency:          req.Consistency,
		Quiesce:              quiesce,
		EarliestSnapshotUnix: earliest,
		LatestSnapshotUnix:   latest,
		MaxSkewSeconds:       req.MaxSkewSeconds,
		Artifacts:            gen.Artifacts(),
		SecretGaps:           SecretGaps(),
	}
	if err := gen.Commit(ctx, m); err != nil {
		return res, err
	}
	committed = true
	res.Manifest = m
	res.ManifestPath = joinPath(gen.Dir(), ManifestName)
	return res, nil
}

// captureOne writes one artifact from one source.
func captureOne(ctx context.Context, gen *Generation, spec ArtifactSpec, src Source) error {
	// The source's description is authoritative for what the artifact
	// is; the id and kind are checked against the spec the caller
	// registered, so a source cannot quietly turn a dataset stream into
	// something else between Describe and Capture.
	art, err := gen.WriteArtifact(ctx, spec, readerFor(ctx, src))
	if err != nil {
		return fmt.Errorf("capturing artifact %d (%s): %w", indexOfArtifact(gen, art), spec.ID, err)
	}
	// A source that wrote nothing and reported no error has either failed
	// silently or captured nothing, and a zero-length payload artifact is
	// not either. A digest of the empty string is a perfectly valid
	// SHA-256, so nothing downstream would notice - which is exactly why
	// it is checked here, at the one place that knows what the source was
	// supposed to produce.
	//
	// The check lives in the capture path rather than in WriteArtifact
	// because writing zero bytes is a legitimate thing for the low-level
	// primitive to do; it is not a legitimate thing for a Source to do.
	if art.SizeBytes == 0 {
		return fmt.Errorf("capturing artifact %q: the source produced no bytes and reported no error, so nothing was captured - an empty payload artifact would verify and be believed forever without ever holding anything", spec.ID)
	}
	return nil
}

// readerFor adapts a Source to the io.Reader that WriteArtifact streams.
// The Source is pulled through a pipe so the hash is computed over the
// bytes as they are produced rather than the source having to buffer
// itself - which matters for a dataset stream, which is unbounded.
func readerFor(ctx context.Context, src Source) io.Reader {
	pr, pw := io.Pipe()
	go func() {
		err := src.Capture(ctx, pw)
		// A non-nil error ends the pipe with that error, so
		// WriteArtifact's read reports the source's failure rather than
		// a bare EOF. A nil error closes it cleanly. Either way the
		// goroutine finishes: there is no path here that leaks one,
		// because a source that returns without writing anything simply
		// closes the writer.
		_ = pw.CloseWithError(err)
	}()
	return pr
}

// indexOfArtifact finds an artifact's 1-based position in the generation,
// for a message that has to say which of N failed. It returns 0 for an
// artifact that was never recorded, which happens when WriteArtifact
// itself failed; the message then names the id, which is more useful
// than a wrong position.
func indexOfArtifact(g *Generation, a Artifact) int {
	if a.ArtifactID == "" {
		return 0
	}
	for i, w := range g.Artifacts() {
		if w.ArtifactID == a.ArtifactID {
			return i + 1
		}
	}
	return 0
}

// withSnapshot returns a copy of d with the snapshot name and time
// filled in, leaving the caller's value untouched. A copy rather than a
// mutation because the caller's DatasetArtifact is its own object and
// silently editing it would make a retry record a second snapshot's name
// against the first snapshot's data.
func withSnapshot(d *DatasetArtifact, name string, atUnix int64) *DatasetArtifact {
	if d == nil {
		return nil
	}
	out := *d
	if name != "" {
		out.Snapshot = name
	}
	if atUnix != 0 {
		out.SnapshotTakenUnix = atUnix
	}
	return &out
}

// SetQuiescer configures the Quiescer a Store uses for
// ConsistencyQuiesced captures. There is deliberately no default: a
// Store with no Quiescer refuses a quiesced capture outright rather than
// falling back, and the refusal says why.
//
// There is deliberately no SetRestarter, either. ADR-0131's
// restart_after_quiesce is a policy flag, and acting on it means starting
// a workload, which is a managerd concern with its own guard rails.
// Leaving it out of this package is a statement that this package does
// not start or stop workloads; it only asks whether one is already
// stopped, and refuses to guess.
func (s *Store) SetQuiescer(q Quiescer) { s.quiescer = q }

// SetSnapshotTaker configures the snapshot seam. Nil means no snapshots
// are taken and the capture window is derived from the store's clock -
// correct for the artifact kinds this package builds itself, and
// explicitly not a dataset stream, which is why a caller capturing one
// has to install a taker.
func (s *Store) SetSnapshotTaker(t SnapshotTaker) { s.snapshots = t }

// RestoreOptions is what a restore is allowed to assume. Every field is a
// guard, and Restore evaluates them in a fixed order so that a failure
// names the first guard that stopped it rather than whichever one the
// code happened to reach.
type RestoreOptions struct {
	// Mode is where the data goes. Out-of-band is the default posture
	// and the only one that writes nothing; in-place is the one that
	// clobbers.
	Mode RestoreMode
	// Clobber must be set by a human, in the UI, on a dialog that states
	// the blast radius in words. It is not settable from a policy and
	// not inferable from anything.
	Clobber bool
	// ObservedStopped is the caller saying a lookup reported the Cell
	// not running. It is a bool rather than something this package
	// computes because stopping and observing a workload is
	// managerd's job, not this package's - see the note on the absence
	// of a SetRestarter in SetQuiescer's doc comment.
	//
	// It is false by default and there is no way to set it except by
	// having actually looked, which is the point. A caller that could
	// not reach GetVM/GetJail leaves it false, and an in-place restore
	// is refused - the same "unknown blocks" rule ADR-0118 established
	// for quorum blockers and ADR-0090's own guard discloses for
	// RestoreVMSnapshot.
	ObservedStopped bool
	// LiveDatasetGUID is the live dataset's GUID, read at restore time
	// and compared against the manifest's source_guid. Empty means it
	// could not be read, which makes an in-place restore ineligible: a
	// guard that cannot be evaluated is not a passing guard.
	LiveDatasetGUID string
	// DestDataset is where an out-of-band restore materialises. It must
	// differ from the source, because a "new" dataset with the source's
	// name is the clobber this mode exists to avoid.
	DestDataset string
}

// RestoreMode is where a restore puts the data.
type RestoreMode string

const (
	// RestoreOutOfBand materialises a new dataset, not attached to
	// anything live. It writes no control-plane state, so it needs no
	// quorum and touches nothing that is running. This is the default
	// and the only mode a single RPC offers in v1.
	RestoreOutOfBand RestoreMode = "out_of_band"

	// RestoreInPlace clobbers the Cell's existing dataset. It requires
	// an observed-stopped Cell, a GUID match against the manifest, an
	// explicit clobber flag, and quorum - see Restore's own doc comment.
	RestoreInPlace RestoreMode = "in_place"
)

// RestorePlan's assessment of whether a restore may proceed.
//
// It is returned as a value rather than applied, because the thing it is
// assessing - "is this archive still intact, and may it be put back
// anywhere" - is worth knowing even when the answer is no, and an
// operator who can see which guard failed will not simply try again
// harder.
type RestorePlan struct {
	Eligible bool
	// Guard names the first guard that refused, in the order they are
	// evaluated. Empty when Eligible.
	Guard string
	// Reason is the human-readable refusal, naming the specific check.
	Reason string

	Verification Verification
	Mode         RestoreMode
	// SkewSeconds is the capture window's width, so an operator can see
	// what they are about to restore without reading the manifest.
	SkewSeconds int64
	// Consistency is the label, and ClaimsQuiesced is what that label
	// actually entitles a caller to.
	Consistency    Consistency
	ClaimsQuiesced bool
	// SecretGaps is reproduced so the surface that asks for a restore
	// can tell the operator, before the button, exactly which
	// credentials they will have to re-issue afterwards.
	SecretGaps []SecretGap
}

// PlanRestore assesses a generation for restoration without restoring
// it. It is read-only, and it runs the whole verification first: a
// restore plan for a generation whose bytes do not match its manifest is
// a plan to put back corruption.
//
// vopts configures the verification pass and ropts carries the restore
// guards. They are separate arguments because they answer different
// questions - "is this archive intact" and "may I put it back here" - and
// folding them into one struct would make it easy to pass a verification
// option where a guard belongs.
func (s *Store) PlanRestore(ctx context.Context, policyID, generationID string, mode RestoreMode, vopts VerifyOptions, ropts RestoreOptions) (RestorePlan, error) {
	v, err := s.Verify(ctx, policyID, generationID, vopts)
	plan := RestorePlan{Verification: v, Mode: mode}
	if err != nil || !v.Understood {
		plan.Guard = "manifest"
		plan.Reason = fmt.Sprintf("the manifest itself is not usable (%s: %s), so no artifact was hashed and there is nothing to restore from - the archive is left exactly as it is",
			v.State, v.Reason)
		return plan, err
	}
	plan.Consistency = v.Consistency
	plan.ClaimsQuiesced = v.ClaimsQuiesced

	m, _, lerr := s.Load(policyID, generationID)
	if lerr != nil {
		plan.Guard = "manifest"
		plan.Reason = lerr.Error()
		return plan, lerr
	}
	plan.SecretGaps = m.SecretGaps
	if skew, ok := m.skew(); ok {
		plan.SkewSeconds = skew
	}

	// The guards, in order. Each returns a named guard so the surface can
	// say which one stopped the operator, which ADR-0131 requires
	// explicitly: "an operator who cannot tell which guard stopped them
	// will try again harder".
	if !v.Ok() {
		plan.Guard = "artifact verification"
		plan.Reason = fmt.Sprintf("the generation's bytes do not all match its manifest (%s); restoring it would put back something that is not what was captured", v.Summary())
		return plan, fmt.Errorf("backup: generation %s failed verification", generationID)
	}

	switch mode {
	case RestoreInPlace:
		if !ropts.ObservedStopped {
			plan.Guard = "observed stopped"
			plan.Reason = "an in-place restore requires the Cell to be observed stopped, and it was not. " +
				"This is a refusal, not a warning: ZFS has no notion of coordinating with a process that already has the file open, and a torn restore is worse than a torn dataset. " +
				"Out-of-band restore does not require this."
			return plan, fmt.Errorf("backup: in-place restore refused: the Cell was not observed stopped")
		}
		if !ropts.Clobber {
			plan.Guard = "explicit clobber"
			plan.Reason = "an in-place restore clobbers the Cell's existing dataset back to the backup's instant, destroying any writes made since. " +
				"It requires an explicit confirmation from a human who has been shown that in words. Out-of-band restore does not require this."
			return plan, fmt.Errorf("backup: in-place restore refused: clobber was not confirmed")
		}
		// The GUID guard, and only for a generation that carries one.
		// An out-of-band restore does not need it and is not blocked by
		// its absence, which is why this is inside the in-place arm.
		if err := m.CheckSourceGUID(ropts.LiveDatasetGUID); err != nil {
			plan.Guard = "dataset GUID"
			plan.Reason = err.Error() + ". Out-of-band restore remains available, because it does not need the GUID."
			return plan, err
		}
	default:
		if ropts.DestDataset == "" {
			plan.Guard = "destination"
			plan.Reason = "an out-of-band restore materialises a NEW dataset and needs to know its name; none was given. " +
				"Out-of-band is the default posture precisely because it cannot damage what is already there, so it is also the mode that needs the least ceremony - but it does need a destination."
			return plan, fmt.Errorf("backup: out-of-band restore refused: no destination dataset was given")
		}
	}

	plan.Eligible = true
	plan.Reason = fmt.Sprintf("every artifact verified, and the %s guards for %s restore are all satisfied", mode, mode)
	return plan, nil
}

// errGUIDMismatch is returned when a dataset's GUID differs from the
// manifest's, meaning the dataset was destroyed and recreated after the
// backup. Clobbering it would destroy post-backup state the operator may
// not know exists.
var errGUIDMismatch = errors.New("backup: the live dataset's GUID does not match the manifest's source_guid, so it was destroyed and recreated after this backup was taken")

// CheckSourceGUID compares a manifest's recorded source GUID with the
// live one. An empty recorded GUID makes an in-place restore ineligible
// rather than permitted, and the message says why in those words: the
// guard exists to stop a restore clobbering a dataset that was recreated
// after the backup, and a manifest with no GUID to compare against
// cannot evaluate that guard.
func (m *Manifest) CheckSourceGUID(live string) error {
	for _, a := range m.Artifacts {
		if a.Kind != KindDataset || a.Dataset == nil {
			continue
		}
		recorded := a.Dataset.SourceGUID
		if recorded == "" {
			return fmt.Errorf("backup: artifact %q records no source_guid, so an in-place restore is INELIGIBLE - the guard that stops a restore clobbering a dataset that was destroyed and recreated after the backup cannot be evaluated, and a guard that cannot be evaluated is not a passing guard; out-of-band restore remains available because it does not need the GUID",
				a.ArtifactID)
		}
		if live == "" {
			return fmt.Errorf("backup: the live dataset's GUID could not be read, so artifact %q's source_guid (%s) cannot be compared; in-place restore is ineligible on absent evidence", a.ArtifactID, shortDigest([]byte(recorded)))
		}
		if recorded != live {
			return fmt.Errorf("%w: manifest records %s, the live dataset reports %s", errGUIDMismatch, shortDigest([]byte(recorded)), shortDigest([]byte(live)))
		}
	}
	return nil
}
