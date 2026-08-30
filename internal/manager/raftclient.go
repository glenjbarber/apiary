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
)

// RaftClient wraps a connection to raftd's internal RaftInternal service
// over a Unix domain socket.
type RaftClient struct {
	conn   *grpc.ClientConn
	client internalpb.RaftInternalClient
}

// Dial connects to raftd's internal socket at socketPath.
func Dial(socketPath string) (*RaftClient, error) {
	conn, err := grpc.NewClient(
		"unix://"+socketPath,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
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

// Close closes the underlying connection to raftd.
func (c *RaftClient) Close() error {
	return c.conn.Close()
}
