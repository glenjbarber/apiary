package raft

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hashicorp/raft"
	"google.golang.org/protobuf/proto"

	internalpb "github.com/glenjbarber/apiary/api/internalpb"
)

// ADR-0156 Stage 1: the Colony identity record.
//
// What is worth testing here is not that a string is stored. It is what
// the record REFUSES, because a Colony identity that could be rewritten
// would not be an identity at all - it would be a label, and every fact
// anchored to it would be anchored to something that could move.
//
// The three refusals are the whole requirement: an empty ID, a second
// ID, and a restore that would drop the identity.

// setIdentityCmd is a SetColonyIdentity command with an explicit set_by,
// so a test can assert that a refusal changed nothing at all rather than
// only that the value is wrong.
func setIdentityCmd(colonyID string) *internalpb.Command {
	return &internalpb.Command{
		Op: &internalpb.Command_SetColonyIdentity{
			SetColonyIdentity: &internalpb.SetColonyIdentity{
				ColonyId:    colonyID,
				SetBy:       "comb-1",
				CreatedUnix: 1700000000,
			},
		},
	}
}

// applyIdentity applies cmd and fails the test on any error, for the
// setup half of a test. The asserting half is the test body.
func applyIdentity(t *testing.T, fsm *FSM, index uint64, colonyID string) *FSMApplyResult {
	t.Helper()
	res := fsm.Apply(&raft.Log{Index: index, Data: mustMarshalCommand(t, setIdentityCmd(colonyID))}).(*FSMApplyResult)
	if res.Error != "" {
		t.Fatalf("SetColonyIdentity(%q) at index %d failed unexpectedly: %s", colonyID, index, res.Error)
	}
	return res
}

// applyIdentityExpectingRefusal applies an identity that must be
// refused and fails the test if it was not.
func applyIdentityExpectingRefusal(t *testing.T, fsm *FSM, index uint64, colonyID string) *FSMApplyResult {
	t.Helper()
	res := fsm.Apply(&raft.Log{Index: index, Data: mustMarshalCommand(t, setIdentityCmd(colonyID))}).(*FSMApplyResult)
	if res.Error == "" {
		t.Fatalf("SetColonyIdentity(%q) at index %d was expected to be refused, but it succeeded", colonyID, index)
	}
	return res
}

func TestFSM_SetColonyIdentity_RecordsTheIdentity(t *testing.T) {
	fsm := NewFSM()

	if got := fsm.ColonyID(); got != "" {
		t.Fatalf("a fresh FSM reports Colony ID %q, want empty (no identity recorded yet)", got)
	}

	applyIdentity(t, fsm, 1, "11111111-2222-4333-8444-555555555555")

	if got := fsm.ColonyID(); got != "11111111-2222-4333-8444-555555555555" {
		t.Fatalf("ColonyID() = %q, want %q", got, "11111111-2222-4333-8444-555555555555")
	}

	state := fsm.ColonyIdentityState()
	if state == nil {
		t.Fatal("ColonyIdentityState() = nil, want the recorded identity")
	}
	if state.GetColonyId() != "11111111-2222-4333-8444-555555555555" {
		t.Fatalf("ColonyIdentityState().ColonyId = %q, want the applied identity", state.GetColonyId())
	}
	if state.GetSetBy() != "comb-1" {
		t.Fatalf("ColonyIdentityState().SetBy = %q, want %q", state.GetSetBy(), "comb-1")
	}
}

func TestFSM_SetColonyIdentity_RefusesAnEmptyID(t *testing.T) {
	fsm := NewFSM()

	res := applyIdentityExpectingRefusal(t, fsm, 1, "")

	if !strings.Contains(res.Error, "empty colony_id") {
		t.Fatalf("refusal message = %q, want it to name the empty colony_id", res.Error)
	}
	if got := fsm.ColonyID(); got != "" {
		t.Fatalf("after a refused empty identity, ColonyID() = %q, want empty", got)
	}
	if fsm.ColonyIdentityState() != nil {
		t.Fatal("after a refused empty identity, ColonyIdentityState() != nil, want nil")
	}
}

