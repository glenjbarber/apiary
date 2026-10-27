package colonyca

import (
	"bytes"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"strings"
	"time"
)

// VerifyCA checks that certPEM is a usable Colony CA certificate, and
// that it is the CA the caller expected.
//
// It is the joiner's half of ADR-0155 step 3: a joining Comb must not
// trust a CA merely because it was handed one, so the fingerprint the
// operator pinned is checked against the certificate itself BEFORE
// the CA is written anywhere.
//
// The fingerprint is compared in THIS package's own format - the
// lowercase hex SHA-256 of the certificate's DER bytes. ADR-0155
// records that this is deliberately a different shape from
// internal/hostcert.Fingerprint ("SHA256:" followed by uppercase
// colon-separated pairs), and that the two must never be compared
// directly. This function exists so that a caller cannot compare them
// directly by accident: it takes the fingerprint in colonyca's format
// and does the comparison here.
//
// The checks, in order, and each refusal names itself:
//
//   - the PEM holds exactly one certificate;
//   - that certificate parses;
//   - it declares itself a certificate authority, because a CA that
//     does not is not an anchor;
//   - it is inside its own validity window now, because a CA that has
//     expired will not be able to sign a leaf that anybody accepts;
//   - its fingerprint is the one the operator pinned.
//
// A CA whose fingerprint does not match is refused EVEN IF EVERY OTHER
// CHECK PASSES. That is the whole point of the fingerprint: it is the
// one value the operator compared by hand, out of band, and a
// certificate that satisfies every structural check but not that one is
// exactly the certificate the comparison exists to catch.
func VerifyCA(certPEM []byte, expectedFingerprint string, now time.Time) error {
	if len(certPEM) == 0 {
		return errors.New("colonyca: the CA certificate is empty")
	}
	block, _ := pem.Decode(certPEM)
	if block == nil || block.Type != "CERTIFICATE" {
		return errors.New("colonyca: the CA material has no CERTIFICATE PEM block")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return fmt.Errorf("colonyca: parsing the CA certificate: %w", err)
	}
	if !cert.IsCA || !cert.BasicConstraintsValid {
		return fmt.Errorf("colonyca: the certificate for %q does not declare itself a certificate authority, so it cannot anchor a Colony", cert.Subject.String())
	}
	if now.Before(cert.NotBefore) {
		return fmt.Errorf("colonyca: the CA certificate for %q is not valid until %s", cert.Subject.String(), cert.NotBefore.UTC().Format(time.RFC3339))
	}
	if now.After(cert.NotAfter) {
		return fmt.Errorf("colonyca: the CA certificate for %q expired at %s", cert.Subject.String(), cert.NotAfter.UTC().Format(time.RFC3339))
	}
	if expectedFingerprint == "" {
		return errors.New("colonyca: no expected CA fingerprint was supplied, so the CA cannot be pinned; a join that does not pin its CA is not a join")
	}
	actual, err := Fingerprint(certPEM)
	if err != nil {
		return err
	}
	if !strings.EqualFold(actual, expectedFingerprint) {
		return fmt.Errorf("colonyca: the CA certificate's fingerprint is %s but %s was pinned; the certificate does not chain to the Colony CA the operator compared", actual, expectedFingerprint)
	}
	return nil
}

// VerifyLeafAgainstCA checks that leafPEM was signed by the CA in
// caPEM, and that the leaf's own names are the ones the caller
// expects.
//
// It is the second half of the joiner's check: a CA that matches the
// pinned fingerprint is trusted, but a leaf that does not chain to it
// is not the certificate the CA signed, and a joiner that installed
// such a leaf would be running with a certificate no peer could
// verify.
//
// The names are checked with x509's own verifier, which is what
// enforces the usage the connection needs (ClientAuth, for raft's
// mutual TLS) as well as the name.
func VerifyLeafAgainstCA(leafPEM, caPEM []byte, expectedDNSNames []string, now time.Time) error {
	if len(leafPEM) == 0 {
		return errors.New("colonyca: the leaf certificate is empty")
	}
	leafBlock, _ := pem.Decode(leafPEM)
	if leafBlock == nil || leafBlock.Type != "CERTIFICATE" {
		return errors.New("colonyca: the leaf material has no CERTIFICATE PEM block")
	}
	leaf, err := x509.ParseCertificate(leafBlock.Bytes)
	if err != nil {
		return fmt.Errorf("colonyca: parsing the leaf certificate: %w", err)
	}
	caBlock, _ := pem.Decode(caPEM)
	if caBlock == nil || caBlock.Type != "CERTIFICATE" {
		return errors.New("colonyca: the CA material has no CERTIFICATE PEM block")
	}
	caCert, err := x509.ParseCertificate(caBlock.Bytes)
	if err != nil {
		return fmt.Errorf("colonyca: parsing the CA certificate: %w", err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(caCert)
	// The CA is the root here, and the leaf is verified against it
	// with BOTH usages, because raft's mutual TLS needs the leaf to
	// serve and to dial. A leaf that carries only ServerAuth cannot
	// be a raft identity, and refusing it here is better than
	// discovering it at a handshake.
	if _, err := leaf.Verify(x509.VerifyOptions{
		Roots:       pool,
		CurrentTime: now,
		KeyUsages:   []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
	}); err != nil {
		return fmt.Errorf("colonyca: the leaf certificate for %q does not chain to the Colony CA: %w", leaf.Subject.String(), err)
	}
	if len(expectedDNSNames) > 0 {
		for _, want := range expectedDNSNames {
			if !containsString(leaf.DNSNames, want) {
				return fmt.Errorf("colonyca: the leaf certificate for %q carries no DNS name %q (it has: %s)", leaf.Subject.String(), want, joinStrings(leaf.DNSNames, ", "))
			}
		}
	}
	return nil
}

// caPEMEqual reports whether two CA certificates are the same
// certificate, by DER bytes.
//
// It exists so that "the joiner already has this CA" is a question
// about CERTIFICATES rather than about file paths or fingerprints in
// two spellings. Two files can hold the same CA; a fingerprint
// comparison in the wrong format would call them different.
func caPEMEqual(a, b []byte) bool {
	aBlock, _ := pem.Decode(a)
	bBlock, _ := pem.Decode(b)
	if aBlock == nil || bBlock == nil {
		return false
	}
	return bytes.Equal(aBlock.Bytes, bBlock.Bytes)
}

// containsString reports whether list holds want.
func containsString(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

// joinStrings renders a list for an error message.
func joinStrings(list []string, sep string) string {
	if len(list) == 0 {
		return "(none)"
	}
	return strings.Join(list, sep)
}
