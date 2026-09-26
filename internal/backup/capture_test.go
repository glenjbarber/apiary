package backup

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"testing"
	"time"
)

// # Consistency labelling

func TestConsistencyValidHasNoDefault(t *testing.T) {
	// The zero value is not "snapshot". A default here would be exactly
	// the silent downgrade the ADR forbids: a producer that forgot to say
	// what guarantee it was producing would produce a manifest claiming
	// filesystem consistency, and nothing downstream could tell that from
	// a deliberate choice.
	if Consistency("").Valid() {
		t.Fatal("the empty consistency is valid; an unlabelled backup states no guarantee and must be refused")
	}
	for _, c := range []Consistency{ConsistencySnapshot, ConsistencyQuiesced} {
		if !c.Valid() {
			t.Errorf("%q is not valid", c)
		}
	}
	if Consistency("quiescent").Valid() || Consistency("QUIESCED").Valid() {
		t.Error("a near-miss label was accepted; the vocabulary is exact")
	}
}

func TestGuaranteesNeverBlursTheTwoLabels(t *testing.T) {
	if got := ConsistencySnapshot.Guarantees(); !containsAll(got, "filesystem consistent", "not application consistent") {
		t.Errorf("snapshot guarantees = %q, want the weaker guarantee stated explicitly", got)
	}
	if got := ConsistencyQuiesced.Guarantees(); !containsAll(got, "quiesced", "observed stopped") {
		t.Errorf("quiesced guarantees = %q", got)
	}
	if got := Consistency("").Guarantees(); !containsAll(got, "states no consistency guarantee", "must not be read as claiming one") {
		t.Errorf("an unlabelled consistency renders as %q, which is dangerously close to hedging", got)
	}
}

// TestClaimsIsAsymmetric: a snapshot generation never claims to be
// quiesced, in any code path, because it did not stop anything.
func TestClaimsIsAsymmetric(t *testing.T) {
	snap := sampleManifest()
	snap.Consistency = ConsistencySnapshot
	if snap.Claims(ConsistencyQuiesced) {
		t.Error("a snapshot generation claims quiescence")
	}
	if !snap.Claims(ConsistencySnapshot) {
		t.Error("a snapshot generation does not claim its own label")
	}

	quiesced := sampleManifest()
	quiesced.Consistency = ConsistencyQuiesced
	quiesced.Quiesce = &Quiesce{CellID: "web-01", ObservedAtUnix: 1780000000, Method: "GetJail reported not running"}
	if !quiesced.Claims(ConsistencyQuiesced) {
		t.Error("a quiesced generation with evidence does not claim quiescence")
	}
	// A quiesced generation does satisfy the weaker claim, because it is
	// a strictly stronger guarantee rather than a different one.
	if !quiesced.Claims(ConsistencySnapshot) {
		t.Error("a quiesced generation does not satisfy the weaker snapshot claim")
	}

	// A label with no evidence claims nothing, and Validate refuses to
	// let such a manifest exist at all.
	unbacked := sampleManifest()
	unbacked.Consistency = ConsistencyQuiesced
	unbacked.Quiesce = nil
	if unbacked.Claims(ConsistencyQuiesced) {
		t.Error("a quiesced label with no evidence claims quiescence")
	}
	if err := unbacked.Validate(); err == nil {
		t.Fatal("a quiesced manifest with no evidence was accepted")
	}
}

func TestValidateRefusesAnUnlabelledManifest(t *testing.T) {
	m := sampleManifest()
	m.Consistency = ""
	err := m.Validate()
	if err == nil {
		t.Fatal("a manifest with no consistency label was accepted")
	}
	if !errIs(t, err, ErrInvalidManifest) {
		t.Errorf("error is %v, want ErrInvalidManifest", err)
	}
	if !containsAll(err.Error(), "unlabelled backup states no guarantee", "refused rather than guessed at") {
		t.Errorf("the refusal does not explain the reasoning: %v", err)
	}
}

