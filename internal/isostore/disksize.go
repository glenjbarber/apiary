package isostore

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

// ADR-0153: measuring an image's VIRTUAL disk size, and expanding a
// VM's own copy of it.
//
// The distinction this whole file exists for: an image's STORED size
// (how many bytes the upload occupies on disk) is not the same thing as
// its VIRTUAL size (how large the disk it contains is). For an
// uncompressed raw image the two happen to be equal; for a compressed
// upload they are not, and for a qcow2 image they are never equal,
// because the file is a container whose declared size lives inside it.
//
// Everything here therefore measures the UNCOMPRESSED payload, and
// refuses rather than guessing.

// DiskFormat is the on-disk format of a decompressed disk image.
type DiskFormat string

const (
	// DiskFormatRaw is a plain byte-for-byte disk image.
	DiskFormatRaw DiskFormat = "raw"

	// DiskFormatQcow2 is the qemu copy-on-write container format.
	DiskFormatQcow2 DiskFormat = "qcow2"
)

// qcow2Magic is the qcow2 file magic ("QFI\xfb"), at offset 0 of an
// uncompressed qcow2 file.
//
// It is checked here rather than delegating every format question to
// qemu-img, because qemu-img may not be installed on a node that only
// ever handles raw images, and a raw image must still be usable without
// it.
var qcow2Magic = []byte{'Q', 'F', 'I', 0xfb}

// maxVirtualSizeBytes is the largest virtual size this package will
// report or produce.
//
// It is the signed 64-bit file-offset limit: a size beyond it cannot be
// represented as a file length by any platform this project supports,
// so accepting one would produce a disk whose declared size no tool
// could agree on. The limit is enforced on the MEASURED size as well as
// on the requested one, because a measured size is not automatically a
// representable one.
const maxVirtualSizeBytes = int64(^uint64(0) >> 1)

// DetectDiskFormat reports the format of an already-decompressed disk
// image file.
//
// Only the formats this package can actually MEASURE and EXPAND are
// recognized. An unrecognized format is not silently treated as raw
// (ADR-0153's explicit requirement): guessing raw for, say, a VMDK
// would produce a disk whose "virtual size" is the container's byte
// length, which is meaningless.
func DetectDiskFormat(path string) (DiskFormat, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()

	header := make([]byte, 4)
	n, err := io.ReadFull(f, header)
	if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
		return "", err
	}
	if n >= 4 && string(header) == string(qcow2Magic) {
		return DiskFormatQcow2, nil
	}
	// Anything else is treated as raw. This is the one place a format is
	// assumed, and it is safe precisely because raw has no header to
	// misread: a raw image's virtual size IS its byte length, so
	// measuring it needs no format knowledge at all.
	return DiskFormatRaw, nil
}

// MeasureRawVirtualSize returns the virtual size of an uncompressed raw
// disk image: its byte length.
//
// It is measured rather than trusted from the stored size, because the
// stored size is the COMPRESSED upload's length for a compressed image.
func MeasureRawVirtualSize(path string) (int64, error) {
	info, err := os.Stat(path)
	if err != nil {
		return 0, err
	}
	size := info.Size()
	if size <= 0 {
		return 0, fmt.Errorf("isostore: image %q has a virtual size of %d bytes; a disk image must be larger than zero bytes", path, size)
	}
	if size > maxVirtualSizeBytes {
		return 0, fmt.Errorf("isostore: image %q has a virtual size of %d bytes, which is beyond the supported maximum of %d bytes", path, size, maxVirtualSizeBytes)
	}
	return size, nil
}

