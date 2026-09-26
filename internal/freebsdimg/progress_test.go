package freebsdimg

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// The progress contract is this package's whole relationship with whatever
// draws the bar, and it is the one thing a caller cannot check for itself
// afterwards: a fetch is minutes long and reports only numbers. So it is
// tested here at the boundary, in the two forms that matter - the arithmetic
// (progressTracker) and a whole real fetch against an httptest server.

func TestProgressTracker_TotalIsFixedAcrossPhases(t *testing.T) {
	var totals []int64
	var counts []int64
	tr := newProgressTracker(func(fetched, total int64) {
		counts = append(counts, fetched)
		totals = append(totals, total)
	}, 1000)

	// Phase one: the download, 400 bytes.
	tr.add(0, 200)
	tr.add(0, 200)
	// Phase two: the decompression, which must continue from 400 rather
	// than restart. It overruns the estimate by 300 bytes, as a real image
	// that expands faster than the conservative 3x would.
	tr.add(400, 300)
	tr.add(400, 300)
	tr.add(400, 300)

	for i, total := range totals {
		if total != 1000 {
			t.Errorf("call %d reported total %d, want the fetch's single total 1000", i, total)
		}
	}
	for i := 1; i < len(counts); i++ {
		if counts[i] < counts[i-1] {
			t.Errorf("count went backwards: %d after %d", counts[i], counts[i-1])
		}
		if counts[i] > 1000 {
			t.Errorf("call %d reported %d of 1000; a progress bar must never overfill", i, counts[i])
		}
	}
	if last := counts[len(counts)-1]; last != 1000 {
		t.Errorf("final count = %d, want it clamped to the total 1000", last)
	}
}

func TestProgressTracker_AccumulatesUnevenChunks(t *testing.T) {
	// Neither io.Copy nor xz's output arrives in even chunks. A tracker
	// that reported offset+len per call instead of accumulating would drop
	// the reported count every time a chunk came out smaller than the one
	// before it, which is the one failure a progress bar cannot recover
	// from.
	var counts []int64
	tr := newProgressTracker(func(fetched, _ int64) { counts = append(counts, fetched) }, 1_000_000)

	tr.add(0, 4096)
	tr.add(0, 17) // a small trailing chunk
	tr.add(4113, 65536)
	tr.add(4113, 100)

	want := []int64{4096, 4113, 69649, 69749}
	if len(counts) != len(want) {
		t.Fatalf("counts = %v, want %v", counts, want)
	}
	for i := range want {
		if counts[i] != want[i] {
			t.Errorf("counts = %v, want %v", counts, want)
			break
		}
	}
}

func TestProgressTracker_CompleteEndsAtTotal(t *testing.T) {
	// The total is an estimate, so a fetch that turned out well under it
	// must still be reported as finished: this is the call that guarantees
	// a bar reaches full, and the one a wrong-looking estimate would
	// otherwise strand at 40%.
	var counts, totals []int64
	tr := newProgressTracker(func(fetched, total int64) {
		counts = append(counts, fetched)
		totals = append(totals, total)
	}, 10_000)

	tr.add(0, 300)
	tr.complete()

	if len(counts) != 2 {
		t.Fatalf("reported %d times, want 2 (one for the bytes, one to end it)", len(counts))
	}
	if last, total := counts[1], totals[1]; last != 10_000 || total != 10_000 {
		t.Errorf("final call = (%d, %d), want (10000, 10000)", last, total)
	}
}

func TestProgressTracker_UnknownTotalReportsTrueBytes(t *testing.T) {
	// A catalog entry with no published size has a total of 0. That is
	// "unknown", not "zero percent": the count must still advance, and the
	// second phase must still continue from the first.
	var counts []int64
	tr := newProgressTracker(func(fetched, total int64) {
		if total != 0 {
			t.Errorf("total = %d, want 0 for a catalog entry with no published size", total)
		}
		counts = append(counts, fetched)
	}, 0)

	tr.add(0, 700)
	tr.add(700, 9000)
	tr.complete()

	if len(counts) != 3 {
		t.Fatalf("reported %d times, want 3", len(counts))
	}
	if counts[0] != 700 || counts[1] != 9700 {
		t.Errorf("counts = %v, want the second phase to continue from the first ([700 9700])", counts)
	}
	if counts[2] != counts[1] {
		t.Errorf("final count = %d, want the last real count %d: with no total there is nothing to reach", counts[2], counts[1])
	}
}

