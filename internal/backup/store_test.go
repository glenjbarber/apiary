package backup

import (
	"errors"
	"io/fs"
	"strings"
	"testing"
	"time"
)

// newTestStore returns a Store on a real temporary directory with a
// deterministic clock, plus the root. Every store test in this file uses
// one, so "where is the target" is never a question.
func newTestStore(t *testing.T, opts ...Option) (*Store, string) {
	t.Helper()
	root := t.TempDir()
	base := []Option{WithClock(fixedClock(time.Unix(1780000000, 0), time.Second))}
	return NewStore(root, append(base, opts...)...), root
}

// sampleGenerationManifest returns a minimal valid manifest for a
// generation with no artifacts, for the tests that are about the manifest
// write and not about payloads.
func sampleGenerationManifest(policy, generation string) Manifest {
	return Manifest{
		FormatVersion:        FormatVersion,
		CreatedUnix:          1780000000,
		PolicyID:             policy,
		GenerationID:         generation,
		Consistency:          ConsistencySnapshot,
		EarliestSnapshotUnix: 1780000000,
		LatestSnapshotUnix:   1780000000,
	}
}

// TestManifestIsNeverVisibleBeforeItsArtifacts is the central atomicity
// test, and it is written as an interleaving assertion rather than an
// end-state one.
//
// The watched filesystem's observe hook runs after EVERY operation and
// asks a question that is only ever true at one point: "is a manifest
// visible right now?" Before the artifacts are written it must be no; at
// the end it must be yes, and yes only after the last artifact. Between
// them there is no instant at which a reader listing this directory would
// have seen a manifest and then failed to find the artifacts it names.
func TestManifestIsNeverVisibleBeforeItsArtifacts(t *testing.T) {
	wfs := newWatchedFS()
	store, root := newTestStore(t, WithFS(wfs))

	gen, err := store.Begin("nightly", "gen-1")
	if err != nil {
		t.Fatal(err)
	}

	// Track, per observation, how many artifacts are on disk and whether
	// a manifest is visible. The invariant is a partial order, not a
	// constant: a visible manifest must never coexist with a missing
	// artifact.
	type snapshot struct {
		artifacts int
		manifest  bool
	}
	var seen []snapshot
	wfs.mu.Lock()
	wfs.observe = func(op string) error {
		entries, err := realReadDir(joinPath(gen.Dir(), artifactsDirName))
		if err != nil {
			return err
		}
		real := OSFS{}
		_, statErr := real.Stat(joinPath(gen.Dir(), ManifestName))
		seen = append(seen, snapshot{artifacts: entries, manifest: statErr == nil})
		return nil
	}
	wfs.mu.Unlock()

	artifacts := make([]Artifact, 0, 3)
	for i, body := range []string{"first payload", "second payload", "third payload"} {
		a, err := gen.WriteBytes(t.Context(), ArtifactSpec{
			ID:   "art-" + string(rune('a'+i)),
			Kind: KindDataset,
			Dataset: &DatasetArtifact{
				Dataset:  "tank/one",
				Snapshot: "snap",
			},
		}, []byte(body))
		if err != nil {
			t.Fatal(err)
		}
		artifacts = append(artifacts, a)
	}
	m := sampleGenerationManifest("nightly", "gen-1")
	m.Artifacts = artifacts
	if err := gen.Commit(t.Context(), m); err != nil {
		t.Fatal(err)
	}

	if len(seen) == 0 {
		t.Fatal("no operations were observed; the test proved nothing")
	}
	// The invariant, checked at every observation.
	for i, s := range seen {
		if s.manifest && s.artifacts < 3 {
			t.Fatalf("observation %d saw a manifest with only %d of 3 artifacts present: %+v", i, s.artifacts, s)
		}
	}
	// And that it really was published at the end, rather than the
	// assertion above passing because nothing ever happened.
	if !seen[len(seen)-1].manifest {
		t.Fatal("no observation ever saw a manifest, so the run published nothing")
	}
	// The first observation must have been a non-manifest one, which
	// establishes that the sequence actually contains a before and an
	// after rather than only an after.
	if seen[0].manifest {
		t.Fatal("a manifest was already visible at the first operation")
	}
	// Count the artifacts actually present now, independently of the
	// generation handle's own bookkeeping.
	entries, err := realReadDir(joinPath(gen.Dir(), artifactsDirName))
	if err != nil {
		t.Fatal(err)
	}
	if entries != 3 {
		t.Errorf("%d artifacts on disk, want 3", entries)
	}
	assertNoManifestAnywhere(t, joinPath(root, "nowhere")) // trivially true; keeps the helper honest
}

