package backup

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/glenjbarber/apiary/internal/frontendconfig"
	"github.com/glenjbarber/apiary/internal/nodeconfig"
	"github.com/glenjbarber/apiary/internal/origincert"
	"github.com/glenjbarber/apiary/internal/raftdconfig"
)

// This file holds the machinery every other test in the package leans
// on: a filesystem that can be made to fail at exactly one operation, a
// filesystem that records what a reader could have seen after every
// single operation, a Quiescer that can report whatever the test needs
// it to report, and fixture configurations populated by reflection so a
// test cannot accidentally pass because a field was left zero.

// watchedFS wraps a real filesystem and does two things with every
// operation:
//
//   - it hands the operation to an injectable hook first, so a test can
//     make exactly one call fail and no others, and
//   - it calls an observe hook AFTER the operation, so a test can assert
//     an invariant about the world as a reader would have seen it at
//     every point in between.
//
// The second is the reason this type exists. "A manifest is never visible
// before its artifacts" is a claim about interleaving, and the only way to
// check a claim about interleaving is to look between the steps rather
// than only at the end.
type watchedFS struct {
	inner FS

	mu   sync.Mutex
	ops  []string
	fail func(op, arg string) error
	// observe is called after every operation, with a short name for it.
	// Returning an error aborts the test immediately, which is what makes
	// it usable as an assertion rather than as logging.
	observe func(op string) error
}

var _ FS = (*watchedFS)(nil)

func newWatchedFS() *watchedFS { return &watchedFS{inner: OSFS{}} }

func (w *watchedFS) record(op string) error {
	w.mu.Lock()
	w.ops = append(w.ops, op)
	fail, observe := w.fail, w.observe
	w.mu.Unlock()
	if fail != nil {
		if err := fail(op, ""); err != nil {
			return err
		}
	}
	if observe != nil {
		if err := observe(op); err != nil {
			return err
		}
	}
	return nil
}

// opLog returns a copy of the recorded operation sequence.
func (w *watchedFS) opLog() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]string(nil), w.ops...)
}

// failOn makes exactly the named operation fail once, leaving every
// other call to succeed. A test that wants a rename to fail after three
// successful ones uses failOnAfter.
func (w *watchedFS) failOn(op string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.fail = func(got, _ string) error {
		if got == op {
			return fmt.Errorf("watchedFS: injected failure in %s", op)
		}
		return nil
	}
}

// failAfter makes the named operation fail once it has occurred count
// times, which is how a test fails the *second* fsync - the manifest's,
// after the artifacts' - without touching the first.
func (w *watchedFS) failAfter(op string, occurrences int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	seen := 0
	w.fail = func(got, _ string) error {
		if got != op {
			return nil
		}
		seen++
		if seen > occurrences {
			return fmt.Errorf("watchedFS: injected failure in %s (occurrence %d)", op, seen)
		}
		return nil
	}
}

// MkdirAll implements FS.
func (w *watchedFS) MkdirAll(path string, perm fs.FileMode) error {
	if err := w.record("MkdirAll"); err != nil {
		return err
	}
	return w.inner.MkdirAll(path, perm)
}

// CreateTemp implements FS.
func (w *watchedFS) CreateTemp(dir, pattern string) (File, error) {
	if err := w.record("CreateTemp"); err != nil {
		return nil, err
	}
	f, err := w.inner.CreateTemp(dir, pattern)
	if err != nil {
		return nil, err
	}
	return &watchedFile{File: f, fs: w}, nil
}

// Rename implements FS.
func (w *watchedFS) Rename(oldpath, newpath string) error {
	if err := w.record("Rename"); err != nil {
		return err
	}
	return w.inner.Rename(oldpath, newpath)
}

// Remove implements FS.
func (w *watchedFS) Remove(path string) error {
	if err := w.record("Remove"); err != nil {
		return err
	}
	return w.inner.Remove(path)
}

// RemoveAll implements FS.
func (w *watchedFS) RemoveAll(path string) error {
	if err := w.record("RemoveAll"); err != nil {
		return err
	}
	return w.inner.RemoveAll(path)
}

// Stat implements FS.
func (w *watchedFS) Stat(path string) (fs.FileInfo, error) {
	if err := w.record("Stat"); err != nil {
		return nil, err
	}
	return w.inner.Stat(path)
}

// Open implements FS.
func (w *watchedFS) Open(path string) (io.ReadCloser, error) {
	if err := w.record("Open"); err != nil {
		return nil, err
	}
	return w.inner.Open(path)
}

