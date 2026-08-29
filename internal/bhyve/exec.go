// Package bhyve manages bhyve VM lifecycle by shelling out to
// bhyve(8)/bhyvectl(8), scoped under a configured name Prefix so Apiary
// never lists or touches a VM it didn't create - the same convention
// internal/jail uses, since bhyve VM names are also a flat namespace
// (via /dev/vmm/<name>), not hierarchical like ZFS datasets.
package bhyve

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
)

// runCmd executes `name args...` and waits for it to exit, returning
// trimmed stdout. On failure, the returned error includes stderr. Use
// this for short-lived commands (bhyvectl); bhyve itself is started
// detached via startDetached instead, since it runs for the VM's
// lifetime.
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
