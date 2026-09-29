package jail

import (
	"context"
	"errors"
	"fmt"
	"path"
	"strconv"
	"strings"
)

// This file holds the in-jail half of DHCP addressing: everything
// needed to find out whether a jail's own vnet interface is configured
// by DHCP, and to start a DHCP client on it if nothing is.
//
// Why this has to happen *inside* the jail, rather than being imposed
// from the host the way a static address is, is not a style preference.
// jail(8) has no creation parameter that makes a vnet interface run
// DHCP - not even the ip4.* family, which exists only for the
// ip4=inherit model (the same point manager.go makes about
// ip4.addr). A lease is negotiated between an interface's own link
// layer and a server on the segment it is attached to, and a VNET
// jail's interface lives behind jail(8)'s own virtual network stack,
// so the client has to run in the same stack as the interface. The host
// cannot do it from outside; only jexec(8) can, which is the same
// seam EnsureAddressing uses for the static case.
//
// This is the half ADR-0117 left out. The raft FSM already skips
// allocateIP for a jail on an uplink_bridged network - the identical
// carve-out it makes for a VM - and internal/jailnet's reconciler
// already declines to touch such a jail's addressing, because on that
// network there is no Apiary-managed subnet for a static address to
// belong to. What was missing is the thing that makes the carve-out
// independently useful: something in the jail actually asking for the
// physical LAN's own DHCP.
const DefaultDHCPClient = "dhclient"

// DHCPState is a snapshot of one jail's own vnet interface as seen from
// inside the jail, at the moment of observation, reduced to the two
// questions a DHCP-managed interface raises: is anything addressed, and
// is a client running that will keep it that way.
//
// It is deliberately not a "DHCP is fine" boolean. On a network that
// skips allocation, Apiary is not the authority on this address - the
// LAN's own DHCP server is - so this snapshot is evidence, and the
// caller decides what to do with it. As in JailNetState, every field
// is a definite answer or the zero value of a question that was not
// answered; the error return is what distinguishes the two.
type DHCPState struct {
	// InterfacePresent is false only when the jail's own ifconfig gave a
	// definite "no such interface" answer. It is never false as a
	// fallback for a failed observation - that is an error return.
	InterfacePresent bool

	// Addresses are the interface's IPv4 addresses, in the order
	// ifconfig listed them, whatever configured them. Apiary does not
	// change them on this path and must not judge them: the address a
	// jail gets on an uplink_bridged network comes from a router
	// outside this project entirely. They are reported so a caller can
	// say what the jail actually ended up with, not so it can enforce
	// anything about them.
	Addresses []Address

	// Clients are the interfaces that a running DHCP client is bound
	// to, in the order ps listed them. Empty means ps ran and no client
	// was running, which is a definite answer; it is never the zero
	// value of a question that was not answered.
	Clients []string
}

// Addressed reports whether the jail's own vnet interface carries at
// least one IPv4 address, i.e. whether anything has configured it at
// all.
func (s DHCPState) Addressed() bool { return len(s.Addresses) > 0 }

// ClientRunning reports whether a DHCP client is running for iface
// specifically. Matching the interface rather than "is any client
// running" is deliberate: a jail with a client on one interface and
// none on the vnet one is the unconfigured case, and treating the other
// interface's client as this one's would hide exactly the drift this
// exists to find.
func (s DHCPState) ClientRunning(iface string) bool {
	for _, c := range s.Clients {
		if c == iface {
			return true
		}
	}
	return false
}

// dhcpClient is the DHCP client binary this Manager runs inside jails,
// defaulted to DefaultDHCPClient. It is settable because the jail root
// is supplied by the caller and nothing guarantees it contains one:
// a root without ISC dhclient is a normal thing to encounter, and the
// remedy (a different client, or a package in the template) is an
// operator's decision, not a rebuild of this package.
func (m *Manager) dhcpClient() string {
	if m.DHCPClient == "" {
		return DefaultDHCPClient
	}
	return path.Base(m.DHCPClient)
}

