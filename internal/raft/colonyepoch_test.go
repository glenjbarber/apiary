package raft

import (
	"bytes"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/raft"
	"google.golang.org/protobuf/proto"

	internalpb "github.com/glenjbarber/apiary/api/internalpb"
)

// ADR-0156 Stage 2: the Colony epoch.
//
// What is worth testing here is not that a number is stored. It is what
// the record REFUSES, because an epoch that could move backwards, skip,
// or be re-established would not be a generation counter at all - and a
// generation counter that can be rewritten cannot turn a stale-epoch
// node away at the door.
//
// The refusals are the whole requirement: an epoch of 0, an initial
// epoch other than 1, a second establishment, an advance that is not
// exactly one step, an advance of a Colony that has no epoch, and a
// join request naming a generation other than the Colony's own.

// setEpochCmd is a SetColonyEpoch command with an explicit set_by, so
// a test can assert that a refusal changed nothing at all rather than
// only that the value is wrong.
func setEpochCmd(epoch uint64) *internalpb.Command {
	return &internalpb.Command{
		Op: &internalpb.Command_SetColonyEpoch{
			SetColonyEpoch: &internalpb.SetColonyEpoch{
				Epoch:     epoch,
				SetBy:     "comb-1",
				SetAtUnix: 1700000000,
			},
		},
	}
}

// advanceEpochCmd is an AdvanceColonyEpoch command naming the epoch
// being moved TO.
func advanceEpochCmd(epoch uint64) *internalpb.Command {
	return &internalpb.Command{
		Op: &internalpb.Command_AdvanceColonyEpoch{
			AdvanceColonyEpoch: &internalpb.AdvanceColonyEpoch{
				Epoch:     epoch,
				SetBy:     "comb-1",
				SetAtUnix: 1700000000,
			},
		},
	}
}

// applyEpoch applies cmd and fails the test on any error, for the setup
// half of a test. The asserting half is the test body.
func applyEpoch(t *testing.T, fsm *FSM, index uint64, cmd *internalpb.Command) *FSMApplyResult {
	t.Helper()
	res := fsm.Apply(&raft.Log{Index: index, Data: mustMarshalCommand(t, cmd)}).(*FSMApplyResult)
	if res.Error != "" {
		t.Fatalf("Apply(%+v) at index %d failed unexpectedly: %s", cmd.GetOp(), index, res.Error)
	}
	return res
}

// applyEpochExpectingRefusal applies a command that must be refused and
// fails the test if it was not.
func applyEpochExpectingRefusal(t *testing.T, fsm *FSM, index uint64, cmd *internalpb.Command) *FSMApplyResult {
	t.Helper()
	res := fsm.Apply(&raft.Log{Index: index, Data: mustMarshalCommand(t, cmd)}).(*FSMApplyResult)
	if res.Error == "" {
		t.Fatalf("Apply(%+v) at index %d was expected to be refused, but it succeeded", cmd.GetOp(), index)
	}
	return res
}

// establishEpoch applies the initial epoch, so a test can start from a
// Colony that has one.
func establishEpoch(t *testing.T, fsm *FSM) {
	t.Helper()
	applyEpoch(t, fsm, 1, setEpochCmd(initialColonyEpoch))
}