func TestValidateRequiresEveryPartOfQuiesceEvidence(t *testing.T) {
	base := func() Manifest {
		m := sampleManifest()
		m.Consistency = ConsistencyQuiesced
		m.Quiesce = &Quiesce{CellID: "web-01", ObservedAtUnix: 1780000000, Method: "GetJail reported not running"}
		return m
	}
	for _, tc := range []struct {
		name    string
		mutate  func(*Manifest)
		wantSub string
	}{
		{"no evidence at all", func(m *Manifest) { m.Quiesce = nil }, "carries no quiesce evidence"},
		{"no Cell", func(m *Manifest) { m.Quiesce.CellID = "" }, "names no Cell"},
		{"no time", func(m *Manifest) { m.Quiesce.ObservedAtUnix = 0 }, "no observation time"},
		{"no method", func(m *Manifest) { m.Quiesce.Method = "" }, "no observation method"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := base()
			tc.mutate(&m)
			err := m.Validate()
			if err == nil {
				t.Fatalf("a quiesced manifest with %s was accepted", tc.name)
			}
			if !containsAll(err.Error(), tc.wantSub) {
				t.Errorf("error %q does not say what is missing (%q)", err, tc.wantSub)
			}
		})
	}
}

func TestValidateEnforcesSkewOnlyForQuiescedGenerations(t *testing.T) {
	// A set of datasets snapshotted one after another is not atomic
	// across the set. For a snapshot generation that is what it is, and
	// enforcing a tight window on it would be theatre. For a quiesced one
	// it is the whole claim, and exceeding the limit means the artifacts
	// are not the single instant the manifest says they are.
	wide := int64(3600)

	snap := sampleManifest()
	snap.Consistency = ConsistencySnapshot
	snap.EarliestSnapshotUnix = 1780000000
	snap.LatestSnapshotUnix = 1780000000 + wide
	snap.MaxSkewSeconds = 60
	if err := snap.Validate(); err != nil {
		t.Errorf("a snapshot generation spanning %ds over a %ds limit was refused: %v", wide, snap.MaxSkewSeconds, err)
	}

	q := sampleManifest()
	q.Consistency = ConsistencyQuiesced
	q.Quiesce = &Quiesce{CellID: "web-01", ObservedAtUnix: 1780000000, Method: "GetJail"}
	q.EarliestSnapshotUnix = 1780000000
	q.LatestSnapshotUnix = 1780000000 + wide
	q.MaxSkewSeconds = 60
	err := q.Validate()
	if err == nil {
		t.Fatal("a quiesced generation spanning an hour over a 60s limit was accepted")
	}
	if !errIs(t, err, ErrSkewExceeded) {
		t.Errorf("error is %v, want ErrSkewExceeded", err)
	}
	if !containsAll(err.Error(), "not one instant") {
		t.Errorf("the refusal does not say what the skew means: %v", err)
	}
}

func TestValidateRefusesAnInvertedCaptureWindow(t *testing.T) {
	m := sampleManifest()
	m.EarliestSnapshotUnix = 1780000100
	m.LatestSnapshotUnix = 1780000000
	if err := m.Validate(); err == nil {
		t.Fatal("a manifest whose window ends before it starts was accepted")
	}
}

func TestValidateRequiresACaptureInstant(t *testing.T) {
	m := sampleManifest()
	m.EarliestSnapshotUnix = 0
	m.LatestSnapshotUnix = 0
	err := m.Validate()
	if err == nil {
		t.Fatal("a manifest that does not say when its data was captured was accepted")
	}
	if !containsAll(err.Error(), "does not say when its data was captured") {
		t.Errorf("error = %v", err)
	}
}

// # The no-downgrade rule

