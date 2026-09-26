package isostore

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/glenjbarber/apiary/internal/freebsdimg"
)

// testImageBytes is the "decompressed" image every test here works with: a
// few kilobytes of recognizable text, standing in for the multi-gigabyte
// official raw.xz. xzCompress produces the .raw.xz bytes the fake mirror
// serves, and the SHA-256 recorded in the catalog is computed from those
// exact bytes, so the verification path under test is the real one.
var testImageBytes = []byte("fake FreeBSD official raw image\n" + strings.Repeat("bhyve base disk filler\n", 128))

// xzCompress returns testImageBytes as a valid .xz stream, using the same
// xz(1) the production path shells out to. Skips the test when xz is not
// installed, rather than reaching for the network or vendoring a decoder.
func xzCompress(t *testing.T) []byte {
	t.Helper()
	if _, err := exec.LookPath("xz"); err != nil {
		t.Skip("xz(1) not installed; skipping (this is the same binary the fetch path requires)")
	}
	cmd := exec.Command("xz", "-c")
	cmd.Stdin = bytes.NewReader(testImageBytes)
	var out, stderr bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("xz -c: %v: %s", err, stderr.String())
	}
	return out.Bytes()
}

// fakeMirror is an httptest server standing in for download.freebsd.org,
// serving one catalog entry.
type fakeMirror struct {
	server *httptest.Server
	images []freebsdimg.OfficialImage

	mu   sync.Mutex
	hits int

	// body, when non-nil, is served instead of the real compressed bytes
	// - used to produce checksum mismatches.
	body []byte
	// truncateTo, when non-zero, serves only that many bytes of the
	// compressed image, chunked, so the client's read ends cleanly and
	// the length check - not a transport error - is what catches it.
	truncateTo int
	// status, when non-zero, is returned instead of the image.
	status int
}

func newFakeMirror(t *testing.T, name string, compressed []byte) *fakeMirror {
	t.Helper()
	m := &fakeMirror{
		images: []freebsdimg.OfficialImage{{
			Name:       name,
			URL:        "", // filled in below, once the server has an address
			SHA256:     sha256Hex(string(compressed)),
			Filesystem: "zfs",
			Release:    "15.1-RELEASE",
			SizeBytes:  int64(len(compressed)),
		}},
	}
	m.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		m.mu.Lock()
		m.hits++
		body, status, truncateTo := m.body, m.status, m.truncateTo
		m.mu.Unlock()

		if status != 0 {
			http.Error(w, "no such image", status)
			return
		}
		if body == nil {
			body = compressed
		}
		if truncateTo > 0 {
			// Chunked explicitly, so the client sees a body that ended
			// cleanly at the wrong length instead of a transport error.
			w.Header().Set("Transfer-Encoding", "chunked")
			body = body[:truncateTo]
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Write(body)
	}))
	t.Cleanup(m.server.Close)
	m.images[0].URL = m.server.URL + "/" + name
	return m
}

func (m *fakeMirror) hitCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.hits
}

func (m *fakeMirror) serve(body []byte, status, truncateTo int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.body, m.status, m.truncateTo = body, status, truncateTo
}

// newFetchManager returns a store wired to the fake mirror's catalog, so
// nothing in this file can reach the real download.freebsd.org.
func newFetchManager(t *testing.T, dir string, mirror *fakeMirror) *Manager {
	t.Helper()
	m := New(dir)
	m.freebsdImages = &freebsdimg.Manager{HTTPClient: mirror.server.Client(), Catalog: mirror.images}
	// Generous by default: a t.TempDir() on a small tmpfs would otherwise
	// fail every fetch test on the preflight, which has its own test.
	m.freeSpace = func(string) (int64, error) { return 1 << 40, nil }
	return m
}

const testImageName = "FreeBSD-15.1-RELEASE-amd64-zfs.raw"

