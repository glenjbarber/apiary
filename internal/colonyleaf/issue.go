// Package colonyleaf issues a Colony's raft transport leaf for a
// joining Comb, and seals it to that Comb.
//
// It is the CA holder's half of ADR-0187, and it is its own package
// for the same reason internal/certintegration and
// internal/colonycainstall are: BOTH sides of a join need it, and
// putting the issuance in the manager package would have made the
// joiner depend on the manager, which drags raft, bbolt and the
// cluster packages into a binary an operator runs during an incident.
//
// The rules are the security properties:
//
//   - THE CA KEY NEVER LEAVES THIS COMB. What leaves it is a LEAF
//     and its key, sealed to the joining Comb's own certificate. The
//     holder can issue for the Colony; it cannot become the Colony's
//     root of trust for anyone else.
//
//   - THE CA IS RE-DERIVED, NOT BELIEVED. The holder checks its own
//     CA's fingerprint against the one the caller expected. A
//     forwarded request that asked for a leaf from a DIFFERENT CA is
//     refused.
//
//   - THE LEAF'S NAMES ARE THE JOINER'S, NOT THE CALLER'S. The leaf
//     covers the name and address the Colony will dial, so a leaf
//     issued for one Comb cannot be used to impersonate another.
package colonyleaf

import (
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/glenjbarber/apiary/internal/colonyca"
	"github.com/glenjbarber/apiary/internal/hostcert"
	"github.com/glenjbarber/apiary/internal/joincredential"
)

// Leaf is the plaintext payload sealed to the joining Comb.
type Leaf struct {
	// CertificatePEM is the issued leaf certificate.
	CertificatePEM string `json:"certificate_pem"`

	// PrivateKeyPEM is the leaf's private key.
	PrivateKeyPEM string `json:"private_key_pem"`

	// CAFingerprint is the fingerprint of the CA that signed it, in
	// colonyca's own format.
	CAFingerprint string `json:"ca_fingerprint"`
}

// IssueOptions says what leaf to issue and for whom.
type IssueOptions struct {
	// CADir holds the CA certificate and the CA key. Both must be
	// here: this Comb is the CA holder, and only the CA holder can
	// issue a leaf.
	CADir string

	// NodeID is the joining Comb's raft identity, and the leaf's DNS
	// name.
	NodeID string

	// RaftBindAddress is the address the Colony will dial the joining
	// Comb on. It contributes an IP SAN.
	RaftBindAddress string

	// RequestID is the join request this leaf is for.
	RequestID string

	// ExpectedCAFingerprint is the CA fingerprint the caller expects
	// this Comb's CA to have, in colonyca's own format.
	ExpectedCAFingerprint string

	// RecipientCertificatePEM is the joining Comb's own serving
	// certificate. The leaf and its key are sealed to it.
	RecipientCertificatePEM string

	// Now overrides the clock, for tests.
	Now time.Time
}

// Issue issues a raft transport leaf for a joining Comb and returns it
// sealed to that Comb's own certificate.
//
// The CA is read from CADir, and its fingerprint is re-derived and
// compared with ExpectedCAFingerprint BEFORE a leaf is issued. A
// mismatch is a refusal, because a forwarded request that asked for a
// leaf from a different CA would otherwise produce a certificate the
// Colony does not trust.
func Issue(opts IssueOptions) (sealed []byte, caFingerprint string, err error) {
	if opts.CADir == "" {
		return nil, "", errors.New("colonyleaf: the CA directory is required")
	}
	if strings.TrimSpace(opts.NodeID) == "" {
		return nil, "", errors.New("colonyleaf: the joining Comb's node_id is required; it is the leaf's DNS name")
	}
	if strings.TrimSpace(opts.RequestID) == "" {
		return nil, "", errors.New("colonyleaf: the join request id is required; the leaf is bound to one request")
	}
	if strings.TrimSpace(opts.RecipientCertificatePEM) == "" {
		return nil, "", errors.New("colonyleaf: the joining Comb's certificate is required; the leaf is sealed to it")
	}
	now := opts.Now
	if now.IsZero() {
		now = time.Now()
	}

	caCertPEM, caKeyPEM, err := readCA(opts.CADir)
	if err != nil {
		return nil, "", err
	}
	// The CA is re-derived and compared, not believed. The holder is
	// asked for a leaf from ITS OWN CA, and the fingerprint says
	// which CA that is meant to be.
	fingerprint, err := colonyca.Fingerprint(caCertPEM)
	if err != nil {
		return nil, "", err
	}
	if opts.ExpectedCAFingerprint == "" {
		return nil, "", errors.New("colonyleaf: no expected CA fingerprint was supplied, so this Comb cannot be sure which CA it is being asked to issue from")
	}
	if !strings.EqualFold(fingerprint, opts.ExpectedCAFingerprint) {
		return nil, "", fmt.Errorf("colonyleaf: this Comb's Colony CA fingerprints as %s but %s was expected; this Comb is not the holder of the CA the Colony pinned", fingerprint, opts.ExpectedCAFingerprint)
	}

	dnsNames, ips, err := leafNames(opts.NodeID, opts.RaftBindAddress)
	if err != nil {
		return nil, "", err
	}
	certPEM, keyPEM, err := colonyca.IssueLeaf(caCertPEM, caKeyPEM, colonyca.LeafRequest{
		DNSNames: dnsNames,
		IPs:      ips,
	}, now)
	if err != nil {
		return nil, "", fmt.Errorf("colonyleaf: issuing the leaf: %w", err)
	}

	plain, err := json.Marshal(Leaf{
		CertificatePEM: string(certPEM),
		PrivateKeyPEM:  string(keyPEM),
		CAFingerprint:  fingerprint,
	})
	if err != nil {
		return nil, "", fmt.Errorf("colonyleaf: encoding the leaf: %w", err)
	}
	// The seal is to the JOINING Comb's own certificate, and its
	// associated data binds it to this request and this Comb. So the
	// ciphertext is readable only by the machine whose identity was
	// already verified, and a ciphertext copied from one join's
	// delivery fails to decrypt under another's.
	recipientFP, err := recipientFingerprint(opts.RecipientCertificatePEM)
	if err != nil {
		return nil, "", err
	}
	envelope, err := joincredential.Seal([]byte(opts.RecipientCertificatePEM), plain,
		joincredential.AssociatedData(opts.RequestID, opts.NodeID, recipientFP))
	if err != nil {
		return nil, "", fmt.Errorf("colonyleaf: sealing the leaf: %w", err)
	}
	return envelope, fingerprint, nil
}