// TestAQuiescedCaptureIsNeverDowngradedToSnapshot is the single most
// important test in this file. A policy that says QUIESCED and a Cell
// that cannot be stopped must produce a failure, not a filesystem-
// consistent backup quietly labelled as what was asked for.
func TestAQuiescedCaptureIsNeverDowngradedToSnapshot(t *testing.T) {
	cases := []struct {
		name    string
		prepare func(*Store) *fakeQuiescer
		wantErr error
	}{
		{
			name: "the stop could not be observed",
			prepare: func(*Store) *fakeQuiescer {
				return &fakeQuiescer{evidence: StopEvidence{Observed: false, Method: "GetJail returned no error but the Cell was still listed as running"}}
			},
			wantErr: ErrConsistencyNotQuiesced,
		},
		{
			name: "the stop call failed outright",
			prepare: func(*Store) *fakeQuiescer {
				return &fakeQuiescer{err: errors.New("no quorum to change desired_state")}
			},
			wantErr: ErrConsistencyNotQuiesced,
		},
		{
			name: "the stop was observed with no method named",
			prepare: func(*Store) *fakeQuiescer {
				return &fakeQuiescer{evidence: StopEvidence{Observed: true}}
			},
			wantErr: ErrConsistencyNotQuiesced,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store, root := newTestStore(t)
			q := tc.prepare(store)
			store.SetQuiescer(q)

			_, err := store.Capture(t.Context(), Request{
				PolicyID: "nightly", GenerationID: "gen-1", CombID: "comb-a",
				Consistency: ConsistencyQuiesced, CellID: "web-01",
				Sources: []Source{
					&okSource{spec: ArtifactSpec{ID: "one", Kind: KindDataset, Dataset: &DatasetArtifact{Dataset: "tank/one"}}, bytes: []byte("payload")},
					&okSource{spec: ArtifactSpec{ID: "two", Kind: KindDataset, Dataset: &DatasetArtifact{Dataset: "tank/two"}}, bytes: []byte("payload")},
				},
			})
			if err == nil {
				t.Fatal("the capture succeeded despite not being able to observe the Cell stopped")
			}
			if !errIs(t, err, tc.wantErr) {
				t.Errorf("error is %v, want %v", err, tc.wantErr)
			}
			if !containsAll(err.Error(), "not downgraded") {
				t.Errorf("the refusal does not say that no downgrade happened: %v", err)
			}
			// The decisive assertion: nothing was published, anywhere.
			assertNoManifestAnywhere(t, root)
			// And the quiescer was asked before anything was captured, so
			// the failure cost no I/O at all.
			if len(q.calls) != 1 || q.calls[0] != "web-01" {
				t.Errorf("quiescer calls = %v, want exactly one call for web-01 before any capture", q.calls)
			}
		})
	}
}

func TestAQuiescedCaptureIsRefusedWithoutAQuiescer(t *testing.T) {
	// A Store with no Quiescer cannot stop anything, so it must not be
	// able to produce a quiesced backup. The refusal names the alternative
	// it is NOT taking, because silence there is how a downgrade happens
	// by default.
	store, root := newTestStore(t)
	_, err := store.Capture(t.Context(), Request{
		PolicyID: "nightly", GenerationID: "gen-1", CombID: "comb-a",
		Consistency: ConsistencyQuiesced, CellID: "web-01",
		Sources: []Source{&okSource{spec: ArtifactSpec{ID: "one", Kind: KindDataset, Dataset: &DatasetArtifact{Dataset: "tank/one"}}, bytes: []byte("x")}},
	})
	if err == nil {
		t.Fatal("a quiesced capture succeeded with no Quiescer configured")
	}
	if !errIs(t, err, ErrConsistencyNotQuiesced) {
		t.Errorf("error is %v, want ErrConsistencyNotQuiesced", err)
	}
	if !containsAll(err.Error(), "no Quiescer", "rather than downgraded") {
		t.Errorf("the refusal does not say what is missing and what it will not do instead: %v", err)
	}
	assertNoManifestAnywhere(t, root)
}

func TestAQuiescedCaptureCarriesItsEvidence(t *testing.T) {
	store, _ := newTestStore(t)
	at := time.Unix(1780000000, 0)
	store.SetQuiescer(observedQuiescer(at))
	store.SetSnapshotTaker(&fakeSnapshots{start: at, step: time.Second})

	res, err := store.Capture(t.Context(), Request{
		PolicyID: "nightly", GenerationID: "gen-1", CombID: "comb-a", ColonyID: "colony-1",
		Consistency: ConsistencyQuiesced, CellID: "web-01", MaxSkewSeconds: 60,
		Sources: []Source{
			&okSource{spec: ArtifactSpec{ID: "one", Kind: KindDataset, Dataset: &DatasetArtifact{Dataset: "tank/one", SourceGUID: "123"}}, bytes: []byte("one")},
			&okSource{spec: ArtifactSpec{ID: "two", Kind: KindDataset, Dataset: &DatasetArtifact{Dataset: "tank/two", SourceGUID: "456"}}, bytes: []byte("two")},
		},
	})
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}
	m := res.Manifest
	if m.Consistency != ConsistencyQuiesced {
		t.Fatalf("consistency = %q", m.Consistency)
	}
	if m.Quiesce == nil {
		t.Fatal("a quiesced generation carries no evidence")
	}
	if m.Quiesce.CellID != "web-01" || m.Quiesce.Method == "" {
		t.Errorf("evidence = %+v, want the Cell and the observation method", m.Quiesce)
	}
	if m.Quiesce.ObservedAtUnix != unixSeconds(at) {
		t.Errorf("observation time = %d, want the Quiescer's own %d rather than a substituted clock reading", m.Quiesce.ObservedAtUnix, unixSeconds(at))
	}
	if !m.Claims(ConsistencyQuiesced) {
		t.Error("a genuinely quiesced generation does not claim quiescence")
	}
	// The window comes from the snapshot taker, not the wall clock, and
	// it is inside the policy's limit.
	if m.EarliestSnapshotUnix != unixSeconds(at) || m.LatestSnapshotUnix != unixSeconds(at.Add(time.Second)) {
		t.Errorf("capture window is [%d, %d], want the taker's own [%d, %d]",
			m.EarliestSnapshotUnix, m.LatestSnapshotUnix, unixSeconds(at), unixSeconds(at.Add(time.Second)))
	}
	// The snapshots the taker created are recorded per artifact, so a
	// reader can see the spread and not only its endpoints.
	for _, a := range m.Artifacts {
		if a.Dataset == nil || a.Dataset.SnapshotTakenUnix == 0 {
			t.Errorf("artifact %s records no snapshot time", a.ArtifactID)
		}
		if a.Dataset.Snapshot == "" {
			t.Errorf("artifact %s records no snapshot name", a.ArtifactID)
		}
	}
	// The source GUIDs the caller supplied survive.
	if m.Artifacts[0].Dataset.SourceGUID != "123" {
		t.Errorf("source_guid = %q, want the caller's value", m.Artifacts[0].Dataset.SourceGUID)
	}
}

