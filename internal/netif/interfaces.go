// Package netif reports the host's current network interfaces for
// operator-facing configuration. This is deliberately a read-only,
// node-local inventory: interface names and addresses have meaning only on
// the Hive where they were observed.
package netif

import (
	"bytes"
	"context"
	"net"
	"os/exec"
	"sort"
	"strings"
)

// Interface is the small subset of host interface state useful when
// selecting a VLAN or NAT uplink in the Machine Configuration page.
type Interface struct {
	Name      string
	Up        bool
	Addresses []string
}

// List returns the interfaces currently visible to this process. An
// address lookup failure for one interface does not discard the rest of the
// inventory, because interfaces can disappear while the host is changing
// state. A failure to enumerate interfaces at all is returned.
func List() ([]Interface, error) {
	interfaces, err := net.Interfaces()
	if err != nil {
		return nil, err
	}

	result := make([]Interface, 0, len(interfaces))
	for _, iface := range interfaces {
		item := Interface{
			Name: iface.Name,
			Up:   iface.Flags&net.FlagUp != 0,
		}
		if addrs, err := iface.Addrs(); err == nil {
			item.Addresses = make([]string, 0, len(addrs))
			for _, addr := range addrs {
				item.Addresses = append(item.Addresses, addr.String())
			}
			sort.Strings(item.Addresses)
		}
		result = append(result, item)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Name < result[j].Name })
	return result, nil
}

// BridgeMembers reports the interfaces currently enslaved to the bridge
// named bridge. The three-way answer (members / isBridge / err) is
// deliberate and mirrors internal/netroute.DefaultRouteInterface's own
// hasRoute/error split: isBridge=false with a nil err is a real,
// reportable fact ("that interface exists and is an ordinary NIC, not a
// bridge"), whereas a non-nil err means the membership could not be
// read at all and a caller must NOT treat that as "not a member".
//
// `ifconfig(8)` is the source, not `bridge(8)`, for two reasons: it is
// in base and always present, and its `groups: bridge` line is what lets
// isBridge be answered outright instead of inferred from an absent
// `member:` line (a bridge with zero members is legal). The `member:`
// lines it prints are the same ones internal/install's own
// `bhyve-bridge` check already parses (internal/install/checks.go's
// strings.Contains(out, "member: "+...) test), so this is not a second,
// differently-formatted view of the same fact.
//
// This is node-local and has the same no-cross-host-meaning caveat as
// List above.
func BridgeMembers(ctx context.Context, bridge string) (members []string, isBridge bool, err error) {
	if bridge == "" {
		return nil, false, errNoInterfaceName
	}
	cmd := exec.CommandContext(ctx, "ifconfig", bridge)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	runErr := cmd.Run()
	return parseBridgeIfconfigOutput(bridge, out.String(), runErr)
}

// parseBridgeIfconfigOutput is a pure function, so the exact
// ifconfig(8) output shape can be exercised from fixtures on a
// non-FreeBSD development host (internal/netroute's own
// parseDefaultRouteOutput follows the same split for the same reason).
//
// FreeBSD prints one block per interface, header at column 0
// ("bridge0: flags=8863<...>"), continuation lines indented, so the
// block is located by an unindented header line rather than by
// accumulating every `member:` line in the output - defensive, but it
// costs nothing and keeps a future `ifconfig -a` switch correct.
func parseBridgeIfconfigOutput(bridge, combinedOutput string, exitErr error) (members []string, isBridge bool, err error) {
	// ifconfig(8) exits non-zero for an unknown interface name. That is
	// a measurement failure here, not a reportable "not a bridge": we
	// were asked about an interface the caller believes exists, and
	// failing to read it must never be read as evidence of absence.
	if exitErr != nil {
		return nil, false, exitErr
	}

	var (
		found     bool
		inBlock   bool
		collectTo []string
	)
	flush := func() {
		if inBlock && found && isBridge {
			members = append(members, collectTo...)
		}
		collectTo = nil
	}
	for _, raw := range strings.Split(combinedOutput, "\n") {
		if name, ok := ifconfigHeaderName(raw); ok {
			flush()
			inBlock = name == bridge
			found = found || inBlock
			continue
		}
		if !inBlock {
			continue
		}
		line := strings.TrimSpace(raw)
		switch {
		case strings.HasPrefix(line, "groups:"):
			for _, g := range strings.Split(strings.TrimPrefix(line, "groups:"), ",") {
				if strings.TrimSpace(g) == "bridge" {
					isBridge = true
				}
			}
		case strings.HasPrefix(line, "member:"):
			if fields := strings.Fields(line); len(fields) >= 2 {
				collectTo = append(collectTo, fields[1])
			}
		}
	}
	flush()

	if !found {
		// Ran without error but printed no block for the interface asked
		// about - unrecognized, so report the failure rather than
		// guessing either way.
		return nil, false, errUnrecognizedOutput
	}
	sort.Strings(members)
	return members, isBridge, nil
}

// ifconfigHeaderName recognizes an ifconfig(8) per-interface header line,
// which is the only line in the output that starts at column 0 and
// contains a colon: "<name>: flags=...". Every other line is indented,
// which is what keeps a member's continuation line
// ("\t        ifmaxaddr 0 port 7 ...") from being mistaken for one.
func ifconfigHeaderName(line string) (string, bool) {
	if line == "" || line[0] == ' ' || line[0] == '\t' {
		return "", false
	}
	colon := strings.Index(line, ":")
	if colon <= 0 {
		return "", false
	}
	rest := line[colon+1:]
	if !strings.HasPrefix(strings.TrimSpace(rest), "flags=") {
		return "", false
	}
	return line[:colon], true
}

var (
	errNoInterfaceName    = noInterfaceNameError{}
	errUnrecognizedOutput = unrecognizedOutputError{}
)

type noInterfaceNameError struct{}

func (noInterfaceNameError) Error() string {
	return "netif: no interface name given"
}

type unrecognizedOutputError struct{}

func (unrecognizedOutputError) Error() string {
	return "netif: ifconfig(8) output did not contain a block for the interface asked about"
}
