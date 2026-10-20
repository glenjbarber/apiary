// Package colonyca is the certificate authority for one Colony's
// inter-node TLS. It is step 1 of issue #97 and is not yet called by
// any daemon or installer command.
//
// The trust model, in plain terms:
//
//   - The first Comb of a new Colony creates a private certificate
//     authority (the CA): one key pair and one self-signed CA
//     certificate. The CA certificate is public and may be copied to
//     every Comb. The CA key is the Colony's root of trust. It stays on
//     the Comb that made it, readable only by root, and it is what
//     signs every node certificate. Whoever holds that key can mint a
//     certificate that the whole Colony will accept.
//
//   - Each Comb has its own leaf certificate, signed by that CA. The
//     leaf can both serve and dial, because raft's mutual TLS needs
//     both directions. Its names (DNS and IP) must match the address
//     that peers use to reach it.
//
//   - A Comb trusts a peer only if the peer's certificate chains to the
//     Colony CA. A Comb that joins the Colony does not learn which CA
//     to trust from the network. It trusts only a CA whose SHA-256
//     fingerprint matches the value carried in the join authorization.
//     Fingerprint computes that value. The join itself is step 3 and is
//     not implemented here.
//
// Refusals are deliberate. GenerateCA never overwrites an existing CA,
// because replacing a CA silently invalidates every leaf the Colony
// holds. IssueLeaf refuses an empty request, a certificate that is not
// a CA, and an expired CA. Those are errors, not warnings.
//
// Nothing here changes the self-signed serving certificates that
// internal/hostcert issues for the external API. Those certificates
// carry only the ServerAuth usage and cannot serve mutual TLS. They are
// superseded for inter-node use by certificates from this CA. See
// ADR-0155.
package colonyca

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"io/fs"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	// CACertFile and CAKeyFile are the names GenerateCA writes inside
	// its directory.
	CACertFile = "ca.pem"
	CAKeyFile  = "ca.key"

	// CAValidity is the lifetime of a Colony CA certificate, counted
	// from the moment it is generated.
	CAValidity = 10 * 365 * 24 * time.Hour

	// DefaultLeafLifetime is used when a LeafRequest leaves Lifetime
	// zero.
	DefaultLeafLifetime = 365 * 24 * time.Hour

	// notBeforeBackdate lets two Combs whose clocks disagree by a
	// little still accept each other's freshly issued certificates. It
	// matches internal/hostcert.
	notBeforeBackdate = time.Hour
)

// LeafRequest describes one node certificate. At least one of DNSNames
// and IPs must be set.
type LeafRequest struct {
	DNSNames []string
	IPs      []net.IP

	// Lifetime is how long the leaf is valid from now. Zero means
	// DefaultLeafLifetime. The leaf may not outlive the CA.
	Lifetime time.Duration
}

// GenerateCA creates a new Colony CA in dir: CACertFile (0644) and
// CAKeyFile (0600). The CA is ECDSA P-256, valid for CAValidity from
// now, with BasicConstraints CA:true and MaxPathLen 0, and KeyUsage
// CertSign|CRLSign.
//
// It refuses if either file already exists. In that case it returns an
// error naming the path and changes neither file. It returns the
// SHA-256 fingerprint of the new CA certificate, which is what a join
// authorization will pin.
func GenerateCA(dir string, now time.Time) (fingerprint string, err error) {
	certPath := filepath.Join(dir, CACertFile)
	keyPath := filepath.Join(dir, CAKeyFile)
	var existing []string
	for _, p := range []string{certPath, keyPath} {
		if _, err := os.Lstat(p); err == nil {
			existing = append(existing, p)
		} else if !errors.Is(err, fs.ErrNotExist) {
			return "", fmt.Errorf("colonyca: checking %s: %w", p, err)
		}
	}
	if len(existing) > 0 {
		return "", fmt.Errorf("colonyca: refusing to overwrite existing CA file(s) %s", strings.Join(existing, ", "))
	}

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return "", fmt.Errorf("colonyca: generating CA key: %w", err)
	}
	serial, err := newSerial()
	if err != nil {
		return "", err
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "Apiary Colony CA"},
		NotBefore:             now.Add(-notBeforeBackdate),
		NotAfter:              now.Add(CAValidity),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLen:            0,
		MaxPathLenZero:        true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return "", fmt.Errorf("colonyca: creating CA certificate: %w", err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return "", fmt.Errorf("colonyca: encoding CA key: %w", err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})

	// The key is published first, so a certificate never exists on disk
	// without the key that signs for it. If the certificate cannot be
	// published, the key this call created is removed again.
	if err := publishNew(keyPath, keyPEM, 0o600); err != nil {
		return "", err
	}
	if err := publishNew(certPath, certPEM, 0o644); err != nil {
		_ = os.Remove(keyPath)
		return "", err
	}
	return Fingerprint(certPEM)
}

