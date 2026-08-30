// Package dhcpd renders a dnsmasq(8) configuration serving real DHCP
// leases for VMs on Apiary-managed networks (api/internalpb's
// NetworkDefinition) and drives the dnsmasq service to pick it up. This
// is what makes NetworkDefinition-based IP allocation (see
// internal/raft's FSM) actually reach a VM's guest OS, rather than
// being bookkeeping alone. See ADR-0022.
package dhcpd

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
// internal/jail/internal/vlan each keeping their own.
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
