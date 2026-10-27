package manager

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	internalpb "github.com/glenjbarber/apiary/api/internalpb"
	rpcpb "github.com/glenjbarber/apiary/api/rpc"
	"github.com/glenjbarber/apiary/internal/colonyca"
	"github.com/glenjbarber/apiary/internal/joinauth"
	"github.com/glenjbarber/apiary/internal/joincredential"
)

// colonyCAForJoin reads this Colony's certificate authority and
// returns it for the join credential bundle.
//
// It is the Colony's half of ADR-0155 step 3.
//
// WHEN IT IS ABSENT, the bundle carries no CA. That is the normal
// case for a Colony whose members do not use raft transport TLS, and
// a joiner in that Colony has nothing to trust and nothing to check.
//
// WHEN IT IS PRESENT, the certificate and its fingerprint travel
// together. The joiner does not trust the certificate because it was
// handed one: it trusts it because its fingerprint matches the one
// the operator pinned in the join authorization. The fingerprint is
// the authority; the certificate is only what the fingerprint is
// checked against.
//
// THE FINGERPRINT IS NOT COMPARED IN THE HOSTCERT FORMAT. It is
// colonyca's own - the lowercase hex SHA-256 of the DER bytes - and
// it is computed by colonyca.Fingerprint, so there is exactly one
// place in the codebase that produces this shape and one place that
// checks it.
//
// A CA that cannot be read, or that does not parse, is a REFUSAL
// rather than a bundle without a CA. A Colony that has raft transport
// TLS configured and cannot produce its own CA cannot issue a
// certificate for a joining Comb, and a join that silently produced a
// Comb with no CA would fail later and further from its cause.
func (s *Server) colonyCAForJoin(pending *internalpb.PendingJoinRequest, recipientCertificatePEM string) (*joincredential.ColonyCA, error) {
	caPath, err := s.colonyCAPath()
	if err != nil {
		return nil, err
	}
	if caPath == "" {
		return nil, nil
	}
	certPEM, err := os.ReadFile(caPath)
	if err != nil {
		if os.IsNotExist(err) {
			// No CA file, so no CA. A Colony without one is not a
			// failure - the raft transport may not use TLS at all.
			return nil, nil
		}
		return nil, fmt.Errorf("reading the Colony CA certificate at %s: %w", caPath, err)
	}
	fingerprint, err := colonyca.Fingerprint(certPEM)
	if err != nil {
		return nil, fmt.Errorf("the Colony CA certificate at %s is not usable: %w", caPath, err)
	}
	// The PINNED fingerprint comes from the join authorization - the
	// root-owned file the operator wrote, and the value they
	// compared by hand.
	//
	// It is looked up here, at the moment the bundle is built, rather
	// than being carried in replicated state: the authorization file
	// is per-Comb and physical, exactly as the authorization itself
	// is, and copying it into raft would make a per-Comb operator
	// decision into replicated Colony state.
	//
	// AN ENTRY THAT PINS NO CA FINGERPRINT GETS NO CA. A join that
	// does not say which CA it expects is a join that cannot check
	// the one it is given, and shipping it anyway would be a CA the
	// joiner has to take on trust - which is the thing ADR-0155
	// exists to prevent.
	pinned, err := s.pinnedColonyCAFingerprint(pending)
	if err != nil {
		return nil, err
	}
	if pinned == "" {
		return nil, nil
	}
	ca := &joincredential.ColonyCA{
		CertificatePEM:    string(certPEM),
		Fingerprint:       fingerprint,
		PinnedFingerprint: pinned,
	}
	// ADR-0187: the leaf is issued by the member that HOLDS the CA
	// key, and the leader relays it.
	//
	// The leaf cannot be issued here. The CA key does not move - only
	// the Comb that ran `apiaryctl colony-ca init` holds it, and on
	// a live Colony that is not the leader. So the leader asks the
	// holder, and the holder seals the leaf to the joining Comb.
	//
	// A LEAF IS NOT ALWAYS WANTED. A join whose authorization names
	// no CA holder gets the CA and no leaf, which is the right answer
	// for a Colony whose members do not use raft transport TLS.
	sealedLeaf, err := s.sealedLeafForJoin(pending, ca, recipientCertificatePEM)
	if err != nil {
		return nil, err
	}
	ca.SealedLeaf = sealedLeaf
	return ca, nil
}