// realReadDir counts the regular files in a directory using the real
// filesystem, bypassing the watched wrapper so an observation is not
// itself an operation that the observer sees.
func realReadDir(dir string) (int, error) {
	entries, err := OSFS{}.ReadDir(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return 0, nil
		}
		return 0, err
	}
	n := 0
	for _, e := range entries {
		if !e.IsDir() {
			n++
		}
	}
	return n, nil
}

// TestCommitRefusesAManifestThatDescribesArtifactsItNeverWrote is the
// structural half of the rule: the manifest cannot claim a payload that
// was not captured, and cannot omit one that was.
func TestCommitRefusesAManifestThatDescribesArtifactsItNeverWrote(t *testing.T) {
	store, _ := newTestStore(t)
	gen, err := store.Begin("nightly", "gen-1")
	if err != nil {
		t.Fatal(err)
	}
	written, err := gen.WriteBytes(t.Context(), ArtifactSpec{
		ID:      "one",
		Kind:    KindDataset,
		Dataset: &DatasetArtifact{Dataset: "tank/one", Snapshot: "s"},
	}, []byte("payload one"))
	if err != nil {
		t.Fatal(err)
	}

	phantom := written
	phantom.ArtifactID = "two"
	phantom.RelativePath = "artifacts/two.stream"
	phantom.SHA256 = strings.Repeat("aa", 32)

	t.Run("claims one that was never written", func(t *testing.T) {
		m := sampleGenerationManifest("nightly", "gen-1")
		m.Artifacts = []Artifact{written, phantom}
		err := gen.Commit(t.Context(), m)
		if err == nil {
			t.Fatal("a manifest describing an artifact that was never captured was accepted")
		}
		if !errIs(t, err, ErrArtifactSetMismatch) {
			t.Errorf("error is %v, want ErrArtifactSetMismatch", err)
		}
		if !containsAll(err.Error(), "two", "never wrote") {
			t.Errorf("the refusal does not name the artifact and the reason: %v", err)
		}
	})

	t.Run("omits one that was written", func(t *testing.T) {
		m := sampleGenerationManifest("nightly", "gen-1")
		m.Artifacts = nil
		err := gen.Commit(t.Context(), m)
		if err == nil {
			t.Fatal("a manifest omitting a written artifact was accepted - a restore would silently never see that payload")
		}
		if !errIs(t, err, ErrArtifactSetMismatch) {
			t.Errorf("error is %v, want ErrArtifactSetMismatch", err)
		}
		if !containsAll(err.Error(), "one", "does not list it") {
			t.Errorf("the refusal does not name the orphaned artifact: %v", err)
		}
	})

	t.Run("disagrees about the digest", func(t *testing.T) {
		m := sampleGenerationManifest("nightly", "gen-1")
		lying := written
		lying.SHA256 = strings.Repeat("bb", 32)
		m.Artifacts = []Artifact{lying}
		err := gen.Commit(t.Context(), m)
		if err == nil {
			t.Fatal("a manifest recording a digest the bytes do not have was accepted")
		}
		if !errIs(t, err, ErrArtifactSetMismatch) {
			t.Errorf("error is %v, want ErrArtifactSetMismatch", err)
		}
		if !containsAll(err.Error(), "hash to") {
			t.Errorf("the refusal does not contrast the two digests: %v", err)
		}
	})

	t.Run("disagrees about the size", func(t *testing.T) {
		m := sampleGenerationManifest("nightly", "gen-1")
		lying := written
		lying.SizeBytes = written.SizeBytes + 1
		m.Artifacts = []Artifact{lying}
		err := gen.Commit(t.Context(), m)
		if err == nil {
			t.Fatal("a manifest recording a size the file does not have was accepted")
		}
		if !containsAll(err.Error(), "bytes") {
			t.Errorf("the refusal does not name the size disagreement: %v", err)
		}
	})

	t.Run("names a different generation", func(t *testing.T) {
		m := sampleGenerationManifest("nightly", "gen-1")
		m.GenerationID = "gen-other"
		m.Artifacts = []Artifact{written}
		if err := gen.Commit(t.Context(), m); !errIs(t, err, ErrArtifactSetMismatch) {
			t.Errorf("error is %v, want ErrArtifactSetMismatch", err)
		}
	})

	// Nothing above may have published anything.
	assertNoManifestAnywhere(t, gen.Dir())

	// And a correct manifest still works afterwards, which proves the
	// refusals left the generation usable rather than half-consumed.
	m := sampleGenerationManifest("nightly", "gen-1")
	m.Artifacts = []Artifact{written}
	if err := gen.Commit(t.Context(), m); err != nil {
		t.Fatalf("the correct manifest was refused after the mismatches: %v", err)
	}
}