func TestProgressTracker_NilFuncIsSafe(t *testing.T) {
	tr := newProgressTracker(nil, 1000)
	tr.add(0, 500)
	tr.add(500, 9000)
	tr.complete()
}

func TestProgressTracker_IgnoresNonPositiveCounts(t *testing.T) {
	calls := 0
	tr := newProgressTracker(func(int64, int64) { calls++ }, 1000)
	tr.add(0, 0)
	tr.add(0, -5)
	if calls != 0 {
		t.Errorf("reported %d times for a phase that wrote nothing, want 0", calls)
	}
}

// xzFixture compresses data with the same xz(1) the fetch path shells out
// to, skipping rather than reaching for the network or vendoring a decoder
// when it is not installed.
func xzFixture(t *testing.T, data []byte) []byte {
	t.Helper()
	if _, err := exec.LookPath("xz"); err != nil {
		t.Skip("xz(1) not installed; skipping (this is the same binary the fetch path requires)")
	}
	cmd := exec.Command("xz", "-c")
	cmd.Stdin = bytes.NewReader(data)
	var out, stderr bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("xz -c: %v: %s", err, stderr.String())
	}
	return out.Bytes()
}

// incompressible returns size bytes that xz cannot shrink, built by chaining
// SHA-256 digests so the fixture is deterministic. This is what a real
// FreeBSD raw.xz looks like from the fetch's point of view: the compressed
// image is roughly its own size, so the 3x decompressed estimate over-reserves
// and the real output lands well short of the total.
func incompressible(t *testing.T, size int) []byte {
	t.Helper()
	out := make([]byte, 0, size)
	var seed [32]byte
	for len(out) < size {
		sum := sha256.Sum256(seed[:])
		out = append(out, sum[:]...)
		copy(seed[:], sum[:])
	}
	return out[:size]
}

// serveImage starts an httptest server standing in for download.freebsd.org
// and returns a Manager that can only fetch that one image from it, expecting
// the digest of want. No test in this package can reach the real mirror.
func serveImage(t *testing.T, served, want []byte) *Manager {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write(served)
	}))
	t.Cleanup(server.Close)

	return &Manager{
		HTTPClient: server.Client(),
		Catalog: []OfficialImage{{
			Name:      "test.raw",
			URL:       server.URL + "/test.raw",
			SHA256:    hexSHA256(want),
			SizeBytes: int64(len(want)),
		}},
	}
}

