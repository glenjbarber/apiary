package deadman

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeAtRunner replaces execAtRunner in tests - no real at(8)/atq(1)/
// atrm(1) calls, matching the fake-injection convention
// internal/cluster's own vlanManager/pfManager fakes follow.
type fakeAtRunner struct {
	nextJobID int
	pending   map[int]bool
	atErr     error
	atqErr    error
	atrmErr   error

	scheduled []string // scripts passed to at, in call order
}

func newFakeAtRunner() *fakeAtRunner {
	return &fakeAtRunner{nextJobID: 1, pending: map[int]bool{}}
}

func (f *fakeAtRunner) at(_ context.Context, script string, _ time.Duration) (int, error) {
	if f.atErr != nil {
		return 0, f.atErr
	}
	id := f.nextJobID
	f.nextJobID++
	f.pending[id] = true
	f.scheduled = append(f.scheduled, script)
	return id, nil
}

func (f *fakeAtRunner) atq(_ context.Context) (string, error) {
	if f.atqErr != nil {
		return "", f.atqErr
	}
	var lines []string
	for id, ok := range f.pending {
		if ok {
			lines = append(lines, fmt.Sprintf("%d\tSun Jan  1 00:00:00 2026 a root", id))
		}
	}
	return strings.Join(lines, "\n"), nil
}

func (f *fakeAtRunner) atrm(_ context.Context, jobID int) error {
	if f.atrmErr != nil {
		return f.atrmErr
	}
	if !f.pending[jobID] {
		return fmt.Errorf("atrm: %d: No such job", jobID)
	}
	delete(f.pending, jobID)
	return nil
}

func newTestManager(t *testing.T, runner atRunner) *Manager {
	t.Helper()
	return &Manager{StateDir: t.TempDir(), runner: runner}
}

func TestManager_ArmTapRevert_SchedulesJobTargetingOnlyTheGivenTap(t *testing.T) {
	fake := newFakeAtRunner()
	m := newTestManager(t, fake)

	if err := m.ArmTapRevert(context.Background(), "bridge0", "tap3"); err != nil {
		t.Fatalf("ArmTapRevert() error: %v", err)
	}
	if len(fake.scheduled) != 1 {
		t.Fatalf("scheduled = %v, want exactly one job", fake.scheduled)
	}
	if !strings.Contains(fake.scheduled[0], "bridge0") || !strings.Contains(fake.scheduled[0], "tap3") {
		t.Errorf("scheduled script = %q, want it to reference bridge0 and tap3", fake.scheduled[0])
	}
	if strings.Contains(fake.scheduled[0], "re0") {
		t.Errorf("scheduled script = %q, must never reference the uplink NIC itself", fake.scheduled[0])
	}
	if len(fake.pending) != 1 {
		t.Errorf("pending jobs = %v, want exactly one armed", fake.pending)
	}
}

func TestManager_ArmTapRevert_IdempotentWhileJobStillPending(t *testing.T) {
	fake := newFakeAtRunner()
	m := newTestManager(t, fake)

	if err := m.ArmTapRevert(context.Background(), "bridge0", "tap3"); err != nil {
		t.Fatalf("first ArmTapRevert() error: %v", err)
	}
	if err := m.ArmTapRevert(context.Background(), "bridge0", "tap3"); err != nil {
		t.Fatalf("second ArmTapRevert() error: %v", err)
	}
	if len(fake.scheduled) != 1 {
		t.Errorf("scheduled = %v, want still exactly one job - re-arming while pending must be a no-op", fake.scheduled)
	}
}

func TestManager_ArmTapRevert_ReArmsAfterJobNoLongerPending(t *testing.T) {
	fake := newFakeAtRunner()
	m := newTestManager(t, fake)

	if err := m.ArmTapRevert(context.Background(), "bridge0", "tap3"); err != nil {
		t.Fatalf("first ArmTapRevert() error: %v", err)
	}
	// Simulate the job having already fired (removed from atq's own
	// queue) without ConfirmBridgeHealthy ever having been called - the
	// state file still names the stale job ID.
	fake.pending = map[int]bool{}

	if err := m.ArmTapRevert(context.Background(), "bridge0", "tap3"); err != nil {
		t.Fatalf("second ArmTapRevert() error: %v", err)
	}
	if len(fake.scheduled) != 2 {
		t.Errorf("scheduled = %v, want a fresh job once the previous one is no longer pending", fake.scheduled)
	}
}

