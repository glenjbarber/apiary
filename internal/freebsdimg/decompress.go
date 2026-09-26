// Package freebsdimg's xz decompression, kept in its own file because it is
// the one part of the fetch that is neither Go nor network: it shells out to
// the system's xz(1).
package freebsdimg

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os/exec"
	"strings"
)

// decompressXZ expands the .xz file at src into out, returning the number of
// bytes written and the SHA-256 of what was written. It runs
// "xz -dc <src>" and copies xz's stdout, so the multi-gigabyte result never
// passes through Go buffers whole and is never held in memory.
//
// base is the number of bytes already on disk for this fetch (the compressed
// download, as actually received), so the reported count continues from where
// the download left off rather than restarting at zero mid-fetch.
func decompressXZ(ctx context.Context, src string, out io.Writer, track *progressTracker, base int64) (int64, string, error) {
	// Resolved up front so a host without xz(1) gets a clear, actionable
	// error instead of exec's "executable file not found in $PATH" from
	// deep inside cmd.Run. FreeBSD's base system ships xz(1); a minimal
	// install or a jail built without it would not.
	xzPath, err := exec.LookPath("xz")
	if err != nil {
		return 0, "", fmt.Errorf("xz(1) not found in PATH (install the xz package): %w", err)
	}

	cmd := exec.CommandContext(ctx, xzPath, "-dc", src)
	stderr := &limitedBuffer{limit: 4096}
	cmd.Stderr = stderr

	// Hashed and counted on the way past, so a 668MB .raw.xz expanding to
	// several gigabytes is never buffered whole: one pass, two outputs.
	// A tracker with a nil fn makes the counting wrapper a no-op, so the
	// no-progress case needs no separate code path.
	h := sha256.New()
	dst := io.Writer(&countingWriter{w: io.MultiWriter(out, h), track: track, base: base})

	// Started explicitly rather than by Run: the output is drained through
	// a pipe so it can be hashed and counted on the way past, and Wait must
	// not be called before that drain reaches EOF.
	// StdoutPipe must be obtained before Start: os/exec refuses it
	// afterwards, and the pipe is what lets the output be hashed and
	// counted on the way past.
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return 0, "", fmt.Errorf("xz stdout pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return 0, "", fmt.Errorf("starting xz: %w%s", err, stderr.suffix())
	}
	written, copyErr := io.Copy(dst, plainReader{stdout})
	// Wait is called on every path so the child is always reaped, and only
	// after the pipe has been drained to EOF, which os/exec requires.
	waitErr := cmd.Wait()
	if copyErr != nil {
		return 0, "", fmt.Errorf("reading xz output: %w", copyErr)
	}
	if waitErr != nil {
		if ctx.Err() != nil {
			return 0, "", ctx.Err()
		}
		// xz is the only thing that can report "this isn't really an xz
		// file" - a checksum mismatch is caught before this point, but a
		// structurally valid .xz that isn't valid xz is not - so its own
		// message is worth surfacing verbatim.
		return 0, "", fmt.Errorf("xz failed: %w%s", waitErr, stderr.suffix())
	}
	return written, hex.EncodeToString(h.Sum(nil)), nil
}

// plainReader hides the WriteTo method *os.File carries, which io.Copy
// prefers over its own buffered loop whenever it is the source. On a pipe
// that method's fast path is unavailable and its fallback copies in the
// wrong direction, which deadlocks against the child still writing; the
// only reader that reliably works here is one that looks like nothing but
// an io.Reader.
type plainReader struct{ io.Reader }

// countingWriter forwards to w and reports the bytes written to the fetch's
// progress tracker, offset by base - the bytes already counted before the
// decompression phase began.
type countingWriter struct {
	w     io.Writer
	track *progressTracker
	base  int64
}

func (c *countingWriter) Write(b []byte) (int, error) {
	n, err := c.w.Write(b)
	c.track.add(c.base, int64(n))
	return n, err
}

// limitedBuffer collects the first limit bytes written to it and silently
// drops the rest - a corrupt multi-gigabyte .xz can make xz emit a lot of
// diagnostics, and an error message wants the beginning of them, not all of
// them.
type limitedBuffer struct {
	limit int
	buf   []byte
	full  bool
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if !b.full {
		if room := b.limit - len(b.buf); room > 0 {
			if len(p) < room {
				b.buf = append(b.buf, p...)
			} else {
				b.buf = append(b.buf, p[:room]...)
				b.full = true
			}
		} else {
			b.full = true
		}
	}
	return len(p), nil
}

// suffix renders the collected diagnostics as a message tail, empty when
// xz said nothing on stderr. Trimmed because this is spliced into a single
// error line that callers log and put in front of operators, where xz's own
// trailing newline would be noise.
func (b *limitedBuffer) suffix() string {
	if len(b.buf) == 0 {
		return ""
	}
	if s := strings.TrimSpace(string(b.buf)); s != "" {
		return ": " + s
	}
	return ""
}
