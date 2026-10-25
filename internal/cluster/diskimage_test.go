package cluster

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCopyDiskImageExpandsRecognizedFormatsByMagic(t *testing.T) {
	for _, tc := range []struct {
		name  string
		magic []byte
		tool  string
	}{
		{"xz", []byte{0xfd, '7', 'z', 'X', 'Z', 0}, "xz"},
		{"gzip", []byte{0x1f, 0x8b}, "gzip"},
		{"bzip2", []byte{'B', 'Z', 'h'}, "bzip2"},
		{"zstd", []byte{0x28, 0xb5, 0x2f, 0xfd}, "zstd"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			binDir := t.TempDir()
			writeFakeDecompressor(t, binDir, tc.tool, "#!/bin/sh\nprintf 'expanded image payload'\n")
			t.Setenv("PATH", binDir)
			src := filepath.Join(t.TempDir(), "uploaded-image")
			if err := os.WriteFile(src, append(append([]byte(nil), tc.magic...), []byte("compressed bytes")...), 0o600); err != nil {
				t.Fatal(err)
			}
			dst := filepath.Join(t.TempDir(), "disk.img")
			if err := copyDiskImage(context.Background(), dst, src); err != nil {
				t.Fatalf("copyDiskImage() error: %v", err)
			}
			got, err := os.ReadFile(dst)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != "expanded image payload" {
				t.Fatalf("disk contents = %q, want expanded payload", got)
			}
		})
	}
}

func TestEnsureDiskImageStreamsCompressedBaseImage(t *testing.T) {
	binDir := t.TempDir()
	writeFakeDecompressor(t, binDir, "xz", "#!/bin/sh\nprintf 'expanded base image'\n")
	t.Setenv("PATH", binDir)
	src := filepath.Join(t.TempDir(), "base.raw.xz")
	if err := os.WriteFile(src, []byte{0xfd, '7', 'z', 'X', 'Z', 0, 'x'}, 0o600); err != nil {
		t.Fatal(err)
	}
	path, err := (&Reconciler{}).ensureDiskImage(context.Background(), t.TempDir(), src, 0, 0, "")
	if err != nil {
		t.Fatalf("ensureDiskImage() error: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "expanded base image" {
		t.Fatalf("disk contents = %q, want decompressed image", got)
	}
}

func TestCopyDiskImageFailureRemovesPartialDestination(t *testing.T) {
	binDir := t.TempDir()
	writeFakeDecompressor(t, binDir, "xz", "#!/bin/sh\nprintf partial\nprintf 'invalid stream' >&2\nexit 1\n")
	t.Setenv("PATH", binDir)
	src := filepath.Join(t.TempDir(), "broken.raw.xz")
	if err := os.WriteFile(src, []byte{0xfd, '7', 'z', 'X', 'Z', 0, 'x'}, 0o600); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(t.TempDir(), "disk.img")
	err := copyDiskImage(context.Background(), dst, src)
	if err == nil || !strings.Contains(err.Error(), "invalid stream") {
		t.Fatalf("copyDiskImage() error = %v, want decompressor error", err)
	}
	if _, err := os.Stat(dst); !os.IsNotExist(err) {
		t.Fatalf("partial destination stat error = %v, want destination removed", err)
	}
}

func TestCopyDiskImageMissingDecompressorIsActionable(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	src := filepath.Join(t.TempDir(), "base.raw.xz")
	if err := os.WriteFile(src, []byte{0xfd, '7', 'z', 'X', 'Z', 0}, 0o600); err != nil {
		t.Fatal(err)
	}
	err := copyDiskImage(context.Background(), filepath.Join(t.TempDir(), "disk.img"), src)
	if err == nil || !strings.Contains(err.Error(), "install the xz package") {
		t.Fatalf("copyDiskImage() error = %v, want actionable missing-xz message", err)
	}
}

func TestValidateInstallerImageRejectsCompressedStreams(t *testing.T) {
	for _, tc := range []struct {
		name  string
		magic []byte
	}{
		{"xz", []byte{0xfd, '7', 'z', 'X', 'Z', 0}},
		{"gzip", []byte{0x1f, 0x8b}},
		{"bzip2", []byte{'B', 'Z', 'h'}},
		{"zstd", []byte{0x28, 0xb5, 0x2f, 0xfd}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "installer")
			if err := os.WriteFile(path, tc.magic, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := validateInstallerImage("installer", path); err == nil {
				t.Fatal("compressed installer unexpectedly accepted")
			}
		})
	}
	path := filepath.Join(t.TempDir(), "installer.iso")
	if err := os.WriteFile(path, []byte("plain installer"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := validateInstallerImage("installer.iso", path); err != nil {
		t.Fatalf("uncompressed installer rejected: %v", err)
	}
}

func writeFakeDecompressor(t *testing.T, dir, name, script string) {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
}
