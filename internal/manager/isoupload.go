package manager

import (
	"context"
	"fmt"
	"net"
	"sync"

	"google.golang.org/grpc/peer"
)

// maxConcurrentISOUploads/maxConcurrentISOUploadsPerCaller bound
// UploadISO (2026-09-29 security audit, "ISO upload can exhaust host
// disk space" - the size/free-space guards live in isostore.Save
// itself; this is the other half, concurrency). Small, fixed numbers
// rather than configurable: this project's own actual deployments are a
// handful of hosts each serving a handful of operators, and an ISO
// upload is a large, deliberate, occasional action, not a hot path that
// needs headroom.
const (
	maxConcurrentISOUploads          = 4
	maxConcurrentISOUploadsPerCaller = 2
	unknownISOUploadCaller           = "unknown"
)

// isoUploadLimiter bounds concurrent UploadISO streams globally and per
// caller, mirroring pamLockoutTracker's mutex-plus-map shape. Callers
// are identified by remote host rather than API key id: UploadISO is
// reached over AuthStreamInterceptor, which (unlike
// AuthUnaryInterceptor) does not thread callerAPIKeyIDContextKey onto
// the stream's context, and a deployment with API-key auth never enabled
// - ADR-0023's explicitly supported "opt-in, non-breaking" state - has
// no key id to key on at all. The remote host is available
// unconditionally and degrades safely: distinct real callers are almost
// always distinct hosts, and callers that do share one (behind a NAT, or
// one operator on one machine) only end up sharing a tighter effective
// limit, never a looser one.
type isoUploadLimiter struct {
	mu     sync.Mutex
	total  int
	byPeer map[string]int
}

func newISOUploadLimiter() *isoUploadLimiter {
	return &isoUploadLimiter{byPeer: make(map[string]int)}
}

// acquire reserves one of the upload slots for caller, or refuses with a
// clear, actionable error if the global or per-caller cap is already
// reached. Every successful acquire must be paired with exactly one
// release.
func (l *isoUploadLimiter) acquire(caller string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.total >= maxConcurrentISOUploads {
		return fmt.Errorf("manager: too many concurrent ISO uploads in progress (%d); try again once one finishes", l.total)
	}
	if l.byPeer[caller] >= maxConcurrentISOUploadsPerCaller {
		return fmt.Errorf("manager: too many concurrent ISO uploads from %s (%d); try again once one finishes", caller, l.byPeer[caller])
	}
	l.total++
	l.byPeer[caller]++
	return nil
}

func (l *isoUploadLimiter) release(caller string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.total--
	l.byPeer[caller]--
	if l.byPeer[caller] <= 0 {
		delete(l.byPeer, caller)
	}
}

// isoUploadCaller identifies UploadISO's caller for isoUploadLimiter,
// from the stream context's grpc/peer address.
//
// The port is deliberately dropped. A TCP peer address is host:port, and
// every upload from one machine gets a fresh ephemeral source port, so
// keying on the whole address would give each concurrent upload its own
// private bucket and the per-caller cap could never fire. Keying on the
// host matches internal/frontend's consoleTunnelCaller, and it makes the
// doc comment's claim true in the right direction: callers that share a
// host (a NAT, or one operator's own machine) also share the cap.
func isoUploadCaller(ctx context.Context) string {
	p, ok := peer.FromContext(ctx)
	if !ok || p.Addr == nil {
		// No real network connection - a same-process test harness,
		// chiefly. One shared bucket rather than panicking or
		// disabling the limit.
		return unknownISOUploadCaller
	}
	if host, _, err := net.SplitHostPort(p.Addr.String()); err == nil {
		return host
	}
	return p.Addr.String()
}
