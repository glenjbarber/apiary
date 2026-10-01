package raft

import (
	"bytes"
	"io"
	"strings"
	"testing"

	"github.com/hashicorp/raft"
	"google.golang.org/protobuf/proto"

	internalpb "github.com/glenjbarber/apiary/api/internalpb"
)

// ADR-0148's Colony-wide VM disk-size floor. What is worth testing here
// is not that a number is stored, it is what the floor REFUSES: a
// lowering, a zero, and a per-VM override below it. Those three are the
// whole requirement, and each of them is a case where the tempting
// implementation - clamp instead of refuse, treat zero as unset, check
// UpdateVM's full-replace payload without consulting what the record
// already held - is observably different and observably wrong.

// setFloorCmd is a SetColonyDiskSize command with an explicit set_by, so
// a test can assert that a refusal changed nothing at all rather than
// only that the value is wrong.
func setFloorCmd(floorMB uint64) *internalpb.Command {
	return &internalpb.Command{
		Op: &internalpb.Command_SetColonyDiskSize{
			SetColonyDiskSize: &internalpb.SetColonyDiskSize{
				SetBy:    "comb-1",
				SetByKey: "key-1",
				FloorMb:  floorMB,
				NowUnix:  1700000000,
			},
		},
	}
}

// applyFloor applies cmd and fails the test on any error, for the setup
// half of a test. The asserting half is the test body.
func applyFloor(t *testing.T, fsm *FSM, index uint64, floorMB uint64) *FSMApplyResult {
	t.Helper()
	res := fsm.Apply(&raft.Log{Index: index, Data: mustMarshalCommand(t, setFloorCmd(floorMB))}).(*FSMApplyResult)
	if res.Error != "" {
		t.Fatalf("SetColonyDiskSize(%d MB) at index %d failed unexpectedly: %s", floorMB, index, res.Error)
	}
	return res
}

// applyFloorExpectingRefusal applies a raise that must be refused and
// returns the error text, checking that the text names the number the
// operator asked for. An operator who cannot tell which number was
// wrong cannot fix anything, so a refusal that does not say is a
// refusal worth rejecting too.
func applyFloorExpectingRefusal(t *testing.T, fsm *FSM, index uint64, floorMB uint64) string {
	t.Helper()
	res := fsm.Apply(&raft.Log{Index: index, Data: mustMarshalCommand(t, setFloorCmd(floorMB))}).(*FSMApplyResult)
	if res.Error == "" {
		t.Fatalf("SetColonyDiskSize(%d MB) at index %d was accepted, want a refusal: the floor must never be reduced", floorMB, index)
	}
	if !strings.Contains(res.Error, "ColonyDiskSize") {
		t.Errorf("refusal does not name the command it refuses, so an operator reading the log cannot tell what was rejected: %q", res.Error)
	}
	return res.Error
}

func TestSetColonyDiskSizeStoresTheFloor(t *testing.T) {
	fsm := NewFSM()

	res := applyFloor(t, fsm, 1, 20480)

	if got := res.ColonyDiskSize.GetFloorMb(); got != 20480 {
		t.Errorf("ColonyDiskSize.FloorMb = %d, want 20480", got)
	}
	// The who and when of the raise is replicated rather than a claim
	// one managerd makes in memory, so it survives the Comb that set it
	// going away. Asserting all three is what distinguishes that from a
	// floor that merely holds a number.
	if got := res.ColonyDiskSize.GetSetBy(); got != "comb-1" {
		t.Errorf("ColonyDiskSize.SetBy = %q, want comb-1", got)
	}
	if got := res.ColonyDiskSize.GetSetByKey(); got != "key-1" {
		t.Errorf("ColonyDiskSize.SetByKey = %q, want key-1", got)
	}
	if got := res.ColonyDiskSize.GetSetAtUnix(); got != 1700000000 {
		t.Errorf("ColonyDiskSize.SetAtUnix = %d, want 1700000000", got)
	}
	if got := fsm.ColonyDiskSizeFloorMB(); got != 20480 {
		t.Errorf("ColonyDiskSizeFloorMB() = %d, want 20480", got)
	}
}

