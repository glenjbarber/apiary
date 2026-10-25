package isostore

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ADR-0153's measurement and expansion. What matters is the
// distinction the whole feature exists for: a stored upload's byte
// length is NOT a disk's virtual size, and a compressed upload's
// length is never a substitute for the decompressed payload's.

// writeFile writes a file with the given contents and returns its path.
func writeFile(t *testing.T, dir, name string, data []byte) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("writing %s: %v", name, err)
	}
	return path
}

func TestMeasureRawVirtualSizeIsTheByteLength(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, "disk.raw", make([]byte, 4096))

	size, err := MeasureRawVirtualSize(path)
	if err != nil {
		t.Fatalf("MeasureRawVirtualSize() error: %v", err)
	}
	if size != 4096 {
		t.Errorf("size = %d, want 4096", size)
	}
}

func TestMeasureRawVirtualSizeRefusesAZeroByteImage(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, "empty.raw", nil)

	if _, err := MeasureRawVirtualSize(path); err == nil {
		t.Fatal("a zero-byte image was measured successfully, want a refusal: a zero-byte disk is never a valid size")
	}
}

func TestDetectDiskFormatRecognizesQcow2(t *testing.T) {
	dir := t.TempDir()
	qcow2 := writeFile(t, dir, "disk.qcow2", append([]byte{'Q', 'F', 'I', 0xfb}, make([]byte, 100)...))
	raw := writeFile(t, dir, "disk.raw", make([]byte, 100))

	format, err := DetectDiskFormat(qcow2)
	if err != nil {
		t.Fatalf("DetectDiskFormat() error: %v", err)
	}
	if format != DiskFormatQcow2 {
		t.Errorf("format = %q, want %q", format, DiskFormatQcow2)
	}

	format, err = DetectDiskFormat(raw)
	if err != nil {
		t.Fatalf("DetectDiskFormat() error: %v", err)
	}
	if format != DiskFormatRaw {
		t.Errorf("format = %q, want %q", format, DiskFormatRaw)
	}
}

func TestMibToBytesRejectsTheBadValues(t *testing.T) {
	if _, err := MibToBytes(0); err == nil {
		t.Error("MibToBytes(0) was accepted, want a refusal")
	}
	// A value whose byte count would overflow int64 must be refused,
	// not silently wrapped to a smaller number.
	if _, err := MibToBytes(1 << 44); err == nil {
		t.Error("MibToBytes(1<<44) was accepted, want a refusal: the byte count overflows int64")
	}
	bytes, err := MibToBytes(20480)
	if err != nil {
		t.Fatalf("MibToBytes(20480) error: %v", err)
	}
	if bytes != 20480*1048576 {
		t.Errorf("bytes = %d, want %d", bytes, 20480*1048576)
	}
}

func TestBytesToMibRoundedUpRoundsUp(t *testing.T) {
	cases := []struct {
		bytes int64
		want  uint64
	}{
		{1048576, 1},
		{1048577, 2},
		{2097152, 2},
		{1, 1},
	}
	for _, tc := range cases {
		got, err := BytesToMibRoundedUp(tc.bytes)
		if err != nil {
			t.Fatalf("BytesToMibRoundedUp(%d) error: %v", tc.bytes, err)
		}
		if got != tc.want {
			t.Errorf("BytesToMibRoundedUp(%d) = %d, want %d", tc.bytes, got, tc.want)
		}
	}
	if _, err := BytesToMibRoundedUp(0); err == nil {
		t.Error("BytesToMibRoundedUp(0) was accepted, want a refusal")
	}
}

func TestSaveMeasuresAnUncompressedImagesVirtualSize(t *testing.T) {
	dir := t.TempDir()
	m := New(dir)

	data := make([]byte, 8192)
	sum := sha256.Sum256(data)
	info, err := m.Save("disk.raw", strings.NewReader(string(data)), hex.EncodeToString(sum[:]))
	if err != nil {
		t.Fatalf("Save() error: %v", err)
	}
	if info.VirtualSizeBytes != 8192 {
		t.Errorf("VirtualSizeBytes = %d, want 8192", info.VirtualSizeBytes)
	}
	if info.DiskFormat != "raw" {
		t.Errorf("DiskFormat = %q, want raw", info.DiskFormat)
	}
	// The stored file is still the ORIGINAL upload byte stream.
	stored, err := os.ReadFile(filepath.Join(dir, "disk.raw"))
	if err != nil {
		t.Fatalf("reading the stored image: %v", err)
	}
	if len(stored) != 8192 {
		t.Errorf("stored size = %d, want 8192", len(stored))
	}
}