func TestAQuiescedCaptureOverTheSkewLimitFailsWithoutPublishing(t *testing.T) {
	store, root := newTestStore(t)
	at := time.Unix(1780000000, 0)
	store.SetQuiescer(observedQuiescer(at))
	// The taker hands out snapshots a full hour apart, so the window is an
	// hour wide against a 60-second policy.
	store.SetSnapshotTaker(&fakeSnapshots{start: at, step: time.Hour})

	_, err := store.Capture(t.Context(), Request{
		PolicyID: "nightly", GenerationID: "gen-1", CombID: "comb-a",
		Consistency: ConsistencyQuiesced, CellID: "web-01", MaxSkewSeconds: 60,
		Sources: []Source{
			&okSource{spec: ArtifactSpec{ID: "one", Kind: KindDataset, Dataset: &DatasetArtifact{Dataset: "tank/one"}}, bytes: []byte("one")},
			&okSource{spec: ArtifactSpec{ID: "two", Kind: KindDataset, Dataset: &DatasetArtifact{Dataset: "tank/two"}}, bytes: []byte("two")},
		},
	})
	if err == nil {
		t.Fatal("a quiesced capture spanning an hour over a 60s limit was accepted")
	}
	if !errIs(t, err, ErrSkewExceeded) {
		t.Errorf("error is %v, want ErrSkewExceeded", err)
	}
	assertNoManifestAnywhere(t, root)
}

// TestASnapshotCaptureOverAnySkewIsFine: a snapshot generation never
// claimed a tight window, so refusing a wide one would be theatre.
func TestASnapshotCaptureOverAnySkewIsFine(t *testing.T) {
	store, _ := newTestStore(t)
	at := time.Unix(1780000000, 0)
	store.SetSnapshotTaker(&fakeSnapshots{start: at, step: time.Hour})
	res, err := store.Capture(t.Context(), Request{
		PolicyID: "nightly", GenerationID: "gen-1", CombID: "comb-a",
		Consistency: ConsistencySnapshot, MaxSkewSeconds: 1,
		Sources: []Source{
			&okSource{spec: ArtifactSpec{ID: "one", Kind: KindDataset, Dataset: &DatasetArtifact{Dataset: "tank/one"}}, bytes: []byte("one")},
			&okSource{spec: ArtifactSpec{ID: "two", Kind: KindDataset, Dataset: &DatasetArtifact{Dataset: "tank/two"}}, bytes: []byte("two")},
		},
	})
	if err != nil {
		t.Fatalf("a snapshot generation spanning an hour was refused: %v", err)
	}
	if res.Manifest.LatestSnapshotUnix-res.Manifest.EarliestSnapshotUnix != 3600 {
		t.Errorf("window = %ds, want the hour the taker actually spanned", res.Manifest.LatestSnapshotUnix-res.Manifest.EarliestSnapshotUnix)
	}
	if res.Manifest.Claims(ConsistencyQuiesced) {
		t.Error("a snapshot capture claims quiescence")
	}
	if res.Manifest.Quiesce != nil {
		t.Errorf("a snapshot capture carries quiesce evidence: %+v", res.Manifest.Quiesce)
	}
}

