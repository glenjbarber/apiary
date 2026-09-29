package manager

import (
	"context"
	"testing"

	rpcpb "github.com/glenjbarber/apiary/api/rpc"
)

// Tests for the managerd half of ADR-0056's membership-read failure:
// raftd reports it, and this handler has to pass it through as its own
// fact rather than drop it or fold it into raft_error.

// TestStatusCarriesNoMembershipErrorWhenReadable pins the zero value
// on a real raftd over a real socket. A healthy node must look healthy:
// if this field were ever populated on the common path, every consumer
// would learn to ignore it.
func TestStatusCarriesNoMembershipErrorWhenReadable(t *testing.T) {
	client := newManagerdRPCClient(t, newRaftdUDSSocket(t))

	resp, err := client.Status(context.Background(), &rpcpb.StatusRequest{})
	if err != nil {
		t.Fatalf("Status() error: %v", err)
	}

	if !resp.GetRaftReachable() {
		t.Fatalf("raft_reachable = false against a live raftd: %v", resp.GetRaftError())
	}
	if got := resp.GetRaftMembershipError(); got != "" {
		t.Errorf("raft_membership_error = %q on a healthy raftd, want empty", got)
	}
	if len(resp.GetMembers()) == 0 {
		t.Error("members is empty on a bootstrapped single-node Colony; it has exactly one member")
	}
}

// TestStatusMembershipErrorIsNotRaftError states the distinction the
// proto doc comment draws, as a test rather than only as prose: the two
// facts have different causes, and merging them would report a daemon
// that is up and answering as one that is unreachable.
func TestStatusMembershipErrorIsNotRaftError(t *testing.T) {
	client := newManagerdRPCClient(t, newRaftdUDSSocket(t))

	resp, err := client.Status(context.Background(), &rpcpb.StatusRequest{})
	if err != nil {
		t.Fatalf("Status() error: %v", err)
	}

	// Only one direction is checkable from here without a raftd whose
	// membership read genuinely fails - and hashicorp/raft's
	// GetConfiguration cannot be made to fail on demand, which is
	// exactly why the error is a reported field at all rather than
	// something a caller has to infer. What is checkable is that the
	// two fields are independent: raft_error is empty on a reachable
	// raftd regardless of membership, and is what raft_reachable
	// reports on.
	if resp.GetRaftReachable() && resp.GetRaftError() != "" {
		t.Errorf("raft_error = %q while raft_reachable is true; the two must not be set together", resp.GetRaftError())
	}
}
