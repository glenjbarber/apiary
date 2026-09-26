package backup

import (
	"errors"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

// sampleManifest returns a manifest with every field populated, which is
// what the round-trip and byte-stability tests need: a fixture with one
// field left zero proves nothing about that field's encoding, and a
// round-trip that only exercises half the schema is a round trip that
// will not notice half the schema breaking.
func sampleManifest() Manifest {
	return Manifest{
		FormatVersion:        FormatVersion,
		CreatedUnix:          1780000000,
		CombID:               "comb-a",
		ColonyID:             "colony-1",
		AppliedIndex:         4711,
		PolicyID:             "nightly",
		GenerationID:         "gen-000042",
		Consistency:          ConsistencySnapshot,
		Quiesce:              nil,
		EarliestSnapshotUnix: 1780000000,
		LatestSnapshotUnix:   1780000037,
		MaxSkewSeconds:       120,
		Artifacts: []Artifact{
			{
				ArtifactID:   "root-dataset",
				Kind:         KindDataset,
				RelativePath: "artifacts/root-dataset.stream",
				SHA256:       strings.Repeat("ab", 32),
				SizeBytes:    4096,
				Dataset: &DatasetArtifact{
					Dataset:           "tank/root",
					Snapshot:          "apiary-backup-1",
					SourceGUID:        "1234567890",
					SnapshotTakenUnix: 1780000000,
				},
			},
			{
				ArtifactID:   "cell-config",
				Kind:         KindConfig,
				RelativePath: "artifacts/cell-config.json",
				SHA256:       strings.Repeat("cd", 32),
				SizeBytes:    128,
				Config: &ConfigArtifact{
					CellKind:       "jail",
					CellID:         "web-01",
					Definition:     []byte{0x0a, 0x12, 0x04},
					RenderedConfig: []byte{0x08, 0x01},
				},
			},
			{
				ArtifactID:   "installer",
				Kind:         KindImage,
				RelativePath: "artifacts/installer.bin",
				SHA256:       strings.Repeat("ef", 32),
				SizeBytes:    99,
				Image: &ImageArtifact{
					Name:              "installer.iso",
					Kind:              "iso",
					ExpectedSHA256Hex: strings.Repeat("12", 32),
					ObservedSHA256Hex: strings.Repeat("12", 32),
					SizeBytes:         99,
				},
			},
			{
				ArtifactID: "base-image",
				Kind:       KindReference,
				Reference: &ReferenceArtifact{
					Name:              "FreeBSD-15.1-RELEASE-amd64.raw.xz",
					Kind:              "base_image",
					ExpectedSHA256Hex: strings.Repeat("34", 32),
				},
			},
		},
		SecretGaps: SecretGaps(),
	}
}

func TestManifestValidateAcceptsFullyPopulatedFixture(t *testing.T) {
	m := sampleManifest()
	if err := m.Validate(); err != nil {
		t.Fatalf("Validate rejected a fully populated manifest: %v", err)
	}
}

func TestCodecRoundTripIsByteStable(t *testing.T) {
	// Encode -> decode -> re-encode must be byte-identical, twice over.
	// This is a correctness requirement rather than a tidiness one: the
	// self-checksum is computed over exactly these bytes, so a
	// non-deterministic encoding would produce a manifest this package
	// itself could not re-verify.
	m := sampleManifest()
	// Validate first, because Validate normalises: it sorts the artifact
	// set and canonicalises digests. Every real writer calls it (Commit
	// refuses to write without it), so the invariant under test is
	// "once normalised, the bytes are fixed" - not "a caller who skipped
	// validation gets the same bytes back".
	if err := m.Validate(); err != nil {
		t.Fatal(err)
	}
	first, err := EncodeManifest(JSONCodec{}, &m)
	if err != nil {
		t.Fatal(err)
	}
	got, info, err := ReadManifest(JSONCodec{}, "manifest.pb", first)
	if err != nil {
		t.Fatalf("ReadManifest: %v (state %s: %s)", err, info.State, info.Reason)
	}
	if !info.Understood {
		t.Fatalf("manifest was not understood: %+v", info)
	}
	second, err := EncodeManifest(JSONCodec{}, got)
	if err != nil {
		t.Fatal(err)
	}
	if string(first) != string(second) {
		t.Fatalf("re-encoding is not byte-stable.\n first: %s\nsecond: %s", first, second)
	}
	third, err := EncodeManifest(JSONCodec{}, got)
	if err != nil {
		t.Fatal(err)
	}
	if string(second) != string(third) {
		t.Fatalf("a third encoding differed from the second")
	}
}

func TestReadManifestVerifiesItsOwnChecksum(t *testing.T) {
	m := sampleManifest()
	body, err := EncodeManifest(JSONCodec{}, &m)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), `"checksum"`) {
		t.Fatalf("the encoded manifest has no checksum field, so a reader could not tell a complete document from a truncated one: %s", body)
	}
}