// MeasureQcow2VirtualSize returns the virtual size a qcow2 image
// declares, via `qemu-img info --output=json`.
//
// The declared size is the ONLY correct answer for a qcow2 container:
// the file's own byte length says nothing about how large the disk
// inside it is.
func MeasureQcow2VirtualSize(path string) (int64, error) {
	tool, err := exec.LookPath("qemu-img")
	if err != nil {
		return 0, fmt.Errorf("isostore: measuring a qcow2 image requires qemu-img, which was not found in PATH: %w", err)
	}
	out, err := exec.Command(tool, "info", "--output=json", path).Output()
	if err != nil {
		return 0, fmt.Errorf("isostore: qemu-img info failed for %q: %w", path, err)
	}
	var parsed struct {
		VirtualSize json.Number `json:"virtual-size"`
		Format      string      `json:"format"`
	}
	if err := json.Unmarshal(out, &parsed); err != nil {
		return 0, fmt.Errorf("isostore: parsing qemu-img info output for %q: %w", path, err)
	}
	if parsed.VirtualSize == "" {
		return 0, fmt.Errorf("isostore: qemu-img info reported no virtual size for %q", path)
	}
	size, err := strconv.ParseInt(parsed.VirtualSize.String(), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("isostore: qemu-img reported an unparseable virtual size %q for %q: %w", parsed.VirtualSize.String(), path, err)
	}
	if size <= 0 {
		return 0, fmt.Errorf("isostore: qemu-img reported a virtual size of %d bytes for %q; a disk image must be larger than zero bytes", size, path)
	}
	if size > maxVirtualSizeBytes {
		return 0, fmt.Errorf("isostore: qemu-img reported a virtual size of %d bytes for %q, which is beyond the supported maximum of %d bytes", size, path, maxVirtualSizeBytes)
	}
	return size, nil
}

// MeasureVirtualSize reports the virtual size of an UNCOMPRESSED disk
// image file, and the format it is in.
//
// The format is detected first, so an unsupported or unreadable format
// is refused rather than measured as if it were raw.
func MeasureVirtualSize(path string) (int64, DiskFormat, error) {
	format, err := DetectDiskFormat(path)
	if err != nil {
		return 0, "", err
	}
	switch format {
	case DiskFormatQcow2:
		size, err := MeasureQcow2VirtualSize(path)
		if err != nil {
			return 0, format, err
		}
		return size, format, nil
	default:
		size, err := MeasureRawVirtualSize(path)
		if err != nil {
			return 0, format, err
		}
		return size, format, nil
	}
}

// ExpandRawDisk expands a raw disk file to targetBytes.
//
// It refuses to SHRINK: a target smaller than the file's current
// length is an error, never a truncation. The file's current length is
// confirmed against expectedSourceBytes first, so a destination whose
// copy did not complete correctly is never expanded from the wrong
// base.
//
// The file is expanded with an ABSOLUTE size, not a relative one:
// `truncate -s <bytes>` sets the length to exactly that value, which
// is the operation ADR-0153 asks for and the one that cannot
// compound if it is somehow applied twice.
func ExpandRawDisk(path string, expectedSourceBytes, targetBytes int64) error {
	if targetBytes < 0 {
		return fmt.Errorf("isostore: refusing to expand %q to a negative size of %d bytes", path, targetBytes)
	}
	// The representable-range check lives in MibToBytes, which is
	// where a size ENTERS this package as a byte count. Here the value
	// is already an int64, so the only remaining checks are the
	// negative one above and the "never shrink" one below.
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	actual := info.Size()
	if expectedSourceBytes > 0 && actual != expectedSourceBytes {
		return fmt.Errorf("isostore: %q is %d bytes, but %d bytes were expected; refusing to expand a destination whose copy did not complete correctly", path, actual, expectedSourceBytes)
	}
	if targetBytes < actual {
		return fmt.Errorf("isostore: refusing to shrink %q from %d bytes to %d bytes", path, actual, targetBytes)
	}
	if targetBytes == actual {
		return nil
	}
	tool, err := exec.LookPath("truncate")
	if err != nil {
		return fmt.Errorf("isostore: expanding a raw disk requires truncate(1), which was not found in PATH: %w", err)
	}
	// The size argument is a decimal byte count, never an expression
	// and never a shell word: exec.Command passes it as a single
	// argument, so there is nothing for a shell to reinterpret.
	if err := exec.Command(tool, "-s", strconv.FormatInt(targetBytes, 10), path).Run(); err != nil {
		return fmt.Errorf("isostore: expanding %q to %d bytes with truncate(1): %w", path, targetBytes, err)
	}
	return nil
}

