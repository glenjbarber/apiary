// Package isostore manages uploaded VM installer images (ISOs) as plain
// files under a configured base directory, on the local node - physical
// data, like a ZFS dataset or a bhyve disk image, never replicated
// through raft (see CLAUDE.md's physical/ephemeral distinction).
//
// Unlike internal/zfs/internal/jail/internal/bhyve, this package shells
// out to nothing - it's pure file I/O, so it needs no FreeBSD host to
// test against and has no integration-test/skip-if-missing-binary split
// the way those packages do.
package isostore

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// Info describes a stored ISO.
type Info struct {
	Name      string
	SizeBytes int64
	SHA256    string
	ModTime   time.Time
}

// Manager stores ISOs as files directly under Dir - no further
// namespacing, since (unlike jail/bhyve names) ISO filenames aren't a
// shared kernel-wide namespace that could collide with something
// outside Apiary's own management.
type Manager struct {
	Dir string
}

// New returns a Manager storing ISOs under dir.
func New(dir string) *Manager {
	return &Manager{Dir: dir}
}

// validateName rejects anything that could escape Dir or isn't a
// plausible ISO filename - the same safety property internal/zfs's
// path validation gives dataset names, applied here since Save writes
// directly to a filesystem path built from name.
func validateName(name string) error {
	if name == "" {
		return fmt.Errorf("isostore: name must not be empty")
	}
	if name != filepath.Base(name) || name == "." || name == ".." {
		return fmt.Errorf("isostore: invalid name %q: must be a plain filename, no path separators", name)
	}
	return nil
}

func (m *Manager) path(name string) string {
	return filepath.Join(m.Dir, name)
}

// Save streams r into the store under name, computing its SHA-256 as it
// writes. If the computed hash doesn't match expectedSHA256 (case-
// insensitive hex), the partially-written file is removed and an error
// is returned - the store never keeps a file whose hash wasn't
// confirmed, matching the "verify before keep" behavior of the web
// UI's own upload flow (see ADR-0017). expectedSHA256 must not be
// empty: an unverified upload is exactly the failure mode this package
// exists to prevent.
func (m *Manager) Save(name string, r io.Reader, expectedSHA256 string) (*Info, error) {
	if err := validateName(name); err != nil {
		return nil, err
	}
	if expectedSHA256 == "" {
		return nil, fmt.Errorf("isostore: expectedSHA256 must not be empty")
	}

	if err := os.MkdirAll(m.Dir, 0o755); err != nil {
		return nil, fmt.Errorf("isostore: creating store dir: %w", err)
	}

	// Write to a temp file first so a verification failure never leaves
	// a partial or wrongly-named file behind under the real name, and
	// so a concurrent Save of the same name can't observe a half-written
	// file via ListISOs/path lookups.
	tmp, err := os.CreateTemp(m.Dir, ".upload-*")
	if err != nil {
		return nil, fmt.Errorf("isostore: creating temp file: %w", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath) // no-op once successfully renamed

	h := sha256.New()
	size, err := io.Copy(io.MultiWriter(tmp, h), r)
	closeErr := tmp.Close()
	if err != nil {
		return nil, fmt.Errorf("isostore: writing upload: %w", err)
	}
	if closeErr != nil {
		return nil, fmt.Errorf("isostore: closing upload: %w", closeErr)
	}

	got := hex.EncodeToString(h.Sum(nil))
	if !sha256Equal(got, expectedSHA256) {
		return nil, fmt.Errorf("isostore: sha256 mismatch: got %s, want %s", got, expectedSHA256)
	}

	dest := m.path(name)
	if err := os.Rename(tmpPath, dest); err != nil {
		return nil, fmt.Errorf("isostore: finalizing upload: %w", err)
	}
	// Recorded once here rather than recomputed on every List() call -
	// re-hashing a multi-gigabyte ISO on every page load would make the
	// images list painfully slow for no benefit (see List's doc comment
	// on why this isn't treated as an integrity re-check).
	if err := os.WriteFile(m.hashSidecarPath(name), []byte(got), 0o644); err != nil {
		return nil, fmt.Errorf("isostore: recording hash: %w", err)
	}

	info, err := os.Stat(dest)
	if err != nil {
		return nil, fmt.Errorf("isostore: stat after save: %w", err)
	}
	return &Info{Name: name, SizeBytes: size, SHA256: got, ModTime: info.ModTime()}, nil
}

func (m *Manager) hashSidecarPath(name string) string {
	return m.path(name) + ".sha256"
}

// sha256Equal compares two hex-encoded SHA-256 digests case-
// insensitively - callers (a human pasting a hash, or a manifest file)
// shouldn't have to match FreeBSD/Go's lowercase-hex convention exactly.
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

// iso9660SniffOffset/iso9660Magic locate the "Standard Identifier" field
// of an ISO9660 Primary Volume Descriptor: sector 16 (2048 bytes each),
// plus 1 byte for the descriptor's own type field. This is the same
// check file(1)/libmagic use to recognize ISO9660 media.
const (
	iso9660SniffOffset = 16*2048 + 1
	iso9660Magic       = "CD001"
)

// IsISO9660 reports whether the stored image named name is a genuine
// ISO9660 filesystem, as opposed to some other kind of disk image -
// most notably a FreeBSD "memstick" image, which is a raw bootable
// MBR/GPT+UFS disk meant to be dd'd to a USB stick, not a CD-ROM
// filesystem at all. internal/cluster's Reconciler uses this to decide
// whether to attach a VM's install media via ahci-cd or ahci-hd -
// attaching a memstick image as a CD-ROM leaves firmware with no
// ISO9660 filesystem to find, so it never boots (confirmed live: a real
// VM attached this way sat at a blank screen indefinitely).
func (m *Manager) IsISO9660(name string) (bool, error) {
	if err := validateName(name); err != nil {
		return false, err
	}
	return isISO9660(m.path(name))
}

func isISO9660(path string) (bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer f.Close()

	buf := make([]byte, len(iso9660Magic))
	n, err := f.ReadAt(buf, iso9660SniffOffset)
	if err != nil && err != io.EOF {
		return false, err
	}
	return n == len(iso9660Magic) && string(buf) == iso9660Magic, nil
}

// Path returns the local filesystem path for name, and whether it
// exists - used by the reconciler to resolve a VM's iso_name into a
// path bhyve can boot from.
func (m *Manager) Path(name string) (string, bool, error) {
	if err := validateName(name); err != nil {
		return "", false, err
	}
	path := m.path(name)
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return "", false, nil
		}
		return "", false, err
	}
	return path, true, nil
}