func TestReadManifestRejectsASingleFlippedByte(t *testing.T) {
	m := sampleManifest()
	body, err := EncodeManifest(JSONCodec{}, &m)
	if err != nil {
		t.Fatal(err)
	}
	// Flip one byte inside a field value, not the JSON syntax, so the
	// document still parses and the checksum is the only thing that can
	// catch it. A syntax error would prove nothing about integrity.
	corrupted := []byte(strings.Replace(string(body), `"comb-a"`, `"comb-b"`, 1))
	if string(corrupted) == string(body) {
		t.Fatal("the fixture does not contain the field the test corrupts")
	}
	_, info, err := ReadManifest(JSONCodec{}, "manifest.pb", corrupted)
	if err == nil {
		t.Fatal("a manifest with a flipped byte was accepted")
	}
	if !errIs(t, err, ErrChecksumMismatch) {
		t.Fatalf("error is %v, want ErrChecksumMismatch", err)
	}
	if info.State != GenerationUnreadable {
		t.Errorf("state is %q, want %q - damaged bytes are a real problem with this copy, distinct from one that is merely from the future", info.State, GenerationUnreadable)
	}
	if !strings.Contains(info.Reason, "failed its own checksum") {
		t.Errorf("reason does not name the failed checksum: %s", info.Reason)
	}
}

// TestReadManifestRejectsUnknownFormatVersion is the core of requirement
// 1: a version this build does not understand is refused, explicitly,
// with both numbers reported, and never guessed at.
func TestReadManifestRejectsUnknownFormatVersion(t *testing.T) {
	for _, found := range []uint32{2, 3, 99, 4294967295} {
		t.Run("v"+strconv.FormatUint(uint64(found), 10), func(t *testing.T) {
			m := sampleManifest()
			m.FormatVersion = found
			// Re-stamp the checksum so the ONLY thing wrong with this
			// document is its version. Otherwise a test would pass
			// because the checksum failed first and would not have
			// proved anything about the version check.
			body, err := EncodeManifest(JSONCodec{}, &m)
			if err != nil {
				t.Fatal(err)
			}
			got, info, err := ReadManifest(JSONCodec{}, "manifest.pb", body)
			if err == nil {
				t.Fatalf("a v%d manifest was accepted by a v%d reader", found, FormatVersion)
			}
			if !errIs(t, err, ErrUnknownFormatVersion) {
				t.Fatalf("error is %v, want ErrUnknownFormatVersion", err)
			}
			var ve *VersionError
			if !errors.As(err, &ve) {
				t.Fatalf("error %v is not a *VersionError, so a caller cannot report the version it found", err)
			}
			if ve.Found != found || ve.Supported != FormatVersion {
				t.Errorf("VersionError = {Found:%d Supported:%d}, want {%d %d}", ve.Found, ve.Supported, found, FormatVersion)
			}
			if info.FormatVersion != found {
				t.Errorf("ReadInfo.FormatVersion = %d, want the observed %d - the version must be recorded verbatim even on the refusal path", info.FormatVersion, found)
			}
			if info.Understood {
				t.Error("ReadInfo.Understood is true for a manifest this build cannot read")
			}
			if info.State != GenerationUnknownVersion {
				t.Errorf("state is %q, want %q - a future archive is unknown, not corrupt and not failed", info.State, GenerationUnknownVersion)
			}
			// The message must not hedge. "probably compatible" is the
			// failure this whole rule exists to prevent.
			if !containsAll(err.Error(), itoa(int(found)), itoa(int(FormatVersion)), "rejected rather than interpreted") {
				t.Errorf("the refusal does not state both versions and that it refused to interpret: %v", err)
			}
			// The parsed-but-untrusted manifest is available for a
			// surface that wants to say "written by a newer Apiary".
			if got == nil {
				t.Fatal("no manifest was returned for a surface to report on")
			}
		})
	}
}

