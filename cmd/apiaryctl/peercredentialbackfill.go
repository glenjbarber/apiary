package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"flag"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strings"
	"time"

	internalpb "github.com/glenjbarber/apiary/api/internalpb"
	rpcpb "github.com/glenjbarber/apiary/api/rpc"
	"github.com/glenjbarber/apiary/internal/frontendconfig"
	"github.com/glenjbarber/apiary/internal/hostcert"
	"github.com/glenjbarber/apiary/internal/joincredential"
	"github.com/glenjbarber/apiary/internal/localraft"
	"github.com/glenjbarber/apiary/internal/nodeconfig"
	"github.com/glenjbarber/apiary/internal/peerca"
	"github.com/glenjbarber/apiary/internal/raftdconfig"
	"github.com/glenjbarber/apiary/internal/tlsdial"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"
)

const (
	colonyPeerKeyID   = "peer-colony-shared-v1"
	colonyPeerKeyName = "Colony peer forwarding"
)

type memberRestartCredential struct {
	Version   int    `json:"version"`
	NodeID    string `json:"node_id"`
	Challenge string `json:"challenge"`
	Token     string `json:"token"`
}

func fetchMemberRestartGuardrailToken(raftdPath string, cfg nodeconfig.Config) (string, error) {
	if cfg.TLSCert == "" || cfg.TLSKey == "" || (cfg.PeerTLS != nil && !*cfg.PeerTLS) {
		return "", fmt.Errorf("restart-guardrail token is absent and this Comb is not configured for verified peer TLS; provision the token locally before backfill")
	}
	certPEM, err := os.ReadFile(cfg.TLSCert)
	if err != nil {
		return "", fmt.Errorf("reading this Comb's pinned TLS certificate: %w", err)
	}
	block, _ := pem.Decode(certPEM)
	if block == nil || block.Type != "CERTIFICATE" {
		return "", fmt.Errorf("this Comb's tls_cert is malformed")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return "", fmt.Errorf("parsing this Comb's TLS certificate: %w", err)
	}
	privateKey, err := os.ReadFile(cfg.TLSKey)
	if err != nil {
		return "", fmt.Errorf("reading this Comb's TLS private key: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	conn, err := localraft.Dial(raftdPath)
	if err != nil {
		return "", err
	}
	defer conn.Close()
	status, err := internalpb.NewRaftInternalClient(conn).Status(ctx, &internalpb.StatusRequest{})
	if err != nil {
		return "", fmt.Errorf("reading local Raft membership: %w", err)
	}
	if status.GetIsLeader() {
		return "", fmt.Errorf("this Comb is the current leader but has no local restart-guardrail token; provision it on the leader")
	}
	nodeID := cfg.NodeID
	if nodeID == "" {
		nodeID = status.GetNodeId()
	}
	if nodeID == "" || nodeID != status.GetNodeId() {
		return "", fmt.Errorf("managerd node_id %q does not match local Raft node_id %q", nodeID, status.GetNodeId())
	}
	var raftAddress string
	for _, member := range status.GetServers() {
		if member.GetId() == status.GetLeaderId() {
			raftAddress = member.GetAddress()
			break
		}
	}
	if raftAddress == "" {
		return "", fmt.Errorf("local Raft status does not identify the current leader address")
	}
	host, _, err := net.SplitHostPort(raftAddress)
	if err != nil {
		return "", fmt.Errorf("invalid Raft leader address %q: %w", raftAddress, err)
	}
	port := cfg.PeerManagerdPort
	if port == "" {
		if _, rpcPort, splitErr := net.SplitHostPort(cfg.RPCAddr); splitErr == nil && rpcPort != "" {
			port = rpcPort
		} else {
			port = "17700"
		}
	}
	target := net.JoinHostPort(host, port)
	serverName := ""
	for _, entry := range strings.Split(cfg.PeerTLSHostnameMap, ",") {
		ip, name, ok := strings.Cut(strings.TrimSpace(entry), "=")
		if ok && ip == host && name != "" {
			serverName = name
			break
		}
	}
	caFile := cfg.PeerTLSCA
	if caFile == "" {
		caFile = peerca.DefaultPath
	}
	dialOpt, err := tlsdial.ManagerDialOption(true, caFile, serverName)
	if err != nil {
		return "", fmt.Errorf("configuring verified TLS to Colony leader: %w", err)
	}
	grpcConn, err := grpc.NewClient(target, dialOpt)
	if err != nil {
		return "", fmt.Errorf("dialing current Colony leader: %w", err)
	}
	defer grpcConn.Close()
	challengeBytes := make([]byte, 32)
	if _, err := rand.Read(challengeBytes); err != nil {
		return "", fmt.Errorf("generating credential challenge: %w", err)
	}
	challenge := hex.EncodeToString(challengeBytes)
	fingerprint := hostcert.Fingerprint(cert.Raw)
	response, err := rpcpb.NewManagerServiceClient(grpcConn).GetMemberRestartGuardrailCredential(ctx, &rpcpb.GetMemberRestartGuardrailCredentialRequest{
		NodeId: nodeID, RecipientTlsCertificatePem: string(certPEM), Challenge: challenge,
	})
	if err != nil {
		return "", fmt.Errorf("requesting the encrypted member credential: %w", err)
	}
	if response.GetLeaderHint() != "" {
		return "", fmt.Errorf("the selected Colony member is not leader; retry after Raft leadership is stable")
	}
	if response.GetError() != "" {
		return "", fmt.Errorf("requesting the encrypted member credential: %s", response.GetError())
	}
	plain, err := joincredential.Open(response.GetSealedCredential(), certPEM, privateKey, joincredential.MemberTokenAssociatedData(nodeID, fingerprint, challenge))
	if err != nil {
		return "", fmt.Errorf("decrypting the credential addressed to this Comb: %w", err)
	}
	var bundle memberRestartCredential
	if err := json.Unmarshal(plain, &bundle); err != nil || bundle.Version != 1 || bundle.NodeID != nodeID || bundle.Challenge != challenge || bundle.Token == "" {
		return "", fmt.Errorf("decrypted credential is malformed or bound to a different Comb")
	}
	return bundle.Token, nil
}

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
join credential delivery. Ensure the current Raft leader has a
restart-guardrail token and has restarted managerd, then run on each existing
Comb. A follower without a local token retrieves it in an envelope encrypted
to its pinned TLS certificate. The command stores only the API key hash in
Raft, derives the same key locally from the token, writes managerd.json and
frontend.json on this Comb, then restarts managerd. The frontend is not
restarted: it reads its manager_api_key live, so the key written above is
already in effect (issue #75).

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
	nodeManager := &nodeconfig.Manager{Path: managerPath}
	nodeCfg, err := nodeManager.Load()
	if err != nil {
		return fmt.Errorf("reading managerd config: %w", err)
	}
	tokenInfo, err := os.Lstat(tokenPath)
	if err == nil && (tokenInfo.Mode()&os.ModeSymlink != 0 || !tokenInfo.Mode().IsRegular() || tokenInfo.Mode().Perm() != 0o600) {
		return fmt.Errorf("%s must be a regular file with mode 0600", tokenPath)
	}
	var token string
	if err == nil {
		tokenBytes, readErr := os.ReadFile(tokenPath)
		if readErr != nil {
			return fmt.Errorf("reading restart-guardrail token: %w", readErr)
		}
		token = strings.TrimSpace(string(tokenBytes))
	} else if os.IsNotExist(err) {
		token, err = fetchMemberRestartGuardrailToken(raftdPath, nodeCfg)
		if err != nil {
			return err
		}
		if err := writeNewTokenAtomic(tokenPath, token); err != nil {
			return fmt.Errorf("installing Colony restart-guardrail token: %w", err)
		}
	} else {
		return fmt.Errorf("checking restart-guardrail token %s: %w", tokenPath, err)
	}
	key, err := joincredential.DeriveColonyPeerAPIKey(token)
	if err != nil {
		return err
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
	// Only managerd is restarted.
	//
	// managerd reads its peer_api_key once, at startup, so it needs the
	// restart to pick the new one up. The FRONTEND does not: its
	// manager_api_key is resolved live from frontend.json on every call
	// (cmd/frontend's liveKey, issue #75), so the key written just
	// above is already in effect and a restart would be a needless
	// interruption of a service that is working.
	//
	// Restarting the frontend here used to be necessary, and it was
	// part of what made the backfill feel heavier than it needed to
	// be - it took the operator's UI down on every member of a
	// multi-Comb Colony.
	if output, err := exec.Command("service", "apiary_managerd", "restart").CombinedOutput(); err != nil {
		return fmt.Errorf("credentials were written but restarting apiary_managerd failed: %v: %s", err, strings.TrimSpace(string(output)))
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
