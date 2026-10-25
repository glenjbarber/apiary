package isostore

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// ADR-0153's per-image disk metadata: the image's measured VIRTUAL
// size, its DISK FORMAT, and its per-image DEFAULT disk size.
//
// It lives in its own sidecar file beside the image, rather than being
// derived on read, for two reasons:
//
//   - The virtual size of a COMPRESSED image cannot be derived from the
//     stored file at all: the stored file IS the compressed stream, and
//     measuring the decompressed payload means decompressing it, which
//     is exactly the work the upload path already did once and should
//     not redo on every listing.
//
//   - The per-image default is an OPERATOR CHOICE, not a measurement.
//     It has to be stored, or it could never be set.
//
// The sidecar is written atomically (temp file plus rename), so a
// crash mid-write can never leave a half-written metadata file that
// would then be read back as a corrupt default.

// imageMetadata is the on-disk shape of the sidecar.
type imageMetadata struct {
	// VirtualSizeBytes is the measured virtual size, in bytes. Zero
	// means unknown - the state of every image stored before ADR-0153.
	VirtualSizeBytes int64 `json:"virtual_size_bytes,omitempty"`

	// DiskFormat is the format of the decompressed payload: "raw" or
	// "qcow2".
	DiskFormat string `json:"disk_format,omitempty"`

	// DefaultSizeMB is the per-image default disk size, in MiB. Zero
	// means "no default recorded".
	DefaultSizeMB uint64 `json:"default_size_mb,omitempty"`
}

func (m *Manager) metadataSidecarPath(name string) string {
	return filepath.Join(m.Dir, ".metadata", name)
}

// writeMetadata stores an image's metadata sidecar.
func (m *Manager) writeMetadata(name string, meta imageMetadata) error {
	dir := filepath.Join(m.Dir, ".metadata")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	data, err := json.Marshal(meta)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".metadata-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpPath, m.metadataSidecarPath(name))
}

// readMetadata reads an image's metadata sidecar.
//
// A missing sidecar is NOT an error: it is exactly the state of an
// image stored before ADR-0153, and it means the virtual size is
// UNKNOWN. A sidecar that exists but cannot be parsed IS an error,
// because silently treating a corrupt sidecar as "no metadata" would
// discard an operator's chosen default without saying so.
func (m *Manager) readMetadata(name string) (imageMetadata, error) {
	data, err := os.ReadFile(m.metadataSidecarPath(name))
	if err != nil {
		if os.IsNotExist(err) {
			return imageMetadata{}, nil
		}
		return imageMetadata{}, err
	}
	var meta imageMetadata
	if err := json.Unmarshal(data, &meta); err != nil {
		return imageMetadata{}, fmt.Errorf("isostore: reading the metadata for %q: %w", name, err)
	}
	return meta, nil
}

// SetDefaultSizeMB sets an image's per-image default disk size, in
// MiB.
//
// The value may not be less than the image's virtual size: a default
// smaller than the image it describes would make every VM created from
// it smaller than the image, which ADR-0153 forbids outright.
//
// A zero value is refused rather than treated as "clear the default":
// there is exactly one way to express "no default recorded", and it is
// the absence of the field.
func (m *Manager) SetDefaultSizeMB(name string, sizeMB uint64) error {
	if sizeMB == 0 {
		return fmt.Errorf("isostore: a default disk size of 0 MB is not a usable value; omit the field to take the Colony's own floor instead")
	}
	if _, exists, err := m.Path(name); err != nil {
		return err
	} else if !exists {
		return fmt.Errorf("isostore: image %q does not exist", name)
	}
	meta, err := m.readMetadata(name)
	if err != nil {
		return err
	}
	if meta.VirtualSizeBytes > 0 {
		minimumMB, err := BytesToMibRoundedUp(meta.VirtualSizeBytes)
		if err != nil {
			return err
		}
		if sizeMB < minimumMB {
			return fmt.Errorf("isostore: image %q has a virtual size of %d MiB, so its default disk size cannot be less than %d MiB", name, minimumMB, minimumMB)
		}
	}
	meta.DefaultSizeMB = sizeMB
	return m.writeMetadata(name, meta)
}

// DefaultSizeMB returns an image's per-image default disk size, in
// MiB, and whether one was recorded.
func (m *Manager) DefaultSizeMB(name string) (uint64, bool, error) {
	meta, err := m.readMetadata(name)
	if err != nil {
		return 0, false, err
	}
	if meta.DefaultSizeMB == 0 {
		return 0, false, nil
	}
	return meta.DefaultSizeMB, true, nil
}

// VirtualSize returns an image's measured virtual size in bytes, and
// the format of its decompressed payload.
//
// A zero size with no error means UNKNOWN: the image was stored before
// ADR-0153, and must be measured before it can be selected for VM
// creation. It is never treated as a zero-byte disk.
func (m *Manager) VirtualSize(name string) (int64, string, error) {
	meta, err := m.readMetadata(name)
	if err != nil {
		return 0, "", err
	}
	return meta.VirtualSizeBytes, meta.DiskFormat, nil
}
