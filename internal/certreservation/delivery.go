package certreservation

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/glenjbarber/apiary/internal/hostcert"
	"github.com/glenjbarber/apiary/internal/joincredential"
)

// Bundle is the plaintext payload sealed inside a delivery envelope.
//
// It carries the prepared certificate AND the bootstrap private key,
// because both are needed to install the pair and neither is safe to
// carry in the clear. The integration configuration - the Cloudflare
// token, the zone, the ACME account - is NOT here: it travels in its
// own protected channel and is provisioned separately, so that
// revoking the prepared certificate does not also revoke the ability
// to obtain a replacement.
type Bundle struct {
	Version        int    `json:"version"`
	ReservationID  string `json:"reservation_id"`
	RequestID      string `json:"request_id"`
	NodeID         string `json:"node_id"`
	FQDN           string `json:"fqdn"`
	CertificatePEM string `json:"certificate_pem"`
	PrivateKeyPEM  string `json:"private_key_pem"`
	// Fingerprint is the hostcert digest of the certificate above, so
	// the receiver can check that what it decrypted is what the
	// reservation named WITHOUT trusting the sender's own claim about
	// it. The receiver re-derives it from the PEM.
	Fingerprint string `json:"fingerprint"`
	// IssuedAtUnix is when the certificate was issued, so a receiver
	// can tell a freshly prepared pair from a stale one.
	IssuedAtUnix int64 `json:"issued_at_unix"`
}

// bundleVersion is the version of the Bundle format.
//
// Versioned so that a future change can fail closed rather than
// guessing how old bytes should be read - the same rule
// joincredential's own envelope version follows.
const bundleVersion = 1

// AssociatedData binds a delivery envelope to one reservation, one
// join request, and one certificate.
//
// This is what makes a delivery non-replayable across reservations: a
// ciphertext copied from one reservation's delivery cannot be opened
// under another reservation's associated data, so it fails to decrypt
// rather than decrypting into the wrong joiner's key.
func AssociatedData(reservationID, requestID, nodeID, fingerprint string) []byte {
	return []byte("apiary-certificate-reservation/v1\x00" + reservationID + "\x00" + requestID + "\x00" + nodeID + "\x00" + fingerprint)
}

// SealDelivery seals a bundle to the joining Comb's serving
// certificate.
//
// The recipient certificate is the one the joiner presented during the
// join attestation, which is exactly the certificate the peers will
// pin. Sealing to it means the prepared private key is readable only
// by the machine whose identity was already verified, and by nobody
// else - not by the preparing Comb, not by a third party that learns
// the reservation ID, and not by a different joiner that presents the
// same hostname.
func SealDelivery(res *Reservation, requestID, nodeID, recipientCertificatePEM, privateKeyPEM string, issuedAt time.Time) ([]byte, error) {
	if res == nil {
		return nil, fmt.Errorf("certreservation: the reservation is required")
	}
	if strings.TrimSpace(recipientCertificatePEM) == "" {
		return nil, fmt.Errorf("certreservation: the recipient's certificate is required")
	}
	if strings.TrimSpace(privateKeyPEM) == "" {
		return nil, fmt.Errorf("certreservation: the bootstrap private key is required")
	}
	if res.Fingerprint == "" {
		return nil, fmt.Errorf("certreservation: the reservation carries no fingerprint to bind the delivery to")
	}
	bundle := Bundle{
		Version:        bundleVersion,
		ReservationID:  res.ID,
		RequestID:      requestID,
		NodeID:         nodeID,
		FQDN:           res.FQDN,
		CertificatePEM: res.CertificatePEM,
		PrivateKeyPEM:  privateKeyPEM,
		Fingerprint:    res.Fingerprint,
		IssuedAtUnix:   issuedAt.UTC().Unix(),
	}
	plaintext, err := marshalBundle(bundle)
	if err != nil {
		return nil, err
	}
	aad := AssociatedData(res.ID, requestID, nodeID, res.Fingerprint)
	envelope, err := joincredential.Seal([]byte(recipientCertificatePEM), plaintext, aad)
	if err != nil {
		return nil, fmt.Errorf("certreservation: sealing the delivery: %w", err)
	}
	return envelope, nil
}

