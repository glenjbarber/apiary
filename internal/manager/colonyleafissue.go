package manager

import (
	"context"
	"path/filepath"
	"strings"
	"time"

	rpcpb "github.com/glenjbarber/apiary/api/rpc"
	"github.com/glenjbarber/apiary/internal/colonyleaf"
)

// IssueColonyRaftLeaf implements rpcpb.ManagerServiceServer - it asks
// THIS Comb to issue a raft transport leaf for a joining Comb, from
// the Colony CA whose key this Comb holds.
//
// It is the CA holder's half of ADR-0187.
//
// WHY IT IS A SEPARATE CALL, AND WHY IT IS NOT UNAUTHENTICATED:
//
// The Colony CA key does not move. Only the Comb that ran
// `apiaryctl colony-ca init` holds it, and it is not necessarily the
// Colony's leader - on a live Colony the leader and the CA holder are
// different Combs. So the leaf cannot be issued by whoever happens to
// be building the join credential bundle.
//
// The call is made by a MEMBER of the Colony forwarding to another
// member, over the ordinary peer channel. That is the difference from
// the join RPCs, which are deliberately unauthenticated and
// fail-closed: a call that could make a Comb sign a certificate has no
// business being answerable by whoever dialled it.
//
// The CA is RE-DERIVED here and compared with the fingerprint the
// caller expected, so a forwarded request cannot ask this Comb to
// issue a leaf from a different CA than the Colony pinned.
func (s *Server) IssueColonyRaftLeaf(ctx context.Context, req *rpcpb.IssueColonyRaftLeafRequest) (*rpcpb.IssueColonyRaftLeafResponse, error) {
	caDir, err := s.colonyCADir()
	if err != nil {
		return &rpcpb.IssueColonyRaftLeafResponse{Error: err.Error()}, nil
	}
	if caDir == "" {
		return &rpcpb.IssueColonyRaftLeafResponse{Error: "this Comb holds no Colony CA, so it cannot issue a raft transport leaf; the leaf must be issued by the Comb that holds the Colony CA key"}, nil
	}
	sealed, fingerprint, err := colonyleaf.Issue(colonyleaf.IssueOptions{
		CADir:                   caDir,
		NodeID:                  req.GetNodeId(),
		RaftBindAddress:         req.GetRaftBindAddress(),
		RequestID:               req.GetRequestId(),
		ExpectedCAFingerprint:   req.GetExpectedCaFingerprint(),
		RecipientCertificatePEM: req.GetRecipientTlsCertificatePem(),
		Now:                     time.Now().UTC(),
	})
	if err != nil {
		return &rpcpb.IssueColonyRaftLeafResponse{Error: err.Error()}, nil
	}
	return &rpcpb.IssueColonyRaftLeafResponse{SealedLeaf: string(sealed), CaFingerprint: fingerprint}, nil
}

// colonyCADir returns the directory holding THIS Comb's Colony CA
// certificate and key, or "" when this Comb holds no CA.
//
// The path comes from raftd's OWN configuration - its raft_tls_ca
// field - because that is the file raftd itself trusts, and ADR-0102's
// posture is that each co-located daemon owns exactly one
// configuration file and the others read it rather than keeping a
// second copy of the setting.
//
// The directory is the CA's, not the leaf's: raft_tls_ca names the CA
// certificate, and the CA key sits beside it.
func (s *Server) colonyCADir() (string, error) {
	if s.raftdConfig == nil {
		return "", nil
	}
	cfg, err := s.raftdConfig.Load()
	if err != nil {
		return "", nil
	}
	caPath := strings.TrimSpace(cfg.RaftTLSCA)
	if caPath == "" {
		return "", nil
	}
	return filepath.Dir(caPath), nil
}
