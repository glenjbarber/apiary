// Package freebsdimg downloads, verifies (SHA-256) and decompresses FreeBSD
// official VM images - the raw.xz disk images published under
// download.freebsd.org/releases/VM-IMAGES/ - for use as bhyve base images.
//
// The package deliberately owns no directory layout of its own: it fetches
// into a caller-supplied directory, under a caller-supplied name, and does
// the temp-file/rename dance so the name only ever appears once its bytes
// are complete. internal/isostore is the caller that decides where an image
// lives and what "present" means (see internal/isostore/freebsd.go); this
// package stays pure byte-moving so it can be tested against an httptest
// server and a temp directory with no FreeBSD host, no network, and no
// dependency on any other Apiary package.
package freebsdimg

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// OfficialImage describes a FreeBSD official VM image available for download.
type OfficialImage struct {
	// Name is the local filename, without the .xz extension that is
	// downloaded - what the caller ends up calling the decompressed file.
	Name string
	// URL is the full download URL for the .raw.xz file.
	URL string
	// SHA256 is the expected SHA-256 checksum of the .raw.xz file, i.e.
	// of what comes down the wire, not of the decompressed raw image.
	// FreeBSD publishes these next to every image on every mirror, and
	// mirrors ship no detached GPG signature for them, so this hash is
	// the entire authenticity story - see the package doc comment.
	SHA256 string
	// Filesystem is either "zfs" or "ufs".
	Filesystem string
	// CloudInit reports whether this is a BASIC-CLOUDINIT variant, which
	// bhyve guests can consume a cloud-config drive on first boot.
	CloudInit bool
	// Release is the FreeBSD release version (e.g. "15.1-RELEASE").
	Release string
	// Architecture is the CPU architecture (e.g. "amd64").
	Architecture string
	// SizeBytes is the compressed .raw.xz file size in bytes, as
	// published in the release manifest. Used to report download
	// progress and to preflight free space; the actual received length
	// is checked against it.
	SizeBytes int64
}

// KnownImages returns the catalog of FreeBSD official VM images Apiary
// supports. The list is deliberately hardcoded rather than scraped from a
// release directory: the SHA-256 of every entry is baked in, and that hash
// is the only thing standing between an operator and a silently corrupted
// multi-gigabyte disk image. A scrape would have to be verified by
// something else first to be worth anything.
//
// A new release is added here, deliberately, by a human who has confirmed
// the published checksums. This list is the only place an official image is
// enumerated, so anything offering an operator a choice of them - the
// VM-creation form's base-image picker, via isostore.FreeBSDImages - must
// render from it rather than keep a list of its own. A picker that kept its
// own copy would silently offer a stale release, or offer one this package
// cannot fetch.
func KnownImages() []OfficialImage {
	return []OfficialImage{
		{
			Name:         "FreeBSD-15.1-RELEASE-amd64-zfs.raw",
			URL:          "https://download.freebsd.org/releases/VM-IMAGES/15.1-RELEASE/amd64/Latest/FreeBSD-15.1-RELEASE-amd64-zfs.raw.xz",
			SHA256:       "f026812a56222b2b8941caa88a405159cea81a5d213e0f9e10843057bbaff42e",
			Filesystem:   "zfs",
			CloudInit:    false,
			Release:      "15.1-RELEASE",
			Architecture: "amd64",
			SizeBytes:    668198388,
		},
		{
			Name:         "FreeBSD-15.1-RELEASE-amd64-ufs.raw",
			URL:          "https://download.freebsd.org/releases/VM-IMAGES/15.1-RELEASE/amd64/Latest/FreeBSD-15.1-RELEASE-amd64-ufs.raw.xz",
			SHA256:       "a8e2c0f8331be615ef008c167069076833a417e968ff6b7d8e83b4ac762e9409",
			Filesystem:   "ufs",
			CloudInit:    false,
			Release:      "15.1-RELEASE",
			Architecture: "amd64",
			SizeBytes:    666379980,
		},
		{
			Name:         "FreeBSD-15.1-RELEASE-amd64-BASIC-CLOUDINIT-zfs.raw",
			URL:          "https://download.freebsd.org/releases/VM-IMAGES/15.1-RELEASE/amd64/Latest/FreeBSD-15.1-RELEASE-amd64-BASIC-CLOUDINIT-zfs.raw.xz",
			SHA256:       "93011721f334015ce203d43c4d204e7cddc73dc6e3e757532fe67b740dc916b4",
			Filesystem:   "zfs",
			CloudInit:    true,
			Release:      "15.1-RELEASE",
			Architecture: "amd64",
			SizeBytes:    668323288,
		},
		{
			Name:         "FreeBSD-15.1-RELEASE-amd64-BASIC-CLOUDINIT-ufs.raw",
			URL:          "https://download.freebsd.org/releases/VM-IMAGES/15.1-RELEASE/amd64/Latest/FreeBSD-15.1-RELEASE-amd64-BASIC-CLOUDINIT-ufs.raw.xz",
			SHA256:       "926733d965078b5d635931eccc34022f74f803d35b79c05ccc2ec8a7f0ebcca2",
			Filesystem:   "ufs",
			CloudInit:    true,
			Release:      "15.1-RELEASE",
			Architecture: "amd64",
			SizeBytes:    666070680,
		},
	}
}