func TestSetColonyDiskSizeRefusesLowering(t *testing.T) {
	fsm := NewFSM()
	applyFloor(t, fsm, 1, 20480)

	err := applyFloorExpectingRefusal(t, fsm, 2, 1024)

	// Naming both numbers is the difference between a refusal an
	// operator can act on and one they have to guess at.
	if !strings.Contains(err, "1024") || !strings.Contains(err, "20480") {
		t.Errorf("refusal names %q, want it to name both the requested 1024 and the current 20480", err)
	}
	if got := fsm.ColonyDiskSizeFloorMB(); got != 20480 {
		t.Errorf("floor = %d after a lowering attempt, want it unchanged at 20480: a refusal that still moved the number is a clamp", got)
	}
}

func TestSetColonyDiskSizeRefusesTheSameFloorAgain(t *testing.T) {
	fsm := NewFSM()
	applyFloor(t, fsm, 1, 20480)

	// "At" is refused as well as "below", deliberately: a no-op apply
	// would still rewrite set_by and set_at_unix, recording a raise that
	// did not happen and moving the audit trail's timestamp forward for
	// nothing.
	applyFloorExpectingRefusal(t, fsm, 2, 20480)

	state := fsm.ColonyDiskSizeState()
	if got := state.GetSetBy(); got != "comb-1" {
		t.Errorf("SetBy = %q after a refused re-raise, want comb-1: a refused command must not rewrite the audit trail", got)
	}
	if got := state.GetSetAtUnix(); got != 1700000000 {
		t.Errorf("SetAtUnix = %d after a refused re-raise, want it unchanged at 1700000000", got)
	}
}

func TestSetColonyDiskSizeRefusesZero(t *testing.T) {
	fsm := NewFSM()

	applyFloorExpectingRefusal(t, fsm, 1, 0)

	if state := fsm.ColonyDiskSizeState(); state != nil {
		t.Errorf("ColonyDiskSizeState() = %+v after a refused zero, want nil: absence is the one way to say unset, so a zero must not be stored as a floor", state)
	}

	// And the refusal must not poison the Colony: the very next
	// legitimate raise still works, which is what a rejected apply that
	// left state half-written would break.
	applyFloor(t, fsm, 2, 8192)
	if got := fsm.ColonyDiskSizeFloorMB(); got != 8192 {
		t.Errorf("floor = %d after a raise following a refused zero, want 8192", got)
	}
}

func TestSetColonyDiskSizeRefusesZeroEvenAfterAFloorExists(t *testing.T) {
	fsm := NewFSM()
	applyFloor(t, fsm, 1, 20480)

	// A zero after a real floor is the case where reading zero as
	// "unset" would be most tempting and most damaging: it would be a
	// lowering written as an absence.
	applyFloorExpectingRefusal(t, fsm, 2, 0)

	if got := fsm.ColonyDiskSizeFloorMB(); got != 20480 {
		t.Errorf("floor = %d after a refused zero, want it unchanged at 20480", got)
	}
}

func TestSetColonyDiskSizeRaises(t *testing.T) {
	fsm := NewFSM()
	applyFloor(t, fsm, 1, 20480)

	applyFloor(t, fsm, 2, 40960)

	if got := fsm.ColonyDiskSizeFloorMB(); got != 40960 {
		t.Errorf("floor = %d after a raise, want 40960: a repeated raise is the same command with a bigger number, and it is what makes the floor climbable", got)
	}
}

