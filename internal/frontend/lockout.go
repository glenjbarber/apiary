package frontend

import (
	"sync"
	"time"
)

// loginAttemptTracker enforces a simple per-username lockout after
// repeated failed login attempts - ADR-0030's own "Deferred" section
// named this explicitly as a real, accepted gap: once login checks a
// real account's real password (via PAM), nothing previously slowed
// down repeated online guesses against it.
//
// Keyed by username only, not source IP - deliberately simple,
// matching this project's single-admin/local-network stage (the same
// reasoning sessionTTL's own fixed, non-configurable value already
// applies). The accepted tradeoff: someone who already knows a valid
// username could lock out its real owner by failing on purpose (a
// denial-of-service against that one account, not a way to actually
// get in) - a real cost, but preferred over doing nothing at all
// against online password guessing now that a wrong guess is checked
// against a real credential instead of never mattering.
type loginAttemptTracker struct {
	mu           sync.Mutex
	maxAttempts  int
	window       time.Duration
	lockDuration time.Duration
	attempts     map[string]*attemptState
	lastSweep    time.Time
}

type attemptState struct {
	count       int
	windowStart time.Time
	lockedUntil time.Time
}

// defaultMaxFailedAttempts/defaultAttemptWindow/defaultLockDuration
// are fixed, not configurable via flag - the same "simple enough for
// now" posture as sessionTTL.
const (
	defaultMaxFailedAttempts = 5
	defaultAttemptWindow     = 15 * time.Minute
	defaultLockDuration      = 15 * time.Minute

	// maxTrackedUsernames bounds the tracker: the login form is reachable
	// without authenticating, so every distinct username anyone submits would
	// otherwise add an entry that lives for a full window.
	maxTrackedUsernames = 10000

	// maxLoginUsernameLen is the longest username tracked or authenticated.
	maxLoginUsernameLen = 256

	// maxLoginFormBytes caps the login form body. Without it ParseForm accepts
	// up to 10 MB from an unauthenticated client.
	maxLoginFormBytes = 64 << 10
)

// lockoutKey caps how much of a username is used as a map key.
func lockoutKey(username string) string {
	if len(username) > maxLoginUsernameLen {
		return username[:maxLoginUsernameLen]
	}
	return username
}

func newLoginAttemptTracker(maxAttempts int, window, lockDuration time.Duration) *loginAttemptTracker {
	return &loginAttemptTracker{
		maxAttempts:  maxAttempts,
		window:       window,
		lockDuration: lockDuration,
		attempts:     make(map[string]*attemptState),
	}
}

// Locked reports whether username is currently locked out, and for how
// much longer.
func (t *loginAttemptTracker) Locked(username string) (locked bool, remaining time.Duration) {
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
// entries rather than growing forever across every username anyone
// has ever mistyped.
func (t *loginAttemptTracker) RecordFailure(username string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := time.Now()
	username = lockoutKey(username)

	st, ok := t.attempts[username]
	if !ok || now.Sub(st.windowStart) > t.window {
		st = &attemptState{windowStart: now}
		t.attempts[username] = st
	}
	st.count++
	if st.count >= t.maxAttempts {
		st.lockedUntil = now.Add(t.lockDuration)
	}

	t.sweepLocked(now, username)
}

// sweepLocked drops entries whose window and lock have both elapsed, but only
// when the map is at its cap or a quarter of a window has passed since the
// last sweep - not on every failure, which let a client submitting many
// distinct usernames make each request scan the whole map under the lock. If
// the map is still over its cap, the oldest unlocked entries are evicted.
// Locked entries are never evicted.
func (t *loginAttemptTracker) sweepLocked(now time.Time, keep string) {
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

// RecordSuccess clears any tracked failures for username - a
// successful login is a strong signal the account is legitimately in
// use again, so past failures shouldn't linger toward some future
// lockout.
func (t *loginAttemptTracker) RecordSuccess(username string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.attempts, lockoutKey(username))
}
