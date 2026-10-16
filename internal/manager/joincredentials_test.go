package manager

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"strings"
	"testing"

	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
	"google.golang.org/protobuf/proto"

	internalpb "github.com/glenjbarber/apiary/api/internalpb"
	rpcpb "github.com/glenjbarber/apiary/api/rpc"
	"github.com/glenjbarber/apiary/internal/hostcert"
	"github.com/glenjbarber/apiary/internal/joincredential"
)

func TestSealApprovedJoinCredentialsStoresOnlyAPIKeyHash(t *testing.T) {
	srv, certificatePEM, privateKeyPEM, fingerprint := joinCredentialTestServer(t)
	request := &rpcpb.GetJoinRequestStatusRequest{RecipientTlsCertificatePem: string(certificatePEM)}
	pending := &internalpb.PendingJoinRequest{
		RequestId: "jreq-0123456789abcdef", NodeId: "comb-7",
		Status:             internalpb.JoinRequestStatus_JOIN_REQUEST_STATUS_APPROVED,
		TlsCertFingerprint: fingerprint,
	}
	ctx := peer.NewContext(context.Background(), &peer.Peer{AuthInfo: credentials.TLSInfo{}})
	if _, leaderAddress, err := srv.provisionApprovedJoinAPIKey(ctx, pending); err != nil || leaderAddress != "" {
		t.Fatalf("provisionApprovedJoinAPIKey() = (%q, %v)", leaderAddress, err)
	}
	envelope, leaderAddress, err := srv.sealApprovedJoinCredentials(ctx, request, pending)
	if err != nil {
		t.Fatal(err)
	}
	if leaderAddress != "" {
		t.Fatalf("leader address = %q, want local leader", leaderAddress)
	}
	if bytes.Contains(envelope, []byte(srv.restartGuardrailToken)) {
		t.Fatal("sealed envelope contains the raw restart-guardrail token")
	}
	plain, err := joincredential.Open(envelope, certificatePEM, privateKeyPEM,
		joincredential.AssociatedData(pending.GetRequestId(), pending.GetNodeId(), fingerprint))
	if err != nil {
		t.Fatal(err)
	}
	var bundle joincredential.Bundle
	if err := json.Unmarshal(plain, &bundle); err != nil {
		t.Fatal(err)
	}
	if bundle.RequestID != pending.GetRequestId() || bundle.NodeID != pending.GetNodeId() || bundle.RestartGuardrailToken != srv.restartGuardrailToken {
		t.Fatalf("decrypted bundle identity/token mismatch: %+v", bundle)
	}
	wantKey, err := joincredential.DerivePeerAPIKey(srv.restartGuardrailToken, pending.GetRequestId(), pending.GetNodeId())
	if err != nil {
		t.Fatal(err)
	}
	if bundle.PeerAPIKey != wantKey {
		t.Fatal("bundle peer API key is not the request-bound derived key")
	}
	keys, err := srv.raft.ListAPIKeys(context.Background())
	if err != nil || keys.GetError() != "" {
		t.Fatalf("ListAPIKeys() = (%+v, %v)", keys, err)
	}
	if len(keys.GetKeys()) != 1 {
		t.Fatalf("key table has %d entries, want exactly one", len(keys.GetKeys()))
	}
	stored := keys.GetKeys()[0]
	if stored.GetRole() != "operator" || stored.GetHashedKey() != hashAPIKey(bundle.PeerAPIKey) {
		t.Fatalf("stored peer key record = %+v", stored)
	}
	storedWire, err := proto.Marshal(keys)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(storedWire, []byte(bundle.PeerAPIKey)) || bytes.Contains(storedWire, []byte(bundle.RestartGuardrailToken)) {
		t.Fatal("Raft API-key state contains plaintext credentials")
	}
	// A repeated poll gets a fresh envelope but the same API key and creates
	// no duplicate record, which survives leader change without storing raw key
	// material in the FSM.
	again, _, err := srv.sealApprovedJoinCredentials(ctx, request, pending)
	if err != nil {
		t.Fatal(err)
	}
	openedAgain, err := joincredential.Open(again, certificatePEM, privateKeyPEM,
		joincredential.AssociatedData(pending.GetRequestId(), pending.GetNodeId(), fingerprint))
	if err != nil {
		t.Fatal(err)
	}
	var bundleAgain joincredential.Bundle
	if err := json.Unmarshal(openedAgain, &bundleAgain); err != nil {
		t.Fatal(err)
	}
	if bundleAgain.PeerAPIKey != bundle.PeerAPIKey {
		t.Fatal("repeated credential poll changed the peer API key")
	}
	keys, err = srv.raft.ListAPIKeys(context.Background())
	if err != nil || len(keys.GetKeys()) != 1 {
		t.Fatalf("repeated poll changed key table: keys=%d err=%v", len(keys.GetKeys()), err)
	}
	_, appErr, _ := srv.applyAPIKeyCommand(context.Background(), &internalpb.Command{
		Op: &internalpb.Command_RevokeApiKey{RevokeApiKey: &internalpb.RevokeAPIKey{Id: stored.GetId()}},
	}, 0)
	if appErr != "" {
		t.Fatalf("revoking test peer key: %s", appErr)
	}
	if _, _, err := srv.sealApprovedJoinCredentials(ctx, request, pending); err == nil || !strings.Contains(err.Error(), "missing or was revoked") {
		t.Fatalf("status poll after revocation error = %v, want no key recreation", err)
	}
}