// TestSetColonyDiskSizeRefusesLoweringOnASecondReplica is the
// determinism claim the ADR makes, asserted rather than assumed. The
// whole argument for putting the monotonicity check in the apply
// function is that every replica decides "never reduced" from the same
// committed log, so two FSM instances fed the identical command sequence
// must reach the identical floor and reject the identical command. A
// handler-side check would pass this test too - which is exactly why
// what is actually being pinned down here is that the REFUSAL is a
// property of state, and it shows up identically on an instance that
// never saw the handler.
func TestSetColonyDiskSizeRefusesLoweringOnASecondReplica(t *testing.T) {
	commands := []*internalpb.Command{
		setFloorCmd(20480),
		setFloorCmd(40960),
		setFloorCmd(1024),
		setFloorCmd(0),
	}

	var floors []uint64
	var refusals []string
	// Three instances, not two: a second one agreeing with the first
	// could be two replicas replaying one bug, and the third is what
	// makes the loop rather than a single comparison the evidence.
	for replica := 0; replica < 3; replica++ {
		fsm := NewFSM()
		var errs []string
		for i, cmd := range commands {
			res := fsm.Apply(&raft.Log{Index: uint64(i + 1), Data: mustMarshalCommand(t, cmd)}).(*FSMApplyResult)
			if res.Error != "" {
				errs = append(errs, res.Error)
			}
		}
		floors = append(floors, fsm.ColonyDiskSizeFloorMB())
		refusals = append(refusals, strings.Join(errs, "\n"))
	}

	for replica := 1; replica < len(floors); replica++ {
		if floors[replica] != floors[0] {
			t.Errorf("replica %d floor = %d, replica 0 floor = %d: the same log must produce the same floor on every replica", replica, floors[replica], floors[0])
		}
		if refusals[replica] != refusals[0] {
			t.Errorf("replica %d refusals:\n%s\nreplica 0 refusals:\n%s\nthe same log must produce the same refusals on every replica", replica, refusals[replica], refusals[0])
		}
	}
	if floors[0] != 40960 {
		t.Errorf("floor = %d after raise 20480, raise 40960, lower 1024, zero 0, want 40960", floors[0])
	}
	// Both refusals, on every replica: two is the count the command
	// sequence is designed to produce, so a guard that silently stopped
	// refusing one of them shows up here as a count mismatch.
	if n := len(strings.Split(refusals[0], "\n")); n != 2 {
		t.Errorf("replica 0 recorded %d refusals, want 2 (the lowering to 1024 and the zero):\n%s", n, refusals[0])
	}
}

// createVMWithDiskCmd is a CreateVM naming a per-VM disk_size_mb.
func createVMWithDiskCmd(id string, diskMB uint64) *internalpb.Command {
	return &internalpb.Command{
		Op: &internalpb.Command_CreateVm{
			CreateVm: &internalpb.CreateVM{Vm: &internalpb.VMDefinition{Id: id, Name: id, DiskSizeMb: diskMB}},
		},
	}
}

func TestCreateVMRefusesAPerVMDiskSizeBelowTheFloor(t *testing.T) {
	fsm := NewFSM()
	applyFloor(t, fsm, 1, 20480)

	res := fsm.Apply(&raft.Log{Index: 2, Data: mustMarshalCommand(t, createVMWithDiskCmd("vm-small", 1024))}).(*FSMApplyResult)
	if res.Error == "" {
		t.Fatalf("CreateVM with disk_size_mb=1024 under a 20480 floor was accepted: a floor that is only a recommendation is not a floor")
	}
	if !strings.Contains(res.Error, "1024") || !strings.Contains(res.Error, "20480") {
		t.Errorf("refusal names %q, want it to name both 1024 and the 20480 floor", res.Error)
	}
	if vms := fsm.ListVMs(); len(vms) != 0 {
		t.Errorf("ListVMs() returned %d VMs after a refused create, want 0", len(vms))
	}
}

