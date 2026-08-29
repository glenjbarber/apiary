// Package hast manages HAST (Highly Available Storage) resources by
// shelling out to hastctl(8) and rendering /etc/hast.conf, replicating a
// local GEOM provider to a peer node over TCP.
package hast

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
)

// runCmd executes `name args...`, returning trimmed stdout. On failure,
// the returned error includes the command's own stderr output.
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
