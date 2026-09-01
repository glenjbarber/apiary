// Package manager implements managerd's core logic: a client for raftd's
// internal protocol, and the server side of managerd's own external RPC
// API (api/rpc).
package manager

import (
	"context"
	"fmt"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	internalpb "github.com/glenjbarber/apiary/api/internalpb"
	raftnode "github.com/glenjbarber/apiary/internal/raft"
)

// RaftClient wraps a connection to raftd's internal RaftInternal service
// over a Unix domain socket.
type RaftClient struct {
	conn   *grpc.ClientConn
	client internalpb.RaftInternalClient
}

// Dial connects to raftd's internal socket at socketPath. token is the
// shared secret raftd's own -internal-token requires, if any - see
// internal/raft.TokenCredentials; an empty token attaches nothing,
// matching raftd's own opt-in behavior when it has no token configured
// either.
func Dial(socketPath, token string) (*RaftClient, error) {
	conn, err := grpc.NewClient(
		"unix://"+socketPath,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithPerRPCCredentials(raftnode.TokenCredentials(token)),
	)
	if err != nil {
		return nil, fmt.Errorf("manager: dialing raftd at %s: %w", socketPath, err)
	}
	return &RaftClient{conn: conn, client: internalpb.NewRaftInternalClient(conn)}, nil
}

// Status queries raftd's current raft state.
func (c *RaftClient) Status(ctx context.Context) (*internalpb.StatusResponse, error) {
	return c.client.Status(ctx, &internalpb.StatusRequest{})
}

// Apply submits payload to raftd's raft log. Unused by managerd's v1
// external API, but kept here so RaftClient is a complete wrapper rather
// than a Status-only stub that needs reworking once real operations land.
func (c *RaftClient) Apply(ctx context.Context, payload []byte, timeout time.Duration) (*internalpb.ApplyResponse, error) {
	return c.client.Apply(ctx, &internalpb.ApplyRequest{
		Payload:   payload,
		TimeoutMs: uint32(timeout.Milliseconds()),
	})
}

// GetVM reads a single VM definition from raftd.
func (c *RaftClient) GetVM(ctx context.Context, id string) (*internalpb.GetVMResponse, error) {
	return c.client.GetVM(ctx, &internalpb.GetVMRequest{Id: id})
}

// ListVMs reads all VM definitions from raftd.
func (c *RaftClient) ListVMs(ctx context.Context) (*internalpb.ListVMsResponse, error) {
	return c.client.ListVMs(ctx, &internalpb.ListVMsRequest{})
}

// GetNetwork reads a single network definition from raftd.
func (c *RaftClient) GetNetwork(ctx context.Context, id string) (*internalpb.GetNetworkResponse, error) {
	return c.client.GetNetwork(ctx, &internalpb.GetNetworkRequest{Id: id})
}

// ListNetworks reads all network definitions from raftd.
func (c *RaftClient) ListNetworks(ctx context.Context) (*internalpb.ListNetworksResponse, error) {
	return c.client.ListNetworks(ctx, &internalpb.ListNetworksRequest{})
}

// ListVMsLocal/ListNetworksLocal are the non-leader-restricted variants
// used by internal/cluster's Reconciler - see raftd.proto's doc comment
// on why the reconciler can't use the leader-only ListVMs/ListNetworks
// above.
func (c *RaftClient) ListVMsLocal(ctx context.Context) (*internalpb.ListVMsResponse, error) {
	return c.client.ListVMsLocal(ctx, &internalpb.ListVMsRequest{})
}

func (c *RaftClient) ListNetworksLocal(ctx context.Context) (*internalpb.ListNetworksResponse, error) {
	return c.client.ListNetworksLocal(ctx, &internalpb.ListNetworksRequest{})
}

// GetJail reads a single jail definition from raftd.
func (c *RaftClient) GetJail(ctx context.Context, id string) (*internalpb.GetJailResponse, error) {
	return c.client.GetJail(ctx, &internalpb.GetJailRequest{Id: id})
}

// ListJails reads all jail definitions from raftd (leader-only).
func (c *RaftClient) ListJails(ctx context.Context) (*internalpb.ListJailsResponse, error) {
	return c.client.ListJails(ctx, &internalpb.ListJailsRequest{})
}

// ListJailsLocal is the non-leader-restricted variant used by
// internal/cluster's Reconciler, mirroring ListVMsLocal/ListNetworksLocal.
func (c *RaftClient) ListJailsLocal(ctx context.Context) (*internalpb.ListJailsResponse, error) {
	return c.client.ListJailsLocal(ctx, &internalpb.ListJailsRequest{})
}

// ValidateAPIKeyHash checks hashedKey (may be empty) against raftd's
// own FSM state - see ADR-0023 for why this is deliberately not
// leader-restricted, unlike every other read above.
func (c *RaftClient) ValidateAPIKeyHash(ctx context.Context, hashedKey string) (*internalpb.ValidateAPIKeyHashResponse, error) {
	return c.client.ValidateAPIKeyHash(ctx, &internalpb.ValidateAPIKeyHashRequest{HashedKey: hashedKey})
}

// ListAPIKeys reads all API keys from raftd (leader-only).
func (c *RaftClient) ListAPIKeys(ctx context.Context) (*internalpb.ListAPIKeysResponse, error) {
	return c.client.ListAPIKeys(ctx, &internalpb.ListAPIKeysRequest{})
}

// Close closes the underlying connection to raftd.
func (c *RaftClient) Close() error {
	return c.conn.Close()
}
