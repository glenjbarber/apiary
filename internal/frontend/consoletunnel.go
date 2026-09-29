package frontend

import (
	"fmt"
	"net"
	"net/http"
	"sync"
)

// maxConcurrentConsoleTunnels/maxConcurrentConsoleTunnelsPerCaller bound
// handleConsoleWS (2026-09-29 security audit, "console WebSocket accepts
// unbounded messages and connections" - the message-size/keepalive half
// of that finding lives in console.go's proxyConsole; this is the other
// half, connection count). Small, fixed numbers: this project's own
// actual deployments are a handful of hosts with a handful of VMs each,
// and an operator legitimately watching a few consoles at once is the
// normal case this still needs to allow room for.
const (
	maxConcurrentConsoleTunnels          = 8
	maxConcurrentConsoleTunnelsPerCaller = 3
)

// consoleTunnelLimiter bounds concurrent console WebSocket tunnels
// globally and per caller, mirroring internal/manager's isoUploadLimiter
// shape (duplicated rather than shared - a small tracker like this one,
// across independent packages, matches this project's established
// convention, e.g. loginAttemptTracker/pamLockoutTracker).
type consoleTunnelLimiter struct {
	mu     sync.Mutex
	total  int
	byUser map[string]int
}

func newConsoleTunnelLimiter() *consoleTunnelLimiter {
	return &consoleTunnelLimiter{byUser: make(map[string]int)}
}

// acquire reserves one of the console-tunnel slots for caller, or
// refuses with a clear, actionable error if the global or per-caller cap
// is already reached. Every successful acquire must be paired with
// exactly one release.
func (l *consoleTunnelLimiter) acquire(caller string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.total >= maxConcurrentConsoleTunnels {
		return fmt.Errorf("frontend: too many console connections open on this frontend (%d); close one and try again", l.total)
	}
	if l.byUser[caller] >= maxConcurrentConsoleTunnelsPerCaller {
		return fmt.Errorf("frontend: too many console connections already open for %s (%d); close one and try again", caller, l.byUser[caller])
	}
	l.total++
	l.byUser[caller]++
	return nil
}

func (l *consoleTunnelLimiter) release(caller string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.total--
	l.byUser[caller]--
	if l.byUser[caller] <= 0 {
		delete(l.byUser, caller)
	}
}

// consoleTunnelCaller identifies handleConsoleWS's caller for
// consoleTunnelLimiter: the logged-in session's username when login is
// enabled and a valid session is present, falling back to the request's
// remote IP otherwise (ADR-0014's login stays opt-in, so there is often
// no session to key on at all). The fallback strips the ephemeral source
// port from r.RemoteAddr - every distinct TCP connection (a second
// browser tab, a reconnect) gets its own port, so keying on the whole
// host:port would make each one count as a different caller and the
// per-caller cap would never actually bind.
func consoleTunnelCaller(s *Server, r *http.Request) string {
	if sess, ok := s.currentSession(r); ok && sess.username != "" {
		return sess.username
	}
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}
