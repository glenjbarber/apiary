// Package localraft dials this Comb's own raftd, over the local unix
// socket its raftd.json names, for the root-run tools that need to ask
// the local raft a question while holding an incident.
//
// `apiaryctl force-restart` has to know whether this Comb is the
// Colony's current leader before it restarts anything on it, and
// `apiaryctl join-authorize` reads the same fact to warn that an
// entry written on a follower is one no approval will ever consult.
// Both resolve the socket, attach the optional internal token and call
// raftd's own Status RPC. Two copies of that is two places for the two
// to disagree about which socket this Comb has, and a disagreement
// there is not a crash: the wrong socket is a wrong Colony's answer,
// read as confidently as the right one.
//
// `apiaryctl pin-trusted-peers` resolves the same socket and calls one
// other read-only RPC, ListTrustedPeersLocal, for the reason its own
// file states: there is no external RPC that lists the trust store, and
// a root-run tool on a Comb should not need one to be written.
//
// The dependency is kept one level down deliberately. This package
// imports internal/raft only for TokenCredentials, so a small operator
// tool that asks raftd one question does not drag the raft library,
// bbolt and the cluster packages in behind it. apiaryctl already links
// all of them for join-authorize, so nothing new reaches the installed
// binary; the point is that the boundary is stated rather than reached
// by accident.
//
// Every call here is a read of raftd's own state and nothing else. No
// function in this package writes, applies, or proposes.
package localraft

import (
	"context"
	"fmt"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/proto"

	internalpb "github.com/glenjbarber/apiary/api/internalpb"
	raftnode "github.com/glenjbarber/apiary/internal/raft"
	"github.com/glenjbarber/apiary/internal/raftdconfig"
)

// DefaultTimeout bounds every call this package makes. It is generous
// for a unix socket on the same machine and short enough that a raftd
// which has wedged is a six-second pause in a command that was about
// to be refused anyway, rather than a hang an operator has to Ctrl-C.
const DefaultTimeout = 6 * time.Second

// Dial opens a client connection to this Comb's own raftd.
//
// Both the socket and the optional internal token come out of the same
// raftd.json, read through raftd's own loader, so the answer is the
// one raftd itself would give for itself and the credential is the one
// raftd itself would check. An empty configPath means
// raftdconfig.DefaultPath, which is the file raftd reads when it is
// started with no -config.
//
// The connection is lazy, exactly as grpc.NewClient is: nothing is
// dialled until an RPC is made, and the first RPC is where a missing
// socket, a dead raftd or a wrong internal token surfaces. That is the
// behaviour these callers want, because a refusal that names the real
// reason is worth more than an eager connect that fails with less
// detail.
func Dial(configPath string) (*grpc.ClientConn, error) {
	if configPath == "" {
		configPath = raftdconfig.DefaultPath
	}
	cfg, err := (&raftdconfig.Manager{Path: configPath}).Load()
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", configPath, err)
	}
	// Unreachable through today's loader, and kept anyway. Manager.Load
	// starts from Defaults(), which names /var/run/apiary/raftd.sock,
	// so a config naming no socket resolves to raftd's own default -
	// which is the right answer, and is what raftd itself opens. The
	// branch is a guard on that contract rather than on the file: if a
	// future loader ever returns an empty socket, this refuses and says
	// which file is wrong, instead of dialing "unix://" and failing
	// later with something about a transport. An earlier copy of this
	// check read as a check on the file and was not one; the difference
	// is the comment.
	if cfg.Socket == "" {
		return nil, fmt.Errorf("%s resolved to no socket for raftd, so there is no local raft to read status from", configPath)
	}
	conn, err := grpc.NewClient(
		"unix://"+cfg.Socket,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithPerRPCCredentials(raftnode.TokenCredentials(cfg.InternalToken)),
	)
	if err != nil {
		return nil, fmt.Errorf("dialing raftd at %s: %w", cfg.Socket, err)
	}
	return conn, nil
}