func TestFSM_SetColonyIdentity_RefusesASecondIdentity(t *testing.T) {
	fsm := NewFSM()

	applyIdentity(t, fsm, 1, "11111111-2222-4333-8444-555555555555")

	// The second identity must be refused BY NAME, naming both values,
	// so that a node which lost the race learns that it lost rather
	// than believing it succeeded.
	res := applyIdentityExpectingRefusal(t, fsm, 2, "99999999-8888-4777-8666-555555555555")

	if !strings.Contains(res.Error, "11111111-2222-4333-8444-555555555555") {
		t.Fatalf("refusal message = %q, want it to name the existing identity", res.Error)
	}
	if !strings.Contains(res.Error, "99999999-8888-4777-8666-555555555555") {
		t.Fatalf("refusal message = %q, want it to name the proposed identity", res.Error)
	}
	if !strings.Contains(res.Error, "exactly once") {
		t.Fatalf("refusal message = %q, want it to say the identity is set exactly once", res.Error)
	}

	// And the existing identity must be untouched.
	if got := fsm.ColonyID(); got != "11111111-2222-4333-8444-555555555555" {
		t.Fatalf("after a refused second identity, ColonyID() = %q, want the original identity", got)
	}
}

func TestFSM_SetColonyIdentity_SurvivesSnapshotAndRestore(t *testing.T) {
	fsm := NewFSM()
	applyIdentity(t, fsm, 1, "11111111-2222-4333-8444-555555555555")

	snap, err := fsm.Snapshot()
	if err != nil {
		t.Fatalf("Snapshot() error: %v", err)
	}
	restored := NewFSM()
	if err := restored.Restore(snapshotReader(t, snap)); err != nil {
		t.Fatalf("Restore() error: %v", err)
	}

	if got := restored.ColonyID(); got != "11111111-2222-4333-8444-555555555555" {
		t.Fatalf("after restore, ColonyID() = %q, want the snapshotted identity", got)
	}

	// A restored identity is still a SET identity: the refusal to
	// rewrite it must survive the restore too.
	applyIdentityExpectingRefusal(t, restored, 2, "99999999-8888-4777-8666-555555555555")
}

// TestFSM_SetColonyIdentity_RestoredFromAPreIdentitySnapshotHasNoIdentity
// pins the restore path for a snapshot taken before ADR-0156: absence is
// the sole representation of "no identity recorded", and a restore must
// not invent one.
func TestFSM_SetColonyIdentity_RestoredFromAPreIdentitySnapshotHasNoIdentity(t *testing.T) {
	// A state message with no colony_identity field at all - exactly
	// what a pre-ADR-0156 snapshot holds on the wire.
	pre := &internalpb.FSMSnapshotState{LastIndex: 7}
	data, err := proto.Marshal(pre)
	if err != nil {
		t.Fatalf("marshaling a pre-identity snapshot state: %v", err)
	}

	fsm := NewFSM()
	if err := fsm.Restore(io.NopCloser(bytes.NewReader(data))); err != nil {
		t.Fatalf("Restore() error: %v", err)
	}

	if got := fsm.ColonyID(); got != "" {
		t.Fatalf("after restoring a pre-identity snapshot, ColonyID() = %q, want empty", got)
	}
	if fsm.ColonyIdentityState() != nil {
		t.Fatal("after restoring a pre-identity snapshot, ColonyIdentityState() != nil, want nil")
	}
}

// TestFSM_SetColonyIdentity_IsInsideTheStateDigest pins that the
// identity is covered by ADR-0143's digest. A state field outside the
// digest is a field two voters can disagree about without anything
// noticing, which is the exact failure this whole feature closes.
func TestFSM_SetColonyIdentity_IsInsideTheStateDigest(t *testing.T) {
	withIdentity := NewFSM()
	applyIdentity(t, withIdentity, 1, "11111111-2222-4333-8444-555555555555")

	withoutIdentity := NewFSM()

	if withIdentity.StateDigest() == withoutIdentity.StateDigest() {
		t.Fatal("two FSMs differing only in their Colony identity have the same state digest; the identity is not covered by ADR-0143's digest")
	}

	// And two FSMs with the SAME identity must agree.
	alsoWith := NewFSM()
	applyIdentity(t, alsoWith, 1, "11111111-2222-4333-8444-555555555555")

	if withIdentity.StateDigest() != alsoWith.StateDigest() {
		t.Fatal("two FSMs holding the same Colony identity have different state digests")
	}
}