func TestCreateVMAcceptsAPerVMDiskSizeAboveTheFloor(t *testing.T) {
	fsm := NewFSM()
	applyFloor(t, fsm, 1, 20480)

	res := fsm.Apply(&raft.Log{Index: 2, Data: mustMarshalCommand(t, createVMWithDiskCmd("vm-big", 102400))}).(*FSMApplyResult)
	if res.Error != "" {
		t.Fatalf("CreateVM with disk_size_mb=102400 under a 20480 floor failed: %s", res.Error)
	}
	if got := res.VM.GetDiskSizeMb(); got != 102400 {
		t.Errorf("stored disk_size_mb = %d, want 102400: the override is recorded on the definition, not resolved at call time", got)
	}
}

func TestCreateVMWithNoPerVMDiskSizeIsUnaffectedByTheFloor(t *testing.T) {
	fsm := NewFSM()
	applyFloor(t, fsm, 1, 20480)

	// Zero means "no override", and that has to keep working for a VM
	// created before this field existed. Nothing here names a size, so
	// there is nothing for the floor to refuse.
	res := fsm.Apply(&raft.Log{Index: 2, Data: mustMarshalCommand(t, createVMWithDiskCmd("vm-old", 0))}).(*FSMApplyResult)
	if res.Error != "" {
		t.Fatalf("CreateVM with no disk_size_mb under a 20480 floor failed: %s", res.Error)
	}
	if got := res.VM.GetDiskSizeMb(); got != 0 {
		t.Errorf("stored disk_size_mb = %d, want 0", got)
	}
}

// updateVMWithDiskCmd is an UpdateVM full-record replace carrying a
// per-VM disk_size_mb, which is what makes the UpdateVM case a trap.
func updateVMWithDiskCmd(id string, diskMB uint64) *internalpb.Command {
	return &internalpb.Command{
		Op: &internalpb.Command_UpdateVm{
			UpdateVm: &internalpb.UpdateVM{Vm: &internalpb.VMDefinition{Id: id, Name: id, DiskSizeMb: diskMB}},
		},
	}
}

// TestUpdateVMCarriesAStalePerVMDiskSize is the trap the ADR calls out
// by name, and the test that exists because of it. UpdateVM replaces the
// whole record, so a caller updating, say, a VM's CPU count sends back
// the whole VMDefinition including whatever disk_size_mb it happens to
// hold. Once the Colony floor has risen past that value, a check that
// does not consult what the record already holds refuses the update -
// and the record becomes un-updatable for a reason that has nothing to
// do with what was being updated. That is why SetVMFirewallPaused
// exists at all (ADR-0049), so it is not a hypothetical.
func TestUpdateVMCarriesAStalePerVMDiskSize(t *testing.T) {
	fsm := NewFSM()
	// Created before any floor exists, at 4096 MB.
	if res := fsm.Apply(&raft.Log{Index: 1, Data: mustMarshalCommand(t, createVMWithDiskCmd("vm-legacy", 4096))}).(*FSMApplyResult); res.Error != "" {
		t.Fatalf("setup CreateVM failed: %s", res.Error)
	}

	// The floor rises well past what the record holds.
	applyFloor(t, fsm, 2, 102400)

	// An unrelated update carries the old value forward. It is not a new
	// request, so it must not be refused.
	res := fsm.Apply(&raft.Log{Index: 3, Data: mustMarshalCommand(t, updateVMWithDiskCmd("vm-legacy", 4096))}).(*FSMApplyResult)
	if res.Error != "" {
		t.Fatalf("UpdateVM carrying an existing disk_size_mb below the raised floor was refused: %s\nthis is the UpdateVM full-replace trap: raising the Colony floor must not make every older VM un-updatable for an unrelated reason", res.Error)
	}
	if got := res.VM.GetDiskSizeMb(); got != 4096 {
		t.Errorf("stored disk_size_mb = %d, want 4096: the record says what was asked for and is deliberately not rewritten when the floor rises past it", got)
	}
}