// # All-or-nothing

// TestAFailedArtifactDiscardsTheWholeGeneration is the all-or-nothing
// rule: there is no best-effort backup, because a backup that silently
// contains four of five datasets will be believed.
func TestAFailedArtifactDiscardsTheWholeGeneration(t *testing.T) {
	store, root := newTestStore(t)
	src := &failingSource{
		spec:    ArtifactSpec{ID: "three", Kind: KindDataset, Dataset: &DatasetArtifact{Dataset: "tank/three"}},
		prefix:  []byte("a prefix and then a failure"),
		failErr: errors.New("the stream died"),
	}
	first := &okSource{spec: ArtifactSpec{ID: "one", Kind: KindDataset, Dataset: &DatasetArtifact{Dataset: "tank/one"}}, bytes: []byte("one")}
	second := &okSource{spec: ArtifactSpec{ID: "two", Kind: KindDataset, Dataset: &DatasetArtifact{Dataset: "tank/two"}}, bytes: []byte("two")}

	res, err := store.Capture(t.Context(), Request{
		PolicyID: "nightly", GenerationID: "gen-1", CombID: "comb-a",
		Consistency: ConsistencySnapshot,
		Sources:     []Source{first, second, src, &okSource{spec: ArtifactSpec{ID: "four", Kind: KindDataset, Dataset: &DatasetArtifact{Dataset: "tank/four"}}, bytes: []byte("four")}},
	})
	if err == nil {
		t.Fatal("the capture reported success despite a source failure")
	}
	if !containsAll(err.Error(), "three", "stream died") {
		t.Errorf("the failure does not name the artifact and the cause: %v", err)
	}
	// The decisive assertion.
	assertNoManifestAnywhere(t, root)
	// The partial generation is gone, and the caller is told so rather
	// than having to discover it.
	if !res.Aborted {
		t.Errorf("Result.Aborted is false; FailedRemoval = %q", res.FailedRemoval)
	}
	if res.CapturedBefore != 2 {
		t.Errorf("CapturedBefore = %d, want 2 - a backup job that ran three of four sources should say so", res.CapturedBefore)
	}
	if res.ManifestPath != "" {
		t.Errorf("a failed capture reports a manifest path %q", res.ManifestPath)
	}
	// The fourth source was never touched: a failure at artifact three
	// stops the capture rather than continuing to spend I/O on artifacts
	// whose generation is already doomed.
	fourth := &okSource{spec: ArtifactSpec{ID: "four", Kind: KindDataset, Dataset: &DatasetArtifact{Dataset: "tank/four"}}, bytes: []byte("four")}
	_ = fourth
	// And the listing reports nothing, which is what an operator or a
	// sweeper will look at.
	sums, err := store.List("nightly")
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range sums {
		if s.IsBackup() {
			t.Errorf("a failed capture is listed as a backup: %+v", s)
		}
	}
}

func TestAFailedSnapshotFailsBeforeAnyBytesAreWritten(t *testing.T) {
	store, root := newTestStore(t)
	store.SetSnapshotTaker(&fakeSnapshots{
		start:    time.Unix(1780000000, 0),
		step:     time.Second,
		failAt:   2,
		failWith: errors.New("the pool is full"),
	})
	first := &okSource{spec: ArtifactSpec{ID: "one", Kind: KindDataset, Dataset: &DatasetArtifact{Dataset: "tank/one"}}, bytes: []byte("one")}
	second := &okSource{spec: ArtifactSpec{ID: "two", Kind: KindDataset, Dataset: &DatasetArtifact{Dataset: "tank/two"}}, bytes: []byte("two")}

	_, err := store.Capture(t.Context(), Request{
		PolicyID: "nightly", GenerationID: "gen-1", CombID: "comb-a",
		Consistency: ConsistencySnapshot,
		Sources:     []Source{first, second},
	})
	if err == nil {
		t.Fatal("a snapshot failure did not fail the capture")
	}
	if !containsAll(err.Error(), "snapshot", "pool is full") {
		t.Errorf("the failure does not say what failed and why: %v", err)
	}
	// The second source must never have been read: its snapshot failed,
	// so there was no data to capture and no reason to ask the source for
	// any. The first is expected to have run - its snapshot succeeded
	// before the second's failed - and its artifact is then discarded
	// with the whole generation.
	if second.calls != 0 {
		t.Errorf("the second source was read %d times even though its snapshot failed", second.calls)
	}
	if first.calls != 1 {
		t.Errorf("the first source ran %d times, want 1", first.calls)
	}
	assertNoManifestAnywhere(t, root)
}

