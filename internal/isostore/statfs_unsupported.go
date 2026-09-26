//go:build !freebsd && !linux && !darwin

package isostore

// freeSpace has no implementation on this platform. It returns the sentinel
// errUnsupportedSpace, which the fetch preflight treats as "cannot ask,
// must not guess" and proceeds anyway - a store on an unknown platform
// behaves exactly as it did before the preflight existed.
func freeSpace(dir string) (int64, error) {
	return 0, errUnsupportedSpace
}