func TestFSM_SetColonyEpoch_RecordsTheEpoch(t *testing.T) {
	fsm := NewFSM()

	if got := fsm.ColonyEpoch(); got != 0 {
		t.Fatalf("a fresh FSM reports Colony epoch %d, want 0 (no epoch recorded yet)", got)
	}
	if fsm.ColonyEpochState() != nil {
		t.Fatal("a fresh FSM has a ColonyEpochState(), want nil")
	}

	applyEpoch(t, fsm, 1, setEpochCmd(initialColonyEpoch))

	if got := fsm.ColonyEpoch(); got != initialColonyEpoch {
		t.Fatalf("ColonyEpoch() = %d, want %d", got, initialColonyEpoch)
	}

	state := fsm.ColonyEpochState()
	if state == nil {
		t.Fatal("ColonyEpochState() = nil, want the recorded epoch")
	}
	if state.GetEpoch() != initialColonyEpoch {
		t.Fatalf("ColonyEpochState().Epoch = %d, want %d", state.GetEpoch(), initialColonyEpoch)
	}
	if state.GetPreviousEpoch() != 0 {
		t.Fatalf("ColonyEpochState().PreviousEpoch = %d, want 0 for the initial epoch", state.GetPreviousEpoch())
	}
	if state.GetSetBy() != "comb-1" {
		t.Fatalf("ColonyEpochState().SetBy = %q, want %q", state.GetSetBy(), "comb-1")
	}
}

func TestFSM_SetColonyEpoch_RefusesZero(t *testing.T) {
	fsm := NewFSM()

	res := applyEpochExpectingRefusal(t, fsm, 1, setEpochCmd(0))

	if !strings.Contains(res.Error, "epoch of 0") {
		t.Fatalf("refusal message = %q, want it to name the zero epoch", res.Error)
	}
	if got := fsm.ColonyEpoch(); got != 0 {
		t.Fatalf("after a refused zero epoch, ColonyEpoch() = %d, want 0", got)
	}
	if fsm.ColonyEpochState() != nil {
		t.Fatal("after a refused zero epoch, ColonyEpochState() != nil, want nil")
	}
}

// TestFSM_SetColonyEpoch_RefusesANonInitialEpoch pins ADR-0156's
// Decision 3: the initial epoch is 1, and establishing a Colony at
// some other generation would leave the log claiming generations that
// never happened.
func TestFSM_SetColonyEpoch_RefusesANonInitialEpoch(t *testing.T) {
	fsm := NewFSM()

	res := applyEpochExpectingRefusal(t, fsm, 1, setEpochCmd(7))

	if !strings.Contains(res.Error, "initialised to 1") {
		t.Fatalf("refusal message = %q, want it to say the epoch is initialised to 1", res.Error)
	}
	if got := fsm.ColonyEpoch(); got != 0 {
		t.Fatalf("after a refused non-initial epoch, ColonyEpoch() = %d, want 0", got)
	}
}

func TestFSM_SetColonyEpoch_RefusesASecondEpoch(t *testing.T) {
	fsm := NewFSM()

	establishEpoch(t, fsm)

	// The second establishment must be refused BY NAME, naming both
	// epochs, so that a node which lost the race learns that it lost
	// rather than believing it succeeded.
	res := applyEpochExpectingRefusal(t, fsm, 2, setEpochCmd(initialColonyEpoch))

	if !strings.Contains(res.Error, "exactly once") {
		t.Fatalf("refusal message = %q, want it to say the epoch is set exactly once", res.Error)
	}
	if !strings.Contains(res.Error, "already at 1") {
		t.Fatalf("refusal message = %q, want it to name the epoch already in state", res.Error)
	}

	// And the existing epoch must be untouched.
	if got := fsm.ColonyEpoch(); got != initialColonyEpoch {
		t.Fatalf("after a refused second epoch, ColonyEpoch() = %d, want %d", got, initialColonyEpoch)
	}
}

func TestFSM_AdvanceColonyEpoch_AdvancesByExactlyOne(t *testing.T) {
	fsm := NewFSM()
	establishEpoch(t, fsm)

	applyEpoch(t, fsm, 2, advanceEpochCmd(initialColonyEpoch+1))

	if got := fsm.ColonyEpoch(); got != initialColonyEpoch+1 {
		t.Fatalf("ColonyEpoch() = %d, want %d", got, initialColonyEpoch+1)
	}

	state := fsm.ColonyEpochState()
	if state.GetPreviousEpoch() != initialColonyEpoch {
		t.Fatalf("ColonyEpochState().PreviousEpoch = %d, want %d", state.GetPreviousEpoch(), initialColonyEpoch)
	}
}

