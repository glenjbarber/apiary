package isostore

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"

	"github.com/glenjbarber/apiary/internal/freebsdimg"
)

// errUnsupportedSpace is returned by the platform free-space probe on a
// platform that has none, and is deliberately not fatal: a fetch is better
// attempted than refused because this process couldn't ask a question.
var errUnsupportedSpace = errors.New("isostore: free space unavailable on this platform")

// FreeBSDImage is one entry of the FreeBSD official image catalog, together
// with what this node's store currently makes of it. It is the data the
// VM-creation form's base-image picker needs to offer an operator the
// official images as a choice without pretending they are already here.
type FreeBSDImage struct {
	// Name is the filename the image has (and will have) in the store,
	// and the value an operator picks, exactly like an uploaded image.
	Name string
	// Release, Architecture, Filesystem and CloudInit are the catalog's
	// descriptive fields, passed through from internal/freebsdimg.
	Release      string
	Architecture string
	Filesystem   string
	CloudInit    bool
	// SizeBytes is the published size of the compressed .raw.xz download.
	SizeBytes int64
	// RequiredBytes is the conservative free space one fetch needs:
	// the download plus EstimatedDecompressedSize of headroom. Shown so
	// an operator can see why the fetch was refused before it started.
	RequiredBytes int64
	// Present reports whether a complete, verified copy is already in
	// the store - see isComplete for exactly what that means.
	Present bool
	// LocalPath is the file's path in the store, empty when not Present.
	LocalPath string
}

// FreeBSDImages returns the whole official-image catalog with this node's
// presence resolved against the store directory. Every entry is returned,
// present or not: the point is to let an operator see what can be fetched,
// not only what has been.
func (m *Manager) FreeBSDImages() []FreeBSDImage {
	imgs := m.freebsd().Known()
	out := make([]FreeBSDImage, 0, len(imgs))
	for _, img := range imgs {
		out = append(out, m.describe(img))
	}
	return out
}

// describe builds a FreeBSDImage for one catalog entry, stat'ing the store
// to resolve presence.
func (m *Manager) describe(img freebsdimg.OfficialImage) FreeBSDImage {
	present, path, err := m.isComplete(img.Name)
	out := FreeBSDImage{
		Name:          img.Name,
		Release:       img.Release,
		Architecture:  img.Architecture,
		Filesystem:    img.Filesystem,
		CloudInit:     img.CloudInit,
		SizeBytes:     img.SizeBytes,
		RequiredBytes: requiredBytes(img),
		Present:       present && err == nil,
	}
	if out.Present {
		out.LocalPath = path
	}
	return out
}

// IsFreeBSDImage reports whether name is an official FreeBSD image this node
// knows how to fetch. It is the test the VM creation path uses to tell
// "fetch this for the operator" apart from "this must already be in the
// store, and the reconciler will fetch it from a peer if it isn't" - the
// same distinction an uploaded image's name falls on the other side of.
func (m *Manager) IsFreeBSDImage(name string) bool {
	_, ok := m.freebsd().Lookup(name)
	return ok
}

// EnsureFreeBSDImage makes the named official image available in the store,
// fetching, verifying and decompressing it if it isn't already there, and
// returns the store Info for it.
//
// A fetched image is indistinguishable from an uploaded one afterwards: same
// directory, same name, same ".sha256" sidecar written with the hash of the
// stored bytes, so List, Path, Delete, the reconciler's base-image resolution
// and ADR-0041's peer fetch all treat it as they would any other image, and
// the operator can re-fetch it on a second node by naming it. Only the bytes
// differ in provenance, and provenance is not replicated through raft
// anyway - this is physical, per-node data, like an uploaded ISO.
//
// Calling it again once the image is present is a no-op that neither
// downloads nor re-verifies anything, the same contract Save gives a second
// upload of the same name. progress, if non-nil, is called as bytes land
// across both phases of a real fetch; a no-op fetch calls it once with
// (RequiredBytes, RequiredBytes) so a caller driving a progress bar ends in
// the same state either way.
func (m *Manager) EnsureFreeBSDImage(ctx context.Context, name string, progress freebsdimg.ProgressFunc) (*Info, error) {
	if err := validateName(name); err != nil {
		return nil, err
	}
	img, ok := m.freebsd().Lookup(name)
	if !ok {
		return nil, fmt.Errorf("isostore: %q is not a FreeBSD official image (known images: %s)", name, strings.Join(m.freebsdNames(), ", "))
	}

	// One fetch per name at a time. A second caller for the same image
	// waits here and then finds the first one's work done, rather than
	// racing it to two full downloads writing the same destination. The
	// lock is per name and deliberately not a single store-wide mutex: a
	// fetch is minutes long, and serializing it against List or another
	// VM's unrelated image would stall unrelated work for the duration.
	unlock := m.lockFetch(name)
	defer unlock()

	// Re-checked under the lock, not before it: the caller that held the
	// lock may have been doing the download this one is about to skip.
	present, path, err := m.isComplete(name)
	if err != nil {
		return nil, err
	}
	if present {
		if progress != nil {
			total := requiredBytes(*img)
			progress(total, total)
		}
		return m.info(name, path)
	}

	if err := m.preflightFreeSpace(*img); err != nil {
		return nil, err
	}

	res, err := m.freebsd().Ensure(ctx, m.Dir, name, progress)
	if err != nil {
		return nil, fmt.Errorf("isostore: fetching official image %s: %w", name, err)
	}

	// The sidecar is what makes this image count as complete (see
	// isComplete), so it is written after the rename, never before: a
	// crash between the two leaves a file the store will re-fetch, which
	// is recoverable, whereas a sidecar without a file would be a lie.
	if err := os.WriteFile(m.hashSidecarPath(name), []byte(res.SHA256), 0o644); err != nil {
		return nil, fmt.Errorf("isostore: recording hash for %s: %w", name, err)
	}
	return m.info(name, res.Path)
}

