package manager

import (
	"sync"
	"time"
)

// pamLockoutTracker enforces a simple per-username lockout after
// repeated failed AuthenticatePassword attempts (ADR-0096) - mirrors
// internal/frontend's own loginAttemptTracker exactly (duplicated
// rather than shared, matching this project's established convention
// of small duplication across independent packages over a shared-but-
// farther-away helper, e.g. peer.go's apiKeyCredentials).
//
// Without this, AuthenticatePassword's own lockout protection lived
// only in internal/frontend's handleLogin - the intended caller, but
// not the only one that can reach this RPC. AuthenticatePassword is
// deliberately exempted from checkAuth (a caller here has no API key
// yet, by definition), so any network client that can reach managerd's
// gRPC port directly could call it in a tight loop, bypassing
// frontend's lockout entirely and brute-forcing a real PAM/UNIX
// account with no rate limit at all. Tracking lockout state here too
// closes that bypass at the actual trust boundary.
type pamLockoutTracker struct {
	mu           sync.Mutex
	maxAttempts  int
	window       time.Duration
	lockDuration time.Duration
	attempts     map[string]*pamAttemptState
	lastSweep    time.Time
}

type pamAttemptState struct {
	count       int
	windowStart time.Time
	lockedUntil time.Time
}

// defaultPAMMaxFailedAttempts/defaultPAMAttemptWindow/
// defaultPAMLockDuration mirror internal/frontend's own fixed,
// non-configurable defaults exactly (defaultMaxFailedAttempts/
// defaultAttemptWindow/defaultLockDuration).
const (
	defaultPAMMaxFailedAttempts = 5
	defaultPAMAttemptWindow     = 15 * time.Minute
	defaultPAMLockDuration      = 15 * time.Minute

	// maxTrackedUsernames bounds the tracker. AuthenticatePassword is
	// unauthenticated by design, so without a bound every distinct username
	// anyone submits would add an entry that lives for a full window.
	maxTrackedUsernames = 10000

	// maxLockoutUsernameLen is the longest username tracked or authenticated.
	// No real account name is anywhere near this long.
	maxLockoutUsernameLen = 256
)

// lockoutKey caps how much of a username is used as a map key, as a second
// line of defense behind the length check in AuthenticatePassword.
func lockoutKey(username string) string {
	if len(username) > maxLockoutUsernameLen {
		return username[:maxLockoutUsernameLen]
	}
	return username
}

func newPAMLockoutTracker() *pamLockoutTracker {
	return &pamLockoutTracker{
		maxAttempts:  defaultPAMMaxFailedAttempts,
		window:       defaultPAMAttemptWindow,
		lockDuration: defaultPAMLockDuration,
		attempts:     make(map[string]*pamAttemptState),
	}
}

// Locked reports whether username is currently locked out, and for how
// much longer.
func (t *pamLockoutTracker) Locked(username string) (locked bool, remaining time.Duration) {
	t.mu.Lock()
	defer t.mu.Unlock()
	st, ok := t.attempts[lockoutKey(username)]
	if !ok {
		return false, 0
	}
	now := time.Now()
	if now.Before(st.lockedUntil) {
		return true, st.lockedUntil.Sub(now)
	}
	return false, 0
}

// RecordFailure records a failed attempt for username, locking it out
// once maxAttempts is reached within window. Also opportunistically
// sweeps other entries whose window has elapsed and whose lock (if
// any) has expired, keeping the map bounded to currently-relevant
// entries.
func (t *pamLockoutTracker) RecordFailure(username string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := time.Now()
	username = lockoutKey(username)

	st, ok := t.attempts[username]
	if !ok || now.Sub(st.windowStart) > t.window {
		st = &pamAttemptState{windowStart: now}
		t.attempts[username] = st
	}
	st.count++
	if st.count >= t.maxAttempts {
		st.lockedUntil = now.Add(t.lockDuration)
	}

	t.sweepLocked(now, username)
}

// sweepLocked drops entries whose window and lock have both elapsed, but
// only when the map is at its cap or a quarter of a window has passed since
// the last sweep - not on every failure, which let an attacker with many
// distinct usernames make each request scan the whole map under the lock. If
// the map is still over its cap, the oldest unlocked entries are evicted.
// Locked entries are never evicted.
func (t *pamLockoutTracker) sweepLocked(now time.Time, keep string) {
	if len(t.attempts) <= maxTrackedUsernames && now.Sub(t.lastSweep) < t.window/4 {
		return
	}
	t.lastSweep = now
	for u, s := range t.attempts {
		if u == keep {
			continue
		}
		if now.Sub(s.windowStart) > t.window && now.After(s.lockedUntil) {
			delete(t.attempts, u)
		}
	}
	for len(t.attempts) > maxTrackedUsernames {
		oldest := ""
		var oldestStart time.Time
		for u, s := range t.attempts {
			if u == keep || now.Before(s.lockedUntil) {
				continue
			}
			if oldest == "" || s.windowStart.Before(oldestStart) {
				oldest, oldestStart = u, s.windowStart
			}
		}
		if oldest == "" {
			return
		}
		delete(t.attempts, oldest)
	}
}

// RecordSuccess clears any tracked failures for username.
func (t *pamLockoutTracker) RecordSuccess(username string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.attempts, lockoutKey(username))
}