// TestCommitFailureLeavesNoManifest tests every point at which the
// atomic write can fail. For each one, the assertion is the same and is
// the whole point: no manifest is visible anywhere afterwards.
func TestCommitFailureLeavesNoManifest(t *testing.T) {
	failures := []struct {
		name    string
		inject  func(w *watchedFS)
		wantSub string
	}{
		{
			name:    "temp file cannot be created",
			inject:  func(w *watchedFS) { w.failOn("CreateTemp") },
			wantSub: "temp file",
		},
		{
			name:    "fsync of the temp file fails",
			inject:  func(w *watchedFS) { w.failOn("File.Sync") },
			wantSub: "syncing",
		},
		{
			name:    "chmod of the temp file fails",
			inject:  func(w *watchedFS) { w.failOn("File.Chmod") },
			wantSub: "mode",
		},
		{
			name:    "close of the temp file fails",
			inject:  func(w *watchedFS) { w.failOn("File.Close") },
			wantSub: "closing",
		},
		{
			name:    "the rename fails",
			inject:  func(w *watchedFS) { w.failOn("Rename") },
			wantSub: "publishing",
		},
		{
			name:    "the parent directory fsync fails",
			inject:  func(w *watchedFS) { w.failOn("SyncDir") },
			wantSub: "syncing",
		},
	}
	for _, tc := range failures {
		t.Run(tc.name, func(t *testing.T) {
			wfs := newWatchedFS()
			store, _ := newTestStore(t, WithFS(wfs))
			gen, err := store.Begin("nightly", "gen-1")
			if err != nil {
				t.Fatal(err)
			}
			// Write the artifact with a healthy filesystem first, so the
			// only injected failure is in the manifest's own write.
			a, err := gen.WriteBytes(t.Context(), ArtifactSpec{
				ID:      "one",
				Kind:    KindDataset,
				Dataset: &DatasetArtifact{Dataset: "tank/one", Snapshot: "s"},
			}, []byte("payload one"))
			if err != nil {
				t.Fatal(err)
			}
			m := sampleGenerationManifest("nightly", "gen-1")
			m.Artifacts = []Artifact{a}

			tc.inject(wfs)
			err = gen.Commit(t.Context(), m)
			if err == nil {
				t.Fatal("Commit reported success despite an injected failure")
			}
			if !containsAll(err.Error(), tc.wantSub) {
				t.Errorf("error %q does not mention %q, so the operator is not told which step failed", err, tc.wantSub)
			}
			// The invariant, for every failure point.
			assertNoManifestAnywhere(t, gen.Dir())
		})
	}
}

// TestCommitDoesNotPublishUntilTheRename checks the ordering inside the
// atomic write: the temp file is fsynced and chmodded BEFORE the rename
// that makes it visible.
//
// The order is the guarantee. A manifest fsynced after its rename could
// be observed - by a reader, or by a crash - as a named, present, and
// completely empty file, which is worse than no manifest at all because
// every reader that checks only for the file's existence would call it a
// backup.
func TestCommitDoesNotPublishUntilTheRename(t *testing.T) {
	wfs := newWatchedFS()
	store, _ := newTestStore(t, WithFS(wfs))
	gen, err := store.Begin("nightly", "gen-1")
	if err != nil {
		t.Fatal(err)
	}
	m := sampleGenerationManifest("nightly", "gen-1")
	if err := gen.Commit(t.Context(), m); err != nil {
		t.Fatal(err)
	}

	ops := wfs.opLog()
	renameAt, syncAt, chmodAt, closeAt := -1, -1, -1, -1
	for i, op := range ops {
		switch op {
		case "Rename":
			if renameAt < 0 {
				renameAt = i
			}
		case "File.Sync":
			if syncAt < 0 {
				syncAt = i
			}
		case "File.Chmod":
			if chmodAt < 0 {
				chmodAt = i
			}
		case "File.Close":
			if closeAt < 0 {
				closeAt = i
			}
		}
	}
	if renameAt < 0 || syncAt < 0 || chmodAt < 0 || closeAt < 0 {
		t.Fatalf("the operation log does not contain the whole sequence: %v", ops)
	}
	if !(syncAt < renameAt && chmodAt < renameAt && closeAt < renameAt) {
		t.Errorf("the manifest was published before it was finished: sync=%d chmod=%d close=%d rename=%d in %v", syncAt, chmodAt, closeAt, renameAt, ops)
	}
	// And the parent directory fsync, which makes the rename itself
	// durable, must come after the rename.
	syncDirAt := -1
	for i, op := range ops {
		if op == "SyncDir" && i > renameAt {
			syncDirAt = i
			break
		}
	}
	if syncDirAt < 0 {
		t.Errorf("no directory fsync follows the rename, so a crash could lose the entry while the data is already on disk: %v", ops)
	}
}

