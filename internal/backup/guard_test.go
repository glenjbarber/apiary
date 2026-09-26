package backup

import (
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"time"
)

// # The guards wired into the write path
//
// secrets_test.go proves the projections cannot carry a secret and that
// the tables are complete. This file proves the two byte-level guards
// are actually REACHED by a write - a guard that is tested in isolation
// and never called is not a guard, and the failure mode of not calling it
// is precisely the one this package exists to prevent.

// TestTheByteGuardBlocksAnArtifactCarryingALiveSecret: the whole point
// of the watchlist is that it runs on the bytes on their way to disk.
func TestTheByteGuardBlocksAnArtifactCarryingALiveSecret(t *testing.T) {
	node, _ := fixtureNodeConfig(t)
	raftd, _ := fixtureRaftdConfig(t)
	front, _ := fixtureFrontendConfig(t)
	san := NewSanitizer(&node, &raftd, &front)

	store, root := newTestStore(t, WithSanitizer(san))
	if !store.GuardArmed() {
		t.Fatal("the store reports an unarmed guard with three live secrets configured")
	}
	gen, err := store.Begin("nightly", "gen-1")
	if err != nil {
		t.Fatal(err)
	}
	_, err = gen.WriteBytes(t.Context(), ArtifactSpec{
		ID:      "smuggled",
		Kind:    KindDataset,
		Dataset: &DatasetArtifact{Dataset: "tank/one"},
	}, []byte("some bytes, and then "+node.PeerAPIKey+" embedded in the middle"))
	if err == nil {
		t.Fatal("an artifact carrying a live credential was written")
	}
	if !errIs(t, err, ErrSecretExposed) {
		t.Errorf("error is %v, want ErrSecretExposed", err)
	}
	if !containsAll(err.Error(), "no part of the generation was published") {
		t.Errorf("the refusal does not say what was not done: %v", err)
	}
	assertNoManifestAnywhere(t, root)
	// And the bytes never reached the filesystem at all, which is a
	// stronger statement than "the manifest does not mention it".
	full := joinPath(gen.Dir(), "artifacts", "smuggled.stream")
	if _, statErr := (OSFS{}).Stat(full); statErr == nil {
		t.Error("the refused artifact is on disk under its final name")
	}
}

// TestTheByteGuardFindsASecretSplitAcrossChunks: the guard streams, so
// a naive per-chunk search would miss a credential straddling a read
// boundary - which for a real token inside a multi-gigabyte stream is not
// an edge case but a coin flip.
func TestTheByteGuardFindsASecretSplitAcrossChunks(t *testing.T) {
	node, _ := fixtureNodeConfig(t)
	secret := node.RaftdToken
	san := NewSanitizer(&node, nil, nil)

	// Split in the middle of the secret, with plenty on both sides so
	// neither chunk contains it whole.
	head := strings.Repeat("filler ", 20000)
	tail := strings.Repeat(" more filler", 20000)
	store, _ := newTestStore(t, WithSanitizer(san))
	gen, err := store.Begin("nightly", "gen-1")
	if err != nil {
		t.Fatal(err)
	}
	cut := len(head) + len(secret)/2
	whole := head + secret + tail
	src := &chunkedReader{parts: []string{whole[:cut], whole[cut:]}}
	_, err = gen.WriteArtifact(t.Context(), ArtifactSpec{
		ID:      "split",
		Kind:    KindDataset,
		Dataset: &DatasetArtifact{Dataset: "tank/one"},
	}, src)
	if err == nil {
		t.Fatal("a secret split across two read chunks was not found")
	}
	if !errIs(t, err, ErrSecretExposed) {
		t.Errorf("error is %v, want ErrSecretExposed", err)
	}
	if src.reads < 2 || src.part < 1 {
		t.Fatalf("the test did not actually deliver both halves: %d reads, ended in part %d", src.reads, src.part)
	}
}