// storeEntries returns the names in the store directory, so a test can
// assert exactly what was left behind.
func storeEntries(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatalf("ReadDir() error: %v", err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

func TestEnsureFreeBSDImage_FetchesVerifiesAndDecompresses(t *testing.T) {
	compressed := xzCompress(t)
	mirror := newFakeMirror(t, testImageName, compressed)
	dir := t.TempDir()
	m := newFetchManager(t, dir, mirror)

	info, err := m.EnsureFreeBSDImage(context.Background(), testImageName, nil)
	if err != nil {
		t.Fatalf("EnsureFreeBSDImage() error: %v", err)
	}
	if info.Name != testImageName {
		t.Errorf("Info.Name = %q, want %q", info.Name, testImageName)
	}
	if info.SizeBytes != int64(len(testImageBytes)) {
		t.Errorf("Info.SizeBytes = %d, want %d (the decompressed size, not the download's)", info.SizeBytes, len(testImageBytes))
	}
	if want := sha256Hex(string(testImageBytes)); info.SHA256 != want {
		t.Errorf("Info.SHA256 = %s, want %s (the decompressed file's hash)", info.SHA256, want)
	}

	got, err := os.ReadFile(m.path(testImageName))
	if err != nil {
		t.Fatalf("reading fetched image: %v", err)
	}
	if !bytes.Equal(got, testImageBytes) {
		t.Errorf("fetched image is not the decompressed contents: got %d bytes, want %d", len(got), len(testImageBytes))
	}

	// The image is a first-class store entry, not a private cache: the
	// reconciler resolving a VM's base_image_name and the UI's image
	// picker both go through exactly these two calls.
	path, exists, err := m.Path(testImageName)
	if err != nil || !exists {
		t.Errorf("Path() = (%q, %v, %v), want the fetched image to resolve", path, exists, err)
	}
	listed, err := m.List()
	if err != nil {
		t.Fatalf("List() error: %v", err)
	}
	if len(listed) != 1 || listed[0].Name != testImageName || listed[0].SHA256 != info.SHA256 {
		t.Errorf("List() = %+v, want the official image listed with its recorded hash", listed)
	}

	// Nothing else is left behind: no compressed copy (dropped once
	// verified), no temp files, and exactly one hash sidecar.
	want := []string{testImageName, testImageName + ".sha256"}
	if entries := storeEntries(t, dir); !equalStrings(entries, want) {
		t.Errorf("store dir holds %v, want exactly %v", entries, want)
	}
}

func TestEnsureFreeBSDImage_SecondCallIsANoOp(t *testing.T) {
	compressed := xzCompress(t)
	mirror := newFakeMirror(t, testImageName, compressed)
	m := newFetchManager(t, t.TempDir(), mirror)

	first, err := m.EnsureFreeBSDImage(context.Background(), testImageName, nil)
	if err != nil {
		t.Fatalf("first EnsureFreeBSDImage() error: %v", err)
	}
	second, err := m.EnsureFreeBSDImage(context.Background(), testImageName, nil)
	if err != nil {
		t.Fatalf("second EnsureFreeBSDImage() error: %v", err)
	}
	if mirror.hitCount() != 1 {
		t.Errorf("mirror saw %d requests, want 1 - a present image must not be re-downloaded", mirror.hitCount())
	}
	if *first != *second {
		t.Errorf("second call returned %+v, want the same as the first %+v", second, first)
	}
}

func TestEnsureFreeBSDImage_ProgressIsMonotonicAndEndsAtTotal(t *testing.T) {
	compressed := xzCompress(t)
	mirror := newFakeMirror(t, testImageName, compressed)
	m := newFetchManager(t, t.TempDir(), mirror)

	var mu sync.Mutex
	var seen [][2]int64
	if _, err := m.EnsureFreeBSDImage(context.Background(), testImageName, func(fetched, total int64) {
		mu.Lock()
		defer mu.Unlock()
		seen = append(seen, [2]int64{fetched, total})
	}); err != nil {
		t.Fatalf("EnsureFreeBSDImage() error: %v", err)
	}
	if len(seen) < 2 {
		t.Fatalf("progress reported %d times, want reports from both the download and the decompression", len(seen))
	}

	total := seen[0][1]
	if total != requiredBytes(mirror.images[0]) {
		t.Errorf("progress total = %d, want the store's own estimate %d", total, requiredBytes(mirror.images[0]))
	}
	last := int64(-1)
	for _, s := range seen {
		if s[1] != total {
			t.Errorf("progress total changed from %d to %d mid-fetch; one fetch is one number", total, s[1])
		}
		if s[0] < last {
			t.Errorf("progress went backwards: %d after %d", s[0], last)
		}
		if s[0] > total {
			t.Errorf("progress reported %d of %d; a progress bar must never overfill", s[0], total)
		}
		last = s[0]
	}
	if last != total {
		t.Errorf("final progress = %d, want it to reach the total %d", last, total)
	}
}

func TestEnsureFreeBSDImage_ProgressOnNoOpReachesTotal(t *testing.T) {
	compressed := xzCompress(t)
	mirror := newFakeMirror(t, testImageName, compressed)
	m := newFetchManager(t, t.TempDir(), mirror)
	if _, err := m.EnsureFreeBSDImage(context.Background(), testImageName, nil); err != nil {
		t.Fatalf("EnsureFreeBSDImage() error: %v", err)
	}

	calls := 0
	var lastFetched, lastTotal int64 = -1, -1
	if _, err := m.EnsureFreeBSDImage(context.Background(), testImageName, func(fetched, total int64) {
		calls++
		lastFetched, lastTotal = fetched, total
	}); err != nil {
		t.Fatalf("second EnsureFreeBSDImage() error: %v", err)
	}
	if calls != 1 {
		t.Errorf("no-op fetch reported progress %d times, want exactly 1 so a progress bar ends full either way", calls)
	}
	if lastFetched != lastTotal || lastTotal <= 0 {
		t.Errorf("no-op progress = (%d, %d), want (total, total)", lastFetched, lastTotal)
	}
}

func TestEnsureFreeBSDImage_ChecksumMismatchKeepsNothing(t *testing.T) {
	compressed := xzCompress(t)
	mirror := newFakeMirror(t, testImageName, compressed)
	// Same length as the image the catalog describes, one byte different:
	// what a mirror that serves subtly wrong bytes looks like. The length
	// must match deliberately, because a shorter body would be caught by
	// the short-read guard first and this test would never reach the
	// checksum comparison it exists to prove - that is the other
	// fixture, in TestEnsureFreeBSDImage_ShortReadIsReported.
	corrupt := append([]byte(nil), compressed...)
	corrupt[len(corrupt)/2] ^= 0xff
	if bytes.Equal(corrupt, compressed) {
		t.Fatal("the corrupted body is identical to the real one; the fixture would not test anything")
	}
	mirror.serve(corrupt, 0, 0)
	dir := t.TempDir()
	m := newFetchManager(t, dir, mirror)

	_, err := m.EnsureFreeBSDImage(context.Background(), testImageName, nil)
	if err == nil {
		t.Fatal("EnsureFreeBSDImage() = nil error, want a checksum-mismatch rejection")
	}
	if !strings.Contains(err.Error(), "sha256") {
		t.Errorf("error = %v, want it to name the checksum mismatch", err)
	}
	if strings.Contains(err.Error(), "short read") {
		t.Errorf("error = %v, want the checksum comparison reached, not the short-read guard: the fixture body must be exactly as long as the manifest says", err)
	}
	if entries := storeEntries(t, dir); len(entries) != 0 {
		t.Errorf("store dir holds %v, want nothing kept after a checksum mismatch", entries)
	}
}

func TestEnsureFreeBSDImage_ShortReadIsReported(t *testing.T) {
	compressed := xzCompress(t)
	mirror := newFakeMirror(t, testImageName, compressed)
	// The mirror sends half the image, chunked, then ends the response
	// cleanly - a truncated download that a checksum-only check would
	// report as a bare hash mismatch, naming neither the real cause nor
	// how many bytes actually arrived.
	mirror.serve(nil, 0, len(compressed)/2)
	dir := t.TempDir()
	m := newFetchManager(t, dir, mirror)

	_, err := m.EnsureFreeBSDImage(context.Background(), testImageName, nil)
	if err == nil {
		t.Fatal("EnsureFreeBSDImage() = nil error, want a short read to be rejected")
	}
	if !strings.Contains(err.Error(), "short read") {
		t.Errorf("error = %v, want it to name the short read", err)
	}
	if entries := storeEntries(t, dir); len(entries) != 0 {
		t.Errorf("store dir holds %v, want nothing kept after a short read", entries)
	}
}

func TestEnsureFreeBSDImage_HTTPErrorIsReported(t *testing.T) {
	compressed := xzCompress(t)
	mirror := newFakeMirror(t, testImageName, compressed)
	mirror.serve(nil, http.StatusNotFound, 0)
	dir := t.TempDir()
	m := newFetchManager(t, dir, mirror)

	_, err := m.EnsureFreeBSDImage(context.Background(), testImageName, nil)
	if err == nil {
		t.Fatal("EnsureFreeBSDImage() = nil error, want an HTTP 404 to be surfaced")
	}
	if !strings.Contains(err.Error(), "404") {
		t.Errorf("error = %v, want it to carry the HTTP status", err)
	}
	if entries := storeEntries(t, dir); len(entries) != 0 {
		t.Errorf("store dir holds %v, want nothing kept after a failed download", entries)
	}
}

func TestEnsureFreeBSDImage_RefusesWithoutEnoughFreeSpace(t *testing.T) {
	compressed := xzCompress(t)
	mirror := newFakeMirror(t, testImageName, compressed)
	m := newFetchManager(t, t.TempDir(), mirror)
	m.freeSpace = func(string) (int64, error) { return 16, nil }

	_, err := m.EnsureFreeBSDImage(context.Background(), testImageName, nil)
	if err == nil {
		t.Fatal("EnsureFreeBSDImage() = nil error, want a preflight refusal on a full disk")
	}
	if !strings.Contains(err.Error(), "not enough free space") {
		t.Errorf("error = %v, want it to explain the space shortfall", err)
	}
	if mirror.hitCount() != 0 {
		t.Errorf("mirror saw %d requests, want 0 - the preflight must refuse before any bytes are requested", mirror.hitCount())
	}
}

func TestEnsureFreeBSDImage_ProceedsWhenFreeSpaceIsUnknowable(t *testing.T) {
	compressed := xzCompress(t)
	mirror := newFakeMirror(t, testImageName, compressed)
	m := newFetchManager(t, t.TempDir(), mirror)
	m.freeSpace = func(string) (int64, error) { return 0, errUnsupportedSpace }

	if _, err := m.EnsureFreeBSDImage(context.Background(), testImageName, nil); err != nil {
		t.Fatalf("EnsureFreeBSDImage() error: %v, want an unknowable free space to be attempted, not refused", err)
	}
}

func TestEnsureFreeBSDImage_SurfacesAStatfsFailure(t *testing.T) {
	compressed := xzCompress(t)
	mirror := newFakeMirror(t, testImageName, compressed)
	m := newFetchManager(t, t.TempDir(), mirror)
	m.freeSpace = func(string) (int64, error) { return 0, errors.New("statfs exploded") }

	_, err := m.EnsureFreeBSDImage(context.Background(), testImageName, nil)
	if err == nil {
		t.Fatal("EnsureFreeBSDImage() = nil error, want a statfs failure surfaced rather than swallowed")
	}
	if !strings.Contains(err.Error(), "statfs exploded") {
		t.Errorf("error = %v, want the underlying cause preserved", err)
	}
}

func TestEnsureFreeBSDImage_RejectsUnknownName(t *testing.T) {
	compressed := xzCompress(t)
	mirror := newFakeMirror(t, testImageName, compressed)
	m := newFetchManager(t, t.TempDir(), mirror)

	_, err := m.EnsureFreeBSDImage(context.Background(), "FreeBSD-99.9-RELEASE-amd64-zfs.raw", nil)
	if err == nil {
		t.Fatal("EnsureFreeBSDImage() = nil error, want an unknown image to be rejected")
	}
	if !strings.Contains(err.Error(), "not a FreeBSD official image") || !strings.Contains(err.Error(), testImageName) {
		t.Errorf("error = %v, want it to say what went wrong and list the known images", err)
	}
}

func TestEnsureFreeBSDImage_RejectsUnsafeNamesBeforeAnyIO(t *testing.T) {
	compressed := xzCompress(t)
	mirror := newFakeMirror(t, testImageName, compressed)
	dir := t.TempDir()
	m := newFetchManager(t, dir, mirror)

	for _, name := range []string{"../escape.raw", "sub/dir.raw", "", ".", ".."} {
		if _, err := m.EnsureFreeBSDImage(context.Background(), name, nil); err == nil {
			t.Errorf("EnsureFreeBSDImage(%q) = nil error, want rejection", name)
		}
	}
	if mirror.hitCount() != 0 {
		t.Errorf("mirror saw %d requests, want 0 - an invalid name must be refused before any IO", mirror.hitCount())
	}
	if entries := storeEntries(t, dir); len(entries) != 0 {
		t.Errorf("store dir holds %v, want nothing written for a rejected name", entries)
	}
}

func TestEnsureFreeBSDImage_CancelledContextKeepsNothing(t *testing.T) {
	compressed := xzCompress(t)
	mirror := newFakeMirror(t, testImageName, compressed)
	dir := t.TempDir()
	m := newFetchManager(t, dir, mirror)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := m.EnsureFreeBSDImage(ctx, testImageName, nil); err == nil {
		t.Fatal("EnsureFreeBSDImage() = nil error, want a cancelled context to abort the fetch")
	}
	if entries := storeEntries(t, dir); len(entries) != 0 {
		t.Errorf("store dir holds %v, want nothing kept after a cancelled fetch", entries)
	}
}

// Two concurrent requests for the same image must download it once between
// them. Without the per-name lock both would see "not present", both would
// fetch, and one would rename over the other's temp file.
func TestEnsureFreeBSDImage_ConcurrentCallersDownloadOnce(t *testing.T) {
	compressed := xzCompress(t)
	mirror := newFakeMirror(t, testImageName, compressed)
	dir := t.TempDir()
	m := newFetchManager(t, dir, mirror)

	const callers = 4
	var wg sync.WaitGroup
	results := make([]*Info, callers)
	errs := make([]error, callers)
	start := make(chan struct{})
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			results[i], errs[i] = m.EnsureFreeBSDImage(context.Background(), testImageName, nil)
		}(i)
	}
	close(start)
	wg.Wait()

	for i := 0; i < callers; i++ {
		if errs[i] != nil {
			t.Errorf("caller %d error: %v", i, errs[i])
		}
	}
	if n := mirror.hitCount(); n != 1 {
		t.Errorf("mirror saw %d requests, want 1 across %d concurrent callers", n, callers)
	}
	for i := 1; i < callers; i++ {
		if *results[i] != *results[0] {
			t.Errorf("caller %d got %+v, want the same as caller 0 %+v", i, results[i], results[0])
		}
	}
	got, err := os.ReadFile(m.path(testImageName))
	if err != nil || !bytes.Equal(got, testImageBytes) {
		t.Errorf("stored image = %d bytes (err %v), want the full decompressed contents", len(got), err)
	}
}

