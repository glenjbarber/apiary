package cloudflare

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
)

// runCmd executes `name args...` and waits for it to exit, returning
// trimmed stdout. On failure, the returned error includes stderr.
// Mirrors internal/bhyve's own runCmd exactly.
func runCmd(ctx context.Context, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return "", fmt.Errorf("%s %s: %s", name, strings.Join(args, " "), msg)
	}
	return strings.TrimSpace(stdout.String()), nil
}
