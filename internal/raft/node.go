package raft

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"time"

	"github.com/hashicorp/raft"
	raftboltdb "github.com/hashicorp/raft-boltdb/v2"
)

// boltFileName is the single BoltDB file used for both the raft log store
// and stable store.
const boltFileName = "raft.db"

// snapshotRetain is how many snapshots raft.FileSnapshotStore keeps.
const snapshotRetain = 2

// ErrNotLeader is returned by Node.Apply when this node is not the current
// raft leader.
var ErrNotLeader = errors.New("raft: this node is not the leader")

// Status is a snapshot of a Node's current raft state, used to answer the
// internal Status RPC.
type Status struct {
	IsLeader     bool
	LeaderID     string
	NodeID       string
	LastLogIndex uint64
	AppliedIndex uint64
	RaftState    string
}

// Node wraps a *raft.Raft instance, its FSM, and the storage/transport it
// was built with.
type Node struct {
	raft   *raft.Raft
	fsm    *FSM
	config Config
	store  *raftboltdb.BoltStore
}

// New constructs a Node backed by BoltDB log/stable stores and a file
// snapshot store under cfg.DataDir, and a real TCP transport on
// cfg.BindAddr. It does not bootstrap a cluster; call Bootstrap for that.
func New(cfg Config) (*Node, error) {
	cfg, err := cfg.withDefaults()
	if err != nil {
		return nil, err
	}

	if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
		return nil, fmt.Errorf("raft: creating data dir: %w", err)
	}

	fsm := NewFSM()

	raftConfig := raft.DefaultConfig()
	raftConfig.LocalID = raft.ServerID(cfg.NodeID)

	boltPath := filepath.Join(cfg.DataDir, boltFileName)
	boltStore, err := raftboltdb.New(raftboltdb.Options{Path: boltPath})
	if err != nil {
		return nil, fmt.Errorf("raft: opening bolt store: %w", err)
	}

	snapshotStore, err := raft.NewFileSnapshotStore(cfg.DataDir, snapshotRetain, os.Stderr)
	if err != nil {
		return nil, fmt.Errorf("raft: creating snapshot store: %w", err)
	}

	addr, err := net.ResolveTCPAddr("tcp", cfg.BindAddr)
	if err != nil {
		return nil, fmt.Errorf("raft: resolving bind addr: %w", err)
	}
	transport, err := raft.NewTCPTransport(cfg.BindAddr, addr, 3, 10*time.Second, os.Stderr)
	if err != nil {
		return nil, fmt.Errorf("raft: creating transport: %w", err)
	}

	r, err := raft.NewRaft(raftConfig, fsm, boltStore, boltStore, snapshotStore, transport)
	if err != nil {
		return nil, fmt.Errorf("raft: creating raft instance: %w", err)
	}

	return &Node{raft: r, fsm: fsm, config: cfg, store: boltStore}, nil
}

// HasExistingState reports whether cfg.DataDir already contains raft state
// (log/stable store entries), i.e. whether this is a restart rather than a
// first start. Callers should skip Bootstrap when this returns true.
func HasExistingState(cfg Config) (bool, error) {
	cfg, err := cfg.withDefaults()
	if err != nil {
		return false, err
	}

	boltPath := filepath.Join(cfg.DataDir, boltFileName)
	if _, err := os.Stat(boltPath); err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}

	boltStore, err := raftboltdb.New(raftboltdb.Options{Path: boltPath})
	if err != nil {
		return false, fmt.Errorf("raft: opening bolt store: %w", err)
	}
	defer boltStore.Close()

	snapshotStore, err := raft.NewFileSnapshotStore(cfg.DataDir, snapshotRetain, os.Stderr)
	if err != nil {
		return false, fmt.Errorf("raft: creating snapshot store: %w", err)
	}

	return raft.HasExistingState(boltStore, boltStore, snapshotStore)
}

// Bootstrap initializes a single-server raft cluster consisting of just
// this node. It is idempotent to call only when HasExistingState is false;
// calling it against a node that already has state returns an error.
func (n *Node) Bootstrap() error {
	cfg := raft.Configuration{
		Servers: []raft.Server{
			{
				ID:      raft.ServerID(n.config.NodeID),
				Address: raft.ServerAddress(n.config.BindAddr),
			},
		},
	}
	return n.raft.BootstrapCluster(cfg).Error()
}

// Apply submits payload to the raft log and waits up to timeout for it to
// be committed and applied. It only succeeds when this node is the leader.
func (n *Node) Apply(payload []byte, timeout time.Duration) (*FSMApplyResult, error) {
	future := n.raft.Apply(payload, timeout)
	if err := future.Error(); err != nil {
		if errors.Is(err, raft.ErrNotLeader) || errors.Is(err, raft.ErrLeadershipLost) {
			return nil, ErrNotLeader
		}
		return nil, err
	}

	result, ok := future.Response().(*FSMApplyResult)
	if !ok {
		return nil, fmt.Errorf("raft: unexpected FSM response type %T", future.Response())
	}
	return result, nil
}

// LeaderHint returns the address of the current leader as raft currently
// knows it, or "" if unknown.
func (n *Node) LeaderHint() string {
	addr, _ := n.raft.LeaderWithID()
	return string(addr)
}

// Status returns a snapshot of this node's current raft state.
func (n *Node) Status() Status {
	_, leaderID := n.raft.LeaderWithID()
	return Status{
		IsLeader:     n.raft.State() == raft.Leader,
		LeaderID:     string(leaderID),
		NodeID:       n.config.NodeID,
		LastLogIndex: n.raft.LastIndex(),
		AppliedIndex: n.fsm.AppliedIndex(),
		RaftState:    n.raft.State().String(),
	}
}

// Shutdown gracefully stops the raft instance and releases its underlying
// BoltDB store, so the data directory can be safely reopened afterward
// (e.g. by a subsequent Node against the same DataDir).
func (n *Node) Shutdown() error {
	if err := n.raft.Shutdown().Error(); err != nil {
		return err
	}
	return n.store.Close()
}