// TestFSM_AdvanceColonyEpoch_RefusesAGap pins that a jump leaves no
// hole in the sequence. The sequence in the log IS the sequence of
// generations, and a hole in it destroys that property.
func TestFSM_AdvanceColonyEpoch_RefusesAGap(t *testing.T) {
	fsm := NewFSM()
	establishEpoch(t, fsm)

	res := applyEpochExpectingRefusal(t, fsm, 2, advanceEpochCmd(initialColonyEpoch+3))

	if !strings.Contains(res.Error, "exactly one") {
		t.Fatalf("refusal message = %q, want it to say the epoch advances by exactly one", res.Error)
	}
	if !strings.Contains(res.Error, "only valid target is 2") {
		t.Fatalf("refusal message = %q, want it to name the only valid target epoch", res.Error)
	}
	if got := fsm.ColonyEpoch(); got != initialColonyEpoch {
		t.Fatalf("after a refused gap, ColonyEpoch() = %d, want %d", got, initialColonyEpoch)
	}
}

// TestFSM_AdvanceColonyEpoch_RefusesARepeat pins that a no-op advance
// is refused. It would otherwise enter the log and rewrite
// set_by/set_at_unix, recording a generation change that did not
// happen.
func TestFSM_AdvanceColonyEpoch_RefusesARepeat(t *testing.T) {
	fsm := NewFSM()
	establishEpoch(t, fsm)

	res := applyEpochExpectingRefusal(t, fsm, 2, advanceEpochCmd(initialColonyEpoch))

	if !strings.Contains(res.Error, "exactly one") {
		t.Fatalf("refusal message = %q, want it to say the epoch advances by exactly one", res.Error)
	}
	if got := fsm.ColonyEpoch(); got != initialColonyEpoch {
		t.Fatalf("after a refused repeat, ColonyEpoch() = %d, want %d", got, initialColonyEpoch)
	}
}

// TestFSM_AdvanceColonyEpoch_RefusesOnAColonyWithNoEpoch pins that
// "advance from nothing" has no defined answer, and that allowing it
// would let the first advance establish an arbitrary starting point.
func TestFSM_AdvanceColonyEpoch_RefusesOnAColonyWithNoEpoch(t *testing.T) {
	fsm := NewFSM()

	res := applyEpochExpectingRefusal(t, fsm, 1, advanceEpochCmd(1))

	if !strings.Contains(res.Error, "no epoch recorded yet") {
		t.Fatalf("refusal message = %q, want it to say this Colony has no epoch yet", res.Error)
	}
	if got := fsm.ColonyEpoch(); got != 0 {
		t.Fatalf("after a refused advance, ColonyEpoch() = %d, want 0", got)
	}
}

func TestFSM_ColonyEpoch_SurvivesSnapshotAndRestore(t *testing.T) {
	fsm := NewFSM()
	establishEpoch(t, fsm)
	applyEpoch(t, fsm, 2, advanceEpochCmd(initialColonyEpoch+1))

	snap, err := fsm.Snapshot()
	if err != nil {
		t.Fatalf("Snapshot() error: %v", err)
	}
	restored := NewFSM()
	if err := restored.Restore(snapshotReader(t, snap)); err != nil {
		t.Fatalf("Restore() error: %v", err)
	}

	if got := restored.ColonyEpoch(); got != initialColonyEpoch+1 {
		t.Fatalf("after restore, ColonyEpoch() = %d, want %d", got, initialColonyEpoch+1)
	}
	if got := restored.ColonyEpochState().GetPreviousEpoch(); got != initialColonyEpoch {
		t.Fatalf("after restore, PreviousEpoch = %d, want %d", got, initialColonyEpoch)
	}

	// A restored epoch is still an ESTABLISHED epoch: the refusal to
	// re-establish it must survive the restore too, and the advance
	// must continue from the restored value rather than from 1.
	applyEpochExpectingRefusal(t, restored, 3, setEpochCmd(initialColonyEpoch))
	applyEpoch(t, restored, 4, advanceEpochCmd(initialColonyEpoch+2))
	if got := restored.ColonyEpoch(); got != initialColonyEpoch+2 {
		t.Fatalf("after an advance on the restored FSM, ColonyEpoch() = %d, want %d", got, initialColonyEpoch+2)
	}
}