// ExpandQcow2Disk expands a qcow2 disk file to targetBytes.
//
// `qemu-img resize` takes an explicit byte size (a plain integer is
// bytes; the "K"/"M"/"G" suffixes are what this deliberately does NOT
// use, because a suffixed size is not a byte count and this package
// does all of its arithmetic in bytes).
func ExpandQcow2Disk(path string, expectedSourceBytes, targetBytes int64) error {
	if targetBytes < 0 {
		return fmt.Errorf("isostore: refusing to expand %q to a negative size of %d bytes", path, targetBytes)
	}
	if expectedSourceBytes > 0 {
		declared, err := MeasureQcow2VirtualSize(path)
		if err != nil {
			return err
		}
		if declared != expectedSourceBytes {
			return fmt.Errorf("isostore: %q declares a virtual size of %d bytes, but %d bytes were expected; refusing to expand a destination whose copy did not complete correctly", path, declared, expectedSourceBytes)
		}
	}
	tool, err := exec.LookPath("qemu-img")
	if err != nil {
		return fmt.Errorf("isostore: expanding a qcow2 image requires qemu-img, which was not found in PATH: %w", err)
	}
	if err := exec.Command(tool, "resize", path, strconv.FormatInt(targetBytes, 10)).Run(); err != nil {
		return fmt.Errorf("isostore: expanding %q to %d bytes with qemu-img: %w", path, targetBytes, err)
	}
	return nil
}

// VerifyVirtualSize confirms that a disk file's virtual size is
// EXACTLY targetBytes.
//
// It is checked after every expansion and before the disk is handed to
// bhyve: an expansion that reported success but did not take effect
// would otherwise produce a guest silently running on a disk of the
// wrong size.
func VerifyVirtualSize(path string, targetBytes int64) error {
	format, err := DetectDiskFormat(path)
	if err != nil {
		return err
	}
	var actual int64
	switch format {
	case DiskFormatQcow2:
		actual, err = MeasureQcow2VirtualSize(path)
	default:
		actual, err = MeasureRawVirtualSize(path)
	}
	if err != nil {
		return err
	}
	if actual != targetBytes {
		return fmt.Errorf("isostore: %q has a virtual size of %d bytes, but %d bytes were requested", path, actual, targetBytes)
	}
	return nil
}

// MibToBytes converts a size in MiB to a byte count, with checked
// arithmetic throughout.
//
// The multiplication is checked rather than done in plain int64
// arithmetic, because a MiB value that overflows would WRAP to a
// smaller number, and a wrapped size is a silently wrong disk. A
// negative value, a zero value, and a value beyond the supported
// signed file-offset range are all refused.
func MibToBytes(sizeMB uint64) (int64, error) {
	const bytesPerMiB = 1048576
	if sizeMB == 0 {
		return 0, fmt.Errorf("isostore: a size of 0 MiB is not a usable disk size")
	}
	if sizeMB > uint64(maxVirtualSizeBytes)/bytesPerMiB {
		return 0, fmt.Errorf("isostore: %d MiB is beyond the supported maximum disk size", sizeMB)
	}
	bytes := int64(sizeMB) * bytesPerMiB
	if bytes <= 0 {
		return 0, fmt.Errorf("isostore: %d MiB does not represent a usable byte count", sizeMB)
	}
	return bytes, nil
}

// BytesToMibRoundedUp converts a byte count to MiB, rounding UP.
//
// Rounding up is what makes the per-image default usable: a default
// equal to the image's own virtual size would leave an operator
// unable to add even one megabyte without editing it, and rounding
// DOWN would produce a default smaller than the image it describes -
// which ADR-0153 forbids outright.
func BytesToMibRoundedUp(sizeBytes int64) (uint64, error) {
	if sizeBytes <= 0 {
		return 0, fmt.Errorf("isostore: a virtual size of %d bytes does not represent a usable disk size", sizeBytes)
	}
	const bytesPerMiB = 1048576
	mib := uint64(sizeBytes) / bytesPerMiB
	if uint64(sizeBytes)%bytesPerMiB != 0 {
		mib++
	}
	if mib == 0 {
		return 0, fmt.Errorf("isostore: a virtual size of %d bytes rounds up to a zero-size disk", sizeBytes)
	}
	return mib, nil
}

// compressions lists the compression formats an upload may use, keyed
// by the magic bytes their streams start with.
//
// It mirrors internal/cluster's own compression detection table (the
// reconciler decompresses a base image the same way), because the two
// MUST agree: if the upload path measured one format and the
// reconciler decompressed another, the measured virtual size would
// describe a different payload than the one the VM actually receives.
var uploadCompressions = []struct {
	magic   []byte
	command string
	args    []string
}{
	{[]byte{0xfd, '7', 'z', 'X', 'Z', 0}, "xz", []string{"-dc", "--"}},
	{[]byte{0x1f, 0x8b}, "gzip", []string{"-dc", "--"}},
	{[]byte{'B', 'Z', 'h'}, "bzip2", []string{"-dc", "--"}},
	{[]byte{0x28, 0xb5, 0x2f, 0xfd}, "zstd", []string{"-q", "-d", "-c", "--"}},
}

