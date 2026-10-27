package raft

import (
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/hashicorp/raft"
	raftboltdb "github.com/hashicorp/raft-boltdb/v2"
	"google.golang.org/protobuf/proto"

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

// colonyIdentityCommitTimeout bounds the commit of this Colony's
// identity record. It is generous because the very first commit after
// an election can wait on the log to be replicated, and the operation
// is a single log entry.
const colonyIdentityCommitTimeout = 30 * time.Second

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

	// MembershipError is the error from this node's own raft membership
	// read, or "" when that read succeeded. It exists because an
	// empty Servers above is otherwise ambiguous: it reads as "this node
	// believes the Colony has no members" when it can equally mean "this
	// node could not read the membership at all", and only the second of
	// those is a fault. The zero value means "no error", so a genuinely
	// empty configuration stays distinguishable from an unreadable one.
	//
	// It is a field on the snapshot rather than a second return value
	// because everything else here is measured, not diagnosed: a caller
	// that wants an answer still gets one, with the reason attached. See
	// internal/recovery.ValidQuorumFact, which still rejects
	// QuorumSize == 0 - a node that genuinely holds no configuration yet
	// remains a real case, and this field makes the difference visible
	// rather than making the guard unnecessary.
	MembershipError string

	// StateDigest is the canonical digest of this node's own FSM state
	// (ADR-0143). It is read from the FSM rather than recomputed here,
	// and AppliedIndex above is likewise the FSM's own view - the two
	// describe the same point in this state machine's history, which is
	// what makes them safe to read together.
	StateDigest string

	// PreferredLeaderID is this node's own configured
	// Config.PreferredLeaderID (ADR-0180), empty when none is set. It
	// is included here, alongside LeaderID, so that "who is leading"
	// and "who is supposed to be leading" are visible together on the
	// same status snapshot rather than requiring a caller to separately
	// know each node's configuration to tell whether LeaderID matches
	// the Colony's declared preference.
	PreferredLeaderID string

	// ColonyID is the Colony this node belongs to (ADR-0156 Stage 1),
	// read from the node's REPLICATED state rather than from its
	// configuration.
	//
	// The distinction is the whole point. Two independently bootstrapped
	// raftd instances each form their own single-node cluster, and each
	// would answer from its own config file with a different value -
	// which is precisely the condition ADR-0156 records as verified and
	// undetectable. Read from replicated state, every voter of ONE
	// Colony answers with the SAME value, and two voters of DIFFERENT
	// colonies answer with different values, so the difference becomes
	// visible instead of silent.
	//
	// Empty means this node has no Colony identity yet - either it has
	// applied no SetColonyIdentity, or it was restored from a snapshot
	// predating ADR-0156. That is a real state, not an error.
	ColonyID string

	// ColonyEpoch is this Colony's GENERATION (ADR-0156 Stage 2) -
	// the monotonic counter that distinguishes two generations of the
	// same Colony. It is read from the node's REPLICATED state, on
	// the same terms as ColonyID above: a node answering from its own
	// configuration would be answering a different question, and a
	// node restored from an older snapshot WOULD answer differently
	// from the Colony it is trying to rejoin.
	//
	// Zero means this node has no epoch yet - either it has applied
	// no SetColonyEpoch, or it was restored from a snapshot predating
	// ADR-0156 Stage 2. That is a real state, not an error, and it is
	// exactly parallel to an empty ColonyID above.
	ColonyEpoch uint64
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

	// preferredLeader is PreferredLeaderTransfer's own cooldown
	// bookkeeping. Zero value is ready to use.
	preferredLeader preferredLeaderState

	// shutdownCh is closed by Shutdown, telling the leadership watcher
	// to stop. It is closed exactly once, by Shutdown.
	shutdownCh   chan struct{}
	shutdownOnce sync.Once

	// logf writes this package's own messages. It is a field rather
	// than a direct log.Printf call so a test can capture the
	// watcher's output, and so a caller can redirect it if it wants.
	logf func(format string, args ...any)
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
	if cfg.FastElectionTiming {
		// Scaled down together, preserving raft's own required
		// LeaderLeaseTimeout <= HeartbeatTimeout <= ElectionTimeout
		// relationship - see Config.FastElectionTiming's own doc
		// comment for why this exists at all.
		raftConfig.HeartbeatTimeout = 250 * time.Millisecond
		raftConfig.ElectionTimeout = 250 * time.Millisecond
		raftConfig.LeaderLeaseTimeout = 125 * time.Millisecond
	}

	boltPath := filepath.Join(cfg.DataDir, boltFileName)
	boltStore, err := raftboltdb.New(raftboltdb.Options{Path: boltPath})
	if err != nil {
		return nil, fmt.Errorf("raft: opening bolt store: %w", err)
	}

	snapshotStore, err := raft.NewFileSnapshotStore(cfg.DataDir, snapshotRetain, os.Stderr)
	if err != nil {
		return nil, fmt.Errorf("raft: creating snapshot store: %w", err)
	}

	transport, err := newTransport(cfg)
	if err != nil {
		return nil, err
	}

	r, err := raft.NewRaft(raftConfig, fsm, boltStore, boltStore, snapshotStore, transport)
	if err != nil {
		return nil, fmt.Errorf("raft: creating raft instance: %w", err)
	}

	node := &Node{
		raft:       r,
		fsm:        fsm,
		config:     cfg,
		store:      boltStore,
		shutdownCh: make(chan struct{}),
		logf:       log.Printf,
	}

	// ADR-0156 Stage 1: watch for leadership and establish the Colony
	// identity when this node becomes the leader.
	//
	// WHY A WATCHER RATHER THAN A ONE-SHOT AT STARTUP: the identity has
	// to be committed by the LEADER, and leadership is not fixed at
	// startup. A node that starts as a follower cannot commit one; a
	// node that later wins an election must; and a node that restarts
	// into an existing Colony must not need a second one. A watcher
	// that acts on every leadership acquisition satisfies all three
	// with one mechanism, and the "already set" refusal in the FSM's
	// apply function makes every repetition of it harmless.
	//
	// The watcher is best-effort: a failure to establish the identity
	// is reported through the node's own error channel rather than
	// taken as fatal, because the node's PRIMARY job - serving raft
	// state - must not be brought down by a bookkeeping step. The
	// identity is retried on the next leadership acquisition.
	go node.watchLeadership()

	return node, nil
}