// TestFSM_ColonyEpoch_RestoredFromAPreEpochSnapshotHasNoEpoch pins the
// restore path for a snapshot taken before ADR-0156 Stage 2: absence is
// the sole representation of "no epoch recorded", and a restore must
// not invent one.
func TestFSM_ColonyEpoch_RestoredFromAPreEpochSnapshotHasNoEpoch(t *testing.T) {
	pre := &internalpb.FSMSnapshotState{LastIndex: 7}
	data, err := proto.Marshal(pre)
	if err != nil {
		t.Fatalf("marshaling a pre-epoch snapshot state: %v", err)
	}

	fsm := NewFSM()
	if err := fsm.Restore(io.NopCloser(bytes.NewReader(data))); err != nil {
		t.Fatalf("Restore() error: %v", err)
	}

	if got := fsm.ColonyEpoch(); got != 0 {
		t.Fatalf("after restoring a pre-epoch snapshot, ColonyEpoch() = %d, want 0", got)
	}
	if fsm.ColonyEpochState() != nil {
		t.Fatal("after restoring a pre-epoch snapshot, ColonyEpochState() != nil, want nil")
	}
}

// TestFSM_ColonyEpoch_IsInsideTheStateDigest pins that the epoch is
// covered by ADR-0143's digest. A state field outside the digest is a
// field two voters can disagree about without anything noticing, which
// is the exact failure this whole feature closes.
func TestFSM_ColonyEpoch_IsInsideTheStateDigest(t *testing.T) {
	withEpoch := NewFSM()
	establishEpoch(t, withEpoch)

	withoutEpoch := NewFSM()

	if withEpoch.StateDigest() == withoutEpoch.StateDigest() {
		t.Fatal("two FSMs differing only in their Colony epoch have the same state digest; the epoch is not covered by ADR-0143's digest")
	}

	// Two FSMs at the SAME epoch must agree, and two at DIFFERENT
	// epochs must not - the second is the whole point of the epoch.
	alsoWith := NewFSM()
	establishEpoch(t, alsoWith)
	if withEpoch.StateDigest() != alsoWith.StateDigest() {
		t.Fatal("two FSMs holding the same Colony epoch have different state digests")
	}

	advanced := NewFSM()
	establishEpoch(t, advanced)
	applyEpoch(t, advanced, 2, advanceEpochCmd(initialColonyEpoch+1))
	if withEpoch.StateDigest() == advanced.StateDigest() {
		t.Fatal("two FSMs at different Colony epochs have the same state digest")
	}
}

// joinRequestCmd is a CreatePendingJoinRequest command naming a
// Colony generation the joiner claims.
func joinRequestCmd(requestID string, claimedEpoch uint64) *internalpb.Command {
	return &internalpb.Command{
		Op: &internalpb.Command_CreatePendingJoinRequest{
			CreatePendingJoinRequest: &internalpb.CreatePendingJoinRequest{
				Request: &internalpb.PendingJoinRequest{
					RequestId:       requestID,
					NodeId:          "comb-2",
					RaftBindAddress: "10.62.0.5:17600",
					Code:            "418263",
					RequestedAtUnix: 1000,
					ExpiresAtUnix:   1000 + 900,
					Status:          internalpb.JoinRequestStatus_JOIN_REQUEST_STATUS_PENDING,
					ColonyEpoch:     claimedEpoch,
				},
			},
		},
	}
}

