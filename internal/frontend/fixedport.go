package frontend

import (
	"net"
	"strings"
)

// Apiary's own daemons each listen/dial on a fixed, documented port -
// raftd:17600, managerd:17700, frontend's own HTTP(S) listener:8080,
// restshimd's own HTTP(S) listener:8081 (see docs/bootstrap.md and
// every "usually port 17700"-style hint already scattered through this
// package's own form field titles). The web UI never lets an operator
// choose a different port for these fields: which host runs a given
// daemon varies across a fleet, but which port it listens/dials on
// does not, and a manually mistyped port here is a real, previously
// seen misconfiguration - not a hypothetical. Every such field is now
// host-only in the UI; withFixedPort appends the correct port
// server-side, so a request crafted outside the browser (not just an
// operator typing into an input) gains nothing by including one.
const (
	raftdListenerPort     = "17600"
	managerdListenerPort  = "17700"
	frontendListenerPort  = "8080"
	restshimdListenerPort = "8081"
)

// hostOnly strips a port from addr if present, returning addr
// unchanged if it has none (or is empty) - used both as a template
// function (populating a host-only field's value from a previously
// saved full host:port address) and internally by withFixedPort.
func hostOnly(addr string) string {
	if addr == "" {
		return addr
	}
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return addr
	}
	return host
}

// withFixedPort builds a host:port address from an operator-typed host
// value and this project's own fixed port for that service - never a
// port the caller supplied, even if one is accidentally present in
// host (e.g. pasted from a full address elsewhere). Empty host yields
// empty, so an unset field still round-trips to the daemon's own
// "clear this setting" behavior rather than becoming ":17700".
func withFixedPort(host, port string) string {
	host = strings.TrimSpace(host)
	if host == "" {
		return ""
	}
	host = hostOnly(host)
	return net.JoinHostPort(host, port)
}