func TestSealApprovedJoinCredentialsRequiresTLSAndMatchingCertificate(t *testing.T) {
	srv, certificatePEM, _, fingerprint := joinCredentialTestServer(t)
	pending := &internalpb.PendingJoinRequest{
		RequestId: "jreq-1123456789abcdef", NodeId: "comb-8",
		Status:             internalpb.JoinRequestStatus_JOIN_REQUEST_STATUS_APPROVED,
		TlsCertFingerprint: fingerprint,
	}
	request := &rpcpb.GetJoinRequestStatusRequest{RecipientTlsCertificatePem: string(certificatePEM)}
	if _, _, err := srv.sealApprovedJoinCredentials(context.Background(), request, pending); err == nil || !strings.Contains(err.Error(), "requires a TLS connection") {
		t.Fatalf("plaintext status poll error = %v, want TLS requirement", err)
	}
	tlsCtx := peer.NewContext(context.Background(), &peer.Peer{AuthInfo: credentials.TLSInfo{}})
	otherCert, _, err := hostcert.Generate(hostcert.GenerateOptions{CommonName: "wrong-joiner.example.test"})
	if err != nil {
		t.Fatal(err)
	}
	request.RecipientTlsCertificatePem = string(otherCert)
	if _, _, err := srv.sealApprovedJoinCredentials(tlsCtx, request, pending); err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("mismatched recipient certificate error = %v, want refusal", err)
	}
}

func TestSealApprovedJoinCredentialsRequiresProvisionedColonyToken(t *testing.T) {
	srv, certificatePEM, _, fingerprint := joinCredentialTestServer(t)
	srv.SetRestartGuardrailToken("")
	request := &rpcpb.GetJoinRequestStatusRequest{RecipientTlsCertificatePem: string(certificatePEM)}
	pending := &internalpb.PendingJoinRequest{RequestId: "jreq-2123456789abcdef", NodeId: "comb-9", TlsCertFingerprint: fingerprint}
	ctx := peer.NewContext(context.Background(), &peer.Peer{AuthInfo: credentials.TLSInfo{}})
	if _, _, err := srv.sealApprovedJoinCredentials(ctx, request, pending); err == nil || !strings.Contains(err.Error(), "has no restart-guardrail token") {
		t.Fatalf("unprovisioned leader error = %v, want fail-closed token message", err)
	}
}

func joinCredentialTestServer(t *testing.T) (*Server, []byte, []byte, string) {
	t.Helper()
	socket := newRaftdUDSSocket(t)
	_, srv := newManagerdRPCClientAndServer(t, socket, "leader-comb")
	srv.SetRestartGuardrailToken("colony-restart-token-test-value")
	certificatePEM, privateKeyPEM, err := hostcert.Generate(hostcert.GenerateOptions{CommonName: "joiner.example.test"})
	if err != nil {
		t.Fatal(err)
	}
	block, _ := pem.Decode(certificatePEM)
	if block == nil {
		t.Fatal("generated certificate has no PEM block")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	return srv, certificatePEM, privateKeyPEM, hostcert.Fingerprint(cert.Raw)
}