func TestReadManifestRejectsMissingFormatVersion(t *testing.T) {
	m := sampleManifest()
	m.FormatVersion = 0
	body, err := EncodeManifest(JSONCodec{}, &m)
	if err != nil {
		t.Fatal(err)
	}
	_, info, err := ReadManifest(JSONCodec{}, "manifest.pb", body)
	if err == nil {
		t.Fatal("a manifest with no format_version was accepted")
	}
	if !errIs(t, err, ErrMissingFormatVersion) {
		t.Fatalf("error is %v, want ErrMissingFormatVersion - a zero version is a corrupt document, not a future one, and the two must be told apart", err)
	}
	// Deliberately NOT ErrUnknownFormatVersion: a zero version is a
	// corrupt document, and a future one is not. A caller that matched
	// on the broader sentinel would report "this archive is from a newer
	// Apiary" about a file with no version at all.
	if errIs(t, err, ErrUnknownFormatVersion) {
		t.Errorf("a manifest with no version was reported as an unknown version, which tells an operator the wrong thing about the file")
	}
	if info.State != GenerationUnreadable {
		t.Errorf("state = %q, want %q", info.State, GenerationUnreadable)
	}
}

// TestUnknownVersionForwardCompatibility checks the other half of the
// version rule: a newer document still PARSES, so a surface can report
// what it is, and only the decision to act on it is refused.
func TestUnknownVersionForwardCompatibility(t *testing.T) {
	m := sampleManifest()
	m.FormatVersion = 2
	m.SecretGaps = SecretGaps()
	body, err := EncodeManifest(JSONCodec{}, &m)
	if err != nil {
		t.Fatal(err)
	}
	// Hand-inject a field this build has never heard of, exactly as a
	// future writer would.
	withUnknown := strings.Replace(string(body), `{"format_version":2`,
		`{"future_only_field":{"nested":"value"},"format_version":2`, 1)
	got, info, err := ReadManifest(JSONCodec{}, "manifest.pb", []byte(withUnknown))
	if err == nil {
		t.Fatal("a v2 manifest was accepted")
	}
	if got == nil {
		t.Fatal("a v2 manifest did not parse, so a surface cannot report what it found - unknown fields must be ignored, not fatal")
	}
	// The fields this build does recognise were still read, which is what
	// lets a listing show the artifact count of an archive it may not
	// restore from.
	if got.GenerationID != "gen-000042" {
		t.Errorf("GenerationID = %q, want the recognised fields to be readable anyway", got.GenerationID)
	}
	if info.FormatVersion != 2 {
		t.Errorf("FormatVersion = %d, want 2", info.FormatVersion)
	}
}

// TestRestoreIsRefusedForAnUnknownVersion is where the version rule earns
// its keep: reading is allowed, acting is not.
func TestRestoreIsRefusedForAnUnknownVersion(t *testing.T) {
	dir := t.TempDir()
	m := sampleManifest()
	m.FormatVersion = 7
	body, err := EncodeManifest(JSONCodec{}, &m)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeFileOS(joinPath(dir, ManifestName), body); err != nil {
		t.Fatal(err)
	}
	got, info, err := ReadManifest(JSONCodec{}, joinPath(dir, ManifestName), body)
	if err == nil {
		t.Fatal("expected an unknown-version refusal")
	}
	if got != nil && info.Understood {
		t.Fatal("a manifest this build cannot read was reported as understood")
	}
	// PlanRestore's first guard is the manifest, and it must fire before
	// any artifact is hashed.
	store := NewStore(dir)
	plan, _ := store.PlanRestore(t.Context(), ".", "x", RestoreOutOfBand, VerifyOptions{}, RestoreOptions{DestDataset: "tank/restored"})
	if plan.Eligible {
		t.Fatal("a generation with an unreadable manifest was found eligible for restore")
	}
	if plan.Guard != "manifest" {
		t.Errorf("guard = %q, want %q", plan.Guard, "manifest")
	}
}

func TestUnknownVersionIsNotReportedAsCorruption(t *testing.T) {
	m := sampleManifest()
	m.FormatVersion = 5
	body, _ := EncodeManifest(JSONCodec{}, &m)
	_, info, _ := ReadManifest(JSONCodec{}, "manifest.pb", body)
	if info.ChecksumVerified {
		t.Error("ChecksumVerified is true, but the version check runs first and a reader that does not understand the format cannot judge its checksum")
	}
	if info.State == GenerationUnreadable {
		t.Errorf("state = %q; a future archive on a target is a normal thing to find and must not read as a damaged one", info.State)
	}
}

