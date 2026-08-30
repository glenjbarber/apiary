// Package pf applies per-VM firewall rules (api/internalpb's
// FirewallRule, part of VMDefinition) via a dedicated pf(8) anchor per
// VM, driven by pfctl(8). See ADR-0022. Requires pf already enabled on
// the host with an anchor point reserved for Apiary in /etc/pf.conf
// (e.g. `anchor "apiary/*"`) - a one-time host prerequisite this
// package does not set up itself, the same way internal/hast doesn't
// manage the hastd service and internal/dhcpd doesn't install dnsmasq.
package pf

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
)

// runCmdStdin executes `name args...`, writing stdin to the process's
// own stdin, and waits for it to exit, returning trimmed stdout. On
// failure, the returned error includes stderr.
func runCmdStdin(ctx context.Context, stdin string, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdin = strings.NewReader(stdin)
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

// runCmd executes `name args...` and waits for it to exit, returning
// trimmed stdout. On failure, the returned error includes stderr.
func runCmd(ctx context.Context, name string, args ...string) (string, error) {
	return runCmdStdin(ctx, "", name, args...)
}
