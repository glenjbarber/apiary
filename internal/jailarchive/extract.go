// Package jailarchive extracts a FreeBSD base.txz-style userland
// archive into a jail's root, shelling out to tar(1) (the same
// shell-out convention as internal/bhyve/internal/hast/internal/jail,
// unlike internal/isostore's pure file I/O - see that package's own
// doc comment on why it's the exception). base.txz is a tar archive
// compressed with xz, and Go's standard library has no xz decoder, but
// tar(1) on both FreeBSD (bsdtar) and every other platform this
// codebase targets auto-detects and decodes xz/gzip/bzip2 compression
// from the archive's own contents, so a plain `tar -xpf` handles it
// without Apiary needing to implement or vendor one. See ADR-0098.
package jailarchive

import (
	"context"
	"fmt"
	"os/exec"
)

// Extractor extracts archives via tar(1).
type Extractor struct{}

// New returns an Extractor.
func New() *Extractor {
	return &Extractor{}
}

// Extract extracts archivePath into destDir, which must already exist.
// Permissions and ownership are preserved (-p) - a base userland
// includes setuid binaries and device nodes whose exact mode matters,
// the same reasoning bsdinstall/ezjail/iocage apply when they extract
// base.txz the same way.
func (e *Extractor) Extract(ctx context.Context, archivePath, destDir string) error {
	cmd := exec.CommandContext(ctx, "tar", "-xpf", archivePath, "-C", destDir)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("jailarchive: tar -xpf %s -C %s: %w: %s", archivePath, destDir, err, out)
	}
	return nil
}