// TestABeginRefusesToOverwriteACommittedGeneration: a published backup
// is immutable. Re-running a job with the same identifier must not
// destroy the only copy of something.
func TestABeginRefusesToOverwriteACommittedGeneration(t *testing.T) {
	store, _ := newTestStore(t)
	gen, err := store.Begin("nightly", "gen-1")
	if err != nil {
		t.Fatal(err)
	}
	if err := gen.Commit(t.Context(), sampleGenerationManifest("nightly", "gen-1")); err != nil {
		t.Fatal(err)
	}
	_, err = store.Begin("nightly", "gen-1")
	if err == nil {
		t.Fatal("a second Begin on a committed generation was allowed")
	}
	if !errIs(t, err, ErrGenerationCommitted) {
		t.Errorf("error is %v, want ErrGenerationCommitted", err)
	}
	if !containsAll(err.Error(), "already has a", "new generation_id") {
		t.Errorf("the refusal does not say what to do instead: %v", err)
	}
}

func TestACommittedGenerationCannotBeCommittedOrAbortedTwice(t *testing.T) {
	store, _ := newTestStore(t)
	gen, err := store.Begin("nightly", "gen-1")
	if err != nil {
		t.Fatal(err)
	}
	if err := gen.Commit(t.Context(), sampleGenerationManifest("nightly", "gen-1")); err != nil {
		t.Fatal(err)
	}
	if err := gen.Commit(t.Context(), sampleGenerationManifest("nightly", "gen-1")); !errIs(t, err, ErrGenerationCommitted) {
		t.Errorf("a second Commit returned %v, want ErrGenerationCommitted", err)
	}
	if err := gen.Abort(); !errIs(t, err, ErrGenerationCommitted) {
		t.Errorf("Abort on a committed generation returned %v, want ErrGenerationCommitted - removing a published backup is not this package's call", err)
	}
}

func TestAbortRemovesTheWholeGeneration(t *testing.T) {
	store, _ := newTestStore(t)
	gen, err := store.Begin("nightly", "gen-1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := gen.WriteBytes(t.Context(), ArtifactSpec{
		ID:      "one",
		Kind:    KindDataset,
		Dataset: &DatasetArtifact{Dataset: "tank/one", Snapshot: "s"},
	}, []byte("payload")); err != nil {
		t.Fatal(err)
	}
	if err := gen.Abort(); err != nil {
		t.Fatal(err)
	}
	real := OSFS{}
	if _, err := real.Stat(gen.Dir()); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the aborted generation directory still exists: %v", err)
	}
	// And the handle is dead: writing to it must fail rather than
	// recreate the directory behind the caller's back.
	if _, err := gen.WriteBytes(t.Context(), ArtifactSpec{
		ID:      "two",
		Kind:    KindDataset,
		Dataset: &DatasetArtifact{Dataset: "tank/two", Snapshot: "s"},
	}, []byte("payload")); err == nil {
		t.Error("an aborted generation accepted another artifact")
	}
}

func TestAnArtifactCannotShadowTheManifest(t *testing.T) {
	store, _ := newTestStore(t)
	gen, err := store.Begin("nightly", "gen-1")
	if err != nil {
		t.Fatal(err)
	}
	// The id cannot produce the manifest's name through the normal path,
	// because the manifest lives beside artifacts/ and an artifact inside
	// it. Assert the rule rather than the outcome of one spelling.
	if _, err := gen.WriteBytes(t.Context(), ArtifactSpec{
		ID:      ManifestName,
		Kind:    KindDataset,
		Dataset: &DatasetArtifact{Dataset: "tank/one", Snapshot: "s"},
	}, []byte("payload")); !errIs(t, err, ErrManifestShadowed) && !errIs(t, err, ErrInvalidManifest) {
		t.Errorf("an artifact named %s was accepted: %v", ManifestName, err)
	}
}

