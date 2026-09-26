package backup

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// buildGeneration writes a real generation on disk with n payload
// artifacts of known contents, and returns the store and the artifacts
// the manifest recorded. Every verification test starts from one of
// these, so a verdict is always asserted against a generation that
// genuinely exists rather than a description of one.
func buildGeneration(t *testing.T, bodies ...string) (*Store, []Artifact) {
	t.Helper()
	store, _ := newTestStore(t)
	gen, err := store.Begin("nightly", "gen-1")
	if err != nil {
		t.Fatal(err)
	}
	var arts []Artifact
	for i, body := range bodies {
		a, err := gen.WriteBytes(t.Context(), ArtifactSpec{
			ID:      "art-" + string(rune('a'+i)),
			Kind:    KindDataset,
			Dataset: &DatasetArtifact{Dataset: "tank/" + string(rune('a'+i)), Snapshot: "s", SourceGUID: "12345"},
		}, []byte(body))
		if err != nil {
			t.Fatal(err)
		}
		arts = append(arts, a)
	}
	m := sampleGenerationManifest("nightly", "gen-1")
	m.Artifacts = arts
	if err := gen.Commit(t.Context(), m); err != nil {
		t.Fatal(err)
	}
	return store, arts
}

// TestVerifyReportsThreeDistinctVerdicts is requirement 5, and the three
// answers are tested together on purpose: a test that checks each in
// isolation would pass even if the code collapsed "missing" into
// "mismatch" whenever only one of them was present at a time.
func TestVerifyReportsThreeDistinctVerdicts(t *testing.T) {
	store, arts := buildGeneration(t, "good payload", "corrupted payload", "gone payload")
	dir := store.GenerationDir("nightly", "gen-1")

	// Corrupt one artifact's bytes, same length, so only the digest can
	// tell. An in-place single-byte change is the real shape of bit rot
	// and of a lying writer, and it keeps the size check from doing the
	// work the checksum test is meant to do.
	corruptPath := joinPath(dir, arts[1].RelativePath)
	raw, err := os.ReadFile(corruptPath)
	if err != nil {
		t.Fatal(err)
	}
	raw[0] ^= 0xff
	if err := os.WriteFile(corruptPath, raw, 0o600); err != nil {
		t.Fatal(err)
	}

	// Delete a third.
	if err := os.Remove(joinPath(dir, arts[2].RelativePath)); err != nil {
		t.Fatal(err)
	}

	v, err := store.Verify(t.Context(), "nightly", "gen-1", VerifyOptions{})
	if err != nil {
		t.Fatalf("Verify returned an error for a readable manifest: %v", err)
	}
	if !v.Understood || v.State != GenerationOK {
		t.Fatalf("the manifest itself is %s/%v, want understood and ok", v.State, v.Understood)
	}
	if !v.ChecksumVerified {
		t.Error("ChecksumVerified is false; the manifest's own checksum was not recomputed")
	}
	if v.Verified != 1 || v.Mismatched != 1 || v.Missing != 1 {
		t.Fatalf("counts are %d verified / %d mismatch / %d missing, want 1/1/1: %+v", v.Verified, v.Mismatched, v.Missing, v.Results)
	}
	if v.Ok() {
		t.Error("a generation with a corrupt and a missing artifact reports Ok")
	}

	byID := map[string]ArtifactCheck{}
	for _, c := range v.Results {
		byID[c.ArtifactID] = c
	}

	good := byID[arts[0].ArtifactID]
	if good.Verdict != ArtifactVerified {
		t.Errorf("intact artifact: verdict %q, want %q", good.Verdict, ArtifactVerified)
	}
	if good.ObservedSHA256 != good.ExpectedSHA256 {
		t.Errorf("intact artifact: observed %s, expected %s", good.ObservedSHA256, good.ExpectedSHA256)
	}
	if !containsAll(good.Detail, "hash to the recorded digest") {
		t.Errorf("intact artifact: detail does not say what was checked: %s", good.Detail)
	}

	bad := byID[arts[1].ArtifactID]
	if bad.Verdict != ArtifactChecksumMismatch {
		t.Errorf("corrupted artifact: verdict %q, want %q - present and wrong is corruption", bad.Verdict, ArtifactChecksumMismatch)
	}
	if !bad.Verdict.Terminal() {
		t.Error("a mismatch is not reported as terminal")
	}
	if !containsAll(bad.Detail, "hashes to", "not the bytes that were captured") {
		t.Errorf("corrupted artifact: detail does not distinguish corruption from absence: %s", bad.Detail)
	}
	if bad.ObservedBytes != bad.ExpectedBytes {
		t.Errorf("corrupted artifact: sizes differ (%d vs %d), so this test is not exercising the digest", bad.ObservedBytes, bad.ExpectedBytes)
	}

	gone := byID[arts[2].ArtifactID]
	if gone.Verdict != ArtifactMissing {
		t.Errorf("absent artifact: verdict %q, want %q - a missing artifact is not a corrupt one", gone.Verdict, ArtifactMissing)
	}
	if gone.ObservedSHA256 != "" {
		t.Errorf("absent artifact: observed digest %q, want empty - nothing was read", gone.ObservedSHA256)
	}
	if !containsAll(gone.Detail, "no file at") {
		t.Errorf("absent artifact: detail does not say the file is not there: %s", gone.Detail)
	}

	if s := v.Summary(); !containsAll(s, "1 verified", "1 checksum mismatch", "1 missing") {
		t.Errorf("Summary = %q, want all three counts", s)
	}
}