// Status is the part of raftd's own Status response a root shell tool
// has any business acting on: who this Comb thinks it is, what raft
// state it is in, and who it believes the leader is.
//
// It is a copy of a protobuf response on purpose. A caller that wanted
// the last applied index or the state digest would be building a
// second opinion about the Colony's state from a local read, and this
// package is not the place to do that.
type Status struct {
	// IsLeader is raftd's own answer for itself, from
	// n.raft.State() == raft.Leader. It is the one field anything here
	// refuses on.
	IsLeader bool

	// NodeID is the raft identity this Comb reports, which is the
	// identity an operator needs in order to act on what the refusal
	// told them.
	NodeID string

	// RaftState is raft's own name for the current state - leader,
	// follower, candidate - and is carried so a refusal can say which
	// one it saw rather than only what it concluded.
	RaftState string

	// LeaderID is the leader as this Comb currently knows it, which is
	// what raftd itself uses to build its leader hints. On a Comb that
	// cannot see a leader it is empty, and that is reported as what it
	// is rather than as an error.
	LeaderID string
}

// ListTrustedPeers returns ADR-0147 Part 4's replicated trust store as
// this Comb's own raftd currently holds it, keyed by node ID.
//
// A read, like everything else here, and for the same reason
// `apiaryctl pin-trusted-peers` cannot use the external API for it:
// there is no ManagerService RPC that lists trusted peers at all, and
// adding one would be adding a read of replicated state to the public
// surface to serve a command that already has a root-owned socket on
// the machine the read is about.
//
// The map is a copy, not the wire message's own map, and every
// TrustedPeer in it is cloned, because a caller that mutates what it
// got back would be editing raftd's snapshot of itself without a log
// entry - which is the one thing the replicated store exists to
// prevent. Sorted is raftd's own ordering and is not re-sorted here.
func ListTrustedPeers(ctx context.Context, configPath string) (map[string]*internalpb.TrustedPeer, error) {
	conn, err := Dial(configPath)
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	resp, err := internalpb.NewRaftInternalClient(conn).ListTrustedPeersLocal(ctx, &internalpb.ListTrustedPeersRequest{})
	if err != nil {
		return nil, fmt.Errorf("asking raftd for its trusted peers: %w", err)
	}
	peers := make(map[string]*internalpb.TrustedPeer, len(resp.GetPeers()))
	for _, peer := range resp.GetPeers() {
		if peer.GetNodeId() == "" {
			// A record with no key is not in the store, whatever else
			// it says, and silently dropping it here would hide a
			// store this code cannot reason about.
			return nil, fmt.Errorf("raftd returned a trusted peer with no node_id, so this Comb's trust store cannot be read as a set of members")
		}
		peers[peer.GetNodeId()] = proto.Clone(peer).(*internalpb.TrustedPeer)
	}
	return peers, nil
}

// Query asks this Comb's own raftd about itself over the local socket
// and returns the subset described on Status.
//
// The caller owns ctx and its deadline, so the same code serves a tool
// that has already bounded its whole run and one that has not. A
// failure is returned as an error and never as a zero Status: "could
// not be answered" and "answered, and the answer is no" are different
// facts, and every caller in this repository acts on them differently.
func Query(ctx context.Context, configPath string) (Status, error) {
	conn, err := Dial(configPath)
	if err != nil {
		return Status{}, err
	}
	defer conn.Close()

	resp, err := internalpb.NewRaftInternalClient(conn).Status(ctx, &internalpb.StatusRequest{})
	if err != nil {
		return Status{}, fmt.Errorf("asking raftd for its status: %w", err)
	}
	return Status{
		IsLeader:  resp.GetIsLeader(),
		NodeID:    resp.GetNodeId(),
		RaftState: resp.GetRaftState(),
		LeaderID:  resp.GetLeaderId(),
	}, nil
}