func TestListReportsAnUnpublishedGenerationAsNotABackup(t *testing.T) {
	store, _ := newTestStore(t)
	gen, err := store.Begin("nightly", "gen-partial")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := gen.WriteBytes(t.Context(), ArtifactSpec{
		ID:      "one",
		Kind:    KindDataset,
		Dataset: &DatasetArtifact{Dataset: "tank/one", Snapshot: "s"},
	}, []byte("payload")); err != nil {
		t.Fatal(err)
	}
	// A committed one too, so the listing has both to report.
	gen2, err := store.Begin("nightly", "gen-good")
	if err != nil {
		t.Fatal(err)
	}
	if err := gen2.Commit(t.Context(), sampleGenerationManifest("nightly", "gen-good")); err != nil {
		t.Fatal(err)
	}

	summaries, err := store.List("nightly")
	if err != nil {
		t.Fatal(err)
	}
	if len(summaries) != 2 {
		t.Fatalf("listed %d generations, want 2", len(summaries))
	}
	if summaries[0].GenerationID != "gen-good" || summaries[1].GenerationID != "gen-partial" {
		t.Fatalf("listing is not in identifier order: %+v", summaries)
	}
	good, partial := summaries[0], summaries[1]
	if good.State != GenerationOK || !good.IsBackup() {
		t.Errorf("the committed generation is %q, want ok", good.State)
	}
	if partial.State != GenerationIncomplete {
		t.Errorf("the partial generation is %q, want incomplete", partial.State)
	}
	if partial.IsBackup() {
		t.Error("a generation with no manifest reports itself as a backup; nothing was ever published, so there is nothing to be unsure about")
	}
	if !containsAll(partial.Reason, "no "+ManifestName, "partial or abandoned") {
		t.Errorf("the reason does not explain what a reader is looking at: %s", partial.Reason)
	}
}

func TestListOfAnUnusedPolicyIsEmptyNotAnError(t *testing.T) {
	store, _ := newTestStore(t)
	got, err := store.List("never-used")
	if err != nil {
		t.Fatalf("listing a policy with no directory failed: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("listed %d generations for a policy that has never run", len(got))
	}
}

// TestSweepRemovesOnlyWhatHasNoManifest is the crash-recovery half. A
// sweep after an unattended power loss has to be as careful as Commit:
// an archive this build cannot read is still somebody's only copy.
func TestSweepRemovesOnlyWhatHasNoManifest(t *testing.T) {
	store, root := newTestStore(t)

	// 1. A committed generation: never swept.
	good, err := store.Begin("nightly", "gen-good")
	if err != nil {
		t.Fatal(err)
	}
	if err := good.Commit(t.Context(), sampleGenerationManifest("nightly", "gen-good")); err != nil {
		t.Fatal(err)
	}

	// 2. A partial generation: swept.
	partial, err := store.Begin("nightly", "gen-partial")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := partial.WriteBytes(t.Context(), ArtifactSpec{
		ID:      "one",
		Kind:    KindDataset,
		Dataset: &DatasetArtifact{Dataset: "tank/one", Snapshot: "s"},
	}, []byte("payload")); err != nil {
		t.Fatal(err)
	}

	// 3. A generation whose manifest is from a future version: NOT
	// swept, and reported as refused, because this build's inability to
	// read it is not evidence that it is garbage.
	future, err := store.Begin("nightly", "gen-future")
	if err != nil {
		t.Fatal(err)
	}
	m := sampleGenerationManifest("nightly", "gen-future")
	m.FormatVersion = 99
	body, err := EncodeManifest(JSONCodec{}, &m)
	if err != nil {
		t.Fatal(err)
	}
	// Write it directly, since Commit would refuse to write it.
	if err := (OSFS{}).WriteFile(joinPath(future.Dir(), ManifestName), body, 0o600); err != nil {
		t.Fatal(err)
	}

	res, err := store.Sweep("nightly")
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Removed) != 1 || res.Removed[0] != "gen-partial" {
		t.Errorf("swept %v, want exactly [gen-partial]", res.Removed)
	}
	if len(res.Refused) != 2 {
		t.Fatalf("refused %d generations, want 2: %+v", len(res.Refused), res.Refused)
	}
	byID := map[string]RefusedGeneration{}
	for _, r := range res.Refused {
		byID[r.GenerationID] = r
	}
	if _, ok := byID["gen-future"]; !ok {
		t.Error("a generation with a future-format manifest was swept; an archive this build cannot read is still somebody's only copy")
	} else if !containsAll(byID["gen-future"].Reason, "still the only copy") {
		t.Errorf("the refusal does not explain why: %s", byID["gen-future"].Reason)
	}
	if _, ok := byID["gen-good"]; !ok {
		t.Error("a committed generation was swept")
	}

	real := OSFS{}
	if _, err := real.Stat(joinPath(root, "nightly", "gen-partial")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the partial generation was not actually removed: %v", err)
	}
	if _, err := real.Stat(joinPath(root, "nightly", "gen-good")); err != nil {
		t.Errorf("the committed generation was removed: %v", err)
	}
	if _, err := real.Stat(joinPath(root, "nightly", "gen-future", ManifestName)); err != nil {
		t.Errorf("the future-format generation was removed: %v", err)
	}
}