// chunkedReader delivers first and then second, advancing properly across
// as many Read calls as it takes. It is a real two-part stream rather than
// a two-Read trick, so the test exercises the scanner's overlap across
// whatever boundaries writeFile's buffer happens to produce.
type chunkedReader struct {
	parts []string
	part  int
	off   int
	reads int
}

func (c *chunkedReader) Read(p []byte) (int, error) {
	c.reads++
	for c.part < len(c.parts) {
		cur := c.parts[c.part]
		if c.off >= len(cur) {
			c.part++
			c.off = 0
			continue
		}
		n := copy(p, cur[c.off:])
		c.off += n
		return n, nil
	}
	return 0, io.EOF
}

// TestTheByteGuardRunsOnTheManifestToo: the manifest's gap list names
// every excluded credential by description, and this is the check that a
// caller cannot put a value into a field of its own.
func TestTheByteGuardRunsOnTheManifestToo(t *testing.T) {
	node, _ := fixtureNodeConfig(t)
	raftd, _ := fixtureRaftdConfig(t)
	front, _ := fixtureFrontendConfig(t)
	san := NewSanitizer(&node, &raftd, &front)
	store, root := newTestStore(t, WithSanitizer(san))
	gen, err := store.Begin("nightly", "gen-1")
	if err != nil {
		t.Fatal(err)
	}
	m := sampleGenerationManifest("nightly", "gen-1")
	// The comb id is an ordinary provenance field a caller might fill
	// from somewhere it should not have. Putting a live credential in it
	// is a mistake the guard has to catch even though the field is not
	// secret-shaped.
	m.CombID = "comb-" + front.ManagerAPIKey
	err = gen.Commit(t.Context(), m)
	if err == nil {
		t.Fatal("a manifest carrying a live credential was published")
	}
	if !errIs(t, err, ErrSecretExposed) {
		t.Errorf("error is %v, want ErrSecretExposed", err)
	}
	assertNoManifestAnywhere(t, root)
}

// # The unconditional node-config guard

