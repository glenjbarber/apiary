package raft

import (
	"io"
	"os"
	"strings"
	"testing"

	"github.com/hashicorp/raft"
	"google.golang.org/protobuf/proto"

	internalpb "github.com/glenjbarber/apiary/api/internalpb"
)

// TestASeededRestoreGetsANewColonyIdentity is ADR-0156 Stage 3's
// central requirement: a restore is a COLONY RE-CREATION, not a
// member rejoin.
//
// A restore preserves the serialized FSM state and nothing else - not
// the log, not the stable vote keys, not the membership history, not
// the leader identity. The restored node is a brand new one-node
// Colony. If it kept the old Colony's identity, it would present the
// same identity to the original members - which is exactly the
// split-leader hazard the whole Colony Protocol layer exists to
// prevent.
//
// The restored payload cannot be trusted to carry an identity the
// surrounding world agrees with, because the world never saw that
// identity.
func TestASeededRestoreGetsANewColonyIdentity(t *testing.T) {
	oldColonyID := "11111111-2222-4333-8444-555555555555"
	oldEpoch := uint64(7)
	state := &internalpb.FSMSnapshotState{
		ColonyIdentity: &internalpb.ColonyIdentity{
			ColonyId:    oldColonyID,
			SetBy:       "old-node.lab3.apiary.work",
			CreatedUnix: 1700000000,
		},
		ColonyEpoch: &internalpb.ColonyEpoch{
			Epoch:     oldEpoch,
			SetBy:     "old-node.lab3.apiary.work",
			SetAtUnix: 1700000000,
		},
		Vms: map[string]*internalpb.VMDefinition{"vm-1": {Id: "vm-1", Name: "preserved"}},
	}

	dir := t.TempDir()
	cfg := Config{DataDir: dir, NodeID: "restored.lab3.apiary.work", BindAddr: "restored.lab3.apiary.work:17600"}
	if err := SeedSnapshot(cfg, state); err != nil {
		t.Fatalf("SeedSnapshot() error = %v", err)
	}

	seeded := readSeededState(t, dir)
	if seeded.GetColonyIdentity().GetColonyId() == oldColonyID {
		t.Error("the restored state kept the OLD Colony's identity; a restore must be a colony re-creation")
	}
	if seeded.GetColonyIdentity().GetColonyId() == "" {
		t.Error("the restored state has no Colony identity at all")
	}
	if seeded.GetColonyEpoch().GetEpoch() != 1 {
		t.Errorf("the restored state's epoch is %d, want 1", seeded.GetColonyEpoch().GetEpoch())
	}
	// The CONTENTS are preserved as data. Only the identity changes.
	if len(seeded.GetVms()) != 1 || seeded.GetVms()["vm-1"] == nil {
		t.Error("the restored state's contents were not preserved")
	}
}

// TestASeededRestoreDoesNotMutateTheCallersState: the caller's
// payload is not ours to rewrite. A caller that inspects its own
// state afterwards must not find its identity changed.
func TestASeededRestoreDoesNotMutateTheCallersState(t *testing.T) {
	oldColonyID := "11111111-2222-4333-8444-555555555555"
	state := &internalpb.FSMSnapshotState{
		ColonyIdentity: &internalpb.ColonyIdentity{ColonyId: oldColonyID, SetBy: "old"},
		ColonyEpoch:    &internalpb.ColonyEpoch{Epoch: 7, SetBy: "old"},
	}

	dir := t.TempDir()
	cfg := Config{DataDir: dir, NodeID: "restored.lab3.apiary.work", BindAddr: "restored.lab3.apiary.work:17600"}
	if err := SeedSnapshot(cfg, state); err != nil {
		t.Fatalf("SeedSnapshot() error = %v", err)
	}
	if state.GetColonyIdentity().GetColonyId() != oldColonyID {
		t.Error("SeedSnapshot() rewrote the caller's own state; the payload it was handed is not ours to mutate")
	}
	if state.GetColonyEpoch().GetEpoch() != 7 {
		t.Error("SeedSnapshot() rewrote the caller's epoch")
	}
}

// TestASeededRestoreIntoANonEmptyDataDirIsRefused: restoring onto a
// node that already has raft state is refused outright, matching how
// raft.BootstrapCluster itself refuses a second bootstrap.
func TestASeededRestoreIntoANonEmptyDataDirIsRefused(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{DataDir: dir, NodeID: "restored.lab3.apiary.work", BindAddr: "restored.lab3.apiary.work:17600"}
	state := &internalpb.FSMSnapshotState{}
	if err := SeedSnapshot(cfg, state); err != nil {
		t.Fatalf("the first SeedSnapshot() error = %v", err)
	}
	err := SeedSnapshot(cfg, state)
	if err == nil {
		t.Fatal("SeedSnapshot() seeded into a data dir that already had raft state")
	}
	if !strings.Contains(err.Error(), "already has raft state") {
		t.Errorf("refusal %q does not say the data dir already had state", err)
	}
}

// readSeededState reads back the state a SeedSnapshot wrote, by
// loading the snapshot the way a real start would.
func readSeededState(t *testing.T, dir string) *internalpb.FSMSnapshotState {
	t.Helper()
	store, err := raft.NewFileSnapshotStore(dir, snapshotRetain, os.Stderr)
	if err != nil {
		t.Fatalf("opening the snapshot store: %v", err)
	}
	snapshots, err := store.List()
	if err != nil {
		t.Fatalf("listing snapshots: %v", err)
	}
	if len(snapshots) == 0 {
		t.Fatal("the seeded directory holds no snapshot")
	}
	_, reader, err := store.Open(snapshots[0].ID)
	if err != nil {
		t.Fatalf("opening the seeded snapshot: %v", err)
	}
	defer reader.Close()
	body, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("reading the seeded snapshot: %v", err)
	}
	var state internalpb.FSMSnapshotState
	if err := proto.Unmarshal(body, &state); err != nil {
		t.Fatalf("unmarshaling the seeded snapshot: %v", err)
	}
	return &state
}

// TestASeededRestoreIntoADirectoryThatBelongedToAColonyIsRefused is
// ADR-0156 Stage 3's second requirement: restoring into a node that
// is currently a member of a live Colony is refused by default.
//
// The check that catches a node whose raft state is still on disk is
// HasExistingState. This one catches the case it does not: a node
// whose raft state was WIPED but which still carries the sidecar
// recording which Colony it belonged to.
//
// Restoring there would form a competing instance of a Colony that
// may still be running somewhere else - the same split-leader hazard
// the identity layer exists to prevent.
func TestASeededRestoreIntoADirectoryThatBelongedToAColonyIsRefused(t *testing.T) {
	dir := t.TempDir()
	if err := WriteColonySidecar(dir, "11111111-2222-4333-8444-555555555555", "member.lab3.apiary.work"); err != nil {
		t.Fatalf("writing the sidecar: %v", err)
	}
	cfg := Config{DataDir: dir, NodeID: "member.lab3.apiary.work", BindAddr: "member.lab3.apiary.work:17600"}
	err := SeedSnapshot(cfg, &internalpb.FSMSnapshotState{})
	if err == nil {
		t.Fatal("SeedSnapshot() restored into a directory that belonged to a Colony")
	}
	if !strings.Contains(err.Error(), "refusing to restore") {
		t.Errorf("refusal %q does not say the restore was refused", err)
	}
	if !strings.Contains(err.Error(), "11111111-2222-4333-8444-555555555555") {
		t.Errorf("refusal %q does not name the Colony the directory belonged to", err)
	}
}
