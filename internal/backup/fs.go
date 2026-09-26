package backup

import (
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
)

// FS is the filesystem surface this package uses. It is an interface for
// the reason internal/resetutil and internal/hostpkg give theirs: the
// manifest's central claim is about what a reader can see at each
// instant, and a claim about interleaving of file operations cannot be
// tested against the real filesystem at all - you cannot make a real
// rename fail halfway through a test, and you certainly cannot make a
// real crash. With this interface a test can fail exactly the fsync, or
// exactly the rename, and then assert that no manifest became visible.
//
// The surface is deliberately tiny. Everything this package needs from
// a filesystem is: make a directory, create a file that can be synced
// and chmodded, rename, remove, stat, open, read, list, and fsync a
// directory. Nothing about it is expressive enough to be a privilege
// boundary or a general-purpose filesystem shim.
type FS interface {
	// MkdirAll creates a directory and any missing parents.
	MkdirAll(path string, perm fs.FileMode) error
	// CreateTemp creates a new file in dir with a name matching pattern,
	// returning it opened for writing. It is the same contract as
	// os.CreateTemp: the file is created with mode 0600.
	CreateTemp(dir, pattern string) (File, error)
	// Rename moves oldpath to newpath, replacing newpath if it exists.
	Rename(oldpath, newpath string) error
	// Remove deletes a single file or empty directory.
	Remove(path string) error
	// RemoveAll deletes a path and everything under it.
	RemoveAll(path string) error
	// Stat returns file information.
	Stat(path string) (fs.FileInfo, error)
	// Open opens a file for reading.
	Open(path string) (io.ReadCloser, error)
	// ReadDir lists a directory.
	ReadDir(path string) ([]fs.DirEntry, error)
	// ReadFile reads a whole file.
	ReadFile(path string) ([]byte, error)
	// WriteFile writes a whole file, creating it if needed.
	WriteFile(path string, data []byte, perm fs.FileMode) error
	// SyncDir fsyncs a directory so that renames and creations inside it
	// are durable. See Store.Commit for why a manifest publication is
	// not complete without it.
	SyncDir(path string) error
}

// File is the subset of *os.File this package needs. Narrower than
// io.WriteCloser on purpose: the fsync and the chmod are load-bearing
// parts of the atomic write, and an interface that omitted them would
// let a test double quietly make the guarantee untestable.
type File interface {
	io.Writer
	// Sync flushes the file's contents to stable storage.
	Sync() error
	// Close closes the file. It is called exactly once, and always
	// after Sync.
	Close() error
	// Chmod sets the file's mode.
	Chmod(mode fs.FileMode) error
	// Name returns the path the file was created at.
	Name() string
}

// OSFS is the real filesystem. It is the only implementation in this
// package that calls into the os package, so a test that needs
// different behaviour wraps this rather than reimplementing it.
type OSFS struct{}

var _ FS = OSFS{}

// MkdirAll implements FS.
func (OSFS) MkdirAll(path string, perm fs.FileMode) error { return os.MkdirAll(path, perm) }

// CreateTemp implements FS.
func (OSFS) CreateTemp(dir, pattern string) (File, error) {
	f, err := os.CreateTemp(dir, pattern)
	if err != nil {
		return nil, err
	}
	return f, nil
}

// Rename implements FS.
func (OSFS) Rename(oldpath, newpath string) error { return os.Rename(oldpath, newpath) }

// Remove implements FS.
func (OSFS) Remove(path string) error { return os.Remove(path) }

// RemoveAll implements FS.
func (OSFS) RemoveAll(path string) error { return os.RemoveAll(path) }

// Stat implements FS.
func (OSFS) Stat(path string) (fs.FileInfo, error) { return os.Stat(path) }

// Open implements FS.
func (OSFS) Open(path string) (io.ReadCloser, error) { return os.Open(path) }

// ReadDir implements FS.
func (OSFS) ReadDir(path string) ([]fs.DirEntry, error) { return os.ReadDir(path) }

// ReadFile implements FS.
func (OSFS) ReadFile(path string) ([]byte, error) { return os.ReadFile(path) }

// WriteFile implements FS.
func (OSFS) WriteFile(path string, data []byte, perm fs.FileMode) error {
	return os.WriteFile(path, data, perm)
}