func TestACaptureRefusesAnUnlabelledConsistencyBeforeAnyIO(t *testing.T) {
	store, root := newTestStore(t)
	src := &okSource{spec: ArtifactSpec{ID: "one", Kind: KindDataset, Dataset: &DatasetArtifact{Dataset: "tank/one"}}, bytes: []byte("one")}
	_, err := store.Capture(t.Context(), Request{
		PolicyID: "nightly", GenerationID: "gen-1", CombID: "comb-a",
		Sources: []Source{src},
	})
	if err == nil {
		t.Fatal("a capture with no consistency label succeeded")
	}
	if !containsAll(err.Error(), "must say whether it is") {
		t.Errorf("the refusal does not tell the caller what to do: %v", err)
	}
	if src.calls != 0 {
		t.Error("a source was captured before the request was found to be incoherent")
	}
	assertNoManifestAnywhere(t, root)
}

// TestAZeroLengthArtifactIsRefusedRatherThanAccepted: a source that
// produces nothing has either failed silently or captured nothing, and
// either way a zero-byte artifact claiming to be a payload is not a
// capture.
func TestAZeroLengthArtifactIsRefusedRatherThanAccepted(t *testing.T) {
	store, root := newTestStore(t)
	empty := &okSource{spec: ArtifactSpec{ID: "empty", Kind: KindDataset, Dataset: &DatasetArtifact{Dataset: "tank/empty"}}, bytes: nil}
	_, err := store.Capture(t.Context(), Request{
		PolicyID: "nightly", GenerationID: "gen-1", CombID: "comb-a",
		Consistency: ConsistencySnapshot,
		Sources:     []Source{empty},
	})
	if err == nil {
		t.Fatal("a source that produced nothing was captured as a payload artifact")
	}
	if !containsAll(err.Error(), "produced no bytes and reported no error") {
		t.Errorf("the refusal does not explain: %v", err)
	}
	assertNoManifestAnywhere(t, root)
}

