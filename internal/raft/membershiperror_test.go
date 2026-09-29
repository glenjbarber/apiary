package raft

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/hashicorp/raft"

	internalpb "github.com/glenjbarber/apiary/api/internalpb"
)

// stubConfigurationFuture is a raft.ConfigurationFuture whose outcome
// the test chooses. It exists because hashicorp/raft's
// GetConfiguration cannot be made to fail on demand: it answers from
// the in-memory latest configuration and reports a nil error, so the
// branch Node.Status actually cares about - a read that failed - is
// otherwise unreachable from a test, and would ship untested.
type stubConfigurationFuture struct {
	err   error
	inner raft.Configuration
}

func (s stubConfigurationFuture) Error() error                      { return s.err }
func (s stubConfigurationFuture) Index() uint64                     { return 0 }
func (s stubConfigurationFuture) Response() interface{}             { return nil }
func (s stubConfigurationFuture) Configuration() raft.Configuration { return s.inner }

// TestReadServersReportsMembershipFailure is the ADR-0056 gap this
// change closes. Before it, a failed membership read produced an empty
// server list and nothing else - the error was dropped on the floor
// where no caller could ever see it.
func TestReadServersReportsMembershipFailure(t *testing.T) {
	servers, membershipErr := readServers(func() raft.ConfigurationFuture {
		return stubConfigurationFuture{err: errors.New("raft: membership unavailable")}
	})

	if len(servers) != 0 {
		t.Errorf("servers = %+v on a failed read, want none", servers)
	}
	if membershipErr == "" {
		t.Fatal("membership error = empty after a failed read, want the failure reported")
	}
}

// TestReadServersSucceedsWithoutError is the other half of the
// contract: a read that works reports no error, and an empty member
// list on that path is a real answer rather than an absence of one.
// Without this, "empty" and "failed" would be the same observation.
func TestReadServersSucceedsWithoutError(t *testing.T) {
	servers, membershipErr := readServers(func() raft.ConfigurationFuture {
		return stubConfigurationFuture{}
	})

	if membershipErr != "" {
		t.Errorf("membership error = %q on a successful read, want empty", membershipErr)
	}
	if servers != nil {
		t.Errorf("servers = %+v on an empty configuration, want nil", servers)
	}
}

// TestReadServersConvertsEverySuffrage keeps the membership conversion
// beside the new error handling rather than leaving it to be
// re-derived: a read that succeeded must still be reported exactly the
// way it always was, suffrage included.
func TestReadServersConvertsEverySuffrage(t *testing.T) {
	suffrages := []raft.ServerSuffrage{raft.Voter, raft.Nonvoter, raft.Staging}
	var cfg raft.Configuration
	for i, s := range suffrages {
		cfg.Servers = append(cfg.Servers, raft.Server{
			ID:       raft.ServerID(fmt.Sprintf("node-%d", i+1)),
			Address:  raft.ServerAddress(fmt.Sprintf("10.0.0.%d:17701", i+1)),
			Suffrage: s,
		})
	}

	servers, membershipErr := readServers(func() raft.ConfigurationFuture {
		return stubConfigurationFuture{inner: cfg}
	})

	if membershipErr != "" {
		t.Fatalf("membership error = %q, want empty", membershipErr)
	}
	want := []ServerInfo{
		{ID: "node-1", Address: "10.0.0.1:17701", Suffrage: "Voter"},
		{ID: "node-2", Address: "10.0.0.2:17701", Suffrage: "Nonvoter"},
		{ID: "node-3", Address: "10.0.0.3:17701", Suffrage: "Staging"},
	}
	if len(servers) != len(want) {
		t.Fatalf("servers = %+v, want %d entries", servers, len(want))
	}
	for i := range want {
		if servers[i] != want[i] {
			t.Errorf("servers[%d] = %+v, want %+v", i, servers[i], want[i])
		}
	}
}

// TestNodeStatusMembershipErrorIsEmptyWhenReadable pins the zero value
// on a real, healthy single-node cluster: the field has to mean "no
// error" for the overwhelmingly common case, or every consumer would
// have to special-case a node that is simply fine.
func TestNodeStatusMembershipErrorIsEmptyWhenReadable(t *testing.T) {
	node := bootstrappedNode(t, "node-1")

	status := node.Status()
	if status.MembershipError != "" {
		t.Errorf("MembershipError = %q on a healthy node, want empty", status.MembershipError)
	}
	if len(status.Servers) == 0 {
		t.Error("Servers is empty on a bootstrapped node; a single-node Colony has exactly one member")
	}
}

// TestServerStatusCarriesMembershipError checks the wire half: what the
// internal Status RPC actually returns to managerd. A field that exists
// on the Go struct but is never copied onto the response would be
// invisible to every caller, which is the same gap one layer up.
func TestServerStatusCarriesMembershipError(t *testing.T) {
	node := bootstrappedNode(t, "node-1")

	resp, err := NewServer(node).Status(context.Background(), &internalpb.StatusRequest{})
	if err != nil {
		t.Fatalf("Status() transport error: %v", err)
	}
	if resp.GetMembershipError() != "" {
		t.Errorf("membership_error = %q on a healthy node, want empty", resp.GetMembershipError())
	}
	if len(resp.GetServers()) == 0 {
		t.Error("servers is empty in the RPC response on a bootstrapped node")
	}
}

// bootstrappedNode returns a single-node, leader-elected raft Node.
func bootstrappedNode(t *testing.T, id string) *Node {
	t.Helper()
	node, err := New(Config{
		NodeID:   id,
		DataDir:  t.TempDir(),
		BindAddr: freeLoopbackAddr(t),
	})
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}
	t.Cleanup(func() { node.Shutdown() })

	if err := node.Bootstrap(); err != nil {
		t.Fatalf("Bootstrap() error: %v", err)
	}
	eventually(t, 5*time.Second, func() bool { return node.Status().IsLeader })
	return node
}
