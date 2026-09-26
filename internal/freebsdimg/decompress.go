// Package freebsdimg manages FreeBSD official VM images.
package freebsdimg

import (
	"os"
	"os/exec"
)

// decompressXZCommand uses the system xz(1) utility to decompress a
// .xz file to the destination path.
func decompressXZCommand(src, dst string) error {
	// xz -dc <src >dst
	cmd := exec.Command("xz", "-dc", src)
	outFile, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer outFile.Close()

	cmd.Stdout = outFile
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		os.Remove(dst) // Clean up partial output on failure
		return err
	}
	return nil
}