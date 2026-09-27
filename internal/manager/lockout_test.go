package manager

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	rpcpb "github.com/glenjbarber/apiary/api/rpc"
)

func TestPAMLockoutTracker_MapIsBoundedByDistinctUsernames(t *testing.T) {
	tr := newPAMLockoutTracker()
	for i := 0; i < maxTrackedUsernames+500; i++ {
		tr.RecordFailure(fmt.Sprintf("user-%d", i))
	}
	if n := len(tr.attempts); n > maxTrackedUsernames {
		t.Errorf("tracked %d usernames, want at most %d", n, maxTrackedUsernames)
	}
}

func TestPAMLockoutTracker_EvictionNeverDropsALockedAccount(t *testing.T) {
	tr := newPAMLockoutTracker()
	for i := 0; i < defaultPAMMaxFailedAttempts; i++ {
		tr.RecordFailure("victim")
	}
	if locked, _ := tr.Locked("victim"); !locked {
		t.Fatalf("victim should be locked after %d failures", defaultPAMMaxFailedAttempts)
	}
	for i := 0; i < maxTrackedUsernames+500; i++ {
		tr.RecordFailure(fmt.Sprintf("junk-%d", i))
	}
	if locked, _ := tr.Locked("victim"); !locked {
		t.Errorf("a locked account was evicted by a flood of other usernames")
	}
}

func TestPAMLockoutTracker_DoesNotSweepOnEveryFailure(t *testing.T) {
	tr := newPAMLockoutTracker()
	tr.RecordFailure("a")
	swept := tr.lastSweep
	tr.RecordFailure("b")
	tr.RecordFailure("c")
	if !tr.lastSweep.Equal(swept) {
		t.Errorf("lastSweep changed between back-to-back failures; the whole map is being rescanned on every failure")
	}
}

func TestPAMLockoutTracker_ExpiredEntriesAreSweptEventually(t *testing.T) {
	tr := newPAMLockoutTracker()
	tr.RecordFailure("old")
	tr.attempts["old"].windowStart = time.Now().Add(-2 * defaultPAMAttemptWindow)
	tr.lastSweep = time.Now().Add(-defaultPAMAttemptWindow)
	tr.RecordFailure("new")
	if _, ok := tr.attempts["old"]; ok {
		t.Errorf("an entry past its window was never swept")
	}
}

type alwaysFailPAM struct{ calls int }

func (a *alwaysFailPAM) Authenticate(string, string) (bool, error) { a.calls++; return false, nil }

func TestServer_AuthenticatePassword_RejectsOversizedUsernameWithoutTracking(t *testing.T) {
	s := NewServer(nil, "node-1", nil, nil, nil, nil, nil, "", nil, nil, nil, 0, nil)
	pam := &alwaysFailPAM{}
	s.authPAM = pam

	resp, err := s.AuthenticatePassword(context.Background(), &rpcpb.AuthenticatePasswordRequest{Username: strings.Repeat("u", maxLockoutUsernameLen+1), Password: "x"})
	if err != nil || resp.GetOk() {
		t.Fatalf("AuthenticatePassword() = %v, %v; want a plain failure", resp, err)
	}
	if pam.calls != 0 {
		t.Errorf("PAM was consulted for an oversized username")
	}
	if n := len(s.pamLockouts.attempts); n != 0 {
		t.Errorf("an oversized username was tracked (%d entries), want none", n)
	}
}