// TestAMissingArtifactIsNeverReportedAsCorruption walks every way an
// artifact can fail to be readable and asserts each lands in the missing
// bucket, never the mismatch one. These are the cases that a naive
// implementation gets wrong, and each sends an operator somewhere
// different.
func TestAMissingArtifactIsNeverReportedAsCorruption(t *testing.T) {
	cases := []struct {
		name    string
		arrange func(t *testing.T, full string)
		wantSub string
	}{
		{
			name:    "file removed",
			arrange: func(t *testing.T, full string) { mustRemove(t, full) },
			wantSub: "no file at",
		},
		{
			name:    "directory in its place",
			arrange: func(t *testing.T, full string) { mustRemove(t, full); mustMkdir(t, full) },
			wantSub: "is a directory, not a file",
		},
		{
			name:    "the parent directory is gone",
			arrange: func(t *testing.T, full string) { mustRemove(t, filepath.Dir(full)) },
			wantSub: "no file at",
		},
		{
			name: "the file cannot be opened",
			arrange: func(t *testing.T, full string) {
				// chmod 000 as a non-root user makes it unreadable. Root
				// ignores the mode, and a test that silently stopped
				// testing anything under root would be worse than not
				// testing it, so the case is skipped instead.
				if os.Geteuid() == 0 {
					t.Skip("running as root: mode bits do not make a file unreadable, so this case would not test anything")
				}
				if err := os.Chmod(full, 0o000); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = os.Chmod(full, 0o600) })
			},
			wantSub: "could not be",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Each case gets a fresh generation so the arrangements
			// cannot interfere with one another.
			store, arts := buildGeneration(t, "only payload")
			dir := store.GenerationDir("nightly", "gen-1")
			full := joinPath(dir, arts[0].RelativePath)
			tc.arrange(t, full)

			v, err := store.Verify(t.Context(), "nightly", "gen-1", VerifyOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if len(v.Results) != 1 {
				t.Fatalf("got %d results, want 1", len(v.Results))
			}
			c := v.Results[0]
			if c.Verdict != ArtifactMissing {
				t.Errorf("verdict %q, want %q", c.Verdict, ArtifactMissing)
			}
			if c.Verdict == ArtifactChecksumMismatch {
				t.Error("an unreadable artifact was reported as corruption")
			}
			if v.Missing != 1 || v.Mismatched != 0 {
				t.Errorf("counts are %d missing / %d mismatch, want 1/0", v.Missing, v.Mismatched)
			}
			if !containsAll(c.Detail, tc.wantSub) {
				t.Errorf("detail %q does not contain %q, so an operator is not told which kind of absence this is", c.Detail, tc.wantSub)
			}
		})
	}
}