// TestNewColonyID_ProducesDistinctUUIDv4s pins the shape of the
// generated identity. A malformed UUID would be carried, compared and
// displayed as though it were real, so the shape is part of the
// contract rather than an implementation detail.
func TestNewColonyID_ProducesDistinctUUIDv4s(t *testing.T) {
	seen := make(map[string]struct{})
	for i := 0; i < 64; i++ {
		id, err := newColonyID()
		if err != nil {
			t.Fatalf("newColonyID() error: %v", err)
		}
		if len(id) != 36 {
			t.Fatalf("newColonyID() = %q, want a 36-character UUID", id)
		}
		if id[14] != '4' {
			t.Fatalf("newColonyID() = %q, want version 4 at position 14", id)
		}
		if c := id[19]; c != '8' && c != '9' && c != 'a' && c != 'b' {
			t.Fatalf("newColonyID() = %q, want a variant nibble (8, 9, a or b) at position 19", id)
		}
		if _, dup := seen[id]; dup {
			t.Fatalf("newColonyID() produced the duplicate %q", id)
		}
		seen[id] = struct{}{}
	}
}

// TestReadColonySidecar_MissingIsNotAnError pins that a directory with
// no sidecar is the normal case, not a failure.
func TestReadColonySidecar_MissingIsNotAnError(t *testing.T) {
	dir := t.TempDir()

	sidecar, err := ReadColonySidecar(dir)
	if err != nil {
		t.Fatalf("ReadColonySidecar() on a directory with no sidecar returned an error: %v", err)
	}
	if sidecar != nil {
		t.Fatalf("ReadColonySidecar() on a directory with no sidecar = %+v, want nil", sidecar)
	}
}

// TestReadColonySidecar_CorruptIsAnError pins that a sidecar which
// cannot be parsed is reported rather than ignored: ignoring it would
// discard the only evidence the startup guard has.
func TestReadColonySidecar_CorruptIsAnError(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, sidecarFileName), []byte("{not json"), 0o600); err != nil {
		t.Fatalf("writing a corrupt sidecar: %v", err)
	}

	sidecar, err := ReadColonySidecar(dir)
	if err == nil {
		t.Fatal("ReadColonySidecar() on a corrupt sidecar returned no error, want one")
	}
	if sidecar != nil {
		t.Fatalf("ReadColonySidecar() on a corrupt sidecar = %+v, want nil", sidecar)
	}
}

func TestWriteColonySidecar_RoundTrips(t *testing.T) {
	dir := t.TempDir()

	if err := WriteColonySidecar(dir, "11111111-2222-4333-8444-555555555555", "comb-1"); err != nil {
		t.Fatalf("WriteColonySidecar() error: %v", err)
	}

	sidecar, err := ReadColonySidecar(dir)
	if err != nil {
		t.Fatalf("ReadColonySidecar() error: %v", err)
	}
	if sidecar == nil {
		t.Fatal("ReadColonySidecar() = nil after writing one, want the sidecar")
	}
	if sidecar.ColonyID != "11111111-2222-4333-8444-555555555555" {
		t.Fatalf("sidecar.ColonyID = %q, want the written identity", sidecar.ColonyID)
	}
	if sidecar.NodeID != "comb-1" {
		t.Fatalf("sidecar.NodeID = %q, want %q", sidecar.NodeID, "comb-1")
	}
}

func TestWriteColonySidecar_RefusesAnEmptyIdentity(t *testing.T) {
	dir := t.TempDir()

	if err := WriteColonySidecar(dir, "", "comb-1"); err == nil {
		t.Fatal("WriteColonySidecar() with an empty colony_id returned no error, want one")
	}
}

// TestGuardSelfBootstrap_AllowsAnEmptyDirectory pins that a node
// forming a NEW Colony is not blocked: every Colony starts this way.
func TestGuardSelfBootstrap_AllowsAnEmptyDirectory(t *testing.T) {
	dir := t.TempDir()

	if err := GuardSelfBootstrap(dir); err != nil {
		t.Fatalf("GuardSelfBootstrap() on an empty directory returned an error: %v", err)
	}
}

