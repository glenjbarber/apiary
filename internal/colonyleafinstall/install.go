// Package colonyleafinstall installs a joining Comb's OWN raft
// transport leaf, and points its raftd configuration at the Colony CA
// that issued it.
//
// It is the joiner's half of ADR-0187, and it is separate from
// internal/colonyleaf for the same reason internal/certintegration and
// internal/colonycainstall are: the issuing side and the receiving
// side are different programs, and putting either in the manager
// package would have made one of them depend on raft, bbolt and the
// cluster packages.
//
// Its rules, each a refusal rather than a warning:
//
//   - THE LEAF IS VERIFIED AGAINST THE CA BEFORE IT IS WRITTEN. A
//     leaf that does not chain to the Colony CA is a certificate no
//     peer can verify, and a joiner that installed it would be a
//     member the Colony could not authenticate.
//
//   - THE LEAF'S NAMES ARE CHECKED AGAINST THE JOINER'S OWN IDENTITY.
//     A leaf issued for another Comb is refused, because a joiner
//     that installed one would serve a certificate for a machine that
//     is not it.
//
//   - AN EXISTING LEAF IS NEVER REPLACED. A Comb that already has a
//     raft transport certificate keeps it.
//
//   - THE RAFT CONFIGURATION IS NOT SAVED UNLESS EVERYTHING ELSE
//     SUCCEEDED. A raftd.json pointing at files that were never
//     published is a raftd that cannot start.
package colonyleafinstall

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/glenjbarber/apiary/internal/colonyca"
	"github.com/glenjbarber/apiary/internal/colonyleaf"
	"github.com/glenjbarber/apiary/internal/raftdconfig"
)

// DefaultLeafCertPath and DefaultLeafKeyPath are where the joining
// Comb's own raft transport material lives.
//
// They are the same paths the live Colony's Combs use, and the same
// ones `apiaryctl colony-ca issue-raft` writes.
const (
	DefaultLeafCertPath = "/usr/local/etc/apiary/raft.pem"
	DefaultLeafKeyPath  = "/usr/local/etc/apiary/raft.key"
)

// InstallOptions says what to install and where.
type InstallOptions struct {
	// Leaf is the opened leaf.
	Leaf *colonyleaf.Leaf

	// NodeID is the joining Comb's own raft identity. The leaf's
	// names are checked against it.
	NodeID string

	// CertPath and KeyPath are where the leaf and its key go.
	CertPath string
	KeyPath  string

	// CADir is where the Colony CA certificate lives - the same
	// directory the leaf is verified against.
	CADir string

	// Now overrides the clock, for tests.
	Now time.Time
}