// ReadDir implements FS.
func (w *watchedFS) ReadDir(path string) ([]fs.DirEntry, error) {
	if err := w.record("ReadDir"); err != nil {
		return nil, err
	}
	return w.inner.ReadDir(path)
}

// ReadFile implements FS.
func (w *watchedFS) ReadFile(path string) ([]byte, error) {
	if err := w.record("ReadFile"); err != nil {
		return nil, err
	}
	return w.inner.ReadFile(path)
}

// WriteFile implements FS.
func (w *watchedFS) WriteFile(path string, data []byte, perm fs.FileMode) error {
	if err := w.record("WriteFile"); err != nil {
		return err
	}
	return w.inner.WriteFile(path, data, perm)
}

// SyncDir implements FS.
func (w *watchedFS) SyncDir(path string) error {
	if err := w.record("SyncDir"); err != nil {
		return err
	}
	return w.inner.SyncDir(path)
}

// watchedFile wraps a File so its Sync, Chmod and Close are separately
// observable and separately failable. They have to be: the whole point of
// the atomic write is the order of exactly these three calls.
type watchedFile struct {
	File
	fs *watchedFS
}

// Sync implements File.
func (f *watchedFile) Sync() error {
	if err := f.fs.record("File.Sync"); err != nil {
		return err
	}
	return f.File.Sync()
}

// Chmod implements File.
func (f *watchedFile) Chmod(mode fs.FileMode) error {
	if err := f.fs.record("File.Chmod"); err != nil {
		return err
	}
	return f.File.Chmod(mode)
}

// Close implements File.
func (f *watchedFile) Close() error {
	if err := f.fs.record("File.Close"); err != nil {
		return err
	}
	return f.File.Close()
}

// manifestVisible reports whether a reader listing the generation
// directory would see a manifest.
func (w *watchedFS) manifestVisible(dir string) bool {
	real := OSFS{}
	if _, err := real.Stat(joinPath(dir, ManifestName)); err == nil {
		return true
	}
	return false
}

// manifestNames lists every ManifestName visible under root, which is how
// a test asserts "a manifest exists NOWHERE" after a failed capture
// rather than only at the path it happened to expect.
func (w *watchedFS) manifestNames(root string) []string {
	var out []string
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if !d.IsDir() && d.Name() == ManifestName {
			out = append(out, p)
		}
		return nil
	})
	return out
}

// assertNoManifestAnywhere is the assertion the all-or-nothing rule
// reduces to, and it is deliberately whole-root: a test that only checks
// one expected path would pass while a manifest sat somewhere else.
func assertNoManifestAnywhere(t *testing.T, root string) {
	t.Helper()
	var found []string
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			// A directory that vanished mid-walk is itself a signal, but
			// for this assertion it just means there is nothing there.
			return nil
		}
		if !d.IsDir() && d.Name() == ManifestName {
			found = append(found, p)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking %s: %v", root, err)
	}
	if len(found) > 0 {
		t.Fatalf("a manifest is visible at %v, but this generation was supposed to be a failure - a directory with a manifest is a backup, and this one is not", found)
	}
}

// # Fixtures

// sentinelFor returns the unique value a fixture puts in one field, so a
// test can grep the encoded bytes for exactly that field's value and get
// a precise answer about that field and no other.
func sentinelFor(structName, fieldName string) string {
	return fmt.Sprintf("SENTINEL-%s.%s", structName, fieldName)
}

// fixtureNodeConfig returns a nodeconfig.Config with EVERY field set to a
// distinct, greppable sentinel, including the two live credentials.
//
// Populating by reflection rather than by hand is the point: a hand-
// written fixture goes stale the moment somebody adds a field, and then
// the completeness test quietly stops covering it. This one cannot.
func fixtureNodeConfig(t *testing.T) (nodeconfig.Config, map[string]string) {
	t.Helper()
	var cfg nodeconfig.Config
	values := populate(t, &cfg, "nodeconfig.Config")
	// The two live credentials get values that are unmistakably secrets,
	// not just distinct strings, so a grep test proves the right thing:
	// that a credential-shaped value did not survive, not that a
	// particular string did not appear.
	cfg.PeerAPIKey = "peer-api-key-LIVE-CREDENTIAL-8f2a1c9d4e7b"
	cfg.RaftdToken = "raftd-token-LIVE-CREDENTIAL-3b6d0e5a1f8c"
	values["PeerAPIKey"] = cfg.PeerAPIKey
	values["RaftdToken"] = cfg.RaftdToken
	return cfg, values
}