func TestEnsureFreeBSDImage_RefetchesAnUnverifiedFile(t *testing.T) {
	compressed := xzCompress(t)
	mirror := newFakeMirror(t, testImageName, compressed)
	dir := t.TempDir()
	m := newFetchManager(t, dir, mirror)

	// A file under the right name with no sidecar: what an interrupted
	// fetch (or a hand-dropped image) leaves behind. It is not trusted.
	if err := os.WriteFile(filepath.Join(dir, testImageName), []byte("truncated half an image"), 0o644); err != nil {
		t.Fatalf("WriteFile() error: %v", err)
	}
	if present, _, _ := m.isComplete(testImageName); present {
		t.Error("isComplete() = true for a file with no recorded hash, want false")
	}

	info, err := m.EnsureFreeBSDImage(context.Background(), testImageName, nil)
	if err != nil {
		t.Fatalf("EnsureFreeBSDImage() error: %v", err)
	}
	if info.SHA256 != sha256Hex(string(testImageBytes)) {
		t.Errorf("Info.SHA256 = %s, want the re-fetched image's hash", info.SHA256)
	}
	if mirror.hitCount() != 1 {
		t.Errorf("mirror saw %d requests, want 1 - an unverified file must be re-fetched", mirror.hitCount())
	}
}

func TestIsComplete_RejectsACorruptSidecar(t *testing.T) {
	compressed := xzCompress(t)
	mirror := newFakeMirror(t, testImageName, compressed)
	dir := t.TempDir()
	m := newFetchManager(t, dir, mirror)

	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("MkdirAll() error: %v", err)
	}
	if err := os.WriteFile(m.path(testImageName), testImageBytes, 0o644); err != nil {
		t.Fatalf("WriteFile() error: %v", err)
	}
	if err := os.WriteFile(m.hashSidecarPath(testImageName), []byte("not a hash"), 0o644); err != nil {
		t.Fatalf("WriteFile() error: %v", err)
	}
	if present, _, _ := m.isComplete(testImageName); present {
		t.Error("isComplete() = true for a sidecar that isn't a hash, want false")
	}
}