// TestGuardSelfBootstrap_RefusesOverAKnownColony pins the guard's whole
// purpose: a node whose data directory already belonged to a Colony
// must not silently form a second one.
func TestGuardSelfBootstrap_RefusesOverAKnownColony(t *testing.T) {
	dir := t.TempDir()
	if err := WriteColonySidecar(dir, "11111111-2222-4333-8444-555555555555", "comb-1"); err != nil {
		t.Fatalf("WriteColonySidecar() error: %v", err)
	}

	err := GuardSelfBootstrap(dir)
	if err == nil {
		t.Fatal("GuardSelfBootstrap() over a directory that belonged to a Colony returned no error, want a refusal")
	}
	if !strings.Contains(err.Error(), "11111111-2222-4333-8444-555555555555") {
		t.Fatalf("refusal message = %q, want it to name the Colony the directory belonged to", err.Error())
	}
}

// TestGuardSelfBootstrap_RefusesOverACorruptSidecar pins that a
// corrupt sidecar is a refusal, not a silent pass.
func TestGuardSelfBootstrap_RefusesOverACorruptSidecar(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, sidecarFileName), []byte("{not json"), 0o600); err != nil {
		t.Fatalf("writing a corrupt sidecar: %v", err)
	}

	if err := GuardSelfBootstrap(dir); err == nil {
		t.Fatal("GuardSelfBootstrap() over a corrupt sidecar returned no error, want a refusal")
	}
}

// snapshotReader turns an FSMSnapshot into the reader Restore expects.
func snapshotReader(t *testing.T, snap raft.FSMSnapshot) io.ReadCloser {
	t.Helper()
	fsmSnapshot, ok := snap.(*fsmSnapshot)
	if !ok {
		t.Fatalf("Snapshot() returned %T, want *fsmSnapshot", snap)
	}
	data, err := proto.Marshal(fsmSnapshot.state)
	if err != nil {
		t.Fatalf("marshaling the snapshot state: %v", err)
	}
	return io.NopCloser(bytes.NewReader(data))
}

// A node that is RESTORED into a fresh data directory must be able to
// come up, and must not be blocked by the self-bootstrap guard.
//
// This matters because the restore path is exactly the one that
// legitimately produces a node whose data directory was empty before
// the restore - and ADR-0156's guard must not turn that into a
// refusal. The guard fires on a sidecar that says "this directory
// belonged to a Colony"; a restored node's directory either has no
// sidecar (a first restore) or carries the sidecar of the Colony it
// was restored from, in which case the restored state names that same
// Colony and the identity is inherited rather than re-bootstrapped.
func TestRestore_AFreshDataDirectoryIsNotBlockedByTheGuard(t *testing.T) {
	dir := t.TempDir()

	if err := GuardSelfBootstrap(dir); err != nil {
		t.Fatalf("GuardSelfBootstrap() on a fresh restore directory returned an error: %v", err)
	}

	// A sidecar naming a Colony is present only when the operator
	// deliberately preserved it; the guard then refuses a
	// self-bootstrap, which is the intended behaviour.
	if err := WriteColonySidecar(dir, "11111111-2222-4333-8444-555555555555", "comb-1"); err != nil {
		t.Fatalf("WriteColonySidecar() error: %v", err)
	}
	if err := GuardSelfBootstrap(dir); err == nil {
		t.Fatal("GuardSelfBootstrap() with a sidecar present returned no error, want a refusal")
	}

	// The sidecar is a plain file beside the raft data, so moving the
	// data directory aside carries it with it - which is what lets the
	// reset path produce a genuinely fresh Colony.
	moved := filepath.Join(t.TempDir(), "moved")
	if err := os.Rename(dir, moved); err != nil {
		t.Fatalf("moving the data directory aside: %v", err)
	}
	if _, err := os.Stat(filepath.Join(moved, sidecarFileName)); err != nil {
		t.Fatalf("the sidecar did not travel with the moved data directory: %v", err)
	}
	if err := GuardSelfBootstrap(moved); err == nil {
		t.Fatal("GuardSelfBootstrap() on a moved-aside directory returned no error, want a refusal")
	}
}