// ObserveDHCP snapshots whether a DHCP client is running for iface
// inside jail, and whether iface carries an address, via jexec(8).
//
// Both questions are asked through the same discipline ObserveNet uses.
// A definite "no such interface" from the jail's own ifconfig is
// DHCPState{InterfacePresent: false} with a nil error, because "the
// jail is running but its vnet interface is gone" is a real, different
// answer that the caller has to act on rather than a failure to look.
// A definite "no such jail" from jexec is reported as an error wrapping
// ErrJailNotFound, for the same reason ObserveNet reports it that way.
//
// The second command (the process listing) is deliberately NOT allowed
// to claim the jail is gone. It runs after the ifconfig call, so the
// jail has already been heard from by the time it is asked, and a
// non-zero exit there is one of three things - the jail died in
// between, the jail's root has no ps(1), or ps itself failed - which
// the tools' wording does not reliably tell apart. Reporting all of
// them as unknown is the honest reading, and the verbatim error is
// carried in the message so a caller escalating it to an operator keeps
// the evidence.
func (m *Manager) ObserveDHCP(ctx context.Context, jail, iface string) (DHCPState, error) {
	var state DHCPState

	out, err := m.run(ctx, "jexec", jail, "ifconfig", iface)
	if err != nil {
		if notFound(err, "ifconfig") {
			return DHCPState{InterfacePresent: false}, nil
		}
		if notFound(err, "jexec") {
			return DHCPState{}, fmt.Errorf("observing interface %s: %w", iface, errors.Join(ErrJailNotFound, err))
		}
		return DHCPState{}, fmt.Errorf("observing interface %s inside jail %q: %w", iface, jail, err)
	}
	state.InterfacePresent = true
	state.Addresses = parseIfconfigAddresses(out)

	// jexec(8) is given the whole jail, and a VNET jail's process
	// table contains only its own processes, so a host-side DHCP client
	// can never be counted here. -o with empty headers keeps the output
	// to two parseable columns and -ww keeps a long command line
	// untruncated, which matters because the interface name is read out
	// of it.
	procs, err := m.run(ctx, "jexec", jail, "ps", "ax", "-ww", "-o", "pid=,args=")
	if err != nil {
		return DHCPState{}, fmt.Errorf("listing processes inside jail %q to see whether a DHCP client is running on %s: %w", jail, iface, err)
	}
	state.Clients = parseDHCPClients(procs, m.dhcpClient())

	return state, nil
}

// EnsureDHCP makes sure a DHCP client is running for iface inside jail,
// starting one if none is.
//
// It observes first and only starts a client when none is running for
// this interface, which is what makes it idempotent: repeated calls
// against an already-configured jail issue no commands at all. It also
// makes it safe against the failure this most obviously has to survive -
// starting a second client on an interface that already has one would
// have two processes fighting over the same address.
//
// A missing interface is an error, not a silent success, for the same
// reason EnsureAddressing gives: there is no way to conjure a vnet
// interface into a running jail, so the caller has to be told this
// jail must be restarted.
//
// A client that fails to start is an error carrying the tool's own
// wording, and is deliberately not classified further. "dhclient is not
// in this jail's root" and "the LAN's DHCP server is not answering" are
// the two likely causes, they call for opposite operator responses, and
// the two cannot be told apart from the exit status or from jexec's
// wording - which is also the wording an absent jail uses. Naming one
// of them here would be a guess; passing both through verbatim lets the
// caller report the one that actually happened.
func (m *Manager) EnsureDHCP(ctx context.Context, jail, iface string) error {
	state, err := m.ObserveDHCP(ctx, jail, iface)
	if err != nil {
		return err
	}
	if !state.InterfacePresent {
		return fmt.Errorf("interface %s is not present inside jail %q; jail(8) cannot add a vnet interface to an already-running jail, so this jail must be restarted", iface, jail)
	}
	if state.ClientRunning(iface) {
		return nil
	}

	client := m.dhcpClient()
	if _, err := m.run(ctx, "jexec", jail, client, iface); err != nil {
		return fmt.Errorf("starting DHCP client %s on %s inside jail %q: %w", client, iface, jail, err)
	}
	return nil
}

// parseDHCPClients returns the interface each running DHCP client is
// bound to, from `ps ax -ww -o pid=,args=` output taken inside a jail,
// e.g.
//
//	 931 /sbin/dhclient epair0b
//	1042 /usr/local/sbin/dhclient -q -1 vnet0
//
// Lines whose first field is not a pid are skipped, so a ps header or
// any other non-process line can never be read as a running client.
// The client is matched on the basename of argv[0], so
// /sbin/dhclient and dhclient are the same client and a differently
// named program is not it. The interface is the first argument that is
// not a flag.
//
// A client line with no such argument is dropped rather than recorded
// with an empty interface, because an unattributable client cannot be
// credited to this jail's interface, and crediting it wrongly would
// hide a jail whose vnet interface has no client at all.
func parseDHCPClients(out, client string) []string {
	var clients []string
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 3 {
			continue
		}
		if _, err := strconv.Atoi(fields[0]); err != nil {
			continue
		}
		if path.Base(fields[1]) != client {
			continue
		}
		for _, arg := range fields[2:] {
			if strings.HasPrefix(arg, "-") {
				continue
			}
			clients = append(clients, arg)
			break
		}
	}
	return clients
}
