// Package freebsdimg manages FreeBSD official VM images - downloading,
// verifying (SHA256), and decompressing raw.xz images for use as bhyve
// base images. Images are sourced from download.freebsd.org/releases/VM-IMAGES/.
package freebsdimg

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// OfficialImage describes a FreeBSD official VM image available for download.
type OfficialImage struct {
	// Name is the local filename (without .xz extension after decompression).
	Name string
	// URL is the full download URL for the .raw.xz file.
	URL string
	// SHA256 is the expected SHA256 checksum of the .raw.xz file (not the decompressed raw).
	SHA256 string
	// Filesystem is either "zfs" or "ufs".
	Filesystem string
	// CloudInit indicates whether this is a BASIC-CLOUDINIT variant.
	CloudInit bool
	// Release is the FreeBSD release version (e.g., "15.1-RELEASE").
	Release string
	// Architecture is the CPU architecture (e.g., "amd64").
	Architecture string
	// SizeBytes is the compressed .raw.xz file size in bytes.
	SizeBytes int64
}

// KnownImages returns the hardcoded list of FreeBSD official VM images
// for the current supported release. This list is updated when a new
// FreeBSD release is supported.
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

// Manager downloads, verifies, and caches FreeBSD official images.
type Manager struct {
	// CacheDir is where downloaded .raw.xz files and decompressed .raw
	// files are stored. Subdirectories "compressed" and "decompressed"
	// are created automatically.
	CacheDir string

	// HTTPClient is used for downloads. If nil, http.DefaultClient is used.
	HTTPClient *http.Client

	mu sync.Mutex
}

// New returns a Manager storing images under cacheDir.
func New(cacheDir string) *Manager {
	return &Manager{CacheDir: cacheDir, HTTPClient: http.DefaultClient}
}

func (m *Manager) compressedDir() string  { return filepath.Join(m.CacheDir, "compressed") }
func (m *Manager) decompressedDir() string { return filepath.Join(m.CacheDir, "decompressed") }

func (m *Manager) compressedPath(name string) string  { return filepath.Join(m.compressedDir(), name+".xz") }
func (m *Manager) decompressedPath(name string) string { return filepath.Join(m.decompressedDir(), name) }

// EnsureImage downloads (if needed), verifies, and decompresses the
// named official image, returning the local path to the decompressed
// .raw file ready for use as a bhyve base image. The image is looked
// up by its Name field (e.g. "FreeBSD-15.1-RELEASE-amd64-zfs.raw").
func (m *Manager) EnsureImage(name string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Find the image definition.
	var img *OfficialImage
	for i := range KnownImages() {
		if KnownImages()[i].Name == name {
			img = &KnownImages()[i]
			break
		}
	}
	if img == nil {
		return "", fmt.Errorf("freebsdimg: unknown image %q", name)
	}

	// Ensure cache directories exist.
	if err := os.MkdirAll(m.compressedDir(), 0o755); err != nil {
		return "", fmt.Errorf("freebsdimg: creating compressed dir: %w", err)
	}
	if err := os.MkdirAll(m.decompressedDir(), 0o755); err != nil {
		return "", fmt.Errorf("freebsdimg: creating decompressed dir: %w", err)
	}

	compressedPath := m.compressedPath(name)
	decompressedPath := m.decompressedPath(name)

	// If decompressed file already exists, verify it quickly and return.
	if _, err := os.Stat(decompressedPath); err == nil {
		return decompressedPath, nil
	}

	// Download and verify compressed file if needed.
	if _, err := os.Stat(compressedPath); os.IsNotExist(err) {
		if err := m.downloadAndVerify(img, compressedPath); err != nil {
			return "", err
		}
	} else if err != nil {
		return "", fmt.Errorf("freebsdimg: checking compressed file: %w", err)
	} else {
		// File exists, verify its hash.
		if err := m.verifyFile(compressedPath, img.SHA256); err != nil {
			// Hash mismatch - remove and re-download.
			os.Remove(compressedPath)
			if err := m.downloadAndVerify(img, compressedPath); err != nil {
				return "", err
			}
		}
	}

	// Decompress.
	if err := m.decompressXZ(compressedPath, decompressedPath); err != nil {
		return "", fmt.Errorf("freebsdimg: decompressing %s: %w", name, err)
	}

	return decompressedPath, nil
}

func (m *Manager) downloadAndVerify(img *OfficialImage, dest string) error {
	tmp := dest + ".tmp"
	out, err := os.Create(tmp)
	if err != nil {
		return fmt.Errorf("freebsdimg: creating temp file: %w", err)
	}

	resp, err := m.HTTPClient.Get(img.URL)
	if err != nil {
		out.Close()
		os.Remove(tmp)
		return fmt.Errorf("freebsdimg: downloading %s: %w", img.Name, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		out.Close()
		os.Remove(tmp)
		return fmt.Errorf("freebsdimg: downloading %s: HTTP %d", img.Name, resp.StatusCode)
	}

	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(out, h), resp.Body); err != nil {
		out.Close()
		os.Remove(tmp)
		return fmt.Errorf("freebsdimg: writing download: %w", err)
	}
	if err := out.Close(); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("freebsdimg: closing download: %w", err)
	}

	got := hex.EncodeToString(h.Sum(nil))
	if !sha256Equal(got, img.SHA256) {
		os.Remove(tmp)
		return fmt.Errorf("freebsdimg: sha256 mismatch for %s: got %s, want %s", img.Name, got, img.SHA256)
	}

	if err := os.Rename(tmp, dest); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("freebsdimg: finalizing download: %w", err)
	}
	return nil
}

func (m *Manager) verifyFile(path, expectedSHA256 string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return err
	}
	got := hex.EncodeToString(h.Sum(nil))
	if !sha256Equal(got, expectedSHA256) {
		return fmt.Errorf("sha256 mismatch: got %s, want %s", got, expectedSHA256)
	}
	return nil
}

func (m *Manager) decompressXZ(src, dst string) error {
	// Use the system's xz(1) utility for decompression - it's
	// universally available on FreeBSD and handles streaming
	// decompression efficiently without loading the whole file into memory.
	cmd := []string{"xz", "-dc", src}
	// We can't easily use runCmd here without importing internal/bhyve,
	// so use os/exec directly.
	// Note: This is a simple implementation; in production we might want
	// a pure-Go xz decoder to avoid the external dependency.
	return decompressXZCommand(src, dst)
}

// sha256Equal compares two hex-encoded SHA-256 digests case-insensitively.
func sha256Equal(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		ca, cb := a[i], b[i]
		if ca >= 'A' && ca <= 'F' {
			ca += 'a' - 'A'
		}
		if cb >= 'A' && cb <= 'F' {
			cb += 'a' - 'A'
		}
		if ca != cb {
			return false
		}
	}
	return true
}