// TestAShortFileIsCorruptionNotAbsence is the mirror image: a file that
// is there and the wrong length is present-and-wrong.
func TestAShortFileIsCorruptionNotAbsence(t *testing.T) {
	store, arts := buildGeneration(t, "a payload of some length")
	dir := store.GenerationDir("nightly", "gen-1")
	full := joinPath(dir, arts[0].RelativePath)
	if err := os.WriteFile(full, []byte("short"), 0o600); err != nil {
		t.Fatal(err)
	}
	v, err := store.Verify(t.Context(), "nightly", "gen-1", VerifyOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if v.Results[0].Verdict != ArtifactChecksumMismatch {
		t.Errorf("verdict %q, want %q - the payload is present and is not what was captured", v.Results[0].Verdict, ArtifactChecksumMismatch)
	}
	if !containsAll(v.Results[0].Detail, "bytes", "present but not the payload that was captured") {
		t.Errorf("detail = %q", v.Results[0].Detail)
	}
	if v.Missing != 0 {
		t.Errorf("Missing = %d, want 0 - a truncated file is not an absent one", v.Missing)
	}
}

// TestAReferenceIsNeverReportedAsVerified: hashing nothing is not
// verification.
func TestAReferenceIsNeverReportedAsVerified(t *testing.T) {
	store, _ := newTestStore(t)
	gen, err := store.Begin("nightly", "gen-1")
	if err != nil {
		t.Fatal(err)
	}
	a, err := gen.WriteBytes(t.Context(), ArtifactSpec{
		ID:      "payload",
		Kind:    KindDataset,
		Dataset: &DatasetArtifact{Dataset: "tank/one", Snapshot: "s"},
	}, []byte("payload"))
	if err != nil {
		t.Fatal(err)
	}
	m := sampleGenerationManifest("nightly", "gen-1")
	m.Artifacts = []Artifact{a, NewReferenceSpec("base-image", "some-base-image", "base_image", strings.Repeat("34", 32)).artifactOrFail(t)}
	if err := gen.Commit(t.Context(), m); err != nil {
		t.Fatal(err)
	}

	v, err := store.Verify(t.Context(), "nightly", "gen-1", VerifyOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if v.Verified != 1 {
		t.Errorf("Verified = %d, want 1 - only the payload was checked", v.Verified)
	}
	if len(v.Results) != 1 {
		t.Fatalf("Results has %d entries, want 1; the reference belongs in NotApplicable", len(v.Results))
	}
	if len(v.NotApplicable) != 1 {
		t.Fatalf("NotApplicable has %d entries, want 1", len(v.NotApplicable))
	}
	na := v.NotApplicable[0]
	if na.ArtifactID != "base-image" {
		t.Errorf("NotApplicable names %q", na.ArtifactID)
	}
	if !containsAll(na.Reason, "no payload", "nothing here has retrieved anything") {
		t.Errorf("the reason does not say what was not checked: %s", na.Reason)
	}
	if sum := v.Summary(); !containsAll(sum, "1 verified", "1 with no payload to check") {
		t.Errorf("Summary = %q, want the unchecked count reported separately from the failed one", sum)
	}
}

// TestOkIsFalseWhenNothingWasChecked: "0 of 0 verified" is not a pass.
func TestOkIsFalseWhenNothingWasChecked(t *testing.T) {
	store, _ := newTestStore(t)
	gen, err := store.Begin("nightly", "gen-1")
	if err != nil {
		t.Fatal(err)
	}
	m := sampleGenerationManifest("nightly", "gen-1")
	m.Artifacts = []Artifact{NewReferenceSpec("only-a-reference", "x", "base_image", strings.Repeat("34", 32)).artifactOrFail(t)}
	if err := gen.Commit(t.Context(), m); err != nil {
		t.Fatal(err)
	}
	v, err := store.Verify(t.Context(), "nightly", "gen-1", VerifyOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if v.Verified != 0 {
		t.Fatalf("Verified = %d, want 0", v.Verified)
	}
	if v.Ok() {
		t.Error("a generation with nothing checkable reports Ok; nothing was established and that is not a pass")
	}
}

// TestVerifyHashesNoArtifactWhenTheManifestIsUnusable: verifying a
// description against bytes establishes nothing, and reporting
// per-artifact verdicts in that case would read as evidence about data.
func TestVerifyHashesNoArtifactWhenTheManifestIsUnusable(t *testing.T) {
	cases := []struct {
		name      string
		mangle    func(t *testing.T, dir string)
		wantState GenerationState
	}{
		{
			name: "future format",
			mangle: func(t *testing.T, dir string) {
				m := sampleGenerationManifest("nightly", "gen-1")
				m.FormatVersion = 42
				body, err := EncodeManifest(JSONCodec{}, &m)
				if err != nil {
					t.Fatal(err)
				}
				mustWrite(t, joinPath(dir, ManifestName), body)
			},
			wantState: GenerationUnknownVersion,
		},
		{
			name: "corrupt bytes",
			mangle: func(t *testing.T, dir string) {
				mustWrite(t, joinPath(dir, ManifestName), []byte(`{"format_version":1,"comb_id":"x"`))
			},
			wantState: GenerationUnreadable,
		},
		{
			name: "no manifest at all",
			mangle: func(t *testing.T, dir string) {
				mustRemove(t, joinPath(dir, ManifestName))
			},
			wantState: GenerationIncomplete,
		},
		{
			name: "well-formed but unsafe path",
			mangle: func(t *testing.T, dir string) {
				m := sampleGenerationManifest("nightly", "gen-1")
				m.Artifacts = []Artifact{{
					ArtifactID:   "escape",
					Kind:         KindDataset,
					RelativePath: "artifacts/../../../etc/shadow",
					SHA256:       "aa" + "bb" + "cc",
					SizeBytes:    1,
					Dataset:      &DatasetArtifact{Dataset: "tank/one", Snapshot: "s"},
				}}
				m.Artifacts[0].SHA256 = "0000000000000000000000000000000000000000000000000000000000000000"
				m.Artifacts[0].RelativePath = "artifacts/../../../etc/shadow"
				// Bypass Validate so the document is well-formed JSON with
				// a correct self-checksum but an unsafe path. This is
				// exactly the shape a hand-edited or hostile archive has.
				body, err := encodeUnsafeManifest(&m)
				if err != nil {
					t.Fatal(err)
				}
				mustWrite(t, joinPath(dir, ManifestName), body)
			},
			wantState: GenerationInconsistent,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store, _ := buildGeneration(t, "payload")
			dir := store.GenerationDir("nightly", "gen-1")
			tc.mangle(t, dir)

			v, err := store.Verify(t.Context(), "nightly", "gen-1", VerifyOptions{})
			if err == nil {
				t.Fatal("Verify reported success for an unusable manifest")
			}
			if v.State != tc.wantState {
				t.Errorf("state = %q, want %q (reason: %s)", v.State, tc.wantState, v.Reason)
			}
			if v.Understood {
				t.Error("Understood is true for a manifest this build cannot act on")
			}
			if len(v.Results) != 0 {
				t.Errorf("Results has %d entries; nothing should have been hashed against a manifest that cannot be read", len(v.Results))
			}
			if v.Verified != 0 || v.Mismatched != 0 || v.Missing != 0 {
				t.Errorf("counts are non-zero (%d/%d/%d) for an unreadable manifest", v.Verified, v.Mismatched, v.Missing)
			}
			if s := v.Summary(); !containsAll(s, "no artifact was checked") {
				t.Errorf("Summary = %q, want it to say nothing was checked", s)
			}
		})
	}
}

// TestHashBudgetLeavesWholeArtifactsUnchecked: the budget is spent on
// whole artifacts, and whatever it does not cover is reported as
// unchecked rather than as verified.
//
// A prefix-based sample was the obvious design and it is wrong: a
// manifest records one digest per artifact, so there is nothing to
// compare a prefix against. Inventing a second digest to compare would
// mean the verifier had produced the evidence it is checking.
func TestHashBudgetLeavesWholeArtifactsUnchecked(t *testing.T) {
	big := make([]byte, 4096)
	for i := range big {
		big[i] = byte('a' + i%26)
	}
	store, _ := newTestStore(t)
	gen, err := store.Begin("nightly", "gen-1")
	if err != nil {
		t.Fatal(err)
	}
	var arts []Artifact
	for i, body := range [][]byte{[]byte("small one"), []byte("small two"), big} {
		a, err := gen.WriteBytes(t.Context(), ArtifactSpec{
			ID:      "art-" + string(rune('a'+i)),
			Kind:    KindDataset,
			Dataset: &DatasetArtifact{Dataset: "tank/x", Snapshot: "s"},
		}, body)
		if err != nil {
			t.Fatal(err)
		}
		arts = append(arts, a)
	}
	m := sampleGenerationManifest("nightly", "gen-1")
	m.Artifacts = arts
	if err := gen.Commit(t.Context(), m); err != nil {
		t.Fatal(err)
	}

	// Enough budget for the two small artifacts and not the third.
	budget := int64(len("small one") + len("small two"))
	v, err := store.Verify(t.Context(), "nightly", "gen-1", VerifyOptions{HashBudgetBytes: budget})
	if err != nil {
		t.Fatal(err)
	}
	if v.Verified != 2 {
		t.Errorf("Verified = %d, want 2", v.Verified)
	}
	if v.Ok() {
		t.Error("a pass that skipped the largest artifact reports Ok; two of three were checked and that is not a clean bill of health")
	}
	if len(v.NotApplicable) != 1 || v.NotApplicable[0].ArtifactID != arts[2].ArtifactID {
		t.Fatalf("NotApplicable = %+v, want the third artifact reported as unchecked", v.NotApplicable)
	}
	if !v.NotApplicable[0].BudgetSkipped {
		t.Error("the budget skip is not distinguished from a reference with no payload")
	}
	if v.Skipped != 1 {
		t.Errorf("Skipped = %d, want 1", v.Skipped)
	}
	if !containsAll(v.NotApplicable[0].Reason, "not hashed", "NOT verified and NOT failed") {
		t.Errorf("the reason does not bound the claim: %s", v.NotApplicable[0].Reason)
	}
	if sum := v.Summary(); !containsAll(sum, "2 verified", "1 not hashed") {
		t.Errorf("Summary = %q, want the unchecked count reported separately", sum)
	}

	// Corruption in an artifact the budget skipped is not found, and the
	// pass does not claim it was.
	full := joinPath(gen.Dir(), arts[2].RelativePath)
	corrupt := append([]byte(nil), big...)
	corrupt[3000] ^= 0xff
	if err := os.WriteFile(full, corrupt, 0o600); err != nil {
		t.Fatal(err)
	}
	v, err = store.Verify(t.Context(), "nightly", "gen-1", VerifyOptions{HashBudgetBytes: budget})
	if err != nil {
		t.Fatal(err)
	}
	if v.Mismatched != 0 {
		t.Error("a budgeted pass found a corruption in an artifact it never hashed")
	}
	// And an unbounded pass finds it, which is what makes the caveat a
	// statement of fact rather than caution.
	v, err = store.Verify(t.Context(), "nightly", "gen-1", VerifyOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if v.Verified != 2 || v.Mismatched != 1 || v.Ok() {
		t.Errorf("a full pass did not check all three and find the corruption: %d verified, %d mismatched, ok=%v", v.Verified, v.Mismatched, v.Ok())
	}

	// A budget too small for even one artifact checks nothing, and says
	// so rather than reporting a pass.
	v, err = store.Verify(t.Context(), "nightly", "gen-1", VerifyOptions{HashBudgetBytes: 1})
	if err != nil {
		t.Fatal(err)
	}
	if v.Verified != 0 || v.Ok() {
		t.Errorf("a one-byte budget reported %d verified and ok=%v", v.Verified, v.Ok())
	}
}

func TestSkipHashIsNotReportedAsIntegrityProof(t *testing.T) {
	store, _ := buildGeneration(t, "payload one", "payload two")
	dir := store.GenerationDir("nightly", "gen-1")
	// Corrupt one, preserving its length.
	entries, err := os.ReadDir(joinPath(dir, artifactsDirName))
	if err != nil {
		t.Fatal(err)
	}
	target := joinPath(dir, artifactsDirName, entries[0].Name())
	raw, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	raw[0] ^= 0xff
	if err := os.WriteFile(target, raw, 0o600); err != nil {
		t.Fatal(err)
	}

	v, err := store.Verify(t.Context(), "nightly", "gen-1", VerifyOptions{SkipHash: true})
	if err != nil {
		t.Fatal(err)
	}
	if v.Verified != 2 {
		t.Errorf("Verified = %d; presence and size were checked and both files are the recorded length, so 2 is right for THIS option", v.Verified)
	}
	for _, c := range v.Results {
		if !containsAll(c.Detail, "NOT a verification") {
			t.Errorf("artifact %s does not say the bytes were not hashed: %s", c.ArtifactID, c.Detail)
		}
		if c.ObservedSHA256 != "" {
			t.Errorf("artifact %s reports an observed digest %q although nothing was hashed", c.ArtifactID, c.ObservedSHA256)
		}
	}
	// And with hashing on, the corruption is found - which is what makes
	// the SkipHash caveat a statement of fact rather than caution.
	v, err = store.Verify(t.Context(), "nightly", "gen-1", VerifyOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if v.Mismatched != 1 {
		t.Errorf("Mismatched = %d, want 1", v.Mismatched)
	}
}

func TestVerifyHonoursCancellation(t *testing.T) {
	store, _ := buildGeneration(t, "a", "b", "c")
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := store.Verify(ctx, "nightly", "gen-1", VerifyOptions{})
	if err == nil {
		t.Fatal("a cancelled verification reported success")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("error is %v, want context.Canceled", err)
	}
}

func TestVerdictPredicates(t *testing.T) {
	if ArtifactVerified.Terminal() || !ArtifactVerified.Verified() {
		t.Error("verified is reported as terminal or not verified")
	}
	for _, v := range []ArtifactVerdict{ArtifactChecksumMismatch, ArtifactMissing} {
		if !v.Terminal() {
			t.Errorf("%q should be terminal", v)
		}
		if v.Verified() {
			t.Errorf("%q should not report itself as verified", v)
		}
	}
}

func TestLoadDirReadsAnArchiveCopiedOffADeadHost(t *testing.T) {
	// The reason the manifest is self-describing: an archive that was
	// copied onto removable media and read back by a build that has no
	// access to the Store's root.
	store, _ := buildGeneration(t, "payload")
	src := store.GenerationDir("nightly", "gen-1")
	dst := t.TempDir()
	copyTree(t, src, dst)

	m, info, err := LoadDir(dst)
	if err != nil {
		t.Fatalf("LoadDir: %v (state %s: %s)", err, info.State, info.Reason)
	}
	if !info.Understood || m.GenerationID != "gen-1" {
		t.Errorf("read back %+v", info)
	}
	if len(m.SecretGaps) == 0 {
		t.Error("the archive read back from a copy carries no secret gaps, so a restore operator would not be told what to re-issue")
	}
}

// # Restore planning

func TestPlanRestoreRefusesEachGuardAndNamesIt(t *testing.T) {
	// Every artifact carries a source_guid, so the in-place GUID guard has
	// something to compare in the cases where it is supposed to pass.
	store, _ := buildGeneration(t, "payload")

	cases := []struct {
		name      string
		mode      RestoreMode
		ropts     RestoreOptions
		wantGuard string
	}{
		{
			name:      "in place without an observed stop",
			mode:      RestoreInPlace,
			ropts:     RestoreOptions{Clobber: true, LiveDatasetGUID: "12345"},
			wantGuard: "observed stopped",
		},
		{
			name:      "in place without clobber",
			mode:      RestoreInPlace,
			ropts:     RestoreOptions{ObservedStopped: true, LiveDatasetGUID: "12345"},
			wantGuard: "explicit clobber",
		},
		{
			name:      "in place with a mismatched GUID",
			mode:      RestoreInPlace,
			ropts:     RestoreOptions{ObservedStopped: true, Clobber: true, LiveDatasetGUID: "99999"},
			wantGuard: "dataset GUID",
		},
		{
			name:      "out of band with no destination",
			mode:      RestoreOutOfBand,
			ropts:     RestoreOptions{},
			wantGuard: "destination",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			plan, err := store.PlanRestore(t.Context(), "nightly", "gen-1", tc.mode, VerifyOptions{}, tc.ropts)
			if err == nil {
				t.Fatal("the plan was eligible despite an unmet guard")
			}
			if plan.Eligible {
				t.Fatal("Eligible is true despite an unmet guard")
			}
			if plan.Guard != tc.wantGuard {
				t.Errorf("Guard = %q, want %q (reason: %s)", plan.Guard, tc.wantGuard, plan.Reason)
			}
			if plan.Reason == "" {
				t.Error("the refusal has no reason; an operator who cannot tell which guard stopped them will try again harder")
			}
		})
	}

	t.Run("in place with every guard met", func(t *testing.T) {
		plan, err := store.PlanRestore(t.Context(), "nightly", "gen-1", RestoreInPlace, VerifyOptions{},
			RestoreOptions{ObservedStopped: true, Clobber: true, LiveDatasetGUID: "12345"})
		if err != nil {
			t.Fatalf("a fully guarded in-place restore was refused: %v (%s: %s)", err, plan.Guard, plan.Reason)
		}
		if !plan.Eligible {
			t.Fatal("Eligible is false despite every guard being met")
		}
		if len(plan.SecretGaps) == 0 {
			t.Error("the plan does not carry the secret gaps, so the surface could not tell the operator what to re-issue before they press the button")
		}
	})

	t.Run("out of band needs no clobber or stop", func(t *testing.T) {
		plan, err := store.PlanRestore(t.Context(), "nightly", "gen-1", RestoreOutOfBand, VerifyOptions{},
			RestoreOptions{DestDataset: "tank/restored"})
		if err != nil {
			t.Fatalf("out-of-band restore was refused: %v (%s: %s)", err, plan.Guard, plan.Reason)
		}
		if !plan.Eligible {
			t.Fatal("an out-of-band restore into a named dataset is not eligible")
		}
	})
}

func TestInPlaceRestoreIsIneligibleWithoutARecordedGUID(t *testing.T) {
	store, _ := newTestStore(t)
	gen, err := store.Begin("nightly", "gen-1")
	if err != nil {
		t.Fatal(err)
	}
	a, err := gen.WriteBytes(t.Context(), ArtifactSpec{
		ID:      "no-guid",
		Kind:    KindDataset,
		Dataset: &DatasetArtifact{Dataset: "tank/one", Snapshot: "s"},
	}, []byte("payload"))
	if err != nil {
		t.Fatal(err)
	}
	m := sampleGenerationManifest("nightly", "gen-1")
	m.Artifacts = []Artifact{a}
	if err := gen.Commit(t.Context(), m); err != nil {
		t.Fatal(err)
	}

	plan, err := store.PlanRestore(t.Context(), "nightly", "gen-1", RestoreInPlace, VerifyOptions{},
		RestoreOptions{ObservedStopped: true, Clobber: true, LiveDatasetGUID: "12345"})
	if err == nil {
		t.Fatal("an in-place restore was allowed for a manifest with no source_guid")
	}
	if plan.Guard != "dataset GUID" {
		t.Errorf("Guard = %q", plan.Guard)
	}
	if !containsAll(plan.Reason, "INELIGIBLE", "not a passing guard", "out-of-band restore remains available") {
		t.Errorf("the refusal does not explain the reasoning or the alternative: %s", plan.Reason)
	}
	// And out-of-band still works, because it does not need the GUID.
	plan, err = store.PlanRestore(t.Context(), "nightly", "gen-1", RestoreOutOfBand, VerifyOptions{},
		RestoreOptions{DestDataset: "tank/restored"})
	if err != nil {
		t.Errorf("out-of-band restore was refused for a manifest with no source_guid: %v", err)
	}
}

func TestPlanRestoreRefusesAVerificationFailure(t *testing.T) {
	store, arts := buildGeneration(t, "payload")
	full := joinPath(store.GenerationDir("nightly", "gen-1"), arts[0].RelativePath)
	raw, err := os.ReadFile(full)
	if err != nil {
		t.Fatal(err)
	}
	raw[0] ^= 0xff
	if err := os.WriteFile(full, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	plan, err := store.PlanRestore(t.Context(), "nightly", "gen-1", RestoreOutOfBand, VerifyOptions{},
		RestoreOptions{DestDataset: "tank/restored"})
	if err == nil {
		t.Fatal("a restore was planned for a generation whose bytes do not match its manifest")
	}
	if plan.Guard != "artifact verification" {
		t.Errorf("Guard = %q, want %q", plan.Guard, "artifact verification")
	}
}

// # Materialise, which reuses internal/jailarchive's Extractor

func TestMaterialiseVerifiesBeforeItUnpacks(t *testing.T) {
	store, arts := buildGeneration(t, "archive bytes")
	ex := &recordedExtractor{}
	dest := t.TempDir()

	// Intact: the extraction happens, into the right place, from the
	// right file.
	if err := store.Materialise(t.Context(), "nightly", "gen-1", arts[0].ArtifactID, MaterialiseOptions{
		Extractor: ex, DestDir: dest,
	}); err != nil {
		t.Fatalf("Materialise: %v", err)
	}
	if len(ex.calls) != 1 {
		t.Fatalf("extractor was called %d times, want 1", len(ex.calls))
	}
	if ex.calls[0].DestDir != dest {
		t.Errorf("extracted into %q, want %q", ex.calls[0].DestDir, dest)
	}
	if want := joinPath(store.GenerationDir("nightly", "gen-1"), arts[0].RelativePath); ex.calls[0].ArchivePath != want {
		t.Errorf("extracted from %q, want %q", ex.calls[0].ArchivePath, want)
	}

	// Corrupt: nothing is extracted at all. Extracting first and
	// verifying afterwards would put corruption into a place an operator
	// will then believe.
	full := joinPath(store.GenerationDir("nightly", "gen-1"), arts[0].RelativePath)
	raw, err := os.ReadFile(full)
	if err != nil {
		t.Fatal(err)
	}
	raw[0] ^= 0xff
	if err := os.WriteFile(full, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	ex2 := &recordedExtractor{}
	err = store.Materialise(t.Context(), "nightly", "gen-1", arts[0].ArtifactID, MaterialiseOptions{
		Extractor: ex2, DestDir: t.TempDir(),
	})
	if err == nil {
		t.Fatal("Materialise unpacked an artifact that does not match the manifest")
	}
	if len(ex2.calls) != 0 {
		t.Errorf("the extractor was called %d times for a corrupt artifact; it must not be called at all", len(ex2.calls))
	}
	if !containsAll(err.Error(), "refusing to unpack", string(ArtifactChecksumMismatch)) {
		t.Errorf("the refusal does not name the verdict: %v", err)
	}
}

func TestMaterialiseRefusesAnUnknownArtifactOrAReference(t *testing.T) {
	store, _ := buildGeneration(t, "payload")
	dest := t.TempDir()
	err := store.Materialise(t.Context(), "nightly", "gen-1", "no-such-artifact", MaterialiseOptions{
		Extractor: &recordedExtractor{}, DestDir: dest,
	})
	if err == nil || !containsAll(err.Error(), "no artifact") {
		t.Errorf("an unknown artifact id returned %v", err)
	}

	// A reference has no payload; the message has to say how to handle
	// it rather than just refusing.
	gen, err := store.Begin("nightly", "gen-2")
	if err != nil {
		t.Fatal(err)
	}
	m := sampleGenerationManifest("nightly", "gen-2")
	m.Artifacts = []Artifact{NewReferenceSpec("ref", "x", "base_image", strings.Repeat("34", 32)).artifactOrFail(t)}
	if err := gen.Commit(t.Context(), m); err != nil {
		t.Fatal(err)
	}
	err = store.Materialise(t.Context(), "nightly", "gen-2", "ref", MaterialiseOptions{
		Extractor: &recordedExtractor{}, DestDir: dest,
	})
	if err == nil || !containsAll(err.Error(), "re-fetching it and checking its expected digest") {
		t.Errorf("materialising a reference returned %v; the message should say how a reference is actually restored", err)
	}
}

func TestMaterialiseSpoolsAndVerifiesTheCopy(t *testing.T) {
	store, arts := buildGeneration(t, "archive bytes")
	spool := t.TempDir()
	dest := t.TempDir()
	var seen string
	ex := &recordedExtractor{onExtract: func(archivePath, _ string) error {
		seen = archivePath
		body, err := os.ReadFile(archivePath)
		if err != nil {
			return err
		}
		if string(body) != "archive bytes" {
			return errors.New("the spooled copy does not have the artifact's contents")
		}
		return nil
	}}
	if err := store.Materialise(t.Context(), "nightly", "gen-1", arts[0].ArtifactID, MaterialiseOptions{
		Extractor: ex, DestDir: dest, SpoolDir: spool,
	}); err != nil {
		t.Fatal(err)
	}
	if filepath.Dir(seen) != spool {
		t.Errorf("extracted from %q, want a file in the spool directory %q", seen, spool)
	}
	// The spool is named by digest, so two generations of the same
	// artifact id with the same bytes deduplicate and a spooled file can
	// never be mistaken for one from a different generation.
	if filepath.Base(seen) != arts[0].SHA256+".stream" {
		t.Errorf("spooled name %q, want the artifact's digest", filepath.Base(seen))
	}
}

func TestRealJailArchiveExtractorIsWired(t *testing.T) {
	// The whole point of Rejected alternative 4 being rejected: this
	// package must not have written a second unpacker. The real one is
	// internal/jailarchive's, reachable through this constructor, and it
	// is exercised here for real on a real tar so the wiring is not just
	// a compile-time claim.
	if _, err := os.Stat("/bin/tar"); err != nil {
		if _, err := os.Stat("/usr/bin/tar"); err != nil {
			t.Skip("no tar(1) on this system; the jailarchive Extractor cannot be exercised")
		}
	}
	store, _ := buildGeneration(t, "not a real archive")
	ex := NewJailArchiveExtractor()
	// On the deliberately non-archive payload the real extractor must
	// fail - which is the behaviour that matters: the call really
	// reaches tar(1) rather than a stub.
	err := store.Materialise(t.Context(), "nightly", "gen-1", "art-a", MaterialiseOptions{
		Extractor: ex, DestDir: t.TempDir(),
	})
	if err == nil {
		t.Fatal("the real jailarchive Extractor accepted a payload that is not an archive, so it probably was not reached")
	}
	if !containsAll(err.Error(), "unpacking artifact") {
		t.Errorf("the error does not come from Materialise's own wrapping: %v", err)
	}
}

// # small helpers

// artifactOrFail turns a spec into the artifact a manifest carries, for
// the tests that need one without a generation.
func (s ArtifactSpec) artifactOrFail(t *testing.T) Artifact {
	t.Helper()
	a, err := s.artifact()
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func mustWrite(t *testing.T, path string, body []byte) {
	t.Helper()
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
}

func mustRemove(t *testing.T, path string) {
	t.Helper()
	if err := os.RemoveAll(path); err != nil {
		t.Fatal(err)
	}
}

func mustMkdir(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o700); err != nil {
		t.Fatal(err)
	}
}

func copyTree(t *testing.T, src, dst string) {
	t.Helper()
	entries, err := os.ReadDir(src)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		sp := filepath.Join(src, e.Name())
		dp := filepath.Join(dst, e.Name())
		if e.IsDir() {
			if err := os.MkdirAll(dp, 0o700); err != nil {
				t.Fatal(err)
			}
			copyTree(t, sp, dp)
			continue
		}
		body, err := os.ReadFile(sp)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(dp, body, 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

var _ = time.Now