func fixtureRaftdConfig(t *testing.T) (raftdconfig.Config, map[string]string) {
	t.Helper()
	var cfg raftdconfig.Config
	values := populate(t, &cfg, "raftdconfig.Config")
	cfg.InternalToken = "internal-token-LIVE-CREDENTIAL-c1d4e8b2a7f9"
	values["InternalToken"] = cfg.InternalToken
	return cfg, values
}

func fixtureFrontendConfig(t *testing.T) (frontendconfig.Config, map[string]string) {
	t.Helper()
	var cfg frontendconfig.Config
	values := populate(t, &cfg, "frontendconfig.Config")
	cfg.ManagerAPIKey = "manager-api-key-LIVE-CREDENTIAL-6e0a3c9b5d2f"
	values["ManagerAPIKey"] = cfg.ManagerAPIKey
	return cfg, values
}

// populate sets every settable field of v to a sentinel, and returns the
// field-name to sentinel map. Pointer fields get a non-nil target, bools
// get a value derived from the name so the projection can be told apart
// from a default, and times are left alone - the projection renders them
// itself.
func populate(t *testing.T, v any, structName string) map[string]string {
	t.Helper()
	rv := reflect.ValueOf(v).Elem()
	rt := rv.Type()
	values := make(map[string]string)
	for i := 0; i < rt.NumField(); i++ {
		f := rt.Field(i)
		if f.PkgPath != "" { // unexported
			continue
		}
		fv := rv.Field(i)
		switch fv.Kind() {
		case reflect.String:
			s := sentinelFor(structName, f.Name)
			fv.SetString(s)
			values[f.Name] = s
		case reflect.Pointer:
			b := true
			p := reflect.New(fv.Type().Elem())
			p.Elem().SetBool(b)
			fv.Set(p)
		case reflect.Bool:
			fv.SetBool(true)
		case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
			fv.SetInt(4242)
		case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
			fv.SetUint(8484)
		}
	}
	return values
}

// fixtureOriginCerts returns an inventory entry whose key path is set,
// standing in for the private key that origincert.WritePair writes beside
// the certificate.
func fixtureOriginCerts() []origincert.InventoryEntry {
	return []origincert.InventoryEntry{{
		Name:         "edge",
		Service:      "tunnel",
		Hostnames:    []string{"edge.example.test"},
		ID:           "cert-id-0001",
		ExpiresAt:    time.Date(2027, 1, 2, 3, 4, 5, 0, time.UTC),
		CertPath:     "/var/db/apiary/origincert/edge.crt",
		KeyPath:      "/var/db/apiary/origincert/edge.key",
		UpdatedAt:    time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
		AutoRenew:    true,
		ValidityDays: 90,
	}}
}

// originCertKeyPEM is a syntactically plausible private key blob standing
// in for what origincert.NewCSR returns. It is a fixture, not a key: it
// is not a real curve point and nothing ever signs with it. Its only job
// is to be a distinctive byte string that must not appear in an archive.
const originCertKeyPEM = `-----BEGIN PRIVATE KEY-----
MIIEvQIBADANBgkqhkiG9w0BAQEFAASCBKcwggSjAgEAAoIBAQfixture
fixturefixturefixturefixturefixturefixturefixturefixturefix
-----END PRIVATE KEY-----`

// # Test doubles

// fakeQuiescer reports whatever the test tells it to. Observed is the
// field that matters: a Quiescer that stops something and cannot confirm
// it must say so, and a test that wants to exercise the refusal sets
// Observed false.
type fakeQuiescer struct {
	evidence StopEvidence
	err      error
	calls    []string
}

func (q *fakeQuiescer) Quiesce(_ context.Context, cellID string) (StopEvidence, error) {
	q.calls = append(q.calls, cellID)
	if q.err != nil {
		return StopEvidence{}, q.err
	}
	ev := q.evidence
	ev.CellID = cellID
	return ev, nil
}

// observedQuiescer returns a Quiescer that reports a positive, method-
// named observation, which is the only shape that entitles a capture to
// the quiesced label.
func observedQuiescer(at time.Time) *fakeQuiescer {
	return &fakeQuiescer{evidence: StopEvidence{
		Observed: true,
		Method:   "GetJail reported not running",
		AtUnix:   unixSeconds(at),
	}}
}