// OpenDelivery opens a delivery envelope with the joining Comb's own
// certificate and private key, and checks that what came out is what
// the reservation promised.
//
// The checks here are the receiver's half of the contract, and they are
// the ones that make a mis-delivery a refusal rather than a
// surprise:
//
//   - The envelope must open at all.
//   - The bundle's version must be one this build understands.
//   - The bundle must be for this reservation, this request and this
//     node.
//   - The certificate must cover the reserved name.
//   - The private key must match the certificate.
//
// The name check matters because a certificate for the right key but
// the wrong hostname would install cleanly and then fail every TLS
// handshake with a name mismatch, which is a much worse place to
// discover it.
func OpenDelivery(envelopeJSON, certificatePEM, privateKeyPEM string, res *Reservation) (*Bundle, error) {
	if res == nil {
		return nil, fmt.Errorf("certreservation: the reservation is required")
	}
	aad := AssociatedData(res.ID, res.RequestID, res.NodeID, res.Fingerprint)
	plaintext, err := joincredential.Open([]byte(envelopeJSON), []byte(certificatePEM), []byte(privateKeyPEM), aad)
	if err != nil {
		return nil, fmt.Errorf("certreservation: opening the delivery: %w", err)
	}
	bundle, err := unmarshalBundle(plaintext)
	if err != nil {
		return nil, err
	}
	if bundle.Version != bundleVersion {
		return nil, fmt.Errorf("certreservation: unsupported delivery bundle version %d", bundle.Version)
	}
	if bundle.ReservationID != res.ID {
		return nil, fmt.Errorf("certreservation: the delivery is for reservation %q, not %q", bundle.ReservationID, res.ID)
	}
	if bundle.RequestID != res.RequestID {
		return nil, fmt.Errorf("certreservation: the delivery is for join request %q, not %q", bundle.RequestID, res.RequestID)
	}
	if bundle.NodeID != res.NodeID {
		return nil, fmt.Errorf("certreservation: the delivery is for node %q, not %q", bundle.NodeID, res.NodeID)
	}
	// The fingerprint is RE-DERIVED from the certificate that arrived,
	// not read from the bundle. A sender that claims a different
	// fingerprint than its certificate has is either broken or lying,
	// and either way the certificate is what will actually be
	// installed.
	derived, err := CertificateFingerprint(bundle.CertificatePEM)
	if err != nil {
		return nil, fmt.Errorf("certreservation: the delivered certificate could not be read: %w", err)
	}
	if derived != res.Fingerprint {
		return nil, fmt.Errorf("certreservation: the delivered certificate's fingerprint (%s) is not the one reserved (%s)", derived, res.Fingerprint)
	}
	if !coversName(bundle.CertificatePEM, res.FQDN) {
		return nil, fmt.Errorf("%w: %q", ErrNameMismatch, res.FQDN)
	}
	if err := CheckKeyMatchesCertificate(bundle.CertificatePEM, bundle.PrivateKeyPEM); err != nil {
		return nil, err
	}
	return bundle, nil
}

// CertificateFingerprint returns the hostcert digest of a PEM
// certificate chain's leaf.
//
// It is the same function internal/hostcert owns, called the same
// way, so that a fingerprint written by the reservation and a
// fingerprint compared by the peer trust store are the same string by
// construction rather than by two implementations agreeing.
func CertificateFingerprint(certificatePEM string) (string, error) {
	cert, err := parseLeaf(certificatePEM)
	if err != nil {
		return "", err
	}
	return hostcert.Fingerprint(cert.Raw), nil
}

// CheckKeyMatchesCertificate verifies that a private key matches a
// certificate's public key.
//
// A mismatched pair installs atomically and then fails every
// handshake, so this is checked BEFORE anything is written.
func CheckKeyMatchesCertificate(certificatePEM, privateKeyPEM string) error {
	cert, err := parseLeaf(certificatePEM)
	if err != nil {
		return fmt.Errorf("certreservation: reading the certificate: %w", err)
	}
	block, _ := pem.Decode([]byte(privateKeyPEM))
	if block == nil {
		return fmt.Errorf("%w: the private key is not PEM-encoded", ErrKeyMismatch)
	}
	var priv any
	switch block.Type {
	case "EC PRIVATE KEY":
		priv, err = x509.ParseECPrivateKey(block.Bytes)
	case "PRIVATE KEY":
		priv, err = x509.ParsePKCS8PrivateKey(block.Bytes)
	case "RSA PRIVATE KEY":
		priv, err = x509.ParsePKCS1PrivateKey(block.Bytes)
	default:
		return fmt.Errorf("%w: unsupported private key type %q", ErrKeyMismatch, block.Type)
	}
	if err != nil {
		return fmt.Errorf("%w: reading the private key: %v", ErrKeyMismatch, err)
	}
	switch pub := cert.PublicKey.(type) {
	case *ecdsa.PublicKey:
		key, ok := priv.(*ecdsa.PrivateKey)
		if !ok {
			return fmt.Errorf("%w: the certificate is ECDSA and the private key is not", ErrKeyMismatch)
		}
		if !key.PublicKey.Equal(pub) {
			return fmt.Errorf("%w: the ECDSA private key does not match the certificate", ErrKeyMismatch)
		}
		return nil
	default:
		// Any other key type is compared through the standard
		// library's own comparison of the certificate's public key
		// against the key's own public half.
		pub, err := publicKeyOf(priv)
		if err != nil {
			return fmt.Errorf("%w: %v", ErrKeyMismatch, err)
		}
		if !publicKeysEqual(pub, cert.PublicKey) {
			return fmt.Errorf("%w: the private key does not match the certificate", ErrKeyMismatch)
		}
		return nil
	}
}