// Lookup returns the catalog entry named name. The returned value is a copy,
// so a caller cannot mutate the package-level catalog through it.
func Lookup(name string) (*OfficialImage, bool) {
	for _, img := range KnownImages() {
		if img.Name == name {
			found := img
			return &found, true
		}
	}
	return nil, false
}

// IsOfficial reports whether name is in KnownImages. Callers use it to tell
// "this operator asked for something we can fetch for them" apart from "this
// operator named an image that must already be in the store".
func IsOfficial(name string) bool {
	_, ok := Lookup(name)
	return ok
}

// Result describes an image materialized on disk by Ensure.
type Result struct {
	// Path is the final path of the decompressed raw image, inside the
	// directory the caller passed to Ensure.
	Path string
	// SizeBytes is the size of the decompressed file.
	SizeBytes int64
	// SHA256 is the SHA-256 of the decompressed file - not of the
	// downloaded .raw.xz, which was verified against the catalog before
	// decompression began.
	SHA256 string
}

// ProgressFunc is called as bytes land on disk: fetched is the number of
// bytes written so far, counted across both of a fetch's phases - the
// compressed download first, then the decompressed output, which picks the
// count up where the download left it - and total is the same number on
// every call of a given fetch.
//
// The unit is the fetch's on-disk footprint, not the download alone,
// because the decompression is the longer half of the work: a bar that hit
// 100% the moment the .raw.xz landed would claim the fetch was finished
// while xz(1) was still writing the image that will actually be used.
//
// That total is an estimate, and deliberately a generous one (see
// EstimatedDecompressedSize), so the reported count does not necessarily
// reach it in real bytes; a successful fetch therefore ends with one final
// (total, total) call, and fetched is never allowed past total, so a caller
// can draw a bar from the numbers alone without inventing its own scale. A
// total of 0 means the catalog published no size for the image: fetched then
// reports true bytes and the total is to be read as unknown, not as zero
// percent.
//
// It may be called from the fetching goroutine and must not block. A nil
// ProgressFunc means "don't bother reporting".
type ProgressFunc func(fetched, total int64)

// progressTracker owns the one total reported for a whole fetch, and the
// arithmetic every phase goes through to keep that total honest: a total
// that does not change mid-fetch, a count that accumulates across both
// phases, and a count that never passes the total. A tracker with a nil fn
// is a no-op, so the no-progress case needs no separate code path anywhere
// below.
type progressTracker struct {
	fn    ProgressFunc
	total int64
	// counted is the real number of bytes counted so far, across every
	// phase; reported is the last value handed to fn. They differ because
	// the reported value is clamped, and a clamp must not stop the count
	// from tracking real work.
	counted  int64
	reported int64
}

func newProgressTracker(fn ProgressFunc, total int64) *progressTracker {
	return &progressTracker{fn: fn, total: total}
}

// add counts n further bytes of a phase that began at offset - the number of
// bytes already counted before this phase started. The download phase passes
// 0; the decompression phase passes the number of bytes the download actually
// wrote, so the count cannot restart at the phase boundary. offset is
// advisory: a caller that got it wrong still cannot make the reported count
// go backwards, which is the property a progress bar cannot recover from.
func (t *progressTracker) add(offset, n int64) {
	if t.fn == nil || n <= 0 {
		return
	}
	t.counted = max(t.counted, offset) + n
	got := t.counted
	if t.total > 0 {
		// Clamped, because the estimate is generous by design: a real
		// image that comes in under it must not report over 100%.
		got = min(got, t.total)
	}
	if got <= t.reported {
		return // A count that is not new is not progress, and repeating or
		// lowering one would make a bar jitter or run backwards.
	}
	t.reported = got
	t.fn(got, t.total)
}

