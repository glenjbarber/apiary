package isostore

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func sha256Hex(data string) string {
	sum := sha256.Sum256([]byte(data))
	return hex.EncodeToString(sum[:])
}

func TestSave_CorrectHashSucceeds(t *testing.T) {
	m := New(t.TempDir())
	data := "fake iso contents"

	info, err := m.Save("test.iso", strings.NewReader(data), sha256Hex(data))
	if err != nil {
		t.Fatalf("Save() error: %v", err)
	}
	if info.Name != "test.iso" || info.SizeBytes != int64(len(data)) {
		t.Errorf("Save() info = %+v, want Name=test.iso SizeBytes=%d", info, len(data))
	}
	if info.SHA256 != sha256Hex(data) {
		t.Errorf("Save() SHA256 = %s, want %s", info.SHA256, sha256Hex(data))
	}

	path, exists, err := m.Path("test.iso")
	if err != nil || !exists {
		t.Fatalf("Path() = (%q, %v, %v), want it to exist", path, exists, err)
	}
	got, err := os.ReadFile(path)
	if err != nil || string(got) != data {
		t.Errorf("stored file contents = %q, %v, want %q", got, err, data)
	}
}

func TestSave_MismatchedHashIsRejectedAndCleanedUp(t *testing.T) {
	dir := t.TempDir()
	m := New(dir)

	_, err := m.Save("test.iso", strings.NewReader("real contents"), sha256Hex("wrong expected contents"))
	if err == nil {
		t.Fatalf("Save() = nil error, want a hash-mismatch rejection")
	}

	if _, exists, _ := m.Path("test.iso"); exists {
		t.Errorf("Path() = exists, want the mismatched upload to not be kept")
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if e.Name() == "test.iso" {
			t.Errorf("test.iso should not exist in the store dir after a hash mismatch")
		}
	}
}

func TestSave_HashComparisonIsCaseInsensitive(t *testing.T) {
	m := New(t.TempDir())
	data := "fake iso contents"

	_, err := m.Save("test.iso", strings.NewReader(data), strings.ToUpper(sha256Hex(data)))
	if err != nil {
		t.Fatalf("Save() error with uppercase hash: %v", err)
	}
}

func TestSave_RejectsEmptyExpectedHash(t *testing.T) {
	m := New(t.TempDir())
	if _, err := m.Save("test.iso", strings.NewReader("data"), ""); err == nil {
		t.Fatalf("Save() = nil error, want rejection of an empty expected hash")
	}
}

func TestSave_RejectsPathSeparatorsInName(t *testing.T) {
	m := New(t.TempDir())
	for _, name := range []string{"../escape.iso", "sub/dir.iso", "", ".", ".."} {
		if _, err := m.Save(name, strings.NewReader("data"), sha256Hex("data")); err == nil {
			t.Errorf("Save(%q) = nil error, want rejection", name)
		}
	}
}

func TestList_ReturnsSortedWithoutSidecarsOrTempFiles(t *testing.T) {
	m := New(t.TempDir())
	m.Save("b.iso", strings.NewReader("b"), sha256Hex("b"))
	m.Save("a.iso", strings.NewReader("a"), sha256Hex("a"))

	infos, err := m.List()
	if err != nil {
		t.Fatalf("List() error: %v", err)
	}
	if len(infos) != 2 || infos[0].Name != "a.iso" || infos[1].Name != "b.iso" {
		t.Fatalf("List() = %+v, want [a.iso, b.iso] sorted", infos)
	}
	if infos[0].SHA256 != sha256Hex("a") {
		t.Errorf("List()[0].SHA256 = %q, want %q", infos[0].SHA256, sha256Hex("a"))
	}
}

func TestList_EmptyDirReturnsNoError(t *testing.T) {
	m := New(filepath.Join(t.TempDir(), "does-not-exist-yet"))
	infos, err := m.List()
	if err != nil {
		t.Fatalf("List() error on nonexistent dir: %v", err)
	}
	if len(infos) != 0 {
		t.Errorf("List() = %+v, want empty", infos)
	}
}

func TestDelete_RemovesFileAndSidecar(t *testing.T) {
	dir := t.TempDir()
	m := New(dir)
	m.Save("test.iso", strings.NewReader("data"), sha256Hex("data"))

	if err := m.Delete("test.iso"); err != nil {
		t.Fatalf("Delete() error: %v", err)
	}
	if _, exists, _ := m.Path("test.iso"); exists {
		t.Errorf("Path() = exists after Delete()")
	}
	if _, err := os.Stat(filepath.Join(dir, "test.iso.sha256")); !os.IsNotExist(err) {
		t.Errorf("sidecar file still present after Delete(), stat err = %v", err)
	}
}

func TestDelete_MissingNameIsNotAnError(t *testing.T) {
	m := New(t.TempDir())
	if err := m.Delete("never-existed.iso"); err != nil {
		t.Errorf("Delete() error = %v, want nil (idempotent)", err)
	}
}

func TestPath_UnknownNameReturnsFalseNotError(t *testing.T) {
	m := New(t.TempDir())
	_, exists, err := m.Path("missing.iso")
	if err != nil {
		t.Fatalf("Path() error: %v", err)
	}
	if exists {
		t.Errorf("Path() = exists, want false for a name never saved")
	}
}