func TestIsFreeBSDImage(t *testing.T) {
	m := New(t.TempDir())
	if !m.IsFreeBSDImage("FreeBSD-15.1-RELEASE-amd64-zfs.raw") {
		t.Error("IsFreeBSDImage() = false for a catalog image, want true")
	}
	if m.IsFreeBSDImage("ubuntu-24.04-server-cloudimg-amd64.img") {
		t.Error("IsFreeBSDImage() = true for an arbitrary uploaded image name, want false")
	}
	if m.IsFreeBSDImage("") {
		t.Error("IsFreeBSDImage(\"\") = true, want false")
	}
}

func TestFreeBSDImages_ReportsPresenceBothWays(t *testing.T) {
	compressed := xzCompress(t)
	mirror := newFakeMirror(t, testImageName, compressed)
	m := newFetchManager(t, t.TempDir(), mirror)

	before := m.FreeBSDImages()
	if len(before) != 1 {
		t.Fatalf("FreeBSDImages() = %+v, want exactly the one catalog entry", before)
	}
	if before[0].Present || before[0].LocalPath != "" {
		t.Errorf("FreeBSDImages()[0] = %+v, want Present=false before the fetch", before[0])
	}
	if before[0].RequiredBytes <= before[0].SizeBytes {
		t.Errorf("RequiredBytes = %d, want more than the %d-byte download to allow for decompression", before[0].RequiredBytes, before[0].SizeBytes)
	}

	if _, err := m.EnsureFreeBSDImage(context.Background(), testImageName, nil); err != nil {
		t.Fatalf("EnsureFreeBSDImage() error: %v", err)
	}

	after := m.FreeBSDImages()
	if len(after) != 1 || !after[0].Present {
		t.Fatalf("FreeBSDImages() = %+v, want the fetched image present", after)
	}
	if after[0].LocalPath != m.path(testImageName) {
		t.Errorf("LocalPath = %q, want %q", after[0].LocalPath, m.path(testImageName))
	}
	if after[0].Name != testImageName || after[0].Filesystem != "zfs" || after[0].Release != "15.1-RELEASE" {
		t.Errorf("FreeBSDImages()[0] = %+v, want the catalog's descriptive fields passed through", after[0])
	}
}