// IssueLeaf signs a new leaf certificate for req with the Colony CA
// given as PEM. It returns the leaf certificate and its PKCS#8 key, both
// PEM-encoded. The leaf key is generated here and nothing is written to
// disk.
//
// It refuses: a request with no DNS names and no IPs; a CA certificate
// that is not a CA; a CA that is expired or not yet valid at now; a CA
// certificate that does not match its key; and a lifetime that would
// outlive the CA.
func IssueLeaf(caCertPEM, caKeyPEM []byte, req LeafRequest, now time.Time) (certPEM, keyPEM []byte, err error) {
	if len(req.DNSNames) == 0 && len(req.IPs) == 0 {
		return nil, nil, errors.New("colonyca: leaf request names no DNS names and no IP addresses")
	}
	for _, name := range req.DNSNames {
		if name == "" {
			return nil, nil, errors.New("colonyca: leaf request contains an empty DNS name")
		}
	}
	for _, ip := range req.IPs {
		if ip == nil {
			return nil, nil, errors.New("colonyca: leaf request contains a nil IP address")
		}
	}
	lifetime := req.Lifetime
	if lifetime == 0 {
		lifetime = DefaultLeafLifetime
	}
	if lifetime < 0 {
		return nil, nil, fmt.Errorf("colonyca: leaf lifetime %s is negative", lifetime)
	}

	pair, err := tls.X509KeyPair(caCertPEM, caKeyPEM)
	if err != nil {
		return nil, nil, fmt.Errorf("colonyca: CA certificate and key do not form a pair: %w", err)
	}
	caCert, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		return nil, nil, fmt.Errorf("colonyca: parsing CA certificate: %w", err)
	}
	if !caCert.BasicConstraintsValid || !caCert.IsCA {
		return nil, nil, errors.New("colonyca: the certificate given as CA is not a CA certificate")
	}
	if now.Before(caCert.NotBefore) {
		return nil, nil, fmt.Errorf("colonyca: CA certificate is not valid until %s", caCert.NotBefore.UTC().Format(time.RFC3339))
	}
	if now.After(caCert.NotAfter) {
		return nil, nil, fmt.Errorf("colonyca: CA certificate expired at %s", caCert.NotAfter.UTC().Format(time.RFC3339))
	}
	notAfter := now.Add(lifetime)
	if notAfter.After(caCert.NotAfter) {
		return nil, nil, fmt.Errorf("colonyca: leaf would expire at %s, after the CA expires at %s",
			notAfter.UTC().Format(time.RFC3339), caCert.NotAfter.UTC().Format(time.RFC3339))
	}
	signer, ok := pair.PrivateKey.(crypto.Signer)
	if !ok {
		return nil, nil, errors.New("colonyca: CA key is not a signing key")
	}

	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, fmt.Errorf("colonyca: generating leaf key: %w", err)
	}
	serial, err := newSerial()
	if err != nil {
		return nil, nil, err
	}
	var cn string
	if len(req.DNSNames) > 0 {
		cn = req.DNSNames[0]
	} else {
		cn = req.IPs[0].String()
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: cn},
		DNSNames:              req.DNSNames,
		IPAddresses:           req.IPs,
		NotBefore:             now.Add(-notBeforeBackdate),
		NotAfter:              notAfter,
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		BasicConstraintsValid: true,
		IsCA:                  false,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, caCert, &leafKey.PublicKey, signer)
	if err != nil {
		return nil, nil, fmt.Errorf("colonyca: signing leaf certificate: %w", err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(leafKey)
	if err != nil {
		return nil, nil, fmt.Errorf("colonyca: encoding leaf key: %w", err)
	}
	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	return certPEM, keyPEM, nil
}

// Fingerprint returns the lowercase hex SHA-256 digest of the DER bytes
// of the first certificate in certPEM. The result has no prefix and no
// separators, for example "9f86d081...". This is deliberately NOT the
// internal/hostcert format ("SHA256:" followed by uppercase,
// colon-separated pairs). Join authorizations use this format, so the
// two formats must not be compared directly.
func Fingerprint(certPEM []byte) (string, error) {
	block, _ := pem.Decode(certPEM)
	if block == nil || block.Type != "CERTIFICATE" {
		return "", errors.New("colonyca: fingerprint input has no CERTIFICATE PEM block")
	}
	if _, err := x509.ParseCertificate(block.Bytes); err != nil {
		return "", fmt.Errorf("colonyca: parsing certificate for fingerprint: %w", err)
	}
	sum := sha256.Sum256(block.Bytes)
	return hex.EncodeToString(sum[:]), nil
}

// newSerial returns a random 128-bit positive serial number.
func newSerial() (*big.Int, error) {
	limit := new(big.Int).Lsh(big.NewInt(1), 128)
	serial, err := rand.Int(rand.Reader, limit)
	if err != nil {
		return nil, fmt.Errorf("colonyca: generating serial number: %w", err)
	}
	return serial.Add(serial, big.NewInt(1)), nil
}

// publishNew writes body to path with the given mode and fails if path
// already exists. The body is written to a temporary file in the same
// directory, chmodded, fsynced, and then hard-linked to path. A hard
// link fails when the target exists, so this gives the no-overwrite
// guarantee that a rename cannot give. The temporary name is removed in
// every case. Renaming over the target would race with another writer
// between the existence check and the rename.
func publishNew(path string, body []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".colonyca-*.tmp")
	if err != nil {
		return fmt.Errorf("colonyca: creating temp file in %s: %w", dir, err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if _, err := tmp.Write(body); err != nil {
		tmp.Close()
		return fmt.Errorf("colonyca: writing temp file for %s: %w", path, err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("colonyca: syncing temp file for %s: %w", path, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("colonyca: closing temp file for %s: %w", path, err)
	}
	if err := os.Chmod(tmpPath, mode); err != nil {
		return fmt.Errorf("colonyca: setting permissions on %s: %w", path, err)
	}
	if err := os.Link(tmpPath, path); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return fmt.Errorf("colonyca: refusing to overwrite existing CA file %s", path)
		}
		return fmt.Errorf("colonyca: publishing %s: %w", path, err)
	}
	return nil
}