// isComplete reports whether name is present in the store as a finished,
// verified file, and returns its path. A file with no readable ".sha256"
// sidecar is treated as not present: that is exactly the state a fetch
// interrupted between its final rename and its sidecar leaves behind (or a
// file somebody dropped in by hand), and re-fetching is the safe reading of
// it - a partially written image under a real name is precisely the failure
// this store exists to rule out.
func (m *Manager) isComplete(name string) (bool, string, error) {
	path := m.path(name)
	fi, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return false, path, nil
		}
		return false, path, fmt.Errorf("isostore: checking %s: %w", name, err)
	}
	if fi.IsDir() {
		return false, path, fmt.Errorf("isostore: %s is a directory, not an image", name)
	}
	sum, err := os.ReadFile(m.hashSidecarPath(name))
	if err != nil {
		if os.IsNotExist(err) {
			return false, path, nil
		}
		return false, path, fmt.Errorf("isostore: reading hash sidecar for %s: %w", name, err)
	}
	if !isSHA256Hex(strings.TrimSpace(string(sum))) {
		return false, path, nil
	}
	return true, path, nil
}

// info builds an Info for an already-stored file.
func (m *Manager) info(name, path string) (*Info, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("isostore: stat %s: %w", name, err)
	}
	sum, _ := os.ReadFile(m.hashSidecarPath(name))
	return &Info{Name: name, SizeBytes: fi.Size(), SHA256: strings.TrimSpace(string(sum)), ModTime: fi.ModTime()}, nil
}

// isSHA256Hex reports whether s looks like a 64-character hex digest - the
// only thing the store will accept as proof that a file's bytes were checked
// at some point (see Save, which refuses an unverified upload outright).
func isSHA256Hex(s string) bool {
	if len(s) != 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') && (c < 'A' || c > 'F') {
			return false
		}
	}
	return true
}

// preflightFreeSpace refuses a fetch that cannot possibly finish, before any
// bytes are requested. Discovering a full disk 20 minutes and 668MB into a
// download is a far worse answer than being told up front, and the store's
// own Save has no equivalent guard because an upload's size is whatever the
// operator's browser decided to send.
func (m *Manager) preflightFreeSpace(img freebsdimg.OfficialImage) error {
	avail, err := m.diskFreeSpace(m.Dir)
	if err != nil {
		if errors.Is(err, errUnsupportedSpace) {
			return nil
		}
		return fmt.Errorf("isostore: checking free space in %s: %w", m.Dir, err)
	}
	if need := requiredBytes(img); avail < need {
		return fmt.Errorf("isostore: not enough free space in %s for %s: need about %d bytes (the %d-byte download plus decompressed headroom), have %d", m.Dir, img.Name, need, img.SizeBytes, avail)
	}
	return nil
}

// requiredBytes is the free space one fetch of img conservatively needs: the
// compressed download, plus an estimate of what it expands to, since
// decompressing in place means both exist at once.
func requiredBytes(img freebsdimg.OfficialImage) int64 {
	return img.SizeBytes + freebsdimg.EstimatedDecompressedSize(img)
}

// freebsd returns this Manager's image fetcher, defaulting to the real
// catalog. Tests replace m.freebsd to point at an httptest server and a
// one-entry catalog; nothing in this repository ever reaches
// download.freebsd.org from a test.
func (m *Manager) freebsd() *freebsdimg.Manager {
	if m.freebsdImages == nil {
		return freebsdimg.New()
	}
	return m.freebsdImages
}

// freebsdNames lists the fetchable image names, for an error message that
// tells an operator what they could have picked instead.
func (m *Manager) freebsdNames() []string {
	catalog := m.freebsd().Known()
	names := make([]string, 0, len(catalog))
	for _, img := range catalog {
		names = append(names, img.Name)
	}
	return names
}

// lockFetch returns a function that releases the per-name fetch lock for
// name, taking it first.
func (m *Manager) lockFetch(name string) func() {
	m.fetchMu.Lock()
	if m.fetchLocks == nil {
		m.fetchLocks = map[string]*sync.Mutex{}
	}
	mu, ok := m.fetchLocks[name]
	if !ok {
		mu = &sync.Mutex{}
		m.fetchLocks[name] = mu
	}
	m.fetchMu.Unlock()

	mu.Lock()
	return mu.Unlock
}
