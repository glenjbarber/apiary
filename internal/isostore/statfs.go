//go:build freebsd || linux || darwin

package isostore

import "syscall"

// freeSpace reports the bytes available to an unprivileged process on the
// filesystem holding dir. It is the standard library's own statfs(2) wrapper
// rather than a dependency: golang.org/x/sys is only an indirect dependency
// of this module, and a disk-space preflight is not worth promoting it to a
// direct one.
//
// The three build tags above are exactly the platforms whose syscall.Statfs_t
// has the Bavail/Bsize fields this reads; statfs_unsupported.go covers the
// rest, where the preflight is skipped rather than guessed at.
func freeSpace(dir string) (int64, error) {
	var fs syscall.Statfs_t
	if err := syscall.Statfs(dir, &fs); err != nil {
		return 0, err
	}
	// Bavail is int64 on FreeBSD and Linux and uint64 on Darwin, Bsize is
	// int32/uint32 respectively; the conversions are the widening ones in
	// every case, so this one expression compiles on all three.
	return int64(fs.Bavail) * int64(fs.Bsize), nil
}
