package manager

import (
	"fmt"
	"os"
	"strings"
	"time"

	internalpb "github.com/glenjbarber/apiary/api/internalpb"
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
func (s *Server) colonyCAForJoin(pending *internalpb.PendingJoinRequest) (*joincredential.ColonyCA, error) {
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
	return &joincredential.ColonyCA{
		CertificatePEM:    string(certPEM),
		Fingerprint:       fingerprint,
		PinnedFingerprint: pinned,
	}, nil
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
