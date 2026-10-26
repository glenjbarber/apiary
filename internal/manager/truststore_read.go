package manager

// ADR-0177's read side: the peer trust store, as the managerd sees
// it.
//
// WHY A MANAGER RPC. The frontend talks to managerd, not to raft. The
// trust store lives in raft, so something has to carry it across, and
// that something is a manager RPC.
//
// WHY IT IS COLONY-WIDE. The store is raft-replicated, so every
// Comb's read of it is the SAME read. That is the opposite of
// ListOriginCertificates and ListLetsEncryptCertificates, which
// answer only for the managerd that receives them and have no
// peer-forwarding path. It is also why this is not on the Certificates
// page: that page has deliberately declared itself a single Comb's
// read-only view.
//
// It carries no private key and no key path, because the store holds
// none. A pin is a certificate and a fingerprint; the key that goes
// with it lives on the Comb that serves it and never travels here.

import (
	"context"

	internalpb "github.com/glenjbarber/apiary/api/internalpb"
	rpcpb "github.com/glenjbarber/apiary/api/rpc"
)

// ListTrustedPeers implements rpcpb.ManagerServiceServer - Viewer-tier.
//
// It is a READ of the trust store, so it is Viewer. Deciding what the
// Colony TRUSTS is Admin - that is what the three transition RPCs are
// for - but seeing what it trusts is not a privileged act, and an
// operator who cannot see the trust store cannot reason about a
// transition they are being asked to authorize.
func (s *Server) ListTrustedPeers(ctx context.Context, _ *rpcpb.ListTrustedPeersRequest) (*rpcpb.ListTrustedPeersResponse, error) {
	resp, err := s.raft.ListTrustedPeersLocal(ctx)
	if err != nil {
		return &rpcpb.ListTrustedPeersResponse{Error: err.Error()}, nil
	}
	out := &rpcpb.ListTrustedPeersResponse{}
	for _, peer := range resp.GetPeers() {
		out.Peers = append(out.Peers, toRPCTustedPeer(peer))
	}
	return out, nil
}

// the internal type is referenced so the conversion stays checked at
// compile time rather than by a comment.
var _ = internalpb.TrustedPeer{}
