package main

import (
	"context"
	"testing"
	"time"

	rpcpb "github.com/glenjbarber/apiary/api/rpc"
	"github.com/glenjbarber/apiary/internal/manager"
)

// fakeRestartCompleter is a restartCompleter test double - no real raft,
// no real managerd - so confirmPendingRestartOnStartup's own retry/
// no-retry decision can be tested in isolation (issue #83).
type fakeRestartCompleter struct {
	calls int
	resp  *rpcpb.ConfirmRestartCompletedResponse
	err   error
}

func (f *fakeRestartCompleter) ConfirmRestartCompletedLocal(ctx context.Context, service, nodeID string, leaseID uint64) (*rpcpb.ConfirmRestartCompletedResponse, error) {
	f.calls++
	return f.resp, f.err
}

// Issue #83: a node with no restart-guardrail token provisioned can
// never have a forwarded confirmation succeed, since the token is read
// once at startup and cannot change mid-process. Retrying anyway was
// the "repeated confirmation attempts failing... noisily" behaviour the
// issue describes. With the token empty, this must not even try once.
func TestConfirmPendingRestartOnStartup_EmptyTokenDoesNotRetry(t *testing.T) {
	store := manager.NewRestartConfirmStore(t.TempDir())
	if err := store.Save(manager.PendingRestart{Service: restartGuardrailService, NodeID: "comb-a", LeaseID: 7}); err != nil {
		t.Fatal(err)
	}
	fake := &fakeRestartCompleter{err: context.DeadlineExceeded}

	confirmPendingRestartOnStartup(context.Background(), fake, store, "comb-a", "")

	if fake.calls != 0 {
		t.Errorf("ConfirmRestartCompletedLocal was called %d times with no token provisioned, want 0 (every attempt would fail identically)", fake.calls)
	}
	if _, found, err := store.Load(restartGuardrailService); err != nil {
		t.Fatal(err)
	} else if !found {
		t.Error("the pending record was cleared despite never confirming - a later startup, once the token is provisioned, must still find it")
	}
}

// No pending record at all is the overwhelmingly common startup case,
// and it must stay silent and cheap regardless of the token.
func TestConfirmPendingRestartOnStartup_NoPendingRecordIsANoOp(t *testing.T) {
	store := manager.NewRestartConfirmStore(t.TempDir())
	fake := &fakeRestartCompleter{}

	confirmPendingRestartOnStartup(context.Background(), fake, store, "comb-a", "a-real-token")

	if fake.calls != 0 {
		t.Errorf("ConfirmRestartCompletedLocal was called %d times with nothing pending, want 0", fake.calls)
	}
}

// With a real token, a confirmation that succeeds on the first attempt
// clears the pending record and never retries.
func TestConfirmPendingRestartOnStartup_SuccessClearsTheRecord(t *testing.T) {
	store := manager.NewRestartConfirmStore(t.TempDir())
	if err := store.Save(manager.PendingRestart{Service: restartGuardrailService, NodeID: "comb-a", LeaseID: 7}); err != nil {
		t.Fatal(err)
	}
	fake := &fakeRestartCompleter{resp: &rpcpb.ConfirmRestartCompletedResponse{}}

	confirmPendingRestartOnStartup(context.Background(), fake, store, "comb-a", "a-real-token")

	if fake.calls != 1 {
		t.Errorf("ConfirmRestartCompletedLocal was called %d times on an immediate success, want 1", fake.calls)
	}
	if _, found, err := store.Load(restartGuardrailService); err != nil {
		t.Fatal(err)
	} else if found {
		t.Error("the pending record was not cleared after a successful confirmation")
	}
}

// With a real token, a confirmation that keeps failing is still
// attempted (unlike the empty-token case above) - this is the normal,
// bounded retry path (ADR-0103's own "bounded, not indefinite" framing),
// untouched by the issue #83 fix. A short context deadline stands in for
// waiting out the full backoff window, so the test does not spend
// several real seconds re-proving a retry count the other tests above
// already establish is reachable.
func TestConfirmPendingRestartOnStartup_RealTokenStillAttempts(t *testing.T) {
	store := manager.NewRestartConfirmStore(t.TempDir())
	if err := store.Save(manager.PendingRestart{Service: restartGuardrailService, NodeID: "comb-a", LeaseID: 7}); err != nil {
		t.Fatal(err)
	}
	fake := &fakeRestartCompleter{err: context.DeadlineExceeded}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	confirmPendingRestartOnStartup(ctx, fake, store, "comb-a", "a-real-token")

	if fake.calls < 1 {
		t.Error("ConfirmRestartCompletedLocal was never called with a real token provisioned")
	}
	if _, found, err := store.Load(restartGuardrailService); err != nil {
		t.Fatal(err)
	} else if !found {
		t.Error("the pending record was cleared despite every attempt failing")
	}
}
