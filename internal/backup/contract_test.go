package backup

import (
	"errors"
	"regexp"
	"strings"
	"testing"
	"time"
)

// checksumField matches the encoded manifest's own checksum field, so a
// test can corrupt exactly that field and nothing else.
var checksumField = regexp.MustCompile(`"checksum":"[0-9a-f]{64}"`)

// TestCommitAlwaysCarriesTheGapList: a manifest that does not say what
// it left out is the failure mode, so Commit fills the list in rather
// than trusting the caller to have.
func TestCommitAlwaysCarriesTheGapList(t *testing.T) {
	store, _ := newTestStore(t)
	gen, err := store.Begin("nightly", "gen-1")
	if err != nil {
		t.Fatal(err)
	}
	// Deliberately pass a manifest with no gaps.
	m := sampleGenerationManifest("nightly", "gen-1")
	m.SecretGaps = nil
	if err := gen.Commit(t.Context(), m); err != nil {
		t.Fatal(err)
	}
	loaded, info, err := store.Load("nightly", "gen-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.SecretGaps) != len(SecretGaps()) {
		t.Errorf("the committed manifest carries %d gaps, want the full %d", len(loaded.SecretGaps), len(SecretGaps()))
	}
	sums, err := store.List("nightly")
	if err != nil {
		t.Fatal(err)
	}
	if sums[0].SecretGapCount != len(SecretGaps()) {
		t.Errorf("the listing reports %d gaps, want %d", sums[0].SecretGapCount, len(SecretGaps()))
	}
	_ = info
}

func TestCommitStampsCreatedUnixWhenAbsent(t *testing.T) {
	store, _ := newTestStore(t, WithClock(fixedClock(time.Unix(1780000123, 0), 0)))
	gen, err := store.Begin("nightly", "gen-1")
	if err != nil {
		t.Fatal(err)
	}
	m := sampleGenerationManifest("nightly", "gen-1")
	m.CreatedUnix = 0
	if err := gen.Commit(t.Context(), m); err != nil {
		t.Fatal(err)
	}
	loaded, _, err := store.Load("nightly", "gen-1")
	if err != nil {
		t.Fatal(err)
	}
	if loaded.CreatedUnix != 1780000123 {
		t.Errorf("CreatedUnix = %d, want the store clock's 1780000123", loaded.CreatedUnix)
	}
}

func TestListSummaryReproducesOnlyWhatItObserved(t *testing.T) {
	store, _ := newTestStore(t)
	gen, err := store.Begin("nightly", "gen-1")
	if err != nil {
		t.Fatal(err)
	}
	a, err := gen.WriteBytes(t.Context(), ArtifactSpec{
		ID:      "one",
		Kind:    KindDataset,
		Dataset: &DatasetArtifact{Dataset: "tank/one", Snapshot: "s"},
	}, []byte("payload"))
	if err != nil {
		t.Fatal(err)
	}
	m := sampleGenerationManifest("nightly", "gen-1")
	m.Artifacts = []Artifact{a}
	m.CombID, m.ColonyID, m.AppliedIndex = "comb-a", "colony-1", 4711
	if err := gen.Commit(t.Context(), m); err != nil {
		t.Fatal(err)
	}
	sums, err := store.List("nightly")
	if err != nil {
		t.Fatal(err)
	}
	s := sums[0]
	if s.ArtifactCount != 1 {
		t.Errorf("ArtifactCount = %d, want 1", s.ArtifactCount)
	}
	if s.PayloadBytes != a.SizeBytes {
		t.Errorf("PayloadBytes = %d, want %d", s.PayloadBytes, a.SizeBytes)
	}
	if s.CombID != "comb-a" || s.AppliedIndex != 4711 {
		t.Errorf("provenance was not reproduced: %+v", s)
	}
	if s.EarliestSnapshot != m.EarliestSnapshotUnix || s.LatestSnapshot != m.LatestSnapshotUnix {
		t.Errorf("the capture window was not reproduced: [%d, %d]", s.EarliestSnapshot, s.LatestSnapshot)
	}
	if s.Consistency != ConsistencySnapshot {
		t.Errorf("Consistency = %q, want the manifest's own label - a listing must not summarise a snapshot generation in words that could be read as quiesced", s.Consistency)
	}
}

