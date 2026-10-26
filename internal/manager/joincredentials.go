package manager

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"strings"
	"time"

	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"

	internalpb "github.com/glenjbarber/apiary/api/internalpb"
	rpcpb "github.com/glenjbarber/apiary/api/rpc"
	"github.com/glenjbarber/apiary/internal/hostcert"
	"github.com/glenjbarber/apiary/internal/joincredential"
)

// sealApprovedJoinCredentials creates a stable per-Comb Operator key, stores
// only its digest in the Raft key table, and returns a public-key-encrypted
// bundle. The Colony's already-provisioned restart-guardrail token is required
// because a new Comb must receive the exact token the current leader uses;
// inventing a second token would leave it unable to confirm or forward leases.
func (s *Server) sealApprovedJoinCredentials(ctx context.Context, req *rpcpb.GetJoinRequestStatusRequest, pending *internalpb.PendingJoinRequest) ([]byte, string, error) {
	if s.restartGuardrailToken == "" {
		return nil, "", fmt.Errorf("cannot provision this joining Comb because the current Colony leader has no restart-guardrail token loaded; provision the Colony token on its current members and restart managerd before approving joins")
	}
	if len(req.GetRecipientTlsCertificatePem()) == 0 || len(req.GetRecipientTlsCertificatePem()) > 64*1024 {
		return nil, "", fmt.Errorf("recipient_tls_certificate_pem must be between 1 and 65536 bytes")
	}
	certBlock, _ := pem.Decode([]byte(req.GetRecipientTlsCertificatePem()))
	if certBlock == nil || certBlock.Type != "CERTIFICATE" {
		return nil, "", fmt.Errorf("recipient_tls_certificate_pem is missing or malformed")
	}
	cert, err := x509.ParseCertificate(certBlock.Bytes)
	if err != nil {
		return nil, "", fmt.Errorf("parsing recipient certificate: %w", err)
	}
	fingerprint := hostcert.Fingerprint(cert.Raw)
	wantFingerprint := pending.GetTlsCertFingerprint()
	if wantFingerprint == "" && len(pending.GetAdvertisedFingerprints()) > 0 {
		wantFingerprint = pending.GetAdvertisedFingerprints()[0]
	}
	if wantFingerprint == "" || fingerprint != wantFingerprint {
		return nil, "", fmt.Errorf("recipient certificate fingerprint does not match the certificate recorded for this join request")
	}
	if !statusPeerUsesTLS(ctx) {
		return nil, "", fmt.Errorf("credential delivery requires a TLS connection; rerun apiaryctl join-introduce without --plaintext")
	}

	key, err := joincredential.DerivePeerAPIKey(s.restartGuardrailToken, pending.GetRequestId(), pending.GetNodeId())
	if err != nil {
		return nil, "", err
	}
	leaderAddress, err := s.verifyJoinPeerAPIKeyRecorded(ctx, pending, key)
	if err != nil || leaderAddress != "" {
		return nil, leaderAddress, err
	}
	// ADR-0174: the certificate integration rides the SAME bundle,
	// and only for a Comb that was PREPARED FOR.
	//
	// A reservation is the request: a Comb with a prepared certificate
	// waiting for it is a Comb that will need to renew one. A Comb
	// with no reservation gets no credential, because provisioning a
	// zone-scoped Cloudflare token to every Comb in a Colony would
	// multiply the blast radius of a single compromise for no benefit.
	integration, err := s.integrationForPreparedJoin(ctx, pending)
	if err != nil {
		return nil, "", err
	}
	bundleVersion := 1
	if integration != nil {
		bundleVersion = 2
	}
	bundle := joincredential.Bundle{
		Version: bundleVersion, RequestID: pending.GetRequestId(), NodeID: pending.GetNodeId(),
		PeerAPIKey: key, RestartGuardrailToken: s.restartGuardrailToken,
		Integration: integration,
	}
	plain, err := json.Marshal(bundle)
	if err != nil {
		return nil, "", fmt.Errorf("encoding join credential bundle: %w", err)
	}
	envelope, err := joincredential.Seal([]byte(req.GetRecipientTlsCertificatePem()), plain,
		joincredential.AssociatedData(pending.GetRequestId(), pending.GetNodeId(), fingerprint))
	if err != nil {
		return nil, "", fmt.Errorf("sealing join credential bundle: %w", err)
	}
	return envelope, "", nil
}