// Open opens a sealed leaf with the joining Comb's own key.
//
// It is the joiner's half, and it is here rather than in the manager
// package for the same reason the issuance is.
func Open(envelopeJSON, certificatePEM, privateKeyPEM []byte, requestID, nodeID string) (*Leaf, error) {
	fingerprint, err := recipientFingerprint(string(certificatePEM))
	if err != nil {
		return nil, err
	}
	plain, err := joincredential.Open(envelopeJSON, certificatePEM, privateKeyPEM,
		joincredential.AssociatedData(requestID, nodeID, fingerprint))
	if err != nil {
		return nil, fmt.Errorf("colonyleaf: decrypting the leaf: %w", err)
	}
	var leaf Leaf
	if err := json.Unmarshal(plain, &leaf); err != nil {
		return nil, errors.New("colonyleaf: the decrypted leaf is malformed")
	}
	if leaf.CertificatePEM == "" || leaf.PrivateKeyPEM == "" {
		return nil, errors.New("colonyleaf: the decrypted leaf carries no certificate or no key")
	}
	return &leaf, nil
}

// readCA reads the CA certificate and the CA key from dir.
func readCA(dir string) (certPEM, keyPEM []byte, err error) {
	cert, err := os.ReadFile(filepath.Join(dir, colonyca.CACertFile))
	if err != nil {
		return nil, nil, fmt.Errorf("colonyleaf: reading the Colony CA certificate: %w - this Comb does not hold the Colony CA certificate and key, so it cannot issue a leaf", err)
	}
	key, err := os.ReadFile(filepath.Join(dir, colonyca.CAKeyFile))
	if err != nil {
		return nil, nil, fmt.Errorf("colonyleaf: reading the Colony CA key: %w - this Comb does not hold the Colony's CA KEY, and only the Comb that holds it can issue a leaf", err)
	}
	return cert, key, nil
}

// leafNames derives the leaf's names from the joining Comb's own
// identity.
//
// The DNS name is the node_id, because that is the name the Colony's
// leader dials the Comb by, and the IP is the one in the raft bind
// address. Both come from what the JOINER itself declared, never
// from the caller, so a leaf issued for one Comb cannot carry another
// Comb's names.
func leafNames(nodeID, raftBindAddress string) (dnsNames []string, ips []net.IP, err error) {
	dnsNames = []string{nodeID}
	host, _, splitErr := net.SplitHostPort(strings.TrimSpace(raftBindAddress))
	if splitErr != nil {
		// A bare address with no port: it may still be an IP.
		host = strings.TrimSpace(raftBindAddress)
	}
	if host != "" {
		if ip := net.ParseIP(host); ip != nil {
			ips = append(ips, ip)
		} else {
			dnsNames = append(dnsNames, host)
		}
	}
	if len(ips) == 0 && len(dnsNames) == 1 {
		return nil, nil, errors.New("colonyleaf: the joining Comb's raft_bind_address names no address the leaf could cover")
	}
	return dnsNames, ips, nil
}

// recipientFingerprint is the joining Comb's serving certificate
// fingerprint, in hostcert's format.
//
// It is the same value the join request recorded, so the seal's
// associated data binds the envelope to the certificate the Colony
// already verified.
func recipientFingerprint(certificatePEM string) (string, error) {
	block, _ := pem.Decode([]byte(certificatePEM))
	if block == nil || block.Type != "CERTIFICATE" {
		return "", errors.New("colonyleaf: the recipient's certificate is missing or malformed")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return "", fmt.Errorf("colonyleaf: parsing the recipient's certificate: %w", err)
	}
	return hostcert.Fingerprint(cert.Raw), nil
}