// watchLeadership establishes the Colony identity whenever this node
// becomes the leader.
//
// It watches raft's leadership channel rather than polling Status(),
// so it reacts promptly to an election instead of on a timer.
//
// Every failure path is non-fatal and logged. The reason is that the
// identity is a SAFETY property layered on top of raft, not a
// prerequisite for it: a node that cannot record its own Colony
// identity can still serve every other function perfectly well, and the
// next leadership acquisition retries. The one thing that MUST be
// fatal - a node that cannot commit because it is not the leader - is
// handled by EnsureColonyIdentity itself.
func (n *Node) watchLeadership() {
	// LeaderCh is raft's own leadership notification channel. It holds
	// the LATEST leadership state rather than every transition: raft's
	// overrideNotifyBool replaces a pending value rather than queueing
	// one, so a node that gains and loses leadership between two reads
	// is reported as whatever it is now. That is exactly what this
	// watcher needs - it acts on the CURRENT state, and a missed
	// intermediate transition is not something it would have acted on
	// differently.
	leadershipCh := n.raft.LeaderCh()
	if leadershipCh == nil {
		return
	}

	for {
		select {
		case isLeader := <-leadershipCh:
			if !isLeader {
				continue
			}

			// Re-check the actual raft state rather than trusting the
			// notification alone. The channel says leadership was
			// acquired; by the time this goroutine runs, the node may
			// have lost it again, and committing then would fail with
			// a confusing error instead of a clear one.
			if n.raft.State() != raft.Leader {
				continue
			}

			colonyID, err := n.EnsureColonyIdentity(colonyIdentityCommitTimeout)
			if err != nil {
				// A shutdown is not a failure worth reporting: the
				// node is going away, and a commit that fails
				// because raft has shut down is the expected
				// outcome of stopping mid-operation.
				select {
				case <-n.shutdownCh:
					return
				default:
				}
				// Reported, not fatal. The next leadership
				// acquisition retries, and a node that never
				// manages it has no Colony identity - which
				// is visible to an operator through Status,
				// rather than being silently wrong.
				n.logf("could not establish the Colony identity: %v", err)
				continue
			}
			n.logf("Colony identity is %s", colonyID)
			// ADR-0156 Stage 2: the epoch is established by the
			// leader too, and for exactly the same reason the
			// identity is - leadership is not fixed at startup,
			// so a node that later wins an election must be able
			// to establish it, and a node restarting into an
			// existing Colony must not need a second one.
			//
			// The "already set" refusal in the apply function
			// makes every repetition harmless, exactly as it does
			// for the identity above.
			epoch, err := n.EnsureColonyEpoch(colonyIdentityCommitTimeout)
			if err != nil {
				select {
				case <-n.shutdownCh:
					return
				default:
				}
				n.logf("could not establish the Colony epoch: %v", err)
			} else {
				n.logf("Colony epoch is %d", epoch)
			}
			if err := WriteColonySidecar(n.config.DataDir, colonyID, n.config.NodeID); err != nil {
				// The sidecar is a guard's HINT, not the
				// authority on identity, so failing to write
				// it is worth saying out loud - it means the
				// self-bootstrap guard will not fire for this
				// node - but it is not a reason to refuse to
				// serve.
				n.logf("WARNING: could not record the Colony identity sidecar: %v", err)
			}
		case <-n.shutdownCh:
			return
		}
	}
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

// EnsureColonyIdentity makes sure this Colony has an identity, and is the
// only way one is ever established.
//
// The ID is a random UUIDv4, generated HERE, on the node that is
// bootstrapping the Colony, and committed through the replicated log.
//
// WHY GENERATED RATHER THAN DERIVED: ADR-0156's default is that the
// Colony ID is independent of the Colony CA fingerprint. Keeping the two
// anchors separate is what lets the Colony CA be rotated without the
// Colony's identity changing, and it stops a certificate from silently
// becoming the Colony's name. A fingerprint-derived ID would couple the
// Colony's identity to the very value ADR-0156's CA-rotation problem
// wants to be able to change.
//
// WHY IT IS COMMITTED RATHER THAN WRITTEN LOCALLY: a value held in one
// process's memory is a value two Combs can disagree about, and two
// Combs disagreeing about which Colony they are in is the exact
// condition this whole feature exists to make impossible. Committed
// through the log, every voter derives the same answer from the same
// bytes.
//
// WHY IT IS SAFE TO CALL THIS ON EVERY STARTUP: the "already set"
// decision lives in the FSM's apply function, not here. A Colony that
// already has an identity has that second SetColonyIdentity REFUSED by
// the apply function, and the refusal is the expected outcome, not an
// error. This method is therefore idempotent from the caller's point of
// view: it returns the Colony's identity, which is the pre-existing one
// in that case.
//
// The one case where this cannot apply is a node that is not the
// leader - it cannot commit anything, and it has no identity to report
// because it has not been told one yet. That is reported as an error
// rather than a silent empty string, because a caller that wanted an
// identity and got none must be able to tell "there is none" from "I
// could not find out".
func (n *Node) EnsureColonyIdentity(timeout time.Duration) (string, error) {
	// An existing identity is reported as-is. This is the common case
	// on every restart of a Colony that is already running.
	if existing := n.fsm.ColonyID(); existing != "" {
		return existing, nil
	}

	if n.raft.State() != raft.Leader {
		return "", fmt.Errorf("raft: cannot establish a Colony identity: this node is %s, not the leader, and only the leader can commit one", n.raft.State().String())
	}

	colonyID, err := newColonyID()
	if err != nil {
		return "", fmt.Errorf("generating a Colony identity: %w", err)
	}

	cmd := &internalpb.Command{
		Op: &internalpb.Command_SetColonyIdentity{
			SetColonyIdentity: &internalpb.SetColonyIdentity{
				ColonyId:    colonyID,
				SetBy:       n.config.NodeID,
				CreatedUnix: time.Now().Unix(),
			},
		},
	}
	payload, err := proto.Marshal(cmd)
	if err != nil {
		return "", fmt.Errorf("marshaling the Colony identity command: %w", err)
	}

	result, err := n.Apply(payload, timeout)
	if err != nil {
		return "", fmt.Errorf("committing the Colony identity: %w", err)
	}
	if result.Error != "" {
		// A refusal because the Colony ALREADY has an identity is not a
		// failure - it is a race we lost. Two leadership acquisitions
		// can both reach here before either has committed: each
		// generates a candidate, each tries to commit, and the FSM's
		// apply function keeps the first and refuses the second. The
		// right answer to "I lost the race" is the identity that WON,
		// not an error.
		//
		// Reading the identity back from the FSM is what makes this
		// correct: the winner's value is the committed one, so
		// reading it after the refusal returns exactly the value
		// every other voter will also hold.
		if existing := n.fsm.ColonyID(); existing != "" {
			return existing, nil
		}
		return "", fmt.Errorf("committing the Colony identity: %s", result.Error)
	}
	return result.GetColonyIdentity().GetColonyId(), nil
}

// EnsureColonyEpoch makes sure this Colony has an epoch, and is the
// only way one is ever established.
//
// The epoch is always the initial one, because ADR-0156's Decision 3
// fixes the starting value for an existing Colony at 1 and the epoch
// advances by a separate command (AdvanceColonyEpoch) rather than by
// being established at whatever value the caller felt like.
//
// WHY IT IS COMMITTED RATHER THAN WRITTEN LOCALLY: the same reason
// EnsureColonyIdentity's own comment gives. A value held in one
// process's memory is a value two Combs can disagree about, and two
// Combs disagreeing about which generation they are in is exactly the
// condition the epoch exists to make impossible.
//
// WHY IT IS SAFE TO CALL THIS ON EVERY LEADERSHIP ACQUISITION: the
// "already set" decision lives in the FSM's apply function, not here.
// A Colony that already has an epoch has that second SetColonyEpoch
// REFUSED by the apply function, and the refusal is the expected
// outcome, not an error. This method is therefore idempotent from the
// caller's point of view: it returns the Colony's epoch, which is the
// pre-existing one in that case.
//
// The one case where this cannot apply is a node that is not the
// leader - it cannot commit anything, and it has no epoch to report
// because it has not been told one yet.
func (n *Node) EnsureColonyEpoch(timeout time.Duration) (uint64, error) {
	// An existing epoch is reported as-is. This is the common case on
	// every restart of a Colony that is already running.
	if existing := n.fsm.ColonyEpoch(); existing != 0 {
		return existing, nil
	}

	if n.raft.State() != raft.Leader {
		return 0, fmt.Errorf("raft: cannot establish a Colony epoch: this node is %s, not the leader, and only the leader can commit one", n.raft.State().String())
	}

	cmd := &internalpb.Command{
		Op: &internalpb.Command_SetColonyEpoch{
			SetColonyEpoch: &internalpb.SetColonyEpoch{
				Epoch:     initialColonyEpoch,
				SetBy:     n.config.NodeID,
				SetAtUnix: time.Now().Unix(),
			},
		},
	}
	payload, err := proto.Marshal(cmd)
	if err != nil {
		return 0, fmt.Errorf("marshaling the Colony epoch command: %w", err)
	}

	result, err := n.Apply(payload, timeout)
	if err != nil {
		return 0, fmt.Errorf("committing the Colony epoch: %w", err)
	}
	if result.Error != "" {
		// A refusal because the Colony ALREADY has an epoch is not a
		// failure - it is a race we lost, for exactly the reason
		// EnsureColonyIdentity's own comment gives. Reading the
		// epoch back from the FSM is what makes this correct: the
		// winner's value is the committed one, so reading it after
		// the refusal returns exactly the value every other voter
		// will also hold.
		if existing := n.fsm.ColonyEpoch(); existing != 0 {
			return existing, nil
		}
		return 0, fmt.Errorf("committing the Colony epoch: %s", result.Error)
	}
	return result.GetColonyEpoch().GetEpoch(), nil
}

// AdvanceColonyEpoch moves this Colony to the next generation.
//
// The rules - advance by exactly one, and never downwards - are
// enforced in the FSM's apply function rather than here, so every
// replica decides them identically from the same committed log. This
// method's job is only to submit the command and report what the FSM
// decided.
//
// WHO CALLS THIS: ADR-0156 says the epoch is advanced on a controlled
// reset of the Colony, or on a full rebuild after the state is
// destroyed. This stage provides the command and its rules; it wires
// no caller to it yet, because the reset path that would want to call
// it is a reset that PRESERVES the Colony's identity while
// superseding its generation, and that is ADR-0156's Stage 3
// (sidecar and restore semantics) territory. A deliberate
// `raftd -reset` today MOVES THE WHOLE DATA DIRECTORY ASIDE, sidecar
// included, and the next start forms a genuinely new Colony with a new
// identity and epoch 1 - which is Stage 1's documented behaviour, not
// a generation change of an existing Colony.
func (n *Node) AdvanceColonyEpoch(timeout time.Duration) (uint64, error) {
	if n.raft.State() != raft.Leader {
		return 0, ErrNotLeader
	}

	current := n.fsm.ColonyEpoch()
	if current == 0 {
		return 0, fmt.Errorf("raft: cannot advance the Colony epoch: this Colony has no epoch recorded yet")
	}

	cmd := &internalpb.Command{
		Op: &internalpb.Command_AdvanceColonyEpoch{
			AdvanceColonyEpoch: &internalpb.AdvanceColonyEpoch{
				Epoch:     current + 1,
				SetBy:     n.config.NodeID,
				SetAtUnix: time.Now().Unix(),
			},
		},
	}
	payload, err := proto.Marshal(cmd)
	if err != nil {
		return 0, fmt.Errorf("marshaling the Colony epoch advance command: %w", err)
	}

	result, err := n.Apply(payload, timeout)
	if err != nil {
		return 0, fmt.Errorf("committing the Colony epoch advance: %w", err)
	}
	if result.Error != "" {
		return 0, fmt.Errorf("committing the Colony epoch advance: %s", result.Error)
	}
	return result.GetColonyEpoch().GetEpoch(), nil
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

	servers, membershipErr := readServers(n.raft.GetConfiguration)

	return Status{
		IsLeader:          n.raft.State() == raft.Leader,
		LeaderID:          string(leaderID),
		NodeID:            n.config.NodeID,
		LastLogIndex:      n.raft.LastIndex(),
		AppliedIndex:      n.fsm.AppliedIndex(),
		RaftState:         n.raft.State().String(),
		Servers:           servers,
		MembershipError:   membershipErr,
		StateDigest:       n.fsm.StateDigest(),
		PreferredLeaderID: n.config.PreferredLeaderID,
		ColonyID:          n.fsm.ColonyID(),
		ColonyEpoch:       n.fsm.ColonyEpoch(),
	}
}

// readServers performs the membership read Status reports, returning the
// member list and the read's error text side by side.
//
// A failed read used to be dropped here entirely, leaving Servers nil
// with nothing anywhere in the returned Status to say so (ADR-0056). The
// future is passed in as a function rather than taken from a *raft.Raft
// directly for a testable reason: hashicorp/raft's GetConfiguration
// answers from the in-memory latest configuration and reports its error
// as nil, so there is no way to make it fail on demand from a test, and
// the branch that actually matters here would otherwise never run.
func readServers(getConfiguration func() raft.ConfigurationFuture) ([]ServerInfo, string) {
	cfgFuture := getConfiguration()
	if err := cfgFuture.Error(); err != nil {
		return nil, err.Error()
	}

	var servers []ServerInfo
	for _, s := range cfgFuture.Configuration().Servers {
		servers = append(servers, ServerInfo{
			ID:       string(s.ID),
			Address:  string(s.Address),
			Suffrage: suffrageString(s.Suffrage),
		})
	}
	return servers, ""
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

// AddNonvoter adds a new non-voting server to the cluster. It is
// AddVoter's exact counterpart - same leader-only requirement, same
// prevIndex guard - and exists because a cluster can legitimately hold
// members that carry no vote (ADR-0056's "Staging"/"Nonvoter"
// suffrage), so the membership API needs to be able to build that shape
// as well as the all-voter one.
func (n *Node) AddNonvoter(id, address string, prevIndex uint64, timeout time.Duration) error {
	future := n.raft.AddNonvoter(raft.ServerID(id), raft.ServerAddress(address), prevIndex, timeout)
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

// ListVMsLocal/ListNetworksLocal read the same FSM state as ListVMs/
// ListNetworks above, but deliberately with NO leadership check - see
// these methods' own doc comment in raftd.proto for why: every node's
// Reconciler needs to see the current VM/network list regardless of
// raft leadership, the same reasoning ValidateAPIKeyHash already
// established for auth checks. Raft already replicates FSM state onto
// every node as they replay the log, so this is just as correct as the
// leader's own copy, modulo the usual brief replication lag right
// after a write.
func (n *Node) ListVMsLocal() []*internalpb.VMDefinition {
	return n.fsm.ListVMs()
}

func (n *Node) ListNetworksLocal() []*internalpb.NetworkDefinition {
	return n.fsm.ListNetworks()
}

// GetPendingJoinRequestLocal/ListPendingJoinRequestsLocal mirror
// ListVMsLocal/ListNetworksLocal exactly - deliberately no leader-only
// restriction, for the same reason (ADR-0083): a joining Comb polls
// whichever specific existing member it originally contacted.
func (n *Node) GetPendingJoinRequestLocal(requestID string) (*internalpb.PendingJoinRequest, bool) {
	return n.fsm.PendingJoinRequest(requestID)
}

func (n *Node) ListPendingJoinRequestsLocal() []*internalpb.PendingJoinRequest {
	return n.fsm.ListPendingJoinRequests()
}

// AuthorizationUse backs GetAuthorizationUse (ADR-0147 Part 3) - the
// replicated record of which request spent which root-owned
// authorization entry.
//
// Leader-only, exactly like GetVM and for a sharper reason: the answer
// decides whether an irreversible membership change proceeds, and a
// follower answering from its own possibly-stale map would report an
// already-spent entry as unspent. Failing OPEN on the record of use is
// the precise bug the replicated record was added to prevent, so a
// follower refuses rather than guesses.
func (n *Node) AuthorizationUse(authorizationID string) (requestID string, consumedAtUnix int64, found bool, err error) {
	if n.raft.State() != raft.Leader {
		return "", 0, false, ErrNotLeader
	}
	requestID, consumedAtUnix, found = n.fsm.AuthorizationUse(authorizationID)
	return requestID, consumedAtUnix, found, nil
}

// RestartLeaseStateLocal backs GetRestartLeaseStateLocal (ADR-0103) - a
// plain read of this node's own FSM copy, mirroring
// GetPendingJoinRequestLocal's own "any node can answer" posture.
func (n *Node) RestartLeaseStateLocal(service string) (*internalpb.RestartLease, *internalpb.RestartRecord) {
	return n.fsm.RestartLeaseState(service)
}

// ColonyJoinWindowLocal backs GetColonyJoinWindowLocal (ADR-0147
// Part 4) - the same "any node can answer from its own FSM copy"
// posture as RestartLeaseStateLocal above, and the reason it is safe:
// the window is already raft-replicated, and a joining Comb introduced
// to a follower must see the same window one introduced to the leader
// would see.
//
// The bool is whether this window is live at this node's own clock. It
// is computed here rather than by the caller so that the liveness
// answer and the window that answer was computed from can never be
// two different reads.
func (n *Node) ColonyJoinWindowLocal() (*internalpb.ColonyJoinWindow, bool) {
	w := n.fsm.ColonyJoinWindowState()
	return w, ColonyJoinWindowLive(w, time.Now().Unix())
}

// ColonyDiskSizeLocal backs GetColonyDiskSizeLocal (ADR-0148) - the
// same "any node can answer from its own FSM copy" posture as
// ColonyJoinWindowLocal above.
//
// It is deliberately NOT leader-only: the floor is replicated state and
// the reconciler reads it on EVERY Comb, so a follower that could not
// answer for itself would size its disks differently from the leader's,
// which is exactly the divergence ADR-0148 exists to prevent.
func (n *Node) ColonyDiskSizeLocal() *internalpb.ColonyDiskSize {
	return n.fsm.ColonyDiskSizeState()
}

// ListTrustedPeersLocal backs ListTrustedPeersLocal's RPC
// (ADR-0147 Part 4) - the same "any node can answer from its own FSM
// copy" posture as ColonyJoinWindowLocal above, and for the same
// reason it is safe: the pins are already raft-replicated.
//
// It is deliberately not leader-restricted even though its only
// consumer is the derived peer-ca.pem writer, because that writer runs
// on every Comb. A follower that could not answer would leave the
// derived file on a follower describing a store one node older than the
// leader's, which is precisely the "members disagree about who they
// trust" state the store exists to prevent.
func (n *Node) ListTrustedPeersLocal() []*internalpb.TrustedPeer {
	return n.fsm.ListTrustedPeers()
}

// ListCertificateReservationsLocal returns ADR-0171's prepared-
// certificate reservations from this node's own replicated copy.
//
// A local read, for the same reason ListTrustedPeersLocal is one: the
// reservations are raft-replicated, so a follower answers from the same
// committed state the leader would.
func (n *Node) ListCertificateReservationsLocal() []*internalpb.CertificateReservation {
	return n.fsm.ListCertificateReservations()
}

// TrustedPeerLocal reports whether a pin exists for nodeID. Used by
// the RemoveServer hook below to ask whether there is anything to drop
// before it drops it. Read-then-unpin rather than
// unpin-and-ignore-the-error, because "this node was not pinned" is
// not a failure of the membership change that preceded it and must not
// be reported as one.
func (n *Node) TrustedPeerPinnedLocal(nodeID string) bool {
	for _, peer := range n.fsm.ListTrustedPeers() {
		if peer.GetNodeId() == nodeID {
			return true
		}
	}
	return false
}

// ColonyUpdateStateLocal backs GetColonyUpdateStateLocal (ADR-0145) - the
// same "any node can answer from its own FSM copy" posture as
// RestartLeaseStateLocal above.
//
// The bool it returns alongside the record is whether THIS node is the
// leader, and it is returned rather than inferred by the caller because
// "can answer for itself" and "is authoritative" are different claims: a
// follower's copy of an applied record can lag the leader's by one
// commit. Nothing in the enforcement path reads this - every grant,
// advance and settle goes through Apply, which only committed consensus
// decides - so the flag exists for a human and a UI, not for a decision.
func (n *Node) ColonyUpdateStateLocal() (active *internalpb.ColonyUpdate, history []*internalpb.ColonyUpdate, authoritative bool) {
	active, history = n.fsm.ColonyUpdateState()
	return active, history, n.raft.State() == raft.Leader
}

// ColonyUpdateByIDLocal is ColonyUpdateStateLocal's one-operation
// variant, for the "what happened to the operation I was told about?"
// question that the colony-wide answer cannot answer once it has
// settled.
func (n *Node) ColonyUpdateByIDLocal(operationID string) (*internalpb.ColonyUpdate, error) {
	rec, ok := n.fsm.ColonyUpdateByID(operationID)
	if !ok {
		return nil, fmt.Errorf("no controlled update with operation id %q has ever been granted on this node's view of replicated state", operationID)
	}
	return rec, nil
}

// GetJail/ListJails mirror GetNetwork/ListNetworks exactly, for
// JailDefinitions instead.
func (n *Node) GetJail(id string) (jail *internalpb.JailDefinition, found bool, err error) {
	if n.raft.State() != raft.Leader {
		return nil, false, ErrNotLeader
	}
	jail, found = n.fsm.Jail(id)
	return jail, found, nil
}

func (n *Node) ListJails() ([]*internalpb.JailDefinition, error) {
	if n.raft.State() != raft.Leader {
		return nil, ErrNotLeader
	}
	return n.fsm.ListJails(), nil
}

// ListJailsLocal mirrors ListVMsLocal/ListNetworksLocal - no leadership
// check, needed by internal/cluster's Reconciler on every node.
func (n *Node) ListJailsLocal() []*internalpb.JailDefinition {
	return n.fsm.ListJails()
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
func (n *Node) ValidateAPIKeyHash(hash string) (id, role string, valid, authEnabled bool) {
	id, role, valid = n.fsm.ValidateHash(hash)
	authEnabled = n.fsm.AuthEnabled()
	return id, role, valid, authEnabled
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

// ExportState returns this node's current, live FSM state - see
// FSM.SnapshotState's doc comment for why this reads the FSM directly
// rather than raft's own on-disk snapshot store. Leader-only, like
// ListAPIKeys: this is a deliberate, occasional admin action, not a
// hot-path reconciler read, so it gets the strongest consistency
// guarantee available rather than the *Local reasoning used
// elsewhere in this file.
func (n *Node) ExportState() (*internalpb.FSMSnapshotState, error) {
	if n.raft.State() != raft.Leader {
		return nil, ErrNotLeader
	}
	return n.fsm.SnapshotState(), nil
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
	n.shutdownOnce.Do(func() { close(n.shutdownCh) })
	if err := n.raft.Shutdown().Error(); err != nil {
		return err
	}
	return n.store.Close()
}