// complete ends the sequence. Because the total is an estimate, this call is
// what makes a bar end full however far off the estimate turned out to be;
// with no published size there is no estimate to reach and the last real
// count stands.
func (t *progressTracker) complete() {
	if t.fn == nil {
		return
	}
	if t.total > 0 {
		t.reported = t.total
	}
	t.fn(t.reported, t.total)
}

// Manager fetches official images. The zero value is usable; New returns one
// pre-populated with the default HTTP client and the full catalog.
//
// HTTPClient and Catalog are exported so a test can point Manager at an
// httptest server and a one-entry catalog instead of the real
// download.freebsd.org - no test in this repository touches the network.
type Manager struct {
	// HTTPClient is used for downloads. If nil, http.DefaultClient is used.
	HTTPClient *http.Client
	// Catalog is the set of images this Manager knows how to fetch. If
	// nil or empty, KnownImages() is used.
	Catalog []OfficialImage
}

// New returns a Manager using http.DefaultClient and the full catalog.
func New() *Manager {
	return &Manager{HTTPClient: http.DefaultClient}
}

func (m *Manager) httpClient() *http.Client {
	if m.HTTPClient != nil {
		return m.HTTPClient
	}
	return http.DefaultClient
}

func (m *Manager) catalog() []OfficialImage {
	if len(m.Catalog) > 0 {
		return m.Catalog
	}
	return KnownImages()
}

// Known returns the effective catalog - m.Catalog when set, else the full
// KnownImages. Exposed so a caller can list what it is able to fetch
// without duplicating the nil-means-default rule.
func (m *Manager) Known() []OfficialImage {
	return m.catalog()
}

// Lookup returns this Manager's catalog entry named name.
func (m *Manager) Lookup(name string) (*OfficialImage, bool) {
	for _, img := range m.catalog() {
		if img.Name == name {
			found := img
			return &found, true
		}
	}
	return nil, false
}

// EstimatedDecompressedSize returns a conservative upper-ish estimate of the
// decompressed size of img, given only its compressed size. The real figure
// is not knowable without parsing the .xz container's LZMA2 index (and is not
// published in the release manifest), so callers that must reserve space use
// this instead - the ratio is deliberately far above the ~1.5x that FreeBSD's
// raw images actually compress to, so a preflight built on it errs towards
// attempting the fetch rather than towards refusing one that would have fit.
func EstimatedDecompressedSize(img OfficialImage) int64 {
	if img.SizeBytes <= 0 {
		return 0
	}
	return img.SizeBytes * decompressedSizeRatio
}

// decompressedSizeRatio is the multiplier behind EstimatedDecompressedSize.
const decompressedSizeRatio = 3