func TestValidateRefusesArtifactPathsThatEscape(t *testing.T) {
	cases := []struct {
		name string
		rel  string
	}{
		{"absolute", "/etc/shadow"},
		{"parent traversal", "artifacts/../../etc/shadow"},
		{"outside artifacts", "elsewhere/payload.bin"},
		{"backslash", `artifacts\payload.bin`},
		{"non-canonical", "artifacts/./payload.bin"},
		{"shadowing the manifest", "artifacts/" + ManifestName},
		{"dotfile", "artifacts/.hidden"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := sampleManifest()
			m.Artifacts[0].RelativePath = tc.rel
			err := m.Validate()
			if err == nil {
				t.Fatalf("a manifest with relative_path %q was accepted", tc.rel)
			}
			if !errIs(t, err, ErrInvalidManifest) && !errIs(t, err, ErrManifestShadowed) {
				t.Errorf("error is %v, want ErrInvalidManifest or ErrManifestShadowed", err)
			}
		})
	}
}

func TestValidateRefusesUnusableArtifactIDs(t *testing.T) {
	cases := []struct {
		name string
		id   string
	}{
		{"separator", "a/b"},
		{"backslash", `a\b`},
		{"dot only", "."},
		{"dotdot", ".."},
		{"leading dot", ".hidden"},
		{"space", "a b"},
		{"empty", ""},
		{"nul-ish", "a\x00b"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := sampleManifest()
			m.Artifacts[0].ArtifactID = tc.id
			m.Artifacts[0].RelativePath = "artifacts/x.stream"
			if err := m.Validate(); err == nil {
				t.Fatalf("artifact id %q was accepted", tc.id)
			}
		})
	}
}

func TestValidateRefusesDuplicateArtifactIDs(t *testing.T) {
	m := sampleManifest()
	m.Artifacts[1] = m.Artifacts[0]
	if err := m.Validate(); err == nil || !containsAll(err.Error(), "more than once") {
		t.Fatalf("duplicate artifact ids were accepted: %v", err)
	}
}

func TestValidateRefusesAReferenceWithAPayload(t *testing.T) {
	// A "reference" with a file behind it is a reference nobody
	// re-fetched and re-verified, so the manifest must not be able to
	// describe one.
	for _, tc := range []struct {
		name  string
		apply func(a *Artifact)
	}{
		{"with a path", func(a *Artifact) { a.RelativePath = "artifacts/base.bin" }},
		{"with a digest", func(a *Artifact) { a.SHA256 = strings.Repeat("aa", 32) }},
		{"with a size", func(a *Artifact) { a.SizeBytes = 42 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := sampleManifest()
			tc.apply(&m.Artifacts[3])
			if err := m.Validate(); err == nil {
				t.Fatalf("a reference %s was accepted", tc.name)
			}
		})
	}
}

func TestValidateRefusesAPayloadArtifactWithNoDigest(t *testing.T) {
	m := sampleManifest()
	m.Artifacts[0].SHA256 = ""
	err := m.Validate()
	if err == nil {
		t.Fatal("a payload artifact with no digest was accepted - it could never be verified")
	}
	if !containsAll(err.Error(), "could never be verified") {
		t.Errorf("the refusal does not say why: %v", err)
	}
}

func TestValidateRefusesAMalformedDigest(t *testing.T) {
	for _, bad := range []string{"abc", strings.Repeat("z", 64), strings.Repeat("ab", 31), ""} {
		m := sampleManifest()
		m.Artifacts[0].SHA256 = bad
		if bad == "" {
			// Covered by its own test; here it would fail for a
			// different reason, so skip it to keep the table honest.
			continue
		}
		if err := m.Validate(); err == nil {
			t.Errorf("digest %q was accepted", bad)
		}
	}
}

func TestValidateRequiresExactlyThePayloadItsKindCallsFor(t *testing.T) {
	m := sampleManifest()
	// A dataset artifact carrying an image payload is a manifest that
	// disagrees with itself, and the disagreement is what a reader acts
	// on.
	m.Artifacts[0].Image = &ImageArtifact{Name: "x"}
	if err := m.Validate(); err == nil || !containsAll(err.Error(), "must not carry") {
		t.Fatalf("a dataset artifact with an image payload was accepted: %v", err)
	}

	m = sampleManifest()
	m.Artifacts[0].Dataset = nil
	if err := m.Validate(); err == nil || !containsAll(err.Error(), "requires its") {
		t.Fatalf("a dataset artifact with no dataset payload was accepted: %v", err)
	}
}