func TestManager_ConfirmBridgeHealthy_CancelsExactlyTheArmedJob(t *testing.T) {
	fake := newFakeAtRunner()
	m := newTestManager(t, fake)

	if err := m.ArmTapRevert(context.Background(), "bridge0", "tap3"); err != nil {
		t.Fatalf("ArmTapRevert() error: %v", err)
	}
	if len(fake.pending) != 1 {
		t.Fatalf("test setup: expected exactly one armed job")
	}

	if err := m.ConfirmBridgeHealthy(context.Background(), "bridge0", "tap3"); err != nil {
		t.Fatalf("ConfirmBridgeHealthy() error: %v", err)
	}
	if len(fake.pending) != 0 {
		t.Errorf("pending jobs = %v, want none after confirming healthy", fake.pending)
	}
}

func TestManager_ConfirmBridgeHealthy_NoOpWhenNothingArmed(t *testing.T) {
	fake := newFakeAtRunner()
	m := newTestManager(t, fake)

	if err := m.ConfirmBridgeHealthy(context.Background(), "bridge0", "tap3"); err != nil {
		t.Fatalf("ConfirmBridgeHealthy() error: %v, want nil for a tap with nothing armed", err)
	}
}

// TestManager_ConfirmThenArmAgain_SchedulesFreshJob confirms a tap that
// was confirmed healthy and later rejoins the bridge (a new VM reusing
// the same tap name, or a genuinely new arm call) gets a real, freshly
// armed job again - the switch never goes permanently unprotected after
// its first successful confirm.
func TestManager_ConfirmThenArmAgain_SchedulesFreshJob(t *testing.T) {
	fake := newFakeAtRunner()
	m := newTestManager(t, fake)

	if err := m.ArmTapRevert(context.Background(), "bridge0", "tap3"); err != nil {
		t.Fatalf("first ArmTapRevert() error: %v", err)
	}
	if err := m.ConfirmBridgeHealthy(context.Background(), "bridge0", "tap3"); err != nil {
		t.Fatalf("ConfirmBridgeHealthy() error: %v", err)
	}
	if err := m.ArmTapRevert(context.Background(), "bridge0", "tap3"); err != nil {
		t.Fatalf("second ArmTapRevert() error: %v", err)
	}
	if len(fake.scheduled) != 2 {
		t.Errorf("scheduled = %v, want two separate jobs across the confirm/re-arm cycle", fake.scheduled)
	}
	if len(fake.pending) != 1 {
		t.Errorf("pending jobs = %v, want exactly one armed after re-arming", fake.pending)
	}
}

func TestManager_ArmTapRevert_DifferentTapsOnSameBridgeAreIndependent(t *testing.T) {
	fake := newFakeAtRunner()
	m := newTestManager(t, fake)

	if err := m.ArmTapRevert(context.Background(), "bridge0", "tap3"); err != nil {
		t.Fatalf("ArmTapRevert(tap3) error: %v", err)
	}
	if err := m.ArmTapRevert(context.Background(), "bridge0", "tap4"); err != nil {
		t.Fatalf("ArmTapRevert(tap4) error: %v", err)
	}
	if err := m.ConfirmBridgeHealthy(context.Background(), "bridge0", "tap3"); err != nil {
		t.Fatalf("ConfirmBridgeHealthy(tap3) error: %v", err)
	}
	if len(fake.pending) != 1 {
		t.Errorf("pending jobs = %v, want tap4's own job untouched by confirming tap3", fake.pending)
	}
}

func TestManager_ArmTapRevert_RequiresStateDir(t *testing.T) {
	m := &Manager{runner: newFakeAtRunner()}
	if err := m.ArmTapRevert(context.Background(), "bridge0", "tap3"); err == nil {
		t.Fatal("ArmTapRevert() error = nil, want a StateDir-required rejection")
	}
}

func TestManager_StatePersistsAcrossManagerInstances(t *testing.T) {
	dir := t.TempDir()
	fake := newFakeAtRunner()
	m1 := &Manager{StateDir: dir, runner: fake}
	if err := m1.ArmTapRevert(context.Background(), "bridge0", "tap3"); err != nil {
		t.Fatalf("ArmTapRevert() error: %v", err)
	}

	// A fresh Manager (e.g. after a managerd restart) sharing the same
	// StateDir and runner must still find and cancel the same job.
	m2 := &Manager{StateDir: dir, runner: fake}
	if err := m2.ConfirmBridgeHealthy(context.Background(), "bridge0", "tap3"); err != nil {
		t.Fatalf("ConfirmBridgeHealthy() on a fresh Manager error: %v", err)
	}
	if len(fake.pending) != 0 {
		t.Errorf("pending jobs = %v, want none - state must survive across Manager instances via StateDir", fake.pending)
	}
}

func TestManager_StatePath_IsScopedPerBridgeAndTap(t *testing.T) {
	m := &Manager{StateDir: "/var/db/apiary/deadman"}
	got := m.statePath("bridge0", "tap3")
	want := filepath.Join("/var/db/apiary/deadman", "deadman-bridge0-tap3.json")
	if got != want {
		t.Errorf("statePath() = %q, want %q", got, want)
	}
}