// sealedLeafForJoin asks the member holding the Colony CA key to
// issue a raft transport leaf for the joining Comb, and returns it
// sealed to that Comb.
//
// The CA HOLDER is named in the join authorization, because the
// operator is the only party that knows where the Colony's CA key
// lives - the key does not move, and the leader is not necessarily
// the Comb that holds it.
//
// A refusal here is a refusal, not a bundle without a leaf. A join
// whose authorization named a CA holder and did not get a leaf is a
// join that would produce a member the Colony cannot verify, and
// letting it through would hide that until a handshake failed.
func (s *Server) sealedLeafForJoin(pending *internalpb.PendingJoinRequest, ca *joincredential.ColonyCA, recipientCertificatePEM string) (string, error) {
	holder, err := s.colonyCAHolderNodeID(pending)
	if err != nil {
		return "", err
	}
	if holder == "" {
		return "", nil
	}
	if s.peers == nil {
		return "", fmt.Errorf("the Colony's CA key is held by %q, and this Comb cannot reach it because peer forwarding is not configured", holder)
	}
	addr, err := s.peerManagerdAddrOfNodeID(holder)
	if err != nil {
		return "", err
	}
	resp, err := s.peers.IssueColonyRaftLeaf(context.Background(), addr, &rpcpb.IssueColonyRaftLeafRequest{
		NodeId:                     pending.GetNodeId(),
		RaftBindAddress:            pending.GetRaftBindAddress(),
		RecipientTlsCertificatePem: recipientCertificatePEM,
		RequestId:                  pending.GetRequestId(),
		ExpectedCaFingerprint:      ca.PinnedFingerprint,
	})
	if err != nil {
		return "", fmt.Errorf("asking %q, which holds the Colony CA key, to issue a raft transport leaf: %w", holder, err)
	}
	if resp.GetError() != "" {
		return "", fmt.Errorf("asking %q, which holds the Colony CA key, to issue a raft transport leaf: %s", holder, resp.GetError())
	}
	if resp.GetSealedLeaf() == "" {
		return "", fmt.Errorf("the Colony CA holder %q returned no leaf", holder)
	}
	return resp.GetSealedLeaf(), nil
}

// colonyCAHolderNodeID returns the raft identity of the member that
// holds the Colony CA key, as named in the join authorization, or ""
// when the authorization names none.
//
// A refusal to read the authorization store is NOT an error here: the
// approval path reads the same store and reports it properly. A store
// that cannot be read at this point means there is no named holder,
// and the join simply gets the CA and no leaf.
func (s *Server) colonyCAHolderNodeID(pending *internalpb.PendingJoinRequest) (string, error) {
	load := s.joinAuthorizationRead
	if load == nil {
		load = func() (joinauth.Store, error) { return joinauth.Load(s.joinAuthorizationFile()) }
	}
	store, err := load()
	if err != nil {
		return "", nil
	}
	entry, refusal := store.Find(pending.GetNodeId(), pending.GetTlsCertFingerprint(), time.Now())
	if refusal != nil {
		return "", nil
	}
	return strings.TrimSpace(entry.ColonyCAHolderNodeID), nil
}

// pinnedColonyCAFingerprint returns the Colony CA fingerprint the join
// authorization for this request pins, or "" when it pins none.
//
// A refusal to read the authorization store is NOT an error here: the
// approval path reads the same store and reports it properly, and
// this function runs while the bundle is being built, which is after
// the approval succeeded. A store that cannot be read at this point
// means there is no pinned CA to carry, and the bundle simply omits
// the CA.
func (s *Server) pinnedColonyCAFingerprint(pending *internalpb.PendingJoinRequest) (string, error) {
	load := s.joinAuthorizationRead
	if load == nil {
		load = func() (joinauth.Store, error) { return joinauth.Load(s.joinAuthorizationFile()) }
	}
	store, err := load()
	if err != nil {
		return "", nil
	}
	entry, refusal := store.Find(pending.GetNodeId(), pending.GetTlsCertFingerprint(), time.Now())
	if refusal != nil {
		return "", nil
	}
	return joinauth.NormalizeCAFingerprint(entry.ColonyCAFingerprint), nil
}

// colonyCAPath returns the path of this Comb's Colony CA
// certificate, or "" when this Comb has no raft transport TLS
// configured.
//
// The path comes from raftd's OWN configuration - its raft_tls_ca
// field - rather than from a guess. raftd is the daemon that uses
// that CA, and ADR-0102's posture is that each co-located daemon
// owns exactly one configuration file and the others read it rather
// than keeping a second copy of the setting.
//
// An empty raft_tls_ca means the raft transport is not using TLS, so
// there is no CA to distribute.
func (s *Server) colonyCAPath() (string, error) {
	if s.raftdConfig == nil {
		return "", nil
	}
	cfg, err := s.raftdConfig.Load()
	if err != nil {
		// A Comb with no raftd configuration has no CA to
		// distribute. That is not a failure.
		return "", nil
	}
	path := strings.TrimSpace(cfg.RaftTLSCA)
	if path == "" {
		return "", nil
	}
	return path, nil
}

// peerManagerdAddrOfNodeID returns the managerd address of the member
// with the given raft identity, resolved from CLUSTER STATE.
//
// It is the address lookup that makes the CA holder reachable, and
// the source matters: the address comes from raft's own membership,
// never from a caller-supplied one. The join poll is unauthenticated,
// so an address that came from the request would be an address anyone
// chose - and this call makes a Comb sign a certificate.
//
// A member that is not in the membership is a refusal naming it,
// because a node id that names no member is a node id the caller got
// wrong, and dialling something else with it would be worse than
// refusing.
func (s *Server) peerManagerdAddrOfNodeID(nodeID string) (string, error) {
	if strings.TrimSpace(nodeID) == "" {
		return "", fmt.Errorf("the CA holder's node id is empty")
	}
	status, err := s.raft.Status(context.Background())
	if err != nil {
		return "", fmt.Errorf("reading the Colony's membership to find %q, which holds the CA key: %w", nodeID, err)
	}
	for _, server := range status.GetServers() {
		if server.GetId() == nodeID {
			return peerManagerdAddrOf(server.GetAddress(), s.peerManagerdPort), nil
		}
	}
	return "", fmt.Errorf("no member of this Colony has the node id %q, so the member holding the CA key cannot be found; the join authorization names a Comb that is not a member", nodeID)
}