func TestCaptureHonoursCancellation(t *testing.T) {
	store, root := newTestStore(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	slow := &okSource{spec: ArtifactSpec{ID: "one", Kind: KindDataset, Dataset: &DatasetArtifact{Dataset: "tank/one"}}, bytes: []byte("one")}
	_, err := store.Capture(ctx, Request{
		PolicyID: "nightly", GenerationID: "gen-1", CombID: "comb-a",
		Consistency: ConsistencySnapshot, Sources: []Source{slow},
	})
	if err == nil {
		t.Fatal("a cancelled capture reported success")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("error is %v, want context.Canceled", err)
	}
	assertNoManifestAnywhere(t, root)
}

// TestACaptureOnlyReusesTheSnapshotNameItWasGiven: withSnapshot copies
// rather than mutating, so a retry cannot record a second snapshot's name
// against the first snapshot's data.
func TestASnapshotFailureDoesNotMutateTheCallersSpec(t *testing.T) {
	store, _ := newTestStore(t)
	store.SetSnapshotTaker(&fakeSnapshots{start: time.Unix(1780000000, 0), step: time.Second, failAt: 2, failWith: errors.New("boom")})
	d := &DatasetArtifact{Dataset: "tank/one"}
	src := &okSource{spec: ArtifactSpec{ID: "one", Kind: KindDataset, Dataset: d}, bytes: []byte("one")}

	_, _ = store.Capture(t.Context(), Request{
		PolicyID: "p", GenerationID: "g-1", CombID: "comb-a",
		Consistency: ConsistencySnapshot, Sources: []Source{src},
	})
	if d.Snapshot != "" || d.SnapshotTakenUnix != 0 {
		t.Errorf("the caller's DatasetArtifact was mutated: snapshot=%q taken=%d", d.Snapshot, d.SnapshotTakenUnix)
	}
	// And the copy that was used carried the name, proving the copy path
	// is the one that runs.
	src2 := &okSource{spec: ArtifactSpec{ID: "one", Kind: KindDataset, Dataset: &DatasetArtifact{Dataset: "tank/one"}}, bytes: []byte("one")}
	store2, _ := newTestStore(t)
	store2.SetSnapshotTaker(&fakeSnapshots{start: time.Unix(1780000000, 0), step: time.Second})
	res, err := store2.Capture(t.Context(), Request{
		PolicyID: "p", GenerationID: "g-1", CombID: "comb-a",
		Consistency: ConsistencySnapshot, Sources: []Source{src2},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Manifest.Artifacts[0].Dataset.Snapshot == "" {
		t.Error("the copy did not carry the snapshot name")
	}
}

func TestACaptureRefusesV2DatasetFieldsItDoesNotProduce(t *testing.T) {
	// recursive and parent_dataset are reserved for a coordinated
	// multi-pool set. A v1 producer that set them would be claiming a
	// capture it did not take, so they are refused at the spec boundary
	// rather than stored and quietly believed.
	store, _ := newTestStore(t)
	for _, tc := range []struct {
		name string
		d    *DatasetArtifact
	}{
		{"recursive", &DatasetArtifact{Dataset: "tank/one", Recursive: true}},
		{"parent", &DatasetArtifact{Dataset: "tank/one", ParentDataset: "tank"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := store.Capture(t.Context(), Request{
				PolicyID: "p", GenerationID: "g-" + tc.name, CombID: "comb-a",
				Consistency: ConsistencySnapshot,
				Sources: []Source{&okSource{
					spec:  ArtifactSpec{ID: "one", Kind: KindDataset, Dataset: tc.d},
					bytes: []byte("payload"),
				}},
			})
			if err == nil {
				t.Fatalf("a dataset artifact setting %s was accepted", tc.name)
			}
			if !containsAll(err.Error(), "does not produce") {
				t.Errorf("the refusal does not say why: %v", err)
			}
		})
	}
}

func TestACaptureWithNoSourcesStillPublishesAConsistentManifest(t *testing.T) {
	// An empty generation is legal - a policy that matched no Cells
	// produces one, and it is honest about containing nothing rather than
	// refusing to exist.
	store, _ := newTestStore(t)
	res, err := store.Capture(t.Context(), Request{
		PolicyID: "nightly", GenerationID: "gen-1", CombID: "comb-a",
		Consistency: ConsistencySnapshot,
	})
	if err != nil {
		t.Fatalf("an empty capture was refused: %v", err)
	}
	if len(res.Manifest.Artifacts) != 0 {
		t.Errorf("Artifacts = %+v, want none", res.Manifest.Artifacts)
	}
	// Verification of it is not Ok - nothing was established - but it is
	// not a failure either.
	v, err := store.Verify(t.Context(), "nightly", "gen-1", VerifyOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if v.Understood != true || v.State != GenerationOK {
		t.Errorf("an empty generation's manifest is %s/%v", v.State, v.Understood)
	}
	if v.Ok() {
		t.Error("an empty generation reports Ok; nothing was established")
	}
}

func TestACaptureIntoAnExistingGenerationIsRefused(t *testing.T) {
	store, _ := newTestStore(t)
	req := Request{
		PolicyID: "nightly", GenerationID: "gen-1", CombID: "comb-a",
		Consistency: ConsistencySnapshot,
		Sources:     []Source{&okSource{spec: ArtifactSpec{ID: "one", Kind: KindDataset, Dataset: &DatasetArtifact{Dataset: "tank/one"}}, bytes: []byte("one")}},
	}
	if _, err := store.Capture(t.Context(), req); err != nil {
		t.Fatal(err)
	}
	_, err := store.Capture(t.Context(), req)
	if err == nil {
		t.Fatal("a second capture overwrote a published generation")
	}
	if !errIs(t, err, ErrGenerationCommitted) {
		t.Errorf("error is %v, want ErrGenerationCommitted", err)
	}
}

func TestResultReportsAFailedRemovalRatherThanSwallowingIt(t *testing.T) {
	// A partial generation that cannot be removed is exactly the case
	// that must not be reported as a clean failure: something is sitting
	// on the target that a future sweep will have to deal with, and a
	// human has to know it is there.
	wfs := newWatchedFS()
	store, root := newTestStore(t, WithFS(wfs))
	wfs.failOn("RemoveAll")

	res, err := store.Capture(t.Context(), Request{
		PolicyID: "nightly", GenerationID: "gen-1", CombID: "comb-a",
		Consistency: ConsistencySnapshot,
		Sources: []Source{&failingSource{
			spec:    ArtifactSpec{ID: "one", Kind: KindDataset, Dataset: &DatasetArtifact{Dataset: "tank/one"}},
			prefix:  []byte("partial"),
			failErr: errors.New("boom"),
		}},
	})
	if err == nil {
		t.Fatal("the capture reported success despite a source failure")
	}
	if res.Aborted {
		t.Error("Aborted is true although the removal failed")
	}
	if res.FailedRemoval == "" {
		t.Fatal("FailedRemoval is empty; a partial generation left on the target was reported as a clean failure")
	}
	// Nothing was published even though the directory survived.
	assertNoManifestAnywhere(t, root)
	// And the listing shows the leftover as a non-backup, so it is
	// visible to a sweep and to an operator.
	sums, lerr := store.List("nightly")
	if lerr != nil {
		t.Fatal(lerr)
	}
	if len(sums) != 1 || sums[0].IsBackup() {
		t.Errorf("listing = %+v, want one non-backup entry for the leftover", sums)
	}
}

// # The Capture source seam

// TestCaptureDoesNotBufferTheWholeStream: a dataset stream is unbounded,
// so the source is pulled through a pipe and the digest is computed as
// the bytes go past. A source that writes more than any fixed buffer must
// still be captured and digested correctly.
func TestCaptureDigestsAStreamLargerThanTheWriteBuffer(t *testing.T) {
	body := make([]byte, 5*64*1024+1234) // several buffer-fill plus a remainder
	for i := range body {
		body[i] = byte(i % 251)
	}
	store, _ := newTestStore(t)
	res, err := store.Capture(t.Context(), Request{
		PolicyID: "nightly", GenerationID: "gen-1", CombID: "comb-a",
		Consistency: ConsistencySnapshot,
		Sources: []Source{&okSource{
			spec:  ArtifactSpec{ID: "big", Kind: KindDataset, Dataset: &DatasetArtifact{Dataset: "tank/big"}},
			bytes: body,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	a := res.Manifest.Artifacts[0]
	if a.SizeBytes != uint64(len(body)) {
		t.Errorf("SizeBytes = %d, want %d", a.SizeBytes, len(body))
	}
	if a.SHA256 != sha256HexOf(body) {
		t.Errorf("digest = %s, want %s", a.SHA256, sha256HexOf(body))
	}
	v, err := store.Verify(t.Context(), "nightly", "gen-1", VerifyOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !v.Ok() {
		t.Errorf("a large captured artifact did not verify: %s", v.Summary())
	}
}

// countingSource reports how many bytes it was asked to produce, used to
// assert the pipe is streaming rather than buffering everything first.
type countingSource struct {
	spec  ArtifactSpec
	total int
	pos   int
}

func (c *countingSource) Describe() ArtifactSpec { return c.spec }

func (c *countingSource) Capture(_ context.Context, w io.Writer) error {
	buf := make([]byte, 4096)
	for c.pos < c.total {
		n := len(buf)
		if remaining := c.total - c.pos; remaining < n {
			n = remaining
		}
		for i := 0; i < n; i++ {
			buf[i] = byte(c.pos + i)
		}
		if _, err := w.Write(buf[:n]); err != nil {
			return err
		}
		c.pos += n
	}
	return nil
}

func TestCaptureStreamsRatherThanBuffering(t *testing.T) {
	total := 1 << 20 // 1 MiB
	src := &countingSource{
		spec:  ArtifactSpec{ID: "stream", Kind: KindDataset, Dataset: &DatasetArtifact{Dataset: "tank/stream"}},
		total: total,
	}
	store, _ := newTestStore(t)
	res, err := store.Capture(t.Context(), Request{
		PolicyID: "nightly", GenerationID: "gen-1", CombID: "comb-a",
		Consistency: ConsistencySnapshot, Sources: []Source{src},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Manifest.Artifacts[0].SizeBytes != uint64(total) {
		t.Errorf("SizeBytes = %d, want %d", res.Manifest.Artifacts[0].SizeBytes, total)
	}
	// Every byte must be there: a pipe closed early would truncate
	// silently and the digest would match the truncated content, so the
	// size is the assertion that matters.
	full := joinPath(res.Generation.Dir(), res.Manifest.Artifacts[0].RelativePath)
	fi, err := (OSFS{}).Stat(full)
	if err != nil {
		t.Fatal(err)
	}
	if uint64(fi.Size()) != uint64(total) {
		t.Errorf("file is %d bytes, want %d", fi.Size(), total)
	}
}

// sha256HexOf digests a byte slice for a test to compare against, using
// the same helper the package's own code uses so a mismatch is a real
// mismatch and not two different hash implementations disagreeing.
func sha256HexOf(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