func TestUpdateVMRefusesANewPerVMDiskSizeBelowTheFloor(t *testing.T) {
	fsm := NewFSM()
	applyFloor(t, fsm, 1, 20480)
	if res := fsm.Apply(&raft.Log{Index: 2, Data: mustMarshalCommand(t, createVMWithDiskCmd("vm-1", 40960))}).(*FSMApplyResult); res.Error != "" {
		t.Fatalf("setup CreateVM failed: %s", res.Error)
	}

	// The narrow check is narrow, not absent: a value that DIFFERS from
	// what the record holds is a new request, and a new request below the
	// floor is refused.
	res := fsm.Apply(&raft.Log{Index: 3, Data: mustMarshalCommand(t, updateVMWithDiskCmd("vm-1", 1024))}).(*FSMApplyResult)
	if res.Error == "" {
		t.Fatalf("UpdateVM setting disk_size_mb=1024 under a 20480 floor was accepted, want a refusal: only a carried-forward value is exempt")
	}
	if vm, ok := fsm.VM("vm-1"); !ok || vm.GetDiskSizeMb() != 40960 {
		t.Errorf("stored disk_size_mb = %v after a refused update, want it unchanged at 40960", vm)
	}
}

// TestUpdateVMCanRaiseAPerVMDiskSizeToTheFloor covers the boundary the
// comparison has to get right from the other side: the floor itself is
// not below the floor.
func TestUpdateVMCanRaiseAPerVMDiskSizeToTheFloor(t *testing.T) {
	fsm := NewFSM()
	applyFloor(t, fsm, 1, 20480)
	if res := fsm.Apply(&raft.Log{Index: 2, Data: mustMarshalCommand(t, createVMWithDiskCmd("vm-1", 102400))}).(*FSMApplyResult); res.Error != "" {
		t.Fatalf("setup CreateVM failed: %s", res.Error)
	}

	res := fsm.Apply(&raft.Log{Index: 3, Data: mustMarshalCommand(t, updateVMWithDiskCmd("vm-1", 20480))}).(*FSMApplyResult)
	if res.Error != "" {
		t.Fatalf("UpdateVM setting disk_size_mb to exactly the 20480 floor failed: %s\nthe floor is a minimum, so the minimum itself is allowed", res.Error)
	}
}

// TestUpdateVMCanDropAPerVMOverrideToZero is the last half of the
// narrow check. Zero means "no override", so a record that holds one may
// go back to taking the floor. That value differs from what the record
// holds, so it goes through the comparison - and zero is short-circuited
// as "no override" before it, because otherwise dropping the override
// would be impossible on any VM whose override was above the floor.
func TestUpdateVMCanDropAPerVMOverrideToZero(t *testing.T) {
	fsm := NewFSM()
	applyFloor(t, fsm, 1, 20480)
	if res := fsm.Apply(&raft.Log{Index: 2, Data: mustMarshalCommand(t, createVMWithDiskCmd("vm-1", 102400))}).(*FSMApplyResult); res.Error != "" {
		t.Fatalf("setup CreateVM failed: %s", res.Error)
	}

	res := fsm.Apply(&raft.Log{Index: 3, Data: mustMarshalCommand(t, updateVMWithDiskCmd("vm-1", 0))}).(*FSMApplyResult)
	if res.Error != "" {
		t.Fatalf("UpdateVM clearing disk_size_mb to 0 failed: %s\nzero means no override, so it must not be compared against the floor", res.Error)
	}
	if vm, ok := fsm.VM("vm-1"); !ok || vm.GetDiskSizeMb() != 0 {
		t.Errorf("stored disk_size_mb = %v, want 0", vm)
	}
}

