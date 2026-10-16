package cluster

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
)

type diskCompression struct {
	name        string
	command     string
	args        []string
	installHint string
}

var diskCompressions = []struct {
	magic []byte
	diskCompression
}{
	{[]byte{0xfd, '7', 'z', 'X', 'Z', 0}, diskCompression{name: "xz", command: "xz", args: []string{"-dc", "--"}, installHint: "install the xz package"}},
	{[]byte{0x1f, 0x8b}, diskCompression{name: "gzip", command: "gzip", args: []string{"-dc", "--"}, installHint: "install gzip"}},
	{[]byte{'B', 'Z', 'h'}, diskCompression{name: "bzip2", command: "bzip2", args: []string{"-dc", "--"}, installHint: "install bzip2"}},
	{[]byte{0x28, 0xb5, 0x2f, 0xfd}, diskCompression{name: "zstd", command: "zstd", args: []string{"-q", "-d", "-c", "--"}, installHint: "install the zstd package"}},
}

func detectDiskCompression(prefix []byte) (diskCompression, bool) {
	for _, candidate := range diskCompressions {
		if bytes.HasPrefix(prefix, candidate.magic) {
			return candidate.diskCompression, true
		}
	}
	return diskCompression{}, false
}

func detectDiskCompressionFile(path string) (diskCompression, bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return diskCompression{}, false, err
	}
	defer f.Close()
	var prefix [6]byte
	n, err := io.ReadFull(f, prefix[:])
	if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
		return diskCompression{}, false, err
	}
	compression, ok := detectDiskCompression(prefix[:n])
	return compression, ok, nil
}

func validateInstallerImage(name, path string) error {
	compression, compressed, err := detectDiskCompressionFile(path)
	if err != nil {
		return fmt.Errorf("checking image format: %w", err)
	}
	if compressed {
		return fmt.Errorf("VM installer image %q is compressed with %s; compressed images can be used as Base image, but installer media must be uploaded uncompressed", name, compression.name)
	}
	return nil
}

// copyDiskImage copies a stored image to dst, expanding supported compressed
// formats as it streams. The store keeps the original upload and its checksum;
// only a VM's newly-created disk receives the expanded bytes.
func copyDiskImage(ctx context.Context, dst, src string) (err error) {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	var prefix [6]byte
	n, readErr := io.ReadFull(in, prefix[:])
	if readErr != nil && readErr != io.EOF && readErr != io.ErrUnexpectedEOF {
		return readErr
	}
	compression, compressed := detectDiskCompression(prefix[:n])
	if !compressed {
		if _, err := in.Seek(0, io.SeekStart); err != nil {
			return fmt.Errorf("rewinding source image: %w", err)
		}
		return copyFile(dst, src)
	}

	command, err := exec.LookPath(compression.command)
	if err != nil {
		return fmt.Errorf("%s-compressed disk image requires %s(1), which was not found in PATH; %s: %w",
			compression.name, compression.command, compression.installHint, err)
	}
	args := append(append([]string(nil), compression.args...), src)
	cmd := exec.CommandContext(ctx, command, args...)
	stderr := &diskImageErrorBuffer{limit: 4096}
	cmd.Stderr = stderr

	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := out.Close(); err == nil && closeErr != nil {
			err = closeErr
		}
		if err != nil {
			_ = os.Remove(dst)
		}
	}()
	cmd.Stdout = out
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("decompressing %s image with %s(1): %w%s", compression.name, compression.command, err, stderr.suffix())
	}
	if err := out.Sync(); err != nil {
		return fmt.Errorf("syncing decompressed disk image: %w", err)
	}
	return nil
}

type diskImageErrorBuffer struct {
	limit int
	data  []byte
}

func (b *diskImageErrorBuffer) Write(p []byte) (int, error) {
	n := len(p)
	if len(b.data) < b.limit {
		remaining := b.limit - len(b.data)
		if len(p) > remaining {
			p = p[:remaining]
		}
		b.data = append(b.data, p...)
	}
	return n, nil
}

func (b *diskImageErrorBuffer) suffix() string {
	if len(b.data) == 0 {
		return ""
	}
	return ": " + string(bytes.TrimSpace(b.data))
}