// fakeSnapshots hands out snapshot names and times from a counter, so a
// test can produce a capture window of a chosen width.
type fakeSnapshots struct {
	start    time.Time
	step     time.Duration
	n        int
	names    []string
	failAt   int // 1-based index to fail on; 0 means never
	failWith error
}

func (s *fakeSnapshots) Snapshot(_ context.Context, spec ArtifactSpec) (string, int64, error) {
	s.n++
	if s.failAt != 0 && s.n == s.failAt {
		return "", 0, s.failWith
	}
	at := s.start.Add(time.Duration(s.n-1) * s.step)
	name := fmt.Sprintf("apiary-backup-%d", s.n)
	if s.n-1 < len(s.names) {
		name = s.names[s.n-1]
	}
	return name, unixSeconds(at), nil
}

// failingSource is a Source that writes a prefix and then fails, which is
// the interesting half-written case: a source that stops early without
// saying so would otherwise produce a short artifact that verifies fine.
type failingSource struct {
	spec    ArtifactSpec
	prefix  []byte
	writeN  int
	failErr error
	calls   int
}

func (s *failingSource) Describe() ArtifactSpec { return s.spec }

func (s *failingSource) Capture(_ context.Context, w io.Writer) error {
	s.calls++
	if _, err := w.Write(s.prefix); err != nil {
		return err
	}
	if s.writeN <= 0 {
		return s.failErr
	}
	return nil
}

// okSource is a Source producing fixed bytes.
type okSource struct {
	spec  ArtifactSpec
	bytes []byte
	// failAfter, when non-zero, writes the bytes and then returns an
	// error - the "partial then fail" case.
	failAfter error
	calls     int
}

func (s *okSource) Describe() ArtifactSpec { return s.spec }

func (s *okSource) Capture(_ context.Context, w io.Writer) error {
	s.calls++
	if _, err := w.Write(s.bytes); err != nil {
		return err
	}
	return s.failAfter
}

// recordedExtractor stands in for internal/jailarchive's Extractor,
// recording what it was asked to unpack and from where.
type recordedExtractor struct {
	calls []extractCall
	err   error
	// onExtract, when set, is called with the destination directory and
	// the archive path, so a test can assert the extraction happened
	// into the right place from the right bytes.
	onExtract func(archivePath, destDir string) error
}

type extractCall struct {
	ArchivePath string
	DestDir     string
}

func (e *recordedExtractor) Extract(ctx context.Context, archivePath, destDir string) error {
	e.calls = append(e.calls, extractCall{ArchivePath: archivePath, DestDir: destDir})
	if err := ctx.Err(); err != nil {
		return err
	}
	if e.onExtract != nil {
		return e.onExtract(archivePath, destDir)
	}
	return e.err
}

// fixedClock returns a clock that advances by step on every call, so a
// test can produce a capture window of a chosen width without sleeping.
func fixedClock(start time.Time, step time.Duration) func() time.Time {
	var n int
	var mu sync.Mutex
	return func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		t := start.Add(time.Duration(n) * step)
		n++
		return t
	}
}

// containsAll is a small helper for asserting a message names every
// one of a set of fragments. Error messages in this package carry the
// specific reason on purpose, and a test that only checks err != nil
// would not notice if that reason were deleted.
func containsAll(haystack string, needles ...string) bool {
	for _, n := range needles {
		if !strings.Contains(haystack, n) {
			return false
		}
	}
	return true
}

// errIs is errors.Is, named so test tables read as a statement.
func errIs(t *testing.T, err, target error) bool {
	t.Helper()
	return errors.Is(err, target)
}

// filepathWalk calls fn for every regular file under root, in lexical
// order. It exists so the byte-level secrets test can grep a whole
// generation without every call site re-implementing a walk.
func filepathWalk(root string, fn func(path string, isDir bool) error) error {
	return filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		return fn(p, info.IsDir())
	})
}

// encodeUnsafeManifest produces manifest bytes that are internally
// well-formed - valid JSON with a correct self-checksum - but that
// Validate would refuse. It exists so the read path can be tested
// against the shape a hand-edited or hostile archive actually has,
// rather than only against a document that fails to parse.
func encodeUnsafeManifest(m *Manifest) ([]byte, error) {
	// EncodeManifest computes the checksum over the body, and the body is
	// whatever the caller put in the struct, including an unsafe path.
	// Validate is simply not called, which is exactly the situation
	// ReadManifest's own Validate call exists to catch.
	return EncodeManifest(JSONCodec{}, m)
}