func TestDelete_RemovesAFetchedOfficialImage(t *testing.T) {
	compressed := xzCompress(t)
	mirror := newFakeMirror(t, testImageName, compressed)
	dir := t.TempDir()
	m := newFetchManager(t, dir, mirror)
	if _, err := m.EnsureFreeBSDImage(context.Background(), testImageName, nil); err != nil {
		t.Fatalf("EnsureFreeBSDImage() error: %v", err)
	}

	// A fetched image must be removable by the same call an uploaded one
	// is - nothing here is special-cased, which is the point of storing
	// it as a plain file.
	if err := m.Delete(testImageName); err != nil {
		t.Fatalf("Delete() error: %v", err)
	}
	if _, exists, _ := m.Path(testImageName); exists {
		t.Error("Path() = exists after Delete()")
	}
	if infos := m.FreeBSDImages(); infos[0].Present {
		t.Error("FreeBSDImages()[0].Present = true after Delete()")
	}
	if _, err := m.EnsureFreeBSDImage(context.Background(), testImageName, nil); err != nil {
		t.Fatalf("re-fetch after Delete() error: %v", err)
	}
}

func TestIsSHA256Hex(t *testing.T) {
	if !isSHA256Hex(sha256Hex("x")) {
		t.Error("isSHA256Hex(hash) = false, want true")
	}
	if !isSHA256Hex(strings.ToUpper(sha256Hex("x"))) {
		t.Error("isSHA256Hex(uppercase hash) = false, want true")
	}
	for _, s := range []string{"", "abc", strings.Repeat("g", 64), strings.Repeat("a", 63), strings.Repeat("a", 65)} {
		if isSHA256Hex(s) {
			t.Errorf("isSHA256Hex(%q) = true, want false", s)
		}
	}
}

