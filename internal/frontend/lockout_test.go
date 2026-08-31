package frontend

import (
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
