package raft

import (
	"testing"
	"time"
)

// TestSeedSnapshot_RestoresIntoFreshNode is the crux end-to-end proof
// for docs/adr/0051-raftd-config-save-restore.md: state exported from
// one real raft node can be seeded into a second, completely fresh
// data dir, and a node started against that seeded dir - with NO
// Bootstrap() call, exactly mirroring cmd/raftd's real
// hadState-true startup branch - comes up as a working single-node
// leader whose FSM state matches what was exported, byte for byte
// down to the individual records.
func TestSeedSnapshot_RestoresIntoFreshNode(t *testing.T) {
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

	for _, cmd := range []*struct {
		name string
		data []byte
	}{
		{"vm", mustMarshalCommand(t, createVMCmd("vm-1", "web-01"))},
		{"network", mustMarshalCommand(t, createNetworkCmd("net-1", "prod", "10.0.0.0/24"))},
		{"jail", mustMarshalCommand(t, createJailCmd("jail-1", "db-01"))},
		{"apikey", mustMarshalCommand(t, createAPIKeyCmd("key-1", "ci", "deadbeef"))},
	} {
		if _, err := source.Apply(cmd.data, 5*time.Second); err != nil {
			t.Fatalf("source.Apply(%s) error: %v", cmd.name, err)
		}
	}

	exported := source.fsm.SnapshotState()

	targetCfg := Config{
		NodeID:   "target",
		DataDir:  t.TempDir(),
		BindAddr: freeLoopbackAddr(t),
	}
	if err := SeedSnapshot(targetCfg, exported); err != nil {
		t.Fatalf("SeedSnapshot() error: %v", err)
	}

	// Checked BEFORE New(targetCfg) opens the same BoltDB file - bbolt's
	// exclusive file lock means checking this once the node is running
	// would deadlock against the node's own already-open handle.
	hadState, err := HasExistingState(targetCfg)
	if err != nil {
		t.Fatalf("HasExistingState(target) error: %v", err)
	}
	if !hadState {
		t.Fatalf("HasExistingState(target) = false, want true after SeedSnapshot")
	}

	// No Bootstrap() call here - this is the point of the test: a
	// seeded data dir must be picked up by the exact same startup
	// branch cmd/raftd's real "hadState == true" path takes.
	target, err := New(targetCfg)
	if err != nil {
		t.Fatalf("New(target) error: %v", err)
	}
	t.Cleanup(func() { target.Shutdown() })

	eventually(t, 5*time.Second, func() bool { return target.Status().IsLeader })

	vms, err := target.ListVMs()
	if err != nil {
		t.Fatalf("target.ListVMs() error: %v", err)
	}
	if len(vms) != 1 || vms[0].GetId() != "vm-1" || vms[0].GetName() != "web-01" {
		t.Errorf("target.ListVMs() = %+v, want one VM vm-1/web-01", vms)
	}

	networks, err := target.ListNetworks()
	if err != nil {
		t.Fatalf("target.ListNetworks() error: %v", err)
	}
	if len(networks) != 1 || networks[0].GetId() != "net-1" {
		t.Errorf("target.ListNetworks() = %+v, want one network net-1", networks)
	}

	jails, err := target.ListJails()
	if err != nil {
		t.Fatalf("target.ListJails() error: %v", err)
	}
	if len(jails) != 1 || jails[0].GetId() != "jail-1" {
		t.Errorf("target.ListJails() = %+v, want one jail jail-1", jails)
	}

	keys, err := target.ListAPIKeys()
	if err != nil {
		t.Fatalf("target.ListAPIKeys() error: %v", err)
	}
	if len(keys) != 1 || keys[0].GetId() != "key-1" {
		t.Errorf("target.ListAPIKeys() = %+v, want one key key-1", keys)
	}

	if id, _, valid, authEnabled := target.ValidateAPIKeyHash("deadbeef"); !valid || id != "key-1" || !authEnabled {
		t.Errorf("target.ValidateAPIKeyHash(deadbeef) = id=%q valid=%v authEnabled=%v, want id=key-1 valid=true authEnabled=true", id, valid, authEnabled)
	}
}

// TestSeedSnapshot_RejectsNonEmptyDataDir confirms SeedSnapshot refuses
// to touch a data dir that already has real raft state, mirroring
// raft.BootstrapCluster's own refusal (ErrCantBootstrap) once
// HasExistingState is true - restoring must never silently clobber an
// already-running node's state.
func TestSeedSnapshot_RejectsNonEmptyDataDir(t *testing.T) {
	cfg := Config{
		NodeID:   "node-1",
		DataDir:  t.TempDir(),
		BindAddr: freeLoopbackAddr(t),
	}
	node, err := New(cfg)
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}
	if err := node.Bootstrap(); err != nil {
		t.Fatalf("Bootstrap() error: %v", err)
	}
	eventually(t, 5*time.Second, func() bool { return node.Status().IsLeader })
	state := node.fsm.SnapshotState()

	// Shut down (closing the BoltDB file) before checking - bbolt's
	// exclusive file lock would otherwise deadlock SeedSnapshot's own
	// HasExistingState check against this still-open node.
	if err := node.Shutdown(); err != nil {
		t.Fatalf("Shutdown() error: %v", err)
	}

	if err := SeedSnapshot(cfg, state); err == nil {
		t.Fatal("SeedSnapshot() into an already-bootstrapped data dir succeeded, want an error")
	}
}