// TestFSM_JoinRequest_RefusesAStaleEpochNode is the admission
// refusal ADR-0156's failure matrix calls for: "Node rejoins with a
// stale epoch ... Refused: admission of the stale node".
//
// The refusal lives in the FSM's apply function, so it is decided
// identically on every replica from the same committed log entry.
func TestFSM_JoinRequest_RefusesAStaleEpochNode(t *testing.T) {
	fsm := NewFSM()
	establishEpoch(t, fsm)
	applyEpoch(t, fsm, 2, advanceEpochCmd(initialColonyEpoch+1))

	// The joiner claims epoch 1 while the Colony is at epoch 2.
	res := applyEpochExpectingRefusal(t, fsm, 3, joinRequestCmd("req-1", initialColonyEpoch))

	if !strings.Contains(res.Error, "claims Colony epoch 1") {
		t.Fatalf("refusal message = %q, want it to name the claimed epoch", res.Error)
	}
	if !strings.Contains(res.Error, "this Colony is at epoch 2") {
		t.Fatalf("refusal message = %q, want it to name the Colony's current epoch", res.Error)
	}
	if !strings.Contains(res.Error, "behind this Colony's generation") {
		t.Fatalf("refusal message = %q, want it to say the joiner is behind", res.Error)
	}

	// And the request must not have been recorded.
	if _, found := fsm.PendingJoinRequest("req-1"); found {
		t.Fatal("a refused stale-epoch join request was recorded anyway")
	}
}

// TestFSM_JoinRequest_RefusesAnAheadEpochNode pins the mirror-image
// rule: a node claiming a generation the Colony has not reached is not
// joining this Colony's generation.
func TestFSM_JoinRequest_RefusesAnAheadEpochNode(t *testing.T) {
	fsm := NewFSM()
	establishEpoch(t, fsm)

	res := applyEpochExpectingRefusal(t, fsm, 2, joinRequestCmd("req-1", initialColonyEpoch+5))

	if !strings.Contains(res.Error, "claims Colony epoch 6") {
		t.Fatalf("refusal message = %q, want it to name the claimed epoch", res.Error)
	}
	if !strings.Contains(res.Error, "this Colony is at epoch 1") {
		t.Fatalf("refusal message = %q, want it to name the Colony's current epoch", res.Error)
	}
	if _, found := fsm.PendingJoinRequest("req-1"); found {
		t.Fatal("a refused ahead-of-generation join request was recorded anyway")
	}
}

// TestFSM_JoinRequest_AcceptsAMatchingEpoch is the ordinary case: a
// joiner that names the Colony's own generation is admitted.
func TestFSM_JoinRequest_AcceptsAMatchingEpoch(t *testing.T) {
	fsm := NewFSM()
	establishEpoch(t, fsm)
	applyEpoch(t, fsm, 2, advanceEpochCmd(initialColonyEpoch+1))

	applyEpoch(t, fsm, 3, joinRequestCmd("req-1", initialColonyEpoch+1))

	stored, found := fsm.PendingJoinRequest("req-1")
	if !found {
		t.Fatal("a join request naming the Colony's own epoch was not recorded")
	}
	if stored.GetColonyEpoch() != initialColonyEpoch+1 {
		t.Fatalf("the recorded request's ColonyEpoch = %d, want %d", stored.GetColonyEpoch(), initialColonyEpoch+1)
	}
}

// TestFSM_JoinRequest_AcceptsAnAbsentEpoch pins the compatibility
// rule: every node built before ADR-0156 Stage 2 sends no epoch, and
// refusing those would break every existing join for no safety gain.
//
// The absent claim is recorded as absent - NOT as a claim of epoch 0 -
// so the audit trail distinguishes "claimed epoch 1" from "claimed
// nothing".
func TestFSM_JoinRequest_AcceptsAnAbsentEpoch(t *testing.T) {
	fsm := NewFSM()
	establishEpoch(t, fsm)

	applyEpoch(t, fsm, 2, joinRequestCmd("req-1", 0))

	stored, found := fsm.PendingJoinRequest("req-1")
	if !found {
		t.Fatal("a join request making no epoch claim was not recorded")
	}
	if stored.GetColonyEpoch() != 0 {
		t.Fatalf("the recorded request's ColonyEpoch = %d, want 0 (the absence of a claim)", stored.GetColonyEpoch())
	}
}