func TestListReportsAFileWhereAGenerationWasExpected(t *testing.T) {
	// Something wrote into the policy directory that this package did
	// not. Skipping it silently would be exactly the "no silent drops"
	// failure; reporting it as incomplete says what was found.
	store, root := newTestStore(t)
	stray := joinPath(root, "nightly")
	if err := (OSFS{}).MkdirAll(stray, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := (OSFS{}).WriteFile(joinPath(stray, "stray"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	sums, err := store.List("nightly")
	if err != nil {
		t.Fatal(err)
	}
	if len(sums) != 1 {
		t.Fatalf("listed %d entries, want 1", len(sums))
	}
	if sums[0].State != GenerationIncomplete || sums[0].IsBackup() {
		t.Errorf("a stray file is %q, want incomplete", sums[0].State)
	}
	if !containsAll(sums[0].Reason, "a file where a generation directory was expected", "not a backup") {
		t.Errorf("reason = %s", sums[0].Reason)
	}
	// And a sweep leaves it alone: removing a file this package did not
	// create is not its call to make.
	res, err := store.Sweep("nightly")
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Removed) != 0 {
		t.Errorf("swept %v, want nothing", res.Removed)
	}
	if len(res.Refused) != 1 || !containsAll(res.Refused[0].Reason, "not its call to make") {
		t.Errorf("refusals = %+v", res.Refused)
	}
}

func TestLoadReportsAMissingManifestDistinctly(t *testing.T) {
	store, _ := newTestStore(t)
	gen, err := store.Begin("nightly", "gen-1")
	if err != nil {
		t.Fatal(err)
	}
	_, info, err := store.Load("nightly", "gen-1")
	if err == nil {
		t.Fatal("loading a generation with no manifest succeeded")
	}
	if info.State != GenerationIncomplete {
		t.Errorf("state = %q, want incomplete", info.State)
	}
	if !containsAll(info.Reason, "no manifest.pb", "so there is no backup here") {
		t.Errorf("reason = %s", info.Reason)
	}
	_ = gen
}

// TestReadManifestRejectsTrailingGarbage: two JSON documents in one
// manifest file is corruption worth naming, not a document to half-read.
func TestReadManifestRejectsTrailingGarbage(t *testing.T) {
	m := sampleManifest()
	if err := m.Validate(); err != nil {
		t.Fatal(err)
	}
	body, err := EncodeManifest(JSONCodec{}, &m)
	if err != nil {
		t.Fatal(err)
	}
	_, info, err := ReadManifest(JSONCodec{}, "manifest.pb", append(body, []byte(`{"format_version":1}`)...))
	if err == nil {
		t.Fatal("a manifest file containing two documents was accepted")
	}
	if info.State != GenerationUnreadable {
		t.Errorf("state = %q", info.State)
	}
}

func TestReadManifestRejectsAnUnparseableDocument(t *testing.T) {
	for _, body := range [][]byte{nil, []byte(""), []byte("not json"), {0x00, 0x01, 0x02}} {
		_, info, err := ReadManifest(JSONCodec{}, "manifest.pb", body)
		if err == nil {
			t.Errorf("an unparseable document %q was accepted", body)
		}
		if info.State != GenerationUnreadable {
			t.Errorf("state = %q for %q, want unreadable", info.State, body)
		}
		if !containsAll(info.Reason, "did not parse") {
			t.Errorf("reason = %q", info.Reason)
		}
	}
}

func TestReadManifestRejectsAMalformedChecksumField(t *testing.T) {
	m := sampleManifest()
	if err := m.Validate(); err != nil {
		t.Fatal(err)
	}
	body, err := EncodeManifest(JSONCodec{}, &m)
	if err != nil {
		t.Fatal(err)
	}
	// Replace the digest with something that is not a SHA-256, keeping
	// the document parseable. Located by pattern rather than by the
	// fixture's own (empty) Checksum field, because the digest is only
	// computed during encoding.
	loc := checksumField.FindStringSubmatchIndex(string(body))
	if loc == nil {
		t.Skip("could not locate the checksum field in the encoded manifest")
	}
	broken := checksumField.ReplaceAllString(string(body), `"checksum":"not-a-digest"`)
	_, info, err := ReadManifest(JSONCodec{}, "manifest.pb", []byte(broken))
	if err == nil {
		t.Fatal("a manifest with a malformed checksum field was accepted")
	}
	if !containsAll(info.Reason, "not a SHA-256 hex digest", "damaged") {
		t.Errorf("reason = %q", info.Reason)
	}
}

func TestReadManifestRejectsAManifestWithNoChecksumField(t *testing.T) {
	m := sampleManifest()
	if err := m.Validate(); err != nil {
		t.Fatal(err)
	}
	body, err := EncodeManifest(JSONCodec{}, &m)
	if err != nil {
		t.Fatal(err)
	}
	// Strip the field entirely, which is what a truncation at a field
	// boundary looks like.
	stripped := strings.Replace(string(body), checksumField.FindString(string(body))+",", "", 1)
	if stripped == string(body) {
		t.Skip("could not locate the checksum field to strip")
	}
	_, info, err := ReadManifest(JSONCodec{}, "manifest.pb", []byte(stripped))
	if err == nil {
		t.Fatal("a manifest with no checksum was accepted")
	}
	if !containsAll(info.Reason, "carries no checksum") {
		t.Errorf("reason = %q", info.Reason)
	}
}

func TestCodecNameIsReportedOnAParseFailure(t *testing.T) {
	_, info, err := ReadManifest(JSONCodec{}, "manifest.pb", []byte("{{{"))
	if err == nil {
		t.Fatal("expected a parse failure")
	}
	if !containsAll(info.Reason, "as a json manifest") {
		t.Errorf("the reason does not say how the file was being read: %q", info.Reason)
	}
}

func TestValidateRejectsAnEmptySecretGapKind(t *testing.T) {
	m := sampleManifest()
	m.SecretGaps = []SecretGap{{Why: "something"}}
	if err := m.Validate(); err == nil {
		t.Fatal("a secret gap with no kind was accepted - it would be indistinguishable from any other gap")
	}
}

func TestWithOptionsAreAppliedAndZeroValuesIgnored(t *testing.T) {
	// A nil option must not blank out a working store: passing
	// WithFS(nil) is a wiring mistake, not an instruction to have no
	// filesystem.
	root := t.TempDir()
	s := NewStore(root, WithFS(nil), WithCodec(nil), WithClock(nil))
	if s.fs == nil || s.codec == nil || s.now == nil {
		t.Fatal("a nil option blanked out a working store")
	}
	if _, err := s.Begin("nightly", "gen-1"); err != nil {
		t.Fatalf("the store does not work after nil options: %v", err)
	}
}

func TestOSFSSyncDirWorksOnThisPlatform(t *testing.T) {
	// atomicWriteFile treats a directory-fsync failure as a failed write,
	// so a platform where it does not work would break every commit.
	// This asserts it works here rather than finding out mid-backup.
	dir := t.TempDir()
	if err := (OSFS{}).SyncDir(dir); err != nil {
		t.Fatalf("directory fsync is not supported here, so every manifest commit would fail: %v", err)
	}
}

func TestValidIDRejectsPaths(t *testing.T) {
	for _, tc := range []struct{ what, id string }{
		{"a", ""}, {"a", "."}, {"a", ".."}, {"a", "a/b"}, {"a", `a\b`},
		{"a", "a b"}, {"a", "a\nb"}, {"a", strings.Repeat("x", 129)},
	} {
		if err := validID(tc.what, tc.id); err == nil {
			t.Errorf("validID accepted %q", tc.id)
		}
	}
	for _, id := range []string{"nightly", "gen-000042", "a.b", "a-b_c", strings.Repeat("x", 128)} {
		if err := validID("id", id); err != nil {
			t.Errorf("validID rejected the usable id %q: %v", id, err)
		}
	}
}

func TestErrorsCarryTheirSpecificReason(t *testing.T) {
	// Every sentinel message is asserted to be a complete sentence an
	// operator could act on. A message that says only "invalid" sends
	// someone looking in the wrong place, which is the whole reason each
	// of these is a distinct wrapped error rather than one umbrella.
	cases := []struct {
		err  error
		want string
	}{
		{ErrArtifactMissing, "missing or unreadable"},
		{ErrChecksumMismatch, "checksum does not match"},
		{ErrSecretExposed, "must never reach a manifest"},
		{ErrConsistencyNotQuiesced, "could not observe the Cell stopped"},
		{ErrSkewExceeded, "skew exceeds"},
		{ErrManifestShadowed, "shadow the manifest"},
		{ErrGenerationCommitted, "already committed"},
		{ErrArtifactSetMismatch, "does not match what was written"},
		{ErrInvalidManifest, "not internally consistent"},
		{ErrUnknownFormatVersion, "not understood"},
		{ErrMissingFormatVersion, "carries no format_version"},
	}
	for _, tc := range cases {
		if !strings.Contains(tc.err.Error(), tc.want) {
			t.Errorf("message %q does not contain %q", tc.err.Error(), tc.want)
		}
	}
}

func TestVersionErrorMessageStatesBothVersionsAndTheRefusal(t *testing.T) {
	ve := &VersionError{Found: 4, Supported: 1}
	msg := ve.Error()
	if !containsAll(msg, "format_version 4", "exactly 1", "rejected rather than interpreted", "newer build") {
		t.Errorf("the version refusal is not complete: %s", msg)
	}
	if !errors.Is(ve, ErrUnknownFormatVersion) {
		t.Error("a VersionError does not match ErrUnknownFormatVersion")
	}
}