// provisionApprovedJoinAPIKey records the per-Comb Operator key before
// AddVoter. If approval later fails, the hash is harmless and a retry reuses
// the same request-bound key; status polls never create or resurrect keys.
func (s *Server) provisionApprovedJoinAPIKey(ctx context.Context, pending *internalpb.PendingJoinRequest) (string, string, error) {
	if s.restartGuardrailToken == "" {
		return "", "", fmt.Errorf("the Colony leader has no restart-guardrail token loaded; provision the Colony token on its current members and restart managerd before approving this join")
	}
	key, err := joincredential.DerivePeerAPIKey(s.restartGuardrailToken, pending.GetRequestId(), pending.GetNodeId())
	if err != nil {
		return "", "", err
	}
	keyID := "join-" + strings.TrimPrefix(pending.GetRequestId(), "jreq-")
	keyName := "peer " + pending.GetNodeId()
	keyHash := hashAPIKey(key)
	keys, err := s.raft.ListAPIKeys(ctx)
	if err != nil {
		return "", "", fmt.Errorf("checking the joiner's peer key record: %w", err)
	}
	if keys.GetLeaderHint() != "" {
		return "", s.peerManagerdAddr(keys.GetLeaderHint()), nil
	}
	if keys.GetError() != "" {
		return "", "", fmt.Errorf("checking the joiner's peer key record: %s", keys.GetError())
	}
	for _, existing := range keys.GetKeys() {
		if existing.GetId() == keyID {
			if existing.GetHashedKey() != keyHash || existing.GetRole() != "operator" || existing.GetName() != keyName {
				return "", "", fmt.Errorf("the reserved peer key record for this join request does not match its derived credential")
			}
			return key, "", nil
		}
	}
	_, appErr, leaderHint := s.applyAPIKeyCommand(ctx, &internalpb.Command{
		Op: &internalpb.Command_CreateApiKey{CreateApiKey: &internalpb.CreateAPIKey{Key: &internalpb.ApiKey{
			Id: keyID, Name: keyName, HashedKey: keyHash, CreatedUnix: time.Now().Unix(), Role: "operator",
		}}},
	}, 0)
	if leaderHint != "" {
		return "", s.peerManagerdAddr(leaderHint), nil
	}
	if appErr == "" {
		return key, "", nil
	}
	// A concurrent approval retry can create the same deterministic ID.
	// Accept only the exact record expected for this request.
	keys, err = s.raft.ListAPIKeys(ctx)
	if err != nil || keys.GetError() != "" || keys.GetLeaderHint() != "" {
		return "", "", fmt.Errorf("recording the joiner's peer key: %s", appErr)
	}
	for _, existing := range keys.GetKeys() {
		if existing.GetId() == keyID && existing.GetHashedKey() == keyHash && existing.GetRole() == "operator" && existing.GetName() == keyName {
			return key, "", nil
		}
	}
	return "", "", fmt.Errorf("recording the joiner's peer key: %s", appErr)
}

// verifyJoinPeerAPIKeyRecorded is read-only. In particular, a status poll
// cannot recreate an API key after an operator revokes it.
func (s *Server) verifyJoinPeerAPIKeyRecorded(ctx context.Context, pending *internalpb.PendingJoinRequest, rawKey string) (string, error) {
	keyID := "join-" + strings.TrimPrefix(pending.GetRequestId(), "jreq-")
	keyName := "peer " + pending.GetNodeId()
	keyHash := hashAPIKey(rawKey)
	keys, err := s.raft.ListAPIKeys(ctx)
	if err != nil {
		return "", fmt.Errorf("checking the joiner's peer key record: %w", err)
	}
	if keys.GetLeaderHint() != "" {
		return s.peerManagerdAddr(keys.GetLeaderHint()), nil
	}
	if keys.GetError() != "" {
		return "", fmt.Errorf("checking the joiner's peer key record: %s", keys.GetError())
	}
	for _, existing := range keys.GetKeys() {
		if existing.GetId() != keyID {
			continue
		}
		if existing.GetHashedKey() != keyHash || existing.GetRole() != "operator" || existing.GetName() != keyName {
			return "", fmt.Errorf("the reserved peer key record for this join request does not match its derived credential; refusing to send credentials")
		}
		return "", nil
	}
	return "", fmt.Errorf("the joiner's peer API key record is missing or was revoked; credentials were not recreated")
}

func statusPeerUsesTLS(ctx context.Context) bool {
	remote, ok := peer.FromContext(ctx)
	if !ok || remote.AuthInfo == nil {
		return false
	}
	switch remote.AuthInfo.(type) {
	case credentials.TLSInfo, *credentials.TLSInfo:
		return true
	default:
		return false
	}
}