// SyncDir implements FS.
//
// This is the step that is easy to leave out and expensive to omit. A
// rename is a directory-entry change; without fsyncing the containing
// directory, a crash can lose the entry while the data blocks it points
// at are already on disk. The result is a backup that reported success
// and then stopped existing - which is strictly worse than one that
// failed, because the operator was told it was fine.
//
// os.Open on a directory and fsync on the resulting descriptor is
// portable across the three platforms this project targets (the behavior
// is specified by POSIX and implemented by Linux, macOS and FreeBSD). A
// platform where it is not supported returns an error, and this package
// treats that as a failed write rather than as something to log and
// ignore: publishing a manifest that cannot be promised durable is
// exactly the silent downgrade the whole design forbids.
func (OSFS) SyncDir(path string) error {
	d, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("opening directory %s to sync it: %w", path, err)
	}
	defer d.Close()
	if err := d.Sync(); err != nil {
		return fmt.Errorf("syncing directory %s: %w", path, err)
	}
	return nil
}

// atomicWriteFile publishes body at path through a temp file, an fsync,
// a chmod, and a rename, in that order, and returns only once the
// containing directory has also been synced.
//
// Every step is there for a reason and the reasons are worth keeping in
// one place, because this function is the whole of the
// manifest-written-last-and-atomically guarantee:
//
//   - The temp file is in the SAME directory as the destination, not in
//     /tmp. rename(2) is only atomic within a filesystem, so a temp file
//     on another filesystem turns the atomic rename into a
//     non-atomic copy, and a reader would be able to observe a
//     half-written manifest.
//   - The chmod is explicit rather than inherited from the temp file's
//     0600 default, because a manifest is a description of a whole
//     backup and the convention here is the same one
//     internal/raftdconfig's and internal/frontendconfig's atomic writes
//     use: 0600, stated, not implied by whatever umask happens to be.
//   - The fsync happens BEFORE the rename. The rename is what makes the
//     file visible; before the fsync, a crash could leave a visible,
//     named, and completely empty manifest. That is the worst possible
//     outcome: it looks like a backup to every reader that only checks
//     for the file's existence.
//   - The directory fsync happens AFTER the rename, and is the step that
//     makes the rename itself durable. See OSFS.SyncDir.
//   - The deferred Remove is a no-op once the rename has succeeded, and
//     cleans up the temp file on every failure path so a failed write
//     leaves nothing behind that a later reader could mistake for
//     something.
func atomicWriteFile(filesystem FS, dir, pattern, path string, body []byte, perm fs.FileMode) (err error) {
	tmp, err := filesystem.CreateTemp(dir, pattern)
	if err != nil {
		return fmt.Errorf("creating a temp file in %s: %w", dir, err)
	}
	tmpPath := tmp.Name()
	defer func() {
		// Best-effort and deliberately not folded into the returned
		// error: if the rename already succeeded there is nothing to
		// clean up, and if it did not, the write has already failed
		// with a more specific message than "could not delete a temp
		// file" would be. Reporting a cleanup failure over a real
		// failure would bury the reason the backup did not happen.
		_ = filesystem.Remove(tmpPath)
	}()

	if _, err := tmp.Write(body); err != nil {
		tmp.Close()
		return fmt.Errorf("writing %d bytes to %s: %w", len(body), tmpPath, err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("syncing %s before publishing it: %w", tmpPath, err)
	}
	if err := tmp.Chmod(perm); err != nil {
		tmp.Close()
		return fmt.Errorf("setting mode %v on %s: %w", perm, tmpPath, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("closing %s: %w", tmpPath, err)
	}
	if err := filesystem.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("publishing %s as %s: %w", tmpPath, path, err)
	}
	if err := filesystem.SyncDir(dir); err != nil {
		// The rename has already happened, so the manifest IS visible.
		// Reporting an error is still correct - the caller must not treat
		// this generation as durably committed - and removing the file
		// again would be a second, non-atomic change to a directory a
		// reader may already be looking at. So the file stays, the error
		// is returned, and Store.Commit turns it into a generation that
		// Sweep will find. A generation whose directory survived but
		// whose durability was not confirmed is still a generation a
		// reader can see, which is why Commit re-syncs the parent
		// directory as a second attempt before giving up.
		return fmt.Errorf("publishing %s succeeded but syncing directory %s did not: %w", path, dir, err)
	}
	return nil
}

// joinPath is filepath.Join with the package's own error-free
// convention kept in one place, so path handling is not scattered.
func joinPath(elem ...string) string { return filepath.Join(elem...) }
