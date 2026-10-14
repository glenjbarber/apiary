package tlsmigrate

import (
	"errors"
	"os"
	"syscall"
)

// isCrossDevice reports whether err is os.Rename's own cross-filesystem
// failure (EXDEV) - the one case moveFile falls back to a copy for.
// Every other error from Rename (permission, not-exist, and so on) is
// returned as-is rather than silently retried as a copy.
func isCrossDevice(err error) bool {
	var linkErr *os.LinkError
	if !errors.As(err, &linkErr) {
		return false
	}
	errno, ok := linkErr.Err.(syscall.Errno)
	return ok && errno == syscall.EXDEV
}