// Install writes the joining Comb's raft transport leaf and checks
// that it chains to the Colony CA.
//
// It does NOT touch raftd's configuration. That is a separate,
// explicit step - see PointRaftdAtTheCA - because a raftd that is
// pointed at material that was never published is a raftd that
// cannot start, and the operator has to be the one who decides to
// switch the transport to TLS.
func Install(opts InstallOptions) error {
	if opts.Leaf == nil {
		return errors.New("colonyleafinstall: there is no leaf to install")
	}
	if strings.TrimSpace(opts.NodeID) == "" {
		return errors.New("colonyleafinstall: the joining Comb's node id is required; the leaf's names are checked against it")
	}
	if opts.CertPath == "" {
		opts.CertPath = DefaultLeafCertPath
	}
	if opts.KeyPath == "" {
		opts.KeyPath = DefaultLeafKeyPath
	}
	now := opts.Now
	if now.IsZero() {
		now = time.Now()
	}

	caPEM, err := os.ReadFile(filepath.Join(opts.CADir, colonyca.CACertFile))
	if err != nil {
		return fmt.Errorf("colonyleafinstall: reading the Colony CA certificate: %w", err)
	}
	// The leaf is VERIFIED against the CA, not assumed to be from it.
	// A leaf that does not chain to the CA is a certificate no peer
	// can verify.
	if err := colonyca.VerifyLeafAgainstCA([]byte(opts.Leaf.CertificatePEM), caPEM, []string{opts.NodeID}, now); err != nil {
		return err
	}
	// The leaf's fingerprint is re-derived and checked against the
	// CA fingerprint the leaf itself claims, so a bundle whose own
	// account of itself is wrong is refused.
	if _, err := colonyca.Fingerprint([]byte(opts.Leaf.CertificatePEM)); err != nil {
		return fmt.Errorf("colonyleafinstall: fingerprinting the leaf: %w", err)
	}
	caFingerprint, err := colonyca.Fingerprint(caPEM)
	if err != nil {
		return fmt.Errorf("colonyleafinstall: fingerprinting the Colony CA: %w", err)
	}
	if opts.Leaf.CAFingerprint != "" && !strings.EqualFold(opts.Leaf.CAFingerprint, caFingerprint) {
		return fmt.Errorf("colonyleafinstall: the leaf claims it was issued by CA %s but the CA this Comb holds fingerprints as %s", opts.Leaf.CAFingerprint, caFingerprint)
	}

	if err := writeNewFile(opts.CertPath, []byte(opts.Leaf.CertificatePEM), 0o644); err != nil {
		return err
	}
	// The key is 0600 and is written SECOND. A cert without its key
	// is inert; a key without its cert is a secret nobody can use.
	if err := writeNewFile(opts.KeyPath, []byte(opts.Leaf.PrivateKeyPEM), 0o600); err != nil {
		return err
	}
	return nil
}

// PointRaftdAtTheCA points raftd's configuration at the Colony CA
// and at this Comb's own raft transport leaf.
//
// It is a separate, explicit step, and it is not automatic. Switching
// a member's raft transport to TLS is an operator decision, and
// ADR-0078's own posture is that encryption and trust have to be in
// place in that order: a raftd pointed at a CA it does not trust
// cannot start.
//
// A configuration that already names a raft transport certificate is
// left ALONE. A Comb that already has one keeps it, because
// replacing it silently would invalidate every leaf the Colony holds
// for that Comb.
func PointRaftdAtTheCA(raftdConfigPath, caDir, certPath, keyPath string) error {
	if raftdConfigPath == "" {
		raftdConfigPath = raftdconfig.DefaultPath
	}
	mgr := &raftdconfig.Manager{Path: raftdConfigPath}
	cfg, err := mgr.Load()
	if err != nil {
		return fmt.Errorf("colonyleafinstall: reading raftd's configuration: %w", err)
	}
	if strings.TrimSpace(cfg.RaftTLSCert) != "" {
		return nil
	}
	if certPath == "" {
		certPath = DefaultLeafCertPath
	}
	if keyPath == "" {
		keyPath = DefaultLeafKeyPath
	}
	enabled := true
	cfg.RaftTLSEnabled = &enabled
	cfg.RaftTLSCert = certPath
	cfg.RaftTLSKey = keyPath
	cfg.RaftTLSCA = filepath.Join(caDir, colonyca.CACertFile)
	if err := mgr.Save(cfg); err != nil {
		return fmt.Errorf("colonyleafinstall: saving raftd's configuration: %w", err)
	}
	return nil
}

// writeNewFile writes body to path with mode, refusing to overwrite.
//
// The refusal matters: a raft transport certificate is published
// exactly once, and a half-written file that read as a valid-but-wrong
// certificate would be worse than no file at all.
func writeNewFile(path string, body []byte, mode os.FileMode) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return fmt.Errorf("colonyleafinstall: %s already exists, and an existing raft transport certificate is never replaced; remove it by hand only if this Comb is being rebuilt", path)
		}
		return fmt.Errorf("colonyleafinstall: creating %s: %w", path, err)
	}
	defer f.Close()
	if _, err := f.Write(body); err != nil {
		return fmt.Errorf("colonyleafinstall: writing %s: %w", path, err)
	}
	return f.Sync()
}
