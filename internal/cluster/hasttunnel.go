package cluster

import (
	"fmt"
	"net"
	"strings"
)

// ADR-0158: the encrypted HAST transport.
//
// hastd(8) has no encryption of its own - verified against the
// upstream FreeBSD source, where a search for tls/ssl/encrypt/crypt
// across the whole of sbin/hastd/ returns nothing - so the encryption
// has to be a transport tunnel carrying hastd's own protocol.
//
// The tunnel is a WireGuard peer link, and the whole reason for that
// choice over a TLS-terminating proxy is one line in hastd's own
// source. On accepting a connection, hastd.c calls
// proto_address_match(conn, res->hr_remoteaddr) for each configured
// resource and closes the connection with "Client %s isn't known." if
// none match. For TCP, tcp_address_match compares ONLY the peer
// connection's IP address against the configured address - the port is
// not compared.
//
// A userspace proxy in front of hastd therefore breaks replication
// outright: the secondary would accept the connection from the proxy's
// own local address, not the primary's, so every connection is
// refused. A layer-3 tunnel preserves the source address, so the
// secondary keeps authenticating the primary HOST rather than a local
// process. That is what makes WireGuard the right shape here, and it
// is also why this file resolves an ADDRESS rather than wrapping a
// socket.

// hastTunnelPort is the port hastd's own TCP transport listens on and
// connects to, and therefore the port the tunnel carries. It matches
// hastd's own HASTD_LISTEN_TCP4 ("tcp4://0.0.0.0:8457") in the
// upstream source.
const hastTunnelPort = "8457"

// hastTunnelEnabled gates the encrypted transport. It is false by
// default: provisioning the tunnel is a host prerequisite (a WireGuard
// interface, a peer key per Comb, a shared address plan), and a
// half-provisioned tunnel would break replication in a way that is
// worse than the plaintext it replaces. See ADR-0158's "The encryption
// is opt-in per Colony" section.
type HASTTunnelConfig struct {
	// Enabled reports whether the encrypted transport is in use.
	Enabled bool

	// Addrs maps a raft node ID to that node's address ON THE TUNNEL.
	// It is the tunnel address plan, provisioned on the hosts
	// themselves: the reconciler reads it, it does not create it.
	//
	// The key is the node ID, which is the same identity hast.conf's
	// Node.Name already uses, so the two cannot drift.
	Addrs map[string]string
}

// hastTunnelAddr returns the tunnel address for a peer node, and
// whether one is known.
//
// A node with no tunnel address is an ERROR when the tunnel is
// enabled, not a silent fallback to the LAN address: falling back
// would mean replication "working" in plaintext, which is exactly the
// state this change exists to make impossible.
func (c HASTTunnelConfig) hastTunnelAddr(nodeID string) (string, error) {
	if !c.Enabled {
		return "", nil
	}
	addr, ok := c.Addrs[nodeID]
	if !ok {
		return "", fmt.Errorf("node %q has no HAST tunnel address; the encrypted transport is enabled but this node's tunnel address plan does not include it", nodeID)
	}
	return addr, nil
}

// resolveHASTPeerAddr returns the address to put in hast.conf's
// Node.Remote for a peer node.
//
// With the tunnel disabled it is the peer's LAN address from raft
// membership - today's behaviour, byte for byte. With the tunnel
// enabled it is the peer's tunnel address.
//
// It returns the LAN address alongside, because the reconciler needs
// the LAN address for its own reachability check against the tunnel
// (see reconcileHASTRoles), and because the two must never be
// confused.
func (c HASTTunnelConfig) resolveHASTPeerAddr(nodeID, lanAddr string) (remote string, err error) {
	tunnel, err := c.hastTunnelAddr(nodeID)
	if err != nil {
		return "", err
	}
	if tunnel == "" {
		// Encryption disabled: the LAN address, exactly as today.
		return lanAddr, nil
	}
	// A tunnel address is a host:port pair, because the tunnel carries
	// hastd's own TCP transport and that transport has a port. A bare
	// host would make hastd fall back to its own default port, which
	// happens to be the right one, but being explicit keeps the
	// rendered config self-describing and keeps the two ends
	// symmetric.
	if _, _, err := net.SplitHostPort(tunnel); err != nil {
		return "", fmt.Errorf("node %q HAST tunnel address %q is not host:port: %w", nodeID, tunnel, err)
	}
	return tunnel, nil
}

// HASTTunnelConfigFromPlan builds the tunnel config from a
// comma-separated "nodeid=host:port" plan.
//
// It is exported because cmd/managerd parses the plan once at startup,
// so a malformed plan is a startup error naming the offending entry
// rather than a per-tick reconciliation failure.
//
// The plan is read from the operator's own configuration, the same way
// ADR-0115's peer_tls_hostname_map is, rather than from a new
// replicated record: it describes host network state that the
// reconciler cannot itself own.
//
// A malformed entry is rejected rather than skipped. A silently
// ignored entry would look like a configured tunnel that is not
// actually covering every member, and the failure would show up later
// as a replication outage rather than as a configuration error.
func HASTTunnelConfigFromPlan(enabled bool, plan string) (HASTTunnelConfig, error) {
	//
	// The plan is read from the operator's own configuration, the same way
	// ADR-0115's peer_tls_hostname_map is, rather than from a new
	// replicated record: it describes host network state that the
	// reconciler cannot itself own.
	//
	// A malformed entry is rejected rather than skipped. A silently
	// ignored entry would look like a configured tunnel that is not
	// actually covering every member, and the failure would show up later
	// as a replication outage rather than as a configuration error.
	cfg := HASTTunnelConfig{Enabled: enabled}
	if !enabled {
		return cfg, nil
	}
	// The map must be non-nil before the first entry is stored: a
	// tunnel that is enabled with an empty plan is a real state (every
	// peer is then missing a tunnel address, and the reconciler refuses
	// accordingly), and it must not panic on the way there.
	cfg.Addrs = make(map[string]string)
	for _, entry := range strings.Split(plan, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		nodeID, addr, ok := strings.Cut(entry, "=")
		if !ok {
			return HASTTunnelConfig{}, fmt.Errorf("HAST tunnel plan entry %q is not in nodeid=host:port form", entry)
		}
		nodeID = strings.TrimSpace(nodeID)
		addr = strings.TrimSpace(addr)
		if nodeID == "" || addr == "" {
			return HASTTunnelConfig{}, fmt.Errorf("HAST tunnel plan entry %q has an empty node id or address", entry)
		}
		if _, _, err := net.SplitHostPort(addr); err != nil {
			return HASTTunnelConfig{}, fmt.Errorf("HAST tunnel plan entry %q address %q is not host:port: %w", entry, addr, err)
		}
		cfg.Addrs[nodeID] = addr
	}
	return cfg, nil
}