// Delete removes name and its hash sidecar from the store. Deleting a
// name that doesn't exist is not an error - there's no external
// replicated record of an ISO's existence to keep consistent with
// (unlike a VM's tombstone in raft), so a simple idempotent delete is
// the right default here.
func (m *Manager) Delete(name string) error {
	if err := validateName(name); err != nil {
		return err
	}
	if err := os.Remove(m.path(name)); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("isostore: deleting %s: %w", name, err)
	}
	os.Remove(m.hashSidecarPath(name))
	return nil
}

// List returns every stored ISO, sorted by name. The hash shown is
// whatever Save recorded in that file's ".sha256" sidecar, read as-is -
// List does not re-hash file contents (see Save's doc comment on why:
// re-hashing a multi-gigabyte ISO on every page load isn't a cost worth
// paying just to detect on-disk corruption between uploads). A file
// with no sidecar (e.g. dropped into Dir by hand, not through Save) is
// still listed, with an empty SHA256.
func (m *Manager) List() ([]Info, error) {
	entries, err := os.ReadDir(m.Dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("isostore: listing %s: %w", m.Dir, err)
	}

	var infos []Info
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || filepath.Ext(name) == ".sha256" || filepath.Base(name)[0] == '.' {
			continue // skip directories, hash sidecars, and our own .upload-* temp files
		}
		fi, err := e.Info()
		if err != nil {
			continue
		}
		sum, _ := os.ReadFile(m.hashSidecarPath(name))
		infos = append(infos, Info{Name: name, SizeBytes: fi.Size(), SHA256: string(sum), ModTime: fi.ModTime()})
	}
	sort.Slice(infos, func(i, j int) bool { return infos[i].Name < infos[j].Name })
	return infos, nil
}