// hexSHA256 is the hex SHA-256 of b, the form a catalog entry stores.
func hexSHA256(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func TestEnsure_ProgressEndsAtTotalWhenTheEstimateOverReserves(t *testing.T) {
	raw := incompressible(t, 64*1024)
	compressed := xzFixture(t, raw)
	m := serveImage(t, compressed, compressed)

	var totals []int64
	var counts []int64
	res, err := m.Ensure(context.Background(), t.TempDir(), "test.raw", func(fetched, total int64) {
		counts = append(counts, fetched)
		totals = append(totals, total)
	})
	if err != nil {
		t.Fatalf("Ensure() error: %v", err)
	}

	// The estimate is 3x, so a real (incompressible) image ends up well
	// short of the total. This is the case where only the final call can
	// complete the bar.
	want := int64(len(compressed)) + EstimatedDecompressedSize(m.Catalog[0])
	if totals[0] != want {
		t.Errorf("progress total = %d, want the fetch's own estimate %d", totals[0], want)
	}
	for i, total := range totals {
		if total != want {
			t.Errorf("call %d reported total %d, want it fixed at %d for the whole fetch", i, total, want)
		}
	}
	for i := 1; i < len(counts); i++ {
		if counts[i] < counts[i-1] {
			t.Errorf("count went backwards: %d after %d", counts[i], counts[i-1])
		}
		if counts[i] > want {
			t.Errorf("call %d reported %d of %d; a progress bar must never overfill", i, counts[i], want)
		}
	}
	if last := counts[len(counts)-1]; last != want {
		t.Errorf("final progress = %d of %d, want the bar to reach the total", last, want)
	}
	// The count must also have tracked real work, not jumped to the total
	// on the first call: a bar that reaches full instantly is the same lie
	// as one that never moves.
	if len(counts) < 3 {
		t.Errorf("progress reported %d times, want reports from the download and the decompression", len(counts))
	}
	if c := counts[0]; c == 0 || c == want {
		t.Errorf("first count = %d, want a real partial count", c)
	}
	if res.SizeBytes != int64(len(raw)) {
		t.Errorf("Result.SizeBytes = %d, want %d", res.SizeBytes, len(raw))
	}
}

func TestEnsure_LeavesNothingBehindOnACorruptDownload(t *testing.T) {
	compressed := xzFixture(t, []byte("the real image"))
	corrupt := append([]byte(nil), compressed...)
	corrupt[0] ^= 0xff
	m := serveImage(t, corrupt, compressed)
	dir := t.TempDir()

	_, err := m.Ensure(context.Background(), dir, "test.raw", nil)
	if err == nil {
		t.Fatal("Ensure() = nil error, want a checksum-mismatch rejection")
	}
	want := fmt.Sprintf("sha256 mismatch for test.raw: got %s, want %s",
		hexSHA256(corrupt), hexSHA256(compressed))
	if err.Error() != "freebsdimg: "+want {
		t.Errorf("error = %q, want %q", err, want)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir() error: %v", err)
	}
	if len(entries) != 0 {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("dir holds %v, want nothing kept after a checksum mismatch", names)
	}
}

func TestEnsure_RejectsUnsafeNamesBeforeAnyIO(t *testing.T) {
	m := serveImage(t, xzFixture(t, []byte("x")), xzFixture(t, []byte("x")))
	for _, name := range []string{"../escape.raw", "sub/dir.raw", "", ".", ".."} {
		if _, err := m.Ensure(context.Background(), t.TempDir(), name, nil); err == nil {
			t.Errorf("Ensure(%q) = nil error, want rejection", name)
		}
	}
}

func TestEnsure_RefusesAnImageThatIsNotInTheCatalog(t *testing.T) {
	m := serveImage(t, xzFixture(t, []byte("x")), xzFixture(t, []byte("x")))
	if _, err := m.Ensure(context.Background(), t.TempDir(), "something-else.raw", nil); err == nil {
		t.Error("Ensure() = nil error for an unknown name, want rejection")
	}
}

func TestEnsure_WritesOnlyTheFinalName(t *testing.T) {
	raw := incompressible(t, 16*1024)
	m := serveImage(t, xzFixture(t, raw), xzFixture(t, raw))
	dir := t.TempDir()

	res, err := m.Ensure(context.Background(), dir, "test.raw", nil)
	if err != nil {
		t.Fatalf("Ensure() error: %v", err)
	}
	if res.Path != filepath.Join(dir, "test.raw") {
		t.Errorf("Result.Path = %q, want it inside the caller's dir under the image's name", res.Path)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir() error: %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != "test.raw" {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("dir holds %v, want only the decompressed image - no compressed copy, no temps", names)
	}
	got, err := os.ReadFile(res.Path)
	if err != nil {
		t.Fatalf("ReadFile() error: %v", err)
	}
	if !bytes.Equal(got, raw) {
		t.Errorf("stored image is not the decompressed contents: %d bytes, want %d", len(got), len(raw))
	}
	if res.SHA256 != hexSHA256(raw) {
		t.Errorf("Result.SHA256 = %s, want the decompressed file's hash", res.SHA256)
	}
}

func TestEstimatedDecompressedSize(t *testing.T) {
	if got := EstimatedDecompressedSize(OfficialImage{}); got != 0 {
		t.Errorf("EstimatedDecompressedSize(no published size) = %d, want 0", got)
	}
	img := OfficialImage{SizeBytes: 1000}
	if got := EstimatedDecompressedSize(img); got != 3000 {
		t.Errorf("EstimatedDecompressedSize() = %d, want 3000", got)
	}
	// It must be an over-estimate of reality, since the preflight it backs
	// refuses a fetch it could have completed. FreeBSD's raw images expand
	// to roughly 1.5x; anything under 1x would refuse fetches that fit.
	if EstimatedDecompressedSize(img) <= img.SizeBytes {
		t.Error("EstimatedDecompressedSize() must exceed the compressed size, or the preflight reserves no room")
	}
}