func TestSaveMeasuresACompressedImagesDECOMPRESSEDSize(t *testing.T) {
	// A fake "xz" that produces a known-length payload, whatever it is
	// given.
	binDir := t.TempDir()
	payload := strings.Repeat("A", 4096)
	script := "#!/bin/sh\nprintf '%s' '" + payload + "'\n"
	if err := os.WriteFile(filepath.Join(binDir, "xz"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir)

	dir := t.TempDir()
	m := New(dir)

	// The upload's bytes are the xz magic plus filler: the STORED
	// length is small, and the DECOMPRESSED length is 4096.
	upload := append([]byte{0xfd, '7', 'z', 'X', 'Z', 0}, make([]byte, 32)...)
	sum := sha256.Sum256(upload)
	info, err := m.Save("disk.raw.xz", strings.NewReader(string(upload)), hex.EncodeToString(sum[:]))
	if err != nil {
		t.Fatalf("Save() error: %v", err)
	}
	if info.SizeBytes != int64(len(upload)) {
		t.Errorf("SizeBytes = %d, want %d (the stored upload's length)", info.SizeBytes, len(upload))
	}
	if info.VirtualSizeBytes != 4096 {
		t.Errorf("VirtualSizeBytes = %d, want 4096 (the DECOMPRESSED length, never the compressed length)", info.VirtualSizeBytes)
	}
}

func TestSaveRejectsAnUnmeasurableImage(t *testing.T) {
	// A fake "xz" that fails: the upload cannot be measured, so it is
	// REJECTED rather than stored with an unknown size.
	binDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(binDir, "xz"), []byte("#!/bin/sh\nexit 1\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir)

	dir := t.TempDir()
	m := New(dir)

	upload := append([]byte{0xfd, '7', 'z', 'X', 'Z', 0}, make([]byte, 32)...)
	sum := sha256.Sum256(upload)
	if _, err := m.Save("disk.raw.xz", strings.NewReader(string(upload)), hex.EncodeToString(sum[:])); err == nil {
		t.Fatal("an unmeasurable upload was accepted, want a rejection")
	}
	// And nothing was left behind.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.Name() == "disk.raw.xz" {
			t.Error("the rejected upload was left in the store")
		}
	}
}

func TestSetDefaultSizeMBRefusesBelowTheImageVirtualSize(t *testing.T) {
	dir := t.TempDir()
	m := New(dir)

	data := make([]byte, 4*1048576) // 4 MiB
	sum := sha256.Sum256(data)
	if _, err := m.Save("disk.raw", strings.NewReader(string(data)), hex.EncodeToString(sum[:])); err != nil {
		t.Fatalf("Save() error: %v", err)
	}

	// 4 MiB is exactly the image's size, so it is acceptable.
	if err := m.SetDefaultSizeMB("disk.raw", 4); err != nil {
		t.Errorf("SetDefaultSizeMB(4) error: %v", err)
	}
	// 3 MiB is below it, and is refused.
	if err := m.SetDefaultSizeMB("disk.raw", 3); err == nil {
		t.Error("SetDefaultSizeMB(3) was accepted, want a refusal: the default cannot be smaller than the image")
	}
	// Zero is not a usable size.
	if err := m.SetDefaultSizeMB("disk.raw", 0); err == nil {
		t.Error("SetDefaultSizeMB(0) was accepted, want a refusal")
	}
}

func TestVirtualSizeIsUnknownForAnImageStoredBeforeADR0153(t *testing.T) {
	dir := t.TempDir()
	m := New(dir)

	// A file dropped in by hand, with no metadata sidecar at all: the
	// state of every image stored before ADR-0153.
	writeFile(t, dir, "old.img", make([]byte, 1024))

	size, format, err := m.VirtualSize("old.img")
	if err != nil {
		t.Fatalf("VirtualSize() error: %v", err)
	}
	if size != 0 {
		t.Errorf("size = %d, want 0: an unmeasured image's size is UNKNOWN, not a zero-byte disk", size)
	}
	if format != "" {
		t.Errorf("format = %q, want empty for an unmeasured image", format)
	}
}

