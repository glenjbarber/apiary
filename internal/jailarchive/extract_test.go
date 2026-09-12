package jailarchive

import (
	"archive/tar"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func requireTar(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("tar"); err != nil {
		t.Skip("tar not available on this host")
	}
}

// writeTestArchive builds a plain (uncompressed) tar containing one
// file - tar(1) auto-detects the absence of compression exactly as it
// would detect xz on a real base.txz, so this exercises the same
// extraction path without needing an xz encoder in the test.
func writeTestArchive(t *testing.T, path string) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	tw := tar.NewWriter(f)
	content := []byte("#!/bin/sh\n")
	if err := tw.WriteHeader(&tar.Header{Name: "bin/sh", Mode: 0o755, Size: int64(len(content))}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(content); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestExtractor_Extract(t *testing.T) {
	requireTar(t)

	dir := t.TempDir()
	archivePath := filepath.Join(dir, "base.tar")
	writeTestArchive(t, archivePath)

	destDir := t.TempDir()
	e := New()
	if err := e.Extract(context.Background(), archivePath, destDir); err != nil {
		t.Fatalf("Extract: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(destDir, "bin", "sh"))
	if err != nil {
		t.Fatalf("reading extracted file: %v", err)
	}
	if string(got) != "#!/bin/sh\n" {
		t.Fatalf("extracted content = %q", got)
	}
}

func TestExtractor_Extract_MissingArchive(t *testing.T) {
	requireTar(t)

	destDir := t.TempDir()
	e := New()
	if err := e.Extract(context.Background(), filepath.Join(destDir, "no-such-archive.txz"), destDir); err == nil {
		t.Fatal("expected an error extracting a nonexistent archive, got nil")
	}
}
