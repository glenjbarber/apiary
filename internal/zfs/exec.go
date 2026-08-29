// Package zfs manages ZFS datasets by shelling out to the local zfs(8)
// binary, scoped under a configured base dataset so no operation can
// touch anything outside it.
package zfs

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
)

// runZFS executes `zfs <args...>`, returning trimmed stdout. On failure,
// the returned error includes zfs(8)'s own stderr output, which is
// normally specific enough to diagnose (e.g. "dataset already exists").
func runZFS(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "zfs", args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return "", fmt.Errorf("zfs %s: %s", strings.Join(args, " "), msg)
	}
	return strings.TrimSpace(stdout.String()), nil
}