// detectUploadCompression identifies the compression of a staged upload
// from its leading bytes.
func detectUploadCompression(path string) (string, []string, bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", nil, false, err
	}
	defer f.Close()
	header := make([]byte, 6)
	n, err := io.ReadFull(f, header)
	if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
		return "", nil, false, err
	}
	for _, candidate := range uploadCompressions {
		if bytes.HasPrefix(header[:n], candidate.magic) {
			return candidate.command, candidate.args, true, nil
		}
	}
	return "", nil, false, nil
}

// measureStagedUpload measures a staged upload's virtual size.
//
// The upload may be COMPRESSED, and the virtual size is the size of the
// DECOMPRESSED payload - the compressed length is never a substitute.
// The payload is therefore decompressed into a temporary staging file
// and measured there; the temporary file is removed on every path.
//
// A measurement failure is an upload REJECTION: the image's size is
// unknown, and an image of unknown size cannot be sized for a VM.
func (m *Manager) measureStagedUpload(stagedPath, name string) (imageMetadata, error) {
	command, args, compressed, err := detectUploadCompression(stagedPath)
	if err != nil {
		return imageMetadata{}, fmt.Errorf("isostore: inspecting the uploaded image %q: %w", name, err)
	}
	if !compressed {
		size, format, err := MeasureVirtualSize(stagedPath)
		if err != nil {
			return imageMetadata{}, fmt.Errorf("isostore: %s: the uploaded image %q could not be measured and was rejected", name, err)
		}
		return imageMetadata{VirtualSizeBytes: size, DiskFormat: normalizeFormatName(format)}, nil
	}

	// Decompress into a staging file beside the upload, so the
	// measurement reads the real payload rather than a pipe.
	tool, err := exec.LookPath(command)
	if err != nil {
		return imageMetadata{}, fmt.Errorf("isostore: the uploaded image %q is %s-compressed, but %s(1) was not found in PATH: %w", name, command, command, err)
	}
	decompressed, err := os.CreateTemp(m.Dir, ".measure-*")
	if err != nil {
		return imageMetadata{}, fmt.Errorf("isostore: creating a measurement staging file: %w", err)
	}
	decompressedPath := decompressed.Name()
	defer os.Remove(decompressedPath)

	cmd := exec.Command(tool, append(append([]string(nil), args...), stagedPath)...)
	cmd.Stdout = decompressed
	runErr := cmd.Run()
	closeErr := decompressed.Close()
	if runErr != nil {
		return imageMetadata{}, fmt.Errorf("isostore: the uploaded image %q could not be decompressed with %s(1) and was rejected: %w", name, command, runErr)
	}
	if closeErr != nil {
		return imageMetadata{}, fmt.Errorf("isostore: the uploaded image %q could not be decompressed with %s(1) and was rejected: %w", name, command, closeErr)
	}

	size, format, err := MeasureVirtualSize(decompressedPath)
	if err != nil {
		return imageMetadata{}, fmt.Errorf("isostore: %s: the uploaded image %q could not be measured and was rejected", name, err)
	}
	return imageMetadata{VirtualSizeBytes: size, DiskFormat: normalizeFormatName(format)}, nil
}

// formatMeasurementError renders a measurement failure with enough
// context for an operator to act on it.
func formatMeasurementError(name string, err error) string {
	return fmt.Sprintf("isostore: measuring the virtual size of %q failed: %v", name, err)
}

// measurementSummary renders a successful measurement for an operator-
// facing message.
func measurementSummary(name string, sizeBytes int64, format DiskFormat) string {
	return fmt.Sprintf("%s (%s, %d bytes)", name, format, sizeBytes)
}

// normalizeFormatName renders a DiskFormat for an operator-facing
// string, defaulting to "raw" for anything unrecognized.
func normalizeFormatName(format DiskFormat) string {
	if format == "" {
		return string(DiskFormatRaw)
	}
	return strings.ToLower(string(format))
}