// coversName reports whether the certificate's SANs cover name.
func coversName(certificatePEM, name string) bool {
	cert, err := parseLeaf(certificatePEM)
	if err != nil {
		return false
	}
	for _, dns := range cert.DNSNames {
		if strings.EqualFold(dns, name) {
			return true
		}
	}
	return false
}

// parseLeaf returns the leaf certificate of a PEM chain.
func parseLeaf(certificatePEM string) (*x509.Certificate, error) {
	block, _ := pem.Decode([]byte(certificatePEM))
	if block == nil || block.Type != "CERTIFICATE" {
		return nil, errors.New("certreservation: the certificate is not PEM-encoded")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("certreservation: parsing the certificate: %w", err)
	}
	return cert, nil
}

// GenerateBootstrapKey creates the temporary key pair a prepared
// certificate is bound to.
//
// It is generated HERE, on the preparing Comb, because the joining
// Comb does not exist yet - which is the whole reason the prepared
// mode exists. That makes this key a secret with two holders, and the
// caller is expected to retire it once the joiner has replaced it with
// a key of its own.
//
// The key is generated with hostcert.Generate's own key generation
// rather than a second implementation, so the bootstrap pair is a
// normal Apiary serving pair - the same shape, the same file format,
// and the same hostcert.Fingerprint - rather than a special kind of
// key that the rest of the system has never seen.
func GenerateBootstrapKey(fqdn string) (privateKeyPEM string, err error) {
	opts := hostcert.GenerateOptions{CommonName: fqdn}
	if fqdn != "" {
		opts.DNSNames = []string{fqdn}
	}
	_, key, err := hostcert.Generate(opts)
	if err != nil {
		return "", fmt.Errorf("certreservation: generating the bootstrap key: %w", err)
	}
	return string(key), nil
}

// publicKeysEqual compares two public keys.
//
// crypto/x509 does not export a cross-type comparison, so each
// supported type is compared through its own standard-library
// equality. An unsupported type on either side is a refusal, not a
// silent pass.
func publicKeysEqual(a, b crypto.PublicKey) bool {
	switch left := a.(type) {
	case *ecdsa.PublicKey:
		right, ok := b.(*ecdsa.PublicKey)
		return ok && left.Equal(right)
	case *rsa.PublicKey:
		right, ok := b.(*rsa.PublicKey)
		return ok && left.Equal(right)
	case ed25519.PublicKey:
		right, ok := b.(ed25519.PublicKey)
		return ok && left.Equal(right)
	default:
		return false
	}
}

// publicKeyOf returns the public half of a parsed private key.
func publicKeyOf(priv any) (crypto.PublicKey, error) {
	switch key := priv.(type) {
	case *ecdsa.PrivateKey:
		return &key.PublicKey, nil
	case *rsa.PrivateKey:
		return &key.PublicKey, nil
	case ed25519.PrivateKey:
		return key.Public(), nil
	default:
		return nil, fmt.Errorf("unsupported private key type %T", priv)
	}
}

// marshalBundle encodes a delivery bundle.
func marshalBundle(b Bundle) ([]byte, error) {
	wire, err := json.Marshal(b)
	if err != nil {
		return nil, fmt.Errorf("certreservation: encoding the delivery bundle: %w", err)
	}
	return wire, nil
}

// unmarshalBundle decodes a delivery bundle.
func unmarshalBundle(plaintext []byte) (*Bundle, error) {
	var b Bundle
	if err := json.Unmarshal(plaintext, &b); err != nil {
		return nil, fmt.Errorf("certreservation: decoding the delivery bundle: %w", err)
	}
	return &b, nil
}