func TestRequiredBytes_LeavesRoomForDecompression(t *testing.T) {
	img := freebsdimg.OfficialImage{SizeBytes: 1000}
	if got, want := requiredBytes(img), int64(1000+freebsdimg.EstimatedDecompressedSize(img)); got != want {
		t.Errorf("requiredBytes() = %d, want %d", got, want)
	}
	if freebsdimg.EstimatedDecompressedSize(img) <= img.SizeBytes {
		t.Error("EstimatedDecompressedSize() must exceed the compressed size, or the preflight reserves no room")
	}
	if got := requiredBytes(freebsdimg.OfficialImage{}); got != 0 {
		t.Errorf("requiredBytes(unknown size) = %d, want 0 (no preflight claim about a size nobody published)", got)
	}
}

func TestLockFetch_SerializesSameNameWithoutBlockingOthers(t *testing.T) {
	m := New(t.TempDir())

	// A different name must not contend: this is the property that keeps a
	// minutes-long fetch from stalling every other isostore operation.
	release := m.lockFetch("a")
	other := m.lockFetch("b")
	other()

	contended := make(chan struct{})
	go func() {
		m.lockFetch("a")()
		close(contended)
	}()
	select {
	case <-contended:
		t.Error("a second lock for the same name was taken while the first was held")
	case <-time.After(50 * time.Millisecond):
	}
	release()
	select {
	case <-contended:
	case <-time.After(5 * time.Second):
		t.Fatal("the second lock for the same name was never granted after the first was released")
	}
}

func TestLockFetch_IsReusableAfterRelease(t *testing.T) {
	m := New(t.TempDir())
	m.lockFetch("a")()
	// A leaked lock would wedge the second CreateVM for this image
	// forever, so re-acquire explicitly rather than trusting the happy path.
	m.lockFetch("a")()
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
