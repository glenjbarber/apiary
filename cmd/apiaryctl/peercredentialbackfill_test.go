package main

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	internalpb "github.com/glenjbarber/apiary/api/internalpb"
	"github.com/glenjbarber/apiary/internal/frontendconfig"
	"github.com/glenjbarber/apiary/internal/joincredential"
	"github.com/glenjbarber/apiary/internal/localraft"
	"github.com/glenjbarber/apiary/internal/nodeconfig"
	raftnode "github.com/glenjbarber/apiary/internal/raft"
	"github.com/glenjbarber/apiary/internal/raftdconfig"
	"google.golang.org/grpc"
)

func TestDeriveColonyPeerAPIKeyIsStableAndSeparateFromJoinKey(t *testing.T) {
	one, err := joincredential.DeriveColonyPeerAPIKey("shared-colony-token")
	if err != nil {
		t.Fatal(err)
	}
	two, err := joincredential.DeriveColonyPeerAPIKey("shared-colony-token")
	if err != nil || one != two {
		t.Fatalf("repeated colony key = %q, %v; want stable %q", two, err, one)
	}
	joinKey, err := joincredential.DerivePeerAPIKey("shared-colony-token", "jreq-123", "comb-1")
	if err != nil {
		t.Fatal(err)
	}
	if one == joinKey {
		t.Fatal("colony-wide backfill key collides with a request-bound join key")
	}
	if _, err := joincredential.DeriveColonyPeerAPIKey(""); err == nil {
		t.Fatal("empty token unexpectedly derived a peer key")
	}
}

func TestEnsureColonyPeerKeyRegistersOnlyHashAndIsIdempotent(t *testing.T) {
	raftdPath, cleanup := startPeerBackfillRaft(t)
	defer cleanup()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := ensureColonyPeerKey(ctx, raftdPath, true, "known-raw-peer-key"); err != nil {
		t.Fatal(err)
	}
	if err := ensureColonyPeerKey(ctx, raftdPath, false, "known-raw-peer-key"); err != nil {
		t.Fatalf("repeat follower check: %v", err)
	}
	conn, err := localraft.Dial(raftdPath)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	client := internalpb.NewRaftInternalClient(conn)
	keys, err := client.ListAPIKeys(ctx, &internalpb.ListAPIKeysRequest{})
	if err != nil || keys.GetError() != "" || len(keys.GetKeys()) != 1 {
		t.Fatalf("ListAPIKeys = (%+v, %v), want one record", keys, err)
	}
	key := keys.GetKeys()[0]
	if key.GetId() != colonyPeerKeyID || key.GetRole() != "operator" || key.GetHashedKey() == "known-raw-peer-key" {
		t.Fatalf("stored record = %+v, want hash-only Operator record", key)
	}
	validated, err := client.ValidateAPIKeyHash(ctx, &internalpb.ValidateAPIKeyHashRequest{HashedKey: key.GetHashedKey()})
	if err != nil || !validated.GetValid() || validated.GetRole() != "operator" {
		t.Fatalf("ValidateAPIKeyHash = (%+v, %v), want valid Operator key", validated, err)
	}
}

func TestSaveLocalPeerCredentialPreservesOtherConfig(t *testing.T) {
	dir := t.TempDir()
	nodeManager := &nodeconfig.Manager{Path: filepath.Join(dir, "managerd.json")}
	frontManager := &frontendconfig.Manager{Path: filepath.Join(dir, "frontend.json")}
	nodeCfg := nodeconfig.Config{NodeID: "comb-1", RPCAddr: "10.0.0.1:17700", PeerAPIKey: ""}
	frontCfg := frontendconfig.Config{ManagerAddr: "127.0.0.1:17700", HTTPAddr: "127.0.0.1:8080"}
	if err := saveLocalPeerCredential(nodeManager, frontManager, nodeCfg, frontCfg, "peer-key"); err != nil {
		t.Fatal(err)
	}
	gotNode, err := nodeManager.Load()
	if err != nil {
		t.Fatal(err)
	}
	gotFrontend, err := frontManager.Load()
	if err != nil {
		t.Fatal(err)
	}
	if gotNode.PeerAPIKey != "peer-key" || gotNode.NodeID != nodeCfg.NodeID || gotNode.RPCAddr != nodeCfg.RPCAddr {
		t.Fatalf("manager config after save = %+v", gotNode)
	}
	if gotFrontend.ManagerAPIKey != "peer-key" || gotFrontend.ManagerAddr != frontCfg.ManagerAddr || gotFrontend.HTTPAddr != frontCfg.HTTPAddr {
		t.Fatalf("frontend config after save = %+v", gotFrontend)
	}
}

func startPeerBackfillRaft(t *testing.T) (string, func()) {
	t.Helper()
	node, err := raftnode.New(raftnode.Config{NodeID: "comb-1", DataDir: t.TempDir(), BindAddr: "127.0.0.1:0", FastElectionTiming: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := node.Bootstrap(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for !node.Status().IsLeader && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if !node.Status().IsLeader {
		t.Fatal("test Raft node did not become leader")
	}
	sockDir, err := os.MkdirTemp("", "peer-backfill-uds")
	if err != nil {
		t.Fatal(err)
	}
	socket := filepath.Join(sockDir, "raftd.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		os.RemoveAll(sockDir)
		t.Fatal(err)
	}
	server := grpc.NewServer()
	internalpb.RegisterRaftInternalServer(server, raftnode.NewServer(node))
	go server.Serve(listener)
	configPath := filepath.Join(t.TempDir(), "raftd.json")
	if err := (&raftdconfig.Manager{Path: configPath}).Save(raftdconfig.Config{Socket: socket}); err != nil {
		server.Stop()
		node.Shutdown()
		os.RemoveAll(sockDir)
		t.Fatal(err)
	}
	return configPath, func() { server.Stop(); node.Shutdown(); os.RemoveAll(sockDir) }
}
