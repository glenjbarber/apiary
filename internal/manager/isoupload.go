package manager

import (
	"context"
	"fmt"
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
// are identified by remote network address rather than API key id:
// UploadISO is reached over AuthStreamInterceptor, which (unlike
// AuthUnaryInterceptor) does not thread callerAPIKeyIDContextKey onto
// the stream's context, and a deployment with API-key auth never
// enabled - ADR-0023's explicitly supported "opt-in, non-breaking"
// state - has no key id to key on at all. Remote address is available
// unconditionally and degrades safely: distinct real callers are almost
// always distinct addresses, and callers that do share one (behind a
// NAT, say) only end up sharing a tighter effective limit, never a
// looser one.
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
// from the stream context's grpc/peer address. Missing peer info (no
// real network connection - a same-process test harness, chiefly) falls
// back to a single shared bucket rather than panicking or disabling the
// limit.
func isoUploadCaller(ctx context.Context) string {
	if p, ok := peer.FromContext(ctx); ok && p.Addr != nil {
		return p.Addr.String()
	}
	return unknownISOUploadCaller
}