// Ensure downloads, verifies and decompresses the named catalog image into
// dir, and returns where the decompressed raw image landed. dir is created
// if it does not exist.
//
// The final name is only ever created by an atomic rename from a temp file in
// the same directory, so a concurrent reader - or the reconciler, resolving a
// VM's base_image_name through internal/isostore - can never observe a
// half-written image under the real name, however the fetch ends. Every
// failure path (bad HTTP status, short body, checksum mismatch, xz failure,
// cancelled context) removes both the partial download and the partial output
// before returning.
//
// progress, if non-nil, is called for the whole fetch under a single total -
// the download plus the decompressed estimate, in bytes of on-disk footprint
// (see ProgressFunc) - and is called one last time with (total, total) once
// the image is in place under its final name.
//
// Ensure does not check whether dst already exists; deciding that a fetch is
// unnecessary (and skipping it) is the caller's policy, because only the
// caller knows what "already here" should mean. internal/isostore uses the
// recorded-hash sidecar to answer that.
func (m *Manager) Ensure(ctx context.Context, dir, name string, progress ProgressFunc) (*Result, error) {
	img, ok := m.Lookup(name)
	if !ok {
		return nil, fmt.Errorf("freebsdimg: unknown image %q", name)
	}
	if strings.ContainsRune(name, filepath.Separator) || name != filepath.Base(name) || name == "." || name == ".." {
		return nil, fmt.Errorf("freebsdimg: invalid image name %q: must be a plain filename, no path separators", name)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("freebsdimg: creating destination dir: %w", err)
	}

	dest := filepath.Join(dir, name)
	// One tracker, one total, both phases: the invariant that a fetch's
	// progress never changes unit is enforced by construction here rather
	// than by each call site remembering to agree.
	track := newProgressTracker(progress, img.SizeBytes+EstimatedDecompressedSize(*img))

	// Temp names follow internal/isostore's own ".upload-*" convention:
	// dot-prefixed, so the store's List never shows a half-written file.
	download, err := os.CreateTemp(dir, ".fetch-*.xz")
	if err != nil {
		return nil, fmt.Errorf("freebsdimg: creating temp download: %w", err)
	}
	downloadPath := download.Name()
	// Both temps are removed on every path that doesn't reach the final
	// rename; the rename of the decompressed file is the last statement
	// that can fail, and the download is deleted before that point.
	defer os.Remove(downloadPath)

	compressed, err := m.download(ctx, *img, download, track)
	if err != nil {
		download.Close()
		return nil, err
	}
	if err := download.Close(); err != nil {
		return nil, fmt.Errorf("freebsdimg: closing download of %s: %w", name, err)
	}

	out, err := os.CreateTemp(dir, ".fetch-*.raw")
	if err != nil {
		return nil, fmt.Errorf("freebsdimg: creating temp output: %w", err)
	}
	outPath := out.Name()
	defer os.Remove(outPath) // no-op once successfully renamed

	// The count the decompression phase starts from is the number of
	// bytes actually on disk, not the size the manifest published, so the
	// two phases cannot disagree by even one byte if a mirror's content
	// length and the manifest ever do.
	size, sum, err := decompressXZ(ctx, downloadPath, out, track, compressed)
	if err != nil {
		out.Close()
		return nil, fmt.Errorf("freebsdimg: decompressing %s: %w", name, err)
	}
	if err := out.Close(); err != nil {
		return nil, fmt.Errorf("freebsdimg: closing decompressed %s: %w", name, err)
	}

	// The compressed copy has served its purpose (it was verified, and the
	// decompressed bytes are on disk) - drop it before the rename rather
	// than after, so a disk that was only just big enough never has to
	// hold both the compressed and decompressed image plus the new file.
	os.Remove(downloadPath)

	if err := os.Rename(outPath, dest); err != nil {
		return nil, fmt.Errorf("freebsdimg: finalizing %s: %w", name, err)
	}
	// Only now, with the image in place under its real name: a bar that
	// reached full before the rename would be reporting a fetch that had
	// not actually happened yet.
	track.complete()
	return &Result{Path: dest, SizeBytes: size, SHA256: sum}, nil
}

// download streams img into out, hashing as it goes, and refuses anything
// that isn't the exact byte sequence the catalog's SHA-256 describes. It
// returns the number of bytes written, which is what the decompression phase
// counts on from.
func (m *Manager) download(ctx context.Context, img OfficialImage, out io.Writer, track *progressTracker) (int64, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, img.URL, nil)
	if err != nil {
		return 0, fmt.Errorf("freebsdimg: building request for %s: %w", img.Name, err)
	}
	resp, err := m.httpClient().Do(req)
	if err != nil {
		return 0, fmt.Errorf("freebsdimg: downloading %s: %w", img.Name, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("freebsdimg: downloading %s: HTTP %d from %s", img.Name, resp.StatusCode, img.URL)
	}

	h := sha256.New()
	// The same single total as the decompression phase, deliberately: the
	// download reports how much of the whole fetch's footprint has landed
	// so far, not how much of the download. See ProgressFunc.
	written, err := io.Copy(io.MultiWriter(out, h), &countingReader{r: resp.Body, track: track})
	if err != nil {
		return written, fmt.Errorf("freebsdimg: writing %s: %w", img.Name, err)
	}

	// A mirror that answers 200 with a truncated body - a cut connection
	// dressed up as a clean end of stream, which io.Copy cannot tell from
	// a genuinely complete file - is caught here rather than by the
	// checksum below, whose message would be far less actionable.
	if img.SizeBytes > 0 && written != img.SizeBytes {
		return written, fmt.Errorf("freebsdimg: short read of %s: got %d bytes, manifest says %d", img.Name, written, img.SizeBytes)
	}

	if got := hex.EncodeToString(h.Sum(nil)); !strings.EqualFold(got, img.SHA256) {
		return written, fmt.Errorf("freebsdimg: sha256 mismatch for %s: got %s, want %s", img.Name, got, img.SHA256)
	}
	return written, nil
}

// countingReader reports every byte read from r to the fetch's progress
// tracker. It deliberately has no WriteTo method, so io.Copy uses its own
// buffered loop over this rather than any fast path the wrapped reader might
// prefer.
type countingReader struct {
	r     io.Reader
	track *progressTracker
}

func (c *countingReader) Read(b []byte) (int, error) {
	n, err := c.r.Read(b)
	c.track.add(0, int64(n))
	return n, err
}
