package manager

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"net"
	"strings"
	"testing"
	"time"

	internalpb "github.com/glenjbarber/apiary/api/internalpb"
	rpcpb "github.com/glenjbarber/apiary/api/rpc"
	"github.com/glenjbarber/apiary/internal/hostcert"
	"github.com/glenjbarber/apiary/internal/joincredential"
)

func TestGetMemberRestartGuardrailCredentialIsPinnedAndEncrypted(t *testing.T) {
	socket := newRaftdUDSSocket(t)
	_, srv := newManagerdRPCClientAndServer(t, socket, "leader-comb")
	srv.SetRestartGuardrailToken("member-restart-token-test")
	certificatePEM, privateKeyPEM, err := hostcert.Generate(hostcert.GenerateOptions{CommonName: "member.example.test", DNSNames: []string{"member.example.test"}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}})
	if err != nil {
		t.Fatal(err)
	}
	block, _ := pem.Decode(certificatePEM)
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	fingerprint := hostcert.Fingerprint(cert.Raw)
	status, err := srv.raft.Status(context.Background())
	if err != nil || len(status.GetServers()) != 1 {
		t.Fatalf("reading test Raft membership: status=%+v err=%v", status, err)
	}
	nodeID := status.GetServers()[0].GetId()
	_, appErr, leaderHint := srv.applyTrustedPeerCommand(context.Background(), &internalpb.Command{
		Op: &internalpb.Command_PinTrustedPeer{PinTrustedPeer: &internalpb.PinTrustedPeer{Peer: &internalpb.TrustedPeer{
			NodeId: nodeID, CombName: "member", Fingerprint: fingerprint, CertPem: string(certificatePEM), NotAfterUnix: cert.NotAfter.Unix(), PinnedAtUnix: time.Now().Unix(), PinnedBy: nodeID,
		}}},
	}, 0)
	if appErr != "" || leaderHint != "" {
		t.Fatalf("pinning test member = (%q, %q)", appErr, leaderHint)
	}
	challenge := "0123456789abcdef0123456789abcdef"
	request := &rpcpb.GetMemberRestartGuardrailCredentialRequest{NodeId: nodeID, RecipientTlsCertificatePem: string(certificatePEM), Challenge: challenge}
	response, err := srv.GetMemberRestartGuardrailCredential(context.Background(), request)
	if err != nil || response.GetError() != "" {
		t.Fatalf("GetMemberRestartGuardrailCredential() = (%+v, %v)", response, err)
	}
	if bytes.Contains(response.GetSealedCredential(), []byte(srv.restartGuardrailToken)) {
		t.Fatal("wire response contains the plaintext restart token")
	}
	plain, err := joincredential.Open(response.GetSealedCredential(), certificatePEM, privateKeyPEM,
		joincredential.MemberTokenAssociatedData(nodeID, fingerprint, challenge))
	if err != nil {
		t.Fatal(err)
	}
	var bundle memberRestartCredential
	if err := json.Unmarshal(plain, &bundle); err != nil {
		t.Fatal(err)
	}
	if bundle.Version != 1 || bundle.NodeID != nodeID || bundle.Challenge != challenge || bundle.Token != srv.restartGuardrailToken {
		t.Fatalf("decrypted credential = %+v, not the requested member token", bundle)
	}

	otherCert, _, err := hostcert.Generate(hostcert.GenerateOptions{CommonName: "other.example.test"})
	if err != nil {
		t.Fatal(err)
	}
	request.RecipientTlsCertificatePem = string(otherCert)
	refused, err := srv.GetMemberRestartGuardrailCredential(context.Background(), request)
	if err != nil || !strings.Contains(refused.GetError(), "not the certificate pinned") {
		t.Fatalf("un-pinned certificate response = (%+v, %v), want refusal", refused, err)
	}
	request.RecipientTlsCertificatePem = string(certificatePEM)
	request.NodeId = "not-a-voter"
	refused, err = srv.GetMemberRestartGuardrailCredential(context.Background(), request)
	if err != nil || !strings.Contains(refused.GetError(), "not a current Raft voter") {
		t.Fatalf("non-voter response = (%+v, %v), want refusal", refused, err)
	}
}
