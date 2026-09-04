// Package netroute reads this node's own default-route interface via
// FreeBSD's route(8) - genuinely greenfield in this codebase (no
// existing package shells out to netstat -rn/route get anywhere; see
// ADR-0055). IPv4 only, matching this project's IPv4-only network model
// elsewhere.
//
// DISCLOSED LIMITATION: the exact output/exit behavior of `route -n get
// default` when a node has no default route at all was not verified
// against a real FreeBSD host during planning - parseDefaultRouteOutput
// takes the combined stdout+stderr+exit-error specifically so a real
// fixture can replace the current best-effort guess once captured on
// apiarium/apiverse. The safe failure direction is always StatusUnknown
// (via a non-nil error), never a false StatusFalse claim.
package netroute

import (
	"bytes"
	"context"
	"os/exec"
	"strings"
)

// DefaultRouteInterface shells `route -n get default` and reports the
// interface name FreeBSD's routing table currently uses for the default
// route. hasRoute is false (with a nil err) when route(8) ran
// successfully but definitively reports no default route configured at
// all - a real, reportable fact, not a measurement failure. err is
// non-nil only when the check itself could not be completed (binary
// missing, permission denied, unrecognized output).
func DefaultRouteInterface(ctx context.Context) (iface string, hasRoute bool, err error) {
	cmd := exec.CommandContext(ctx, "route", "-n", "get", "default")
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	runErr := cmd.Run()
	return parseDefaultRouteOutput(out.String(), runErr)
}

// parseDefaultRouteOutput is a pure function so the output-format
// guesswork noted in the package doc comment can be exercised and
// corrected via table-driven fixtures without shelling out.
func parseDefaultRouteOutput(combinedOutput string, exitErr error) (iface string, hasRoute bool, err error) {
	lower := strings.ToLower(combinedOutput)

	// route(8) on FreeBSD reports this (case varies by version) when no
	// route matches the destination - a real, definitive "no default
	// route" answer, not an exec failure, even though the command exits
	// non-zero in this case.
	if strings.Contains(lower, "not in table") || strings.Contains(lower, "no such process") {
		return "", false, nil
	}

	if exitErr != nil {
		return "", false, exitErr
	}

	for _, line := range strings.Split(combinedOutput, "\n") {
		line = strings.TrimSpace(line)
		fields := strings.Fields(line)
		if len(fields) == 2 && strings.EqualFold(fields[0], "interface:") {
			return fields[1], true, nil
		}
	}

	// Ran without error but the output didn't match any recognized
	// shape - report unknown (via an error) rather than guessing either
	// way.
	return "", false, errUnrecognizedOutput
}

var errUnrecognizedOutput = unrecognizedOutputError{}

type unrecognizedOutputError struct{}

func (unrecognizedOutputError) Error() string {
	return "netroute: route(8) output did not match any recognized format"
}