func TestExpandRawDiskGrowsTheFileAndNeverShrinksIt(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, "disk.raw", make([]byte, 4096))

	if err := ExpandRawDisk(path, 4096, 8192); err != nil {
		t.Fatalf("ExpandRawDisk() error: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() != 8192 {
		t.Errorf("size = %d, want 8192", info.Size())
	}

	// Shrinking is refused, never silently done.
	if err := ExpandRawDisk(path, 8192, 4096); err == nil {
		t.Error("ExpandRawDisk() shrank the disk, want a refusal")
	}
}

func TestExpandRawDiskRefusesAMismatchedSource(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, "disk.raw", make([]byte, 4096))

	// The destination is 4096 bytes but the source was expected to be
	// 8192: the copy did not complete correctly, and expanding it
	// would produce a disk whose contents are wrong.
	if err := ExpandRawDisk(path, 8192, 16384); err == nil {
		t.Fatal("ExpandRawDisk() expanded a destination whose copy did not complete, want a refusal")
	}
}

func TestVerifyVirtualSizeConfirmsTheExactSize(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, "disk.raw", make([]byte, 4096))

	if err := VerifyVirtualSize(path, 4096); err != nil {
		t.Errorf("VerifyVirtualSize() error: %v", err)
	}
	if err := VerifyVirtualSize(path, 8192); err == nil {
		t.Error("VerifyVirtualSize() accepted a mismatched size, want a refusal")
	}
}

func TestExpandRawDiskRefusesAnUnsupportedTargetSize(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, "disk.raw", make([]byte, 4096))

	if err := ExpandRawDisk(path, 4096, -1); err == nil {
		t.Error("ExpandRawDisk() accepted a negative target, want a refusal")
	}
	// A target at the very top of the representable range is accepted
	// by the arithmetic and refused (or attempted) by the tool; what
	// matters here is that the NEGATIVE case is refused before
	// anything is touched.
}

func TestSaveRejectsAZeroByteUploadAsAnUnmeasurableImage(t *testing.T) {
	dir := t.TempDir()
	m := New(dir)

	sum := sha256.Sum256(nil)
	if _, err := m.Save("empty.raw", strings.NewReader(""), hex.EncodeToString(sum[:])); err == nil {
		t.Fatal("a zero-byte upload was accepted, want a rejection: its virtual size cannot be measured")
	}
}

func TestMetadataSurvivesAListing(t *testing.T) {
	dir := t.TempDir()
	m := New(dir)

	data := make([]byte, 2*1048576)
	sum := sha256.Sum256(data)
	if _, err := m.Save("disk.raw", strings.NewReader(string(data)), hex.EncodeToString(sum[:])); err != nil {
		t.Fatalf("Save() error: %v", err)
	}
	if err := m.SetDefaultSizeMB("disk.raw", 64); err != nil {
		t.Fatalf("SetDefaultSizeMB() error: %v", err)
	}

	infos, err := m.List()
	if err != nil {
		t.Fatalf("List() error: %v", err)
	}
	if len(infos) != 1 {
		t.Fatalf("List() returned %d images, want 1", len(infos))
	}
	if infos[0].DefaultSizeMB != 64 {
		t.Errorf("DefaultSizeMB = %d, want 64", infos[0].DefaultSizeMB)
	}
	if infos[0].VirtualSizeBytes != 2*1048576 {
		t.Errorf("VirtualSizeBytes = %d, want %d", infos[0].VirtualSizeBytes, 2*1048576)
	}
}

func TestMeasureStagedUploadRefusesAnEmptyImage(t *testing.T) {
	dir := t.TempDir()
	m := New(dir)
	path := writeFile(t, dir, "empty.raw", nil)

	if _, err := m.measureStagedUpload(path, "empty.raw"); err == nil {
		t.Fatal("an empty image was measured successfully, want a refusal")
	} else if !strings.Contains(err.Error(), "rejected") {
		t.Errorf("refusal %q does not say the image was rejected", err.Error())
	}
}

func TestMeasureStagedUploadNamesTheImageInItsRefusal(t *testing.T) {
	dir := t.TempDir()
	m := New(dir)
	path := writeFile(t, dir, "empty.raw", nil)

	_, err := m.measureStagedUpload(path, "the-image.img")
	if err == nil {
		t.Fatal("an empty image was measured successfully, want a refusal")
	}
	if !strings.Contains(err.Error(), "the-image.img") {
		t.Errorf("refusal %q does not name the image", err.Error())
	}
	_ = fmt.Sprint(err)
}
