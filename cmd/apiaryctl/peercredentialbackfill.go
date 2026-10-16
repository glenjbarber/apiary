package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	internalpb "github.com/glenjbarber/apiary/api/internalpb"
	"github.com/glenjbarber/apiary/internal/frontendconfig"
	"github.com/glenjbarber/apiary/internal/joincredential"
	"github.com/glenjbarber/apiary/internal/localraft"
	"github.com/glenjbarber/apiary/internal/nodeconfig"
	"github.com/glenjbarber/apiary/internal/raftdconfig"
	"google.golang.org/protobuf/proto"
)

const (
	colonyPeerKeyID   = "peer-colony-shared-v1"
	colonyPeerKeyName = "Colony peer forwarding"
)

func runPeerCredentialBackfill(args []string) int {
	fs := flag.NewFlagSet("apiaryctl peer-credential-backfill", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	var managerPath, frontendPath, raftdPath, tokenPath string
	fs.StringVar(&managerPath, "manager-config", nodeconfig.DefaultPath, "managerd.json path")
	fs.StringVar(&frontendPath, "frontend-config", frontendconfig.DefaultPath, "frontend.json path")
	fs.StringVar(&raftdPath, "raftd-config", raftdconfig.DefaultPath, "raftd.json path")
	fs.StringVar(&tokenPath, "token-file", defaultRestartGuardrailTokenPath, "Colony restart-guardrail token path")
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, `usage: apiaryctl peer-credential-backfill

Backfill the shared Colony peer API key on a Comb that predates automatic
join credential delivery. Run once on the current Raft leader first, then on
each other existing Comb. Every Comb must already hold the same
restart-guardrail token. The command stores only the API key hash in Raft,
derives the same key locally from the token, writes managerd.json and
frontend.json on this Comb, then restarts managerd and frontend.

Root only. The key and restart token are never printed. If this Comb is not
the leader and the shared key is not yet in Raft, run the command on the
leader first, wait for replication, then retry here.
`)
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintf(os.Stderr, "apiaryctl peer-credential-backfill: unexpected argument %q\n", fs.Arg(0))
		return 2
	}
	if os.Geteuid() != 0 {
		fmt.Fprintln(os.Stderr, "apiaryctl peer-credential-backfill: run as root; it writes daemon configuration and restarts services")
		return 1
	}
	if err := backfillPeerCredential(managerPath, frontendPath, raftdPath, tokenPath); err != nil {
		fmt.Fprintf(os.Stderr, "apiaryctl peer-credential-backfill: %v\n", err)
		return 1
	}
	fmt.Println("peer credentials were installed on this Comb; managerd and frontend were restarted")
	return 0
}