// TestColonyDiskSizeStateIsACopy checks the accessor hands out a clone.
// A caller that could mutate the FSM's own pointer through the returned
// message would be a way to change the floor without a command, which is
// the whole thing this state exists to prevent.
func TestColonyDiskSizeStateIsACopy(t *testing.T) {
	fsm := NewFSM()
	applyFloor(t, fsm, 1, 20480)

	first := fsm.ColonyDiskSizeState()
	first.FloorMb = 1

	if got := fsm.ColonyDiskSizeFloorMB(); got != 20480 {
		t.Errorf("floor = %d after mutating a returned copy, want 20480: the accessor must hand out a clone, not the FSM's own pointer", got)
	}
	if fsm.ColonyDiskSizeState() == first {
		t.Error("ColonyDiskSizeState() returned the same pointer twice, so it is not handing out a fresh clone each call")
	}
}

func TestColonyDiskSizeStateIsNilBeforeAnyFloorIsSet(t *testing.T) {
	fsm := NewFSM()

	if got := fsm.ColonyDiskSizeState(); got != nil {
		t.Errorf("ColonyDiskSizeState() = %+v on a Colony that never set a floor, want nil: absence is how unset is expressed", got)
	}
	if got := fsm.ColonyDiskSizeFloorMB(); got != 0 {
		t.Errorf("ColonyDiskSizeFloorMB() = %d on a Colony that never set a floor, want 0", got)
	}
}

// TestSetColonyDiskSizeSurvivesSnapshotRestore is the guarantee a restore
// cannot quietly undo. A floor that lived outside the snapshot would let
// whichever voter restored last re-open the Colony to a lower default,
// which is precisely what the replicated state exists to prevent.
func TestSetColonyDiskSizeSurvivesSnapshotRestore(t *testing.T) {
	fsm := NewFSM()
	applyFloor(t, fsm, 1, 20480)
	applyFloor(t, fsm, 2, 40960)

	snap, err := fsm.Snapshot()
	if err != nil {
		t.Fatalf("Snapshot() error: %v", err)
	}
	sink := &fakeSnapshotSink{}
	if err := snap.Persist(sink); err != nil {
		t.Fatalf("Persist() error: %v", err)
	}

	restored := NewFSM()
	if err := restored.Restore(io.NopCloser(bytes.NewReader(sink.Bytes()))); err != nil {
		t.Fatalf("restore: %v", err)
	}

	state := restored.ColonyDiskSizeState()
	if got := state.GetFloorMb(); got != 40960 {
		t.Errorf("restored floor = %d, want 40960: a restore that drops the floor re-opens this Colony to a lower default", got)
	}
	if got := state.GetSetBy(); got != "comb-1" {
		t.Errorf("restored SetBy = %q, want comb-1: who raised the floor is replicated, not local", got)
	}

	// And the restored FSM still refuses a lowering, rather than holding
	// the right number as an inert copy with no enforcement behind it.
	applyFloorExpectingRefusal(t, restored, 3, 20480)
	if got := restored.ColonyDiskSizeFloorMB(); got != 40960 {
		t.Errorf("restored floor = %d after a lowering attempt, want it unchanged at 40960", got)
	}
}

// TestSnapshotRestoreOfAPreFloorSnapshotLeavesNoFloor checks the other
// direction: a snapshot taken before this feature existed has no
// colony_disk_size field, and absence must stay absence rather than
// becoming a fabricated zero floor.
func TestSnapshotRestoreOfAPreFloorSnapshotLeavesNoFloor(t *testing.T) {
	restored := NewFSM()
	legacy, err := proto.Marshal(&internalpb.FSMSnapshotState{LastIndex: 7})
	if err != nil {
		t.Fatalf("marshaling legacy state: %v", err)
	}
	if err := restored.Restore(io.NopCloser(bytes.NewReader(legacy))); err != nil {
		t.Fatalf("restore: %v", err)
	}
	if got := restored.ColonyDiskSizeState(); got != nil {
		t.Errorf("ColonyDiskSizeState() = %+v after restoring a snapshot with no floor in it, want nil", got)
	}
	// And a raise still works on the restored FSM, which a state left
	// half-initialised by the restore would refuse.
	applyFloor(t, restored, 8, 20480)
}
