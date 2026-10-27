package raft

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/hashicorp/raft"
	raftboltdb "github.com/hashicorp/raft-boltdb/v2"
	"google.golang.org/protobuf/proto"

	internalpb "github.com/glenjbarber/apiary/api/internalpb"
)

// seedSnapshotVersion is the raft snapshot format version SeedSnapshot
// writes - raft.FileSnapshotStore.Create only supports version 1 today
// (raft.SnapshotVersion(1)), and a version-1 snapshot's Configuration
// field (not the legacy Peers encoding) is what raft.NewRaft actually
// reads back on restore.
const seedSnapshotVersion = raft.SnapshotVersion(1)

// SeedSnapshot pre-populates an empty cfg.DataDir with a single raft
// snapshot containing state, so that a subsequent normal (non-reset,
// non-join) raftd start picks it up as its initial FSM state - see
// docs/adr/0051-raftd-config-save-restore.md for the full reasoning.
// This relies on existing, unmodified raft behavior, not a new runtime
// code path:
//
//   - raft.HasExistingState (used by this package's own HasExistingState
//     wrapper, and by cmd/raftd's normal startup to decide whether to
//     call Node.Bootstrap) reports true once ANY snapshot exists, even
//     with a completely empty log/stable store.
//   - raft.NewRaft's own construction-time restoreSnapshot loads the
//     newest snapshot's FSM payload AND its Configuration/
//     ConfigurationIndex fields before ever scanning the log - no log
//     entries or explicit BootstrapCluster call are needed.
//   - raft.BootstrapCluster itself refuses (ErrCantBootstrap) once
//     HasExistingState is already true, so a seeded node can never be
//     bootstrapped a second time by accident.
//
// cmd/raftd's own HasExistingState wrapper checks for the BoltDB log/
// stable store FILE's existence before ever consulting the snapshot
// store at all - so SeedSnapshot also opens (creating) and immediately
// closes that BoltDB file, even though it writes no log entries into
// it, purely so that existence check sees a real file on the next
// start. Skipping this would make the seeded snapshot silently
// invisible: HasExistingState would report false, and the next start
// would bootstrap a fresh, empty cluster instead of loading it.
//
// Returns an error if cfg.DataDir already has existing raft state -
// restoring onto a non-empty node is refused outright, matching how
// raft.BootstrapCluster itself refuses a second bootstrap attempt.
//
// ADR-0156 STAGE 3: A RESTORE IS A COLONY RE-CREATION, NOT A MEMBER
// REJOIN. The seeded state therefore gets a NEW Colony identity and
// epoch 1, whatever the restored payload says.
//
// WHY. A restore preserves the serialized FSM state and nothing else:
// not the log, not the stable vote keys, not the membership history,
// not the leader identity. The restored node is a brand new one-node
// Colony. If it kept the old Colony's identity, it would be a second
// instance of that Colony, presenting the same identity to the
// original members - which is exactly the split-leader hazard the
// whole Colony Protocol layer exists to prevent. The restored PAYLOAD
// cannot be trusted to carry an identity the surrounding world agrees
// with, because the world never saw that identity: the original
// members' membership history, vote keys and log are exactly what a
// restore does not preserve.
//
// The restored state's CONTENTS are preserved as data. Its identity is
// not. An operator who wants the restored data back in the original
// Colony performs the restore into a separate directory and then JOINS
// through the normal, authorized join flow, so the joining node's
// identity is negotiated rather than assumed.
func SeedSnapshot(cfg Config, state *internalpb.FSMSnapshotState) error {
	cfg, err := cfg.withDefaults()
	if err != nil {
		return err
	}

	hadState, err := HasExistingState(cfg)
	if err != nil {
		return fmt.Errorf("raft: checking existing state before seeding: %w", err)
	}
	if hadState {
		return errors.New("raft: refusing to seed a snapshot into a data dir that already has raft state")
	}

	// ADR-0156 Stage 3, requirement 2: restoring into a node that is
	// currently a member of a live Colony is refused by default.
	//
	// The check above catches a node whose raft state is still on
	// disk. This one catches the case it does not: a node whose raft
	// state was WIPED but which still carries the sidecar recording
	// which Colony it belonged to. Restoring there would form a
	// competing instance of a Colony that is still running
	// somewhere else, which is the same split-leader hazard the
	// identity layer exists to prevent.
	//
	// The sidecar is a HINT, not the authority - but for the purpose
	// of a REFUSAL it is exactly the right evidence, and a refusal is
	// the safe direction. An operator who genuinely wants to restore
	// there moves the whole data directory aside (raftd -reset),
	// which carries the sidecar with it.
	sidecar, err := ReadColonySidecar(cfg.DataDir)
	if err != nil {
		return fmt.Errorf("raft: reading the Colony sidecar before seeding: %w", err)
	}
	if sidecar != nil && sidecar.ColonyID != "" {
		return fmt.Errorf("raft: refusing to restore into %s, which records that it belonged to Colony %s (node %s). Restoring here would form a competing instance of a Colony that may still be running. Restore into a separate directory and join through the normal join flow, or move the whole data directory aside (raftd -reset), which carries the sidecar with it",
			cfg.DataDir, sidecar.ColonyID, sidecar.NodeID)
	}

	if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
		return fmt.Errorf("raft: creating data dir: %w", err)
	}

	// Open (creating) and close the BoltDB log/stable store purely so
	// its file exists on disk - see the doc comment above. No log
	// entries are ever written to it.
	boltPath := filepath.Join(cfg.DataDir, boltFileName)
	boltStore, err := raftboltdb.New(raftboltdb.Options{Path: boltPath})
	if err != nil {
		return fmt.Errorf("raft: creating bolt store: %w", err)
	}
	if err := boltStore.Close(); err != nil {
		return fmt.Errorf("raft: closing bolt store: %w", err)
	}

	snapshotStore, err := raft.NewFileSnapshotStore(cfg.DataDir, snapshotRetain, os.Stderr)
	if err != nil {
		return fmt.Errorf("raft: creating snapshot store: %w", err)
	}

	// InmemTransport is only used to encode the legacy, unread-on-
	// restore Peers field a version-1 Create call still computes - see
	// SeedSnapshot's doc comment. No real network transport is needed.
	_, trans := raft.NewInmemTransport("")

	configuration := raft.Configuration{
		Servers: []raft.Server{
			{
				ID:      raft.ServerID(cfg.NodeID),
				Address: raft.ServerAddress(cfg.BindAddr),
			},
		},
	}

	// ADR-0156 Stage 3: the seeded state is a NEW Colony, so it gets
	// a new identity and epoch 1 - regardless of what the restored
	// payload carried.
	//
	// The state is COPIED first, because the caller's state is not
	// ours to mutate: a caller that inspects its own payload
	// afterwards must not find its identity rewritten.
	seeded := proto.Clone(state).(*internalpb.FSMSnapshotState)
	colonyID, err := newColonyID()
	if err != nil {
		return fmt.Errorf("raft: generating a new Colony identity for the restored state: %w", err)
	}
	seeded.ColonyIdentity = &internalpb.ColonyIdentity{
		ColonyId:    colonyID,
		SetBy:       cfg.NodeID,
		CreatedUnix: time.Now().UTC().Unix(),
	}
	seeded.ColonyEpoch = &internalpb.ColonyEpoch{
		Epoch:     1,
		SetBy:     cfg.NodeID,
		SetAtUnix: time.Now().UTC().Unix(),
	}
	state = seeded

	const seedIndex, seedTerm, seedConfigurationIndex = 1, 1, 1
	sink, err := snapshotStore.Create(seedSnapshotVersion, seedIndex, seedTerm, configuration, seedConfigurationIndex, trans)
	if err != nil {
		return fmt.Errorf("raft: creating snapshot sink: %w", err)
	}

	data, err := proto.Marshal(state)
	if err != nil {
		sink.Cancel()
		return fmt.Errorf("raft: marshaling seeded state: %w", err)
	}
	if _, err := sink.Write(data); err != nil {
		sink.Cancel()
		return fmt.Errorf("raft: writing seeded snapshot: %w", err)
	}
	if err := sink.Close(); err != nil {
		return fmt.Errorf("raft: finalizing seeded snapshot: %w", err)
	}

	return nil
}
