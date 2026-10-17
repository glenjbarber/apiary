package manager

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"time"

	rpcpb "github.com/glenjbarber/apiary/api/rpc"
	"github.com/glenjbarber/apiary/internal/hostcert"
	"github.com/glenjbarber/apiary/internal/joincredential"
)

type memberRestartCredential struct {
	Version   int    `json:"version"`
	NodeID    string `json:"node_id"`
	Challenge string `json:"challenge"`
	Token     string `json:"token"`
}

// GetMemberRestartGuardrailCredential returns the restart token only as an
// authenticated envelope for a certificate pinned to the named current
// voter. The plaintext is never returned by an RPC or written to Raft.
func (s *Server) GetMemberRestartGuardrailCredential(ctx context.Context, req *rpcpb.GetMemberRestartGuardrailCredentialRequest) (*rpcpb.GetMemberRestartGuardrailCredentialResponse, error) {
	out := &rpcpb.GetMemberRestartGuardrailCredentialResponse{}
	if s.restartGuardrailToken == "" {
		out.Error = "the Colony leader has no restart-guardrail token loaded"
		return out, nil
	}
	if req.GetNodeId() == "" || len(req.GetChallenge()) < 16 || len(req.GetChallenge()) > 256 || len(req.GetRecipientTlsCertificatePem()) == 0 || len(req.GetRecipientTlsCertificatePem()) > 64*1024 {
		out.Error = "node_id, a 16-to-256-byte challenge and a valid recipient certificate are required"
		return out, nil
	}
	status, err := s.raft.Status(ctx)
	if err != nil {
		out.Error = fmt.Sprintf("reading current Raft membership: %v", err)
		return out, nil
	}
	if status.GetMembershipError() != "" {
		out.Error = "current Raft membership is unavailable: " + status.GetMembershipError()
		return out, nil
	}
	if !status.GetIsLeader() {
		for _, member := range status.GetServers() {
			if member.GetId() == status.GetLeaderId() {
				out.LeaderHint = s.peerManagerdAddr(member.GetAddress())
				break
			}
		}
		if out.LeaderHint == "" {
			out.LeaderHint = status.GetLeaderId()
		}
		return out, nil
	}
	isVoter := false
	for _, member := range status.GetServers() {
		if member.GetId() == req.GetNodeId() && member.GetSuffrage() == "Voter" {
			isVoter = true
			break
		}
	}
	if !isVoter {
		out.Error = "the requested node is not a current Raft voter"
		return out, nil
	}
	block, _ := pem.Decode([]byte(req.GetRecipientTlsCertificatePem()))
	if block == nil || block.Type != "CERTIFICATE" {
		out.Error = "recipient certificate is malformed"
		return out, nil
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		out.Error = "recipient certificate is malformed"
		return out, nil
	}
	fingerprint := hostcert.Fingerprint(cert.Raw)
	pins, err := s.raft.ListTrustedPeersLocal(ctx)
	if err != nil {
		out.Error = fmt.Sprintf("reading pinned member certificates: %v", err)
		return out, nil
	}
	matched := false
	for _, pin := range pins.GetPeers() {
		if pin.GetNodeId() == req.GetNodeId() && pin.GetFingerprint() == fingerprint {
			if pin.GetNotAfterUnix() <= time.Now().Unix() {
				out.Error = "the pinned recipient certificate has expired"
				return out, nil
			}
			matched = true
			break
		}
	}
	if !matched {
		out.Error = "recipient certificate is not the certificate pinned for this Raft member"
		return out, nil
	}
	plain, err := json.Marshal(memberRestartCredential{Version: 1, NodeID: req.GetNodeId(), Challenge: req.GetChallenge(), Token: s.restartGuardrailToken})
	if err != nil {
		out.Error = "encoding credential envelope payload failed"
		return out, nil
	}
	out.SealedCredential, err = joincredential.Seal([]byte(req.GetRecipientTlsCertificatePem()), plain, joincredential.MemberTokenAssociatedData(req.GetNodeId(), fingerprint, req.GetChallenge()))
	if err != nil {
		out.Error = fmt.Sprintf("sealing member credential: %v", err)
	}
	return out, nil
}
