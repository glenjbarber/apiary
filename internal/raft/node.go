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

	internalpb "github.com/glenjbarber/apiary/api/internalpb"
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
	Servers      []ServerInfo
}

// ServerInfo describes one member of the cluster configuration.
type ServerInfo struct {
	ID       string
	Address  string
	Suffrage string
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
	if err := translateMembershipErr(future.Error()); err != nil {
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

	var servers []ServerInfo
	if cfgFuture := n.raft.GetConfiguration(); cfgFuture.Error() == nil {
		for _, s := range cfgFuture.Configuration().Servers {
			servers = append(servers, ServerInfo{
				ID:       string(s.ID),
				Address:  string(s.Address),
				Suffrage: suffrageString(s.Suffrage),
			})
		}
	}

	return Status{
		IsLeader:     n.raft.State() == raft.Leader,
		LeaderID:     string(leaderID),
		NodeID:       n.config.NodeID,
		LastLogIndex: n.raft.LastIndex(),
		AppliedIndex: n.fsm.AppliedIndex(),
		RaftState:    n.raft.State().String(),
		Servers:      servers,
	}
}

func suffrageString(s raft.ServerSuffrage) string {
	switch s {
	case raft.Voter:
		return "Voter"
	case raft.Nonvoter:
		return "Nonvoter"
	case raft.Staging:
		return "Staging"
	default:
		return "Unknown"
	}
}

// AddVoter adds a new voting server to the cluster. It only succeeds when
// this node is the current leader. prevIndex, if non-zero, guards against
// concurrent configuration changes (see raft.Raft.AddVoter).
func (n *Node) AddVoter(id, address string, prevIndex uint64, timeout time.Duration) error {
	future := n.raft.AddVoter(raft.ServerID(id), raft.ServerAddress(address), prevIndex, timeout)
	return translateMembershipErr(future.Error())
}

// RemoveServer removes a server from the cluster, whether voter or
// non-voter. It only succeeds when this node is the current leader.
func (n *Node) RemoveServer(id string, prevIndex uint64, timeout time.Duration) error {
	future := n.raft.RemoveServer(raft.ServerID(id), prevIndex, timeout)
	return translateMembershipErr(future.Error())
}

// GetVM reads a single VM definition. It only succeeds when this node is
// the current leader, matching Apply's write consistency model - v1
// deliberately does not serve reads from potentially-lagging followers.
func (n *Node) GetVM(id string) (vm *internalpb.VMDefinition, found bool, err error) {
	if n.raft.State() != raft.Leader {
		return nil, false, ErrNotLeader
	}
	vm, found = n.fsm.VM(id)
	return vm, found, nil
}

// ListVMs reads all VM definitions, subject to the same leader-only
// requirement as GetVM.
func (n *Node) ListVMs() ([]*internalpb.VMDefinition, error) {
	if n.raft.State() != raft.Leader {
		return nil, ErrNotLeader
	}
	return n.fsm.ListVMs(), nil
}

// GetNetwork/ListNetworks mirror GetVM/ListVMs exactly, for
// NetworkDefinitions instead.
func (n *Node) GetNetwork(id string) (network *internalpb.NetworkDefinition, found bool, err error) {
	if n.raft.State() != raft.Leader {
		return nil, false, ErrNotLeader
	}
	network, found = n.fsm.Network(id)
	return network, found, nil
}

func (n *Node) ListNetworks() ([]*internalpb.NetworkDefinition, error) {
	if n.raft.State() != raft.Leader {
		return nil, ErrNotLeader
	}
	return n.fsm.ListNetworks(), nil
}

// ValidateAPIKeyHash checks hash against this node's own FSM state and
// reports whether API-key auth has ever been enabled at all (see
// FSM.AuthEnabled - permanent once set, never reverts even if every
// key is later revoked) - deliberately with NO leadership check,
// unlike every other read method in this file. Raft already replicates
// FSM state (including API keys) onto every node, follower or leader,
// as they replay the log; several managerd RPCs need to keep
// authenticating callers even on a non-leader node (HostStats,
// GetVMConsole, and UploadISO are all local, per-node operations that
// don't otherwise depend on raft leadership at all), so key validation
// can't require the leader the way GetNetwork/ListNetworks do. The
// tradeoff is a lookup against a possibly-just-slightly-stale local
// copy during the brief replication window right after a
// create/revoke - acceptable, and documented in ADR-0023.
func (n *Node) ValidateAPIKeyHash(hash string) (id string, valid, authEnabled bool) {
	id, valid = n.fsm.ValidateHash(hash)
	authEnabled = n.fsm.AuthEnabled()
	return id, valid, authEnabled
}

// ListAPIKeys is a normal leader-only read (mirrors ListNetworks),
// used only for the admin-facing key list - never for per-request
// authentication, which goes through ValidateAPIKeyHash instead.
func (n *Node) ListAPIKeys() ([]*internalpb.ApiKey, error) {
	if n.raft.State() != raft.Leader {
		return nil, ErrNotLeader
	}
	return n.fsm.ListAPIKeys(), nil
}

func translateMembershipErr(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, raft.ErrNotLeader) || errors.Is(err, raft.ErrLeadershipLost) {
		return ErrNotLeader
	}
	return err
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