// TestANodeConfigArtifactIsAuditedBeforeItIsWritten is the check that
// runs with NO live secrets configured at all - the case where the
// watchlist is correctly silent and something still has to catch a
// projection that gained an unclassified field.
func TestANodeConfigArtifactIsAuditedBeforeItIsWritten(t *testing.T) {
	store, root := newTestStore(t) // no sanitizer: the watchlist is empty
	if store.GuardArmed() {
		t.Fatal("a store with no sanitizer reports an armed guard")
	}
	gen, err := store.Begin("nightly", "gen-1")
	if err != nil {
		t.Fatal(err)
	}

	// A record with a secret-shaped key that nothing classified. This is
	// exactly the shape of a future nodeconfig.Config field that somebody
	// added and forgot to add to the deny list.
	smuggled := map[string]any{
		"node_id":       "comb-a",
		"uplink":        "re0",
		"encryptionkey": "live-credential-value",
	}
	body, err := json.Marshal(smuggled)
	if err != nil {
		t.Fatal(err)
	}
	_, err = gen.WriteBytes(t.Context(), ArtifactSpec{ID: "node-config", Kind: KindNodeConfig}, body)
	if err == nil {
		t.Fatal("a node config record carrying an unclassified secret-shaped key was written")
	}
	if !errIs(t, err, ErrSecretExposed) {
		t.Errorf("error is %v, want ErrSecretExposed", err)
	}
	if !containsAll(err.Error(), "encryptionkey", "must be deny-listed") {
		t.Errorf("the refusal does not name the key and what has to happen: %v", err)
	}
	assertNoManifestAnywhere(t, root)

	// A nested object is audited too, not just the top level.
	nested, err := json.Marshal(map[string]any{
		"peer": map[string]any{"PeerAPIKey": "live-credential-value"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := gen.WriteBytes(t.Context(), ArtifactSpec{ID: "nested", Kind: KindNodeConfig}, nested); !errIs(t, err, ErrSecretExposed) {
		t.Errorf("a nested secret was not found: %v", err)
	}
	// And an array of records, which is what the certificate inventory
	// legitimately is, is walked rather than skipped.
	arr, err := json.Marshal([]map[string]any{{"key_path": "/var/db/x.key"}, {"EncryptionKey": "live"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := gen.WriteBytes(t.Context(), ArtifactSpec{ID: "arr", Kind: KindNodeConfig}, arr); !errIs(t, err, ErrSecretExposed) {
		t.Errorf("a secret inside an array element was not found: %v", err)
	}
	// A recorded path is not a secret, and must still pass.
	clean, err := json.Marshal(map[string]any{
		"tls_key":               "/var/db/apiary/managerd.key",
		"cloudflare_token_file": "/var/db/apiary/tunnel.token",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := gen.WriteBytes(t.Context(), ArtifactSpec{ID: "clean", Kind: KindNodeConfig}, clean); err != nil {
		t.Errorf("a record of secret PATHS was refused: %v", err)
	}
}

func TestANodeConfigArtifactThatIsNotJSONIsRefused(t *testing.T) {
	// A decode failure is not a pass. An unauditable record is exactly
	// the case the audit exists to catch, so it is a hard error.
	store, root := newTestStore(t)
	gen, err := store.Begin("nightly", "gen-1")
	if err != nil {
		t.Fatal(err)
	}
	_, err = gen.WriteBytes(t.Context(), ArtifactSpec{ID: "node-config", Kind: KindNodeConfig}, []byte("this is not JSON"))
	if err == nil {
		t.Fatal("a node config artifact that is not JSON was written")
	}
	if !containsAll(err.Error(), "could not be audited") {
		t.Errorf("the refusal does not say that the contents were unauditable: %v", err)
	}
	assertNoManifestAnywhere(t, root)
}

func TestANodeConfigArtifactThatIsEmptyIsRefusedByTheCapturePath(t *testing.T) {
	store, root := newTestStore(t)
	_, err := store.Capture(t.Context(), Request{
		PolicyID: "nightly", GenerationID: "gen-1", CombID: "comb-a",
		Consistency: ConsistencySnapshot,
		Sources: []Source{&okSource{
			spec:  ArtifactSpec{ID: "node-config", Kind: KindNodeConfig},
			bytes: nil,
		}},
	})
	if err == nil {
		t.Fatal("an empty node config artifact was captured")
	}
	assertNoManifestAnywhere(t, root)
}

// TestACaptureWithAnArmedGuardCatchesASmuggledConfigEndToEnd: the whole
// chain, from a Source that produces a record carrying a live credential
// to a generation directory with nothing in it.
func TestACaptureWithAnArmedGuardCatchesASmuggledConfigEndToEnd(t *testing.T) {
	node, _ := fixtureNodeConfig(t)
	raftd, _ := fixtureRaftdConfig(t)
	front, _ := fixtureFrontendConfig(t)
	san := NewSanitizer(&node, &raftd, &front)
	_ = front
	if err := san.AddSecret("origincert.key", originCertKeyPEM); err != nil {
		t.Fatal(err)
	}
	if san.Watched() != 5 {
		t.Fatalf("watching %d values, want 5", san.Watched())
	}

	store, root := newTestStore(t, WithSanitizer(san))
	res, err := store.Capture(t.Context(), Request{
		PolicyID: "nightly", GenerationID: "gen-1", CombID: "comb-a",
		Consistency: ConsistencySnapshot,
		Sources: []Source{
			&okSource{spec: ArtifactSpec{ID: "good", Kind: KindDataset, Dataset: &DatasetArtifact{Dataset: "tank/good"}}, bytes: []byte("a perfectly good dataset stream")},
			// A source that smuggles the certificate key PEM inside an
			// otherwise ordinary-looking stream. The projection layer
			// cannot catch this - the bytes are opaque - which is why the
			// watchlist exists as a separate layer.
			&okSource{spec: ArtifactSpec{ID: "smuggled", Kind: KindDataset, Dataset: &DatasetArtifact{Dataset: "tank/bad"}},
				bytes: []byte("prefix\n" + originCertKeyPEM + "\nsuffix")},
			&okSource{spec: ArtifactSpec{ID: "never", Kind: KindDataset, Dataset: &DatasetArtifact{Dataset: "tank/never"}}, bytes: []byte("x")},
		},
	})
	if err == nil {
		t.Fatal("a capture whose second artifact carried a live credential succeeded")
	}
	if !errIs(t, err, ErrSecretExposed) {
		t.Errorf("error is %v, want ErrSecretExposed", err)
	}
	if !res.Aborted {
		t.Errorf("the generation was not aborted: %+v", res)
	}
	if res.CapturedBefore != 1 {
		t.Errorf("CapturedBefore = %d, want 1", res.CapturedBefore)
	}
	assertNoManifestAnywhere(t, root)
	// The good artifact went with it: all-or-nothing, not best-effort.
	if _, statErr := (OSFS{}).Stat(joinPath(root, "nightly", "gen-1")); statErr == nil {
		t.Error("the partial generation directory survived the abort")
	}
}

// # Sanitizer mechanics

func TestSecretScannerIsANoOpWhenUnarmed(t *testing.T) {
	var s *Sanitizer
	sc := s.newSecretScanner("anything")
	if sc != nil {
		t.Fatal("an unarmed sanitizer produced a scanner")
	}
	// A nil receiver's Write must be a pass-through, not a panic, because
	// the write path calls it unconditionally.
	n, err := sc.Write([]byte("anything"))
	if n != 8 || err != nil {
		t.Errorf("nil scanner Write = (%d, %v), want (8, nil)", n, err)
	}
}

func TestSecretScannerFindsEveryWatchedValue(t *testing.T) {
	secrets := []string{
		"alpha-secret-value-here",
		"bravo-secret-value-here",
		"charlie-secret-value",
	}
	sc := &secretScanner{watchlist: secrets, longest: len(secrets[0]), what: "test"}
	for _, want := range secrets {
		payload := []byte(strings.Repeat("x", 10) + want + strings.Repeat("y", 10))
		s := &secretScanner{watchlist: secrets, longest: len(secrets[0]), what: "test"}
		if _, err := s.Write(payload); err == nil {
			t.Errorf("the scanner missed %q", want)
		} else if !errors.Is(err, ErrSecretExposed) {
			t.Errorf("error for %q is %v, want ErrSecretExposed", want, err)
		}
	}
	// A payload with none of them passes, and the scanner keeps the
	// overlap it needs.
	if _, err := sc.Write([]byte(strings.Repeat("x", 200))); err != nil {
		t.Errorf("a clean payload was refused: %v", err)
	}
	if len(sc.overlap) != sc.longest-1 {
		t.Errorf("overlap is %d bytes, want %d - a scanner that keeps the wrong tail will miss a secret split across writes", len(sc.overlap), sc.longest-1)
	}
}

func TestSecretScannerHandlesAZeroLengthWrite(t *testing.T) {
	sc := &secretScanner{watchlist: []string{"abc"}, longest: 3, what: "t"}
	n, err := sc.Write(nil)
	if n != 0 || err != nil {
		t.Errorf("Write(nil) = (%d, %v)", n, err)
	}
}

func TestAuditSecretFreeChecksMapKeys(t *testing.T) {
	// A decoded JSON record is a map, and the field names live in the
	// keys. Without the key arm of the walk, auditing a decoded record
	// would check values and never names.
	if err := AuditSecretFree(map[string]any{"node_id": "comb-a", "uplink": "re0"}); err != nil {
		t.Errorf("a clean record was refused: %v", err)
	}
	if err := AuditSecretFree(map[string]any{"peer_api_key": "live"}); err == nil {
		t.Error("a record with a deny-listed key was accepted")
	}
	if err := AuditSecretFree(map[string]any{"upstream_webhook": "https://example.test"}); err != nil {
		t.Errorf("an ordinary record was refused: %v", err)
	}
}

func TestAuditSecretFreeIgnoresNullAndEmptyValues(t *testing.T) {
	// A field present but null carries no secret, and a config on a
	// single-node install legitimately has an empty one.
	if err := AuditSecretFree(map[string]any{"peer_api_key": nil, "raftd_token": ""}); err != nil {
		t.Errorf("an unset credential field was refused: %v", err)
	}
}

func TestNewStoreWithoutASanitizerIsUsable(t *testing.T) {
	// A node with no live secrets must still be able to back up, and the
	// unconditional guards still apply. Refusing to publish with nothing
	// to watch would be a bug, not caution.
	store, _ := newTestStore(t)
	if store.GuardArmed() {
		t.Fatal("a store with no sanitizer claims an armed guard")
	}
	gen, err := store.Begin("nightly", "gen-1")
	if err != nil {
		t.Fatal(err)
	}
	a, err := gen.WriteBytes(t.Context(), ArtifactSpec{ID: "ok", Kind: KindDataset, Dataset: &DatasetArtifact{Dataset: "tank/one"}}, []byte("payload"))
	if err != nil {
		t.Fatalf("a write was refused with nothing to watch: %v", err)
	}
	m := sampleGenerationManifest("nightly", "gen-1")
	m.Artifacts = []Artifact{a}
	if err := gen.Commit(t.Context(), m); err != nil {
		t.Fatalf("a commit was refused with nothing to watch: %v", err)
	}
}

func TestWithSanitizerIgnoresNil(t *testing.T) {
	store, _ := newTestStore(t, WithSanitizer(nil))
	if store.GuardArmed() {
		t.Error("WithSanitizer(nil) armed the guard")
	}
}

func TestAuditNodeConfigRecordAcceptsEveryProjection(t *testing.T) {
	// Every record this package actually produces must survive its own
	// guard, or the guard is a tripwire that fires on correct behaviour.
	node, _ := fixtureNodeConfig(t)
	raftd, _ := fixtureRaftdConfig(t)
	front, _ := fixtureFrontendConfig(t)
	for name, v := range map[string]any{
		"node":     BuildNodeConfigRecord(node, []string{"comb-b"}),
		"raftd":    BuildRaftdConfigRecord(raftd),
		"frontend": BuildFrontendConfigRecord(front),
		"certs":    BuildOriginCertRecords(fixtureOriginCerts()),
		"one cert": BuildOriginCertRecords(fixtureOriginCerts()[:1]),
		"no certs": BuildOriginCertRecords(nil),
	} {
		body, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		if err := auditNodeConfigRecord(body); err != nil {
			t.Errorf("the %s projection was refused by its own guard: %v", name, err)
		}
	}
}

func TestCapturesWithNoSourcesCarryACoherentInstant(t *testing.T) {
	// A generation with no artifacts has no spread to record, so its
	// window is the single instant it was taken. It is still an instant,
	// and a manifest with no instant at all would be refused.
	store, _ := newTestStore(t, WithClock(fixedClock(time.Unix(1780000042, 0), 0)))
	res, err := store.Capture(t.Context(), Request{
		PolicyID: "nightly", GenerationID: "gen-1", CombID: "comb-a",
		Consistency: ConsistencySnapshot,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Manifest.EarliestSnapshotUnix != 1780000042 || res.Manifest.LatestSnapshotUnix != 1780000042 {
		t.Errorf("window = [%d, %d], want the single instant 1780000042",
			res.Manifest.EarliestSnapshotUnix, res.Manifest.LatestSnapshotUnix)
	}
}
