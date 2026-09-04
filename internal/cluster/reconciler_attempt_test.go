package cluster

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/glenjbarber/apiary/api/internalpb"
)

func TestRunOnce_RecordsAttemptEvenWhenListFails(t *testing.T) {
	raft := &fakeRaftClient{err: errors.New("raftd unreachable")}
	r := &Reconciler{Raft: raft, ZFS: newFakeDatasetManager(), LocalNodeID: "node-a"}

	if _, ok := r.LastReconcileAttempt(); ok {
		t.Fatal("LastReconcileAttempt() ok = true before any RunOnce call")
	}

	before := time.Now()
	if err := r.RunOnce(context.Background()); err == nil {
		t.Fatal("RunOnce() error = nil, want the ListVMsLocal failure surfaced")
	}
	after := time.Now()

	attemptAt, ok := r.LastReconcileAttempt()
	if !ok {
		t.Fatal("LastReconcileAttempt() ok = false after a failed RunOnce - an attempt must still be recorded")
	}
	if attemptAt.Before(before) || attemptAt.After(after) {
		t.Errorf("LastReconcileAttempt() = %v, want between %v and %v", attemptAt, before, after)
	}
	if _, ok := r.LastReconcileSuccess(); ok {
		t.Error("LastReconcileSuccess() ok = true after a failed RunOnce - success must not be recorded")
	}
}

func TestRunOnce_RecordsSuccessOnlyWhenFirstErrNil(t *testing.T) {
	// ListVMsLocal itself succeeds, but the one VM's dataset creation
	// fails - that becomes RunOnce's firstErr, so the whole tick is "not
	// successful" even though the initial fetch worked fine.
	raft := &fakeRaftClient{resp: &internalpb.ListVMsResponse{
		Vms: []*internalpb.VMDefinition{{Id: "vm-1", NodeId: "node-a"}},
	}}
	zfs := newFakeDatasetManager()
	zfs.createErr = errors.New("disk full")
	r := &Reconciler{Raft: raft, ZFS: zfs, LocalNodeID: "node-a"}

	if err := r.RunOnce(context.Background()); err == nil {
		t.Fatal("RunOnce() error = nil, want the underlying CreateDataset error surfaced")
	}

	if _, ok := r.LastReconcileAttempt(); !ok {
		t.Error("LastReconcileAttempt() ok = false, want an attempt recorded")
	}
	if _, ok := r.LastReconcileSuccess(); ok {
		t.Error("LastReconcileSuccess() ok = true, want no success recorded - firstErr was non-nil")
	}
}

func TestRunOnce_SuccessMonotonicWithAttempt(t *testing.T) {
	failing := &fakeRaftClient{resp: &internalpb.ListVMsResponse{
		Vms: []*internalpb.VMDefinition{{Id: "vm-1", NodeId: "node-a"}},
	}}
	failingZFS := newFakeDatasetManager()
	failingZFS.createErr = errors.New("disk full")
	r := &Reconciler{Raft: failing, ZFS: failingZFS, LocalNodeID: "node-a"}

	if err := r.RunOnce(context.Background()); err == nil {
		t.Fatal("expected the first RunOnce to fail")
	}
	attemptAfterFail, _ := r.LastReconcileAttempt()
	if _, ok := r.LastReconcileSuccess(); ok {
		t.Fatal("no success should be recorded yet")
	}

	// Swap in a raft client and ZFS manager that reconcile cleanly.
	time.Sleep(time.Millisecond)
	r.Raft = &fakeRaftClient{resp: &internalpb.ListVMsResponse{
		Vms: []*internalpb.VMDefinition{{Id: "vm-2", NodeId: "node-a"}},
	}}
	r.ZFS = newFakeDatasetManager()
	if err := r.RunOnce(context.Background()); err != nil {
		t.Fatalf("expected the second RunOnce to succeed, got: %v", err)
	}
	attemptAfterSuccess, _ := r.LastReconcileAttempt()
	successAt, ok := r.LastReconcileSuccess()
	if !ok {
		t.Fatal("LastReconcileSuccess() ok = false after a successful RunOnce")
	}

	if !attemptAfterSuccess.After(attemptAfterFail) {
		t.Errorf("attempt timestamp did not advance: %v -> %v", attemptAfterFail, attemptAfterSuccess)
	}
	if successAt.Before(attemptAfterFail) {
		t.Errorf("success timestamp %v predates the first (failed) attempt %v", successAt, attemptAfterFail)
	}
	// Within the second (successful) tick, attempt is recorded at the
	// very top and success via a deferred check after all the tick's
	// work completes - so success's own timestamp is at-or-after that
	// same tick's attempt timestamp, never before it.
	if successAt.Before(attemptAfterSuccess) {
		t.Errorf("LastReconcileSuccess (%v) must be >= that same tick's LastReconcileAttempt (%v)", successAt, attemptAfterSuccess)
	}
}

func TestReconciler_ReconcileIntervalAccessor(t *testing.T) {
	r := &Reconciler{Interval: 45 * time.Second}
	if got := r.ReconcileInterval(); got != 45*time.Second {
		t.Errorf("ReconcileInterval() = %v, want 45s", got)
	}
}
