// Package vlan manages per-node VLAN interface and bridge lifecycle by
// shelling out to ifconfig(8) - the physical realization of a
// NetworkDefinition (api/internalpb/state.proto), which is itself
// ephemeral state replicated through raft. See ADR-0022.
package vlan

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
)

// runCmd executes `name args...` and waits for it to exit, returning
// trimmed stdout. On failure, the returned error includes stderr. Own
// private copy, same convention as internal/hast/internal/bhyve/
// internal/jail each keeping their own rather than sharing one.
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