func TestValidateRefusesAZeroLengthDigestLookalike(t *testing.T) {
	// A digest of the wrong length is worse than none at all, because it
	// looks verified.
	m := sampleManifest()
	m.Artifacts[1].SHA256 = "deadbeef"
	err := m.Validate()
	if err == nil || !containsAll(err.Error(), "64 hex characters") {
		t.Fatalf("a short digest was accepted: %v", err)
	}
}

func TestValidateRefusesANodeConfigWithATypedPayload(t *testing.T) {
	m := sampleManifest()
	m.Artifacts = append(m.Artifacts, Artifact{
		ArtifactID:   "node-config",
		Kind:         KindNodeConfig,
		RelativePath: "artifacts/node-config.json",
		SHA256:       strings.Repeat("aa", 32),
		SizeBytes:    10,
		Image:        &ImageArtifact{Name: "not-an-image"},
	})
	if err := m.Validate(); err == nil {
		t.Fatal("a node config artifact carrying a typed payload was accepted")
	}
}

func TestKnownKindsMatchesTheSchema(t *testing.T) {
	// Every kind this package names must have a filename extension
	// decision, because a kind with no entry there would write a payload
	// under a path with no suffix and a reference under one too.
	for _, k := range []ArtifactKind{KindDataset, KindConfig, KindNodeConfig, KindImage, KindReference} {
		if _, ok := extensionByKind[k]; !ok {
			t.Errorf("kind %q has no extension entry", k)
		}
		if _, ok := payloadRules[k]; !ok {
			t.Errorf("kind %q has no payload rule, so its typed payload shape is unstated", k)
		}
	}
	if len(knownKinds()) != len(extensionByKind) {
		t.Errorf("knownKinds() returns %d kinds but there are %d", len(knownKinds()), len(extensionByKind))
	}
}

func TestManifestJSONFieldNamesMatchTheADRSketch(t *testing.T) {
	// The JSON tags are taken from ADR-0131's proto sketch so that
	// adding api/internalpb/backup.proto later is a field-for-field
	// mapping. Read off the struct tags rather than off encoded output:
	// every field here is omitempty, so a zero value would encode to {}
	// and the test would pass while checking nothing.
	want := map[string][]string{
		"Manifest": {"format_version", "created_unix", "comb_id", "colony_id", "applied_index",
			"policy_id", "generation_id", "consistency", "quiesce", "earliest_snapshot_unix",
			"latest_snapshot_unix", "max_skew_seconds", "artifacts", "secret_gaps", "checksum"},
		"Artifact": {"artifact_id", "kind", "relative_path", "sha256", "bytes",
			"dataset", "config", "image", "reference"},
		"DatasetArtifact":   {"dataset", "snapshot", "source_guid", "snapshot_taken_unix", "recursive", "parent_dataset"},
		"ConfigArtifact":    {"cell_kind", "cell_id", "definition", "rendered_config"},
		"ImageArtifact":     {"name", "kind", "expected_sha256_hex", "observed_sha256_hex", "bytes"},
		"ReferenceArtifact": {"name", "kind", "expected_sha256_hex"},
		"SecretGap":         {"kind", "why", "live_copy_location", "restore_procedure"},
		"Quiesce":           {"cell_id", "observed_at_unix", "method"},
	}
	types := map[string]any{
		"Manifest":          Manifest{},
		"Artifact":          Artifact{},
		"DatasetArtifact":   DatasetArtifact{},
		"ConfigArtifact":    ConfigArtifact{},
		"ImageArtifact":     ImageArtifact{},
		"ReferenceArtifact": ReferenceArtifact{},
		"SecretGap":         SecretGap{},
		"Quiesce":           Quiesce{},
	}
	for typeName, wantTags := range want {
		rt := reflect.TypeOf(types[typeName])
		got := map[string]struct{}{}
		for i := 0; i < rt.NumField(); i++ {
			tag := strings.Split(rt.Field(i).Tag.Get("json"), ",")[0]
			if tag == "" {
				t.Errorf("%s.%s has no json tag; every field in this schema is named for the future proto", typeName, rt.Field(i).Name)
				continue
			}
			got[tag] = struct{}{}
		}
		for _, tag := range wantTags {
			if _, ok := got[tag]; !ok {
				t.Errorf("%s has no %q field, so the mapping to backup.proto would need a rename", typeName, tag)
			}
		}
	}
}

// writeFileOS is a tiny helper so the tests above do not need os
// imported for a single call.
func writeFileOS(path string, body []byte) error {
	return OSFS{}.WriteFile(path, body, 0o600)
}

// itoa renders a version number for a table-driven test name and for
// error-message assertions.
func itoa(i int) string { return strconv.Itoa(i) }
