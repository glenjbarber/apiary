package frontend

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestLoginAttemptTracker_NotLockedInitially(t *testing.T) {
	tr := newLoginAttemptTracker(3, time.Minute, time.Minute)
	if locked, _ := tr.Locked("alice"); locked {
		t.Errorf("Locked() = true for a username with no attempts, want false")
	}
}

func TestLoginAttemptTracker_LocksAfterMaxFailures(t *testing.T) {
	tr := newLoginAttemptTracker(3, time.Minute, time.Minute)
	for i := 0; i < 2; i++ {
		tr.RecordFailure("alice")
		if locked, _ := tr.Locked("alice"); locked {
			t.Fatalf("Locked() = true after %d failure(s), want false (threshold is 3)", i+1)
		}
	}
	tr.RecordFailure("alice")
	locked, remaining := tr.Locked("alice")
	if !locked {
		t.Fatalf("Locked() = false after 3 failures, want true")
	}
	if remaining <= 0 || remaining > time.Minute {
		t.Errorf("remaining = %v, want a positive duration up to the lock duration", remaining)
	}
}

func TestLoginAttemptTracker_DoesNotLockDifferentUsername(t *testing.T) {
	tr := newLoginAttemptTracker(3, time.Minute, time.Minute)
	for i := 0; i < 3; i++ {
		tr.RecordFailure("alice")
	}
	if locked, _ := tr.Locked("bob"); locked {
		t.Errorf("Locked(bob) = true after only alice's failures, want false")
	}
}

func TestLoginAttemptTracker_SuccessClearsFailures(t *testing.T) {
	tr := newLoginAttemptTracker(3, time.Minute, time.Minute)
	tr.RecordFailure("alice")
	tr.RecordFailure("alice")
	tr.RecordSuccess("alice")
	tr.RecordFailure("alice")
	if locked, _ := tr.Locked("alice"); locked {
		t.Errorf("Locked() = true after a success reset the count, want false (only 1 failure since)")
	}
}

func TestLoginAttemptTracker_OldFailuresOutsideWindowDoNotCount(t *testing.T) {
	tr := newLoginAttemptTracker(3, 10*time.Millisecond, time.Minute)
	tr.RecordFailure("alice")
	tr.RecordFailure("alice")
	time.Sleep(20 * time.Millisecond)
	// This failure starts a fresh window - the first two are stale.
	tr.RecordFailure("alice")
	if locked, _ := tr.Locked("alice"); locked {
		t.Errorf("Locked() = true, want false - the first two failures should have aged out of the window")
	}
}

func TestLoginAttemptTracker_LockExpiresAfterDuration(t *testing.T) {
	tr := newLoginAttemptTracker(1, time.Minute, 10*time.Millisecond)
	tr.RecordFailure("alice")
	if locked, _ := tr.Locked("alice"); !locked {
		t.Fatalf("Locked() = false immediately after the triggering failure, want true")
	}
	time.Sleep(20 * time.Millisecond)
	if locked, _ := tr.Locked("alice"); locked {
		t.Errorf("Locked() = true after the lock duration elapsed, want false")
	}
}

func TestLoginAttemptTracker_MapIsBoundedByDistinctUsernames(t *testing.T) {
	tr := newLoginAttemptTracker(5, time.Hour, time.Hour)
	for i := 0; i < maxTrackedUsernames+500; i++ {
		tr.RecordFailure(fmt.Sprintf("user-%d", i))
	}
	if n := len(tr.attempts); n > maxTrackedUsernames {
		t.Errorf("tracked %d usernames, want at most %d", n, maxTrackedUsernames)
	}
}

func TestLoginAttemptTracker_EvictionNeverDropsALockedAccount(t *testing.T) {
	tr := newLoginAttemptTracker(2, time.Hour, time.Hour)
	tr.RecordFailure("victim")
	tr.RecordFailure("victim")
	if locked, _ := tr.Locked("victim"); !locked {
		t.Fatalf("victim should be locked after 2 failures")
	}
	for i := 0; i < maxTrackedUsernames+500; i++ {
		tr.RecordFailure(fmt.Sprintf("junk-%d", i))
	}
	if locked, _ := tr.Locked("victim"); !locked {
		t.Errorf("a locked account was evicted by a flood of other usernames")
	}
}

func TestLoginAttemptTracker_DoesNotSweepOnEveryFailure(t *testing.T) {
	tr := newLoginAttemptTracker(5, time.Minute, time.Minute)
	tr.RecordFailure("a")
	swept := tr.lastSweep
	tr.RecordFailure("b")
	tr.RecordFailure("c")
	if !tr.lastSweep.Equal(swept) {
		t.Errorf("lastSweep changed between back-to-back failures; the whole map is being rescanned on every failure")
	}
}

func TestLoginAttemptTracker_OversizedUsernameIsKeyedByItsPrefix(t *testing.T) {
	tr := newLoginAttemptTracker(2, time.Minute, time.Minute)
	long := strings.Repeat("x", 100000)
	tr.RecordFailure(long)
	tr.RecordFailure(long + "different-tail")
	for k := range tr.attempts {
		if len(k) > maxLoginUsernameLen {
			t.Errorf("tracker stored a %d byte key, want at most %d", len(k), maxLoginUsernameLen)
		}
	}
	if locked, _ := tr.Locked(long); !locked {
		t.Errorf("two failures with the same %d byte prefix should share one lockout key", maxLoginUsernameLen)
	}
}

func TestHandleLogin_RejectsOversizedUsernameAndBodyWithoutTracking(t *testing.T) {
	s := newTestServerWithAuth(t, &fakeClient{}, "admin", "secret")

	form := url.Values{"username": {strings.Repeat("u", maxLoginUsernameLen+1)}, "password": {"x"}}
	req := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if !strings.Contains(rec.Body.String(), "invalid username or password") {
		t.Errorf("oversized username: want the generic invalid-credentials message, got: %s", rec.Body.String())
	}
	if n := len(s.lockouts.attempts); n != 0 {
		t.Errorf("an oversized username was tracked (%d entries), want none", n)
	}

	big := url.Values{"username": {"admin"}, "password": {strings.Repeat("p", maxLoginFormBytes*2)}}
	req = httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(big.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec = httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if !strings.Contains(rec.Body.String(), "invalid form") {
		t.Errorf("oversized body: want the form to be refused, got: %s", rec.Body.String())
	}
}