func TestSweepOfAnUnusedPolicyIsANoOp(t *testing.T) {
	store, _ := newTestStore(t)
	res, err := store.Sweep("never-used")
	if err != nil {
		t.Fatalf("sweeping a policy with no directory failed: %v", err)
	}
	if len(res.Removed) != 0 || len(res.Refused) != 0 {
		t.Errorf("sweeping an unused policy did something: %+v", res)
	}
}

func TestSweepRefusesWhenItCannotTellWhetherAManifestExists(t *testing.T) {
	// An inability to check is not permission to delete. A Stat that
	// fails for a reason other than "does not exist" is exactly this case.
	wfs := newWatchedFS()
	store, _ := newTestStore(t, WithFS(wfs))
	gen, err := store.Begin("nightly", "gen-1")
	if err != nil {
		t.Fatal(err)
	}
	if err := gen.Commit(t.Context(), sampleGenerationManifest("nightly", "gen-1")); err != nil {
		t.Fatal(err)
	}
	wfs.mu.Lock()
	wfs.fail = func(op, _ string) error {
		if op == "Stat" {
			return errors.New("injected: I/O error")
		}
		return nil
	}
	wfs.mu.Unlock()

	res, err := store.Sweep("nightly")
	if err != nil {
		t.Fatalf("Sweep returned an error instead of refusing: %v", err)
	}
	if len(res.Removed) != 0 {
		t.Errorf("swept %v while unable to check whether a manifest existed", res.Removed)
	}
	if len(res.Refused) != 1 || !containsAll(res.Refused[0].Reason, "not permission to delete") {
		t.Errorf("refusals = %+v, want one naming the inability to check", res.Refused)
	}
}

func TestStoreRefusesIDsThatCouldEscapeTheTarget(t *testing.T) {
	store, _ := newTestStore(t)
	for _, tc := range []struct{ what, policy, generation string }{
		{"separator in policy", "nightly/../etc", "gen"},
		{"traversal in generation", "nightly", "../../etc"},
		{"absolute in policy", "/etc", "gen"},
		{"empty policy", "", "gen"},
		{"empty generation", "nightly", ""},
		{"dotdot policy", "..", "gen"},
		{"space", "night ly", "gen"},
	} {
		t.Run(tc.what, func(t *testing.T) {
			if _, err := store.Begin(tc.policy, tc.generation); err == nil {
				t.Fatalf("Begin(%q, %q) was accepted", tc.policy, tc.generation)
			}
		})
	}
}

func TestGenerationArtifactsReturnsACopy(t *testing.T) {
	store, _ := newTestStore(t)
	gen, err := store.Begin("nightly", "gen-1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := gen.WriteBytes(t.Context(), ArtifactSpec{
		ID:      "one",
		Kind:    KindDataset,
		Dataset: &DatasetArtifact{Dataset: "tank/one", Snapshot: "s"},
	}, []byte("payload")); err != nil {
		t.Fatal(err)
	}
	got := gen.Artifacts()
	got[0].SHA256 = strings.Repeat("ff", 32)
	if again := gen.Artifacts(); again[0].SHA256 == strings.Repeat("ff", 32) {
		t.Error("a caller mutated the generation's own artifact list through the copy it was handed")
	}
}