func backfillPeerCredential(managerPath, frontendPath, raftdPath, tokenPath string) error {
	tokenInfo, err := os.Lstat(tokenPath)
	if err != nil {
		return fmt.Errorf("checking restart-guardrail token %s: %w", tokenPath, err)
	}
	if tokenInfo.Mode()&os.ModeSymlink != 0 || !tokenInfo.Mode().IsRegular() || tokenInfo.Mode().Perm() != 0o600 {
		return fmt.Errorf("%s must be a regular file with mode 0600", tokenPath)
	}
	tokenBytes, err := os.ReadFile(tokenPath)
	if err != nil {
		return fmt.Errorf("reading restart-guardrail token: %w", err)
	}
	token := strings.TrimSpace(string(tokenBytes))
	key, err := joincredential.DeriveColonyPeerAPIKey(token)
	if err != nil {
		return err
	}

	nodeManager := &nodeconfig.Manager{Path: managerPath}
	nodeCfg, err := nodeManager.Load()
	if err != nil {
		return fmt.Errorf("reading managerd config: %w", err)
	}
	frontManager := &frontendconfig.Manager{Path: frontendPath}
	frontCfg, err := frontManager.Load()
	if err != nil {
		return fmt.Errorf("reading frontend config: %w", err)
	}
	if nodeCfg.PeerAPIKey != "" && nodeCfg.PeerAPIKey != key {
		return fmt.Errorf("managerd.json already has a different peer_api_key; refusing to replace it")
	}
	if frontCfg.ManagerAPIKey != "" && frontCfg.ManagerAPIKey != key {
		return fmt.Errorf("frontend.json already has a different manager_api_key; refusing to replace it")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	status, err := localraft.Query(ctx, raftdPath)
	if err != nil {
		return fmt.Errorf("reading local Raft status: %w", err)
	}
	if nodeCfg.NodeID == "" {
		nodeCfg.NodeID = status.NodeID
	}
	if nodeCfg.NodeID == "" || nodeCfg.NodeID != status.NodeID {
		return fmt.Errorf("managerd node_id %q does not match local Raft node_id %q", nodeCfg.NodeID, status.NodeID)
	}
	if err := ensureColonyPeerKey(ctx, raftdPath, status.IsLeader, key); err != nil {
		return err
	}

	if err := saveLocalPeerCredential(nodeManager, frontManager, nodeCfg, frontCfg, key); err != nil {
		return err
	}
	for _, service := range []string{"apiary_managerd", "apiary_frontend"} {
		if output, err := exec.Command("service", service, "restart").CombinedOutput(); err != nil {
			return fmt.Errorf("credentials were written but restarting %s failed: %v: %s", service, err, strings.TrimSpace(string(output)))
		}
	}
	return nil
}

func saveLocalPeerCredential(nodeManager *nodeconfig.Manager, frontManager *frontendconfig.Manager, nodeCfg nodeconfig.Config, frontCfg frontendconfig.Config, key string) error {
	nodeCfg.PeerAPIKey = key
	frontCfg.ManagerAPIKey = key
	if err := nodeManager.Save(nodeCfg); err != nil {
		return fmt.Errorf("saving managerd peer_api_key: %w", err)
	}
	if err := frontManager.Save(frontCfg); err != nil {
		return fmt.Errorf("saving frontend manager_api_key: %w", err)
	}
	return nil
}

func ensureColonyPeerKey(ctx context.Context, raftdPath string, isLeader bool, rawKey string) error {
	hash := sha256.Sum256([]byte(rawKey))
	hashed := hex.EncodeToString(hash[:])
	conn, err := localraft.Dial(raftdPath)
	if err != nil {
		return fmt.Errorf("connecting to local Raft: %w", err)
	}
	defer conn.Close()
	client := internalpb.NewRaftInternalClient(conn)
	if !isLeader {
		check, err := client.ValidateAPIKeyHash(ctx, &internalpb.ValidateAPIKeyHashRequest{HashedKey: hashed})
		if err != nil {
			return fmt.Errorf("checking replicated peer key: %w", err)
		}
		if !check.GetValid() || check.GetKeyId() != colonyPeerKeyID || check.GetRole() != "operator" {
			return fmt.Errorf("the shared peer key is not in this Comb's Raft state; run apiaryctl peer-credential-backfill on the Colony leader first, then retry here")
		}
		return nil
	}
	listed, err := client.ListAPIKeys(ctx, &internalpb.ListAPIKeysRequest{})
	if err != nil {
		return fmt.Errorf("listing leader API keys: %w", err)
	}
	if listed.GetLeaderHint() != "" || listed.GetError() != "" {
		return fmt.Errorf("listing leader API keys: %s", firstNonEmpty(listed.GetError(), "leader is "+listed.GetLeaderHint()))
	}
	for _, existing := range listed.GetKeys() {
		if existing.GetId() != colonyPeerKeyID {
			continue
		}
		if existing.GetHashedKey() != hashed || existing.GetName() != colonyPeerKeyName || existing.GetRole() != "operator" {
			return fmt.Errorf("reserved shared peer API key record conflicts with the restart token; refusing to overwrite it")
		}
		return nil
	}
	command := &internalpb.Command{Op: &internalpb.Command_CreateApiKey{CreateApiKey: &internalpb.CreateAPIKey{Key: &internalpb.ApiKey{
		Id: colonyPeerKeyID, Name: colonyPeerKeyName, HashedKey: hashed, CreatedUnix: time.Now().Unix(), Role: "operator",
	}}}}
	payload, err := proto.Marshal(command)
	if err != nil {
		return fmt.Errorf("encoding shared peer key record: %w", err)
	}
	applied, err := client.Apply(ctx, &internalpb.ApplyRequest{Payload: payload, TimeoutMs: 10000})
	if err != nil {
		return fmt.Errorf("recording shared peer key: %w", err)
	}
	if applied.GetError() != "" {
		return fmt.Errorf("recording shared peer key: %s", applied.GetError())
	}
	return nil
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return "unknown error"
}