// TestFSM_JoinRequest_AcceptsAnAbsentEpochOnAColonyWithNoEpoch pins
// that a Colony which has no epoch at all - every node built before
// this record - still accepts an ordinary join.
func TestFSM_JoinRequest_AcceptsAnAbsentEpochOnAColonyWithNoEpoch(t *testing.T) {
	fsm := NewFSM()

	applyEpoch(t, fsm, 1, joinRequestCmd("req-1", 0))

	if _, found := fsm.PendingJoinRequest("req-1"); found != true {
		t.Fatal("a join request making no epoch claim was not recorded on a Colony with no epoch")
	}
}

// TestSeedSnapshot_CarriesTheEpoch proves the epoch survives the
// ADR-0051 restore path - the seeded-recovery route, which is NOT a
// raft snapshot restore and so is not covered by the snapshot test
// above.
//
// A restore that dropped the epoch would restore a node that believes
// it belongs to no generation at all, and the refusal of stale-epoch
// nodes that this record exists to provide would then have nothing to
// compare against. It would also be a SILENT drop: the restored node
// would look exactly like a fresh bootstrap.
func TestSeedSnapshot_CarriesTheEpoch(t *testing.T) {
	sourceCfg := Config{
		NodeID:   "source",
		DataDir:  t.TempDir(),
		BindAddr: freeLoopbackAddr(t),
	}
	source, err := New(sourceCfg)
	if err != nil {
		t.Fatalf("New(source) error: %v", err)
	}
	if err := source.Bootstrap(); err != nil {
		t.Fatalf("source.Bootstrap() error: %v", err)
	}
	t.Cleanup(func() { source.Shutdown() })
	eventually(t, 5*time.Second, func() bool { return source.Status().IsLeader })

	// The source establishes the epoch and advances it once, so the
	// seeded state carries an epoch that is NOT the initial one - the
	// case where a dropped field would be visible.
	if _, err := source.EnsureColonyEpoch(5 * time.Second); err != nil {
		t.Fatalf("EnsureColonyEpoch() error: %v", err)
	}
	if _, err := source.AdvanceColonyEpoch(5 * time.Second); err != nil {
		t.Fatalf("AdvanceColonyEpoch() error: %v", err)
	}
	if got := source.Status().ColonyEpoch; got != initialColonyEpoch+1 {
		t.Fatalf("source Status().ColonyEpoch = %d, want %d", got, initialColonyEpoch+1)
	}

	exported := source.fsm.SnapshotState()
	if exported.GetColonyEpoch().GetEpoch() != initialColonyEpoch+1 {
		t.Fatalf("the exported state carries epoch %d, want %d", exported.GetColonyEpoch().GetEpoch(), initialColonyEpoch+1)
	}

	targetCfg := Config{
		NodeID:   "target",
		DataDir:  t.TempDir(),
		BindAddr: freeLoopbackAddr(t),
	}
	if err := SeedSnapshot(targetCfg, exported); err != nil {
		t.Fatalf("SeedSnapshot() error: %v", err)
	}

	target, err := New(targetCfg)
	if err != nil {
		t.Fatalf("New(target) error: %v", err)
	}
	t.Cleanup(func() { target.Shutdown() })

	eventually(t, 5*time.Second, func() bool { return target.Status().ColonyEpoch == initialColonyEpoch+1 })

	if got := target.Status().ColonyEpoch; got != initialColonyEpoch+1 {
		t.Fatalf("the seeded node's ColonyEpoch = %d, want %d", got, initialColonyEpoch+1)
	}
}
